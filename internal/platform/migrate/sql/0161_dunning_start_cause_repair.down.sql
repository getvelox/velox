-- Restore the pre-fix uniform reason. Lossy by design: the up repaired
-- a hardcoded value, so down returns to that hardcode.
UPDATE invoice_dunning_runs SET reason = 'payment_failed' WHERE reason = 'no_payment_method';
UPDATE invoice_dunning_events SET reason = 'payment_failed' WHERE event_type = 'dunning_started' AND reason = 'no_payment_method';
