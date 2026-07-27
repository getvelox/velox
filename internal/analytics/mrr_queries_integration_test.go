package analytics

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// P9 spill (audit HIGH #13 + MRR mediums): removed items must stop
// counting at t, every MRR sum is currency-scoped, and item add/remove
// on continuing subs is expansion/contraction — so Net finally
// reconciles with the headline MRR delta.
//
// Seeded world (default currency USD, period=30d, t = window start):
//
//	S1 continuing (activated 60d ago):
//	  item0 USD $10 — created 60d ago, REMOVED 40d ago (before t)
//	  item1 USD $10 — created 60d ago, REMOVED 5d ago (inside window)
//	  item2 USD $20 — ADDED 10d ago (inside window)
//	  item5 USD $10 — created 60d ago, REMOVED 40d ago, RESURRECTED
//	    (un-deleted) 20d ago, quantity 1→2 10d ago: at t it was removed
//	    and must NOT count even though a post-t event carries from_*
//	S2 continuing EUR (activated 60d ago): €99 item + one added in-window
//	S3 activated 15d ago: USD $10 item            → New
//	S4 activated 60d ago, canceled 8d ago: USD $20 → Churned
//
// Expectations:
//
//	MRR now      = item2 + S3          = 3000
//	MRR at t     = item1 + S4          = 3000  (item0 excluded: removed
//	                                            before t — the bug made
//	                                            it count forever)
//	New=1000 Churned=2000 Expansion=2000(add) Contraction=1000(remove)
//	Net = 0 = MRR − MRRPrev  ← the reconciliation the audit flagged
//
// Mutation-verify:
//   - drop the state_type<>'remove' exclusion → MRRPrev gains item0 → fails
//   - revert expansion joins to INNER/plan-quantity-only → Expansion=
//     Contraction=0 → fails
//   - drop any p.currency scope → EUR amounts leak in → fails
func TestMRRQueries_RemoveAware_CurrencyScoped_AddRemoveMovement(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "P9 MRR Queries")

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_mrr", DisplayName: "MRR",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}

	pstore := pricing.NewPostgresStore(db)
	mkPlan := func(code, currency string, cents int64) string {
		t.Helper()
		p, err := pstore.CreatePlan(ctx, tenantID, domain.Plan{
			Code: code, Name: code, Currency: currency,
			BillingInterval: domain.BillingMonthly, Status: domain.PlanActive,
			BaseAmountCents: cents, MeterIDs: []string{},
		})
		if err != nil {
			t.Fatalf("create plan %s: %v", code, err)
		}
		return p.ID
	}
	// Distinct plan per item — subscription_items enforces one item per
	// (subscription, plan).
	usd10a := mkPlan("pln-usd10a", "USD", 1000)
	usd10b := mkPlan("pln-usd10b", "USD", 1000)
	usd10c := mkPlan("pln-usd10c", "USD", 1000)
	usd20a := mkPlan("pln-usd20a", "USD", 2000)
	usd20b := mkPlan("pln-usd20b", "USD", 2000)
	usd10d := mkPlan("pln-usd10d", "USD", 1000)
	eur99 := mkPlan("pln-eur99", "EUR", 9900)
	eur49 := mkPlan("pln-eur49", "EUR", 4900)

	now := time.Now().UTC().Truncate(time.Microsecond)
	d := func(days int) time.Time { return now.Add(-time.Duration(days) * 24 * time.Hour) }

	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)

	mkSub := func(code string, activatedAt time.Time, canceledAt *time.Time) string {
		t.Helper()
		id := postgres.NewID("vlx_sub")
		status := "active"
		if canceledAt != nil {
			status = "canceled"
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO subscriptions (id, tenant_id, code, display_name, customer_id, status, billing_time,
				activated_at, canceled_at,
				current_billing_period_start, current_billing_period_end, next_billing_at, created_at, updated_at)
			VALUES ($1, $2, $3, $3, $4, $5, 'anniversary', $6, $7, $6, $8, $8, $6, $6)
		`, id, tenantID, code, cust.ID, status, activatedAt, canceledAt, now.Add(24*time.Hour)); err != nil {
			t.Fatalf("insert sub %s: %v", code, err)
		}
		return id
	}
	mkItem := func(subID, planID string, qty int64, createdAt time.Time) string {
		t.Helper()
		id := postgres.NewID("vlx_si")
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO subscription_items (id, tenant_id, subscription_id, plan_id, quantity, metadata, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, '{}'::jsonb, $6, $6)
		`, id, tenantID, subID, planID, qty, createdAt); err != nil {
			t.Fatalf("insert item: %v", err)
		}
		return id
	}
	softDelete := func(itemID string, at time.Time) {
		t.Helper()
		// The 0129 trigger stamps the 'remove' change row from deleted_at.
		if _, err := tx.ExecContext(ctx, `
			UPDATE subscription_items SET deleted_at = $2, updated_at = $2 WHERE id = $1
		`, itemID, at); err != nil {
			t.Fatalf("soft delete: %v", err)
		}
	}

	s1 := mkSub("s1-continuing", d(60), nil)
	item0 := mkItem(s1, usd10a, 1, d(60))
	softDelete(item0, d(40)) // removed BEFORE t=d(30): must not count at t
	item1 := mkItem(s1, usd10b, 1, d(60))
	softDelete(item1, d(5))          // removed INSIDE window: counts at t, contraction in window
	_ = mkItem(s1, usd20a, 1, d(10)) // added INSIDE window: expansion
	// Resurrection-shaped log: removed before t, re-added + resized after
	// t. Un-deleting subscription_items is REFUSED since ADR-101 (0159 —
	// no flow maintains billing_intervals for resurrection), so the 0129
	// trigger can no longer mint this shape; the MRR queries must still
	// fold any legacy add-after-remove rows correctly, so the fixture
	// writes the fact rows directly with the item row left in the same
	// end state the old un-delete produced (live, qty 2).
	item5 := mkItem(s1, usd10d, 1, d(60))
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO subscription_item_changes
			(tenant_id, livemode, subscription_id, subscription_item_id,
			 change_type, from_plan_id, from_quantity, changed_at)
		VALUES ($1, false, $2, $3, 'remove', $4, 1, $5)
	`, tenantID, s1, item5, usd10d, d(40)); err != nil {
		t.Fatalf("seed remove row: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO subscription_item_changes
			(tenant_id, livemode, subscription_id, subscription_item_id,
			 change_type, to_plan_id, to_quantity, changed_at)
		VALUES ($1, false, $2, $3, 'add', $4, 1, $5)
	`, tenantID, s1, item5, usd10d, d(20)); err != nil {
		t.Fatalf("seed re-add row: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE subscription_items SET quantity = 2, updated_at = $2 WHERE id = $1
	`, item5, d(10)); err != nil {
		t.Fatalf("resize: %v", err)
	}

	s2 := mkSub("s2-eur", d(60), nil)
	_ = mkItem(s2, eur99, 1, d(60))
	_ = mkItem(s2, eur49, 1, d(10)) // EUR in-window add: excluded everywhere

	s3 := mkSub("s3-new", d(15), nil)
	_ = mkItem(s3, usd10c, 1, d(15))

	canceledAt := d(8)
	s4 := mkSub("s4-churned", d(60), &canceledAt)
	_ = mkItem(s4, usd20b, 1, d(60))

	if err := tx.Commit(); err != nil {
		t.Fatalf("commit seed: %v", err)
	}

	h := NewHandler(db)
	req := httptest.NewRequest("GET", "/overview?period=30d", nil)
	req = req.WithContext(auth.WithTenantID(postgres.WithLivemode(req.Context(), false), tenantID))
	rr := httptest.NewRecorder()
	h.overview(rr, req)
	if rr.Code != 200 {
		t.Fatalf("overview status: %d body=%s", rr.Code, rr.Body.String())
	}
	var resp OverviewResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if resp.MRR != 5000 {
		t.Errorf("MRR now: got %d, want 5000 (item2 2000 + S3 1000 + item5 2000; EUR + deleted + canceled excluded)", resp.MRR)
	}
	if resp.MRRPrev != 3000 {
		t.Errorf("MRR at t: got %d, want 3000 (item1 1000 + S4 2000; item0 and resurrected item5 were both REMOVED at t and must not count, EUR excluded)", resp.MRRPrev)
	}
	mvt := resp.MRRMovement
	if mvt.New != 1000 || mvt.Churned != 2000 || mvt.Expansion != 4000 || mvt.Contraction != 1000 {
		t.Errorf("movement: new=%d churned=%d expansion=%d contraction=%d, want 1000/2000/4000/1000 (add/remove/resurrection on continuing subs = expansion/contraction, EUR excluded)",
			mvt.New, mvt.Churned, mvt.Expansion, mvt.Contraction)
	}
	if mvt.Net != resp.MRR-resp.MRRPrev {
		t.Errorf("Net %d does not reconcile with MRR delta %d — the exact drift the audit flagged", mvt.Net, resp.MRR-resp.MRRPrev)
	}

	// The dedicated movement endpoint agrees with the overview totals.
	req = httptest.NewRequest("GET", "/mrr-movement?period=30d", nil)
	req = req.WithContext(auth.WithTenantID(postgres.WithLivemode(req.Context(), false), tenantID))
	rr = httptest.NewRecorder()
	h.mrrMovement(rr, req)
	if rr.Code != 200 {
		t.Fatalf("mrr-movement status: %d body=%s", rr.Code, rr.Body.String())
	}
	var mv struct {
		Totals MRRMovementTotals `json:"totals"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &mv); err != nil {
		t.Fatalf("decode movement: %v", err)
	}
	if mv.Totals != mvt {
		t.Errorf("movement endpoint totals %+v disagree with overview %+v", mv.Totals, mvt)
	}
}
