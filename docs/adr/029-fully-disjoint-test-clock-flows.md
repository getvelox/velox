# ADR-029: Fully disjoint test-clock flows across every time-aware engine path

**Status:** Accepted
**Date:** 2026-05-08
**Implemented**: 2026-05-08 (single session, six phases)
**Supersedes**: amends ADR-028 (deferred-decoupling section — now resolved)

## Summary

Every time-driven scheduler job (auto-charge retry, tax retry, threshold scan, dunning, credit expiry) is split into two separate flows. The wall-clock cron query excludes entities attached to a test clock. A per-clock variant (`*ForClock`) processes those entities only when an operator clicks Advance, through `testclock.CatchupWorker`. Each entity is processed by exactly one flow, so the cron can no longer charge a test-clock invoice, and simulated time moves only on Advance. The tax-commit reconciler (`RetryPendingTaxCommit`) also excludes test-clock invoices but has no per-clock variant, because those invoices are test-mode only. Amended 2026-06-13: the invoice-reminder job was removed.

## Context

ADR-028 made the wall-clock cron and the operator-Advance catchup
flows disjoint for **period generation only**:

- `GetDueBilling` filters `WHERE test_clock_id IS NULL` → cron sees only
  non-clock subs.
- `GetDueBillingForClock` is the catchup-only path.

The same ADR explicitly deferred the rest of the time-aware engine
paths (`ScanThresholds`, `RetryPendingCharges`, `RetryPendingTax`,
dunning, credit expiry, invoice reminders) to "future Stripe-parity
work." That deferred state is the gap that surfaced 2026-05-08 when an
operator with a clock-pinned customer + payment method attached saw a
real Stripe charge fire from the wall-clock 5-min tick — the
auto-charge retry path was never split, so the cron picked up the
clock-pinned `auto_charge_pending` invoice and processed it.

Six paths still mix wall-clock cron processing with clock-pinned
entities. Each is a small leak in the simulation contract; the
collective is the gap between "test-clock-correct" and
"kind-of-test-clock."

### Industry reference

Stripe Test Clocks specify (and enforce) full simulation purity:

> *"All time-based operations on objects associated with a test clock
> are driven by the test clock's simulated time."*

Their wall-clock infrastructure never touches clock-pinned objects.
Every state transition — invoice finalize, charge attempt, dunning,
subscription auto-cancel, trial conversion, tax recalc, credit expiry
— is driven by the clock-advance event. Recurly's Test Mode follows
the same model.

ADR-028 already chose this contract for period generation. ADR-029
extends it to every remaining time-aware path so the contract is
honored end-to-end.

## Decision

Every wall-clock scheduler step that touches clock-pinned entities
must be split using the ADR-028 pattern:

1. **Cron-side SQL filter** that excludes clock-pinned entities
   (directly or via JOIN).
2. **Per-clock variant** (`*ForClock(clockID)`) called by the catchup
   orchestrator on Advance.
3. **Catchup orchestrator** (`Engine.RunCycleForClock` + the
   `testclock.CatchupWorker`) drives each per-clock variant in lockstep
   with simulated time.

The disjoint pattern guarantees:

- A clock-pinned entity is processed by exactly ONE flow (the catchup
  on Advance), never by the cron.
- A non-clock entity is processed by exactly ONE flow (the cron),
  never by the catchup.
- No SKIP-LOCKED race between cron and catchup (already proven for
  period generation in ADR-028).
- Operator's mental model upheld: simulation time progresses ONLY on
  Advance, full stop, for every consequence.

## The six paths and their splits

For each: the entity, its clock relationship, the cron-side SQL
predicate to add, and the per-clock variant to introduce.

### 1. Auto-charge retry

| | Existing | After |
|---|---|---|
| **Entity** | `invoices` with `auto_charge_pending=true` | same |
| **Cron filter** | none | `AND NOT EXISTS (SELECT 1 FROM subscriptions s WHERE s.id = i.subscription_id AND s.test_clock_id IS NOT NULL)` |
| **Per-clock variant** | none | `ListAutoChargePendingForClock(ctx, tenantID, clockID, limit)` |
| **Time predicate** | wall-clock now (cron tick fires every 5min) | clock.frozen_time at catchup time |
| **Stripe call** | real (test or live keys per livemode) | real, same — only the trigger differs |

