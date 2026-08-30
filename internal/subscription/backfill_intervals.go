package subscription

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

// BackfillBillingIntervals reconstructs billing_intervals for every
// pre-ADR-101 subscription item (one-shot, boot-time, idempotent: items
// that already have interval rows are skipped, so re-boots and the
// Phase-1 deploy itself converge on the same table).
//
// State-grounded replay: the fact log alone cannot rebuild lifetimes —
// pre-0029 items have no 'add' row and 0102→0129 removals wrote no
// 'remove' row — so the item row (created_at, deleted_at, current
// plan/quantity) anchors what the log leaves open. The day-grade 'add'
// clamp is applied ONLY to an open that lands inside the sub's CURRENT
// period (against the stored current_billing_period_start — never
// re-floored, preserving mid-day TZ-seam/trial-stub starts): past
// periods were already billed off the fact log, and the interval reader
// only ever bills current/future periods, so historical opens keep
// their raw instants.
//
// Runs per (tenant, livemode) partition under TxTenant so RLS scoping
// and the set_livemode trigger stamp rows exactly as live writers do.
func (s *PostgresStore) BackfillBillingIntervals(ctx context.Context) (int, error) {
	parts, err := s.intervalBackfillPartitions(ctx)
	if err != nil {
		return 0, err
	}
	total := 0
	for _, p := range parts {
		n, err := s.backfillIntervalsPartition(postgres.WithLivemode(ctx, p.livemode), p.tenantID)
		if err != nil {
			if postgres.IsExclusionViolation(err) {
				// Every replica runs this at boot with no cross-replica
				// serialization. Two booting together read "no rows" for the
				// same items; the second committer trips the EXCLUDE
				// constraint AFTER the first committed, so the partition is
				// already reconstructed. Skip it — a boot failure here cost
				// one replica a restart for nothing (sweep 2026-08-30 S6).
				slog.Info("billing-intervals backfill: partition reconstructed by a sibling replica first — skipping", "tenant_id", p.tenantID, "livemode", p.livemode)
				continue
			}
			return total, fmt.Errorf("backfill billing intervals tenant %s livemode %v: %w", p.tenantID, p.livemode, err)
		}
		total += n
	}
	return total, nil
}

// backfillBeforeInsert is a test-only barrier called after a partition's
// items have been read and before its segments are inserted — the window two
// booting replicas race through. nil in production.
var backfillBeforeInsert func()

type backfillPartition struct {
	tenantID string
	livemode bool
}

