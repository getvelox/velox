# ADR-046: Manual tax — document-level total + largest-remainder line apportionment

**Status:** Accepted
**Date:** 2026-06-03
**Supersedes:** the "dump the residual on the positionally-last line"
reconciliation in `internal/tax/manual.go` (introduced `401d990`, both
the exclusive and inclusive paths).

## Summary

The manual tax provider (`ManualProvider`) computes an invoice's tax once, on the subtotal, and then splits that total across the line items. Each line gets the whole-cent floor of its exact share, and the leftover cents go to the lines with the largest fractional remainders, with ties going to the lowest index. The old code put the whole rounding difference on the last line, which could tax a larger line less than a smaller one. Now line taxes sum to the invoice tax and each line is within 1¢ of its exact share. Rounding is half-to-even, as in the rest of the money path, and a per-tenant line-level rounding setting is deferred.

## Context

The `ManualProvider` applies one flat tenant rate to every line item.
Two facts about integer-cent money force a reconciliation step:

1. **Tax is rounded.** A line's tax (`base × rate`) rarely lands on a
   whole cent, so each line's tax is rounded to the nearest cent.
2. **Per-line rounding doesn't sum to the document tax.** Rounding each
   line independently and summing can drift ±1¢ (more with many lines)
   from the tax computed on the document subtotal.

Worked example — three lines `$33.33 / $33.33 / $33.34` at 7.25%:

| line | base (¢) | exact tax | naive round |
|---|---|---|---|
| 1 | 3333 | 241.6425 | 242 |
| 2 | 3333 | 241.6425 | 242 |
| 3 | 3334 | 241.7150 | 242 |
| **Σ** | **10000** | **725.00** | **726** |

The exact document tax is `10000 × 7.25% = 725¢`; naive per-line rounding
gives `726¢`. Something must reconcile the 1¢ gap so that
`Σ(line tax) == invoice tax`.

There are two independent decisions hiding in that reconciliation.

### Axis 1 — what is the authoritative document total?

- **Top-down** (compute tax on the subtotal): `725¢`. Exact; this is the
  VAT-correct figure and what an auditor recomputes as `$100.00 × 7.25%`.
- **Bottom-up** (sum the rounded per-line taxes): `726¢`. Over-collects
  the residual cent versus the true rate × base.

### Axis 2 — which line absorbs the residual?

Given a chosen total, the rounded per-line taxes must be nudged to hit
it. The **old** code added the entire residual to the line that was
**last in array order**:

```go
lines[len(lines)-1].TaxAmountCents += totalTax - lineTaxSum
```

That preserves the sum but says nothing about *placement*. When the
residual is **negative** (here −1¢, since 725 < 726) and the last line is
the largest, that line is docked below its smaller peers:
`242 / 242 / 241` — the `$33.34` line taxed *less* than two `$33.33`
lines. On the invoice this reads as a bug ("biggest line, least tax"),
and it is one: the placement is arbitrary, not principled. The flaw was
latent from the day the reconciliation was added and surfaced only on
inputs that drive the residual negative onto the largest line — earlier
examples and tests all happened to have a positive residual landing as a
round-*up* on the last line, which reads naturally.

## Decision

**Keep the total top-down (725¢, exact). Distribute the residual by
largest remainder.**

- **Axis 1 — top-down total, unchanged.** `totalTax` stays
  `round(taxableBase × rate)`. This is the exact, audit-defensible
  figure for a single flat rate, and matches the document-level rounding
  EU VAT explicitly permits. (Stripe's *default* and automatic Stripe
  Tax sum bottom-up → would yield 726¢; Stripe also offers a document /
  "invoice level" rounding mode for manual rates, which is what we do.
  Since this provider is the *manual* one, top-down is the right default
  and the contained change.)

- **Axis 2 — largest-remainder apportionment.** This is the textbook
  **Largest Remainder Method** (a.k.a. *Hamilton's method*, or
  *Hare–Niemeyer* in proportional-representation contexts) — the same
  algorithm used to apportion legislative seats to parties by vote share,
  applied here to cents instead of seats. New helper
  `distributeLargestRemainder(total, nums, den)` in `internal/tax/manual.go`:
  each line gets `floor(nums[i] / den)`, then the leftover cents
  (`total − Σfloor`, mathematically in `[0, n]`) are handed out one at a
  time to the lines with the **largest fractional remainders**, ties
  broken by **lowest index**. It satisfies the *quota rule* — every line
  receives either the floor or the ceiling of its exact share (no line
  more than 1¢ off), which is exactly the within-1¢ / no-inversion
  guarantee below. Applied to both the exclusive and inclusive paths (the
  exact per-line share is `base × ppm / 1_000_000` exclusive,
  `grossBase × ppm / denom` inclusive).

