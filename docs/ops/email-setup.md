# Email Setup

How to connect a Velox deployment's outbound email to an SMTP provider
and check that it works. It is written for the engineer who operates
the deployment, and it assumes no prior knowledge of this codebase.

Velox sends all customer-facing email through SMTP:

- invoices, receipts and credit notes
- [dunning](../README.md#glossary) notices (automated reminders about overdue payments)
- payment-failed notifications and payment-setup links
- password resets and team invites

You plug in your existing email service provider (ESP) through env
vars. Swapping providers needs no code changes.

This doc covers production-ready SMTP configuration. Bounce and
complaint webhooks are out of scope for v1. These would be per-ESP
webhook receivers that feed `email_status` back into Velox
(`email_status` is Velox's per-customer deliverability field). Instead,
configure suppressions (do-not-send lists) on the ESP side.

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

Restart Velox. The next email queued in `email_outbox` then goes out
through your provider. `email_outbox` is the Postgres table where every
outgoing email waits for a background dispatcher to send it (see
[outbox](../README.md#glossary)).

The server logs a WARN line at boot for each missing env var. A
misconfiguration is therefore easy to see, and it does not stop startup:

| Env var unset | Boot warning | Customer-visible failure |
|---|---|---|
| `SMTP_HOST` | `SMTP NOT CONFIGURED — …` | Every send returns `ErrSMTPNotConfigured`, dispatcher retries → DLQ. No stdout fallback. |
| `HOSTED_INVOICE_BASE_URL` | `HOSTED_INVOICE_BASE_URL NOT SET — …` | Invoice / receipt / dunning / payment-failed emails render with **no link** in the CTA button. |
| `CUSTOMER_PORTAL_URL` | *(no dedicated boot warning)* | Not a customer email variable — it is the SPA base for the Stripe payment-method-setup return URL. Unset → the return URL silently defaults to `http://localhost:5173`. |
| `DASHBOARD_BASE_URL` | `DASHBOARD_BASE_URL NOT SET — …` | Password-reset emails are not sent. Team invites fail because the accept link cannot be built. |
| `PAYMENT_UPDATE_URL` | `PAYMENT_UPDATE_URL NOT SET — …` | Payment-update-request emails (no-PM-at-finalize, charge-failure) skipped at send time. |

("No-PM-at-finalize" above = an invoice reached finalization with no
payment method on file for the customer.)

For local dev, point Velox at the Mailpit container bundled in
`docker-compose.yml`. Mailpit is a local SMTP catcher with a web inbox.
See the Mailpit section at the end of the provider list below.

## Sender domain authentication (your responsibility)

Your sending domain's DNS is yours to manage as the team operating the
deployment, so Velox can't configure it for you. Before going to production, configure your sending domain so
emails don't land in spam:

- **SPF** record: list your ESP's sending IPs in DNS.
- **DKIM**: most ESPs auto-sign if you add their CNAME records.
- **DMARC**: start with `p=none` (monitor mode), then tighten to
  `p=quarantine` once SPF + DKIM are passing.

Each ESP has step-by-step DNS-config docs. You do this setup once per
sending domain. If you skip it, expect ~30%+ of emails to be delivered
into spam folders.

## Per-provider configuration

Each section below assumes you've already created the sender domain
and verified DKIM/SPF in the ESP's dashboard.

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
Token. Postmark is transactional-only. Use it for billing emails, and
don't send marketing email through it. [docs](https://postmarkapp.com/developer/user-guide/send-email-with-smtp)

### AWS SES (port 587, STARTTLS)

```bash
SMTP_HOST=email-smtp.us-east-1.amazonaws.com   # use your region
SMTP_PORT=587
SMTP_USERNAME=YOUR-SMTP-USERNAME-FROM-IAM
SMTP_PASSWORD=YOUR-SMTP-PASSWORD-FROM-IAM
SMTP_FROM=billing@yourdomain.com
SMTP_TLS=starttls
```

The username and password aren't your AWS credentials. Generate
SMTP-specific credentials in the SES console (IAM → Create SMTP creds).
Verify your sending domain in SES first. New accounts start in sandbox
mode, which sends only to verified recipients. [docs](https://docs.aws.amazon.com/ses/latest/dg/send-email-smtp.html)

### AWS SES (port 465, implicit TLS)

```bash
SMTP_HOST=email-smtp.us-east-1.amazonaws.com
SMTP_PORT=465
SMTP_USERNAME=YOUR-SMTP-USERNAME-FROM-IAM
SMTP_PASSWORD=YOUR-SMTP-PASSWORD-FROM-IAM
SMTP_FROM=billing@yourdomain.com
SMTP_TLS=implicit
```

Use this if your egress firewall blocks STARTTLS on 587. Delivery
works the same way; only the transport differs.

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
emails/day. It has the cleanest API and dashboard among modern
providers, so pick it if you don't have an ESP yet. [docs](https://resend.com/docs/send-with-smtp)

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

Emails appear in the Mailtrap dashboard. Nothing reaches the actual
recipient inbox. [docs](https://help.mailtrap.io/article/12-getting-started-guide)

### Mailpit (local dev — bundled in `docker-compose.yml`)

The repo's `docker-compose.yml` already runs Mailpit alongside
Postgres and Redis. Bring it up with:

```bash
docker compose up -d mailpit
```

Then point Velox at it, setting all the vars below together. If you
leave any of the URL vars unset, the matching email links stay blank
or the email is not sent:

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
DASHBOARD_BASE_URL=http://localhost:5173
```

View captured email at <http://localhost:8025>. Nothing leaves your
machine, yet dev runs the same SMTP code path as production. An
earlier "log to stdout when SMTP_HOST is unset" fallback was removed,
so dev and prod can't drift apart.

The inbox at `http://localhost:8025` is local-only and needs no DNS or
auth. Don't use `SMTP_TLS=none` in production, because emails then
travel in plaintext.

## Common configuration mistakes

| Mistake | Symptom | Fix |
|---|---|---|
| Forgot DNS for sender domain | Emails land in spam | Configure SPF + DKIM at the ESP |
| Wrong username | `535 authentication failed` | Username is provider-specific (see above) |
| `SMTP_FROM` not verified at ESP | `550 not authorized` | Verify the sender domain or use a verified address |
| ESP in sandbox/sandbox-restricted | `554 recipient not verified` | Move ESP out of sandbox (SES) or verify recipient |
| Firewall blocks 587 outbound | Connect timeout | Switch to port 465 + `SMTP_TLS=implicit` |
| ESP rate-limited the relay | `421 throttled` | Check ESP dashboard; raise the sending limit at the ESP |

## Verifying your configuration

After setting env vars, restart Velox and run a test send. The
commands use three shell variables that you set first:

- `$API`: the base URL of your Velox API.
- `$KEY`: a secret API key (`vlx_secret_…`) for the tenant that owns the
  invoice, in the same mode (test or live) as the invoice. See
  [API key types](../README.md#glossary).
- `$INV_ID`: the ID of a finalized invoice.

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

How to read the result:

- Status `dispatched` means the email was delivered to the ESP.
- Status `pending` with attempts > 0 and a `last_error` means the ESP
  rejected it. Fix the cause the error message names.

## Bounce + complaint handling (deferred)

When SMTP returns a permanent 5xx, Velox marks the customer's
`email_status` as `bounced` (via `bounceReporterAdapter` in
`router.go`). This catches synchronous failures.

Asynchronous bounces don't flow back through SMTP. In an asynchronous
bounce, the ESP accepts the message but the recipient mailbox rejects
it later. Each ESP has its own webhook format for these:

- **SES**: SNS notifications → forward to a Velox webhook endpoint.
- **SendGrid**: Event Webhook → POST to your endpoint.
- **Postmark**: Webhooks → POST.

For v1, configure suppressions inside the ESP's dashboard. This stops
repeat sends to bouncing addresses without a Velox-side integration.
Per-ESP webhook receivers can be added later, when a
[design partner](../README.md#glossary) (DP) needs them.

## When to consider an API-based backend

Velox today is SMTP-only. For most DPs at v1 scale (thousands of
emails/month), SMTP is sufficient. Switch to an ESP-native API
backend when:

- You need per-message tracking (opens, clicks) inline in the Velox
  dashboard. SMTP doesn't carry these signals.
- Volume justifies dedicated IPs (typically >100k emails/month).
- Bounce/complaint webhooks need to feed back into Velox without a
  separate IMAP listener.

When you reach that point, add a backend selector that swaps the
underlying transport: `EMAIL_PROVIDER=smtp` (default) or
`EMAIL_PROVIDER=resend|postmark|ses`. SMTP stays as the option that
works with every provider.

## Sending high-volume from one tenant

A [tenant](../README.md#glossary) is one business that uses a Velox
install to bill its own customers. If a single tenant sends >10k
emails/hour:

- **Check your ESP rate limit.** Velox runs one email dispatcher. It
  sends at most 5 emails every 5 seconds (60 a minute), and this is
  not configurable. Above that rate, emails queue in `email_outbox`.
- **Spread across providers.** Some tenants use one ESP for
  transactional billing emails and another for marketing, with
  separate sending domains.
- **Negotiate dedicated IPs with your ESP** before crossing 50k
  emails/day from a single sender.

These are operations-level decisions. Velox doesn't enforce limits.
