package subscription

import (
	"math/rand/v2"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// Property tests for the proration arithmetic.
//
// The existing proration tests are example-based: ~44 cases across 14 files,
// each pinning a specific input to a specific output. They are good tests and
// they encode real regressions. What they cannot do is explore the inputs
// nobody thought of, and that is where this codebase's arithmetic bugs have
// actually come from — the ADR-091 timezone-seam overbill, the Jan-31→Feb-28
// clamp, the $4.43 proration floor were all found by walking flows by hand,
// not by the suite.
//
// So these assert PROPERTIES over generated inputs instead of outputs over
// chosen ones. Each property below is either (a) explicitly promised by a doc
// comment in proration_math.go, or (b) a relation that must hold for the
// result to be money-safe. A failure here is a real defect, not a changed
// expectation.
//
// Deliberately using a seeded generator over math/rand/v2 rather than adding a
// property-testing dependency: the generators need domain constraints
// (remaining <= total, positive periods, realistic magnitudes) that are
// clearer written directly, and a fixed seed makes any failure reproducible.

const propIters = 20000

// genRand returns a deterministically seeded source. A fixed seed means a
// failure is reproducible from the test output alone; bump the seed to widen
// the search rather than making it time-based, which would make failures
// unreproducible.
func genRand() *rand.Rand { return rand.New(rand.NewPCG(0x5EED, 0xB1115)) }

// realistic money magnitudes in cents: sub-cent edge cases up to ~$10M lines.
func genAmount(r *rand.Rand) int64 {
	switch r.IntN(4) {
	case 0:
		return int64(r.IntN(100)) // 0..99c — rounding lives here
	case 1:
		return int64(r.IntN(100_000)) // up to $1,000
	case 2:
		return int64(r.IntN(100_000_000)) // up to $1M
	default:
		return int64(r.IntN(1_000_000_000)) // up to $10M
	}
}

// genPeriod returns (remainingDays, totalDays) with 0 <= remaining <= total,
// total >= 1 — the domain the callers actually pass. Yearly cycles reach 366.
func genPeriod(r *rand.Rand) (int64, int64) {
	total := int64(r.IntN(366) + 1)
	return int64(r.IntN(int(total) + 1)), total
}

// TestProperty_ProrationCents_FullPeriodIsFullDelta: when no time has elapsed
// (remaining == total) the customer is charged the entire difference. Any
// scaling error shows up immediately here because the ratio is exactly 1.
func TestProperty_ProrationCents_FullPeriodIsFullDelta(t *testing.T) {
	r := genRand()
	for i := 0; i < propIters; i++ {
		oldA, newA := genAmount(r), genAmount(r)
		_, total := genPeriod(r)
		if got, want := prorationCents(oldA, newA, total, total), newA-oldA; got != want {
			t.Fatalf("remaining==total must yield the whole delta: old=%d new=%d total=%d got=%d want=%d",
				oldA, newA, total, got, want)
		}
	}
}

// TestProperty_ProrationCents_NoTimeNoMoney: a change with zero remaining days
// prorates to nothing. The customer is not charged for time they will not use.
func TestProperty_ProrationCents_NoTimeNoMoney(t *testing.T) {
	r := genRand()
	for i := 0; i < propIters; i++ {
		oldA, newA := genAmount(r), genAmount(r)
		_, total := genPeriod(r)
		if got := prorationCents(oldA, newA, 0, total); got != 0 {
			t.Fatalf("zero remaining days must prorate to 0: old=%d new=%d total=%d got=%d",
				oldA, newA, total, got)
		}
	}
}

// TestProperty_ProrationCents_SameAmountIsFree: swapping to a plan that costs
// the same must never move money, at any point in the cycle. A sign or
// rounding bug that survives the examples would surface here.
func TestProperty_ProrationCents_SameAmountIsFree(t *testing.T) {
	r := genRand()
	for i := 0; i < propIters; i++ {
		amt := genAmount(r)
		rem, total := genPeriod(r)
		if got := prorationCents(amt, amt, rem, total); got != 0 {
			t.Fatalf("equal amounts must prorate to 0: amt=%d remaining=%d total=%d got=%d",
				amt, rem, total, got)
		}
	}
}

// TestProperty_ProrationCents_Antisymmetric: upgrading A→B and downgrading B→A
// at the same instant must be equal and opposite. If they are not, a customer
// could be moved back and forth to extract or lose money.
func TestProperty_ProrationCents_Antisymmetric(t *testing.T) {
	r := genRand()
	for i := 0; i < propIters; i++ {
		a, b := genAmount(r), genAmount(r)
		rem, total := genPeriod(r)
		up := prorationCents(a, b, rem, total)
		down := prorationCents(b, a, rem, total)
		if up != -down {
			t.Fatalf("A→B and B→A must be equal and opposite: a=%d b=%d remaining=%d total=%d up=%d down=%d",
				a, b, rem, total, up, down)
		}
	}
}

// TestProperty_ProrationCents_SignAndBound: an upgrade never credits, a
// downgrade never charges, and neither ever exceeds the full-period delta.
// The bound is what stops a partial period costing more than a whole one.
func TestProperty_ProrationCents_SignAndBound(t *testing.T) {
	r := genRand()
	for i := 0; i < propIters; i++ {
		oldA, newA := genAmount(r), genAmount(r)
		rem, total := genPeriod(r)
		got := prorationCents(oldA, newA, rem, total)
		delta := newA - oldA
		switch {
		case delta > 0 && got < 0:
			t.Fatalf("upgrade produced a credit: old=%d new=%d rem=%d total=%d got=%d", oldA, newA, rem, total, got)
		case delta < 0 && got > 0:
			t.Fatalf("downgrade produced a charge: old=%d new=%d rem=%d total=%d got=%d", oldA, newA, rem, total, got)
		}
		if abs64(got) > abs64(delta) {
			t.Fatalf("partial period exceeded the full delta: old=%d new=%d rem=%d total=%d got=%d delta=%d",
				oldA, newA, rem, total, got, delta)
		}
	}
}

// TestProperty_ProrationCents_MonotoneInRemainingDays: more remaining time
// never costs less. A tier/rounding bug that inverts this would let a customer
// pay less by changing plans EARLIER in the cycle.
func TestProperty_ProrationCents_MonotoneInRemainingDays(t *testing.T) {
	r := genRand()
	for i := 0; i < propIters; i++ {
		oldA, newA := genAmount(r), genAmount(r)
		if newA <= oldA {
			continue // fix the sign so "more" is unambiguous
		}
		_, total := genPeriod(r)
		r1 := int64(r.IntN(int(total) + 1))
		r2 := int64(r.IntN(int(total) + 1))
		if r1 > r2 {
			r1, r2 = r2, r1
		}
		a := prorationCents(oldA, newA, r1, total)
		b := prorationCents(oldA, newA, r2, total)
		if a > b {
			t.Fatalf("fewer remaining days cost MORE: old=%d new=%d total=%d r1=%d->%d r2=%d->%d",
				oldA, newA, total, r1, a, r2, b)
		}
	}
}

// TestProperty_SplitUpgradeProration_ConservesNetAndTax verifies the guarantee
// the implementation's own doc comment makes:
//
//	"guarantees creditCents+chargeCents == netCents and
//	 creditTax+chargeTax == taxCents EXACTLY (int64) ... the split can never
//	 drift ±1 cent from it"
//
// This is the property that keeps an invoice's subtotal and tax unchanged when
// a single net proration is displayed as two lines. A ±1c drift here is a
// visible, customer-facing wrong total, and it is exactly the kind of defect
// that independent rounding of both halves would introduce.
func TestProperty_SplitUpgradeProration_ConservesNetAndTax(t *testing.T) {
	r := genRand()
	for i := 0; i < propIters; i++ {
		oldAmount := genAmount(r)
		rem, denom := genPeriod(r)
		net := int64(r.IntN(10_000_000)) + 1 // callers pass net > 0 (upgrade branch)
		tax := int64(r.IntN(3_000_000))      // 0 for none/manual/deferred

		credit, charge, creditTax, chargeTax := splitUpgradeProration(oldAmount, rem, denom, net, tax)

		if credit+charge != net {
			t.Fatalf("net not conserved: old=%d rem=%d denom=%d net=%d tax=%d credit=%d charge=%d sum=%d",
				oldAmount, rem, denom, net, tax, credit, charge, credit+charge)
		}
		if creditTax+chargeTax != tax {
			t.Fatalf("tax not conserved: old=%d rem=%d denom=%d net=%d tax=%d creditTax=%d chargeTax=%d sum=%d",
				oldAmount, rem, denom, net, tax, creditTax, chargeTax, creditTax+chargeTax)
		}
	}
}

// TestProperty_SplitUpgradeProration_CreditIsNeverPositive: the credit line
// represents unused time on the OLD plan and is displayed as a negative
// amount. A positive "credit" would read as an extra charge on the invoice.
func TestProperty_SplitUpgradeProration_CreditIsNeverPositive(t *testing.T) {
	r := genRand()
	for i := 0; i < propIters; i++ {
		oldAmount := genAmount(r)
		rem, denom := genPeriod(r)
		net := int64(r.IntN(10_000_000)) + 1
		credit, _, _, _ := splitUpgradeProration(oldAmount, rem, denom, net, 0)
		if credit > 0 {
			t.Fatalf("credit line must not be positive: old=%d rem=%d denom=%d net=%d credit=%d",
				oldAmount, rem, denom, net, credit)
		}
	}
}

// TestProperty_GrossUpByInvoiceRatio_IdentityWithoutTax: an invoice carrying
// no tax must gross up to itself. Any drift here would silently inflate every
// clawback credit on zero-rated invoices.
func TestProperty_GrossUpByInvoiceRatio_IdentityWithoutTax(t *testing.T) {
	r := genRand()
	for i := 0; i < propIters; i++ {
		net := genAmount(r)
		total := genAmount(r)
		if got := grossUpByInvoiceRatio(net, 0, total); got != net {
			t.Fatalf("no-tax invoice must gross up to identity: net=%d total=%d got=%d", net, total, got)
		}
		if got := grossUpByInvoiceRatio(net, net, net); got != net {
			t.Fatalf("subtotal==total must be identity: net=%d got=%d", net, got)
		}
	}
}

// TestProperty_ProrationCents_RoundsToNearestNotToward0 closes a gap mutation
// testing exposed: every earlier property also passes if banker's rounding is
// replaced by integer truncation, because truncation is antisymmetric too
// (trunc(-x) == -trunc(x)) and stays within the same bounds. But truncation is
// BIASED TOWARD ZERO — it systematically under-charges every upgrade and
// under-credits every downgrade by up to a cent, on every invoice, forever.
//
// The distinguishing property is accuracy against the exact rational: the
// result must be the NEAREST integer, so it can never be more than half a cent
// from the true value. Truncation violates this whenever the remainder exceeds
// half the denominator.
func TestProperty_ProrationCents_RoundsToNearestNotToward0(t *testing.T) {
	r := genRand()
	for i := 0; i < propIters; i++ {
		oldA, newA := genAmount(r), genAmount(r)
		rem, total := genPeriod(r)
		got := prorationCents(oldA, newA, rem, total)

		// Compare 2*got*total against 2*numerator: staying in integers avoids
		// introducing the float error we are trying to detect.
		num := (newA - oldA) * rem
		diff := got*total - num // exact error, scaled by total
		if abs64(diff)*2 > total {
			t.Fatalf("result is not the nearest integer (truncation-style bias): "+
				"old=%d new=%d rem=%d total=%d got=%d exact=%d/%d err*total=%d",
				oldA, newA, rem, total, got, num, total, diff)
		}
	}
}

// TestProperty_ProrationCents_DegenerateAndOverRangePeriods covers inputs the
// earlier generator never produced — mutation testing showed the totalDays<=0
// guard could be removed without a single test noticing, because genPeriod
// always returned total >= 1.
func TestProperty_ProrationCents_DegenerateAndOverRangePeriods(t *testing.T) {
	r := genRand()
	for i := 0; i < 2000; i++ {
		oldA, newA := genAmount(r), genAmount(r)
		// A zero or negative denominator means "no period to prorate over".
		// It must return 0, not panic and not divide.
		for _, total := range []int64{0, -1, -365} {
			if got := prorationCents(oldA, newA, int64(r.IntN(400)), total); got != 0 {
				t.Fatalf("non-positive totalDays must prorate to 0: old=%d new=%d total=%d got=%d",
					oldA, newA, total, got)
			}
		}
	}
}

// TestProperty_FullBillingCycleDays_SaneAcrossZonesAndAnchors targets the code
// where this codebase's proration bugs have actually lived. The properties
// above exercise prorationCents, which is pure integer arithmetic; ADR-091's
// timezone-seam overbill and the Jan-31→Feb-28 clamp were both in the DATE
// MATH that produces the denominator.
//
// The anchor is generated ALIGNED with the period start, because that is the
// only state the writers produce: domain.AnchorDayFor derives the anchor from
// periodStart.Day(), and both call sites (subscription create, and the plan
// swap which re-anchors to `now` while setting CurrentBillingPeriodStart=now)
// keep them consistent. Generating arbitrary anchors would test a state the
// system cannot reach — see the hazard test below for why that distinction
// matters.
func TestProperty_FullBillingCycleDays_SaneAcrossZonesAndAnchors(t *testing.T) {
	zones := []string{"UTC", "Asia/Kolkata", "America/New_York", "Pacific/Chatham", "Australia/Lord_Howe"}
	locs := make([]*time.Location, 0, len(zones))
	for _, z := range zones {
		loc, err := time.LoadLocation(z)
		if err != nil {
			t.Skipf("tzdata unavailable for %s: %v", z, err)
		}
		locs = append(locs, loc)
	}
	r := genRand()
	for i := 0; i < propIters; i++ {
		loc := locs[r.IntN(len(locs))]
		start := time.Date(2024, time.Month(r.IntN(12)+1), r.IntN(28)+1,
			r.IntN(24), r.IntN(60), 0, 0, loc).AddDate(r.IntN(10), 0, 0)

		for _, iv := range []domain.BillingInterval{domain.BillingMonthly, domain.BillingYearly} {
			for _, bt := range []domain.SubscriptionBillingTime{domain.BillingTimeCalendar, domain.BillingTimeAnniversary} {
				// Exactly how production derives it.
				anchor := domain.AnchorDayFor(start, bt, iv, loc)
				days := fullBillingCycleDays(start, iv, loc, anchor)

				lo, hi := int64(28), int64(31)
				if iv == domain.BillingYearly {
					lo, hi = 365, 366
				}
				if days < lo || days > hi {
					t.Fatalf("cycle length outside the only legal range: start=%s zone=%s billing=%s anchor=%d interval=%s got=%d want %d..%d",
						start.Format(time.RFC3339), loc, bt, anchor, iv, days, lo, hi)
				}
			}
		}
	}
}

// TestFullBillingCycleDays_MisalignedAnchorYieldsStubLength documents a hazard
// rather than a live defect, and exists so the next person does not have to
// rediscover it.
//
// fullBillingCycleDays is the guard against the stub-period overcharge: its
// doc states proration must divide by the FULL cycle, never by the current
// period length. That holds while anchorDay agrees with periodStart.Day(),
// which is what AnchorDayFor guarantees today. If a future change ever writes
// an anchor that disagrees with the period start, this function silently
// returns the distance to the next anchor — a STUB length — and reintroduces
// the exact overcharge it was written to prevent, with no error anywhere.
//
// Measured: signup Oct 24 with an anchor on the 6th yields a 13-day
// denominator instead of 31, charging 1077c for an upgrade that should cost
// 452c — 2.38x. This test pins that behaviour so the day it becomes reachable,
// something fails.
//
// Found by property testing: every existing test of this function passes
// anchorDay=0, so the parameter the single production caller actually supplies
// had never been exercised.
func TestFullBillingCycleDays_MisalignedAnchorYieldsStubLength(t *testing.T) {
	loc := time.UTC
	start := time.Date(2030, 10, 24, 0, 0, 0, 0, loc)

	aligned := fullBillingCycleDays(start, domain.BillingMonthly, loc, domain.AnchorDayFor(start, domain.BillingTimeAnniversary, domain.BillingMonthly, loc))
	if aligned < 28 || aligned > 31 {
		t.Fatalf("aligned anchor must give a real cycle: got %d", aligned)
	}

	misaligned := fullBillingCycleDays(start, domain.BillingMonthly, loc, 6)
	if misaligned >= 28 {
		t.Fatalf("EXPECTED HAZARD GONE: a misaligned anchor now yields %d days. If the function "+
			"was hardened to defend against drift, delete this test and the warning in its doc; "+
			"if a writer changed, re-check that anchor and period start still agree.", misaligned)
	}
	t.Logf("hazard present as documented: aligned=%dd, misaligned(anchor=6)=%dd", aligned, misaligned)
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
