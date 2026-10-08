package invoice_test

import (
	"context"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/credit"
	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/tenant"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestManualFinalize_ZeroDue_SettlesPaid_E2E pins Service.SettleZeroDue, which
// the operator "Collect payment" action uses on an invoice with nothing due.
// (Finalize itself now collects through the billing engine's collector,
// ADR-116; billing's TestManualFinalize_CollectsThroughTheEngine_E2E covers
// that path.) Against Postgres: the invoice must land PAID — exercising the store's MarkPaid state-machine
// guard (finalized-only, tax ok) with real column defaults — not strand
// finalized/payment_pending as a permanently-overdue attention item
// (ADR-066 class; the manual writer's T12).
func TestManualFinalize_ZeroDue_SettlesPaid_E2E(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Zero Due Corp")

	store := invoice.NewPostgresStore(db)
	svc := invoice.NewService(store, clock.Real(), tenant.NewSettingsStore(db))

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_zero_1", DisplayName: "Zero Due",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}

	draft, err := svc.Create(ctx, tenantID, invoice.CreateInput{CustomerID: cust.ID})
	if err != nil {
		t.Fatalf("create empty manual invoice: %v", err)
	}
	if draft.AmountDueCents != 0 || draft.Status != domain.InvoiceDraft {
		t.Fatalf("seed expectations: due=%d status=%s, want 0/draft", draft.AmountDueCents, draft.Status)
	}

	finalized, err := svc.Finalize(ctx, tenantID, draft.ID)
	if err != nil {
		t.Fatalf("finalize empty draft: %v", err)
	}
	if finalized.Status != domain.InvoiceFinalized {
		t.Fatalf("finalize: status=%s, want finalized", finalized.Status)
	}

	settled, err := svc.SettleZeroDue(ctx, tenantID, finalized.ID)
	if err != nil {
		t.Fatalf("SettleZeroDue: %v", err)
	}
	if settled.Status != domain.InvoicePaid || settled.PaymentStatus != domain.PaymentSucceeded {
		t.Errorf("settled = %s/%s, want paid/succeeded", settled.Status, settled.PaymentStatus)
	}

	got, err := store.Get(ctx, tenantID, finalized.ID)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if got.Status != domain.InvoicePaid || got.PaymentStatus != domain.PaymentSucceeded {
		t.Errorf("stored = %s/%s, want paid/succeeded (zero-due must settle, never strand)", got.Status, got.PaymentStatus)
	}
	if got.PaidAt == nil {
		t.Error("paid_at must be stamped on the zero-due settle")
	}
}

// TestManualInvoice_CreditBalanceApplied_E2E pins Service.ApplyCreditBalance,
// which the operator "Collect payment" action uses before charging, against
// Postgres with the REAL credit ledger: the balance drains into the invoice
// and the remainder is what a card would be charged. (Finalize collects
// through the billing engine instead, ADR-116.)
func TestManualInvoice_CreditBalanceApplied_E2E(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Manual Credit Corp")

	store := invoice.NewPostgresStore(db)
	svc := invoice.NewService(store, clock.Real(), tenant.NewSettingsStore(db))
	creditSvc := credit.NewService(credit.NewPostgresStore(db))
	svc.SetCreditApplier(creditSvc)

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_manual_credit", DisplayName: "Manual Credit",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	if _, err := creditSvc.Grant(ctx, tenantID, credit.GrantInput{
		CustomerID: cust.ID, AmountCents: 2000, Description: "seed", At: time.Now().UTC().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("grant: %v", err)
	}

	draft, err := svc.Create(ctx, tenantID, invoice.CreateInput{CustomerID: cust.ID})
	if err != nil {
		t.Fatalf("create draft: %v", err)
	}
	if _, err := svc.AddLineItem(ctx, tenantID, draft.ID, invoice.AddLineItemInput{
		Description: "Onboarding", Quantity: 1, UnitAmountCents: 5000,
	}); err != nil {
		t.Fatalf("add line: %v", err)
	}
	if _, err := svc.Finalize(ctx, tenantID, draft.ID); err != nil {
		t.Fatalf("finalize: %v", err)
	}

	applied, err := svc.ApplyCreditBalance(ctx, tenantID, draft.ID)
	if err != nil {
		t.Fatalf("ApplyCreditBalance: %v", err)
	}
	if applied.AmountDueCents != 3000 || applied.CreditsAppliedCents != 2000 {
		t.Errorf("due=%d creditsApplied=%d, want 3000/2000 (card would charge exactly the remainder)",
			applied.AmountDueCents, applied.CreditsAppliedCents)
	}
	bal, err := creditSvc.GetBalance(ctx, tenantID, cust.ID)
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	if bal.BalanceCents != 0 {
		t.Errorf("ledger balance = %d, want 0 (fully drained into the invoice)", bal.BalanceCents)
	}
}
