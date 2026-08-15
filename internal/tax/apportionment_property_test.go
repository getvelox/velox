package tax

import (
	"context"
	"math/rand/v2"
	"testing"

	"github.com/sagarsuperuser/velox/internal/platform/money"
)

// Property tests for tax calculation and penny apportionment.
//
// Tax apportionment is an unusually dangerous piece of arithmetic: every bug is
// a penny that either vanishes or is invented, and it appears only for specific
// combinations of amount, rate, discount and line count — exactly the shape
// example tests under-cover and generators cover well.
//
// Most properties below drive the PUBLIC Calculate entry point rather than the
// unexported distributeLargestRemainder helper. That is deliberate. The helper
// documents a precondition — "leftover is mathematically in [0, len(nums)]" —
// so a generator that feeds it a freely-chosen total tests a function the
// program never calls. A first draft of this file did exactly that and reported
// a conservation failure at total=546066 against 5 lines, which was the
// generator being out of domain, not a defect. Calculate has no such
// precondition: any Request a caller can construct is legal input, so a failure
// there is always real.

const taxIters = 20000

func taxRand() *rand.Rand { return rand.New(rand.NewPCG(0xFEE5, 0x9A17)) }

// genRequest builds a Request weighted toward the awkward cases: single lines,
// many lines, zero amounts, discounts that exceed the subtotal, and rates with
// full 4-decimal precision (the ppm scaling's reason to exist).
func genRequest(r *rand.Rand) (Request, float64) {
	n := r.IntN(8) + 1
	lines := make([]RequestLine, n)
	var subtotal int64
	for i := range lines {
		var amt int64
		switch r.IntN(6) {
		case 0:
			amt = 0
		case 1:
			amt = int64(r.IntN(100) + 1) // sub-dollar, where rounding bites hardest
		case 2:
			amt = int64(r.IntN(1_000_000_00)) // up to $1M
		default:
			amt = int64(r.IntN(100_000))
		}
		lines[i] = RequestLine{Ref: "line", AmountCents: amt, Quantity: 1}
		subtotal += amt
	}

	// Rates: common real ones plus arbitrary 4-decimal values.
	var rate float64
	switch r.IntN(4) {
	case 0:
		rate = []float64{8.875, 20, 19, 7.25, 5, 0.5}[r.IntN(6)]
	case 1:
		rate = 0 // no-tax short circuit
	default:
		rate = float64(r.IntN(300000)) / 10000 // 0.0000 .. 30.0000
	}

	var discount int64
	switch r.IntN(4) {
	case 0:
		discount = 0
	case 1:
		discount = subtotal + int64(r.IntN(1000)) // at or beyond the subtotal
	default:
		if subtotal > 0 {
			discount = int64(r.IntN(int(min(subtotal, 1_000_000))))
		}
	}

	return Request{
		Currency:      "usd",
		LineItems:     lines,
		DiscountCents: discount,
		TaxInclusive:  r.IntN(2) == 0,
	}, rate
}

// TestProperty_Calculate_LineTaxesSumToTotal is the guarantee the whole
// apportionment exists to provide, asserted where the invoice actually reads
// it. If the parts do not sum to the total, the invoice's tax lines contradict
// its tax row — a wrong invoice and an audit finding.
func TestProperty_Calculate_LineTaxesSumToTotal(t *testing.T) {
	r := taxRand()
	ctx := context.Background()
	for i := 0; i < taxIters; i++ {
		req, rate := genRequest(r)
		res, err := NewManualProvider(rate, "VAT").Calculate(ctx, req)
		if err != nil {
			t.Fatalf("Calculate returned an error for a well-formed request: %v", err)
		}

		var sum int64
		for _, l := range res.Lines {
			sum += l.TaxAmountCents
		}
		if sum != res.TotalTaxCents {
			t.Fatalf("line taxes do not sum to the invoice tax total: Σlines=%d total=%d\n"+
				"rate=%v inclusive=%v discount=%d lines=%+v",
				sum, res.TotalTaxCents, rate, req.TaxInclusive, req.DiscountCents, req.LineItems)
		}
	}
}

