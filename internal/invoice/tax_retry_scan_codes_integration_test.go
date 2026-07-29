package invoice_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// recordingTaxRetrier satisfies invoice.TaxRetrier and records which
// invoices the reconciler handed it.
type recordingTaxRetrier struct {
	mu  sync.Mutex
	ids []string
}

func (r *recordingTaxRetrier) RetryTaxForInvoice(_ context.Context, _, invoiceID string) (domain.Invoice, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ids = append(r.ids, invoiceID)
	return domain.Invoice{ID: invoiceID}, nil
}

func (r *recordingTaxRetrier) ComputeTaxForInvoice(_ context.Context, _, invoiceID string) (domain.Invoice, error) {
	return domain.Invoice{ID: invoiceID}, nil
}

// TestRetryPendingTax_ScansTheDeclaredSingleSource locks the 2026-07-29
// find: domain.TaxRetryableErrorCodes() — whose doc comment names it "the
// single source for WHICH tax error codes the reconciler retries" — was
// contradicted by a hand-rolled {provider_outage, unknown} list at BOTH
// scan call sites, so provider_not_configured rows never entered the queue
// the attention banner promised ("calculation will retry on the next
// scheduler tick"). Two scratch invoices sat through twelve live ticks at
// retry_count=0 proving it. This test seeds one stuck invoice PER
// retryable code and asserts the wall-cron scan picks up every one of
// them — so the next hand-rolled divergence fails here, whatever the code.
func TestRetryPendingTax_ScansTheDeclaredSingleSource(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)

	tenantID := testutil.CreateTestTenant(t, db, "Tax scan single source")
	custStore := customer.NewPostgresStore(db)
	invStore := invoice.NewPostgresStore(db)

	cust, err := custStore.Create(ctx, tenantID, domain.Customer{ExternalID: "scan-cus", DisplayName: "Scan Cus"})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}

	now := time.Now().UTC()
	want := map[string]string{} // invoice id → code
	for i, code := range domain.TaxRetryableErrorCodes() {
		inv, err := invStore.Create(ctx, tenantID, domain.Invoice{
			CustomerID:         cust.ID,
			InvoiceNumber:      "VLX-SCAN-" + string(rune('A'+i)),
			Status:             domain.InvoiceDraft,
			PaymentStatus:      domain.PaymentPending,
			Currency:           "USD",
			SubtotalCents:      1000,
			TotalAmountCents:   1000,
			AmountDueCents:     1000,
			BillingPeriodStart: now.Add(-30 * 24 * time.Hour),
			BillingPeriodEnd:   now,
			BillingReason:      domain.BillingReasonSubscriptionCycle,
			TaxFacts:           domain.TaxFacts{TaxStatus: domain.InvoiceTaxPending, TaxErrorCode: code},
		})
		if err != nil {
			t.Fatalf("create invoice for code %s: %v", code, err)
		}
		want[inv.ID] = code
	}

	svc := invoice.NewService(invStore, nil, nil)
	rec := &recordingTaxRetrier{}
	svc.SetTaxRetrier(rec)

	processed, errs := svc.RetryPendingTax(ctx, 50)
	if len(errs) > 0 {
		t.Fatalf("retry errors: %v", errs)
	}
	if processed < len(want) {
		t.Errorf("processed = %d, want at least %d", processed, len(want))
	}

	seen := map[string]bool{}
	for _, id := range rec.ids {
		seen[id] = true
	}
	for id, code := range want {
		if !seen[id] {
			t.Errorf("invoice with tax_error_code=%q was NOT picked up by the scan — the call site has diverged from domain.TaxRetryableErrorCodes() again", code)
		}
	}
}
