# ADR-028: Billing engine — per-sub period loop + disjoint flows for catchup vs cron

**Status:** Accepted
**Date:** 2026-05-05

## Summary

Billing a subscription now bills every due period in one call: `billSubscription` repeats `billOnePeriod` until `next_billing_at` is after the subscription's effective now (wall-clock time, or the test clock's `frozen_time` for a subscription on a clock), with a safety cap of 10000 periods. It also stops early when a period does not move `next_billing_at` forward. The wall-clock scheduler and the test-clock Advance now bill separate sets of subscriptions. `GetDueBilling` returns only subscriptions with no `test_clock_id`, and `GetDueBillingForClock` returns only the subscriptions on one clock whose `next_billing_at` has reached its `frozen_time`. Before, a large Advance stopped after 120 passes, and the scheduler raced it and billed test-clock subscriptions one period per tick with no operator action. ADR-029 (2026-05-08) applied the same split to threshold scans, pending-charge retry, dunning, credit expiry, tax retry and invoice reminders, so test-clock time now moves only when the operator advances it. Amended 2026-05-04: one Advance may move `frozen_time` by at most 1 year.

## Context

Operator clicked Advance on a test clock at frozen_time = 2053 with a sub
whose `next_billing_at` was 2028. Instead of catching up the ~300 missing
periods in one operator action, only ~25 invoices generated and the sub
sat stuck — drip-billed thereafter at 1 invoice per 5-minute scheduler
tick. End-to-end audit surfaced two architectural issues, not one:

### Issue 1 — `billSubscription` was single-period

`Engine.billSubscription` advanced exactly one billing period per call.
The wall-clock production scheduler was built around this (a sub
naturally accumulates one period of debt before the next tick — perfect
fit). The test-clock catchup worker inherited the same primitive but
its job is fundamentally different: compress N periods of accumulated
simulated time into one operator action.

The pre-fix design wrapped this primitive in an outer loop in the
test-clock service (`runCatchupLoop`) capped at `MaxAdvanceCatchupLoops = 120`
passes. A 25-year monthly catchup needs 300 passes — the cap fired,
status flipped to `internal_failure`, retries hit the same cap, and the
sub stayed stuck.

### Issue 2 — wall-clock cron and test-clock catchup overlapped

Both flows called the same `engine.RunCycle`, which used a single
`GetDueBilling` SQL query that filtered subs by either wall-clock-now
(for non-clock-pinned subs) or test-clock frozen_time (for clock-pinned
subs). Three follow-on smells:

- **SKIP-LOCKED race.** `GetDueBilling` uses `FOR UPDATE OF s SKIP LOCKED`
  for safe concurrent scheduler ticks. When the wall-clock cron tick
  fired during a test-clock catchup run, both racing on the same
  clock-pinned sub: whoever locked first won, the other returned `n=0`
  rows. The test-clock catchup loop interpreted `n=0` as "caught up"
  and exited cleanly while the sub was still due.
- **Drip-bill artifact.** Even after the catchup worker exited, the
  wall-clock cron's 5-minute (dev) / 1-hour (prod) tick continued
  picking up clock-pinned subs and billing one period per tick. The
  stuck sub crept forward at 1/period/tick. Operators watching their
  dashboard saw invoice counts grow with no Advance action — confusing
  AND violating the operator's mental model that simulated-time
  progression is operator-controlled.
- **Operator-controlled simulation, undermined.** The whole *point* of
  a test clock is for the operator to drive simulated time. A
  background cron quietly billing on simulated time without an Advance
  click is a category error — it makes simulation time ambient instead
  of explicitly operator-controlled.

### Industry comparison

Stripe Test Clocks have NO background cron touching clock-pinned
customers. Advance is the SOLE path for clock-pinned billing. Their
billing engine vectorises period generation per sub during advance —
no per-period outer loop needed by the caller. Velox was an outlier on
both axes.

## Decision

**Two architectural changes, shipped together:**

### 1. `billSubscription` loops internally per sub

Extracted the existing `billSubscription` body to a new internal
`billOnePeriod` (unchanged behaviour). The outer `billSubscription`
wraps it in a loop:

```go
func (e *Engine) billSubscription(ctx, sub) (count int, err error) {
    for i := 0; i < maxPeriodsPerSubPerCall; i++ {
        if err := ctx.Err(); err != nil { return count, ctx-err }
        sub, _ = e.subs.Get(ctx, sub.TenantID, sub.ID)  // refresh
        now := e.effectiveNow(ctx, sub)
        if sub.NextBillingAt == nil || sub.NextBillingAt.After(now) {
            return count, nil  // caught up
        }
        invoiced, err := e.billOnePeriod(ctx, sub)
        if err != nil { return count, err }
        if invoiced { count++ }
        // No-progress guard: if billOnePeriod skipped without
        // advancing next_billing_at, exit cleanly (sub paused, no
        // items, etc.).
    }
    return count, fmt.Errorf("per-sub safety cap %d hit", maxPeriodsPerSubPerCall)
}
```

