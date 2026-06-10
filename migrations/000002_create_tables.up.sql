CREATE EXTENSION IF NOT EXISTS "pgcrypto";

CREATE TYPE account_type AS ENUM ('asset', 'liability', 'equity', 'revenue', 'expense');

CREATE TABLE accounts (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    name        TEXT        NOT NULL,
    type        account_type NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE transactions (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE entries (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id      UUID        NOT NULL REFERENCES accounts(id),
    transaction_id  UUID        NOT NULL REFERENCES transactions(id),
    credit          NUMERIC(20, 2) NOT NULL DEFAULT 0,
    debit           NUMERIC(20, 2) NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (credit >= 0),
    CHECK (debit >= 0),
    CHECK (credit = 0 OR debit = 0)
);

CREATE TABLE idempotency_keys (
    key             TEXT        PRIMARY KEY,
    transaction_id  UUID        NOT NULL REFERENCES transactions(id),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
