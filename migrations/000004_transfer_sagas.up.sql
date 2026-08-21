-- Ledger boundaries are explicit. Existing rows stay in the legacy default
-- ledger so this migration is safe for an already-populated service.
ALTER TABLE accounts ADD COLUMN ledger_id UUID NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000';
ALTER TABLE transactions ADD COLUMN ledger_id UUID NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000';
CREATE INDEX accounts_ledger_id_idx ON accounts (ledger_id);
CREATE UNIQUE INDEX accounts_ledger_name_unique ON accounts (ledger_id, name);
CREATE INDEX transactions_ledger_id_idx ON transactions (ledger_id);

CREATE TABLE sagas (
    id UUID PRIMARY KEY,
    idempotency_key TEXT NOT NULL UNIQUE,
    source_ledger_id UUID NOT NULL,
    source_account_id UUID NOT NULL REFERENCES accounts(id),
    destination_ledger_id UUID NOT NULL,
    destination_account_id UUID NOT NULL REFERENCES accounts(id),
    amount NUMERIC(20,2) NOT NULL CHECK (amount > 0),
    status TEXT NOT NULL CHECK (status IN ('pending', 'completed', 'compensated', 'failed')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE saga_steps (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    saga_id UUID NOT NULL REFERENCES sagas(id),
    step TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('pending', 'completed', 'failed')),
    attempt_count INTEGER NOT NULL DEFAULT 0,
    error TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (saga_id, step)
);

CREATE TABLE outbox_events (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    saga_id UUID NOT NULL REFERENCES sagas(id),
    event_type TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'processed')),
    attempt_count INTEGER NOT NULL DEFAULT 0,
    available_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    processed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (saga_id, event_type)
);
CREATE INDEX outbox_events_ready_idx ON outbox_events (status, available_at, created_at);
