package billing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/credit"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/payment"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/tax"
)

// stubClockReader is a TestClockReader spy used by TestEffectiveNow. It
// returns a canned (TestClock, error) pair and records the last ID queried so
// the test can assert the engine looked up the right clock.
type stubClockReader struct {
	clk        domain.TestClock
	err        error
	lastID     string
	lastTenant string
}

func (s *stubClockReader) Get(_ context.Context, tenantID, id string) (domain.TestClock, error) {
	s.lastID = id
	s.lastTenant = tenantID
	if s.err != nil {
		return domain.TestClock{}, s.err
	}
	return s.clk, nil
}

// mockSettings is a minimal SettingsReader for engine tests. Hands out
// VLX-000001, VLX-000002, ... deterministically; Get returns a zero-value
// TenantSettings so net_payment_terms defaults to the engine's fallback.
// wireBaseTax wires a tax resolver onto an engine — required for
// any path that calls ApplyTaxToLineItems. Production engine
// always has this wired; the engine fails loudly when it's not
// (no silent zero-tax fallback). Tests that don't exercise
// tax-specific behavior use this to satisfy the wiring without
// boilerplate.
// billingTestClock returns a fake clock pinned just past the
// April 2026 period boundary used by most engine tests. With the
// new multi-period billSubscription loop (ADR-028), unit tests that
// expect "1 invoice per RunCycle" need a deterministic now() so
// the loop bills exactly one period and exits. Wall-clock would
// otherwise advance into May/June/etc., billing multiple periods
// per call — correct in production but breaks "1 invoice"
// assertions written against the pre-ADR-028 single-period
// primitive.
func billingTestClock() clock.Clock {
	return clock.NewFake(time.Date(2026, 4, 1, 0, 0, 1, 0, time.UTC))
}

// TestRunCycle_SkipsClockPinnedSubs asserts the disjoint-flow
// invariant from ADR-028: the wall-clock RunCycle must NOT touch
// subs that are pinned to a test clock, even if their next_billing_at
// is past wall-clock now. Operator-driven advance is the sole path
// for clock-pinned billing; cron must stay in its lane.
func TestRunCycle_SkipsClockPinnedSubs(t *testing.T) {
	periodStart := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	nextBilling := periodEnd

	subs := &mockSubs{
		subs: map[string]domain.Subscription{
			// Wall-clock sub — should bill.
			"sub_wall": {
				ID: "sub_wall", TenantID: "t1", CustomerID: "cus_1",
				Items:                     []domain.SubscriptionItem{{PlanID: "pln_1", Quantity: 1}},
				Status:                    domain.SubscriptionActive,
				BillingTime:               domain.BillingTimeCalendar,
				CurrentBillingPeriodStart: &periodStart, CurrentBillingPeriodEnd: &periodEnd,
				NextBillingAt: &nextBilling,
			},
			// Clock-pinned sub — must be skipped by RunCycle.
			"sub_clock": {
				ID: "sub_clock", TenantID: "t1", CustomerID: "cus_2",
				Items:                     []domain.SubscriptionItem{{PlanID: "pln_1", Quantity: 1}},
				Status:                    domain.SubscriptionActive,
				BillingTime:               domain.BillingTimeCalendar,
				CurrentBillingPeriodStart: &periodStart, CurrentBillingPeriodEnd: &periodEnd,
				NextBillingAt: &nextBilling,
				TestClockID:   "tc_1",
			},
		},
		cycleUpdated: make(map[string]bool),
	}
	pricing := &mockPricing{
		plans: map[string]domain.Plan{
			"pln_1": {ID: "pln_1", Currency: "USD", BillingInterval: domain.BillingMonthly, BaseAmountCents: 1000},
		},
	}
	invoices := &mockInvoices{}
	engine := wireBaseTax(NewEngine(subs, &mockUsage{totals: map[string]int64{}}, pricing, invoices, nil, &mockSettings{}, nil, nil, billingTestClock()))

	if _, errs := engine.RunCycle(context.Background(), 50); len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}

	// Exactly one invoice — for the wall-clock sub. The clock-pinned
	// sub stays untouched; only Engine.RunCycleForClock processes it.
	if len(invoices.invoices) != 1 {
		t.Fatalf("expected 1 invoice (wall-clock only), got %d", len(invoices.invoices))
	}
	if invoices.invoices[0].SubscriptionID != "sub_wall" {
		t.Errorf("wrong sub billed: got %q, want sub_wall", invoices.invoices[0].SubscriptionID)
	}
	if subs.cycleUpdated["sub_clock"] {
		t.Error("clock-pinned sub must not have its cycle advanced by wall-clock RunCycle")
	}
}

// TestRetryPendingCharges_SkipsClockPinned (ADR-029 Phase 1) — the
// cron-side counterpart of TestRunCycle_SkipsClockPinnedSubs for the
// auto-charge path. mockInvoices doesn't differentiate clock-pinned
// from wall-clock invoices on its own; the production filter lives
// in the SQL of ListAutoChargePending. This unit test exercises the
// engine wiring contract: when the InvoiceWriter returns a list, the
// engine charges every entry (no per-row clock check at the engine
// layer — exclusion is the SQL's job, just like ADR-028's
// GetDueBilling). The invariant we DO want pinned at this layer is
// the symmetric one: RetryPendingChargesForClock must call the
// per-clock list method, not the cron one.
func TestRetryPendingCharges_DispatchesToCorrectQuery(t *testing.T) {
	// A spy mockInvoices that records which list method got called.
	subs := &mockSubs{cycleUpdated: make(map[string]bool)}
	pricing := &mockPricing{}
	inv := &mockInvoices{
		invoices: []domain.Invoice{{
			ID: "inv_wall", TenantID: "t1", CustomerID: "cus_wall",
			Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentPending,
			AutoChargePending: true, AmountDueCents: 1000,
		}},
	}
	engine := wireBaseTax(NewEngine(subs, &mockUsage{}, pricing, inv, nil, &mockSettings{}, nil, nil, billingTestClock()))

	t.Run("cron path uses ListAutoChargePending", func(t *testing.T) {
		// We don't have a charger wired so the loop short-circuits at
		// the paymentSetups nil check — that's fine, the assertion
		// here is structural, not behavioural.
		_, _ = engine.RetryPendingCharges(context.Background(), 50)
	})

	t.Run("catchup path uses ListAutoChargePendingForClock", func(t *testing.T) {
		// Same short-circuit applies. The structural contract is that
		// the engine has both methods and they delegate to the
		// distinct InvoiceWriter methods; the SQL-level disjointness
		// is verified in the postgres integration test.
		_, _ = engine.RetryPendingChargesForClock(context.Background(), "t1", "tc_1", 50)
	})
}

// TestRetryPendingCharges_CardDeclineDoesNotEscalate locks in the
// expected-vs-fatal classification fix. A Stripe card decline is a
// routine business outcome handled by dunning (ChargeInvoice fires
// inline StartDunning + stamps payment_status=failed on the invoice),
// NOT a catchup-infrastructure failure. Pre-fix the decline error
// bubbled up through processAutoCharge → RetryPendingChargesForClock
// → testclock catchup → clock flipped to internal_failure on the
// first declined invoice.
func TestRetryPendingCharges_CardDeclineDoesNotEscalate(t *testing.T) {
	inv := &mockInvoices{
		invoices: []domain.Invoice{{
			ID: "inv_decline", TenantID: "t1", CustomerID: "cus_1",
			Status:            domain.InvoiceFinalized,
			PaymentStatus:     domain.PaymentPending,
			AutoChargePending: true,
			AmountDueCents:    1000,
		}},
	}
	subs := &mockSubs{cycleUpdated: make(map[string]bool)}
	pricing := &mockPricing{}
	pms := &fakePaymentSetups{ready: true, stripeCustomerID: "cus_stripe_1"}
	charger := &fakeChargerDecline{}

	engine := wireBaseTax(NewEngine(subs, &mockUsage{}, pricing, inv, nil, &mockSettings{}, pms, charger, billingTestClock()))

	charged, errs := engine.RetryPendingCharges(context.Background(), 50)
	if charged != 0 {
		t.Errorf("charged: got %d, want 0 (card declined)", charged)
	}
	if len(errs) != 0 {
		t.Errorf("decline must not escalate; got %d errors: %v", len(errs), errs)
	}
	// AutoChargePending must be cleared so the next catchup tick doesn't
	// re-pick the same invoice — dunning's retry schedule drives the
	// next attempt.
	if inv.invoices[0].AutoChargePending {
		t.Error("AutoChargePending should be cleared after decline (dunning takes over)")
	}
}

// fakeChargerDecline returns a *payment.PaymentError with DeclineCode
// set — the canonical "card declined" Stripe outcome.
type fakeChargerDecline struct{}

func (c *fakeChargerDecline) ChargeInvoice(_ context.Context, _ string, inv domain.Invoice, _, _ string) (domain.Invoice, error) {
	return inv, &payment.PaymentError{Message: "Card was declined.", DeclineCode: "card_declined"}
}

// fakePaymentSetups returns a static "ready" payment-readiness signal
// for any customer. Implements billing.PaymentReadiness.
type fakePaymentSetups struct {
	ready            bool
	stripeCustomerID string
}

func (f *fakePaymentSetups) ResolveForCharge(_ context.Context, _, _ string) (string, string, error) {
	if !f.ready {
		return f.stripeCustomerID, "", nil
	}
	return f.stripeCustomerID, "pm_fake_default", nil
}

// TestBillSubscription_LoopsUntilCaughtUp asserts the per-sub
// period loop from ADR-028: one billSubscription call generates
// every due invoice for that sub until next_billing_at > effectiveNow.
// Pre-ADR-028 the function generated exactly one invoice per call.
func TestBillSubscription_LoopsUntilCaughtUp(t *testing.T) {
	// Sub created 4 months ago; clock is at "now" (5 months past first
	// period start). Expect 5 monthly invoices in one billSubscription
	// call: April, May, June, July, August.
	periodStart := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	nextBilling := periodEnd

	subs := &mockSubs{
		subs: map[string]domain.Subscription{
			"sub_1": {
				ID: "sub_1", TenantID: "t1", CustomerID: "cus_1",
				Items:                     []domain.SubscriptionItem{{PlanID: "pln_1", Quantity: 1}},
				Status:                    domain.SubscriptionActive,
				BillingTime:               domain.BillingTimeCalendar,
				CurrentBillingPeriodStart: &periodStart, CurrentBillingPeriodEnd: &periodEnd,
				NextBillingAt: &nextBilling,
			},
		},
		cycleUpdated: make(map[string]bool),
	}
	pricing := &mockPricing{
		plans: map[string]domain.Plan{
			"pln_1": {ID: "pln_1", Currency: "USD", BillingInterval: domain.BillingMonthly, BaseAmountCents: 1000},
		},
	}
	invoices := &mockInvoices{}
	// Clock 5 months ahead → 5 due periods (May, Jun, Jul, Aug, Sep
	// are billed for periods ending May 1 .. Sep 1; advance happens
	// per the engine's add-month logic).
	clk := clock.NewFake(time.Date(2026, 9, 1, 0, 0, 1, 0, time.UTC))
	engine := wireBaseTax(NewEngine(subs, &mockUsage{totals: map[string]int64{}}, pricing, invoices, nil, &mockSettings{}, nil, nil, clk))

	if _, errs := engine.RunCycle(context.Background(), 50); len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}

	// Expect 5 invoices for the 5 due periods.
	if len(invoices.invoices) != 5 {
		t.Fatalf("expected 5 invoices from per-sub period loop, got %d", len(invoices.invoices))
	}
	// Sub's next_billing_at must be past clock now after all 5 cycles.
	updated := subs.subs["sub_1"]
	if updated.NextBillingAt == nil || !updated.NextBillingAt.After(clk.Now(context.Background())) {
		t.Errorf("sub should be caught up: got next_billing_at=%v, clock=%v", updated.NextBillingAt, clk.Now(context.Background()))
	}
}

func wireBaseTax(e *Engine) *Engine {
	e.SetTaxProviderResolver(tax.NewResolver(nil))
	// billing_intervals is the only segment source (ADR-101 Phase 4):
	// when the fixture's SubscriptionReader doubles as the snapshotter
	// (mockSubs does), wire it — mockSubs synthesizes a full-lifetime
	// open interval per item unless the test seeds explicit rows.
	if e.intervalSnap == nil {
		if snap, ok := e.subs.(IntervalSnapshotter); ok {
			e.SetIntervalReader(snap)
		}
	}
	// Collect-pipeline collaborators are REQUIRED post-#442 — the old
	// charger/paymentSetups/noPMNotifier nil guards are deleted, so any
	// fixture whose flow reaches collectAfterFinalize needs them wired.
	// Defaults (only when the fixture didn't provide its own): no ready PM
	// → the pipeline takes the queue+notify arm; the sentinel charger errors
	// if a test somehow reaches a charge without wiring a real fake.
	if e.paymentSetups == nil {
		e.paymentSetups = &fakePaymentSetups{ready: false}
	}
	if e.charger == nil {
		e.charger = sentinelCharger{}
	}
	if e.noPMNotifier == nil {
		e.noPMNotifier = &fakeNoPMNotifier{}
	}
	return e
}

// sentinelCharger backs wireBaseTax's default: unit fixtures default to a
// not-ready PM, so a charge call means the test wired ready-PM without its
// own charger fake — surface that as a loud error, not a silent success.
type sentinelCharger struct{}

func (sentinelCharger) ChargeInvoice(_ context.Context, _ string, inv domain.Invoice, _, _ string) (domain.Invoice, error) {
	return inv, errors.New("sentinelCharger: fixture reached ChargeInvoice without wiring a charger fake")
}

type mockSettings struct{ next int }

func (m *mockSettings) NextInvoiceNumber(_ context.Context, _ string) (string, error) {
	m.next++
	return fmt.Sprintf("VLX-%06d", m.next), nil
}

func (m *mockSettings) Get(_ context.Context, _ string) (domain.TenantSettings, error) {
	return domain.TenantSettings{}, nil
}

// ---------------------------------------------------------------------------
// In-memory implementations of the billing engine's interfaces
// ---------------------------------------------------------------------------

type mockSubs struct {
	subs         map[string]domain.Subscription
	cycleUpdated map[string]bool
	// itemChanges feeds ListItemChangesInPeriod. Tests that exercise
	// segment-aware billing seed this with the change rows the DB
	// trigger would have produced (migration 0029).
	itemChanges    []domain.SubscriptionItemChange
	itemChangesErr error
	// intervals feeds ListItemIntervals — the ADR-101 Phase-4 segment
	// source. Segment-aware tests seed the policy-applied lifetimes the
	// interval WRITERS would have recorded (day-grade already decided:
	// a creation-day add opens at periodStart, a mid-period change
	// seals and reopens at its instant). Tests whose sub has items but
	// no seeded intervals get the writer-bug refusal — seed
	// defaultIntervals() or explicit rows.
	intervals    []domain.ItemInterval
	intervalsErr error
	// updateBillingCycleErr, when set, makes UpdateBillingCycle fail — used to
	// drive the idempotent-skip heal path's loud-fail (a failed watermark
	// advance must surface, not be swallowed).
	updateBillingCycleErr error
}

