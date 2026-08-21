-- Duplicate idempotency lookups and transaction event publication both fetch
-- entries by transaction. PostgreSQL does not index foreign keys implicitly.
CREATE INDEX entries_transaction_id_idx ON entries (transaction_id);

-- Account history filters and orders on this exact prefix. The id tie-breaker
-- makes equal-timestamp pagination deterministic without another sort.
CREATE INDEX entries_account_created_at_id_idx ON entries (account_id, created_at, id);

-- Keep processed outbox history out of the hot ready queues. created_at first
-- satisfies worker order; available_at remains an inexpensive filter/include.
CREATE INDEX outbox_transaction_ready_partial_idx
    ON outbox_events (created_at, id)
    INCLUDE (transaction_id, available_at)
    WHERE status = 'pending' AND event_type = 'transaction_posted';

CREATE INDEX outbox_transfer_ready_partial_idx
    ON outbox_events (created_at, id)
    INCLUDE (saga_id, event_type, attempt_count, available_at)
    WHERE status = 'pending'
      AND event_type IN ('destination_credit', 'compensate_source');
