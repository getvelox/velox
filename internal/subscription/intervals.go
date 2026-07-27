package subscription

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
)

// ADR-101 Phase 1: billing_intervals dual-write.
//
// Every item mutation records its policy-applied billable lifetime
// [starts_at, ends_at) in the SAME transaction as the item write. The
// subscription_item_changes fact log is untouched — facts stay raw; the
// day-grade policy (ADR-012 + its 2026-07-26 'add'-clamp amendment) is
// applied HERE, once, at write time, instead of re-derived at every read.
// Nothing reads this table yet; the cycle reader cuts over in Phase 3
// behind VELOX_BILLING_INTERVALS_READER. Writers are NEVER gated by that
// flag — the table must be complete from the moment this ships.
//
// Interval writers run on the caller's tx and are loud: a failed interval
// write fails the whole item mutation. The table's exclusion constraints
// (one open per item, no overlaps) turn writer bugs into transaction
// errors instead of silent misbilling.

// intervalSubCtx is the per-mutation policy context: everything the
// interval writers need to decide open/close instants for one
// subscription. Loaded in-tx so it can't drift from the row the mutation
// just touched.
type intervalSubCtx struct {
	subID       string
	tenantID    string
	status      domain.SubscriptionStatus
	periodStart *time.Time
	periodEnd   *time.Time
	loc         *time.Location
}

// intervalSubContext loads the policy context FOR SHARE: item-level
// writers hold a share lock on the subscription row for the rest of
// their tx, so a concurrent cancel (which takes the exclusive row lock
// first, via its status UPDATE) serializes against them — an item
// mutation can't thread an open interval under a committing cancel's
// close-all sweep. Lock order everywhere is subscription row → interval
// rows, so the share lock adds no deadlock cycle.
func intervalSubContext(ctx context.Context, tx *sql.Tx, subscriptionID string) (intervalSubCtx, error) {
	ic := intervalSubCtx{subID: subscriptionID}
	var status, tz string
	err := tx.QueryRowContext(ctx, `
		SELECT s.tenant_id, s.status, s.current_billing_period_start, s.current_billing_period_end,
		       COALESCE(NULLIF(ts.timezone, ''), 'UTC')
		FROM subscriptions s
		LEFT JOIN tenant_settings ts ON ts.tenant_id = s.tenant_id
		WHERE s.id = $1
		FOR SHARE OF s`,
		subscriptionID,
	).Scan(&ic.tenantID, &status, &ic.periodStart, &ic.periodEnd, &tz)
	if err == sql.ErrNoRows {
		return ic, errs.ErrNotFound
	}
	if err != nil {
		return ic, fmt.Errorf("interval sub context %s: %w", subscriptionID, err)
	}
	ic.status = domain.SubscriptionStatus(status)
	loc, err := time.LoadLocation(tz)
	if err != nil {
		// Same collapse the engine's tenantLocation applies (ADR-077): an
		// unparseable tz setting means UTC, not a failed mutation.
		loc = time.UTC
	}
	ic.loc = loc
	return ic, nil
}

// intervalOpen inserts one open interval. livemode is stamped by the
// set_livemode trigger from the tx session, like every mode-aware table.
func intervalOpen(ctx context.Context, tx *sql.Tx, ic intervalSubCtx, itemID, planID string, qty int64, startsAt time.Time, source string) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO billing_intervals (tenant_id, subscription_id, subscription_item_id, plan_id, quantity, starts_at, source)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		ic.tenantID, ic.subID, itemID, planID, qty, startsAt, source,
	)
	if err != nil {
		return fmt.Errorf("open billing interval item %s (%s): %w", itemID, source, err)
	}
	return nil
}

// intervalOpenForItems opens one interval per item at the subscription's
// current_billing_period_start — the creation/activation/trial-convert
// writer. The stored period start is used AS IS: it is already
// day-snapped where policy says so (firstPeriodForActivate /
// firstPeriodAfterTrial), and deliberately mid-day at TZ-seam and
// trial-stub starts — re-snapping here would re-introduce the
// derive-at-read bug class this table exists to end.
func intervalOpenForItems(ctx context.Context, tx *sql.Tx, sub domain.Subscription, source string) error {
	if sub.CurrentBillingPeriodStart == nil {
		return fmt.Errorf("interval open (%s): subscription %s is %s with no current_billing_period_start", source, sub.ID, sub.Status)
	}
	ic := intervalSubCtx{subID: sub.ID, tenantID: sub.TenantID}
	for _, it := range sub.Items {
		if err := intervalOpen(ctx, tx, ic, it.ID, it.PlanID, it.Quantity, *sub.CurrentBillingPeriodStart, source); err != nil {
			return err
		}
	}
	return nil
}

