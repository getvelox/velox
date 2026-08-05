-- ADR-112 — dunning's terminal outcome is TWO decisions, not one.
--
-- `final_action` was a single enum over two orthogonal axes: what happens
-- to the SUBSCRIPTION and what happens to the unpaid INVOICE. Every peer
-- that has a write-off concept configures both separately (verified
-- 2026-08-05, quoted in ADR-112): Stripe derives the invoice write-off
-- from the subscription setting, Recurly ships an invoice auto-fail flag
-- beside an expire-subscription flag, Chargebee states it outright —
-- "the action to be performed on the subscription ... and invoice".
--
-- Velox was the only one forcing a choice BETWEEN them, so Stripe's own
-- default outcome (cancel the subscription AND write off the invoice) was
-- inexpressible, and the three actions that are not 'mark_uncollectible'
-- left the debt finalized/failed/amount_due>0 with no closer anywhere in
-- the tree.
--
-- Backfill is behavior-preserving: every existing policy keeps doing
-- exactly what it does today. No policy starts writing off an invoice it
-- did not already write off.
--
--   manual_review        → (none,   none)
--   pause                → (pause,  none)
--   cancel_subscription  → (cancel, none)
--   mark_uncollectible   → (none,   mark_uncollectible)
--
-- 'manual_review' has no column of its own by design: it IS the
-- (none, none) corner, which is Chargebee's "remain active" + "not paid".

ALTER TABLE dunning_policies
  ADD COLUMN final_subscription_action TEXT NOT NULL DEFAULT 'none'
    CHECK (final_subscription_action IN ('none', 'pause', 'cancel')),
  ADD COLUMN final_invoice_action TEXT NOT NULL DEFAULT 'none'
    CHECK (final_invoice_action IN ('none', 'mark_uncollectible'));

UPDATE dunning_policies SET final_subscription_action = 'pause'  WHERE final_action = 'pause';
UPDATE dunning_policies SET final_subscription_action = 'cancel' WHERE final_action = 'cancel_subscription';
UPDATE dunning_policies SET final_invoice_action = 'mark_uncollectible' WHERE final_action = 'mark_uncollectible';

-- The default for NEW policies stays exactly what 0071 set: pause the
-- subscription, leave the invoice open. Deliberately NOT defaulted to
-- writing off, even though Recurly defaults its auto-fail on: a write-off
-- is an accounting assertion about bad debt, and a machine should not make
-- one on a fresh tenant's behalf unasked (same reasoning as ADR-111).
ALTER TABLE dunning_policies ALTER COLUMN final_subscription_action SET DEFAULT 'pause';

ALTER TABLE dunning_policies DROP CONSTRAINT IF EXISTS dunning_policies_final_action_check;
ALTER TABLE dunning_policies DROP COLUMN final_action;
