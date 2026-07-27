package analytics

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// P9 (audit High #13): the 0029 MRR-audit trigger predates 0102's soft
// delete — removing an item became `UPDATE ... SET deleted_at`, which
// emitted NO 'remove' row, so MRR contraction silently vanished from
// the change log (the analytics source of truth), ongoing and
// unbackfillable once the timing context is gone. Migration 0129
// replaces the trigger function.
//
// Mutation-verify: re-apply the 0029 function body (the down migration)
// — the soft-delete assertions fail.
func TestItemChangeTrigger_SoftDeleteEmitsRemove(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "SIC Trigger")

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_sic", DisplayName: "SIC",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	plan, err := pricing.NewPostgresStore(db).CreatePlan(ctx, tenantID, domain.Plan{
		Code: "pln-sic", Name: "SIC Plan", Currency: "USD",
		BillingInterval: domain.BillingMonthly, Status: domain.PlanActive,
		BaseAmountCents: 1000, MeterIDs: []string{},
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	subID := postgres.NewID("vlx_sub")
	itemID := postgres.NewID("vlx_si")
	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO subscriptions (id, tenant_id, code, display_name, customer_id, status, billing_time,
			current_billing_period_start, current_billing_period_end, next_billing_at, created_at, updated_at)
		VALUES ($1, $2, 'code-sic', 'SIC Sub', $3, 'active', 'anniversary', $4, $5, $5, $4, $4)
	`, subID, tenantID, cust.ID, now.Add(-24*time.Hour), now.Add(29*24*time.Hour)); err != nil {
		t.Fatalf("insert sub: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO subscription_items (id, tenant_id, subscription_id, plan_id, quantity, metadata, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 3, '{}'::jsonb, $5, $5)
	`, itemID, tenantID, subID, plan.ID, now.Add(-24*time.Hour)); err != nil {
		t.Fatalf("insert item: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit seed: %v", err)
	}

	changeRows := func() []struct {
		changeType string
		fromPlan   string
		fromQty    int64
		changedAt  time.Time
	} {
		t.Helper()
		btx, err := db.BeginTx(ctx, postgres.TxBypass, "")
		if err != nil {
			t.Fatalf("begin bypass: %v", err)
		}
		defer postgres.Rollback(btx)
		rows, err := btx.Query(`
			SELECT change_type, COALESCE(from_plan_id,''), COALESCE(from_quantity,0), changed_at
			FROM subscription_item_changes WHERE subscription_item_id = $1 ORDER BY created_at
		`, itemID)
		if err != nil {
			t.Fatalf("query changes: %v", err)
		}
		defer func() { _ = rows.Close() }()
		var out []struct {
			changeType string
			fromPlan   string
			fromQty    int64
			changedAt  time.Time
		}
		for rows.Next() {
			var r struct {
				changeType string
				fromPlan   string
				fromQty    int64
				changedAt  time.Time
			}
			if err := rows.Scan(&r.changeType, &r.fromPlan, &r.fromQty, &r.changedAt); err != nil {
				t.Fatalf("scan: %v", err)
			}
			out = append(out, r)
		}
		return out
	}

	// INSERT emitted 'add' (0029 behavior, unchanged).
	if rows := changeRows(); len(rows) != 1 || rows[0].changeType != "add" {
		t.Fatalf("after insert: %+v, want one 'add'", rows)
	}

	// Soft delete = the remove event. Pre-0129 this UPDATE emitted NOTHING.
	deletedAt := now.Add(-time.Hour)
	tx2, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx2)
	if _, err := tx2.ExecContext(ctx, `UPDATE subscription_items SET deleted_at = $1, updated_at = $1 WHERE id = $2`, deletedAt, itemID); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	if err := tx2.Commit(); err != nil {
		t.Fatalf("commit soft delete: %v", err)
	}

	rows := changeRows()
	if len(rows) != 2 {
		t.Fatalf("after soft delete: %d rows (%+v), want add + remove", len(rows), rows)
	}
	rm := rows[1]
	if rm.changeType != "remove" {
		t.Fatalf("second row type: %q, want remove", rm.changeType)
	}
	if rm.fromPlan != plan.ID || rm.fromQty != 3 {
		t.Errorf("remove row provenance: plan=%q qty=%d, want %q/3", rm.fromPlan, rm.fromQty, plan.ID)
	}
	if !rm.changedAt.Equal(deletedAt) {
		t.Errorf("remove changed_at: %v, want the deleted_at stamp %v (honors sim-time)", rm.changedAt, deletedAt)
	}

	// Bookkeeping on the dead row emits nothing.
	tx3, _ := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	defer postgres.Rollback(tx3)
	if _, err := tx3.ExecContext(ctx, `UPDATE subscription_items SET quantity = 9 WHERE id = $1`, itemID); err != nil {
		t.Fatalf("update dead row: %v", err)
	}
	_ = tx3.Commit()
	if rows := changeRows(); len(rows) != 2 {
		t.Fatalf("after dead-row update: %d rows, want still 2 (dead rows don't move MRR)", len(rows))
	}

	// Un-delete is REFUSED outright (ADR-101, migration 0159): no flow
	// maintains billing_intervals for a resurrected item, so a manual
	// resurrection would silently un-bill it once the interval reader cuts
	// over. The 0129 behavior (emit 'add' so MRR reappearance is loud) is
	// superseded by refusing the write entirely — louder still.
	tx4, _ := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	defer postgres.Rollback(tx4)
	_, err = tx4.ExecContext(ctx, `UPDATE subscription_items SET deleted_at = NULL, updated_at = $1 WHERE id = $2`, now, itemID)
	if err == nil || !strings.Contains(err.Error(), "ADR-101") {
		t.Fatalf("un-delete must be refused by the trigger (ADR-101), got: %v", err)
	}
	if rows := changeRows(); len(rows) != 2 {
		t.Fatalf("after refused un-delete: %d rows, want still 2", len(rows))
	}
}

