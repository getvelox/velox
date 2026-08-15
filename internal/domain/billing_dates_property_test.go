package domain

import (
	"math/rand/v2"
	"testing"
	"time"
)

// Property tests for billing date math.
//
// This is where this codebase's real proration bugs have lived: ADR-058's
// calendar-month overflow (Jan 31 + 1 month landing on Mar 3 in Go's AddDate,
// silently skipping February), ADR-055's Feb-29 anniversary ratchet, and
// ADR-091's timezone seam. All three are month-end or zone effects, and all
// three were found by walking flows rather than by the test suite.
//
// The generators here deliberately concentrate on the dangerous inputs:
//   - days 28, 29, 30, 31 (the clamp region), not just safe mid-month days
//   - February in leap and non-leap years
//   - zones with non-hour offsets and southern-hemisphere DST, where a
//     "day" boundary moves relative to UTC
//
// A separate proration property test caps generated days at 28; that was a gap
// — it could never have produced the clamp cases below.

const dateIters = 4000

func dateRand() *rand.Rand { return rand.New(rand.NewPCG(0xDA7E, 0x5EED)) }

func dateZones(t *testing.T) []*time.Location {
	names := []string{
		"UTC",
		"Asia/Kolkata",        // +05:30, non-hour offset
		"America/New_York",    // northern DST
		"Australia/Lord_Howe", // +10:30 / +11:00, half-hour DST shift
		"Pacific/Chatham",     // +12:45 / +13:45
	}
	locs := make([]*time.Location, 0, len(names))
	for _, n := range names {
		loc, err := time.LoadLocation(n)
		if err != nil {
			t.Skipf("tzdata unavailable for %s: %v", n, err)
		}
		locs = append(locs, loc)
	}
	return locs
}

// genRiskyDate returns an instant weighted toward month ends and February,
// which is where every historical bug in this area has been.
func genRiskyDate(r *rand.Rand, loc *time.Location) time.Time {
	year := 2024 + r.IntN(8) // spans leap years 2024 and 2028
	month := time.Month(r.IntN(12) + 1)
	if r.IntN(3) == 0 {
		month = time.February // over-sample February
	}
	last := time.Date(year, month+1, 1, 0, 0, 0, 0, loc).AddDate(0, 0, -1).Day()
	var day int
	if r.IntN(2) == 0 {
		day = last - r.IntN(3) // 28..31 region
		if day < 1 {
			day = 1
		}
	} else {
		day = r.IntN(last) + 1
	}
	return time.Date(year, month, day, r.IntN(24), r.IntN(60), 0, 0, loc)
}

// TestProperty_NextBillingPeriodEnd_AlwaysAdvances: a cycle boundary must move
// strictly forward. A non-advancing boundary means the engine bills the same
// period forever, or spins.
func TestProperty_NextBillingPeriodEnd_AlwaysAdvances(t *testing.T) {
	r := dateRand()
	for _, loc := range dateZones(t) {
		for i := 0; i < dateIters; i++ {
			start := genRiskyDate(r, loc)
			for _, iv := range []BillingInterval{BillingMonthly, BillingYearly} {
				for _, bt := range []SubscriptionBillingTime{BillingTimeCalendar, BillingTimeAnniversary} {
					anchor := AnchorDayFor(start, bt, iv, loc)
					next := NextBillingPeriodEnd(start, bt, iv, loc, anchor)
					if !next.After(start) {
						t.Fatalf("boundary did not advance: start=%s zone=%s billing=%s interval=%s anchor=%d next=%s",
							start.Format(time.RFC3339), loc, bt, iv, anchor, next.Format(time.RFC3339))
					}
				}
			}
		}
	}
}

// TestProperty_NextBillingPeriodEnd_NeverSkipsAMonth is the ADR-058 regression
// as a property. Jan 31 + 1 month in Go's AddDate is Mar 3 — February is
// skipped entirely and the customer is never billed for it. Any monthly
// boundary must land within one real month OF ITS OWN CADENCE.
//
// The two billing times have different domains for `periodEnd`, and feeding
// one the other's inputs tests nothing: CALENDAR periods always end on the 1st
// of a month, so that is what is generated for them; ANNIVERSARY periods end on
// the anchor day. An earlier draft of this test fed calendar an arbitrary
// mid-month date and flagged the 2.5-day gap to the next 1st as a defect — it
// was the generator being out of domain, not the code.
func TestProperty_NextBillingPeriodEnd_NeverSkipsAMonth(t *testing.T) {
	r := dateRand()
	for _, loc := range dateZones(t) {
		for i := 0; i < dateIters; i++ {
			risky := genRiskyDate(r, loc)

			// Calendar: period ends are month boundaries.
			calStart := BeginningOfMonthIn(risky, loc)
			calNext := NextBillingPeriodEnd(calStart, BillingTimeCalendar, BillingMonthly, loc, 0)
			if d := calNext.Sub(calStart).Hours() / 24; d < 27 || d > 32 {
				t.Fatalf("calendar boundary is not one month away: start=%s zone=%s next=%s gap=%.2f days",
					calStart.In(loc).Format(time.RFC3339), loc, calNext.In(loc).Format(time.RFC3339), d)
			}

			// Anniversary: period ends sit on the anchor day.
			annAnchor := AnchorDayFor(risky, BillingTimeAnniversary, BillingMonthly, loc)
			annNext := NextBillingPeriodEnd(risky, BillingTimeAnniversary, BillingMonthly, loc, annAnchor)
			if d := annNext.Sub(risky).Hours() / 24; d < 27 || d > 32 {
				t.Fatalf("anniversary boundary is not one month away: start=%s zone=%s anchor=%d next=%s gap=%.2f days",
					risky.Format(time.RFC3339), loc, annAnchor, annNext.In(loc).Format(time.RFC3339), d)
			}
		}
	}
}

