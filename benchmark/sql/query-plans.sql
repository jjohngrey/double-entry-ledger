\pset pager off
\timing on

ANALYZE accounts;
ANALYZE entries;
ANALYZE transactions;
ANALYZE idempotency_keys;
ANALYZE sagas;
ANALYZE outbox_events;
ANALYZE processed_events;
ANALYZE daily_account_aggregates;
ANALYZE daily_ledger_aggregates;

\echo 'entry lookup by transaction (idempotent replay and event publisher)'
EXPLAIN (ANALYZE, BUFFERS, WAL, SETTINGS)
SELECT id, account_id, transaction_id, credit, debit, created_at
FROM entries
WHERE transaction_id = (
    SELECT transaction_id
    FROM idempotency_keys
    WHERE key LIKE 'benchmark-duplicate-%'
    LIMIT 1
);

\echo 'account history range in stable order'
EXPLAIN (ANALYZE, BUFFERS, WAL, SETTINGS)
SELECT id, account_id, transaction_id, credit, debit, created_at
FROM entries
WHERE account_id = (SELECT id FROM accounts WHERE name = 'bench-history-debit')
  AND created_at >= NOW() - INTERVAL '1 year'
  AND created_at <= NOW()
ORDER BY created_at, id;

\echo 'saga lookup by primary key'
EXPLAIN (ANALYZE, BUFFERS, WAL, SETTINGS)
SELECT id, source_ledger_id, source_account_id, destination_ledger_id,
       destination_account_id, amount, status, created_at, updated_at
FROM sagas
WHERE id = (SELECT id FROM sagas LIMIT 1);

\echo 'saga lookup by idempotency key'
EXPLAIN (ANALYZE, BUFFERS, WAL, SETTINGS)
SELECT id, source_ledger_id, source_account_id, destination_ledger_id,
       destination_account_id, amount, status, created_at, updated_at
FROM sagas
WHERE idempotency_key = (SELECT idempotency_key FROM sagas LIMIT 1);

\echo 'batched posting account lock'
EXPLAIN (ANALYZE, BUFFERS, WAL, SETTINGS)
SELECT id, ledger_id, type::text, balance::text
FROM accounts
WHERE id IN (
    (SELECT id FROM accounts WHERE name = 'bench-normal-debit-000000'),
    (SELECT id FROM accounts WHERE name = 'bench-normal-credit-000000')
)
ORDER BY id
FOR UPDATE;

\echo 'ready transaction outbox event'
BEGIN;
WITH fixture AS (
    INSERT INTO transactions (ledger_id)
    VALUES ('00000000-0000-0000-0000-000000000000')
    RETURNING id
)
INSERT INTO outbox_events (transaction_id, event_type, status, available_at)
SELECT id, 'transaction_posted', 'pending', NOW() FROM fixture;
EXPLAIN (ANALYZE, BUFFERS, WAL, SETTINGS)
SELECT o.id, t.id, t.ledger_id, t.created_at
FROM outbox_events o
JOIN transactions t ON t.id = o.transaction_id
WHERE o.status = 'pending'
  AND o.event_type = 'transaction_posted'
  AND o.available_at <= NOW()
ORDER BY o.created_at
FOR UPDATE OF o SKIP LOCKED
LIMIT 1;
ROLLBACK;

\echo 'exact transaction projection status poll'
EXPLAIN (ANALYZE, BUFFERS, WAL, SETTINGS)
SELECT o.id,
       EXISTS (
           SELECT 1
           FROM processed_events p
           WHERE p.consumer = 'daily-aggregates-v1' AND p.event_id = o.id
       ) projected
FROM outbox_events o
WHERE o.transaction_id = 'd64ba0b9-020a-5b4e-949b-b33f1e785337'
  AND o.event_type = 'transaction_posted';

\echo 'aggregate projection upserts (rolled back)'
BEGIN;
EXPLAIN (ANALYZE, BUFFERS, WAL, SETTINGS)
INSERT INTO daily_account_aggregates (account_id,day,debit,credit,last_event_id)
VALUES ('15620168-31cd-51e8-b265-e4147db1e51a',CURRENT_DATE,1,0,gen_random_uuid())
ON CONFLICT (account_id,day) DO UPDATE
SET debit=daily_account_aggregates.debit+EXCLUDED.debit,
    credit=daily_account_aggregates.credit+EXCLUDED.credit,
    updated_at=NOW(),
    last_event_id=EXCLUDED.last_event_id;
EXPLAIN (ANALYZE, BUFFERS, WAL, SETTINGS)
INSERT INTO daily_ledger_aggregates (ledger_id,day,debit,credit,last_event_id)
VALUES ('a980efbf-6ed7-5614-865b-eca8344b1c9b',CURRENT_DATE,1,1,gen_random_uuid())
ON CONFLICT (ledger_id,day) DO UPDATE
SET debit=daily_ledger_aggregates.debit+EXCLUDED.debit,
    credit=daily_ledger_aggregates.credit+EXCLUDED.credit,
    updated_at=NOW(),
    last_event_id=EXCLUDED.last_event_id;
ROLLBACK;

\echo 'batched entry insert and balance update (rolled back)'
BEGIN;
EXPLAIN (ANALYZE, BUFFERS, WAL, SETTINGS)
WITH fixture AS (
    INSERT INTO transactions (ledger_id)
    SELECT ledger_id FROM accounts WHERE name = 'bench-normal-debit-000000'
    RETURNING id
)
INSERT INTO entries (account_id, transaction_id, credit, debit)
SELECT account.id, fixture.id,
       CASE WHEN account.name LIKE '%credit%' THEN 1 ELSE 0 END,
       CASE WHEN account.name LIKE '%debit%' THEN 1 ELSE 0 END
FROM accounts account
CROSS JOIN fixture
WHERE account.name IN ('bench-normal-debit-000000', 'bench-normal-credit-000000');
EXPLAIN (ANALYZE, BUFFERS, WAL, SETTINGS)
UPDATE accounts AS account
SET balance = account.balance + change.debit - change.credit
FROM (
    VALUES
      ((SELECT id FROM accounts WHERE name = 'bench-normal-debit-000000'), 1::numeric, 0::numeric),
      ((SELECT id FROM accounts WHERE name = 'bench-normal-credit-000000'), 0::numeric, 1::numeric)
) AS change(id, debit, credit)
WHERE account.id = change.id;
ROLLBACK;
