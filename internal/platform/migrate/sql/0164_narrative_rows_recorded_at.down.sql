ALTER TABLE invoice_dunning_events DROP COLUMN IF EXISTS recorded_at;
ALTER TABLE credit_notes DROP COLUMN IF EXISTS recorded_at;
