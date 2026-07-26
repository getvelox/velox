package billing

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
)

// thresholdMockSubs extends mockSubs with a candidate filter so the
// scan-tick test can exercise the candidate-fetch path without coupling to
// mockSubs's status-based GetDueBilling filter.
type thresholdMockSubs struct {
	*mockSubs
	candidates []domain.Subscription
}

func (m *thresholdMockSubs) ListWithThresholds(_ context.Context, _ bool, afterID string, limit int) ([]domain.Subscription, error) {
	// Page the candidate set by the id cursor, mirroring the store's drain
	// contract (ORDER BY id ASC, id > afterID, LIMIT) so the ScanThresholds
	// drain loop is exercised with real paging.
	sorted := append([]domain.Subscription(nil), m.candidates...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	var out []domain.Subscription
	for _, c := range sorted {
		if c.ID <= afterID {
			continue
		}
		out = append(out, c)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

// ListWithThresholdsForClock — ADR-029 Phase 3 stub. The threshold-
// scan unit tests in this file exercise the cron path; the per-clock
// path's behaviour is identical (same scan logic, different fetch
// scope), so a no-op satisfies the interface for the cron-side tests.
func (m *thresholdMockSubs) ListWithThresholdsForClock(_ context.Context, _, _, _ string, _ int) ([]domain.Subscription, error) {
	return nil, nil
}

// setupThresholdEngine builds a minimal engine wired with mocks that mirror the
// natural-cycle setupEngine but with a single-meter, single-rule plan so the
// running subtotal under test is unambiguous. Returns the engine, the wrapping
// mockSubs (for cycle-advance assertion), and the mock invoice store.
func setupThresholdEngine(thresholds *domain.BillingThresholds, usageQty int64) (*Engine, *thresholdMockSubs, *mockInvoices) {
	periodStart := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	nextBilling := periodEnd

	sub := domain.Subscription{
		ID: "sub_1", TenantID: "t1", CustomerID: "cus_1",
		Items: []domain.SubscriptionItem{
			{ID: "subitem_1", PlanID: "pln_1", Quantity: 1},
		},
		Status:                    domain.SubscriptionActive,
		BillingTime:               domain.BillingTimeCalendar,
		CurrentBillingPeriodStart: &periodStart,
		CurrentBillingPeriodEnd:   &periodEnd,
		NextBillingAt:             &nextBilling,
		BillingThresholds:         thresholds,
	}

	base := &mockSubs{
		subs:         map[string]domain.Subscription{"sub_1": sub},
		cycleUpdated: make(map[string]bool),
	}
	subs := &thresholdMockSubs{
		mockSubs:   base,
		candidates: []domain.Subscription{sub},
	}

	usage := &mockUsage{
		totals: map[string]int64{
			"mtr_api": usageQty,
		},
	}

	pricing := &mockPricing{
		plans: map[string]domain.Plan{
			"pln_1": {
				ID: "pln_1", Name: "Pro Plan", Currency: "USD",
				BillingInterval: domain.BillingMonthly,
				BaseAmountCents: 4900,
				MeterIDs:        []string{"mtr_api"},
			},
		},
		meters: map[string]domain.Meter{
			"mtr_api": {ID: "mtr_api", Name: "API Calls", Unit: "calls", RatingRuleVersionID: "rrv_api"},
		},
		rules: map[string]domain.RatingRuleVersion{
			"rrv_api": {
				ID: "rrv_api", RuleKey: "api_calls", Version: 1, Mode: domain.PricingFlat,
				FlatAmountCents: decimal.NewFromInt(100), // 1 cent / call (in basis-of-100 — 100 = $1)
			},
		},
	}

	invoices := &mockInvoices{}

	engine := wireBaseTax(NewEngine(subs, usage, pricing, invoices, nil, &mockSettings{}, nil, nil, nil))
	engine.SetTxRunner(&fakeTxRunner{})
	return engine, subs, invoices
}

// TestEvaluateThresholds_AmountCross verifies the running-subtotal check —
// usage * unit_amount + base_fee crossing AmountGTE flips CrossedAny and
// CrossedAmount. The minimum bar to ship; if this drifts the engine would
// either over- or under-fire.
func TestEvaluateThresholds_AmountCross(t *testing.T) {
	// Plan: $49 base + 1000 calls @ $1 each = $1049 (104900 cents).
	// Threshold: $1000 (100000 cents) → crossed.
	// reset=false so the FULL in_arrears base counts (byte-identical legacy
	// arm); reset=true prorates the base — covered by the ResetProratesBase
	// tests below.
	thresholds := &domain.BillingThresholds{
		AmountGTE:         100000,
		ResetBillingCycle: false,
	}
	engine, _, _ := setupThresholdEngine(thresholds, 1000)

	sub, _ := engine.subs.Get(context.Background(), "t1", "sub_1")
	periodStart := *sub.CurrentBillingPeriodStart
	now := periodStart.AddDate(0, 0, 5) // 5 days into the cycle

	eval, err := engine.evaluateThresholds(context.Background(), sub, periodStart, now)
	if err != nil {
		t.Fatalf("evaluateThresholds: %v", err)
	}

	if !eval.CrossedAny {
		t.Error("expected CrossedAny to be true")
	}
	if !eval.CrossedAmount {
		t.Error("expected CrossedAmount to be true")
	}
	if eval.CrossedItem {
		t.Error("CrossedItem should be false (no item caps configured)")
	}
	// $49 base (4900) + 1000 calls × 100 cents/call (100000) = 104900 cents
	wantSubtotal := int64(4900 + 1000*100)
	if eval.RunningSubtotal != wantSubtotal {
		t.Errorf("running subtotal: got %d, want %d", eval.RunningSubtotal, wantSubtotal)
	}
	if eval.InvoiceCurrency != "USD" {
		t.Errorf("currency: got %q, want USD", eval.InvoiceCurrency)
	}
	if len(eval.LineItems) == 0 {
		t.Error("expected non-empty line items")
	}
}

// TestEvaluateThresholds_BelowAmount asserts no fire when running subtotal
// is below the configured cap. The natural cycle would still bill at end —
// the threshold scan just stays out of the way.
func TestEvaluateThresholds_BelowAmount(t *testing.T) {
	// Plan: $49 base + 100 calls × 100 cents = 14900 cents. Threshold: 100000 → not crossed.
	thresholds := &domain.BillingThresholds{
		AmountGTE:         100000,
		ResetBillingCycle: true,
	}
	engine, _, _ := setupThresholdEngine(thresholds, 100)

	sub, _ := engine.subs.Get(context.Background(), "t1", "sub_1")
	periodStart := *sub.CurrentBillingPeriodStart
	now := periodStart.AddDate(0, 0, 5)

	eval, err := engine.evaluateThresholds(context.Background(), sub, periodStart, now)
	if err != nil {
		t.Fatalf("evaluateThresholds: %v", err)
	}

	if eval.CrossedAny {
		t.Error("expected CrossedAny to be false")
	}
	if eval.CrossedAmount {
		t.Error("expected CrossedAmount to be false")
	}
}

// TestEvaluateThresholds_ItemCross verifies the per-item quantity-cap path.
// Sums quantities across each item's plan meters during the partial cycle —
// any single item crossing fires.
func TestEvaluateThresholds_ItemCross(t *testing.T) {
	thresholds := &domain.BillingThresholds{
		ResetBillingCycle: true,
		ItemThresholds: []domain.SubscriptionItemThreshold{
			{SubscriptionItemID: "subitem_1", UsageGTE: decimal.NewFromInt(1000)},
		},
	}
	engine, _, _ := setupThresholdEngine(thresholds, 1500) // qty 1500 > cap 1000

	sub, _ := engine.subs.Get(context.Background(), "t1", "sub_1")
	periodStart := *sub.CurrentBillingPeriodStart
	now := periodStart.AddDate(0, 0, 5)

	eval, err := engine.evaluateThresholds(context.Background(), sub, periodStart, now)
	if err != nil {
		t.Fatalf("evaluateThresholds: %v", err)
	}

	if !eval.CrossedAny {
		t.Error("expected CrossedAny to be true (item crossed)")
	}
	if !eval.CrossedItem {
		t.Error("expected CrossedItem to be true")
	}
	if eval.CrossedItemID != "subitem_1" {
		t.Errorf("crossed item id: got %q, want subitem_1", eval.CrossedItemID)
	}
	if eval.CrossedAmount {
		t.Error("CrossedAmount should be false (no amount cap configured)")
	}
}

// TestEvaluateThresholds_BelowItemQuantity confirms the per-item path also
// respects the < cap case so we never spuriously fire.
func TestEvaluateThresholds_BelowItemQuantity(t *testing.T) {
	thresholds := &domain.BillingThresholds{
		ResetBillingCycle: true,
		ItemThresholds: []domain.SubscriptionItemThreshold{
			{SubscriptionItemID: "subitem_1", UsageGTE: decimal.NewFromInt(1000)},
		},
	}
	engine, _, _ := setupThresholdEngine(thresholds, 500)

	sub, _ := engine.subs.Get(context.Background(), "t1", "sub_1")
	periodStart := *sub.CurrentBillingPeriodStart
	now := periodStart.AddDate(0, 0, 5)

	eval, err := engine.evaluateThresholds(context.Background(), sub, periodStart, now)
	if err != nil {
		t.Fatalf("evaluateThresholds: %v", err)
	}

	if eval.CrossedAny {
		t.Error("expected CrossedAny to be false")
	}
}

// TestEvaluateThresholds_ResetProratesBase locks fix 4 (ADR-066): with
// reset_billing_cycle=true the fire re-anchors the cycle, so the cycle close
// never true-ups this window's base — the threshold invoice is the base bill,
// and it must carry only the ELAPSED fraction. Pre-fix the full month's base
// rode every fire (a sub crossing 3x/month paid base 3x). The whole line
// triple must be rewritten (emitBaseSegmentLine parity), not just the amount.
func TestEvaluateThresholds_ResetProratesBase(t *testing.T) {
	thresholds := &domain.BillingThresholds{
		AmountGTE:         100000,
		ResetBillingCycle: true,
	}
	engine, _, _ := setupThresholdEngine(thresholds, 1000)

	sub, _ := engine.subs.Get(context.Background(), "t1", "sub_1")
	periodStart := *sub.CurrentBillingPeriodStart // Apr 1; monthly ⇒ 30-day full cycle
	now := periodStart.AddDate(0, 0, 9)           // 9 days elapsed

	eval, err := engine.evaluateThresholds(context.Background(), sub, periodStart, now)
	if err != nil {
		t.Fatalf("evaluateThresholds: %v", err)
	}

	// Base 4900 × 9/30 = 1470 exactly (RoundHalfToEven). Usage unchanged.
	wantBase := int64(1470)
	if eval.RunningSubtotal != 100000+wantBase {
		t.Errorf("running subtotal: got %d, want %d (prorated base feeds the cap)", eval.RunningSubtotal, 100000+wantBase)
	}
	var base *domain.InvoiceLineItem
	for i := range eval.LineItems {
		if eval.LineItems[i].LineType == domain.LineTypeBaseFee {
			base = &eval.LineItems[i]
		}
	}
	if base == nil {
		t.Fatal("expected a base_fee line")
	}
	if base.AmountCents != wantBase || base.TotalAmountCents != wantBase {
		t.Errorf("base amount = %d/%d, want %d", base.AmountCents, base.TotalAmountCents, wantBase)
	}
	// qty × unit must reconcile with amount on every render surface.
	if base.UnitAmountCents != wantBase {
		t.Errorf("base unit = %d, want %d (qty 1 — unit must be recomputed, not the full plan price)", base.UnitAmountCents, wantBase)
	}
	if !strings.Contains(base.Description, "prorated 9/30 days") {
		t.Errorf("base description = %q, want the standard '(prorated 9/30 days)' suffix", base.Description)
	}
}

// TestEvaluateThresholds_ResetProration_StubPeriodDenominator locks the
// denominator convention: the FULL plan interval advanced from periodStart —
// never the current period length (domain/subscription.go invariant). A
// re-anchored stub period (a prior reset=true fire left [Apr 10, May 1), 21
// days) must still prorate a second fire against the plan's 30-day month:
// 10 elapsed days = 4900×10/30 = 1633, NOT 4900×10/21 = 2333 (a 43% base
// over-bill that compounds per fire).
func TestEvaluateThresholds_ResetProration_StubPeriodDenominator(t *testing.T) {
	thresholds := &domain.BillingThresholds{
		AmountGTE:         50000,
		ResetBillingCycle: true,
	}
	engine, subs, _ := setupThresholdEngine(thresholds, 1000)

	// Re-anchor the sub to a 21-day stub [Apr 10, May 1).
	stubStart := time.Date(2026, 4, 10, 0, 0, 0, 0, time.UTC)
	stubEnd := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	sub := subs.subs["sub_1"]
	sub.CurrentBillingPeriodStart = &stubStart
	sub.CurrentBillingPeriodEnd = &stubEnd
	subs.subs["sub_1"] = sub

	now := stubStart.AddDate(0, 0, 10) // Apr 20 — 10 days into the stub

	eval, err := engine.evaluateThresholds(context.Background(), sub, stubStart, now)
	if err != nil {
		t.Fatalf("evaluateThresholds: %v", err)
	}
	var base *domain.InvoiceLineItem
	for i := range eval.LineItems {
		if eval.LineItems[i].LineType == domain.LineTypeBaseFee {
			base = &eval.LineItems[i]
		}
	}
	if base == nil {
		t.Fatal("expected a base_fee line")
	}
	// advanceBillingPeriod(Apr 10, monthly) = May 10 ⇒ fullCycleDays = 30.
	want := int64(1633) // RoundHalfToEven(4900×10, 30)
	if base.AmountCents != want {
		t.Errorf("stub-period prorated base = %d, want %d (denominator must be the full 30-day interval, not the 21-day stub)", base.AmountCents, want)
	}
	if !strings.Contains(base.Description, "prorated 10/30 days") {
		t.Errorf("base description = %q, want 'prorated 10/30 days'", base.Description)
	}
}

// TestEvaluateThresholds_ResetProration_SwapMutatedAnchorIgnored locks the
// #581 anchor convention onto the threshold mirror: the denominator's anchor
// derives from THE PERIOD BEING BILLED (domain.AnchorDayFor(periodStart)),
// never the sub's live billing_anchor_day. A cross-interval swap rewrites
// the sub anchor before the old window closes; #581 fixed the four
// emitBaseSegmentLine billers but this fifth copy of the math still read
// the mutated anchor and stretched (or collapsed) the fire invoice's
// denominator — the same "prorated 15/46 days" leak class.
func TestEvaluateThresholds_ResetProration_SwapMutatedAnchorIgnored(t *testing.T) {
	thresholds := &domain.BillingThresholds{
		AmountGTE:         100000,
		ResetBillingCycle: true,
	}
	engine, subs, _ := setupThresholdEngine(thresholds, 1000)

	// Mimic the post-swap state: the sub-level anchor now points at day
	// 15 while the period being billed still runs [Apr 1, May 1).
	sub := subs.subs["sub_1"]
	sub.BillingAnchorDay = 15
	subs.subs["sub_1"] = sub

	periodStart := *sub.CurrentBillingPeriodStart
	now := periodStart.AddDate(0, 0, 9) // 9 days elapsed

	eval, err := engine.evaluateThresholds(context.Background(), sub, periodStart, now)
	if err != nil {
		t.Fatalf("evaluateThresholds: %v", err)
	}
	var base *domain.InvoiceLineItem
	for i := range eval.LineItems {
		if eval.LineItems[i].LineType == domain.LineTypeBaseFee {
			base = &eval.LineItems[i]
		}
	}
	if base == nil {
		t.Fatal("expected a base_fee line")
	}
	// The period's own anchor (calendar, Apr 1) gives a 30-day cycle:
	// RoundHalfToEven(4900×9, 30) = 1470 — identical to the unmutated
	// case. Reading sub.BillingAnchorDay=15 instead stretches the
	// denominator to Apr 1 → May 15 = 44 days (1002, an under-bill).
	want := int64(1470)
	if base.AmountCents != want {
		t.Errorf("prorated base = %d, want %d (anchor must derive from the period, not the swap-mutated sub anchor)", base.AmountCents, want)
	}
	if !strings.Contains(base.Description, "prorated 9/30 days") {
		t.Errorf("base description = %q, want 'prorated 9/30 days'", base.Description)
	}
}

// TestEvaluateThresholds_ResetProration_CrossIntervalDenominator: a
// yearly-interval item riding a monthly-period sub (cross-interval swaps are
// allowed) must prorate its base against ITS OWN 365-day cycle, exactly as
// emitBaseSegmentLine does per line — 36500×9/365 = 900, not 36500×9/30 =
// 10950 (a 12x over-bill).
func TestEvaluateThresholds_ResetProration_CrossIntervalDenominator(t *testing.T) {
	thresholds := &domain.BillingThresholds{
		AmountGTE:         50000,
		ResetBillingCycle: true,
	}
	engine, _, _ := setupThresholdEngine(thresholds, 1000)
	mp := engine.pricing.(*mockPricing)
	pln := mp.plans["pln_1"]
	pln.BillingInterval = domain.BillingYearly
	pln.BaseAmountCents = 36500
	mp.plans["pln_1"] = pln

	sub, _ := engine.subs.Get(context.Background(), "t1", "sub_1")
	periodStart := *sub.CurrentBillingPeriodStart
	now := periodStart.AddDate(0, 0, 9)

	eval, err := engine.evaluateThresholds(context.Background(), sub, periodStart, now)
	if err != nil {
		t.Fatalf("evaluateThresholds: %v", err)
	}
	var base *domain.InvoiceLineItem
	for i := range eval.LineItems {
		if eval.LineItems[i].LineType == domain.LineTypeBaseFee {
			base = &eval.LineItems[i]
		}
	}
	if base == nil {
		t.Fatal("expected a base_fee line")
	}
	want := int64(900) // RoundHalfToEven(36500×9, 365)
	if base.AmountCents != want {
		t.Errorf("cross-interval prorated base = %d, want %d (denominator = the LINE plan's own interval)", base.AmountCents, want)
	}
}

// TestScanThresholds_ResetAdvanceFailure_IsLoud locks the ADR-066 error
// contract: a reset=true fire whose cycle re-anchor fails must surface an
// ERROR from the scan (the whole tx rolls back and the next tick retries).
// Pre-fix the failure arm logged and returned (fired=true, nil) — and because
// the invoice had already committed, the fire-once probe blocked every retry,
// stranding the reset forever.
func TestScanThresholds_ResetAdvanceFailure_IsLoud(t *testing.T) {
	thresholds := &domain.BillingThresholds{AmountGTE: 50000, ResetBillingCycle: true}
	engine, subs, _ := setupThresholdEngine(thresholds, 1000)
	engine.clock = clock.NewFake(time.Date(2026, 4, 15, 0, 0, 0, 0, time.UTC))
	subs.updateBillingCycleErr = fmt.Errorf("injected advance failure")

	fired, errs := engine.ScanThresholds(context.Background(), 50)
	if fired != 0 {
		t.Errorf("fired = %d, want 0 (a failed reset fire must not count)", fired)
	}
	if len(errs) == 0 {
		t.Fatal("scan swallowed the cycle-advance failure; want a loud per-sub error (retryable next tick)")
	}
}

// TestScanThresholds_ResetWithoutTxRunner_FailsLoud: the engine must refuse a
// reset=true fire when the coordinator-tx seam is missing rather than degrade
// to the non-atomic two-write shape (no silent fallbacks).
func TestScanThresholds_ResetWithoutTxRunner_FailsLoud(t *testing.T) {
	thresholds := &domain.BillingThresholds{AmountGTE: 50000, ResetBillingCycle: true}
	engine, _, invoices := setupThresholdEngine(thresholds, 1000)
	engine.clock = clock.NewFake(time.Date(2026, 4, 15, 0, 0, 0, 0, time.UTC))
	engine.txRunner = nil

	fired, errs := engine.ScanThresholds(context.Background(), 50)
	if fired != 0 || len(errs) == 0 {
		t.Fatalf("fired=%d errs=%v; want 0 fired + a loud tx-runner-required error", fired, errs)
	}
	if len(invoices.invoices) != 0 {
		t.Fatalf("invoice created without the atomic seam: %d", len(invoices.invoices))
	}
}

// setupThresholdEngineWithTiming mirrors setupThresholdEngine but lets the
// caller set the plan's BaseBillTiming so the in_advance double-bill guard
// can be exercised directly.
func setupThresholdEngineWithTiming(thresholds *domain.BillingThresholds, usageQty int64, baseTiming domain.BillTiming) (*Engine, *thresholdMockSubs, *mockInvoices) {
	engine, subs, invoices := setupThresholdEngine(thresholds, usageQty)
	mp, ok := engine.pricing.(*mockPricing)
	if !ok {
		panic("expected *mockPricing")
	}
	pln := mp.plans["pln_1"]
	pln.BaseBillTiming = baseTiming
	mp.plans["pln_1"] = pln
	return engine, subs, invoices
}

// TestEvaluateThresholds_InAdvanceBaseExcluded is the regression test for the
// double-bill: an in_advance base fee is already billed up front by
// BillOnCreate / cycle close, so it must NOT count toward the threshold
// running total or ride along on the early-finalize invoice's line items.
// Without the guard, running would include the 4900-cent prepaid base and the
// line items would carry a duplicate base_fee row.
func TestEvaluateThresholds_InAdvanceBaseExcluded(t *testing.T) {
	thresholds := &domain.BillingThresholds{
		AmountGTE:         100000,
		ResetBillingCycle: true,
	}
	// $49 base is in_advance (prepaid); 1000 calls × 100 cents = 100000 usage.
	engine, _, _ := setupThresholdEngineWithTiming(thresholds, 1000, domain.BillInAdvance)

	sub, _ := engine.subs.Get(context.Background(), "t1", "sub_1")
	periodStart := *sub.CurrentBillingPeriodStart
	now := periodStart.AddDate(0, 0, 5)

	eval, err := engine.evaluateThresholds(context.Background(), sub, periodStart, now)
	if err != nil {
		t.Fatalf("evaluateThresholds: %v", err)
	}

	// Running must be usage-only: the prepaid in_advance base is excluded.
	wantSubtotal := int64(1000 * 100)
	if eval.RunningSubtotal != wantSubtotal {
		t.Errorf("running subtotal: got %d, want %d (in_advance base must be excluded)", eval.RunningSubtotal, wantSubtotal)
	}
	// No base_fee line should survive — only usage lines.
	for _, li := range eval.LineItems {
		if li.LineType == domain.LineTypeBaseFee {
			t.Errorf("unexpected base_fee line item in threshold eval (in_advance base double-bills): %+v", li)
		}
	}
	// Usage still crosses the cap on its own, so the fire decision is unchanged.
	if !eval.CrossedAny {
		t.Error("expected CrossedAny to be true (usage alone crosses the cap)")
	}
}

// TestEvaluateThresholds_InArrearsBaseIncluded is the control: an in_arrears
// base fee is NOT prepaid, so it must still count toward the running total and
// appear on the early-finalize line items.
func TestEvaluateThresholds_InArrearsBaseIncluded(t *testing.T) {
	// reset=false: the full in_arrears base counts (the reset=true arm
	// prorates it — see the ResetProratesBase tests).
	thresholds := &domain.BillingThresholds{
		AmountGTE:         100000,
		ResetBillingCycle: false,
	}
	engine, _, _ := setupThresholdEngineWithTiming(thresholds, 1000, domain.BillInArrears)

	sub, _ := engine.subs.Get(context.Background(), "t1", "sub_1")
	periodStart := *sub.CurrentBillingPeriodStart
	now := periodStart.AddDate(0, 0, 5)

	eval, err := engine.evaluateThresholds(context.Background(), sub, periodStart, now)
	if err != nil {
		t.Fatalf("evaluateThresholds: %v", err)
	}

	// $49 base (4900) + 1000 calls × 100 cents (100000) = 104900.
	wantSubtotal := int64(4900 + 1000*100)
	if eval.RunningSubtotal != wantSubtotal {
		t.Errorf("running subtotal: got %d, want %d (in_arrears base must be included)", eval.RunningSubtotal, wantSubtotal)
	}
	var baseFeeLines int
	for _, li := range eval.LineItems {
		if li.LineType == domain.LineTypeBaseFee {
			baseFeeLines++
		}
	}
	if baseFeeLines != 1 {
		t.Errorf("base_fee line items: got %d, want 1", baseFeeLines)
	}
}

// TestScanThresholds_SkipsTerminalSubs covers the gate that prevents
// firing on canceled/archived subs. The candidate query in postgres
// already restricts to active+trialing — this test guards the second-line
// defense in scanOneThreshold so a stale candidate row can't bypass.
func TestScanThresholds_SkipsTerminalSubs(t *testing.T) {
	thresholds := &domain.BillingThresholds{
		AmountGTE:         100, // very low cap so we'd cross if it fired
		ResetBillingCycle: true,
	}
	engine, subs, invoices := setupThresholdEngine(thresholds, 1000)

	// Force-cancel the candidate after build.
	s := subs.subs["sub_1"]
	s.Status = domain.SubscriptionCanceled
	subs.subs["sub_1"] = s
	subs.candidates[0] = s

	fired, errs := engine.ScanThresholds(context.Background(), 50)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if fired != 0 {
		t.Errorf("expected 0 fires for canceled sub, got %d", fired)
	}
	if len(invoices.invoices) != 0 {
		t.Errorf("expected 0 invoices, got %d", len(invoices.invoices))
	}
}

// TestScanThresholds_SkipsPauseCollection covers pause_collection: the
// scan must not emit an invoice that can't be charged or dunned.
func TestScanThresholds_SkipsPauseCollection(t *testing.T) {
	thresholds := &domain.BillingThresholds{
		AmountGTE:         100,
		ResetBillingCycle: true,
	}
	engine, subs, invoices := setupThresholdEngine(thresholds, 1000)

	s := subs.subs["sub_1"]
	s.PauseCollection = &domain.PauseCollection{
		Behavior: domain.PauseCollectionKeepAsDraft,
	}
	subs.subs["sub_1"] = s
	subs.candidates[0] = s

	fired, errs := engine.ScanThresholds(context.Background(), 50)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if fired != 0 {
		t.Errorf("expected 0 fires for paused sub, got %d", fired)
	}
	if len(invoices.invoices) != 0 {
		t.Errorf("expected 0 invoices, got %d", len(invoices.invoices))
	}
}

// TestScanThresholds_NoCandidates handles the fast-path when no rows
// have thresholds configured. Returns (0, nil) without entering the loop.
func TestScanThresholds_NoCandidates(t *testing.T) {
	engine, subs, _ := setupThresholdEngine(&domain.BillingThresholds{AmountGTE: 100}, 100)
	subs.candidates = nil

	fired, errs := engine.ScanThresholds(context.Background(), 50)
	if len(errs) > 0 {
		t.Errorf("unexpected errors: %v", errs)
	}
	if fired != 0 {
		t.Errorf("fired count: got %d, want 0", fired)
	}
}

// TestThresholdFire_ClockPinned_StampsIsSimulated locks the writer-drift fix:
// the threshold invoice writer omitted IsSimulated, so a threshold invoice
// fired on a test-clock-pinned subscription persisted is_simulated=false and
// rendered without the Simulated badge while sibling cycle/proration invoices
// on the same clock showed it. The frontend reads the field authoritatively
// (no timestamp inference), so the write-time stamp is the only source.
func TestThresholdFire_ClockPinned_StampsIsSimulated(t *testing.T) {
	thresholds := &domain.BillingThresholds{AmountGTE: 100000}
	engine, subs, invoices := setupThresholdEngine(thresholds, 1000)

	// Pin the sub to a test clock frozen 5 days into the cycle.
	pinned := subs.candidates[0]
	pinned.TestClockID = "tc_1"
	subs.candidates = []domain.Subscription{pinned}
	subs.subs["sub_1"] = pinned
	frozen := pinned.CurrentBillingPeriodStart.AddDate(0, 0, 5)
	engine.SetTestClockReader(&stubClockReader{clk: domain.TestClock{ID: "tc_1", FrozenTime: frozen}})

	fired, errs := engine.ScanThresholds(context.Background(), 50)
	if len(errs) > 0 {
		t.Fatalf("scan errors: %v", errs)
	}
	if fired != 1 {
		t.Fatalf("fired = %d, want 1", fired)
	}
	if len(invoices.invoices) != 1 {
		t.Fatalf("invoices = %d, want 1", len(invoices.invoices))
	}
	inv := invoices.invoices[0]
	if !inv.IsSimulated {
		t.Error("threshold invoice on a clock-pinned sub must stamp IsSimulated=true (Simulated badge)")
	}
	if !inv.IssuedAt.Equal(frozen) {
		t.Errorf("issued_at = %v, want frozen time %v (simulation timeline)", inv.IssuedAt, frozen)
	}
	if inv.BillingReason != domain.BillingReasonThreshold {
		t.Errorf("billing_reason = %q, want threshold", inv.BillingReason)
	}
	// Usage lines must carry the exact decimal quantity (cycle-writer
	// parity) — the threshold writer used to truncate to the integer only.
	var sawUsage bool
	for _, li := range invoices.lineItems {
		if li.LineType != domain.LineTypeUsage {
			continue
		}
		sawUsage = true
		if li.QuantityDecimal.IsZero() {
			t.Errorf("usage line %q: QuantityDecimal is zero — fractional quantities would truncate on display", li.Description)
		}
	}
	if !sawUsage {
		t.Fatal("expected at least one usage line on the threshold invoice")
	}
}

// TestScanThresholds_FireOnceProbe_NoReEvaluate locks the fire-once probe. A
// reset=false threshold that has already fired keeps its crossed running total
// on the next tick (the cycle stays put), so without the LatestThresholdPeriodEnd
// short-circuit the scan re-evaluates and re-fires every tick — burning an
// invoice number + a paid tax calculation before the dedup index rejects it.
// The mock invoice store has NO unique index, so the SECOND scan would create a
// genuine duplicate here: that is the mutation seam. Delete the probe in
// scanOneThreshold and the final assertion flips to 2 invoices.
func TestScanThresholds_FireOnceProbe_NoReEvaluate(t *testing.T) {
	thresholds := &domain.BillingThresholds{AmountGTE: 50000, ResetBillingCycle: false}
	engine, _, invoices := setupThresholdEngine(thresholds, 1000)
	// Anchor now mid-cycle. setupThresholdEngine's period is [2026-04, 2026-05],
	// which is in the past under the real wall clock — the boundary-skip guard
	// would fire and mask the probe. A fake clock inside the window isolates the
	// probe as the sole thing under test.
	engine.clock = clock.NewFake(time.Date(2026, 4, 15, 0, 0, 0, 0, time.UTC))
	ctx := context.Background()

	fired1, errs1 := engine.ScanThresholds(ctx, 50)
	if len(errs1) != 0 {
		t.Fatalf("first scan errors: %v", errs1)
	}
	if fired1 != 1 {
		t.Fatalf("first scan fired = %d, want 1", fired1)
	}
	if len(invoices.invoices) != 1 {
		t.Fatalf("after first scan: %d invoices, want 1", len(invoices.invoices))
	}

	// Second tick over the same reset=false cycle — the probe must short-circuit
	// before evaluate/fire.
	fired2, errs2 := engine.ScanThresholds(ctx, 50)
	if len(errs2) != 0 {
		t.Fatalf("second scan errors: %v", errs2)
	}
	if fired2 != 0 {
		t.Fatalf("second scan fired = %d, want 0 (fire-once probe)", fired2)
	}
	if len(invoices.invoices) != 1 {
		t.Fatalf("re-scan created a duplicate threshold invoice: %d invoices, want 1", len(invoices.invoices))
	}
}

// TestScanThresholds_DrainsPastBatchSize locks the cursor-drain. Pre-fix,
// ScanThresholds fetched a single `ORDER BY id LIMIT batch` page and never
// advanced a cursor, so with more crossed subs than the batch size the tail was
// never scanned — spend caps silently disabled past `batchSize` subscriptions.
// 60 crossed subs with batchSize 50 forces a second page. Mutation seam: revert
// ScanThresholds to a single ListWithThresholds fetch and `fired` drops to 50.
func TestScanThresholds_DrainsPastBatchSize(t *testing.T) {
	thresholds := &domain.BillingThresholds{AmountGTE: 50000, ResetBillingCycle: false}
	engine, subs, invoices := setupThresholdEngine(thresholds, 1000)
	engine.clock = clock.NewFake(time.Date(2026, 4, 15, 0, 0, 0, 0, time.UTC))

	periodStart := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	nextBilling := periodEnd
	many := make([]domain.Subscription, 0, 60)
	for i := 1; i <= 60; i++ {
		many = append(many, domain.Subscription{
			// Zero-padded ids so the id cursor (id > afterID, ORDER BY id) pages
			// deterministically — sub_0051 sorts after sub_0050, not before.
			ID: fmt.Sprintf("sub_%04d", i), TenantID: "t1", CustomerID: "cus_1",
			Items:                     []domain.SubscriptionItem{{ID: "subitem_1", PlanID: "pln_1", Quantity: 1}},
			Status:                    domain.SubscriptionActive,
			BillingTime:               domain.BillingTimeCalendar,
			CurrentBillingPeriodStart: &periodStart,
			CurrentBillingPeriodEnd:   &periodEnd,
			NextBillingAt:             &nextBilling,
			BillingThresholds:         thresholds,
		})
	}
	subs.candidates = many

	fired, errs := engine.ScanThresholds(context.Background(), 50)
	if len(errs) != 0 {
		t.Fatalf("scan errors: %v", errs)
	}
	if fired != 60 {
		t.Fatalf("drain incomplete: fired = %d, want 60 (subs #51-60 past the first batch must scan)", fired)
	}
	if len(invoices.invoices) != 60 {
		t.Fatalf("expected 60 threshold invoices, got %d", len(invoices.invoices))
	}
}

// TestScanOneThreshold_BoundarySkip locks the boundary-skip. When `now` reaches
// the period end (scheduler downtime, or a crossing first observed in the last
// inter-tick window), firing would emit a [periodStart, now) window that spills
// into the next period; the cycle-close watermark is scoped to THIS period's
// threshold invoices, so the spilled usage bills again — double-billing across
// the boundary. The scan must SKIP and let the same-tick natural cycle bill the
// whole elapsed period through the full-fidelity path. Mutation seam: delete the
// `!now.Before(periodEnd)` guard and this crossed sub fires.
func TestScanOneThreshold_BoundarySkip(t *testing.T) {
	thresholds := &domain.BillingThresholds{AmountGTE: 50000, ResetBillingCycle: false}
	engine, subs, invoices := setupThresholdEngine(thresholds, 1000)
	// now == periodEnd: the window is closed.
	engine.clock = clock.NewFake(time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC))

	didFire, err := engine.scanOneThreshold(context.Background(), subs.candidates[0])
	if err != nil {
		t.Fatalf("scanOneThreshold: %v", err)
	}
	if didFire {
		t.Fatal("threshold fired on the boundary tick (now == period_end); want skip")
	}
	if len(invoices.invoices) != 0 {
		t.Fatalf("boundary tick created %d invoices; want 0 (defer to natural cycle)", len(invoices.invoices))
	}
}

// TestFireThreshold_ZeroTotal_AutoPaid is T12 for the threshold writer: a
// usage_gte item cap crossed by zero-priced usage (free-rated lines) produces
// a $0 finalized invoice, which must be auto-marked paid — never charged
// (amount_due=0 skips the charge arm), never dunned. Pre-fix the gate's
// `totalWithTax > 0` conjunct stranded it payment_pending forever, polluting
// the attention queue as permanently overdue. Mutation seam: restore the
// conjunct and this fails.
func TestFireThreshold_ZeroTotal_AutoPaid(t *testing.T) {
	thresholds := &domain.BillingThresholds{
		ResetBillingCycle: false,
		ItemThresholds: []domain.SubscriptionItemThreshold{
			{SubscriptionItemID: "subitem_1", UsageGTE: decimal.NewFromInt(1000)},
		},
	}
	engine, _, invoices := setupThresholdEngine(thresholds, 1500)
	engine.clock = clock.NewFake(time.Date(2026, 4, 15, 0, 0, 0, 0, time.UTC))
	// The $0 auto-paid settle resolves dunning (required post-#442).
	resolver := &recordingDunningResolver{}
	engine.SetDunningResolver(resolver)
	// Free-rate the plan: $0 base + $0/call — the cap is quantity-based, so
	// it crosses with a $0 running total.
	mp := engine.pricing.(*mockPricing)
	pln := mp.plans["pln_1"]
	pln.BaseAmountCents = 0
	mp.plans["pln_1"] = pln
	rule := mp.rules["rrv_api"]
	rule.FlatAmountCents = decimal.Zero
	// The fixture's currency normally rides the base line (plan.Currency);
	// with base gone the $0 usage line must carry it.
	rule.Currency = "USD"
	mp.rules["rrv_api"] = rule

	fired, errs := engine.ScanThresholds(context.Background(), 50)
	if len(errs) != 0 {
		t.Fatalf("scan errors: %v", errs)
	}
	if fired != 1 {
		t.Fatalf("fired = %d, want 1 (quantity cap crossed)", fired)
	}
	if len(invoices.invoices) != 1 {
		t.Fatalf("invoices = %d, want 1", len(invoices.invoices))
	}
	inv := invoices.invoices[0]
	if inv.TotalAmountCents != 0 {
		t.Fatalf("total = %d, want 0", inv.TotalAmountCents)
	}
	if inv.Status != domain.InvoicePaid || inv.PaymentStatus != domain.PaymentSucceeded {
		t.Errorf("$0 threshold invoice stranded: status=%q payment=%q, want paid/succeeded (Stripe parity: zero-amount invoices auto-mark paid)",
			inv.Status, inv.PaymentStatus)
	}
}

// TestScanThresholds_ProbeHealsStrandedZeroDue: a crash between the invoice
// create and its $0/credited MarkPaid re-enters ONLY through the fire-once
// probe — so the probe must repair the stranded payment_pending row
// (ADR-066 heal-on-re-entry). Mutation seam: drop the healStrandedZeroDue
// call from the probe hit and this fails.
func TestScanThresholds_ProbeHealsStrandedZeroDue(t *testing.T) {
	thresholds := &domain.BillingThresholds{AmountGTE: 50000, ResetBillingCycle: false}
	engine, _, invoices := setupThresholdEngine(thresholds, 1000)
	engine.clock = clock.NewFake(time.Date(2026, 4, 15, 0, 0, 0, 0, time.UTC))

	// Seed the stranded state: threshold invoice committed finalized with
	// nothing due, MarkPaid never ran (process died).
	periodStart := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	fireAt := time.Date(2026, 4, 10, 0, 0, 0, 0, time.UTC)
	invoices.invoices = append(invoices.invoices, domain.Invoice{
		ID: "vlx_inv_stranded", SubscriptionID: "sub_1", TenantID: "t1",
		Status:        domain.InvoiceFinalized,
		PaymentStatus: domain.PaymentPending,
		TaxFacts: domain.TaxFacts{
			TaxStatus: domain.InvoiceTaxOK,
		},
		AmountDueCents:     0,
		BillingReason:      domain.BillingReasonThreshold,
		BillingPeriodStart: periodStart,
		BillingPeriodEnd:   fireAt,
	})

	fired, errs := engine.ScanThresholds(context.Background(), 50)
	if len(errs) != 0 {
		t.Fatalf("scan errors: %v", errs)
	}
	if fired != 0 {
		t.Fatalf("fired = %d, want 0 (probe hit)", fired)
	}
	healed := invoices.invoices[len(invoices.invoices)-1]
	if healed.Status != domain.InvoicePaid || healed.PaymentStatus != domain.PaymentSucceeded {
		t.Errorf("stranded $0 invoice not healed on probe re-entry: status=%q payment=%q", healed.Status, healed.PaymentStatus)
	}
}
