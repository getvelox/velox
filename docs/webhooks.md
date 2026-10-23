# Webhooks

A webhook is a message Velox sends to a URL in your app when something
happens, such as an invoice paid, a payment failed or a credit balance
running low. Your app can then react: send a receipt, pause a service,
remind a customer to top up.

## Receive your first event

You run a small test receiver, register it with Velox, make something
happen, and check the message.

**Before you start:** Velox is running, and you have a test API key (see
[API keys](api-keys.md)). Open a terminal in the `velox` folder and set:

```bash
export VELOX=http://localhost:8080
export VELOX_KEY=vlx_secret_test_…   # your test key
```

1. **Start the test receiver.** It answers every message with `200` and
   writes each one to a file.

   ```bash
   python3 scripts/dev/webhook-sink.py /tmp/velox-webhooks.jsonl
   ```

   **You see:** `webhook sink on 127.0.0.1:9099 -> /tmp/velox-webhooks.jsonl (paths under /fail return 500)`

2. **Pick the address Velox will call.** It depends on how you run Velox.

   - **From source** (`make dev`): use `http://localhost:9099/ok`.
   - **In Docker** (the [quick start](quick-start.md) or a server): inside
     its container, `localhost` is the container itself, and Velox refuses
     private network addresses, so it cannot reach the receiver directly.
     In a second terminal, start a tunnel, which gives your computer a
     public `https://` address. For example:
     `cloudflared tunnel --url http://localhost:9099`. Use the `https://`
     address it prints, plus `/ok`.

   ```bash
   export HOOK_URL=http://localhost:9099/ok   # or https://<tunnel address>/ok
   ```

   **You see:** nothing yet. The address is saved in `HOOK_URL`.

3. **Register the endpoint.** `"*"` asks for every event.

   ```bash
   jq -n --arg url "$HOOK_URL" '{url: $url, events: ["*"]}' |
     curl -s -X POST "$VELOX/v1/webhook-endpoints/endpoints" \
       -H "Authorization: Bearer $VELOX_KEY" \
       -H "Content-Type: application/json" \
       -d @-
   ```

   **You see:** JSON with `endpoint.id` (`vlx_whe_…`) and `secret`
   (`whsec_…`). The secret is shown only this once. Save it:
   `export WHSEC=whsec_…`.

4. **Make something happen.** Run the demo again. It creates a
   subscription and a month of usage, then bills it.
   - Docker quick start: `cd deploy/compose && ./quickstart.sh demo`
   - From source: `./scripts/demo.sh $VELOX_KEY`

   Then look at the file:

   ```bash
   tail -n 3 /tmp/velox-webhooks.jsonl
   ```

   **You see:** one JSON line per message, each with `"path": "/ok"`, a
   `"sig"` such as `t=…,v1=…`, and a `"body"` whose `event_type` is, for
   example, `subscription.created` or `invoice.finalized`.

5. **Check the signature.** This proves the message came from Velox.

   ```bash
   head -n 1 /tmp/velox-webhooks.jsonl | python3 -c '
   import hashlib, hmac, json, os, sys
   r = json.loads(sys.stdin.readline())
   p = dict(x.split("=", 1) for x in r["sig"].split(","))
   mac = hmac.new(os.environ["WHSEC"].encode(), (p["t"] + "." + r["body"]).encode(), hashlib.sha256)
   print(mac.hexdigest() == p["v1"])'
   ```

   **You see:** `True`

The dashboard shows the same messages: open **Webhooks**, then your
endpoint, to see each delivery and its result.

To test failures, register an address whose path starts with `/fail`, such
as `http://localhost:9099/fail`. The receiver answers `500`, so Velox
retries on the schedule below.

## Three rules for your receiver

1. **The same event can arrive more than once.** Once you have handled an
   event, store its `id`, and skip any event whose `id` you have stored.
   Store it after handling, not on arrival: otherwise a failure in between
   makes you skip the retry and any replay, and the event is never handled.
