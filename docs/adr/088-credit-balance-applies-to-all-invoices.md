# ADR-088: Customer credit balance applies to every invoice at finalize, card charged the remainder

**Status:** Accepted (2026-07-11)
**Context:** ADR-087 recorded credit-apply presence as a per-site product
decision (census D2, trap R1) rather than a refactor side-effect. This ADR is
that decision, grounded in a 103-agent adversarially-verified survey of
primary vendor docs (Stripe, Chargebee, Lago, Orb, Metronome — all claims
3-0 verified, 2026-07-11).

## Decision

The customer credit balance auto-applies to **all** invoice types at
finalize-time amount computation — day-1 subscription-create, cancellation
final, and operator-composed manual invoices join the existing cycle-close and
threshold sites. The card is charged only the post-credit remainder; a fully
covered invoice settles paid with no payment attempt. No per-invoice or
per-type opt-out.

## Industry evidence (verified quotes, primary docs)

- **Stripe** applies the balance "to the next invoice finalised for the
  customer" — and its own definition of that population includes "manually
  create a one-off invoice." No per-invoice opt-out exists in GA ("You can't
  choose a specific invoice to apply the credit balance to"); partial coverage
  charges exactly the remainder ("The charge that gets generated for the
  invoice will be for the amount specified in amount_due"); full coverage
  auto-pays with no charge. (docs.stripe.com/billing/customer/balance,
  /invoicing/customer/balance, /api/invoices/object)
- **Chargebee** auto-applies at invoice generation to "all recurring and
  one-time invoices"; it is the only surveyed platform with per-invoice-type
  toggles (Credits Flexibility, Beta).
- **Lago** applies wallet credits to all subscription invoices at generation
  but **categorically excludes one-off invoices** with no override.
- **Orb** applies prepaid credits inside invoice calculation (pre-tax) but
  only to in-arrears charges — its credits are usage-commitment-flavored, a
  different primitive from a general balance.
- **Metronome** applies credits continuously against the draft.

Parity synthesis: **no platform excludes a subscription's first or final
invoice** from automatic application — Velox charging full price there was out
of parity with every surveyed vendor. One-off invoices split the industry
(Stripe+Chargebee apply, Lago excludes); Velox anchors on Stripe.

## Why apply at manual invoices too (the split call)

Stripe parity is Velox's stated anchor, and the two largest platforms agree.
The customer-visible logic is also simpler to explain: "your balance pays your
invoices" with no invoice-type asterisk. Rejected alternative: Lago-style
exclusion (shippable, but creates the support question "why didn't my credit
apply?"); deferred alternative: a Chargebee-style per-type toggle —
over-engineering pre-launch, trigger = the first design partner asking to
reserve balance for subscription charges only.

## Velox-specific boundaries

- The **credit ledger** is a general balance (refunds, goodwill,
  cancel-proration credits) — the Stripe model. **Prepaid commits (ADR-078)**
  are a separate line-item primitive and keep their own semantics; Orb's
  in-arrears-only rule applies to *its* commitment credits and is not imported
  here.
- Application happens at finalize-time amount computation (Stripe-exact:
  Velox's finalize step is the analogue of Stripe finalization), never at
  payment time, and never retroactively to already-finalized invoices.

## Implementation shape (trap R1 honored)

The new sites reuse the retry sweep's proven sequence — apply → reload →
settle-if-zero → collect — not billOnePeriod's block (whose $0 arm is
entangled with cycle-advance, trap R2):

*Amended 2026-10-08 ([ADR-116](116-collection-intent-at-finalize-one-collector.md)):
the per-site blocks below are gone. Every finalize site collects through one
collector (`processAutoCharge`), which applies credits before any charge on
every invoice. The policy of this ADR is unchanged; the mechanism below is
history.*

- Engine: `applyCreditsAndCollect` wraps `collectAfterFinalize` for the day-1
  (`FinalizeOnCreateInvoice`) and final-on-cancel sites. An apply failure
  **flags for the sweep and returns without charging** — never a pre-credit
  card charge (the 2026-05-30 overcharge class); the sweep already re-applies
  credits atomically before charging, so recovery pre-exists.
- Manual finalize: the handler applies via a consumer-defined `CreditApplier`
  (wired to credit.Service in the composition root), then its existing
  zero-due settle arm handles full coverage; apply failure flags for the
  sweep, same as the engine.
- Cycle-close and threshold keep their existing in-place blocks (deliberate:
  behavior-preserving; billOnePeriod's is cycle-advance-entangled). Collapsing
  them onto `applyCreditsAndCollect` is a later, separate simplification.
