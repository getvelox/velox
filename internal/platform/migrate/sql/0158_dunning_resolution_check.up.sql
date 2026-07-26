-- invoice_dunning_runs.resolution had no CHECK constraint: the resolve
-- endpoint stored any free-text string verbatim (found live 2026-07-26 —
-- a typo'd "payment_received" landed in the column, skipped the
-- mark-paid propagation, and rendered as a raw slug in the dashboard).
-- The handler now 422s unknown operator input; this constraint is the
-- backstop for every writer (operator endpoint, engine final-actions,
-- background settle resolvers). NULL stays legal — unresolved runs.
ALTER TABLE invoice_dunning_runs
  ADD CONSTRAINT invoice_dunning_runs_resolution_check
  CHECK (resolution IS NULL OR resolution IN (
    'payment_recovered',
    'manually_resolved',
    'retries_exhausted',
    'invoice_not_collectible',
    'action_failed'
  ));
