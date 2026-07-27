package subscription

import (
	"context"
	"sort"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

// SegmentShadowSnapshot reads the fact-log rows for one billing window
// AND the item's full billing_intervals history in ONE SQL statement —
// one statement means one MVCC snapshot even under read committed, so
// the ADR-101 Phase 2 comparator never sees a concurrent item mutation
// land between its two reads (the "two-tx shadow reads produce false
// divergence" hazard the ADR names).
//
// Changes are windowed (periodStart, periodEnd] exactly like
// ListItemChangesInPeriod (exclusive-left, inclusive-right, created_at
// tie-break). Intervals are deliberately UNWINDOWED: the reader clips
// in Go, and the missing-interval invariant needs to distinguish "no
// rows overlap this window" (a legitimate zero — e.g. an item whose
// lifetime starts after the window) from "no rows AT ALL" (a writer
// bug).
func (s *PostgresStore) SegmentShadowSnapshot(ctx context.Context, tenantID, subscriptionID string, periodStart, periodEnd time.Time) ([]domain.SubscriptionItemChange, []domain.ItemInterval, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, nil, err
	}
	defer postgres.Rollback(tx)

	rows, err := tx.QueryContext(ctx, `
		SELECT 'change' AS kind,
		       COALESCE(subscription_item_id, ''), change_type,
		       COALESCE(from_plan_id, ''), COALESCE(to_plan_id, ''),
		       COALESCE(from_quantity, 0), COALESCE(to_quantity, 0),
		       changed_at, NULL::timestamptz, created_at
		FROM subscription_item_changes
		WHERE subscription_id = $1 AND changed_at > $2 AND changed_at <= $3
		UNION ALL
		SELECT 'interval',
		       subscription_item_id, '',
		       plan_id, '',
		       quantity, 0,
		       starts_at, ends_at, created_at
		FROM billing_intervals
		WHERE subscription_id = $1
	`, subscriptionID, periodStart, periodEnd)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = rows.Close() }()

	var changes []domain.SubscriptionItemChange
	var intervals []domain.ItemInterval
	for rows.Next() {
		var kind, itemID, changeType, planA, planB string
		var qtyA, qtyB int64
		var at time.Time
		var endsAt *time.Time
		var createdAt time.Time
		if err := rows.Scan(&kind, &itemID, &changeType, &planA, &planB, &qtyA, &qtyB, &at, &endsAt, &createdAt); err != nil {
			return nil, nil, err
		}
		if kind == "change" {
			changes = append(changes, domain.SubscriptionItemChange{
				SubscriptionItemID: itemID, ChangeType: changeType,
				FromPlanID: planA, ToPlanID: planB,
				FromQuantity: qtyA, ToQuantity: qtyB,
				ChangedAt: at, CreatedAt: createdAt,
			})
		} else {
			intervals = append(intervals, domain.ItemInterval{
				SubscriptionItemID: itemID, PlanID: planA,
				Quantity: qtyA, StartsAt: at, EndsAt: endsAt,
			})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	// Same walk order the billing reader uses: changed_at, then the
	// wall-clock insert stamp for same-sim-instant rows.
	sort.SliceStable(changes, func(i, j int) bool {
		if !changes[i].ChangedAt.Equal(changes[j].ChangedAt) {
			return changes[i].ChangedAt.Before(changes[j].ChangedAt)
		}
		return changes[i].CreatedAt.Before(changes[j].CreatedAt)
	})
	sort.SliceStable(intervals, func(i, j int) bool {
		if !intervals[i].StartsAt.Equal(intervals[j].StartsAt) {
			return intervals[i].StartsAt.Before(intervals[j].StartsAt)
		}
		// Zero-width rows sort before the row that reopened at the same
		// instant (open/END-later rows last).
		ie, je := intervals[i].EndsAt, intervals[j].EndsAt
		if ie == nil {
			return false
		}
		if je == nil {
			return true
		}
		return ie.Before(*je)
	})
	return changes, intervals, nil
}
