package domain

import (
	"errors"
	"time"

	"github.com/shopspring/decimal"
)

type SubscriptionStatus string

const (
	SubscriptionDraft    SubscriptionStatus = "draft"
	SubscriptionTrialing SubscriptionStatus = "trialing"
	SubscriptionActive   SubscriptionStatus = "active"
	SubscriptionCanceled SubscriptionStatus = "canceled"
	SubscriptionArchived SubscriptionStatus = "archived"
)

type SubscriptionBillingTime string

const (
	BillingTimeCalendar    SubscriptionBillingTime = "calendar"
	BillingTimeAnniversary SubscriptionBillingTime = "anniversary"
)

// BeginningOfDayIn snaps `t` to 00:00:00 on its calendar date in `loc`,
// returned as a UTC instant for storage. Day-grade billing requires
// this to align UI-displayed dates with proration math.
func BeginningOfDayIn(t time.Time, loc *time.Location) time.Time {
	if loc == nil {
		loc = time.UTC
	}
	local := t.In(loc)
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc).UTC()
}

// SameCalendarDayIn reports whether a and b fall on the same calendar
// day in loc. Calendar-day identity — deliberately not a duration
// test — so DST-transition days and mid-day period starts (threshold
// reset, ADR-091 org-TZ seams) classify correctly. Shared by the
// day-grade add clamps in billing.itemBaseSegments and the
// subscription AddItem proration (ADR-012 amendment 2026-07-26).
func SameCalendarDayIn(a, b time.Time, loc *time.Location) bool {
	if loc == nil {
		loc = time.UTC
	}
	ay, am, ad := a.In(loc).Date()
	by, bm, bd := b.In(loc).Date()
	return ay == by && am == bm && ad == bd
}

// BeginningOfMonthIn snaps `t` to the first-of-month-at-00:00 in `loc`,
// returned as UTC. Calendar-billing anchor helper. Shared between
// subscription.Service (initial activation / trial-end / reset) and
// billing.Engine (cycle close re-anchoring) so both compute the same
// boundary from the same inputs. loc=nil falls back to UTC.
func BeginningOfMonthIn(t time.Time, loc *time.Location) time.Time {
	if loc == nil {
		loc = time.UTC
	}
	local := t.In(loc)
	return time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, loc).UTC()
}

// LoadLocationOrUTC resolves an IANA timezone name to a *time.Location,
// falling back to UTC on empty/invalid input — the shared tenant-TZ resolution
// used at render boundaries (PDF) and anywhere a tenant_settings.Timezone needs
// turning into a Location for ADR-058 calendar math.
func LoadLocationOrUTC(name string) *time.Location {
	if name == "" {
		return time.UTC
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return time.UTC
	}
	return loc
}

// InclusiveDisplayEnd converts a half-open period_end (the EXCLUSIVE boundary,
// = the next period's start) into the last calendar day FULLY covered by the
// period, in the tenant timezone `loc` — the industry-standard inclusive
// display end ("Jun 1 – Jun 30", not the exclusive "Jun 1 – Jul 1"). Storage
// stays half-open; this is render-only.
//
// It snaps the exclusive end to civil midnight in `loc`, then steps back ONE
// CALENDAR day. The calendar step (not a 24h instant subtraction) is essential:
// 24h-before an instant at a DST boundary, or a non-midnight end, lands on the
// wrong civil date — the same off-by-one class as ADR-058. loc=nil → UTC.
// Returns a loc-located civil-midnight time; format it with loc-reading layouts
// (e.g. "Jan 2, 2006") — never via UTC fields.
func InclusiveDisplayEnd(periodEnd time.Time, loc *time.Location) time.Time {
	if loc == nil {
		loc = time.UTC
	}
	local := periodEnd.In(loc)
	midnight := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	return midnight.AddDate(0, 0, -1)
}