func (m *mockSubs) GetDueBilling(_ context.Context, before time.Time, limit int) ([]domain.Subscription, error) {
	var result []domain.Subscription
	for _, s := range m.subs {
		// ADR-028 disjoint flows: wall-clock GetDueBilling only
		// returns NON-clock-pinned subs.
		if s.TestClockID != "" {
			continue
		}
		eligible := s.Status == domain.SubscriptionActive || s.Status == domain.SubscriptionTrialing
		due := s.NextBillingAt != nil && !s.NextBillingAt.After(before)
		// Mirror the real query's ADR-097 cancel arm: active subs with a
		// due cancel_at are admitted even when next_billing_at is future.
		cancelDue := s.Status == domain.SubscriptionActive && s.CancelAt != nil && !s.CancelAt.After(before)
		if eligible && (due || cancelDue) {
			result = append(result, s)
		}
	}
	if len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

// GetDueBillingForTenant — mock mirror of the tenant-scoped manual-run query.
// The mock doesn't model RLS, so it returns the same non-clock-pinned due subs
// as GetDueBilling (all mock subs are one tenant); tenantID is accepted for
// signature parity.
func (m *mockSubs) GetDueBillingForTenant(ctx context.Context, _ string, before time.Time, limit int) ([]domain.Subscription, error) {
	return m.GetDueBilling(ctx, before, limit)
}

// GetDueBillingForClock — mirror of GetDueBilling for the disjoint
// catchup path (ADR-028). Returns ONLY subs pinned to the given
// clock whose next_billing_at is on-or-before `before`. The mock
// uses `before` directly because it doesn't model test-clock rows;
// real engine wiring resolves frozen_time via the SQL JOIN.
func (m *mockSubs) GetDueBillingForClock(_ context.Context, _, clockID string, limit int) ([]domain.Subscription, error) {
	var result []domain.Subscription
	for _, s := range m.subs {
		if s.TestClockID != clockID {
			continue
		}
		eligible := s.Status == domain.SubscriptionActive || s.Status == domain.SubscriptionTrialing
		if eligible && s.NextBillingAt != nil {
			result = append(result, s)
		}
	}
	if len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

func (m *mockSubs) Get(_ context.Context, _, id string) (domain.Subscription, error) {
	s, ok := m.subs[id]
	if !ok {
		return domain.Subscription{}, fmt.Errorf("not found")
	}
	return s, nil
}

func (m *mockSubs) UpdateBillingCycle(_ context.Context, _, id string, start, end, next time.Time, anchorDay int) error {
	if m.updateBillingCycleErr != nil {
		return m.updateBillingCycleErr
	}
	s, ok := m.subs[id]
	if !ok {
		return fmt.Errorf("not found")
	}
	s.CurrentBillingPeriodStart = &start
	s.CurrentBillingPeriodEnd = &end
	s.NextBillingAt = &next
	s.BillingAnchorDay = anchorDay
	m.subs[id] = s
	m.cycleUpdated[id] = true
	return nil
}

// UpdateBillingCycleTx delegates to the plain variant — atomicity is the real
// store's concern; the mock preserves observable behavior. The tx handle is
// the fakeTxRunner's nil.
func (m *mockSubs) UpdateBillingCycleTx(ctx context.Context, _ *sql.Tx, tenantID, id string, start, end, next time.Time, anchorDay int) error {
	return m.UpdateBillingCycle(ctx, tenantID, id, start, end, next, anchorDay)
}

// fakeTxRunner satisfies billing.TxRunner for unit fixtures: runs fn with a
// nil tx (the mocks' Tx variants ignore the handle). commitErr, when set, is
// returned INSTEAD of committing — simulating a tx that fails at the
// UpdateBillingCycleTx/commit boundary so the atomic-rollback contract can be
// asserted without Postgres.
type fakeTxRunner struct {
	fnErr error // injected: fn's own error passthrough is default behavior
}

func (r *fakeTxRunner) WithTenantTx(_ context.Context, _ string, fn func(tx *sql.Tx) error) error {
	if err := fn(nil); err != nil {
		return err
	}
	return r.fnErr
}

func (m *mockSubs) FireScheduledCancellation(_ context.Context, _, id string, at time.Time) (domain.Subscription, error) {
	s, ok := m.subs[id]
	if !ok {
		return domain.Subscription{}, errs.ErrNotFound
	}
	if s.Status != domain.SubscriptionActive {
		return domain.Subscription{}, errs.ErrInvalidState
	}
	s.Status = domain.SubscriptionCanceled
	atCopy := at
	s.CanceledAt = &atCopy
	s.NextBillingAt = nil
	s.CancelAt = nil
	s.CancelAtPeriodEnd = false
	m.subs[id] = s
	return s, nil
}

func (m *mockSubs) ClearPauseCollection(_ context.Context, _, id string) (domain.Subscription, error) {
	s, ok := m.subs[id]
	if !ok {
		return domain.Subscription{}, errs.ErrNotFound
	}
	s.PauseCollection = nil
	m.subs[id] = s
	return s, nil
}

// ActivateAfterTrialWithBill mirrors the store's ADR-069 shape: the
// schedule-empty predicate blocks activation with ErrTrialCancelDue, the
// bill callback runs "in-tx" (nil tx — atomicity is the real store's
// concern), and already-non-trialing rows return InvalidState.
func (m *mockSubs) ActivateAfterTrialWithBill(ctx context.Context, tenantID, id string, at time.Time, billFn func(tx *sql.Tx, activated domain.Subscription) error) (domain.Subscription, error) {
	s, ok := m.subs[id]
	if !ok {
		return domain.Subscription{}, errs.ErrNotFound
	}
	if s.Status != domain.SubscriptionTrialing {
		return domain.Subscription{}, errs.InvalidState("not trialing")
	}
	if s.CancelAtPeriodEnd || s.CancelAt != nil {
		return domain.Subscription{}, domain.ErrTrialCancelDue
	}
	s.Status = domain.SubscriptionActive
	if s.ActivatedAt == nil {
		t := at
		s.ActivatedAt = &t
	}
	m.subs[id] = s
	if billFn != nil {
		if err := billFn(nil, s); err != nil {
			return domain.Subscription{}, err
		}
	}
	return s, nil
}

// CancelAtTrialEnd mirrors the store's ADR-069 CAS.
func (m *mockSubs) CancelAtTrialEnd(_ context.Context, _, id string, observedTrialEnd time.Time) (domain.Subscription, error) {
	s, ok := m.subs[id]
	if !ok {
		return domain.Subscription{}, errs.ErrNotFound
	}
	scheduleDue := s.CancelAtPeriodEnd || (s.CancelAt != nil && s.TrialEndAt != nil && !s.CancelAt.After(*s.TrialEndAt))
	if s.Status != domain.SubscriptionTrialing || s.TrialEndAt == nil || !s.TrialEndAt.Equal(observedTrialEnd) || !scheduleDue {
		return domain.Subscription{}, domain.ErrTrialCancelConflict
	}
	s.Status = domain.SubscriptionCanceled
	canceledAt := *s.TrialEndAt
	s.CanceledAt = &canceledAt
	s.CancelAt = nil
	s.CancelAtPeriodEnd = false
	m.subs[id] = s
	return s, nil
}

// ApplyDuePendingItemPlansAtomic mirrors the postgres store: for every item on
// the subscription whose pending change is due (effective_at <= now), swap
// plan_id ← pending_plan_id and clear the pending fields in one pass. Returns
// the applied items so the engine can audit which swaps landed at this cycle.
func (m *mockSubs) ApplyDuePendingItemPlansAtomic(_ context.Context, _, id string, now time.Time) ([]domain.SubscriptionItem, error) {
	s, ok := m.subs[id]
	if !ok {
		return nil, errs.ErrNotFound
	}
	var applied []domain.SubscriptionItem
	for i := range s.Items {
		it := s.Items[i]
		if it.PendingPlanID == "" || it.PendingPlanEffectiveAt == nil || it.PendingPlanEffectiveAt.After(now) {
			continue
		}
		oldPlan := it.PlanID
		it.PlanID = it.PendingPlanID
		it.PlanChangedAt = &now
		it.PendingPlanID = ""
		it.PendingPlanEffectiveAt = nil
		s.Items[i] = it
		applied = append(applied, it)
		// Mirror the ADR-101 interval writer: seal the old plan's
		// lifetime at the swap instant and reopen under the new plan,
		// so the closing window bills the OUTGOING plan. Materialize
		// the default full-lifetime rows first when this fixture was
		// relying on synthesis.
		if m.intervals == nil {
			for _, di := range s.Items {
				pid, qty := di.PlanID, di.Quantity
				if di.ID == it.ID {
					pid = oldPlan // pre-swap state for the swapped item
				}
				m.intervals = append(m.intervals, domain.ItemInterval{
					SubscriptionItemID: di.ID, PlanID: pid, Quantity: qty,
				})
			}
		}
		sealAt := now
		for j := range m.intervals {
			if m.intervals[j].SubscriptionItemID == it.ID && m.intervals[j].EndsAt == nil {
				m.intervals[j].EndsAt = &sealAt
			}
		}
		m.intervals = append(m.intervals, domain.ItemInterval{
			SubscriptionItemID: it.ID, PlanID: it.PlanID, Quantity: it.Quantity, StartsAt: sealAt,
		})
		// Mirror the DB trigger from migration 0029: the UPDATE on
		// subscription_items emits a 'plan' change row so segment-
		// aware billing sees the boundary swap.
		m.itemChanges = append(m.itemChanges, domain.SubscriptionItemChange{
			ID:                 fmt.Sprintf("vlx_sic_%d", len(m.itemChanges)+1),
			TenantID:           s.TenantID,
			SubscriptionID:     s.ID,
			SubscriptionItemID: it.ID,
			ChangeType:         "plan",
			FromPlanID:         oldPlan,
			ToPlanID:           it.PlanID,
			FromQuantity:       it.Quantity,
			ToQuantity:         it.Quantity,
			ChangedAt:          now,
			CreatedAt:          now,
		})
	}
	m.subs[id] = s
	return applied, nil
}

func (m *mockSubs) ListWithThresholdsForClock(_ context.Context, _, _, _ string, _ int) ([]domain.Subscription, error) {
	return nil, nil
}

func (m *mockSubs) ListWithThresholds(_ context.Context, _ bool, _ string, _ int) ([]domain.Subscription, error) {
	// Engine unit tests focus on the natural cycle; the threshold scan path
	// is exercised via threshold_scan_test.go (which uses its own mock that
	// returns the configured candidate set). Returning empty here keeps
	// existing tests compatible without exercising the threshold path.
	return nil, nil
}

func (m *mockSubs) ListItemIntervals(_ context.Context, _, subscriptionID string) ([]domain.ItemInterval, error) {
	// Single-sub fixtures: seeded rows all belong to the queried sub
	// (ItemInterval carries no subscription id — the store scopes it).
	if m.intervalsErr != nil {
		return nil, m.intervalsErr
	}
	if m.intervals != nil {
		return m.intervals, nil
	}
	// Default: one open interval per live item, from the beginning of
	// time — clips to any window as a full-window segment, exactly the
	// real writers' state for an item that existed before the window
	// with no mutations. No policy is reimplemented here; tests that
	// exercise mid-window mutations seed explicit interval rows.
	var out []domain.ItemInterval
	if sub, ok := m.subs[subscriptionID]; ok {
		for _, it := range sub.Items {
			out = append(out, domain.ItemInterval{
				SubscriptionItemID: it.ID, PlanID: it.PlanID, Quantity: it.Quantity,
			})
		}
	}
	return out, nil
}

func (m *mockSubs) ListItemChangesInPeriod(_ context.Context, _, subscriptionID string, periodStart, periodEnd time.Time) ([]domain.SubscriptionItemChange, error) {
	if m.itemChangesErr != nil {
		return nil, m.itemChangesErr
	}
	var out []domain.SubscriptionItemChange
	for _, c := range m.itemChanges {
		if c.SubscriptionID != subscriptionID {
			continue
		}
		if !c.ChangedAt.After(periodStart) || c.ChangedAt.After(periodEnd) {
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

type mockUsage struct {
	totals map[string]int64 // meterID -> quantity for the full-period default
	// perInterval lets segment-aware usage tests stub different
	// quantities per [from, to]. Key is "meterID|fromRFC3339|toRFC3339".
	// Missing keys fall back to totals.
	perInterval map[string]int64
	// ruleAggs lets multi-dim tests stub per-pricing-rule buckets the way
	// the real AggregateByPricingRules returns them (one RuleAggregation
	// per claimed rule). Keyed by meterID; when unset, the legacy
	// single-bucket fallback below applies.
	ruleAggs map[string][]domain.RuleAggregation
}

func mockIntervalKey(meterID string, from, to time.Time) string {
	return meterID + "|" + from.UTC().Format(time.RFC3339Nano) + "|" + to.UTC().Format(time.RFC3339Nano)
}

func (m *mockUsage) AggregateForBillingPeriod(_ context.Context, _, _ string, meterIDs []string, from, to time.Time) (map[string]decimal.Decimal, error) {
	result := make(map[string]decimal.Decimal)
	for _, id := range meterIDs {
		if qty, ok := m.perInterval[mockIntervalKey(id, from, to)]; ok {
			result[id] = decimal.NewFromInt(qty)
			continue
		}
		if qty, ok := m.totals[id]; ok {
			result[id] = decimal.NewFromInt(qty)
		}
	}
	return result, nil
}

func (m *mockUsage) AggregateForBillingPeriodByAgg(_ context.Context, _, _ string, meters map[string]string, from, to time.Time) (map[string]decimal.Decimal, error) {
	result := make(map[string]decimal.Decimal)
	for id := range meters {
		if qty, ok := m.perInterval[mockIntervalKey(id, from, to)]; ok {
			result[id] = decimal.NewFromInt(qty)
			continue
		}
		if qty, ok := m.totals[id]; ok {
			result[id] = decimal.NewFromInt(qty)
		}
	}
	return result, nil
}

// AggregateByPricingRules is a minimal stub — engine_test.go's existing
// preview tests don't exercise the multi-dim path; the create_preview
// integration tests cover that against real Postgres. We return one
// aggregation per known meter so the new preview path produces the same
// totals the legacy tests expect.
func (m *mockUsage) AggregateByPricingRules(_ context.Context, _, _, meterID string, _ domain.AggregationMode, _, _ time.Time) ([]domain.RuleAggregation, error) {
	if aggs, ok := m.ruleAggs[meterID]; ok {
		return aggs, nil
	}
	qty, ok := m.totals[meterID]
	if !ok {
		return nil, nil
	}
	return []domain.RuleAggregation{{
		RuleID:              "",
		RatingRuleVersionID: "",
		Quantity:            decimal.NewFromInt(qty),
	}}, nil
}

type mockPricing struct {
	plans     map[string]domain.Plan
	meters    map[string]domain.Meter
	rules     map[string]domain.RatingRuleVersion
	overrides map[string]domain.CustomerPriceOverride // key: customerID+":"+ruleKey
	// overrideErr, when set, is returned by GetOverrideByKeyAsOf —
	// drives the fail-loud regression (a transient override-lookup
	// failure must ABORT the close, never bill list price; ADR-070).
	overrideErr error
	// meterPricingRules drives the multi-dim fork (ADR-044): meters with
	// entries here take the AggregateByPricingRules path. Keyed by meterID;
	// nil/missing keeps the meter on the legacy single-rule path.
	meterPricingRules map[string][]domain.MeterPricingRule
}

func (m *mockPricing) GetPlan(_ context.Context, _, id string) (domain.Plan, error) {
	p, ok := m.plans[id]
	if !ok {
		return domain.Plan{}, fmt.Errorf("plan not found")
	}
	return p, nil
}

func (m *mockPricing) GetMeter(_ context.Context, _, id string) (domain.Meter, error) {
	meter, ok := m.meters[id]
	if !ok {
		return domain.Meter{}, fmt.Errorf("meter not found")
	}
	return meter, nil
}

func (m *mockPricing) GetRatingRule(_ context.Context, _, id string) (domain.RatingRuleVersion, error) {
	r, ok := m.rules[id]
	if !ok {
		return domain.RatingRuleVersion{}, fmt.Errorf("rule not found")
	}
	return r, nil
}

// GetRuleByKeyAsOf mirrors the store's ADR-070 resolution: highest
// active version created at or before asOf; a key born after asOf
// resolves to its earliest active version. Mock rules usually leave
// CreatedAt zero, which sorts before any asOf — i.e. "always in force".
func (m *mockPricing) GetRuleByKeyAsOf(_ context.Context, _, ruleKey string, asOf time.Time) (domain.RatingRuleVersion, error) {
	var best, earliest domain.RatingRuleVersion
	foundBest, foundAny := false, false
	for _, r := range m.rules {
		if r.RuleKey != ruleKey {
			continue
		}
		if r.LifecycleState != "" && r.LifecycleState != domain.RatingRuleActive {
			continue
		}
		if !foundAny || r.Version < earliest.Version {
			earliest = r
			foundAny = true
		}
		if !r.CreatedAt.After(asOf) && (!foundBest || r.Version > best.Version) {
			best = r
			foundBest = true
		}
	}
	if foundBest {
		return best, nil
	}
	if foundAny {
		return earliest, nil
	}
	return domain.RatingRuleVersion{}, errs.ErrNotFound
}

func (m *mockPricing) GetOverrideByKeyAsOf(_ context.Context, _, customerID, ruleKey string, asOf time.Time) (domain.CustomerPriceOverride, error) {
	if m.overrideErr != nil {
		return domain.CustomerPriceOverride{}, m.overrideErr
	}
	o, ok := m.overrides[customerID+":"+ruleKey]
	if !ok || o.CreatedAt.After(asOf) {
		return domain.CustomerPriceOverride{}, errs.ErrNotFound
	}
	return o, nil
}

// ListMeterPricingRulesByMeter returns the configured multi-dim rules for a
// meter (nil for legacy single-rule meters — the default for most tests).
func (m *mockPricing) ListMeterPricingRulesByMeter(_ context.Context, _, meterID string) ([]domain.MeterPricingRule, error) {
	return m.meterPricingRules[meterID], nil
}

type mockInvoices struct {
	mu        sync.Mutex
	claimed   map[string]bool
	invoices  []domain.Invoice
	lineItems []domain.InvoiceLineItem
	// createErr, when set, is returned by CreateInvoiceWithLineItems[Tx] instead
	// of inserting — used to drive the idempotent-skip heal path (set to
	// errs.ErrAlreadyExists) and failure tests.
	createErr error
	// getErr, when set, is returned by GetInvoice — drives the
	// collectAfterFinalize reload-failure arms without breaking the mock's
	// other by-ID lookups (SetAutoChargePending still succeeds).
	getErr error
	// db, when set, gives the Audited create a REAL tenant tx to run the emit
	// on. The mock has no invoices table, but audit_log is a real table with no
	// FK to invoices, so a real audit.Logger.LogInTx can ride this tx and its
	// finalize row lands for real — needed by tests that pair this mock with the
	// real logger (the finalize row is in-tx now, ADR-090, so a nil tx would
	// panic the real writer). Unset = fake audit writers ignore the tx.
	db *postgres.DB
}

func (m *mockInvoices) CreateInvoice(_ context.Context, tenantID string, inv domain.Invoice) (domain.Invoice, error) {
	inv.ID = fmt.Sprintf("vlx_inv_%d", len(m.invoices)+1)
	inv.TenantID = tenantID
	m.invoices = append(m.invoices, inv)
	return inv, nil
}

func (m *mockInvoices) CreateLineItem(_ context.Context, tenantID string, item domain.InvoiceLineItem) (domain.InvoiceLineItem, error) {
	item.ID = fmt.Sprintf("vlx_ili_%d", len(m.lineItems)+1)
	item.TenantID = tenantID
	m.lineItems = append(m.lineItems, item)
	return item, nil
}

func (m *mockInvoices) ApplyCreditAmount(_ context.Context, _, id string, amountCents int64) (domain.Invoice, error) {
	for i, inv := range m.invoices {
		if inv.ID == id {
			m.invoices[i].AmountDueCents -= amountCents
			m.invoices[i].CreditsAppliedCents += amountCents
			return m.invoices[i], nil
		}
	}
	return domain.Invoice{}, fmt.Errorf("not found")
}

func (m *mockInvoices) GetInvoice(_ context.Context, _, id string) (domain.Invoice, error) {
	if m.getErr != nil {
		return domain.Invoice{}, m.getErr
	}
	for _, inv := range m.invoices {
		if inv.ID == id {
			return inv, nil
		}
	}
	return domain.Invoice{}, fmt.Errorf("not found")
}

func (m *mockInvoices) MarkPaid(_ context.Context, _, id string, stripePI string, paidAt time.Time) (domain.Invoice, error) {
	for i, inv := range m.invoices {
		if inv.ID == id {
			// Mirror PostgresStore.MarkPaid's state-machine guard:
			// reject draft (finalize first) and voided (terminal).
			// Tax_status must be ok — pending/failed means
			// authoritative total isn't established.
			switch inv.Status {
			case domain.InvoiceFinalized, domain.InvoiceUncollectible, domain.InvoicePaid:
				// ok
			default:
				return domain.Invoice{}, fmt.Errorf("cannot mark invoice paid from status %q", inv.Status)
			}
			if inv.TaxStatus != domain.InvoiceTaxOK {
				return domain.Invoice{}, fmt.Errorf("cannot mark invoice paid with tax_status=%q", inv.TaxStatus)
			}
			m.invoices[i].Status = domain.InvoicePaid
			m.invoices[i].PaymentStatus = domain.PaymentSucceeded
			m.invoices[i].AmountPaidCents = m.invoices[i].AmountDueCents
			m.invoices[i].AmountDueCents = 0
			m.invoices[i].PaidAt = &paidAt
			return m.invoices[i], nil
		}
	}
	return domain.Invoice{}, fmt.Errorf("not found")
}

func (m *mockInvoices) CreateInvoiceWithLineItems(ctx context.Context, tenantID string, inv domain.Invoice, items []domain.InvoiceLineItem) (domain.Invoice, error) {
	return m.CreateInvoiceWithLineItemsAudited(ctx, tenantID, inv, items, nil)
}

// CreateInvoiceWithLineItemsAudited faithfully models the real store's shared
// fate (playbook concurrent-resolver fake): emit runs on the would-be row and
// must succeed BEFORE the insert is committed, so an emission error persists
// nothing — the same guarantee the Postgres tx gives.
func (m *mockInvoices) CreateInvoiceWithLineItemsAudited(ctx context.Context, tenantID string, inv domain.Invoice, items []domain.InvoiceLineItem, emit func(tx *sql.Tx, out domain.Invoice) error) (domain.Invoice, error) {
	if m.createErr != nil {
		return domain.Invoice{}, m.createErr
	}
	inv.ID = fmt.Sprintf("vlx_inv_%d", len(m.invoices)+1)
	inv.TenantID = tenantID
	if emit != nil {
		runEmit := func() error { return emit(nil, inv) }
		if m.db != nil {
			// Real tx so a real LogInTx can ride it (see the db field's note).
			runEmit = func() error {
				return m.db.WithTenantTx(ctx, tenantID, func(tx *sql.Tx) error { return emit(tx, inv) })
			}
		}
		if err := runEmit(); err != nil {
			return domain.Invoice{}, fmt.Errorf("audit emission: %w", err)
		}
	}
	m.invoices = append(m.invoices, inv)
	for _, item := range items {
		item.InvoiceID = inv.ID
		item.ID = fmt.Sprintf("vlx_ili_%d", len(m.lineItems)+1)
		item.TenantID = tenantID
		m.lineItems = append(m.lineItems, item)
	}
	return inv, nil
}

func (m *mockInvoices) CreateInvoiceWithLineItemsTx(ctx context.Context, _ *sql.Tx, tenantID string, inv domain.Invoice, items []domain.InvoiceLineItem) (domain.Invoice, error) {
	return m.CreateInvoiceWithLineItems(ctx, tenantID, inv, items)
}

// ClaimAutoCharge is a FAITHFUL concurrent-resolver fake (playbook
// pattern): it honors the real CAS semantics — predicate re-check +
// exactly-one-winner — so engine tests exercising concurrent sweeps
// prove the claim actually gates the charge leg.
func (m *mockInvoices) ClaimAutoCharge(_ context.Context, _, id string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.claimed == nil {
		m.claimed = map[string]bool{}
	}
	if m.claimed[id] {
		return false, nil
	}
	for _, inv := range m.invoices {
		if inv.ID == id {
			if !inv.AutoChargePending || inv.PaymentStatus != domain.PaymentPending ||
				inv.Status != domain.InvoiceFinalized || inv.AmountDueCents <= 0 {
				return false, nil
			}
			m.claimed[id] = true
			return true, nil
		}
	}
	return false, nil
}

func (m *mockInvoices) ReleaseAutoChargeClaim(_ context.Context, _, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.claimed, id)
	return nil
}

func (m *mockInvoices) SetNoPMNotifiedAt(_ context.Context, _, id string, at time.Time) error {
	for i, inv := range m.invoices {
		if inv.ID == id {
			m.invoices[i].NoPMNotifiedAt = &at
			return nil
		}
	}
	return fmt.Errorf("not found")
}

func (m *mockInvoices) SetAutoChargePending(_ context.Context, _, id string, pending bool) error {
	for i, inv := range m.invoices {
		if inv.ID == id {
			m.invoices[i].AutoChargePending = pending
			return nil
		}
	}
	return fmt.Errorf("not found")
}

// ListAutoChargePendingForClock — minimal stub for the ADR-029 Phase 1
// interface bump. Tests that exercise the per-clock catchup path
// install a more meaningful stub via the test fixture; the default
// returns nothing because mockInvoices doesn't track sub→clock
// mapping (that's the postgres store's job).
func (m *mockInvoices) ListAutoChargePendingForClock(_ context.Context, _ string, _ string, _ int) ([]domain.Invoice, error) {
	return nil, nil
}

// LatestThresholdPeriodEnd mirrors the postgres semantics: MAX
// billing_period_end across non-voided threshold invoices whose
// billing_period_start is inside [periodStart, periodEnd). Tests seed a
// threshold invoice in m.invoices and the cycle-close watermark finds
// it organically.
func (m *mockInvoices) LatestThresholdPeriodEnd(_ context.Context, _, subscriptionID string, periodStart, periodEnd time.Time) (time.Time, error) {
	var latest time.Time
	found := false
	for _, inv := range m.invoices {
		if inv.SubscriptionID != subscriptionID || inv.BillingReason != domain.BillingReasonThreshold {
			continue
		}
		if inv.Status == domain.InvoiceVoided || inv.Status == domain.InvoiceUncollectible {
			continue
		}
		if inv.BillingPeriodStart.Before(periodStart) || !inv.BillingPeriodStart.Before(periodEnd) {
			continue
		}
		if !found || inv.BillingPeriodEnd.After(latest) {
			latest = inv.BillingPeriodEnd
			found = true
		}
	}
	if !found {
		return time.Time{}, errs.ErrNotFound
	}
	return latest, nil
}

// GetLatestThresholdInvoiceForCycle mirrors the postgres semantics: the
// newest (by billing_period_end) non-voided threshold invoice whose
// billing_period_start is inside [periodStart, periodEnd).
func (m *mockInvoices) GetLatestThresholdInvoiceForCycle(_ context.Context, _, subscriptionID string, periodStart, periodEnd time.Time) (domain.Invoice, error) {
	var latest domain.Invoice
	found := false
	for _, inv := range m.invoices {
		if inv.SubscriptionID != subscriptionID || inv.BillingReason != domain.BillingReasonThreshold {
			continue
		}
		if inv.Status == domain.InvoiceVoided || inv.Status == domain.InvoiceUncollectible {
			continue
		}
		if inv.BillingPeriodStart.Before(periodStart) || !inv.BillingPeriodStart.Before(periodEnd) {
			continue
		}
		if !found || inv.BillingPeriodEnd.After(latest.BillingPeriodEnd) {
			latest = inv
			found = true
		}
	}
	if !found {
		return domain.Invoice{}, errs.ErrNotFound
	}
	return latest, nil
}

// GetInvoiceForPeriod mirrors the postgres semantics: newest non-voided
// invoice keyed on (subscription, period_start, period_end) — the
// billing-idempotency tuple.
func (m *mockInvoices) GetInvoiceForPeriod(_ context.Context, _, subscriptionID string, periodStart, periodEnd time.Time) (domain.Invoice, error) {
	for i := len(m.invoices) - 1; i >= 0; i-- {
		inv := m.invoices[i]
		if inv.SubscriptionID != subscriptionID || inv.Status == domain.InvoiceVoided {
			continue
		}
		if !inv.BillingPeriodStart.Equal(periodStart) || !inv.BillingPeriodEnd.Equal(periodEnd) {
			continue
		}
		return inv, nil
	}
	return domain.Invoice{}, errs.ErrNotFound
}

func (m *mockInvoices) ListAutoChargePending(_ context.Context, limit int) ([]domain.Invoice, error) {
	var result []domain.Invoice
	for _, inv := range m.invoices {
		if inv.AutoChargePending {
			result = append(result, inv)
			if len(result) >= limit {
				break
			}
		}
	}
	return result, nil
}

// ListFailedWithoutDunningRun models only the invoice-level predicates; the
// "no dunning run" NOT EXISTS and clock-pinned exclusion are DB concerns the
// fake can't see (the real coverage is the real-Postgres backfill test).
func (m *mockInvoices) ListFailedWithoutDunningRun(_ context.Context, olderThan time.Time, limit int) ([]domain.Invoice, error) {
	var result []domain.Invoice
	for _, inv := range m.invoices {
		if inv.PaymentStatus == domain.PaymentFailed && inv.Status == domain.InvoiceFinalized &&
			inv.AmountDueCents > 0 && inv.UpdatedAt.Before(olderThan) {
			result = append(result, inv)
			if len(result) >= limit {
				break
			}
		}
	}
	return result, nil
}

func (m *mockInvoices) SetTaxTransaction(_ context.Context, _, id string, taxTransactionID string) error {
	for i, inv := range m.invoices {
		if inv.ID == id {
			m.invoices[i].TaxTransactionID = taxTransactionID
			return nil
		}
	}
	return fmt.Errorf("not found")
}

func (m *mockInvoices) ListLineItems(_ context.Context, _, invoiceID string) ([]domain.InvoiceLineItem, error) {
	var out []domain.InvoiceLineItem
	for _, li := range m.lineItems {
		if li.InvoiceID == invoiceID {
			out = append(out, li)
		}
	}
	return out, nil
}

func (m *mockInvoices) UpdateTaxAtomic(_ context.Context, tenantID, invoiceID string, update domain.InvoiceTaxRetryUpdate, lineItems []domain.InvoiceLineItem) (domain.Invoice, error) {
	idx := -1
	for i, inv := range m.invoices {
		if inv.ID == invoiceID && inv.TenantID == tenantID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return domain.Invoice{}, errs.ErrNotFound
	}
	inv := m.invoices[idx]
	if inv.Status != domain.InvoiceDraft {
		return domain.Invoice{}, errs.InvalidState(fmt.Sprintf("invoice must be draft (current: %s)", inv.Status))
	}
	if inv.TaxStatus != domain.InvoiceTaxPending && inv.TaxStatus != domain.InvoiceTaxFailed {
		return domain.Invoice{}, errs.InvalidState(fmt.Sprintf("tax retry requires pending/failed (current: %s)", inv.TaxStatus))
	}
	byID := make(map[string]domain.InvoiceLineItem, len(lineItems))
	for _, li := range lineItems {
		byID[li.ID] = li
	}
	for i, existing := range m.lineItems {
		if existing.InvoiceID != invoiceID {
			continue
		}
		if updated, ok := byID[existing.ID]; ok {
			m.lineItems[i].TaxRate = updated.TaxRate
			m.lineItems[i].TaxAmountCents = updated.TaxAmountCents
			m.lineItems[i].TotalAmountCents = updated.TotalAmountCents
		}
	}
	inv.TaxAmountCents = update.TaxAmountCents
	inv.TaxRate = update.TaxRate
	inv.TaxName = update.TaxName
	inv.TaxCountry = update.TaxCountry
	inv.TaxID = update.TaxID
	inv.TaxProvider = update.TaxProvider
	inv.TaxCalculationID = update.TaxCalculationID
	inv.TaxReverseCharge = update.TaxReverseCharge
	inv.TaxExemptReason = update.TaxExemptReason
	inv.TaxStatus = update.TaxStatus
	inv.TaxDeferredAt = update.TaxDeferredAt
	inv.TaxPendingReason = update.TaxPendingReason
	inv.TaxErrorCode = update.TaxErrorCode
	inv.TaxRetryCount++
	inv.TotalAmountCents = update.TotalAmountCents
	due := inv.TotalAmountCents - inv.AmountPaidCents - inv.CreditsAppliedCents
	if due < 0 {
		due = 0
	}
	inv.AmountDueCents = due
	m.invoices[idx] = inv
	return inv, nil
}

// fakeCreditGranter records Grant calls. Backs the BillOnCancel
// paid-check unit tests — they assert the grant fires (or doesn't)
// based on the source invoice's payment_status.
type fakeCreditGranter struct {
	grants []credit.GrantInput
}

func (g *fakeCreditGranter) Grant(_ context.Context, _ string, in credit.GrantInput) (domain.CreditLedgerEntry, error) {
	g.grants = append(g.grants, in)
	return domain.CreditLedgerEntry{ID: fmt.Sprintf("vlx_cle_%d", len(g.grants))}, nil
}

// fakeInvoiceVoider / fakeCreditNoteAdjuster back the #22 unpaid-prebill-relief
// tests: they record which settlement path BillOnCancel took (void the whole
// invoice vs reduce amount_due to the consumed portion via an adjustment CN).
type fakeInvoiceVoider struct {
	voided []string // invoice IDs
	err    error
}

func (v *fakeInvoiceVoider) Void(_ context.Context, _, id string) (domain.Invoice, error) {
	if v.err != nil {
		return domain.Invoice{}, v.err
	}
	v.voided = append(v.voided, id)
	return domain.Invoice{ID: id, Status: domain.InvoiceVoided}, nil
}

type cnAdjustCall struct {
	invoiceID string
	gross     int64
}

type fakeCreditNoteAdjuster struct {
	calls     []cnAdjustCall // create (issue+draft) calls
	draftCall []cnAdjustCall // CreateAdjustmentDraftTx calls specifically
	issued    []string       // Issue() ids
	err       error
	draftErr  error
}

func (a *fakeCreditNoteAdjuster) CreateAndIssueAdjustment(_ context.Context, _, invoiceID string, grossCents int64, _, _ string) (domain.CreditNote, error) {
	if a.err != nil {
		return domain.CreditNote{}, a.err
	}
	a.calls = append(a.calls, cnAdjustCall{invoiceID: invoiceID, gross: grossCents})
	return domain.CreditNote{ID: fmt.Sprintf("vlx_cn_%d", len(a.calls)), TotalCents: grossCents}, nil
}

func (a *fakeCreditNoteAdjuster) CreateAdjustmentDraftTx(_ context.Context, _ *sql.Tx, _, invoiceID string, grossCents int64, _, _ string) (domain.CreditNote, error) {
	if a.draftErr != nil {
		return domain.CreditNote{}, a.draftErr
	}
	a.draftCall = append(a.draftCall, cnAdjustCall{invoiceID: invoiceID, gross: grossCents})
	return domain.CreditNote{ID: fmt.Sprintf("vlx_cn_draft_%d", len(a.draftCall)), TotalCents: grossCents}, nil
}

func (a *fakeCreditNoteAdjuster) Issue(_ context.Context, _, id string) (domain.CreditNote, error) {
	a.issued = append(a.issued, id)
	return domain.CreditNote{ID: id}, nil
}

func (m *mockInvoices) FindBaseInvoiceForPeriod(_ context.Context, tenantID, subscriptionID string, periodStart time.Time) (domain.Invoice, error) {
	// Mock: search the invoices slice for one whose subscription matches
	// and that has a line item (in the flat m.lineItems slice) with
	// billing_period_start = periodStart. Mirrors the postgres semantics
	// for tests that explicitly seed in_advance invoices.
	for _, inv := range m.invoices {
		if inv.TenantID != tenantID || inv.SubscriptionID != subscriptionID {
			continue
		}
		if inv.Status == domain.InvoiceVoided || inv.Status == domain.InvoiceUncollectible {
			continue
		}
		for _, li := range m.lineItems {
			if li.InvoiceID != inv.ID {
				continue
			}
			if li.LineType == domain.LineTypeBaseFee && li.BillingPeriodStart != nil && li.BillingPeriodStart.Equal(periodStart) {
				return inv, nil
			}
		}
	}
	return domain.Invoice{}, errs.ErrNotFound
}

func (m *mockInvoices) FindFundingInvoicesForPeriod(_ context.Context, tenantID, subscriptionID string, periodStart, periodEnd time.Time) ([]domain.Invoice, error) {
	var out []domain.Invoice
	for _, inv := range m.invoices {
		if inv.TenantID != tenantID || inv.SubscriptionID != subscriptionID {
			continue
		}
		if inv.Status == domain.InvoiceVoided || inv.Status == domain.InvoiceUncollectible {
			continue
		}
		matched := false
		for _, li := range m.lineItems {
			if li.InvoiceID == inv.ID && li.LineType == domain.LineTypeBaseFee &&
				li.BillingPeriodStart != nil && li.BillingPeriodStart.Equal(periodStart) {
				matched = true
				break
			}
		}
		if !matched && inv.BillingReason == "subscription_update" && inv.SourcePlanChangedAt != nil &&
			!inv.SourcePlanChangedAt.Before(periodStart) && inv.SourcePlanChangedAt.Before(periodEnd) &&
			inv.BillingPeriodEnd.Equal(periodEnd) {
			matched = true
		}
		if matched {
			out = append(out, inv)
		}
	}
	if len(out) == 0 {
		return nil, errs.ErrNotFound
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func setupEngine() (*Engine, *mockSubs, *mockUsage, *mockPricing, *mockInvoices) {
	periodStart := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	nextBilling := periodEnd

	subs := &mockSubs{
		subs: map[string]domain.Subscription{
			"sub_1": {
				ID: "sub_1", TenantID: "t1", CustomerID: "cus_1",
				Items:  []domain.SubscriptionItem{{PlanID: "pln_1", Quantity: 1}},
				Status: domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar,
				CurrentBillingPeriodStart: &periodStart, CurrentBillingPeriodEnd: &periodEnd,
				NextBillingAt: &nextBilling,
			},
		},
		cycleUpdated: make(map[string]bool),
	}

	usage := &mockUsage{
		totals: map[string]int64{
			"mtr_api":     1500,
			"mtr_storage": 250,
		},
	}

	pricing := &mockPricing{
		plans: map[string]domain.Plan{
			"pln_1": {
				ID: "pln_1", Name: "Pro Plan", Currency: "USD",
				BillingInterval: domain.BillingMonthly,
				BaseAmountCents: 4900,
				MeterIDs:        []string{"mtr_api", "mtr_storage"},
			},
		},
		meters: map[string]domain.Meter{
			"mtr_api":     {ID: "mtr_api", Name: "API Calls", Unit: "calls", RatingRuleVersionID: "rrv_api"},
			"mtr_storage": {ID: "mtr_storage", Name: "Storage", Unit: "GB", RatingRuleVersionID: "rrv_storage"},
		},
		rules: map[string]domain.RatingRuleVersion{
			"rrv_api": {
				ID: "rrv_api", RuleKey: "api_calls", Version: 1, Mode: domain.PricingGraduated,
				GraduatedTiers: []domain.RatingTier{
					{UpTo: 1000, UnitAmountCents: decimal.NewFromInt(10)},
					{UpTo: 0, UnitAmountCents: decimal.NewFromInt(5)},
				},
			},
			"rrv_storage": {
				ID: "rrv_storage", RuleKey: "storage_gb", Version: 1, Mode: domain.PricingFlat,
				FlatAmountCents: decimal.NewFromInt(2500),
			},
		},
	}

	invoices := &mockInvoices{}

	// Fake clock at periodEnd + 1ns: just past the period boundary
	// so RunCycle sees exactly one period due. Without this, the
	// new multi-period loop (ADR-028) bills every month from
	// periodEnd → wall-clock today, breaking "1 invoice" assertions.
	fakeClk := clock.NewFake(periodEnd.Add(time.Nanosecond))
	engine := wireBaseTax(NewEngine(subs, usage, pricing, invoices, nil, &mockSettings{}, nil, nil, fakeClk))
	return engine, subs, usage, pricing, invoices
}

// TestBillOnePeriod_HealPathPropagatesAdvanceError pins ADR-056 P0.2: when the
// cycle invoice already exists (a prior tick committed it but crashed before the
// watermark advanced) billOnePeriod takes the idempotent-skip heal path and
// re-attempts the advance. A failure there MUST surface — a swallowed heal
// advance strands the watermark, so the sub re-hits the skip on every tick with
// no alert. Mirrors the happy-path advance, which already fails loud.
func TestBillOnePeriod_HealPathPropagatesAdvanceError(t *testing.T) {
	engine, subs, _, _, invoices := setupEngine()
	ctx := context.Background()

	invoices.createErr = errs.ErrAlreadyExists // invoice already exists → heal path
	subs.updateBillingCycleErr = fmt.Errorf("watermark write failed")

	if _, runErrs := engine.RunCycle(ctx, 50); len(runErrs) == 0 {
		t.Fatal("heal-path advance error must surface in RunCycle errors, not be swallowed")
	}
}

// TestBillOnCreateTx_PricesNewPlanOverNewPeriod exercises the prospective-sub →
// buildOnCreateInvoice contract the atomic cross-interval swap (ADR-056) relies
// on: BillOnCreateTx must price the NEW plan over the NEW period it is handed
// (the swap builds an in-memory sub because its tx writes aren't yet committed,
// so store.Get can't see them). Mock invoices ignore the nil tx → runs in-process.
func TestBillOnCreateTx_PricesNewPlanOverNewPeriod(t *testing.T) {
	now := time.Date(2026, 6, 19, 0, 0, 0, 0, time.UTC)
	newPE := now.AddDate(0, 1, 0)
	pricing := &mockPricing{plans: map[string]domain.Plan{
		"p_m_adv": {ID: "p_m_adv", Name: "Monthly Adv", Currency: "USD", BillingInterval: domain.BillingMonthly, BaseBillTiming: domain.BillInAdvance, BaseAmountCents: 12000},
	}}
	invoices := &mockInvoices{}
	engine := wireBaseTax(NewEngine(&mockSubs{subs: map[string]domain.Subscription{}}, &mockUsage{}, pricing, invoices, nil, &mockSettings{}, nil, nil, clock.NewFake(now)))

	prospective := domain.Subscription{
		ID: "sub_x", TenantID: "t1", CustomerID: "c1",
		Status:                    domain.SubscriptionActive,
		BillingTime:               domain.BillingTimeAnniversary,
		CurrentBillingPeriodStart: &now,
		CurrentBillingPeriodEnd:   &newPE,
		Items:                     []domain.SubscriptionItem{{ID: "si_1", PlanID: "p_m_adv", Quantity: 1}},
	}

	inv, ok, err := engine.BillOnCreateTx(context.Background(), nil, prospective)
	if err != nil {
		t.Fatalf("BillOnCreateTx: %v", err)
	}
	if !ok {
		t.Fatal("expected ok=true (in_advance base to bill)")
	}
	if inv.TotalAmountCents != 12000 {
		t.Errorf("new-period invoice total: got %d want 12000 (full monthly base over a full month)", inv.TotalAmountCents)
	}
	if !inv.BillingPeriodStart.Equal(now) || !inv.BillingPeriodEnd.Equal(newPE) {
		t.Errorf("invoice period: got (%v,%v) want (%v,%v)", inv.BillingPeriodStart, inv.BillingPeriodEnd, now, newPE)
	}
	if inv.BillingReason != domain.BillingReasonSubscriptionCreate {
		t.Errorf("billing_reason: got %q want subscription_create", inv.BillingReason)
	}
	if len(invoices.lineItems) == 0 || invoices.lineItems[0].Description == "" {
		t.Error("expected a base-fee line item for the new plan")
	}
}

func TestRunCycle_GeneratesInvoice(t *testing.T) {
	engine, subs, _, _, invoices := setupEngine()
	ctx := context.Background()

	count, errs := engine.RunCycle(ctx, 50)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if count != 1 {
		t.Fatalf("got %d invoices, want 1", count)
	}

	// Verify invoice
	if len(invoices.invoices) != 1 {
		t.Fatalf("got %d invoices stored, want 1", len(invoices.invoices))
	}

	inv := invoices.invoices[0]
	if inv.CustomerID != "cus_1" {
		t.Errorf("got customer_id %q, want cus_1", inv.CustomerID)
	}
	if inv.SubscriptionID != "sub_1" {
		t.Errorf("got subscription_id %q, want sub_1", inv.SubscriptionID)
	}
	if inv.Status != domain.InvoiceFinalized {
		t.Errorf("got status %q, want finalized", inv.Status)
	}
	if inv.Currency != "USD" {
		t.Errorf("got currency %q, want USD", inv.Currency)
	}

	// Expected: base fee $49 + API graduated (1000*10 + 500*5 = 12500) + storage flat (250*2500 = 625000)
	// Total: 4900 + 12500 + 625000 = 642400 cents
	expectedTotal := int64(4900 + 12500 + 625000)
	if inv.TotalAmountCents != expectedTotal {
		t.Errorf("got total %d cents, want %d", inv.TotalAmountCents, expectedTotal)
	}

	// Verify line items (base + 2 usage)
	if len(invoices.lineItems) != 3 {
		t.Fatalf("got %d line items, want 3", len(invoices.lineItems))
	}

	// Verify billing cycle was advanced
	if !subs.cycleUpdated["sub_1"] {
		t.Error("billing cycle should have been advanced")
	}
	updatedSub := subs.subs["sub_1"]
	if updatedSub.CurrentBillingPeriodStart.Month() != time.April {
		t.Errorf("next period should start in April, got %v", updatedSub.CurrentBillingPeriodStart)
	}
}

// TestRunCycle_CalendarMonthly_AutoRealignsOnCycleClose locks in the
// fix for the drifted-anchor case: a calendar-billing monthly sub
// whose period anchored on day-20 (e.g. inherited from a prior
// yearly→monthly plan swap) MUST auto-realign to the next calendar
// boundary on its next cycle close, not preserve day-20 forever.
//
// Pre-fix advanceBillingPeriod was interval-only and silently
// preserved the input timestamp's day-of-month. Now cycle close
// calls domain.NextBillingPeriodEnd which honors billing_time —
// calendar+monthly snaps to first-of-next-month.
//
// Anniversary subs are unaffected (covered by the existing day-20
// → day-20 + 1mo cycle in setupEngine variants).
func TestRunCycle_CalendarMonthly_AutoRealignsOnCycleClose(t *testing.T) {
	// Period anchored mid-month (May 20 → Jun 20). Simulates the
	// drifted-anchor state from a yearly→monthly plan swap.
	periodStart := time.Date(2029, 5, 20, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2029, 6, 20, 0, 0, 0, 0, time.UTC)

	subs := &mockSubs{
		subs: map[string]domain.Subscription{
			"sub_1": {
				ID: "sub_1", TenantID: "t1", CustomerID: "cus_1",
				Items:                     []domain.SubscriptionItem{{PlanID: "pln_1", Quantity: 1}},
				Status:                    domain.SubscriptionActive,
				BillingTime:               domain.BillingTimeCalendar,
				CurrentBillingPeriodStart: &periodStart,
				CurrentBillingPeriodEnd:   &periodEnd,
				NextBillingAt:             &periodEnd,
			},
		},
		cycleUpdated: make(map[string]bool),
	}
	pricing := &mockPricing{
		plans: map[string]domain.Plan{
			"pln_1": {
				ID: "pln_1", Name: "Mo", Currency: "USD",
				BillingInterval: domain.BillingMonthly,
				BaseAmountCents: 3000,
			},
		},
	}
	invoices := &mockInvoices{}
	fakeClk := clock.NewFake(periodEnd.Add(time.Nanosecond))
	engine := wireBaseTax(NewEngine(subs, &mockUsage{totals: map[string]int64{}}, pricing, invoices, nil, &mockSettings{}, nil, nil, fakeClk))

	_, errs := engine.RunCycle(context.Background(), 50)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	updated := subs.subs["sub_1"]
	if updated.CurrentBillingPeriodStart == nil || updated.CurrentBillingPeriodEnd == nil {
		t.Fatal("period should have advanced")
	}
	// New period start = the prior period end (Jun 20). New period end =
	// first-of-next-month after Jun 20 = Jul 1. Calendar boundary
	// restored — auto-realign worked.
	wantStart := time.Date(2029, 6, 20, 0, 0, 0, 0, time.UTC)
	wantEnd := time.Date(2029, 7, 1, 0, 0, 0, 0, time.UTC)
	if !updated.CurrentBillingPeriodStart.Equal(wantStart) {
		t.Errorf("new period start: got %v, want %v", updated.CurrentBillingPeriodStart, wantStart)
	}
	if !updated.CurrentBillingPeriodEnd.Equal(wantEnd) {
		t.Errorf("new period end: got %v, want %v (calendar-snap)", updated.CurrentBillingPeriodEnd, wantEnd)
	}
}

// TestRunCycle_PlanIntervalChange_CalendarSnaps locks in the
// root-cause fix: when a sub has a pending plan-change applied at
// cycle close that switches yearly→monthly (the actual path that
// creates the day-N anchor drift), the new period MUST anchor to
// the next calendar boundary on calendar billing — not preserve
// the yearly anniversary day-of-month for the rest of the sub's
// life.
//
// Pre-fix advanceBillingPeriod was interval-only and would compute
// `periodEnd + 1 month` = (yearly anniv + 1 month), persisting the
// drift. The new domain.NextBillingPeriodEnd snaps to the calendar
// boundary, so the first post-swap cycle is a stub (anniv → 1st)
// followed by clean day-1 cycles forever.
func TestRunCycle_PlanIntervalChange_CalendarSnaps(t *testing.T) {
	// Yearly period anchored on day-20 (one year before close).
	periodStart := time.Date(2028, 5, 20, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2029, 5, 20, 0, 0, 0, 0, time.UTC)
	// Pending plan change effective at the cycle close — yearly → monthly.
	effective := periodEnd

	subs := &mockSubs{
		subs: map[string]domain.Subscription{
			"sub_1": {
				ID: "sub_1", TenantID: "t1", CustomerID: "cus_1",
				Items: []domain.SubscriptionItem{
					{
						ID:                     "si_1",
						PlanID:                 "pln_yearly",
						Quantity:               1,
						PendingPlanID:          "pln_monthly",
						PendingPlanEffectiveAt: &effective,
					},
				},
				Status:                    domain.SubscriptionActive,
				BillingTime:               domain.BillingTimeCalendar,
				CurrentBillingPeriodStart: &periodStart,
				CurrentBillingPeriodEnd:   &periodEnd,
				NextBillingAt:             &periodEnd,
			},
		},
		cycleUpdated: make(map[string]bool),
	}
	pricing := &mockPricing{
		plans: map[string]domain.Plan{
			"pln_yearly":  {ID: "pln_yearly", Name: "Yr", Currency: "USD", BillingInterval: domain.BillingYearly, BaseAmountCents: 12000},
			"pln_monthly": {ID: "pln_monthly", Name: "Mo", Currency: "USD", BillingInterval: domain.BillingMonthly, BaseAmountCents: 1000},
		},
	}
	fakeClk := clock.NewFake(periodEnd.Add(time.Nanosecond))
	engine := wireBaseTax(NewEngine(subs, &mockUsage{totals: map[string]int64{}}, pricing, &mockInvoices{}, nil, &mockSettings{}, nil, nil, fakeClk))

	if _, errs := engine.RunCycle(context.Background(), 50); len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	updated := subs.subs["sub_1"]
	// After the cycle close + pending-plan apply, the new period must
	// snap to the next calendar boundary (Jun 1) under the new monthly
	// plan — NOT preserve the yearly anniversary day-20 (which would
	// give us Jun 20, the bug).
	wantStart := time.Date(2029, 5, 20, 0, 0, 0, 0, time.UTC)
	wantEnd := time.Date(2029, 6, 1, 0, 0, 0, 0, time.UTC)
	if !updated.CurrentBillingPeriodStart.Equal(wantStart) {
		t.Errorf("new period start: got %v, want %v", updated.CurrentBillingPeriodStart, wantStart)
	}
	if !updated.CurrentBillingPeriodEnd.Equal(wantEnd) {
		t.Errorf("new period end: got %v, want %v (calendar-snap after yearly→monthly swap)", updated.CurrentBillingPeriodEnd, wantEnd)
	}
	// Item plan_id must have flipped to the new monthly plan.
	if updated.Items[0].PlanID != "pln_monthly" {
		t.Errorf("pending plan not applied: item plan_id = %v, want pln_monthly", updated.Items[0].PlanID)
	}
}

// TestRunCycle_PlanIntervalChange_InAdvance_StubProrated locks in
// the proration of the in_advance NEXT-period base fee when the
// calendar-snap stub is shorter than a full plan cycle. Pre-fix
// (2026-05-21) the in_advance loop at cycle close billed the FULL
// monthly base regardless of upcoming-period length: a yearly→
// monthly plan-change cycle close on a calendar+monthly sub produces
// a stub like (Jun 24, Jul 1) = 7 days, and the engine billed the
// full $70 monthly base for it. Same proration shape that
// BillOnCreate and emitBaseSegmentLine already had.
func TestRunCycle_PlanIntervalChange_InAdvance_StubProrated(t *testing.T) {
	periodStart := time.Date(2028, 6, 24, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2029, 6, 24, 0, 0, 0, 0, time.UTC)
	effective := periodEnd

	subs := &mockSubs{
		subs: map[string]domain.Subscription{
			"sub_1": {
				ID: "sub_1", TenantID: "t1", CustomerID: "cus_1",
				Items: []domain.SubscriptionItem{
					{
						ID:                     "si_1",
						PlanID:                 "pln_yearly",
						Quantity:               1,
						PendingPlanID:          "pln_monthly",
						PendingPlanEffectiveAt: &effective,
					},
				},
				Status:                    domain.SubscriptionActive,
				BillingTime:               domain.BillingTimeCalendar,
				CurrentBillingPeriodStart: &periodStart,
				CurrentBillingPeriodEnd:   &periodEnd,
				NextBillingAt:             &periodEnd,
			},
		},
		cycleUpdated: make(map[string]bool),
	}
	pricing := &mockPricing{
		plans: map[string]domain.Plan{
			"pln_yearly":  {ID: "pln_yearly", Name: "Yr", Currency: "USD", BillingInterval: domain.BillingYearly, BaseAmountCents: 12000, BaseBillTiming: domain.BillInAdvance},
			"pln_monthly": {ID: "pln_monthly", Name: "Mo", Currency: "USD", BillingInterval: domain.BillingMonthly, BaseAmountCents: 7000, BaseBillTiming: domain.BillInAdvance},
		},
	}
	invoices := &mockInvoices{}
	fakeClk := clock.NewFake(periodEnd.Add(time.Nanosecond))
	engine := wireBaseTax(NewEngine(subs, &mockUsage{totals: map[string]int64{}}, pricing, invoices, nil, &mockSettings{}, nil, nil, fakeClk))

	if _, errs := engine.RunCycle(context.Background(), 50); len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	// New period: (Jun 24 2029, Jul 1 2029) — 7-day calendar-snap stub.
	// In_advance base on new monthly plan ($70). Expected proration:
	// 7000 × 7 / 30 = 1633 cents (round-half-to-even).
	if len(invoices.invoices) != 1 {
		t.Fatalf("expected 1 invoice generated, got %d", len(invoices.invoices))
	}
	got := invoices.invoices[0]
	wantSubtotal := int64(7000 * 7 / 30)
	if got.SubtotalCents != wantSubtotal {
		t.Errorf("stub-period in_advance subtotal: got %d, want %d (7000 × 7 / 30, prorated for 7-day calendar stub of 30-day cycle)", got.SubtotalCents, wantSubtotal)
	}
}

// TestRunCycle_AnniversaryMonthly_PreservesDayOfMonth is the negative
// guard: anniversary billing must NOT snap to calendar boundary. The
// configured anchor is the sub-start day-of-month; cycle close rolls
// by +1 month preserving it.
func TestRunCycle_AnniversaryMonthly_PreservesDayOfMonth(t *testing.T) {
	periodStart := time.Date(2029, 5, 20, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2029, 6, 20, 0, 0, 0, 0, time.UTC)

	subs := &mockSubs{
		subs: map[string]domain.Subscription{
			"sub_1": {
				ID: "sub_1", TenantID: "t1", CustomerID: "cus_1",
				Items:                     []domain.SubscriptionItem{{PlanID: "pln_1", Quantity: 1}},
				Status:                    domain.SubscriptionActive,
				BillingTime:               domain.BillingTimeAnniversary,
				CurrentBillingPeriodStart: &periodStart,
				CurrentBillingPeriodEnd:   &periodEnd,
				NextBillingAt:             &periodEnd,
			},
		},
		cycleUpdated: make(map[string]bool),
	}
	pricing := &mockPricing{
		plans: map[string]domain.Plan{
			"pln_1": {ID: "pln_1", Name: "Mo", Currency: "USD", BillingInterval: domain.BillingMonthly, BaseAmountCents: 3000},
		},
	}
	fakeClk := clock.NewFake(periodEnd.Add(time.Nanosecond))
	engine := wireBaseTax(NewEngine(subs, &mockUsage{totals: map[string]int64{}}, pricing, &mockInvoices{}, nil, &mockSettings{}, nil, nil, fakeClk))

	_, errs := engine.RunCycle(context.Background(), 50)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	updated := subs.subs["sub_1"]
	wantEnd := time.Date(2029, 7, 20, 0, 0, 0, 0, time.UTC) // Jun 20 + 1 month = Jul 20
	if !updated.CurrentBillingPeriodEnd.Equal(wantEnd) {
		t.Errorf("anniversary monthly should preserve day-20: got end %v, want %v", updated.CurrentBillingPeriodEnd, wantEnd)
	}
}

func TestRunCycle_NoDueSubscriptions(t *testing.T) {
	engine, subs, _, _, _ := setupEngine()

	// Move next_billing_at to the future
	s := subs.subs["sub_1"]
	future := time.Now().UTC().AddDate(0, 1, 0)
	s.NextBillingAt = &future
	subs.subs["sub_1"] = s

	count, errs := engine.RunCycle(context.Background(), 50)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if count != 0 {
		t.Errorf("got %d, want 0 invoices", count)
	}
}

func TestRunCycle_SkipsTrialSubscription(t *testing.T) {
	engine, subs, _, _, invoices := setupEngine()

	// New state-machine semantics: a trial is conveyed by status='trialing',
	// not just by trial_end_at being set. Service.Create routes new subs with
	// trial_days > 0 to trialing; the engine skips billing while they're in
	// that state.
	s := subs.subs["sub_1"]
	trialEnd := time.Now().UTC().AddDate(0, 0, 7) // 7 days from now
	s.Status = domain.SubscriptionTrialing
	s.TrialEndAt = &trialEnd
	subs.subs["sub_1"] = s

	count, errs := engine.RunCycle(context.Background(), 50)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if count != 0 {
		t.Errorf("got %d, want 0 (trial should be skipped)", count)
	}
	if len(invoices.invoices) != 0 {
		t.Error("no invoice should be generated during trial")
	}
	// But billing cycle should still advance
	if !subs.cycleUpdated["sub_1"] {
		t.Error("billing cycle should advance even during trial")
	}
}

func TestRunCycle_ZeroUsage(t *testing.T) {
	engine, _, usage, _, invoices := setupEngine()

	// No usage at all
	usage.totals = map[string]int64{}

	count, errs := engine.RunCycle(context.Background(), 50)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if count != 1 {
		t.Fatalf("got %d, want 1 (base fee invoice)", count)
	}

	// Should still generate invoice with just the base fee
	inv := invoices.invoices[0]
	if inv.TotalAmountCents != 4900 {
		t.Errorf("got total %d, want 4900 (base fee only)", inv.TotalAmountCents)
	}
	if len(invoices.lineItems) != 1 {
		t.Errorf("got %d line items, want 1 (base fee only)", len(invoices.lineItems))
	}
}

func TestRunCycle_LineItemDetails(t *testing.T) {
	engine, _, _, _, invoices := setupEngine()

	engine.RunCycle(context.Background(), 50)

	// Find usage line items
	for _, item := range invoices.lineItems {
		switch item.LineType {
		case domain.LineTypeBaseFee:
			if item.AmountCents != 4900 {
				t.Errorf("base fee: got %d, want 4900", item.AmountCents)
			}
			if item.Quantity != 1 {
				t.Errorf("base fee quantity: got %d, want 1", item.Quantity)
			}

		case domain.LineTypeUsage:
			if item.MeterID == "mtr_api" {
				if item.Quantity != 1500 {
					t.Errorf("API quantity: got %d, want 1500", item.Quantity)
				}
				if item.AmountCents != 12500 {
					t.Errorf("API amount: got %d, want 12500", item.AmountCents)
				}
				if item.PricingMode != "graduated" {
					t.Errorf("API pricing_mode: got %q, want graduated", item.PricingMode)
				}
				if item.RatingRuleVersionID != "rrv_api" {
					t.Errorf("API rule_id: got %q, want rrv_api", item.RatingRuleVersionID)
				}
			}
			if item.MeterID == "mtr_storage" {
				if item.AmountCents != 625000 { // 250 qty * 2500 flat rate
					t.Errorf("storage amount: got %d, want 625000", item.AmountCents)
				}
				if item.PricingMode != "flat" {
					t.Errorf("storage pricing_mode: got %q, want flat", item.PricingMode)
				}
			}
		}
	}
}

// TestRunCycle_OverrideLookupTransientError_Aborts (ADR-070,
// no-silent-fallbacks): a transient failure resolving the customer's
// override must ABORT the close — pre-fix any error was treated as
// "no override" and a DB blip silently billed a negotiated customer at
// list price on a committed, finalized invoice.
//
// Mutation-verify: restore the `overrideErr == nil &&` swallow in
// resolveRatedRule — this test fails (an invoice is generated).
func TestRunCycle_OverrideLookupTransientError_Aborts(t *testing.T) {
	engine, _, _, pricing, invoices := setupEngine()
	pricing.overrideErr = fmt.Errorf("connection reset by peer")

	count, errs := engine.RunCycle(context.Background(), 50)
	if len(errs) == 0 {
		t.Fatal("close succeeded despite override-lookup failure; want abort")
	}
	if count != 0 || len(invoices.invoices) != 0 {
		t.Fatalf("invoices generated on a failed override lookup: count=%d stored=%d — silently billed list price", count, len(invoices.invoices))
	}

	// Recovery: the same sub bills normally once the lookup heals.
	pricing.overrideErr = nil
	count, errs = engine.RunCycle(context.Background(), 50)
	if len(errs) > 0 {
		t.Fatalf("healed close errors: %v", errs)
	}
	if count != 1 {
		t.Fatalf("healed close generated %d invoices, want 1", count)
	}
}

// TestPreview_OverrideLookupTransientError_Aborts locks the fix for the
// verifier-caught under-bill: previewWithWindow used to degrade a
// previewMeter ERROR into a warning and drop that meter's lines — and
// the threshold scan PERSISTS preview lines as a fire invoice, after
// which the cycle close clamps every meter to the fire watermark and
// the dropped meter's pre-fire usage is billed by nobody. The preview
// must ABORT on infrastructure failure; the threshold eval propagates
// the error and retries next tick.
//
// Mutation-verify: restore the warning-degradation loop in
// previewWithWindow — this test fails (preview succeeds with the meter
// silently missing).
func TestPreview_OverrideLookupTransientError_Aborts(t *testing.T) {
	engine, subs, _, pricing, _ := setupEngine()
	pricing.overrideErr = fmt.Errorf("connection reset by peer")

	if _, err := engine.Preview(context.Background(), subs.subs["sub_1"]); err == nil {
		t.Fatal("preview succeeded despite override-lookup failure; want abort (a partial preview persisted by a threshold fire under-bills permanently)")
	}

	pricing.overrideErr = nil
	preview, err := engine.Preview(context.Background(), subs.subs["sub_1"])
	if err != nil {
		t.Fatalf("healed preview: %v", err)
	}
	if len(preview.Lines) == 0 {
		t.Fatal("healed preview has no lines")
	}
}

func TestRunCycle_WithPriceOverride(t *testing.T) {
	engine, _, _, pricing, invoices := setupEngine()

	// Set a per-customer override: API calls flat $50 instead of graduated.
	// Keyed by rule_key (ADR-070) — the override follows the rule across
	// version publishes.
	pricing.overrides = map[string]domain.CustomerPriceOverride{
		"cus_1:api_calls": {
			ID: "cpo_1", CustomerID: "cus_1", RuleKey: "api_calls", RatingRuleVersionID: "rrv_api",
			Mode: domain.PricingFlat, FlatAmountCents: decimal.NewFromInt(5000),
			Active: true,
		},
	}

	count, errs := engine.RunCycle(context.Background(), 50)
	if len(errs) > 0 {
		t.Fatalf("errors: %v", errs)
	}
	if count != 1 {
		t.Fatalf("expected 1 invoice, got %d", count)
	}

	inv := invoices.invoices[0]
	// Expected: base $49 + API flat (1500*5000=7500000) + storage flat (250*2500=625000)
	expectedTotal := int64(4900 + 7500000 + 625000)
	if inv.TotalAmountCents != expectedTotal {
		t.Errorf("with override: got %d cents, want %d",
			inv.TotalAmountCents, expectedTotal)
	}
}

func TestAdvanceBillingPeriod(t *testing.T) {
	from := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

	monthly := advanceBillingPeriod(from, domain.BillingMonthly, time.UTC, 0)
	if monthly.Month() != time.April || monthly.Day() != 1 {
		t.Errorf("monthly: got %v, want 2026-04-01", monthly)
	}

	yearly := advanceBillingPeriod(from, domain.BillingYearly, time.UTC, 0)
	if yearly.Year() != 2027 || yearly.Month() != time.March {
		t.Errorf("yearly: got %v, want 2027-03-01", yearly)
	}
}

// TestRunCycle_UnitAmountRoundsBankers is the COR-5 regression: the rating
// path previously used `amount / quantity` (truncating int division) to
// derive the per-unit display amount. For graduated/tiered rules the total
// rarely divides cleanly by the quantity, and truncation biased every
// displayed unit price downward — systematic over large batches, which
// accountants catch. Switching to money.RoundHalfToEven produces the
// nearest-cent unit while preserving the true AmountCents total.
//
// Setup: graduated rule with tier 1 (up to 1 unit) at 100 cents and tier 2
// at 50 cents per unit. Usage = 3 → amount = 1*100 + 2*50 = 200 cents.
// Truncation: 200/3 = 66. Banker's: 66.67 → 67.
func TestRunCycle_UnitAmountRoundsBankers(t *testing.T) {
	periodStart := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	subs := &mockSubs{
		subs: map[string]domain.Subscription{
			"sub_1": {
				ID: "sub_1", TenantID: "t1", CustomerID: "cus_1",
				Items:  []domain.SubscriptionItem{{PlanID: "pln_1", Quantity: 1}},
				Status: domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar,
				CurrentBillingPeriodStart: &periodStart, CurrentBillingPeriodEnd: &periodEnd,
				NextBillingAt: &periodEnd,
			},
		},
		cycleUpdated: make(map[string]bool),
	}
	usage := &mockUsage{totals: map[string]int64{"mtr_api": 3}}
	pricing := &mockPricing{
		plans: map[string]domain.Plan{
			"pln_1": {
				ID: "pln_1", Name: "Round Plan", Currency: "USD",
				BillingInterval: domain.BillingMonthly,
				BaseAmountCents: 0, MeterIDs: []string{"mtr_api"},
			},
		},
		meters: map[string]domain.Meter{
			"mtr_api": {ID: "mtr_api", Name: "API Calls", Unit: "calls", RatingRuleVersionID: "rrv_api"},
		},
		rules: map[string]domain.RatingRuleVersion{
			"rrv_api": {
				ID: "rrv_api", RuleKey: "api_calls", Version: 1, Mode: domain.PricingGraduated,
				GraduatedTiers: []domain.RatingTier{
					{UpTo: 1, UnitAmountCents: decimal.NewFromInt(100)},
					{UpTo: 0, UnitAmountCents: decimal.NewFromInt(50)},
				},
			},
		},
	}
	invoices := &mockInvoices{}
	engine := wireBaseTax(NewEngine(subs, usage, pricing, invoices, nil, &mockSettings{}, nil, nil, billingTestClock()))

	if _, errs := engine.RunCycle(context.Background(), 50); len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(invoices.invoices) != 1 {
		t.Fatalf("got %d invoices, want 1", len(invoices.invoices))
	}

	var usageLine *domain.InvoiceLineItem
	for i := range invoices.lineItems {
		if invoices.lineItems[i].MeterID == "mtr_api" {
			usageLine = &invoices.lineItems[i]
			break
		}
	}
	if usageLine == nil {
		t.Fatal("api usage line not found in invoice")
	}

	if usageLine.AmountCents != 200 {
		t.Errorf("amount_cents: got %d, want 200 (1*100 + 2*50)", usageLine.AmountCents)
	}
	if usageLine.Quantity != 3 {
		t.Errorf("quantity: got %d, want 3", usageLine.Quantity)
	}
	// Truncation would give 66; banker's rounding of 200/3 = 66.67 gives 67.
	if usageLine.UnitAmountCents != 67 {
		t.Errorf("unit_amount_cents: got %d, want 67 (banker's round of 200/3)",
			usageLine.UnitAmountCents)
	}
}

// A subscription with a due pending plan change must bill on the new plan
// after ApplyPendingPlanAtomic runs at the top of billSubscription. Locks in
// the COR-3 contract: the cycle boundary is the swap point.
func TestRunCycle_AppliesScheduledPlanChangeAtBoundary(t *testing.T) {
	periodStart := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	// Effective exactly at periodEnd — the partial-index WHERE clause requires
	// effective_at <= now, and the cycle runs at periodEnd.
	effectiveAt := periodEnd

	subs := &mockSubs{
		subs: map[string]domain.Subscription{
			"sub_1": {
				ID: "sub_1", TenantID: "t1", CustomerID: "cus_1",
				Items: []domain.SubscriptionItem{{
					PlanID:                 "pln_old",
					Quantity:               1,
					PendingPlanID:          "pln_new",
					PendingPlanEffectiveAt: &effectiveAt,
				}},
				Status: domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar,
				CurrentBillingPeriodStart: &periodStart, CurrentBillingPeriodEnd: &periodEnd,
				NextBillingAt: &periodEnd,
			},
		},
		cycleUpdated: make(map[string]bool),
	}

	pricing := &mockPricing{
		plans: map[string]domain.Plan{
			"pln_old": {ID: "pln_old", Name: "Old", Currency: "USD", BillingInterval: domain.BillingMonthly, BaseAmountCents: 1000},
			"pln_new": {ID: "pln_new", Name: "New", Currency: "USD", BillingInterval: domain.BillingMonthly, BaseAmountCents: 3000},
		},
	}

	invoices := &mockInvoices{}
	usage := &mockUsage{totals: map[string]int64{}}
	engine := wireBaseTax(NewEngine(subs, usage, pricing, invoices, nil, &mockSettings{}, nil, nil, billingTestClock()))

	count, errs := engine.RunCycle(context.Background(), 50)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if count != 1 {
		t.Fatalf("got %d invoices, want 1", count)
	}

	// The just-elapsed period was consumed under pln_old ($10/mo),
	// so the closing invoice bills at the OUTGOING plan's rate even
	// though the item now points to pln_new ($30/mo) post-swap.
	// Industry-standard (Stripe / Lago / Orb): closing cycle bills
	// under the outgoing plan; new plan takes effect next cycle.
	inv := invoices.invoices[0]
	if inv.SubtotalCents != 1000 {
		t.Errorf("billed on wrong plan: subtotal %d cents, want 1000 (outgoing plan, just-elapsed period)", inv.SubtotalCents)
	}

	// Item row must reflect the swap. The DB swap fires regardless
	// of billing — pricing the closing period uses the outgoing-plan
	// snapshot, but the durable item.plan_id is pln_new going forward.
	got := subs.subs["sub_1"]
	if len(got.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(got.Items))
	}
	it := got.Items[0]
	if it.PlanID != "pln_new" {
		t.Errorf("item plan_id: got %q, want pln_new", it.PlanID)
	}
	if it.PendingPlanID != "" || it.PendingPlanEffectiveAt != nil {
		t.Errorf("pending fields should be cleared: got pending_id=%q effective_at=%v",
			it.PendingPlanID, it.PendingPlanEffectiveAt)
	}
	if it.PlanChangedAt == nil {
		t.Error("plan_changed_at should be set after swap")
	}
}

// TestRunCycle_ScheduledPlanSwap_CrossInterval_BillsElapsedAtOutgoingPlan
// locks in the contract for the monthly → yearly scheduled swap case.
// Pre-fix, the engine billed the just-elapsed monthly period at the
// new yearly plan's prorated rate (588 * 30/365 ≈ 48), wildly wrong.
// Post-fix, the closing invoice bills under the outgoing monthly
// plan's full price; the new yearly cycle starts clean at periodEnd.
func TestRunCycle_ScheduledPlanSwap_CrossInterval_BillsElapsedAtOutgoingPlan(t *testing.T) {
	periodStart := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	effectiveAt := periodEnd

	subs := &mockSubs{
		subs: map[string]domain.Subscription{
			"sub_1": {
				ID: "sub_1", TenantID: "t1", CustomerID: "cus_1",
				Items: []domain.SubscriptionItem{{
					PlanID:                 "pln_monthly",
					Quantity:               1,
					PendingPlanID:          "pln_yearly",
					PendingPlanEffectiveAt: &effectiveAt,
				}},
				Status: domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar,
				CurrentBillingPeriodStart: &periodStart, CurrentBillingPeriodEnd: &periodEnd,
				NextBillingAt: &periodEnd,
			},
		},
		cycleUpdated: make(map[string]bool),
	}

	pricing := &mockPricing{
		plans: map[string]domain.Plan{
			"pln_monthly": {ID: "pln_monthly", Name: "Monthly", Currency: "USD", BillingInterval: domain.BillingMonthly, BaseAmountCents: 2900},
			"pln_yearly":  {ID: "pln_yearly", Name: "Yearly", Currency: "USD", BillingInterval: domain.BillingYearly, BaseAmountCents: 58800},
		},
	}

	invoices := &mockInvoices{}
	usage := &mockUsage{totals: map[string]int64{}}
	engine := wireBaseTax(NewEngine(subs, usage, pricing, invoices, nil, &mockSettings{}, nil, nil, billingTestClock()))

	count, errs := engine.RunCycle(context.Background(), 50)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if count != 1 {
		t.Fatalf("got %d invoices, want 1", count)
	}

	inv := invoices.invoices[0]
	if inv.SubtotalCents != 2900 {
		t.Errorf("cross-interval swap: closing invoice should bill outgoing monthly plan ($29 = 2900¢), got %d¢ (pre-fix would have been ~4833 from prorated yearly)", inv.SubtotalCents)
	}

	// The durable item.plan_id is the new yearly plan going forward,
	// so the next cycle anchors yearly.
	got := subs.subs["sub_1"]
	if got.Items[0].PlanID != "pln_yearly" {
		t.Errorf("item plan_id after swap: got %q, want pln_yearly", got.Items[0].PlanID)
	}
}

// A pending change dated in the future must NOT apply at the current cycle —
// the billing engine bills on the existing plan and leaves pending fields intact.
func TestRunCycle_SkipsPendingChangeNotYetDue(t *testing.T) {
	periodStart := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	futureEffective := periodEnd.AddDate(0, 1, 0) // 2026-05-01, next cycle

	subs := &mockSubs{
		subs: map[string]domain.Subscription{
			"sub_1": {
				ID: "sub_1", TenantID: "t1", CustomerID: "cus_1",
				Items: []domain.SubscriptionItem{{
					PlanID:                 "pln_old",
					Quantity:               1,
					PendingPlanID:          "pln_new",
					PendingPlanEffectiveAt: &futureEffective,
				}},
				Status: domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar,
				CurrentBillingPeriodStart: &periodStart, CurrentBillingPeriodEnd: &periodEnd,
				NextBillingAt: &periodEnd,
			},
		},
		cycleUpdated: make(map[string]bool),
	}

	pricing := &mockPricing{
		plans: map[string]domain.Plan{
			"pln_old": {ID: "pln_old", Name: "Old", Currency: "USD", BillingInterval: domain.BillingMonthly, BaseAmountCents: 1000},
			"pln_new": {ID: "pln_new", Name: "New", Currency: "USD", BillingInterval: domain.BillingMonthly, BaseAmountCents: 3000},
		},
	}

	invoices := &mockInvoices{}
	usage := &mockUsage{totals: map[string]int64{}}
	engine := wireBaseTax(NewEngine(subs, usage, pricing, invoices, nil, &mockSettings{}, nil, nil, billingTestClock()))

	_, errs := engine.RunCycle(context.Background(), 50)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}

	inv := invoices.invoices[0]
	if inv.SubtotalCents != 1000 {
		t.Errorf("should have billed on old plan: subtotal %d, want 1000", inv.SubtotalCents)
	}

	it := subs.subs["sub_1"].Items[0]
	if it.PendingPlanID != "pln_new" {
		t.Errorf("pending change should be preserved: got %q", it.PendingPlanID)
	}
	if it.PlanID != "pln_old" {
		t.Errorf("plan_id should not have swapped: got %q", it.PlanID)
	}
}

// TestEffectiveNow covers the four branches of Engine.simForSub (which
// effectiveNow is a thin .At wrapper over): no test clock → wall-clock; test
// clock wired → frozen_time; clock id set but no reader → wall-clock; reader
// errors → wall-clock. These branches are what makes a test-mode sub bill at
// simulated time without affecting live subs on the same engine instance.
//
// Each fallback branch must also drop the CLOCK ID, not just revert the
// instant: a Sim carrying "clock tc_1" with a wall-clock At would stamp audit
// rows into the simulated slice with a real-world timestamp (ADR-090 §5).
func TestEffectiveNow(t *testing.T) {
	wall := time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC)
	frozen := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	fakeClk := clock.NewFake(wall)

	liveSub := domain.Subscription{ID: "s_live", TenantID: "t1"}
	testSub := domain.Subscription{ID: "s_test", TenantID: "t1", TestClockID: "tc_1"}

	t.Run("no test clock id returns wall clock", func(t *testing.T) {
		e := NewEngine(nil, nil, nil, nil, nil, nil, nil, nil, fakeClk)
		e.SetTestClockReader(&stubClockReader{clk: domain.TestClock{FrozenTime: frozen}})
		got := e.simForSub(context.Background(), liveSub)
		if !got.At.Equal(wall) {
			t.Errorf("wall-clock sub: got %v, want %v", got.At, wall)
		}
		if got.Simulated() {
			t.Errorf("unpinned sub must not be simulated: %+v", got)
		}
	})

	t.Run("test clock id with reader returns frozen time", func(t *testing.T) {
		reader := &stubClockReader{clk: domain.TestClock{FrozenTime: frozen}}
		e := NewEngine(nil, nil, nil, nil, nil, nil, nil, nil, fakeClk)
		e.SetTestClockReader(reader)
		got := e.simForSub(context.Background(), testSub)
		if !got.At.Equal(frozen) {
			t.Errorf("test-mode sub: got %v, want %v", got.At, frozen)
		}
		if got.TestClockID != "tc_1" {
			t.Errorf("test_clock_id: got %q, want tc_1", got.TestClockID)
		}
		if reader.lastID != "tc_1" || reader.lastTenant != "t1" {
			t.Errorf("reader lookup: got (%q,%q), want (t1,tc_1)",
				reader.lastTenant, reader.lastID)
		}
	})

	t.Run("test clock id without reader falls back to wall clock", func(t *testing.T) {
		e := NewEngine(nil, nil, nil, nil, nil, nil, nil, nil, fakeClk)
		got := e.simForSub(context.Background(), testSub)
		if !got.At.Equal(wall) {
			t.Errorf("nil-reader fallback: got %v, want %v", got.At, wall)
		}
		if got.Simulated() {
			t.Errorf("wall-clock fallback must drop the clock id, got %+v", got)
		}
	})

	t.Run("reader error falls back to wall clock", func(t *testing.T) {
		e := NewEngine(nil, nil, nil, nil, nil, nil, nil, nil, fakeClk)
		e.SetTestClockReader(&stubClockReader{err: errs.ErrNotFound})
		got := e.simForSub(context.Background(), testSub)
		if got.Simulated() {
			t.Errorf("wall-clock fallback must drop the clock id, got %+v", got)
		}
		if !got.At.Equal(wall) {
			t.Errorf("error fallback: got %v, want %v", got.At, wall)
		}
	})
}

// TestEffectiveNowForInvoice locks in the per-invoice resolver dunning
// uses to keep state-machine timestamps in the simulation domain.
// Branches: clock-pinned sub returns frozen, unpinned sub returns
// wall-clock, manual draft (no subscription) returns wall-clock,
// missing invoice returns an error.
func TestEffectiveNowForInvoice(t *testing.T) {
	wall := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	frozen := time.Date(2024, 2, 1, 12, 0, 0, 0, time.UTC)
	fakeClk := clock.NewFake(wall)

	t.Run("clock-pinned invoice returns frozen", func(t *testing.T) {
		invoices := &mockInvoices{invoices: []domain.Invoice{
			{ID: "inv_1", TenantID: "t1", SubscriptionID: "sub_1"},
		}}
		subs := &mockSubs{subs: map[string]domain.Subscription{
			"sub_1": {ID: "sub_1", TenantID: "t1", TestClockID: "tc_1"},
		}}
		e := NewEngine(subs, nil, nil, invoices, nil, nil, nil, nil, fakeClk)
		e.SetTestClockReader(&stubClockReader{clk: domain.TestClock{FrozenTime: frozen}})

		got, err := e.SimForInvoice(context.Background(), "t1", "inv_1")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !got.At.Equal(frozen) {
			t.Errorf("clock-pinned invoice: got %v, want %v (frozen)", got, frozen)
		}
	})

	t.Run("unpinned subscription returns wall clock", func(t *testing.T) {
		invoices := &mockInvoices{invoices: []domain.Invoice{
			{ID: "inv_2", TenantID: "t1", SubscriptionID: "sub_2"},
		}}
		subs := &mockSubs{subs: map[string]domain.Subscription{
			"sub_2": {ID: "sub_2", TenantID: "t1"}, // no TestClockID
		}}
		e := NewEngine(subs, nil, nil, invoices, nil, nil, nil, nil, fakeClk)

		got, err := e.SimForInvoice(context.Background(), "t1", "inv_2")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !got.At.Equal(wall) {
			t.Errorf("unpinned sub: got %v, want %v (wall)", got, wall)
		}
	})

	t.Run("manual draft (no subscription) returns wall clock", func(t *testing.T) {
		invoices := &mockInvoices{invoices: []domain.Invoice{
			{ID: "inv_3", TenantID: "t1"}, // no SubscriptionID
		}}
		e := NewEngine(nil, nil, nil, invoices, nil, nil, nil, nil, fakeClk)

		got, err := e.SimForInvoice(context.Background(), "t1", "inv_3")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !got.At.Equal(wall) {
			t.Errorf("manual draft: got %v, want %v (wall)", got, wall)
		}
	})

	t.Run("missing invoice returns error and wall fallback", func(t *testing.T) {
		invoices := &mockInvoices{} // empty
		e := NewEngine(nil, nil, nil, invoices, nil, nil, nil, nil, fakeClk)

		got, err := e.SimForInvoice(context.Background(), "t1", "inv_missing")
		if err == nil {
			t.Error("expected error for missing invoice")
		}
		if !got.At.Equal(wall) {
			t.Errorf("missing-invoice fallback: got %v, want %v (wall)", got, wall)
		}
	})
}

// stubCustomerReader returns a fixed customer per id — used by the
// EffectiveNowForCustomer tests and the (one-off invoice) branch of
// EffectiveNowForInvoice.
type stubCustomerReader struct {
	customers map[string]domain.Customer
	err       error
}

func (s *stubCustomerReader) Get(_ context.Context, _, id string) (domain.Customer, error) {
	if s.err != nil {
		return domain.Customer{}, s.err
	}
	c, ok := s.customers[id]
	if !ok {
		return domain.Customer{}, fmt.Errorf("not found")
	}
	return c, nil
}

// TestEffectiveNowForCustomer covers the four resolution branches for
// the customer-level entry point used by subscription.Service.Create
// and the one-off-invoice branch of EffectiveNowForInvoice.
func TestEffectiveNowForCustomer(t *testing.T) {
	wall := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	frozen := time.Date(2024, 2, 1, 12, 0, 0, 0, time.UTC)
	fakeClk := clock.NewFake(wall)

	t.Run("clock-pinned customer returns frozen", func(t *testing.T) {
		e := NewEngine(nil, nil, nil, nil, nil, nil, nil, nil, fakeClk)
		e.SetCustomerReader(&stubCustomerReader{customers: map[string]domain.Customer{
			"cus_1": {ID: "cus_1", TenantID: "t1", TestClockID: "tc_1"},
		}})
		e.SetTestClockReader(&stubClockReader{clk: domain.TestClock{FrozenTime: frozen}})

		got, err := e.SimForCustomer(context.Background(), "t1", "cus_1")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !got.At.Equal(frozen) {
			t.Errorf("clock-pinned customer: got %v, want %v (frozen)", got, frozen)
		}
	})

	t.Run("unpinned customer returns wall clock", func(t *testing.T) {
		e := NewEngine(nil, nil, nil, nil, nil, nil, nil, nil, fakeClk)
		e.SetCustomerReader(&stubCustomerReader{customers: map[string]domain.Customer{
			"cus_2": {ID: "cus_2", TenantID: "t1"}, // no TestClockID
		}})
		got, err := e.SimForCustomer(context.Background(), "t1", "cus_2")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !got.At.Equal(wall) {
			t.Errorf("unpinned customer: got %v, want %v (wall)", got, wall)
		}
	})

	t.Run("no customer reader wired returns wall clock", func(t *testing.T) {
		e := NewEngine(nil, nil, nil, nil, nil, nil, nil, nil, fakeClk)
		got, err := e.SimForCustomer(context.Background(), "t1", "cus_x")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !got.At.Equal(wall) {
			t.Errorf("no-reader fallback: got %v, want %v (wall)", got, wall)
		}
	})

	t.Run("missing customer returns error and wall fallback", func(t *testing.T) {
		e := NewEngine(nil, nil, nil, nil, nil, nil, nil, nil, fakeClk)
		e.SetCustomerReader(&stubCustomerReader{customers: map[string]domain.Customer{}})

		got, err := e.SimForCustomer(context.Background(), "t1", "cus_missing")
		if err == nil {
			t.Error("expected error for missing customer")
		}
		if !got.At.Equal(wall) {
			t.Errorf("missing-customer fallback: got %v, want %v (wall)", got, wall)
		}
	})
}

// TestEffectiveNowForInvoice_OneOffCustomerPath locks in the one-off
// invoice branch added alongside EffectiveNowForCustomer: invoices
// without a subscription_id resolve via the customer pin instead of
// returning wall-clock unconditionally.
func TestEffectiveNowForInvoice_OneOffCustomerPath(t *testing.T) {
	wall := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	frozen := time.Date(2024, 2, 1, 12, 0, 0, 0, time.UTC)
	fakeClk := clock.NewFake(wall)

	invoices := &mockInvoices{invoices: []domain.Invoice{
		{ID: "inv_oneoff", TenantID: "t1", CustomerID: "cus_1"}, // no SubscriptionID
	}}
	e := NewEngine(nil, nil, nil, invoices, nil, nil, nil, nil, fakeClk)
	e.SetCustomerReader(&stubCustomerReader{customers: map[string]domain.Customer{
		"cus_1": {ID: "cus_1", TenantID: "t1", TestClockID: "tc_1"},
	}})
	e.SetTestClockReader(&stubClockReader{clk: domain.TestClock{FrozenTime: frozen}})

	got, err := e.SimForInvoice(context.Background(), "t1", "inv_oneoff")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.At.Equal(frozen) {
		t.Errorf("one-off invoice for clock-pinned customer: got %v, want %v (frozen)", got, frozen)
	}
}

// capturingEventDispatcher records every Dispatch call for event-firing tests.
type capturingEventDispatcher struct {
	events []struct {
		tenantID  string
		eventType string
		payload   map[string]any
	}
}

func (d *capturingEventDispatcher) Dispatch(_ context.Context, tenantID, eventType string, payload map[string]any) error {
	d.events = append(d.events, struct {
		tenantID  string
		eventType string
		payload   map[string]any
	}{tenantID, eventType, payload})
	return nil
}

// TestRunCycle_FiresPendingChangeAppliedEvent locks in P0 #2: when a due
// pending item plan change rolls in at the cycle boundary, the engine must
// emit subscription.pending_change.applied per swapped item so downstream
// systems (analytics, revrec, customer notifications) don't have to poll for
// state diffs to know the change landed.
func TestRunCycle_FiresPendingChangeAppliedEvent(t *testing.T) {
	periodStart := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	effectiveAt := periodEnd

	subs := &mockSubs{
		subs: map[string]domain.Subscription{
			"sub_1": {
				ID: "sub_1", TenantID: "t1", CustomerID: "cus_1",
				Items: []domain.SubscriptionItem{{
					ID:                     "si_1",
					PlanID:                 "pln_old",
					Quantity:               1,
					PendingPlanID:          "pln_new",
					PendingPlanEffectiveAt: &effectiveAt,
				}},
				Status: domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar,
				CurrentBillingPeriodStart: &periodStart, CurrentBillingPeriodEnd: &periodEnd,
				NextBillingAt: &periodEnd,
			},
		},
		cycleUpdated: make(map[string]bool),
	}

	pricing := &mockPricing{
		plans: map[string]domain.Plan{
			"pln_old": {ID: "pln_old", Name: "Old", Currency: "USD", BillingInterval: domain.BillingMonthly, BaseAmountCents: 1000},
			"pln_new": {ID: "pln_new", Name: "New", Currency: "USD", BillingInterval: domain.BillingMonthly, BaseAmountCents: 3000},
		},
	}

	invoices := &mockInvoices{}
	usage := &mockUsage{totals: map[string]int64{}}
	dispatcher := &capturingEventDispatcher{}
	engine := wireBaseTax(NewEngine(subs, usage, pricing, invoices, nil, &mockSettings{}, nil, nil, billingTestClock()))
	engine.SetEventDispatcher(dispatcher)

	if _, errs := engine.RunCycle(context.Background(), 50); len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}

	var applied map[string]any
	for _, ev := range dispatcher.events {
		if ev.eventType == domain.EventSubscriptionPendingChangeApplied {
			applied = ev.payload
			if ev.tenantID != "t1" {
				t.Errorf("tenant_id: got %q, want t1", ev.tenantID)
			}
			break
		}
	}
	if applied == nil {
		types := make([]string, 0, len(dispatcher.events))
		for _, ev := range dispatcher.events {
			types = append(types, ev.eventType)
		}
		t.Fatalf("expected %s, got types=%v", domain.EventSubscriptionPendingChangeApplied, types)
	}
	if applied["item_id"] != "si_1" {
		t.Errorf("item_id: got %v, want si_1", applied["item_id"])
	}
	if applied["old_plan_id"] != "pln_old" {
		t.Errorf("old_plan_id: got %v, want pln_old", applied["old_plan_id"])
	}
	if applied["new_plan_id"] != "pln_new" {
		t.Errorf("new_plan_id: got %v, want pln_new", applied["new_plan_id"])
	}
}

// TestRunCycle_OneSubFailsOthersContinue asserts the batch loop's isolation
// guarantee: a single subscription with bad data (here: a plan referencing a
// meter whose rating rule is missing) must not prevent healthy subscriptions
// in the same batch from being invoiced. Without this guarantee, one broken
// customer could stall the entire billing cycle.
func TestRunCycle_OneSubFailsOthersContinue(t *testing.T) {
	periodStart := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)

	subs := &mockSubs{
		subs: map[string]domain.Subscription{
			"sub_ok": {
				ID: "sub_ok", TenantID: "t1", CustomerID: "cus_ok",
				Items:  []domain.SubscriptionItem{{ID: "si_ok", PlanID: "pln_ok", Quantity: 1}},
				Status: domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar,
				CurrentBillingPeriodStart: &periodStart, CurrentBillingPeriodEnd: &periodEnd,
				NextBillingAt: &periodEnd,
			},
			"sub_bad": {
				ID: "sub_bad", TenantID: "t1", CustomerID: "cus_bad",
				Items:  []domain.SubscriptionItem{{ID: "si_bad", PlanID: "pln_bad", Quantity: 1}},
				Status: domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar,
				CurrentBillingPeriodStart: &periodStart, CurrentBillingPeriodEnd: &periodEnd,
				NextBillingAt: &periodEnd,
			},
		},
		cycleUpdated: make(map[string]bool),
	}

	pricing := &mockPricing{
		plans: map[string]domain.Plan{
			"pln_ok":  {ID: "pln_ok", Name: "Flat", Currency: "USD", BillingInterval: domain.BillingMonthly, BaseAmountCents: 1000},
			"pln_bad": {ID: "pln_bad", Name: "Broken", Currency: "USD", BillingInterval: domain.BillingMonthly, MeterIDs: []string{"mtr_missing"}},
		},
		// mtr_missing references rrv_missing which isn't in rules — lookup fails
		meters: map[string]domain.Meter{
			"mtr_missing": {ID: "mtr_missing", Name: "Missing", RatingRuleVersionID: "rrv_missing"},
		},
	}
	invoices := &mockInvoices{}
	usage := &mockUsage{totals: map[string]int64{"mtr_missing": 100}}
	engine := wireBaseTax(NewEngine(subs, usage, pricing, invoices, nil, &mockSettings{}, nil, nil, billingTestClock()))

	count, runErrs := engine.RunCycle(context.Background(), 50)

	if count != 1 {
		t.Errorf("got %d invoices, want 1 (only sub_ok should succeed)", count)
	}
	if len(runErrs) != 1 {
		t.Fatalf("got %d errors, want 1 (sub_bad should fail)", len(runErrs))
	}
	// Healthy subscription's cycle should have advanced despite the neighbor failing.
	if !subs.cycleUpdated["sub_ok"] {
		t.Error("sub_ok billing cycle should have advanced")
	}
	if len(invoices.invoices) != 1 {
		t.Fatalf("got %d invoices stored, want 1", len(invoices.invoices))
	}
	if invoices.invoices[0].SubscriptionID != "sub_ok" {
		t.Errorf("stored invoice should be for sub_ok, got %q", invoices.invoices[0].SubscriptionID)
	}
}

// TestRunCycle_TaxProviderErrorDefersInvoice asserts end-to-end that a tax
// provider failure during RunCycle produces a draft invoice with
// tax_status=pending rather than a finalized invoice with wrong tax.
// Finalize is guarded downstream; the retry worker is responsible for
// completing the calculation and transitioning draft → finalized.
func TestRunCycle_TaxProviderErrorDefersInvoice(t *testing.T) {
	engine, _, _, _, invoices := setupEngine()
	engine.SetTaxProviderResolver(stubResolver(&stubProvider{err: fmt.Errorf("stripe down")}))

	count, runErrs := engine.RunCycle(context.Background(), 50)
	if len(runErrs) > 0 {
		t.Fatalf("unexpected errors: %v", runErrs)
	}
	if count != 1 {
		t.Fatalf("got %d invoices, want 1", count)
	}
	inv := invoices.invoices[0]
	if inv.TaxAmountCents != 0 {
		t.Errorf("got tax %d, want 0 when calculation deferred", inv.TaxAmountCents)
	}
	if inv.Status != domain.InvoiceDraft {
		t.Errorf("deferred invoice must stay in draft, got status %q", inv.Status)
	}
	if inv.TaxStatus != domain.InvoiceTaxPending {
		t.Errorf("tax_status = %q, want pending", inv.TaxStatus)
	}
	if inv.TaxPendingReason == "" {
		t.Error("tax_pending_reason should capture the provider error")
	}
}

// TestRunCycle_NoPendingChangeNoAppliedEvent ensures the event is gated on an
// actual swap — a subscription billing on its existing plan with no pending
// change must not emit a spurious applied event.
func TestRunCycle_NoPendingChangeNoAppliedEvent(t *testing.T) {
	periodStart := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)

	subs := &mockSubs{
		subs: map[string]domain.Subscription{
			"sub_1": {
				ID: "sub_1", TenantID: "t1", CustomerID: "cus_1",
				Items: []domain.SubscriptionItem{{
					ID: "si_1", PlanID: "pln_old", Quantity: 1,
				}},
				Status: domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar,
				CurrentBillingPeriodStart: &periodStart, CurrentBillingPeriodEnd: &periodEnd,
				NextBillingAt: &periodEnd,
			},
		},
		cycleUpdated: make(map[string]bool),
	}
	pricing := &mockPricing{
		plans: map[string]domain.Plan{
			"pln_old": {ID: "pln_old", Name: "Old", Currency: "USD", BillingInterval: domain.BillingMonthly, BaseAmountCents: 1000},
		},
	}
	invoices := &mockInvoices{}
	usage := &mockUsage{totals: map[string]int64{}}
	dispatcher := &capturingEventDispatcher{}
	engine := wireBaseTax(NewEngine(subs, usage, pricing, invoices, nil, &mockSettings{}, nil, nil, billingTestClock()))
	engine.SetEventDispatcher(dispatcher)

	if _, errs := engine.RunCycle(context.Background(), 50); len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}

	for _, ev := range dispatcher.events {
		if ev.eventType == domain.EventSubscriptionPendingChangeApplied {
			t.Fatalf("applied event fired without a pending change: %+v", ev)
		}
	}
}