Engine entry: `Engine.RetryPendingCharges` becomes the cron path;
`Engine.RetryPendingChargesForClock` is added for catchup.

### 2. Threshold scan

| | Existing | After |
|---|---|---|
| **Entity** | `subscriptions` with `billing_thresholds` set | same |
| **Cron filter** | none | `WHERE s.test_clock_id IS NULL` on `ListWithThresholds` |
| **Per-clock variant** | none | `ListWithThresholdsForClock(ctx, tenantID, clockID, limit)` |
| **Time predicate** | wall-clock now (compares running cycle subtotal to threshold) | clock.frozen_time during catchup |

Engine entry: `Engine.ScanThresholds` becomes cron path;
`Engine.ScanThresholdsForClock` is added for catchup.

Threshold scan is partially in the catchup loop already (per-period
inside `billOnePeriod`), but the cron-side scan still touches
clock-pinned subs. Split the cron-side query and rely on the per-period
threshold check inside catchup as the canonical clock-pinned path.

### 3. Tax retry

| | Existing | After |
|---|---|---|
| **Entity** | `invoices` with `tax_status IN ('pending','failed')` and a retryable code | same |
| **Cron filter** | `tax_next_retry_at` predicate | `AND NOT EXISTS (clock pinning JOIN)` |
| **Per-clock variant** | none | `RetryPendingTaxForClock(ctx, tenantID, clockID, batch)` |
| **Time predicate** | wall-clock now | clock.frozen_time |

Service entry: `Service.RetryPendingTax` becomes the cron path
(`internal/invoice/service.go`); `Service.RetryPendingTaxForClock` is
added for catchup.

ADR-019's Stripe-reconnect-flush remains; that's a different trigger
(operator-driven, not time-driven) and stays cross-mode.

