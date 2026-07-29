# ADR-030: Simulated time everywhere on clock-pinned entities

**Status:** Accepted
**Date:** 2026-05-08
**Implemented**: 2026-05-08
**Supersedes**: closes the deferred-decoupling section ADR-029 left open for non-load-bearing timestamps

## Context

After ADR-029 shipped catchup correctness for the six load-bearing
catchup paths (period gen, auto-charge, tax retry, threshold scan,
credit expiry, dunning, reminders), a cross-flow audit found wall-clock
leakage at the per-callsite level:

- `dunning.Service` stamped `next_action_at` via `s.clock.Now()` even
  when called from catchup against a clock-pinned invoice.
- `subscription.Service.Create / Activate / ChangeItem` stamped
  `next_billing_at`, period bounds, `trial_end_at` in wall-clock for
  brand-new subs of clock-pinned customers.
- `invoice.Service.Create` stamped `due_at` in wall-clock for one-off
  invoices on clock-pinned customers.
- `payment.Stripe.handlePaymentSucceeded` stamped `paid_at` in
  wall-clock when the Stripe webhook fired (real network = real time)
  even though the invoice belonged to a simulated 2024-04 timeline.
- Postgres stores wrote `created_at` / `updated_at` via
  `time.Now().UTC()` regardless of the entity's pin.
- PDF "Generated on" footers, audit log entries, and dashboard
  "X minutes ago" formatting all read wall-clock.

The bug class: wall-clock timestamps written onto rows that catchup
queries later compare against `frozen_time`. The dominant trigger is
the **idle-clock-then-advance pattern** — operator creates a clock,
walks away for hours/days/weeks, comes back and advances. Wall-clock
has drifted ahead of `frozen_time` by the idle duration; any timestamp
stamped wall-clock during the advance now lands outside the catchup
window and is invisible to subsequent advance scans.

Per-callsite resolver patches (the post-ADR-029 fixes for dunning,
subscription, invoice, customer-flush) plugged the load-bearing leaks
but didn't scale: every new code path is one forgotten resolver call
away from re-introducing the bug.

## Stripe's published model

Stripe TestClock's "no semantic change in the presentation of Billing
objects in the API" guarantee, from their engineering blog:

> *"We made a no-op change to our internal logic to remove dependencies
> on real-world time, and instead retrieve timestamps from an abstract
> 'time provider' backed either by a real-world clock or a test clock."*

Two architectural rules that follow:

1. **Time-provider abstraction**: every business-logic timestamp is
   read from an injected provider, not from `time.Now()`. The provider
   resolves to the entity's pin (frozen_time on pinned, wall-clock
   otherwise).
2. **Scheduler carve-out**: pinned rows are filtered out of every
   wall-clock scheduler query. The test clock's catchup is the only
   thing that scans pinned rows. Without this, even simulated
   timestamps wouldn't help — the cron would still pick up the row at
   the wrong time.

Stripe's deliberate exception: **PaymentIntent / Charge / Refund** and
the actual Stripe payment webhooks stay wall-clock. Their docs are
explicit: *"Test clock advancement currently doesn't support
collecting payments through bank debits…Stripe collects payments after
the test clock advances."* They drew the line at "what the time
provider abstraction reaches" — Billing engine got the refactor;
Payments did not.

## Decision

Adopt Stripe's model end-to-end:

1. **Time provider is ctx-aware.** Replace `clock.Clock.Now() time.Time`
   with `clock.Clock.Now(ctx context.Context) time.Time`. Add
   `clock.WithEffectiveNow(ctx, t)`, `clock.EffectiveNow(ctx)`, and a
   package-level `clock.Now(ctx)` helper for postgres-store and
   render-layer code that doesn't own a Clock struct field.

2. **Bind effective-now at every operator entry point** on a
   clock-pinned entity. Subscription.Create / Activate / ChangeItem /
   ScheduleCancel / PauseCollection / EndTrial / ExtendTrial; invoice
   Create / Finalize / Void / RecordPayment / RecordPaymentFailure /
   ApplyDiscount / RetryTax / AddLineItem; dunning StartDunning /
   processRun / exhaustRun / ResolveRun / ResolveByInvoice; customer
   Update / UpsertBillingProfile; credit Grant; payment.Stripe
   handlePaymentSucceeded / handlePaymentFailed. Each binds via
   `clock.BindEffectiveNow(ctx, resolver, Pin{...})` at the top.

