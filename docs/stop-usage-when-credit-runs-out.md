# Stop usage when credit runs out

The short version:

1. Ask Velox for the customer's live credit balance on an interval.
2. Keep the answer in your own cache.
3. Check your cache before you serve a request.

## Velox records usage. It never refuses it.

Velox accepts every usage event you send, including for a customer who has
no credit left. It does not block, throttle or reject usage. The decision to
stop serving a customer is always yours, and it runs in your code.

This is deliberate. A billing system that goes down, or answers slowly,
should not take your product down with it.

## The read

```
GET /v1/credits/balance/{customer_id}/live
Authorization: Bearer <secret key>
```

```json
{
  "customer_id": "vlx_cus_…",
  "balances": [
    {
      "currency": "USD",
      "balance_cents": 4000,
      "unbilled_usage_cents": 1000,
      "estimated_remaining_cents": 3000
    }
  ],
  "computed_at": "2026-10-13T09:30:00Z"
}
```

`balances` has one entry per currency: each currency the customer holds
credit in, and each currency a live subscription will invoice usage in.
Credit pays only invoices in its own currency, so gate on the entry in the
currency the customer is billed in. No entry in that currency means no
credit in it.

| Field | Meaning |
|---|---|
| `currency` | The currency of the entry. |
| `balance_cents` | The credit the customer can spend now in this currency: what is left after every invoice issued so far. A grant that is past its expiry date is not counted. |
| `unbilled_usage_cents` | What the usage of the current period will cost when it is invoiced in this currency. |
| `estimated_remaining_cents` | The first minus the second, never below zero. Gate on this. |
| `computed_at` | When Velox worked the figures out. |

A customer id that is not in your tenant and mode answers `404`.

`estimated_remaining_cents` stops at zero. To see how far past zero a
customer has gone, subtract `unbilled_usage_cents` from `balance_cents`
yourself.

## Poll it and cache it. Do not call it on every request.

Each call adds up the customer's usage for the period, so it slows as usage
grows. On one laptop: about 0.2 s at 100,000 events, 1.7 to 2.5 s at
1,000,000 ([how it was measured](../internal/billing/live_balance_timing_integration_test.go)).
The read gives up after 30 seconds. Measure your largest customers first.

So treat it as a background read:

- Poll once a minute for each customer who has sent usage since your last
  poll.
- Keep polling each customer you are refusing, too. A refused customer sends
  no usage, so a poll that follows usage alone would never look at them
  again.
- Store `estimated_remaining_cents` in your own cache (Redis, memory, your
  database), with no expiry time.
- Before you serve a request, read your cache. Never call Velox in the
  request path.

A stored zero must stay until a later read replaces it. If the entry expires,
or your cache drops it, a customer you were refusing is served again. So
write the entry only after a read that worked, and never let a missing entry
turn a refused customer back on.

Between two polls a customer can spend a little more than they have. With a
one-minute poll that is at most one minute of usage. Poll faster for a
customer who is close to zero if that matters to you.

```python
# Runs on a timer, once a minute.
for customer_id in customers_active_since_last_poll() | customers_refused():
    try:
        live = velox.get(f"/v1/credits/balance/{customer_id}/live", timeout=30)
    except Exception as err:
        alert(customer_id, err)   # keep the last figure, go on to the next customer
        continue
    # The entry in the currency this customer is billed in; none means no credit in it.
    entry = next((b for b in live["balances"] if b["currency"] == billing_currency(customer_id)), None)
    cache.set(f"credit:{customer_id}", entry["estimated_remaining_cents"] if entry else 0)   # no expiry

# Runs in your request path.
def may_serve(customer_id):
    remaining = cache.get(f"credit:{customer_id}")
    if remaining is None:
        return True   # never polled yet. Your choice: serve or refuse
    return remaining > 0
```

`customers_refused()` is every customer whose stored figure is zero.
`billing_currency()` is the currency you invoice the customer in.

