package invoice_test

import (
	"context"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestFinalizeWritesQueueCollection pins the intent-in-the-same-write rule on
// every store writer that can make an invoice finalized and unpaid: the row
// commits with auto_charge_pending=true, so no crash between the finalize and
// its collection can hide the invoice from the auto-charge sweep. Drafts are
// not queued, and void retires the queue in its own write.
func TestFinalizeWritesQueueCollection(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Finalize Queues")
	store := invoice.NewPostgresStore(db)

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{ExternalID: "cus_q", DisplayName: "Q"})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	n := 0
	base := func(status domain.InvoiceStatus) domain.Invoice {
		n++
		return domain.Invoice{
			CustomerID: cust.ID, InvoiceNumber: "INV-Q-" + string(rune('A'+n)),
			Status: status, PaymentStatus: domain.PaymentPending, Currency: "USD",
			SubtotalCents: 5000, TotalAmountCents: 5000, AmountDueCents: 5000,
			BillingPeriodStart: time.Now().UTC().Add(-30 * 24 * time.Hour),
			BillingPeriodEnd:   time.Now().UTC().Add(time.Duration(n) * time.Minute),
			BillingReason:      domain.BillingReasonManual,
		}
	}
	queued := func(t *testing.T, id string) bool {
		t.Helper()
		got, err := store.Get(ctx, tenantID, id)
		if err != nil {
			t.Fatalf("re-read: %v", err)
		}
		return got.AutoChargePending
	}

	t.Run("Create finalized → queued; draft → not", func(t *testing.T) {
		fin, err := store.Create(ctx, tenantID, base(domain.InvoiceFinalized))
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if !queued(t, fin.ID) {
			t.Error("Create(finalized) must queue collection in the INSERT")
		}
		draft, err := store.Create(ctx, tenantID, base(domain.InvoiceDraft))
		if err != nil {
			t.Fatalf("create draft: %v", err)
		}
		if queued(t, draft.ID) {
			t.Error("a draft is not owed yet and must not be queued")
		}
	})

	t.Run("CreateWithLineItems finalized → queued", func(t *testing.T) {
		inv, err := store.CreateWithLineItems(ctx, tenantID, base(domain.InvoiceFinalized), nil)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if !queued(t, inv.ID) {
			t.Error("CreateWithLineItems(finalized) must queue collection in the INSERT")
		}
	})

	t.Run("UpdateStatus draft→finalized → queued; void → retired", func(t *testing.T) {
		draft, err := store.Create(ctx, tenantID, base(domain.InvoiceDraft))
		if err != nil {
			t.Fatalf("create draft: %v", err)
		}
		if _, err := store.UpdateStatus(ctx, tenantID, draft.ID, domain.InvoiceFinalized); err != nil {
			t.Fatalf("finalize: %v", err)
		}
		if !queued(t, draft.ID) {
			t.Error("UpdateStatus(finalized) must queue collection in the same UPDATE")
		}
		if _, err := store.UpdateStatus(ctx, tenantID, draft.ID, domain.InvoiceVoided); err != nil {
			t.Fatalf("void: %v", err)
		}
		if queued(t, draft.ID) {
			t.Error("void must retire the queue flag")
		}
	})

	t.Run("FinalizeWithDates draft→finalized → queued", func(t *testing.T) {
		draft, err := store.Create(ctx, tenantID, base(domain.InvoiceDraft))
		if err != nil {
			t.Fatalf("create draft: %v", err)
		}
		now := time.Now().UTC()
		if _, err := store.FinalizeWithDates(ctx, tenantID, draft.ID, now, now.AddDate(0, 0, 30)); err != nil {
			t.Fatalf("finalize: %v", err)
		}
		if !queued(t, draft.ID) {
			t.Error("FinalizeWithDates must queue collection in the same UPDATE")
		}
	})
}
