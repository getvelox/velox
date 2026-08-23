# Create-Preview Endpoint — Technical Design

> **Status:** Shipped (2026-04-26) — `POST /v1/invoices/create_preview`
> **Owner:** Track A (the backend workstream)
> **Last revised:** 2026-04-26
> **Related:** `docs/design-customer-usage.md` (sibling read surface, same composition pattern), `docs/design-multi-dim-meters.md` (multi-dim dependency — `usage.AggregateByPricingRules` is the engine)

## Motivation

Stripe ships `Invoice.create_preview` (formerly `Invoice.upcoming`) as a Tier 1 surface — every customer asks "what is my bill going to be?" before the cycle closes. Velox already has the in-memory plumbing in `Engine.Preview` (the dashboard `GET /v1/billing/preview/{subscription_id}` route). But it's wired against the **old** aggregation path `usage.AggregateForBillingPeriod`, which is not multi-dim aware (multi-dimensional metering: one meter carrying differently-priced slices of usage, split by dimension labels such as model or token type). Week 2 shipped `usage.AggregateByPricingRules` (a LATERAL JOIN using priority+claim resolution — higher-priority rules claim events first, each event counted by at most one rule per billing window — across 5 aggregation modes); the cycle scan (the billing engine's scheduled pass that turns closed cycles into invoices) and the customer-usage endpoint already use it. The preview is the only consumer left on the old path — every multi-dim tenant is silently looking at wrong projected-bill numbers today.

This slice has three deliverables:

1. **Switch `Engine.Preview` to use `AggregateByPricingRules`.** Same code path as the cycle scan → preview math == invoice math. Drift between dashboard projected-bill and the actual finalized invoice is the worst possible UX for a billing engine. As multi-dim tenants ramp, this gap widens silently.
2. **Expose the result over a stable wire contract** at `POST /v1/invoices/create_preview`. Stripe-equivalent path, request body the dashboard can reuse for plan-change confirmation dialogs (Week 5c) and the operator plan-migration preview (Week 6).
3. **Match the response shape pattern of `customer-usage`** so a single TS type set in the dashboard handles both surfaces — cost dashboard renders the projected-bill line by reading the same `totals[]` shape it already reads for current-period spend.

The dashboard's "projected bill" line in `web-v2/src/components/CostDashboard.tsx` is the immediate unblock for Track B (the dashboard/UI workstream). Plan-change confirmation dialog (Week 5c) and the bulk plan-migration preview (Week 7) are the next two consumers.

## Goals

