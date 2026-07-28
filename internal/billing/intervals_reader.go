package billing

import (
	"context"
	"fmt"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// ADR-101 Phase 4: the single segment source.
//
// Every base-fee and usage segment the engine bills derives from
// billing_intervals — policy-applied item lifetimes recorded at write
// time. The read side is a dumb interval×window intersection with
// nothing to interpret. The legacy fact-log interpretation, the shadow
// comparator, and the VELOX_BILLING_INTERVALS_READER modes were removed
// at Phase 4 (2026-07-28) after a clean soak: zero unexplained
// divergences across the cutover sweep (140/140 subs), the two-mode CI
// corpus on every PR since, and every dev cycle close in between.
// There is no legacy fallback — a missing interval row for a live item
// is a WRITER bug and fails the sub's billing loudly.

// IntervalSnapshotter is the engine's consumer-defined seam for the
// interval read. *subscription.PostgresStore satisfies it directly.
type IntervalSnapshotter interface {
	// ListItemIntervals returns the sub's full interval history,
	// ordered (starts_at, then zero-width/closed rows before the row
	// that reopened at the same instant). Deliberately UNWINDOWED: the
	// reader clips in Go, and the missing-interval invariant must
	// distinguish "no rows overlap this window" (legitimate zero — a
	// lifetime entirely outside the window) from "no rows AT ALL" (a
	// writer bug).
	ListItemIntervals(ctx context.Context, tenantID, subscriptionID string) ([]domain.ItemInterval, error)
}

// SetIntervalReader wires the interval read seam. The snapshotter is a
// required collaborator (MustValidate refuses to boot without it);
// panicking here on nil keeps narrow tests honest — an engine that
// bills needs interval data, there is no other reader.
func (e *Engine) SetIntervalReader(snap IntervalSnapshotter) {
	if snap == nil {
		panic("SetIntervalReader: nil snapshotter — billing_intervals is the only segment source (ADR-101 Phase 4)")
	}
	e.intervalSnap = snap
}

// windowSegments resolves the per-item base segments for one billing
// window [windowStart, windowEnd] from billing_intervals. The returned
// map covers live items AND items that left the sub during the window
// (their sealed intervals still intersect it); callers iterate it for
// the removed-item pass.
func (e *Engine) windowSegments(ctx context.Context, sub domain.Subscription, windowStart, windowEnd time.Time) (map[string][]baseSegment, error) {
	intervals, err := e.intervalSnap.ListItemIntervals(ctx, sub.TenantID, sub.ID)
	if err != nil {
		return nil, fmt.Errorf("billing-intervals read: %w", err)
	}
	rowsByItem := map[string][]domain.ItemInterval{}
	for _, iv := range intervals {
		rowsByItem[iv.SubscriptionItemID] = append(rowsByItem[iv.SubscriptionItemID], iv)
	}

	// The loud missing-interval invariant. A live item with NO interval
	// rows at all would silently bill zero forever — that is a writer
	// bug, and the sub's billing fails instead. An item whose rows
	// simply don't overlap this window is a legitimate zero (its
	// lifetime starts after the window).
	for _, it := range sub.Items {
		if len(rowsByItem[it.ID]) == 0 {
			return nil, fmt.Errorf("billing-intervals: active item %s on sub %s has no interval rows (writer bug — refusing to bill it as zero)", it.ID, sub.ID)
		}
	}
	return intervalSegmentsByItem(rowsByItem, windowStart, windowEnd), nil
}

// intervalSegmentsByItem derives segments from billing_intervals rows:
// clip each row to the window, drop empty slices, merge adjacent
// equal-(plan, qty) slices. No policy here — the day-grade decisions
// were made at write time; this is the "dumb intersection" the ADR
// promises.
func intervalSegmentsByItem(rowsByItem map[string][]domain.ItemInterval, windowStart, windowEnd time.Time) map[string][]baseSegment {
	out := make(map[string][]baseSegment, len(rowsByItem))
	for itemID, rows := range rowsByItem {
		var segs []baseSegment
		for _, iv := range rows {
			start := iv.StartsAt
			if start.Before(windowStart) {
				start = windowStart
			}
			end := windowEnd
			if iv.EndsAt != nil && iv.EndsAt.Before(windowEnd) {
				end = *iv.EndsAt
			}
			if !end.After(start) {
				continue
			}
			segs = append(segs, baseSegment{start: start, end: end, planID: iv.PlanID, quantity: iv.Quantity})
		}
		if merged := mergeEqualAdjacent(segs); len(merged) > 0 {
			out[itemID] = merged
		}
	}
	return out
}

// mergeEqualAdjacent collapses touching segments with identical
// (plan, qty) — defensive normalization so a semantically-identical
// split never bills as two lines. Input is start-ordered (store sort)
// and non-overlapping (DB exclusion constraint).
func mergeEqualAdjacent(segs []baseSegment) []baseSegment {
	if len(segs) < 2 {
		return segs
	}
	out := segs[:1]
	for _, s := range segs[1:] {
		last := &out[len(out)-1]
		if s.planID == last.planID && s.quantity == last.quantity && s.start.Equal(last.end) {
			last.end = s.end
			continue
		}
		out = append(out, s)
	}
	return out
}