Guarantees:
- `Σ(line tax) == totalTax` (the invariant the old code also held).
- Every line is within 1¢ of its exact share.
- **No base-order inversion**: a line with a larger base never carries
  smaller tax than a smaller line. (For the example: floors are
  `241 / 241 / 241`; the two leftover cents go to the largest remainders —
  the `$33.34` line at .7150 and one `$33.33` line at .6425 — giving
  `242 / 241 / 242`. The `$33.34` line is promoted, never left behind.)

`stripe_tax` is unaffected — Stripe returns per-line tax amounts and a
total directly; the engine copies them verbatim
(`internal/billing/engine.go` `ApplyTaxToLineItems`). This decision is
scoped to the internal manual provider.

## Consequences

- **Invoices never show a larger line taxed less than a smaller one.**
  The operator-visible artifact that triggered this ADR is gone.
- **Document total is unchanged.** Still `725¢` for the example; only
  *which* line absorbs the cent moved. No customer is charged a
  different total than before.
- **Method matches every reference indirect-tax engine.** See below —
  this is the convergent industry rule, not a Velox invention.
- **One small helper, two call sites.** No new dependency, no schema
  change, no API change. Pure provider-internal logic.

## Industry references

Verified 2026-06-03 across multiple platforms (not a single-source
spot-check):

- **Sovos** (enterprise indirect-tax): the residual "is split among the
  lines that have the highest remainder to the right of the truncated
  values"; on ties "added to the first line." → largest remainder,
  lowest-index tie-break. (This is the rule we implemented.)
- **Avalara AvaTax** / **Microsoft Dynamics 365**: adjust "the line that
  results in the minimum percentage change in tax amount" — the same
  minimum-distortion intent.
- **Stripe**: manual rates support both "line item level" (round per
  line, then sum → bottom-up) and "invoice level" (apply rate to
  subtotal, then round → top-down) rounding; automatic Stripe Tax always
  sums per-line then rounds. Stripe never dumps the residual on a
  positional line.

**Zero** platforms in the surveyed set distribute the residual by array
position. The old behavior was the outlier.

## Revisit trigger

- A design partner is in a **line-level rounding** jurisdiction (per-line
  tax is the source of truth; the document total = the sum of rounded
  lines → 726¢ for the example). This is the document-level-vs-line-level
  axis detailed in the **Addendum** below — it is jurisdiction-mandated,
  not a preference. The fix is a per-tenant rounding-mode setting
  (`document` | `line`), exactly as Stripe and Avalara expose; add it as a
  `tenant_settings` option rather than changing the default. Until a DP in
  a line-level jurisdiction uses the *manual* provider, document-level is
  the default.
- Multi-jurisdiction manual rates (per-line different rates) — the
  apportionment would run per rate group, not across the whole document.
  Out of scope while the manual provider is single-rate.

## Related

- ADR-042 / ADR-043: tax-rate decimal precision (the `ratePPM` integer
  math this apportionment builds on).
- ADR-041: manual-fallback removal — same provider, prior cleanup.
- `feedback_verify_real_path_not_shortcut`: the old reconciliation
  passed the sum-invariant test but its *placement* was never tested;
  the new tests assert the real provider path + no-inversion.

## Addendum (2026-06-04): document-level vs line-level rounding is jurisdiction-dependent

"Axis 1" above (what the authoritative document total is) is the
industry's **document-level vs line-level rounding** choice. A follow-up
multi-platform review confirmed it is **jurisdiction-mandated**, not a
mere preference:

- **Line-level (bottom-up):** round each line's tax, then sum; the
  invoice tax is whatever the rounded lines add up to. Required where each
  line item is its own taxable sale / separate GL posting — several **US
  sales-tax** states.
- **Document-level (top-down):** sum the lines, apply the rate to the
  subtotal, round once. Closest to the mathematically exact tax; favored
  by **EU VAT** and much of APAC, where the *invoice* is the taxable unit.

The two methods can disagree on the **invoice total itself**, not just
the per-line split:

| input @ 7.25% | line-level (Σ rounded lines) | document-level (round once) |
|---|---|---|
| 3333 / 3333 / 3334¢ | 242 + 242 + 242 = **726¢** | round(725.00) = **725¢** |
| ten × 7¢ | 10 × round(0.5075) = **10¢** | round(5.075) = **5¢** |

