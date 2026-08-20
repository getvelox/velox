# Email Setup

How to wire a Velox deployment's outbound email to an SMTP provider
and verify it works. Written for the engineer operating the
deployment — no prior familiarity with this codebase assumed.

Velox sends customer-facing emails (invoices, receipts, credit notes,
dunning notices — automated overdue-payment reminders — plus
payment-failed notifications, payment-setup links,
password resets, team invites) through SMTP. Plug in your existing
email service provider (ESP) via env vars; no code changes needed to
swap providers.

This doc covers production-ready SMTP configuration. Bounce/
complaint webhooks (per-ESP webhook receivers feeding `email_status`
— Velox's per-customer deliverability field — back into Velox) are
out of scope for v1 — configure suppressions (do-not-send lists) on
the ESP side instead.

## Quickstart

SMTP relay (six env vars cover most setups):

```bash
SMTP_HOST=smtp.your-provider.com
SMTP_PORT=587
SMTP_USERNAME=your-username-or-apikey
SMTP_PASSWORD=your-password-or-apikey-secret
SMTP_FROM=billing@yourdomain.com
SMTP_TLS=starttls         # starttls (default) | implicit | none
```

Plus three URL vars that build the call-to-action (CTA) links in
customer-facing emails:

```bash
HOSTED_INVOICE_BASE_URL=https://billing.example.com    # email "View & pay invoice" / "View receipt" CTA target → /invoice/<public_token>
CUSTOMER_PORTAL_URL=https://billing.example.com        # SPA base for the Stripe payment-method-setup return URL
PAYMENT_UPDATE_URL=https://billing.example.com/update-payment   # payment-update-request emails (no-PM-at-finalize, charge-failure)
```

Restart Velox; the next email queued in `email_outbox` — the Postgres
table where every outgoing email waits for a background dispatcher to
send it — goes out via your provider. The server boots with WARN
lines for each missing env, so misconfiguration is unmissable without
preventing startup:

| Env var unset | Boot warning | Customer-visible failure |
|---|---|---|
| `SMTP_HOST` | `SMTP NOT CONFIGURED — …` | Every send returns `ErrSMTPNotConfigured`, dispatcher retries → DLQ. No stdout fallback. |
| `HOSTED_INVOICE_BASE_URL` | `HOSTED_INVOICE_BASE_URL NOT SET — …` | Invoice / receipt / dunning / payment-failed emails render with **no link** in the CTA button. |
| `CUSTOMER_PORTAL_URL` | *(no dedicated boot warning)* | Not a customer email variable — it is the SPA base for the Stripe payment-method-setup return URL. Unset → the return URL silently defaults to `http://localhost:5173`. |
| `PAYMENT_UPDATE_URL` | `PAYMENT_UPDATE_URL NOT SET — …` | Payment-update-request emails (no-PM-at-finalize, charge-failure) skipped at send time. |

("No-PM-at-finalize" above = an invoice reached finalization with no
payment method on file for the customer.)

For local dev, point at the Mailpit container — a local SMTP catcher
with a web inbox — bundled in `docker-compose.yml` (see the Mailpit
section at the end of the provider list below).

## Sender domain authentication (DP responsibility)

A DP (design partner) is an early customer running their own Velox
deployment — DNS sits with you, the operator; Velox can't configure
it for you. Before going to production, configure your sending
domain so emails don't land in spam:

- **SPF** record: list your ESP's sending IPs in DNS.
- **DKIM**: most ESPs auto-sign if you add their CNAME records.
- **DMARC**: start with `p=none` (monitor mode), then tighten to
  `p=quarantine` once SPF + DKIM are passing.

Each ESP has step-by-step DNS-config docs; this is one-time setup
per sending domain. Ignoring it = ~30%+ delivery into spam folders.

## Per-provider configuration

Each section below assumes you've already created the sender domain
+ verified DKIM/SPF in the ESP's dashboard.

### SendGrid (most common)

```bash
SMTP_HOST=smtp.sendgrid.net
SMTP_PORT=587
SMTP_USERNAME=apikey
SMTP_PASSWORD=SG.xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
SMTP_FROM=billing@yourdomain.com
SMTP_TLS=starttls
```

`SMTP_USERNAME` is literally the string `apikey`; the password is
the API key. Free tier: 100 emails/day. [docs](https://docs.sendgrid.com/for-developers/sending-email/integrating-with-the-smtp-api)

### Postmark

```bash
SMTP_HOST=smtp.postmarkapp.com
SMTP_PORT=587
SMTP_USERNAME=YOUR-SERVER-API-TOKEN
SMTP_PASSWORD=YOUR-SERVER-API-TOKEN
SMTP_FROM=billing@yourdomain.com
SMTP_TLS=starttls
```

Both `SMTP_USERNAME` and `SMTP_PASSWORD` are the same Server API
Token. Postmark is transactional-only — use for billing emails;
don't try to push marketing through it. [docs](https://postmarkapp.com/developer/user-guide/send-email-with-smtp)

### AWS SES (port 587, STARTTLS)

```bash
SMTP_HOST=email-smtp.us-east-1.amazonaws.com   # use your region
SMTP_PORT=587
SMTP_USERNAME=YOUR-SMTP-USERNAME-FROM-IAM
SMTP_PASSWORD=YOUR-SMTP-PASSWORD-FROM-IAM
SMTP_FROM=billing@yourdomain.com
SMTP_TLS=starttls
```

The username/password aren't your AWS credentials — generate SMTP-
specific credentials in the SES console (IAM → Create SMTP creds).
Verify your sending domain in SES first; new accounts start in
sandbox mode (recipient-verified only). [docs](https://docs.aws.amazon.com/ses/latest/dg/send-email-smtp.html)

### AWS SES (port 465, implicit TLS)

```bash
SMTP_HOST=email-smtp.us-east-1.amazonaws.com
SMTP_PORT=465
SMTP_USERNAME=YOUR-SMTP-USERNAME-FROM-IAM
SMTP_PASSWORD=YOUR-SMTP-PASSWORD-FROM-IAM
SMTP_FROM=billing@yourdomain.com
SMTP_TLS=implicit
```

Use this if your egress firewall blocks STARTTLS on 587. Identical
delivery semantics; just different transport.

### Mailgun

```bash
SMTP_HOST=smtp.mailgun.org
SMTP_PORT=587
SMTP_USERNAME=postmaster@yourdomain.mailgun.org
SMTP_PASSWORD=YOUR-MAILGUN-SMTP-PASSWORD
SMTP_FROM=billing@yourdomain.com
SMTP_TLS=starttls
```

`SMTP_USERNAME` is the SMTP-specific user from the Mailgun dashboard
(domain → SMTP credentials), not your account email. [docs](https://documentation.mailgun.com/en/latest/quickstart-sending.html#send-via-smtp)

### Resend (modern alternative)

```bash
SMTP_HOST=smtp.resend.com
SMTP_PORT=587
SMTP_USERNAME=resend
SMTP_PASSWORD=re_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
SMTP_FROM=billing@yourdomain.com
SMTP_TLS=starttls
```

Username is literally `resend`. Free tier: 3000 emails/month, 100
emails/day. Cleanest API + dashboard among modern providers; pick
this if you don't have an ESP yet. [docs](https://resend.com/docs/send-with-smtp)

### Mailtrap (testing sandbox — non-production)

For staging environments where you want to inspect what would have
been sent without actually delivering:

```bash
SMTP_HOST=sandbox.smtp.mailtrap.io
SMTP_PORT=2525
SMTP_USERNAME=YOUR-MAILTRAP-USERNAME
SMTP_PASSWORD=YOUR-MAILTRAP-PASSWORD
SMTP_FROM=billing@example.com
SMTP_TLS=starttls
```

Emails appear in the Mailtrap dashboard; nothing reaches the actual
recipient inbox. [docs](https://help.mailtrap.io/article/12-getting-started-guide)

### Mailpit (local dev — bundled in `docker-compose.yml`)

The repo's `docker-compose.yml` already runs Mailpit alongside
Postgres and Redis. Bring it up with:

```bash
docker compose up -d mailpit
```

Then point Velox at it (all five URL/SMTP vars together — leaving any
of HOSTED_INVOICE_BASE_URL / CUSTOMER_PORTAL_URL / PAYMENT_UPDATE_URL
unset will leave the corresponding email links blank):

```bash
SMTP_HOST=localhost
SMTP_PORT=1025
SMTP_USERNAME=
SMTP_PASSWORD=
SMTP_FROM=billing@local.test
SMTP_TLS=none
HOSTED_INVOICE_BASE_URL=http://localhost:5173
CUSTOMER_PORTAL_URL=http://localhost:5173
PAYMENT_UPDATE_URL=http://localhost:5173/update-payment
```

View captured email at <http://localhost:8025>. Nothing leaves
your machine, yet dev exercises the same SMTP code path as
production — an earlier "log to stdout when SMTP_HOST is
unset" fallback was removed so dev and prod can't drift.

The inbox at `http://localhost:8025` is local-only; no DNS or auth
required. Don't use `SMTP_TLS=none` in production — emails travel
in plaintext.

## Common configuration mistakes

| Mistake | Symptom | Fix |
|---|---|---|
| Forgot DNS for sender domain | Emails land in spam | Configure SPF + DKIM at the ESP |
| Wrong username | `535 authentication failed` | Username is provider-specific (see above) |
| `SMTP_FROM` not verified at ESP | `550 not authorized` | Verify the sender domain or use a verified address |
| ESP in sandbox/sandbox-restricted | `554 recipient not verified` | Move ESP out of sandbox (SES) or verify recipient |
| Firewall blocks 587 outbound | Connect timeout | Switch to port 465 + `SMTP_TLS=implicit` |
| ESP rate-limited the relay | `421 throttled` | Check ESP dashboard; lower outbox dispatcher concurrency |

## Verifying your configuration

After setting env vars, restart Velox and run a test send:

```bash
# Trigger an invoice email (requires a finalized invoice)
curl -X POST -H "Authorization: Bearer $KEY" \
  -H "Content-Type: application/json" \
  -d '{"email":"you@yourdomain.com"}' \
  "$API/v1/invoices/$INV_ID/send"

# Watch the outbox dispatcher pick it up
PGPASSWORD=velox psql -h localhost -U velox -d velox \
  -c "SELECT email_type, status, attempts, last_error, dispatched_at
      FROM email_outbox ORDER BY created_at DESC LIMIT 5;"
```

Status `dispatched` = delivered to ESP. `pending` with attempts > 0 +
`last_error` = ESP rejected; fix per the error message.

## Bounce + complaint handling (deferred)

When SMTP returns a permanent 5xx, Velox marks the customer's
`email_status` as `bounced` (via `bounceReporterAdapter` in
`router.go`). This catches synchronous failures.

Asynchronous bounces — where the ESP accepts the message but the
recipient mailbox rejects it later — don't flow back through SMTP.
Each ESP has its own webhook format for these:

- **SES**: SNS notifications → forward to a Velox webhook endpoint.
- **SendGrid**: Event Webhook → POST to your endpoint.
- **Postmark**: Webhooks → POST.

For v1, configure suppressions inside the ESP's dashboard — this
prevents repeat-sending to bouncing addresses without needing a
Velox-side integration. Per-ESP webhook receivers can be added later
when a DP needs them.

## When to consider an API-based backend

Velox today is SMTP-only. For most DPs at v1 scale (thousands of
emails/month), SMTP is sufficient. Switch to an ESP-native API
backend when:

- You need per-message tracking (opens, clicks) inline in the Velox
  dashboard. SMTP doesn't carry these signals.
- Volume justifies dedicated IPs (typically >100k emails/month).
- Bounce/complaint webhooks need to feed back into Velox without a
  separate IMAP listener.

When that bar is hit, add a backend selector — `EMAIL_PROVIDER=smtp`
(default) or `EMAIL_PROVIDER=resend|postmark|ses` — that swaps the
underlying transport. SMTP stays as the universal floor.

## Sending high-volume from one tenant

A tenant is one company's account on a Velox deployment — the
business doing the billing, not its end customers. If a single
tenant sends >10k emails/hour:

- **Lower the email-outbox dispatcher concurrency** — Velox runs one
  dispatcher worker today; high concurrency against rate-limited
  relays = throttling.
- **Spread across providers** — some tenants use one ESP for
  transactional billing emails and another for marketing, with
  separate sending domains.
- **Negotiate dedicated IPs with your ESP** before crossing 50k
  emails/day from a single sender.

These are operations-level decisions; Velox doesn't enforce limits.
