# ADR-110: A written-off invoice closes the customer's self-service payment route

**Status:** Accepted (2026-08-05)
**Relates to:** [ADR-036](036-dunning-campaigns-model.md) (dunning campaigns —
`mark_uncollectible` is a terminal final action), [ADR-107](107-unknown-is-terminal-until-a-human.md)
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

Velox **cannot charge a written-off invoice**. `chargeInvoice`
(`internal/payment/stripe.go:502`) refuses anything but `finalized`, and the
status machine's allowed-source set has no reopen edge — `finalized←draft;
voided←draft/finalized/uncollectible; uncollectible←finalized`. A customer Pay
button on such an invoice would call a gate that rejects it. Removing it
removed a button pointing at a closed door; the parity check below confirms
that is also where the industry sits, but the code settles it either way.

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

## Accepted limitation, deferred with a trigger

**Velox's write-off is a card dead end where Stripe's is not.** Stripe keeps
`POST /pay` legal from `uncollectible`, so an operator can still run the card.
Velox cannot: there is no `uncollectible → finalized` edge, and the only
settlement route is `RecordOfflinePayment` — the customer pays out of band and
an operator records it.

This matters more than its rarity suggests, because the state is
**machine-set**: `internal/dunning/service.go` marks it automatically when
retries exhaust under `final_action='mark_uncollectible'`. It arrives by
timer, at scale, and the customer who returns weeks later with a working card
is the highest-intent payer there is. "Contact support" is the
highest-friction path that exists for money coming *in*.

**Not built now** — zero customers, no named pressure, and it is a backend
capability question rather than the UI question this ADR settles.

**Trigger to close it:** the first real request to pay a written-off invoice
by card. The fix is then an **operator-side reopen**
(`uncollectible → finalized`), Velox's analogue of Stripe's `POST /pay` — not
a customer-facing Pay button, which would still front a refusing gate.
Secondary trigger: `mark_uncollectible` becoming a common dunning final
action, which converts a rare state into a routine one.

**Cheap check that would upgrade this ADR's confidence:** in a Stripe sandbox,
create → finalize → `mark_uncollectible` → open `hosted_invoice_url` and look.
~5 minutes, and the only thing that converts the one unverified cell. It would
not change the decision (2–1 at worst); it would change how confidently the
Stripe row can be written.

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
