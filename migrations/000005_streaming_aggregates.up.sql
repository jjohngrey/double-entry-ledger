-- The saga outbox also carries committed ledger postings.  A posting is linked
-- to its immutable transaction; saga_id remains populated for saga commands.
ALTER TABLE outbox_events ALTER COLUMN saga_id DROP NOT NULL;
ALTER TABLE outbox_events ADD COLUMN transaction_id UUID REFERENCES transactions(id);
ALTER TABLE outbox_events ADD CONSTRAINT outbox_event_target CHECK (saga_id IS NOT NULL OR transaction_id IS NOT NULL);
CREATE UNIQUE INDEX outbox_transaction_event_unique
    ON outbox_events (transaction_id, event_type) WHERE transaction_id IS NOT NULL;

-- Consumer-local inbox.  The primary key makes applying an at-least-once
-- JetStream message atomic with its projection updates.
CREATE TABLE processed_events (
    consumer TEXT NOT NULL,
    event_id UUID NOT NULL,
    processed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (consumer, event_id)
);

CREATE TABLE daily_account_aggregates (
    account_id UUID NOT NULL REFERENCES accounts(id),
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
