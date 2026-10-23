# ADR-131: Webhook events carry the resource they are about

**Date:** 2026-10-24
**Status:** Accepted

## Summary

An outbound webhook event's `data` holds the resource the event is about,
under the resource's name, in the JSON shape the API returns for it. Every
event of a family carries the same resource. Facts the resource does not
hold sit beside it as named keys, listed per event. Before this, each emit
site built its own field list: the five `invoice.*` events carried five
different key sets.

## Context

The simplification audit before the first tag (B6) found that the invoice
events disagreed on their fields, and other families were no better:

- `invoice.finalized` and `invoice.voided` sent `status` and
  `total_amount_cents`, `invoice.marked_uncollectible` did not, and only
  `invoice.finalized` sent `credits_applied_cents`. `invoice.paid` sent
  `subscription_id` and `paid_at` that the others left out.
  `invoice.payment_recorded` called the paid amount `amount_cents`.
- `payment.succeeded` and `payment.failed` sent `amount_cents` as the
  invoice total, not the amount charged.
- `subscription.collection_resumed` sent four keys from the schedule and
  six from the operator path. Item events sent a hand-picked `item` object
  in some events and only `item_id` in another.
- No event carried the resource, so a receiver read the API after every
  event to learn anything beyond a few ids and amounts.

Payloads are a public contract. Changing them after the tag breaks
receivers, so the rule is set now.

How the peers shape a payload (verified 2026-10-24):

- **Stripe:** "`data.object`: Object containing the API resource relevant to
  the event. For example, an `invoice.created` event will have a full
  invoice object as the value of the object key"
  ([Event object](https://docs.stripe.com/api/events/object)).
- **Lago:** `Webhooks::BaseService#call` builds
  `{webhook_type, object_type, organization_id, <object_type> => serializer}`,
  and each invoice service serializes the invoice with
  `V1::InvoiceSerializer` (`app/services/webhooks/base_service.rb`,
  `app/services/webhooks/invoices/*.rb`). An event that needs more adds a
  key beside the resource: `payment_dispute_lost` sends
  `{invoice:, provider_error:}`
  (`app/serializers/v1/invoices/payment_dispute_lost_serializer.rb`).

## Decision

1. `data` is `{<resource>: <resource as the API returns it>, <extras>}`.
   The resource key per family is fixed in `internal/domain/webhook_payload.go`:

   | Family | Key | Shape |
   |---|---|---|
   | `invoice.*` | `invoice` | `GET /v1/invoices/{id}` → `invoice` |
   | `payment.*` | `invoice` | the invoice the payment was for |
   | `payment_method.*` | `payment_method` | a row of `GET /v1/customers/{id}/payment-methods` |
   | `subscription.*` | `subscription` | `GET /v1/subscriptions/{id}` |
   | `customer.*` | `customer` | `GET /v1/customers/{id}` |
   | `dunning.*` | `dunning_run` | `GET /v1/dunning/runs/{id}` → `run` |
   | `credit.*` | `credit_balance` | a row of `GET /v1/credits/balance/{customer_id}`, in the event's currency |

2. The resource is the row as the change left it. In-transaction events
   read it in that transaction; post-commit events read it after the
   commit, or use the row the write returned. Fields the API works out per
   read for the dashboard are left out: an invoice's `attention`,
   `recovery_warning`, `billing_period_display` and `test_clock_id`; a
   subscription's `current_billing_period_display`; a run's list-only
   joined figures. Invoice line items are not included.
3. `WebhookEventExtras` lists the keys each event may carry beside the
   resource. A key the resource already holds is not repeated. A fact it
   does not hold is kept, so no amount is lost: for example the anomaly
   charge (`incoming_payment_intent_id`, `captured_cents`), the dispute,
   `recovered_from`, `old_plan_id`, a threshold crossing's figures, and the
   commit-retire figures.
4. One builder per family (`domain.InvoiceWebhookData` and siblings) makes
   every payload. The envelope (`id`, `event_type`, `created_at`, `data`),
   the outbox, signing, retries and replay do not change.
5. When a post-commit event cannot read its resource, it is not sent, and
   an error is logged. An event with a made-up resource would tell the
   receiver something the API does not show.

## Why this design

- **The receiver gets the state it needs from the event.** Most receivers
  act on the resource, and the API's own shape means one parser for both.
- **One shape per family.** A receiver can handle `invoice.*` with one code
  path.
- **It reuses what exists.** The envelope stays, and the `invoice` key is
  the one `GET /v1/invoices/{id}` already wraps the invoice in.
- **The code enforces it.** The goldens, the catalog lock test and the
  real-Postgres checks fail when an emitter, the docs or the rule drift.

## Alternatives considered

- **A. Stripe's generic `data.object`:** one key name for every family. It
  hides the type behind `event_type` and has no obvious place for a second
  resource, such as the dispute beside its invoice. Velox resources carry
  no `object` field to say what they are. Rejected for the named key.
- **B. Keep the flat payloads and only make each family's keys agree:**
  smaller payloads, but receivers would still call the API for the rest,
  and a hand-kept key list drifts again, as it did. Rejected.
- **C. A payment object for `payment.*`:** Velox has none in its API, and
  inventing one for webhooks would make a second payment model. The invoice
  already holds the charge, its error and how it was paid.

## Consequences

### Positive

- A receiver no longer needs an API call to learn an invoice's amounts,
  a subscription's items or a dunning run's state.
- New fields on a resource reach its events with no extra work.

### Risks / open items

- **Breaking for every receiver.** Each event's fields moved. The CHANGELOG
  lists old and new keys per event.
- **Payloads are larger,** mostly the invoice. Line items are left out to
  keep them bounded.
- **A post-commit event can show a later state** than the change that sent
  it, when another write lands between the commit and the read. It never
  shows a state the row did not have.
- **The resource follows the API.** Hiding a field from the API (the
  rename pass plans to hide some invoice internals) also removes it from
  the events.

## References

- [docs/webhooks.md](../webhooks.md): the payload of each family and event.
- `internal/domain/webhook_payload.go`, `internal/domain/testdata/webhook_payloads/`.
- ADR-040 (transactional outbox), ADR-068 (payment anomalies), ADR-078
  (credit balance events), ADR-129 (credit per currency).
