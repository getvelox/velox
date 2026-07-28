# ADR-102: Charge attempts as first-class facts

Date: 2026-07-28
Status: Accepted

## Context

A charge attempt is a billing fact — the engine tried to collect an
invoice at some instant and the attempt had an outcome — but it had no
record of its own. Its timeline representation rode on whichever
neighbor happened to exist:

- the **Stripe webhook row** (`payment_intent.payment_failed` /
  `succeeded`) — a wall-clock fact, exiled to the wall-clock lane on
  simulated invoices because its timestamp can't sort into a
  simulated sequence;
- the **dunning-started row** — anchored on the billing axis via
  `simulatedFailureAt`, but an event of an *optional* subsystem.

Consequence, found by the I5 dunning-disabled walk (2026-07-28): on a
simulated invoice with dunning off, **both** carriers are absent from
the billing axis and the Activity timeline goes silent about a failed
charge. The invoice's own story changed shape based on a collections
setting. Related smaller gaps: a lost webhook left auto-charge failures
with no timeline row at all, and a PaymentIntent-create failure
(network error — no PI, no webhook, ever) was never representable.

## Decision

**One table, `invoice_charge_attempts`: one row per charge attempt,
upserted by PaymentIntent id as provider truth arrives.**

Writers (all best-effort — see Consequences):

1. **The saved-PM charge chokepoint** (`chargeInvoice`) inserts the row
   when it creates the PI — it is the only writer that knows the
   `trigger_source` (`auto_charge` | `dunning_retry` via the PI
   purpose) and the **sim anchor** (`clock.SimOf(ctx)` during a
   test-clock Advance). Outcome at insert: `pending` (PI created),
   `failed`/`unknown` (create-time decline/ambiguity — including the
   empty-PI shape, whose row is the attempt's *only* record).
2. **The settle paths** (`SettleSucceeded` / `SettleFailed` — webhook,
   inline sync-success, reconciler) upsert the outcome by PI. For a PI
   Velox didn't mint inline (hosted checkout) this insert IS the
   record, `trigger_source='external'`, deliberately wall-clock — an
   interactive act is a wall fact.

Upsert semantics (enforced in SQL): `succeeded` is terminal; every
other outcome may advance — including `failed → succeeded` (a 3DS PI
retried within one checkout session). Insert-time identity
(`trigger_source`, `sim_effective_at`) is never overwritten by a
settle upsert. Partial UNIQUE on `(tenant_id, stripe_payment_intent_id)
WHERE pi <> ''`; empty-PI attempts are insert-only.

**Rendering: every attempt appears exactly once, via the richest owner
available — dunning row → attempt row → stripe webhook row.**

- A PI carried by a **dunning row** (the walked #639/#640 fold design)
  renders there; the attempt is suppressed.
- Otherwise the **attempt row** renders — on the billing axis
  (`sim_effective_at`) when sim-stamped, closing the dunning-off gap;
  wall-stamped attempts join the wall-clock lane. On simulated
  invoices a sim-stamped attempt **replaces** its surviving webhook
  echo (lifting the echo's folded email Detail + provider error); on
  wall-clock invoices the webhook row already sits in the right time
  domain and the attempt defers to it — zero rendering churn.
- A `succeeded` attempt defers to the `invoice.paid` lifecycle row;
  on a NOT-paid invoice it renders ("Payment collected") — that shape
  is an anomaly worth surfacing. `pending` never renders (the
  attention banner owns in-flight).

**No backfill.** Invoices predating the table have no attempt rows, so
the precedence chain bottoms out at the webhook row and they render
exactly as before. The fallback tier is permanent, not transitional —
it also covers any future lost attempt write.

## Alternatives rejected

- **Synthesize a failure row from invoice state** (`payment_status` +
  `simulatedFailureAt`): no new storage, but state-derived — the row
  vanishes when the invoice is later paid, mislabels interactive
  declines on dunning-off simulated invoices with the cycle-close
  instant, and represents only the *latest* attempt. A patch over the
  same modeling gap.
- **Enroll dunning always, act conditionally**: makes runs that aren't
  campaigns; contaminates dunning's semantics to solve a display
  problem.
- **Route webhook rows into Activity on simulated invoices**: puts
  wall-clock stamps into a simulated sequence — the exact bug class
  the timeline-surfaces audit fixed for credit notes.

## Consequences

- The attempt write is **best-effort, not in-tx** with
  `UpdatePayment`: a crash between them loses only the attempt row,
  and the renderer falls back to the webhook row — display
  degradation, never money. This is the justified-dual-write shape
  (2026-06-24 audit): the fallback chain is the reconciliation.
- `invoice_charge_attempts` cascades on invoice delete (ADR-086
  test-clock teardown).
- The second invoice-page card has one constant identity — "Real-time
  activity" — instead of renaming itself between "Notifications" and
  "Real-time activity" as folds consume rows (2026-07-28 design
  review; one caption covers emails + payment events and points to the
  customer page for dunning reminders).
- Future surface: `trigger_source` gives an exact per-attempt
  provenance if payment-attempt history becomes a product surface —
  without re-deriving it from PI metadata.
