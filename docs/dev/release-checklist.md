# Release checklist: live services

Run this by hand for each release. It covers what automated tests cannot
prove, because they fake the provider: Stripe, email, LiteLLM and the invoice
PDF. Use Stripe test mode only. See [How we test](../testing.md).

**When to run each part:**

| Part | When |
|---|---|
| [Setup](#setup) to [Email, PDF, LiteLLM](#email-pdf-litellm) | Before you tag the release |
| [Fresh install and upgrade](#fresh-install-and-upgrade) and [First invoice tutorial](#first-invoice-tutorial) | After the tag's image build has pushed the release images, and before you publish the release notes |

The checks write Stripe-linked test data into the database the server uses.
Point the server at a fresh or dedicated database if you want to keep yours
clean.

## Setup

- `cp .env.example .env`, then `make up` (Postgres, Redis, Mailpit), `make bootstrap` (prints the sign-in and the test keys), `make dev` (API on :8080) and `cd web-v2 && npm run dev` (dashboard on :5173).
- The `.env.example` defaults send email to Mailpit and point email links at the dashboard on :5173. Read mail at http://localhost:8025.
- Settings → Payments: paste the Stripe test keys. Run `stripe listen --forward-to localhost:8080/v1/webhooks/stripe/<vlx_spc_id>` and paste its `whsec_…` back.
- Test cards: `4242 4242 4242 4242` succeeds; `4000 0000 0000 0341` saves but declines on charge; `4000 0027 6000 3184` asks for 3-D Secure.
- In Checkout, choose "Pay without Link" if offered, and give a ZIP for a US address, or Save does nothing.
- [Stripe end-to-end test](stripe-end-to-end-test.md) has the step-by-step commands for the Stripe checks.

## Stripe

- [ ] Wrong keys are refused: a made-up `sk_test_` key shows "Stripe didn't accept these keys".
- [ ] Charge: the first invoice of a subscription whose base fee bills in advance is charged to `4242`, Velox shows it paid, and the Stripe dashboard shows the same amount.
- [ ] Webhook: `stripe listen` shows `payment_intent.succeeded` answered with 200.
- [ ] Hosted invoice page: Pay with `4242` → "Paid" banner within seconds, and the card appears on the customer.
- [ ] 3-D Secure: Pay with `4000 0027 6000 3184`, fail the challenge → the invoice stays unpaid; pass it → paid.
- [ ] Refund: a credit note with "Refund to card" → `refund_status=succeeded`, and the refund shows in the Stripe dashboard.
- [ ] Stripe Tax: with a US state registration in the Stripe test account, tax provider "Stripe Tax", customer with a full US address → the invoice has a tax line that matches Stripe's calculation.

## Money walk across a renewal

A dunning run is the automatic retries after a failed payment.

- [ ] Dunning policies page: "Default retries" is the default and enabled. A bootstrap creates it in both modes (ADR-123). When its retries run out it changes nothing: final actions none and none. If you ran the demo first, its recipe's "AI default retry" is the default instead, and "Default retries" stays listed.
- [ ] Mint a test clock, create a customer with an email, pinned to the clock, and subscribe it to a plan with a base fee (plans bill in arrears by default).
- [ ] With `stripe listen` running, open the customer's setup link (Add payment method) and save card `0341` in Checkout → the card is listed on the customer. Do this before you advance the clock.
- [ ] Advance the clock past the renewal → the renewal invoice is declined and a dunning run starts.
- [ ] Open the invoice link in the payment-failed email and pay with `4242` → the invoice is paid, and the dunning run shows recovered.

## Email, PDF, LiteLLM

- [ ] Settings → Email → Send test email: the page says "Sent to" your address and the message arrives in Mailpit. With a wrong `SMTP_PORT`, the page shows the mail server's error instead.
- [ ] Invoice → Email: the message arrives in Mailpit; open it, read it, and check the amount and the links.
- [ ] The emailed PDF opens, and its total, lines, tax and buyer address match the invoice page.
- [ ] Run a real LiteLLM proxy with the callback from [the LiteLLM guide](../integrations/litellm.md) → one completion creates one `input` and one `output` usage event on the customer (plus `cache_read` if the prompt was cached).

## Fresh install and upgrade

These prove that the images the quick start pulls work, and that an existing install can move to them. Use a machine that has Docker but no Velox containers or volumes, for example a new VM.

### Fresh install

- [ ] Clone the release tag, then run `./quickstart.sh` in `deploy/compose`. The output names the new release ("Starting Velox v0.X.Y from the published images.") and ends with a login and a test key.
- [ ] `docker compose exec velox-api /usr/local/bin/velox version` prints the new release.
- [ ] `./quickstart.sh demo` ends with links to a finalized invoice and its customer, and both open in the dashboard.

### Upgrade drill

1. Run `./quickstart.sh destroy` to start from an empty install. Check out the previous release tag and run `./quickstart.sh`, then `./quickstart.sh demo`.
   **You see:** the demo's invoice link. Open it and note the invoice number, total and line amounts. Download its PDF.
2. Check out the new release tag. In `deploy/compose/.env`, change the tag on the `VELOX_IMAGE` and `VELOX_DASHBOARD_IMAGE` lines to the new release. Run `docker compose pull`, then `docker compose run --rm velox-api migrate status`.
   **You see:** the plan, with `running now:  no`. From v0.8.1 to v0.9.0: `ready to move`. For a later release: `behind`, listing the release's new migrations. Nothing is `refused`.
3. Run `docker compose up -d --no-build`.
   **You see:** `velox-api` is recreated, and `velox version` prints the new release.
4. Check the migrations: `docker compose logs velox-api | grep migration`.
   **You see:** one `migration applied` line per migration (`00180 v0.8.1 baseline` when moving from v0.8.1), then `migrations applied`, with `version` set to the new release's last migration number, and no error. `velox migrate status` now says `up to date`.
5. Open the invoice from step 1.
   **You see:** the same number, total and line amounts. Its PDF has the same lines and amounts as the one you downloaded. The "Generated on" time at the bottom differs, because each PDF is made when you open it. A release that changes what the PDF prints says so in its notes.
6. Open the Dunning policies page.
   **You see:** the same policies as before the upgrade. An upgrade never adds or changes a retry policy on an existing account.
7. Run `./quickstart.sh demo` again.
   **You see:** a second finalized invoice for a new demo customer.
8. Run `./quickstart.sh destroy` when you are done.

## First invoice tutorial

- [ ] Run [docs/first-invoice.md](../first-invoice.md) end to end against the quick-start stack from the fresh install. Every command works as written, and each output matches its "You see:" line. When a step differs, fix the page on `main` before you publish the release notes.