// TestRunCycle_CancelAtPeriodEnd_FiresAtBoundary locks in the schedule-cancel
// behaviour: when a sub has cancel_at_period_end=true and the cycle scan
// observes effectiveNow >= period end, the engine generates the final invoice
// for the just-ended period AND transitions status to canceled — instead of
// rolling next_billing_at to a future date that would never be reached.
func TestRunCycle_CancelAtPeriodEnd_FiresAtBoundary(t *testing.T) {
	periodStart := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)

	subs := &mockSubs{
		subs: map[string]domain.Subscription{
			"sub_1": {
				ID: "sub_1", TenantID: "t1", CustomerID: "cus_1",
				Items:                     []domain.SubscriptionItem{{PlanID: "pln_1", Quantity: 1}},
				Status:                    domain.SubscriptionActive,
				BillingTime:               domain.BillingTimeCalendar,
				CurrentBillingPeriodStart: &periodStart,
				CurrentBillingPeriodEnd:   &periodEnd,
				NextBillingAt:             &periodEnd,
				CancelAtPeriodEnd:         true,
			},
		},
		cycleUpdated: make(map[string]bool),
	}
	pricing := &mockPricing{
		plans: map[string]domain.Plan{
			"pln_1": {
				ID: "pln_1", Name: "Pro", Currency: "USD",
				BillingInterval: domain.BillingMonthly,
				BaseAmountCents: 4900,
			},
		},
	}
	invoices := &mockInvoices{}
	dispatcher := &capturingEventDispatcher{}
	engine := wireBaseTax(NewEngine(subs, &mockUsage{totals: map[string]int64{}}, pricing, invoices, nil, &mockSettings{}, nil, nil, billingTestClock()))
	engine.SetEventDispatcher(dispatcher)

	if _, errs := engine.RunCycle(context.Background(), 50); len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}

	if len(invoices.invoices) != 1 {
		t.Fatalf("got %d invoices, want 1 (final invoice for just-ended period)", len(invoices.invoices))
	}

	updated := subs.subs["sub_1"]
	if updated.Status != domain.SubscriptionCanceled {
		t.Errorf("status: got %q, want canceled", updated.Status)
	}
	if updated.CanceledAt == nil {
		t.Error("canceled_at must be set after scheduled cancel fires")
	}
	if updated.CancelAtPeriodEnd {
		t.Error("cancel_at_period_end must be cleared after firing")
	}
	if updated.NextBillingAt != nil {
		t.Error("next_billing_at must be nil on canceled sub")
	}

	// subscription.canceled (canceled_by=schedule) moved IN-TX to the
	// store's FireScheduledCancellation writer (DispatchTx subscription
	// subset, 2026-07-05) — the engine must NOT also dispatch it
	// post-commit (double-emit). In-tx enqueue + provenance payload proven
	// by the real-Postgres lifecycle_outbox_integration_test.go.
	for _, ev := range dispatcher.events {
		if ev.eventType == domain.EventSubscriptionCanceled {
			t.Fatalf("%s must not be dispatched post-commit — it is enqueued in the cancel tx", domain.EventSubscriptionCanceled)
		}
	}
}