// FormatInclusivePeriod renders the human period string
// "<start> – <inclusiveEnd>" using inclusive display dates in loc — the single
// backend-authored value every render surface (PDF, hosted, dashboard, list)
// shows verbatim, so the period cannot drift across the Go and TS runtimes (and
// the subtle ADR-058 civil-day math lives in exactly one place). Both ends are
// rendered date-only in the tenant timezone. Returns "" when start == end (a
// one-off / no-period invoice — Stripe/Chargebee/Lago all omit the period
// there) so callers show no period row. Clamps the inclusive end to >= the
// start's civil day so a sub-day stub never renders an inverted range.
func FormatInclusivePeriod(start, end time.Time, loc *time.Location) string {
	if loc == nil {
		loc = time.UTC
	}
	if start.Equal(end) {
		return ""
	}
	startLocal := start.In(loc)
	startCivil := time.Date(startLocal.Year(), startLocal.Month(), startLocal.Day(), 0, 0, 0, 0, loc)
	inc := InclusiveDisplayEnd(end, loc)
	if inc.Before(startCivil) {
		inc = startCivil
	}
	const layout = "Jan 2, 2006"
	return startCivil.Format(layout) + " – " + inc.Format(layout) //tz:ok — both values constructed IN loc above (time.Date(..., loc)); the zone is applied at construction, not at render
}

// NextBillingPeriodEnd computes the next period's end boundary at
// cycle close, honoring billing_time + interval. The semantics:
//
//   - Yearly: always anniversary (periodEnd + 1 year). No industry analog
//     for "calendar yearly stub to Jan 1"; Velox doesn't ship it either.
//   - Calendar monthly: snap to first-of-next-month in tenant TZ, so the
//     boundary is always the 1st regardless of the input day-of-month
//     (anchorDay is unused on this path). A high day-of-month only reaches
//     here transiently — the first-period stub when a sub is activated on
//     the 29th/30th/31st (cross-interval plan swaps re-anchor to `now`, they
//     do NOT carry an anniversary day onto a calendar sub).
//   - Anniversary monthly: advance to the anchor day-of-month, clamped to the
//     target month's last day (ADR-055), from the stored anchorDay — so a
//     Jan-31 anchor yields Jan 31, Feb 28, Mar 31, … and never ratchets off
//     month-end.
//
// Returns the new periodEnd. The new periodStart = the old periodEnd
// at every cycle close (continuous coverage). Caller passes
// `next_billing_at = new periodEnd` so the scheduler picks the sub up
// at the right boundary.
//
// Replaces the legacy interval-only `advanceBillingPeriod(from, interval)`
// used in cycle close, which silently preserved a drifted anchor across
// plan-interval changes (industry-standard per Stripe-flexible / Lago /
// Chargebee but conflicts with the operator's calendar-billing intent).
func NextBillingPeriodEnd(periodEnd time.Time, billingTime SubscriptionBillingTime, interval BillingInterval, loc *time.Location, anchorDay int) time.Time {
	if interval == BillingYearly {
		// Anniversary yearly: advance in tenant TZ (ADR-058) AND clamp the
		// anchor day to the target month's length (ADR-055) so a Feb-29
		// (leap) anchor bills Feb 28 in non-leap years and restores to Feb 29
		// the next leap year — instead of Go's AddDate overflowing Feb 29 +
		// 1yr to Mar 1 and ratcheting the anniversary off February forever.
		return advanceAnchored(periodEnd, interval, loc, anchorDay)
	}
	if billingTime == BillingTimeCalendar {
		// Snap to the 1st of periodEnd's month in tenant TZ, THEN add a
		// month — so a day-29/30/31 anchor never overflows a short month.
		// Pre-fix this added the month first (Jan 31 + 1mo = Mar 3 in Go's
		// overflow), then snapped to the 1st of the *result's* month →
		// Mar 1, silently skipping February (ADR-058 Root C). Adding the
		// month in `loc` also keeps the boundary in the tenant's zone.
		// anchorDay is irrelevant here — calendar always lands on the 1st.
		return addIntervalIn(BeginningOfMonthIn(periodEnd, loc), BillingMonthly, loc)
	}
	// Anniversary monthly: advance to the same day-of-month, clamped to the
	// target month's last day (ADR-055). Computed from the stored anchorDay,
	// not periodEnd's (possibly already month-end-clamped) day, so a Jan-31
	// anchor restores to the 31st in long months: Jan 31, Feb 28, Mar 31, …
	return advanceAnchored(periodEnd, interval, loc, anchorDay)
}

