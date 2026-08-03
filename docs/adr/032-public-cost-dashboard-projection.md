# ADR-032: Public cost-dashboard projection — token shape + sanitization contract

**Status:** Accepted
**Date:** 2026-05-14
**Related**: ADR-021 (hosted-invoice public token shape), ADR-031 (per-plan bill_timing)

## Context

The cost-dashboard was half-built: the operator-facing surface (`GET /v1/customers/{id}/usage` + `<CostDashboard>` React component) shipped earlier; the **public** surface for partners to embed in their own apps did not. The token column + Customer model field landed in migration 0064 but no public route, rotation endpoint, or sanitized projection existed.

This is wedge-relevant work — cost visibility *is* the AI-native pitch — and demo-blocking. MANUAL_TEST FLOW CU8 documented the expected shape; the code lagged.

## Decision

Ship a single sanitized JSON endpoint for the public projection. Defer the embeddable React widget until a real design partner asks for it (the JSON is consumer-ready; partners can render their own UI today).

### Token

- Format: `vlx_pcd_` + 64 hex (32 bytes entropy). Same shape as the hosted-invoice public token (ADR-021).
- Persisted plaintext in `customers.cost_dashboard_token` with a partial UNIQUE index.
- Rotation invalidates the previous token IMMEDIATELY — no grace window. Read-only surface; the rotation intent is "stop the previous URL right now."
- Audit log entry on rotation (`action=rotate`, `metadata.surface=cost_dashboard_token`). Plaintext token NEVER in the audit row.

### Lookup

- Token lookup uses RLS-bypass (`TxBypass`). The token IS the credential — no tenant context yet, and the returned customer's `tenant_id` is what scopes everything downstream.
- 401 envelope identical for invalid / never-existed / rotated tokens (anti-enumeration). Same shape Velox uses for revoked API keys.
- Prefix-mismatch fast-path: any token not starting with `vlx_pcd_` → 401 without DB lookup.

### Sanitization contract

The projection is built by composing the existing `CustomerUsageService.Get` (same math the operator dashboard uses → dashboard math == public math) then stripping PII at the assembler boundary.

| Field | Public? | Why |
|---|---|---|
| customer_id, tenant_id | yes | Caller has the token; these are not secrets. |
| billing_period { start, end, source } | yes | Cycle-bound context. `source` is `"subscription"` or `"no_subscription"`. |
| subscriptions[].{id, plan_name, currency, period} | yes | What's running and when the cycle closes. |
| usage[].{meter_key, meter_name, unit, currency, totals, rules[]} | yes | Cost attribution — the whole point. |
| totals[], projected_total_cents | yes | Headline number partners surface to their customer. |
| **email** | NO | PII |
| **display_name** | NO | PII |
| **external_id** | NO | Caller chose this id privately |
| **metadata** | NO | Caller-controlled — could carry anything |
| **billing_profile** | NO | Legal name, address, tax_id |
| **warnings** | NO | Operator-facing tech messages |
| **plan_id**, **rating_rule_version_id** | NO | Internal IDs, not useful to the partner |

The mapping happens in `internal/usage/cost_dashboard.go::CostDashboardAssembler.GetByToken`. Adding a new field to the operator-facing `CustomerUsageResult` does NOT automatically leak it to the public surface — the assembler walks an explicit allowlist of fields. Future PII fields stay private by default.

### Empty state

No active subscription → 200 with `billing_period.source = "no_subscription"` and empty arrays. Not a 404, not a 5xx — the partner widget renders a clean empty state rather than handling an error code.

### Rate limit

Mounted under the existing `hostedInvoiceRL` 60/min/IP bucket. Tighter than the general 100/min for the same reason hosted invoice tightened: payment-adjacent surfaces are higher-value targets, and the widget may poll.

## Consequences

**Positive:**
- Velox can now demo the AI-native cost-visibility pitch end-to-end. Partner embeds the JSON into their own app; rotation gives the operator a kill switch.
- Sanitization is explicit (allowlist, not denylist) — future operator-facing additions don't leak by accident.
- Same token + rotation shape as hosted invoice (ADR-021) — one mental model for operators.

**Negative:**
- No widget. Partners build their own renderer or wait for the React component to ship. Tradeoff: shipping the JSON now unblocks the demo; the widget is uncertain UX work that benefits from real DP feedback before locking in.
- Token plaintext in the DB. Same tradeoff hosted invoice made; 256-bit entropy makes brute-force infeasible and the read-only surface limits blast radius if the column leaks.

## Industry reference

