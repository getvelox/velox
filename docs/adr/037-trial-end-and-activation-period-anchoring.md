# ADR-037: Trial-end and activation period anchoring

**Date:** 2026-05-18
**Status:** Accepted (amended 2026-05-18 — invoice-shape verification for `in_advance` + metered usage; hard-pause API removed in PR-8 with corrected industry framing)

## Context

A 2026-05-18 deep audit of every time-aware subscription flow surfaced a
related cluster of period-anchoring bugs that share one root cause: the
service layer's trial-end and activation paths each had their own
inline period math, and none of them honored the full
`(billing_time, plan.billing_interval)` matrix correctly. The
operator-visible symptoms covered the full lifecycle:

| # | Bug | Effect (calendar billing, sub created Nov 29, 14d trial) |
|---|---|---|
| 1 | `Create + trial + calendar` dropped the stub period | trial_end=Dec 13, but `current_period_start=Jan 1` — 19 unbilled days (revenue leak) |
| 2 | `EndTrial-early` didn't reset period boundaries | Operator EndTrial Dec 5 → status flips to active but next charge still fires Jan 1 (8 unbilled days Dec 5 → Dec 13) |
| 6 | `in_advance + trial` first paid period was never billed | engine auto-flip at cycle close bills in_advance items for the NEXT period; the trial-end period went unbilled (revenue leak specific to in_advance) |
| 8 | `status='trialing'` hung past `trial_end_at` | Status hung at trialing for the gap between actual trial-end and the first chargeable cycle close — up to ~30 days for calendar billing |
| 10 | Yearly + trial first cycle was 1 month, not 1 year | Customer paid 1/12 of yearly fee for an off-cycle first invoice, then full year invoices thereafter — 13 months of service for 13/12 of yearly fee |
| 11 | `Activate` (draft → active) backdated `period_start` to month-start, ignored `billing_time` | Sub activated Nov 29 was billed for the full Nov cycle including days it was a draft; anniversary draft activated mid-month still got calendar periods |

Each bug looked self-contained but they all sat on the same defect:
the period-anchoring helpers were duplicated inline in each entry
point with subtle differences. Fix scope crossed Create / Activate /
EndTrial / ExtendTrial + the engine's auto-flip path + the catchup
orchestrator + the wall-clock cron tick.

**Industry verification** (the period-anchoring shapes were verified
across two reference platforms before settling on the contract):

