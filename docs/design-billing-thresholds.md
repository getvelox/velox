# Billing Thresholds — Technical Design

> **Status:** Shipped — see [`CHANGELOG.md`](../CHANGELOG.md) for the merged commits and migration `0056_subscription_billing_thresholds`.
> **Last revised:** 2026-04-26
> **Related:** `docs/design-create-preview.md` (sibling — same composition, same wire conventions), `docs/design-multi-dim-meters.md` (multi-dim dependency — `usage.AggregateByPricingRules`)
>
> The text below is preserved as the design-time RFC. The implementation is live in `main`; refer to `internal/billing/threshold_scan.go` and `internal/subscription/` for the current behaviour.

## Motivation

Stripe ships **billing thresholds** as Tier 1 (top-priority in Velox's Stripe-parity feature ranking) — a tenant configures a per-subscription cap (`amount_gte` in cents, or `usage_gte` per item in units). When the in-cycle running total crosses the threshold, the engine **finalizes an invoice early** (issues it mid-cycle instead of waiting for period end), optionally **resets the billing cycle** (so the next cycle starts at zero), and emits a webhook event. This shields the tenant's customers from runaway bills mid-cycle, and shields Velox tenants from collection risk on customers whose usage suddenly explodes.

Velox already has every primitive to do this:

- The cycle scan (the scheduler pass that bills each subscription at period end: `Engine.RunCycle` → `Engine.billSubscription` → `CreateInvoiceWithLineItems` → `advanceCycleOrCancel`) already builds line items from the partial-cycle window, prices them via `domain.ComputeAmountCents`, finalizes the invoice, and rolls the cycle forward.
- `usage.AggregateByPricingRules` already computes the running total over an arbitrary window, including for multi-dim (multi-dimensional) meters — one meter carrying differently-priced slices of usage, split by dimension labels.
- The scheduler already runs a billing tick under an advisory lock (a Postgres application-level lock, here `s.billingLockKey`) that gates exactly-one finalize per (subscription, cycle).

The slice (the unit of work here) is composition: **a mid-cycle scan tick**. For every subscription with thresholds configured, it computes the partial-cycle total via the same aggregation path the cycle scan uses; when the threshold is crossed, it **calls into the existing finalize-cycle code path early** with `billing_reason="threshold"`. We do **not** duplicate the build-line-items or advance-cycle code — same code path, different entry point.

The unblock for Track B (the dashboard/UI workstream): the cost dashboard's projected-bill line gets a new "approaching threshold" indicator (red bar at 80%, lock icon at 100%) reading from a new `subscription.billing_thresholds` field on the existing GET endpoint.

## Goals

- **Per-subscription threshold configuration.** Tenants set `billing_thresholds` on a subscription via `PATCH /v1/subscriptions/{id}`. Two threshold types: `amount_gte` (currency-major-unit-cents int64) and `item_thresholds[]` (per-item `usage_gte` decimal). Either alone or both — first to fire wins.
- **Mid-cycle scan that fires the existing finalize path.** When a threshold crosses, the engine emits the same line set the next cycle scan would have emitted, finalizes with `billing_reason="threshold"`, and (when `reset_billing_cycle=true`) rolls the cycle forward as if the period had ended naturally.
- **Idempotent under retry.** The scheduler's existing advisory-lock leader gating (only the instance holding the lock runs a tick) already prevents two ticks from doing the same work; the threshold scan inherits this. Within a single tick, a subscription whose threshold has already fired in the current cycle is skipped (no second invoice).
- **Multi-dim parity.** `item_thresholds[]` use `usage.AggregateByPricingRules` so a multi-dim meter's running total respects rule priority + claim semantics (each event is counted by at most one rule, highest priority first), identical to the cycle scan.
- **Webhook event.** `subscription.threshold_crossed` fires when the threshold causes the early finalize. Standard webhook outbox path (the event row is written to the database and delivered by a background dispatcher); standard signature.
- **Wire-contract clean.** Snake-case JSON, decimal-string `usage_gte`, integer-cents `amount_gte`, always-array `item_thresholds`. Same ergonomics as preview/customer-usage.

## Non-goals (deferred)

- **Plan-level thresholds.** Stripe lets you configure thresholds on a price; Velox v1 attaches them to a subscription only. Per-plan defaults are a tenant-level concern that doesn't have a good home yet. Reproducing the precedence rule (sub overrides plan overrides product) invites the kind of decision tree that has to be unwound when wrong. Real consumer first.
- **Currency conversion for cross-currency thresholds.** A subscription with multiple line currencies cannot have a single `amount_gte`; v1 rejects multi-currency subscriptions with `amount_gte` set (validated at PATCH time). Multi-currency tenants surface as a documented edge in the changelog.
- **Threshold-driven email/dashboard alerts (the soft-warning path).** That's Week 5d (billing alerts); separate slice, separate sibling worktree, separate event family. The threshold here is the **hard cap that fires an invoice**, not the "you've used 80% of your monthly $X" soft notification.
- **Resetting the cycle to a partial-period boundary.** When `reset_billing_cycle=true`, the new cycle starts at the moment the threshold fired and runs for one full cycle's duration (calendar-month-equivalent). We do not preserve the original anniversary day (the day-of-month the cycle renewed on before the reset) — that's a Stripe quirk we can add later if a real consumer asks.
- **Coupon / credit application to the threshold-fired invoice.** Same boundary as create_preview (the upcoming-invoice preview endpoint): v1 finalizes subtotal-only thresholds. Discounts and credits apply at finalize via the existing pipeline if configured on the subscription, but the threshold *check* is computed against subtotal. Checking net of discount+credit could prevent a true threshold cross from firing — the wrong shape, because the customer's *raw* spend is what tenants want capped. Documented in changelog.

## Today's surface (in repo)

Everything we need already exists; this slice composes:

- `internal/billing/engine.go::Engine.billSubscription(ctx, sub, period, billingReason)` — already builds line items from base fee + per-meter aggregation, calls `CreateInvoiceWithLineItems`, applies tax + coupon. Threshold scan reuses this with `billingReason="threshold"`.
- `internal/billing/engine.go::Engine.advanceCycleOrCancel(ctx, sub, periodEnd, ...)` — rolls the subscription's cycle forward to the next period. Threshold scan reuses with the *crossed-at* time as the new `current_billing_period_start`.
- `internal/usage/service.go::Service.AggregateByPricingRules(ctx, tenantID, customerID, meterID, defaultMode, from, to)` — partial-cycle aggregation. Same window as the cycle scan, just computed at tick-time-now instead of period-end.
- `internal/billing/scheduler.go::Scheduler.runBillingCycleForMode(ctx, livemode)` — leader-gated under `s.billingLockKey`. New step inserted between step 0 (reconcile) and step 1 (RunCycle): scan thresholds.
- `internal/subscription/store.go::Store` — extended with `SetBillingThresholds`, `ClearBillingThresholds`, and `ListWithThresholds` (the candidate-set query for the scan).
- `internal/domain/webhook_outbound.go` — extended with `EventSubscriptionThresholdCrossed`.

The only **new** code is:

1. A `BillingThresholds` domain type + JSON/wire shape.
2. A migration adding columns to `subscriptions` and a `subscription_item_thresholds` aux table.
3. The new `billing_reason` column on `invoices` (with `'threshold'` in the CHECK constraint — also covers the existing implicit reasons `'subscription_cycle'` and `'manual'`).
4. The threshold scan loop in `internal/billing/threshold_scan.go`.
5. The `PATCH /v1/subscriptions/{id}` route (the existing handler doesn't have an item-level PATCH for the subscription itself yet; only items have one).

## Wire contract

> **Conventions** (consistent with `/v1/*`, see `docs/design-customer-usage.md` § wire-contract and `docs/design-create-preview.md`):
>
> - **Snake-case JSON keys.**
> - **Decimal `usage_gte` marshals as a precise string** (`"1000000.000000000000"`) per ADR-005.
> - **Integer-cents `amount_gte`.**
> - **Always-array `item_thresholds`.** Even when zero or one entry. (The dashboard's `.map()` over the array works in all cases.)

### `PATCH /v1/subscriptions/{id}`

```http
PATCH /v1/subscriptions/{id}
Authorization: Bearer <secret_key>
Content-Type: application/json

{
  "billing_thresholds": {
    "amount_gte": 100000,
    "reset_billing_cycle": true,
    "item_thresholds": [
      { "subscription_item_id": "vlx_subi_abc", "usage_gte": "1000000" }
    ]
  }
}
```

To clear: `{"billing_thresholds": null}`.

`amount_gte` is the running cycle subtotal (integer cents in the subscription's currency). When the partial-cycle subtotal **crosses** this threshold (i.e., this tick the running total is `>= amount_gte` and the previous tick was `<` or there was no previous tick), the engine fires an early finalize.

`item_thresholds[].usage_gte` is the running cycle quantity for the single meter rated by the item's plan. When the item's running quantity crosses this, same early-finalize fires. Item thresholds are scoped to the item's plan's *meter* — a base-fee-only plan cannot have an item threshold (validated at PATCH time).

`reset_billing_cycle` (default `true`) controls whether the cycle resets after the early finalize. `false` keeps the cycle running on its original schedule; the threshold-fired invoice is one extra invoice within the cycle, and the cycle's natural-boundary invoice still fires at period end (billing whatever residual usage accumulated between the threshold fire and the period end).

`item_thresholds` is an array; subscriptions with multiple items can configure per-item caps.

Response: the full updated subscription, hydrated with items and thresholds.

```json
{
  "id": "vlx_sub_xyz",
  "customer_id": "vlx_cus_abc",
  "status": "active",
  ...,
  "billing_thresholds": {
    "amount_gte": 100000,
    "reset_billing_cycle": true,
    "item_thresholds": [
      { "subscription_item_id": "vlx_subi_abc", "usage_gte": "1000000.000000000000" }
    ]
  }
}
```

### Error shapes

- `404 subscription_not_found` — subscription ID does not exist for the tenant.
- `400 invalid_request` — `amount_gte <= 0`, or `usage_gte` non-positive, or `item_thresholds[].subscription_item_id` not on this subscription, or `billing_thresholds` set while the subscription has multiple line currencies (multi-currency cap not supported in v1).
- `409 invalid_state` — subscription is canceled or archived (no point setting a threshold on a terminal subscription).

### Webhook event

`subscription.threshold_crossed` fires immediately after the threshold-fired invoice transitions to `finalized`. Payload includes the subscription, the invoice ID, and the threshold definition that fired. Standard outbox path; same signing.

## Internals

### Threshold scan tick

```go
// internal/billing/threshold_scan.go (sketch)
func (e *Engine) ScanThresholds(ctx context.Context, batchSize int) error {
    subs, err := e.subs.ListWithThresholds(ctx, e.livemode, batchSize)
    if err != nil { return err }

    for _, sub := range subs {
        if err := e.scanOneThreshold(ctx, sub); err != nil {
            // log + continue; next tick will retry
            slog.Error("threshold scan failed", "sub", sub.ID, "err", err)
        }
    }
    return nil
}

func (e *Engine) scanOneThreshold(ctx context.Context, sub domain.Subscription) error {
    // Skip subs that don't bill mid-cycle (no current period)
    if sub.CurrentBillingPeriodStart == nil { return nil }

    now := e.effectiveNow(sub)
    from := *sub.CurrentBillingPeriodStart
    to := now

    // Partial-cycle subtotal across all priced lines (same shape as billSubscription).
    runningSubtotal, runningPerItem, err := e.computePartialCycleTotals(ctx, sub, from, to)
    if err != nil { return err }

    crossed, reason := evaluateThresholds(sub.BillingThresholds, runningSubtotal, runningPerItem)
    if !crossed { return nil }

    // Re-fetch sub under tx to avoid double-fire in the rare case the
    // previous tick crossed the threshold but failed before advance.
    return e.fireThreshold(ctx, sub, from, to, reason)
}
```

`evaluateThresholds` compares running totals against the configured thresholds. It returns `(crossed, reason)` where `reason` documents which threshold tripped (used in the line item description and the webhook payload).

`fireThreshold` opens a tx, claims the subscription via the existing advisory lock keyed on `(billing, sub.ID)`, calls `Engine.billSubscription(ctx, sub, period, "threshold")`, and when `reset_billing_cycle=true` calls `advanceCycleOrCancel(ctx, sub, now, ...)` so the new cycle starts at `now`.

The advisory-lock claim is the **idempotency seam** — if a previous tick already fired the threshold for the current cycle, the cycle has already advanced; this tick's `from` is the new (post-advance) period start, and the running totals start at zero. The threshold can fire again in the new cycle, which is correct.

### Per-cycle once-fired guard

The naive failure mode: a tick crosses the threshold, `billSubscription` succeeds, but `advanceCycleOrCancel` fails (DB blip). The next tick would re-aggregate the same window, see the threshold still crossed, and try to fire again — producing a duplicate invoice.

The guard: `Engine.fireThreshold` uses the **existing** invoice-source-key dedup column on the `invoices` table. We add a new natural key `(tenant, subscription, cycle_start, billing_reason='threshold')` via a partial unique index — at most one threshold-fired invoice per cycle. The next tick's `INSERT … ON CONFLICT DO NOTHING` returns the existing row; the engine notices "already fired this cycle" and short-circuits before computing line items. Cycle reset is idempotent because it's keyed on `current_billing_period_start <= now` (re-running on an already-advanced row leaves it unchanged).

We don't need a new "threshold_fired_at" column — the existence of a finalized invoice with `billing_reason='threshold'` and the matching `billing_period_start` IS the marker.

### Scheduler wiring

`Scheduler.runBillingCycleForMode` already runs under `s.billingLockKey`. We add a step `0.5` between reconcile and the cycle scan:

```go
// step 0.5: threshold scan (mid-cycle early finalize)
if err := s.engine.ScanThresholds(ctx, 100); err != nil {
    slog.Error("threshold scan", "err", err)
    // continue — cycle scan still runs
}
```

Same lock, same fan-out per livemode. If one tick of the threshold scan takes longer than the tick interval, the next tick is gated by the leader lock — no double-execution.

### Migration

`0056_subscription_billing_thresholds.up.sql`:

```sql
-- Per-subscription billing thresholds. amount_gte is integer cents
-- (subscription currency); reset_billing_cycle controls whether the
-- cycle resets after the threshold fires. NULL on either column = no
-- amount-based threshold configured.
ALTER TABLE subscriptions
    ADD COLUMN billing_threshold_amount_gte BIGINT,
    ADD COLUMN billing_threshold_reset_cycle BOOLEAN NOT NULL DEFAULT TRUE,
    ADD CONSTRAINT subscriptions_billing_threshold_amount_gte_check
        CHECK (billing_threshold_amount_gte IS NULL OR billing_threshold_amount_gte > 0);

-- Per-item usage thresholds. One row per (subscription, item) pair
-- with a quantity threshold. NUMERIC(38,12) keeps decimal precision
-- consistent with the rest of usage rating.
CREATE TABLE subscription_item_thresholds (
    subscription_id        TEXT NOT NULL REFERENCES subscriptions(id) ON DELETE CASCADE,
    subscription_item_id   TEXT NOT NULL REFERENCES subscription_items(id) ON DELETE CASCADE,
    tenant_id              TEXT NOT NULL,
    usage_gte              NUMERIC(38, 12) NOT NULL CHECK (usage_gte > 0),
    created_at             TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (subscription_item_id)
);

CREATE INDEX idx_subscription_item_thresholds_subscription
    ON subscription_item_thresholds (subscription_id);

ALTER TABLE subscription_item_thresholds ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON subscription_item_thresholds
    FOR ALL TO velox_app
    USING (tenant_id = current_setting('app.tenant_id', TRUE))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', TRUE));

-- Threshold-fired invoices. New billing_reason column + the existing
-- implicit reasons. CHECK constraint allows NULL for legacy rows.
ALTER TABLE invoices
    ADD COLUMN billing_reason TEXT,
    ADD CONSTRAINT invoices_billing_reason_check
        CHECK (billing_reason IS NULL OR billing_reason IN (
            'subscription_cycle', 'subscription_create', 'manual', 'threshold'
        ));

-- Partial unique index: at most one threshold-fired invoice per
-- (tenant, subscription, cycle_start) pair. Idempotency seam for the
-- re-tick-after-failed-advance case.
CREATE UNIQUE INDEX idx_invoices_threshold_unique_per_cycle
    ON invoices (tenant_id, subscription_id, billing_period_start)
    WHERE billing_reason = 'threshold';

-- Scan candidate index: subscriptions with thresholds configured.
CREATE INDEX idx_subscriptions_billing_thresholds_amount
    ON subscriptions (id)
    WHERE billing_threshold_amount_gte IS NOT NULL;
```

`0056_subscription_billing_thresholds.down.sql`:

```sql
DROP INDEX IF EXISTS idx_subscriptions_billing_thresholds_amount;
DROP INDEX IF EXISTS idx_invoices_threshold_unique_per_cycle;
ALTER TABLE invoices
    DROP CONSTRAINT IF EXISTS invoices_billing_reason_check,
    DROP COLUMN IF EXISTS billing_reason;
DROP TABLE IF EXISTS subscription_item_thresholds;
ALTER TABLE subscriptions
    DROP CONSTRAINT IF EXISTS subscriptions_billing_threshold_amount_gte_check,
    DROP COLUMN IF EXISTS billing_threshold_reset_cycle,
    DROP COLUMN IF EXISTS billing_threshold_amount_gte;
```

### Invoice domain extension

`internal/domain/invoice.go`:

```go
type InvoiceBillingReason string

const (
    BillingReasonSubscriptionCycle  InvoiceBillingReason = "subscription_cycle"
    BillingReasonSubscriptionCreate InvoiceBillingReason = "subscription_create"
    BillingReasonManual             InvoiceBillingReason = "manual"
    BillingReasonThreshold          InvoiceBillingReason = "threshold"
)

// In Invoice struct:
BillingReason InvoiceBillingReason `json:"billing_reason,omitempty"`
```

The existing `billSubscription` accepts a new `billingReason` parameter; existing callers pass `BillingReasonSubscriptionCycle` and `BillingReasonSubscriptionCreate` (the proration paths become `BillingReasonSubscriptionCycle` for now — they're cycle-driven). The threshold scan passes `BillingReasonThreshold`.

## Tests

### Unit tests

- `evaluateThresholds` table-driven: amount-only crossed, amount-only not crossed, item-only crossed (single item), item-only crossed (multi-item, only second crosses), both configured, multi-currency rejected.
- `Service.SetBillingThresholds` validation: `amount_gte <= 0` → invalid_request, `usage_gte` non-positive-decimal → invalid_request, `subscription_item_id` not on sub → invalid_request, multi-currency sub with `amount_gte` → invalid_request, terminal sub → invalid_state.
- `decodeBillingThresholds` (handler-level JSON parse): null clears, empty `item_thresholds` accepted, decimal-string `usage_gte` round-trips precisely.
- **`TestWireShape_SnakeCase` (the merge gate):** marshal a real Subscription with thresholds set, assert every required snake_case key (`billing_thresholds`, `amount_gte`, `reset_billing_cycle`, `item_thresholds`, `subscription_item_id`, `usage_gte`), assert no PascalCase leaks, assert empty `item_thresholds` marshals as `[]` not `null`, assert `usage_gte` marshals as a string with exact value `"1000000.000000000000"` (no float coercion).

### Integration tests (real Postgres)

- `TestThresholdAmountCrossesFinalizesEarly` — seed a sub with `amount_gte=1000`, push usage events totaling 1500 cents, run a tick. Assert: finalized invoice exists with `billing_reason='threshold'`, cycle has advanced (`current_billing_period_start` is now `>= tick-time`), webhook outbox has `subscription.threshold_crossed` row.
- `TestThresholdItemCrossesFinalizesEarly` — same as above but with `item_thresholds[0].usage_gte=1000` units, push 1500 units across the meter.
- `TestThresholdReTickIsIdempotent` — fire the threshold once, run the scan again immediately. Assert: only one invoice was created (same ID), no duplicate webhook event.
- `TestThresholdResetFalseKeepsOriginalCycle` — `reset_billing_cycle=false`. Threshold fires, `current_billing_period_start` is unchanged, the cycle scan at period end still produces a second invoice.
- `TestThresholdMultiDimRespectsRules` — multi-dim meter with two rules (input @3¢, output @5¢), `amount_gte=1000`. Push usage that totals 1100 across both rules; threshold fires. Assert the threshold-fired invoice's lines mirror the cycle scan's per-rule line set.
- `TestThresholdSkipsTerminalSubscription` — a canceled sub with thresholds set is skipped by the scan tick.
- `TestThresholdSkipsTrialingSubscription` — a trialing sub with thresholds set is skipped (no current period until activated).

## Performance

- Per-tick: 1 indexed query for candidate subs (filtered by `billing_threshold_amount_gte IS NOT NULL OR EXISTS (item_thresholds)`), then 1 `AggregateByPricingRules` call per meter per candidate sub. For a tenant with 10K subs and a 100-batch tick, that's at most 100 aggregation calls per minute — well under the cycle scan's existing budget.
- Tick interval: piggybacks on the existing scheduler tick (`s.tickInterval`, default 30s). No new ticker.
- The "did the threshold cross since last tick?" check is **not** implemented — we just check "is the threshold currently crossed?". A tick that ran during the cross window will fire it; a tick that runs after `fireThreshold` already advanced the cycle will see the new (post-advance) running total, which starts at zero. The unique partial index on `(tenant, subscription, billing_period_start)` is the duplicate-prevention seam, not a "since last tick" check.

## Decimal & numeric considerations

`usage_gte` is `NUMERIC(38, 12)` in Postgres and `decimal.Decimal` in Go, marshaled as a JSON string per ADR-005. `amount_gte` is `BIGINT` in Postgres and `int64` in Go; integer cents.

The `item_thresholds` array is stored as an aux (auxiliary) table because per-item config doesn't compose cleanly into a single row's columns, and JSONB on a hot-path-aggregation row is a footgun (no per-item indexes; query plans surprise people). One row per item is the same shape as `subscription_items` itself.

## Open questions

1. **Should `reset_billing_cycle=false` be supported in v1?** Stripe allows it. **Proposal: yes.** Simple to implement (just skip the `advanceCycleOrCancel` call), and it covers the tenant case "I want a hard cap *plus* a normal monthly bill" — the threshold-fire becomes an extra invoice rather than a cycle boundary. The integration test confirms the second invoice fires at period end.
2. **Should the threshold-fired invoice's `billing_period_end` be the original cycle end or the fire time?** **Proposal: the fire time.** Stripe-equivalent: the invoice's billing period is the partial cycle that produced the spend. The new cycle (when reset_billing_cycle=true) starts at fire time and gets a fresh natural cycle end. When reset_billing_cycle=false, the second invoice's billing period is `(fire_time, original_period_end)`, which means the period covered by the threshold-fired invoice is `(period_start, fire_time)`.
3. **Should we support `usage_gte` as an integer instead of a decimal?** Stripe uses integer "quantity" for usage thresholds. **Proposal: decimal.** Velox already exposes decimal quantities in customer-usage and create_preview; consistency matters. A tenant who wants integer behavior just sets `"1000.000000000000"`. The aggregation always returns a decimal anyway.
4. **Should we preview the threshold-fire in `create_preview`?** **Proposal: defer to v2.** The preview's job is "what will the natural-cycle invoice look like"; a threshold-fired invoice is a separate event. We could add a `would_threshold_fire: true` flag but it muddles the response shape. Real consumer first.
5. **Should the threshold scan run independently of the cycle scan tick interval?** **Proposal: same tick.** A separate ticker would mean a second advisory-lock domain, more code paths to debug. The existing 30s tick is granular enough that the worst-case delay between threshold-cross and finalize is 30s — well within tenant tolerance for "early finalize", which is a soft-real-time event not a hard-real-time one.
6. **Should we emit `subscription.threshold_warning` at 80% of threshold?** That's Week 5d (billing alerts) — different feature. Linked, but not co-shipped.
7. **Should `amount_gte` be denominated in the subscription's currency or platform-canonical USD?** **Proposal: subscription currency.** A multi-currency tenant configures different thresholds per currency (different subscriptions). Avoids FX conversion in the hot scan path.

## Implementation checklist

- [x] `internal/domain/subscription.go` — `BillingThresholds` struct + `SubscriptionItemThreshold`.
- [x] `internal/domain/invoice.go` — `InvoiceBillingReason` enum + `BillingReason` field on `Invoice`.
- [x] `internal/platform/migrate/sql/0056_subscription_billing_thresholds.{up,down}.sql`.
- [x] `internal/subscription/store.go` — `SetBillingThresholds`, `ClearBillingThresholds`, `ListWithThresholds`.
- [x] `internal/subscription/postgres.go` — implementations.
- [x] `internal/subscription/service.go` — validation + `SetBillingThresholds` method.
- [x] `internal/subscription/handler.go` — `PATCH /v1/subscriptions/{id}` route.
- [x] `internal/billing/threshold_scan.go` — `Engine.ScanThresholds` + `scanOneThreshold` + `fireThreshold`.
- [x] `internal/billing/scheduler.go` — `ScanThresholds` wired into `runBillingCycleForMode`.
- [x] `internal/billing/engine.go` — `billSubscription` accepts `billing_reason`.
- [x] `internal/invoice/postgres.go` — `CreateInvoiceWithLineItems` persists `billing_reason`.
- [x] `internal/domain/webhook_outbound.go` — `EventSubscriptionThresholdCrossed`.
- [x] `internal/subscription/wire_shape_test.go` — `TestWireShape_SnakeCase` regression.
- [x] Unit tests for service validation + handler PATCH path + `evaluateThresholds`.
- [x] Integration tests covering the seven scenarios listed above.
- [x] CHANGELOG entry + public changelog rollup.

## Track B unblock

Track B can wire the cost dashboard's "approaching threshold" indicator off the existing GET /v1/subscriptions/{id} response (which now includes `billing_thresholds`) plus the existing `customer-usage` projected-bill total. Math is `100 * customer_usage.totals[0].amount_cents / billing_thresholds.amount_gte` for the percentage.

The dashboard call:

```ts
const sub = await api.getSubscription(subscriptionID)
const usage = await api.customerUsage({ customer_id: sub.customer_id })
const pct = sub.billing_thresholds
  ? Math.min(100, 100 * usage.totals[0].amount_cents / sub.billing_thresholds.amount_gte)
  : null
```

The dashboard shape:

- **Threshold progress bar** under the projected-bill line. Color: green < 50%, amber 50-80%, red > 80%, lock icon at 100%.
- **Edit-threshold modal** (gear icon next to the bar) lets tenants set/clear the threshold via the new PATCH route.

Track B implements the UI; the API stabilizes in this slice.

## Review status

- **Track A author:** drafted 2026-04-26 alongside the implementation
- **Track B review:** pending — UI wedge can scaffold against this design without waiting
- **Human review:** pending — flag any open question to revisit
