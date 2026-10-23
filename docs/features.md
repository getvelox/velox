# What's in the box

Everything below is in the one MIT edition. For what Velox doesn't do, see
[Is Velox right for you?](../README.md#is-velox-right-for-you).

## Pricing for AI and usage

A meter is a counter for one kind of usage, such as tokens. Dimensions are
labels on a usage event, such as `model` or `token_type`.

| Feature | What it does for you | Learn more |
|---|---|---|
| Meters with dimensions | One meter can have many prices. Each usage event carries labels, such as `model`, that pick its price. So one Tokens meter prices every model and token type. | [How Velox works](how-velox-works.md) |
| Pricing recipes | One call creates a meter, prices and a plan for a common pricing shape: `anthropic_style`, `openai_style` or `replicate_style`. It also adds a payment-retry policy, and a webhook endpoint that stays off until you give it your URL and turn it on. | [AI pricing](ai-pricing.md) |
| LiteLLM | Point LiteLLM's spend callback at Velox, and each AI call becomes token usage, labelled with `model` and `provider`. A resent call is counted once. No SDK (client library) is needed. | [LiteLLM guide](integrations/litellm.md) |
| Prepaid commits | Sell "pay $9k, get $10k of usage" as a line on a one-off invoice. The credit is added when that invoice is finalized, in the same step and before it is paid. Each later invoice uses it up when Velox collects it, free (promotional) credit first. | [AI pricing](ai-pricing.md#sell-prepaid-credit-a-prepaid-commit) |
| Margin per customer | Enter what each AI provider charges you per token, once. Velox then shows each customer's usage revenue next to what it cost you. Covers usage only: base fees, credits and taxes are left out. | [AI pricing](ai-pricing.md#see-your-margin) |
| Exact decimals | Quantities can be fractions, such as part of a GPU-hour, with up to 12 decimal places. Prices are kept exactly as written, such as $3.00 per 1M tokens. Each invoice line is rounded to whole cents. | [AI pricing](ai-pricing.md#price-a-model-a-recipe-does-not-cover) |
| Ways to count | Each meter picks how its usage adds up: `sum`, `count`, `max`, or `last` (the latest value in the period). Every price on the meter uses it, and it is fixed once the meter has a usage event. Held usage does not count. | [AI pricing](ai-pricing.md#price-a-model-a-recipe-does-not-cover) |
| Usage your customers can see | A usage and cost breakdown per customer in the dashboard, and a JSON link your app can show the customer ("$4.31 of GPT-4 this billing period"). | [Stop usage when credit runs out](stop-usage-when-credit-runs-out.md#the-same-figure-on-the-public-cost-page) |

## Runs on your own servers

| Feature | What it does for you | Learn more |
|---|---|---|
| One program to run | Velox is a single Go program. Background jobs run inside it, so there is no queue, worker fleet or separate scheduler to run. The Compose file adds Postgres, Redis (for rate limits only), the dashboard and nginx in front: five containers, one of them Velox. | [Self-host](self-host.md) |
| Nothing held back, nothing sent home | MIT with no licence key or paid tier. No telemetry; data goes only to services you set up, such as Stripe, your email server and your tracing collector. Billing data lives in your own Postgres. | [Why Velox](why-velox.md) |
| Audit log that cannot be edited | A log of who changed what in your account. The database refuses to change or delete any entry in it. | [Architecture](architecture.md) |
| Data kept apart | On most API requests, each account's data, and its test and live data, are kept apart by Postgres Row-Level Security (RLS) as well as by the application code. Some work runs outside RLS and filters by account in the application code. That includes background jobs, sign-in and sessions, public links such as the hosted invoice page, incoming webhooks, Stripe credential settings, and team members and invites ([full list](architecture.md#design-rules)). With `APP_ENV` set to `staging` or `production`, Velox will not start without RLS. | [Postgres requirements](ops/postgres-requirements.md#the-two-database-users) |

Velox has no SSO; [ADR-014](adr/014-sso-direction-embedded-oidc-saml.md)
says how it would be added. It has no roles either: every team member has
full access ([Roadmap](roadmap.md#later)).

## Subscriptions and invoices

| Feature | What it does for you | Learn more |
|---|---|---|
| Subscriptions | Trials, pausing collection (invoices stay drafts), cancelling at period end, and plan changes with proration: the customer pays only for the part of the period each price was active. | [The Velox API](api-surface.md#customers-and-subscriptions) |
| Discounts | A different price for one customer (a price override), or credit. There are no coupons: they were cut before launch, and credit and price overrides do the job. | [The Velox API](api-surface.md#credits-and-refunds) |
| Invoices | PDF invoices, a hosted invoice page the customer opens from a secure link, and emails with your branding. | [How Velox works](how-velox-works.md) |
| Invoice preview | See the next invoice before it is made. The preview shows the subtotal only, without tax or credits. | [The Velox API](api-surface.md#invoices-and-billing) |
| Dunning | When a card payment fails, Velox retries it on a schedule and, once email is set up, emails the customer. If every retry fails, it does what your policy says. | [How Velox works](how-velox-works.md) |
| Billing thresholds | Set a spend or usage level, and Velox makes an invoice early once the period's usage reaches it. It bills earlier; it does not stop usage. | [Stop usage when credit runs out](stop-usage-when-credit-runs-out.md) |

A threshold fires at most once per period, and the period keeps its dates.
It counts and bills usage only. The base fee is billed as it would be with
no threshold.
It is checked on each scheduled billing run: every hour, or every 5 minutes
with `APP_ENV=local`. `POST /v1/billing/run` does not check it. The
period's own invoice does not bill again what the threshold invoice billed,
even after you write the threshold invoice off.
Voiding it before the period's invoice is built does: that invoice then
bills those charges too ([runbook](ops/runbook/invoice-timing-late-usage.md)).

## Credits and refunds

| Feature | What it does for you | Learn more |
|---|---|---|
| Credit balance | Give customers credit and see every change to it. Each change is a new record; nothing is overwritten. | [The Velox API](api-surface.md#credits-and-refunds) |
| Live balance | The credit left after usage not yet invoiced, so your app can stop serving a customer whose credit has run out. | [Stop usage when credit runs out](stop-usage-when-credit-runs-out.md) |
| Credit notes and refunds | A credit note lowers what a customer owes on an invoice, as a balance credit or a refund to the card. | [The Velox API](api-surface.md#credits-and-refunds) |

## Reliability

| Feature | What it does for you | Learn more |
|---|---|---|
| Safe retries | Send an `Idempotency-Key` header on a POST, PUT or PATCH, and a retry within 24 hours gets the first answer instead of running again. A server error is saved too, so a retry cannot repeat a charge that may have gone through. The usage routes ignore the header: each event's own key stops double counting. | [The Velox API](api-surface.md#sending-usage-safely) |
| Careful card charges | When Velox cannot tell whether a charge went through, it asks Stripe instead of charging again. Only one charge runs on an invoice at a time, and a second payment that gets through is flagged for you to refund. | [Card payment failure handling](payments.md) |
| Webhooks kept in the database | Every webhook is queued in the database and retried. The events marked *always sent* are saved with the change, so a crash cannot drop them. A crash just after a change can drop one of the others. | [Webhooks](webhooks.md#three-rules-for-your-receiver) |
| Signed webhooks | Every webhook is signed. When you rotate the secret, both the old and the new secret sign each message for 72 hours. | [Webhooks](webhooks.md#verifying-signatures) |
| Live webhook log | Watch webhooks as they go out, with every delivery attempt. | [Webhooks](webhooks.md#retries) |
| [Test clocks](glossary.md#testing-with-simulated-time) | Move a test customer's clock forward, and months of billing happen in seconds. | [First invoice](first-invoice.md) |
| Emails kept in a queue | Customer emails wait in a queue in the database, and a send that fails is retried. Payment receipts, failed-payment and dunning emails are saved with the change. Others, such as the invoice email, are queued just after it, so a crash at that moment can drop one. | [Email setup](ops/email-setup.md) |

Everything shipped, by date: [CHANGELOG](../CHANGELOG.md).

**Next:** see it run on your computer with the [quick start](quick-start.md).