// TestProperty_Calculate_ShapeAndSign: one result line per request line, in
// order, and no negative tax from non-negative amounts. A negative tax line
// reads as a refund the customer did not earn.
func TestProperty_Calculate_ShapeAndSign(t *testing.T) {
	r := taxRand()
	ctx := context.Background()
	for i := 0; i < taxIters; i++ {
		req, rate := genRequest(r)
		res, err := NewManualProvider(rate, "VAT").Calculate(ctx, req)
		if err != nil {
			t.Fatalf("Calculate: %v", err)
		}

		if len(res.Lines) != len(req.LineItems) {
			t.Fatalf("got %d result lines for %d request lines", len(res.Lines), len(req.LineItems))
		}
		if res.TotalTaxCents < 0 {
			t.Fatalf("negative invoice tax total %d: rate=%v inclusive=%v discount=%d lines=%+v",
				res.TotalTaxCents, rate, req.TaxInclusive, req.DiscountCents, req.LineItems)
		}
		for j, l := range res.Lines {
			if l.TaxAmountCents < 0 {
				t.Fatalf("negative tax on line %d: %d (rate=%v inclusive=%v discount=%d lines=%+v)",
					j, l.TaxAmountCents, rate, req.TaxInclusive, req.DiscountCents, req.LineItems)
			}
		}
	}
}

// TestProperty_Calculate_TaxNeverExceedsTheTaxableBase is a sanity bound that
// no rounding path may violate: in exclusive mode tax is rate × base, and for
// any rate under 100% it must stay below the base it was computed from. A tax
// larger than the thing being taxed is the signature of a scaling error (ppm
// applied twice, or a denominator mixed up between the two modes).
func TestProperty_Calculate_TaxNeverExceedsTheTaxableBase(t *testing.T) {
	r := taxRand()
	ctx := context.Background()
	for i := 0; i < taxIters; i++ {
		req, rate := genRequest(r)
		if rate >= 100 {
			continue
		}
		req.TaxInclusive = false
		res, err := NewManualProvider(rate, "VAT").Calculate(ctx, req)
		if err != nil {
			t.Fatalf("Calculate: %v", err)
		}

		var subtotal int64
		for _, l := range req.LineItems {
			subtotal += l.AmountCents
		}
		base := subtotal - min(max(req.DiscountCents, 0), subtotal)
		if res.TotalTaxCents > base {
			t.Fatalf("tax %d exceeds taxable base %d at rate %v%%: discount=%d lines=%+v",
				res.TotalTaxCents, base, rate, req.DiscountCents, req.LineItems)
		}
	}
}

// TestProperty_Calculate_FullyDiscountedInvoiceIsUntaxed: when the discount
// wipes out the subtotal there is nothing to tax, in either mode. This is the
// branch that returns early, and it is worth pinning because "discount larger
// than subtotal" is a real operator action (a credit applied to a shrinking
// subscription) and an unclamped version taxes a negative base.
func TestProperty_Calculate_FullyDiscountedInvoiceIsUntaxed(t *testing.T) {
	r := taxRand()
	ctx := context.Background()
	for i := 0; i < taxIters/4; i++ {
		req, rate := genRequest(r)
		var subtotal int64
		for _, l := range req.LineItems {
			subtotal += l.AmountCents
		}
		req.DiscountCents = subtotal + int64(r.IntN(10_000))

		res, err := NewManualProvider(rate, "VAT").Calculate(ctx, req)
		if err != nil {
			t.Fatalf("Calculate: %v", err)
		}
		if res.TotalTaxCents != 0 {
			t.Fatalf("fully discounted invoice was taxed %d: rate=%v inclusive=%v subtotal=%d discount=%d",
				res.TotalTaxCents, rate, req.TaxInclusive, subtotal, req.DiscountCents)
		}
	}
}