// advanceAnchored advances `t` by one interval (+1 month / +1 year) in tenant
// TZ, placing the operator's original anchor day-of-month CLAMPED to the
// target month's last day. This is the ADR-055 fix for the anniversary
// month-end ratchet: because the historical advance added onto the previously
// computed (already-drifted) boundary, a day-29/30/31 anchor permanently lost
// its billing day after the first short month (Jan 31 → Mar 3 → Apr 3 …).
// Clamping from the stored anchorDay both prevents the overflow AND restores
// the higher day in long months (min(anchorDay, lastDay)), matching Stripe /
// Chargebee / Lago end-of-month behavior.
//
// anchorDay <= 0 means "unknown" (legacy rows pre-migration-0120): we fall
// back to the historical addIntervalIn path so the column is additive and the
// behavior is unchanged when it is unset.
func advanceAnchored(t time.Time, interval BillingInterval, loc *time.Location, anchorDay int) time.Time {
	if loc == nil {
		loc = time.UTC
	}
	if anchorDay <= 0 {
		return addIntervalIn(t, interval, loc)
	}
	local := t.In(loc)
	var year int
	var month time.Month
	if interval == BillingYearly {
		year, month = local.Year()+1, local.Month()
	} else {
		// First-of-next-month normalizes December → January of next year.
		fn := time.Date(local.Year(), local.Month()+1, 1, 0, 0, 0, 0, loc)
		year, month = fn.Year(), fn.Month()
	}
	day := anchorDay
	if last := lastDayOfMonthIn(year, month, loc); day > last {
		day = last
	}
	return time.Date(year, month, day, local.Hour(), local.Minute(), local.Second(), local.Nanosecond(), loc).UTC()
}

// lastDayOfMonthIn returns the last calendar day (28/29/30/31) of the given
// year+month in loc. Uses the day-0-of-next-month trick, which Go normalizes
// (month+1 wraps December → January of the next year correctly).
func lastDayOfMonthIn(year int, month time.Month, loc *time.Location) int {
	return time.Date(year, month+1, 0, 0, 0, 0, 0, loc).Day()
}

// AnchorDayFor returns the billing anchor day-of-month (1..31) to persist for a
// subscription, read from the first period's start in the tenant TZ. It is the
// stable reference advanceAnchored clamps against. Calendar-monthly subs return
// 0 (their boundary is always the 1st, so the anchor is meaningless and the
// clamp must not fire on the proration denominator); yearly and
// anniversary-monthly subs return the real day-of-month.
func AnchorDayFor(periodStart time.Time, billingTime SubscriptionBillingTime, interval BillingInterval, loc *time.Location) int {
	if loc == nil {
		loc = time.UTC
	}
	if interval != BillingYearly && billingTime == BillingTimeCalendar {
		return 0
	}
	return periodStart.In(loc).Day()
}

// IsPeriodStartOnAnchor reports whether t falls on the subscription's billing
// anchor day-of-month in loc, with month-end clamping (ADR-055: a day-31 anchor
// is on-anchor at Feb 28). It is the "is this a clean anniversary boundary?"
// test the in_advance base-fee proration uses to tell a FULL upcoming cycle
// (on-anchor start) apart from a re-anchor SEAM (off-anchor start) — the latter
// produced when the org timezone changes (ADR-077, resolved live each roll) and
// the stored boundary is re-resolved in the new zone a few hours off the anchor.
//
// anchorDay <= 0 is calendar billing, which snaps to a fixed grid (the 1st) and
// whose proration denominator never collapses; it is reported on-anchor so the
// caller's calendar path is untouched.
func IsPeriodStartOnAnchor(t time.Time, anchorDay int, loc *time.Location) bool {
	if anchorDay <= 0 {
		return true
	}
	if loc == nil {
		loc = time.UTC
	}
	local := t.In(loc)
	clamped := anchorDay
	if last := lastDayOfMonthIn(local.Year(), local.Month(), loc); clamped > last {
		clamped = last
	}
	return local.Day() == clamped
}

// addIntervalIn advances `t` by one billing interval (+1 month, or +1 year for
// yearly), performing the calendar add in `loc` so the result is anchored to
// the tenant's timezone rather than `t`'s ambient Location or the host
// time.Local (ADR-058). Returns a UTC instant. loc=nil falls back to UTC.
//
// This is the single fix for the timezone date-math defect class: every
// month/year advance MUST go through here (or NextBillingPeriodEnd, which
// does), so the result no longer varies with whether the input time.Time was
// freshly built in UTC or DB-scanned as time.Local. Day-grade adds
// (AddDate(0,0,N) for trial/due dates) are TZ-invariant and need not route here.
func addIntervalIn(t time.Time, interval BillingInterval, loc *time.Location) time.Time {
	if loc == nil {
		loc = time.UTC
	}
	local := t.In(loc)
	if interval == BillingYearly {
		return local.AddDate(1, 0, 0).UTC()
	}
	return local.AddDate(0, 1, 0).UTC()
}

