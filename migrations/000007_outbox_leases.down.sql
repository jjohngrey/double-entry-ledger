DROP INDEX IF EXISTS outbox_transaction_lease_expiry_idx;

ALTER TABLE outbox_events
    DROP CONSTRAINT outbox_events_status_check,
    DROP COLUMN lease_owner,
    DROP COLUMN lease_expires_at;

ALTER TABLE outbox_events
    ADD CONSTRAINT outbox_events_status_check
    CHECK (status IN ('pending', 'processed'));