func (s *PostgresStore) intervalBackfillPartitions(ctx context.Context) ([]backfillPartition, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT tenant_id, livemode FROM subscriptions`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var parts []backfillPartition
	for rows.Next() {
		var p backfillPartition
		if err := rows.Scan(&p.tenantID, &p.livemode); err != nil {
			return nil, err
		}
		parts = append(parts, p)
	}
	return parts, rows.Err()
}

// backfillItem is one item awaiting reconstruction, joined with the
// subscription facts that decide its clamp and terminal close.
type backfillItem struct {
	id, subID, planID string
	quantity          int64
	createdAt         time.Time
	deletedAt         *time.Time
	subStatus         string
	periodStart       *time.Time
	periodEnd         *time.Time
	canceledAt        *time.Time
}

func (s *PostgresStore) backfillIntervalsPartition(ctx context.Context, tenantID string) (int, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return 0, err
	}
	defer postgres.Rollback(tx)

	var tz string
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(NULLIF(timezone, ''), 'UTC') FROM tenant_settings WHERE tenant_id = $1`, tenantID,
	).Scan(&tz); err != nil && err != sql.ErrNoRows {
		return 0, err
	}
	loc, err := time.LoadLocation(tz)
	if err != nil || tz == "" {
		loc = time.UTC
	}

	// Draft/trialing subs are excluded by design, not just for economy:
	// they have no billable lifetime yet, and their activation writer
	// opens intervals with the values holding at the flip.
	rows, err := tx.QueryContext(ctx, `
		SELECT it.id, it.subscription_id, it.plan_id, it.quantity, it.created_at, it.deleted_at,
		       s.status, s.current_billing_period_start, s.current_billing_period_end, s.canceled_at
		FROM subscription_items it
		JOIN subscriptions s ON s.id = it.subscription_id
		WHERE s.status NOT IN ('draft', 'trialing')
		  AND NOT EXISTS (SELECT 1 FROM billing_intervals bi WHERE bi.subscription_item_id = it.id)
		ORDER BY it.subscription_id, it.created_at, it.id`,
	)
	if err != nil {
		return 0, err
	}
	defer func() { _ = rows.Close() }()

	var items []backfillItem
	for rows.Next() {
		var it backfillItem
		if err := rows.Scan(&it.id, &it.subID, &it.planID, &it.quantity, &it.createdAt, &it.deletedAt,
			&it.subStatus, &it.periodStart, &it.periodEnd, &it.canceledAt); err != nil {
			return 0, err
		}
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	inserted := 0
	for _, it := range items {
		n, err := backfillOneItem(ctx, tx, tenantID, loc, it)
		if err != nil {
			return 0, fmt.Errorf("item %s (sub %s): %w", it.id, it.subID, err)
		}
		inserted += n
	}
	if inserted == 0 {
		return 0, nil
	}
	return inserted, tx.Commit()
}

// ivSegment is one reconstructed [startsAt, endsAt) lifetime slice.
type ivSegment struct {
	planID   string
	quantity int64
	startsAt time.Time
	endsAt   *time.Time // nil = open
}

