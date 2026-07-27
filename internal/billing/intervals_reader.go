package billing

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// ADR-101 Phases 2/3: the segment source seam.
//
// Every base-fee and usage segment the engine bills is derived from a
// per-item []baseSegment. Phase 2 computes that twice — the legacy
// interpretation (fact-log walk + day-grade policy at read time) and
// the billing_intervals reader (policy already applied at write time)
// — from ONE snapshot, compares them, and screams on divergence while
// the legacy side keeps billing. Phase 3 flips which side bills via
// VELOX_BILLING_INTERVALS_READER (off | shadow | on, read once at
// boot):
//
//   off    — legacy bills, no snapshot, no comparison (kill switch).
//   shadow — legacy bills, comparator runs on every window.
//   on     — intervals bill, comparator still runs (the dormant legacy
//            interpretation stays exercised until Phase 4 removes it).

// IntervalSnapshotter is the engine's consumer-defined seam for the
// one-statement snapshot read. *subscription.PostgresStore satisfies
// it directly.
type IntervalSnapshotter interface {
	SegmentShadowSnapshot(ctx context.Context, tenantID, subscriptionID string, periodStart, periodEnd time.Time) ([]domain.SubscriptionItemChange, []domain.ItemInterval, error)
}

const (
	IntervalReaderOff    = "off"
	IntervalReaderShadow = "shadow"
	IntervalReaderOn     = "on"
)

// SetIntervalReader wires the snapshot seam and the reader mode.
// Panics on an unknown mode — a mistyped env value must kill the boot,
// not silently bill the wrong way. A nil snapshotter forces mode off
// (narrow tests construct the engine without one; production always
// wires the real store).
func (e *Engine) SetIntervalReader(snap IntervalSnapshotter, mode string) {
	switch mode {
	case IntervalReaderOff, IntervalReaderShadow, IntervalReaderOn:
	default:
		panic(fmt.Sprintf("VELOX_BILLING_INTERVALS_READER=%q: want off|shadow|on", mode))
	}
	if snap == nil && mode != IntervalReaderOff {
		panic("SetIntervalReader: mode " + mode + " requires a snapshotter")
	}
	e.intervalSnap = snap
	e.intervalMode = mode
}

// ShadowParityStats returns the process-lifetime comparator counters:
// windows compared, divergences in a known-allowlisted class, and
// unexplained divergences. The corpus CI gate asserts unexplained==0.
func (e *Engine) ShadowParityStats() (compared, allowlisted, unexplained uint64) {
	return e.shadowCompared.Load(), e.shadowAllowlisted.Load(), e.shadowUnexplained.Load()
}

