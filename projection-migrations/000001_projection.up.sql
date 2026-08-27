-- Projection storage is intentionally independent from the OLTP schema.
-- Event payloads carry account and ledger identifiers, so these tables do not
-- need cross-database foreign keys back to transactional accounts.
CREATE TABLE processed_events (
    consumer TEXT NOT NULL,
    event_id UUID NOT NULL,
    processed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (consumer, event_id)
);

CREATE TABLE daily_account_aggregates (
    account_id UUID NOT NULL,
    day DATE NOT NULL,
    debit NUMERIC(20,2) NOT NULL DEFAULT 0,
    credit NUMERIC(20,2) NOT NULL DEFAULT 0,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_event_id UUID NOT NULL,
    PRIMARY KEY (account_id, day)
);

CREATE TABLE daily_ledger_aggregates (
    ledger_id UUID NOT NULL,
    day DATE NOT NULL,
    debit NUMERIC(20,2) NOT NULL DEFAULT 0,
    credit NUMERIC(20,2) NOT NULL DEFAULT 0,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_event_id UUID NOT NULL,
    PRIMARY KEY (ledger_id, day)
);