// TestProperty_Calculate_ExemptAndReverseChargeAreUntaxed: the two customer
// statuses that zero tax must do so regardless of rate, amounts or mode, and
// must set the flag the invoice PDF renders its legend from. Tax charged to a
// reverse-charge buyer is a double-taxation incident, not a rounding penny.
func TestProperty_Calculate_ExemptAndReverseChargeAreUntaxed(t *testing.T) {
	r := taxRand()
	ctx := context.Background()
	for i := 0; i < taxIters/4; i++ {
		req, rate := genRequest(r)
		for _, st := range []CustomerTaxStatus{StatusExempt, StatusReverseCharge} {
			req.CustomerStatus = st
			res, err := NewManualProvider(rate, "VAT").Calculate(ctx, req)
			if err != nil {
				t.Fatalf("Calculate: %v", err)
			}
			if res.TotalTaxCents != 0 {
				t.Fatalf("status %s was taxed %d (rate=%v)", st, res.TotalTaxCents, rate)
			}
			for j, l := range res.Lines {
				if l.TaxAmountCents != 0 {
					t.Fatalf("status %s taxed line %d by %d", st, j, l.TaxAmountCents)
				}
			}
			if st == StatusReverseCharge && !res.ReverseCharge {
				t.Fatalf("reverse-charge result did not set the ReverseCharge flag — the invoice legend depends on it")
			}
			if st == StatusExempt && !res.Exempt {
				t.Fatalf("exempt result did not set the Exempt flag — the invoice legend depends on it")
			}
		}
	}
}

// TestProperty_Calculate_RateIsMonotonic: raising the rate must never lower the
// tax. Monotonicity is the cheapest available check on the ppm scaling — it
// fails loudly if precision is truncated somewhere in the chain, because a
// truncating step makes tax flat (and then non-monotonic) across nearby rates.
func TestProperty_Calculate_RateIsMonotonic(t *testing.T) {
	r := taxRand()
	ctx := context.Background()
	for i := 0; i < taxIters/4; i++ {
		req, _ := genRequest(r)
		prev := int64(-1)
		prevRate := 0.0
		for _, rate := range []float64{0, 0.5, 1, 5, 8.875, 19, 20, 27} {
			res, err := NewManualProvider(rate, "VAT").Calculate(ctx, req)
			if err != nil {
				t.Fatalf("Calculate: %v", err)
			}
			if res.TotalTaxCents < prev {
				t.Fatalf("raising the rate lowered the tax: %v%%→%d then %v%%→%d (inclusive=%v discount=%d lines=%+v)",
					prevRate, prev, rate, res.TotalTaxCents, req.TaxInclusive, req.DiscountCents, req.LineItems)
			}
			prev, prevRate = res.TotalTaxCents, rate
		}
	}
}

// TestProperty_Calculate_RejectsNegativeLineAmounts pins the guard added after
// this file's first mutation run. A negative line amount used to be clamped to
// zero per-line while still counting toward the invoice total, so the residual
// exceeded what the largest-remainder loop can move: +100.00 and −30.00 at 20%
// produced a 14.00 total against line taxes of [19.99, −0.01].
//
// billing.collapseTaxRequestLines upholds the invariant upstream (issue #556);
// this asserts the provider no longer answers silently when something reaches
// it directly.
func TestProperty_Calculate_RejectsNegativeLineAmounts(t *testing.T) {
	r := taxRand()
	ctx := context.Background()
	for i := 0; i < taxIters/4; i++ {
		req, rate := genRequest(r)
		// Make exactly one line negative.
		j := r.IntN(len(req.LineItems))
		req.LineItems[j].AmountCents = -int64(r.IntN(100_000) + 1)

		res, err := NewManualProvider(rate, "VAT").Calculate(ctx, req)
		if err == nil {
			t.Fatalf("negative line %d (%d) was accepted: total=%d lines=%+v",
				j, req.LineItems[j].AmountCents, res.TotalTaxCents, res.Lines)
		}
	}
}

