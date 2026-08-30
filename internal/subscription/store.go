package subscription

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// SubPlanCurrency is one (subscription, plan currency) pair from the
// customer currency-pin query (ADR-100).
type SubPlanCurrency struct {
	SubCode  string
	Currency string
}

// ErrNotPaused is returned by ClearPauseCollection when the row exists but
// carries no pause — the clear changed nothing, so nothing that announces a
// resume may fire. Sentinel, not a DomainError: the operator path treats it
// as an idempotent no-op, the schedule paths skip the row.
var ErrNotPaused = errors.New("subscription: collection is not paused")

// ErrWatermarkMoved is returned by AdvanceBillingCycle when next_billing_at
// no longer equals the value the caller read: another leader already advanced
// the cycle. The write is a benign no-op for the caller — the invoice
// idempotency index already made the second invoice impossible; this makes
// the second WATERMARK write impossible too (ADR-114 §Consequences).
var ErrWatermarkMoved = errors.New("subscription: billing watermark already advanced")

type Store interface {
	// CustomerSubPlanCurrencies backs the ADR-100 customer currency pin:
	// distinct plan currencies (incl. pending swaps) across the customer's
	// non-terminal subs, excluding excludeSubID ("" = none).
	CustomerSubPlanCurrencies(ctx context.Context, tenantID, customerID, excludeSubID string) ([]SubPlanCurrency, error)
	// Create writes a subscription plus its initial items in one transaction.
	// sub.Items is required and must be non-empty. The returned subscription
	// is hydrated with the inserted items (including their assigned IDs).
	Create(ctx context.Context, tenantID string, s domain.Subscription) (domain.Subscription, error)

	// CreateWithBill writes the subscription + items AND runs billFn (the day-1
	// in_advance invoice insert) in the SAME transaction, so a billing failure
	// rolls the create back rather than silently dropping the first-period base
	// fee (the cycle scheduler skips the just-elapsed in_advance segment, so a
	// lost day-1 invoice is a permanent revenue leak). billFn may be nil.
	CreateWithBill(ctx context.Context, tenantID string, s domain.Subscription, billFn func(tx *sql.Tx, created domain.Subscription) error) (domain.Subscription, error)

	// Get, List, and GetDueBilling hydrate the returned subscriptions with
	// their current items via a second query. Callers that need the items
	// should rely on sub.Items directly — a subscription without items is
	// not a valid state and indicates a hydration bug.
	Get(ctx context.Context, tenantID, id string) (domain.Subscription, error)
	List(ctx context.Context, filter ListFilter) ([]domain.Subscription, int, error)
	GetDueBilling(ctx context.Context, before time.Time, limit int) ([]domain.Subscription, error)

	// Update mutates subscription-level columns only (status, billing period,
	// trial, etc). Item mutations use the item-scoped methods below so a
	// partial update to one field doesn't accidentally overwrite an entire
	// item list.
	UpdateBillingCycle(ctx context.Context, tenantID, id string, periodStart, periodEnd, nextBillingAt time.Time, anchorDay int) error
	// UpdateBillingCycleTx is the in-tx variant — advances the watermark on the
	// caller's tx so a coordinator (e.g. the atomic cross-interval swap) can move
	// the billing period only when the same tx also commits the new-period invoice.
	UpdateBillingCycleTx(ctx context.Context, tx *sql.Tx, tenantID, id string, periodStart, periodEnd, nextBillingAt time.Time, anchorDay int) error

	// CancelAtomic executes a conditional UPDATE that only transitions
	// when the row is in an allowed source state. Closes the
	// read-check-write race where two goroutines observing the same
	// non-terminal status could each apply a different target state
	// across two transactions. When the source state does not match, a
	// re-query distinguishes not-found from a stale-status conflict and
	// surfaces the current status in the error message.
	CancelAtomic(ctx context.Context, tenantID, id string) (domain.Subscription, error)

	// CancelAtomicWithBill cancels the subscription AND runs billFn (the
	// final-on-cancel partial-period invoice insert) in the SAME transaction, so
	// a billing failure rolls the cancel back rather than leaving a canceled sub
	// with an uninvoiced partial period (a revenue leak — no final-on-cancel
	// reconciler exists). billFn may be nil.
	CancelAtomicWithBill(ctx context.Context, tenantID, id string, billFn func(tx *sql.Tx, canceled domain.Subscription) error) (domain.Subscription, error)

	// ScheduleCancellation persists a future cancel intent. cancelAt is a
	// nullable timestamp; cancelAtPeriodEnd is the soft-cancel flag. Both
	// may be set in the same call (the boundary that fires first wins);
	// the service layer rejects "schedule both at once" so callers don't
	// pass them together in practice.
	// ScheduleCancellation writes the cancel schedule, CAS-ing on the status
	// the caller validated the intent under (ADR-069 — the flag's meaning is
	// status-polymorphic; landing it on a just-activated sub would silently
	// invert the operator's promise). Status-changed conflicts return
	// InvalidState (409).
	ScheduleCancellation(ctx context.Context, tenantID, id string, cancelAt *time.Time, cancelAtPeriodEnd bool, observedStatus domain.SubscriptionStatus) (domain.Subscription, error)
	// CancelAtTrialEnd is the dedicated free trialing→canceled transition
	// (ADR-069): NO invoice, canceled_at = trial_end_at, schedule cleared.
	// CAS on observed trial_end + a due schedule; conflicts return
	// ErrTrialCancelConflict (caller re-reads and routes).
	CancelAtTrialEnd(ctx context.Context, tenantID, id string, observedTrialEnd time.Time) (domain.Subscription, error)

	// ClearScheduledCancellation clears any cancel_at and cancel_at_period_end
	// on the row. Idempotent.
	ClearScheduledCancellation(ctx context.Context, tenantID, id string) (domain.Subscription, error)

	// FireScheduledCancellation atomically transitions a sub to canceled when
	// its scheduled cancel boundary has been crossed. Called by the billing
	// engine cycle scan. Returns errs.InvalidState if the row was no longer
	// active by the time the UPDATE ran.
	FireScheduledCancellation(ctx context.Context, tenantID, id string, at time.Time) (domain.Subscription, error)

	// FireScheduledCancellationWithBill is the ADR-097 mid-period variant:
	// flip + billFn (final partial invoice + relief drafts) in ONE tx, CAS
	// on status='active' AND cancel_at = at so a concurrent unschedule or
	// re-schedule defeats the fire. errs.InvalidState covers both benign
	// race outcomes (already terminated; schedule changed).
	FireScheduledCancellationWithBill(ctx context.Context, tenantID, id string, at time.Time, billFn func(tx *sql.Tx, canceled domain.Subscription) error) (domain.Subscription, error)

	// SetPauseCollection writes the (behavior, resumes_at) pair onto the row.
	// Distinct from the removed hard-pause (Service.Pause/PauseAtomic, dropped
	// in PR-8 / migration 0090) — pause_collection keeps the cycle running but
	// neuters the financial side; the row's status field is not touched.
	// Permitted on any non-terminal status (active, paused, draft); a paused
	// (hard) sub can also have collection paused, and clearing one without
	// the other is supported. Service layer enforces behavior whitelist.
	SetPauseCollection(ctx context.Context, tenantID, id string, pc domain.PauseCollection) (domain.Subscription, error)

	// ClearPauseCollection nulls the pause_collection_* columns as a CAS: it
	// changes the row only if a pause is set and returns ErrNotPaused
	// otherwise (ErrNotFound if the row does not exist). Callers that fire
	// `subscription.collection_resumed` must fire it only on success — two
	// leaders (a failover window, a resumed stale process) clearing the same
	// pause used to each fire the event (HA-readiness hazard 5).
	ClearPauseCollection(ctx context.Context, tenantID, id string) (domain.Subscription, error)

	// SetBillingThresholds writes the (amount_gte, reset_cycle, item_thresholds)
	// triple onto the row in one transaction. Replaces the full item_thresholds
	// set — the per-item rows for any item not in the new slice are deleted.
	// Rejects rows in canceled/archived since a threshold on a terminated sub
	// has no meaning. The service layer is responsible for validating that
	// every subscription_item_id in t.ItemThresholds belongs to this
	// subscription.
	SetBillingThresholds(ctx context.Context, tenantID, id string, t domain.BillingThresholds) (domain.Subscription, error)

	// ClearBillingThresholds nulls the amount_gte column and deletes every
	// row in subscription_item_thresholds for this subscription. Idempotent
	// — clearing on a sub that has no threshold returns the unchanged
	// subscription.
	ClearBillingThresholds(ctx context.Context, tenantID, id string) (domain.Subscription, error)

	// ListWithThresholds returns subscriptions in the given livemode partition
	// that have at least one threshold configured (amount or per-item) and are
	// in active or trialing status. Used by the threshold scan tick. Result is
	// hydrated with items + thresholds.
	ListWithThresholds(ctx context.Context, livemode bool, afterID string, limit int) ([]domain.Subscription, error)

	// ListWithThresholdsForClock is the catchup-path counterpart to
	// ListWithThresholds. ADR-029 Phase 3: clock-pinned threshold
	// scans fire only on operator Advance, never on the wall-clock
	// tick.
	ListWithThresholdsForClock(ctx context.Context, tenantID, clockID, afterID string, limit int) ([]domain.Subscription, error)

	// ListExpiredTrialsForClock returns subs whose trial_end_at has
	// elapsed in simulated time but whose status is still 'trialing' —
	// the trial-expiry catchup scan picks these up at Phase 0.5 and
	// flips them to active at trial_end_at (not at the later cycle
	// close). Without this scan, status stays 'trialing' until the
	// engine wakes at next_billing_at, which for a calendar+trial sub
	// can be ~30 days after the actual trial-end (sub created Nov 29
	// + 14d trial → trial_end Dec 13, next_billing_at Jan 1 →
	// status='trialing' from Dec 13 to Jan 1).
	//
	// Hydrated with items so the caller can fire BillOnCreate for
	// in_advance items without a round-trip.
	ListExpiredTrialsForClock(ctx context.Context, tenantID, clockID string, frozen time.Time, limit int) ([]domain.Subscription, error)

	// ListExpiredTrials is the wall-clock counterpart for the cron
	// tick — returns non-clock-pinned subs whose `trial_end_at` has
	// elapsed in wall-clock time but whose status is still 'trialing'.
	// ADR-028 disjoint flows: this query explicitly excludes
	// `test_clock_id IS NOT NULL` rows (those run through the
	// catchup orchestrator's Phase 0.5 instead). Scoped per livemode
	// partition by the caller's ctx (the scheduler fans out per mode).
	ListExpiredTrials(ctx context.Context, before time.Time, livemode bool, limit int) ([]domain.Subscription, error)

	// ListExpiredPauseCollections returns wall-clock subs whose
	// pause_collection_resumes_at has passed. The scheduler tick walks
	// these and clears the pause + writes audit (triggered_by=schedule).
	// Stripe-parity: resume happens AT resumes_at, not at the next cycle
	// close. Excludes clock-pinned rows (ADR-028 — those flow through
	// ListExpiredPauseCollectionsForClock at catchup time).
	ListExpiredPauseCollections(ctx context.Context, before time.Time, limit int) ([]domain.Subscription, error)

	// ListExpiredPauseCollectionsForClock is the clock-scoped counterpart
	// invoked from the catchup orchestrator (new phase that pairs with
	// trial-expiry — runs before cycle billing so an Advance that crosses
	// resumes_at unpauses the sub in the same window).
	ListExpiredPauseCollectionsForClock(ctx context.Context, tenantID, clockID string, frozen time.Time, limit int) ([]domain.Subscription, error)

	// ActivateAfterTrial atomically transitions a subscription from
	// 'trialing' to 'active'. Sets activated_at = `at` if the column is
	// still NULL (preserves the original activation timestamp on
	// re-runs). Does NOT touch period boundaries — the caller (the
	// billing engine's cycle-close path) has already advanced them to
	// the right cycle. For operator-driven early-EndTrial use
	// EndTrialEarly which resets the period anchor to the activation
	// instant.
	ActivateAfterTrial(ctx context.Context, tenantID, id string, at time.Time) (domain.Subscription, error)

	// ActivateAfterTrialWithBill flips trialing→active AND runs billFn (the day-1
	// in_advance invoice insert) in the SAME transaction, so a billing failure
	// rolls the activation back rather than leaving an active sub with no
	// first-period invoice (revenue leak). billFn may be nil.
	ActivateAfterTrialWithBill(ctx context.Context, tenantID, id string, at time.Time, billFn func(tx *sql.Tx, activated domain.Subscription) error) (domain.Subscription, error)

	// ActivateDraftWithBill atomically flips a DRAFT subscription to active —
	// stamping activation instants, the first billing period, and the anchor
	// day — AND runs billFn (the day-1 in_advance invoice) in the SAME tx.
	// The draft→activate sibling of ActivateAfterTrialWithBill: pre-fix the
	// plain Activate flipped status without ever billing, so a draft-then-
	// activated in_advance sub's first base fee was silently never invoiced
	// (the revenue-leak class ADR-056 fixed for trial→active). A billFn error
	// rolls the activation back — never an active sub with a lost invoice.
	ActivateDraftWithBill(ctx context.Context, tenantID, id string, at, periodStart, periodEnd time.Time, anchorDay int, billFn func(tx *sql.Tx, activated domain.Subscription) error) (domain.Subscription, error)

	// EndTrialEarly is the operator-driven counterpart to
	// ActivateAfterTrial. In one atomic UPDATE it: flips status to
	// 'active', stamps activated_at if currently NULL, truncates
	// trial_end_at to `at` (historical record: the trial ended early),
	// and replaces the period anchor with (periodStart, periodEnd,
	// nextBilling) — the caller computes these via firstPeriodForActivate
	// so the new period reflects "billing starts now."
	//
	// Pre-fix, EndTrial called ActivateAfterTrial and left period
	// boundaries pointing at the original post-trial anchor — a Dec 5
	// EndTrial on a sub whose period was Dec 13 → Jan 1 produced no
	// billing until Jan 1 (8 unbilled days during the bridge from
	// early-end to original-anchor).
	//
	// Returns errs.InvalidState if the row's status is not 'trialing'
	// at UPDATE time.
	EndTrialEarly(ctx context.Context, tenantID, id string, at, periodStart, periodEnd, nextBilling time.Time, anchorDay int) (domain.Subscription, error)

	// EndTrialEarlyWithBill flips trialing→active (resetting the period anchor)
	// AND runs billFn (the day-1 in_advance invoice insert) in the SAME
	// transaction, so a billing failure rolls the early-end back rather than
	// leaving an active sub with no first-period invoice (revenue leak). billFn
	// may be nil.
	EndTrialEarlyWithBill(ctx context.Context, tenantID, id string, at, periodStart, periodEnd, nextBilling time.Time, anchorDay int, billFn func(tx *sql.Tx, activated domain.Subscription) error) (domain.Subscription, error)

	// ExtendTrial atomically updates trial_end_at AND recomputes the
	// period anchor on a 'trialing' row. Mirrors EndTrialEarly's shape
	// for the opposite operator action: pushing trial_end later moves
	// the first-chargeable-cycle anchor to match. Without the period
	// recompute, extending past the original current_period_end would
	// silently drop the stub between new_trial_end and the next cycle
	// close (same bug class as the pre-fix calendar+trial Create
	// branch).
	//
	// The service layer is responsible for rejecting newTrialEnd values
	// that don't make sense (in the past, or before the existing
	// trial_end_at — those callers should use EndTrialEarly instead).
	// Returns errs.InvalidState if the row's status is not 'trialing'
	// at UPDATE time.
	ExtendTrial(ctx context.Context, tenantID, id string, newTrialEnd, periodStart, periodEnd, nextBilling time.Time, anchorDay int) (domain.Subscription, error)

	// ---- Subscription items ----

	// ListItems returns all items for a subscription ordered by created_at.
	ListItems(ctx context.Context, tenantID, subscriptionID string) ([]domain.SubscriptionItem, error)

	// GetItem returns a single item by ID, scoped to its parent subscription
	// (callers verify the item belongs to the expected subscription before
	// mutating it).
	GetItem(ctx context.Context, tenantID, itemID string) (domain.SubscriptionItem, error)

	// AddItem appends a new item to a subscription. UNIQUE (subscription_id,
	// plan_id) prevents a second item with the same plan; the constraint
	// violation surfaces as errs.ErrAlreadyExists.
	AddItem(ctx context.Context, tenantID string, item domain.SubscriptionItem) (domain.SubscriptionItem, error)
	// AddItemTx is the in-transaction variant used by the handler's
	// atomic AddItem-with-proration flow — the caller owns the tx and
	// composes the item insert with downstream proration writes so a
	// failure on either side rolls back both. ADR-030 atomic-proration
	// follow-through (2026-05-29).
	AddItemTx(ctx context.Context, tx *sql.Tx, tenantID string, item domain.SubscriptionItem) (domain.SubscriptionItem, error)

	// UpdateItemQuantity mutates only the quantity — plan and pending-change
	// fields are left untouched. Used for quantity-only PATCH. Returns the
	// updated item.
	UpdateItemQuantity(ctx context.Context, tenantID, itemID string, quantity int64) (domain.SubscriptionItem, error)
	// UpdateItemQuantityTx is the in-transaction variant — same atomicity
	// rationale as AddItemTx. ADR-030 atomic-proration follow-through.
	UpdateItemQuantityTx(ctx context.Context, tx *sql.Tx, tenantID, itemID string, quantity int64) (domain.SubscriptionItem, error)

	// ApplyItemPlanImmediately swaps plan_id on the item and stamps
	// plan_changed_at. Used for immediate plan changes. Clears any pending
	// change since the caller just superseded it.
	ApplyItemPlanImmediately(ctx context.Context, tenantID, itemID, newPlanID string, changedAt time.Time) (domain.SubscriptionItem, error)
	// ApplyItemPlanImmediatelyTx is the in-transaction variant.
	ApplyItemPlanImmediatelyTx(ctx context.Context, tx *sql.Tx, tenantID, itemID, newPlanID string, changedAt time.Time) (domain.SubscriptionItem, error)

	// SetItemPendingPlan schedules a plan change on an item.
	SetItemPendingPlan(ctx context.Context, tenantID, itemID, pendingPlanID string, effectiveAt time.Time) (domain.SubscriptionItem, error)

	// ClearItemPendingPlan removes any scheduled plan change on an item.
	// Idempotent — a second call on an item with no pending change returns
	// the unchanged item.
	ClearItemPendingPlan(ctx context.Context, tenantID, itemID string) (domain.SubscriptionItem, error)

	// ApplyDuePendingItemPlansAtomic swaps plan_id ← pending_plan_id for every
	// item under the given subscription whose pending change has come due
	// (pending_plan_effective_at <= now). All applicable items are updated in
	// one statement. Returns the post-swap items. Called by the billing engine
	// at the cycle boundary, mirroring the old ApplyPendingPlanAtomic semantics
	// at item granularity.
	ApplyDuePendingItemPlansAtomic(ctx context.Context, tenantID, subscriptionID string, now time.Time) ([]domain.SubscriptionItem, error)

	// RemoveItem deletes a single item. The service layer is responsible for
	// rejecting removal of the last item on an active subscription (deletion
	// of the last item would leave a subscription with no priced lines,
	// which is not a valid state).
	RemoveItem(ctx context.Context, tenantID, itemID string) error
	// RemoveItemTx is the in-transaction variant — same atomicity
	// rationale as AddItemTx.
	RemoveItemTx(ctx context.Context, tx *sql.Tx, tenantID, itemID string) error

	// FindMeterConflicts returns, for each candidate meter, the live
	// (draft / trialing / active) subscriptions of the customer whose
	// items' plans already bill that meter — including scheduled
	// pending_plan_id swaps, which will start billing at the next cycle
	// boundary. excludeItemID skips one item (the item being plan-swapped;
	// "" for creates and adds). Backs the double-billing guard: usage
	// aggregation is customer+meter scoped with no subscription filter,
	// so two live subs sharing a meter would each invoice the same usage.
	FindMeterConflicts(ctx context.Context, tenantID, customerID, excludeItemID string, meterIDs []string) ([]MeterConflict, error)
}

// MeterConflict names one meter that a live subscription of the customer
// already bills. Returned by FindMeterConflicts for the double-billing guard.
type MeterConflict struct {
	MeterID          string
	SubscriptionID   string
	SubscriptionCode string
}

type ListFilter struct {
	TenantID   string
	CustomerID string
	PlanID     string // Joins subscription_items to filter by item plan
	Status     string
	// Search filters by display_name OR code, case-insensitive
	// substring (ILIKE, metacharacters escaped). Empty = no filter.
	// Backs the dashboard list search box and the command palette.
	Search string
	Limit  int
	Offset int
	// Sort: column from a closed allow-list (validated in store).
	// Empty defaults to created_at.
	Sort string
	// SortDir: "asc" or "desc". Empty defaults to desc.
	SortDir string
}