**Tax-commit reconciler — intentionally NOT split (added #267, 2026-06-14).**
A sibling reconciler, `RetryPendingTaxCommit`, recovers a *different* orphan:
a finalized invoice whose Stripe `tax_transaction` committed but whose local
`tax_transaction_id` write failed/crashed. Its cron query (`ListPendingTaxCommit`)
excludes clock-pinned invoices per the rule above, but — unlike tax retry — it
has **no** `*ForClock` half, so a clock-pinned commit orphan is never
auto-recovered. This deviation from the "every excluded path gets a per-clock
variant" pattern is deliberate: clock-pinned invoices are test-mode by
construction (test clocks are `livemode=false` by CHECK constraint → `sk_test_`
Stripe account, no real tax authority), so a stranded id only breaks *simulated*
reversal fidelity, recoverable by replaying the clock. Build the `*ForClock`
half only when a design partner's test-clock void/credit-note simulation reports
a silently no-op'd tax reversal.

### 4. Dunning advance

| | Existing | After |
|---|---|---|
| **Entity** | `invoice_dunning_runs` with `next_action_at <= now()` | same |
| **Cron filter** | `due_at` predicate by tenant | `AND NOT EXISTS (clock pinning JOIN through the invoice→sub)` |
| **Per-clock variant** | none | `ListDueRunsForClock(ctx, tenantID, clockID, frozenTime, limit)` |
| **Time predicate** | wall-clock now | clock.frozen_time |

Service entry: `Service.ProcessDueRuns` becomes cron path;
`Service.ProcessDueRunsForClock` is added for catchup.

Dunning is the most architecturally invasive split because the dunning
state machine has its own per-tick model (`runDunningHalf` in
scheduler). The catchup must drive a single sweep of due dunning runs
for the clock's invoices in one pass, not on a tick cadence.

### 5. Credit expiry

| | Existing | After |
|---|---|---|
| **Entity** | `credit_grants` with `expires_at < now()` | same |
| **Cron filter** | none | `AND NOT EXISTS (SELECT 1 FROM customers c WHERE c.id = g.customer_id AND c.test_clock_id IS NOT NULL)` |
| **Per-clock variant** | none | `ExpireCreditsForClock(ctx, tenantID, clockID, frozenTime)` |
| **Time predicate** | wall-clock now | clock.frozen_time |

Service entry: `Service.ExpireCredits` becomes cron path;
`Service.ExpireCreditsForClock` is added for catchup.

### 6. Invoice reminders (N-days-before-due nudges)

**REMOVED 2026-06-13 — descoped, never built past "list and log."** This
phase only ever queried invoices approaching `due_at` and logged the
count; no reminder email was ever dispatched. Both the cron hook
(`Scheduler` reminders) and the per-clock catchup mirror, their
`InvoiceReminder` / `ReminderLister` interfaces, the
`Service.ListApproachingDue` / `…ForClock` entries, and the
`PostgresStore` queries were deleted.

Rationale: a proactive **pre-due** payment-reminder email to the
customer is a B2C/SMB pattern, not the B2B/AI-infra norm Velox targets —
where operators own collection and **dunning** (Phase 5, post-due) is the
real money-protection path. Carrying inert scaffolding for an unbuilt,
off-wedge feature is the kind of doc/code-that-lies this codebase
deletes rather than maintains. The disjoint catchup orchestrator now ends
at Phase 5 (dunning). Rebuild trigger: a design partner explicitly asks
for pre-due reminders — at which point it returns as a real
outbox-dispatching phase, not a logging stub.

## The orchestration sequence

`testclock.CatchupWorker.process(job)` already drains catchup jobs and
calls `engine.RunCycleForClock`. After ADR-029, that single call
expands to a documented sequence executing each per-clock concern in
deterministic order:

```
process(job CatchupJob):
    clock = testClockStore.Get(job.tenantID, job.clockID)
    frozen = clock.FrozenTime
    ctx = WithLivemode(ctx, false)  // test clocks are test-mode-only

    // Phase 1 — period generation (existing, ADR-028)
    invoiceCount = engine.RunCycleForClock(ctx, job.tenantID, job.clockID, batchSize)

    // Phase 2 — tax retry on any invoices left at tax_status=pending
    //          after Phase 1's finalize attempts. Pre-Phase-3 because
    //          a successful tax retry unblocks finalize, which Phase
    //          3 needs (charge requires finalized invoice).
    invoiceSvc.RetryPendingTaxForClock(ctx, job.tenantID, job.clockID, batchSize)

    // Phase 3 — auto-charge retry on auto_charge_pending invoices.
    //          Customer may have attached a PM since the last advance.
    engine.RetryPendingChargesForClock(ctx, job.tenantID, job.clockID, batchSize)

    // Phase 4 — dunning advance on past-due invoices.
    //          Runs after Phase 3 because a successful charge clears
    //          dunning state; we don't want to advance dunning on an
    //          invoice that just got paid in Phase 3.
    dunningSvc.ProcessDueRunsForClock(ctx, job.tenantID, job.clockID, frozen, batchSize)

    // Phase 5 — credit expiry for the customers pinned to this clock.
    //          Independent of invoice state; ordering doesn't matter
    //          beyond placing it after Phase 1 (a fresh period close
    //          may have used credits we shouldn't expire mid-flight).
    creditSvc.ExpireCreditsForClock(ctx, job.tenantID, job.clockID, frozen)

    // Phase 6 — REMOVED 2026-06-13 (invoice reminders descoped; see
    //          section 6 above). The orchestrator ends at Phase 5.

    // Phase 7 — threshold scan is implicit per-period inside
    //          billOnePeriod (existing). The cron-side scan is
    //          decoupled in ADR-029 but no extra catchup-side step is
    //          needed; the per-period check is sufficient because
    //          thresholds depend on running cycle subtotal which is
    //          recomputed each period during catchup.
```

Each phase is independently retryable: a failure in Phase 4 doesn't
prevent Phase 5 from running. The catchup worker's outer transaction
boundary stays per-phase (current shape).

## Architecture properties

### What this guarantees

- **Disjoint exhaustive coverage**: every clock-pinned entity is
  processed by exactly one flow (catchup, on Advance), every non-clock
  entity by exactly one (cron). No row visible to both.
- **No SKIP-LOCKED race**: cron and catchup run disjoint queries; they
  cannot conflict on the same row. Safe for concurrent execution.
- **Simulation purity**: simulation time progresses ONLY on operator
  Advance. No background process drives any time-aware consequence on
  a clock-pinned entity.
- **Failure isolation**: each phase is independently retryable; a
  partial failure leaves the catchup worker in a recoverable state
  (status='internal_failure' on the clock; existing Retry advance
  flow resumes).
- **Deterministic ordering**: Phases 1-6 run in a documented order
  per Advance. Operator running the same advance twice gets identical
  end-state for the entities involved.

### What this costs

- ~600-800 LOC of new code: 6 SQL splits + 6 per-clock variants + 6
  interface methods + 1 orchestration sequence in catchup worker.
- ~6 unit / integration tests, one per phase ("cron skips clock-pinned"
  + "catchup processes clock-pinned").
- Each domain service grows a `*ForClock` method, slight surface
  expansion.

### What's NOT changing

- Schema: zero migrations. All splits are query-side (filters via
  existing `test_clock_id` columns + JOINs).
- API surface: external callers see no difference. The split is purely
  internal scheduler discipline.
- Wall-clock scheduler cadence: 5min dev / 1hr prod, unchanged.
- Catchup worker shape: still queue-driven, still 10-min per-job
  ceiling, still single-goroutine drain.
- ADR-019 Stripe-reconnect-flush: cross-mode by design (operator
  trigger, not time-driven). Unaffected.

## Test strategy

For each of the six phases, two new tests:

1. **`Test{Concern}_CronSkipsClockPinned`** — populate one clock-pinned
   entity that would be due if scanned, one non-clock entity that is
   due, run the cron path, assert only the non-clock one was processed.
   Mirrors `TestRunCycle_SkipsClockPinnedSubs` from ADR-028.
2. **`Test{Concern}_CatchupProcessesClockPinned`** — populate the same
   clock-pinned entity, call the per-clock variant directly, assert
   processed.

Plus one e2e:

3. **`TestCatchupOrchestration_FullSequence`** — set up a clock-pinned
   sub with: due period, pending tax invoice, auto-charge-pending
   invoice, past-due dunning run, expiring credit grant. Click Advance,
   assert all phases ran and end state is consistent.

## Rollout order

Single PR per phase, in dependency order:

1. **Phase 1 (P0)** — auto-charge retry. Smallest scope, highest
   visibility (the operator-surprise from 2026-05-08).
2. **Phase 2** — tax retry. Already has reconnect-flush precedent;
   pattern is well-understood.
3. **Phase 3** — threshold scan. Cron-side filter only; per-period
   loop already covers catchup.
4. **Phase 4** — credit expiry. Simple SQL filter, simple per-clock.
5. **Phase 5** — dunning advance. Most invasive (state machine);
   ship after the others to validate the pattern at scale first.
6. ~~**Phase 6** — invoice reminders.~~ Removed 2026-06-13 (descoped;
   see section 6).

After the remaining phases: amend ADR-028's "deferred decoupling"
section to point at ADR-029, mark the deferred work complete.

## Alternatives considered

- **Approach B: effective-now resolution at engine layer.** One SQL
  query per concern; engine resolves `effectiveNow(entity)` per row
  (frozen_time if clock-pinned, wall-clock otherwise). Simpler to
  write, but the cron query still TOUCHES clock-pinned rows even if it
  skips them on per-row inspection. SKIP-LOCKED race remains. Operator
  mental model still violated — the cron "considers" clock-pinned
  rows in its candidate set. **Rejected** for the same reasons
  ADR-028 rejected it for period generation.
- **Approach C: keep wall-clock cron, scope to non-test mode only.**
  In production (livemode=true), test clocks don't exist, so the cron
  path is correct. In test mode (livemode=false), partition the cron
  to skip the entire mode. **Rejected** because non-clock test-mode
  subs DO need cron processing — operators who don't use test clocks
  but operate in test mode would see no scheduler activity.

## Compatibility

Pre-launch, zero design partners, no API consumers. Internal-only
refactor. No migration needed. No SDK breakage. CHANGELOG entry per
phase shipped, MANUAL_TEST FLOW TC4 updated to add the
catchup-orchestration assertion line.

## Acceptance

ADR-029 is complete when:

1. All six phases shipped (six PRs / commits).
2. Twelve tests added (two per phase) + one e2e orchestration test.
3. ADR-028 deferred-decoupling section amended to reference ADR-029
   completion.
4. MANUAL_TEST FLOW TC4 includes a "click Advance after attaching PM
   to clock-pinned customer → charge fires as part of catchup, not on
   wall-clock tick" assertion.
5. Strict-mode integration sweep stays green throughout (the
   livemode-strict guard from earlier work is the safety net).
