# Card payment failure handling

Velox charges cards with Stripe PaymentIntents. How it handles the usual
payment failures, and the gaps still open.

## Rules

- **An unclear answer is `unknown`, not failed.** A Stripe 5xx, any 409, a
  network error or a timeout leaves the payment `unknown`, and nothing
  charges that invoice again until a Stripe webhook or the reconciler gives
  the answer. The two `unknown` rows under Cases say what happens next. A
  409 counts as unclear because Stripe sends it when an idempotency key is
  reused, so an earlier request may have charged the card.
- **One charge per invoice at a time, and none after a lost result.**
  Auto-charge, dunning retries and **Collect payment** share one 5-minute
  claim on the invoice, the charge hold. An open hosted Checkout page blocks
  them, and a running or `unknown` charge blocks the page
  ([ADR-068](adr/068-checkout-session-dedup.md)). A charge that sent
  nothing (the circuit breaker open, Stripe credentials unreadable) frees its
  claim at once. A claim whose charge result was lost (a crash, a shutdown,
  a closed **Collect payment**, a 429) stays set after its 5 minutes, and so
  does a parked invoice's. Every charge, the hosted page, void,
  offline payment and dunning's final action wait for it. The reconciler asks
  Stripe about it on the first scheduled run at least 10 minutes after the
  claim ended. It records a payment it finds, or clears the claim if Stripe
  has nothing. With the hourly run, that is usually within about an hour
  ([ADR-127](adr/127-a-lost-charge-result-is-resolved-by-asking-stripe.md)).
- **Idempotency key per attempt.** Re-sending the same attempt (same attempt
  counter, charge kind and card) sends the same key, so Stripe returns the
  first PaymentIntent. A dunning retry after a decline is a new attempt with a
  new key, and so is the next charge after the reconciler finds nothing at
  Stripe for a lost or parked one. Stripe may drop a key once it is 24 hours old.
- **Webhooks: deduplicated, first result wins.** A processed event ID is
  skipped. A failure arriving after the invoice is paid, or from an older
  attempt, is ignored. A transient processing error gets a 5xx, so Stripe
  redelivers; an event that names no Velox invoice is acknowledged.
- **Charges do not wait on webhooks.** The reconciler asks Stripe on every
  scheduled run: hourly, or every 5 minutes when `APP_ENV` is `local` or
  unset. It reads `unknown` payments that carry a PaymentIntent ID after 1
  minute, `processing` after 30 minutes, hosted Checkout sessions after 5
  minutes, and lost or parked charges through Stripe's list of the
  customer's payments once the claim is 10 minutes old. Results go through the same settlement code as webhooks. Still
  webhook-only: refund status after a pending refund. A decline Stripe shows
  for a parked charge counts as no payment, so the charge 24 hours later
  records the decline again.

## Cases

