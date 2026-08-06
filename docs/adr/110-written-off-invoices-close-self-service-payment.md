# ADR-110: A written-off invoice closes the customer's self-service payment route

**Status:** Accepted (2026-08-05)
**Relates to:** [ADR-036](036-dunning-campaigns-model.md) (dunning campaigns),
[ADR-112](112-dunning-exhaustion-settles-two-questions.md) (`mark_uncollectible`
is the terminal INVOICE action, set independently of the subscription's),
[ADR-107](107-unknown-is-terminal-until-a-human.md)
(the other "terminal until a human" state)
**Shipped by:** PR #734

## Context

Velox has three terminal invoice outcomes. Two are unambiguous — `paid`, and
`voided` (annulled; the debt never existed). The third, `uncollectible`, is
not: it is a **write-off**, the invoice stays on the books for audit, and
`RecordOfflinePayment` remains legal against it.

Two customer-facing pages disagreed about it. The hosted invoice page had said
"This invoice is closed" and dropped its Pay button since before this ADR; the
tokenized payment-update page had never been taught the state and kept showing
an amber **"Payment method needed · Amount Due $30.00"** demand. One invoice,
two links from the same dunning email, opposite answers (found live on TC Walk
Co VLX-000152; VLX-000136 had been sitting in that state with a live token).

Making them agree required deciding *which* page was right — which is a
product question, not a consistency one, so it was checked against the
industry before being settled.

## Decision

**A written-off invoice ends the customer's ability to pay it themselves.**
Both public surfaces say collection is closed and point to support. The money
figure is retained and relabelled from "Amount Due" to "Invoice amount" — the
number stays true, only its framing as a demand is dropped.

**The add-a-payment-method button stays** on the payment-update page even
though the hosted invoice page drops its Pay button. These are different
actions: paying *this* invoice is closed, saving a card still serves the
*next* one. Payment-method capture is account-scoped, not invoice-scoped.

Both pages read one predicate (`web-v2/src/lib/invoiceTerminal.ts`) rather
than re-typing the status list, because this rule had been missed three times
— #651 relabelled one figure and left its twin, the FLOW D4 walk found the
payment-update page ignoring status entirely, and that fix then stopped at
`paid || voided`.

### The decisive argument is internal, not parity

At the time of this decision Velox **could not charge a written-off invoice at
all**: `chargeInvoice` refused anything but `finalized`, and the status machine
had no edge back. A customer Pay button would have called a gate that rejects
it — so removing it removed a button pointing at a closed door.

*(History: the 2026-08-05 amendment below briefly made an operator able to
charge a written-off invoice; [ADR-113](113-nothing-charges-a-written-off-invoice.md)
removed that the next day, so the flat "cannot charge" is TRUE again — now by
decision rather than by limitation. The customer-facing closure this section
argues for was never touched.)*

## Industry evidence (verified 2026-08-05)

**2 verified closed · 0 verified open · 1 unverified.**

- **Chargebee — CLOSED.** Write Off produces an adjustment credit note and
  "the invoice's status will be marked as Paid"; `amount_due` → 0. Its
  customer surfaces pay only from the unpaid set ("the list of unpaid
  invoices"). https://www.chargebee.com/docs/billing/2.0/invoices-credit-notes-and-quotes/invoice-operations
- **Recurly — CLOSED.** "An invoice is Failed when it is deemed uncollectable
  and written off as bad debt"; the write-off "removes the amount from the
  customer's account balance." Dunning halts entirely: "Stopping collection
  also halts all dunning activity."
  https://docs.recurly.com/docs/invoices
- **Stripe — model verified, UI UNVERIFIED.** Stripe's model is the one Velox
  mirrors: "An uncollectible invoice might still be paid"
  (https://docs.stripe.com/revenue-recognition/methodology/subscriptions-and-invoicing),
  in explicit contrast to void — "the invoice can no longer be paid." `POST
  /v1/invoices/:id/pay` is a documented transition from `uncollectible`. But
  **Stripe never documents what its Hosted Invoice Page renders for
  uncollectible** — it documents that page's behaviour for *void* and for URL
  expiry, and is silent here.
- **Lago and Orb — the state does not exist.** Zero hits for
  uncollectible/write-off/bad-debt across Lago's 401 doc pages plus its
  official OpenAPI, and Orb's 309 doc pages plus its full API reference.
  "Written off but still on the books" is a Stripe concept two of the four
  usage-based peers declined to copy.

**On the label, Velox is ahead of the closers.** Stripe *preserves* the amount
on write-off (`amount_due` unchanged), which anchors keeping the figure.
Chargebee and Recurly zero it, and Chargebee's surface then tells the customer
the invoice is **"Paid"** — false to someone who never paid.

### Do NOT cite as parity

- **Any peer's customer-facing copy.** No vendor publishes hosted-page strings
  for any invoice state. Velox's wording rests on its own reasoning.
- **Stripe's hosted page.** Unverified in both directions. The
  `"hosted_invoice_url": null` in Stripe's `mark_uncollectible` API example is
  a trap, not evidence — that example invoice was never finalized
  (`number: null`, `finalized_at: null`), and the field is null for *any*
  unfinalized invoice.
- **Chargebee/Recurly closure** is entailed from mechanism (balance → 0) plus
  their payable-set wording, not from a sentence saying "the Pay button is
  hidden." Strong, but entailment. Recorded this way deliberately: ADR-078
  once carried an inference written as a citation and had to be corrected.

## The counter-signal, answered

**Orb resolves the same tension the opposite way.** When its dunning exhausts,
the invoice stays `issued` and the customer portal — which "embed[s] a form
… that allows your user to pay invoices manually" — stays payable. Orb
separates "we stopped chasing you" from "you can't pay."

Orb can do that because it never claims a write-off happened; it has no such
state. Velox does, and a write-off is an accounting assertion — bad debt
recognised. Offering a self-service payment button on an invoice the business
has formally written off invites the state to be reversed by a route that
cannot currently reverse it. The honest surface is the one that matches what
the system can actually do.

## Amendment 2026-08-05 — the limitation is closed, by charging in place

> ⚠️ **SUPERSEDED by [ADR-113](113-nothing-charges-a-written-off-invoice.md)
> (2026-08-06).** Charge-in-place recovery shipped on 2026-08-05 and was
> removed the next day on industry evidence: 1 of 6 verified platforms
> (Stripe alone) charges the written-off object. The section below is kept as
> the state this was decided in. The core decision of THIS ADR — public pages
> closed, add-a-card kept — stands, and the "three refusals" table below
> describes gates that no longer exist.

The limitation this ADR recorded — *"Velox's write-off is a card dead end where
Stripe's is not"* — **is closed.** Its trigger fired the same day it was
written, so the section it replaced is deleted rather than appended to: leaving
a "deferred with a trigger" block standing next to the thing that closed it is
the kind of doc rot this repo pays for.

**The remedy this ADR prescribed was wrong.** It named an operator-side
*reopen* (`uncollectible → finalized`). What shipped instead charges the
invoice **in place**, and the reopen edge is now explicitly rejected:

- **A reopen would erase the write-off, not reverse it.** `grep -rn
  uncollectible internal/analytics/` returns zero hits — AR and open-invoice
  counts are recomputed from CURRENT status with no time dimension. After a
  flip, no query in the system could tell an accountant the invoice had ever
  been written off.
- **It would re-enter automation.** `finalized` is the state every sweep and
  claim predicate admits, so a reopen silently re-arms dunning enrolment and
  the auto-charge sweeps on a debt the business had given up on.
- **It is not Stripe's shape either.** Stripe has no `uncollectible → open`
  edge; its only non-void exit is straight to `paid`. Charging in place is the
  closer parallel, not the further one.

So the invoice stays `uncollectible` until money actually arrives, then settles
`uncollectible → paid` — a transition `markPaidReportingTransition` already
allowed — ending with `uncollectible_at` AND `paid_at` both set, which is
Stripe's `status_transitions` shape.

**The customer-facing closure this ADR decided is untouched.** Recovery is
operator-initiated only: `ClaimChargeForManualCollect` admits `uncollectible`
while the dunning and auto-charge claims still require `finalized`. An operator
may charge a written-off invoice; no machine may. The public pages are
unchanged, so §"Decision" above still holds in full.

### Three refusals, enforced in the claim CAS

A service-side pre-read would be a TOCTOU against the very sweeps that create
these states, so each gate is SQL in the claim; the handler repeats them only
to answer *why* with a typed 409 rather than a bare claim miss.

| Refusal | Code | Why |
|---|---|---|
| tax already reversed | `tax_reversed_unrecoverable` | `MarkUncollectible` always attempts an upstream tax reversal — irreversible at the provider, and not re-committable here (23h calculation TTL; tax computation is draft-only). Charging would collect tax the tenant has already reported as not collected. |
| threshold usage re-billed | `recovery_superseded` | Writing off a threshold invoice does not stop the next cycle close re-billing that usage window; collecting this one double-bills it. |
| unapplied clawback relief | `relief_not_reissued` | An `issue_pending` credit note that was voided before issuing never reduced `amount_due`, so the figure is stale-HIGH and charging over-collects. |

The tax gate keys on the **structural** fact (`tax_transaction_id != ''`), not
the best-effort `tax_reversed_at` stamp, which is written post-commit and can
lag its own sweep.

**Parity note, verified:** Stripe *also* decreases reported tax on
`mark_uncollectible`. The difference is that Stripe owns its tax ledger and can
re-report on recovery; Velox reversed an *external* Stripe Tax transaction it
cannot restore. So the refusal is forced by architecture rather than by a Velox
defect — and **tax re-commit on recovery remains deferred**, trigger: the first
tax-registered tenant to hit `tax_reversed_unrecoverable`.

### Still deferred

- **Any `uncollectible → finalized` edge.** Trigger: a DP needs written-off
  invoices back in *automated* collection — which needs a bad-debt journal
  entry first, not a status flip.
- **Restarting dunning on recovery.** A failed recovery deliberately starts no
  campaign (both dunning-start sites are guarded). Trigger: recovery becomes a
  collections motion rather than a one-off.
- **`RecordOfflinePayment`'s own tax and threshold exposure.** The same two
  hazards pre-exist on the offline route and are NOT gated there, deliberately:
  the money already arrived, and refusing to record it would only make the
  books wrong too. Governing rule — *refuse to CREATE a bad money event; never
  refuse to RECORD one that already happened.* Tracked as its own defect.

## Consequences

- Both public pages agree, from one predicate, guarded by
  `web-v2/tests/invoiceTerminal.test.ts` — mutation-verified: dropping
  `uncollectible` fails 3 tests, and a predicate returning true for everything
  fails the 2 negative controls (an unknown status must stay **collectible**,
  since defaulting to terminal would silently stop asking for money that is
  owed).
- Written-off invoices are settleable only out of band until the trigger above
  fires.
- The "Amount Due"/"Amount due" casing split on the hosted invoice page is
  gone; both labels come from the shared helper.
