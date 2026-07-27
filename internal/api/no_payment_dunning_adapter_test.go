package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
)

type fakeRunStarter struct {
	calls int
	err   error
}

func (f *fakeRunStarter) StartDunning(_ context.Context, _, _, _ string, _ time.Time, _ domain.DunningStartCause) (domain.InvoiceDunningRun, error) {
	f.calls++
	return domain.InvoiceDunningRun{}, f.err
}

// TestDunningStarterAdapter_SwallowsDisabled verifies the no-payment
// enrollment adapter treats "dunning disabled" (the only InvalidState
// StartDunning returns) as a deliberate no-op, NOT a sweep error — so a
// disabled-dunning tenant doesn't emit an error per stalled invoice per
// tick.
func TestDunningStarterAdapter_SwallowsDisabled(t *testing.T) {
	f := &fakeRunStarter{err: errs.InvalidState("dunning is disabled")}
	a := &dunningStarterAdapter{dunning: f}
	if err := a.StartDunning(context.Background(), "t1", "inv_1", "cus_1", time.Time{}, domain.DunningCauseNoPaymentMethod); err != nil {
		t.Fatalf("disabled StartDunning should be swallowed, got %v", err)
	}
	if f.calls != 1 {
		t.Fatalf("calls = %d, want 1", f.calls)
	}
}

// TestDunningStarterAdapter_SwallowsNotConfigured verifies the second
// deliberate-skip case (Finding 2): a tenant with NO effective policy —
// StartDunning maps that to InvalidState — is swallowed as a no-op, exactly
// like disabled, so an unconfigured tenant never poisons the money-path sweep.
func TestDunningStarterAdapter_SwallowsNotConfigured(t *testing.T) {
	f := &fakeRunStarter{err: errs.InvalidState("dunning not configured — no policy for tenant")}
	a := &dunningStarterAdapter{dunning: f}
	if err := a.StartDunning(context.Background(), "t1", "inv_1", "cus_1", time.Time{}, domain.DunningCauseNoPaymentMethod); err != nil {
		t.Fatalf("no-policy InvalidState should be swallowed, got %v", err)
	}
	if f.calls != 1 {
		t.Fatalf("calls = %d, want 1", f.calls)
	}
}

// TestDunningStarterAdapter_PropagatesRealError verifies a genuine failure
// (e.g. a DB error in CreateRun) is surfaced so the sweep logs it rather
// than silently dropping the enrollment.
func TestDunningStarterAdapter_PropagatesRealError(t *testing.T) {
	f := &fakeRunStarter{err: errors.New("create run: db down")}
	a := &dunningStarterAdapter{dunning: f}
	if err := a.StartDunning(context.Background(), "t1", "inv_1", "cus_1", time.Time{}, domain.DunningCauseNoPaymentMethod); err == nil {
		t.Fatal("expected the DB error to propagate, got nil")
	}
}

// TestDunningStarterAdapter_SuccessReturnsNil verifies a successful
// enrollment returns nil.
func TestDunningStarterAdapter_SuccessReturnsNil(t *testing.T) {
	f := &fakeRunStarter{}
	a := &dunningStarterAdapter{dunning: f}
	if err := a.StartDunning(context.Background(), "t1", "inv_1", "cus_1", time.Time{}, domain.DunningCauseNoPaymentMethod); err != nil {
		t.Fatalf("success should return nil, got %v", err)
	}
}
