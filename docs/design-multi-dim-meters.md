# Multi-Dimensional Meters — Technical Design

> **Status:** Shipped 2026-04-25 — see [`CHANGELOG.md`](../CHANGELOG.md) for the merged commits.
> **Last revised:** 2026-04-25
> **Related:** ADR-002 (per-domain), ADR-003 (RLS), ADR-005 (integer cents)
>
> The text below is preserved as the design-time RFC. The implementation is live in `main`; refer to the package-level docs in `internal/usage/` and `internal/billing/` for the current behaviour.

> **Note (2026-06-01):** ADR-044 replaced the `operation` + boolean `cached` dimensions shown in examples below with a single `token_type` enum (input, output, cache_read, cache_write_5m, cache_write_1h). The mechanism (one meter, N dimension-matched rules, priority+claim) is unchanged; only the example dimension names are dated. See ADR-044 for the canonical shape.

## Motivation

Stripe's Meter API was bolted onto a card-subscription engine. To bill `gpt-4 input cached` vs `gpt-4 input uncached` vs `gpt-4 output` at different rates, you create three Meters with three event names and three pricing rules. For full Anthropic / OpenAI parity (3 models × 4 operations × 2 cache states), you need ~24 Meters. The dimensional structure is encoded in event-name strings instead of data.

Velox's wedge is **AI-native billing for usage-heavy SaaS**. The minimum-viable expression of that is: **one meter** receives events with arbitrary dimension labels, **many pricing rules** pick out subsets to apply rates. Same data, far fewer meters, far simpler subscription wiring.

Without this design, the wedge is just a slide.

## Goals

- Single meter per usage type (`tokens`, `requests`, `gb_hours`)
- Events carry arbitrary dimensions (`{model, operation, cached, tier}`) on the existing `properties JSONB` column
- Pricing rules match dimension subsets and apply rates
- Aggregation modes per rule, not per meter — `sum`, `count`, `last_during_period`, `last_ever`, `max` (Stripe Tier 1 gap, hoisted)
- Decimal quantities — `NUMERIC` instead of `BIGINT` (Stripe Tier 1 gap, hoisted)
- Forward-compatible with existing `usage_events` / `meters` / `rating_rule_versions` schema (no breaking change for current tenants)
- Sustained 50k events/sec ingest on a single tenant on commodity Postgres

## Non-goals (deferred)

- Streaming meter events (Stripe v2 stream API) — Phase 4
- Bulk S3 ingest — Phase 4
- Server-sent / push aggregation — Phase 4
- Cross-meter formulas (`cost = tokens × rate × surcharge`) — separate "computed meters" design
- Schema enforcement on dimension keys — free-form for v1, revisit after first design partner

## Today's schema (in repo at `internal/platform/migrate/sql/0001_schema.up.sql`)

```sql
CREATE TABLE meters (
    id                      TEXT PRIMARY KEY,
    tenant_id               TEXT NOT NULL REFERENCES tenants(id),
    key                     TEXT NOT NULL,
    name                    TEXT NOT NULL,
    unit                    TEXT NOT NULL DEFAULT 'unit',
    aggregation             TEXT NOT NULL DEFAULT 'sum',
    rating_rule_version_id  TEXT REFERENCES rating_rule_versions(id),
    ...
    UNIQUE (tenant_id, key)
);

CREATE TABLE usage_events (
    id              TEXT PRIMARY KEY,
    tenant_id       TEXT NOT NULL REFERENCES tenants(id),
    customer_id     TEXT NOT NULL REFERENCES customers(id),
    meter_id        TEXT NOT NULL REFERENCES meters(id),
    subscription_id TEXT REFERENCES subscriptions(id),
    quantity        BIGINT NOT NULL DEFAULT 0,
    properties      JSONB NOT NULL DEFAULT '{}',
    idempotency_key TEXT,
    timestamp       TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, idempotency_key)
);
```

Today: one meter has one rating rule, one aggregation mode. The `properties` column is metadata-only and not used in pricing.

## Proposed schema changes — migration `0054_multi_dim_meters`

> **Migration number caveat:** pick at PR-open time from `origin/main`, not local branch (per memory `feedback_migration_numbering`). 0054 is a placeholder.

