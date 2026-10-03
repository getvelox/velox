# Velox documentation

Start here to find the right doc. Pick what you want to do, then read the
docs in the order listed. Words you may not know are in the
[glossary](#glossary) at the end.

## Decide if Velox fits

1. [README](../README.md): what Velox is, who it is for, and a 30-second demo.
2. [What Velox is not](../README.md#what-velox-is-not): the cases where
   another tool is the better choice.
3. [ENGINEERING.md](ENGINEERING.md): how it is built, and the evidence for
   each claim.
4. [Correctness under failure](benchmarks/failure-correctness.md) and
   [Sustained throughput](benchmarks/sustained-throughput.md): the measured
   results.

## Try it on your machine

1. [Quick start](../README.md#quick-start): Postgres, the API and the
   dashboard on localhost.
2. [`scripts/demo.sh`](../scripts/demo.sh): the full flow from usage events
   to a paid invoice, in about 30 seconds.

## Connect your product

1. [API surface](api-surface.md): the core routes. The full reference is
   [`api/openapi.yaml`](../api/openapi.yaml).
2. [API keys](api-keys.md): key types, rotation, and adding tenants.
3. [Webhooks](webhooks.md): events, signatures, and retries.
4. [LiteLLM integration](integrations/litellm.md): send token usage from a
   LiteLLM proxy without writing code.

## Run it in production

1. [Self-host](self-host.md): what you need and how the pieces fit.
2. [Docker Compose on one VM](../deploy/compose/README.md): step-by-step
   install.
3. [Deployment](../deploy/README.md): scaling, upgrades, and shutdown.
4. [Postgres requirements](ops/postgres-requirements.md),
   [email setup](ops/email-setup.md),
   [backups](ops/backup-considerations.md) and
   [tax calculation](ops/tax-calculation.md).
5. When something breaks: the [operations runbook](ops/runbook.md), the
   [leader-lease runbook](ops/runbook-leader-leases.md) and the
   [failure-mode catalog](failure-modes/README.md).
6. [Stripe end-to-end test](ops/stripe-end-to-end-test.md): check a real
   Stripe account before go-live.

## Contribute

1. [CONTRIBUTING](../CONTRIBUTING.md): setup, tests, and what CI checks.
2. [Architecture](architecture.md): package layout and design rules.
3. [Money-path robustness playbook](dev/money-path-robustness-playbook.md):
   required for any change to invoices, payments, credits, dunning,
   subscriptions or tax.
4. [Manual-test strategy](dev/manual-test-strategy.md) and
   [MANUAL_TEST](../MANUAL_TEST.md): how flows are checked by hand.
5. [OpenAPI workflow](dev/openapi-workflow.md): changing the API contract.
6. [Writing style](dev/writing-style.md): how to write docs here.
7. AI coding agents start at [CLAUDE.md](../CLAUDE.md), then
   [agent prompting standards](dev/agent-prompting-standards.md).

## Understand a decision

- [Architecture decision records (ADRs)](adr/README.md): one decision per
  file, with the reasons and the alternatives.
- Design docs, written while each feature was planned. Where a design doc
  and the code disagree, the code and the ADRs are the current record.
  - [Billing thresholds](design-billing-thresholds.md)
  - [CC on billing emails](design-cc-emails.md)
  - [Create-preview endpoint](design-create-preview.md)
  - [Customer usage endpoint](design-customer-usage.md)
  - [Multi-dimensional meters](design-multi-dim-meters.md)
  - [Pricing recipes](design-recipes.md)
  - [Provider cost tables](design-cost-tables.md)
  - [Prepaid commits](design-prepaid-commits.md)
  - [Paid-commit credit-note relief](design-commit-cn-relief.md)

## Records

Dated documents. They describe the state on their date; the current truth
is in the code and the docs above.

- [Full-product audit, 2026-07-02](dev/audit-2026-07-02-full-product.md) and
  its [remediation plan](dev/audit-2026-07-02-remediation-plan.md)
- [HA readiness, 2026-07-06](dev/ha-readiness-2026-07-06.md)
- [Design review, 2026-07-10](dev/design-review-2026-07-10.md)
- [Product stability plan](dev/product-stability-plan.md) and
  [manual-test findings](dev/manual-test-findings.md)
- [CHANGELOG](../CHANGELOG.md)

## Glossary

Grouped by area. Each entry links to the doc that explains it in full.

### Accounts and access

- **tenant**: One business that uses a Velox install to bill its own customers. Tenants share the same tables; tenant-owned rows carry a tenant_id, and Postgres Row-Level Security keeps each tenant's data invisible to others. Dashboard users can belong to several tenants. [More](adr/003-postgresql-rls-multi-tenancy.md)
- **operator**: Has two meanings. Usually the business that runs Velox to bill its customers, or its staff who sign in to the dashboard. In self-host and ops docs, the engineer who deploys and runs the install. [More](self-host.md)
- **customer**: A person or company that a tenant bills. Each customer belongs to one tenant and has an external_id, the tenant's own ID for it, which usage events use to say whose usage it is. [More](api-surface.md)
- **test mode and live mode**: Two separate data sets inside one tenant, as in Stripe: test data for trying things safely, live for real billing. An API key's mode (the _test_ or _live_ in its prefix) or the dashboard's mode switch picks which set you see. Tenant settings are shared, but each mode has its own Stripe connection. [More](api-keys.md)
- **API key types (secret, publishable, platform)**: Three kinds of Bearer key. A secret key (vlx_secret_) has full access to one tenant from your server. A publishable key (vlx_pub_) is safe in a browser but reads no tenant data. A platform key (vlx_platform_) manages tenants and is not issued in-product. [More](api-keys.md)
- **RLS (Row-Level Security)**: A PostgreSQL feature Velox uses to isolate tenants in shared tables. A policy shows only rows matching the transaction's tenant_id, and the live or test mode on mode-aware tables, unless the transaction set the bypass flag. [More](adr/003-postgresql-rls-multi-tenancy.md)
- **TxTenant and TxBypass**: The two modes for opening a database transaction with db.BeginTx. TxTenant sets the tenant (and test or live mode) so RLS shows only that tenant's rows. TxBypass turns RLS off for cross-tenant work, such as the billing scheduler. [More](adr/003-postgresql-rls-multi-tenancy.md#decision)

### Pricing and usage

- **meter**: A named thing a tenant counts and charges for, such as tokens or API requests. Each meter has a unique key, a unit, an aggregation, and optionally a default rating rule. Usage events name the meter by its key in event_name. [More](design-multi-dim-meters.md)
- **usage event**: One record that a customer used something, sent to POST /v1/usage-events. It names the customer and the meter, gives a decimal quantity, and can carry dimensions and an idempotency key so a retried send is not counted twice. [More](api-surface.md)
- **dimensions (on a usage event)**: Labels on a usage event, such as model or token_type, as key-value pairs. Pricing rules match on them, so one meter can be priced at many rates. Up to 16 keys, scalar values only; stored in the properties column. [More](design-multi-dim-meters.md#subset-match-semantics)
- **pricing rule**: A meter rule that selects usage events whose dimensions contain every key-value pair in its dimension_match. It combines them into one quantity (sum, count, max or last value) and prices it with a rating rule. If rules overlap, the highest-priority rule claims each event, so nothing counts twice. [More](design-multi-dim-meters.md#priority--claim-semantics)
- **rating rule**: A versioned price formula, identified by its rule_key, that turns a usage quantity into money: a flat per-unit rate, graduated tiers, or fixed-size packages. Billing uses the active version that was in force when the period opened, so a new version takes effect next period. [More](adr/070-price-change-semantics.md)
- **plan**: A price package a tenant sells: a currency, a billing interval (monthly or yearly), a fixed base fee, and the meters it charges usage for. A subscription has one or more items, each pairing a plan with a quantity. Status: draft, active or archived. [More](../README.md#whats-in-the-box)
- **recipe**: A built-in pricing template (anthropic_style, openai_style, replicate_style) that one API call applies. It creates meters, rating rules, pricing rules and plans, sets the tenant's dunning policy, and adds a webhook endpoint that stays inactive until you give it a real URL. Applying the same recipe again changes nothing. [More](adr/085-recipe-idempotent-apply.md)
- **idempotency key**: A unique string that makes a retry safe: the same key means the same operation, so it takes effect once. The main kinds are the Idempotency-Key HTTP header, the per-event key on usage events, and the key sent to Stripe on each charge. [More](api-surface.md)

### Subscriptions and invoices

- **subscription**: A customer's agreement to pay for one or more plans over repeating billing periods. It holds items (each a plan plus a quantity) and has a status: draft, trialing, active, canceled or archived. [More](../README.md#stripe-grade-primitives-already-shipped)
- **billing period**: The date range one subscription invoice covers: start included, end excluded, with boundaries at midnight in the tenant's billing timezone. Monthly periods start on the 1st (calendar, after a first partial period) or the start day (anniversary). Yearly periods are always anniversary. [More](adr/012-day-grade-calendar-billing.md)
- **billing timezone**: The one organization-wide timezone (tenant setting, default UTC) in which Velox places period boundaries at midnight and counts days for proration. Each invoice also stores the zone it was issued under (invoices.billing_timezone) so its dates never shift later. [More](adr/077-org-level-billing-timezone.md)
- **day-grade billing**: Billing counts whole days, not hours. Normal period boundaries snap to midnight in the billing timezone, the start day counts in full, and day counts are rounded so daylight saving never adds or loses a day. Threshold resets can start periods mid-day. [More](adr/012-day-grade-calendar-billing.md)
- **bill timing (in advance / in arrears)**: A per-plan setting for when the fixed base fee is invoiced: in advance, at the start of the period it covers, or in arrears, at the end. Usage is always billed in arrears. The default is in arrears. [More](adr/031-per-plan-base-bill-timing.md)
- **proration**: Charging only for the whole days a price was active in a billing period. For plans billed at period start (in advance), a mid-period change charges the difference now, or credits it on a downgrade, and a short first period is prorated when billed. Plans billed at period end (in arrears) are prorated by days when the period closes. [More](adr/050-unpaid-source-proration-policy.md)
- **spend threshold**: A per-subscription trigger: a money amount (amount_gte) checked against the period's running subtotal, or a per-item usage count (usage_gte). Crossing it issues an early invoice with billing_reason "threshold" and can optionally restart the cycle. Usage is never blocked. [More](design-billing-thresholds.md)
- **invoice states (draft, finalized, paid, void, uncollectible)**: The status of an invoice. Draft: still editable. Finalized: issued and owed. Paid. Voided: cancelled, as if the debt never existed (the code stores void as voided). Uncollectible: written off as bad debt but kept for audit. [More](adr/110-written-off-invoices-close-self-service-payment.md)
- **hosted invoice page**: A public web page for one issued (non-draft) invoice, opened by a secret token in its link, so the end customer needs no login. It shows the invoice, offers the PDF, and, while money is still owed, has a Pay button that opens Stripe Checkout. [More](adr/021-hosted-invoice-pay-flow-saves-pm.md)

### Credits

- **credit grant / credit block**: A credit grant adds credit to a customer's append-only ledger, for example a prepaid commit, promotional credit, goodwill, or credit from a proration or credit note. Every positive entry is a block that invoices draw down when billed: promotional first, then soonest-expiring, then oldest. [More](adr/071-credit-expiry-retires-the-block.md)
- **prepaid commit**: An up-front purchase of usage credit, sold as a line on a manual invoice. When that invoice finalizes, Velox adds a credit block to the customer's balance. The credit can exceed the price (pay $9k, get $10k). [More](design-prepaid-commits.md#why-wedge-fit)
- **drawdown**: Applying a customer's credit balance to an invoice when it is billed, reducing what is charged. Free promotional credit is used before paid credit; within each, soonest-expiring credit goes first. A commit's own purchase invoice can never be paid with credit. [More](design-prepaid-commits.md#d7-drawdown-order-null-safe)
- **credit note (CN)**: A formal document that reduces what a customer owes on an invoice. On an unpaid invoice it lowers the amount due. On a paid one, its total is split between a refund, credit to the customer's balance, and money returned outside Velox (out of band). States: draft, issued, voided. [More](adr/038-credit-note-three-channel-allocation.md)
- **clawback**: Two meanings. In subscriptions: a paid, in-advance period is cut short (cancel, downgrade, item removal or plan swap). Velox then issues a credit note that adds the unused amount, tax included, to the customer's credit balance and reverses that tax. In the credit ledger: a negative adjustment that removes granted credit. [More](adr/048-credit-clawback-tax-reversal.md)

### Payments and collection

- **PaymentIntent**: Stripe's object for one card charge attempt. Velox charges only through PaymentIntents, not Stripe Billing, so Velox owns the invoice. An invoice stores its PaymentIntent id; an offline payment stores an out_of_band: marker instead. [More](adr/001-paymentintent-only-stripe.md)
- **payment method (PM)**: A customer's saved card. Stripe holds the card; Velox's payment_methods table keeps its Stripe id and display details, and a customer may have several. Velox marks one as default and names that exact card on every automatic charge; Stripe never chooses. [More](adr/053-explicit-payment-method-at-charge.md)
- **unknown payment status**: An invoice payment status meaning a charge attempt got an unclear answer (timeout, server error, dropped connection), so Velox cannot tell if money moved. Nothing retries it. A webhook or Velox's periodic Stripe check resolves it; with no PaymentIntent id recorded, it is parked. [More](adr/107-unknown-is-terminal-until-a-human.md)
- **parked invoice**: An invoice whose charge result is unclear (payment status unknown) with no PaymentIntent id recorded, so Velox cannot tell if the customer paid. Nothing charges it. It exits when a Stripe webhook or Velox's Stripe search finds the charge, or an operator writes it off. [More](adr/108-parked-invoices-search-and-adopt.md)
- **dunning**: Velox's automatic collection process for an unpaid invoice, started by a failed charge or a missing payment method. It retries on a schedule and sends reminders. When retries run out, the policy can pause or cancel the subscription, write off the invoice, both, or neither. [More](adr/036-dunning-campaigns-model.md)
- **dunning run**: One dunning process for one unpaid invoice, stored in invoice_dunning_runs (at most one per invoice). It records the policy, attempt count, next action time, a paused flag, and a state: active, resolved or escalated, plus a resolution once closed. [More](adr/064-dunning-run-creation-derived-from-invoice-state.md)
- **write-off**: Marking an invoice uncollectible: Velox stops collecting it but keeps it in the records. Nothing charges it again and its tax is not reversed, but an offline payment can still be recorded. An operator or dunning's final action sets it. [More](adr/110-written-off-invoices-close-self-service-payment.md)

### Costs and margin

- **cost table**: A per-tenant list, kept separately for live and test mode (table provider_cost_rates), of what the operator pays an LLM provider per token, by provider, model and token type. At ingest Velox uses it to stamp an event's cost unless the sender supplied an observed cost. [More](design-cost-tables.md)
- **COGS and margin**: COGS (cost of goods sold) is what the operator pays providers for a customer's usage, stamped on each event at ingest from an observed cost or the cost table. Margin is rated usage revenue minus that cost, as a share of revenue, per customer over a window, shown only to operators. [More](adr/079-provider-cost-tables.md)
- **cost dashboard**: A view of one customer's current-period usage, charges and projected bill (never the operator's provider cost). It comes in two forms: a card on the operator's customer page, and a public read-only JSON endpoint, opened with a show-once secret token, that partners can embed. [More](adr/032-public-cost-dashboard-projection.md)

### Testing with simulated time

- **test clock**: A test-mode object that holds a fake current time (frozen_time). An operator attaches a customer to it, then moves the time forward to see renewals, trial ends, invoices and dunning happen in minutes instead of months. [More](adr/027-customer-level-test-clock.md)
- **catch-up (catchup)**: The background work that runs after a test clock moves forward. It runs every trial end, billing period, threshold, retry, dunning step and credit expiry that fell in the skipped time. The clock then returns to ready, or to internal_failure if any step failed. [More](adr/015-test-clock-async-catchup.md)
- **clock-pinned**: Describes a customer, and everything it owns (subscriptions, invoices, credits), that is attached to a test clock. Its time comes from that clock, not the real wall clock, and the normal billing scheduler skips it. [More](adr/030-simulated-time-everywhere-on-clock-pinned-entities.md)
- **simulated time**: The fake current time of a test clock. Velox uses it for every business date on a clock-pinned record (periods, due dates, paid dates) and marks such invoices and credit notes is_simulated, shown in the UI as "Simulated". [More](adr/030-simulated-time-everywhere-on-clock-pinned-entities.md)

### Running Velox

- **outbox**: A database table of messages waiting to be sent (webhook_outbox, email_outbox). Most webhook events and the four money emails (payment receipt, payment failed, dunning warning, dunning escalation) are queued inside the transaction that caused them; the rest right after it commits. A background dispatcher then delivers each row with retries. [More](adr/040-outbox-always-on.md)
- **leader lease**: A row in the leader_leases table that lets exactly one replica run one tick of a cluster-wide job (billing, dunning, both outbox dispatchers, webhook delivery). It is renewed during the tick, released after it, and expires 10 seconds after the last renewal. [More](adr/114-leader-leases-tick-scoped-fencing.md)
- **fencing token**: A per-job number in leader_leases (holder_token) that goes up every time a replica acquires the lease. The five queries that claim due work check it with leader_fence, so a stale or frozen former leader claims nothing. [More](adr/114-leader-leases-tick-scoped-fencing.md#fencing-boundary-the-five-claim-funnels)
- **velox-doctor**: A read-only command that scans a Velox database for states no correct code could produce, such as invoice totals that do not add up. It exits 1 on violations, or 2 if a check cannot run, runs in CI, and needs a database role that can see every row. [More](dev/product-stability-plan.md)

### Project terms

- **design partner (DP)**: An early customer who works with the team to shape the product. In Velox docs, many features are deferred until a design partner asks for them, for example RBAC or Helm charts. [More](../README.md#explicitly-deferred-on-hold-pending-design-partner)
- **site-set (and effect-firer, gated reader)**: Every code place that touches one state, such as an invoice status. That means each writer and each effect-firer (code that sends an email, webhook, Stripe call or ledger write on a change). It also means each gated reader (code that branches on the state), each caller and callee, and each crash point. [More](dev/money-path-robustness-playbook.md#2-the-meta-practice-complete-site-set-enumeration)
- **FLOW (in MANUAL_TEST)**: One named section of MANUAL_TEST.md that a person runs by hand to prove a product feature works end to end. Each flow has a stable ID (for example FLOW S1) and checkbox steps, each with one observable result. [More](../MANUAL_TEST.md#flow-index)
