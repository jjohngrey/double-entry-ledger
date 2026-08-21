DROP TABLE IF EXISTS daily_ledger_aggregates;
DROP TABLE IF EXISTS daily_account_aggregates;
DROP TABLE IF EXISTS processed_events;
DROP INDEX IF EXISTS outbox_transaction_event_unique;
ALTER TABLE outbox_events DROP CONSTRAINT IF EXISTS outbox_event_target;
ALTER TABLE outbox_events DROP COLUMN IF EXISTS transaction_id;
ALTER TABLE outbox_events ALTER COLUMN saga_id SET NOT NULL;