// TestProperty_Calculate_TaxIsTheNEARESTCent is the property that distinguishes
// banker's rounding from truncation. Conservation and monotonicity both survive
// a truncating divide — the apportionment simply redistributes whatever total
// it is handed — so neither can protect ADR-042/043's rounding decision. Being
// within HALF a cent of the exact tax is what only correct rounding satisfies.
func TestProperty_Calculate_TaxIsTheNEARESTCent(t *testing.T) {
	r := taxRand()
	ctx := context.Background()
	for i := 0; i < taxIters; i++ {
		req, rate := genRequest(r)
		m := NewManualProvider(rate, "VAT")
		if m.ratePPM <= 0 {
			continue
		}
		res, err := m.Calculate(ctx, req)
		if err != nil {
			t.Fatalf("Calculate: %v", err)
		}

		var subtotal int64
		for _, l := range req.LineItems {
			subtotal += l.AmountCents
		}
		base := subtotal - min(max(req.DiscountCents, 0), subtotal)
		if base <= 0 {
			continue
		}

		// Exact tax is base×ppm/den (exclusive) or base×ppm/(1e6+ppm)
		// (inclusive). Assert |tax·den − base·ppm|·2 ≤ den, i.e. the result is
		// the nearest whole cent to the exact value.
		den := int64(1_000_000)
		if req.TaxInclusive {
			den = 1_000_000 + m.ratePPM
		}
		err2 := res.TotalTaxCents*den - base*m.ratePPM
		if err2 < 0 {
			err2 = -err2
		}
		if err2*2 > den {
			t.Fatalf("tax is not the nearest cent: got=%d exact=%d/%d off-by=%.4f cents\n"+
				"rate=%v inclusive=%v base=%d discount=%d",
				res.TotalTaxCents, base*m.ratePPM, den, float64(err2)/float64(den),
				rate, req.TaxInclusive, base, req.DiscountCents)
		}
	}
}

// --- helper-level properties -------------------------------------------------
//
// These drive distributeLargestRemainder directly, but construct their inputs
// the way calculateExclusive does, so the documented precondition holds. The
// point of testing the helper separately is the fairness properties below,
// which are invisible from Calculate: conservation alone does not imply a
// sensible split, and handing the entire total to line 0 would satisfy every
// property in the section above.

// genExclusiveShares mirrors calculateExclusive exactly: nums[i] = base × ppm,
// den = 1_000_000, total = round-half-even(Σbase × ppm / 1_000_000).
func genExclusiveShares(r *rand.Rand) (total int64, nums []int64, den int64) {
	n := r.IntN(8) + 1
	nums = make([]int64, n)
	den = 1_000_000
	ppm := int64(r.IntN(300000))

	var base int64
	for i := range nums {
		var amt int64
		switch r.IntN(4) {
		case 0:
			amt = 0
		case 1:
			amt = int64(r.IntN(100) + 1)
		default:
			amt = int64(r.IntN(100_000))
		}
		nums[i] = amt * ppm
		base += amt
	}
	return money.RoundHalfToEven(base*ppm, den), nums, den
}

// TestProperty_Apportion_ConservesTheTotal restates the conservation guarantee
// at the helper level, under the precondition the helper documents.
func TestProperty_Apportion_ConservesTheTotal(t *testing.T) {
	r := taxRand()
	for i := 0; i < taxIters; i++ {
		total, nums, den := genExclusiveShares(r)
		out := distributeLargestRemainder(total, nums, den)

		var sum int64
		for _, v := range out {
			sum += v
		}
		if sum != total {
			t.Fatalf("apportionment lost or invented money: total=%d Σout=%d nums=%v den=%d out=%v",
				total, sum, nums, den, out)
		}
	}
}

