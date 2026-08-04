-- Reverse the 0170 split: fold invoice_voided back into manually_resolved and
-- restore the 0158 constraint.
--
-- NOT symmetric, and it cannot be. The up-migration's write-off rows became
-- `invoice_not_collectible`, which was ALREADY legal before 0170 and is a
-- value the operator endpoint could always produce — so there is no way to
-- tell a row this migration converted from one an operator wrote directly.
-- Folding all of them back would rewrite rows 0170 never touched.
--
-- So down reverses only the unambiguous half (invoice_voided did not exist
-- before 0170, so every one of those rows is ours), and leaves
-- invoice_not_collectible alone. The consequence, stated plainly: after a
-- down-migration the write-off rows keep the more precise value, which the
-- restored CHECK still permits. Nothing is lost and nothing is fabricated.

UPDATE invoice_dunning_runs
   SET resolution = 'manually_resolved'
 WHERE resolution = 'invoice_voided';

ALTER TABLE invoice_dunning_runs
  DROP CONSTRAINT IF EXISTS invoice_dunning_runs_resolution_check;

ALTER TABLE invoice_dunning_runs
  ADD CONSTRAINT invoice_dunning_runs_resolution_check
  CHECK (resolution IS NULL OR resolution IN (
    'payment_recovered',
    'manually_resolved',
    'retries_exhausted',
    'invoice_not_collectible',
    'action_failed'
  ));
