-- 0163: email_outbox gains its billing-axis anchor (ADR-104, unified
-- invoice timeline).
--
-- An email row was the ONE invoice-timeline fact with no simulated
-- timestamp: enqueue stamped SQL now() and nothing else, which is why
-- simulated invoices needed a second "Real-time activity" lane — an
-- email could only sort by its wall instant, weeks away from the story
-- that caused it. These two columns are the write-time record of WHICH
-- billing instant caused the send:
--
--   sim_effective_at — the clock's frozen instant at enqueue, read from
--       the bound ctx (clock.SimOf). NULL when the enqueue ran unbound,
--       i.e. the causing entity is not clock-pinned: for live-mode mail
--       the two calendars coincide and there is nothing to record.
--   test_clock_id — which clock, so ADR-086 teardown can sweep a
--       deleted clock's emails (a simulation must be fully disposable).
--
-- Write-time only, mirroring invoice_charge_attempts (0162): a NULL is
-- a fact ("no clock was bound"), never backfilled — inferring an anchor
-- at read time is the heuristic the no-heuristic-proxies rule bans.
-- Both halves must be present together (clock.Sim.Simulated() treats a
-- partial pair as absent); the CHECK enforces that invariant at rest.
ALTER TABLE email_outbox ADD COLUMN sim_effective_at TIMESTAMPTZ;
ALTER TABLE email_outbox ADD COLUMN test_clock_id TEXT;
ALTER TABLE email_outbox ADD CONSTRAINT email_outbox_sim_anchor_pair_check
    CHECK ((sim_effective_at IS NULL) = (test_clock_id IS NULL));

-- Teardown sweep path: ADR-086 deletes by clock id.
CREATE INDEX idx_email_outbox_test_clock ON email_outbox (test_clock_id)
    WHERE test_clock_id IS NOT NULL;
