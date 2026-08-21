-- name: CreateAccount :one
INSERT INTO accounts (name, type) VALUES ($1, $2) RETURNING id;

-- name: CreateTransaction :one
INSERT INTO transactions DEFAULT VALUES RETURNING id;

-- name: CreateEntry :one
INSERT INTO entries (account_id, transaction_id, credit, debit) VALUES ($1, $2, $3, $4) RETURNING id;

-- name: GetAccountEntries :many
SELECT id, account_id, transaction_id, credit, debit, created_at
FROM entries
WHERE account_id = $1 AND created_at >= $2 AND created_at <= $3
ORDER BY created_at ASC, id ASC;

-- name: GetAccountBalance :one
SELECT CAST(CASE
    WHEN type IN ('asset', 'expense') THEN balance
    ELSE balance * -1
END AS NUMERIC(20,2)) AS balance
FROM accounts
WHERE id = $1;

-- name: UpdateAccountBalance :one
UPDATE accounts
SET balance = balance + $2 - $3
WHERE id = $1
RETURNING balance;

-- name: ClaimIdempotencyKey :one
INSERT INTO idempotency_keys (key, request_checksum, status)
VALUES ($1, $2, 'processing')
ON CONFLICT (key) DO UPDATE
SET updated_at = NOW()
RETURNING transaction_id, request_checksum, status;

-- name: CompleteIdempotencyKey :exec
UPDATE idempotency_keys
SET transaction_id = $2, status = 'completed', updated_at = NOW()
WHERE key = $1;

-- name: GetEntriesByTransactionID :many
SELECT id, account_id, transaction_id, credit, debit, created_at
FROM entries
WHERE transaction_id = $1;
