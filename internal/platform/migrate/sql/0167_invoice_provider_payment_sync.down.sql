-- Reverses 0167. Both columns are pure observations rebuilt by the next
-- reconciler sweep, so dropping them loses no money fact — but it DOES restore
-- the liveness defect they exist to fix: with provider_synced_at gone the sweep
-- falls back to ORDER BY updated_at ASC, where a never-resolving row heads the
-- queue forever. Roll back only alongside the code that reads them.
DROP INDEX IF EXISTS idx_invoices_provider_sync_inflight;

ALTER TABLE invoices
  DROP COLUMN IF EXISTS provider_payment_status,
  DROP COLUMN IF EXISTS provider_synced_at;