// AddBillingInterval returns t advanced by exactly one billing interval —
// the anniversary advance: +1 year for yearly, otherwise +1 month. It is the
// SINGLE source of truth for "one full cycle from an anchor".
//
// This is the denominator proration MUST divide by — the FULL cycle length,
// NOT the current period length. On a stub/partial period (mid-cycle signup,
// e.g. a 14-day period of a 30-day monthly cycle) the current period is
// shorter than a cycle, so dividing by it over-charges upgrades and
// over-credits downgrades. The engine's day-1 stub base fee is prorated against
// this same full cycle (segDays/fullCycleDays), and the subscription handler's
// immediate plan-change proration now derives its denominator from here too —
// so the two paths can never disagree (the defect class that produced the
// stub-period proration mischarge).
//
// Deliberately calendar-agnostic. For the calendar-snapping cycle-CLOSE
// advance (which re-anchors to the 1st of the next month in tenant TZ), use
// NextBillingPeriodEnd instead.
//
// `loc` is the tenant's billing timezone: the advance is computed in `loc`
// (ADR-058) so the full-cycle length is independent of whether `t` was built
// in UTC or DB-scanned as time.Local. loc=nil falls back to UTC. Pre-fix this
// took no loc and did a bare AddDate on `t`'s ambient Location, making the
// proration denominator host-TZ-dependent (30 vs 31 for an offset-TZ tenant).
func AddBillingInterval(t time.Time, interval BillingInterval, loc *time.Location, anchorDay int) time.Time {
	return advanceAnchored(t, interval, loc, anchorDay)
}

// PauseCollectionBehavior controls what the engine does with the invoice it
// would normally finalize during a paused-collection cycle. v1 supports
// only KeepAsDraft; the other Stripe modes (mark_uncollectible, void) need
// an "uncollectible" invoice status that doesn't exist in Velox yet.
type PauseCollectionBehavior string

const (
	PauseCollectionKeepAsDraft PauseCollectionBehavior = "keep_as_draft"
)

// PauseCollection is the per-subscription collection-pause state. The
// pointer-on-Subscription form mirrors Stripe's nullable subscription.
// pause_collection — nil = running normally, non-nil = paused.
type PauseCollection struct {
	Behavior  PauseCollectionBehavior `json:"behavior"`
	ResumesAt *time.Time              `json:"resumes_at,omitempty"`
}

// SubscriptionItemThreshold is one configured per-item usage cap on a
// subscription. UsageGTE is decimal because meter quantities can be
// fractional (cached-token ratios, GPU-hours) and we want round-trip
// precision in the wire contract; Postgres column is NUMERIC(38,12).
type SubscriptionItemThreshold struct {
	SubscriptionItemID string          `json:"subscription_item_id"`
	UsageGTE           decimal.Decimal `json:"usage_gte"`
}

// BillingThresholds is the Stripe-parity hard-cap configuration. Two
// independent threshold types:
//
//   - AmountGTE: the running cycle subtotal (cents) at which the engine
//     fires an early finalize. Currency is the subscription's own
//     currency; multi-currency subs are rejected at PATCH time.
//
//   - ItemThresholds: per-subscription-item quantity caps. When any
//     item's running cycle quantity crosses its UsageGTE, the engine
//     fires the same early finalize.
//
// Either alone or both. First to fire wins.
//
// ResetBillingCycle controls whether the cycle resets after fire. When
// true (default), the new cycle starts at fire time and the next bill
// is the natural cycle invoice for the new cycle. When false, the
// original cycle continues and a second invoice fires at the natural
// cycle end with whatever residual usage accumulated.
type BillingThresholds struct {
	AmountGTE         int64                       `json:"amount_gte,omitempty"`
	ResetBillingCycle bool                        `json:"reset_billing_cycle"`
	ItemThresholds    []SubscriptionItemThreshold `json:"item_thresholds"`
}