| Case | Velox | Operator |
|---|---|---|
| Card declined on a Velox charge | Payment `failed`, customer emailed (if SMTP is set), `payment.failed` sent, dunning starts (new accounts' default policy: day 3, 7, 14). A decline on the hosted page leaves the invoice as it was. A card the customer saves through a setup or update link becomes the default; the next retry charges it. | Nothing, or ask for a new card (**Update payment method**). |
| Card refused for good: lost, stolen, pick up card, revoked authorization and the rest of Stripe's hard decline codes | As above, but retries no longer charge that card for this invoice. They keep their schedule and count, and charge only once another card is the customer's default. A card saved after the refusal becomes the default, as above (paying on the hosted page also saves one). The customer is asked for a different card; the reason is not named. The invoice says "needs a different card". | Ask for a new card (**Update payment method**), or let the customer pay on the hosted page. A card saved before the refusal needs **Set as default** on the customer's page ([ADR-126](adr/126-a-card-refused-for-good-is-not-retried.md)). |
| The bank wants the customer to confirm the payment (3-D Secure, Stripe's `authentication_required`) | Payment `failed` and dunning starts, as for a decline. Retries count but don't charge that card while it is the default, as in Stripe Billing; a card saved through a setup or update link becomes the default and the next retry charges it. The customer's email and each reminder ask them to confirm on the hosted invoice page, with nothing about a decline. The invoice says "Customer must confirm payment". | **Email payment link**: the customer pays and confirms on the hosted invoice page ([runbook](ops/runbook/customer-must-confirm-payment.md)). |
| Stripe 5xx that names a PaymentIntent | `unknown` ("Confirming payment"); the reconciler reads the PaymentIntent after 1 minute, or a webhook settles it. | Nothing. If it lasts hours: [runbook](ops/runbook/unclear-charge.md#unconfirmed-with-a-paymentintent). |
| An unclear answer with no PaymentIntent ID: a 5xx, any 409, a network error, or a request past the caller's time limit (15 s for dunning, 30 s for auto-charge and **Collect payment**) | Invoice *parked* ("Payment status unknown"); nothing charges it. Every run from 10 minutes after the claim, the reconciler lists the customer's payments in Stripe: a succeeded payment marks the invoice paid, a processing one is followed. If none has appeared 24 hours after the attempt, the invoice goes back to `pending` (`failed` after a dunning retry) and is charged again as usual, under a new idempotency key ([ADR-127](adr/127-a-lost-charge-result-is-resolved-by-asking-stripe.md)). | Nothing. If the customer paid another way, **Mark uncollectible** within the 24 hours: [runbook](ops/runbook/unclear-charge.md#parked-no-paymentintent). |
| Crash after finalize, before the charge | Finalize and the charge queue are one write; the next run charges it. | Nothing. |
| Crash after Stripe charged, before Velox saved; or Velox stopped waiting for its own reasons (a shutdown, a closed **Collect payment**) | The charge's claim stays set, so nothing charges again: not another charge kind, a changed card, the hosted page, or the same attempt after Stripe dropped its key. Void and offline payment are refused; write-off is allowed. A webhook marks it paid. Without one, the reconciler lists the customer's payments in Stripe once the claim is 10 minutes old. A payment found is recorded (a second one is flagged); a payment still processing keeps the claim until it ends; nothing found clears the claim (rule 2). | Nothing. If Stripe is disconnected or the customer is gone from it, the claim cannot clear: [runbook](ops/runbook/unclear-charge.md#interrupted-velox-cannot-ask-stripe). |
| Concurrent charges (auto, dunning, manual, second replica) | One wins the claim; a refused **Collect payment** says a charge is being confirmed with Stripe. | Nothing. |
| Customer pays on the hosted page during a Velox charge | Mutually exclusive (see Rules). Sessions expire after 1 hour. | Nothing. |
| Second payment, amount or currency mismatch, money on a voided invoice | "Payment needs review"; `payment.duplicate_charge`, `payment.amount_mismatch`, `payment.currency_mismatch` or `payment.received_on_voided_invoice` sent. | Refund the extra, or reconcile the difference, in Stripe. |
| Customer disputes a card payment (a chargeback: the customer asks their bank to reverse it) | Recorded against the invoice, which stays paid. `payment.dispute_opened` sent, then `payment.dispute_closed` when Stripe closes it. A lost dispute shows a critical notice on the invoice. Needs the three `charge.dispute.*` events on the Stripe webhook endpoint. | Respond in Stripe before the deadline. If lost: a credit note with the money returned outside Velox (never a card refund), a new invoice, or a write-off in your own books ([runbook](ops/runbook/card-disputes.md)). |
| Void, write-off or offline payment while a charge is running | Refused until the charge settles. A parked invoice can still be written off; if its charge later turns out to have succeeded, it is marked paid. After a lost result, void and offline payment wait for the reconciler (rule 2); write-off is allowed. | Retry after it settles. |
| Cancel or plan swap that voids an unused prebill during a charge | Refused and rolled back while the charge's claim is set, including after a lost result until the reconciler has asked Stripe (rule 2). A dunning cancel refused this way is tried again 24 hours later. | Retry the cancel or swap later. |
| Credit note that leaves nothing due on an unpaid invoice (an operator waives it) | The invoice is marked paid with the note: `invoice.paid` is sent, its invoice with `amount_paid_cents` 0, and its dunning run closes as `payment_recovered`, escalated runs included. The Dunning page's "Recovered" card and the recovery rate leave it out: no money arrived. It can no longer be voided. Revenue does not count the waived amount. A later item removal, quantity cut or downgrade in the same period gives no further credit: the waiver already credited the whole invoice. | Nothing. |
| Credit note while a charge is running, or after a lost result | Refused while the payment is `processing` or `unknown`; allowed while the charge call is in flight and while a lost result waits for the reconciler. A note that leaves nothing due then leaves the invoice open at $0; if that charge fails, the next dunning retry marks it paid. If that charge did succeed, the payment is flagged when it is found (`payment.duplicate_charge` or `payment.amount_mismatch`). | Wait for the charge result before issuing one. |
| Retries used up, and a payment may still go through when dunning's final action is due (`processing`, `unknown`, a charge running, or a lost result) | No cancel, pause or write-off while that payment may still go through; dunning checks again later. The final action holds the charge claim itself, so no charge starts while it cancels. A payment that succeeds closes dunning; one that fails gets the final action. A parked invoice waits for the reconciler: with no payment 24 hours after the attempt, it goes back to `failed` or `pending` and dunning goes on. | Nothing. If you write off a parked invoice whose dunning used every retry, apply the policy's cancel or pause by hand ([runbook](ops/runbook/unclear-charge.md#parked-no-paymentintent)). |
| Stripe failing | Circuit breaker, one for the whole process: after 5 unclear failures in a row, no charges for 30 s, and no reconciler reads by PaymentIntent. The interrupted-charge check skips the breaker on purpose, so its failures never stop charges. A skipped dunning retry is not counted. | Usually nothing: [runbook](ops/runbook/stripe-circuit-breaker-open.md). |
| Database error during a dunning retry | The retry is skipped and does not use up an attempt; a `WARN` line names the invoice and the error. The next pass usually retries it within the hour. If the charge claim was saved but its answer lost, the retry waits until the reconciler clears the claim: up to about 2 hours. If credits covered the invoice and only marking it paid failed, the next pass marks it paid. | Nothing. |
| Refund pending or failed at the bank | Status follows Stripe's refund webhooks; failed never moves back to succeeded. Failed, or pending over 72 hours, shows on the home page. | **Retry refund** on the credit note. It looks the refund up in Stripe first and reuses it. |
| Refund timeout, Stripe 5xx, 409 or 429 (no clear answer) | Refund `unknown`: the customer may already be refunded. It shows on the home page at once. Webhooks do not change it. | Don't refund in the Stripe dashboard. Retry the refund: Velox looks for the refund in Stripe, then resends the same request with the same idempotency key. While Stripe keeps the key (at least 24 hours), it returns the first result instead of making a second refund. |

## Not handled yet

- **The dashboard shows an `unknown` refund as "refund pending"**, with no
  **Retry refund** button, and the "Refunded to Card" card leaves its amount
  out. Retry it with
  `POST /v1/credit-notes/{id}/retry-refund` until the dashboard shows it.
- **A refund still `unknown` after 24 hours is resent**, after a search in
  Stripe finds nothing. So is a refund that never got a result recorded
  (`none`). Stripe may drop the idempotency key by then, so a
  refund Stripe makes later, while recovering from an incident, could be
  repeated
  ([ADR-107](adr/107-unknown-is-terminal-until-a-human.md#amendment-2026-10-18--the-refund-half-is-done)).
- **A parked invoice waits 24 hours before it is charged again, even when
  Stripe never received the request** (a network error before the request
  left). Velox cannot tell that apart from a 5xx that Stripe may still
  complete, so it waits Stripe's 24 hours for both.
- **A payment Stripe creates for a parked charge more than 24 hours after
  the attempt** is recorded only by its webhook. If the invoice was charged
  again by then, that payment is flagged as `payment.duplicate_charge`.
- **A parked invoice whose customer is gone from Stripe, or whose Stripe
  connection is broken, stays parked**: Velox cannot list that customer's
  payments. Write it off ([runbook](ops/runbook/unclear-charge.md#parked-no-paymentintent)).
- **A database restart or failover during a dunning retry can still use up
  the attempt.** If the database is still down when Velox undoes the
  attempt, it stays counted. On the last retry, the policy's final action
  then runs without a final charge.
- **A dunning retry that comes back `processing` closes dunning as
  recovered**, although the invoice is unpaid. If that payment then fails,
  nothing retries the invoice; collect it by hand
  ([#1289](https://github.com/getvelox/velox/issues/1289)).
- **A Velox charge that Stripe left waiting for the customer
  (`requires_action`) never ends** once its result was saved. The invoice
  stays `processing`, and dunning's final action waits on it.
- **Collect payment still charges a card refused for good, or one whose
  bank wants the customer to confirm.** The invoice notice no longer offers
  a retry, but the invoice page's **Collect payment** reaches Stripe with
  that card; for a confirmation, Stripe asks again and the customer gets
  another email
  ([ADR-126](adr/126-a-card-refused-for-good-is-not-retried.md)).
- **A different card the customer pays with on the hosted page does not
  replace a default that needed confirmation.** It pays that invoice; the
  next invoice charges the old default again, and the bank may ask again.
  If they pay with the same card, the copy that payment saved becomes the
  default (Velox keeps one copy of a card), and the next retry or invoice
  charges it.
- **The dashboard has no badge for an open dispute.** An open dispute shows
  only as a row in the invoice's Activity list, and the lost-dispute notice
  carries the generic title "Needs attention". The dashboard's list of Stripe
  events to turn on does not name the three dispute events yet, so a setup
  that follows only that list records no disputes; add them by hand
  ([runbook](ops/runbook/card-disputes.md)). Read disputes with
  `GET /v1/invoices/{id}/disputes`.
- **A dispute that Stripe's dispute prevention settles (status
  `prevented`) shows no notice.** Prevention settles a dispute by
  refunding the payment in Stripe, and Velox does not record a refund made
  in Stripe. The dispute is in the invoice's Activity list; record the
  refund with a credit note
  ([ADR-049](adr/049-payment-settlement-primitive.md), "Deferred").

**Next:** react to these events in your app with
[webhooks](webhooks.md#event-catalog). Operators:
[runbook](ops/runbook/index.md).