// TestProperty_NextBillingPeriodEnd_CalendarLandsOnTheFirst is the ADR-058
// regression, and it deliberately feeds RISKY dates (days 29/30/31) rather
// than well-formed month boundaries.
//
// That is the opposite of the domain restriction used by the gap property
// above, and the difference matters: "the result lands on the 1st" is a valid
// assertion for ANY input to the calendar path, whereas "the gap is one month"
// is only meaningful from a month boundary. ADR-058's bug was exactly a
// day-31 value reaching this path — addIntervalIn(Jan 31) overflows to Mar 3,
// which snaps to Mar 1 and skips February entirely.
//
// Mutation testing caught this: an earlier draft snapped the input to the 1st
// here too, and reintroducing ADR-058 then SURVIVED, because from the 1st both
// orderings agree. Narrowing a generator can silently remove the only input
// that exposes the defect the test exists for.
func TestProperty_NextBillingPeriodEnd_CalendarLandsOnTheFirst(t *testing.T) {
	r := dateRand()
	for _, loc := range dateZones(t) {
		for i := 0; i < dateIters; i++ {
			start := genRiskyDate(r, loc)
			anchor := AnchorDayFor(start, BillingTimeCalendar, BillingMonthly, loc)
			next := NextBillingPeriodEnd(start, BillingTimeCalendar, BillingMonthly, loc, anchor)
			if d := next.In(loc).Day(); d != 1 {
				t.Fatalf("calendar boundary did not land on the 1st: start=%s zone=%s next=%s (day %d)",
					start.Format(time.RFC3339), loc, next.In(loc).Format(time.RFC3339), d)
			}
			// Landing on the 1st is not enough — it must be the 1st of the
			// NEXT month. ADR-058's bug landed on the 1st of the month AFTER
			// next (Jan 31 -> Mar 1), skipping February entirely, so an
			// assertion that only checks the day-of-month cannot see it.
			wantMonth := BeginningOfMonthIn(start, loc).In(loc).AddDate(0, 1, 0).Month()
			if got := next.In(loc).Month(); got != wantMonth {
				t.Fatalf("calendar boundary skipped a month: start=%s zone=%s next=%s (month %s, want %s)",
					start.Format(time.RFC3339), loc, next.In(loc).Format(time.RFC3339), got, wantMonth)
			}
		}
	}
}

// TestProperty_AdvanceAnchored_ClampsToRealDatesAndRestores is the ADR-055
// regression as a property: a day-31 anchor must bill the 30th in a 30-day
// month and the 28th/29th in February, then RESTORE to 31 in a long month —
// never ratchet down permanently, and never overflow into the next month.
func TestProperty_AdvanceAnchored_ClampsToRealDatesAndRestores(t *testing.T) {
	for _, loc := range dateZones(t) {
		for _, anchor := range []int{29, 30, 31} {
			// Walk 36 consecutive monthly boundaries from a January start.
			cur := time.Date(2027, time.January, anchor, 12, 0, 0, 0, loc)
			sawClamp, restored := false, false
			for step := 0; step < 36; step++ {
				next := NextBillingPeriodEnd(cur, BillingTimeAnniversary, BillingMonthly, loc, anchor)
				ld := lastDayOfMonthIn(next.In(loc).Year(), next.In(loc).Month(), loc)
				wantDay := anchor
				if wantDay > ld {
					wantDay = ld
					sawClamp = true
				} else if sawClamp {
					restored = true
				}
				if got := next.In(loc).Day(); got != wantDay {
					t.Fatalf("anchor %d in zone %s: boundary landed on day %d, want %d (month has %d days) at %s",
						anchor, loc, got, wantDay, ld, next.In(loc).Format(time.RFC3339))
				}
				cur = next
			}
			if !sawClamp {
				t.Fatalf("anchor %d never hit a short month in 36 steps — the generator is not exercising the clamp", anchor)
			}
			if !restored {
				t.Fatalf("anchor %d clamped but never RESTORED to the full anchor day in zone %s — "+
					"this is the ratchet ADR-055 fixed", anchor, loc)
			}
		}
	}
}

// TestProperty_AddBillingInterval_YearlyStaysInItsMonth: a yearly anniversary
// must remain in the same calendar month. Feb 29 + 1 year overflowing to Mar 1
// walks the anniversary off February permanently.
func TestProperty_AddBillingInterval_YearlyStaysInItsMonth(t *testing.T) {
	r := dateRand()
	for _, loc := range dateZones(t) {
		for i := 0; i < dateIters; i++ {
			start := genRiskyDate(r, loc)
			anchor := AnchorDayFor(start, BillingTimeAnniversary, BillingYearly, loc)
			next := AddBillingInterval(start, BillingYearly, loc, anchor)
			sm, nm := start.In(loc).Month(), next.In(loc).Month()
			if sm != nm {
				t.Fatalf("yearly boundary changed month: start=%s (%s) -> next=%s (%s) zone=%s anchor=%d",
					start.Format(time.RFC3339), sm, next.In(loc).Format(time.RFC3339), nm, loc, anchor)
			}
			if y := next.In(loc).Year() - start.In(loc).Year(); y != 1 {
				t.Fatalf("yearly boundary advanced %d years: start=%s next=%s", y,
					start.Format(time.RFC3339), next.In(loc).Format(time.RFC3339))
			}
		}
	}
}