func backfillOneItem(ctx context.Context, tx *sql.Tx, tenantID string, loc *time.Location, it backfillItem) (int, error) {
	// Tie-break same-instant rows by wall-clock created_at, NOT id: under a
	// frozen test clock an add and a plan change can share one sim instant
	// (changed_at), ids are random hex, and only the insert stamp carries
	// the true sequence — found on real dev data, where id-ordering put a
	// 'plan' row before its own item's 'add'.
	logRows, err := tx.QueryContext(ctx, `
		SELECT change_type, COALESCE(from_plan_id,''), COALESCE(to_plan_id,''), from_quantity, to_quantity, changed_at
		FROM subscription_item_changes
		WHERE subscription_item_id = $1
		ORDER BY changed_at, created_at, id`,
		it.id,
	)
	if err != nil {
		return 0, err
	}
	defer func() { _ = logRows.Close() }()

	type change struct {
		typ, fromPlan, toPlan string
		fromQty, toQty        sql.NullInt64
		at                    time.Time
	}
	var changes []change
	for logRows.Next() {
		var c change
		if err := logRows.Scan(&c.typ, &c.fromPlan, &c.toPlan, &c.fromQty, &c.toQty, &c.at); err != nil {
			return 0, err
		}
		changes = append(changes, c)
	}
	if err := logRows.Err(); err != nil {
		return 0, err
	}

	// Forward replay. `open` tracks whether a segment is in progress;
	// synthetic opens at item.created_at cover the pre-0029 no-'add' gap
	// (the first logged transition's from_* names the state that held
	// since creation).
	var segs []ivSegment
	open := false
	var curPlan string
	var curQty int64
	var curStart time.Time

	syntheticOpen := func(fromPlan string, fromQty sql.NullInt64) {
		open = true
		curStart = it.createdAt
		curPlan = fromPlan
		curQty = it.quantity
		if fromQty.Valid {
			curQty = fromQty.Int64
		}
	}
	closeSeg := func(at time.Time) {
		end := at
		if end.Before(curStart) {
			// Log-derived boundaries can't regress (sorted), so this floor
			// only fires on the terminal cancel close — the same
			// GREATEST(starts_at, …) zero-width the live cancel writer
			// applies when a contracted cancel instant predates the open.
			end = curStart
		}
		segs = append(segs, ivSegment{planID: curPlan, quantity: curQty, startsAt: curStart, endsAt: &end})
		open = false
	}

	for _, c := range changes {
		switch c.typ {
		case "add":
			if open {
				return 0, fmt.Errorf("fact log has 'add' while a segment is open (double add at %s)", c.at)
			}
			open = true
			curStart = c.at
			curPlan = c.toPlan
			curQty = 1
			if c.toQty.Valid {
				curQty = c.toQty.Int64
			}
		case "plan", "quantity":
			if !open {
				syntheticOpen(c.fromPlan, c.fromQty)
			}
			closeSeg(c.at)
			open = true
			curStart = c.at
			if c.toPlan != "" {
				curPlan = c.toPlan
			}
			if c.toQty.Valid {
				curQty = c.toQty.Int64
			}
		case "remove":
			if !open {
				syntheticOpen(c.fromPlan, c.fromQty)
			}
			closeSeg(c.at)
		default:
			return 0, fmt.Errorf("unknown change_type %q in fact log", c.typ)
		}
	}

	if len(changes) == 0 {
		// Pre-0029 item that was never mutated: its whole lifetime is the
		// current row state from creation.
		open = true
		curStart = it.createdAt
		curPlan = it.planID
		curQty = it.quantity
	}

	if open && it.deletedAt != nil {
		// 0102→0129 removal gap: the soft delete wrote no 'remove' row.
		closeSeg(*it.deletedAt)
	}
	if !open && it.deletedAt == nil {
		return 0, fmt.Errorf("fact log ends removed but the item row is live (un-delete era row?) — refusing to guess")
	}

	if open {
		switch domain.SubscriptionStatus(it.subStatus) {
		case domain.SubscriptionActive:
			segs = append(segs, ivSegment{planID: curPlan, quantity: curQty, startsAt: curStart, endsAt: nil})
		default:
			// Canceled (or any other terminal shape): seal at the same
			// instant the cancel writer would have — LEAST(canceled_at,
			// period end), floored at the segment's own start.
			var closeAt time.Time
			switch {
			case it.canceledAt != nil && it.periodEnd != nil:
				closeAt = *it.canceledAt
				if it.periodEnd.Before(closeAt) {
					closeAt = *it.periodEnd
				}
			case it.canceledAt != nil:
				closeAt = *it.canceledAt
			case it.periodEnd != nil:
				closeAt = *it.periodEnd
			default:
				return 0, fmt.Errorf("%s subscription has neither canceled_at nor a period end to close intervals at", it.subStatus)
			}
			closeSeg(closeAt)
		}
	}

	if len(segs) == 0 {
		return 0, nil
	}

	// Day-grade 'add' clamp, current period only: an open that lands on
	// the period-start calendar day (tenant TZ, as of backfill) bills
	// from the stored period start itself — the same policy the AddItem
	// writer applies going forward.
	if domain.SubscriptionStatus(it.subStatus) == domain.SubscriptionActive && it.periodStart != nil {
		ps := *it.periodStart
		first := &segs[0]
		if first.startsAt.After(ps) && domain.SameCalendarDayIn(first.startsAt, ps, loc) {
			first.startsAt = ps
		}
	}

	if backfillBeforeInsert != nil {
		backfillBeforeInsert()
	}
	for _, seg := range segs {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO billing_intervals (tenant_id, subscription_id, subscription_item_id, plan_id, quantity, starts_at, ends_at, source)
			VALUES ($1,$2,$3,$4,$5,$6,$7,'backfill')`,
			tenantID, it.subID, it.id, seg.planID, seg.quantity, seg.startsAt, seg.endsAt,
		); err != nil {
			return 0, fmt.Errorf("insert segment [%s, %v): %w", seg.startsAt, seg.endsAt, err)
		}
	}
	return len(segs), nil
}
