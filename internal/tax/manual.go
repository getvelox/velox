package tax

import (
	"context"
	"fmt"
	"math"
	"sort"

	"github.com/sagarsuperuser/velox/internal/platform/money"
)

// ManualProvider applies a single tenant-configured tax rate (basis points)
// uniformly across every line item. Deliberately simple: no jurisdiction
// lookup, no per-product tax codes, no cross-border auto zero-rating.
// Tenants who need jurisdictional accuracy pick stripe_tax.
//
// Inclusive vs exclusive: honoured via Request.TaxInclusive. In exclusive
// mode the line amount is net and tax is added on top; in inclusive mode
// the line amount is gross and tax is carved out so subtotal + tax = gross.
//
// Exempt / reverse-charge routing lives one layer up: Calculate honours
// req.CustomerStatus and returns a zeroed Result with the appropriate flag
// set, so the invoice PDF renders the right legend regardless of provider.
//
// References for the flat-rate shape: Stripe's legacy tax_rates object,
// Lago's integration-agnostic tax model, Chargebee's manual tax rules. All
// three concluded that the only safe manual behaviour is "apply what the
// tenant configured" — inferring cross-border zero-rating silently
// produces wrong invoices under OSS/OIDAR and in most US sales-tax regimes.
type ManualProvider struct {
	rate    float64 // Percent rate (4-decimal precision). ADR-042/043.
	ratePPM int64   // Same rate scaled to parts-per-million (1 ppm = 0.0001%) for integer math without precision loss.
	taxName string
}

// NewManualProvider returns a provider that applies rate uniformly.
// rate is in percent (4-decimal precision; e.g. 8.875 for NYC).
// taxName is the label shown on the invoice tax row ("VAT", "Sales Tax", …).
// ADR-042/043: switched from integer basis-points to NUMERIC(7,4) percent.
// The provider stores BOTH the float (for ResultLine.TaxRate display) and
// the integer ppm scaling (for line-tax math without float drift).
func NewManualProvider(rate float64, taxName string) *ManualProvider {
	if rate < 0 {
		rate = 0
	}
	return &ManualProvider{
		rate:    rate,
		ratePPM: int64(math.Round(rate * 10000)), // 8.875% → 88750 ppm
		taxName: taxName,
	}
}

func (*ManualProvider) Name() string { return "manual" }

func (m *ManualProvider) Calculate(_ context.Context, req Request) (*Result, error) {
	switch req.CustomerStatus {
	case StatusExempt:
		return exemptResult("manual", req, false, req.CustomerExemptReason), nil
	case StatusReverseCharge:
		return exemptResult("manual", req, true, ""), nil
	}

	// A negative line amount is out of contract, and the failure it used to
	// produce was silent: the per-line base is clamped at zero, so the clamped
	// line's share drops out of the apportionment while the invoice-level total
	// still counts it. The residual then exceeds the one-cent-per-line the
	// largest-remainder loop can move, and the result is line taxes that miss
	// the total by an arbitrary amount AND a negative tax line — e.g. lines of
	// +100.00 and −30.00 at 20% return a 14.00 total against line taxes of
	// [19.99, −0.01].
	//
	// billing.collapseTaxRequestLines is the chokepoint that upholds the
	// invariant (issue #556). This is not a second guard for the same failure:
	// it converts the remaining silent-wrong-invoice mode into a loud one for
	// any caller that reaches a provider directly.
	for i, li := range req.LineItems {
		if li.AmountCents < 0 {
			return nil, fmt.Errorf("tax: line %d (%q) has a negative amount (%d) — tax providers must be given net non-negative lines", i, li.Ref, li.AmountCents)
		}
	}

	lines := make([]ResultLine, len(req.LineItems))
	if m.ratePPM <= 0 || len(req.LineItems) == 0 {
		for i, li := range req.LineItems {
			lines[i] = ResultLine{Ref: li.Ref, NetAmountCents: li.AmountCents}
		}
		return &Result{Provider: "manual", Lines: lines}, nil
	}

	subtotal := int64(0)
	for _, li := range req.LineItems {
		subtotal += li.AmountCents
	}

	if req.TaxInclusive {
		return m.calculateInclusive(req, lines, subtotal), nil
	}
	return m.calculateExclusive(req, lines, subtotal), nil
}

