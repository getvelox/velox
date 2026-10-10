package billing

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// recordingDunningStarter records every StartDunning call so a test can
// assert which invoices got enrolled. When err is set it's returned for
// every call (simulating a dunning failure).
type recordingDunningStarter struct {
	started []string
	err     error
	causes  []domain.DunningStartCause
}

func (d *recordingDunningStarter) StartDunning(_ context.Context, _, invoiceID, _ string, _ time.Time, cause domain.DunningStartCause) error {
	d.causes = append(d.causes, cause)
	if d.err != nil {
		return d.err
	}
	d.started = append(d.started, invoiceID)
	return nil
}

// recordingDunningResolver backs the settle-path tests: post-#442 the
// resolver is a required collaborator, so any fixture whose flow can
// MarkPaid an invoice must wire one.
type recordingDunningResolver struct {
	resolved []string
	err      error
}

func (d *recordingDunningResolver) ResolveByInvoice(_ context.Context, _, invoiceID string, _ domain.DunningResolution) error {
	if d.err != nil {
		return d.err
	}
	d.resolved = append(d.resolved, invoiceID)
	return nil
}

func noPMEngine(t *testing.T, inv *mockInvoices) *Engine {
	t.Helper()
	return wireBaseTax(NewEngine(
		&mockSubs{cycleUpdated: make(map[string]bool)},
		&mockUsage{}, &mockPricing{}, inv, nil, &mockSettings{},
		&fakePaymentSetups{}, &recordingCharger{}, billingTestClock(),
	))
}

// The collector starts no-payment dunning itself, at the one point where it
// is known to be right: credits applied, money still owed, and no card. That
// is what keeps a card-less invoice from waiting forever with nothing to
// charge (it reaches pause/cancel/write-off), and it is the only start site.
func TestCollector_NoCard_StartsNoPaymentDunningOnce(t *testing.T) {
	inv := &mockInvoices{invoices: []domain.Invoice{pendingInvoice()}}
	engine := noPMEngine(t, inv)
	starter := &recordingDunningStarter{}
	engine.SetDunningStarter(starter)

	if _, errs := engine.RetryPendingCharges(context.Background(), 10, nil); len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(starter.started) != 1 || starter.started[0] != "inv_1" {
		t.Fatalf("StartDunning calls = %v, want [inv_1]", starter.started)
	}
	// Nothing was ever charged, so the run must NOT claim a payment failure.
	if starter.causes[0] != domain.DunningCauseNoPaymentMethod {
		t.Fatalf("cause = %v, want no_payment_method", starter.causes[0])
	}
}

// A failed start is surfaced and leaves the invoice queued, so the next sweep
// retries it rather than the invoice silently never reaching dunning.
func TestCollector_NoCard_StartFailureIsRetried(t *testing.T) {
	inv := &mockInvoices{invoices: []domain.Invoice{pendingInvoice()}}
	engine := noPMEngine(t, inv)
	engine.SetDunningStarter(&recordingDunningStarter{err: errors.New("create run failed")})

	if _, errs := engine.RetryPendingCharges(context.Background(), 10, nil); len(errs) != 1 {
		t.Fatalf("errors = %v, want the start failure surfaced", errs)
	}
	if !inv.invoices[0].AutoChargePending {
		t.Fatal("the invoice must stay queued so the next sweep retries the start")
	}
}

// No dunning on anything that isn't "owed, no card": a credit apply that
// failed (the invoice may well be covered once it succeeds), a payment-method
// lookup error (unknown is not missing), an invoice credits fully covered, and
// a customer with a card.
func TestCollector_NoDunningUnlessOwedAndCardless(t *testing.T) {
	cases := []struct {
		name  string
		setup func(e *Engine, inv *mockInvoices)
	}{
		{"credit apply failed", func(e *Engine, inv *mockInvoices) {
			e.credits = &fakeCreditApplier{inv: inv, err: errors.New("ledger blip")}
		}},
		{"payment-method lookup error", func(e *Engine, _ *mockInvoices) {
			e.paymentSetups = &erroringPaymentSetups{err: errors.New("db blip")}
		}},
		{"credits cover it", func(e *Engine, inv *mockInvoices) {
			e.credits = &fakeCreditApplier{inv: inv, applyCents: 1000}
			e.SetDunningResolver(&recordingDunningResolver{})
		}},
		{"customer has a card", func(e *Engine, _ *mockInvoices) {
			e.paymentSetups = &fakePaymentSetups{ready: true, stripeCustomerID: "cus_stripe"}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inv := &mockInvoices{invoices: []domain.Invoice{pendingInvoice()}}
			engine := noPMEngine(t, inv)
			starter := &recordingDunningStarter{}
			engine.SetDunningStarter(starter)
			tc.setup(engine, inv)

			_, _ = engine.RetryPendingCharges(context.Background(), 10, nil)
			if len(starter.causes) != 0 {
				t.Fatalf("StartDunning called (%v); must not be", starter.causes)
			}
		})
	}
}
