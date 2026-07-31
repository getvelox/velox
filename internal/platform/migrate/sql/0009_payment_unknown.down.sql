-- LOAD-BEARING FOR A MONEY INVARIANT ADDED LATER (ADR-107, 2026-07-31).
--
-- This rollback rewrites every payment_status='unknown' row across all tenants
-- and both livemodes in one statement. Since ADR-107 the no-double-charge
-- guarantee rests on 'unknown' being a state NO charge-claim predicate admits:
-- an invoice whose PaymentIntent could never be named is parked there
-- deliberately, uncollectible and loud, instead of being settled failed and made
-- claimable again.
--
-- Running this converts every one of those parked invoices into a claimable one
-- at once, and each is a candidate for a second charge against a PaymentIntent
-- that may still be live at Stripe. Before running it (velox migrate rollback),
-- reconcile every 'unknown' invoice by hand.

-- Clear any 'unknown' rows so the old CHECK reinstalls cleanly. In practice
-- this downgrade would only run in test/dev; 'unknown' invoices in prod
-- should be reconciled first.
UPDATE invoices SET payment_status = 'failed' WHERE payment_status = 'unknown';

ALTER TABLE invoices
    DROP CONSTRAINT invoices_payment_status_check;

ALTER TABLE invoices
    ADD CONSTRAINT invoices_payment_status_check
    CHECK (payment_status IN ('pending', 'processing', 'succeeded', 'failed'));