// calculateExclusive: line amounts are net; tax is added on top.
// Discount is distributed proportionally across lines before rate is applied.
func (m *ManualProvider) calculateExclusive(req Request, lines []ResultLine, subtotal int64) *Result {
	discount := min(max(req.DiscountCents, 0), subtotal)
	taxableBase := subtotal - discount
	if taxableBase <= 0 {
		for i, li := range req.LineItems {
			lines[i] = ResultLine{Ref: li.Ref, NetAmountCents: li.AmountCents}
		}
		return &Result{Provider: "manual", Lines: lines}
	}

	// Integer math via parts-per-million (1 ppm = 0.0001%). 8.875%
	// = 88750 ppm; tax = base × ppm / 1_000_000. Preserves 4-decimal
	// precision without float drift.
	totalTax := money.RoundHalfToEven(taxableBase*m.ratePPM, 1_000_000)

	// Per-line taxable base after proportional discount. The exact (unrounded)
	// per-line tax is base × ppm / 1_000_000; we hand those exact shares to
	// largest-remainder apportionment so the rounded line taxes sum to totalTax.
	nums := make([]int64, len(req.LineItems))
	var discountApplied int64
	for i, li := range req.LineItems {
		var linePortion int64
		if i == len(req.LineItems)-1 {
			linePortion = discount - discountApplied
		} else {
			linePortion = money.RoundHalfToEven(discount*li.AmountCents, subtotal)
			discountApplied += linePortion
		}
		lineBase := max(li.AmountCents-linePortion, 0)
		nums[i] = lineBase * m.ratePPM
	}
	lineTaxes := distributeLargestRemainder(totalTax, nums, 1_000_000)

	for i, li := range req.LineItems {
		lines[i] = ResultLine{
			Ref:            li.Ref,
			NetAmountCents: li.AmountCents,
			TaxAmountCents: lineTaxes[i],
			TaxRate:        m.rate, // ADR-042/043
			TaxName:        m.taxName,
		}
	}

	return m.wrap(totalTax, lines)
}

// calculateInclusive: line amounts are gross (tax-inclusive). Net and tax
// are carved out per line so engine-side subtotal == sum(Net) and
// subtotal + tax == sum(original gross).
//
// Discount is applied in GROSS terms (matches the semantics tenants expect
// from inclusive pricing: "10% off sticker"). Net and tax are derived from
// the post-discount gross base.
func (m *ManualProvider) calculateInclusive(req Request, lines []ResultLine, subtotal int64) *Result {
	discount := min(max(req.DiscountCents, 0), subtotal)
	taxableGross := subtotal - discount
	if taxableGross <= 0 {
		for i, li := range req.LineItems {
			lines[i] = ResultLine{Ref: li.Ref, NetAmountCents: li.AmountCents}
		}
		return &Result{Provider: "manual", Lines: lines}
	}

	// Inclusive math: gross = net × (1 + rate). With rate in ppm
	// scaled to 1_000_000-base, denom = 1_000_000 + ratePPM.
	denom := int64(1_000_000 + m.ratePPM)
	totalTax := taxableGross - money.RoundHalfToEven(taxableGross*1_000_000, denom)

	// Exact per-line inclusive tax is grossBase × ratePPM / denom; feed those
	// exact shares to largest-remainder apportionment so rounded line taxes sum
	// to totalTax.
	nums := make([]int64, len(req.LineItems))
	var discountApplied int64
	for i, li := range req.LineItems {
		var linePortion int64
		if i == len(req.LineItems)-1 {
			linePortion = discount - discountApplied
		} else {
			linePortion = money.RoundHalfToEven(discount*li.AmountCents, subtotal)
			discountApplied += linePortion
		}
		lineGrossBase := max(li.AmountCents-linePortion, 0)
		nums[i] = lineGrossBase * m.ratePPM
	}
	lineTaxes := distributeLargestRemainder(totalTax, nums, denom)

	for i, li := range req.LineItems {
		// Full-line (undiscounted) net for NetAmountCents so the engine's
		// stored subtotal is undiscounted-net and DiscountCents is net-units,
		// matching what the invoice shows.
		lineNetUndisc := money.RoundHalfToEven(li.AmountCents*1_000_000, denom)

		lines[i] = ResultLine{
			Ref:            li.Ref,
			NetAmountCents: lineNetUndisc,
			TaxAmountCents: lineTaxes[i],
			TaxRate:        m.rate, // ADR-042/043
			TaxName:        m.taxName,
		}
	}

	return m.wrap(totalTax, lines)
}