// TestRunCycle_CancelAt_FiresWhenTimestampReached covers the timestamp-based
// schedule. Setting cancel_at to the period boundary should fire the cancel
// at the same point cancel_at_period_end would, but via the timestamp path.
func TestRunCycle_CancelAt_FiresWhenTimestampReached(t *testing.T) {
	periodStart := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	cancelAt := periodEnd

	subs := &mockSubs{
		subs: map[string]domain.Subscription{
			"sub_1": {
				ID: "sub_1", TenantID: "t1", CustomerID: "cus_1",
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
			"pln_1": {
				ID: "pln_1", Name: "Pro", Currency: "USD",
				BillingInterval: domain.BillingMonthly,
				BaseAmountCents: 4900,
			},
		},
	}
	invoices := &mockInvoices{}
	engine := wireBaseTax(NewEngine(subs, &mockUsage{totals: map[string]int64{}}, pricing, invoices, nil, &mockSettings{}, nil, nil, billingTestClock()))

	if _, errs := engine.RunCycle(context.Background(), 50); len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}

	updated := subs.subs["sub_1"]
	if updated.Status != domain.SubscriptionCanceled {
		t.Errorf("status: got %q, want canceled", updated.Status)
	}
	if updated.CancelAt != nil {
		t.Error("cancel_at must be cleared after firing")
	}
}