// intervalOpenForAdd is the AddItem writer. Day-grade 'add' clamp
// (ADR-012 amendment 2026-07-26): an item added on the period-start
// calendar day (tenant TZ) bills from periodStart itself — the same rule
// the engine's itemBaseSegments applies to the item's first 'add'; any
// other day opens at the raw instant. Draft/trialing subs open NOTHING —
// their activation writer owns the open (there is no billable lifetime
// before activation).
func intervalOpenForAdd(ctx context.Context, tx *sql.Tx, itemID, subscriptionID, planID string, qty int64, now time.Time) error {
	ic, err := intervalSubContext(ctx, tx, subscriptionID)
	if err != nil {
		return err
	}
	switch ic.status {
	case domain.SubscriptionActive:
		openAt := now
		if ic.periodStart != nil && domain.SameCalendarDayIn(now, *ic.periodStart, ic.loc) {
			openAt = *ic.periodStart
		}
		return intervalOpen(ctx, tx, ic, itemID, planID, qty, openAt, "add")
	case domain.SubscriptionDraft, domain.SubscriptionTrialing:
		return nil
	default:
		return errs.InvalidState(fmt.Sprintf("cannot add item to %s subscription %s", ic.status, subscriptionID))
	}
}

// intervalTransition ends the item's billable state at `at` and starts
// the next one — the plan/quantity writer. `newPlanID` empty keeps the
// plan; `newQty` nil keeps the quantity.
//
// The instant is normally inside the OPEN interval (close it, open the
// successor). It can also land inside a CLOSED one: the engine-down
// catch-up applies a due pending plan at its period gate, which a
// later-instant quantity change may already have sealed past. That is a
// retroactive splice — split the containing interval at `at` and carry
// the new plan onto every subsequent interval (their quantities are
// their own facts and stay). An instant OLDER than the item's entire
// history, or a transition on an item with no open interval on an
// active sub, is a writer bug and fails the tx.
func intervalTransition(ctx context.Context, tx *sql.Tx, itemID, subscriptionID string, at time.Time, newPlanID string, newQty *int64, source string) error {
	ic, err := intervalSubContext(ctx, tx, subscriptionID)
	if err != nil {
		return err
	}

	rows, err := tx.QueryContext(ctx, `
		SELECT id, plan_id, quantity, starts_at, ends_at
		FROM billing_intervals
		WHERE subscription_item_id = $1
		ORDER BY starts_at
		FOR UPDATE`,
		itemID,
	)
	if err != nil {
		return fmt.Errorf("interval transition item %s: %w", itemID, err)
	}
	defer func() { _ = rows.Close() }()

	type ivRow struct {
		id, planID string
		qty        int64
		startsAt   time.Time
		endsAt     *time.Time
	}
	var ivs []ivRow
	for rows.Next() {
		var r ivRow
		if err := rows.Scan(&r.id, &r.planID, &r.qty, &r.startsAt, &r.endsAt); err != nil {
			return err
		}
		ivs = append(ivs, r)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	if len(ivs) == 0 {
		// Never-activated subs have no intervals — their items' plan and
		// quantity are still just draft/trial configuration, and the
		// activation writer opens with whatever values hold at the flip.
		if ic.status == domain.SubscriptionActive {
			return fmt.Errorf("interval transition (%s): active subscription %s item %s has no billing intervals", source, subscriptionID, itemID)
		}
		return nil
	}

	// Locate the interval containing `at`: starts_at <= at < ends_at.
	containing := -1
	for i, r := range ivs {
		if r.startsAt.After(at) {
			break
		}
		if r.endsAt == nil || r.endsAt.After(at) {
			containing = i
			break
		}
	}
	if containing < 0 {
		return fmt.Errorf("interval transition (%s): instant %s is outside item %s's interval history (sub %s)", source, at.UTC().Format(time.RFC3339Nano), itemID, subscriptionID)
	}
	cur := ivs[containing]

	nextPlan := cur.planID
	if newPlanID != "" {
		nextPlan = newPlanID
	}
	nextQty := cur.qty
	if newQty != nil {
		nextQty = *newQty
	}
	if nextPlan == cur.planID && nextQty == cur.qty {
		// No billable change (mirrors the fact-log trigger's IS DISTINCT
		// FROM guards) — don't mint zero-delta interval churn.
		return nil
	}
	if cur.endsAt != nil && newQty != nil && nextQty != cur.qty {
		// No writer produces a retroactive quantity change (quantity always
		// moves at clock-now, which is at or after the open interval's
		// start). Reaching this is a new writer this splice wasn't designed
		// for — refuse rather than guess how quantities compose backward.
		return fmt.Errorf("interval transition (%s): retroactive quantity change at %s on item %s is not supported", source, at.UTC().Format(time.RFC3339Nano), itemID)
	}

	// Seal the containing interval at `at` (shrink BEFORE insert so the
	// no-overlap exclusion never sees both ranges wide).
	if _, err := tx.ExecContext(ctx,
		`UPDATE billing_intervals SET ends_at = $1 WHERE id = $2`,
		at, cur.id,
	); err != nil {
		return fmt.Errorf("interval transition (%s) close %s: %w", source, cur.id, err)
	}

	// Successor carries the remainder of the containing range: open-ended
	// when we split the open interval, [at, old end) when splicing a
	// sealed one.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO billing_intervals (tenant_id, subscription_id, subscription_item_id, plan_id, quantity, starts_at, ends_at, source)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		ic.tenantID, ic.subID, itemID, nextPlan, nextQty, at, cur.endsAt, source,
	); err != nil {
		return fmt.Errorf("interval transition (%s) open item %s: %w", source, itemID, err)
	}

	// Retroactive plan splice: the plan decided at `at` holds until the
	// next PLAN change, so every subsequent interval (they exist only for
	// quantity changes recorded after `at`) re-plans in place.
	if cur.endsAt != nil && newPlanID != "" {
		if _, err := tx.ExecContext(ctx, `
			UPDATE billing_intervals SET plan_id = $1
			WHERE subscription_item_id = $2 AND starts_at > $3`,
			newPlanID, itemID, at,
		); err != nil {
			return fmt.Errorf("interval transition (%s) re-plan successors of item %s: %w", source, itemID, err)
		}
	}
	return nil
}

