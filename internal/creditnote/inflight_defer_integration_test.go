package creditnote_test

import (
	"context"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/creditnote"
	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestListPendingClawbackDrafts_DefersInFlightSource is the real-Postgres proof
// of the ADR-059 reconciler gate: a clawback draft whose source invoice's
// payment is in flight (processing/unknown) is EXCLUDED from the scan, and
// becomes eligible the moment the source settles — with NO time window, so a
// draft far older than the prior 24h bound still issues once its (slow ACH/SEPA)
// source settles. The in-memory store can't model the cross-table NOT-EXISTS
// gate; this proves it against real SQL.
func TestListPendingClawbackDrafts_DefersInFlightSource(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "CN Defer Gate")

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_defer", DisplayName: "Defer",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}

	invStore := invoice.NewPostgresStore(db)
	now := time.Now().UTC()
	issued := now
	inv, err := invStore.Create(ctx, tenantID, domain.Invoice{
		CustomerID:         cust.ID,
		InvoiceNumber:      "INV-DEFER-1",
		Status:             domain.InvoiceFinalized,
		PaymentStatus:      domain.PaymentProcessing, // in flight
		Currency:           "USD",
		SubtotalCents:      10000,
		TotalAmountCents:   10000,
		AmountDueCents:     10000,
		BillingPeriodStart: now.Add(-30 * 24 * time.Hour),
		BillingPeriodEnd:   now,
		IssuedAt:           &issued,
	})
	if err != nil {
		t.Fatalf("create invoice: %v", err)
	}

	cnStore := creditnote.NewPostgresStore(db)
	draft, err := cnStore.Create(ctx, tenantID, domain.CreditNote{
		InvoiceID:        inv.ID,
		CustomerID:       cust.ID,
		CreditNoteNumber: "CN-DEFER-1",
		Status:           domain.CreditNoteDraft,
		Reason:           "subscription_cancellation",
		SubtotalCents:    4000,
		TotalCents:       4000,
		Currency:         "USD",
		RefundStatus:     domain.RefundNone,
		IssuePending:     true,
	})
	if err != nil {
		t.Fatalf("create clawback draft: %v", err)
	}

	mustCount := func(want int, msg string) {
		t.Helper()
		drafts, err := cnStore.ListPendingClawbackDrafts(ctx, 100, false)
		if err != nil {
			t.Fatalf("list pending clawback drafts: %v", err)
		}
		if len(drafts) != want {
			t.Fatalf("%s: got %d pending drafts, want %d", msg, len(drafts), want)
		}
	}

	// 1. Source in flight (processing) → the draft is deferred, excluded from the scan.
	mustCount(0, "in-flight source must be skipped by the reconciler scan")

	// 2. Backdate the draft well past the old 24h window. A slow ACH/SEPA source
	//    can settle days later, so the scan must NOT age the draft out.
	backdateCreditNote(t, db, draft.ID)
	mustCount(0, "aged draft on an in-flight source must STILL be skipped (gate is source state, not age)")

	// 3. Source settles → the draft becomes eligible despite being >24h old,
	//    proving the 24h window was removed (else this would return 0).
	if _, err := invStore.UpdatePayment(ctx, tenantID, inv.ID, domain.PaymentSucceeded, "pi_defer", "", &now); err != nil {
		t.Fatalf("settle source: %v", err)
	}
	mustCount(1, "settled source must make even an aged draft eligible — no 24h window")
}

