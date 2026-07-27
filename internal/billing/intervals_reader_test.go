package billing

import (
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// Unit tests for the ADR-101 segment-source seam's pure pieces:
// clipping, normalization, equality, and the divergence classifier.

var (
	irPS = time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	irPE = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
)

func seg(start, end time.Time, plan string, qty int64) baseSegment {
	return baseSegment{start: start, end: end, planID: plan, quantity: qty}
}

func TestMergeEqualAdjacent(t *testing.T) {
	t1 := irPS.AddDate(0, 0, 10)
	t2 := irPS.AddDate(0, 0, 20)

	// Touching equal-(plan, qty) slices merge; a value change survives.
	got := mergeEqualAdjacent([]baseSegment{
		seg(irPS, t1, "a", 1), seg(t1, t2, "a", 1), seg(t2, irPE, "b", 1),
	})
	if len(got) != 2 || !got[0].end.Equal(t2) || got[1].planID != "b" {
		t.Fatalf("merge mismatch: %+v", got)
	}
	// A gap blocks the merge even with equal values.
	got = mergeEqualAdjacent([]baseSegment{
		seg(irPS, t1, "a", 1), seg(t2, irPE, "a", 1),
	})
	if len(got) != 2 {
		t.Fatalf("gapped equal slices must not merge: %+v", got)
	}
}

func TestIntervalSegmentsByItem_ClipsAndDrops(t *testing.T) {
	t1 := irPS.AddDate(0, 0, 10)
	before := irPS.AddDate(0, -1, 0)
	after := irPE.AddDate(0, 0, 5)

	iv := func(item, plan string, qty int64, start time.Time, end *time.Time) domain.ItemInterval {
		return domain.ItemInterval{SubscriptionItemID: item, PlanID: plan, Quantity: qty, StartsAt: start, EndsAt: end}
	}
	byItem := map[string][]domain.ItemInterval{
		// Open interval from before the window → clipped to [ps, pe].
		"open": {iv("open", "a", 1, before, nil)},
		// Sealed inside the window plus successor → two slices.
		"split": {iv("split", "a", 1, before, &t1), iv("split", "b", 1, t1, nil)},
		// Zero-width (same-instant transition artifact) → dropped.
		"zero": {iv("zero", "a", 1, t1, &t1), iv("zero", "b", 2, t1, nil)},
		// Entire lifetime after the window → no segments at all.
		"later": {iv("later", "a", 1, after, nil)},
	}
	got := intervalSegmentsByItem(byItem, irPS, irPE)

	if s := got["open"]; len(s) != 1 || !s[0].start.Equal(irPS) || !s[0].end.Equal(irPE) {
		t.Fatalf("open: %+v", s)
	}
	if s := got["split"]; len(s) != 2 || !s[0].end.Equal(t1) || s[1].planID != "b" || !s[1].end.Equal(irPE) {
		t.Fatalf("split: %+v", s)
	}
	if s := got["zero"]; len(s) != 1 || s[0].planID != "b" || s[0].quantity != 2 || !s[0].start.Equal(t1) {
		t.Fatalf("zero-width must drop, successor stays: %+v", s)
	}
	if _, ok := got["later"]; ok {
		t.Fatalf("lifetime outside window must yield no entry: %+v", got["later"])
	}
}

func TestClassifyDivergence(t *testing.T) {
	t1 := irPS.Add(14 * time.Hour) // same UTC calendar day as irPS
	nextDay := irPS.AddDate(0, 0, 3)

	// Catch-up shape: legacy bills a full window for a LIVE item whose
	// interval lifetime is entirely outside it.
	if c := classifyDivergence([]baseSegment{seg(irPS, irPE, "a", 1)}, nil, true, true, irPS, irPE, time.UTC); c != divergenceCatchupLifetime {
		t.Fatalf("catchup class: got %s", c)
	}
	// Same shape but NO interval rows on a LIVE item → writer bug.
	if c := classifyDivergence([]baseSegment{seg(irPS, irPE, "a", 1)}, nil, false, true, irPS, irPE, time.UTC); c != divergenceUnexplained {
		t.Fatalf("live item with missing rows must be unexplained: got %s", c)
	}
	// No rows on a NON-live item: the pre-0102 hard-delete residue —
	// legacy bills a phantom for a row that no longer exists.
	if c := classifyDivergence([]baseSegment{seg(irPS, irPE, "a", 1)}, nil, false, false, irPS, irPE, time.UTC); c != divergenceHardDeletedResidue {
		t.Fatalf("hard-deleted residue: got %s", c)
	}
	// 0102→0129 remove-gap residue: legacy blind to a pre-window add
	// sealed INSIDE the window.
	stubEnd := irPS.Add(12 * time.Hour)
	if c := classifyDivergence(nil, []baseSegment{seg(irPS, stubEnd, "a", 1)}, true, false, irPS, irPE, time.UTC); c != divergenceRemoveGapResidue {
		t.Fatalf("remove-gap residue: got %s", c)
	}
	// A spurious interval running to the window END is not that class.
	if c := classifyDivergence(nil, []baseSegment{seg(irPS, irPE, "a", 1)}, true, false, irPS, irPE, time.UTC); c != divergenceUnexplained {
		t.Fatalf("open-to-window-end with no legacy view must stay unexplained: got %s", c)
	}
	// TZ clamp-miss: intervals clamped to window start at write time,
	// legacy (current TZ) sees a different calendar day and won't clamp.
	legacy := []baseSegment{seg(nextDay, irPE, "a", 1)}
	shadow := []baseSegment{seg(irPS, irPE, "a", 1)}
	if c := classifyDivergence(legacy, shadow, true, true, irPS, irPE, time.UTC); c != divergenceTZClampMiss {
		t.Fatalf("tz clamp-miss (interval clamped): got %s", c)
	}
	// Mirror: legacy clamps now (same calendar day in current TZ) but
	// the write-time TZ didn't.
	legacy = []baseSegment{seg(irPS, irPE, "a", 1)}
	shadow = []baseSegment{seg(t1, irPE, "a", 1)}
	if c := classifyDivergence(legacy, shadow, true, true, irPS, irPE, time.UTC); c != divergenceTZClampMiss {
		t.Fatalf("tz clamp-miss (legacy clamped): got %s", c)
	}
	// Quantity mismatch is never allowlisted.
	legacy = []baseSegment{seg(irPS, irPE, "a", 1)}
	shadow = []baseSegment{seg(irPS, irPE, "a", 2)}
	if c := classifyDivergence(legacy, shadow, true, true, irPS, irPE, time.UTC); c != divergenceUnexplained {
		t.Fatalf("qty mismatch must be unexplained: got %s", c)
	}
}

func TestSetIntervalReader_Validation(t *testing.T) {
	e := &Engine{}
	mustPanic := func(fn func()) {
		t.Helper()
		defer func() {
			if recover() == nil {
				t.Fatal("expected panic")
			}
		}()
		fn()
	}
	mustPanic(func() { e.SetIntervalReader(nil, "banana") })
	mustPanic(func() { e.SetIntervalReader(nil, IntervalReaderOn) })
	// nil + off is the narrow-test shape and must be fine.
	e.SetIntervalReader(nil, IntervalReaderOff)
}