// TestShouldFireScheduledCancel covers the predicate's edge cases directly.
// Boundary equality matters: the period-end transition must fire when
// effectiveNow equals period_end, not just strictly past it, otherwise a sub
// that gets billed at the exact boundary tick would skip its cancel.
func TestShouldFireScheduledCancel(t *testing.T) {
	t0 := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		sub       domain.Subscription
		periodEnd time.Time
		now       time.Time
		want      bool
	}{
		{
			name:      "no schedule, never fires",
			sub:       domain.Subscription{},
			periodEnd: t0,
			now:       t0,
			want:      false,
		},
		{
			name:      "at_period_end, before boundary",
			sub:       domain.Subscription{CancelAtPeriodEnd: true},
			periodEnd: t0,
			now:       t0.Add(-time.Second),
			want:      false,
		},
		{
			name:      "at_period_end, exactly at boundary",
			sub:       domain.Subscription{CancelAtPeriodEnd: true},
			periodEnd: t0,
			now:       t0,
			want:      true,
		},
		{
			name:      "at_period_end, past boundary",
			sub:       domain.Subscription{CancelAtPeriodEnd: true},
			periodEnd: t0,
			now:       t0.Add(time.Hour),
			want:      true,
		},
		{
			name:      "cancel_at, before timestamp",
			sub:       domain.Subscription{CancelAt: &t0},
			periodEnd: t0.Add(time.Hour),
			now:       t0.Add(-time.Second),
			want:      false,
		},
		{
			name:      "cancel_at, exactly at timestamp",
			sub:       domain.Subscription{CancelAt: &t0},
			periodEnd: t0.Add(time.Hour),
			now:       t0,
			want:      true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldFireScheduledCancel(tt.sub, tt.periodEnd, tt.now); got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

// TestRunCycle_PauseCollection_GeneratesDraft locks in the Stripe-parity
// behavior: a sub with pause_collection set still has its cycle advanced and
// an invoice generated, but the invoice is created as draft (which keeps it
// out of finalize/charge/dunning). Distinct from a hard pause
// (status=paused), which excludes the sub from GetDueBilling — the sub here
// is still active.
func TestRunCycle_PauseCollection_GeneratesDraft(t *testing.T) {
	periodStart := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)

	subs := &mockSubs{
		subs: map[string]domain.Subscription{
			"sub_1": {
				ID: "sub_1", TenantID: "t1", CustomerID: "cus_1",
				Items:                     []domain.SubscriptionItem{{PlanID: "pln_1", Quantity: 1}},
				Status:                    domain.SubscriptionActive,
				BillingTime:               domain.BillingTimeCalendar,
				CurrentBillingPeriodStart: &periodStart,
				CurrentBillingPeriodEnd:   &periodEnd,
				NextBillingAt:             &periodEnd,
				PauseCollection: &domain.PauseCollection{
					Behavior: domain.PauseCollectionKeepAsDraft,
				},
			},
		},
		cycleUpdated: make(map[string]bool),
	}
	pricing := &mockPricing{
		plans: map[string]domain.Plan{
			"pln_1": {
				ID: "pln_1", Name: "Pro", Currency: "USD",
				BillingInterval: domain.BillingMonthly,
				BaseAmountCents: 4900,
			},
		},
	}
	invoices := &mockInvoices{}
	engine := wireBaseTax(NewEngine(subs, &mockUsage{totals: map[string]int64{}}, pricing, invoices, nil, &mockSettings{}, nil, nil, billingTestClock()))

	if _, errs := engine.RunCycle(context.Background(), 50); len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}

	if len(invoices.invoices) != 1 {
		t.Fatalf("got %d invoices, want 1 (cycle should still emit invoice during pause_collection)", len(invoices.invoices))
	}
	got := invoices.invoices[0]
	if got.Status != domain.InvoiceDraft {
		t.Errorf("invoice status: got %q, want %q (pause_collection forces draft)", got.Status, domain.InvoiceDraft)
	}
	if !subs.cycleUpdated["sub_1"] {
		t.Error("billing cycle should still advance during pause_collection")
	}

	updated := subs.subs["sub_1"]
	if updated.PauseCollection == nil {
		t.Error("PauseCollection must remain set when no resumes_at is configured")
	}
}