2. **Events can arrive out of order.** Do not work out the current state
   from the order they arrive. Read the event's data, or fetch the
   invoice or subscription from the API.
3. **Events marked *(always sent)* are never lost; others can be.** Those
   events in the [catalog](#event-catalog) are saved together with the
   change that caused them. If the change is saved, the event will be sent,
   even after a crash. Other events, including `invoice.finalized`, are
   queued just after the change is saved, and a crash at that moment can
   drop one.

## What a message looks like

Every message is an HTTP `POST` to your URL with
`Content-Type: application/json` and this body:

```json
{
  "id": "vlx_whevt_…",
  "event_type": "invoice.paid",
  "created_at": "2026-07-05T14:03:22Z",
  "data": {
    "invoice": { "id": "vlx_inv_…", "customer_id": "vlx_cus_…", "status": "paid", "amount_paid_cents": 4900, "…": "…" }
  }
}
```

- `id` is the event's ID and `created_at` is when Velox created the event.
  Both stay the same on every retry and replay, so use `id` to skip repeats
  (rule 1).
- `data` holds the resource the event is about, under the resource's name:
  `data.invoice`, `data.subscription` and so on. It is the same object the
  API returns for that resource, as it was when Velox created the event.
  Every event of a family carries the same resource. To act on the newest
  state, read the resource from the API.
- A few events also carry facts the resource does not hold, such as who
  canceled a subscription. They sit beside the resource in `data`. The
  [catalog](#event-catalog) lists them for each event.

Headers:

| Header | Value |
|---|---|
| `Velox-Signature` | `t=<unix-seconds>,v1=<hex hmac>` |
| `Velox-Event-Type` | The event type, so you can route the message before you read the body |

## Verifying signatures

The signature is `HMAC-SHA256(secret, "<t>.<raw body>")`, hex-encoded.

- The key is the full secret string, `whsec_` prefix included, used as
  UTF-8 bytes. Do not strip the prefix or decode it.
- Check against the raw request bytes. Do not parse and re-serialize the
  JSON first.
- While a secret is being rotated, the header has two `v1=` entries, one
  for the old secret and one for the new. Accept the message if any `v1=`
  matches.

Node:

```js
const crypto = require("crypto");

function verifyVeloxSignature(rawBody, header, secret, toleranceSec = 300) {
  const t = header.match(/(?:^|,)t=(\d+)/)?.[1];
  // Collect every v1= entry: there are two during a secret rotation.
  const sigs = [...header.matchAll(/v1=([0-9a-f]+)/g)].map(m => m[1]);
  if (!t || sigs.length === 0) return false;
  if (Math.abs(Date.now() / 1000 - Number(t)) > toleranceSec) return false;
  const expected = crypto
    .createHmac("sha256", secret)
    .update(`${t}.${rawBody}`)
    .digest("hex");
  return sigs.some(s =>
    s.length === expected.length &&
    crypto.timingSafeEqual(Buffer.from(s), Buffer.from(expected)),
  );
}
```

Python:

```python
import hashlib, hmac, re, time

def verify_velox_signature(raw_body: bytes, header: str, secret: str, tolerance: int = 300) -> bool:
    m = re.search(r"(?:^|,)t=(\d+)", header)
    sigs = re.findall(r"v1=([0-9a-f]+)", header)
    if not m or not sigs:
        return False
    t = int(m.group(1))
    if abs(time.time() - t) > tolerance:
        return False
    expected = hmac.new(secret.encode(), f"{t}.".encode() + raw_body, hashlib.sha256).hexdigest()
    return any(hmac.compare_digest(s, expected) for s in sigs)
```

### Rotating the secret

The secret (`whsec_…`) is shown once when you create the endpoint, and once
when you rotate it.

1. Call `POST /v1/webhook-endpoints/endpoints/{id}/rotate-secret`.
   **You see:** the new secret. For the next 72 hours, Velox signs each
   message with both the old and the new secret.
2. Update your receiver to use the new secret within those 72 hours.
   **You see:** messages keep passing your check.

Changing an endpoint with `PATCH` (its URL, events, or whether it is
active) never changes the secret.

## Managing endpoints

Manage endpoints in the dashboard under **Webhooks**, or with the API (see
the [API reference](api-surface.md#the-full-reference)):

| Method | Path | What it does |
|---|---|---|
| `POST` | `/v1/webhook-endpoints/endpoints` | Register an endpoint |
| `GET` | `/v1/webhook-endpoints/endpoints` | List endpoints |
| `PATCH` | `/v1/webhook-endpoints/endpoints/{id}` | Change the URL, events or active flag |
| `DELETE` | `/v1/webhook-endpoints/endpoints/{id}` | Remove an endpoint |

- `events` takes event names from the [catalog](#event-catalog). `"*"`
  means every event, and a prefix such as `"invoice.*"` means every event
  that starts with it.
- An event name Velox never sends is refused with `422`, so a typo cannot
  leave you waiting for messages that never come.
- The URL must start with `https://` and must not point to a private
  network address, IPv4 or IPv6, such as `10.0.0.1`, `169.254.169.254`,
  `fd00::1` or `fe80::1`. Velox checks the address when you save the URL
  and again each time it sends.
- On a server (`APP_ENV=production`), `localhost` is refused too. In local
  development, `localhost` is allowed, over `http://` as well.

## Retries

A message counts as delivered when your endpoint answers with any `2xx`
status within 10 seconds. Anything else is retried on this schedule:

| Attempt | Wait after the previous failure |
|---|---|
| 1 | none |
| 2 | 1 minute |
| 3 | 5 minutes |
| 4 | 30 minutes |
| 5 | 2 hours |
| 6 | 24 hours |

That is six attempts over about 26.5 hours. After the last one, the
delivery is marked failed.

Velox does not follow redirects, so a `3xx` answer is retried like any
other failure: register the URL that answers with the `2xx` itself.

**If your endpoint was down for longer:** failed deliveries stay visible in
the dashboard, with each endpoint's success rate and each event's delivery
history. Send any event again from there, or with
`POST /v1/webhook_events/{id}/replay`. `GET /v1/webhook-endpoints/events`
lists the newest 50 events; it has no filters.

## Event catalog

Each family of events carries one resource in `data`, named in the family's
heading below. The **Also in `data`** column lists the other keys an event
carries, and `—` means none. A key with nothing to say is left out, such as
`reason` on most cancels. A full example of each event's `data` is in
[internal/domain/testdata/webhook_payloads](../internal/domain/testdata/webhook_payloads/).

*(always sent)*: see [rule 3](#three-rules-for-your-receiver). Terms:
[glossary](glossary.md).

### Invoices: `data.invoice`

`data.invoice` is the invoice as `GET /v1/invoices/{id}` returns it under
`invoice`. Four fields the API works out when you read it are left out:
`attention`, `recovery_warning`, `billing_period_display` and
`test_clock_id`. Line items are not included; `GET /v1/invoices/{id}`
returns them.

| Event | Sent when | Also in `data` |
|---|---|---|
| `invoice.finalized` | An invoice is final and ready to pay. Sent once per invoice, including the invoice for a mid-period upgrade, seat increase or added item. A draft sends it when it is finalized. Its `amount_due_cents` is before credit ([more](#invoicefinalized-shows-the-amount-before-credit)) | — |
| `invoice.paid` | An invoice is fully paid, by card, credit or an offline payment, or a credit note left nothing due. Its `amount_paid_cents` is what was paid at that moment, 0 when credit or a credit note covered the rest *(always sent)* | — |
| `invoice.payment_recorded` | Someone recorded a payment made outside Velox, such as a bank transfer or cheque. `recovered_from` is the status it was paid from: `finalized` or `uncollectible` | `recovered_from` |
| `invoice.marked_uncollectible` | An invoice was written off as bad debt | — |
| `invoice.voided` | An invoice was voided. Under Stripe Tax, Velox reverses the invoice's tax after it sends this event; `GET /v1/invoices/{id}` shows `tax_reversed_at` once that is done. | — |

### `invoice.finalized` shows the amount before credit

Velox uses the customer's credit when it collects the invoice. That is
after it sends `invoice.finalized`, usually within seconds. Then it charges
the card for what is left. So in this event's invoice:

- `amount_due_cents` is the amount before credit.
- `credits_applied_cents` is 0: credit is used after this event.

Credit is used again each time Velox tries to collect an unpaid invoice,
such as a payment retry. So credit the customer gets later can pay an older
invoice.

No invoice event is sent when credit pays only part of an invoice. When
credit pays all of it, `invoice.paid` is sent. To know what is still owed,
read the invoice with `GET /v1/invoices/{id}`: its `amount_due_cents` and
`credits_applied_cents` are current.

### Payments: `data.invoice`

`data.invoice` is the invoice the payment was for, in the same shape as for
the invoice events. Velox has no separate payment object. The invoice holds
the charge's Stripe PaymentIntent (`stripe_payment_intent_id`), its error
(`last_payment_error`) and whether it was paid outside Velox
(`paid_out_of_band`).

| Event | Sent when | Also in `data` |
|---|---|---|
| `payment.succeeded` | A charge was collected. The invoice names its PaymentIntent. *(always sent)* | — |
| `payment.failed` | A charge the invoice was waiting on failed. Sent once per charge attempt (PaymentIntent). A decline on the hosted payment page does not send it, because the customer saw it there and can try again. *(always sent)* | — |
| `payment.duplicate_charge` | A second charge succeeded on an invoice that was already paid. Refund it. | `incoming_payment_intent_id`, `captured_cents` |
| `payment.amount_mismatch` | A charge settled for a different amount than the invoice owed | `incoming_payment_intent_id`, `captured_cents` |
| `payment.currency_mismatch` | A charge settled in a different currency than the invoice's. The payment is recorded as usual and nothing is converted. Review it in Stripe. | `incoming_payment_intent_id`, `captured_cents` |
| `payment.received_on_voided_invoice` | Money arrived for a voided invoice. Refund it. | `incoming_payment_intent_id`, `captured_cents` |
| `payment.dispute_opened` | A customer disputed a card payment with their bank (a chargeback), and Velox recorded it against the invoice. Sent once per dispute. *(always sent)* | `dispute` |
| `payment.dispute_closed` | A recorded dispute closed. Sent again if Stripe turns a loss into a win. *(always sent)* | `dispute` |

For the four anomaly events, `incoming_payment_intent_id` is the charge that
raised the event and `captured_cents` is what it took. Match the charge by
`incoming_payment_intent_id`, not by the invoice's `stripe_payment_intent_id`:

- `payment.duplicate_charge`: the invoice's `stripe_payment_intent_id` is
  the other charge, the one Velox recorded on the paid invoice. It is left
  out when Velox recorded none, such as when credit paid the invoice or it
  was paid outside Velox (`paid_out_of_band` is true).
- `payment.amount_mismatch`: the invoice was just paid by this charge, so
  its `stripe_payment_intent_id` is this charge and `amount_paid_cents` is
  what Velox booked.
- `payment.currency_mismatch` and `payment.received_on_voided_invoice`: the
  invoice is as it was when the charge arrived. Its
  `stripe_payment_intent_id` may be this charge, an earlier one, or none.

`dispute` is the dispute as `GET /v1/invoices/{id}/disputes` lists it, with
`stripe_dispute_id`, `status`, `reason`, `amount_cents`, `currency`,
`evidence_due_by` (the deadline to respond in Stripe), `opened_at`, and
`closed_at` once it is closed. On `payment.dispute_closed`, `status` is
`won`, `lost`, `warning_closed` (an inquiry that never became a chargeback)
or `prevented`. When the first Stripe event Velox receives is already the
close, both events are sent with the closed status, in either order. What a
dispute does and how to turn on Stripe's dispute events:
[Card disputes](ops/runbook/card-disputes.md).

### Payment methods: `data.payment_method`

`data.payment_method` is the saved card as
`GET /v1/customers/{id}/payment-methods` lists it: its brand, last 4 digits,
expiry and whether it is the default. A Stripe event that arrives after the
card was removed in Velox leaves it removed and sends neither event.

| Event | Sent when | Also in `data` |
|---|---|---|
| `payment_method.attached` | A customer saved a payment method. Invoices waiting for one are charged on the next run. `stripe_payment_method_id` is the card's Stripe ID | `stripe_payment_method_id` |
| `payment_method.updated` | A customer's payment method was set up or replaced through Stripe Checkout. `stripe_customer_id` is the Stripe customer it was saved to | `stripe_customer_id` |

### Subscriptions: `data.subscription`

`data.subscription` is the subscription with its items, as
`GET /v1/subscriptions/{id}` returns it, without
`current_billing_period_display` (a date range the API writes out when you
read it). A cancel's time is the subscription's `canceled_at`, and a
schedule's is its `cancel_at` and `cancel_effective_at`.

| Event | Sent when | Also in `data` |
|---|---|---|
| `subscription.created` | A subscription was created *(always sent)* | — |
| `subscription.activated` | A draft subscription was activated *(always sent)* | — |
| `subscription.canceled` | A subscription was canceled. `canceled_by` is `operator`, `customer`, `schedule`, `dunning` or `system`. A cancel at trial end carries `reason: trial_end_cancel`. *(always sent)* | `canceled_by`, `reason` |
| `subscription.trial_ended` | A trial ended. `triggered_by` is `operator` or `schedule`, and `ended_at` is when it ended. *(always sent)* | `ended_at`, `triggered_by` |
| `subscription.trial_extended` | A trial's end date moved later. The new end is the subscription's `trial_end_at`. `triggered_by` is `operator` | `triggered_by` |
| `subscription.cancel_scheduled` / `.cancel_cleared` | A cancel at period end was scheduled, or the schedule was removed | — |
| `subscription.collection_paused` | Collection was paused: invoices stay drafts. The subscription's `pause_collection` says how and until when | — |
| `subscription.collection_resumed` | Collection resumed. `triggered_by` is `operator`, `schedule` (its resume date came) or `payment` (a payment ended a pause that dunning set). A resume by date carries `resumed_at` | `triggered_by`, `resumed_at` |
| `subscription.item.added` / `.updated` / `.removed` | A plan or quantity on the subscription was added, changed or removed. `item_id` names the item; a removed item is no longer in `items` | `item_id` |
| `subscription.pending_change.scheduled` / `.canceled` | A plan change for the next billing period was scheduled or canceled. The item's `pending_plan_id` and `pending_plan_effective_at` show it | `item_id` |
| `subscription.pending_change.applied` | A scheduled plan change was applied at the period end. The item's `plan_id` is the new plan, `old_plan_id` the one it left, and `applied_at` when. The subscription is as it was before the close that starts the next period | `item_id`, `old_plan_id`, `applied_at` |
| `subscription.threshold_crossed` | Usage crossed a billing threshold, so an invoice was made early ([fields](#what-subscriptionthreshold_crossed-carries)) | `invoice_id`, `running_subtotal`, `crossed_amount`, `crossed_item_id` |

#### What `subscription.threshold_crossed` carries

- `invoice_id`: the invoice made early.
- `running_subtotal`: the spend compared with the threshold, in cents.
- `crossed_amount`: whether the amount threshold was crossed.
- `crossed_item_id`: the item whose usage threshold was crossed, empty if none.

### Customers: `data.customer`

`data.customer` is the customer as `GET /v1/customers/{id}` returns it.

| Event | Sent when | Also in `data` |
|---|---|---|
| `customer.email_bounced` | An email to a customer bounced, and Velox stopped emailing that address. `reason` is the email provider's | `reason` |
| `customer.email_complained` | A customer marked an email from Velox as spam, and Velox stopped emailing that address. `reason` is the email provider's | `reason` |

### Dunning: `data.dunning_run`

Dunning is Velox retrying a failed payment on a schedule. `data.dunning_run`
is the run as `GET /v1/dunning/runs/{id}` returns it under `run`: its
invoice, state, attempts and how it was resolved.

| Event | Sent when | Also in `data` |
|---|---|---|
| `dunning.started` | Payment retries started for an invoice *(always sent)* | — |
| `dunning.escalated` | Payment retries ran out, and the final action from your policy was taken. `final_subscription_action` and `final_invoice_action` are your policy's; `applied` is what was done to this invoice, such as `subscription_canceled+invoice_marked_uncollectible`, empty when nothing applied | `final_subscription_action`, `final_invoice_action`, `applied` |
| `dunning.resolved` | Payment retries ended early, for example because the invoice was paid or someone resolved it by hand. The run's `resolution` says why | — |

### Credit: `data.credit_balance`

`data.credit_balance` is the customer's credit balance in one currency, as
`GET /v1/credits/balance/{customer_id}` lists it, after the change that sent
the event.

| Event | Sent when | Also in `data` |
|---|---|---|
| `credit.balance_low` | The settled credit balance in one currency fell below your account's threshold, `threshold_cents` *(always sent)* | `threshold_cents` |
| `credit.balance_depleted` | The settled credit balance in one currency reached zero or less *(always sent)* | — |
| `credit.balance_recovered` | The settled credit balance in one currency went above zero again *(always sent)* | — |
| `credit.commit_retired` | A credit note took back prepaid credit ([fields](#what-creditcommit_retired-carries)) *(always sent)* | `grant_id`, `funding_invoice_id`, `credit_note_id`, `retired_cents`, `refunded_gross_cents`, `remaining_after_cents` |

#### What `credit.commit_retired` carries

- `grant_id`: the prepaid credit grant.
- `funding_invoice_id`: the invoice that bought it.
- `credit_note_id`: the credit note that took the credit back.
- `retired_cents`: the credit taken back.
- `refunded_gross_cents`: the money the credit note returns.
- `remaining_after_cents`: what is left on the grant.

### The credit events follow the settled balance

`credit.balance_low`, `credit.balance_depleted` and
`credit.balance_recovered` follow the settled balance: the credit a customer
holds after every invoice issued so far. They fire when an invoice, a grant,
an adjustment or an expiry moves that balance. They do not follow usage.

Each currency has its own balance, and each event is about one of them: a
customer whose EUR credit runs out gets `credit.balance_depleted` with
`currency` `EUR` even while it holds USD credit. Credit pays only invoices in
its own currency. The low threshold is one number applied in each currency's
own units: 1000 means $10.00 for USD credit and €10.00 for EUR credit.

Usage draws on credit only when it is invoiced. A customer billed at the end
of the period can use up all their credit in the first week, and
`credit.balance_depleted` fires weeks later, when the invoice is issued.

So use these events to remind a customer to top up, and to keep your own
records in step. Do not use them to decide whether to keep serving a
customer. For that, poll `GET /v1/credits/balance/{customer_id}/live`, which
subtracts the usage not yet invoiced. See
[Stop usage when credit runs out](stop-usage-when-credit-runs-out.md).

A typical receiver for prepaid customers subscribes to `credit.balance_low`
(remind the customer to top up), `credit.balance_recovered` (a top-up
landed), and `subscription.canceled` (turn off their service).

**Next:** gate usage on the live credit balance: [Stop usage when credit runs out](stop-usage-when-credit-runs-out.md).
