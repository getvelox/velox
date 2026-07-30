-- 0165: invoices gain charge_attempt_seq — the Stripe charge idempotency
-- key's seed, replacing updated_at.
--
-- The key must change EXACTLY when a charge attempt outcome was recorded,
-- and at no other time:
--
--   * a crash between the Stripe call and the outcome write must reuse the
--     key, so Stripe returns the in-flight PaymentIntent instead of creating
--     a second one (no double charge);
--   * a RECORDED failure must move the key, so a retry after the customer
--     fixes their card gets a fresh PaymentIntent instead of Stripe handing
--     back the declined one forever (no liveness sink).
--
-- updated_at was standing in for that. It answers a much broader question —
-- "did anything at all touch this row?" — so every unrelated writer became
-- an accidental participant in the payment protocol. tax_commit's
-- SetTaxTransaction stamp (which runs immediately BEFORE the auto-charge
-- sweep in the same scheduler tick), a credit application, or an operator
-- action landing in the crash window all re-seeded the key and produced a
-- SECOND PaymentIntent for an invoice already being charged.
--
-- Observed, not theorised — the pre-fix drift is in
-- internal/invoice/auto_charge_key_stability_integration_test.go (#677):
--   velox_inv_<id>_1785420781393005000 → velox_inv_<id>_1785420781397042000
--
-- The seq is bumped by exactly the three writes that stamp
-- stripe_payment_intent_id (UpdatePayment, MarkPaymentFailedReportingTransition,
-- markPaidReportingTransition). That coupling is CI-enforced by
-- TestChargeAttemptSeqBumpedByEveryPIStamp, so a fourth outcome writer added
-- later fails the build instead of silently stranding a declined PI.
--
-- Backfill: DEFAULT 0 on existing rows is correct and deliberate. Pre-migration
-- keys were seeded from updated_at, so no in-flight key can collide with the
-- new velox_inv_<id>_0 shape (a nanosecond timestamp is never 0), and Stripe
-- keys expire after 24h regardless.

ALTER TABLE invoices
    ADD COLUMN IF NOT EXISTS charge_attempt_seq BIGINT NOT NULL DEFAULT 0;

COMMENT ON COLUMN invoices.charge_attempt_seq IS
    'Seeds the Stripe charge idempotency key (velox_inv_<id>_<seq>). Bumped ONLY by writes that record a charge attempt outcome — i.e. every UPDATE that stamps stripe_payment_intent_id. Never bumped by the auto-charge lease, and never by unrelated writers.';