// SubscriptionItem is a single priced line on a subscription. A subscription
// holds one or more items; each item pairs a plan with a quantity and carries
// its own pending-plan-change state so upgrades and downgrades can schedule
// independently per line.
type SubscriptionItem struct {
	ID                     string     `json:"id"`
	TenantID               string     `json:"tenant_id,omitempty"`
	SubscriptionID         string     `json:"subscription_id"`
	PlanID                 string     `json:"plan_id"`
	Quantity               int64      `json:"quantity"`
	Metadata               []byte     `json:"metadata,omitempty"` // raw JSONB
	PendingPlanID          string     `json:"pending_plan_id,omitempty"`
	PendingPlanEffectiveAt *time.Time `json:"pending_plan_effective_at,omitempty"`
	// PlanChangedAt stamps the last immediate plan swap on this item. Feeds the
	// per-item proration dedup key (invoices.source_plan_changed_at plus
	// source_subscription_item_id) so retries of the same change converge on
	// the existing invoice. Nil until the first immediate plan change.
	PlanChangedAt *time.Time `json:"plan_changed_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

// SubscriptionItemChange is one row from the subscription_item_changes
// audit table (migration 0029). Captures every plan/quantity mutation
// on a subscription_item with both before- and after-state. Drives the
// segment-aware base-fee billing at cycle close: each row demarcates
// a [pre-change, post-change] boundary the engine uses to bill each
// segment at its own plan + quantity rate. Matches the Lago /
// Chargebee / Orb shape for mid-period proration.
type SubscriptionItemChange struct {
	ID                 string    `json:"id"`
	TenantID           string    `json:"tenant_id"`
	SubscriptionID     string    `json:"subscription_id"`
	SubscriptionItemID string    `json:"subscription_item_id,omitempty"`
	ChangeType         string    `json:"change_type"` // add | remove | plan | quantity
	FromPlanID         string    `json:"from_plan_id,omitempty"`
	ToPlanID           string    `json:"to_plan_id,omitempty"`
	FromQuantity       int64     `json:"from_quantity,omitempty"`
	ToQuantity         int64     `json:"to_quantity,omitempty"`
	ChangedAt          time.Time `json:"changed_at"`
	CreatedAt          time.Time `json:"created_at"`
}

type Subscription struct {
	ID           string                  `json:"id"`
	TenantID     string                  `json:"tenant_id,omitempty"`
	Code         string                  `json:"code"`
	DisplayName  string                  `json:"display_name"`
	CustomerID   string                  `json:"customer_id"`
	Status       SubscriptionStatus      `json:"status"`
	BillingTime  SubscriptionBillingTime `json:"billing_time"`
	TrialStartAt *time.Time              `json:"trial_start_at,omitempty"`
	TrialEndAt   *time.Time              `json:"trial_end_at,omitempty"`
	StartedAt    *time.Time              `json:"started_at,omitempty"`
	ActivatedAt  *time.Time              `json:"activated_at,omitempty"`
	CanceledAt   *time.Time              `json:"canceled_at,omitempty"`
	// CancelAt is a future timestamp at which the billing cycle should
	// transition the subscription to canceled. Distinct from CanceledAt
	// (past-tense, set only when the cancel has fired). Nil means no
	// timestamp-based schedule.
	CancelAt *time.Time `json:"cancel_at,omitempty"`
	// CancelAtPeriodEnd is the soft-cancel flag. When true and the cycle
	// scan observes effectiveNow >= CurrentBillingPeriodEnd, the engine
	// transitions the sub to canceled and skips the next invoice. Setting
	// false before the boundary fires undoes the schedule.
	CancelAtPeriodEnd bool `json:"cancel_at_period_end"`
	// CancelEffectiveAt is the DERIVED, read-only answer to "when does this
	// subscription actually cancel?" (ADR-069). Velox's
	// current_billing_period_end on a TRIALING sub is the end of the first
	// PAID period (unlike Stripe, where period_end == trial_end while
	// trialing), so every consumer re-deriving the date from the flag needs
	// status-aware logic — and both existing UI surfaces got it wrong.
	// Populated at scan time from one choke point; never persisted:
	//   trialing + at_period_end → trial_end_at (free cancel)
	//   otherwise  at_period_end → current_billing_period_end
	//   explicit cancel_at       → cancel_at
	CancelEffectiveAt *time.Time `json:"cancel_effective_at,omitempty"`
	// PauseCollection holds the Stripe-parity collection-pause state. When
	// non-nil, the cycle still advances but the engine generates the
	// invoice as draft and skips finalize/charge/dunning. Distinct from
	// Status=paused, which is the hard freeze (sub excluded from
	// GetDueBilling entirely). Nil means collection is running normally.
	PauseCollection *PauseCollection `json:"pause_collection,omitempty"`
	// BillingThresholds holds the Stripe-parity hard-cap config. When
	// non-nil, the threshold scan tick computes the in-cycle running
	// totals and fires an early finalize once any configured cap is
	// crossed. Nil means no threshold; the cycle scan is the only
	// invoice-emitting path.
	BillingThresholds         *BillingThresholds `json:"billing_thresholds,omitempty"`
	CurrentBillingPeriodStart *time.Time         `json:"current_billing_period_start,omitempty"`
	CurrentBillingPeriodEnd   *time.Time         `json:"current_billing_period_end,omitempty"`
	// CurrentBillingPeriodDisplay is the human period string with the INCLUSIVE
	// last covered day ("Jun 1, 2028 – Jun 30, 2028"), rendered date-only in the
	// org's billing timezone (ADR-077) — the exact analogue of
	// Invoice.BillingPeriodDisplay. Computed on read (never persisted); the raw
	// half-open Current*Period*Start/End above are unchanged (SDK/wire contract
	// stays half-open). Empty when the sub has no current period. Backend-authored
	// so the inclusive end can't drift across the Go and TS runtimes.
	CurrentBillingPeriodDisplay string     `json:"current_billing_period_display,omitempty"`
	NextBillingAt               *time.Time `json:"next_billing_at,omitempty"`
	// BillingAnchorDay is the operator's intended billing day-of-month (1..31)
	// for yearly and anniversary-monthly subs — the stable reference the
	// month-end clamp advances from so a Jan-31 anchor bills Jan 31, Feb 28,
	// Mar 31, … instead of ratcheting off month-end (ADR-055). 0 for
	// calendar-monthly subs (always the 1st) and legacy/unset rows (the
	// advance then falls back to the historical path). Recomputed whenever the
	// cycle re-anchors to "now" (cross-interval swap, threshold reset).
	BillingAnchorDay int `json:"billing_anchor_day,omitempty"`
	// Subscriptions have no per-sub billing timezone: every sub in a tenant
	// bills in the one org timezone (default UTC), resolved live from tenant
	// settings at compute time and denormalized onto the INVOICE at issue for
	// historical stability (ADR-077, superseding the ADR-074 per-sub snapshot).
	UsageCapUnits *int64    `json:"usage_cap_units,omitempty"` // Max usage units per billing period (nil = unlimited)
	OverageAction string    `json:"overage_action,omitempty"`  // "block" or "charge" (default: charge)
	TestClockID   string    `json:"test_clock_id,omitempty"`   // Test mode only — attached simulator clock
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`

	// Items is populated by store reads that hydrate the subscription with
	// its current priced lines. Writes through Store.Create require a
	// non-empty Items slice; runtime lookups (billing engine, coupon apply)
	// iterate this. A subscription without items is not a valid state.
	Items []SubscriptionItem `json:"items,omitempty"`
}

