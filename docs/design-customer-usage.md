# Customer Usage Endpoint — Technical Design

> **Status:** Shipped — see [`CHANGELOG.md`](../CHANGELOG.md) for the merged commits and the embeddable surface in `web-v2/src/components/CostDashboard.tsx`.
> **Last revised:** 2026-04-26
> **Related:** `docs/design-multi-dim-meters.md` (multi-dim dependency), `docs/design-recipes.md` (same wire-contract conventions)
>
> The text below is preserved as the design-time RFC. The implementation is live in `main`; refer to `internal/usage/customer_usage.go` + `customer_usage_handler.go` (dashboard UI in `web-v2/src/components/CostDashboard.tsx`) for the current behaviour.

## Motivation

A buyer evaluating Velox asks the same question on day one: "How do I show a customer their current usage and what they'll be charged?" Stripe Billing's answer is a hosted Customer Portal page — but that's locked to Stripe Billing, which Velox does not use (per ADR on PaymentIntent-only). Buyers self-hosting Velox today have no first-party answer; they hand-roll a SQL query against `usage_events` and pray the dimension-match logic stays in sync with their pricing rules.

This is the second flagship developer-experience surface, alongside recipes. Where recipes collapse "set up billing" to one POST, **`GET /v1/customers/{id}/usage` collapses "show me what this customer used and owes" to one GET**. The cost-dashboard component is the in-product manifestation; the API itself unblocks every operator who needs the same data in their own portal, in Slack alerts, in CSV exports.

The dimension-match-aware aggregation is the part that can't be hand-rolled: the rules-ranked LATERAL JOIN in `internal/usage/postgres.go:236–279` is non-trivial, and any external SQL would silently drift from canonical pricing the moment a tenant adds a `meter_pricing_rule`. This endpoint is the supported way to get the right answer.

This endpoint composes the multi-dim aggregation surface and the invoice-line-item fan-out so callers can distinguish billed from not-yet-billed usage in a single response.

## Goals

