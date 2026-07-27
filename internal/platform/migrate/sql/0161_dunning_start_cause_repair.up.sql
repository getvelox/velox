-- Dunning start-cause repair. StartDunning hardcoded reason =
-- 'payment_failed' on every run and dunning_started event — including
-- the card-less enrollments (EnrollStalledForDunning), where no charge
-- was ever attempted. The code now records the true cause at write
-- time ('payment_failed' | 'no_payment_method'); this repairs the
-- historical rows so the newly cause-aware timeline never renders
-- "Payment failed" for an invoice that was never charged.
--
-- Predicate: an invoice with NO payment intent and payment_status
-- still 'pending' provably never had a charge attempt (declines store
-- their failed PI; settled/failed invoices left 'pending'). Runs and
-- started events on such invoices were card-less enrollments.
UPDATE invoice_dunning_runs r
SET reason = 'no_payment_method'
FROM invoices i
WHERE i.id = r.invoice_id
  AND r.reason = 'payment_failed'
  AND COALESCE(i.stripe_payment_intent_id, '') = ''
  AND i.payment_status = 'pending';

UPDATE invoice_dunning_events e
SET reason = 'no_payment_method'
FROM invoices i
WHERE i.id = e.invoice_id
  AND e.event_type = 'dunning_started'
  AND e.reason = 'payment_failed'
  AND COALESCE(i.stripe_payment_intent_id, '') = ''
  AND i.payment_status = 'pending';
