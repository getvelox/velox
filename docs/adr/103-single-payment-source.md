# ADR-103: One owner for payment rows on the invoice timeline

Date: 2026-07-28
Status: Accepted
Supersedes the payment half of: ADR-020 (timeline dedup), ADR-102 §Rendering

## Context

ADR-102 made a charge attempt a first-class fact, but kept the Stripe
webhook rows as a fallback display tier for invoices that predate the
table. That left FOUR mechanisms answering one question — *which row
represents this charge?*

1. lifecycle dedup: drop `payment_intent.succeeded` when `paid_at` is set;
   drop `payment_intent.canceled` when `voided_at` is (within a window);
2. `mergeFailedPaymentTwins`: pair a Stripe failure to its dunning row by
   PaymentIntent id — falling back to **positional index** for older rows;
3. `foldEmailIntoStripeFailed`: pair the payment-failed email to that
   Stripe row by a **2-minute time window**;
4. the ADR-102 precedence chain layered on top.

Two of those are heuristics, and this family has already produced real
bugs (#619 duplicate resolved rows, #623 a fold that missed a fourth
reason-spelling). Meanwhile the underlying data says the redundancy is
unnecessary: measured on the walkthrough tenant, every charge produces
**exactly one** attempt row and **2–3 rows of Stripe chatter**
(`created`, then `succeeded`/`failed`).

## Decision

**Payments render from one owner: `invoice_charge_attempts`.** The
Stripe webhook table is what it always was — ingestion infrastructure
for idempotency and replay — and stops being a display source.

Two supports make that safe:

1. **The outcome is resolved inside the settle transaction.** Both
   settle transitions (`MarkPaidCardSettlementTransition`,
   `MarkPaymentFailedReportingTransition`) upsert the attempt in their
   own tx, so the attempt fact and the invoice's state can never
   disagree. A settle for a PaymentIntent Velox never minted (hosted
   checkout) INSERTS the row there — that path needs no chokepoint.
   Insert-time identity (`trigger_source`, `sim_effective_at`) is
   preserved by the ON CONFLICT: the chokepoint knows those, a settle
   does not.
2. **A loud invariant replaces the silent fallback.** A card-paid
   invoice with no succeeded attempt (and no `out_of_band:` marker)
   logs a warning naming the missing fact, rather than quietly
   substituting a webhook row.

Rendering keeps exactly two suppressions, both **exact-keyed**:

- a dunning row already carrying the attempt's PaymentIntent absorbs it
  (the campaign row is the richer telling of the same charge) and lifts
  the attempt's verbatim provider reason + amount onto itself;
- a *succeeded* attempt on a paid invoice defers to the "Invoice paid"
  lifecycle row — the superset, since credits, an offline payment, or a
  $0 total all pay an invoice with **no charge at all**.

`pending` attempts never render (in flight — the attention banner owns
that); a succeeded attempt on a NOT-paid invoice does, because that
shape is an anomaly worth seeing.

Same-instant ordering gains one rank: a charge attempt sorts *above*
the dunning campaign it caused, so a decline and its "Payment recovery
started" never render effect-before-cause.

## Consequences

- Deleted: `dropCanceledForVoid`, `foldEmailIntoStripeFailed`,
  `mergeFailedPaymentTwins`, `withinWindow`, `describeStripeEvent`,
  `relevantStripeEvents`, and the ADR-102 three-tier precedence — with
  the tests that guarded them. Both heuristics (time window, positional
  index) are gone.
- **`payment_intent.canceled` no longer renders.** A void already shows
  "Invoice voided", and an abandoned checkout is an attempt that never
  completed — provider bookkeeping, not an invoice event. This also
  retires the cause-blind "Payment canceled" row flagged during the
  ADR-102 review.
- **The payment-failed email is now its own row** rather than a
  sub-line folded onto the failure. It is a distinct fact (we told the
  customer) with its own timestamp; merging it required the time
  window. On a live invoice it sorts directly after the failure.
- **Accepted cost: invoices charged before ADR-102 lose their payment
  rows** — they have webhooks but no attempts, and no backfill can
  invent an attempt that was never recorded. Acceptable at zero
  customers with fixtures we own; it would not be in production, which
  is an argument for doing this now rather than later.

## Alternatives rejected

- **Keep the fallback tier** (status quo): preserves history for old
  invoices but keeps four reconciliation mechanisms and two heuristics
  alive forever, on the surface operators trust to explain money.
- **Backfill attempts from webhook rows**: the webhook payload knows the
  PI and outcome but not the trigger or the billing-axis instant, so the
  backfilled rows would be a different, weaker fact wearing the same
  name — the kind of half-truth ADR-101 Phase 4 deliberately refused.
- **Render both and de-duplicate at read time**: that is precisely the
  four-mechanism design being removed.
