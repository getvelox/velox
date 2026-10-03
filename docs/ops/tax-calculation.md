# Velox — Tax Calculation

Velox ships three tax providers: pluggable backends that compute the tax
on an invoice. Each tenant picks one via `tenant_settings.tax_provider`.
The billing engine resolves the provider when it builds the invoice. It
reuses the same provider at finalize and at credit-note issuance.

Velox does not pick a tax model. The tenant's Stripe account holds the
legal registration, and Velox supports whatever shape that registration
produces. Examples:

- OIDAR (registration for cross-border digital services)
- domestic GST/VAT
- US sales tax, …

## Providers

### `none`

Zero-tax backend. `Calculate` returns zero per line. `Commit` and
`Reverse` are no-ops. (These are the three provider methods; see
*Provider interface* below.)

Pick this when the tenant doesn't collect tax (early-stage B2B,
unregulated jurisdictions).

An empty `tax_provider` falls through to `none`. An unrecognised value is
a loud error that aborts invoice creation for that tenant. A corrupted
settings row stalls billing rather than silently taxing at zero.

### `manual`

Flat percent rate applied uniformly across every line item. Configure it
per tenant with two settings:

- `tenant_settings.tax_rate`: a percent, so `7.25` = 7.25%.
- `tenant_settings.tax_name`: the label that renders on the invoice
  ("VAT", "Sales Tax", "GST", …).

It honours three cases:

- Tax-inclusive vs exclusive pricing, via `Request.TaxInclusive`.
  Inclusive means the tax is contained in the listed price; exclusive
  means it is added on top.
- Exempt customers (`StatusExempt`).
- Reverse-charge customers (`StatusReverseCharge`): the buyer accounts
  for the tax instead of the seller.

`Commit` and `Reverse` are no-ops, because there is no upstream state to
record.

Pick this when:

- Single jurisdiction, single legal rate.
- Tenant exempt (set `tax_rate=0`).
- Tenant does not have Stripe Tax registered or wired up.

There is no automatic manual fallback for `stripe_tax` failures. A failed
Stripe Tax call always defers the invoice; see *Failure handling* below.

### `stripe_tax`

This provider makes three calls to Stripe:

1. At invoice build, it calls Stripe's Tax Calculations API.
2. At invoice finalize, `Commit` creates a `tax_transaction`.
3. At credit-note issue, `Reverse` reverses against that transaction.

It is multi-tenant. The Stripe client is resolved per ctx (the request
context: `tenant_id` + `livemode`) via the `StripeClientResolver`
interface. So each tenant's calls hit their own Stripe account in the
correct mode.

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

`Commit` makes a tax decision durable upstream: it is recorded on
Stripe's side, not just Velox's. Stripe Tax *calculations* expire after
24 hours, but `tax_transaction`s are permanent and surface in Stripe's
tax reporting. The transaction ID is persisted on
`invoices.tax_transaction_id`.

`Commit` uses the invoice ID as Stripe's reference. Stripe enforces
reference uniqueness across `tax_transaction`s in the account, so a
retried finalize is idempotent.

`Reverse` issues a reversal against an existing `tax_transaction` when a
credit note is issued. The credit-note ID is the reversal's reference,
so retried credit-note issuance is idempotent too.

`none` and `manual` return an empty `ReversalResult`. The credit-note
flow can then ignore the outcome without branching on provider name.

## Failure handling: fallback vs defer

`stripe_tax` can fail for three reasons:

| Condition                                                      | Reason label         |
|----------------------------------------------------------------|----------------------|
| Customer has no country on file                                | `no_country`         |
| No Stripe client configured for the request's livemode         | `no_client_for_mode` |
| Stripe Tax API returns any error                               | `api_error`          |

On failure there is one behavior:

1. The error propagates.
2. The engine defers the invoice to `tax_status=pending` (parked, not
   issued).
3. A scheduled retry re-runs `Calculate`.

No invoice is ever issued at a guessed rate. `block` is now the only
`OnFailure` value. The legacy `fallback_manual` policy was removed
2026-05-30; see
[ADR-041](../adr/041-tax-fallback-manual-removed.md).

The outcome is counted on `velox_tax_outcome_total{outcome, reason}`:

- `outcome=deferred`: a blocked billing cycle. It needs operator action
  if retries do not clear it.

Happy-path calculations are not counted on this metric. It is a pure
failure-mode signal, suitable for `rate(...) > 0` alerting.

### Why defer rather than fall back to a manual rate?

An earlier design fell back to the tenant's manual rate on a Stripe Tax
outage. It traded correctness for availability, and it was cut.

A billing engine that issues invoices at a *guessed* tax rate is a
compliance liability. A wrong rate legally exposes regulated
marketplaces and OIDAR-registered EU VAT tenants.

Deferring the invoice with a retry is the safe default. The pile of
deferred invoices is itself the alert, and a transient outage clears on
the next tick.

### Log lines

```
WARN stripe tax failed, deferring invoice for retry reason=api_error error=… livemode=true
WARN stripe tax failed, deferring invoice for retry reason=no_country
WARN stripe tax failed, deferring invoice for retry reason=no_client_for_mode livemode=true
```

These are `warn`, not `error`, because the system continues correctly:
the invoice is deferred and retried. There is no silent manual-rate
fallback on a Stripe Tax failure. Alert on the metric, not on log
presence.

## When to investigate a fallback or defer

| Pattern | Likely cause | Action |
|---|---|---|
| **Single tenant, repeated** | Stripe Tax setup is broken on the tenant side, or their customers are missing country data. | Contact the tenant. |
| **Many tenants, correlated in time** | Stripe API incident. Check [status.stripe.com](https://status.stripe.com). | Ensure the retry scheduler clears the `deferred` invoices once Stripe recovers. |
| **Single tenant, sudden start** | Stripe API key rotated or revoked. | Ask the tenant to update credentials. |

## Mode-split semantics

A test-mode invoice resolves to the test Stripe client, and a live-mode
invoice to the live client (see
[test mode and live mode](../README.md#accounts-and-access) in the glossary). This matters
for two reasons:

1. Test-mode keys are rate-limited differently. A test-mode load test
   should not exhaust a tenant's live-mode Stripe quota.
2. Calling Tax on the wrong Stripe account is incorrect accounting, even
   though no state mutation occurs. A calculation creates no upstream
   state, but `Commit` does.

If only one mode has a key configured, calculations in the other mode
fail. The invoice is deferred to tax retry (metric outcome `deferred`,
reason `no_client_for_mode`); there is no fallback policy. Tenants who
want both modes operational must configure both keys in their tenant
settings.

## Persistence

Tax decisions are recorded on the invoice itself, not in a separate
audit table. The invoice is the durable record:

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
invoice-level fields are the upstream linkage. They let `Reverse` find
the original `tax_transaction` when a credit note is issued. They also
let operators reconcile a Velox invoice against Stripe Tax reports
without joining log lines.

## Tax IDs

`internal/tax/taxid.go` validates customer tax IDs (VAT numbers, GST
numbers, etc.). It surfaces the validated ID on the invoice so the PDF
renders the correct legal text.

Format validation is local and checks the ID's shape only. Checking that
the ID actually exists with a tax authority is out of scope.

## Related

- [runbook.md](./runbook.md) — alerts that page on billing-cycle
  failures, including tax outcomes that exceed thresholds.