// TestListPendingClawbackDrafts_ParkedSourceReleasedByWriteOff covers the one
// source that NEVER settles, which the defer gate above was written before we
// had.
//
// ADR-107 parks an invoice at payment_status='unknown' with no PaymentIntent id
// when a charge attempt cannot be identified with the provider. "The draft waits
// until the source settles" then becomes a wait with no end: the draft is
// excluded from this scan permanently — never issued, never voided, no error log
// (deferred drafts log nothing by design), while the pending-drafts gauge counts
// it forever. The customer is owed the relief and cannot be given it.
//
// The exit is the operator writing the invoice off, which is the only action a
// parked invoice allows. That must release the draft — and the release has to
// come from the invoice's STATUS, because a write-off deliberately does not
// touch payment_status (we never learned whether the card was charged, so
// recording an answer would be a lie). This test pins exactly that: still
// 'unknown' after the write-off, and eligible anyway.
//
// Eligible, not issued: Issue()'s orphan guard then VOIDS the draft, since a
// written-off invoice collected nothing and there is nothing to claw back. That
// branch already existed and was already covered (inflight_gate_test.go); it was
// simply unreachable, because this query never handed it the draft.
func TestListPendingClawbackDrafts_ParkedSourceReleasedByWriteOff(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "CN Parked Source")

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_parked", DisplayName: "Parked",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}

	invStore := invoice.NewPostgresStore(db)
	now := time.Now().UTC()
	issued := now
	// Parked: unknown payment, no PaymentIntent id — nothing can ever name the
	// attempt, so no webhook and no sweep will resolve this row.
	inv, err := invStore.Create(ctx, tenantID, domain.Invoice{
		CustomerID:            cust.ID,
		InvoiceNumber:         "INV-PARKED-1",
		Status:                domain.InvoiceFinalized,
		PaymentStatus:         domain.PaymentUnknown,
		StripePaymentIntentID: "",
		Currency:              "USD",
		SubtotalCents:         10000,
		TotalAmountCents:      10000,
		AmountDueCents:        10000,
		BillingPeriodStart:    now.Add(-30 * 24 * time.Hour),
		BillingPeriodEnd:      now,
		IssuedAt:              &issued,
	})
	if err != nil {
		t.Fatalf("create parked invoice: %v", err)
	}

	cnStore := creditnote.NewPostgresStore(db)
	if _, err := cnStore.Create(ctx, tenantID, domain.CreditNote{
		InvoiceID:        inv.ID,
		CustomerID:       cust.ID,
		CreditNoteNumber: "CN-PARKED-1",
		Status:           domain.CreditNoteDraft,
		Reason:           "subscription_cancellation",
		SubtotalCents:    4000,
		TotalCents:       4000,
		Currency:         "USD",
		RefundStatus:     domain.RefundNone,
		IssuePending:     true,
	}); err != nil {
		t.Fatalf("create clawback draft: %v", err)
	}

	mustCount := func(want int, msg string) {
		t.Helper()
		drafts, err := cnStore.ListPendingClawbackDrafts(ctx, 100, false)
		if err != nil {
			t.Fatalf("list pending clawback drafts: %v", err)
		}
		if len(drafts) != want {
			t.Fatalf("%s: got %d pending drafts, want %d", msg, len(drafts), want)
		}
	}

	// 1. While the invoice is open, a parked source defers like any other
	//    in-flight source. This is the control: it proves the release below
	//    comes from the write-off and not from 'unknown' being ignored here.
	mustCount(0, "a parked source on an OPEN invoice must still defer — its charge may yet be confirmed by a late webhook")

	// 2. The operator writes it off — the only action ADR-107 leaves available.
	if _, err := invStore.UpdateStatus(ctx, tenantID, inv.ID, domain.InvoiceUncollectible); err != nil {
		t.Fatalf("write off parked invoice: %v", err)
	}

	// 3. The payment question is still open, and must stay that way. If a later
	//    change starts settling payment_status on write-off, this fails loudly:
	//    that would be recording "the card was not charged" on the strength of an
	//    operator's click, which is precisely what we do not know.
	after, err := invStore.Get(ctx, tenantID, inv.ID)
	if err != nil {
		t.Fatalf("get invoice after write-off: %v", err)
	}
	if after.PaymentStatus != domain.PaymentUnknown {
		t.Fatalf("write-off changed payment_status to %q — it must stay 'unknown'; the write-off closes the INVOICE, it does not answer whether the card was charged", after.PaymentStatus)
	}

	// 4. Released — despite payment_status being unchanged. Without the status
	//    term in the scan predicate this returns 0 and the draft is stranded for
	//    the life of the tenant.
	mustCount(1, "a written-off source must release its clawback draft to the reconciler, which voids it as an orphan")
}

// backdateCreditNote pushes a credit note's updated_at 10 days into the past to
// prove the reconciler scan has no time window (the prior 24h bound would have
// dropped it). TxBypass: the test is RLS-agnostic infrastructure setup.
func backdateCreditNote(t *testing.T, db *postgres.DB, id string) {
	t.Helper()
	tx, err := db.BeginTx(context.Background(), postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin backdate tx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(context.Background(),
		`UPDATE credit_notes SET updated_at = now() - interval '10 days' WHERE id = $1`, id); err != nil {
		t.Fatalf("backdate credit note: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit backdate: %v", err)
	}
}
