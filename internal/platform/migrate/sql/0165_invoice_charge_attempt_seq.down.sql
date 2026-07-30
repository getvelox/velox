-- Reverting drops the idempotency-key seed; the key falls back to
-- updated_at, restoring the drift documented in the up migration.
ALTER TABLE invoices DROP COLUMN IF EXISTS charge_attempt_seq;
