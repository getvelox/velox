# ADR-065: Threshold-scan kernel — boundary-skip, fire-once probe, full drain

**Date:** 2026-07-02
**Status:** Accepted

## Context

Billing thresholds (spend caps) let an operator configure a subscription to
fire an **early-finalize invoice** mid-cycle once its running total crosses a
cap — `AmountGTE`, a per-item quantity cap, or a `max`/`last` variant. The
scheduler calls `Engine.ScanThresholds` on every tick (between the auto-charge
retry sweep and the natural-cycle scan); `scanOneThreshold` evaluates one
subscription, and `fireThreshold` emits the invoice. The clock-pinned catchup
path is the disjoint `ScanThresholdsForClock` (ADR-029 Phase 3).

The full-product audit (2026-07-02, finding cluster P1) found three defects in
this path, all in the class "the scan re-evaluates state it should have treated
as closed." They are independent but share one root cause: **`scanOneThreshold`
reasoned locally about a single tick and never enumerated what happens across
the period boundary, across a re-tick, or across a candidate set larger than one
page** (the money-path playbook's classes F "incomplete-set read", A
"exactly-once", G "liveness"). This ADR records the three decisions so a future
session does not re-litigate them, and so the deliberately-accepted semantic
loss (no `threshold_crossed` event for a boundary-observed crossing) is on the
record rather than discovered as a "bug" later.

## Decision

### 1. Boundary — SKIP, do not clamp-and-fire

Once `now` reaches `current_billing_period_end`, `scanOneThreshold` returns
without firing:

```go
if !now.Before(*sub.CurrentBillingPeriodEnd) {
    return false, nil
}
```

**Why this is the bug.** `fireThreshold` bills the window `[periodStart, now)`.
When a crossing is first *observed* on or after the boundary tick — scheduler
downtime spanning the boundary, or the crossing landing in the last inter-tick
window — `now` has already advanced past `periodEnd`, so the window spills into
the **next** period. The natural cycle close's dedup watermark
(`LatestThresholdPeriodEnd`) is scoped to *this* period's threshold invoices, so
the spilled usage is billed a second time at the next cycle close:
**double-billing across the boundary.**

**Why skip and not clamp.** Clamping the window to `[periodStart, periodEnd)`
would be money-correct, but it would route mid-period-swap and
scheduled-cancel subscriptions through `fireThreshold`'s crude path —
`fireThreshold` has no proration segmentation, no item-change handling, and no
terminal-cancel logic. The **same-tick natural cycle scan runs immediately
after** (scheduler step order: thresholds → cycle close, single goroutine) and
bills the whole elapsed period through the full-fidelity `billOnePeriod` path.
Deferring to it is strictly more correct than clamp-and-fire.

**Accepted semantic loss.** A crossing first observed on/after the boundary
emits **no** `subscription.threshold_crossed` event and no early-finalize
invoice — the operator-visible artifact is the cycle-close invoice instead. This
is acceptable: the threshold's *purpose* (bound in-cycle spend / trigger an
early charge) is moot once the cycle is closing anyway, and the money is billed
exactly once. We do not emit a synthetic event for the closed window.

### 2. Fire-once — probe before evaluate, index stays the correctness seam

Before the expensive evaluation, `scanOneThreshold` probes for an existing
threshold invoice this cycle and short-circuits:

```go
if _, err := e.invoices.LatestThresholdPeriodEnd(ctx, sub.TenantID, sub.ID, periodStart, *sub.CurrentBillingPeriodEnd); err == nil {
    return false, nil // already fired this cycle
} else if !errors.Is(err, errs.ErrNotFound) {
    return false, fmt.Errorf("probe threshold invoice for cycle: %w", err)
}
```

**Why this is the bug.** With `reset_billing_cycle=false` the cycle stays put
after a fire, so the crossed running total is still crossed on the next tick.
Pre-fix, the scan re-evaluated and re-fired **every tick**. Each doomed re-fire
ran `ApplyTaxToLineItems` (a *paid* Stripe Tax calculation) and
`NextInvoiceNumber` — which commits the counter increment in its own
transaction — **before** `CreateInvoiceWithLineItems` bounced off the partial
unique index (`idx_invoices_threshold_unique_per_cycle`, 0056). Result: a
permanent gap in the tenant's invoice-number sequence and a billable tax API
call on every tick until the period rolled — on the order of ~600 burned
numbers + paid calls per cycle for a busy scheduler.