`maxPeriodsPerSubPerCall = 10000` (covers 833 years monthly, 27 years
daily). The 10-min `CatchupTimeout` on the worker's ctx is the outer
ceiling for total operation duration.

This collapses three previous concerns into one:
- The per-sub period loop replaces `runCatchupLoop`'s outer pass loop.
- The `MaxAdvanceCatchupLoops = 120` cap is removed; the per-sub
  safety counter is a much higher implicit ceiling.
- The "n==0 means caught up" early-exit logic in the old outer loop
  is replaced with explicit "next_billing_at past now" checks at the
  inner level.

The wall-clock production scheduler benefits too — when a tick runs,
each sub catches up fully in one pass, even if it fell behind by N
periods (e.g., scheduler outage + recovery).

### 2. Wall-clock cron and test-clock catchup operate on disjoint subs

The two flows now use **different SQL queries** that produce
**non-overlapping result sets**:

- `GetDueBilling` (wall-clock cron): filters `WHERE test_clock_id IS NULL`.
  Returns ONLY production subs. Used by `Engine.RunCycle`.
- `GetDueBillingForClock(clockID)` (operator advance): filters
  `WHERE test_clock_id = $1 AND next_billing_at <= clock.frozen_time`.
  Returns ONLY this clock's pinned subs. Used by new
  `Engine.RunCycleForClock`.

`Service.RunCatchup` calls `RunCycleForClock` instead of `RunCycle`.
The wall-clock scheduler tick continues calling `RunCycle` (now
narrower scope, same behaviour for non-clock subs).

This eliminates:
- The SKIP-LOCKED race (cron and advance lock disjoint row sets).
- The drip-bill artifact (cron never touches clock-pinned subs).
- The operator's mental-model confusion (simulation time progresses
  ONLY when the operator advances the clock).

Stripe-parity confirmed: their cron never touches clock-pinned
customers either.

## Consequences

**Operator-visible behaviour:**
- **Click Advance once → catchup completes for ALL pinned subs of that
  clock.** Even a 25-year advance on a multi-sub customer finishes in
  one operator action; `internal_failure` becomes a real diagnostic
  signal (something genuinely broke), not a "you advanced too far"
  surface.
- **No background drip-bill.** Stuck clock-pinned subs only progress
  via Retry advance or a fresh Advance click. If a sub somehow ends
  up with `next_billing_at < clock.frozen_time` between advances (rare
  edge case), it stays that way until the operator notices and acts —
  matching Stripe.

**Code paths removed:**
- `MaxAdvanceCatchupLoops = 120` constant.
- The old outer pass loop in `runCatchupLoop`.
- `runCatchupLoop` itself (folded into `RunCatchup`).
- `envCatchupDelay` / `catchupDelayFromEnv` in `internal/testclock/`
  (moved into `internal/billing/engine.go` since pacing now operates
  at the inner per-period level).

**Code paths added:**
- `billOnePeriod` (pure rename of old `billSubscription` body).
- New `billSubscription` outer loop wrapper.
- `Engine.RunCycleForClock(tenantID, clockID, batchSize)`.
- `SubscriptionReader.GetDueBillingForClock(tenantID, clockID, limit)`.
- Per-sub safety counter `maxPeriodsPerSubPerCall = 10000`.

**Scheduler tasks unchanged in scope — DEFERRED WORK COMPLETED 2026-05-08
via ADR-029.**
- `Engine.ScanThresholds`, `Engine.RetryPendingCharges`, dunning
  processor, credit expiry, tax retry, invoice reminders — six
  paths originally deferred from this ADR have been closed under
  the same disjoint-flow pattern. Each path now has a
  `*ForClock(clockID, ...)` variant called by the catchup
  orchestrator, and each cron-side query filters out clock-pinned
  entities via `NOT EXISTS` on the subscription's `test_clock_id`.
- The catchup orchestrator (`testclock.Service.RunCatchup`) drives
  all six concerns in lockstep with simulated time per Advance click.
  Operator's "I drive time" mental model now holds end-to-end across
  every time-aware engine path. Stripe Test Clocks parity exact.
- See `docs/adr/029-fully-disjoint-test-clock-flows.md` for the full
  six-phase breakdown, orchestration sequence, and per-phase test
  strategy.