```sql
-- 0054_multi_dim_meters.up.sql

-- 1. Decimal quantity support. Existing BIGINT data widens losslessly.
--    NUMERIC(38,12) covers tokens (integer) + GPU-hours (6 decimals) +
--    everything plausible without precision loss.
ALTER TABLE usage_events
    ALTER COLUMN quantity TYPE NUMERIC(38, 12) USING quantity::numeric;

-- 2. GIN index on properties for dimension-keyed lookups during aggregation.
CREATE INDEX idx_usage_events_properties_gin
    ON usage_events USING GIN (properties);

-- 3. New table: pricing rules per meter, with dimension match + per-rule
--    aggregation mode. Enables N rules per meter (vs today's 1:1 on meters).
CREATE TABLE meter_pricing_rules (
    id                       TEXT PRIMARY KEY DEFAULT 'vlx_mpr_' || encode(gen_random_bytes(12), 'hex'),
    tenant_id                TEXT NOT NULL REFERENCES tenants(id),
    meter_id                 TEXT NOT NULL REFERENCES meters(id),
    rating_rule_version_id   TEXT NOT NULL REFERENCES rating_rule_versions(id),
    dimension_match          JSONB NOT NULL DEFAULT '{}',
    aggregation_mode         TEXT NOT NULL DEFAULT 'sum'
                              CHECK (aggregation_mode IN ('sum', 'count', 'last_during_period', 'last_ever', 'max')),
    priority                 INT NOT NULL DEFAULT 0,
    created_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, meter_id, rating_rule_version_id)
);

CREATE INDEX idx_meter_pricing_rules_lookup
    ON meter_pricing_rules (tenant_id, meter_id, priority DESC);

ALTER TABLE meter_pricing_rules ENABLE ROW LEVEL SECURITY;
ALTER TABLE meter_pricing_rules FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON meter_pricing_rules FOR ALL USING (
    current_setting('app.bypass_rls', true) = 'on'
    OR tenant_id = current_setting('app.tenant_id', true)
);
GRANT ALL ON TABLE meter_pricing_rules TO velox_app;
```

### Why a new table instead of extending `meters`

`meters.rating_rule_version_id` is 1:1 today. The new model is N:1. New table is the cleanest expression. The existing `meters.rating_rule_version_id` becomes the **default rule** — events not claimed by any pricing rule fall back to it, which preserves backward compatibility for tenants currently in production.

### Backward compatibility

