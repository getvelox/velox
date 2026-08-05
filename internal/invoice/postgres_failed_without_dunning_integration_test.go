package invoice_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/dunning"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/subscription"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestListFailedWithoutDunningRun_CandidateSet is the real-Postgres proof of the
// dunning_backfill sweep's eligibility query. A finalized, still-owed, failed
// invoice with NO dunning run is a candidate (the SettleFailed post-commit crash /
// exhausted-retry window). Every exclusion is filtered out:
//   - an invoice that already has a run in ANY state (here RESOLVED) — the
//     state-agnostic NOT EXISTS, the load-bearing invariant: a state-filtered
//     predicate would re-dun a resolved invoice forever;
//   - a freshly-updated invoice inside the cool-off;
//   - a paid invoice, and a zero-balance one.
func TestListFailedWithoutDunningRun_CandidateSet(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	store := invoice.NewPostgresStore(db)
	tenantID := testutil.CreateTestTenant(t, db, "Dunning Backfill Sweep")

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_backfill", DisplayName: "Backfill",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	plan, err := pricing.NewPostgresStore(db).CreatePlan(ctx, tenantID, domain.Plan{
		Code: "backfill-plan", Name: "Backfill", Currency: "USD",
		BillingInterval: domain.BillingMonthly, Status: domain.PlanActive, BaseAmountCents: 5000,
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	ps := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	sub, err := subscription.NewPostgresStore(db).Create(ctx, tenantID, domain.Subscription{
		Code: "sub-backfill", DisplayName: "Backfill Sub", CustomerID: cust.ID,
		Status: domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar,
		StartedAt: &ps, Items: []domain.SubscriptionItem{{PlanID: plan.ID, Quantity: 1}},
		CurrentBillingPeriodStart: &ps, CurrentBillingPeriodEnd: bivPE(ps),
	})
	if err != nil {
		t.Fatalf("create subscription: %v", err)
	}

	// seed creates a finalized invoice, then forces its terminal columns + age via
	// raw SQL (Create only makes drafts). ageMinutes backdates updated_at for the
	// cool-off dimension.
	n := 0
	seed := func(t *testing.T, status domain.InvoiceStatus, pay domain.InvoicePaymentStatus, amountDue int64, ageMinutes int) string {
		t.Helper()
		n++
		// Distinct period per invoice — the store enforces one invoice per
		// subscription per billing period (billing idempotency).
		periodStart := ps.AddDate(0, n, 0)
		periodEnd := periodStart.AddDate(0, 1, 0)
		due := periodStart.AddDate(0, 0, 30)
		inv, err := store.Create(ctx, tenantID, domain.Invoice{
			CustomerID: cust.ID, SubscriptionID: sub.ID,
			InvoiceNumber: fmt.Sprintf("VLX-BACKFILL-%03d", n),
			Status:        domain.InvoiceDraft, PaymentStatus: domain.PaymentPending, Currency: "USD",
			BillingPeriodStart: periodStart, BillingPeriodEnd: periodEnd,
			IssuedAt: &periodStart, DueAt: &due, NetPaymentTermDays: 30,
		})
		if err != nil {
			t.Fatalf("create invoice: %v", err)
		}
		tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
		if err != nil {
			t.Fatalf("begin seed tx: %v", err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE invoices
			   SET status = $1, payment_status = $2,
			       amount_due_cents = $3, total_amount_cents = $3,
			       updated_at = now() - ($4 * interval '1 minute')
			 WHERE id = $5`, string(status), string(pay), amountDue, ageMinutes, inv.ID); err != nil {
			t.Fatalf("seed invoice state: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit seed tx: %v", err)
		}
		return inv.ID
	}

	candidate := seed(t, domain.InvoiceFinalized, domain.PaymentFailed, 5000, 60)       // eligible
	withResolvedRun := seed(t, domain.InvoiceFinalized, domain.PaymentFailed, 5000, 60) // excluded: has a run
	fresh := seed(t, domain.InvoiceFinalized, domain.PaymentFailed, 5000, 0)            // excluded: cool-off
	paid := seed(t, domain.InvoicePaid, domain.PaymentSucceeded, 0, 60)                 // excluded: paid
	zero := seed(t, domain.InvoiceFinalized, domain.PaymentFailed, 0, 60)               // excluded: nothing owed

	// Give withResolvedRun a RESOLVED dunning run — the point is that a run EXISTS in
	// a NON-active state, so the state-agnostic NOT EXISTS still excludes it.
	dstore := dunning.NewPostgresStore(db)
	pol, err := dstore.UpsertPolicy(ctx, tenantID, domain.DunningPolicy{
		Name: "backfill-pol", Enabled: true, RetrySchedule: []string{"72h", "120h"},
		MaxRetryAttempts: 3, FinalSubscriptionAction: domain.SubActionNone, FinalInvoiceAction: domain.InvActionNone, GracePeriodDays: 1,
	})
	if err != nil {
		t.Fatalf("create dunning policy: %v", err)
	}
	resolvedAt := time.Now().UTC()
	if _, err := dstore.CreateRun(ctx, tenantID, domain.InvoiceDunningRun{
		InvoiceID: withResolvedRun, CustomerID: cust.ID, PolicyID: pol.ID,
		State: domain.DunningResolved, Resolution: domain.ResolutionPaymentRecovered, ResolvedAt: &resolvedAt,
	}); err != nil {
		t.Fatalf("seed resolved dunning run: %v", err)
	}

	olderThan := time.Now().UTC().Add(-10 * time.Minute)
	got, err := store.ListFailedWithoutDunningRun(ctx, olderThan, 50)
	if err != nil {
		t.Fatalf("ListFailedWithoutDunningRun: %v", err)
	}
	in := map[string]bool{}
	for _, inv := range got {
		in[inv.ID] = true
	}

	if !in[candidate] {
		t.Errorf("a failed, un-dunned, aged, still-owed invoice must be a backfill candidate")
	}
	if in[withResolvedRun] {
		t.Errorf("an invoice with a RESOLVED dunning run must be excluded — the NOT EXISTS is state-agnostic; a state-filtered predicate would re-dun a resolved invoice forever")
	}
	if in[fresh] {
		t.Errorf("a freshly-updated failed invoice must be excluded by the cool-off (inline SettleFailed dunning must win the common case)")
	}
	if in[paid] {
		t.Errorf("a paid invoice must never be a dunning candidate")
	}
	if in[zero] {
		t.Errorf("a zero-balance invoice must never be a dunning candidate")
	}
}