- **[Stripe — Subscription billing-cycle](https://docs.stripe.com/billing/subscriptions/billing-cycle)**:
  "Subscriptions with trials use the `trial_end` timestamp as the
  billing cycle anchor." First chargeable cycle starts at trial-end,
  prorated through the next billing-cycle anchor (e.g. month start
  for calendar). Trial-end transitions emit `customer.subscription.trial_will_end`
  at trial-end minus 3 days and `customer.subscription.updated` with
  `status='active'` at trial-end exactly.

- **[Lago — Billing time](https://doc.getlago.com/guide/subscriptions/billing-time)**:
  "For calendar billing-time, the first chargeable period anchors at
  the start of the next calendar period." Trial-end equivalent
  (`ending_at`) flips status without waiting for cycle close.

- **Both platforms** force anniversary semantics for yearly billing —
  neither ships a "calendar yearly" model where a stub bridges to a
  fixed annual anchor.

## Decision

Centralize period-anchor computation in two helpers, gated on
`(billing_time, plan.billing_interval)`, and route every entry point
through them. Pair the anchor computation with state-machine
guarantees: trial transitions are atomic and stamp their period at
the same instant the status flips.

**Helpers** (in `internal/subscription/service.go`):

- `firstPeriodAfterTrial(trialEnd, billingTime, interval, loc) → (ps, pe)`
  - Yearly: `ps = trialEnd (day-snapped), pe = ps + 1 year`.
    `billingTime` is ignored — Stripe and Lago both force anniversary
    on yearly.
  - Monthly + calendar: `ps = trialEnd (day-snapped), pe = next-month-start`.
    Edge case: `ps == pe` (trial_end already on a calendar boundary)
    collapses the stub and promotes `pe` to the following month.
  - Monthly + anniversary: `ps = trialEnd, pe = ps + 1 month`.

- `firstPeriodForActivate(at, billingTime, interval, loc) → (ps, pe)`
  - Same matrix as above but anchored on the activation instant `at`
    instead of `trialEnd`.

`interval` is resolved per-sub from `items[0].plan.BillingInterval`
via a new `PlanReader` interface on `subscription.Service` (wired in
`router.go` to the pricing store). Mixed-interval items on a single
sub are rejected at `Create` and `AddItem` (Stripe / Lago / Chargebee
all reject this).

**State-machine guarantees:**

- `Service.Create` — trial branch + start_now branch use the helpers.
- `Service.Activate` (draft → active) — uses `firstPeriodForActivate`.
  Pre-fix hardcoded `beginningOfMonth(now)` for `period_start`,
  backdating to first-of-month and ignoring `billing_time`.
- `Service.EndTrial` — early end resets period: new store atomic
  `EndTrialEarly(at, ps, pe, next_billing)` truncates `trial_end_at=at`,
  flips status, and re-anchors the period in one UPDATE.
- `Service.ExtendTrial` — recomputes anchor on `new_trial_end`. Store
  signature expanded: `ExtendTrial(at, ps, pe, next_billing)`.
- `Service.ProcessExpiredTrialsForClock` (catchup Phase 0.5) and
  `Service.ProcessExpiredTrials` (wall-clock cron phase 0.9) — both
  call `ActivateAfterTrial(at=trial_end_at)` so the status flips at
  the actual trial-end instant, not at the deferred cycle close.
  Both also fire `BillOnCreate` to cover the in_advance first paid
  period and dispatch `subscription.trial_ended` to match the
  engine auto-flip path's webhook.
- `engine.billOnePeriod` trial-elapsed branch — kept as a defensive
  fallback. With Phases 0.5 / 0.9 wired, the engine branch's
  `status='trialing'` guard naturally skips already-flipped subs.

## Why this design

- **One source of truth.** Pre-fix every entry point computed its own
  anchor with subtle differences; the bugs came from drift. The
  helpers are pure functions; tests pin every cell of the
  `(billing_time × interval)` matrix exactly.
- **Trial transitions happen at `trial_end_at`, not at cycle close.**
  The catchup-orchestrator and wall-clock-cron phases (`Phase 0.5` /
  `phase 0.9`) ensure status flips when the trial actually ends,
  matching Stripe / Lago's `subscription.trial_ended` semantics.
  The engine's existing cycle-close auto-flip branch is the
  defensive fallback.
- **`feedback_billing_accuracy`** — financial logic must be correct
  first time; the full matrix is exercised by `TestPeriodAnchoring`'s
  11 scenarios.
- **`feedback_stripe_parity_framing`** — the chosen design is a
  superset that contains Stripe's behavior as a configuration:
  Velox's `billing_time` lets the operator pick calendar vs
  anniversary, but for yearly plans (where neither platform ships
  a meaningful calendar variant) anniversary is forced.

## Alternatives considered

- **A. Keep inline math at every entry point, fix each bug
  separately.** Rejected: drift is the root cause, fixing each
  symptom would leave the next entry point exposed to the same class
  of bug (see `feedback_long_term_means_cross_flow_audit`).
- **B. Cycle-close-only trial transitions (status='trialing' until
  next_billing_at fires).** Rejected: Bug #8 — dashboard lies about
  lifecycle state for the gap between trial-end and cycle close (up
  to ~30 days for calendar billing). Webhook consumers would also
  receive `subscription.trial_ended` weeks late.
- **C. Stub-to-next-Jan-1 for yearly + calendar.** Rejected: no
  industry analog. Stripe and Lago both force anniversary for
  yearly. A partial-year first invoice followed by full-year
  cycles is a Velox-specific oddity with no operator demand.
- **D. Reject `billing_time=calendar` for yearly plans at Create.**
  Rejected: would force operators to think about the interaction
  when only one combination is meaningful. Forcing anniversary
  semantics silently and ignoring the `billing_time` field for
  yearly is friendlier and matches the implementations of
  reference platforms.

## Consequences

### Positive

- Revenue leaks closed for calendar+trial (Bug #1), early-EndTrial
  (Bug #2), in_advance+trial (Bug #6), and yearly+trial (Bug #10).
- Status correctness for the trial → active transition (Bug #8),
  ending the gap between actual trial-end and visible status flip.
- Operator UX improvements (PR-3): the subscription detail page
  renders trial-aware timeline dots and a "Trial period" stat card
  while `status='trialing'`.
- Webhook consumers see `subscription.trial_ended` events fired at
  `trial_end_at` (or at activation for operator-EndTrial),
  regardless of which path activated the sub.

### Risks / open items

- The `firstPlanInterval` helper falls open to `BillingMonthly` on
  any PlanReader error (RLS gap, deleted plan, unwired reader). This
  preserves narrow-test behavior at the cost of masking
  configuration mistakes — a deleted plan id mid-life would silently
  re-anchor as monthly. Considered acceptable because the period
  helpers run BEFORE the sub is persisted at Create time (plan FK
  failure surfaces downstream), and the runtime entry points
  (`Activate`, `EndTrial`, `ExtendTrial`) operate on already-persisted
  subs whose plan ids are FK-validated.
- Mixed-interval validation is currently first-item-wins for already-
  persisted subs (`UpdateItem` plan-change doesn't yet re-validate).
  Separate follow-up.
- True cycle-skip "hard pause" semantics are not implemented (Bug
  #12). The dashboard's "Pause subscription" radio option was
  removed in PR-6 rather than fixing the implementation, per
  `feedback_pre_launch_scoping` — no named pressure for cycle-skip
  pause semantics.
- The wall-clock cron's `ProcessExpiredTrials` runs at the
  scheduler's tick interval (typically 5 minutes), so wall-clock
  status flips lag the actual `trial_end_at` by up to one tick.
  The catchup-orchestrator path is exact (fires on operator Advance).

## Amendment 2026-05-18: `in_advance` + metered usage invoice shape

Verified industry behavior for the case where a plan's base fee is
`in_advance` AND the sub has metered/usage charges — that mix is
what `engine.billOnePeriod` emits as a single hybrid invoice at every
cycle close (next-period base prepayment + just-elapsed-period usage
in arrears). Recorded here because the post-fix `Billing Period` UI
copy on `SubscriptionDetail.tsx` surfaced operator confusion ("the
period's invoice is already generated — why is it labeled Billing
Period?"), which led to digging into how reference platforms
structure the same invoice. Three platforms checked:

- **[Stripe — Advanced usage-based billing](https://docs.stripe.com/billing/subscriptions/usage-based/advanced/about)** — *"When a billing interval ends, all accrued charges since the last billing interval are compiled into an invoice and sent to the customer."* / *"License fees are billed one service interval in advance regardless of the billing interval."* / *"Customers are only billed for completed service intervals."* (usage rule). Result: one invoice per cycle close, combining in-advance base for the upcoming period + arrears usage for the just-completed period.
- **[Lago — Plan model](https://doc.getlago.com/guide/plans/plan-model)** / **[Lago — Usage-based charge](https://getlago.com/docs/guide/plans/charges/usage-based-charges)** — *"Additional charges for per-usage Billable metrics **are always paid in arrears because they are linked to a past consumption of your customers.**"* Combined with their pay-in-advance subscription-fee rule: same hybrid invoice shape.
- **[Chargebee — Metered Billing](https://www.chargebee.com/docs/billing/2.0/usage-based-billing/metered_billing)** — diverges. *"Processing the final invoice amount for subscriptions with metered items requires the finalized usage data, which is not available in advance. As a result, **scheduling advanced invoices for subscriptions with metered items is not possible.**"* Chargebee disallows the combination — metered subs cannot bill the base in advance.

**Velox follows the Stripe + Lago pattern** — the dominant convention
across the two platforms operators most often compare us against.
Base line items stamp `billing_period_start/end = [periodEnd,
nextPeriodEnd]` (`internal/billing/engine.go:1494-1495`); usage line
items stamp `billing_period_start/end = [periodStart, periodEnd]`
(engine.go:1601-1602). Both pre-trial first-invoice (`BillOnCreate`)
and steady-state cycle invoices follow this shape.

**Not adopted from Chargebee**: a Plan-create validation that rejects
`base_bill_timing=in_advance` when the plan has `meter_ids`. Defensible
to add later if an operator surveys produce a named need; per
`feedback_pre_launch_scoping` it's not justified pre-launch since
Velox's reference platforms allow the combination.

**Operator-facing implication**: a customer on an in_advance metered
plan sees their period-N invoice combining period-(N+1) base lines
with period-N usage lines. The dashboard's invoice-detail view should
surface the per-line `billing_period_start/end` so this isn't
ambiguous when an operator inspects a mixed-period invoice. Currently
the data flows through; a follow-up check that the rendering is clear
is on the open-items list. The `SubscriptionDetail.tsx` Current period
stat card (renamed 2026-05-18 from "Billing Period" — same data,
clearer label) refers to `current_period_start/end` which is the
consumption window, agnostic of when each line on the corresponding
invoice fires.

## Amendment 2026-05-18 (PR-8): hard-pause API removed; industry framing corrected

PR-6 removed the dashboard's "Pause subscription" radio option on the
grounds that the implementation's "freezes the cycle entirely"
description was a lie: paused subs were filtered out of the cycle
scan, but on resume the engine caught up by billing every "missed"
cycle — the opposite of a freeze. The earlier framing said *"no
industry analog"* for true cycle-skip hard pause. **That claim was
wrong.** Re-verification on 2026-05-18:

- **[Chargebee — Pause Subscription](https://www.chargebee.com/docs/billing/2.0/subscriptions/pause-subscription)**: ships full hard-pause with cycle skip. *"Pause at the end of the current term, and resume automatically after the set number of billing cycles (in `skip_billing_cycles`) have been skipped."* Three pause-options (end_of_term / scheduled / immediate); three resume-options (after-cycles / on-date / indefinite); three unbilled-charges strategies (invoice / defer / discard).
- **[Recurly — Pause subscription](https://docs.recurly.com/docs/pause-subscription)**: also ships it. *"This allows you to freeze billing for a specified number of cycles at the next renewal date."* Auto-resume after `remaining_pause_cycles`; cycle anchor resets on resume.
- **Stripe**: only `pause_collection` (keep_as_draft / void / mark_uncollectible). No cycle-skip hard pause.
- **Lago**: no documented pause feature.

So the honest framing: **hard pause exists in industry (Chargebee, Recurly), Velox deferred for scope, not absence**.

**PR-8 removes the entire hard-pause surface:**
- Routes: `POST /v1/subscriptions/:id/pause` + `/resume`
- Handlers: `Handler.pause` + `Handler.resume`
- Service: `Service.Pause` + `Service.Resume`
- Store: `Store.PauseAtomic` + `Store.ResumeAtomic` (interface + postgres + mem)
- Events: `EventSubscriptionPaused` + `EventSubscriptionResumed` enum constants
- SDK: `api.pauseSubscription` + `api.resumeSubscription` in `web-v2/src/lib/api.ts`
- Dashboard: dead `resumeMutation` + `status === 'paused'` Resume button block
- Status enum: `domain.SubscriptionPaused` removed
- Schema: migration **0090** drops `'paused'` from `subscriptions.status` CHECK (with a safety guard that errors loudly if any row IS in 'paused' state — pre-launch zero data, so moot in practice)
- OpenAPI spec: removed `paused` from the status enum (and re-added missing `trialing`, fixing a stale gap from migration 0053)
- Generated types regenerated via `make gen`

The Chargebee/Recurly e2e shape (paused_at + resume_at + remaining_pause_cycles + unbilled-charges decision tree) is documented in the body of this ADR as the **future re-implementation target** when a design partner names the need. The pre-launch decision is to ship truth over feature-count: `pause_collection` covers the legitimate "operator wants to freeze charging" use case for Velox's wedge market (AI infra Series A-B), and rebuilding hard-pause properly is a 3-5 day project that should be designed against a real customer's requirements.

**Updated risks/open items:**

- If a DP asks for hard pause, the design starting point is Chargebee/Recurly's API shape outlined above. Don't reinherit the old `/pause` endpoint — design the new one to take `pause_option` / `resume_option` / `unbilled_charges` upfront.
- The in_advance scheduled-cancel-at-period-end overcharge (flagged separately in the cancel-flow audit) is the same shape of bug that a future hard-pause implementation would need to guard against: in_advance base lines should NOT be emitted at a cycle close where the sub is about to transition out (cancel or pause activation). Single fix would cover both cases.

## References

- Related ADRs: [ADR-028](028-billing-engine-period-loop-and-disjoint-flows.md)
  (disjoint flows — clock-pinned vs wall-clock subs go through
  separate scan paths), [ADR-029](029-fully-disjoint-test-clock-flows.md)
  (catchup orchestrator phase ordering), [ADR-031](031-per-plan-base-bill-timing.md)
  (in_advance bill-timing — Bug #6's BillOnCreate trigger ties back to
  this), [ADR-035](035-per-fact-simulated-time-anchoring.md)
  (per-fact sim-time anchoring — the bind-effective-now pattern this
  ADR builds on).
- Commits implementing this contract: `2233600` (Bug #1, #2, #11 +
  ExtendTrial re-anchor), `7d26023` (Bug #6 — in_advance + trial
  day-1), `e51face` (Bug #8 — catchup Phase 0.5), `af47996` (Bug #8 —
  wall-clock cron phase 0.9), `786e299` (Bug #10 — yearly).
- Memory pointers: `feedback_billing_accuracy`,
  `feedback_long_term_means_cross_flow_audit`,
  `feedback_stripe_parity_framing`,
  `feedback_design_for_production`,
  `feedback_verify_stripe_parity_claims`.
- [Stripe — Subscription billing-cycle](https://docs.stripe.com/billing/subscriptions/billing-cycle)
- [Lago — Billing time](https://doc.getlago.com/guide/subscriptions/billing-time)
- [Stripe — Cancel a subscription](https://docs.stripe.com/api/subscriptions/cancel) (Bug #13 — trial-cancel parity)
