# ADR-035: Per-fact simulated-time anchoring under test-clock catchup

**Date:** 2026-05-16
**Status:** Accepted

## Summary

ADR-029 runs every time-driven billing step in order inside one test-clock Advance. Even so, records written during that pass were stamped with the advance's end time (`frozen_time`), so timelines piled up at the end. Dunning retries (retrying a failed payment) were also scheduled past the end time, so only the first due retry fired. Now each record carries the simulated time its event happened. The engine passes that time explicitly: cycle billing uses `sub.NextBillingAt`, each dunning retry uses `run.NextActionAt`, and the dunning and credit stores accept a caller-supplied `CreatedAt`. When none is given, they fall back to `clock.Now(ctx)`, so the wall-clock operator paths are unchanged. The dunning step re-queries for due runs until none are left, stopping at a 50-iteration safety cap or when a pass makes no progress. On a definite charge failure, `ChargeInvoice` now starts dunning inline instead of waiting for the `payment_intent.payment_failed` webhook, which arrives after the dunning step has exited. Ambiguous failures still go to the reconciler. Starting dunning from both paths is safe because migration 0085 allows one dunning run per invoice, so a second `StartDunning` returns the existing run. Retry charges are tagged so the payment_failed webhook doesn't send its own email: an exhausted N-retry run sends N+1 emails, one per attempt. As a result, timelines show events in their real order, and one Advance runs the full retry schedule. Email send times stay wall-clock.

## Context

ADR-029 (fully disjoint test-clock flows) gave clock-pinned subscriptions
their own catchup orchestrator: every time-driven engine concern (cycle
billing, threshold scan, tax retry, auto-charge, credit expiry, dunning
advance, reminders) runs in dependency order inside one Advance click.
ADR-030 added a clock resolver so per-invoice timestamp writes
(`next_action_at`, `last_attempt_at`, `resolved_at`, `created_at`) land
in the simulated-time domain instead of wall-clock.

A multi-week dogfood pass in May 2026 surfaced a deeper structural gap:
even with the resolver wired, time-driven engine writes stamped the
**orchestrator's current frozen_time** (= advance-end), not the
**per-fact simulated instant** the engine concern actually represented.

Concrete symptoms reported against the dashboard / invoice timeline /
mailpit:

- An Apr → Jul-21 advance billing the May 1 cycle stamped that
  invoice's `IssuedAt` / `CreatedAt` / `DueAt` at `2024-07-21`, not
  `2024-05-01`. The invoice activity timeline read "Invoice
  created Jul 21" for a cycle whose period boundary was May 1.
- Dunning runs created during catchup had `next_action_at = frozen_time
  + grace_period`. So a May 1 invoice failing during a May 20 advance
  scheduled retry #1 for May 23 — past frozen_time — and zero retries
  fired in the same Advance.
