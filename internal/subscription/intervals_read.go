package subscription

import (
	"context"
	"sort"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

// ListItemIntervals reads the sub's full billing_intervals history —
// the single segment source post-ADR-101-Phase-4. Deliberately
// unwindowed: the billing reader clips in Go, and its missing-interval
// invariant must distinguish "no rows overlap this window" (legitimate
// zero) from "no rows AT ALL" (a writer bug).
func (s *PostgresStore) ListItemIntervals(ctx context.Context, tenantID, subscriptionID string) ([]domain.ItemInterval, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	rows, err := tx.QueryContext(ctx, `
		SELECT subscription_item_id, plan_id, quantity, starts_at, ends_at
		FROM billing_intervals
		WHERE subscription_id = $1
	`, subscriptionID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var intervals []domain.ItemInterval
	for rows.Next() {
		var iv domain.ItemInterval
		if err := rows.Scan(&iv.SubscriptionItemID, &iv.PlanID, &iv.Quantity, &iv.StartsAt, &iv.EndsAt); err != nil {
			return nil, err
		}
		intervals = append(intervals, iv)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Same order the reader's merge step expects: starts_at, with
	// zero-width/closed rows before the row that reopened at the same
	// instant (open/end-later rows last).
	sort.SliceStable(intervals, func(i, j int) bool {
		if !intervals[i].StartsAt.Equal(intervals[j].StartsAt) {
			return intervals[i].StartsAt.Before(intervals[j].StartsAt)
		}
		ie, je := intervals[i].EndsAt, intervals[j].EndsAt
		if ie == nil {
			return false
		}
		if je == nil {
			return true
		}
		return ie.Before(*je)
	})
	return intervals, nil
}
