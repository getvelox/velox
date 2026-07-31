-- ADR-107 follow-up: persist what the reconciler already learns.
--
-- The payment reconciler polls Stripe for every in-flight invoice on every
-- sweep and, for a NON-TERMINAL PaymentIntent status (processing /
-- requires_action / requires_confirmation / requires_capture), returned without
-- writing anything at all. Discarding that observation caused a liveness bug:
--
--   The sweep is ORDER BY updated_at ASC LIMIT n. A row nothing ever writes has
--   a frozen updated_at, so it sorts first forever; once batch-size of them
--   accumulate the sweep returns only those and never reaches a NEW ambiguous
--   charge that Stripe would resolve in one call.
--
-- That is not a hypothetical shape. internal/payment's own contract notes that
-- "async methods legitimately sit in processing for days" — those are exactly
-- the rows that froze, with no stuck 3-D Secure or exotic failure needed.
--
-- provider_payment_status is Stripe's verbatim PaymentIntent status;
-- provider_synced_at is when we last observed it. Both are OBSERVATIONS, never
-- derived and never authoritative over payment_status — the invoice's money
-- state stays the settle path's to write. Deliberately NOT a CHECK-constrained
-- enum: it mirrors an external vocabulary Stripe can extend without asking us,
-- and a CHECK here would turn a new provider status into a failed write.
--
-- No logic branches on provider_payment_status today. It is stored because it
-- costs nothing at the point we already have it, and it turns "this invoice is
-- mysteriously stuck" into "this invoice is stuck at requires_action" — the
-- difference between a silent row and a diagnosable one.
ALTER TABLE invoices
  ADD COLUMN provider_payment_status TEXT,
  ADD COLUMN provider_synced_at      TIMESTAMPTZ;

-- The sweep's new ordering key. NULLS FIRST puts never-polled rows (every row
-- that exists today, and every newly ambiguous charge) ahead of ones we already
-- looked at, and a polled row rotates to the back — so the queue round-robins
-- instead of jamming. Partial: only in-flight rows are ever ordered by it.
CREATE INDEX idx_invoices_provider_sync_inflight
  ON invoices (provider_synced_at NULLS FIRST, updated_at)
  WHERE payment_status IN ('processing', 'unknown');

COMMENT ON COLUMN invoices.provider_payment_status IS
  'Verbatim Stripe PaymentIntent status at the last reconciler poll. An observation, not a money fact — payment_status remains authoritative.';
COMMENT ON COLUMN invoices.provider_synced_at IS
  'When the reconciler last observed provider_payment_status. Orders the in-flight sweep so a never-resolving row cannot head the queue forever.';
