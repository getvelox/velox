# Velox — Tax Calculation

Velox ships three tax providers — pluggable backends that compute the tax
on an invoice. Each tenant picks one via `tenant_settings.tax_provider`.
The billing engine resolves the provider at invoice build time and reuses
it through finalize and credit-note issuance. Velox does not pick a tax
model — the tenant's Stripe account holds the legal registration (OIDAR — cross-border
digital services — domestic GST/VAT, US sales tax, …)
and Velox supports whatever shape that registration produces.

## Providers

### `none`

Zero-tax backend. `Calculate` returns zero per line; `Commit` and
`Reverse` are no-ops (the three provider methods — see *Provider
interface* below). Pick this when the tenant doesn't collect tax
(early-stage B2B, unregulated jurisdictions). An empty `tax_provider`
falls through to `none`. An unrecognised value is a loud error that
aborts invoice creation for that tenant: a corrupted settings row
stalls billing rather than silently taxing at zero.

### `manual`

Flat percent rate applied uniformly across every line item.
Configured per tenant via `tenant_settings.tax_rate` (a percent —
`7.25` = 7.25%) and
`tenant_settings.tax_name` (the label that renders on the invoice —
"VAT", "Sales Tax", "GST", …). Honours tax-inclusive vs exclusive pricing (tax contained in the
listed price vs added on top of it) via
`Request.TaxInclusive`, exempt customers (`StatusExempt`), and
reverse-charge customers (`StatusReverseCharge` — the buyer accounts for
the tax instead of the seller). `Commit` and `Reverse`
are no-ops (no upstream state to record).

Pick this when:

- Single jurisdiction, single legal rate.
- Tenant exempt (set `tax_rate=0`).
- Tenant does not have Stripe Tax registered or wired up.

(There is no automatic manual fallback for `stripe_tax` failures — post
ADR-041 a failed Stripe Tax call always defers the invoice; see *Failure
handling* below.)

### `stripe_tax`

Calls Stripe's Tax Calculations API at invoice build, creates a
`tax_transaction` at invoice finalize via `Commit`, and reverses against
that transaction at credit-note issue via `Reverse`. Multi-tenant: the
Stripe client is resolved per ctx (per request context: `tenant_id` +
`livemode`) via the `StripeClientResolver` interface, so each tenant's
calls hit their own Stripe account in the correct mode.

Pick this when:

- Tenant sells across multiple jurisdictions (US sales tax, EU VAT, GB
  VAT, …).
- Tenant has registered for tax collection with Stripe and needs Stripe
  Tax reporting to reflect Velox-issued invoices.
- Tenant needs durable jurisdictional records that reconcile against
  Stripe's tax reports.

## Provider interface

All three implement the same `Provider` shape:

| Method      | Called at         | Mutates upstream?    |
|-------------|-------------------|----------------------|
| `Name`      | —                 | no                   |
| `Calculate` | invoice build     | no                   |
| `Commit`    | invoice finalize  | yes (`stripe_tax`)   |
| `Reverse`   | credit-note issue | yes (`stripe_tax`)   |

`Commit` is what makes a tax decision durable upstream (recorded on
Stripe's side, not just Velox's). Stripe Tax
*calculations* expire after 24 hours, but `tax_transaction`s are
permanent and surface in Stripe's tax reporting. The transaction ID is
persisted on `invoices.tax_transaction_id`. `Commit` uses the invoice ID
as Stripe's reference, so a retried finalize is idempotent (Stripe
enforces reference uniqueness across `tax_transaction`s in the account).

`Reverse` issues a reversal against an existing `tax_transaction` when a
credit note is issued. The credit-note ID is the reversal's reference,
so retried credit-note issuance is similarly idempotent. `none` and
`manual` return an empty `ReversalResult` so the credit-note flow can
ignore the outcome without branching on provider name.

## Failure handling: fallback vs defer

`stripe_tax` can fail for three reasons:

| Condition                                                      | Reason label         |
|----------------------------------------------------------------|----------------------|
| Customer has no country on file                                | `no_country`         |
| No Stripe client configured for the request's livemode         | `no_client_for_mode` |
| Stripe Tax API returns any error                               | `api_error`          |