The second row shows the gap is **not** bounded at 1¢: many small lines
that each round up make line-level over-collect substantially.

**Velox's manual provider hardcodes document-level** (top-down total +
largest-remainder split — Axis 1 + Axis 2 above). That is a valid,
legally-accepted method, and `stripe_tax` already selects the correct
per-jurisdiction method automatically — so the hardcode only under-serves
a tenant who is in a **line-level jurisdiction**, uses the **manual**
provider, AND needs strict per-line compliance.

**Deferred (not built):** a per-tenant `tax_rounding_level`
(`document` | `line`) setting, exactly as **Stripe** (line-item vs
invoice-level rounding for manual rates) and **Avalara** (document vs line
level rounding) expose. Trigger = the first design partner who hits the
line-level case above. Until then, document-level is the deliberate
default and this is a documented, intentional divergence — not a latent
bug.

Sources (verified 2026-06-04): Avalara "Document vs Line level rounding"
FAQ; Stripe manual-rate rounding (line-item vs invoice level); Microsoft
Dynamics 365 / NetSuite tax-rounding-level settings; TaxJar on
per-jurisdiction line-vs-invoice rounding requirements.

## Addendum (2026-06-08): the rounding tie-break MODE is banker's (half-to-even), deliberately

The decision above fixed *where* rounding happens (document-level, Axis 1)
and *which line* absorbs the residual (largest-remainder, Axis 2), but never
recorded the **tie-break mode** of the rounding step itself. Stating it, since
an e2e tax-parity audit (2026-06-07) found it unstated and it is a real,
defensible choice — not an accident.

**What the code does.** Both the document total
(`money.RoundHalfToEven(taxableBase × ratePPM, 1_000_000)`) and the
largest-remainder share math use **banker's rounding (round-half-to-even)** —
the `internal/platform/money` package default — on the manual tax path, the
same mode used for proration and discount distribution across the engine.

**Why half-to-even, not statutory half-up.** Most indirect-tax authorities
specify round-**half-up** per transaction (HMRC VAT Notice 700 §17.5: "0.5 of
one penny or more … rounded up"; US SSUTA: third decimal > 4 rounds up). Velox
deliberately diverges to half-to-even, for the reason documented at
`internal/platform/money/round.go`: *half-up introduces a systematic positive
bias over large batches of monetary amounts — a measurable accounting drift
auditors flag; half-to-even averages to zero bias (IEEE 754 / Python `round()`
/ .NET `decimal` / GAAP-adjacent default).* Two defensible audit positions
exist — statutory per-transaction half-up vs anti-bias half-to-even — and Velox
takes the anti-bias one **consistently across the whole money path** (tax,
proration, discounts). Flipping only the tax path to half-up would reintroduce
exactly the bias `money/round.go` exists to avoid and desync tax rounding from
the rest of the engine.

**Impact is tiny.** The two modes differ only on an **exact half-cent tie**
(remainder = denom/2), which is rare; on every other input they agree. On such
a tie banker's can land 1¢ opposite to statutory half-up.

**Disposition: keep half-to-even; this addendum is the record.** No code
change. If a future remittance-correctness need ever requires statutory
per-transaction half-up (e.g. the manual provider gains a real filing path, or
a DP's jurisdiction audits to the ½-cent), that becomes its own ADR-level
decision that must reconcile with `money/round.go`'s anti-bias rationale and the
cross-path consistency — **not** a silent patch to `manual.go`. Same disposition
shape as the deferred `tax_rounding_level` setting above: a documented,
intentional choice with a named re-open trigger.

**Precision note on the "matches every reference engine" framing (Axis 2).**
The fully-verified claim is that the residual goes to the **smallest-error
line, never a positional one** — the old positional-last-line behavior was the
outlier. Of the surveyed engines, **Sovos** (highest-remainder, ties → first
line) is the *verbatim* match to Velox's largest-remainder + lowest-index tie-
break; Avalara/Dynamics "minimum percentage change" is the same minimum-
distortion *intent* that largest-remainder approximates (it minimizes absolute
cent error, not percentage change). Convergent principle, not four-way
bit-identical apportionment.

Source (verified 2026-06-07): `internal/platform/money/round.go` rationale;
HMRC VAT Notice 700 §17.5; Streamlined Sales and Use Tax Agreement rounding
rule; Sovos / Avalara / Dynamics residual-distribution docs.
