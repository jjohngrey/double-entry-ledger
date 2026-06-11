-- name: CreateAccount :one
INSERT INTO accounts (name, type) VALUES ($1, $2) RETURNING id;

-- name: CreateTransaction :one
INSERT INTO transactions DEFAULT VALUES RETURNING id;

-- name: CreateEntry :one
INSERT INTO entries (account_id, transaction_id, credit, debit) VALUES ($1, $2, $3, $4) RETURNING id;

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

-- name: GetIdempotencyKey :one
SELECT transaction_id FROM idempotency_keys WHERE key = $1;

-- name: CreateIdempotencyKey :exec
INSERT INTO idempotency_keys (key, transaction_id) VALUES ($1, $2);

-- name: GetEntriesByTransactionID :many
SELECT id, account_id, transaction_id, credit, debit, created_at
FROM entries
WHERE transaction_id = $1;