**This probe is an optimization, not the exactly-once mechanism.** Two
concurrent scans can both pass it (check-then-act TOCTOU). The **partial unique
index remains the correctness seam**: the loser lands on `errs.ErrAlreadyExists`
and `fireThreshold` short-circuits, exactly as before. The probe's job is to make
the *common* re-tick cost one indexed lookup instead of a preview aggregation +
tax call + burned number. Blind-scanning on a probe error would re-introduce the
exact burn the probe exists to prevent, so a non-`ErrNotFound` probe error
**fails loud** and retries next tick (playbook class E, no silent fallback).

### 3. Drain — cursor the whole candidate set, do not stop at one page

`ScanThresholds` and `ScanThresholdsForClock` cursor-drain the candidate set by
a strictly-increasing `id`:

```go
afterID := ""
for {
    candidates, err := e.subs.ListWithThresholds(ctx, livemode, afterID, batchSize)
    // ... scan each ...
    afterID = candidates[len(candidates)-1].ID
    if len(candidates) < batchSize { break } // short page = drained
}
```

**Why this is the bug.** Pre-fix the scan fetched a single
`ORDER BY s.id LIMIT batchSize` page per tick, with no cursor. A fired sub still
has thresholds configured, so `ListWithThresholds` always returns the *same*
first page — the candidate set **never drains**, and every subscription beyond
the first `batchSize` was **never scanned**. Spend caps were silently disabled
for the (batchSize+1)-th threshold subscription onward.

The `id` cursor (`AND s.id > $afterID ORDER BY s.id ASC`) makes the loop
**terminate by construction** — no max-pages belt-and-suspenders needed
(playbook: no belt-and-suspenders) — and it advances past a failing sub so one
bad row can neither wedge the scan nor cause a re-evaluation within the same
tick. The fire-once probe (decision 2) is what keeps a full drain affordable:
already-fired subs cost one indexed lookup, not a `previewWithWindow`
aggregation.

## Consequences

- A boundary-observed crossing produces no `threshold_crossed` event; the cycle
  invoice is the artifact. Documented; not a bug report.
- Threshold invoices are emitted at most once per `(sub, period_start)` by the
  index, and now *evaluated* at most once per cycle by the probe under normal
  (non-racing) operation.
- Every threshold subscription is scanned every tick regardless of fleet size.
- Test locks (mutation-verified): `TestScanThresholds_FireOnceProbe_NoReEvaluate`,
  `TestScanThresholds_DrainsPastBatchSize`, `TestScanOneThreshold_BoundarySkip`
  (unit), and `TestThresholdScan_Idempotent` extended with an invoice-number
  no-burn assertion on real Postgres.

## Follow-ups (out of scope for this ADR)

The `reset_billing_cycle=true` proration granularity, the `max`/`last` threshold
variants' interaction with cycle close, and the `$0`-threshold guard are a
separate change (audit cluster P1b) and will be recorded when they ship. This
ADR covers only the three kernel defects above.

## Amendment 2026-08-30 (ADR-115)

The boundary skip's justification above leans on "the same-tick natural cycle
scan runs immediately after (scheduler step order: thresholds → cycle close,
single goroutine)". That ordering no longer carries any correctness: since
ADR-115 every threshold fire and every cycle close proves the period snapshot
it read — status, period start, watermark — in the first statement of its own
transaction, under the subscription row lock, and the close re-reads the
threshold watermark inside that transaction. A fire and a close from any
goroutine, any replica, or an operator's `POST /v1/billing/run` can no longer
both bill the same window; whichever commits second writes nothing. The skip
stays as what it always also was: the optimization that keeps a
boundary-observed crossing on the full-fidelity `billOnePeriod` path. The
fire-once probe likewise stays a probe; the reset=false fire now takes the row
lock too (a verify-only `ClosePeriodTx`), so two concurrent fires serialize on
the row before the partial index ever sees the second.