> **Corrected 2026-08-03 (FLOW CU8 parity check).** This section previously
> claimed "Lago / Orb / Metronome: none ship a partner-embeddable cost
> dashboard out of the box. Velox is shipping ahead of the peer set here."
> **That is false, and it was the line carrying the wedge argument.** All three
> ship exactly that, two of them as literally an embeddable iframe URL. The
> re-checked findings, each verified against official documentation, are below.
> The *decision* still stands — see why under "Verdict" — but it stands on
> different grounds than "we're ahead."

- **Stripe** — API only, no end-customer usage page. The customer portal covers
  payment details, subscriptions and invoice history ("Pay, download, and view
  current and past invoices"), but shows no current-period usage or spend
  ([customer-management](https://docs.stripe.com/customer-management)). For usage
  it ships data, not pixels: *"Use the Meter Usage Analytics API to query and
  analyse your customers' meter usage data. This enables you to build custom
  usage dashboards"*
  ([usage-based/analytics](https://docs.stripe.com/billing/subscriptions/usage-based/analytics)).
- **Metronome** — embeddable widget **and** API. `POST /v1/dashboards/getEmbeddableUrl`
  returns a URL to display *"through an iframe within your internal billing UI"*,
  with a `color_overrides` array to match the host design; dashboards are
  `usage` (30/60/90 days), `invoices` (90 days of history) and
  `commits_and_credits`
  ([customer-dashboards](https://docs.metronome.com/guides/customers-billing/optimize-customer-experience/customer-dashboards-and-reporting)).
- **Orb** — hosted page, embeddable, and API. *"Orb generates a signed, expiring
  URL to an invoice portal and a signed, non-expiring URL to a customer portal…
  These portals can be directly accessed by your customers"*, showing invoices
  plus current usage ([invoice-portal](https://docs.withorb.com/invoicing/invoice-portal)).
  Orb also documents the build-your-own path as first-class, for Velox's exact
  use case: *"Retrieving usage can be especially useful to power in-product
  visibility for your end customers"*
  ([usage-visibility](https://docs.withorb.com/product-catalog/usage-visibility)).
- **Lago** — customer portal with an embeddable iframe
  (`/customers/:external_customer_id/portal_url`, token expires after 12 hours),
  showing *"a detailed usage report, showing the number of units consumed and
  associated costs"* ([customer-portal](https://docs.getlago.com/guide/customers/customer-portal)).
  Listed on the pricing page under **Premium only**, not the open-source tier
  ([pricing](https://getlago.com/pricing)) — so for a self-hosted OSS comparison
  the gap narrows, though we did not verify license enforcement in their code.
- **What an AI company's customers already expect**, as the ICP reference point:
  OpenAI ships a console usage dashboard with per-project breakdowns, spend
  categories, CSV export and 1-minute granularity
  ([usage dashboard](https://help.openai.com/en/articles/10478918-api-usage-dashboard));
  Anthropic ships Usage and Cost console pages plus a Usage & Cost Admin API
  with `1m`/`1h`/`1d` buckets
  ([usage-cost-api](https://platform.claude.com/docs/en/api/usage-cost-api)).

**Verdict: BEHIND on the rendered surface, AT PARITY on the data primitive.**
Two gaps are real, and only one of them is a UI question:

1. **No pixels.** Every usage-based peer ships something an end customer can
   look at; Velox ships JSON. This stays deferred and the reasoning is
   unchanged — Stripe, the volume leader, ships *only* the API and expects the
   vendor to build the dashboard, and an AI-infra design partner has its own
   product UI that an iframe fights (Metronome's `color_overrides` exists
   because of exactly that friction). For a self-hosted product whose users are
   engineers, JSON is the correct primitive and a widget is a convenience layer.
   Trigger unchanged: build when a design partner asks.
2. **Current period only — and this one is a *data* gap, not a UI gap.** Peers
   expose history and time series (Metronome 30/60/90 days, Orb history plus
   current, both AI consoles down to minute granularity). Velox's projection
   cannot answer "what did I spend yesterday versus today", which is table
   stakes for the AI-infra ICP this endpoint exists to serve. **Deferred with
   trigger: if the first design partner is AI infrastructure, historical
   time-series on this projection is the fast-follow — it is cheap relative to
   a UI and is where the peer set is genuinely ahead.**

Token shape is unaffected by the above: Orb's customer-portal URL is likewise
non-expiring, so Velox's non-expiry is at parity. The plaintext-at-rest storage
noted under Negative is Velox-specific and remains gated on the first
production deploy.