// windowSegments resolves the per-item base segments for one billing
// window [windowStart, windowEnd] — the ONLY place that decides which
// interpretation bills. The returned map covers live items AND items
// that left the sub during the window; callers iterate it for the
// removed-item pass.
func (e *Engine) windowSegments(ctx context.Context, sub domain.Subscription, changesByItem map[string][]domain.SubscriptionItemChange, windowStart, windowEnd time.Time) (map[string][]baseSegment, error) {
	loc := e.tenantLocation(ctx, sub.TenantID)
	legacy := legacySegmentsByItem(sub, changesByItem, windowStart, windowEnd, loc)
	if e.intervalSnap == nil || e.intervalMode == IntervalReaderOff {
		return legacy, nil
	}

	snapChanges, snapIntervals, err := e.intervalSnap.SegmentShadowSnapshot(ctx, sub.TenantID, sub.ID, windowStart, windowEnd)
	if err != nil {
		if e.intervalMode == IntervalReaderOn {
			return nil, fmt.Errorf("billing-intervals snapshot: %w", err)
		}
		// Shadow mode must never break billing: log and bill legacy.
		slog.Warn("billing-intervals shadow snapshot failed; billing legacy without comparison",
			"subscription_id", sub.ID, "tenant_id", sub.TenantID, "error", err)
		return legacy, nil
	}

	snapByItem := map[string][]domain.SubscriptionItemChange{}
	for _, c := range snapChanges {
		snapByItem[c.SubscriptionItemID] = append(snapByItem[c.SubscriptionItemID], c)
	}
	// Legacy recomputed FROM THE SNAPSHOT — comparing the engine's own
	// (earlier-tx) walk against snapshot intervals would manufacture
	// divergence out of any concurrent item mutation.
	snapLegacy := legacySegmentsByItem(sub, snapByItem, windowStart, windowEnd, loc)
	rowsByItem := map[string][]domain.ItemInterval{}
	for _, iv := range snapIntervals {
		rowsByItem[iv.SubscriptionItemID] = append(rowsByItem[iv.SubscriptionItemID], iv)
	}
	shadow := intervalSegmentsByItem(rowsByItem, windowStart, windowEnd)

	e.compareShadow(sub, snapLegacy, shadow, rowsByItem, windowStart, windowEnd, loc)

	if e.intervalMode != IntervalReaderOn {
		return legacy, nil
	}

	// Reader on: the loud missing-interval invariant. A live item with
	// NO interval rows at all would silently bill zero forever — that is
	// a writer bug, and the sub's billing fails instead. An item whose
	// rows simply don't overlap this window is a legitimate zero (its
	// lifetime starts after the window — the catch-up interleave shape
	// the legacy reader gets wrong).
	for _, it := range sub.Items {
		if len(rowsByItem[it.ID]) == 0 {
			return nil, fmt.Errorf("billing-intervals: active item %s on sub %s has no interval rows (writer bug — refusing to bill it as zero)", it.ID, sub.ID)
		}
	}
	return shadow, nil
}

// legacySegmentsByItem reproduces the pre-ADR-101 segment derivation
// exactly: current items walk their in-window changes (full-window
// single segment when there are none), items present only in the
// change log (removed mid-window) walk with a nil item.
func legacySegmentsByItem(sub domain.Subscription, changesByItem map[string][]domain.SubscriptionItemChange, windowStart, windowEnd time.Time, loc *time.Location) map[string][]baseSegment {
	out := make(map[string][]baseSegment, len(sub.Items)+len(changesByItem))
	live := make(map[string]bool, len(sub.Items))
	for _, it := range sub.Items {
		live[it.ID] = true
		itemForSeg := it
		out[it.ID] = itemBaseSegments(&itemForSeg, changesByItem[it.ID], windowStart, windowEnd, loc)
	}
	for itemID, changes := range changesByItem {
		if live[itemID] {
			continue
		}
		out[itemID] = itemBaseSegments(nil, changes, windowStart, windowEnd, loc)
	}
	return out
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
// split never reads as divergence. Input is start-ordered (snapshot
// sort) and non-overlapping (DB exclusion constraint).
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

func segmentsEqual(a, b []baseSegment) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].planID != b[i].planID || a[i].quantity != b[i].quantity ||
			!a[i].start.Equal(b[i].start) || !a[i].end.Equal(b[i].end) {
			return false
		}
	}
	return true
}

// Divergence classes. The two allowlisted classes are shapes where the
// interval side is MORE correct by design (ADR-101) — logging them as
// WARN would train WARN-blindness, so they log INFO and count
// separately.
const (
	divergenceUnexplained = "unexplained"
	// The write-time day-grade decision (made in the tenant TZ that
	// existed then) disagrees with a read-time re-evaluation in the
	// CURRENT tenant TZ — the org-TZ clamp-miss class ADR-101 §Context
	// names. Only the first segment's start can differ, by exactly the
	// clamp.
	divergenceTZClampMiss = "org-tz-clamp-miss"
	// The item's entire interval lifetime lies outside this window
	// (typically: item added AFTER the window during an engine-down
	// catch-up), while the legacy walk — seeing a live item with no
	// in-window changes — bills it for the full window. The registered
	// catchup-interleave class; interval side authoritative.
	divergenceCatchupLifetime = "catchup-lifetime-outside-window"
	// Pre-0102 residue: the item row was HARD-deleted (that era's
	// removal), leaving an 'add' fact whose wall-stamped 'remove'
	// partner falls outside sim-time windows — the legacy walk bills a
	// phantom for a row that no longer exists. No interval rows is
	// CORRECT here (nothing exists to bill); post-Phase-1 items always
	// have rows, so this shape can only be that era's residue.
	divergenceHardDeletedResidue = "hard-deleted-item-residue"
	// 0102→0129 residue: the soft delete of that era wrote no 'remove'
	// fact, so a pre-window add + in-window seal is invisible to the
	// legacy walk (it under-bills the stub). The backfilled interval —
	// sealed at deleted_at, ending INSIDE the window — is the truth. A
	// spurious open interval would run to the window end and stays
	// unexplained.
	divergenceRemoveGapResidue = "remove-gap-fact-residue"
)

