package billing

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// fakeSweep returns canned (advanced, errs) and records call count.
type fakeSweep struct {
	advanced int
	errs     []error
	calls    int
}

func (f *fakeSweep) run(_ context.Context, _ int) (int, []error) {
	f.calls++
	return f.advanced, f.errs
}

// The three fakes implement the scheduler's retrier interfaces; each method
// appends its name to a shared order slice so reconcilers()'s ordering is
// observable.
type fakeTaxRetrier struct {
	retry, commit, reversal fakeSweep
	order                   *[]string
}

func (f *fakeTaxRetrier) RetryPendingTax(ctx context.Context, b int) (int, []error) {
	*f.order = append(*f.order, "tax_retry")
	return f.retry.run(ctx, b)
}
func (f *fakeTaxRetrier) RetryPendingTaxCommit(ctx context.Context, b int) (int, []error) {
	*f.order = append(*f.order, "tax_commit")
	return f.commit.run(ctx, b)
}
func (f *fakeTaxRetrier) RetryPendingTaxReversal(ctx context.Context, b int) (int, []error) {
	*f.order = append(*f.order, "tax_reversal")
	return f.reversal.run(ctx, b)
}

type fakeClawbackRetrier struct {
	issue, cnRev fakeSweep
	order        *[]string
}

func (f *fakeClawbackRetrier) RetryPendingClawbackIssue(ctx context.Context, b int) (int, []error) {
	*f.order = append(*f.order, "clawback_issue")
	return f.issue.run(ctx, b)
}
func (f *fakeClawbackRetrier) RetryPendingCreditNoteTaxReversal(ctx context.Context, b int) (int, []error) {
	*f.order = append(*f.order, "cn_tax_reversal")
	return f.cnRev.run(ctx, b)
}

type fakePaymentReconciler struct {
	sweep fakeSweep
	order *[]string
}

func (f *fakePaymentReconciler) Run(ctx context.Context, b int) (int, []error) {
	*f.order = append(*f.order, "payment_unknown")
	return f.sweep.run(ctx, b)
}

func (f *fakePaymentReconciler) RecoverChargeIntents(ctx context.Context, b int) (int, []error) {
	*f.order = append(*f.order, "charge_intent")
	return f.sweep.run(ctx, b)
}

// TestScheduler_ReconcilerOrder is the load-bearing guard: the recovery sweeps
// MUST run in this exact order (payment_unknown + tax before auto-charge), or a
// freshly-finalized invoice's finalize-time auto-charge can slip a tick. A
// silent slice reorder is a money-timing bug, so this asserts exact order, not
// membership.
func TestScheduler_ReconcilerOrder(t *testing.T) {
	order := []string{}
	s := &Scheduler{
		paymentReconciler: &fakePaymentReconciler{order: &order},
		taxRetrier:        &fakeTaxRetrier{order: &order},
		clawbackRetrier:   &fakeClawbackRetrier{order: &order},
		engine:            &Engine{}, // wires dunning_backfill (Name() only; never Reconcile'd here)
	}

	var got []string
	for _, r := range s.reconcilers() {
		got = append(got, r.Name())
	}
	// charge_intent runs FIRST — it names PaymentIntents payment_unknown cannot
	// see, and payment_unknown is the sweep that would otherwise settle those
	// invoices failed and free a retry to double-charge (ADR-106).
	// dunning_backfill runs LAST — an order-independent backstop on already-failed invoices.
	want := []string{"charge_intent", "payment_unknown", "tax_retry", "tax_commit", "tax_reversal", "clawback_issue", "cn_tax_reversal", "dunning_backfill"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("reconciler order:\n got %v\nwant %v", got, want)
	}
}

// TestScheduler_Reconcilers_NilDepsSkipped: an unwired retrier contributes no
// reconcilers (the driver's nil-skip is at the assembly layer).
func TestScheduler_Reconcilers_NilDepsSkipped(t *testing.T) {
	if rs := (&Scheduler{}).reconcilers(); len(rs) != 0 {
		t.Fatalf("all deps nil → no reconcilers, got %d", len(rs))
	}
	order := []string{}
	only := &Scheduler{taxRetrier: &fakeTaxRetrier{order: &order}}
	var names []string
	for _, r := range only.reconcilers() {
		names = append(names, r.Name())
	}
	if want := []string{"tax_retry", "tax_commit", "tax_reversal"}; !reflect.DeepEqual(names, want) {
		t.Errorf("only-tax-wired: got %v want %v", names, want)
	}
}

// TestRunReconcilers_OrderSkipErrorsMeterAlways: nil entries are skipped, a
// per-row error never aborts the batch, and the sweep metric fires once per
// reconciler — INCLUDING a 0-advanced/0-error run (liveness), so an operator can
// alert on a reconciler that stops running.
func TestRunReconcilers_OrderSkipErrorsMeterAlways(t *testing.T) {
	var metered []string
	orig := recordReconcilerSweep
	recordReconcilerSweep = func(name, _ string, _, _ int) { metered = append(metered, name) }
	defer func() { recordReconcilerSweep = orig }()

	var runOrder []string
	mk := func(name string, adv int, errs []error) Reconciler {
		return reconcilerFunc{name, func(_ context.Context, _ int) (int, []error) {
			runOrder = append(runOrder, name)
			return adv, errs
		}}
	}
	rs := []Reconciler{
		mk("a", 1, nil),
		nil,                                     // must be skipped, not panic
		mk("b", 0, []error{errors.New("boom")}), // error must not abort the rest
		mk("c", 0, nil),                         // zero/zero must STILL be metered
	}
	runReconcilers(context.Background(), "live", rs, 50)

	want := []string{"a", "b", "c"}
	if !reflect.DeepEqual(runOrder, want) {
		t.Errorf("run order/skip/no-abort: got %v want %v", runOrder, want)
	}
	if !reflect.DeepEqual(metered, want) {
		t.Errorf("metric must fire once per reconciler incl. the 0/0 one: got %v want %v", metered, want)
	}
}
