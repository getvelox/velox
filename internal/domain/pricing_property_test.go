package domain

import (
	"math/rand/v2"
	"testing"

	"github.com/shopspring/decimal"
)

// Property tests for usage pricing.
//
// ComputeAmountCents is the function that turns metered quantity into money.
// It has three modes (flat, graduated tiers, package+overage), and the
// graduated path walks tier boundaries — historically the single most
// bug-prone shape in any billing engine, because the errors hide exactly at
// the boundary and nowhere else. An example test picks boundaries someone
// thought of; a generator walks every quantity across randomly-shaped ladders.
//
// The properties here are the ones a customer would notice being wrong:
// pricing that goes DOWN when you use more, a boundary that double-charges or
// skips a unit, and a total that is not the sum of its tiers.
//
// Seeded generator over math/rand/v2 rather than a new dependency: the inputs
// need structural constraints (ascending tier ceilings, catch-all last) that
// are clearer built directly, and a fixed seed makes failures reproducible.

const pricingIters = 5000

func pricingRand() *rand.Rand { return rand.New(rand.NewPCG(0xB111, 0x1237)) }

// genLadder builds a structurally VALID graduated ladder: strictly ascending
// ceilings, optionally ending in a catch-all (UpTo==0) which must be last.
func genLadder(r *rand.Rand) []RatingTier {
	n := r.IntN(4) + 1
	tiers := make([]RatingTier, 0, n+1)
	upto := int64(0)
	for i := 0; i < n; i++ {
		upto += int64(r.IntN(500) + 1)
		tiers = append(tiers, RatingTier{
			UpTo:            upto,
			UnitAmountCents: decimal.NewFromInt(int64(r.IntN(500))),
		})
	}
	if r.IntN(2) == 0 {
		tiers = append(tiers, RatingTier{UpTo: 0, UnitAmountCents: decimal.NewFromInt(int64(r.IntN(500)))})
	}
	return tiers
}

func ladderCapacity(tiers []RatingTier) int64 {
	for _, t := range tiers {
		if t.UpTo == 0 {
			return -1 // unbounded
		}
	}
	return tiers[len(tiers)-1].UpTo
}

// TestProperty_Pricing_MonotonicInQuantity is the property a customer would
// notice first: using MORE must never cost LESS. Every tier-boundary
// off-by-one produces a local dip or jump-back, so this catches boundary bugs
// without needing to know the correct price at any point.
func TestProperty_Pricing_MonotonicInQuantity(t *testing.T) {
	r := pricingRand()
	for i := 0; i < pricingIters; i++ {
		tiers := genLadder(r)
		rule := RatingRuleVersion{Mode: PricingGraduated, GraduatedTiers: tiers}

		cap := ladderCapacity(tiers)
		limit := int64(2000)
		if cap > 0 {
			limit = cap // a bounded ladder errors above its top tier
		}
		if limit < 2 {
			continue
		}
		q1 := int64(r.IntN(int(limit)) + 1)
		q2 := int64(r.IntN(int(limit)) + 1)
		if q1 > q2 {
			q1, q2 = q2, q1
		}

		a1, err1 := ComputeAmountCents(rule, decimal.NewFromInt(q1))
		a2, err2 := ComputeAmountCents(rule, decimal.NewFromInt(q2))
		if err1 != nil || err2 != nil {
			continue // invalid config for this draw; validity is tested separately
		}
		if a1 > a2 {
			t.Fatalf("using more cost less: q=%d -> %dc but q=%d -> %dc\ntiers=%+v", q1, a1, q2, a2, tiers)
		}
	}
}

// TestProperty_Pricing_ZeroQuantityIsFree: no usage, no charge. A tier walk
// that charges a minimum for zero would silently bill every idle customer.
func TestProperty_Pricing_ZeroQuantityIsFree(t *testing.T) {
	r := pricingRand()
	for i := 0; i < pricingIters; i++ {
		var rule RatingRuleVersion
		switch r.IntN(3) {
		case 0:
			rule = RatingRuleVersion{Mode: PricingFlat, FlatAmountCents: decimal.NewFromInt(int64(r.IntN(10000)))}
		case 1:
			rule = RatingRuleVersion{Mode: PricingGraduated, GraduatedTiers: genLadder(r)}
		default:
			rule = RatingRuleVersion{
				Mode: PricingPackage, PackageSize: int64(r.IntN(100) + 1),
				PackageAmountCents:     int64(r.IntN(10000)),
				OverageUnitAmountCents: decimal.NewFromInt(int64(r.IntN(500))),
			}
		}
		got, err := ComputeAmountCents(rule, decimal.Zero)
		if err != nil {
			continue
		}
		if got != 0 {
			t.Fatalf("zero quantity charged %dc: mode=%s rule=%+v", got, rule.Mode, rule)
		}
	}
}