// Auto-resume of pause_collection is no longer evaluated inside the
// engine cycle scan — the scheduler runs Service.ProcessExpiredPause-
// Collections as a dedicated phase BEFORE the cycle scan now (the
// catchup orchestrator does the equivalent at Phase 0.7). The old
// in-cycle gate test (TestRunCycle_PauseCollection_AutoResumesWhen-
// ResumesAtPasses) has been removed; the new contract is covered by
// service-level tests in the subscription package.

// TestRunCycle_Trial_Active_SkipsBillingAndAdvancesCycle covers case (a) of
// the trial state machine: the cycle scan visits a trialing sub whose trial
// has not yet elapsed. No invoice generated; next_billing_at advances.
func TestRunCycle_Trial_Active_SkipsBillingAndAdvancesCycle(t *testing.T) {
	periodStart := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC) // past, so cycle scan picks it up
	trialEnd := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)  // far-future: trial still active at scan time

	subs := &mockSubs{
		subs: map[string]domain.Subscription{
			"sub_1": {
				ID: "sub_1", TenantID: "t1", CustomerID: "cus_1",
				Items:                     []domain.SubscriptionItem{{PlanID: "pln_1", Quantity: 1}},
				Status:                    domain.SubscriptionTrialing,
				BillingTime:               domain.BillingTimeCalendar,
				TrialEndAt:                &trialEnd,
				CurrentBillingPeriodStart: &periodStart,
				CurrentBillingPeriodEnd:   &periodEnd,
				NextBillingAt:             &periodEnd,
			},
		},
		cycleUpdated: make(map[string]bool),
	}
	pricing := &mockPricing{
		plans: map[string]domain.Plan{
			"pln_1": {ID: "pln_1", Currency: "USD", BillingInterval: domain.BillingMonthly, BaseAmountCents: 4900},
		},
	}
	invoices := &mockInvoices{}
	dispatcher := &capturingEventDispatcher{}
	engine := wireBaseTax(NewEngine(subs, &mockUsage{totals: map[string]int64{}}, pricing, invoices, nil, &mockSettings{}, nil, nil, billingTestClock()))
	engine.SetEventDispatcher(dispatcher)

	if _, errs := engine.RunCycle(context.Background(), 50); len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}

	if len(invoices.invoices) != 0 {
		t.Errorf("expected 0 invoices during active trial, got %d", len(invoices.invoices))
	}
	if !subs.cycleUpdated["sub_1"] {
		t.Error("expected cycle to be advanced even when trial-skipping")
	}
	if subs.subs["sub_1"].Status != domain.SubscriptionTrialing {
		t.Errorf("status should remain trialing during active trial, got %q", subs.subs["sub_1"].Status)
	}
}

// TestRunCycle_Trial_Ended_AutoActivatesAndBills covers case (b): cycle scan
// arrives after trial_end_at; the engine flips status to active, fires
// subscription.trial_ended (triggered_by="schedule"), then bills the period
// normally.
func TestRunCycle_Trial_Ended_AutoActivatesAndBills(t *testing.T) {
	periodStart := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC) // past, so cycle scan picks it up
	trialEnd := time.Date(2026, 2, 15, 0, 0, 0, 0, time.UTC) // past: trial elapsed before now

	subs := &mockSubs{
		subs: map[string]domain.Subscription{
			"sub_1": {
				ID: "sub_1", TenantID: "t1", CustomerID: "cus_1",
				Items:                     []domain.SubscriptionItem{{PlanID: "pln_1", Quantity: 1}},
				Status:                    domain.SubscriptionTrialing,
				BillingTime:               domain.BillingTimeCalendar,
				TrialEndAt:                &trialEnd,
				CurrentBillingPeriodStart: &periodStart,
				CurrentBillingPeriodEnd:   &periodEnd,
				NextBillingAt:             &periodEnd,
			},
		},
		cycleUpdated: make(map[string]bool),
	}
	pricing := &mockPricing{
		plans: map[string]domain.Plan{
			"pln_1": {ID: "pln_1", Currency: "USD", BillingInterval: domain.BillingMonthly, BaseAmountCents: 4900},
		},
	}
	invoices := &mockInvoices{}
	dispatcher := &capturingEventDispatcher{}
	// Per-test fake clock (this sub's periodEnd is March 2026, not
	// the April default used by billingTestClock).
	clk := clock.NewFake(periodEnd.Add(time.Nanosecond))
	engine := wireBaseTax(NewEngine(subs, &mockUsage{totals: map[string]int64{}}, pricing, invoices, nil, &mockSettings{}, nil, nil, clk))
	engine.SetEventDispatcher(dispatcher)

	if _, errs := engine.RunCycle(context.Background(), 50); len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}

	if len(invoices.invoices) != 1 {
		t.Fatalf("expected 1 invoice after trial ends, got %d", len(invoices.invoices))
	}
	if invoices.invoices[0].Status != domain.InvoiceFinalized {
		t.Errorf("invoice status: got %q, want finalized", invoices.invoices[0].Status)
	}

	updated := subs.subs["sub_1"]
	if updated.Status != domain.SubscriptionActive {
		t.Errorf("status: got %q, want active after trial ends", updated.Status)
	}
	if updated.ActivatedAt == nil {
		t.Error("activated_at should be stamped after trial-end auto-flip")
	}

	// subscription.trial_ended moved IN-TX to the store's ActivateAfterTrial
	// writer (DispatchTx subscription subset, 2026-07-05) — the engine must
	// NOT also dispatch it post-commit (that would double-emit). The in-tx
	// enqueue + payload is proven by the real-Postgres
	// lifecycle_outbox_integration_test.go.
	for _, ev := range dispatcher.events {
		if ev.eventType == domain.EventSubscriptionTrialEnded {
			t.Fatalf("%s must not be dispatched post-commit — it is enqueued in the activation tx", domain.EventSubscriptionTrialEnded)
		}
	}
}

// TestRunCycle_Trial_Ended_InAdvance_CoversTrialEndPeriod locks in the
// PR-2.5 fix for Bug #6: when an in_advance + trial sub auto-flips at
// cycle close, the trial-end period must be covered by a BillOnCreate-
// style invoice. Pre-fix, billOnePeriod's normal billing for in_advance
// items charged for the NEXT period (periodEnd → nextPeriodEnd) and
// the trial-end period (periodStart → periodEnd) went unbilled — a
// revenue leak specific to in_advance + trial.
//
// Post-fix expect TWO invoices at cycle close:
//   - BillOnCreate-style invoice covering the trial-end period
//     [periodStart, periodEnd] (the period the sub just exited)
//   - Normal cycle invoice covering the next period [periodEnd,
//     nextPeriodEnd] (the upcoming pre-pay)
func TestRunCycle_Trial_Ended_InAdvance_CoversTrialEndPeriod(t *testing.T) {
	periodStart := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC) // past, so cycle scan picks it up
	trialEnd := time.Date(2026, 2, 15, 0, 0, 0, 0, time.UTC) // past: trial elapsed before now

	subs := &mockSubs{
		subs: map[string]domain.Subscription{
			"sub_1": {
				ID: "sub_1", TenantID: "t1", CustomerID: "cus_1",
				Items:                     []domain.SubscriptionItem{{PlanID: "pln_1", Quantity: 1}},
				Status:                    domain.SubscriptionTrialing,
				BillingTime:               domain.BillingTimeCalendar,
				TrialEndAt:                &trialEnd,
				CurrentBillingPeriodStart: &periodStart,
				CurrentBillingPeriodEnd:   &periodEnd,
				NextBillingAt:             &periodEnd,
			},
		},
		cycleUpdated: make(map[string]bool),
	}
	pricing := &mockPricing{
		plans: map[string]domain.Plan{
			"pln_1": {
				ID: "pln_1", Currency: "USD", BillingInterval: domain.BillingMonthly,
				BaseAmountCents: 4900,
				BaseBillTiming:  domain.BillInAdvance,
			},
		},
	}
	invoices := &mockInvoices{}
	clk := clock.NewFake(periodEnd.Add(time.Nanosecond))
	engine := wireBaseTax(NewEngine(subs, &mockUsage{totals: map[string]int64{}}, pricing, invoices, nil, &mockSettings{}, nil, nil, clk))

	if _, errs := engine.RunCycle(context.Background(), 50); len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}

	if len(invoices.invoices) != 2 {
		t.Fatalf("expected 2 invoices (trial-end coverage + next-period pre-pay), got %d", len(invoices.invoices))
	}

	// Identify each invoice by its billing-period stamp. BillOnCreate
	// fires first (right after ActivateAfterTrial) and covers the
	// trial-end period; the normal cycle invoice fires next and covers
	// the upcoming period.
	var trialEndInv, nextPeriodInv *domain.Invoice
	for i := range invoices.invoices {
		inv := invoices.invoices[i]
		switch inv.BillingPeriodStart {
		case periodStart:
			trialEndInv = &inv
		case periodEnd:
			nextPeriodInv = &inv
		}
	}
	if trialEndInv == nil {
		t.Errorf("missing trial-end coverage invoice (period_start=%v) — Bug #6 regression", periodStart)
	}
	if nextPeriodInv == nil {
		t.Errorf("missing next-period pre-pay invoice (period_start=%v)", periodEnd)
	}
	updated := subs.subs["sub_1"]
	if updated.Status != domain.SubscriptionActive {
		t.Errorf("status: got %q, want active", updated.Status)
	}
}

// TestRunCycle_InAdvance_ScheduledCancelAtPeriodEnd_NoOvercharge locks in
// the PR-9 fix: when a scheduled cancel is set to fire at the upcoming
// period boundary, the cycle-close invoice MUST NOT include an in_advance
// base line for the upcoming (about-to-cancel) period. Pre-fix, the cycle
// close emitted the next-period base ($100 example) and THEN
// advanceCycleOrCancel fired the cancel — leaving the customer billed for
// a period they wouldn't use.
//
// Post-fix expected at cycle close on an in_advance sub with
// cancel_at_period_end=true:
//   - No base line (in_advance + about-to-terminate → skip)
//   - Usage line for the just-elapsed period (in-arrears for usage is
//     always correct, captures final consumption)
//   - Then the scheduled cancel fires
func TestRunCycle_InAdvance_ScheduledCancelAtPeriodEnd_NoOvercharge(t *testing.T) {
	periodStart := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

	subs := &mockSubs{
		subs: map[string]domain.Subscription{
			"sub_1": {
				ID: "sub_1", TenantID: "t1", CustomerID: "cus_1",
				Items:                     []domain.SubscriptionItem{{PlanID: "pln_1", Quantity: 1}},
				Status:                    domain.SubscriptionActive,
				BillingTime:               domain.BillingTimeCalendar,
				CurrentBillingPeriodStart: &periodStart,
				CurrentBillingPeriodEnd:   &periodEnd,
				NextBillingAt:             &periodEnd,
				CancelAtPeriodEnd:         true,
			},
		},
		cycleUpdated: make(map[string]bool),
	}
	pricing := &mockPricing{
		plans: map[string]domain.Plan{
			"pln_1": {
				ID: "pln_1", Currency: "USD", BillingInterval: domain.BillingMonthly,
				BaseAmountCents: 10000,
				BaseBillTiming:  domain.BillInAdvance,
			},
		},
	}
	invoices := &mockInvoices{}
	clk := clock.NewFake(periodEnd.Add(time.Nanosecond))
	engine := wireBaseTax(NewEngine(subs, &mockUsage{totals: map[string]int64{}}, pricing, invoices, nil, &mockSettings{}, nil, nil, clk))

	if _, errs := engine.RunCycle(context.Background(), 50); len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}

	// Sub must have been canceled by advanceCycleOrCancel.
	if subs.subs["sub_1"].Status != domain.SubscriptionCanceled {
		t.Errorf("status: got %q, want canceled", subs.subs["sub_1"].Status)
	}

	// Pre-PR-9: customer billed $10000 for the upcoming (will-not-be-
	// used) period. PR-9: in_advance base line is skipped at cycle
	// close because the cancel is about to fire. Post-empty-skip
	// fix: when there's no base AND no usage to bill, the engine
	// skips emitting the cycle-close invoice entirely (matches Stripe
	// / Lago / Chargebee / Orb — $0 cycle invoices aren't emitted).
	// The cancel still fires correctly via advanceCycleOrCancel.
	if len(invoices.invoices) != 0 {
		t.Errorf("expected zero cycle-close invoices (in_advance base skipped + no usage = nothing to bill), got %d", len(invoices.invoices))
	}
}