**Lesson learned (memorialized in `feedback_long_term_means_cross_flow_audit`):**
The original "deferred decoupling" framing in this ADR was a smell —
when designing a contract that the rest of the engine has to honor
(in this case "operator drives time"), the audit of every flow that
touches the contract should happen in the SAME design pass. ADR-029's
existence is evidence ADR-028 was scoped wrong; future ADRs that
introduce a contract should enumerate every consumer in the original
deliverable, not punt them to follow-up work.

**Invariants reinforced:**
- A sub is processed by exactly ONE flow based on its `test_clock_id`
  field. No flow sees both kinds.
- Operator-visible simulation time progression == operator click. No
  ambient progression.
- The 10-minute `CatchupTimeout` on the worker is the only outer
  bound on a single advance; no per-pass cap is needed.

## Alternatives considered

- **Raise `MaxAdvanceCatchupLoops` to a high number (e.g., 5000).**
  A symptom fix that keeps the architectural smell. Doesn't address
  the SKIP-LOCKED race or the drip-bill behaviour. Rejected.
- **Use `FOR UPDATE WAIT` instead of SKIP LOCKED.** Solves the race
  without solving the dual-flow issue. Worse — adds blocking-wait
  latency to scheduler ticks. Rejected.
- **Keep `MaxAdvanceCatchupLoops` as a defensive guard.** The 10-min
  `CatchupTimeout` already bounds runaway loops. The per-sub safety
  counter (`maxPeriodsPerSubPerCall`) catches per-sub bugs. Two-layer
  cap redundant. Rejected — kept the inner one only.
- **Couple advance to scheduler tick (drop test-clock worker entirely).**
  Operator clicks Advance, status flips to advancing, then waits up
  to 5 min (dev) or 1 hour (prod) for the next scheduler tick. Bad
  UX — advance feedback loop becomes async-with-no-clear-signal.
  Rejected.

## Tests

- `internal/billing/engine_test.go` — existing tests adapted to the
  new multi-period semantics. Where a test wanted "exactly 1 invoice"
  it now passes a fake clock pinned just past the period boundary so
  only one period is due. New helper `billingTestClock()` returns a
  fake clock for the common case.
- `internal/billing/engine_test.go` mock `mockSubs.GetDueBilling`
  filters out clock-pinned subs (the new SQL invariant); new
  `mockSubs.GetDueBillingForClock` matches the catchup path.
- `internal/testclock/service_test.go` — `stubRunner` now implements
  `RunCycleForClock` and mirrors the engine's per-sub period loop.
  `TestAdvance_RunsBillingUntilQuiet` asserts `runner.calls == 1`
  (post-ADR-028 single call) and verifies the sub catches up past the
  target frozen_time.

## Amendment 2026-05-04 — per-advance window cap (Stripe parity)

A single `Advance` call may shift `frozen_time` by **at most 1 year**.
Larger ranges are chunked into successive operator clicks. Enforced in
`Service.Advance` via `current.FrozenTime.AddDate(1, 0, 0)`; the SPA's
Advance dialog gates the submit button on the same window so the
operator sees the constraint before round-tripping.

**Why a cap (Stripe parity):**
- Predictable per-click resource use — a single advance can't trigger a
  multi-decade catchup that overruns the 10-min worker timeout and
  leaves the clock in `internal_failure` for the operator to clean up.
- Iteration discipline — each chunk's invoices, payments and dunning
  state is reviewable before the next advance. A 25-year jump in one
  click hides regressions that would have been obvious year-by-year.
- Failure isolation — a bug surfacing in year 14 doesn't poison
  earlier years' simulated billing run.

**Why exactly 1 year (and not, e.g., 1 month):**
- Annual subscriptions need to close at least one full cycle per
  advance to be useful — a sub-monthly cap would force an annual sub
  through 12 advances just to hit one renewal.
- Stripe's documented test-clock cap is also "≤ 1 year per advance".

**Failure mode is a typed error**:
`errs.Invalid("frozen_time", "advance cannot exceed 1 year per call — chunk longer ranges into successive advances (Stripe parity)")`.
The dialog renders the same guidance inline and points at the maximum
allowed target so the operator can fix the picker without guessing.

`time.AddDate(1, 0, 0)` is leap-year correct (`Feb 29 → Feb 28/29` per
year alignment) — using a fixed nanosecond duration would drift on
leap years.

## Compatibility

- API surface unchanged. Operators see identical UI semantics
  (Advance, Retry advance, Delete) — just with predictable behaviour
  for large advances.
- DB schema unchanged.
- Existing test clocks in `internal_failure` from pre-ADR-028 cap-hit
  scenarios will catch up cleanly on the next Retry advance.