// intervalCloseForRemove seals the open interval at the soft-delete
// instant. Items on never-activated subs have no interval — tolerated;
// a live-sub item missing its open interval is a writer bug.
func intervalCloseForRemove(ctx context.Context, tx *sql.Tx, itemID, subscriptionID string, at time.Time) error {
	ic, err := intervalSubContext(ctx, tx, subscriptionID)
	if err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE billing_intervals SET ends_at = $1 WHERE subscription_item_id = $2 AND ends_at IS NULL`,
		at, itemID,
	)
	if err != nil {
		return fmt.Errorf("interval close (remove) item %s: %w", itemID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 && ic.status == domain.SubscriptionActive {
		return fmt.Errorf("interval close (remove): active subscription %s item %s has no open billing interval", subscriptionID, itemID)
	}
	return nil
}

// intervalCloseAllForCancel seals every open interval under the sub at
// LEAST(canceledAt, current period end): an immediate cancel closes at
// the cancel instant; a boundary/backdated fire (cancel_at_period_end,
// or an ADR-097 contracted instant months past) closes no later than the
// period the final invoice covers. GREATEST(starts_at, …) is the
// contracted-retroactive floor: a cancel instant predating an interval's
// open (the remediation-cohort shape) zero-widths that interval rather
// than minting a negative range — the earlier, already-billed intervals
// keep their history. Draft/trialing cancels sweep zero rows (nothing
// ever opened), which is correct, not an error.
func intervalCloseAllForCancel(ctx context.Context, tx *sql.Tx, sub domain.Subscription, canceledAt time.Time) error {
	closeAt := canceledAt
	if sub.CurrentBillingPeriodEnd != nil && sub.CurrentBillingPeriodEnd.Before(closeAt) {
		closeAt = *sub.CurrentBillingPeriodEnd
	}
	_, err := tx.ExecContext(ctx, `
		UPDATE billing_intervals SET ends_at = GREATEST(starts_at, $1::timestamptz)
		WHERE subscription_id = $2 AND ends_at IS NULL`,
		closeAt, sub.ID,
	)
	if err != nil {
		return fmt.Errorf("interval close-all (cancel) sub %s: %w", sub.ID, err)
	}
	return nil
}
