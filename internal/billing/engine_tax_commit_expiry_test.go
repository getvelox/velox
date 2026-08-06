package billing

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/tax"
)

// fakeCalcStore is a minimal TaxCalculationWriter that returns a fixed
// created_at, so the CommitTax expiry guard can be exercised without postgres.
type fakeCalcStore struct {
	createdAt time.Time
	lookedUp  bool
}

func (f *fakeCalcStore) Record(_ context.Context, _, _ string, _ tax.Request, _ *tax.Result) (string, error) {
	return "taxcalc_fake", nil
}

func (f *fakeCalcStore) LinkInvoice(_ context.Context, _, _, _ string) error { return nil }

func (f *fakeCalcStore) LookupCalculationCreatedAt(_ context.Context, _, _, _ string) (time.Time, error) {
	f.lookedUp = true
	return f.createdAt, nil
}

// TestCommitTax_ExpiryGuardIgnoresSimulatedClock locks the fix for the
// clock-pinned proration orphan: a calc created seconds ago in REAL time must
// commit even when the request ctx carries a far-future test-clock
// effective-now. Pre-fix the guard used clock.Now(ctx) (the simulated clock),
// so a customer whose test clock was advanced >23h past wall-clock produced a
// false "tax calculation expired" — the commit was skipped and the proration
// invoice kept tax_calculation_id but never got tax_transaction_id.
func TestCommitTax_ExpiryGuardIgnoresSimulatedClock(t *testing.T) {
	store := &fakeCalcStore{createdAt: time.Now().UTC()} // created just now, real time
	e := &Engine{
		settings:     &taxSettings{provider: "stripe_tax"},
		taxProviders: stubResolver(&stubProvider{}), // Commit -> ("", nil): no SetTaxTransaction needed
		taxCalcStore: store,
	}
	// Simulate a customer pinned to a test clock advanced 30 days past real
	// wall-clock; clock.Now(ctx) would return this far-future time.
	ctx := clock.WithEffectiveNow(context.Background(), time.Now().UTC().Add(30*24*time.Hour))

	if err := e.CommitTax(ctx, "t1", "inv_1", "taxcalc_1"); err != nil {
		t.Fatalf("commit of a fresh calc must not be blocked by an advanced test clock: %v", err)
	}
	if !store.lookedUp {
		t.Fatal("expiry guard never ran (lookup not called) — assertion would be vacuous")
	}
}

// TestCommitTax_ExpiryGuardTripsOnRealAge is the over-correction guard: the
// fix must not disable expiry. A calc genuinely 30h old in real time must be
// rejected regardless of where the simulated clock sits.
func TestCommitTax_ExpiryGuardTripsOnRealAge(t *testing.T) {
	store := &fakeCalcStore{createdAt: time.Now().UTC().Add(-30 * time.Hour)} // real-stale
	e := &Engine{
		settings:     &taxSettings{provider: "stripe_tax"},
		taxProviders: stubResolver(&stubProvider{}),
		taxCalcStore: store,
	}
	// Simulated clock pinned to "now" — the calc is still genuinely expired.
	ctx := clock.WithEffectiveNow(context.Background(), time.Now().UTC())

	err := e.CommitTax(ctx, "t1", "inv_1", "taxcalc_1")
	if !errors.Is(err, errs.ErrInvalidState) {
		t.Fatalf("a genuinely expired calc must be rejected with ErrInvalidState, got %v", err)
	}
	if !strings.Contains(err.Error(), "expired") {
		t.Fatalf("want an expiry message, got %v", err)
	}
}

// TestCheckTaxCalculationUsable_DirectContract pins the standalone check —
// the method invoice.Service.Finalize consults BEFORE the state transition.
// Expired → InvalidState carrying the stable machine code; fresh → nil;
// uncertainty (unwired store / nil provider) → nil, because only a PROVEN
// expiry may block a finalize.
func TestCheckTaxCalculationUsable_DirectContract(t *testing.T) {
	t.Run("expired calc blocks with the stable code", func(t *testing.T) {
		e := &Engine{
			settings:     &taxSettings{provider: "stripe_tax"},
			taxProviders: stubResolver(&stubProvider{}),
			taxCalcStore: &fakeCalcStore{createdAt: time.Now().UTC().Add(-30 * time.Hour)},
		}
		err := e.CheckTaxCalculationUsable(context.Background(), "t1", "inv_1", "taxcalc_1")
		if !errors.Is(err, errs.ErrInvalidState) {
			t.Fatalf("want ErrInvalidState, got %v", err)
		}
		var de *errs.DomainError
		if !errors.As(err, &de) || de.Code != "tax_calculation_expired" {
			t.Fatalf("want stable code tax_calculation_expired, got %+v", err)
		}
		if !strings.Contains(err.Error(), "retry tax to refresh, then finalize") {
			t.Fatalf("the message must carry the operator remedy, got %v", err)
		}
	})
	t.Run("fresh calc passes", func(t *testing.T) {
		e := &Engine{
			settings:     &taxSettings{provider: "stripe_tax"},
			taxProviders: stubResolver(&stubProvider{}),
			taxCalcStore: &fakeCalcStore{createdAt: time.Now().UTC()},
		}
		if err := e.CheckTaxCalculationUsable(context.Background(), "t1", "inv_1", "taxcalc_1"); err != nil {
			t.Fatalf("fresh calc must pass: %v", err)
		}
	})
	t.Run("unwired store is uncertainty, not a block", func(t *testing.T) {
		e := &Engine{settings: &taxSettings{provider: "stripe_tax"}, taxProviders: stubResolver(&stubProvider{})}
		if err := e.CheckTaxCalculationUsable(context.Background(), "t1", "inv_1", "taxcalc_1"); err != nil {
			t.Fatalf("no calc store wired must pass: %v", err)
		}
	})
	t.Run("nil provider skips — an aged calc must not block a tenant whose commit would skip too", func(t *testing.T) {
		e := &Engine{
			settings:     &taxSettings{provider: "none"},
			taxProviders: stubResolver(nil),
			taxCalcStore: &fakeCalcStore{createdAt: time.Now().UTC().Add(-30 * time.Hour)},
		}
		if err := e.CheckTaxCalculationUsable(context.Background(), "t1", "inv_1", "taxcalc_1"); err != nil {
			t.Fatalf("nil provider must pass even with an aged calc: %v", err)
		}
	})
}
