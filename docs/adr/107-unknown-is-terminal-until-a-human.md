# ADR-107: An unnameable charge attempt parks the invoice; it is never settled failed

**Status:** Accepted
**Date:** 2026-07-31
**Supersedes:** [ADR-106](106-charge-intent-ledger.md) as the answer to this
problem. ADR-106's charge-intent ledger is **parked, not deleted** — see
"Why not the ledger" and its trigger.
**Builds on:** ADR-105 (`charge_attempt_seq` as the idempotency-key seed, shipped
in #678 and unaffected by this decision), ADR-049 (payment reconciler).

## Context

A Stripe charge can create a PaymentIntent that Velox never learns the name of:
the process dies between the call and the outcome write (a **timeout** does this
with no crash at all), or the call returns an ambiguous error carrying no
PaymentIntent id.

`payment.reconcileOne`'s empty-PI branch then settled the invoice **failed**,
under the comment *"a safe retry generates a new PI"*. That comment described
the bug. Settling failed does two things at once:

- it bumps `charge_attempt_seq`, which **rotates the Stripe idempotency key**, so
  the next attempt can no longer dedup against the first;
- it moves `payment_status` out of `unknown` into a state every charge-claim
  predicate admits.

So the next retry opens a **second PaymentIntent beside one that may be live and
charging**. The customer is charged twice.

## Decision

**Delete the give-up write.** When an ambiguous outcome leaves no PaymentIntent
id, `reconcileOne` logs CRITICAL and does nothing else. The invoice stays at
`payment_status='unknown'` until a webhook names the PaymentIntent (the ordinary
case) or a human resolves it.

That closes the hole **by construction**, because `unknown` is a state no charge
path admits — verified against the code, not assumed:

| Path | Predicate |
|---|---|
| `ClaimAutoCharge` — the background sweep | `payment_status = 'pending'` |
| `ClaimChargeForManualCollect` — operator "Collect payment" | `payment_status IN ('pending','failed')` |
| `ClaimChargeForDunningRetry` — dunning | `payment_status IN ('pending','failed')` |

An independent audit of every writer confirmed the closure is total: there are
exactly three production writers of `payment_status`, `unknown → pending` is
unreachable in Go (nothing passes `domain.PaymentPending` to `UpdatePayment`),
no `retry-payment` / `charge-now` / `reset` route exists, the customer portal has
no pay route, and `Void`, `MarkUncollectible` and `RecordOfflinePayment` all
refuse an in-flight invoice via `IsInFlight()`.

Because that closure IS the guarantee — three separate SQL predicates that a
fourth charge path or one careless widening could break — it is mechanised:
`TestUnknownPaymentIsUnchargeableByEveryClaimPath` claims each path against a
claimable invoice (the control, so the negative means something) and then against
an `unknown` one. Admitting `unknown` anywhere fails the build, naming the path.

### What ships with it

- **The hosted page stops lying.** `payEnabled` had no `payment_status` term, so
  a live "Pay" button rendered on an `unknown` invoice and answered 409 when the
  customer pressed it. Now gated on `IsInFlight()`.
- **The checkout claim** refuses `unknown`, not merely `processing` —
  `IsInFlight()` has always meant both.
- **Two unguarded writers deleted.** `Service.RecordPayment` and
  `RecordPaymentFailure` wrote `payment_status` with no state guard and had zero
  production callers. Under this ADR they were the single easiest way to reopen
  the hole by accident: convenient names, obvious signatures, no guard.
- **Migration 0009's rollback carries a warning.** `0009_payment_unknown.down.sql`
  rewrites every `unknown` row across all tenants in one statement, and is
  reachable from `velox migrate rollback`. It predates this invariant and is now
  load-bearing for it.

## Consequences

**A stuck invoice needs a human — and the human needs a lever.** The first
version of this ADR said "a human must act" without checking that they could.
They could not: `Void`, `MarkUncollectible` and `RecordOfflinePayment` all refuse
an in-flight payment, and `unknown` is in-flight, so the complete set of
state-changing operator actions on a parked invoice was EMPTY. An invoice that
can reach no terminal state violates the every-invoice-terminates rule, and the
CRITICAL log was instructing operators to "settle or void it by hand" — both
refused. Corrected the same day (the parked-invoice honesty sweep):

- **`MarkUncollectible` is now allowed** on a parked invoice. It is the one safe
  exit: it moves no money, and if the charge did succeed the provider webhook
  still marks the invoice paid through the ordinary recovery path. `Void` stays
  refused (a voided-then-succeeded invoice is a contradiction) and
  `RecordOfflinePayment` stays refused (it would label a card charge as
  out-of-band). The invoice page already carried the button; only the service
  refused it.
- **The banner tells the truth.** It rendered at Info severity saying "Velox
  re-checks automatically and resolves" — of an invoice that will never resolve,
  on day 1 and day 400. A parked invoice now raises Critical, states that it
  will not resolve on its own, explains that no further charge is attempted
  deliberately, and names the write-off.
- **No collection promise goes out.** `resend-setup-link` was allowed on a
  parked invoice and emails the customer "add a payment method and we'll collect
  it automatically" — a promise the engine will never keep. Now refused.
- **The Collect refusal distinguishes the two cases.** "Retry after it resolves"
  is right when a PaymentIntent id exists and wrong when one does not.

**And a liveness sink this ADR introduced.** `ListUnknownPayments` is
oldest-first with a batch limit, and a parked row's `updated_at` is frozen
because nothing ever writes it again — so parked rows permanently headed the
queue, and once a batch-size accumulated the sweep would never reach a NEW
ambiguous charge that DOES carry an id and that the provider would resolve in one
call. The safety fix had quietly converted into a liveness sink for exactly the
invoices the reconciler exists to save. Parked rows are now excluded from that
sweep at the SQL level — they are not reconcilable by definition — and the parked
state is announced ONCE, where it is created, rather than as an identical
CRITICAL every tick forever.

**How often:** only when the response is lost *and* the `payment_intent.*`
webhook never arrives. The webhook names the PaymentIntent and settles the
invoice through the ordinary path, which is what happens in nearly every real
occurrence. At zero customers the expected count is zero.

**Stuck-and-loud beats silently-double-charged.** That is the whole trade, and
it is the same one the no-silent-fallbacks rule makes everywhere else in billing.

## Why not the ledger (ADR-106)

ADR-106 solved this properly: record the attempt before calling Stripe, recover
by replaying its idempotency key. The design is sound. The implementation did not
converge:

| Review round | Reviewed | Confirmed defects | Criticals |
|---|---|---|---|
| 1 | the original implementation | 14 | 4 |
| 2 | round 1's fixes | 30 | 4 |
| 3 | round 2's fixes | 34 | 10 |

Defect density **rose** each round, and each round found the previous round's
fixes broken — including one headline fix that was completely **inert** (a
deliberately non-transient error still classified as transient downstream,
because the classifier tests a type it is not), and two individually-correct
fixes that **cancelled each other out**. Several findings were "this fix has zero
executing coverage": fakes that had drifted from the SQL they stood for, so
mutation checks agreed with themselves.

~1,900 lines and a dozen interacting guards, versus ~30 lines that make the
failure unreachable. The standing rule is that accreting containment means the
model is wrong and the answer is the simplest COMPLETE abstraction. This is that
abstraction.

**Parked, not deleted.** The branch and ADR-106 remain. What the ledger buys
over this is *automatic* recovery — no human, no stuck invoice. That becomes
worth its complexity when stuck invoices are frequent enough to cost more than
the guards do.

**Trigger to revisit:** the first real stuck `unknown` invoice, or production
cutover — whichever comes first. Resume it **on top of** this ADR, where its two
worst round-3 criticals (the give-up gate un-gating, and the reconciler treating
a never-sent request as proof of nothing) are unreachable code, because there is
no give-up write left to un-gate.
