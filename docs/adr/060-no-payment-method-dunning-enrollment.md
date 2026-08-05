# ADR-060: Card-less invoices enter dunning (no-payment-method enrollment)

## Status

Accepted — 2026-06-23.

## Context

A product-wide **liveness / termination-invariant** audit (every stateful
entity must reach a terminal state; all entry points into a shared state
must converge) surfaced an absorbing sink in the collection path.

When a subscription invoice finalizes for a customer with **no resolvable
payment method**, the engine flags it `auto_charge_pending=true`, sends one
"add a payment method" email, and moves on. The background auto-charge
sweep (`RetryPendingCharges`) re-checks it every tick — but with no card
there is nothing to charge, so it does nothing, indefinitely.

Crucially, dunning — the machine that escalates a failing invoice to a
**terminal** (`pause` / `cancel_subscription` / `mark_uncollectible`) — only
starts from an *actually-attempted-and-declined* charge (`ChargeInvoice`
fires `StartDunning` inline on a decline). A no-card invoice never attempts
a charge, so it never enters dunning, so it never reaches a terminal:

- the subscription stays `active`,
- it keeps drafting invoices each cycle,
- the unpaid balance grows **unbounded**,
- nothing ever forces a pause/cancel/write-off decision.

This is the asymmetry: the **declined-card** entry point escalates; the
**no-card** entry point silently dead-ends. It is also a `feedback`-class
silent fallback — a path that cannot produce a correct outcome parks
instead of failing loud.

Industry framing (verified): keeping an unpaid subscription *active during
recovery* is the norm (Stripe/Chargebee/Recurly/Orb/Lago/Metronome); the
defect is not "active while unpaid" but "active while unpaid **with no
off-ramp**." Grace is correct only if it terminates.

## Decision

Route card-less `auto_charge_pending` invoices into the **same dunning
machinery** the declined-card path already uses, rather than inventing a
parallel recovery system or a new subscription state.

Mechanism:

- The engine gains an optional `DunningStarter` interface (mirroring
  `NoPaymentMethodNotifier`), wired in `router.go` to a thin adapter over
  `dunning.Service`. Nil = inert (local dev / tests).
- `EnrollStalledForDunning` (cron) and `EnrollStalledForDunningForClock`
  (test-clock catchup) reuse the existing `ListAutoChargePending` /
  `ListAutoChargePendingForClock` candidate queries and call the
  **idempotent** `StartDunning` for each candidate.
- The sweep runs **after** `RetryPendingCharges` in the cycle, so the
  remaining `auto_charge_pending` candidates are the genuinely card-less
  ones: a decline already cleared the flag and started its own run; a
  success cleared the flag. `StartDunning` is one-run-per-invoice, so any
  invoice that still carries a run is a no-op.
- The dunning retrier (`RetryPayment`) already returns a *real failed
  attempt* on "no payment method" (not `ErrTransientSkip`), so the campaign
  ticks through grace + retries and **exhausts to the policy
  terminal actions** (ADR-112 split these into `final_subscription_action`
  and `final_invoice_action`) — the card-less delinquent reaches a terminal.
- Adding a card mid-campaign lets the existing auto-charge sweep collect it
  and the run resolves `payment_recovered`.
- A tenant with dunning **disabled** keeps the invoice un-dunned: the
  adapter swallows the `dunning is disabled` `InvalidState` as a deliberate
  no-op (same outcome the declined-card path gets with dunning off) rather
  than emitting a per-invoice error every tick.

No migration: reuses the `auto_charge_pending` flag, the candidate queries,
and the dunning state machine.

## Consequences

- The no-card limbo sink is closed on both the wall-clock and test-clock
  paths; a card-less subscription now reaches a terminal exactly as a
  declined-card one does.
- Telemetry: the scheduler logs a `no-payment dunning enrollment` line with
  a swept count; errors are collected per-invoice so one bad row doesn't
  abort the sweep.
- Minor wart: a no-card dunning run ticks `attempt_count` on retries that
  never attempt a charge ("attempt N of M" on a never-charged invoice). The
  dunning-list label keys on the run so it reads as a no-payment campaign,
  not a card decline. A two-phase accounting refinement (don't tick until a
  card exists) is a deferred follow-up.

## Alternatives considered

- **An explicit `incomplete`/gated subscription state (Stripe
  `payment_behavior`).** Rejected as the *default* — Velox is B2B,
  operator-gated, PaymentIntent-only; create-live matches the AI-native peer
  set (Orb/Lago/Metronome). An *opt-in* gate is deferred (below).
- **A persisted `collection_state`/`past_due` subscription column.**
  Rejected — it would be a second source of truth that desyncs from invoice
  truth (verified: `SettleSucceeded` doesn't resolve dunning runs, so a
  stored status would lie on self-pay/reconciler paths). Health, when
  surfaced, will be *derived* on read.
- **Do nothing (pre-launch, no victim yet).** Rejected — this is a silent
  money/recovery sink in a core path, the class Velox fixes at the root
  regardless of stage; the fix is contained and zero-migration.

## Deferred (trigger-gated, for a design partner)

- Operator `collection_health` signal (`current|past_due|unpaid`), derived
  on read.
- Opt-in first-payment gate (`require_payment`, reusing `draft`).
- An `unpaid` access-revoke terminal between `pause` and `cancel`
  (Stripe's revoke-but-recoverable step).
- Usage spend-cap hard-stop / prepaid-balance (the AI-native exposure
  limiter for unbounded unpaid usage).