- Inside the dunning state machine's retry loop, each retry's
  `last_attempt_at` and the next scheduled `next_action_at` were
  anchored on `clock.Now(ctx)` (= frozen_time, constant for the whole
  pass), so retry #N+1's `next_action_at` was always `frozen_time +
  interval` — past advance-end → never fires.
- Dunning event rows (`started`, `retry_attempted` x N, `escalated`)
  all stamped `clock.Now(ctx)` at write time. Operator saw 4 rows at
  one timestamp instead of the per-fact chronology.
- Credit ledger entries created during catchup (cancel-proration
  grants, expiry, applied-to-invoice usage) stacked at advance-end on
  the customer's Credits tab.
- `Phase 5` (dunning) fired only once per Advance — even when multiple
  retries were due in the simulated window, only the first fired,
  because `processRun` left the next `next_action_at` past `frozen_time`
  and `ProcessDueRunsForClock` did not re-query.

These were not separate bugs; they were one architectural pattern
applied uniformly across the engine: **`clock.Now(ctx)` returns
advance-end frozen_time during catchup, and was used as the per-fact
timestamp for events that actually occurred at distinct earlier
simulated instants.**

## Decision

Every per-fact write under test-clock catchup stamps the **simulated
instant the fact occurred**, not the orchestrator's current frozen_time.
Implemented as a cascade of explicit-parameter handoffs from outermost
phase to innermost write:

### 1. Cycle billing anchors on `sub.NextBillingAt`

`engine.billOnePeriod` resolves `now` from `*sub.NextBillingAt` (the
cycle's own close instant) rather than `effectiveNow(sub)` (= clock's
current value). Falls back to `effectiveNow` when `NextBillingAt` is
unset (unreachable from `billSubscription`'s caught-up check, kept
defensive).

`now` is then used uniformly for `IssuedAt` / `CreatedAt` / `DueAt`,
tax calculation date, pause-resume gate, trial-end activation,
`advanceCycleOrCancel` stamps, and `MarkPaid` for zero-amount
auto-paid invoices. In the cron (wall-clock) path `NextBillingAt`
≈ scheduler tick within minutes; change is neutral.

### 2. Dunning state machine carries simulated time explicitly

`dunning.Service.StartDunning(... failureAt time.Time)` — the caller
supplies the cycle-close instant. The Stripe-webhook caller and the
inline-failure caller in `ChargeInvoice` both derive `failureAt` via
the new `simulatedFailureAt(inv)` helper: latest invoice period
boundary at or before `IssuedAt` (which is `BillingPeriodEnd` for
in_arrears and `BillingPeriodStart` for in_advance — the cycle-close
moment in both directions).

`processRun` anchors each retry's effective-now on `run.NextActionAt`
(the moment the retry was scheduled for) rather than `clock.Now(ctx)`.
The chain advances forward in simulated time: retry #1 at `T + grace`,
retry #2 at `T + grace + retry_schedule[0]`, etc. — each fires at its
own scheduled instant rather than collapsing onto frozen_time.

`exhaustRun(... firedAt time.Time)` — caller passes the triggering
retry's instant so the escalated state's `resolved_at` and the
escalated event row align with the retry that caused the exhaustion.

### 3. Phase 5 loops until empty

`ProcessDueRunsForClock` re-queries `ListDueRunsForClock` after each
batch until it returns zero rows. Safety cap of 50 iterations; a
non-progressing-run guard breaks the loop if a transient-skip case
returns the same run without advancing `attempt_count`. With the
in-state-machine simulated-time chain (item 2), each retry's new
`next_action_at` lands at a simulated instant that may still be
≤ frozen_time, so the loop walks the full retry schedule in one
Advance.

### 4. Postgres stores honor caller-supplied `CreatedAt`

`dunning.PostgresStore.CreateEvent` and `credit.PostgresStore.AppendEntry`
respect a non-zero `CreatedAt` on the input struct, falling back to
`clock.Now(ctx)` only when unset. Wall-clock callers (operator-action
paths) continue to use the fallback. Service-layer callers in catchup
paths pass the per-fact instant: dunning_started at cycle close,
retry_attempted at the scheduled instant, escalated at the triggering
retry, credit grants at the action moment (cancel-proration at
`cancelAt`), expiries at `grant.ExpiresAt`.

### 5. `ApplyToInvoiceAtomic` takes an `at time.Time`

`credit.Service.ApplyToInvoiceAt(... at time.Time)` and a new
`engine.CreditApplier.ApplyToInvoiceAt` interface — engine
(billOnePeriod, threshold_scan) pass their `now` (= cycle close) so
the usage entry and invoice `updated_at` land on per-cycle simulated
time. Operator-path callers pass `time.Time{}` to opt out and fall
back to `clock.Now(ctx)`.

### 6. Inline `StartDunning` on definitive charge failure

Closes a Phase 3 → Phase 5 timing race: `ChargeInvoice` previously
deferred dunning-run creation to the `payment_intent.payment_failed`
webhook. On wall-clock fine — the cron's Phase 5 ticks every 5 min and
catches up. Under catchup the orchestrator runs Phase 3 → Phase 5
back-to-back; the webhook arrives async after Phase 5 has exited, so
the new run sits at `attempt_count=0` until the next Advance.

`ChargeInvoice`, when it gets a **definitively** failed PI error (not
the ambiguous unknown/timeout case — those still defer to the
reconciler), now inline-calls `StartDunning`. Safe alongside the
webhook path because `StartDunning` is idempotent by invoice (migration
0085 UNIQUE index on `invoice_dunning_runs.invoice_id`). The webhook
path becomes a redundant secondary trigger — better resilience for
wall-clock too (e.g. Stripe webhook outage).

### 7. One email per retry attempt

The generic payment-failed email is skipped when the PI carries
`velox_purpose=dunning_retry` metadata. (Amended 2026-10-11: the skip lives in
`SettleFailed`, so it applies to whichever source reports the decline — the
charge response, the webhook or the reconciler — not only the webhook.) Dunning's `paymentRetrierAdapter` tags retry PIs via a
dedicated `Stripe.ChargeInvoiceForDunningRetry` method (typed,
explicit — not a ctx-threaded side channel, not a cross-domain
options import). End state per exhausted N-retry run: 1 initial-fail
email + (N-1) warning emails + 1 escalation = N+1 total. Stripe
Smart Retries / Lago parity.

## Why this design

**Explicit per-fact instants vs implicit ctx-clock.** ADR-030 made
`clock.Now(ctx)` resolve to frozen_time for clock-pinned contexts —
a useful single-clock abstraction, but `frozen_time` is constant
across a multi-period catchup pass. The simulated instant for
event N differs from event N+1. An implicit single value can't
represent "many simulated instants in one wall-clock pass."

Explicit parameter handoffs (`failureAt`, `firedAt`, `at`, `CreatedAt`
on the struct) carry the right per-fact instant from where the engine
knows it (e.g. `sub.NextBillingAt` at billing time, `run.NextActionAt`
at retry-fire time) down to the persistence layer.

**Honor-caller + fallback at the store boundary.** Postgres stores
respect a caller-supplied `CreatedAt` when set, fall back to
`clock.Now(ctx)` when zero. This keeps the operator-action path
(wall-clock, no specific instant to anchor on) cheap and the catchup
path correct without a global flag.

**Migration 0085's UNIQUE index is the keystone.** One dunning run per
invoice for lifetime means `StartDunning` can be called from anywhere
(inline charge-failure path, async webhook, manual operator action)
safely — duplicates return the existing run. This is what makes the
inline + webhook redundancy work without coordination.

**Phase 5 loop vs new orchestrator design.** Rather than restructure
catchup to interleave phases at simulated-time boundaries (e.g. fire
Phase 3 + 5 at each cycle-close in lockstep), Phase 5 loops until the
state machine drains. Cheaper to implement, less invasive, matches
the natural "queue of due work" shape.

## Alternatives considered

- **Single `now` parameter threaded through everything.** Rejected:
  each phase's `now` is different. Cycle billing's `now` is
  `sub.NextBillingAt`; dunning's per-retry `now` is
  `run.NextActionAt`. One parameter would have to carry a function,
  which is just ctx-threading with extra steps.
- **Variadic options on `ChargeInvoice`.** Considered for the
  retry-purpose tagging. Rejected: requires `payment.ChargeOption`
  type, and either (a) engine's `InvoiceCharger` interface imports
  `payment` (cross-domain import per CLAUDE.md), or (b) the engine
  loses static type safety. A dedicated typed method
  (`ChargeInvoiceForDunningRetry`) on the concrete `*Stripe` is
  narrower and more explicit.
- **Context-threading `velox_purpose`.** Initial implementation;
  reverted in favor of the typed method. Ctx-threading is the right
  pattern for cross-cutting concerns (tenant, livemode, deadline)
  but inappropriate for a narrow per-call configuration that lives
  at one boundary.
- **Restructure the orchestrator to interleave phases by simulated
  time.** Cleaner conceptually but a much larger change. Deferred —
  Phase 5 looping captures the only phase where intra-pass
  recursion actually matters (auto-charge is one-shot per invoice;
  tax retry is one-shot per code-class; credit expiry is one-shot
  per grant).

## Consequences

### Positive
- Invoice activity timeline reads per-fact chronology — no more "all
  events at advance-end."
- One Advance click walks the dunning state machine to completion
  (Stripe Test Clocks parity).
- Customer email cadence matches industry: 1 initial-failure + (N-1)
  warnings + 1 escalation per N-retry exhausted run.
- Pause-collection / trial-end / scheduled-change gates evaluate in
  simulated time per cycle, not at the orchestrator's advance-end.
- Wall-clock production path benefits from the inline `StartDunning`
  resilience: a Stripe webhook outage no longer blocks dunning-run
  creation.

### Risks / open items
- **No explicit invariant test** locking the Phase 3 → Phase 5
  invariant (Phase 5 sees the dunning run created by Phase 3 in the
  same Advance). If a future change reverts inline `StartDunning`
  back to webhook-only, the race returns. Followup work:
  integration test against real Postgres + mock Stripe charger.
- **Email outbox timestamps stay wall-clock** by design — the actual
  SMTP send happens in real time. Operator looking at Mailpit will
  see wall-clock send times next to simulated-time dunning rows.
  This is correct semantically but visually inconsistent.
- **Tax retry phase is still single-shot per Advance.** Edge case
  where one tax retry unblocks one invoice and exposes another
  retryable invoice in the same Advance — that second one waits.
  Low real-world impact; not fixed in this pass.

## References

- ADR-028: billing-engine period loop and disjoint flows
- ADR-029: fully disjoint test-clock flows
- ADR-030: simulated time everywhere on clock-pinned entities
- Migration 0085: `idx_dunning_runs_one_per_invoice` UNIQUE
