package billing

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// mockCancelExecutor records FireDueScheduledCancel calls and applies the
// flip to the shared mockSubs map so the engine's scan stops returning the
// sub — mirroring the real executor's terminal effect (concurrent-resolver
// fake discipline: the fake resolves state the way production does).
type mockCancelExecutor struct {
	subs  *mockSubs
	calls []struct {
		id string
		at time.Time
	}
}

func (m *mockCancelExecutor) FireDueScheduledCancel(_ context.Context, _ string, id string, at time.Time) error {
	m.calls = append(m.calls, struct {
		id string
		at time.Time
	}{id, at})
	s := m.subs.subs[id]
	s.Status = domain.SubscriptionCanceled
	s.CanceledAt = &at
	s.CancelAt = nil
	m.subs.subs[id] = s
	return nil
}

// Dates sit relative to billingTestClock's frozen 2026-04-01: the yearly
// period surrounds it (2025-05-01 → 2026-05-01), so next_billing_at is in
// the clock's FUTURE while cancel_at (2025-05-16) is due — the exact
// motivating shape: due cancel, boundary not due.
func midPeriodCancelFixture(cancelAt time.Time) (*mockSubs, domain.Subscription) {
	periodStart := time.Date(2025, 5, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	sub := domain.Subscription{
		ID: "sub_mid", TenantID: "t1", CustomerID: "cus_1",
		Items:                     []domain.SubscriptionItem{{PlanID: "pln_1", Quantity: 1}},
		Status:                    domain.SubscriptionActive,
		BillingTime:               domain.BillingTimeCalendar,
		CurrentBillingPeriodStart: &periodStart,
		CurrentBillingPeriodEnd:   &periodEnd,
		NextBillingAt:             &periodEnd,
		CancelAt:                  &cancelAt,
	}
	return &mockSubs{
		subs:         map[string]domain.Subscription{"sub_mid": sub},
		cycleUpdated: make(map[string]bool),
	}, sub
}

func midPeriodEngine(subs *mockSubs) *Engine {
	pricing := &mockPricing{
		plans: map[string]domain.Plan{
			"pln_1": {ID: "pln_1", Name: "Annual", Currency: "USD",
				BillingInterval: domain.BillingYearly, BaseAmountCents: 29000},
		},
	}
	return wireBaseTax(NewEngine(subs, &mockUsage{totals: map[string]int64{}}, pricing, &mockInvoices{}, nil, &mockSettings{}, nil, nil, billingTestClock()))
}

// TestRunCycle_MidPeriodCancelAt_FiresViaExecutor is the ADR-097 regression
// for the FLOW TC8 live find: cancel_at strictly between boundaries (here 15
// days into a yearly period) was invisible to the scans and the sub renewed
// past its own cancellation. The engine must route it to the executor with
// the EXACT contracted instant, and must not also advance the cycle.
func TestRunCycle_MidPeriodCancelAt_FiresViaExecutor(t *testing.T) {
	// The engine's test clock reads wall now; pick a cancel_at safely in
	// the past relative to it so the arm is due.
	cancelAt := time.Date(2025, 5, 16, 0, 0, 0, 0, time.UTC)
	subs, _ := midPeriodCancelFixture(cancelAt)
	engine := midPeriodEngine(subs)
	exec := &mockCancelExecutor{subs: subs}
	engine.SetScheduledCancelExecutor(exec)

	if _, errs := engine.RunCycle(context.Background(), 50, nil); len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}

	if len(exec.calls) != 1 {
		t.Fatalf("executor calls: got %d, want exactly 1", len(exec.calls))
	}
	if !exec.calls[0].at.Equal(cancelAt) {
		t.Errorf("fired at %v, want the contracted cancel_at %v", exec.calls[0].at, cancelAt)
	}
	if subs.cycleUpdated["sub_mid"] {
		t.Error("cycle must NOT advance for a mid-period cancel fire")
	}
	if got := subs.subs["sub_mid"].Status; got != domain.SubscriptionCanceled {
		t.Errorf("status: got %q, want canceled", got)
	}
}

// TestRunCycle_CancelAtEqualsBoundary_TieGoesToBoundaryPath locks the strict
// Before: cancel_at == next_billing_at must ride the boundary close (bill
// the full period, then fire) and never reach the executor — with <=, both
// paths become eligible and the stub shares the cycle invoice's period key.
func TestRunCycle_CancelAtEqualsBoundary_TieGoesToBoundaryPath(t *testing.T) {
	periodStart := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	cancelAt := periodEnd
	subs := &mockSubs{
		subs: map[string]domain.Subscription{
			"sub_tie": {
				ID: "sub_tie", TenantID: "t1", CustomerID: "cus_1",
				Items:                     []domain.SubscriptionItem{{PlanID: "pln_1", Quantity: 1}},
				Status:                    domain.SubscriptionActive,
				BillingTime:               domain.BillingTimeCalendar,
				CurrentBillingPeriodStart: &periodStart,
				CurrentBillingPeriodEnd:   &periodEnd,
				NextBillingAt:             &periodEnd,
				CancelAt:                  &cancelAt,
			},
		},
		cycleUpdated: make(map[string]bool),
	}
	pricing := &mockPricing{
		plans: map[string]domain.Plan{
			"pln_1": {ID: "pln_1", Name: "Pro", Currency: "USD",
				BillingInterval: domain.BillingMonthly, BaseAmountCents: 4900},
		},
	}
	engine := wireBaseTax(NewEngine(subs, &mockUsage{totals: map[string]int64{}}, pricing, &mockInvoices{}, nil, &mockSettings{}, nil, nil, billingTestClock()))
	exec := &mockCancelExecutor{subs: subs}
	engine.SetScheduledCancelExecutor(exec)

	if _, errs := engine.RunCycle(context.Background(), 50, nil); len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(exec.calls) != 0 {
		t.Fatalf("executor must not run on a boundary tie; got %d calls", len(exec.calls))
	}
	if got := subs.subs["sub_tie"].Status; got != domain.SubscriptionCanceled {
		t.Errorf("boundary path should have fired the cancel: status %q", got)
	}
}

// TestRunCycle_MidPeriodCancel_NoExecutorFailsLoud: an unwired executor with
// a due mid-period cancel must surface an error (fail loud), not silently
// skip — a silent skip re-fetches the sub on every scan pass forever and
// keeps renewing it past its own cancellation.
func TestRunCycle_MidPeriodCancel_NoExecutorFailsLoud(t *testing.T) {
	cancelAt := time.Date(2025, 5, 16, 0, 0, 0, 0, time.UTC)
	subs, _ := midPeriodCancelFixture(cancelAt)
	engine := midPeriodEngine(subs)
	// deliberately NOT wiring the executor

	_, errs := engine.RunCycle(context.Background(), 50, nil)
	if len(errs) == 0 {
		t.Fatal("expected a loud error for a due mid-period cancel with no executor wired")
	}
	if !strings.Contains(errs[0].Error(), "no ScheduledCancelExecutor wired") {
		t.Errorf("error should name the missing executor; got: %v", errs[0])
	}
}