- **One call answers "what did this customer use?"** Per-meter aggregates over a caller-chosen window, with the same dimension-match → rating-rule binding the cycle scan uses. Operators read the same numbers their invoices will. No SQL drift.
- **Cost transparency on the same response.** Each per-meter (and per-rule, for multi-dim) line includes the rated `amount_cents` so dashboards render `$12.34 — 1.2M tokens · gpt-4 input · uncached` without a second pricing call.
- **Period defaults to current billing cycle.** With no query params, returns the live customer's current cycle so the obvious dashboard call is the cheapest one to write. `?from=…&to=…` overrides for historical / arbitrary windows (last-90d, audit, etc.).
- **Subscription context co-emitted.** Response includes the plan(s) the customer is on plus the cycle boundaries, so a dashboard can render "Plan: AI API Pro · cycle Apr 1 → May 1 · 87% through" without follow-up calls.
- **RLS-safe.** All queries scope through `BeginTx(ctx, postgres.TxTenant, tenantID)`; the response cannot leak across tenants even when an operator passes a customer ID from another tenant — the lookup just 404s.
- **Cheap.** Constant-bounded SQL (one aggregate per meter on the customer's plans). No event-by-event scan in the hot path. Live dashboards refresh every few seconds without melting the DB.
- **Documented contract for downstream consumers.** This is the API the cost-dashboard component calls and the portal surface uses.

## Non-goals (deferred)

- **Time-series breakdown.** v1 returns aggregates over the window, not per-day buckets. The dashboard's first iteration is "current period total + last period total + plan boundary"; sparkline data lands in v2. Postgres aggregation by `date_trunc('day', timestamp)` is cheap when needed — we just don't wire it yet.
- **Forecast / projection.** "At current rate, this customer will use 3.4M tokens by cycle end." Pure UI math on top of the v1 response. Punt.
- **Cross-customer rollups.** No "top customers by usage" query. Use the `/v1/usage-events` list endpoint with appropriate filters; or, when a real user need shows up, design `GET /v1/usage/top-customers` separately. This endpoint is per-customer.
- **Threshold alerts.** "Notify me when this customer hits 80% of plan cap." Webhook concern (`usage.threshold_crossed`) — owned by the dunning/notification surface, not this endpoint. Defer to Week 6+.
- **Past-cycle line-item replay.** v1 returns aggregates, not the canonical `invoice_line_items` rows for closed cycles. The latter is what `GET /v1/invoices/{id}` already serves; no double-implementation here.
- **Currency conversion.** Amounts are returned in the plan's currency. If a tenant has multi-currency plans, each line carries its own `currency` field; clients render whichever they need.

## Today's surface (in repo)

- `internal/usage/store.go::AggregateForBillingPeriod(ctx, tenantID, customerID, meterIDs, from, to)` — already returns `map[string]decimal.Decimal` per meter. Multi-dim aware via `meter_pricing_rules` LATERAL JOIN (`internal/usage/postgres.go:236–279`).
- `internal/customer/store.go::Get(ctx, tenantID, id)` — RLS-safe customer lookup.
- `internal/subscription/store.go::List(ctx, tenantID, filter)` — list active subs for a customer (filter by `customer_id`).
- `internal/domain/pricing.go::ComputeAmountCents(rule RatingRuleVersion, quantity decimal.Decimal)` — pricing engine; same path the cycle scan calls.
- `internal/billing/engine` — orchestrates cycle scan; reads the same store layer this endpoint composes from. The cycle scan is the canonical reference for "how to rate a customer's events", and this design intentionally reuses the same store calls.

This endpoint is **composition, not new aggregation logic**. The hard SQL already exists. The win is exposing it on a stable wire contract so operators (and Track B's dashboard) don't reinvent it.

## API surface

> **Wire-contract conventions** (consistent with `/v1/*`, see `docs/design-recipes.md` § wire-contract):
>
> - **Snake-case JSON keys**, struct-tag enforced. Picker-side TS types match.
> - **Customer identity is the customer ID** (`vlx_cus_…`), not external ID. Use `?external_id=` if you need external-ID lookup; defer for v2 or wrap as a separate endpoint.
> - **Period bounds are RFC 3339** (`?from=2026-04-01T00:00:00Z&to=2026-05-01T00:00:00Z`). Both inclusive of `from`, exclusive of `to`. If only `from` is supplied → 400 (we don't guess "to=now"). If both omitted → defaults to the customer's current billing cycle.
> - **Empty results are `[]`, never `null`.** Aligns with the existing list endpoints.

### `GET /v1/customers/{id}/usage` — current cycle (default)

```http
GET /v1/customers/vlx_cus_abc123/usage
Authorization: Bearer <secret_key>
```

Response `200`:
```json
{
  "customer_id": "vlx_cus_abc123",
  "period": {
    "from": "2026-04-01T00:00:00Z",
    "to": "2026-05-01T00:00:00Z",
    "source": "current_billing_cycle"
  },
  "subscriptions": [
    {
      "id": "vlx_sub_xyz",
      "plan_id": "vlx_pln_pro",
      "plan_name": "AI API Pro",
      "currency": "USD",
      "current_period_start": "2026-04-01T00:00:00Z",
      "current_period_end": "2026-05-01T00:00:00Z"
    }
  ],
  "meters": [
    {
      "meter_id": "vlx_mtr_tokens",
      "meter_key": "tokens",
      "meter_name": "Tokens",
      "unit": "tokens",
      "currency": "USD",
      "total_quantity": "1234567.000000000000",
      "total_amount_cents": 3704,
      "rules": [
        {
          "rating_rule_version_id": "vlx_rrv_gpt4_input_uncached",
          "rule_key": "gpt4_input_uncached",
          "dimension_match": { "model": "gpt-4", "token_type": "input" },
          "quantity": "1000000.000000000000",
          "amount_cents": 3000
        },
        {
          "rating_rule_version_id": "vlx_rrv_gpt4_output",
          "rule_key": "gpt4_output",
          "dimension_match": { "model": "gpt-4", "token_type": "output" },
          "quantity": "234567.000000000000",
          "amount_cents": 704
        }
      ]
    }
  ],
  "totals": {
    "amount_cents": 3704,
    "currency": "USD"
  },
  "warnings": []
}
```

`rules` is the per-rule breakdown for multi-dim meters; for a flat single-rule meter the slice is length 1 with no `dimension_match`. `total_quantity` is the sum across rules; `total_amount_cents` is the sum across rules' `amount_cents`. Multi-currency plans (rare) emit one entry per currency under `meters` with `currency` set; `totals` becomes a list of `{currency, amount_cents}` rather than a scalar.

`warnings` is the same shape as recipes' preview warnings — non-fatal conditions (a meter has events outside any rule's `dimension_match` and is falling through to the meter's default aggregation; a subscription is past its cycle end and a re-cycle hasn't run; `currency` mismatch between subscription and rating rule). Empty array in v1 if everything is clean.

### `GET /v1/customers/{id}/usage?from=…&to=…` — explicit window

```http
GET /v1/customers/vlx_cus_abc123/usage?from=2026-01-01T00:00:00Z&to=2026-04-01T00:00:00Z
Authorization: Bearer <secret_key>
```

Same response shape; `period.source` is `"explicit"`. `subscriptions` reflects whatever was active during any portion of the window; if the customer changed plans mid-window, both plans surface (sorted by `current_period_start` ascending).

### Error shapes

- `404 customer_not_found` — no customer with this ID for the tenant. Same shape as other `/v1/customers/{id}` 404s.
- `400 invalid_period` — `from` after `to`, partial bounds (`from` without `to`), unparseable RFC 3339, or window > 1 year (sanity bound — overrideable via internal `?max_window=true` once we hit a real audit need).
- `400 customer_has_no_subscription` — caller passed no period and the customer has zero active subscriptions, so there's no canonical "current cycle". Hint message: "Pass `?from=` and `?to=` to query usage outside a billing cycle."

## Internals

### Composition

```go
// internal/usage/service.go (sketch)
func (s *Service) GetCustomerUsage(
    ctx context.Context,
    tenantID, customerID string,
    period CustomerUsagePeriod,
) (CustomerUsageResult, error) {
    cust, err := s.customers.Get(ctx, tenantID, customerID)
    if err != nil { return CustomerUsageResult{}, err } // 404 propagates

    subs, err := s.subscriptions.ListActiveForCustomer(ctx, tenantID, customerID)
    if err != nil { return CustomerUsageResult{}, err }

    from, to, source, err := resolvePeriod(period, subs) // current cycle vs explicit
    if err != nil { return CustomerUsageResult{}, err }

    // Walk the union of meter IDs across the customer's subscribed plans.
    meterIDs := collectMeterIDs(subs)
    rated, err := s.usage.RateForCustomer(ctx, tenantID, customerID, meterIDs, from, to)
    if err != nil { return CustomerUsageResult{}, err }

    return assembleResult(cust, subs, period, rated), nil
}
```

`usage.RateForCustomer` is a thin wrapper that calls the existing `AggregateByPricingRules` per meter, hands the resulting `(rule, quantity)` pairs to `pricing.ComputeAmountCents`, and assembles the per-rule breakdown. **No new SQL.** The rating-rule lookup uses the same priority-ordered scan as the cycle engine, so dashboard math == invoice math.

### Period resolution

- **Default (no `from`/`to`):** look at the customer's primary active subscription (most recent `current_period_start`). Use its `current_period_start` and `current_period_end`. If multiple active subs with divergent cycles, pick the latest start; emit a warning if cycle bounds disagree.
- **Explicit:** parse `from` / `to`, validate `from < to`, validate window ≤ 1 year (configurable via env at v2).
- **Cap to 1 year** because the LATERAL JOIN cost grows linearly with row count; one year of high-volume usage events on a multi-dim meter is already 100M+ rows for active tenants. We surface this in 400, not silently degrade.

### RLS

Standard `BeginTx(ctx, postgres.TxTenant, tenantID)`. The customer lookup naturally 404s for cross-tenant IDs (RLS hides the row). No new policy.

### Caching

**No caching in v1.** Each call hits Postgres. The aggregate query is fast (~tens of ms on dev hardware for 1M events). Add caching only if a real performance issue emerges; the failure mode of a stale dashboard is worse than the cost of the live query.

## Tests

### Unit tests

- `resolvePeriod` table-driven: explicit window, default → current cycle, default → no subscription → 400, partial-bounds → 400, window > 1 year → 400, `from > to` → 400.
- `assembleResult` shape: multi-currency plans emit one meter entry per currency; total aggregations across rules sum correctly; empty events → 0-amount lines, not omitted entries.

### Integration tests (real Postgres)

- **Single-meter, single-rule.** Seed 1000 events on a seat-based / flat-rate meter. Verify `total_quantity` and `total_amount_cents` match the cycle-scan output for the same period.
- **Multi-dim meter.** Seed events split across 3 dimension combinations on `anthropic_style`. Verify the per-rule breakdown sums to `total_amount_cents` and matches `usage.AggregateByPricingRules` exactly. This is the parity test that catches drift between dashboard math and invoice math.
- **Cycle alignment.** Seed events both inside and outside the cycle window. Default call only counts in-cycle. Explicit-window call counts the requested range.
- **Cross-tenant isolation.** Customer ID exists for tenant A; tenant B's GET → 404. RLS-by-construction.
- **No subscription, no period.** Customer with zero active subs + no query params → 400. With explicit `?from=&to=` → 200 with `subscriptions: []` and per-meter aggregates by raw event scan.
- **Plan transition mid-window.** Customer was on plan X (Apr 1–15) then plan Y (Apr 15–30). Explicit window Apr 1 → May 1 returns both subscriptions; per-meter aggregates span both.
- **Closed cycle parity.** A previously billed cycle's aggregate matches the sum of `invoice_line_items` for the cycle's invoice (small drift tolerance for any rounding mode differences — should be zero for `flat` rules).

### End-to-end test (against running stack)

Delivered as `internal/usage/customer_usage_integration_test.go` (real Postgres: ingest across dimension combinations, `GET /v1/customers/{id}/usage` asserted against expected fixtures) plus MANUAL_TEST FLOW S2 for the running-stack smoke. The standalone e2e binary this section originally proposed (`cmd/velox-customer-usage-e2e`, or extending `velox-recipes-e2e`) was never built — neither ever existed in git.

## Migrations

**None.** The schema (`usage_events`, `meter_pricing_rules`, `subscriptions`, `plans`) is sufficient. This is a read-side endpoint over existing tables.

## Performance

- One aggregate query per meter on the customer's subscribed plans. For a typical customer on 1 plan with 1 multi-dim meter and 12 rules, the LATERAL JOIN runs once over the customer's in-cycle events.
- `usage_events` is already indexed on `(tenant_id, customer_id, timestamp)` — the period scan is a range scan on this index, not a full-table scan.
- Target: < 100ms p95 for the typical customer (10K events / cycle). Target: < 500ms p99 for the heavy customer (1M events / cycle, 12 dimension rules). Beyond that, we add per-day rollups (deferred to v2 if it ever becomes a problem).
- No N+1: `subscriptions.ListActiveForCustomer` is one query; the per-meter aggregate is one query per meter (typically 1–3). No per-event call-out.

## Decimal & numeric considerations

Quantity is `decimal.Decimal` (NUMERIC(38,12)) per ADR-005, marshaled as a string (`"1234567.000000000000"`) to preserve precision. Amounts are integer cents per ADR-005; the rating-rule mode (`flat`, `graduated`, `package`) determines how the aggregated quantity becomes cents — all math runs through `pricing.ComputeAmountCents`, no recompute here.

## Open questions

1. **Should the endpoint accept `external_id` in the path** (e.g. `GET /v1/customers/by-external-id/{external_id}/usage`)? **Proposal: no for v1.** Add a separate lookup endpoint or a `?customer_external_id=` filter on the existing `GET /v1/customers` endpoint if a partner asks. Avoids two paths to the same resource.
2. **Should `rules[].dimension_match` echo the rule's match expression, or the raw event dimensions seen?** **Proposal: the rule's match expression** — that's the canonical pricing identity. The dashboard already knows what dimensions exist on the meter. Echoing observed values would be log data, which belongs on `/v1/usage-events`.
3. **Should we expose pre-cycle / not-yet-billed usage separately from billed?** **Proposal: not in v1.** The "current period" answer already implies "not yet billed". For closed cycles, the matching invoice is the source of truth for billed numbers, and the usage aggregate matches it (parity test above). Two-section responses add UI complexity for no clear win.
4. **Does the response need a `next_invoice_estimate` block** (projected end-of-cycle cents)? **Proposal: no — pure UI math.** The dashboard has the rate (cents-per-unit per rule) and the elapsed-vs-total cycle ratio; it can extrapolate. Server-side projection invites disagreement with whatever heuristic the dashboard wants.
5. **Pagination on `meters[]`?** **Proposal: no.** A customer is on at most a handful of meters; even the heaviest tenant we can imagine has ≤ 20. If we hit a real catalog-size customer, paginate then.
6. **Should the endpoint live under `/v1/customers/{id}/usage` or `/v1/usage/by-customer/{id}`?** **Proposal: `/v1/customers/{id}/usage`** — it composes naturally with the rest of the customer namespace and matches Stripe's convention (`/v1/customers/{id}/balance_transactions`). The /usage namespace stays for raw event ingest/list.
7. **Should `total_amount_cents` come from a fresh `ComputeAmountCents` call or from the matching `invoice_line_items` if the cycle is closed?** **Proposal: always fresh compute.** If the customer's pricing was edited mid-cycle, the dashboard wants the live answer; the invoice has the historical answer. Cycle-closed parity test guards drift.
8. **Should warnings include "subscription cycle has rolled over but `current_period_*` hasn't been refreshed yet"?** **Proposal: yes**, surfaces a real ops issue (cycle scanner stuck) without breaking the response. Format: `cycle_refresh_lagging` warning code + the offending subscription ID.

## Implementation checklist

- [x] `internal/usage/service.go` — `GetCustomerUsage` + `RateForCustomer` composing existing store calls.
- [x] `internal/usage/handler.go` — `GET /v1/customers/{id}/usage` route.
- [x] `resolvePeriod` helper + unit tests.
- [x] Integration tests: single-meter parity, multi-dim parity, cycle alignment, cross-tenant 404, no-sub + explicit window, plan transition, closed-cycle parity.
- [x] End-to-end assertion fixture against the running stack.
- [x] OpenAPI spec update (`api/openapi.yaml`).
- [x] CHANGELOG entry + public changelog rollup.

## Track B unblock

Track B can scaffold the cost-dashboard against this design today. The contract Track B should mock:

- `GET /v1/customers/{id}/usage` (no params) → response shape above with `period.source: "current_billing_cycle"`.
- `GET /v1/customers/{id}/usage?from=…&to=…` → same shape with `period.source: "explicit"`.
- 400 on `customer_has_no_subscription` when neither cycle nor explicit window is resolvable.
- 404 on cross-tenant / unknown customer.

The dashboard shape Track B should aim at:

- Header: customer name + plan + "cycle X of Y days · Z% through".
- Big number: cycle-to-date `totals.amount_cents` formatted in `totals.currency`.
- Per-meter cards: `meter_name · total_quantity · total_amount_cents`. Click → expanded view of `rules[]` with dimension chips and per-rule cents.
- Filters: cycle (default) / last cycle / last 90 days / custom — all map to the same endpoint with different `from`/`to`.

Track B can ship the UI against a mocked API (MSW handlers seeded from the example response above) before Track A finishes the backend, then swap to the real API at integration time. Same design-first / RFC parallel-work pattern as recipes.

## Review status

- **Track A author:** drafted 2026-04-26
- **Track B review:** pending — Track B can scaffold the cost-dashboard against this design without waiting for implementation
- **Human review:** pending — please flag any open question to resolve before Week 5 starts
