-- Split `manually_resolved` into the outcome it actually describes.
--
-- One value was written for TWO different invoice outcomes — voided AND
-- uncollectible — by three writers: the operator's "Void invoice" resolution,
-- the mark-uncollectible handler, and the engine's terminal floor
-- (dunning/service.go). So `resolution` could not answer the one question it
-- exists to answer: which terminal state closed this run.
--
-- `invoice_not_collectible` already named the write-off exactly and was being
-- avoided at the mark-uncollectible call site on the written grounds that
-- passing it "would recurse via ResolveRun's cross-flow branch". That comment
-- was false: the call site uses ResolveByInvoice, which has no such branch —
-- only ResolveRun does. A comment born false forced a wrong value for months.
--
-- Backfill maps by the invoice's OWN status, never by guesswork:
--   invoice voided        -> invoice_voided
--   invoice uncollectible -> invoice_not_collectible
--   anything else         -> LEFT AS manually_resolved, deliberately.
--
-- That last case is real (1 row at authoring time, NIM-000243): a run resolved
-- while its invoice stayed `finalized`, because the invoice-side write is
-- best-effort and logged rather than rolled back. Neither successor is true of
-- it, so it keeps the value it was actually written with. `manually_resolved`
-- therefore stays legal in the CHECK as a LEGACY value that no writer emits —
-- dropping it would either delete history or force a fabricated outcome onto
-- rows whose outcome nobody knows.

ALTER TABLE invoice_dunning_runs
  DROP CONSTRAINT IF EXISTS invoice_dunning_runs_resolution_check;

UPDATE invoice_dunning_runs r
   SET resolution = 'invoice_voided'
  FROM invoices i
 WHERE i.id = r.invoice_id
   AND r.resolution = 'manually_resolved'
   AND i.status = 'voided';

UPDATE invoice_dunning_runs r
   SET resolution = 'invoice_not_collectible'
  FROM invoices i
 WHERE i.id = r.invoice_id
   AND r.resolution = 'manually_resolved'
   AND i.status = 'uncollectible';

ALTER TABLE invoice_dunning_runs
  ADD CONSTRAINT invoice_dunning_runs_resolution_check
  CHECK (resolution IS NULL OR resolution IN (
    'payment_recovered',
    'invoice_voided',
    'invoice_not_collectible',
    'retries_exhausted',
    'action_failed',
    -- LEGACY, pre-0170. No writer emits it; see the header.
    'manually_resolved'
  ));