- Tenants with zero `meter_pricing_rules` rows behave identically to today (every event matches the meter's default rule, with `aggregation_mode = meters.aggregation`).
- `properties` JSONB column gains pricing semantics but no schema change — existing data is forward-compatible.
- `quantity BIGINT → NUMERIC(38, 12)` is a widening cast; no data loss; existing Go code reading int64 needs the type-update pass.

## API surface

> **Wire-contract conventions** (consistent with the rest of `/v1/*`):
>
> - **Paths use hyphens**, not underscores: `/v1/usage-events`, `/v1/meters/{id}/pricing-rules`. Matches `/v1/credit-notes`, `/v1/rating-rules`, etc.
> - **Quantity field is `quantity`, both directions.** JSON wire form is a string for decimal precision (`"1234.5"`); numeric form is also accepted on input. There is no `value` alias — pre-launch we don't owe back-compat, and one canonical name beats two.
> - **External identifiers on input.** Ingest takes `external_customer_id` (the developer's own ID) and `event_name` (the meter key); the handler resolves both to internal `customer_id` / `meter_id` server-side.

### `POST /v1/usage-events`

```http
POST /v1/usage-events
Idempotency-Key: <key>
Authorization: Bearer <secret_key>

{
  "external_customer_id": "cust_xyz",
  "event_name": "tokens",
  "quantity": "1234.5",                // string for decimal precision; numeric also accepted
  "dimensions": {
    "model": "gpt-4",
    "operation": "input",
    "cached": false
  },
  "idempotency_key": "evt-1",          // optional; alternative to the header form
  "timestamp": "2026-04-25T12:34:56Z"  // optional, defaults to now
}
```

Response `201`:
```json
{
  "id": "vlx_evt_...",
  "tenant_id": "tnt_...",
  "customer_id": "vlx_cus_...",
  "meter_id": "vlx_mtr_...",
  "subscription_id": "vlx_sub_...",
  "quantity": "1234.5",
  "properties": { "model": "gpt-4", "operation": "input", "cached": false },
  "timestamp": "2026-04-25T12:34:56Z",
  "origin": "api"
}
```

The response carries internal IDs (`vlx_cus_*`, `vlx_mtr_*`) and surfaces dimensions on `properties` (the storage column name) — `dimensions` on the request is just an alias and collapses onto `properties` at the storage layer.


### `POST /v1/meters/{id}/pricing-rules`

```http
POST /v1/meters/mtr_xyz/pricing-rules
Authorization: Bearer <secret_key>

{
  "rating_rule_version_id": "rrv_xyz",
  "dimension_match": {
    "model": "gpt-4",
    "operation": "input",
    "cached": false
  },
  "aggregation_mode": "sum",
  "priority": 100
}
```

Plus `GET`, `LIST`, `DELETE` for full CRUD.

### `GET /v1/customers/{id}/usage`

```http
GET /v1/customers/cust_xyz/usage?event_name=tokens&period_start=2026-04-01&period_end=2026-04-30&group_by=model,operation
```

Returns the per-rule aggregated quantity + projected charges, grouped by requested dimensions. Powers the cost-dashboard component (Week 5).

## Aggregation semantics

When billing finalizes for `(customer, meter, period_start, period_end)`:

1. Load all `meter_pricing_rules` for the meter, ordered by `priority DESC`, then `created_at ASC`, then `id ASC` for deterministic tie-breaking. The `id` final tiebreaker covers the corner case where two same-priority rules share a `created_at` tick (bulk import, same-txn bootstrap, clock-resolution collisions) — without it, Postgres is free to pick an order, which would make billing non-reproducible across replicas.
2. Iterate rules. For each rule:
   - Find events in the period whose `properties` is a **superset** of `dimension_match` AND that haven't been claimed by a higher-priority rule.
   - Apply the rule's `aggregation_mode`:
     - `sum` → `SUM(quantity)`
     - `count` → `COUNT(*)`
     - `last_during_period` → quantity of the latest event by `timestamp` within the period
     - `last_ever` → quantity of the latest event by `timestamp` across all time, regardless of period (used for "current state" billing like seat counts)
     - `max` → `MAX(quantity)`
   - Resolve the aggregated quantity → cents via the rule's `rating_rule_version` (existing flat / graduated / package logic, unchanged).
3. Events not claimed by any pricing rule fall back to the meter's default `rating_rule_version_id` with mode `meters.aggregation`.

### Subset-match semantics

`dimension_match: {model: "gpt-4"}` matches an event with `properties: {model: "gpt-4", operation: "input", cached: false}` — extra dimensions on the event are fine. **Subset, not equality.**

This lets you write coarse rules ("all gpt-4 usage at $X / 1k tokens") and override with finer rules ("but gpt-4 cached input at $0.30 / 1k tokens") via priority.

### Priority + claim semantics

Higher priority rules claim events first; each event is claimed by at most one rule per `(customer, period)`. This prevents double-counting when rules overlap.

Implementation: walk rules in order; for each rule, query events not already in the claimed set; add results to the claimed set. A single SQL query with ordered window-function logic is preferable to N queries — see "Implementation notes" below.

## Implementation notes

### Aggregation query shape (illustrative)

```sql
WITH ranked_rules AS (
    SELECT id, dimension_match, aggregation_mode, rating_rule_version_id,
           ROW_NUMBER() OVER (ORDER BY priority DESC, created_at ASC, id ASC) AS rule_rank
    FROM meter_pricing_rules
    WHERE tenant_id = $1 AND meter_id = $2
),
claimed AS (
    SELECT DISTINCT ON (e.id)
        e.id, e.quantity, e.timestamp,
        r.id AS rule_id, r.aggregation_mode, r.rating_rule_version_id
    FROM usage_events e
    CROSS JOIN ranked_rules r
    WHERE e.tenant_id = $1
      AND e.meter_id = $2
      AND e.customer_id = $3
      AND e.timestamp >= $4
      AND e.timestamp < $5
      AND e.properties @> r.dimension_match  -- JSONB superset operator
    ORDER BY e.id, r.rule_rank ASC
)
SELECT rule_id, aggregation_mode, ...
FROM claimed
GROUP BY rule_id, aggregation_mode, rating_rule_version_id;
```

`@>` is the Postgres JSONB superset operator and uses the GIN index. The `DISTINCT ON (e.id) ORDER BY rule_rank` claims each event to its highest-priority matching rule. Aggregation modes are dispatched in the outer aggregation step.

For `last_during_period`, `last_ever`, `max` modes the aggregation differs — implementation will fork by mode. Likely cleanest: one query per mode, scoped by rule_id.

### Decimal handling in Go

- Domain type: `decimal.Decimal` from `github.com/shopspring/decimal` (already battle-tested, used by Stripe's own SDK and many Go billing systems)
- Postgres driver: `pgtype.Numeric` ↔ `decimal.Decimal` conversion in store layer
- `domain.UsageEvent.Quantity` becomes `decimal.Decimal` (kept the `Quantity` name — wire field is `quantity`, no rename to `Value`)
- Existing internal callers passing `int64` need adapters

Per memory `feedback_prefer_battle_tested_libs`: use `shopspring/decimal`, do not roll our own.

## Test strategy

### Unit (`internal/usage/service_test.go`)
- Ingest with dimensions → stored correctly, idempotency-keyed
- Aggregation correctness across all five modes
- Subset-match semantics (extra dimensions on event don't disqualify)
- Priority + claim — overlapping rules don't double-count
- Default-rule fallback when no pricing rule matches
- Decimal precision round-trip (1234.567890123456 stays 1234.567890123456)

### Integration (`internal/usage/postgres_test.go`)
- Real Postgres
- RLS isolation: tenant A cannot see tenant B's pricing rules or events
- GIN index actually used (EXPLAIN ANALYZE assertion)
- Concurrent ingest with same idempotency key → single row, deterministic winner
- Migration up/down round-trip (per memory `feedback_longterm_fixes`)

### Handler (`internal/usage/handler_test.go`)
- Decimal in JSON parses correctly (string and number forms)
- Invalid `dimension_match` (non-object) rejected with 400
- `aggregation_mode` outside enum rejected with 400
- `meter_id` from another tenant returns 404 (RLS)

## Benchmark plan

- **Target:** sustained 50k events/sec ingest on a single tenant
- **Hardware:** Postgres 16, 8 vCPU, 16GB RAM, NVMe SSD (matches commodity AWS m5/m6)
- **Workload:** `scripts/bench-rig/ingest.js` (k6) — synthetic event stream with realistic dimension cardinality (~10 model values, 4 operation values, 2 boolean cache states), driving the public ingest endpoint. `cmd/velox-bench-seed` creates the fixtures and mints the key.
- **Measurement:** total events ingested over 60s, p50/p95/p99 ingest latency, Postgres CPU + IO
- **Mitigations if we miss target:**
  - Batch INSERTs (already needed for any high-throughput workload)
  - Move idempotency-key uniqueness to a separate dedupe table to avoid full-row UNIQUE pressure
  - Hash-partition `usage_events` by `(tenant_id, month)` — out-of-scope for this design unless benchmark forces it

## Open questions

1. **Should dimensions be schema-validated against the meter?** Proposal: **no** for v1. Free-form JSONB. Revisit after first design partner reports a typo'd dimension caused a billing miss.
2. **Should pricing rules support time-windowed match?** Proposal: **no**. Period boundary is the only window for v1.
3. **How do we handle events whose `quantity` is zero?** Proposal: ingest, count for `count` mode, sum to zero for `sum` mode (no special case).
4. **Decimal precision — `NUMERIC(38, 12)` sufficient?** Proposal: **yes**. Tokens are integer; GPU-hours need 4–6 decimals; 12 is generous.
5. **Cap on dimension count per event?** Proposal: **16 keys, soft limit**. Reject events with >16 dimension keys to bound JSONB size and avoid pathological tenants.
6. **Do existing `usage_events.quantity` BIGINT values need any migration data work?** Proposal: **no**. Widening cast is lossless; max BIGINT (≈9.2 × 10¹⁸) fits NUMERIC(38, 12) trivially.
7. **Should we deprecate the existing `meters.aggregation` column?** Proposal: **not yet**. Keep it as the default-rule aggregation mode for back-compat; flag for removal in 6 months once all tenants migrate to `meter_pricing_rules`.

## Implementation checklist

- [x] Migration `0054_multi_dim_meters.{up,down}.sql`
- [x] `domain.UsageEvent.Quantity` → `decimal.Decimal`
- [x] `domain.MeterPricingRule` struct
- [x] `usage.Store.UpsertPricingRule(...)`, `ListPricingRulesByMeter(...)`, `DeletePricingRule(...)`
- [x] `usage.Service.IngestEvent(...)` accepts decimal quantity + dimensions
- [x] `usage.Service.AggregateForBillingPeriod(...)` resolves rules with priority + dimension match
- [x] HTTP handlers: `POST /v1/usage-events`, `POST/GET/LIST/DELETE /v1/meters/{id}/pricing-rules`
- [x] `GET /v1/customers/{id}/usage` powering the cost dashboard
- [x] Unit tests: ingest, aggregation per mode, subset-match, priority-claim
- [x] Integration tests: real Postgres, RLS-isolated tenants, decimal precision, idempotency
- [x] Ingest benchmark (the 50k events/sec target was never validated. Measured 2026-08-15 on an isolated Postgres 16 container, 8 workers, 10s, with the since-deleted `velox-bench --batch` (k6 `BATCH=` is the equivalent today): **2,714 ev/s single-event; 8,245 at batch 10; 10,821 at batch 50; 12,095 at batch 500**, the curve flattening after ~50 while per-call latency keeps climbing. The long-standing "single-row-INSERT-bound" reading is WRONG — statement-level profiling attributes **~66% of in-database time to COMMIT and only ~28% to the INSERT**, with the three per-transaction `set_config` calls at 5%. Ingest is fsync/commit-bound, so the lever is commits-per-event (batching), not statements-per-event; the plateau above batch 50 is the INSERT-bound ceiling underneath. Laptop-container numbers — not a cloud benchmark.)
- [x] OpenAPI spec update (`api/openapi.yaml`)
- [x] CHANGELOG entry + public changelog rollup