3. **Postgres stores read directly from ctx.** Every
   `time.Now().UTC()` in a store layer becomes `clock.Now(ctx)` —
   simulated for clock-pinned ctx-bound writes, wall-clock for the
   rest. No store-side Clock struct field needed.

4. **Render layer (PDF, email) takes ctx.** PDF "Generated on" footer,
   email date interpolations all read `clock.Now(ctx)` so a
   clock-pinned invoice's PDF shows simulated time consistently with
   its body.

5. **Webhook handlers bind from the invoice they're processing.**
   `payment.Stripe.handle*` resolves the invoice's effective-now via
   the engine and binds ctx before any state-machine call.

6. **Unified `clock.Resolver` interface.** Replaces the per-service
   `dunning.ClockResolver` / `subscription.ClockResolver` /
   `invoice.ClockResolver` interfaces shipped during the ADR-029
   patches. `*billing.Engine` implements `clock.Resolver` via three
   methods (`EffectiveNowForCustomer`, `EffectiveNowForSubscription`,
   `EffectiveNowForInvoice`), wired in `api/router.go` to every
   service that needs binding.

7. **Stripe's PaymentIntent/Charge carve-out is preserved.**
   `payment.Stripe` writes `paid_at` via `clock.Now(ctx)` (which is
   simulated when the invoice is clock-pinned), but the Stripe
   PaymentIntent / Charge resources Stripe itself owns remain
   wall-clock — that's outside Velox's database. Mirrors Stripe.

## The line we're drawing

| Surface | Domain | Why |
|---|---|---|
| Subscription, Invoice, DunningRun, CreditGrant, Customer state-machine timestamps | simulated when clock-pinned | Catchup correctness + dashboard consistency |
| Postgres `created_at` on every clock-pinned row | simulated | "No semantic change" — all visible timestamps in the simulation domain |
| Postgres `updated_at` on every clock-pinned row | simulated (via the same `clock.Now(ctx)` helper) | Same; no separate forensics column |
| PDF "Generated on" footer | simulated | Operator viewing a simulated 2024 invoice sees 2024 in the footer |
| Outbound webhook event `occurred_at` for billing events | simulated | Stripe `event.created` for billing events tracks frozen_time |
| Cron tick scheduler timestamp | wall-clock | The scheduler IS wall-clock; carve-out filters out pinned rows |
| Stripe webhook delivery (`Stripe-Signature` t=, retry timing) | wall-clock | Real network; replay protection requires real timestamps |
| Audit log row `created_at` | wall-clock **always** | Forensics. The row records "when did the operator click" — a real-time event regardless of which test clock the affected entity is pinned to. Simulated effect-time enriched as `metadata.sim_effective_at` + `metadata.test_clock_id` when relevant; UI renders that as a subline below the wall-clock primary stamp. See 2026-05-28 amendment below. |
| Webhook delivery audit (delivered_at, failed_at) | wall-clock | Real HTTP timing |

No `physical_created_at` forensics column on any business-logic
table. Stripe deliberately doesn't have one — exposing wall-clock on
pinned objects forces every read path to choose, which is the bug
surface we're escaping. If forensics ever becomes a concrete need, it
goes in a separate audit table, not on the row.

## Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│  Operator HTTP request                                           │
│      ↓                                                            │
│  Service entry point (Subscription.Create, Invoice.Finalize, …)  │
│      ↓ clock.BindEffectiveNow(ctx, engine, Pin{...})              │
│      ↓ ctx now carries effective-now                              │
│  Service body                                                     │
│      ↓ s.clock.Now(ctx) → simulated time                          │
│      ↓ s.store.Update(ctx, …)                                     │
│  Postgres store                                                   │
│      ↓ clock.Now(ctx) → same simulated time                       │
│      ↓ INSERT … created_at = simulated, updated_at = simulated    │
│  Render (PDF, email)                                              │
│      ↓ clock.Now(ctx) → same simulated time                       │
└─────────────────────────────────────────────────────────────────┘
```

The chain is one bind at the boundary, ctx propagation everywhere
below. New code that calls `s.clock.Now(ctx)` is automatically
correct; new code that calls `time.Now()` directly is automatically
wrong, and the linter (Phase 7, deferred — see below) catches it.

## Consequences

- **Clean abstraction**: operators never need to know about
  wall-clock when working with test clocks. The "advance to past"
  question becomes irrelevant — every timestamp Just Works in the
  simulation timeline.
- **Compositional correctness**: nested service calls inherit the ctx
  binding. No per-callsite resolver discipline.
- **One-time refactor cost**: ~80 sites changed for the Clock
  interface signature; ~20 entry points wire the binding; ~50
  postgres stores read from ctx. Mechanical, all caught at compile
  time by the Go compiler.
- **PaymentIntent / Charge stay wall-clock**: matches Stripe; ours is
  a thin wrapper around their real API anyway.
- **The `updated_at` decision flipped from "wall-clock forensics" to
  "simulated"** during implementation. Reasoning: keeping `updated_at`
  wall-clock would require splitting every postgres-store write into
  two timestamps in different domains, doubling complexity for a
  forensics use case nobody has asked for. Stripe doesn't expose
  physical-write time on the public API; we don't either. If forensics
  is needed, audit logs cover it.

## Deferred to a follow-up ADR

- **Per-cause test_clock webhook events** (`test_clock.created`,
  `.advancing`, `.ready`, `.internal_failure`, `.deleted`): Stripe
  exposes these; Velox doesn't yet. Add when first design partner
  asks for clock-state change notifications.
- **`status_details.advancing.target_frozen_time` field on the test
  clock JSON**: Stripe surfaces this; lets dashboards render advance
  progress. Velox stores the target internally but doesn't expose it.
- **Auto-delete after 30 days** + per-clock entity caps (3 customers,
  3 subs/customer, 10 quotes): pre-launch we don't have a sprawl
  problem. Documented as future-enforce.
- **Advance cap** (≤2 intervals from shortest sub or ≤2 years if no
  sub): Stripe caps to prevent runaway catchup loops. Velox already
  caps at +1 year per call (FLOW TC4); could tighten to match Stripe
  per-sub interval count.
- **List-API filtering** (default-hide test-clock objects from
  `GET /v1/customers` etc., require `?test_clock=...` to surface):
  Stripe does this; Velox today returns mixed results. Defer until a
  customer asks.
- **Linter rule** that flags bare `time.Now()` in `internal/{service,
  store,api}` packages. Locks in the architecture against
  regression. Can ship as a `go vet` analyzer or a CI grep job; ~50
  LOC. Treat as a follow-up commit.

## Test coverage

Unit tests added at the platform/clock level cover the binding
mechanics (`WithEffectiveNow` / `EffectiveNow` / `BindEffectiveNow`
edge cases). Each domain that gained a `SetResolver` method has a
test that asserts simulated time stamping via a `stubClockResolver`
under a clock-pinned entity, and a wall-clock fallback test for the
unwired path. The cross-flow integration ("create clock-pinned
customer, advance, assert all timestamps simulated") is captured in
MANUAL_TEST FLOW TC4 — a real-DB assertion is the only way to verify
the postgres-store-layer stamps end-to-end.

## References

- [Stripe Test Clocks engineering blog](https://stripe.dev/blog/test-clocks-how-we-made-it-easier-to-test-stripe-billing-integrations)
- [Stripe TestClock API reference](https://docs.stripe.com/api/test_clocks)
- ADR-027 (customer-level test clock pin)
- ADR-028 (billing engine period loop + disjoint flows)
- ADR-029 (fully disjoint test-clock flows across every time-aware
  engine path)

## Amendment 2026-05-28 — audit-log actor distinction

**What changed:** Audit-log `created_at` is wall-clock for **all** audit
rows, not "wall-clock by default but simulated when the handler binds
ctx." Sim-time of the action on a clock-pinned entity moves into
metadata as `sim_effective_at` + `test_clock_id`.

**Why:** The table line above (originally line 131) always said
wall-clock for forensics. But two weeks after this ADR shipped, the
`subscription.auditCtxForSub` helper landed in `internal/subscription/
handler.go` — it bound ctx to `sub.UpdatedAt` (simulated) before
calling `audit.Logger.Log`, so subscription audit rows stamped
simulated `created_at`. Invoice and dunning handlers never had that
helper, so their audit rows stayed wall-clock. The result was a
mixed-domain `audit_log.created_at` column where:

- Subscription mutations on clock-pinned subs → `created_at` = simulated
- Invoice / dunning mutations on the same clock-pinned customer →
  `created_at` = wall-clock
- Middleware catch-all → wall-clock unless a handler bound ctx earlier

This broke the audit page's compliance story (auditor can't trust
"when did X happen" because the answer depends on which handler wrote
the row) and confused the activity-timeline-on-sub-detail-page
rendering (the embedded audit rows showed timestamps an operator
couldn't reconcile against the wall-clock "Sagar clicked Cancel just
now" they actually experienced).

**The clean actor model:**

| Actor | `created_at` domain | Sim-time exposure |
|---|---|---|
| **Operator action** (HTTP handler emits audit row) | wall-clock — when did Sagar click? | `metadata.sim_effective_at` + `metadata.test_clock_id` if entity is clock-pinned |
| **Engine-generated event** (invoice.issued_at on period rollover, dunning row scheduled by RunCycleForClock) | simulated — engine ran on the clock's frozen_time | Already in the primary timestamp; no metadata enrichment needed |
| **External-event ingest** (Stripe webhook `occurred_at`, SMTP dispatch) | wall-clock — real network event | n/a |
| **Async-worker writes** (background workers running on wall-clock cadence) | wall-clock — worker ran in real time | n/a |

**Code changes:**

- `internal/audit/audit.go`: `created_at = time.Now().UTC()`
  unconditionally; dropped the `clock` import.
- `internal/api/middleware/audit.go`: same — middleware catch-all
  stamps wall-clock. (That middleware was DELETED by ADR-090's uninstall,
  2026-07-14; audit rows are now emitted in the business transaction and
  `created_at` stays wall-clock there. The sim axis moved to first-class
  `sim_effective_at` / `test_clock_id` columns — migration 0148.)
- `internal/subscription/handler.go`: `auditCtxForSub` deleted;
  replaced with `auditMetaForSub(sub, extra) map[string]any` that
  injects `sim_effective_at` + `test_clock_id` into the metadata bag
  for clock-pinned subs. All ~11 call sites updated.
- `internal/dunning/handler.go`: `resolveRun` now binds ctx via
  `clock.BindEffectiveNow` from the invoice's pin before calling
  `invoices.MarkPaid` — this was a separate pre-existing leak where
  `invoice.paid_at` stamped wall-clock on clock-pinned invoices.
  Added `dunning.Handler.SetResolver`; wired from `router.go`.
- UI: `web-v2` audit-log page reads `metadata.sim_effective_at` /
  `metadata.test_clock_id` and renders an "effective on clock X at
  sim-T" subline under the wall-clock primary timestamp on rows that
  have it.

**Industry-research validation:** Stripe's audit/Events API records
operator-action `created` in wall-clock; engine-generated events from
test-clock advance use the simulated `created` from the advance.
Chargebee / Orb / Lago / Recurly all use single-timestamp-per-row but
the timestamp is the effect-time of the event, not the audit-actor
click time. The convergence is "one timestamp per row," not "always
simulated." Velox's choice — wall-clock primary on audit rows, sim
effect-time as metadata — preserves both the forensics answer and the
sim-effect answer without conflating them.

**Why no `physical_created_at` column?** The line above still holds —
adding a parallel forensics column on business-logic rows forces every
read path to choose. The audit log IS the forensics table; that's
where wall-clock lives.

## Addendum (2026-06-02): the boundary taxonomy + read-side authoritative flag

A product-wide audit confirmed the model above is sound (5 of 7 subsystems
clean) but surfaced two recurring failure modes at the edges. This addendum
codifies the rule so neither recurs.

**The taxonomy — which clock a timestamp rides:**

- **Clock-pinned** (`clock.Now(ctx)` / `effectiveNow`): records *when a
  billing-domain event happened on the customer's timeline* — the thing the
  simulation fast-forwards. Invoice lifecycle (created/issued/due/paid/voided),
  billing-period boundaries, subscription lifecycle, credit-ledger entries,
  dunning **run/event** domain times, reconciler `paid_at`.
- **Wall-clock** (`time.Now()` / SQL `now()`): records a *real-world
  operational event outside the simulation* — fires in the operator's
  datacenter regardless of any customer clock. Audit `created_at` (with
  `sim_effective_at` as the secondary), all infra outbox/dispatch/**retry
  scheduling**, webhook delivery, auth/session windows, Stripe-sourced times,
  `usage_events.timestamp` (the metered activity's real instant), and —
  caught by this audit — **operator-administrative config writes**
  (`dunning_policy.created_at/updated_at`, test-clock object audit columns).

**The litmus test:** *"If the operator never advanced this clock, would this
event still need to fire at this real instant?"* Yes → wall-clock. *"Does this
only make sense relative to the cycle being simulated?"* Yes → clock-pinned.
Administrative config and infra plumbing always escape the simulation; per-
customer billing lifecycle never does.

**Read-side rule — persist an authoritative flag, never re-derive:** when a
read path (timeline, header, list) must render *whether* a row is simulated,
it reads a flag the write path persisted (`invoices.is_simulated`, captured at
create from the owning entity's **`test_clock_id`** — engine: `sub.TestClockID
!= ""`; manual composer: the customer's `TestClockID != ""`). Do NOT re-derive
from the parent's *current* `test_clock_id`: that is a mutable read-time
snapshot (it rots when a clock is unpinned) and it misses manual one-off
invoices, which have no subscription to look through. Re-deriving was the exact
bug this addendum closes — the invoice timeline read
`subscription.test_clock_id` at render time, so manual invoices on a
clock-pinned customer showed simulated dates with no badge.

**Capture it from the pin, not the ctx binding.** The flag must come from the
entity's `test_clock_id`, NOT from "is the ctx clock-bound" — because
`BindEffectiveNow` binds the ctx to the resolver's effective-now even for an
*unpinned* entity (the resolver returns wall-clock for those, not an error), so
a binding-based check ("is an effective-now bound?") reports true for *every*
resolvable entity and would mis-flag every manual invoice as simulated. The
ctx binding drives the *timestamps*; the `test_clock_id` drives the *flag*.

**UI rule — don't interleave two clocks in one list.** The invoice activity
timeline is domain-time (possibly simulated); customer-notification emails are
wall-clock dispatch time. Rendering them in one chronological list sorts a
real-time "sent" row *before* the simulated event that triggered it. They live
in separate lanes ("Activity" vs "Notifications"), each internally coherent.
Email dispatch is genuinely wall-clock — it is NOT clock-pinned to fake
coherence; the lanes make the boundary visible instead.

**Implementation (this change):**

- `invoices.is_simulated BOOLEAN NOT NULL DEFAULT false` (migration 0109),
  stamped at every create site from the entity's `test_clock_id` — engine
  (`sub.TestClockID != ""`) and the manual composer (the customer's
  `TestClockID != ""`, read via `invoice.Service.SetCustomerClockReader`).
- `internal/invoice/handler.go`: the timeline reads `inv.IsSimulated` for
  lifecycle/dunning rows; the dead `SubscriptionClockReader` snapshot lookup is
  removed.
- `internal/dunning/postgres.go`: policy-config writes flipped to wall-clock
  (run timestamps stay clock-pinned).
- `web-v2`: invoice header + list render the authoritative `is_simulated`
  badge; the activity timeline splits email rows into a real-time
  "Notifications" lane.

## Amendment 2026-07-18 — narrative surfaces render sim-primary; forensic surfaces stay wall-primary

**What changed (display only — storage unchanged):** Audit-log
`created_at` remains wall-clock **always**, with `metadata.sim_effective_at`
+ `metadata.test_clock_id` enrichment. What changes is which stamp each
*surface* leads with:

- **Narrative surfaces** (the Activity timeline on subscription/customer
  detail pages): the row's primary right-aligned timestamp is now
  `sim_effective_at` when present — the simulated instant that lines up
  with every other date on those pages (periods, invoices, stat cards).
  Wall-clock is demoted to a muted provenance subline:
  `Recorded <wall> · by <actor>`. The raw test-clock ID leaves the
  visible copy (operator jargon; the page header already links the
  clock). *(The per-row chip this clause originally kept — with the
  dual-stamp tooltip — was removed 2026-07-21 by ADR-099's
  once-per-scope rule: the page banner is the sole simulation flag on
  a detail page, and the row's own two timestamps carry the
  provenance. The sim-primary / wall-subline rendering rule of this
  amendment is unchanged and reaffirmed by ADR-099.)*
- **Forensic surfaces** (the Audit Log page): wall-primary, unchanged —
  that page answers "what happened when in the real world", and the
  clock ID is legitimate forensic detail there.

**Why:** Found live walking FLOW TC7. The subscription page said
"Created: Jun 1, 2027" in Properties while the Activity row for the
same event led with "Jul 18, 2026" — two dates for one event on one
page, with the prominent one contradicting the page's own timeline.
This inverted the ADR's core principle ("operators never need to know
about wall-clock when working with test clocks"). The 2026-05-28
amendment's "subline below the wall-clock primary stamp" rendering is
superseded for narrative surfaces only; its storage rule is untouched.

## Amendment 2026-07-21: the payment state-sync reconciler is wall-plane by design

The 2026-07-21 clock-design review flagged that the `payment_unknown` /
`payment_processing` reconciler (`listInflightPayments`) carries no
`is_simulated` gate, unlike every sibling invoice sweep. That is ratified,
not fixed: it is this ADR's Stripe exception applied consistently. Payment
truth (PaymentIntent state) lives at Stripe on real time even for
clock-pinned invoices — Stripe's own test clocks draw the same line — and
the catchup worker deliberately has no payment-sync phase, so gating the
wall sweep would strand an ambiguous simulated charge in
`unknown`/`processing` with no resolver (a liveness sink). The settle path
re-binds simulated time (`BindEffectiveNow`, `velox_anchor_at`), so a
wall-tick recovery still stamps outcomes at the simulated instant; the one
disjointness bruise — a failed settle enrolling dunning at the wall tick —
is bounded, because the created run only ever ADVANCES during a clock
Advance (the dunning sweep is gated).

## Amendment 2026-07-29 — credit notes: the entity's clock decides, not the actor's

Found by walking FLOW I13 on a clock-pinned invoice: an engine-issued
clawback credit note landed on the simulated axis while an operator's
credit note **on the same invoice** landed on wall-clock, dated five
weeks *before* the invoice it credited.

Two causes, both now closed:

1. `creditnote.Service` held **no `clock.Resolver` at all**. Only the
   catchup path bound time (it is handed `frozen_time` explicitly), so
   every operator entry point — Create, Issue, Void, CreateRefund,
   RetryRefund, CreateAndIssueCommitRelief — stamped wall-clock onto rows
   owned by a clock-pinned invoice. Precisely the scaling failure this
   ADR's Context section predicted: *"every new code path is one forgotten
   resolver call away from re-introducing the bug."*

2. `buildCreditNote` encoded the opposite rule in a comment —
   *"Operator HTTP issuance is always wall-clock"* — as
   `IsSimulated: input.IsSimulated && inv.IsSimulated`, with a unit test
   pinning it. That belief has no basis in this ADR: the **audit log is
   the sole always-wall-clock carve-out**, and the decision table lists
   CreditGrant and every clock-pinned `created_at` as simulated.

**Rule:** `IsSimulated: inv.IsSimulated`. A credit note is a financial
document in the invoice's timeline, not a record of a click. The clock
belongs to the entity, not to whoever acted on it.

**Nothing forensic is lost**, which is the load-bearing point. The
`credit_note.issued` audit row still carries wall-clock `created_at`,
`actor_type=user`, and `metadata.sim_effective_at` + `test_clock_id` —
verified live. The old rule was not buying forensics; it was duplicating
the audit log's job on the financial document and paying for it with an
incoherent one.

**Operator-visible consequences of the old rule** (all observed, not
theorised): a credit note dated before its own invoice; the two halves of
one downgrade-then-credit sequence rendered five weeks apart in opposite
directions; a row displaying the test clock's date sitting under the
"Real times — not the test clock's dates" caption; and — the sharp edge —
`is_simulated=false` meant the row would **survive the ADR-086 teardown**
that deletes a clock's data, stranding a credit note against a deleted
invoice plus a ledger grant on a real customer balance.

One deliberate consequence: operator credit notes on simulated invoices
now join engine ones in the refund-retry queue's `is_simulated = false`
exclusion (*"test-clock CNs aren't an operator obligation"*). That
removes an asymmetry rather than creating one — previously two identical
credit notes on the same simulated invoice were treated differently based
on who created them.

Regression: `TestOperatorCreditNote_StampsSimulatedTime` (mutation-verified)
plus an unpinned negative; `TestCreate_IsSimulated`'s middle case amended
from "operator HTTP issue → wall-clock" to "→ simulated".
