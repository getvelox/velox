-- Down: restore the mode-blind badge (0055 shape). If a tenant installed the
-- same recipe in BOTH modes after 0157, the two-column unique cannot be
-- restored without dropping one row — keep the earliest install per
-- (tenant, key), matching pre-0157 idempotency semantics (first install wins).
DROP TRIGGER IF EXISTS set_livemode ON recipe_instances;

DELETE FROM recipe_instances a
USING recipe_instances b
WHERE a.tenant_id = b.tenant_id
  AND a.recipe_key = b.recipe_key
  AND a.created_at > b.created_at;

ALTER TABLE recipe_instances
    DROP CONSTRAINT recipe_instances_tenant_livemode_key_key;
ALTER TABLE recipe_instances
    ADD CONSTRAINT recipe_instances_tenant_id_recipe_key_key
    UNIQUE (tenant_id, recipe_key);

ALTER TABLE recipe_instances
    DROP COLUMN livemode;

DROP POLICY IF EXISTS tenant_isolation ON recipe_instances;
CREATE POLICY tenant_isolation ON recipe_instances FOR ALL USING (
    current_setting('app.bypass_rls', true) = 'on'
    OR tenant_id = current_setting('app.tenant_id', true)
);