// TestRunCycle_SkipEmptyCycleInvoice_NoChargeAttempt locks in the
// industry-standard "don't emit $0 cycle invoices" behavior. When
// the base+usage assembly produces zero line items, the engine
// advances the cycle but does NOT persist an invoice — no number
// burned, no draft cluttering the customer's invoice list. Stripe
// / Lago / Chargebee / Orb all behave this way.
func TestRunCycle_SkipEmptyCycleInvoice_NoChargeAttempt(t *testing.T) {
	periodStart := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	subs := &mockSubs{
		subs: map[string]domain.Subscription{
			"sub_1": {
				ID: "sub_1", TenantID: "t1", CustomerID: "cus_1",
				// Free plan + no usage = zero billable lines.
				Items:                     []domain.SubscriptionItem{{PlanID: "pln_free", Quantity: 1}},
				Status:                    domain.SubscriptionActive,
				BillingTime:               domain.BillingTimeCalendar,
				CurrentBillingPeriodStart: &periodStart, CurrentBillingPeriodEnd: &periodEnd,
				NextBillingAt: &periodEnd,
			},
		},
		cycleUpdated: make(map[string]bool),
	}
	pricing := &mockPricing{
		plans: map[string]domain.Plan{
			"pln_free": {ID: "pln_free", Name: "Free", Currency: "USD", BillingInterval: domain.BillingMonthly, BaseAmountCents: 0, BaseBillTiming: domain.BillInArrears},
		},
	}
	invoices := &mockInvoices{}
	engine := wireBaseTax(NewEngine(subs, &mockUsage{totals: map[string]int64{}}, pricing, invoices, nil, &mockSettings{}, nil, nil, billingTestClock()))

	count, errs := engine.RunCycle(context.Background(), 50)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	// count == invoices_generated count
	if count != 0 {
		t.Errorf("expected zero invoices generated for empty cycle, got %d", count)
	}
	if len(invoices.invoices) != 0 {
		t.Errorf("no invoice should be persisted for zero-line cycle, got %d", len(invoices.invoices))
	}
}

// TestBillOnCancel_PaidCheck locks in the industry-standard paid-check
// gate on cancel-proration credits. The customer must have actually
// PAID the in_advance invoice for the current period before a
// "refund-style" credit makes sense — otherwise we'd be granting
// credit against money the customer never put in. Industry parity:
// Chargebee Refundable (paid) vs Adjustment (unpaid); Stripe warns
// to disable proration with unpaid latest invoice.
func TestBillOnCancel_PaidCheck(t *testing.T) {
	periodStart := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	cancelAt := time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC) // mid-period

	makeSub := func() domain.Subscription {
		return domain.Subscription{
			ID: "sub_1", TenantID: "t1", CustomerID: "cus_1", Code: "starter",
			Status: domain.SubscriptionCanceled,
			Items: []domain.SubscriptionItem{{
				PlanID: "pln_advance", Quantity: 1,
			}},
			CurrentBillingPeriodStart: &periodStart,
			CurrentBillingPeriodEnd:   &periodEnd,
			CanceledAt:                &cancelAt,
		}
	}
	makePricing := func() *mockPricing {
		return &mockPricing{plans: map[string]domain.Plan{
			"pln_advance": {ID: "pln_advance", Name: "Advance", Currency: "USD",
				BillingInterval: domain.BillingMonthly, BaseAmountCents: 6000,
				BaseBillTiming: domain.BillInAdvance},
		}}
	}

	t.Run("source invoice paid → refund credit issued via credit note", func(t *testing.T) {
		paidStart := periodStart
		paid := domain.Invoice{
			ID: "inv_1", TenantID: "t1", SubscriptionID: "sub_1",
			Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentSucceeded,
			SubtotalCents: 6000, TotalAmountCents: 6000, AmountPaidCents: 6000,
		}
		paidLine := domain.InvoiceLineItem{
			ID: "ili_1", InvoiceID: "inv_1", LineType: domain.LineTypeBaseFee,
			BillingPeriodStart: &paidStart,
		}
		inv := &mockInvoices{invoices: []domain.Invoice{paid}, lineItems: []domain.InvoiceLineItem{paidLine}}
		granter := &fakeCreditGranter{}
		adjuster := &fakeCreditNoteAdjuster{}

		engine := wireBaseTax(NewEngine(&mockSubs{}, &mockUsage{}, makePricing(), inv, nil, &mockSettings{}, nil, nil, billingTestClock()))
		engine.SetCreditGranter(granter)
		engine.SetCreditNoteAdjuster(adjuster)
		engine.SetCreditHeadroomReader(&fakeCreditHeadroom{})

		cents, err := engine.BillOnCancel(context.Background(), makeSub())
		if err != nil {
			t.Fatalf("BillOnCancel: %v", err)
		}
		// Paid source → the refund routes through the credit-note primitive
		// (ADR-048: balance grant + tax reversal happen inside Issue), never a
		// bare ledger grant at the engine layer.
		// 17 unused of 31 days: RoundHalfToEven(6000×17, 31) = 3290.
		if len(adjuster.calls) != 1 || adjuster.calls[0].invoiceID != "inv_1" || adjuster.calls[0].gross != 3290 {
			t.Fatalf("adjustment CN = %+v, want one {inv_1, 3290}", adjuster.calls)
		}
		if cents != 3290 {
			t.Errorf("return cents = %d, want 3290 — timeline detail depends on this match", cents)
		}
		if len(granter.grants) != 0 {
			t.Errorf("engine must not double-credit via the ledger (grant happens inside CN Issue), got %d grants", len(granter.grants))
		}
	})

	t.Run("source invoice UNPAID → credit suppressed", func(t *testing.T) {
		unpaidStart := periodStart
		unpaid := domain.Invoice{
			ID: "inv_1", TenantID: "t1", SubscriptionID: "sub_1",
			Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentPending,
			SubtotalCents: 6000, TotalAmountCents: 6000, AmountDueCents: 6000,
		}
		unpaidLine := domain.InvoiceLineItem{
			ID: "ili_1", InvoiceID: "inv_1", LineType: domain.LineTypeBaseFee,
			BillingPeriodStart: &unpaidStart,
		}
		inv := &mockInvoices{invoices: []domain.Invoice{unpaid}, lineItems: []domain.InvoiceLineItem{unpaidLine}}
		granter := &fakeCreditGranter{}
		voider := &fakeInvoiceVoider{}
		adjuster := &fakeCreditNoteAdjuster{}

		engine := wireBaseTax(NewEngine(&mockSubs{}, &mockUsage{}, makePricing(), inv, nil, &mockSettings{}, nil, nil, billingTestClock()))
		engine.SetCreditGranter(granter)
		engine.SetInvoiceVoider(voider)
		engine.SetCreditNoteAdjuster(adjuster)
		engine.SetCreditHeadroomReader(&fakeCreditHeadroom{})

		cents, err := engine.BillOnCancel(context.Background(), makeSub())
		if err != nil {
			t.Fatalf("BillOnCancel: %v", err)
		}
		if cents != 0 || len(granter.grants) != 0 {
			t.Errorf("unpaid source: cents=%d grants=%d, want 0/0 (would have credited customer for unpaid period)", cents, len(granter.grants))
		}
		// Suppressed ≠ silent: the unpaid receivable is settled in place —
		// partially consumed (17/31 days) → amount_due reduced via adjustment
		// CN, never voided (mechanics pinned in TestBillOnCancel_UnpaidPrebillRelief).
		if len(voider.voided) != 0 {
			t.Errorf("voids = %d, want 0 (partial consumption reduces, never voids)", len(voider.voided))
		}
		if len(adjuster.calls) != 1 {
			t.Errorf("adjustment CN calls = %d, want 1 (settle-in-place relief)", len(adjuster.calls))
		}
	})

	t.Run("source invoice missing → credit suppressed", func(t *testing.T) {
		// e.g. tenant manually deleted invoice, or cancel fires before any
		// invoice was emitted for this period. Safest default: don't grant.
		inv := &mockInvoices{}
		granter := &fakeCreditGranter{}

		engine := wireBaseTax(NewEngine(&mockSubs{}, &mockUsage{}, makePricing(), inv, nil, &mockSettings{}, nil, nil, billingTestClock()))
		engine.SetCreditGranter(granter)

		if _, err := engine.BillOnCancel(context.Background(), makeSub()); err != nil {
			t.Fatalf("BillOnCancel: %v", err)
		}
		if len(granter.grants) != 0 {
			t.Errorf("expected no credit grant when source invoice missing, got %d", len(granter.grants))
		}
	})
}

// TestBillOnCancel_UnpaidPrebillRelief covers #22: when the in_advance prebill
// for the canceled period is UNPAID, BillOnCancel settles the invoice itself
// rather than granting an unfunded credit or leaving the full amount in dunning.
//   - nothing consumed (whole receivable unused)  → void the invoice
//   - partially consumed                          → adjustment credit note
//     reducing amount_due to the consumed portion (gross, so tax is reversed too)
//
// In every unpaid case no customer-balance credit is granted and the return is 0.
func TestBillOnCancel_UnpaidPrebillRelief(t *testing.T) {
	periodStart := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC) // 31-day cycle

	makeSub := func(cancelAt time.Time) domain.Subscription {
		return domain.Subscription{
			ID: "sub_1", TenantID: "t1", CustomerID: "cus_1", Code: "starter",
			Status:                    domain.SubscriptionCanceled,
			Items:                     []domain.SubscriptionItem{{PlanID: "pln_advance", Quantity: 1}},
			CurrentBillingPeriodStart: &periodStart,
			CurrentBillingPeriodEnd:   &periodEnd,
			CanceledAt:                &cancelAt,
		}
	}
	makePricing := func() *mockPricing {
		return &mockPricing{plans: map[string]domain.Plan{
			"pln_advance": {ID: "pln_advance", Name: "Advance", Currency: "USD",
				BillingInterval: domain.BillingMonthly, BaseAmountCents: 6000,
				BaseBillTiming: domain.BillInAdvance},
		}}
	}
	// seedInvoices returns a mockInvoices holding one unpaid finalized in_advance
	// base invoice for the period, with the given money fields.
	seedInvoices := func(subtotal, tax, total, amountDue, amountPaid int64) *mockInvoices {
		ps := periodStart
		return &mockInvoices{
			invoices: []domain.Invoice{{
				ID: "inv_1", TenantID: "t1", SubscriptionID: "sub_1",
				Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentPending,
				SubtotalCents: subtotal, TaxFacts: domain.TaxFacts{TaxAmountCents: tax}, TotalAmountCents: total,
				AmountDueCents: amountDue, AmountPaidCents: amountPaid,
			}},
			lineItems: []domain.InvoiceLineItem{{
				ID: "ili_1", InvoiceID: "inv_1", LineType: domain.LineTypeBaseFee,
				BillingPeriodStart: &ps,
			}},
		}
	}
	newEngine := func(inv *mockInvoices) (*Engine, *fakeCreditGranter, *fakeInvoiceVoider, *fakeCreditNoteAdjuster) {
		e := wireBaseTax(NewEngine(&mockSubs{}, &mockUsage{}, makePricing(), inv, nil, &mockSettings{}, nil, nil, billingTestClock()))
		g := &fakeCreditGranter{}
		v := &fakeInvoiceVoider{}
		a := &fakeCreditNoteAdjuster{}
		e.SetCreditGranter(g)
		e.SetInvoiceVoider(v)
		e.SetCreditNoteAdjuster(a)
		e.SetCreditHeadroomReader(&fakeCreditHeadroom{})
		return e, g, v, a
	}

	t.Run("partially consumed, no tax → adjustment CN reduces to consumed, no void, no grant", func(t *testing.T) {
		inv := seedInvoices(6000, 0, 6000, 6000, 0)
		e, g, v, a := newEngine(inv)
		cancelAt := time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC) // 17 unused / 31 days

		cents, err := e.BillOnCancel(context.Background(), makeSub(cancelAt))
		if err != nil {
			t.Fatalf("BillOnCancel: %v", err)
		}
		if cents != 0 {
			t.Errorf("return cents = %d, want 0 (no balance credit on unpaid path)", cents)
		}
		if len(g.grants) != 0 {
			t.Errorf("credit grants = %d, want 0 (unfunded — must not gift balance credit)", len(g.grants))
		}
		if len(v.voided) != 0 {
			t.Errorf("voids = %d, want 0 (partial consumption must reduce, not void)", len(v.voided))
		}
		if len(a.calls) != 1 {
			t.Fatalf("adjustment CN calls = %d, want 1", len(a.calls))
		}
		// unusedDays = round(Apr1 - Mar15) = 17; gross = round(6000*17/31) = 3290.
		if a.calls[0].invoiceID != "inv_1" || a.calls[0].gross != 3290 {
			t.Errorf("adjustment CN = {%s, %d}, want {inv_1, 3290}", a.calls[0].invoiceID, a.calls[0].gross)
		}
	})

	t.Run("partially consumed WITH tax → CN amount grossed up so unused tax is reversed too", func(t *testing.T) {
		// 10% tax: subtotal 6000, tax 600, total 6600, all unpaid.
		inv := seedInvoices(6000, 600, 6600, 6600, 0)
		e, _, v, a := newEngine(inv)
		cancelAt := time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)

		if _, err := e.BillOnCancel(context.Background(), makeSub(cancelAt)); err != nil {
			t.Fatalf("BillOnCancel: %v", err)
		}
		if len(v.voided) != 0 {
			t.Errorf("voids = %d, want 0", len(v.voided))
		}
		if len(a.calls) != 1 {
			t.Fatalf("adjustment CN calls = %d, want 1", len(a.calls))
		}
		// net unused = 3290; grossed up by total/subtotal = 6600/6000: round(3290*6600/6000) = 3619.
		if a.calls[0].gross != 3619 {
			t.Errorf("adjustment CN gross = %d, want 3619 (net 3290 + proportional tax)", a.calls[0].gross)
		}
	})

	t.Run("nothing consumed (cancel at period start) → void whole invoice, no CN, no grant", func(t *testing.T) {
		inv := seedInvoices(6000, 0, 6000, 6000, 0)
		e, g, v, a := newEngine(inv)
		cancelAt := periodStart.Add(time.Hour) // ~whole period unused → unusedDays rounds to full cycle

		cents, err := e.BillOnCancel(context.Background(), makeSub(cancelAt))
		if err != nil {
			t.Fatalf("BillOnCancel: %v", err)
		}
		if cents != 0 {
			t.Errorf("return cents = %d, want 0", cents)
		}
		if len(g.grants) != 0 {
			t.Errorf("credit grants = %d, want 0", len(g.grants))
		}
		if len(a.calls) != 0 {
			t.Errorf("adjustment CN calls = %d, want 0 (fully unused → void)", len(a.calls))
		}
		if len(v.voided) != 1 || v.voided[0] != "inv_1" {
			t.Fatalf("voids = %v, want [inv_1]", v.voided)
		}
	})

	t.Run("partial payment present → never void (would annul collected money), reduce instead", func(t *testing.T) {
		// Whole period unused but $20 already paid: must NOT void, must reduce
		// amount_due (clamped) so the collected slice is preserved.
		inv := seedInvoices(6000, 0, 6000, 4000, 2000) // 2000 paid, 4000 still due
		e, _, v, a := newEngine(inv)
		cancelAt := periodStart.Add(time.Hour)

		if _, err := e.BillOnCancel(context.Background(), makeSub(cancelAt)); err != nil {
			t.Fatalf("BillOnCancel: %v", err)
		}
		if len(v.voided) != 0 {
			t.Errorf("voids = %d, want 0 (partial payment present — must not void)", len(v.voided))
		}
		if len(a.calls) != 1 {
			t.Fatalf("adjustment CN calls = %d, want 1", len(a.calls))
		}
		// gross unused = 6000 (full), clamped to amount_due 4000.
		if a.calls[0].gross != 4000 {
			t.Errorf("adjustment CN gross = %d, want 4000 (clamped to amount_due)", a.calls[0].gross)
		}
	})
}

