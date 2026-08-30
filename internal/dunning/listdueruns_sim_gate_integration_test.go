package dunning_test

import (
	"context"
	"github.com/sagarsuperuser/velox/internal/platform/leader"
	"github.com/sagarsuperuser/velox/internal/platform/leader/leadertest"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/dunning"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// ListDueRuns (the wall-clock dunning-retry sweep) must exclude a run whose
// invoice is SIMULATED, keyed on the invoice's OWN durable is_simulated flag —
// the subscriptions-join it replaced missed customer-pinned one-offs, letting a
// simulated invoice be dunned against wall-clock time (ADR-029). Simulated
// dunning is driven by the catchup counterpart ListDueRunsForClock instead.
func TestListDueRuns_ExcludesSimulatedInvoiceRun(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := leadertest.Token(t, testutil.AdminPool(t), postgres.WithLivemode(context.Background(), false), leader.RoleDunning)
	tenantID := testutil.CreateTestTenant(t, db, "DueRuns Sim Gate")

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{ExternalID: "cus_due", DisplayName: "Due"})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	dstore := dunning.NewPostgresStore(db)
	policy, err := dstore.UpsertPolicy(ctx, tenantID, domain.DunningPolicy{
		Name: "default", Enabled: true, RetrySchedule: []string{"72h"}, MaxRetryAttempts: 3,
		FinalSubscriptionAction: domain.SubActionNone, FinalInvoiceAction: domain.InvActionMarkUncollectible, GracePeriodDays: 3,
	})
	if err != nil {
		t.Fatalf("upsert policy: %v", err)
	}
	istore := invoice.NewPostgresStore(db)
	now := time.Now().UTC()

	// mkDueRun creates a finalized-failed invoice + an active dunning run, forces
	// the run due (next_action_at in the past), and for the simulated case stamps
	// the invoice's durable is_simulated flag directly.
	mkDueRun := func(num string, simulated bool) string {
		inv, err := istore.Create(ctx, tenantID, domain.Invoice{
			CustomerID: cust.ID, InvoiceNumber: num, Status: domain.InvoiceFinalized,
			PaymentStatus: domain.PaymentFailed, Currency: "USD", SubtotalCents: 5000,
			TotalAmountCents: 5000, AmountDueCents: 5000,
			BillingPeriodStart: now.Add(-time.Hour), BillingPeriodEnd: now, IssuedAt: &now,
		})
		if err != nil {
			t.Fatalf("create invoice %s: %v", num, err)
		}
		run, err := dstore.CreateRun(ctx, tenantID, domain.InvoiceDunningRun{
			InvoiceID: inv.ID, CustomerID: cust.ID, PolicyID: policy.ID,
			State: domain.DunningActive, Reason: "payment_failed",
		})
		if err != nil {
			t.Fatalf("create run %s: %v", num, err)
		}
		tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE invoice_dunning_runs SET next_action_at = now() - interval '1 hour' WHERE id = $1`, run.ID); err != nil {
			t.Fatalf("set next_action_at: %v", err)
		}
		if simulated {
			if _, err := tx.ExecContext(ctx, `UPDATE invoices SET is_simulated = true WHERE id = $1`, inv.ID); err != nil {
				t.Fatalf("set is_simulated: %v", err)
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
		return run.ID
	}

	simRun := mkDueRun("INV-DUE-SIM", true)
	realRun := mkDueRun("INV-DUE-REAL", false)

	due, err := dstore.ListDueRuns(ctx, tenantID, now.Add(time.Minute), 200)
	if err != nil {
		t.Fatalf("ListDueRuns: %v", err)
	}
	got := map[string]bool{}
	for _, r := range due {
		got[r.ID] = true
	}
	if got[simRun] {
		t.Error("a run whose invoice is simulated must be excluded from the wall-clock dunning-retry sweep (ADR-029)")
	}
	if !got[realRun] {
		t.Error("a run whose invoice is a wall-clock failed invoice must remain due")
	}
}
