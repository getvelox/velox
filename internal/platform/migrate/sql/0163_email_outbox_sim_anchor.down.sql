DROP INDEX IF EXISTS idx_email_outbox_test_clock;
ALTER TABLE email_outbox DROP CONSTRAINT IF EXISTS email_outbox_sim_anchor_pair_check;
ALTER TABLE email_outbox DROP COLUMN IF EXISTS test_clock_id;
ALTER TABLE email_outbox DROP COLUMN IF EXISTS sim_effective_at;
