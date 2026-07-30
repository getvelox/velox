-- ADR-106: charge_intents — the durable NAME of a charge attempt, written
-- BEFORE Stripe is called so recovery never depends on the response.
--
-- ADR-105 closed the "unrelated writer re-seeds the idempotency key" double
-- charge. One residual survived it: Stripe can create a PaymentIntent that
-- Velox never learns about — the process dies between the call and the outcome
-- write, or the call returns an ambiguous error carrying no PaymentIntent id.
-- The invoice then has no stripe_payment_intent_id, and payment.reconcileOne
-- cannot query what it cannot name: its empty-PI branch settles the invoice
-- FAILED, which bumps charge_attempt_seq, which frees the next retry to open a
-- SECOND PaymentIntent beside the live one. That is the double charge, reached
-- by a different door.
--
-- No key scheme can fix that, because the problem is not which key we send —
-- it is that we never recorded that we were about to send one. So we record it
-- first (playbook class E: record-before-effect + requeryable state).
--
-- RECOVERY IS IDEMPOTENT REPLAY, not a search. The row stores the EXACT
-- Idempotency-Key header and the exact request parameters, so the sweep
-- re-issues the identical request. Per Stripe's documented contract, "Stripe's
-- idempotency works by saving the resulting status code and body of the first
-- request made for any given idempotency key" and "subsequent requests with the
-- same key return the same result" — so the replay returns the ORIGINAL
-- PaymentIntent rather than making a second one. If Stripe saved nothing (the
-- request never began execution), the replay creates the one PaymentIntent we
-- always intended. Both outcomes are safe; there is no third.
--
-- Stripe retains keys for "at least 24 hours", so replay past that floor would
-- MINT a second PaymentIntent — exactly the bug. The sweep therefore refuses to
-- replay beyond a 12h TTL and quarantines the row instead.

CREATE TABLE charge_intents (
    id          TEXT PRIMARY KEY DEFAULT 'vlx_cin_' || encode(gen_random_bytes(12), 'hex'),
    tenant_id   TEXT NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    livemode    BOOLEAN NOT NULL DEFAULT true,
    -- CASCADE mirrors 0162: ADR-086 test-clock teardown hard-deletes simulated
    -- invoices, and an intent for a deleted invoice is unresolvable anyway.
    invoice_id  TEXT NOT NULL REFERENCES invoices(id) ON DELETE CASCADE,

    -- The EXACT header sent to Stripe, not a seed of it. Replay reproduces the
    -- request byte-for-byte from this row; a key we cannot reproduce is a
    -- PaymentIntent we cannot find.
    idempotency_key TEXT NOT NULL,
    amount_cents    BIGINT NOT NULL,
    currency        TEXT NOT NULL,
    stripe_customer_id       TEXT NOT NULL,
    stripe_payment_method_id TEXT NOT NULL,
    description     TEXT NOT NULL DEFAULT '',
    metadata        JSONB NOT NULL DEFAULT '{}'::jsonb,
    purpose         TEXT NOT NULL DEFAULT '',

    -- open         → a request may be in flight at Stripe; the invoice is
    --                quarantined from further charges until it resolves.
    -- resolved     → the PaymentIntent is named (or provably never created).
    -- needs_review → auto-recovery gave up; an operator must look. Deliberately
    --                NOT swept: it must keep blocking, because "we do not know"
    --                is precisely when a second charge must not be opened.
    state       TEXT NOT NULL DEFAULT 'open' CHECK (state IN ('open', 'resolved', 'needs_review')),
    stripe_payment_intent_id TEXT NOT NULL DEFAULT '',
    recovery_attempts INT NOT NULL DEFAULT 0,
    last_error  TEXT NOT NULL DEFAULT '',

    occurred_at TIMESTAMPTZ NOT NULL,
    -- The billing-axis instant when the attempt was made under a test-clock
    -- Advance, so a wall-plane recovery still stamps the contracted instant
    -- (ADR-030). NULL = a wall-clock attempt.
    sim_effective_at TIMESTAMPTZ,
    resolved_at TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Identity is Stripe's own dedup primitive. Two initiators computing the same
-- key converge on one row and send one key, so Stripe returns one PaymentIntent
-- to both.
CREATE UNIQUE INDEX charge_intents_key
    ON charge_intents (tenant_id, idempotency_key);

-- THE guard, and it is structural rather than advisory: while any attempt is
-- unresolved, a second one cannot be RECORDED, and since the pre-call write is
-- fail-closed, what cannot be recorded cannot be made.
--
-- The predicate is `<> 'resolved'`, NOT `= 'open'`. A needs_review row is the
-- case where we do not know whether a PaymentIntent is live — the last state in
-- which it would be safe to start another charge.
CREATE UNIQUE INDEX charge_intents_one_unresolved
    ON charge_intents (tenant_id, invoice_id)
    WHERE state <> 'resolved';

-- Partial so the sweep index stays small: resolved rows are the overwhelming
-- majority and needs_review rows are deliberately not auto-swept.
CREATE INDEX charge_intents_sweep
    ON charge_intents (livemode, updated_at)
    WHERE state = 'open';

-- An unresolved row that cannot be replayed is WORSE than no row: it looks
-- recoverable to the sweep and is unresolvable in fact. Refuse it at write time
-- rather than discovering it during an incident.
ALTER TABLE charge_intents ADD CONSTRAINT charge_intents_replayable CHECK (
    state = 'resolved'
    OR (idempotency_key <> ''
        AND stripe_customer_id <> ''
        AND stripe_payment_method_id <> ''
        AND currency <> ''
        AND amount_cents > 0)
);

ALTER TABLE charge_intents ENABLE ROW LEVEL SECURITY;
ALTER TABLE charge_intents FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON charge_intents FOR ALL USING (
    current_setting('app.bypass_rls', true) = 'on'
    OR (tenant_id = current_setting('app.tenant_id', true)
        AND livemode = (current_setting('app.livemode', true) IS DISTINCT FROM 'off'))
);

GRANT ALL ON TABLE charge_intents TO velox_app;

-- Mode-aware: livemode derives from the tx session like every other
-- mode-partitioned table — an explicit INSERT value is overwritten so a
-- caller cannot write across the partition.
CREATE TRIGGER set_livemode
    BEFORE INSERT ON charge_intents
    FOR EACH ROW EXECUTE FUNCTION set_livemode_from_session();