## When the read fails

The read answers `500` when Velox cannot work the figure out. It never
returns part of a figure.

- Keep the last figure you stored for that customer. Do not clear it.
- Go on to the next customer. One failed read must not stop the loop.
- Alert yourself. If a customer's pricing is set up wrongly, the read fails
  for that customer every time until the pricing is fixed.

## A top-up shows at once

Velox stores nothing for this read. It works the figure out again on every
call. When you grant credit, the next read already includes it, so poll that
customer again straight after a top-up and update your cache.

## What the figure includes

- Every credit grant, adjustment and expiry.
- Every invoice that has already drawn on the credit.
- The metered usage of the current period that no invoice has billed yet,
  priced with the same rules the invoice will use.
- Each subscription counted over its own billing period, when a customer has
  more than one.

**A grant that has passed its expiry date is not counted**, even before the
expiry run writes the expiry to the ledger. That run waits while a close or
a period invoice that ends before the expiry can still draw on the grant.
Until that close or invoice is settled, `balance_cents` here is lower than
the balance `GET /v1/credits/balance/{customer_id}` returns.

## What the figure leaves out

The figure is an estimate. It can read higher than what the customer really
has left, because these are not subtracted:

- **Tax.** The invoice adds tax; the estimate does not.
- **A base fee billed at the end of the period.** It appears only when the
  period closes and the invoice is issued.
- **Unpaid invoices.** An invoice that is issued but not yet settled can
  still draw on the credit.
- **Usage sent after a period has ended and before Velox has closed it.**
  Velox closes a period on the first billing run after
  `INVOICE_GRACE_PERIOD` has passed, so this gap lasts 1 to 2 hours by
  default ([ADR-120](adr/120-invoice-grace-period-for-late-usage.md)).
- **Credit that expires before the period ends.** It counts as spendable
  until its expiry date. If the period's invoice is issued after that date,
  the invoice cannot draw on it.

It can read lower than the truth in one case:

- **A period has ended and its invoice is not built yet.** This lasts
  through `INVOICE_GRACE_PERIOD`, or longer while a failed close waits for
  its next attempt. The estimate subtracts that period's usage but no
  longer counts credit that expired after the period ended. That period's
  invoice still draws on that credit
  ([ADR-071](adr/071-credit-expiry-retires-the-block.md)).

Usage sent for a customer who has no active subscription is recorded but not
priced, so it does not lower the figure.

If these gaps matter for your product, stop serving a little above zero
rather than at zero.

<details>
<summary><strong>Edge cases</strong></summary>

Three cases are handled so that nothing is counted twice or wrongly:

- **A billing threshold invoiced part of the period early.** That usage has
  been paid for from credit already, so only the usage since that invoice is
  counted.
- **A subscription is in a trial.** Usage sent before the first paid period
  starts is not billed, so it is not counted. The first paid period starts at
  the beginning of the day the trial ends. Usage sent on that last day is
  billed, so it is counted, even while the subscription still says
  `trialing`. A trial that is set to cancel when it ends is not billed at
  all, and none of its usage is counted.
- **The plan changed in the middle of the period.** Each meter is counted for
  the time a plan that carries it was on the subscription, as the invoice
  bills it. Usage on the old plan's meter before the change still counts.

</details>

## Do not use the credit webhooks for this

The `credit.balance_*` webhooks follow the settled balance, which moves when
an invoice, grant, adjustment or expiry is written, not when usage arrives.
So they can fire weeks after the credit ran out. Use them for top-up
reminders, and the live read to decide whether to serve
([why](webhooks.md#the-credit-events-follow-the-settled-balance)).

## The same figure on the public cost page

The public cost dashboard, `GET /v1/public/cost-dashboard/{token}`, returns
the same three numbers for each currency in its `credits` array. Your
customer sees the figure your backend gates on.

**Next:** remind customers to top up with the [credit webhooks](webhooks.md#credit-datacredit_balance).
