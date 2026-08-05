-- Recreates the single `final_action` column dropped by the up.
--
-- This down is LOSSY BY CONSTRUCTION and cannot be otherwise: the up's
-- whole point is that the two axes express combinations the single enum
-- has no value for. (cancel, mark_uncollectible) — Stripe's default
-- pairing — must collapse to one of its two halves.
--
-- Precedence: THE SUBSCRIPTION ACTION WINS.
--
--   (cancel, *)                 → cancel_subscription
--   (pause,  *)                 → pause
--   (none,   mark_uncollectible)→ mark_uncollectible
--   (none,   none)              → manual_review
--
-- Chosen on which direction of loss is dangerous. A down migration rolls
-- code back too, and the rolled-back code can only fire one action. Losing
-- the subscription half means a policy whose operator asked to STOP billing
-- keeps billing — the machine creates money events nobody authorized.
-- Losing the invoice half means a debt the operator meant to write off stays
-- open on the books — visible, inert, and reversible by hand. Leave the debt
-- open; never keep charging.

ALTER TABLE dunning_policies
  ADD COLUMN final_action TEXT NOT NULL DEFAULT 'manual_review';

UPDATE dunning_policies SET final_action = 'mark_uncollectible'
  WHERE final_invoice_action = 'mark_uncollectible';
UPDATE dunning_policies SET final_action = 'pause'
  WHERE final_subscription_action = 'pause';
UPDATE dunning_policies SET final_action = 'cancel_subscription'
  WHERE final_subscription_action = 'cancel';

ALTER TABLE dunning_policies
  ADD CONSTRAINT dunning_policies_final_action_check
  CHECK (final_action = ANY (ARRAY[
    'manual_review'::text,
    'pause'::text,
    'mark_uncollectible'::text,
    'cancel_subscription'::text
  ]));

-- Restore 0071's default.
ALTER TABLE dunning_policies ALTER COLUMN final_action SET DEFAULT 'pause';

ALTER TABLE dunning_policies
  DROP COLUMN final_subscription_action,
  DROP COLUMN final_invoice_action;