On failure there is one behavior (post ADR-041 — the legacy
`fallback_manual` policy was removed 2026-05-30; `block` is now the only
`OnFailure` value): the error propagates, the engine defers the invoice
to `tax_status=pending` (parked, not issued), and a scheduled retry
re-runs `Calculate`. No invoice is ever issued at a guessed rate.

The outcome is counted on `velox_tax_outcome_total{outcome, reason}`:

- `outcome=deferred` — blocked billing cycle; needs operator action if
  retries do not clear.

Happy-path calculations are not counted on this metric — it is a pure
failure-mode signal, suitable for `rate(...) > 0` alerting.

### Why defer rather than fall back to a manual rate?

An earlier design fell back to the tenant's manual rate on a Stripe Tax
outage, trading correctness for availability. That was cut (ADR-041): a
billing engine issuing invoices at a *guessed* tax rate is a compliance
liability — regulated marketplaces and OIDAR-registered EU VAT tenants
are legally exposed by a wrong rate. Deferring the invoice with a retry
is the safe default; the deferred pile is itself the alert, and a
transient outage clears on the next tick.

### Log lines

```
WARN stripe tax failed, deferring invoice reason=api_error error=… livemode=true
WARN stripe tax failed, deferring invoice reason=no_country
WARN stripe tax failed, deferring invoice reason=no_client_for_mode livemode=true
```

These are `warn`, not `error`, because the system continues correctly —
either a tenant-configured rate applied or the invoice was deferred to
retry. Alert on the metric, not on log presence.

## When to investigate a fallback or defer

- **Single tenant, repeated** — Stripe Tax setup is broken on the
  tenant side, or their customers are missing country data. Action:
  contact the tenant.
- **Many tenants, correlated in time** — Stripe API incident. Check
  [status.stripe.com](https://status.stripe.com). Action: ensure the
  retry scheduler clears the `deferred` invoices once Stripe recovers.
- **Single tenant, sudden start** — Stripe API key rotated or revoked.
  Action: ask the tenant to update credentials.

## Mode-split semantics

A test-mode invoice resolves to the test Stripe client, a live-mode
invoice to the live client. This matters because:

1. Test-mode keys are rate-limited differently — a test-mode load test
   should not exhaust a tenant's live-mode Stripe quota.
2. Calling Tax on the wrong Stripe account is incorrect accounting even
   though no state mutation occurs (a calculation creates no upstream
   state, but `Commit` does).

If only one mode has a key configured, calculations in the other mode
fail and the invoice is deferred to tax retry (metric outcome `deferred`,
reason `no_client_for_mode`) — post-ADR-041 there is no fallback policy.
Tenants who want both modes operational must configure both keys in
their tenant settings.

## Persistence

Tax decisions are recorded on the invoice itself, not in a separate
audit table — the invoice is the durable record:

| Column                              | Source                                                  |
|-------------------------------------|---------------------------------------------------------|
| `invoices.tax_provider`             | provider that produced this invoice                     |
| `invoices.tax_calculation_id`       | Stripe Tax `calc_xxx` (`stripe_tax` only)               |
| `invoices.tax_transaction_id`       | Stripe Tax `tx_xxx`, set at `Commit` (`stripe_tax` only) |
| `invoices.tax_reverse_charge`       | reverse-charge flag                                     |
| `invoices.tax_exempt_reason`        | exempt-customer reason                                  |
| `invoice_line_items.tax_rate`       | rate applied to this line (percent, e.g. `7.25` = 7.25%) |
| `invoice_line_items.tax_name`       | label rendered on the invoice                           |

The line-level fields are what the customer sees on the PDF. The
invoice-level fields are the upstream linkage: they let `Reverse` find
the original `tax_transaction` when a credit note is issued, and they let
operators reconcile a Velox invoice against Stripe Tax reports without
joining log lines.

## Tax IDs

`internal/tax/taxid.go` validates customer tax IDs (VAT numbers, GST
numbers, etc.) and surfaces the validated ID on the invoice so the PDF
renders the correct legal text. Format validation is local (the ID's
shape only); checking that the ID actually exists with a tax authority is
out of scope.

## Related

- [runbook.md](./runbook.md) — alerts that page on billing-cycle
  failures, including tax outcomes that exceed thresholds.
