package customer_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/subscription"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestFK_RestrictOnDelete verifies HYG-4: every child→parent foreign key now
// has an explicit ON DELETE RESTRICT policy. Hard-deleting a parent row while
// child rows exist must raise a Postgres 23503 foreign_key_violation so we
// never silently orphan billing data (invoices without a customer, credit
// ledger entries without a tenant, etc.).
//
// We exercise three representative paths:
//
//   - customers → subscriptions  (operational entity)
//   - plans     → subscriptions  (catalog entity)
//   - tenants   → customers      (top-level isolation boundary)
//
// If any of these paths stops returning an FK violation, either a cascade
// was added by accident or the RESTRICT policy was weakened. Both are
// regressions worth failing on.
func TestFK_RestrictOnDelete(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 15*time.Second)
	defer cancel()

	tenantID := testutil.CreateTestTenant(t, db, "FK Restrict")

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID:  "cus_fk_test",
		DisplayName: "FK Test Co",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	plan, err := pricing.NewPostgresStore(db).CreatePlan(ctx, tenantID, domain.Plan{
		Code: "plan-fk-test", Name: "FK Plan", Currency: "USD",
		BillingInterval: domain.BillingMonthly, Status: domain.PlanActive,
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	now := time.Now().UTC()
	sub, err := subscription.NewPostgresStore(db).Create(ctx, tenantID, domain.Subscription{
		Code: "sub-fk-test", DisplayName: "FK Sub",
		CustomerID: cust.ID,
		Status:     domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar,
		StartedAt:                 &now,
		CurrentBillingPeriodStart: &now, CurrentBillingPeriodEnd: bivPE(now),
		Items: []domain.SubscriptionItem{{PlanID: plan.ID, Quantity: 1}},
	})
	if err != nil {
		t.Fatalf("create subscription: %v", err)
	}

	// Hard-delete via TxBypass so RLS doesn't hide the row from us — we want
	// the FK layer to be what stops the DELETE, not tenant isolation.
	expectFKViolation := func(t *testing.T, label, sql string, args ...any) {
		t.Helper()
		tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
		if err != nil {
			t.Fatalf("%s: begin tx: %v", label, err)
		}
		defer postgres.Rollback(tx)
		_, err = tx.ExecContext(ctx, sql, args...)
		if err == nil {
			t.Fatalf("%s: expected FK violation, got nil", label)
		}
		if !postgres.IsForeignKeyViolation(err) {
			t.Fatalf("%s: expected 23503 foreign_key_violation, got: %v", label, err)
		}
		// Sanity: Postgres spells out "violates foreign key constraint" in the
		// message — makes the failure readable without decoding sqlstate.
		if !strings.Contains(err.Error(), "violates foreign key constraint") {
			t.Errorf("%s: missing expected FK message: %v", label, err)
		}
	}

	expectFKViolation(t, "delete customer with subscription",
		`DELETE FROM customers WHERE id = $1`, cust.ID)

	expectFKViolation(t, "delete plan with subscription",
		`DELETE FROM plans WHERE id = $1`, plan.ID)

	expectFKViolation(t, "delete tenant with customer",
		`DELETE FROM tenants WHERE id = $1`, tenantID)

	// Clean-up path: removing children in the right order must release the
	// parents. This proves RESTRICT is not over-broad — it blocks orphaning,
	// not legitimate deletion.
	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("cleanup begin: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM subscriptions WHERE id = $1`, sub.ID); err != nil {
		_ = tx.Rollback()
		t.Fatalf("delete subscription: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM customers WHERE id = $1`, cust.ID); err != nil {
		_ = tx.Rollback()
		t.Fatalf("delete customer after child gone: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("cleanup commit: %v", err)
	}
}
