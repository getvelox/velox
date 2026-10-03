# Stripe end-to-end test runbook

This runbook checks by hand that Velox's Stripe integration works against a real Stripe test account.
Run it before each [design-partner](../README.md#glossary) cutover, which brings an early pilot
customer live on Velox. Also run it after any change touching
`internal/payment/`, `internal/tenantstripe/`, or `internal/dunning/`.

**Audience:** Velox maintainer, design-partner technical contact during sandbox cutover.

**Never use live keys for this runbook.**

**Prerequisites:**

- A Stripe **test-mode** account with keys minted: at least a Restricted Key
  (a Stripe key limited to chosen permissions), or a Standard secret key and publishable key.
  - The secret key authenticates server-side API calls.
  - The publishable key is its browser-safe counterpart.
  - Once entered, the keys never leave the dashboard's connection form.
    Velox encrypts them at rest with the same AES-256-GCM key used for customer email.
- The operator dashboard running on `http://localhost:5173` for step 2b.
  Step 0 starts only the API; start the dashboard as in the README
  [Quick start](../../README.md#quick-start).
- `jq`, used to read the JSON responses in steps 3, 4 and 6.
- `$STRIPE_SK` set to your Stripe test secret key, used by the Stripe CLI in step 5.

---

## Step 0 — Stand up Velox locally

```bash
docker compose up -d postgres redis mailpit
DATABASE_URL="postgres://velox:velox@localhost:5432/velox?sslmode=disable" \
  RUN_MIGRATIONS_ON_BOOT=true \
  go run ./cmd/velox-bootstrap   # only on first boot — creates owner + tenant
DATABASE_URL="postgres://velox:velox@localhost:5432/velox?sslmode=disable" \
  RUN_MIGRATIONS_ON_BOOT=true \
  go run ./cmd/velox &
```

Confirm the API is up: `curl -sf http://localhost:8080/health` returns
`{"status":"ok"}`.

Login as the owner the bootstrap step printed and capture cookies for the curl
calls below:

```bash
curl -s -X POST http://localhost:8080/v1/auth/login \
  -H 'Content-Type: application/json' \
  -c /tmp/velox-cookies.txt \
  -d '{"email":"<OWNER_EMAIL>","password":"<OWNER_PASSWORD>"}'
```

---

## Step 1 — Connect the Stripe test account

The connection form encrypts secret keys at rest (AES-256-GCM, same key as customer
email). Do not paste keys into shell history or commit them. Instead, write them to a `umask 077`
tmp file and delete it afterwards:

```bash
umask 077 && cat > /tmp/stripe-connect.json <<'EOF'
{
  "livemode": false,
  "secret_key": "sk_test_...",
  "publishable_key": "pk_test_..."
}
EOF
curl -s -X POST http://localhost:8080/v1/settings/stripe \
  -b /tmp/velox-cookies.txt \
  -H 'Content-Type: application/json' \
  -d @/tmp/stripe-connect.json
rm -f /tmp/stripe-connect.json
```

**Expected response (200):**

```json
{
  "id": "vlx_spc_...",
  "tenant_id": "vlx_ten_...",
  "livemode": false,
  "stripe_account_id": "acct_...",
  "stripe_account_name": "<your account display name>",
  "secret_key_prefix": "sk_test_...",
  "secret_key_last4": "...",
  "verified_at": "2026-04-27T..."
}
```

**What this proves:** Velox called `GET /v1/account` with the supplied
secret key, got an answer, and stored the encrypted keys. A `verified_at` timestamp means the key shape
and scope are valid and Stripe acknowledged the request.

If the response carries `last_verified_error`, the key is stored but Stripe
rejected the verify call. Common causes:

- a live key in test mode;
- a restricted key without `read_only` scope on Account.

> **Code note:** the verify call is `internal/tenantstripe/service.go` `Connect()` → `sc.V1Accounts.Retrieve`.

---

## Step 2 — Create a customer with a saved payment method

A [PaymentIntent](../README.md#glossary) is Stripe's object tracking the
collection of one payment. PaymentIntent flows require a saved payment method.
The Velox dashboard saves one through Stripe's Setup Intent flow, which saves a card
for later charges without charging it now.

Where it can, this runbook calls the API directly and uses one of Stripe's pre-built test payment methods:

- Customer creation (2a) needs no browser.
- Attaching the card (2b) goes through the dashboard and Stripe's hosted page.

### 2a. Create the Velox customer

```bash
curl -s -X POST http://localhost:8080/v1/customers \
  -b /tmp/velox-cookies.txt \
  -H 'Content-Type: application/json' \
  -d '{
    "display_name": "Stripe E2E Smoke",
    "email": "e2e-smoke@velox.test",
    "external_id": "stripe-e2e-smoke-1"
  }'
# → captures vlx_cus_...
```

### 2b. Attach a Stripe test card via the dashboard UI

Open `http://localhost:5173/customers/<vlx_cus_...>` in a browser.
Click **Payment methods → Add card**, then paste Stripe's standard test card:

- Card: `4242 4242 4242 4242`
- Expiry: any future date
- CVC: any 3 digits
- Postal code: any 5 digits

The dashboard creates a Stripe Checkout session in **setup mode**.
The user enters the card on the hosted Stripe page and is redirected back.
Velox stores the resulting `pm_*` payment method ID as a row in the
`payment_methods` table for that customer. That table is the canonical store for a
customer's multiple payment methods.

**Expected:** customer detail shows the card with last4=4242, brand=visa, status=valid.

> **Code note:** the Checkout session is created in `internal/payment/checkout.go`. The old
> `customer_payment_setups.stripe_payment_method_id` column was dropped in
> migration 0097.

### 2c. Failed-card variant (run after the happy path)

Repeat 2b with `4000 0000 0000 0341`. On this Stripe test card the attach succeeds
but charges decline. Do not use `4000 0000 0000 0002`: it declines at SetupIntent
time too, so it never produces this state.

The Setup Intent succeeds, but the eventual PaymentIntent in step 4 will fail.
That failure triggers the [dunning](../README.md#glossary) flow:
automated retry-and-notify handling of a failed payment.

---

## Step 3 — Subscribe the customer to a flat plan

Bootstrap seeds no plans, so create one first. Use the dashboard's Pricing
page, or instantiate a [recipe](../README.md#glossary): a pre-built pricing template shipped with
Velox. The recipe flows live in `MANUAL_TEST.md`.

Price the plan at **2900 cents/month**, because the expected amounts in steps 4 and 7 assume it.
Then pick it:

```bash
PLAN_ID=$(curl -s http://localhost:8080/v1/plans -b /tmp/velox-cookies.txt \
  | jq -r '.data[0].id')

curl -s -X POST http://localhost:8080/v1/subscriptions \
  -b /tmp/velox-cookies.txt \
  -H 'Content-Type: application/json' \
  -d "{
    \"customer_id\": \"<vlx_cus_...>\",
    \"plan_id\": \"$PLAN_ID\",
    \"start_at\": \"now\"
  }"
```

**Expected:** `vlx_sub_...` returned with `status=active`, current period bounds set
to monthly (anchor: now).

---

## Step 4 — Trigger the immediate invoice + PaymentIntent

The flat-fee charge for the first cycle should fire the moment the subscription
activates. Confirm:

```bash
curl -s "http://localhost:8080/v1/invoices?customer_id=<vlx_cus_...>" \
  -b /tmp/velox-cookies.txt | jq '.data[0]'
```

**Expected:** an invoice with `status=open` (or `paid` if the auto-finalize
window has passed), `amount_due_cents=2900`, and `stripe_payment_intent_id`
populated. The PaymentIntent ID also shows up on the Stripe dashboard under
**Test data → Payments**.

If the customer has the `4242` card from step 2b, the PaymentIntent should
auto-confirm and the invoice flips to `status=paid` within ~5 seconds.

---

## Step 5 — Webhook delivery from Stripe → Velox

Velox's webhook ingestion receives Stripe's event notifications. It verifies
their signatures with a **per-tenant** secret. There is no operator-level
`STRIPE_WEBHOOK_SECRET` env var.

- **Where the secret lives:** encrypted, in
  `stripe_provider_credentials.webhook_secret_encrypted`.
- **How a request finds it:** through the `endpoint_id` embedded in the URL path
  `/v1/webhooks/stripe/{endpoint_id}`.
- **Where verified events go:**
  `stripe_webhook_events`, keyed on `(tenant_id, livemode, stripe_event_id)` for
  idempotency. The key is a UNIQUE constraint, so a replayed event cannot be stored twice.
  There is no `processed` column.

> **Code note:** the per-request lookup is `LookupEndpoint` in
> `internal/payment/handler.go`.

For local testing without exposing port 8080 to the public internet:

```bash
brew install stripe/stripe-cli/stripe   # one-time
stripe listen --api-key $STRIPE_SK \
  --forward-to http://localhost:8080/v1/webhooks/stripe/<endpoint_id>
```

`<endpoint_id>` is your tenant's `stripe_provider_credentials.id` (`vlx_spc_…`), the per-tenant webhook endpoint.
There is no platform-level webhook URL.

The CLI will print a temporary webhook secret. Paste it into the Velox dashboard
under **Settings → Stripe → Webhook secret** (or via
`PATCH /v1/settings/stripe/test/webhook`). All subsequent test-mode events flow
through the CLI tunnel.

**Expected:** every Stripe event for steps 2–4 (`payment_method.attached`,
`payment_intent.created`, `payment_intent.succeeded`, `charge.succeeded`) lands
as a row in `stripe_webhook_events` (one per `stripe_event_id` per mode; a
replayed event is deduped by the UNIQUE constraint).

The dashboard's `/webhook_events` page does not show this inbound Stripe-ingestion table.
It shows **outbound** webhook deliveries: events Velox sends to your configured endpoints.

---

## Step 6 — Failed payment + dunning

Re-run step 3 against the customer from step 2c (declined card). The
PaymentIntent fails and dunning kicks in:

```bash
curl -s "http://localhost:8080/v1/dunning/runs?customer_id=<vlx_cus_...>" \
  -b /tmp/velox-cookies.txt | jq
```

**Expected:** a [dunning run](../README.md#glossary) appears **only if a dunning policy is configured
and set default**. Bootstrap deliberately seeds none (ADR-036 amendment).
So on a fresh tenant the failed payment skips enrollment with a
"dunning not configured" WARN, and the runs list stays empty.

1. Create a policy first: dashboard → Dunning policies, or `POST /v1/dunning/policies` +
   `set-default`.
2. Retries then follow that policy's grace period and `retry_schedule`.
3. Each retry is a fresh PaymentIntent. On the Stripe dashboard's Payments page,
   confirm that the PI (PaymentIntent) ID changes per attempt.

To recover the customer, swap the card via step 2b with `4242`. Then wait
for the next scheduled dunning retry. Once the payment succeeds, the run
resolves automatically.

`POST /v1/dunning/runs/{id}/resolve` is the operator
action to close a run manually. There is no per-attempt force-retry
endpoint.

---

## Step 7 — Refund via credit note

Issue a [credit note](../README.md#glossary) against the paid invoice from step 4.
A credit note is the document that records money credited or refunded against an invoice.

```bash
curl -s -X POST http://localhost:8080/v1/credit-notes \
  -b /tmp/velox-cookies.txt \
  -H 'Content-Type: application/json' \
  -d '{
    "invoice_id": "<vlx_inv_...>",
    "reason": "duplicate",
    "lines": [
      {"description": "Refund - full invoice amount", "quantity": 1, "unit_amount_cents": 2900}
    ],
    "refund_amount_cents": 2900,
    "auto_issue": true
  }'
```

**Expected:**

- The credit note is created with `status=issued`.
- Because `refund_amount_cents` is set, Velox calls `refunds.Create` against Stripe, and
  the refund lands on the same charge.
- A `charge.refunded` webhook flows back. The credit note's `refund_status` flips to `succeeded`, and
  `stripe_refund_id` is populated.

A credit note has three allocation fields: `refund_amount_cents`, `credit_amount_cents` and
`out_of_band_amount_cents`. If you leave all three at zero on a paid invoice, Velox defaults to
`credit_amount = total`. That is a customer-balance credit with **no** Stripe refund.

---

## Step 8 — Disconnect

```bash
curl -s -X DELETE http://localhost:8080/v1/settings/stripe/test \
  -b /tmp/velox-cookies.txt
```

**Expected:** 204. The credentials row is deleted, and the encrypted blobs are
purged. Subsequent payment attempts fail with a
`"stripe not configured for this mode"` error rather than panicking.

---

## What to capture in your test report

A passing run reports:

- Stripe account name + ID returned from step 1 verify (proof the key works
  and is scoped to the intended account)
- Webhook event count from step 5 (≥4 events expected for the happy path)
- Final invoice status from step 4 (`paid` for happy, `open` after recovery
  in the dunning path)
- Refund webhook landed within 30 seconds (step 7)

Anything that doesn't match the **Expected** sections above is a bug. File it
under the Velox repo with the request ID Velox returns in the error envelope:
`{"error": {"type", "code", "message", "request_id"}}`.
The envelope is defined in `internal/api/respond/respond.go`. The request ID is also echoed in the `Velox-Request-Id`
response header.

---

## What this runbook does NOT cover

- **Stripe Connect / Express accounts:** Velox is a self-host product, not a
  marketplace. Connect onboarding is out of scope.
- **Live mode:** explicitly forbidden in this runbook. Live testing belongs in
  the design-partner cutover playbook, maintained in the internal `velox-ops`
  repo.
- **Tax:** Stripe Tax integration is exercised separately. See
  `docs/ops/tax-calculation.md`.
- **Payouts / balance:** Velox does not surface Stripe payouts. The operator's
  Stripe dashboard is canonical for that.

---

## Last verified

| Date | Velox SHA | Stripe API version | Verified by | Result |
|---|---|---|---|---|
| 2026-04-27 | `f1b2301` | `2025-08-27.basil` (default) | maintainer | Step 1 (connect + verify) ✅; steps 2–8 documented as runbook, full flow validated during sandbox cutover for first design partner |

Re-run and append a row after every change touching `internal/payment/`,
`internal/tenantstripe/`, or `internal/dunning/`.
