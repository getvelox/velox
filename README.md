# Velox

### Meter every token. Sell commits. Know your margin.

**The open-source billing engine for AI and usage-heavy SaaS — runs in your own VPC.**

[![CI](https://github.com/sagarsuperuser/velox/actions/workflows/ci.yml/badge.svg)](https://github.com/sagarsuperuser/velox/actions/workflows/ci.yml)
[![Go](https://img.shields.io/badge/Go-1.25-00ADD8?logo=go)](https://go.dev)
[![License](https://img.shields.io/badge/License-MIT-green)](LICENSE)
[![Release](https://img.shields.io/github/v/tag/sagarsuperuser/velox?label=release)](CHANGELOG.md)

**Pre-1.0.** The public API is stabilising but not yet frozen — breaking changes land on MINOR until 1.0.0 ([versioning policy](CHANGELOG.md)).

Velox owns the layer above PaymentIntent: pricing, subscriptions, multi-dimensional usage metering, invoicing, dunning, and credits. Stripe still does cards under the hood. The 0.5% Stripe Billing fee disappears, and customer billing data never leaves your infrastructure.

Built for three market truths Stripe Billing structurally cannot serve:

1. **AI/LLM apps need multi-dimensional pricing.** Real model pricing today is `model × token_type × tier × context-window`, where `token_type` is a disjoint role (`input`, `output`, `cache_read`, `cache_write_5m`, `cache_write_1h`). Stripe's Meter API forces one Meter per dimension combination — many Meters and ugly subscription wiring just to model a single model's token roles. Velox treats dimensions as first-class on the meter — and ingests straight from a LiteLLM proxy spend callback, no SDK.
2. **AI infra sells commit + usage.** "$10k prepaid commit, drawn down against metered usage" is the default AI-infra contract. Stripe Billing has no commit primitive; the engines that do (Orb, Metronome) are closed-source SaaS. In Velox a commit line on an invoice funds a credit block at finalize (atomic, fund-once); usage drains it — promotional credits first — with `credit.balance_low` / `_depleted` / `_recovered` webhooks to drive top-up nudges.
3. **Regulated tenants cannot put customer billing data on Stripe's servers.** EU GDPR-strict, India RBI data localization, healthcare-adjacent SaaS, government procurement. Stripe's whole model is "send us the data." Velox runs in your VPC.

---

## The wedge in code

Bill Anthropic-style multi-dimensional pricing (model × token_type) with **one meter**, sell a prepaid commit against it, and see per-customer margin — five steps, no Stripe Billing objects. (API from `make dev`, key from `make bootstrap` — see Quick start.)

```bash
# 1. Create one meter for "tokens"
curl -X POST http://localhost:8080/v1/meters \
  -H "Authorization: Bearer $VELOX_SECRET" \
  -d '{"key": "tokens", "name": "LLM tokens", "unit": "token"}'

# 2. Ingest events that carry the dimensions inline
curl -X POST http://localhost:8080/v1/usage-events \
  -H "Authorization: Bearer $VELOX_SECRET" \
  -H "Idempotency-Key: req_8f2c..." \
  -d '{
    "event_name": "tokens",
    "external_customer_id": "cust_acme",
    "quantity": "12450",
    "dimensions": {"model": "gpt-4", "token_type": "input"}
  }'

# 3. Define one pricing rule per (dimension subset, rate)
curl -X POST http://localhost:8080/v1/meters/$METER_ID/pricing-rules \
  -H "Authorization: Bearer $VELOX_SECRET" \
  -d '{
    "dimension_match": {"model": "gpt-4", "token_type": "input"},
    "rating_rule_version_id": "rrv_gpt4_input",
    "aggregation_mode": "sum",
    "priority": 100
  }'

# 4. Sell a $10k prepaid commit for $9k — the credit block funds when the
#    invoice finalizes, and usage draws it down
#    ($INVOICE_ID: a draft one-off invoice from POST /v1/invoices — elided for brevity)
curl -X POST http://localhost:8080/v1/invoices/$INVOICE_ID/line-items \
  -H "Authorization: Bearer $VELOX_SECRET" \
  -d '{"description": "Annual commit", "line_type": "add_on",
       "quantity": 1, "unit_amount_cents": 900000,
       "commit_granted_cents": 1000000}'
curl -X POST http://localhost:8080/v1/invoices/$INVOICE_ID/finalize \
  -H "Authorization: Bearer $VELOX_SECRET"

# 5. Know which customers lose you money — stamped provider COGS vs rated revenue
curl http://localhost:8080/v1/customers/$CUSTOMER_ID/margin \
  -H "Authorization: Bearer $VELOX_SECRET"
```

Already running a LiteLLM proxy? Skip step 2 — point its spend callback at `POST /v1/integrations/litellm/spend` and every completion lands as dimensioned token events (`model`, `token_type`), replay-deduped, no SDK. See [`docs/integrations/litellm.md`](docs/integrations/litellm.md).

What lands on the invoice (from `./scripts/demo.sh`, real output):

```text
ACME Corp — VLX-000001                                    $3.88
──────────────────────────────────────────────────────────────
Tokens (claude-sonnet-4.5 · input)       400,000    →   $1.20
Tokens (claude-sonnet-4.5 · output)      175,000    →   $2.62
Tokens (claude-sonnet-4.5 · cache_read)  200,000    →   $0.06
──────────────────────────────────────────────────────────────
Margin (billed $3.88 vs provider cost $1.28)            67.1%
```

Token roles are disjoint, so each `{model, token_type}` is exactly one rule at equal priority — no double-count. A coarse catch-all (`{"model": "gpt-4"}`) and finer per-role rules (`{"model": "gpt-4", "token_type": "cache_read"}`) still compose cleanly via the priority+claim resolver. The full design — schema, aggregation semantics, decimal quantities, all five aggregation modes (`sum`, `count`, `last_during_period`, `last_ever`, `max`) — lives in [`docs/design-multi-dim-meters.md`](docs/design-multi-dim-meters.md).

---

## What's in the box

### AI/usage-native (the wedge)
- **Multi-dimensional meters** — one meter, N rules, dimensions on every event
- **LiteLLM drop-in** — point your proxy's spend callback at `POST /v1/integrations/litellm/spend`; calls become dimensioned token events (`model`, `token_type`) with idempotent replay dedupe — no SDK, no schema work
- **Prepaid commits + drawdown** — sell "pay $9k, get $10k" commits as invoice lines; the credit block funds atomically at finalize and usage draws it down, promotional credits first (ADR-078)
- **Per-customer margin (COGS) in-app** — maintain your provider rates once (`/v1/provider-costs`); every usage event is stamped with provider cost at ingest; `GET /v1/customers/{id}/margin` answers "which customers lose us money?" — per-model where pricing rules pin `model`, honest `unattributed_revenue` bucket otherwise (ADR-079)
- **Decimal quantities & rates** — `NUMERIC(38, 12)` for fractional GPU-hours and partial tokens; decimal per-unit prices so $3.00 / 1M tokens bills linearly and exactly (Stripe `unit_amount_decimal` model) while invoice totals stay whole cents
- **Per-rule aggregation modes** — `sum`, `count`, `last_during_period`, `last_ever`, `max`
- **Pricing recipes** — one call instantiates products + prices + meters + dunning (`anthropic_style`, `openai_style`, `replicate_style`)
- **Customer cost visibility** — per-customer usage/cost breakdown in the dashboard, plus a token-authenticated public JSON endpoint (`/v1/public/cost-dashboard/{token}`, rotatable per-customer token) your app can render — "$4.31 of GPT-4 today" with a projected bill. A packaged embeddable widget is roadmap, not shipped.

### Self-host first
- **Docker Compose** — single-VM install, ~5 min from clone to invoice
- **Data sovereignty** — customer billing data never leaves your infrastructure
- **Append-only audit log** — DB-trigger enforced tamper-evidence
- **Row-Level Security** — one Velox deployment cleanly serves N internal tenants

### Stripe-grade primitives (already shipped)
- **Subscriptions** — trial state machine with atomic flips · pause-collection · scheduled cancellation · plan changes with proration
- **Pricing & discounts** — per-customer price overrides · credit ledger (prepaid allowances, drawn against usage). Coupons cut pre-launch (ADR-039) — AI-native peers converge on credits, not promo codes.
- **Invoicing & collection** — PDF invoices · hosted invoice page with secure tokens · branded multipart emails · dunning with breaker · invoice preview (`Invoice.upcoming` parity)
- **Spend controls** — hard-cap thresholds with mid-cycle finalize
- **Credits & refunds** — event-sourced credit ledger · credit notes + refunds
- **Reliability** — idempotency · transactional outbox · webhook signing with 72h rotation grace · test clocks

See [`CHANGELOG.md`](CHANGELOG.md) for the full ship log.

---

## How it fits

|                          | **Velox** | Stripe Billing | Lago        | Orb / Metronome¹  | OpenMeter²        |
|--------------------------|-----------|----------------|-------------|-------------------|-------------------|
| OSS / self-host          | ✅        | ❌             | ✅          | ❌                | ✅                |
| AI-native pricing        | ✅        | ❌             | ⚠️ generic  | ⚠️ closed source  | ⚠️ metering-first |
| Full billing engine      | ✅        | ✅             | ✅          | ✅                | ⚠️ emerging       |
| Stripe-grade primitives  | ✅        | ✅             | ⚠️          | ✅                | ⚠️                |
| Prepaid commits + drawdown | ✅      | ❌             | ⚠️ wallets  | ✅                | ❌                |
| Per-customer margin (COGS) | ✅ in-app | ❌           | ❌          | ❌ warehouse join | ❌                |
| Pricing                  | OSS       | 0.5% of GMV    | OSS / cloud | $30K+/yr          | OSS / cloud       |
| Data sovereignty         | ✅        | ❌             | ⚠️          | ❌                | ✅                |

¹ Metronome was acquired by Stripe (Jan 2026) — still SaaS-only, so your billing data lives on Stripe's servers either way.
² OpenMeter (acquired by Kong, Sep 2025) is expanding from metering into billing — closing the engine gap, but not the self-host-with-Stripe-grade-depth one.

Velox lives in the empty cell: **OSS + self-host + AI-native + full billing engine.** Decision tree: pick **Stripe Billing** (or Stripe + Metronome) for hosted SaaS billing; pick **Lago** for generic OSS billing without an AI-shaped wedge; pick **Orb/Metronome** if you can't self-host and budget for usage-based contracts; pick **Velox** when you need AI-native billing that runs in your own VPC.

---

## What Velox is **not**

Stating these loudly so the wrong customers self-select out:

- **Not for vanilla card-first SaaS** with simple per-seat pricing — Stripe Billing is fine for them.
- **Not multi-PSP yet** — Stripe is the only payment processor. Razorpay/Adyen come when a paying tenant asks.
- **Not for marketplaces or Stripe Connect** — Velox bills the tenant's customers directly; it doesn't split payouts across sub-merchants. (Velox itself is multi-tenant via RLS — many billing tenants per deployment — but each tenant collects on its own behalf, not on behalf of others.)
- **No Revenue Recognition / Sigma** — bring your own warehouse + dbt.
- **No Quotes or Subscription Schedules** — sales-led contract billing should pick Recurly or Maxio.
- **No 50+ payment-method types** — cards via Stripe + send-invoice. ACH/SEPA expand from there.

---

## Quick start

Prereqs: Docker, Go 1.25+, Node 20+ (dashboard), `jq` (demo script).

```bash
git clone https://github.com/sagarsuperuser/velox.git && cd velox

# Backend — Postgres + bootstrap demo tenant + operator user + API keys
cp .env.example .env # make dev reads it; the defaults work for local dev as-is
docker compose up -d postgres
make bootstrap       # prints operator email + password + secret-test, secret-live, publishable-test keys
make dev             # API on :8080

# Operator dashboard (separate terminal)
cd web-v2 && npm install && npm run dev
# → http://localhost:5173 — sign in with the email + password from bootstrap
```

End-to-end demo — the whole wedge in ~30 seconds (Anthropic-style price matrix via one recipe call, LiteLLM-shaped token ingest, provider cost rates, a **test clock** that simulates a full billing month, a finalized invoice with per-`(model, token_type)` lines + PDF, and the per-customer margin report):

```bash
./scripts/demo.sh <vlx_secret_test_... from make bootstrap>
```

Every call in the script is checked — it fails loudly at the first API mismatch instead of pretending. Rerun it as often as you like (each run creates a fresh demo customer on its own test clock).

Self-host: single-VM Docker Compose. See [`docs/self-host.md`](docs/self-host.md). Helm/Terraform/multi-replica HA paths land when a design partner names which Kubernetes flavour they actually run — pre-emptively shipping three deployment shapes produced surface nobody was running.

---

## Architecture

```
cmd/velox/                  — single Go binary
internal/
  domain/                   — pure domain models, zero deps
  api/respond/              — Stripe-style JSON responses
  auth/                     — API key auth (3 key types, 17 permissions)
  tenant/                   — tenants + settings
  customer/                 — customer CRUD + billing profiles
  pricing/                  — meters, rating rules, plans, price overrides
  subscription/             — lifecycle (draft → trialing → active → paused → canceled)
  usage/                    — event ingestion + multi-dim aggregation
  invoice/                  — state machine (draft → finalized → paid) + PDF
  billing/                  — billing engine + scheduler + preview
  payment/                  — Stripe PaymentIntent + webhook receiver
  dunning/                  — payment retry state machine
  credit/                   — event-sourced prepaid balance ledger
  creditnote/               — credit notes + refunds
  webhook/                  — outbound webhooks (HMAC-signed delivery)
  audit/                    — immutable append-only audit log
  platform/postgres/        — RLS-aware database layer
  platform/migrate/         — embedded SQL migrations

web-v2/                     — operator dashboard (React 19 + TypeScript + Tailwind)
```

Design rules:
- **Per-domain packages** — each domain owns its store, service, and handler. No peer domain touches another's service or store; the imports that do cross domains — cross-cutting infra (auth, audit, session), shared value types, the billing coordinator's narrow interfaces — are pinned edge-by-edge in an allowlist.
- **Row-Level Security** — every tenant-scoped query runs inside an RLS-enforced transaction. Proven by integration tests.
- **PaymentIntent-only Stripe** — no Stripe Billing/Invoices. We own invoices end-to-end; Stripe executes the card charge.
- **Billing engine as coordinator** — orchestrates across domains via narrow interfaces, not a god object.
- **Append-only event sourcing for money** — credits, audit log, outbound webhook outbox.

ADRs explaining the load-bearing decisions live in [`docs/adr/`](docs/adr/).

---

## Engineering

Velox moves money, so correctness is the product, not a feature. The disciplines that show up in the code:

- **Money-path changes follow a written protocol, not judgment.** Any change to an invoice, payment, credit, or state machine goes through the [money-path robustness playbook](docs/dev/money-path-robustness-playbook.md): enumerate the state's *complete* site-set — every writer, effect-firer, gated reader, crash point — before writing a line, because local reasoning is exactly how money bugs ship. Ten named failure classes, each with the gate that closes it.
- **Invariants are enforced by machines, not convention.** Tenant isolation is Postgres Row-Level Security, proven by integration tests that fail if a query escapes its tenant. Exactly-once auto-charge is a compare-and-swap claim that holds through a dual-leader failover — no double charge when a Postgres failover hands two schedulers the same lock. A new cross-domain import fails an [architecture test](internal/arch/boundaries_test.go) until it's justified in an allowlist. `time.Now()` on a clock-pinned entity fails a lint. The rule: if a mistake can recur, a machine catches the next one.
- **The database is never mocked.** Every test that touches a database touches real Postgres — ~90k lines of Go test code against ~88k of production Go — so a green suite means migrations, RLS, and the money math work end-to-end, including concurrent-claimer collision tests and mutation-verified assertions (break the logic on purpose; the test must fail).
- **Decisions are written down — including the reversals.** [80+ ADRs](docs/adr/) record the load-bearing calls, and the honest ones: a per-subscription timezone snapshot was built, shipped, then *deleted* once org-level proved the complete abstraction (ADR-074 → 077). When a design keeps spawning guard machinery, the model is treated as wrong, not the guards as missing.
- **Audited like production, pre-launch.** A [117-finding end-to-end audit](docs/dev/audit-2026-07-02-full-product.md) remediated in gated PRs; an [N=2 HA-readiness audit](docs/dev/ha-readiness-2026-07-06.md) that names every single-point-of-failure before the word "production" gets used.

---

## API surface (selected)

```
POST   /v1/usage-events                 — ingest with dimensions + decimal value
POST   /v1/usage-events/batch           — batch ingest, up to 1000 per call
POST   /v1/meters/{id}/pricing-rules    — add a dimension-matched pricing rule
GET    /v1/customers/{id}/usage         — period aggregation, grouped by dimension

POST   /v1/customers                    — create customer
POST   /v1/subscriptions                — create subscription
PATCH  /v1/subscriptions/{id}/items/{itemID}     — plan/quantity change (proration on immediate)
PUT    /v1/subscriptions/{id}/pause-collection   — keep cycle, invoice as draft
POST   /v1/subscriptions/{id}/extend-trial       — push trial_end_at later

POST   /v1/billing/run                  — finalize all due cycles
GET    /v1/billing/preview/{sub_id}     — invoice preview (dry run)

POST   /v1/credit-notes                 — issue credit note (credit or refund)
POST   /v1/credits/grant                — grant prepaid credits to a customer
GET    /v1/credits/balance/{customer_id} — current balance + ledger

GET    /v1/dunning/runs                 — list dunning runs
POST   /v1/webhook-endpoints/endpoints  — register an outbound webhook endpoint
GET    /v1/audit-log                    — query the append-only audit log
```

API reference: [`api/openapi.yaml`](api/openapi.yaml) covers the core resource routes (usage, subscriptions, invoices, pricing, credits, provider costs, webhook endpoints); some operational routes (exports, analytics, audit log, settings) aren't in the spec yet. Webhook consumers: [`docs/webhooks.md`](docs/webhooks.md) — signature verification, envelope, retry ladder, event catalog, delivery contract.

---

## API key types

| Type        | Prefix            | What it can do                                           |
|-------------|-------------------|----------------------------------------------------------|
| Platform    | `vlx_platform_`   | Tenant management only                                   |
| Secret      | `vlx_secret_`     | Full tenant access (server-side)                         |
| Publishable | `vlx_pub_`        | Authenticate-only — no tenant data access (browser-safe) |

API keys are salted-SHA-256 hashed at rest; rotation supports an optional grace window (immediate by default, up to 7 days) so in-flight requests keep authenticating while the old key winds down. (The 72-hour overlap window is Stripe's webhook-signing-secret pattern, which Velox mirrors for outbound webhook secrets — not API keys.)

---

## Roadmap

### Recently shipped

- **Ambiguous-charge safety, end to end (Jul-Aug 2026, ADR-105-108)** — a lost Stripe response can no longer double-charge: the idempotency key is seeded by an attempt counter (not `updated_at`), an unidentifiable outcome parks the invoice instead of guessing, every surface tells the truth about it (banner, hosted page, collections, dunning, clawbacks, a gauge), and the reconciler now searches Stripe by metadata and adopts the attempt when the provider can find it — positive evidence may write money; absence only ever writes an observation
- **Team invites (Jul 2026, ADR-081)** — invite teammates by email (tokenized single-use accept links, member removal with session revocation); kills the shared-password reality and gives the audit log real per-person actors. No RBAC yet — every member has full access, roles recorded for the future split
- **Provider cost tables + margin (Jul 2026, ADR-079)** — enter what you pay LLM providers; every usage event is stamped with its COGS at ingest; per-customer margin report (billed vs cost by model) — the report every other billing engine makes you build in your warehouse
- **Prepaid commits + drawdown (Jul 2026, ADR-078)** — sell commit + usage: a commit line on an invoice funds a credit block at finalize (fund-once, atomic), promotional credits drain before paid, balance-threshold webhooks (`credit.balance_low/_depleted/_recovered`), void retires the unfunded remainder
- **Full-product audit hardening (Jul 2026)** — a 117-finding end-to-end audit remediated in 16 gated PRs: threshold billing exactness, checkout/trial state-machine races, credit expiry atomicity, price-change semantics (pinned per-period resolution), transport delivery leases (no more duplicate emails/webhooks under load), self-host truth (one bootstrap, real `APP_DATABASE_URL`, race-safe migrations), and honest MRR analytics
- **Operator search that actually searches (Jun 2026)** — server-side `?search=` on customers (matches encrypted name/email post-decryption), invoices, and subscriptions; invoice date-range + past-due filters; ⌘K palette queries the full dataset

See [`CHANGELOG.md`](CHANGELOG.md) for the full ship log.

### Explicitly deferred (on hold pending design partner)

- Per-million rate display for token meters (`$3.00 / 1M tokens`) — the last residual of the AI-native rate surfacing arc; the per-(model, token-type) split already ships on dashboard + invoices (ADR-044/045/054)
- Helm chart + Terraform AWS module + multi-replica HA
- Stripe Billing migration tool (`velox-import`)
- SOC 2 / GDPR-deletion / audit-log retention / encryption-at-rest enterprise-readiness docs
- RBAC / role enforcement (invites shipped Jul 2026 per ADR-081 with every member holding full access; role-scoped permissions land when a DP names the split. SSO direction predetermined per ADR-014 — embedded OIDC/SAML libs in-process when a DP asks, never a SaaS auth vendor)
- Operator polish: bulk actions, billing-alerts UI, plan-migration cohort UI, live event stream, embedded dashboard docs site

These are paused — not killed. They land when a real customer names the specific shape they need; pre-launch, pre-evidence builds optimise the wrong version of each.

---

## Tech stack

**Backend** — Go 1.25, chi/v5 router, PostgreSQL 16 with RLS, `shopspring/decimal` for money, `signintech/gopdf` for invoices, Prometheus metrics.

**Frontend** — React 19, TypeScript, Vite, TailwindCSS, shadcn/ui, Lucide icons.

**Payments** — Stripe (PaymentIntents + Checkout Sessions). No Stripe Billing dependency.

---

## Running tests

```bash
make test                # unit tests only
make test-integration    # full integration suite (needs Postgres)
```

Integration tests exercise real Postgres with RLS enforced. Per project convention the database is never mocked — no sqlmock, no mock framework in the repo — so a passing suite means migrations and RLS work end-to-end.

---

## Contributing

Velox is open source under MIT. Contributions welcome — see [`CONTRIBUTING.md`](CONTRIBUTING.md). Major features land with a design RFC alongside the code, so the reasoning is reviewable before the implementation is; read any `docs/design-*.md` or the [ADRs](docs/adr/) for the pattern.

Running AI inference, a vector DB, or usage-heavy SaaS, and Stripe Billing is starting to chafe? Open an issue — happy to help you get a self-hosted deployment going.

---

## License

[MIT](LICENSE)