func classifyDivergence(legacy, shadow []baseSegment, hasIntervalRows, live bool, windowStart, windowEnd time.Time, loc *time.Location) string {
	if len(shadow) == 0 && len(legacy) > 0 {
		if hasIntervalRows {
			return divergenceCatchupLifetime
		}
		if !live {
			return divergenceHardDeletedResidue
		}
	}
	if !live && len(legacy) == 0 && len(shadow) > 0 && shadow[len(shadow)-1].end.Before(windowEnd) {
		return divergenceRemoveGapResidue
	}
	if len(legacy) > 0 && len(shadow) > 0 && len(legacy) == len(shadow) {
		restEqual := segmentsEqual(legacy[1:], shadow[1:])
		l0, s0 := legacy[0], shadow[0]
		firstBodyEqual := l0.planID == s0.planID && l0.quantity == s0.quantity && l0.end.Equal(s0.end)
		if restEqual && firstBodyEqual && !l0.start.Equal(s0.start) {
			// Exactly one side clamped its opening to the window start.
			intervalClamped := s0.start.Equal(windowStart) && l0.start.After(windowStart) && !domain.SameCalendarDayIn(l0.start, windowStart, loc)
			legacyClamped := l0.start.Equal(windowStart) && s0.start.After(windowStart) && domain.SameCalendarDayIn(s0.start, windowStart, loc)
			if intervalClamped || legacyClamped {
				return divergenceTZClampMiss
			}
		}
	}
	return divergenceUnexplained
}

// compareShadow diffs the two derivations item by item and records the
// verdicts. Never mutates anything and never errors — parity is
// evidence, not a gate on the money path (CI corpora gate on the
// counters instead).
func (e *Engine) compareShadow(sub domain.Subscription, legacy, shadow map[string][]baseSegment, rowsByItem map[string][]domain.ItemInterval, windowStart, windowEnd time.Time, loc *time.Location) {
	e.shadowCompared.Add(1)
	live := make(map[string]bool, len(sub.Items))
	for _, it := range sub.Items {
		live[it.ID] = true
	}
	itemIDs := make(map[string]bool, len(legacy)+len(shadow))
	for id := range legacy {
		itemIDs[id] = true
	}
	for id := range shadow {
		itemIDs[id] = true
	}
	for id := range itemIDs {
		l := mergeEqualAdjacent(legacy[id])
		s := shadow[id]
		if segmentsEqual(l, s) {
			continue
		}
		class := classifyDivergence(l, s, len(rowsByItem[id]) > 0, live[id], windowStart, windowEnd, loc)
		fields := []any{
			"tenant_id", sub.TenantID, "subscription_id", sub.ID, "item_id", id,
			"window_start", windowStart, "window_end", windowEnd,
			"legacy", formatSegments(l), "intervals", formatSegments(s),
			"class", class,
		}
		if class == divergenceUnexplained {
			e.shadowUnexplained.Add(1)
			slog.Warn("billing-intervals shadow divergence (unexplained)", fields...)
		} else {
			e.shadowAllowlisted.Add(1)
			slog.Info("billing-intervals shadow divergence (known class, interval side authoritative)", fields...)
		}
	}
}

func formatSegments(segs []baseSegment) string {
	if len(segs) == 0 {
		return "(none)"
	}
	var b strings.Builder
	for i, s := range segs {
		if i > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "[%s→%s %s q%d]",
			s.start.UTC().Format(time.RFC3339), s.end.UTC().Format(time.RFC3339), s.planID, s.quantity)
	}
	return b.String()
}