// ErrTrialCancelDue: a trialing→active store transition was blocked because
// a cancel schedule is due at trial end (ADR-069). Every activation writer
// (subscription service scans, EndTrial, the billing engine's trial branch)
// routes this to the dedicated free CancelAtTrialEnd transition instead of
// activating and billing a customer who canceled. Lives in domain so the
// billing engine can route on it without a peer import.
var ErrTrialCancelDue = errors.New("trial has a cancel schedule due — route to CancelAtTrialEnd")

// ErrTrialCancelConflict: CancelAtTrialEnd's CAS matched no row — the
// schedule was cleared, the trial extended, or another site already
// canceled. The caller treats the sub as NOT handled this pass.
var ErrTrialCancelConflict = errors.New("trial-end cancel conflicted — re-read and route")

// DeriveCancelEffectiveAt computes CancelEffectiveAt from the schedule
// fields (ADR-069). Called from the subscription store's row scan — the one
// choke point every read path flows through.
func (s *Subscription) DeriveCancelEffectiveAt() {
	s.CancelEffectiveAt = nil
	switch {
	case s.CancelAt != nil:
		t := *s.CancelAt
		s.CancelEffectiveAt = &t
	case s.CancelAtPeriodEnd && s.Status == SubscriptionTrialing && s.TrialEndAt != nil:
		t := *s.TrialEndAt
		s.CancelEffectiveAt = &t
	case s.CancelAtPeriodEnd && s.CurrentBillingPeriodEnd != nil:
		t := *s.CurrentBillingPeriodEnd
		s.CancelEffectiveAt = &t
	}
}
