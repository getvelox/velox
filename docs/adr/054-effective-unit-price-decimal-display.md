# ADR-054: Display the per-unit price at full precision — derive it on read, don't store it

**Date:** 2026-06-17
**Status:** Accepted

## Context

A usage invoice line for a sub-cent rate rendered **Unit Price $0.00** while the **Amount** was correct (e.g. 1,000 units billed $3.00 — a $0.003/unit rate). The line amount is computed from the full-precision decimal rate (ADR-045) and is exact; but the invoice line stores the per-unit price only as `unit_amount_cents` (int64 whole cents), **back-derived** from the rounded amount: `round(300 ÷ 1000) = round(0.3) = 0`. Both the dashboard and the PDF then print that `0` at two decimals → `$0.00`. The result looks internally inconsistent (1,000 × $0.00 ≠ $3.00) and is wrong for the AI-token / usage case Velox targets, where sub-cent rates are the norm.

Verified industry behavior (research, 2026-06-17) is a **two-track precision model**: the per-unit price is shown at full/native decimal precision, and rounding to currency precision (whole cents) is applied **only** to the derived line amount and totals — never to the displayed unit price.

- **Stripe** — `unit_amount_decimal`, up to 12 decimal places; *"rounding occurs on the line item level … after multiplying the quantity by the decimal amount"* ([products-prices](https://docs.stripe.com/products-prices/manage-prices)).
- **Chargebee** — up to 20 dp; *"invoices … display the product price and quantity in multiple decimal places … line item amounts are rounded to the currency's precision"* ([multi-decimal](https://www.chargebee.com/docs/2.0/multi-decimal-support.html)).
- **Recurly** — up to 9 dp; *"the unit_amount_decimal value is displayed on the invoice … line item totals are rounded"* ([decimal-pricing](https://docs.recurly.com/recurly-subscriptions/docs/decimal-pricing)).
- **Metronome / Lago** — carry the rate in fractional minor units / configurable precision; rounding is opt-in at the amount level.

## Decision

Surface a full-precision per-unit price, **derived on read**, never stored.

1. **One computation** — `domain.InvoiceLineItem.EffectiveUnitAmountDecimal()` returns `amount_cents ÷ quantity` in decimal cents, capped at 12 dp (Stripe parity), guarding `quantity == 0`. It uses `QuantityDecimal` when set, else the integer `Quantity`.
2. **Effective, not nominal** — the displayed unit price is the *effective* rate (amount ÷ quantity), so it always **reconciles** with the rounded line amount and stays well-defined for blended/tiered/multi-dimensional lines that have no single nominal rate. (The nominal rule rate lives upstream and is undefined for aggregated lines; surfacing it would not reconcile with the amount.)
3. **Plumbed through the API, computed once** — `InvoiceLineItem.MarshalJSON` injects `unit_amount_decimal` (decimal-cents string) into the dashboard wire form; the hosted-invoice DTO and the PDF call the same method. No frontend arithmetic (`feedback_no_heuristic_proxies`): the backend serves the value, the renderers display it via the existing decimal-aware `formatRate` (which never collapses a sub-cent rate to `$0.00`). `unit_amount_cents` is retained unchanged for back-compat.

**No new column, no migration.** The unit price is a *derived* quantity; persisting it would be denormalization that can drift from `amount_cents` — exactly the dual-write class this codebase has been bitten by. Deriving on read from the authoritative `amount_cents` + `quantity` is the normalized, drift-proof design and also fixes every existing row with no backfill.

## Why this design

`amount_cents` and `quantity` are the authoritative persisted facts; the unit price is `amount ÷ quantity` by definition. Computing it in one domain method and serving it (rather than storing it, or re-deriving it in each of the dashboard / PDF / hosted renderers) gives a single source of truth that cannot disagree with the amount and cannot drift across surfaces (`project_tax_field_propagation_drift` is the cautionary precedent). Line amounts and totals stay whole-cent int64 (ADR-045 unchanged); only the per-unit *display* gains precision — the two-track model the peers converge on.

## Alternatives considered

- **Store a `unit_amount_decimal` column populated at line construction.** Rejected: a migration plus ~16 construction sites to populate, a denormalized field that can drift from `amount_cents`, and a backfill question for existing rows — all to persist a value that is trivially derivable on read.
- **Plumb the nominal rule rate onto the line.** Rejected: undefined for tiered / multi-dimensional / package lines (no single rate), and `quantity × nominal` need not equal the rounded `amount_cents`, so the displayed unit price wouldn't reconcile with the charged amount.
- **Compute `amount ÷ quantity` in the frontend.** Rejected per `feedback_no_heuristic_proxies` — the backend knows the value authoritatively, so it plumbs it through the API; the dashboard and hosted page render the served field rather than each re-implementing the arithmetic.

## Consequences

### Positive
- Sub-cent rates display correctly ($0.003, $0.0000030) on the dashboard, the PDF, and the public hosted page; `quantity × unit ≈ amount` reconciles.
- Zero schema change, zero migration, zero backfill — every existing line is fixed on read.
- Single computation (`EffectiveUnitAmountDecimal`) shared by all three render surfaces; no per-renderer drift.

### Risks / open items
- **Credit-note line items** (`credit_note_line_items`) carry the same whole-cent `unit_amount_cents` and would show the same `$0.00` for a sub-cent gross unit. Deferred — same pattern (a parallel method + the CN render surfaces); credit notes for sub-cent usage are rare and the reported surface is the invoice. Trigger: first sub-cent credit-note display complaint.
- The effective rate can differ from the *nominal* configured rate by the amount's rounding at very small quantities (e.g. one unit of a sub-cent item rounds the amount itself); this is correct invoice behavior (Stripe rounds the line amount too) and reconciles with what's charged.

## Amendment (2026-07-07): show the NOMINAL configured rate on flat usage lines

**Status:** Accepted — implemented. Supersedes decision point 2 ("effective, not nominal") *for flat single-rule usage lines only*.

### Why revisit

A sub-cent usage line rendered `$0.00000333333333` (6,000 tokens billed 2¢ at a configured $3.00/1M = 0.0003¢/token). It's correct — the effective rate at 12dp — but reads like a glitch, and worse, it is *inflated*: the 2¢ was rounded up from 1.8¢, so the effective rate (0.000333¢) overstates the configured rate (0.0003¢) by ~11%. Verified industry behavior (research 2026-07-07, quotes/URLs in memory `project_unit_rate_display_nominal_vs_effective`): peers show the **configured/nominal** rate (Stripe's `unit_amount_decimal` is the rate you *set*, which terminates → no repeating decimal), and **exact `qty × unit = amount` reconciliation is NOT an industry requirement** (ProjectWorks / ERP.net / EU e-invoice all display `qty × unit ≠ rounded amount`). That removes this ADR's *original* main reason (reconciliation) for choosing effective over nominal.

### Decision

Stamp the actually-billed **nominal** per-unit rate on **flat-mode usage lines** and display it; keep the effective rate everywhere else.

1. **Stamped at build, not derived on read.** The override trap: `domain.CustomerPriceOverride.ApplyTo` swaps the price but **preserves the `rating_rule_versions` id** the line stores, so reading that row back yields *list* price, not the negotiated price billed. The engine stamps `rule.FlatAmountCents` from the *override-applied* rule (`resolveRatedRule`), so `invoice_line_items.nominal_unit_amount_decimal` (migration 0142, nullable NUMERIC) holds the real billed rate. Helper `nominalRate(rule)` returns nil for non-flat modes; the 4 usage writers + the preview/overage copy-through are the complete writer set.
2. **Flat only; effective is the fallback.** Graduated/package usage (blended rate, no single nominal), base_fee/proration (the effective per-seat amount is the honest figure — the full plan price would misrepresent a prorated line), and add_on/discount/tax (whole-cent unit already equals nominal, `qty × unit` is an exact product) all leave the column NULL and fall back to `EffectiveUnitAmountDecimal` — today's behavior. Verified by a per-line-type audit (2026-07-07).
3. **One display value, all surfaces.** `DisplayUnitAmountDecimal()` returns nominal when stamped, else effective; `MarshalJSON` (dashboard), the hosted DTO, and the PDF all call it — no FE change (the wire's `unit_amount_decimal` just gets cleaner). The nominal field is internal-only (`json:"-"`) — the wire already carries the display value.
4. **No backfill.** Historical lines keep the effective rate (NULL nominal); only newly-built lines stamp it. Consistent with this ADR's no-backfill posture.

Result: the screenshot line now shows `$0.000003` (the rate card), not `$0.00000333333333`. Still deferred (a genuine feature, its own trigger): a **per-million display scale** for token meters (`$3.00 / 1M tokens`, the Metronome/Anthropic/OpenAI convention) — needs meter display-scale config; and the parallel `credit_note_line_items` gap (§Risks above).

## Amendment (2026-07-08) — extend to the live usage surfaces

The decision above shipped **invoice-line only**. Two other surfaces show a usage unit price and were still deriving the *effective* `amount ÷ quantity` — on the **frontend**, with `toFixed`, which rounded a sub-cent rate down to **$0.0000**: the customer **Activity panel** (`usage.CustomerUsageRule`) and the **public cost dashboard** (`usage.CostDashboardRule`). Both resolve the rating rule *live* (as-of period open, customer override applied — `CustomerUsageService.rateMeter`), so the authoritative nominal rate was already in hand; the frontend was re-deriving a worse value from `amount_cents` and `quantity`. That is the exact inconsistency this ADR set out to remove, on a different screen — and a `feedback_no_heuristic_proxies` violation (the backend knew the rate; the FE guessed it).

**Change.** Extract the derivation into package `domain` as the single source both billing and the usage view call:
- `NominalUnitAmountDecimal(rule)` — flat → configured `FlatAmountCents`, nil for graduated/package (`billing.nominalRate` now delegates to it).
- `EffectiveUnitAmountDecimalFor(amountCents, quantity)` — the shared effective formula, 12dp (`InvoiceLineItem.EffectiveUnitAmountDecimal` now delegates to it).
- `DisplayUnitAmountDecimalFor(rule, amountCents, quantity)` — the **live-rule twin** of `InvoiceLineItem.DisplayUnitAmountDecimal`: nominal for flat, else effective. The usage view stamps each rule row's new `unit_amount_decimal` from it, mapped through to the public cost-dashboard DTO.

**Frontend.** Renders `unit_amount_decimal` with the decimal-aware `formatRate` and **does not** fall back to a client-side `amount÷qty` — if the backend didn't send it, the row shows no rate, so an absent value is a visible signal rather than a silent proxy (`feedback_no_heuristic_proxies`, `feedback_no_silent_fallbacks`). The FE-synthesized `__other__` catch-all bucket (no single rule) carries no nominal and stays rate-hidden as before.

**No schema change** — the usage view is computed live per request; nothing is persisted (contrast the invoice line, which stamps `nominal_unit_amount_decimal` at build because it can't re-derive on read past an override). Pinned by a `DisplayUnitAmountDecimalFor` domain test (flat nominal wins over the diverging effective; graduated → effective; zero-qty guard) and a `CustomerUsageService.Get` regression that reproduces the screenshot (1,750 tokens billed 3¢ at 0.0015¢/token → row shows nominal `0.0015`, not effective `0.0017142857…`).

## Amendment (2026-08-09): per-1M display on invoice surfaces — the arc's last residual

The 07-07 re-examination recorded the second industry finding — AI-native
peers show token rates PER MILLION (Metronome: "display and price per
million tokens on the invoice"; Anthropic and OpenAI both quote $/MTok) —
and deferred it pending a design partner, because at the time an invoice
line had no persisted link to its meter.

Both premises have since changed: `invoice_line_items.meter_id` ships, and
the Pricing section already displays per-1M via the unit-keyed convention
in `web-v2/src/lib/priceDisplay.ts` (`unitScale`: unit `tokens`/`token` →
per 1M). The trigger was also re-judged as mis-set by the same reasoning
that closed the cost-dashboard hashing gate early: the harm (an "AI-native"
engine whose invoices quote `$0.000003` in a world that quotes `$3.00/1M`)
lands during first EVALUATION — before a design partner exists to ask.

**Decision.** Migration 0173 stamps the meter's `unit` string onto usage
lines at write time (`meter_unit`), and every invoice renderer applies the
one display convention: dashboard and hosted page read "$3.00 / 1M tokens",
the PDF reads the compact "$3.00 / 1M" (the unit word is already in the
description, and the full form overflowed the Qty column — caught on
render). The RAW unit is stamped, not a resolved scale, so the convention
lives in exactly one place per renderer (`domain.MeterUnitDisplayScale` in
Go, `unitScale` in TS — deliberately tiny twins, each pinned by its own
test) and an invoice stays a stable document: editing a meter never
re-labels history. Backfill is exact (the meter_id join), unlike the 0142
nominal-rate backfill which was impossible for overridden lines — verified
in place: 109/109 pre-migration usage lines named on migrate.

Still deferred from this arc: the `credit_note_line_items` sub-cent display
gap (§Risks) — unchanged premises, unchanged trigger.

## References
- ADR-045 (decimal per-unit pricing rates — line amounts stay int64), ADR-053 (single source of truth posture).
- Memory: `project_decimal_pricing_rates`, `feedback_no_heuristic_proxies`, `project_tax_field_propagation_drift`, `feedback_verify_stripe_parity_claims`.
- [Stripe prices](https://docs.stripe.com/products-prices/manage-prices), [Chargebee multi-decimal](https://www.chargebee.com/docs/2.0/multi-decimal-support.html), [Recurly decimal pricing](https://docs.recurly.com/recurly-subscriptions/docs/decimal-pricing).
