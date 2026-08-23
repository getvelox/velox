# Pricing Recipes — Technical Design

> **Status:** Shipped — see [`CHANGELOG.md`](../CHANGELOG.md) for the merged commits and the operator-facing surface in `web-v2/src/pages/Recipes.tsx`.
> **Last revised:** 2026-04-25 (original); amended 2026-05-14 (Phase 2 wedge-alignment trim); amended 2026-07-08 (ADR-085 — idempotent apply, no uninstall)
> **Related:** `docs/design-multi-dim-meters.md` (multi-dim meter dependency), ADR-002 (per-domain), ADR-003 (RLS), ADR-085 (idempotent apply — supersedes the uninstall/force/conformance-gate shape below), ADR-083 (superseded), ADR-070 (rule adoption), ADR-044 (shared `tokens` meter)
>
> The text below is preserved as the design-time RFC. The implementation is live in `main`; refer to the package-level docs in `internal/recipe/` for the current behaviour.
>
> **2026-05-14 amendment:** the original "five built-ins" plan was trimmed to three AI-native built-ins (`anthropic_style`, `openai_style`, `replicate_style`). `b2b_saas_pro` and `marketplace_gmv` were dropped — generic SaaS / marketplace shapes diluted the AI-native wedge pitch when prospects browsed the Recipes page. The technical mechanism (one-call instantiate, preview, atomic rollback, per-tenant idempotency) is unchanged. So every "five built-ins" / `b2b_saas_pro` / `marketplace_gmv` reference in the RFC body below is dated — only the three AI-native recipes ship.
>
> **2026-06-01 amendment (ADR-044/045):** the token dimension shown throughout the body as `operation` + boolean `cached` was replaced by a single `token_type` enum (`input`, `output`, `cache_read`, `cache_write_5m`, `cache_write_1h`) — a boolean can't express the three distinct Anthropic cache prices. So the live `anthropic_style`/`openai_style` recipes carry ~20/14 pricing rules (2026-08-23: the live catalogs have since grown to 95/81) over `{model, token_type}` at equal priority (not the "9 rules, operation × cached" the body describes), and per-unit rates are decimals (ADR-045). See ADR-044/045 for the canonical shape; the body examples below use the dated `operation`/`cached` form.
>
> **2026-07-08 amendment (ADR-085 — supersedes ADR-083, read this first):** a recipe is an **idempotent, additive provisioning EVENT**, not an owned resource — one verb, `POST /v1/recipes/{key}/instantiate` ("apply"). The sections below describe an earlier shape that no longer matches the live code; the paragraphs that now **lie** are:
>
> - **"Uninstall" (`DELETE /v1/recipes/instances/{id}`, the API surface section, the Track A checklist, the "uninstall" open question) — removed entirely, no replacement route.** A recipe's objects are owned by the operator the instant they exist (plans carry live subscriptions under `ON DELETE RESTRICT` with no hard-delete; the catalog — rating rules, the shared `tokens` meter — is shared reference data). There is nothing an uninstall could safely retract; the design doc's own "no cascade... this is intentional" line was already saying so. To retire a generated plan, archive it via the existing plan-domain verb. The `recipe_instances` badge is now a **permanent** record — it is never deleted.
> - **`force=true` (Goals, the API surface's `POST /instantiate` body, Open question 3) — removed.** Once a plan is never adopted (below), there's nothing to force through: a code collision mints a new plan (`ai_api_pro_2`, …) instead of colliding.
> - **`seed_sample_data` / sample-data scaffolding (Goals, the YAML's `sample_data` block, Open question 2) — dead.** It shipped fully parsed and validated but was never wired to a writer, and stays that way; the demo-seeding need it targeted is deferred to a real design (`feedback_no_seed_data_shortcut`: empty-and-educate or a dedicated demo tenant, not a flag on the provisioning call).
> - **Reuse/adoption-by-code + the conformance gate it needed (ADR-083, `docs/adr/083-recipe-adoption-conformance-gate.md`) — dissolved, not just gated.** `Instantiate` **never adopts an existing plan by code.** Every apply mints a fresh, born-unique plan (`freePlanCode`: the desired code if free, else `<code>_2`, `<code>_3`, … — the incumbent is never touched). There is no more "does this existing plan match the recipe?" question, so there's no conformance diff to maintain. Rating rules and the `tokens` meter still **adopt** by natural key (that part is unchanged and is load-bearing, ADR-070/ADR-044) — meters gate on aggregation-match only (a mismatch is a loud 409; 2026-08-23: rule adoption has since gained a money-field conformance gate — mode, currency, rate scalars, and tiers refuse divergence with a 409, added 2026-07-26 in `internal/recipe/conformance.go`), rules are adopted at their current version and never republished (adopting a rule is not a price change; publishing a new version would reprice every live sub at next period open).
> - **Idempotency ("Idempotency at the recipe-instance level" in Goals) is unchanged in intent but now unconditional:** re-applying an already-installed recipe (the badge exists) is **always** a no-op returning the existing instance — `201`, same `id`, same `created_objects`. There is no `force`-gated alternate path anymore because there's no `force`.
>
> Everything else — preview being pure in-memory, one-tx atomicity, the YAML recipe format, per-tenant RLS isolation, the three shipped built-ins — is unchanged. See `docs/adr/085-recipe-idempotent-apply.md` for the full decision record, money-safety argument, and deferred items (additive update-execution against a newer template version is the real repeat use case "uninstall" was standing in for — not built yet, named trigger in the ADR).

## Motivation

Stripe Billing's onboarding for an AI-native product is a multi-day chore: you create N Meters by hand (one per `model × operation × cached` combination), wire each into a Plan, attach a webhook, configure dunning retry logic (the automated retries after a payment fails), then mock the whole thing for QA. The first invoice is a week away. Buyers leave.

Velox's AI-native wedge — its differentiating, AI-first pitch — includes **pricing recipes**: a single API call, ~30 seconds, walks away with a working `anthropic_style` setup — plans, meters, multi-dim (multi-dimensional) pricing rules, dunning, webhook endpoint, sample data. The picker UI in the dashboard means a non-technical CS rep can do the same thing.

This is the developer-experience flagship that turns the AI-native pillar from a slide into a 5-minute quickstart. Without it, the multi-dim meter machinery is invisible: no prospect builds a 12-rule `anthropic_style` setup by hand to evaluate it. With recipes, evaluation collapses to one POST.

This design depends on multi-dim meters being merged.

## Goals

- **One-call instantiation.** `POST /v1/recipes/instantiate` atomically creates the full surface area for a given recipe key (plans, meters, multi-dim pricing rules, dunning policy, webhook endpoint placeholder, optional sample data). All-or-nothing transaction.
- **Five built-ins shipping in v1:** `anthropic_style`, `openai_style`, `replicate_style`, `b2b_saas_pro`, `marketplace_gmv`. Each represents a real-world pricing pattern Velox-class buyers actually use.
- **Preview before commit.** `POST /v1/recipes/{key}/preview` returns the full graph of objects that *would* be created, with no DB writes. Powers the "review and instantiate" dialog in the dashboard.
- **Embedded YAML, no DB recipe table.** Recipes are versioned with the binary (`internal/recipe/recipes/*.yaml`, compiled in via Go's `embed.FS`, the standard library's file-embedding mechanism). Tenants don't author them in v1; built-ins only.
- **Per-instantiation overrides.** Currency, customer-facing names, base prices accept overrides at instantiate time. Changes must round-trip through preview.
- **Idempotency at the recipe-instance level.** Re-running a recipe by `(tenant_id, recipe_key)` returns the existing instance, no duplicates. Forced re-instantiation is a separate `force=true` flag for ops use.
- **Dashboard UI: pick → preview → instantiate.** Track B (the frontend workstream) owns the React surface; this design IS the API contract Track B mocks against.
- **Documented at `/docs/recipes` with copy-pasteable curl.** Each recipe gets a one-pager describing the resulting object graph in plain English.

## Non-goals (deferred)

- **Tenant-authored recipes.** Defer until a paying tenant asks. The maintenance cost — a tenant-recipe registry, validation, version migration, and a security review (a recipe is arbitrary infra-as-code) — is steep and unwarranted before product/market fit.
- **Recipe upgrades / migrations across versions.** A recipe is instantiated once. If Velox ships `anthropic_style` v2, instantiating it on an existing tenant either no-ops (idempotency match by key) or overwrites (force flag). No "upgrade my v1 instance to v2" path. Defer until the API stabilizes.
- **Marketplace / shared recipes from third parties.** Phase 4+. Security model alone (signing, sandboxing) is its own design.
- **Cross-tenant recipe templates.** Each tenant gets its own copy. No shared plan catalog across tenants by design — RLS (Row-Level Security: Postgres itself filters every tenant-scoped query down to the current tenant's rows) isolation is the whole point.
- **Recipes that span Stripe configuration** (tax codes per region, payment method preferences). Defer; the tenant's own `tenantstripe` (per-tenant Stripe configuration) settings already cover this.

## Today's surface (in repo)

- `internal/pricing.Service.CreateMeter`, `CreateRatingRule`, `CreatePlan` — narrow per-domain creates, no atomic-graph helper exists yet.
- `internal/dunning.Service.UpsertPolicy` — per-tenant retry schedule.
- `internal/webhook.Service.CreateEndpoint` — registers a webhook URL, returns the signing secret.
- `internal/usage.Store.UpsertPricingRule` — multi-dim rule per meter.
- No coordinator. Today, building `anthropic_style` by hand is ~30 HTTP calls with manual ID-passing between them. Recipes collapse this to one.

## Proposed schema changes — migration `00XX_recipe_instances`

> **Migration number caveat:** pick at PR-open time from `origin/main`, not local branch (per memory `feedback_migration_numbering`). Placeholder used here.

```sql
-- 00XX_recipe_instances.up.sql

-- Tracks which recipes have been instantiated for which tenant. The
-- objects themselves live in their normal tables (plans, meters,
-- pricing rules, etc.); this table is a thin index that lets us:
--   (a) idempotency-check on (tenant_id, recipe_key)
--   (b) show the dashboard "anthropic_style instantiated 3 days ago"
--   (c) clean up an instance via cascade if force-re-instantiated
CREATE TABLE recipe_instances (
    id                TEXT PRIMARY KEY DEFAULT 'vlx_rec_' || encode(gen_random_bytes(12), 'hex'),
    tenant_id         TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    recipe_key        TEXT NOT NULL,         -- e.g. 'anthropic_style'
    recipe_version    TEXT NOT NULL,         -- e.g. '1.0.0' (from YAML frontmatter)
    overrides         JSONB NOT NULL DEFAULT '{}',  -- redacted operator-supplied overrides
    created_object_ids JSONB NOT NULL DEFAULT '{}', -- map of role → entity ID for cleanup
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by        TEXT,                  -- operator user ID or "api"
    UNIQUE (tenant_id, recipe_key)            -- idempotency anchor
);

CREATE INDEX idx_recipe_instances_tenant
    ON recipe_instances (tenant_id, created_at DESC);

ALTER TABLE recipe_instances ENABLE ROW LEVEL SECURITY;
CREATE POLICY recipe_instances_tenant_isolation ON recipe_instances
    USING (tenant_id = current_setting('app.tenant_id', true)
        OR current_setting('app.bypass_rls', true) = 'true');

GRANT ALL ON TABLE recipe_instances TO velox_app;
```

Storing only the index and the `created_object_ids` map keeps the design boring. The canonical entities live in their own tables and obey their own constraints. If a tenant deletes a meter that came from a recipe, the recipe instance is unaffected — this is intentional. Cascading recipe deletion is a `force=true` operator flow, not a default.

## API surface

> **Wire-contract conventions** (consistent with `/v1/*`):
>
> - **Paths use hyphens:** `/v1/recipes`, `/v1/recipes/{key}/preview`, `/v1/recipes/instantiate`.
> - **Recipe identity is the `key` string** (e.g. `anthropic_style`), not an opaque ID. Keys are stable across versions; `version` field surfaces the underlying YAML revision.
> - **Overrides are a flat JSON object** (`{"currency": "EUR", "plan_name": "Inference Pro"}`), keys defined per recipe. Unknown override keys for a given recipe → 400.

### `GET /v1/recipes` — list available

```http
GET /v1/recipes
Authorization: Bearer <secret_key>
```

Response `200`:
```json
{
  "data": [
    {
      "key": "anthropic_style",
      "version": "1.0.0",
      "name": "Anthropic-style AI inference",
      "summary": "Per-token billing across model × operation × cached. 9 pricing rules, 1 multi-dim meter, monthly billing.",
      "creates": {
        "meters": 1,
        "pricing_rules": 9,
        "plans": 1,
        "rating_rules": 9,
        "dunning_policies": 1,
        "webhook_endpoints": 1
      },
      "overridable": ["currency", "plan_name", "plan_code"],
      "instantiated": {
        "id": "vlx_rec_abc",
        "instantiated_at": "2026-04-22T10:00:00Z"
      }
    },
    { "key": "openai_style", "...": "..." },
    { "key": "replicate_style", "...": "..." },
    { "key": "b2b_saas_pro", "...": "..." },
    { "key": "marketplace_gmv", "...": "..." }
  ]
}
```

`instantiated` is `null` (or omitted) for recipes the tenant has not yet instantiated. The `creates` summary lets the picker UI render "12 pricing rules · 1 meter · monthly" without a separate preview call.

### `GET /v1/recipes/{key}` — recipe detail

```http
GET /v1/recipes/anthropic_style
Authorization: Bearer <secret_key>
```

Response `200`: same shape as one entry from `GET /v1/recipes`, plus a long-form `description` (markdown) and the full `overridable_schema` (per-key type + default + bounds).

### `POST /v1/recipes/{key}/preview` — dry-run

```http
POST /v1/recipes/anthropic_style/preview
Authorization: Bearer <secret_key>

{
  "overrides": {
    "currency": "USD",
    "plan_name": "AI API"
  }
}
```

Response `200`:
```json
{
  "key": "anthropic_style",
  "version": "1.0.0",
  "objects": {
    "meters": [{ "key": "tokens", "name": "Tokens", "unit": "tokens", "aggregation": "sum" }],
    "rating_rules": [
      { "rule_key": "gpt4_input_uncached", "mode": "flat", "currency": "USD", "flat_amount_cents": 30 },
      { "rule_key": "gpt4_input_cached",   "mode": "flat", "currency": "USD", "flat_amount_cents": 3  },
      "..."
    ],
    "pricing_rules": [
      {
        "meter_key": "tokens",
        "rating_rule_key": "gpt4_input_uncached",
        "dimension_match": { "model": "gpt-4", "operation": "input", "cached": false },
        "aggregation_mode": "sum",
        "priority": 100
      },
      "..."
    ],
    "plans": [{ "code": "ai_api_pro", "name": "AI API", "currency": "USD", "billing_interval": "monthly", "base_amount_cents": 0, "meter_keys": ["tokens"] }],
    "dunning_policies": [{ "name": "AI default retry", "max_retries": 4, "intervals_hours": [24, 72, 168, 336] }],
    "webhook_endpoints": [{ "url": "https://example.com/webhooks/velox", "events": ["invoice.finalized", "invoice.paid", "subscription.updated"], "_placeholder": true }]
  },
  "warnings": []
}
```

`warnings` surfaces non-fatal conditions ("currency override `EUR` requires Stripe account in EUR mode", "webhook URL is a placeholder — set a real URL post-instantiate"). The dashboard renders these inline.

### `POST /v1/recipes/instantiate` — commit

```http
POST /v1/recipes/instantiate
Authorization: Bearer <secret_key>
Idempotency-Key: <key>

{
  "key": "anthropic_style",
  "overrides": { "currency": "USD", "plan_name": "AI API" },
  "seed_sample_data": false,
  "force": false
}
```

Response `201`:
```json
{
  "id": "vlx_rec_abc",
  "key": "anthropic_style",
  "version": "1.0.0",
  "tenant_id": "vlx_ten_...",
  "created_at": "2026-04-25T12:34:56Z",
  "created_objects": {
    "meter_ids": ["vlx_mtr_..."],
    "rating_rule_ids": ["vlx_rrv_..."],
    "pricing_rule_ids": ["vlx_mpr_...", "..."],
    "plan_ids": ["vlx_pln_..."],
    "dunning_policy_id": "vlx_dpc_...",
    "webhook_endpoint_id": "vlx_whk_..."
  }
}
```

Response `409` if the recipe is already instantiated and `force=false`:
```json
{
  "error": "recipe_already_instantiated",
  "instance": { "id": "vlx_rec_abc", "created_at": "..." },
  "hint": "Pass force=true to delete the existing instance and re-instantiate."
}
```

`seed_sample_data=true` additionally creates one demo customer + one trialing subscription against the new plan, so the tenant immediately sees usage flow. Default false; the dashboard's onboarding wizard sets it true on first-time install.

`force=true` deletes the prior `recipe_instances` row plus all entities listed in its `created_object_ids` map (cascade-style cleanup), then runs the recipe fresh. Gated behind a separate operator-only API key scope (`platform`); secret keys cannot force.

### `DELETE /v1/recipes/instances/{id}` — uninstall

```http
DELETE /v1/recipes/instances/vlx_rec_abc
Authorization: Bearer <platform_key>
```

Removes all entities listed in `created_object_ids` (best-effort; foreign-key blocks return 409 with the list of dependent objects, e.g. "plan still has 3 active subscriptions"). Platform-key only. Manual recipe cleanup for tenants whose recipe choice didn't pan out.

## Recipe definition format

Recipes are YAML files in `internal/recipe/recipes/`, embedded into the binary via `embed.FS`. One file per recipe. Schema validated at boot — a malformed recipe panics at startup, not at first instantiation.

### `internal/recipe/recipes/anthropic_style.yaml` (excerpt)

```yaml
key: anthropic_style
version: 1.0.0
name: Anthropic-style AI inference
summary: |
  Per-token billing across model × operation × cached.
  12 pricing rules, 1 multi-dim meter, monthly billing.
description: |
  Models the public Anthropic pricing page as of 2026-04: 4 models
  (claude-3-opus, claude-3-sonnet, claude-3-haiku, claude-3.5-sonnet),
  2 operations (input, output), with cache discounts on input.
  ...

overridable:
  - key: currency
    type: string
    default: USD
    enum: [USD, EUR, GBP, INR]
  - key: plan_name
    type: string
    default: AI API
    max_length: 80
  - key: plan_code
    type: string
    default: ai_api_pro
    pattern: '^[a-z0-9_]+$'

meters:
  - key: tokens
    name: Tokens
    unit: tokens
    aggregation: sum  # default for unclaimed events; per-rule modes override

rating_rules:
  - key: claude35_input_uncached
    name: Claude 3.5 Sonnet input (uncached)
    mode: flat
    currency: '{{ .currency }}'
    flat_amount_cents: 30
  - key: claude35_input_cached
    mode: flat
    currency: '{{ .currency }}'
    flat_amount_cents: 3
  # ... 10 more

pricing_rules:
  - meter: tokens
    rating_rule: claude35_input_uncached
    dimension_match: { model: claude-3.5-sonnet, operation: input, cached: false }
    aggregation_mode: sum
    priority: 100
  - meter: tokens
    rating_rule: claude35_input_cached
    dimension_match: { model: claude-3.5-sonnet, operation: input, cached: true }
    aggregation_mode: sum
    priority: 100
  # ... 10 more

plans:
  - code: '{{ .plan_code }}'
    name: '{{ .plan_name }}'
    currency: '{{ .currency }}'
    billing_interval: monthly
    base_amount_cents: 0
    meters: [tokens]

dunning:
  policy:
    name: AI default retry
    max_retries: 4
    intervals_hours: [24, 72, 168, 336]
    # What exhausting all retries does — two independent decisions (ADR-112).
    final_subscription_action: pause   # none | pause | cancel
    final_invoice_action: none         # none | mark_uncollectible

webhook:
  events:
    - invoice.finalized
    - invoice.paid
    - invoice.payment_failed
    - subscription.updated
    - subscription.canceled
  url_placeholder: 'https://example.com/webhooks/velox'

sample_data:
  customer:
    external_id: demo-customer
    display_name: Demo Customer
    email: demo@example.com
  subscription:
    plan: '{{ .plan_code }}'
    trial_days: 14
```

### Templating

`{{ .currency }}` etc. are resolved against the merged override map (defaults + per-instantiate overrides) using Go's `text/template`. No conditionals, no functions, no loops in v1 — just substitution. Validated at YAML load time: every `{{ .X }}` must resolve to a declared `overridable` key.

### Versioning

`version` field is semver. The version is recorded on `recipe_instances.recipe_version` so an operator can see which iteration their tenant ran. Velox does not migrate existing instances when a recipe's YAML changes — the canonical entities live in their own tables and the operator owns them once they exist.

## Internals

### Package layout

New per-domain package: `internal/recipe/` with the canonical store/service/handler trio (data access, business logic, HTTP routes — the repo's standard per-domain layering).

```
internal/recipe/
    recipes/                  # embedded YAML
        anthropic_style.yaml
        openai_style.yaml
        replicate_style.yaml
        b2b_saas_pro.yaml
        marketplace_gmv.yaml
    embed.go                  # embed.FS + Load() at boot
    parse.go                  # YAML → domain.Recipe; validation
    template.go               # override merge + text/template render
    service.go                # Preview, Instantiate, Uninstall
    handler.go                # HTTP routes
    store.go                  # recipe_instances CRUD only
    postgres.go               # store impl
```

`Service` depends on:
- `pricing.Service` for plan + meter + rating rule creation
- `dunning.Service` for policy upsert
- `webhook.Service` for endpoint creation
- `usage.Service` for `MeterPricingRule` upsert
- `customer.Service` + `subscription.Service` for `seed_sample_data`

The dependencies inject as narrow interfaces — each declared by the recipe package itself, naming only the methods it calls — so the recipe package owns no cross-domain state. Same pattern as `internal/billing/engine`.

### Atomicity

`Instantiate` runs in a single `BeginTx(ctx, postgres.TxTenant, tenantID)` — one tenant-scoped database transaction (`TxTenant` sets the RLS tenant variable for the transaction's duration). All `Service.Create*` calls accept a `*sql.Tx` (already the convention via `ExecContext` on a transaction). Failure at any step rolls back the whole graph — no half-instantiated tenants.

```go
// service.go (sketch)
func (s *Service) Instantiate(ctx context.Context, tenantID, recipeKey string, overrides map[string]any, opts InstantiateOptions) (domain.RecipeInstance, error) {
    recipe, err := s.registry.Load(recipeKey)
    if err != nil { return domain.RecipeInstance{}, err }

    if err := recipe.ValidateOverrides(overrides); err != nil { return domain.RecipeInstance{}, err }

    rendered, err := recipe.Render(overrides)
    if err != nil { return domain.RecipeInstance{}, err }

    tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
    if err != nil { return domain.RecipeInstance{}, err }
    defer postgres.Rollback(tx)

    // Idempotency check first — avoids partial work if the row already exists.
    if existing, err := s.store.GetByKeyTx(ctx, tx, tenantID, recipeKey); err == nil && !opts.Force {
        return domain.RecipeInstance{}, errs.ErrAlreadyInstantiated
    }
    if opts.Force {
        if err := s.deleteInstanceTx(ctx, tx, tenantID, recipeKey); err != nil { return domain.RecipeInstance{}, err }
    }

    objs := domain.CreatedObjects{}

    // Create in dependency order. Each Create* takes the tx so the
    // graph is one atomic unit.
    // meters → rating_rules → pricing_rules → plans → dunning → webhook → sample_data
    // ... (~150 lines of similar)

    inst, err := s.store.CreateTx(ctx, tx, domain.RecipeInstance{
        TenantID: tenantID, RecipeKey: recipeKey, RecipeVersion: recipe.Version,
        Overrides: overrides, CreatedObjects: objs,
    })
    if err != nil { return domain.RecipeInstance{}, err }

    if err := tx.Commit(); err != nil { return domain.RecipeInstance{}, err }
    return inst, nil
}
```

Where today's `Service.Create*` methods don't accept a `*sql.Tx` (most don't), Week 3 work adds the `*Tx` variant alongside the existing method. Both delegate to the same SQL — no dual code paths.

### Preview (no-DB)

`Preview` reuses `recipe.Render(overrides)` and returns the in-memory graph. No DB writes, no `BeginTx`. Cheap; safe to call on every dialog open.

### Built-in recipes — content shape

The five v1 recipes are calibrated against real public pricing pages for the wedge-aligned customer profile:

| Recipe | Models / Pattern | Pricing rules | Use case |
|---|---|---|---|
| `anthropic_style` | claude-3.5-sonnet, claude-3-opus, claude-3-sonnet, claude-3-haiku × {input, output} × {cached, uncached} | 12 | LLM API resellers / inference platforms |
| `openai_style` | gpt-4-turbo, gpt-4o, gpt-3.5-turbo × {input, output, embedding} × {cached, uncached} | 14 | LLM API resellers / inference platforms |
| `replicate_style` | per-second GPU billing across {a100, a40, t4, cpu} with `last_during_period` for spot duration | 4 | GPU-time platforms |
| `b2b_saas_pro` | seat-based with `last_ever` aggregation + per-seat overage | 2 | Vanilla SaaS that needs Velox for compliance, not AI |
| `marketplace_gmv` | percentage-of-volume with min/max, monthly settle | 1 plus `count` for transaction count | Marketplaces using Velox without Connect |

Each recipe gets a one-pager at `/docs/recipes/{key}.md` (Track B's `/docs/recipes` content surface) with copy-pasteable curl, a screenshot of the resulting dashboard, and a pricing page comparison.

## Tests

### Unit tests

- YAML load + validate: each of the 5 built-ins parses cleanly at boot. Malformed test fixtures return clear errors.
- Override validation: unknown keys → 400; type mismatches → 400; defaults applied when omitted.
- Templating: `{{ .currency }}` substitutes; missing template variable returns a clear error at load time, not at render.

### Integration tests (real Postgres)

- `Instantiate(anthropic_style)` produces a tenant with: 1 meter, 9 rating rules, 9 pricing rules, 1 plan, 1 dunning policy, 1 webhook endpoint. Verify object counts via direct SQL.
- `Instantiate` twice without `force` → second call returns 409 with the existing instance ID.
- `Instantiate` twice with `force=true` → second call returns a fresh instance, the first instance's objects are gone.
- Atomicity: inject a failing `pricing.CreatePlan` mid-instantiation; verify zero objects exist post-rollback (no orphan meters / rules).
- RLS: instantiate `anthropic_style` for tenant A; tenant B's `GET /v1/recipes` shows `anthropic_style` with `instantiated: null`.
- `Preview` on tenant A produces an identical object graph to what `Instantiate` then creates (modulo the IDs).

### End-to-end test (against the running stack)

`cmd/velox-recipes-e2e/main.go`: instantiate `anthropic_style`, post 1000 usage events through the new meter, run the cycle scan, assert the resulting invoice line items match the recipe's expected line breakdown. Catches contract drift between this design and the multi-dim meter implementation.

## Migrations

- `00XX_recipe_instances` — new table per the schema section above.

## Decimal & numeric considerations

Recipes inherit the `NUMERIC(38, 12)` quantity type from `usage_events`; no recipe-specific changes. All amount fields are integer cents per ADR-005. Currency conversion is the operator's responsibility (Stripe handles the actual settlement).

## Performance

- `GET /v1/recipes` is a constant-time read of `embed.FS` + one indexed `recipe_instances` query. Cheap.
- `Preview` is pure in-memory render; no DB.
- `Instantiate` is a single transaction with ~30 INSERTs for the largest recipe (`openai_style`). Well under 500ms on dev hardware. No batching needed — instantiation is rare per tenant.
- Recipe registry loads once at boot; no per-request file I/O.

## Open questions

1. **Should `instantiate` accept inline overrides for `pricing_rules.dimension_match` keys?** Proposal: **no** for v1. Recipe authors choose dimension shapes; tenants override prices but not dimension structure. Revisit if a design partner wants `gpt-4-turbo` pricing on the `anthropic_style` recipe.
2. **Should `seed_sample_data` create a `test_clock`?** Proposal: **no**. Sample sub starts with `trial_days=14` real-time; ops can attach a clock manually if they want to fast-forward. Keeps `seed_sample_data` semantics simple.
3. **Should `force=true` cascade-delete subscriptions that reference the recipe's plan?** Proposal: **no**. 409 with the dependents list, force the operator to migrate or cancel subscriptions first. Auto-cascade on subscriptions risks losing real billing data.
4. **Where do recipe webhook endpoints get their real URL?** Proposal: created with `url_placeholder` from YAML (`https://example.com/webhooks/velox`), plus an `is_placeholder=true` flag. Dashboard surfaces a "set webhook URL" prompt next to the instantiated recipe. Endpoint stays inactive (no events dispatched) until URL is updated and `is_placeholder` cleared. **Open:** does inactive-endpoint surface need a new column on `webhook_endpoints`, or do we reuse `enabled=false`? Lean toward `enabled=false` — same observable behavior, no schema delta.
5. **Should preview be cacheable?** Proposal: **no for v1**. Reads happen at most a few times per tenant. Cache invalidation across YAML version bumps + override permutations is its own problem.
6. **Recipe ordering in `GET /v1/recipes` — is there a "featured" notion?** Proposal: **alphabetical, no featuring v1**. The picker UI (Track B) can apply its own ordering; backend doesn't pick favorites.
7. **Should built-in recipes ship with `webhook.events` set or empty?** Proposal: **set per recipe**. `anthropic_style` and `openai_style` get the AI-relevant events (`invoice.finalized`, `usage.threshold_crossed` once Week 5 ships); `b2b_saas_pro` gets the SaaS-classic ones (`subscription.created`, `subscription.canceled`). Concrete defaults beat empty-and-let-the-tenant-figure-it-out.

## Implementation checklist

- [x] Migration `0055_recipe_instances.{up,down}.sql`
- [x] `domain.RecipeInstance` struct, `domain.Recipe` (parsed YAML), `domain.CreatedObjects`
- [x] `internal/recipe/embed.go` — `embed.FS` and registry
- [x] `internal/recipe/parse.go` — YAML schema + validation
- [x] `internal/recipe/template.go` — override merge + `text/template` render
- [x] `internal/recipe/store.go` + `postgres.go` — `recipe_instances` CRUD
- [x] `internal/recipe/service.go` — `Preview`, `Instantiate`, `Uninstall`
- [x] HTTP handlers: `GET /v1/recipes`, `GET /v1/recipes/{key}`, `POST /v1/recipes/{key}/preview`, `POST /v1/recipes/instantiate`, `DELETE /v1/recipes/instances/{id}`
- [x] `*Tx` variants on `pricing.Service`, `dunning.Service`, `webhook.Service`, `usage.Service` for transactional graph creation
- [x] 5 built-in recipes in `internal/recipe/recipes/*.yaml`
- [x] Per-recipe one-pagers in `docs/recipes/*.md` rendered at `/docs/recipes`
- [x] Unit tests: YAML parse, override validation, template render
- [x] Integration tests: instantiate / preview / force / RLS / atomicity rollback
- [x] End-to-end coverage — delivered as MANUAL_TEST FLOW S2 (recipe instantiate → in_advance plan → LiteLLM ingest → hybrid cycle invoice → public cost dashboard) plus the integration tests above; the `cmd/velox-recipes-e2e` binary this line originally named was never built (no commit ever contained it)
- [x] OpenAPI spec update (`api/openapi.yaml`)
- [x] CHANGELOG entry + public changelog rollup

## Track B unblock

Track B can scaffold the recipe-picker UI against this design today. The contract Track B should mock:

- `GET /v1/recipes` → list with `key`, `name`, `summary`, `creates`, `overridable`, `instantiated`
- `GET /v1/recipes/{key}` → detail with markdown `description`
- `POST /v1/recipes/{key}/preview` body `{ overrides: {} }` → object graph
- `POST /v1/recipes/instantiate` body `{ key, overrides, seed_sample_data, force }` → 201 with `id` + `created_objects`

The picker UI shape Track B should aim at:
- Card grid of recipes (hero name + summary + creates-count badges)
- Click → detail drawer with description + override form (per `overridable_schema`)
- "Preview" button → preview modal showing the object graph (collapsible per type)
- "Instantiate" button → confirmation → success state with deep-links into the created plan / meter / webhook detail pages

Track B can ship the UI against a mocked API (e.g. MSW — Mock Service Worker — handlers) before Track A finishes the backend, then swap to the real API at integration time. This is the design-first / RFC parallel-work pattern.

## Review status

- **Track A author:** drafted 2026-04-25
- **Track B review:** pending — Track B can scaffold the picker UI against this design without waiting for implementation
- **Human review:** pending — please flag any open question to resolve before Week 3 starts (May 9)
