# ADR-105: The charge idempotency key is seeded by an attempt counter, not `updated_at`

**Status:** Accepted
**Date:** 2026-07-30
**Supersedes:** the `velox_inv_<id>_<UpdatedAt>` key shape, and the
money-path playbook's gate #2 wording that blessed it.
**Builds on:** ADR-029 (disjoint wall/clock charge flows), ADR-049
(payment reconciler), ADR-059 (in-flight invoice guards).

## Context

Every Stripe charge carries an idempotency key. Stripe's contract:

- **same key + same params** → returns the *original* PaymentIntent, no
  second charge;
- **same key + different params** → `409`, the call is refused.

So the key answers exactly one question: *is this call the same charge
attempt as before, or a new one?* Two requirements pull against each
other, and both are money-critical:

| | Requirement | Cost of violating it |
|---|---|---|
| **R1** | A call that reached Stripe but whose outcome was never recorded (crash, timeout, lost response) must RETRY UNDER THE SAME KEY | **Double charge.** PI_A charges, PI_B charges |
| **R2** | A RECORDED decline must retry under a FRESH key | **Liveness sink.** Stripe replays the declined PI forever; the customer fixes their card and still cannot pay, with no error raised |

The key must therefore move **exactly when an attempt outcome is
recorded**, and at no other time.

The implementation used `inv.UpdatedAt` as the seed. The reasoning was
sound on its face: a recorded failure runs `UpdatePayment`, which writes
`updated_at` (R2 ✓); a crash writes nothing at all, so `updated_at` is
unchanged and Stripe dedups (R1 ✓). No extra column, no extra write.

## The problem

`updated_at` does not answer "was an attempt outcome recorded?" It
answers "did *anything* touch this row?" — a **broad proxy for a narrow
question**. That makes every other writer on the invoice an accidental
participant in the payment protocol:

```
tick N     auto-charge calls Stripe, PI_A created ($100), charging the card
           process dies before the outcome is stamped
           invoice: payment_status='pending', updated_at unchanged   ← R1 still holds

tick N+1   tax_commit lands its transaction id  → updated_at MOVES
           auto-charge retries, different key   → PI_B created ($100)

           both settle. $200 collected on a $100 invoice.
```

`tax_commit` is not a hypothetical: it is the reconciler that runs
*immediately before* the auto-charge sweep in the same scheduler tick.
Credit application, the sweep's own no-PM email marker, and operator
actions sit in the same window.

The drift was **observed**, not theorised (#677):

```
velox_inv_<id>_1785420781393005000  →  velox_inv_<id>_1785420781397042000
```

The deeper problem is that the invariant could not be stated, so it could
not be followed. It is not "never touch `updated_at`" — most writers
should. The true rule was roughly *"don't touch `updated_at` on a
finalized invoice with `auto_charge_pending=true` while a charge may be
in flight, unless you are recording an attempt outcome, in which case you
must."* The codebase already carried one hand-made exception for it
(`ClaimAutoCharge` skips `updated_at` and explains why, #396). Every
future writer needed the same reasoning and nothing forced it.

## Decision

Seed the key from an explicit counter, `invoices.charge_attempt_seq`
(migration 0165):

```
velox_inv_<invoice_id>_<charge_attempt_seq>[_<purpose>]
```

The counter is bumped by exactly the three writes that record a charge
attempt outcome — the three that stamp `stripe_payment_intent_id`:
`UpdatePayment`, `MarkPaymentFailedReportingTransition`,
`markPaidReportingTransition`. Nothing else touches it.

The invariant is now statable in one line, and therefore enforceable:

> **Every UPDATE on `invoices` that sets `stripe_payment_intent_id` bumps
> `charge_attempt_seq`. Nothing else does.**

`TestChargeAttemptSeqBumpedByEveryPIStamp` scans the store's SQL and
fails the build in both directions — a stamp without a bump (R2 broken:
liveness sink) and a bump without a stamp (R1 broken: re-seeding from an
unrelated write). This is a class-C2 hazard (a guard spread across
several writers), so the mechanised gate is the point, not a nicety.

### Corollary: an idempotency conflict is ambiguous, not a rejection

`classifyStripeError` grouped `ErrorTypeIdempotency` with card declines
and invalid requests under *"Stripe explicitly rejected the request; no
charge occurred"*. That is backwards. Stripe raises it when the key was
**already used with different parameters**, so the one thing it proves is
that an attempt under this key reached Stripe — a PaymentIntent may be
live and charging. Treating it as a definite failure stamped
`payment_status='failed'`, started dunning inline, told the customer
their card had failed, and freed the next retry to open a second
PaymentIntent beside the live one.

It is now classified `Unknown`, routing to the payment reconciler — the
component whose entire job is resolving "an attempt may exist at Stripe".

## Consequences

**The drift class is gone by construction.** Unrelated writers cannot
reach the seed. This is the substance of the fix.

**409s become relatively more likely, and that is the correct trade.** A
param change (a credit lands, the customer swaps cards) inside the crash
window used to produce a *new key and a silent second charge*; it now
produces a *409 and a loud failure*. Losing money silently is strictly
worse than refusing loudly.

**One residual, pre-existing and unchanged by this ADR.** If a 409 (or
any ambiguous error) carries no PaymentIntent id, the reconciler cannot
query what it cannot name: `reconcileOne` settles the invoice failed, and
a later retry — now with a bumped seq — may open a second PI beside a
live one. This is the "ambiguous charge with no PI id" class, which
predates this change and is untouched by it. Mitigation shipped here: the
charge path logs the idempotency key at ERROR for that exact case, and
Stripe's dashboard is searchable by idempotency key, so an operator has a
route to the live PaymentIntent.

The complete fix is to record the attempt **before** calling Stripe, so
recovery never depends on the response. That belongs with ADR-062's
obligation queue rather than a bespoke table, and is deferred with an
explicit trigger: **before production cutover, or the first report of a
duplicate charge, whichever comes first.**

**Backfill is a no-op by design.** Existing rows default to 0. Pre-migration
keys were nanosecond timestamps, which never equal 0, so no in-flight key
can collide with the new shape — and Stripe expires idempotency keys after
24h regardless.

## Alternatives considered

**Keep `updated_at`, forbid unrelated writes in the charge window.**
Rejected: the rule cannot be stated simply (see above), so it cannot be
followed or mechanised. The existing hand-made carve-out in
`ClaimAutoCharge` was evidence of the maintenance burden, not a
counter-example.

**Include the mutable params (amount, payment method) in the key.** Then
a param change yields a different key and no 409 — but PI_A is still
live and unrecorded, so it converts a loud 409 into a silent double
charge. Strictly worse.

**Bump the counter at claim time instead of at outcome time.** Breaks R1
directly: a crash after the bump but before the outcome means the retry
presents a different key and opens a second PI. Recovering that would
need two counters (attempted vs resolved) — a state machine to avoid one
column.

**Search Stripe by `metadata.velox_invoice_id` on 409.** Viable (the
metadata is already stamped) and it would close the residual above, but
it adds a Stripe client method and a network round-trip on a rare path.
Deferred with the ADR-062 work rather than bolted on here.
