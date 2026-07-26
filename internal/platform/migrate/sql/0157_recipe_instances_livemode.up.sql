-- recipe_instances was livemode-BLIND while every object a recipe provisions
-- (meters, plans, rating rules, dunning policies, webhook endpoints) is
-- livemode-partitioned (0020). The idempotency gate reads the badge FIRST
-- (service.Instantiate → GetByKeyTx), so a test-mode install silently
-- no-op'd the live-mode install of the same recipe: Live showed "Installed"
-- with ZERO live pricing objects. Found by the 2026-07-26 recipe e2e review.
--
-- Why the class fired here and nowhere else: a mode-blind tenant table is
-- safe only when its lookups ride FK-scoped mode-aware parents (customers,
-- meters, subscriptions inherit mode through the referenced PK). The badge
-- is keyed by a NATURAL key (recipe_key) with no mode-aware parent — the
-- one shape 0020's FK-inheritance argument doesn't cover.
--
-- Fix = the standard 0020/0137 shape: livemode column, mode-scoped UNIQUE,
-- mode-aware RLS predicate, session-stamped INSERT trigger. The Go store
-- needs no query changes — RLS filters reads, the trigger stamps writes.

ALTER TABLE recipe_instances
    ADD COLUMN livemode BOOLEAN NOT NULL DEFAULT true;

-- Backfill existing rows from EVIDENCE, not a guess: the badge records the
-- plan it provisioned, and plans carry livemode. Unresolvable rows (no plan
-- recorded, or the id no longer resolves) fall back to test mode — the safe
-- direction: worst case Live shows "Install" again and a re-install adopts
-- the catalog and uniquifies the plan code (loud clutter), whereas a wrong
-- live badge is exactly the silent lying-"Installed" this migration kills.
UPDATE recipe_instances ri
SET livemode = COALESCE(
    (SELECT p.livemode FROM plans p
      WHERE p.id = ri.created_object_ids->'plan_ids'->>0
        AND p.tenant_id = ri.tenant_id),
    false);

-- Same recipe installable once PER MODE.
ALTER TABLE recipe_instances
    DROP CONSTRAINT recipe_instances_tenant_id_recipe_key_key;
ALTER TABLE recipe_instances
    ADD CONSTRAINT recipe_instances_tenant_livemode_key_key
    UNIQUE (tenant_id, livemode, recipe_key);

-- Mode-aware tenant_isolation (0006/0020 shape).
DROP POLICY IF EXISTS tenant_isolation ON recipe_instances;
CREATE POLICY tenant_isolation ON recipe_instances FOR ALL USING (
    current_setting('app.bypass_rls', true) = 'on'
    OR (
        tenant_id = current_setting('app.tenant_id', true)
        AND livemode = (current_setting('app.livemode', true) IS DISTINCT FROM 'off')
    )
);

-- livemode is session-derived, never caller-supplied (0021 mechanism —
-- the trigger list there is hard-coded, so the new table wires its own).
CREATE TRIGGER set_livemode
    BEFORE INSERT ON recipe_instances
    FOR EACH ROW EXECUTE FUNCTION set_livemode_from_session();
