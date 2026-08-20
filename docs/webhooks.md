# Consuming Velox webhooks

Velox is a usage-based billing engine; a webhook is how it tells your
service that billing state changed — an invoice paid, a subscription
canceled, a credit balance depleted. This page is for the engineer
building or operating a receiving endpoint — no prior Velox knowledge
assumed — and covers everything a receiver needs: the delivery envelope, signature
verification (with copy-paste verifiers), the retry ladder, the delivery
contract, endpoint management, and the full event catalog.

Endpoints are managed at `POST/GET/PATCH/DELETE /v1/webhook-endpoints/endpoints`
(see [`api/openapi.yaml`](../api/openapi.yaml)) or in the dashboard under
**Webhooks**. Event names are validated at create/update against the
catalog below — subscribing to a name Velox never emits is a 422, not
silence. `*` subscribes to everything; `invoice.*`-style prefix wildcards
work too.

## Delivery envelope

Every delivery is an HTTP `POST` to your endpoint URL with
`Content-Type: application/json` and this body:

```json
{
  "id": "vlx_whevt_…",
  "event_type": "invoice.paid",
  "created_at": "2026-07-05T14:03:22Z",
  "data": { "invoice_id": "vlx_inv_…", "customer_id": "vlx_cus_…", "…": "…" }
}
```

- `id` is the **event** id — stable across retries and replays. Use it as
  your idempotency key: you WILL see the same event more than once
  (at-least-once delivery).
- `data` is the event-specific payload (see the catalog below for the
  fields each event carries).

Headers:

| Header | Value |
|---|---|
| `Velox-Signature` | `t=<unix-seconds>,v1=<hex hmac>` (two `v1=` entries during a secret rotation) |
| `Velox-Event-Type` | the event type, duplicated for cheap routing before body parse |

## Verifying signatures

The signature is `HMAC-SHA256(secret, "<t>.<raw body>")`, hex-encoded.
Verify against the **raw request bytes** — do not re-serialize the JSON.
During the 72-hour rotation grace window the header carries **two** `v1=`
entries (old + new secret, the Stripe multi-signature convention): accept
the delivery if **any** `v1=` matches.

Node:

```js
const crypto = require("crypto");

function verifyVeloxSignature(rawBody, header, secret, toleranceSec = 300) {
  const parts = Object.fromEntries(
    header.split(",").map(kv => kv.split("=")),
  ); // NOTE: naive parse — with two v1= entries, collect them all instead:
  const t = header.match(/(?:^|,)t=(\d+)/)?.[1];
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

The signing secret (`whsec_…`) is shown once at endpoint creation and on
rotation. `POST /v1/webhook-endpoints/endpoints/{id}/rotate-secret` starts
the 72h dual-signing window so you can stage the new verifier without an
outage. `PATCH` on the endpoint (URL, events, active) never rotates the
secret.

## Retries and the success criterion

A delivery succeeds on any **2xx** response (10-second client timeout).
Anything else retries on this ladder:

| Attempt | Delay after previous failure |
|---|---|
| 1 | immediate |
| 2 | 1 minute |
| 3 | 5 minutes |
| 4 | 30 minutes |
| 5 | 2 hours |
| 6 | 24 hours |

Six total attempts over ~26.5 hours, then the delivery is marked failed.
Failed deliveries stay visible in the dashboard (per-endpoint success
rate, per-event delivery timeline) and any event can be **replayed**
manually (`POST /v1/webhook-endpoints/events/{id}/replay`).

## Delivery contract

- **Enqueue is transactional for the events that matter most.** The money
  events (`invoice.paid`, `payment.succeeded`, `payment.failed`), the
  subscription lifecycle events (`subscription.created` / `.activated` /
  `.canceled` / `.trial_ended`), and the credit balance events
  (`credit.balance_low` / `_depleted` / `_recovered`) are written to the
  outbox (the pending-deliveries table a background sender drains)
  **inside the same database transaction as the state change** —
  if the business operation commits, the event exists; a crash cannot
  drop it. Remaining notification events enqueue immediately after their
  transaction commits (best-effort; a crash in that window can drop one).
- **Delivery is at-least-once.** Dedupe on the envelope `id`.
- **Ordering is not guaranteed** across events. Don't infer state from
  arrival order; read the payload (or re-fetch the resource).
- **Recovery from a receiver outage:** the retry ladder covers ~26.5h per
  event on its own. Beyond that, the dashboard's event log supports
  per-event replay. (A filterable programmatic events API for bulk
  reconciliation is on the roadmap; today the list endpoint returns the
  newest 50.)

## Event catalog

The canonical list lives in code at
`internal/domain/webhook_outbound.go` (`KnownWebhookEventTypes`) and is
what endpoint validation enforces. Two Velox terms used below:
**dunning** is the automated follow-up on a failed payment (escalating
reminders and retries until the invoice settles or is written off);
a **tenant** is one business account in Velox — the vendor doing the
billing, as distinct from its customers. Summary:

| Event | Fires when |
|---|---|
| `invoice.finalized` | Invoice finalized and ready for payment |
| `invoice.paid` | Invoice fully paid — card, credits, or offline (transactional) |
| `invoice.payment_recorded` | Operator recorded an out-of-band payment (cheque/wire) |
| `invoice.marked_uncollectible` | Invoice written off as bad debt |
| `invoice.voided` | Invoice voided |
| `payment.succeeded` | Charge collected (carries the PaymentIntent id; transactional) |
| `payment.failed` | Charge attempt failed (transactional) |
| `payment_method.attached` | A payment method landed on a customer (Checkout setup completed) — queued invoices charge on the next sweep |
| `payment_method.updated` | A customer's payment-method setup completed/refreshed (carries card brand + last4) |
| `payment.duplicate_charge` | A second charge succeeded on an already-paid invoice — refund needed |
| `payment.amount_mismatch` | A charge settled for a different amount than was owed at settle |
| `payment.received_on_voided_invoice` | Money landed on a voided invoice — refund owed |
| `subscription.created` | Subscription created (transactional) |
| `subscription.activated` | Draft activated (transactional) |
| `subscription.canceled` | Canceled — payload `canceled_by` is `operator` or `schedule`; trial-end cancels carry `reason: trial_end_cancel` (transactional) |
| `subscription.trial_ended` | Trial ended — payload `triggered_by` is `operator` or `schedule` (transactional) |
| `subscription.trial_extended` | Trial end pushed later |
| `subscription.cancel_scheduled` / `.cancel_cleared` | Cancel scheduled for period end / schedule cleared |
| `subscription.collection_paused` / `.collection_resumed` | Collection paused (invoices draft only) / resumed |
| `subscription.item.added` / `.updated` / `.removed` | Item CRUD (plan or quantity changes) |
| `subscription.pending_change.scheduled` / `.applied` / `.canceled` | Next-cycle change lifecycle |
| `subscription.threshold_crossed` | A usage billing threshold crossed (early finalize) |
| `customer.email_bounced` | A customer email bounced and was suppressed |
| `dunning.started` / `.escalated` / `.resolved` | Dunning lifecycle |
| `credit.balance_low` | Balance crossed below the tenant's configured threshold (payload carries `threshold_cents`; transactional) |
| `credit.balance_depleted` | Balance crossed to ≤ 0 (transactional) |
| `credit.balance_recovered` | Balance went positive again (transactional) |
| `credit.commit_retired` | A relief credit note retired commit credits — payload carries `grant_id`, `credit_note_id`, `retired_cents`, `refunded_gross_cents`, `remaining_after_cents` (transactional) |

Typical entitlement receiver for the commit-drawdown wedge (customers
prepay a credit balance that usage draws down): subscribe to
`credit.balance_low` (nudge the customer to top up), `credit.balance_depleted`
(suspend service), `credit.balance_recovered` (restore), and
`subscription.canceled` (deprovision).
