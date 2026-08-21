ALTER TABLE outbox_events
    DROP CONSTRAINT outbox_events_status_check;

ALTER TABLE outbox_events
    ADD CONSTRAINT outbox_events_status_check
    CHECK (status IN ('pending', 'processing', 'processed')),
    ADD COLUMN lease_owner UUID,
    ADD COLUMN lease_expires_at TIMESTAMPTZ;

CREATE INDEX outbox_transaction_lease_expiry_idx
    ON outbox_events (lease_expires_at)
    WHERE status = 'processing' AND event_type = 'transaction_posted';
