DROP TABLE IF EXISTS outbox_events;
DROP TABLE IF EXISTS saga_steps;
DROP TABLE IF EXISTS sagas;
DROP INDEX IF EXISTS transactions_ledger_id_idx;
DROP INDEX IF EXISTS accounts_ledger_name_unique;
DROP INDEX IF EXISTS accounts_ledger_id_idx;
ALTER TABLE transactions DROP COLUMN IF EXISTS ledger_id;
ALTER TABLE accounts DROP COLUMN IF EXISTS ledger_id;
