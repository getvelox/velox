package invoice_test

import (
	"context"
	"testing"

	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestUpdateTaxAtomic_InitialAttemptDoesNotCountAsRetry locks the FLOW
// I12 finding (2026-07-26, NIM-000258): the finalize-time FIRST tax
// computation of an operator-composed invoice rode the shared
// UpdateTaxAtomic writer and booked itself as "retry #1" — every
// healthy one-off invoice then carried a "Diagnostic detail · Tax
// retries 1" card for a retry that never happened, and the counter
// meant different things per invoice type (cycle invoices' build-time
// computation never counted). InitialAttempt=true skips the bump;
// retries (InitialAttempt=false — the operator button, the ADR-017
// worker, and a re-finalize of a STUCK draft) still count every
// attempt.
func TestUpdateTaxAtomic_InitialAttemptDoesNotCountAsRetry(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Tax Attempt Count")
	store := invoice.NewPostgresStore(db)

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus-tax-count", DisplayName: "Tax Count",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}

	inv, err := store.Create(ctx, tenantID, domain.Invoice{
		CustomerID: cust.ID, Status: domain.InvoiceDraft,
		PaymentStatus: domain.PaymentPending,
		Currency:      "USD", SubtotalCents: 10000, TotalAmountCents: 10000,
		AmountDueCents: 10000,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	update := domain.InvoiceTaxRetryUpdate{
		SubtotalCents:    10000,
		TotalAmountCents: 11000,
	}
	update.TaxAmountCents = 1000
	update.TaxStatus = domain.InvoiceTaxOK
	update.TaxProvider = "manual"

	// Finalize-time initial computation: no bump.
	update.InitialAttempt = true
	got, err := store.UpdateTaxAtomic(ctx, tenantID, inv.ID, update, nil)
	if err != nil {
		t.Fatalf("initial UpdateTaxAtomic: %v", err)
	}
	if got.TaxRetryCount != 0 {
		t.Errorf("initial attempt bumped tax_retry_count to %d, want 0 (a first computation is not a retry)", got.TaxRetryCount)
	}

	// A genuine retry: bumps.
	update.InitialAttempt = false
	got, err = store.UpdateTaxAtomic(ctx, tenantID, inv.ID, update, nil)
	if err != nil {
		t.Fatalf("retry UpdateTaxAtomic: %v", err)
	}
	if got.TaxRetryCount != 1 {
		t.Errorf("retry left tax_retry_count at %d, want 1", got.TaxRetryCount)
	}
}