func (m *ManualProvider) wrap(totalTax int64, lines []ResultLine) *Result {
	return &Result{
		Provider:      "manual",
		TotalTaxCents: totalTax,
		EffectiveRate: m.rate, // ADR-042/043
		TaxName:       m.taxName,
		Lines:         lines,
		Breakdowns: []Breakdown{{
			Name:        m.taxName,
			Rate:        m.rate,
			AmountCents: totalTax,
		}},
	}
}

// distributeLargestRemainder allocates total across len(nums) lines using the
// largest-remainder (a.k.a. minimum-distortion) method. Each line's exact share
// is nums[i]/den; it receives floor(nums[i]/den), then the leftover cents
// (total − Σfloor) are handed out one at a time to the lines with the largest
// fractional remainders, ties broken by lowest index.
//
// This is the residual-distribution rule every reference indirect-tax engine
// converges on (Sovos: "split among the lines that have the highest remainder…
// added to the first line" on ties; Avalara / Dynamics 365: "the line that
// results in the minimum percentage change"). It guarantees Σ(line tax) == total
// while never docking a larger-base line below a smaller-base one — the
// inversion the previous "dump the residual on the positionally-last line"
// shortcut produced when the residual was negative.
func distributeLargestRemainder(total int64, nums []int64, den int64) []int64 {
	out := make([]int64, len(nums))
	if len(nums) == 0 || den <= 0 {
		return out
	}

	type rem struct {
		idx       int
		remainder int64
	}
	rems := make([]rem, len(nums))
	var allocated int64
	for i, n := range nums {
		if n < 0 {
			n = 0
		}
		out[i] = n / den
		rems[i] = rem{idx: i, remainder: n % den}
		allocated += out[i]
	}

	// leftover is mathematically in [0, len(nums)] (Σfloor ≤ floor(Σ) ≤ total);
	// the clawback branch is defensive against an upstream total that doesn't
	// match the supplied shares.
	leftover := total - allocated
	if leftover > 0 {
		sort.SliceStable(rems, func(a, b int) bool {
			if rems[a].remainder != rems[b].remainder {
				return rems[a].remainder > rems[b].remainder // largest remainder first
			}
			return rems[a].idx < rems[b].idx // tie → lowest index
		})
		for k := int64(0); k < leftover && int(k) < len(rems); k++ {
			out[rems[k].idx]++
		}
	} else if leftover < 0 {
		sort.SliceStable(rems, func(a, b int) bool {
			if rems[a].remainder != rems[b].remainder {
				return rems[a].remainder < rems[b].remainder // smallest remainder first
			}
			return rems[a].idx < rems[b].idx
		})
		for k := int64(0); k < -leftover && int(k) < len(rems); k++ {
			out[rems[k].idx]--
		}
	}

	return out
}

func (*ManualProvider) Commit(_ context.Context, _, _ string) (string, error) { return "", nil }

// Reverse is a no-op — manual providers have no upstream tax_transaction
// to reverse. The credit note flow treats an empty ReversalResult.
// TransactionID as "nothing to record" and proceeds.
func (*ManualProvider) Reverse(_ context.Context, _ ReversalRequest) (*ReversalResult, error) {
	return &ReversalResult{}, nil
}
