-- Reverting removes the pre-call record of a charge attempt, restoring the
-- orphaned-PaymentIntent class ADR-106 closed. Indexes, policy and trigger drop
-- with the table.
DROP TABLE IF EXISTS charge_intents;