// TestProperty_Apportion_NeverInvertsLineOrder is the inversion the doc says
// the largest-remainder rule exists to prevent: a line with a LARGER exact
// share must never be allocated LESS than a line with a smaller one. The
// previous "dump the residual on the positionally-last line" shortcut produced
// exactly that.
func TestProperty_Apportion_NeverInvertsLineOrder(t *testing.T) {
	r := taxRand()
	for i := 0; i < taxIters; i++ {
		total, nums, den := genExclusiveShares(r)
		out := distributeLargestRemainder(total, nums, den)

		for a := range nums {
			for b := range nums {
				if nums[a] > nums[b] && out[a] < out[b] {
					t.Fatalf("larger share allocated less: line %d (share %d) got %d, line %d (share %d) got %d\n"+
						"total=%d den=%d nums=%v out=%v",
						a, nums[a], out[a], b, nums[b], out[b], total, den, nums, out)
				}
			}
		}
	}
}

// TestProperty_Apportion_WithinOneCentOfExactShare is the minimum-distortion
// half of the largest-remainder promise. Conservation alone does not imply
// fairness — handing the whole total to line 0 also conserves it — so this
// pins that every line lands within one cent of its exact proportional share.
func TestProperty_Apportion_WithinOneCentOfExactShare(t *testing.T) {
	r := taxRand()
	for i := 0; i < taxIters; i++ {
		total, nums, den := genExclusiveShares(r)
		out := distributeLargestRemainder(total, nums, den)

		for j := range nums {
			// |out[j]·den − nums[j]| < den ⇔ out[j] is within one whole cent
			// of the exact share nums[j]/den.
			diff := out[j]*den - nums[j]
			if diff < 0 {
				diff = -diff
			}
			if diff >= den {
				t.Fatalf("line %d is more than a cent from its exact share: got=%d exact=%d/%d\n"+
					"total=%d nums=%v out=%v", j, out[j], nums[j], den, total, nums, out)
			}
		}
	}
}

// TestApportion_TiesGoToTheLowestIndex is an example, not a property, because
// the behaviour it pins is a convention rather than an invariant: on equal
// remainders the residual cent goes to the earliest line ("added to the first
// line" — Sovos; the doc on distributeLargestRemainder cites it).
//
// It is here for reproducibility, not for a cent. The retry family
// (operator retry-tax, the tax_retry reconciler, the clock variant) recomputes
// tax from the STORED lines, so a tie-break that changed would move a cent
// between lines on a recalculation of an unchanged invoice.
//
// Two other mutations of this function survived the property suite —
// removing the discount clamp and the negative-share clamp — and deliberately
// get no test: both are unreachable, and a 50,000-request fingerprint of every
// Result was byte-identical with and without them.
func TestApportion_TiesGoToTheLowestIndex(t *testing.T) {
	// Three identical shares of 1/3 each, one cent of residual: line 0 takes it.
	got := distributeLargestRemainder(1, []int64{100, 100, 100}, 300)
	want := []int64{1, 0, 0}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("tie-break moved the residual cent: got %v, want %v", got, want)
		}
	}
}

// TestProperty_Apportion_DegenerateInputs covers the guards: no lines, and a
// non-positive denominator. Both must return a zeroed result rather than
// dividing by zero.
func TestProperty_Apportion_DegenerateInputs(t *testing.T) {
	if got := distributeLargestRemainder(100, nil, 10); len(got) != 0 {
		t.Fatalf("no lines must yield no allocations, got %v", got)
	}
	for _, den := range []int64{0, -1, -1000} {
		got := distributeLargestRemainder(100, []int64{5, 5}, den)
		for _, v := range got {
			if v != 0 {
				t.Fatalf("non-positive denominator %d must allocate nothing, got %v", den, got)
			}
		}
	}
}