- **Read-only preview of the customer's next invoice.** Caller passes a `customer_id` (and optionally a `subscription_id`); response is the line set the cycle scan would emit if billing fired right now. Zero DB writes — assertable in tests.
- **Multi-dim parity with the cycle scan.** Each `(meter, rule)` pair surfaces as a distinct preview line with `dimension_match` (the rule's dimension-matching expression) echoed from the meter pricing rule. Single-rule meters keep the existing one-line-per-meter shape. Same priority+claim resolution the cycle scan uses; same `domain.ComputeAmountCents` per rule bucket.
- **Cost-dashboard ergonomic.** Response shape mirrors `customer-usage`: per-line `quantity` as a precise decimal string, integer cents on `amount_cents`, `totals[]` always-array (one entry per distinct currency, even when there's only one). Cost dashboard's existing `formatCents(totals[0].amount_cents)` chain works on both.
- **Stripe-shaped path.** `POST /v1/invoices/create_preview`, body `{customer_id, subscription_id?, period?}`. Stripe's surface accepts an inline `subscription` payload to model "what would happen if I changed this customer's plan to X"; we defer that to v2 (see open question 1) — v1 is read-only against the existing subscription.
- **RLS-safe.** RLS (Postgres Row-Level Security) makes the database itself filter every tenant-scoped query to that tenant's rows. Standard `BeginTx(ctx, postgres.TxTenant, tenantID)` through every collaborator. Cross-tenant customer IDs surface as 404 at the customer lookup, by construction.
- **Documented contract Track B can call today.** Cost dashboard swaps the projected-bill line from "elapsed-vs-total cycle ratio extrapolation" to a real backend call as soon as this ships.

## Non-goals (deferred)

- **Inline `invoice_items` additions / one-off line adjustments.** Stripe's `create_preview` accepts an `invoice_items` array so a caller can model "preview my bill if I also added this $50 charge". Useful, but it's a separable surface — defer to v2 once a real consumer asks. The current dashboard projected-bill line doesn't need it.
- **Plan-change override semantics.** Stripe lets the caller pass `subscription_items` to model "preview my bill if I switched plans". The Week 5c plan-change confirmation dialog will need this; punt to that work so we don't merge a half-shape that the dialog rebuilds anyway.
- **Coupon / credit application preview.** Engine.Preview today doesn't apply discounts or credits; we keep that boundary in v1. The cycle scan's discount and credit application is its own pass; reproducing it here invites drift between two implementations of "what's the bill". Open question 4 covers a v2 rev that applies them server-side once customer-usage exposes a shared apply-discount-to-line-set helper.
- **Tax preview.** Same rationale — tax calculation depends on the tenant's provider config and the customer's billing profile; reproducing the path is real work and the dashboard projected-bill line doesn't need it. Defer to v2 with a `taxes_enabled` flag if a real consumer asks.
- **Past-period replay.** v1 always previews the current cycle. Closed cycles are served by `GET /v1/invoices/{id}` (the canonical historical record). No double-implementation here.

## Today's surface (in repo)

This endpoint is **composition over invention**. The hard work already exists:

- `internal/billing/preview.go::Engine.Preview(ctx, sub) (PreviewResult, error)` — existing in-memory preview. Walks subscription items, fetches plans, emits `base_fee` lines per item plus per-meter usage lines. Currently uses `usage.AggregateForBillingPeriod` (not multi-dim aware) — this slice swaps it.
- `internal/usage/service.go::Service.AggregateByPricingRules(ctx, tenantID, customerID, meterID, defaultMode, from, to)` — the priority+claim LATERAL JOIN across 5 aggregation modes. Already shipped, already used by the cycle scan and customer-usage.
- `internal/pricing/service.go::Service.{GetPlan, GetMeter, GetRatingRule, ListMeterPricingRulesByMeter}` — the read surface for plan/meter/rule resolution. Same surface customer-usage composes against.
- `internal/customer/store.go::Store.Get(ctx, tenantID, id)` — RLS-safe customer lookup. 404s for cross-tenant IDs.
- `internal/subscription/store.go::Store.List(ctx, filter)` — list active subs for a customer (filter by `customer_id`).
- `internal/domain/pricing.go::ComputeAmountCents(rule, quantity)` — pricing engine. Same path the cycle scan calls; multi-dim tenants get rule-by-rule rating identical to billing.

The slice is exactly the same composition pattern as `customer-usage`: customer existence → subscription resolution → period → walk meters → rate per rule → assemble. The only differences are (a) we're previewing one specific subscription rather than aggregating across all of a customer's, and (b) the response includes `base_fee` lines from the plan's `BaseAmountCents`.

## API surface

> **Wire-contract conventions** (consistent with `/v1/*`, see `docs/design-customer-usage.md` § wire-contract):
>
> - **Snake-case JSON keys**, struct-tag enforced.
> - **Customer identity is the customer ID** (`vlx_cus_…`). Mirrors customer-usage.
> - **Period bounds are RFC 3339** (`2026-04-01T00:00:00Z`). Both inclusive of `from`, exclusive of `to`. If both omitted → defaults to the subscription's current cycle. Partial bounds (one zero, one non-zero) are rejected with 400.
> - **Empty results are `[]`, never `null`.**
> - **Decimal quantity** marshals as a precise string (`"1234567.000000000000"`) per ADR-005. Amounts are integer cents.
> - **Always-array totals.** `totals[]` is one entry per distinct currency, even when there's only one. Same shape customer-usage uses; one TS type covers both surfaces.

### `POST /v1/invoices/create_preview`

```http
POST /v1/invoices/create_preview
Authorization: Bearer <secret_key>
Content-Type: application/json

{
  "customer_id": "vlx_cus_abc123",
  "subscription_id": "vlx_sub_xyz",
  "period": {
    "from": "2026-04-01T00:00:00Z",
    "to":   "2026-05-01T00:00:00Z"
  }
}
```

Both `subscription_id` and `period` are optional. If `subscription_id` is omitted, the server picks the customer's primary active or trialing subscription. If `period` is omitted, the server uses the subscription's current billing cycle.

Response `200`:
```json
{
  "customer_id": "vlx_cus_abc123",
  "subscription_id": "vlx_sub_xyz",
  "plan_name": "AI API Pro",
  "billing_period_start": "2026-04-01T00:00:00Z",
  "billing_period_end":   "2026-05-01T00:00:00Z",
  "lines": [
    {
      "line_type": "base_fee",
      "description": "AI API Pro - base fee (qty 1)",
      "currency": "USD",
      "quantity": "1",
      "unit_amount_cents": 0,
      "amount_cents": 0
    },
    {
      "line_type": "usage",
      "description": "Tokens (gpt-4 · input)",
      "meter_id": "vlx_mtr_tokens",
      "rating_rule_version_id": "vlx_rrv_gpt4_input",
      "rule_key": "gpt4_input",
      "dimension_match": { "model": "gpt-4", "token_type": "input" },
      "currency": "USD",
      "quantity": "1000000.000000000000",
      "unit_amount_cents": 0,
      "amount_cents": 3000,
      "pricing_mode": "flat"
    },
    {
      "line_type": "usage",
      "description": "Tokens - output",
      "meter_id": "vlx_mtr_tokens",
      "rating_rule_version_id": "vlx_rrv_gpt4_output",
      "rule_key": "gpt4_output",
      "dimension_match": { "model": "gpt-4", "token_type": "output" },
      "currency": "USD",
      "quantity": "234567.000000000000",
      "unit_amount_cents": 0,
      "amount_cents": 704
    }
  ],
  "totals": [
    { "currency": "USD", "amount_cents": 3704 }
  ],
  "warnings": [],
  "generated_at": "2026-04-26T12:00:00Z"
}
```

`lines[]` is a flat list — each `(meter, rule)` pair becomes its own line. Single-rule meters emit one line per meter (no `dimension_match` echo). Multi-rule meters emit one line per rule with `dimension_match` carrying the rule's match expression (the canonical pricing identity, mirrored from customer-usage).

`totals[]` is always an array — one entry per distinct currency seen across lines. Single-currency tenants get a one-entry array; multi-currency setups (rare today, real for cross-region tenants) get one entry per currency. Cost dashboard reads `totals[0].amount_cents` for both shapes.

`warnings[]` mirrors customer-usage: non-fatal conditions (a meter has events outside any rule's `dimension_match` and is falling through to the meter default; a meter has rating rules with mismatched currencies; etc.). Empty array in v1 if everything is clean.

`generated_at` is the server clock at preview-compute time. Useful for the dashboard to show "as of 12:00 UTC" subtitle without an extra request-header read.

### Error shapes

- `400 invalid_request` — `customer_id` blank or missing.
- `404 customer_not_found` — no customer with this ID for the tenant.
- `400 customer_has_no_subscription` (coded) — caller passed no `subscription_id` and the customer has zero active or trialing subscriptions. Same error code as customer-usage so the dashboard's empty-state branch covers both surfaces.
- `404 subscription_not_found` — `subscription_id` was passed but no sub exists for the tenant with that ID.
- `400 invalid_period` — `from` after `to`, partial bounds, or unparseable RFC 3339.

## Internals

### Composition

```go
// internal/billing/preview_create.go (sketch)
func (s *PreviewService) CreatePreview(
    ctx context.Context,
    tenantID string,
    req CreatePreviewRequest,
) (PreviewResult, error) {
    cust, err := s.customers.Get(ctx, tenantID, req.CustomerID)
    if err != nil { return PreviewResult{}, err } // 404 propagates

    sub, err := s.resolveSubscription(ctx, tenantID, cust.ID, req.SubscriptionID)
    if err != nil { return PreviewResult{}, err }

    from, to, err := s.resolvePeriod(req.Period, sub)
    if err != nil { return PreviewResult{}, err }

    plansByID, meterIDs, err := s.collectPlanAndMeters(ctx, tenantID, sub)
    if err != nil { return PreviewResult{}, err }

    var lines []PreviewLine

    // Base-fee lines from each subscription item's plan.
    for _, item := range sub.Items {
        plan := plansByID[item.PlanID]
        if plan.BaseAmountCents > 0 {
            lines = append(lines, baseFeeLine(plan, item))
        }
    }

    // Usage lines per (meter, rule).
    for _, meterID := range meterIDs {
        meterLines, warnings, err := s.rateMeter(ctx, tenantID, sub.CustomerID, meterID, from, to)
        if err != nil { return PreviewResult{}, err }
        lines = append(lines, meterLines...)
        result.Warnings = append(result.Warnings, warnings...)
    }

    return assembleResult(sub, plansByID, lines, from, to), nil
}
```

`rateMeter` is the same composition as `customer-usage.rateMeter`: call `usage.AggregateByPricingRules`, walk the `(rule_id, quantity)` pairs, hand each to `domain.ComputeAmountCents`, echo `dimension_match` from the meter pricing rule. Output is one `PreviewLine` per rule rather than a single roll-up — the preview's job is to project the invoice's line set, not summarize.

### Subscription resolution

- **Explicit `subscription_id`:** look it up via `subs.Get(ctx, tenantID, id)`; 404 propagates as `subscription_not_found`. Verify the sub belongs to the requested customer (defensive against the rare case where the operator typoed both IDs to plausible-looking-but-mismatched values); mismatch is `400 invalid_request`.
- **Implicit (no `subscription_id`):** `subs.List(ctx, ListFilter{CustomerID})` then filter to `active`/`trialing`. Pick the one with the latest `current_period_start` (same heuristic as customer-usage's "primary active subscription"). If zero matches, return `customer_has_no_subscription` so the cost dashboard's empty-state branch covers it.

### Period resolution

- **Default:** subscription's `current_billing_period_start`/`_end`. The cycle scan's bounds — preview shows what the next invoice will look like.
- **Explicit:** parse `from`/`to`, validate `from < to`. Same bounds-check semantics as customer-usage, minus the 1-year cap (preview is always within a single cycle, so caps don't make sense — defer to caller hygiene).

### RLS

Standard `BeginTx(ctx, postgres.TxTenant, tenantID)` through every collaborator. The customer lookup naturally 404s for cross-tenant IDs.

### No DB writes

A standout property of this endpoint vs. the existing `/v1/invoices/{id}/finalize` path: nothing persists. The preview composes reads only; the integration test asserts the row count of `invoices` and `invoice_line_items` is unchanged before/after the call.

## Tests

### Unit tests

- `resolveSubscription` table-driven: explicit ID happy path, explicit ID for wrong customer → invalid_request, implicit pick of latest active sub, implicit with zero subs → coded error, implicit with multiple subs → most-recent-cycle wins.
- `resolvePeriod` table-driven: default to sub's current cycle, explicit window, partial bounds → 400, `from >= to` → 400, default with no current cycle → coded error.
- `rateMeter`: single-rule meter emits one line, multi-rule meter emits one line per rule with `dimension_match` echoed, mismatched-rule-currency surfaces a warning, missing-rating-rule line is skipped + warning.
- `assembleResult`: per-currency `totals[]` rolls up correctly, empty meter slice still emits `lines: []` not null.
- **`TestWireShape_SnakeCase` (the merge gate):** marshal a real `PreviewResult`, assert every required snake_case key is present (`customer_id`, `subscription_id`, `lines`, `totals`, `warnings`, `billing_period_start`, etc.), assert no PascalCase leaks, assert empty fields marshal as `[]` not `null`. Same regression-test pattern the recipes feature uses.

### Integration tests (real Postgres)

- `TestCreatePreview_SingleMeterFlatParity` — same fixture as `TestCustomerUsage_SingleMeterFlatParity` (100 events × qty=10 × 1¢ = 1000c). Confirms preview emits the same totals number → preview math == invoice math.
- `TestCreatePreview_MultiDimDimensionMatchEcho` — same fixture as the multi-dim customer-usage test (1000 input @3¢ + 100 output @5¢ = 3500c). Confirms both rule lines surface with `dimension_match` echoed.
- `TestCreatePreview_NoWrites` — count rows in `invoices` + `invoice_line_items` before and after the preview call; assert unchanged.
- `TestCreatePreview_CrossTenantIsolation` — tenant B's request for tenant A's customer ID surfaces as 404.
- `TestCreatePreview_CustomerHasNoSubscription` — confirm the documented error code (`customer_has_no_subscription`) for symmetry with customer-usage.

## Migrations

**None.** All reads, no schema changes.

## Performance

- Composition over the same read paths customer-usage uses. Per-call cost: 1 customer lookup + 1 subscription read + 1 plan read per distinct plan + 1 `AggregateByPricingRules` call per meter. For a typical sub on 1 plan with 1 multi-dim meter: 4 round-trips, all on indexed columns.
- No event-by-event scan in the hot path; the LATERAL JOIN already runs over the in-cycle slice of `usage_events`.
- Target: < 200ms p95 for the typical sub (10K events / cycle). Same envelope as customer-usage.

## Decimal & numeric considerations

Quantity field type: **`decimal.Decimal` marshaled as a string**, matching customer-usage. Single-rule meters keep the existing per-line shape but with the precise decimal — fractional AI-usage primitives (GPU-hours, cached-token ratios) round-trip without precision loss. Amounts stay integer cents per ADR-005.

Note: the existing `Engine.Preview` exposed `Quantity int64` and the `description` formatted the decimal. We change to `Quantity decimal.Decimal` (string-marshaled) for v1. There's no consumer in main yet that would break: only `web-v2/src/components/CostDashboard.tsx` and the `/billing/preview/{subscription_id}` debug route consume the type, both via the typed API client. The TS surface adds a `.toString()` call where the dashboard previously took an integer; one-line change in the consumer.

## Open questions

1. **Should the request body accept inline `subscription_items` to model "preview if I changed this sub's plan to X"?** Stripe does. **Proposal: no for v1.** The Week 5c plan-change confirmation dialog will need this, and it'll model the change with explicit fields rather than the subscription-shaped overlay (which carries its own validation problem). Defer there.
2. **Should the response shape mirror the persisted `domain.Invoice` byte-for-byte?** **Proposal: no — keep `PreviewResult` separate.** The persisted invoice has fields that are nonsense for a preview (`status`, `payment_status`, `amount_paid_cents`, etc.); reusing it would force every preview consumer to ignore half the type. The shape we ship is the cost-dashboard-shaped subset; it's small enough to enumerate and aligns with customer-usage's idiom.
3. **Should `lines[]` be one line per `(meter, rule)` pair, or one line per meter with rules nested inside?** **Proposal: flat one-line-per-rule.** Matches Stripe's flat `lines.data` and matches the cycle scan's `invoice_line_items` (one row per rule's roll-up). Nested would be cleaner for multi-rule meters but inconsistent with the canonical invoice shape, and the dashboard already groups by `meter_id` client-side for the customer-usage view.
4. **Should the preview apply customer credits + coupon discounts?** The cycle scan does, so a true "what will my next invoice look like" view should too. **Proposal: defer to v2.** Reproducing the apply-credit + apply-coupon paths server-side is real work and risks drift; v1 ships subtotal-only, with a clear note in the changelog that credits and coupons aren't reflected. Real consumers (the cost dashboard's projected-bill line) only need subtotal today.
5. **Should `period` accept a `cycle_offset` shorthand (e.g. `next_cycle`, `last_cycle`)?** Useful for "what would this customer pay next month at current usage." **Proposal: no for v1.** Pure UI math on top of explicit RFC 3339 bounds. Add only when a real consumer asks.
6. **Why `POST` for a read-only operation?** Stripe convention (`POST /v1/invoices/create_preview`) — the body carries enough structure (period, optional subscription overlay in their version) that GET with query parameters would be ugly. A consistent verb-with-body is also more discoverable for the SDK ecosystem than a one-off GET. We follow.
7. **Should we surface `invoice_number_preview` (the number the next invoice would receive)?** **Proposal: no.** The number is allocated by the tenant settings store at finalize time; previewing it would either reserve numbers (state mutation, defeating the point) or guess (wrong if a concurrent invoice claims that number first). Punt.

## Implementation checklist (Week 5b)

- [x] `internal/billing/preview.go` — refactor to use `usage.AggregateByPricingRules`; new `PreviewResult` shape with `lines[]` per `(meter, rule)`, `totals[]` always-array, decimal-string quantities. Existing `Engine.Preview` keeps its signature for the in-app `/billing/preview/{subscription_id}` route.
- [x] `internal/billing/preview_create.go` — `PreviewService.CreatePreview` composing customer / subscription / period / per-meter rating.
- [x] `internal/billing/create_preview_handler.go` — `POST /v1/invoices/create_preview` route.
- [x] `internal/api/router.go` — mount under `/v1/invoices/create_preview` behind `auth.PermInvoiceRead`.
- [x] `internal/billing/preview_wire_shape_test.go` — `TestWireShape_SnakeCase` regression test (the merge gate).
- [x] Unit tests: subscription resolution, period resolution, multi-dim per-rule lines, no-sub error path.
- [x] Integration tests: single-meter parity, multi-dim parity, no-writes assertion, cross-tenant 404, no-sub error code.
- [x] CHANGELOG.md (Track A) + Changelog.tsx (Track B) entries.
- [x] Track A → Track B handoff note (private velox-ops repo).

## Track B unblock

Track B can swap the cost dashboard's projected-bill line from extrapolation to a real backend call today. Same auth as customer-usage (PermInvoiceRead), same `totals[]` shape, same RLS-by-construction.

The dashboard call:

```ts
const preview = await api.createInvoicePreview({ customer_id })
// preview.totals[0].amount_cents → projected bill in formatCents()
// preview.lines[] → per-rule breakdown for the drill-down panel
```

The dashboard shape Track B should aim at:

- **Projected-bill summary line** below the existing "current usage" total: `"Projected bill: $37.04 — based on usage so far this cycle"`. Link to the drill-down opens the lines table.
- **Drill-down (modal or expanded card):** flat list of `lines[]`. Group by `meter_id` client-side; show `dimension_match` chips on multi-rule meters; show `base_fee` lines at top. Highlight the discrepancy if `customer-usage.totals` and `create_preview.totals` differ (sanity check that both reads agree).
- **Plan-change confirmation dialog (Week 5c):** opens a preview with the new plan baked in. v1 of the API doesn't accept the overlay; the dialog falls back to "preview at current plan" with a tooltip, and the overlay lands in v2.

Same parallel-work pattern as recipes / customer-usage: Track B mocks the contract from this RFC, swaps to the real API at integration time.

## Review status

- **Track A author:** drafted 2026-04-26 alongside the implementation
- **Track B review:** pending — Track B can scaffold the projected-bill line against this design without waiting for further iteration
- **Human review:** pending — flag any open question to revisit
