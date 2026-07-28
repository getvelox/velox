-- ADR-102: invoice_charge_attempts — charge attempts as first-class facts.
--
-- A charge attempt is a billing fact, but it had no record of its own:
-- its timeline representation rode on the Stripe webhook row (wall-clock
-- axis) or the dunning-started row (an OPTIONAL subsystem's event). On a
-- simulated invoice with dunning disabled, both carriers are absent from
-- the billing axis and the Activity timeline goes silent about the
-- failure (found by the I5 dunning-disabled walk, 2026-07-28).
--
-- Every saved-PM charge now records one row here at PaymentIntent-create
-- time; settle paths (webhook, inline, reconciler) UPSERT the outcome by
-- PI as provider truth arrives. Rendering precedence per attempt —
-- exactly one row: dunning row → attempt row → stripe webhook row.
-- NO backfill: invoices predating this table keep their webhook-row
-- rendering (the precedence chain degrades gracefully).

CREATE TABLE invoice_charge_attempts (
    id          TEXT PRIMARY KEY DEFAULT 'vlx_ica_' || encode(gen_random_bytes(12), 'hex'),
    tenant_id   TEXT NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    livemode    BOOLEAN NOT NULL DEFAULT true,
    -- CASCADE: ADR-086 test-clock teardown hard-deletes simulated
    -- invoices; their attempt history goes with them.
    invoice_id  TEXT NOT NULL REFERENCES invoices(id) ON DELETE CASCADE,
    -- Empty when the PaymentIntent create itself failed (network /
    -- provider outage) — the attempt still happened and still renders.
    stripe_payment_intent_id TEXT NOT NULL DEFAULT '',
    -- Which writer attempted: 'auto_charge' (cycle close, auto-charge
    -- sweep, operator collect — the saved-PM chokepoint), 'dunning_retry'
    -- (same chokepoint, retry purpose), 'external' (settle-path row for
    -- a PI Velox didn't mint inline: hosted-checkout attempts).
    trigger_source TEXT NOT NULL CHECK (trigger_source IN ('auto_charge', 'dunning_retry', 'external')),
    -- 'pending' (PI created, awaiting settle), 'unknown' (ambiguous
    -- create error — reconciler resolves), 'failed', 'succeeded'.
    -- Only 'succeeded' is terminal: a 3DS PI can go failed → succeeded
    -- on the customer's second try within one checkout session.
    outcome     TEXT NOT NULL CHECK (outcome IN ('pending', 'unknown', 'failed', 'succeeded')),
    provider_reason TEXT NOT NULL DEFAULT '',
    amount_cents BIGINT NOT NULL DEFAULT 0,
    occurred_at TIMESTAMPTZ NOT NULL,
    -- The billing-axis instant for attempts made under a test-clock
    -- Advance (clock.SimOf at the charge chokepoint). NULL = the
    -- attempt is a wall-clock fact (real scheduler, hosted checkout).
    sim_effective_at TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- One attempt per PaymentIntent: settle paths UPDATE the row rather
-- than minting twins. Empty-PI rows (create-failed) stay insert-only.
CREATE UNIQUE INDEX invoice_charge_attempts_pi
    ON invoice_charge_attempts (tenant_id, stripe_payment_intent_id)
    WHERE stripe_payment_intent_id <> '';
CREATE INDEX invoice_charge_attempts_invoice
    ON invoice_charge_attempts (tenant_id, invoice_id);

ALTER TABLE invoice_charge_attempts ENABLE ROW LEVEL SECURITY;
ALTER TABLE invoice_charge_attempts FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON invoice_charge_attempts FOR ALL USING (
    current_setting('app.bypass_rls', true) = 'on'
    OR (tenant_id = current_setting('app.tenant_id', true)
        AND livemode = (current_setting('app.livemode', true) IS DISTINCT FROM 'off'))
);

GRANT ALL ON TABLE invoice_charge_attempts TO velox_app;

-- Mode-aware: livemode derives from the tx session like every other
-- mode-partitioned table — explicit INSERT values are overwritten so a
-- buggy producer can't poison a partition.
CREATE TRIGGER set_livemode
    BEFORE INSERT ON invoice_charge_attempts
    FOR EACH ROW EXECUTE FUNCTION set_livemode_from_session();
