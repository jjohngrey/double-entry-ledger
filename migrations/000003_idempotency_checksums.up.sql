ALTER TABLE idempotency_keys
    ALTER COLUMN transaction_id DROP NOT NULL,
    ADD COLUMN request_checksum TEXT NOT NULL DEFAULT '',
    ADD COLUMN status TEXT NOT NULL DEFAULT 'completed',
    ADD COLUMN updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    ADD CONSTRAINT idempotency_keys_status_check CHECK (status IN ('processing', 'completed'));
