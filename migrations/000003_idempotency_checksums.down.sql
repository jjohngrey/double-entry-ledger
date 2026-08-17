ALTER TABLE idempotency_keys
    DROP CONSTRAINT IF EXISTS idempotency_keys_status_check,
    DROP COLUMN IF EXISTS updated_at,
    DROP COLUMN IF EXISTS status,
    DROP COLUMN IF EXISTS request_checksum,
    ALTER COLUMN transaction_id SET NOT NULL;
