package creditnote_test

import (
	"context"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/creditnote"
	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestCreditNote_RecordedAt_DualStamp pins ADR-104 Invariant A's corrected
// boundary (found live: an operator-issued CN on a frozen-clock invoice was
// the only row on the timeline with no "Recorded" subline). A CN created
// under a bound simulated ctx must carry BOTH calendars: created_at on the
// entity's (the frozen instant, ADR-030) and recorded_at on ours (the real
// INSERT moment) — the same dual-stamp shape as charge attempts (0162) and
// emails (0163).
func TestCreditNote_RecordedAt_DualStamp(t *testing.T) {
	db := testutil.SetupTestDB(t)
	base := postgres.WithLivemode(context.Background(), false)

	tenantID := testutil.CreateTestTenant(t, db, "CN recorded_at")
	custStore := customer.NewPostgresStore(db)
	invStore := invoice.NewPostgresStore(db)
	cnStore := creditnote.NewPostgresStore(db)

	cust, err := custStore.Create(base, tenantID, domain.Customer{ExternalID: "rec-cus", DisplayName: "Rec Cus"})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	now := time.Now().UTC()
	inv, err := invStore.Create(base, tenantID, domain.Invoice{
		CustomerID: cust.ID, InvoiceNumber: "VLX-REC-1",
		Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentPending,
		Currency: "USD", SubtotalCents: 5000, TotalAmountCents: 5000, AmountDueCents: 5000,
		BillingPeriodStart: now.Add(-30 * 24 * time.Hour), BillingPeriodEnd: now,
		IssuedAt: &now,
	})
	if err != nil {
		t.Fatalf("create invoice: %v", err)
	}

	frozen := time.Date(2027, 6, 1, 12, 0, 0, 0, time.UTC)
	simCtx := clock.WithSim(base, clock.Sim{At: frozen, TestClockID: "vlx_tclk_rec"})

	cn, err := cnStore.Create(simCtx, tenantID, domain.CreditNote{
		InvoiceID: inv.ID, CustomerID: cust.ID, CreditNoteNumber: "CN-REC-1",
		Status: domain.CreditNoteDraft, Reason: "billing error",
		SubtotalCents: 100, TotalCents: 100, CreditAmountCents: 100,
		Currency: "USD", RefundStatus: domain.RefundNone, IsSimulated: true,
	})
	if err != nil {
		t.Fatalf("create cn: %v", err)
	}

	// Entity calendar: created_at is the frozen instant, not wall.
	if !cn.CreatedAt.Equal(frozen) {
		t.Errorf("created_at = %s, want the frozen instant %s (ADR-030)", cn.CreatedAt, frozen)
	}
	// Our calendar: recorded_at is the real INSERT moment — present, wall,
	// and NOT the simulated instant.
	if cn.RecordedAt == nil {
		t.Fatal("recorded_at is nil — the row carries no wall stamp, and the timeline cannot show both calendars (Invariant A)")
	}
	if cn.RecordedAt.Equal(frozen) {
		t.Errorf("recorded_at = the frozen instant — stamped from the simulated clock instead of the real INSERT moment")
	}
	if d := time.Since(*cn.RecordedAt); d < 0 || d > 2*time.Minute {
		t.Errorf("recorded_at = %s, not within the test's real-time window (drift %s)", cn.RecordedAt, d)
	}

	// Read path returns it too (List projection).
	list, err := cnStore.List(base, creditnote.ListFilter{TenantID: tenantID, InvoiceID: inv.ID})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].RecordedAt == nil {
		t.Fatalf("List dropped recorded_at (rows=%d)", len(list))
	}
}