// TestProperty_Pricing_NeverNegative: no quantity may produce a credit. A
// negative line item on an invoice reads as a refund the customer did not earn.
func TestProperty_Pricing_NeverNegative(t *testing.T) {
	r := pricingRand()
	for i := 0; i < pricingIters; i++ {
		tiers := genLadder(r)
		rule := RatingRuleVersion{Mode: PricingGraduated, GraduatedTiers: tiers}
		cap := ladderCapacity(tiers)
		limit := int64(2000)
		if cap > 0 {
			limit = cap
		}
		q := int64(r.IntN(int(limit)) + 1)
		got, err := ComputeAmountCents(rule, decimal.NewFromInt(q))
		if err != nil {
			continue
		}
		if got < 0 {
			t.Fatalf("negative charge: q=%d -> %dc tiers=%+v", q, got, tiers)
		}
	}
}

// TestProperty_Pricing_GraduatedIsSumOfItsTiers independently recomputes the
// ladder walk and requires the implementation to agree. This is a differential
// oracle: the reference is written from the DEFINITION of graduated pricing
// (each tier prices only the units that fall inside it), so agreement means the
// implementation matches the definition rather than matching itself.
//
// Restricted to whole-unit quantities and integer rates so the reference needs
// no rounding of its own — otherwise the two would differ on rounding rather
// than on the tier walk, which is the thing under test.
func TestProperty_Pricing_GraduatedIsSumOfItsTiers(t *testing.T) {
	r := pricingRand()
	for i := 0; i < pricingIters; i++ {
		tiers := genLadder(r)
		rule := RatingRuleVersion{Mode: PricingGraduated, GraduatedTiers: tiers}
		cap := ladderCapacity(tiers)
		limit := int64(2000)
		if cap > 0 {
			limit = cap
		}
		q := int64(r.IntN(int(limit)) + 1)

		got, err := ComputeAmountCents(rule, decimal.NewFromInt(q))
		if err != nil {
			continue
		}

		// Reference walk, straight from the definition.
		var want, lastUpper, remaining int64 = 0, 0, q
		for _, tier := range tiers {
			if remaining <= 0 {
				break
			}
			rate := tier.UnitAmountCents.IntPart()
			if tier.UpTo == 0 { // catch-all
				want += remaining * rate
				remaining = 0
				break
			}
			capacity := tier.UpTo - lastUpper
			take := remaining
			if take > capacity {
				take = capacity
			}
			want += take * rate
			remaining -= take
			lastUpper = tier.UpTo
		}
		if remaining > 0 {
			continue // above ladder capacity; implementation errors, reference cannot
		}
		if got != want {
			t.Fatalf("graduated total disagrees with an independent tier walk: q=%d got=%dc want=%dc\ntiers=%+v",
				q, got, want, tiers)
		}
	}
}

// TestProperty_Pricing_PackageChargesWholePackagesPlusOverage checks the
// package model against its definition: floor(q/size) packages at the package
// price, plus the remainder at the overage rate.
func TestProperty_Pricing_PackageChargesWholePackagesPlusOverage(t *testing.T) {
	r := pricingRand()
	for i := 0; i < pricingIters; i++ {
		size := int64(r.IntN(100) + 1)
		pkgCents := int64(r.IntN(10000))
		overage := int64(r.IntN(500))
		rule := RatingRuleVersion{
			Mode: PricingPackage, PackageSize: size,
			PackageAmountCents:     pkgCents,
			OverageUnitAmountCents: decimal.NewFromInt(overage),
		}
		q := int64(r.IntN(5000))
		got, err := ComputeAmountCents(rule, decimal.NewFromInt(q))
		if err != nil {
			continue
		}
		want := (q/size)*pkgCents + (q%size)*overage
		if got != want {
			t.Fatalf("package pricing disagrees with definition: q=%d size=%d pkg=%dc overage=%dc got=%dc want=%dc",
				q, size, pkgCents, overage, got, want)
		}
	}
}

// TestProperty_Pricing_RejectsNegativeQuantity: metered usage cannot be
// negative, and a silently-accepted negative quantity would emit a credit line.
func TestProperty_Pricing_RejectsNegativeQuantity(t *testing.T) {
	r := pricingRand()
	for i := 0; i < 500; i++ {
		rule := RatingRuleVersion{Mode: PricingGraduated, GraduatedTiers: genLadder(r)}
		q := decimal.NewFromInt(-int64(r.IntN(1000) + 1))
		if _, err := ComputeAmountCents(rule, q); err == nil {
			t.Fatalf("negative quantity %s was accepted", q)
		}
	}
}

// TestProperty_Pricing_RejectsCatchAllBeforeEnd guards the config validation
// the implementation calls out explicitly: a catch-all tier consumes all
// remaining quantity, so anything after it is dead config that would price
// overflow at the wrong rate.
func TestProperty_Pricing_RejectsCatchAllBeforeEnd(t *testing.T) {
	r := pricingRand()
	for i := 0; i < 500; i++ {
		tiers := genLadder(r)
		// Splice a catch-all into a non-final position.
		bad := append([]RatingTier{{UpTo: 0, UnitAmountCents: decimal.NewFromInt(5)}}, tiers...)
		rule := RatingRuleVersion{Mode: PricingGraduated, GraduatedTiers: bad}
		if _, err := ComputeAmountCents(rule, decimal.NewFromInt(int64(r.IntN(100)+1))); err == nil {
			t.Fatalf("catch-all tier before the end was accepted: %+v", bad)
		}
	}
}
