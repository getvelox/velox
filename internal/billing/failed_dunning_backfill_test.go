package billing

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// failedBackfillInvoice is a finalized, failed, still-owed invoice updated at the
// given instant — the dunning_backfill candidate shape (the "no run" NOT EXISTS is
// a DB concern the mock can't model; that's the real-Postgres test's job).
func failedBackfillInvoice(id string, updatedAt time.Time) domain.Invoice {
	return domain.Invoice{
		ID: id, TenantID: "t1", CustomerID: "cus_1",
		Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentFailed,
		AmountDueCents: 5000, UpdatedAt: updatedAt,
	}
}

// TestEnrollFailedWithoutDunning_EnrollsFailedInvoice locks the backstop: a
// finalized invoice left failed with no run (the SettleFailed post-commit crash /
// exhausted-retry window) is handed to the idempotent StartDunning by the sweep.
func TestEnrollFailedWithoutDunning_EnrollsFailedInvoice(t *testing.T) {
	aged := time.Now().UTC().Add(-time.Hour) // older than the cool-off
	inv := &mockInvoices{invoices: []domain.Invoice{failedBackfillInvoice("inv_1", aged)}}
	engine := noPMEngine(t, inv)
	starter := &recordingDunningStarter{}
	engine.SetDunningStarter(starter)

	swept, errsOut := engine.EnrollFailedWithoutDunning(context.Background(), 10)
	if len(errsOut) != 0 {
		t.Fatalf("unexpected errors: %v", errsOut)
	}
	if swept != 1 {
		t.Fatalf("swept = %d, want 1", swept)
	}
	if len(starter.started) != 1 || starter.started[0] != "inv_1" {
		t.Fatalf("StartDunning calls = %v, want [inv_1]", starter.started)
	}
	// The failed-invoice backfill enrolls invoices whose charge REALLY
	// failed — its declared cause is payment_failed.
	if len(starter.causes) != 1 || starter.causes[0] != domain.DunningCausePaymentFailed {
		t.Fatalf("causes = %v, want [payment_failed]", starter.causes)
	}
}

// TestEnrollFailedWithoutDunning_CoolOffExcludesFresh locks the cool-off: a
// just-updated failed invoice is skipped so the inline SettleFailed StartDunning
// wins the common case and the sweep stays a pure backstop.
func TestEnrollFailedWithoutDunning_CoolOffExcludesFresh(t *testing.T) {
	inv := &mockInvoices{invoices: []domain.Invoice{failedBackfillInvoice("inv_fresh", time.Now().UTC())}}
	engine := noPMEngine(t, inv)
	starter := &recordingDunningStarter{}
	engine.SetDunningStarter(starter)

	swept, errsOut := engine.EnrollFailedWithoutDunning(context.Background(), 10)
	if swept != 0 || len(starter.started) != 0 || len(errsOut) != 0 {
		t.Fatalf("a fresh failed invoice must be inside the cool-off: swept=%d started=%v errs=%v", swept, starter.started, errsOut)
	}
}

// TestEnrollFailedWithoutDunning_CollectsPerInvoiceErrors: one bad row doesn't abort
// the sweep — the error is collected, not panicked.
func TestEnrollFailedWithoutDunning_CollectsPerInvoiceErrors(t *testing.T) {
	aged := time.Now().UTC().Add(-time.Hour)
	inv := &mockInvoices{invoices: []domain.Invoice{failedBackfillInvoice("inv_1", aged)}}
	engine := noPMEngine(t, inv)
	engine.SetDunningStarter(&recordingDunningStarter{err: errors.New("create run failed")})

	swept, errsOut := engine.EnrollFailedWithoutDunning(context.Background(), 10)
	if swept != 0 {
		t.Fatalf("swept = %d, want 0 (the only candidate errored)", swept)
	}
	if len(errsOut) != 1 {
		t.Fatalf("errors = %v, want 1", errsOut)
	}
}