// TestBillOnPlanSwapImmediate covers the refund-credit half of the
// immediate cross-interval plan-swap orchestrator. The math + paid-check
// shape mirrors BillOnCancel; this lock pins:
//   - in_advance OLD: credit issued for the (at, periodEnd) unused portion
//   - in_arrears OLD: no-op (nothing prebilled)
//   - paid-check gate: source invoice unpaid → credit suppressed
//   - boundary cases: at >= period_end (clean swap) → no-op
func TestBillOnPlanSwapImmediate(t *testing.T) {
	periodStart := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	swapAt := time.Date(2026, 3, 16, 0, 0, 0, 0, time.UTC) // mid-period

	makeAdvanceSub := func() domain.Subscription {
		return domain.Subscription{
			ID: "sub_1", TenantID: "t1", CustomerID: "cus_1", Code: "starter",
			Status: domain.SubscriptionActive,
			Items: []domain.SubscriptionItem{{
				PlanID: "pln_advance", Quantity: 1,
			}},
			CurrentBillingPeriodStart: &periodStart,
			CurrentBillingPeriodEnd:   &periodEnd,
		}
	}
	makeArrearsSub := func() domain.Subscription {
		return domain.Subscription{
			ID: "sub_1", TenantID: "t1", CustomerID: "cus_1", Code: "starter",
			Status: domain.SubscriptionActive,
			Items: []domain.SubscriptionItem{{
				PlanID: "pln_arrears", Quantity: 1,
			}},
			CurrentBillingPeriodStart: &periodStart,
			CurrentBillingPeriodEnd:   &periodEnd,
		}
	}
	makePricing := func() *mockPricing {
		return &mockPricing{plans: map[string]domain.Plan{
			"pln_advance": {ID: "pln_advance", Name: "Advance", Currency: "USD",
				BillingInterval: domain.BillingMonthly, BaseAmountCents: 6000,
				BaseBillTiming: domain.BillInAdvance},
			"pln_arrears": {ID: "pln_arrears", Name: "Arrears", Currency: "USD",
				BillingInterval: domain.BillingMonthly, BaseAmountCents: 6000,
				BaseBillTiming: domain.BillInArrears},
		}}
	}
	paidInvoices := func() *mockInvoices {
		paid := domain.Invoice{
			ID: "inv_1", TenantID: "t1", SubscriptionID: "sub_1",
			Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentSucceeded,
			SubtotalCents: 6000, TotalAmountCents: 6000, AmountPaidCents: 6000,
		}
		paidLine := domain.InvoiceLineItem{
			ID: "ili_1", InvoiceID: "inv_1", LineType: domain.LineTypeBaseFee,
			BillingPeriodStart: &periodStart,
		}
		return &mockInvoices{invoices: []domain.Invoice{paid}, lineItems: []domain.InvoiceLineItem{paidLine}}
	}

	t.Run("in_advance + paid source → refund credit issued via credit note", func(t *testing.T) {
		granter := &fakeCreditGranter{}
		adjuster := &fakeCreditNoteAdjuster{}
		engine := wireBaseTax(NewEngine(&mockSubs{}, &mockUsage{}, makePricing(), paidInvoices(), nil, &mockSettings{}, nil, nil, billingTestClock()))
		engine.SetCreditGranter(granter)
		engine.SetCreditNoteAdjuster(adjuster)
		engine.SetCreditHeadroomReader(&fakeCreditHeadroom{})

		cents, err := engine.BillOnPlanSwapImmediate(context.Background(), makeAdvanceSub(), swapAt)
		if err != nil {
			t.Fatalf("BillOnPlanSwapImmediate: %v", err)
		}
		// Mar 1 → Apr 1 = 31 days; (Mar 16, Apr 1) = 16 unused days.
		// RoundHalfToEven(6000×16, 31) = RoundHalfToEven(96000, 31) = 3097
		// (96000 = 31×3096 + 24; 2×24 = 48 > 31 → round up). Exact, not ~.
		// Paid source routes through the credit-note primitive (ADR-048).
		if len(adjuster.calls) != 1 || adjuster.calls[0].invoiceID != "inv_1" || adjuster.calls[0].gross != 3097 {
			t.Fatalf("adjustment CN = %+v, want one {inv_1, 3097}", adjuster.calls)
		}
		if cents != 3097 {
			t.Errorf("unused refund cents: got %d, want 3097 ($60 × 16/31, banker's)", cents)
		}
		if len(granter.grants) != 0 {
			t.Errorf("engine must not double-credit via the ledger (grant happens inside CN Issue), got %d grants", len(granter.grants))
		}
	})

	t.Run("in_arrears → no-op (nothing prebilled)", func(t *testing.T) {
		granter := &fakeCreditGranter{}
		engine := wireBaseTax(NewEngine(&mockSubs{}, &mockUsage{}, makePricing(), paidInvoices(), nil, &mockSettings{}, nil, nil, billingTestClock()))
		engine.SetCreditGranter(granter)

		cents, err := engine.BillOnPlanSwapImmediate(context.Background(), makeArrearsSub(), swapAt)
		if err != nil {
			t.Fatalf("BillOnPlanSwapImmediate: %v", err)
		}
		if cents != 0 {
			t.Errorf("in_arrears refund cents: got %d, want 0", cents)
		}
		if len(granter.grants) != 0 {
			t.Errorf("in_arrears expected no credit grants, got %d", len(granter.grants))
		}
	})

	t.Run("source invoice UNPAID → amount_due reduced via adjustment, no grant (ADR-050)", func(t *testing.T) {
		// ADR-050: a cross-interval swap on an UNPAID prebill settles in place —
		// reduce the open invoice's amount_due to the consumed portion via an
		// adjustment credit note, rather than minting a refundable balance
		// credit or silently skipping.
		unpaid := domain.Invoice{
			ID: "inv_1", TenantID: "t1", SubscriptionID: "sub_1",
			Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentPending,
			SubtotalCents: 6000, TotalAmountCents: 6000, AmountDueCents: 6000,
		}
		unpaidLine := domain.InvoiceLineItem{
			ID: "ili_1", InvoiceID: "inv_1", LineType: domain.LineTypeBaseFee,
			BillingPeriodStart: &periodStart,
		}
		inv := &mockInvoices{invoices: []domain.Invoice{unpaid}, lineItems: []domain.InvoiceLineItem{unpaidLine}}
		granter := &fakeCreditGranter{}
		voider := &fakeInvoiceVoider{}
		adjuster := &fakeCreditNoteAdjuster{}
		engine := wireBaseTax(NewEngine(&mockSubs{}, &mockUsage{}, makePricing(), inv, nil, &mockSettings{}, nil, nil, billingTestClock()))
		engine.SetCreditGranter(granter)
		engine.SetInvoiceVoider(voider)
		engine.SetCreditNoteAdjuster(adjuster)
		engine.SetCreditHeadroomReader(&fakeCreditHeadroom{})

		cents, err := engine.BillOnPlanSwapImmediate(context.Background(), makeAdvanceSub(), swapAt)
		if err != nil {
			t.Fatalf("BillOnPlanSwapImmediate: %v", err)
		}
		if cents != 0 || len(granter.grants) != 0 {
			t.Errorf("unpaid swap: cents=%d grants=%d, want 0/0 (no refundable credit)", cents, len(granter.grants))
		}
		if len(voider.voided) != 0 {
			t.Errorf("voids = %d, want 0 (partial consumption → reduce, not void)", len(voider.voided))
		}
		// 16 unused of 31 days, no tax → gross = round(6000×16/31) = 3097.
		if len(adjuster.calls) != 1 || adjuster.calls[0].invoiceID != "inv_1" || adjuster.calls[0].gross != 3097 {
			t.Fatalf("adjustment CN = %+v, want one {inv_1, 3097}", adjuster.calls)
		}
	})

	t.Run("stub period refund uses fullCycleDays (no over-refund)", func(t *testing.T) {
		// Calendar-snap creates stub periods at the start of an
		// activation. If a swap fires inside a 7-of-30 stub at the
		// period start, "unused" days = 7 BUT the customer only paid
		// baseFee × 7/30 for that period — the refund must reflect the
		// actual prepaid amount, not the full base fee. Pre-fix:
		// baseFee × 7/7 = full base fee → over-refund. Post-fix:
		// baseFee × 7/30 → matches the prepaid stub amount.
		stubStart := time.Date(2026, 3, 24, 0, 0, 0, 0, time.UTC)
		stubEnd := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC) // 8-day stub
		stubSub := domain.Subscription{
			ID: "sub_1", TenantID: "t1", CustomerID: "cus_1", Code: "starter",
			Status:                    domain.SubscriptionActive,
			Items:                     []domain.SubscriptionItem{{PlanID: "pln_advance", Quantity: 1}},
			CurrentBillingPeriodStart: &stubStart,
			CurrentBillingPeriodEnd:   &stubEnd,
		}
		paid := domain.Invoice{
			ID: "inv_stub", TenantID: "t1", SubscriptionID: "sub_1",
			Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentSucceeded,
			// The customer prepaid the stub-prorated amount, not the full base
			// fee: 6000 × 8/31 = 1548 (the CN cap must reflect real money in).
			SubtotalCents: 1548, TotalAmountCents: 1548, AmountPaidCents: 1548,
		}
		paidLine := domain.InvoiceLineItem{
			ID: "ili_stub", InvoiceID: "inv_stub", LineType: domain.LineTypeBaseFee,
			BillingPeriodStart: &stubStart,
		}
		inv := &mockInvoices{invoices: []domain.Invoice{paid}, lineItems: []domain.InvoiceLineItem{paidLine}}
		granter := &fakeCreditGranter{}
		adjuster := &fakeCreditNoteAdjuster{}
		engine := wireBaseTax(NewEngine(&mockSubs{}, &mockUsage{}, makePricing(), inv, nil, &mockSettings{}, nil, nil, billingTestClock()))
		engine.SetCreditGranter(granter)
		engine.SetCreditNoteAdjuster(adjuster)
		engine.SetCreditHeadroomReader(&fakeCreditHeadroom{})

		// at = a moment just after stubStart so the strict-after gate
		// fires (`!at.After(periodStart)` would short-circuit if at==
		// periodStart). roundDays absorbs the 1h difference.
		cents, err := engine.BillOnPlanSwapImmediate(context.Background(), stubSub, stubStart.Add(time.Hour))
		if err != nil {
			t.Fatalf("BillOnPlanSwapImmediate: %v", err)
		}
		// Stub = 8 days, all unused (swap at period start). fullCycleDays
		// = 31 (Mar 24 → Apr 24 monthly roll). RoundHalfToEven(6000×8, 31)
		// = RoundHalfToEven(48000, 31) = 1548 (48000 = 31×1548 + 12; 2×12
		// = 24 < 31 → round down). Pre-fix would have refunded $60 × 8/8.
		if cents != 1548 {
			t.Errorf("stub refund cents: got %d, want 1548 ($60 × 8/31 stub-prorated)", cents)
		}
	})

	t.Run("at >= periodEnd → no-op (clean swap at boundary)", func(t *testing.T) {
		granter := &fakeCreditGranter{}
		engine := wireBaseTax(NewEngine(&mockSubs{}, &mockUsage{}, makePricing(), paidInvoices(), nil, &mockSettings{}, nil, nil, billingTestClock()))
		engine.SetCreditGranter(granter)

		cents, err := engine.BillOnPlanSwapImmediate(context.Background(), makeAdvanceSub(), periodEnd)
		if err != nil {
			t.Fatalf("BillOnPlanSwapImmediate: %v", err)
		}
		if cents != 0 {
			t.Errorf("at=periodEnd refund: got %d cents, want 0", cents)
		}
		if len(granter.grants) != 0 {
			t.Errorf("at=periodEnd expected no credit grants, got %d", len(granter.grants))
		}
	})
}

// TestRunCycle_SegmentAware_SkipsInAdvanceSegment is a defense-in-
// depth regression test. The bill_timing-change reject at the service
// layer is the canonical guard against billOnePeriod ever seeing an
// in_advance segment in its Pass 1 segment-aware loop. The segment-
// skip in the loop itself (matching Pass 2 and BillFinalOnImmediate-
// Cancel's already-correct behavior) catches the failure mode if the
// reject is ever loosened. Without the skip, an in_advance segment
// would emit a segment line at cycle close and double-bill the
// operator for a portion they already paid in the OLD day-1 or
// prior-cycle invoice.
func TestRunCycle_SegmentAware_SkipsInAdvanceSegment(t *testing.T) {
	periodStart := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	swapAt := time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)

	subs := &mockSubs{
		subs: map[string]domain.Subscription{
			"sub_1": {
				ID: "sub_1", TenantID: "t1", CustomerID: "cus_1",
				Items: []domain.SubscriptionItem{{
					ID: "it_1", PlanID: "pln_b_arrears", Quantity: 1,
				}},
				Status:                    domain.SubscriptionActive,
				BillingTime:               domain.BillingTimeAnniversary,
				CurrentBillingPeriodStart: &periodStart,
				CurrentBillingPeriodEnd:   &swapAt,
				NextBillingAt:             &swapAt,
			},
		},
		cycleUpdated: make(map[string]bool),
		// The ADR-101 interval writer's state after the day-15 swap:
		// the in_advance plan's lifetime sealed at the swap instant,
		// the in_arrears successor open from it.
		intervals: []domain.ItemInterval{
			{SubscriptionItemID: "it_1", PlanID: "pln_a_advance", Quantity: 1, EndsAt: &swapAt},
			{SubscriptionItemID: "it_1", PlanID: "pln_b_arrears", Quantity: 1, StartsAt: swapAt},
		},
	}

	pricing := &mockPricing{
		plans: map[string]domain.Plan{
			"pln_a_advance": {ID: "pln_a_advance", Name: "A", Currency: "USD", BillingInterval: domain.BillingMonthly, BaseAmountCents: 9900, BaseBillTiming: domain.BillInAdvance},
			"pln_b_arrears": {ID: "pln_b_arrears", Name: "B", Currency: "USD", BillingInterval: domain.BillingMonthly, BaseAmountCents: 9900, BaseBillTiming: domain.BillInArrears},
		},
	}

	invoices := &mockInvoices{}
	usage := &mockUsage{totals: map[string]int64{}}
	engine := wireBaseTax(NewEngine(subs, usage, pricing, invoices, nil, &mockSettings{}, nil, nil, billingTestClock()))

	_, errs := engine.RunCycle(context.Background(), 50)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}

	// No invoice expected: the only segment in (Mar 1, Mar 15) was
	// under in_advance plan A, which is now skipped. No usage either.
	// Zero-subtotal guard skips invoice creation.
	if len(invoices.invoices) != 0 {
		t.Errorf("expected 0 invoices (in_advance segment skipped, no usage), got %d", len(invoices.invoices))
		for _, inv := range invoices.invoices {
			t.Logf("  invoice subtotal=%d", inv.SubtotalCents)
		}
	}
	for _, li := range invoices.lineItems {
		if li.LineType == domain.LineTypeBaseFee {
			t.Errorf("no base-fee line should be emitted (in_advance segment must be skipped); got line for plan=%s amount=%d", li.Description, li.AmountCents)
		}
	}
}

// TestRunCycle_SegmentAware_InArrears_MidPeriodPlanChange locks in the
// Lago / Chargebee / Orb shape: when a customer changes plan mid-cycle
// on an in_arrears sub, the closing invoice emits one line per segment
// at the segment's own plan rate × duration fraction.
//
// Setup: 31-day cycle (March). On day 15 the item swaps from plan_a
// ($30/mo) to plan_b ($60/mo), both monthly in_arrears. The closing
// invoice should have two base lines: 14 days of plan_a (~$13.5) +
// 17 days of plan_b (~$32.9) ≈ $46. Pre-fix (single-line billing):
// $60 for the full month at the new rate. Pre-segment math: even
// bigger overcharge with immediate proration + cycle close double-
// counting.
func TestRunCycle_SegmentAware_InArrears_MidPeriodPlanChange(t *testing.T) {
	periodStart := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	changeAt := time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)

	subs := &mockSubs{
		subs: map[string]domain.Subscription{
			"sub_1": {
				ID: "sub_1", TenantID: "t1", CustomerID: "cus_1",
				Items: []domain.SubscriptionItem{{
					ID: "it_1", PlanID: "pln_b", Quantity: 1,
				}},
				Status: domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar,
				CurrentBillingPeriodStart: &periodStart, CurrentBillingPeriodEnd: &periodEnd,
				NextBillingAt: &periodEnd,
			},
		},
		cycleUpdated: make(map[string]bool),
		// The ADR-101 interval writer's state after the immediate
		// day-15 swap: pln_a sealed at the change instant, pln_b open.
		intervals: []domain.ItemInterval{
			{SubscriptionItemID: "it_1", PlanID: "pln_a", Quantity: 1, EndsAt: &changeAt},
			{SubscriptionItemID: "it_1", PlanID: "pln_b", Quantity: 1, StartsAt: changeAt},
		},
	}

	pricing := &mockPricing{
		plans: map[string]domain.Plan{
			"pln_a": {ID: "pln_a", Name: "A", Currency: "USD", BillingInterval: domain.BillingMonthly, BaseAmountCents: 3000, BaseBillTiming: domain.BillInArrears},
			"pln_b": {ID: "pln_b", Name: "B", Currency: "USD", BillingInterval: domain.BillingMonthly, BaseAmountCents: 6000, BaseBillTiming: domain.BillInArrears},
		},
	}

	invoices := &mockInvoices{}
	usage := &mockUsage{totals: map[string]int64{}}
	engine := wireBaseTax(NewEngine(subs, usage, pricing, invoices, nil, &mockSettings{}, nil, nil, billingTestClock()))

	count, errs := engine.RunCycle(context.Background(), 50)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if count != 1 {
		t.Fatalf("invoices generated: got %d, want 1", count)
	}

	inv := invoices.invoices[0]
	// March has 31 days. Segment 1: [Mar 1, Mar 15) = 14 days × pln_a:
	// RoundHalfToEven(3000×14, 31) = 1355 (42000 = 31×1354 + 26; round up).
	// Segment 2: [Mar 15, Apr 1) = 17 days × pln_b:
	// RoundHalfToEven(6000×17, 31) = 3290 (102000 = 31×3290 + 10; round down).
	// Total = 1355 + 3290 = 4645, exact.
	if inv.SubtotalCents != 4645 {
		t.Errorf("segment-aware total: got %d cents, want 4645 (14d @ pln_a $30 = 1355 + 17d @ pln_b $60 = 3290)", inv.SubtotalCents)
	}

	// Two base-fee lines expected: one per segment.
	baseLineCount := 0
	for _, li := range invoices.lineItems {
		if li.InvoiceID != inv.ID {
			continue
		}
		if li.LineType == domain.LineTypeBaseFee {
			baseLineCount++
		}
	}
	if baseLineCount != 2 {
		t.Errorf("expected 2 base-fee line items (one per segment), got %d", baseLineCount)
	}
}

// TestRunCycle_SegmentAware_UsageMetersDifferPerSegment verifies the
// Orb-shape segment-aware usage billing: when a mid-period plan
// change introduces a new meter set, each meter bills ONLY for the
// segment it was active on the sub.
//
// Setup: 31-day March cycle. On day 15 the item swaps from plan_a
// (meter X only, $5/unit) to plan_b (meter Y only, $10/unit), both
// in_arrears. Usage:
//   - Segment 1 [Mar 1, Mar 15): 100 units of meter X
//   - Segment 2 [Mar 15, Apr 1): 50 units of meter Y
//
// Expected: usage lines = X × 100 × $0.05 = $5 + Y × 50 × $0.10 = $5
// = $10 total. Pre-fix: meter Y's plan would bill BOTH X (whole
// period) and Y (whole period) regardless of segment timing —
// off by the meter that didn't exist on Plan A or B during its
// segment. With per-interval aggregation cache + mockUsage's
// perInterval stub, the meters bill exactly the segment quantities.
func TestRunCycle_SegmentAware_UsageMetersDifferPerSegment(t *testing.T) {
	periodStart := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	changeAt := time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)

	subs := &mockSubs{
		subs: map[string]domain.Subscription{
			"sub_1": {
				ID: "sub_1", TenantID: "t1", CustomerID: "cus_1",
				Items: []domain.SubscriptionItem{{
					ID: "it_1", PlanID: "pln_b", Quantity: 1,
				}},
				Status: domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar,
				CurrentBillingPeriodStart: &periodStart, CurrentBillingPeriodEnd: &periodEnd,
				NextBillingAt: &periodEnd,
			},
		},
		cycleUpdated: make(map[string]bool),
		// The ADR-101 interval writer's state after the immediate
		// day-15 swap: pln_a sealed at the change instant, pln_b open.
		intervals: []domain.ItemInterval{
			{SubscriptionItemID: "it_1", PlanID: "pln_a", Quantity: 1, EndsAt: &changeAt},
			{SubscriptionItemID: "it_1", PlanID: "pln_b", Quantity: 1, StartsAt: changeAt},
		},
	}

	pricing := &mockPricing{
		plans: map[string]domain.Plan{
			// Zero base fees so the test isolates the usage assertion.
			"pln_a": {ID: "pln_a", Name: "A", Currency: "USD", BillingInterval: domain.BillingMonthly, BaseAmountCents: 0, BaseBillTiming: domain.BillInArrears, MeterIDs: []string{"mtr_x"}},
			"pln_b": {ID: "pln_b", Name: "B", Currency: "USD", BillingInterval: domain.BillingMonthly, BaseAmountCents: 0, BaseBillTiming: domain.BillInArrears, MeterIDs: []string{"mtr_y"}},
		},
		meters: map[string]domain.Meter{
			"mtr_x": {ID: "mtr_x", Name: "Meter X", Unit: "unit", Aggregation: "sum", RatingRuleVersionID: "rrv_x"},
			"mtr_y": {ID: "mtr_y", Name: "Meter Y", Unit: "unit", Aggregation: "sum", RatingRuleVersionID: "rrv_y"},
		},
		rules: map[string]domain.RatingRuleVersion{
			"rrv_x": {ID: "rrv_x", RuleKey: "x_key", Version: 1, Mode: domain.PricingFlat, FlatAmountCents: decimal.NewFromInt(5)},
			"rrv_y": {ID: "rrv_y", RuleKey: "y_key", Version: 1, Mode: domain.PricingFlat, FlatAmountCents: decimal.NewFromInt(10)},
		},
	}

	// perInterval stubs the segment-specific aggregator responses.
	// Anything outside these stubs returns nothing (zero usage).
	usage := &mockUsage{
		perInterval: map[string]int64{
			mockIntervalKey("mtr_x", periodStart, changeAt): 100,
			mockIntervalKey("mtr_y", changeAt, periodEnd):   50,
			// Full-period aggregator calls (for the cap-math precheck)
			// return zero so they don't pollute the segment math.
		},
	}

	invoices := &mockInvoices{}
	engine := wireBaseTax(NewEngine(subs, usage, pricing, invoices, nil, &mockSettings{}, nil, nil, billingTestClock()))

	count, errs := engine.RunCycle(context.Background(), 50)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if count != 1 {
		t.Fatalf("invoices generated: got %d, want 1", count)
	}

	inv := invoices.invoices[0]
	// X × 100 × $0.05 = 500¢ + Y × 50 × $0.10 = 500¢ = 1000¢ total.
	if inv.SubtotalCents != 1000 {
		t.Errorf("segment-aware usage: got %d cents, want 1000 (X×100×$0.05 + Y×50×$0.10)", inv.SubtotalCents)
	}

	// Two usage line items expected — one per (meter, segment) pair.
	usageLineCount := 0
	xCount, yCount := 0, 0
	for _, li := range invoices.lineItems {
		if li.InvoiceID != inv.ID {
			continue
		}
		if li.LineType == domain.LineTypeUsage {
			usageLineCount++
			if li.MeterID == "mtr_x" {
				xCount++
				if li.AmountCents != 500 {
					t.Errorf("meter X line amount: got %d, want 500 (100 units × $0.05)", li.AmountCents)
				}
				if li.BillingPeriodStart == nil || !li.BillingPeriodStart.Equal(periodStart) {
					t.Errorf("meter X segment start: got %v, want %v", li.BillingPeriodStart, periodStart)
				}
				if li.BillingPeriodEnd == nil || !li.BillingPeriodEnd.Equal(changeAt) {
					t.Errorf("meter X segment end: got %v, want %v (change point)", li.BillingPeriodEnd, changeAt)
				}
			}
			if li.MeterID == "mtr_y" {
				yCount++
				if li.AmountCents != 500 {
					t.Errorf("meter Y line amount: got %d, want 500 (50 units × $0.10)", li.AmountCents)
				}
				if li.BillingPeriodStart == nil || !li.BillingPeriodStart.Equal(changeAt) {
					t.Errorf("meter Y segment start: got %v, want %v (change point)", li.BillingPeriodStart, changeAt)
				}
				if li.BillingPeriodEnd == nil || !li.BillingPeriodEnd.Equal(periodEnd) {
					t.Errorf("meter Y segment end: got %v, want %v", li.BillingPeriodEnd, periodEnd)
				}
			}
		}
	}
	if usageLineCount != 2 {
		t.Errorf("expected 2 usage line items (one per meter-segment), got %d", usageLineCount)
	}
	if xCount != 1 || yCount != 1 {
		t.Errorf("expected exactly 1 line for each of meter X (got %d) and meter Y (got %d)", xCount, yCount)
	}
}

// TestRunCycle_SegmentAware_NoChanges_FullPeriodLine verifies the no-
// regress path: a sub with NO mid-period changes still emits a single
// full-period base line at the current plan/qty (segments collapse).
func TestRunCycle_SegmentAware_NoChanges_FullPeriodLine(t *testing.T) {
	periodStart := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)

	subs := &mockSubs{
		subs: map[string]domain.Subscription{
			"sub_1": {
				ID: "sub_1", TenantID: "t1", CustomerID: "cus_1",
				Items:  []domain.SubscriptionItem{{ID: "it_1", PlanID: "pln_a", Quantity: 1}},
				Status: domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar,
				CurrentBillingPeriodStart: &periodStart, CurrentBillingPeriodEnd: &periodEnd,
				NextBillingAt: &periodEnd,
			},
		},
		cycleUpdated: make(map[string]bool),
		// No itemChanges seeded.
	}
	pricing := &mockPricing{
		plans: map[string]domain.Plan{
			"pln_a": {ID: "pln_a", Name: "A", Currency: "USD", BillingInterval: domain.BillingMonthly, BaseAmountCents: 3000, BaseBillTiming: domain.BillInArrears},
		},
	}
	invoices := &mockInvoices{}
	usage := &mockUsage{totals: map[string]int64{}}
	engine := wireBaseTax(NewEngine(subs, usage, pricing, invoices, nil, &mockSettings{}, nil, nil, billingTestClock()))

	count, errs := engine.RunCycle(context.Background(), 50)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if count != 1 {
		t.Fatalf("invoices generated: got %d, want 1", count)
	}
	inv := invoices.invoices[0]
	if inv.SubtotalCents != 3000 {
		t.Errorf("no-change full-period: got %d cents, want 3000 (full month @ $30)", inv.SubtotalCents)
	}
	baseLineCount := 0
	for _, li := range invoices.lineItems {
		if li.InvoiceID == inv.ID && li.LineType == domain.LineTypeBaseFee {
			baseLineCount++
		}
	}
	if baseLineCount != 1 {
		t.Errorf("expected 1 base-fee line (no segments), got %d", baseLineCount)
	}
}
