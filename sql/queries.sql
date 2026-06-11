-- name: CreateAccount :one
INSERT INTO accounts (name, type) VALUES ($1, $2) RETURNING id;

-- name: CreateTransaction :one
INSERT INTO transactions DEFAULT VALUES RETURNING id;

-- name: CreateEntry :one
-- Ensure to call UpdateAccountBalance after this to maintain consistency
INSERT INTO entries (account_id, transaction_id, credit, debit) VALUES ($1, $2, $3, $4) returning id;

-- name: GetAccountBalance :one
SELECT CASE
    WHEN type IN ('asset', 'expense') THEN accounts.balance
    ELSE accounts.balance * -1
END AS balance
FROM accounts
WHERE id = $1;

-- name: UpdateAccountBalance :one
UPDATE accounts
SET balance = balance + $2 - $3
WHERE id = $1
RETURNING balance;