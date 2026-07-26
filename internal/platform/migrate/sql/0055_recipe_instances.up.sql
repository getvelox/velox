-- Pricing recipes: thin index of which recipes have been instantiated for
-- which tenant.
--
-- The canonical entities a recipe creates (meters, rating rules,
-- pricing rules, plans, dunning policies, webhook endpoints) live in their
-- own per-domain tables and obey their own constraints. This table only
-- tracks instantiation metadata so we can:
--
--   1. Idempotency-check by (tenant_id, recipe_key) on POST /v1/recipes/{key}/instantiate.
--   2. Surface "anthropic_style instantiated 3 days ago" on the dashboard.
--   3. Record the created_object_ids map for the dashboard's install badge.
--
-- A tenant deleting a meter that came from a recipe does NOT cascade to
-- recipe_instances; recipes are an instantiation event, not an ownership
-- relationship. Re-instantiating is idempotent (a no-op if already
-- installed); to retire generated entities operators edit/archive them
-- directly (ADR-085: apply is idempotent additive — no force, no uninstall).
--
-- See docs/design-recipes.md for the full design and override semantics.

CREATE TABLE recipe_instances (
    id                  TEXT PRIMARY KEY DEFAULT 'vlx_rec_' || encode(gen_random_bytes(12), 'hex'),
    tenant_id           TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    recipe_key          TEXT NOT NULL,
    recipe_version      TEXT NOT NULL,
    overrides           JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_object_ids  JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by          TEXT,
    UNIQUE (tenant_id, recipe_key)
);

-- Hot read: dashboard "recipes installed for this tenant, newest first".
CREATE INDEX idx_recipe_instances_tenant
    ON recipe_instances (tenant_id, created_at DESC);

-- Standard tenant-isolation pattern (see 0054). FORCE applies the policy
-- even to the table owner so a misconfigured connection string can't
-- accidentally bypass it.
ALTER TABLE recipe_instances ENABLE ROW LEVEL SECURITY;
ALTER TABLE recipe_instances FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON recipe_instances FOR ALL USING (
    current_setting('app.bypass_rls', true) = 'on'
    OR tenant_id = current_setting('app.tenant_id', true)
);

GRANT ALL ON TABLE recipe_instances TO velox_app;