// TestP9Indexes: the tenant-wide time-window analytics query must not
// seq-scan usage_events (0130), and the per-item change index exists
// (0131 — the table is too small in tests for a planner assertion).
func TestP9Indexes(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "P9 Indexes")

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_idx", DisplayName: "Idx",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	rrv, err := pricing.NewPostgresStore(db).CreateRatingRule(ctx, tenantID, domain.RatingRuleVersion{
		RuleKey: "idx_calls", Name: "Idx", LifecycleState: domain.RatingRuleActive,
		Mode: domain.PricingFlat, Currency: "USD", FlatAmountCents: decimal.NewFromInt(1),
	})
	if err != nil {
		t.Fatalf("create rrv: %v", err)
	}
	meter, err := pricing.NewPostgresStore(db).CreateMeter(ctx, tenantID, domain.Meter{
		Key: "idx_calls", Name: "Idx", Unit: "calls", Aggregation: "sum", RatingRuleVersionID: rrv.ID,
	})
	if err != nil {
		t.Fatalf("create meter: %v", err)
	}

	// Enough rows + ANALYZE that the planner prefers the index for a
	// narrow tenant+time window.
	btx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin bypass: %v", err)
	}
	if _, err := btx.Exec(`SET LOCAL app.livemode = 'off'`); err != nil {
		t.Fatalf("set livemode: %v", err)
	}
	base := time.Now().UTC().Add(-90 * 24 * time.Hour)
	var sb strings.Builder
	sb.WriteString(`INSERT INTO usage_events (id, tenant_id, customer_id, meter_id, quantity, timestamp) VALUES `)
	for i := 0; i < 2000; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, "('%s', '%s', '%s', '%s', 1, '%s')",
			postgres.NewID("vlx_evt"), tenantID, cust.ID, meter.ID,
			base.Add(time.Duration(i)*time.Hour).Format(time.RFC3339))
	}
	if _, err := btx.Exec(sb.String()); err != nil {
		t.Fatalf("seed events: %v", err)
	}
	if err := btx.Commit(); err != nil {
		t.Fatalf("commit seed: %v", err)
	}

	verify, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin verify: %v", err)
	}
	defer postgres.Rollback(verify)
	if _, err := verify.Exec(`ANALYZE usage_events`); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	rows, err := verify.Query(`
		EXPLAIN SELECT COUNT(*) FROM usage_events
		WHERE tenant_id = $1 AND timestamp >= $2 AND timestamp < $3
	`, tenantID, base.Add(24*time.Hour), base.Add(48*time.Hour))
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		plan.WriteString(line + "\n")
	}
	// Planner-portable assertion (never a specific index name): the
	// tenant+time filter must not be forced through a full scan.
	if strings.Contains(plan.String(), "Seq Scan on usage_events") {
		t.Errorf("tenant+time window seq-scans usage_events:\n%s", plan.String())
	}

	var exists bool
	if err := verify.QueryRow(`
		SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE tablename = 'subscription_item_changes' AND indexdef LIKE '%(subscription_item_id, changed_at)%')
	`).Scan(&exists); err != nil {
		t.Fatalf("check sic index: %v", err)
	}
	if !exists {
		t.Error("per-item index on subscription_item_changes (subscription_item_id, changed_at) missing (0131)")
	}
}
