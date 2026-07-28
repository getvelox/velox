package billing_test

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/billing"
	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/money"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/subscription"
	"github.com/sagarsuperuser/velox/internal/tax"
	"github.com/sagarsuperuser/velox/internal/tenant"
	"github.com/sagarsuperuser/velox/internal/testutil"
	"github.com/sagarsuperuser/velox/internal/usage"
)

// thresholdFixture wires a real-store engine and seeds a single subscription
// with billing thresholds configured. Mirrors previewFixture but exposes the
// engine's ScanThresholds path against actual Postgres so the partial unique
// index, ListWithThresholds candidate query, and CreateInvoiceWithLineItems
// are all exercised end-to-end.
type thresholdFixture struct {
	db         *postgres.DB
	tenantID   string
	subStore   *subscription.PostgresStore
	subSvc     *subscription.Service
	usageSvc   *usage.Service
	invStore   *invoice.PostgresStore
	settings   *tenant.SettingsStore
	pricingSvc *pricing.Service
	subAdapter *failableSubAdapter
	engine     *billing.Engine

	// Seed-by-default test data
	customerID string
	planID     string
	subID      string
	itemID     string
	meterID    string
	cycleStart time.Time
	cycleEnd   time.Time
}

// newThresholdFixture sets up a tenant with a single-meter, single-rule plan
// (1 cent / call flat rate, no base fee) and an active sub mid-cycle. Tests
// can configure thresholds on the seeded sub via subSvc.SetBillingThresholds.
func newThresholdFixture(t *testing.T, name string) *thresholdFixture {
	t.Helper()
	db := testutil.SetupTestDB(t)
	tenantID := testutil.CreateTestTenant(t, db, name)

	customerStore := customer.NewPostgresStore(db)
	customerSvc := customer.NewService(customerStore)
	pricingStore := pricing.NewPostgresStore(db)
	pricingSvc := pricing.NewService(pricingStore)
	subStore := subscription.NewPostgresStore(db)
	subSvc := subscription.NewService(subStore, nil)
	usageStore := usage.NewPostgresStore(db)
	usageSvc := usage.NewService(usageStore)
	invoiceStore := invoice.NewPostgresStore(db)
	settingsStore := tenant.NewSettingsStore(db)

	subAdapter := &failableSubAdapter{subStoreAdapter: &subStoreAdapter{subStore}}
	engine := billing.NewEngine(
		subAdapter,
		&usageStoreAdapter{usageStore},
		&pricingStoreAdapter{pricingStore},
		&invoiceStoreAdapter{invoiceStore},
		nil, settingsStore, testPaymentSetupsNoPM{}, testChargerSentinel{}, nil,
	)
	engine.SetIntervalReader(subStore)
	// Production wires a tax resolver; engine fails loudly without
	// one (no silent zero-tax fallback). NoneProvider is the
	// minimal wiring for tests that don't exercise tax behavior.
	engine.SetTaxProviderResolver(tax.NewResolver(nil))
	engine.SetNoPaymentMethodNotifier(&testNoPMNotifier{})
	engine.SetTxRunner(db)

	ctx := postgres.WithLivemode(context.Background(), false)

	cust, err := customerSvc.Create(ctx, tenantID, customer.CreateInput{
		ExternalID:  "cus_thresh",
		DisplayName: "Threshold Customer",
		Email:       "thresh@example.test",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}

	rrv, err := pricingSvc.CreateRatingRule(ctx, tenantID, pricing.CreateRatingRuleInput{
		RuleKey: "calls_flat", Name: "Calls Flat",
		Mode: domain.PricingFlat, Currency: "USD", FlatAmountCents: decimal.NewFromInt(1),
	})
	if err != nil {
		t.Fatalf("create rrv: %v", err)
	}

	meter, err := pricingSvc.CreateMeter(ctx, tenantID, pricing.CreateMeterInput{
		Key: "calls", Name: "Calls", Unit: "calls",
		Aggregation: "sum", RatingRuleVersionID: rrv.ID,
	})
	if err != nil {
		t.Fatalf("create meter: %v", err)
	}

	plan, err := pricingSvc.CreatePlan(ctx, tenantID, pricing.CreatePlanInput{
		Code: "pln_thresh", Name: "Threshold Plan",
		Currency: "USD", BillingInterval: domain.BillingMonthly,
		BaseAmountCents: 0, MeterIDs: []string{meter.ID},
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}

	cycleStart := time.Now().UTC().Truncate(time.Hour).Add(-72 * time.Hour)
	cycleEnd := cycleStart.Add(30 * 24 * time.Hour)

	subID := postgres.NewID("vlx_sub")
	itemID := postgres.NewID("vlx_si")
	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin sub tx: %v", err)
	}
	defer postgres.Rollback(tx)

	now := time.Now().UTC()
	_, err = tx.ExecContext(ctx, `
		INSERT INTO subscriptions (
			id, tenant_id, code, display_name, customer_id, status, billing_time,
			current_billing_period_start, current_billing_period_end, next_billing_at,
			created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, 'active', 'anniversary', $6, $7, $7, $8, $8)
	`, subID, tenantID, "code-thresh", "Threshold Sub", cust.ID, cycleStart, cycleEnd, now)
	if err != nil {
		t.Fatalf("insert sub: %v", err)
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO subscription_items (id, tenant_id, subscription_id, plan_id, quantity, metadata, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 1, '{}'::jsonb, $5, $5)
	`, itemID, tenantID, subID, plan.ID, now)
	if err != nil {
		t.Fatalf("insert sub item: %v", err)
	}
	// Raw-SQL fixture bypasses the ADR-101 interval writer — mirror it:
	// the item's billable lifetime opens at the cycle start.
	_, err = tx.ExecContext(ctx, `
		INSERT INTO billing_intervals (tenant_id, subscription_id, subscription_item_id, plan_id, quantity, starts_at, source)
		VALUES ($1, $2, $3, $4, 1, $5, 'create')
	`, tenantID, subID, itemID, plan.ID, cycleStart)
	if err != nil {
		t.Fatalf("insert billing interval: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit sub: %v", err)
	}

	return &thresholdFixture{
		db:         db,
		tenantID:   tenantID,
		subStore:   subStore,
		subSvc:     subSvc,
		usageSvc:   usageSvc,
		invStore:   invoiceStore,
		settings:   settingsStore,
		pricingSvc: pricingSvc,
		subAdapter: subAdapter,
		engine:     engine,
		customerID: cust.ID,
		planID:     plan.ID,
		subID:      subID,
		itemID:     itemID,
		meterID:    meter.ID,
		cycleStart: cycleStart,
		cycleEnd:   cycleEnd,
	}
}

// ingestUsage seeds N events spread evenly over the first 60 minutes of the
// cycle. cycleStart is 72h ago so all events are in the past relative to
// wall-clock now (which is what the threshold scan uses as the period
// upper bound). Each event has the supplied quantity, so
// ingestUsage(t, ctx, 100, 10) produces 100 events × qty 10 = 1000
// total quantity, billed at 1c/call = 1000c subtotal.
func (f *thresholdFixture) ingestUsage(t *testing.T, ctx context.Context, count int, qty int64) {
	t.Helper()
	for i := 0; i < count; i++ {
		ts := f.cycleStart.Add(time.Duration(i) * time.Minute)
		if _, err := f.usageSvc.Ingest(ctx, f.tenantID, usage.IngestInput{
			CustomerID: f.customerID,
			MeterID:    f.meterID,
			Quantity:   decimal.NewFromInt(qty),
			Timestamp:  &ts,
		}); err != nil {
			t.Fatalf("ingest %d: %v", i, err)
		}
	}
}

func (f *thresholdFixture) listInvoices(t *testing.T, ctx context.Context) []domain.Invoice {
	t.Helper()
	invoices, _, err := f.invStore.List(ctx, invoice.ListFilter{TenantID: f.tenantID})
	if err != nil {
		t.Fatalf("list invoices: %v", err)
	}
	return invoices
}

// TestThresholdScan_AmountCrossFiresEarly is the headline test: usage drives
// the running subtotal past AmountGTE, the scan emits a threshold-billed
// invoice, the cycle resets, and a second scan against the new (empty) cycle
// is a no-op. End-to-end against real Postgres so the CreateWithLineItems
// path, partial unique index, and ListWithThresholds candidate query are
// all exercised together.
func TestThresholdScan_AmountCrossFiresEarly(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	f := newThresholdFixture(t, "Threshold Amount Cross")
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 60*time.Second)
	defer cancel()

	// 100 events × qty 10 × 1c = 1000c subtotal.
	f.ingestUsage(t, ctx, 100, 10)

	// Configure amount threshold at 500c — well below 1000c. This test
	// exercises RESET semantics, so reset_billing_cycle is explicit: the
	// omitted-field default flipped to false (keep-anchor, Stripe parity)
	// on 2026-07-10.
	resetCycle := true
	if _, err := f.subSvc.SetBillingThresholds(ctx, f.tenantID, f.subID, subscription.BillingThresholdsInput{
		AmountGTE:         500,
		ResetBillingCycle: &resetCycle,
	}); err != nil {
		t.Fatalf("set threshold: %v", err)
	}

	fired, errs := f.engine.ScanThresholds(ctx, 50)
	if len(errs) > 0 {
		t.Fatalf("scan errors: %v", errs)
	}
	if fired != 1 {
		t.Fatalf("fired count: got %d, want 1", fired)
	}

	invoices := f.listInvoices(t, ctx)
	if len(invoices) != 1 {
		t.Fatalf("expected 1 invoice, got %d", len(invoices))
	}
	inv := invoices[0]
	if inv.BillingReason != domain.BillingReasonThreshold {
		t.Errorf("billing_reason: got %q, want %q", inv.BillingReason, domain.BillingReasonThreshold)
	}
	if inv.SubscriptionID != f.subID {
		t.Errorf("subscription_id: got %q, want %q", inv.SubscriptionID, f.subID)
	}
	if inv.SubtotalCents != 1000 {
		t.Errorf("subtotal_cents: got %d, want 1000", inv.SubtotalCents)
	}
	if !inv.BillingPeriodStart.Equal(f.cycleStart) {
		t.Errorf("billing_period_start: got %v, want %v", inv.BillingPeriodStart, f.cycleStart)
	}

	// Cycle reset: with reset_billing_cycle=true (explicit), the sub's
	// current_billing_period_start should now be the fire-time, not the
	// original cycleStart.
	updated, err := f.subStore.Get(ctx, f.tenantID, f.subID)
	if err != nil {
		t.Fatalf("reload sub: %v", err)
	}
	if updated.CurrentBillingPeriodStart == nil {
		t.Fatal("current_billing_period_start should be set after reset")
	}
	if updated.CurrentBillingPeriodStart.Equal(f.cycleStart) {
		t.Error("expected period_start to advance after reset_billing_cycle=true")
	}
}

// TestThresholdScan_Idempotent guards against double-finalize on retry AND
// against the re-fire burn. A second ScanThresholds tick over the same
// reset=false cycle must emit zero new invoices — and, critically, do so
// WITHOUT allocating an invoice number or a tax calculation. The fire-once
// probe (LatestThresholdPeriodEnd) short-circuits before fireThreshold's
// NextInvoiceNumber/ApplyTax; the partial unique index remains the correctness
// backstop for a genuinely concurrent retry (two ticks both passing the probe).
// Pre-probe, each re-tick allocated (and committed) a number in its own tx, then
// bounced off the unique index — ~600 burned numbers + paid tax calls per cycle.
func TestThresholdScan_Idempotent(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	f := newThresholdFixture(t, "Threshold Idempotent")
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 60*time.Second)
	defer cancel()

	f.ingestUsage(t, ctx, 100, 10)

	// reset_billing_cycle=false means the cycle stays put after fire, so
	// the next tick observes the same partial cycle that already fired —
	// the partial unique index is the only thing preventing a second
	// invoice. Perfect setup for verifying the idempotency seam.
	resetFalse := false
	if _, err := f.subSvc.SetBillingThresholds(ctx, f.tenantID, f.subID, subscription.BillingThresholdsInput{
		AmountGTE:         500,
		ResetBillingCycle: &resetFalse,
	}); err != nil {
		t.Fatalf("set threshold: %v", err)
	}

	// First tick — fires the invoice.
	fired1, errs1 := f.engine.ScanThresholds(ctx, 50)
	if len(errs1) > 0 {
		t.Fatalf("first scan errors: %v", errs1)
	}
	if fired1 != 1 {
		t.Fatalf("first fired count: got %d, want 1", fired1)
	}

	// Snapshot the invoice-number counter right after the first fire. The
	// probe means the second tick must NOT advance it; pre-fix each re-tick
	// committed a number in its own tx before the unique index rejected the
	// insert — a permanent gap the operator sees in their invoice sequence.
	nextBeforeRetick, err := f.settings.NextInvoiceNumber(ctx, f.tenantID)
	if err != nil {
		t.Fatalf("allocate number probe: %v", err)
	}

	// Second tick against the same cycle — the fire-once probe short-circuits
	// before evaluate/fire. No additional invoice, no error returned.
	fired2, errs2 := f.engine.ScanThresholds(ctx, 50)
	if len(errs2) > 0 {
		t.Fatalf("second scan errors: %v", errs2)
	}
	if fired2 != 0 {
		t.Fatalf("second fired count: got %d, want 0 (idempotent skip)", fired2)
	}

	invoices := f.listInvoices(t, ctx)
	if len(invoices) != 1 {
		t.Fatalf("expected exactly 1 invoice after re-tick, got %d", len(invoices))
	}

	// No-burn: the number the second tick would have allocated must be exactly
	// the one immediately after our snapshot — i.e. the re-tick consumed none.
	// Mutation seam: delete the probe in scanOneThreshold and fireThreshold
	// commits a number on the doomed re-tick, so this allocation skips ahead.
	nextAfterRetick, err := f.settings.NextInvoiceNumber(ctx, f.tenantID)
	if err != nil {
		t.Fatalf("allocate number probe (after): %v", err)
	}
	if seqOf(t, nextAfterRetick) != seqOf(t, nextBeforeRetick)+1 {
		t.Fatalf("re-tick burned an invoice number: snapshot %q then %q (want consecutive) — fire-once probe leaked into fireThreshold",
			nextBeforeRetick, nextAfterRetick)
	}
}

// seqOf extracts the trailing numeric sequence from an invoice number like
// "INV-0007" → 7 so consecutive allocations can be compared without coupling
// to the tenant's prefix format.
func seqOf(t *testing.T, invoiceNumber string) int {
	t.Helper()
	i := len(invoiceNumber)
	for i > 0 && invoiceNumber[i-1] >= '0' && invoiceNumber[i-1] <= '9' {
		i--
	}
	n, err := strconv.Atoi(invoiceNumber[i:])
	if err != nil {
		t.Fatalf("parse invoice number %q: %v", invoiceNumber, err)
	}
	return n
}

// TestThresholdScan_ItemUsageCross verifies the per-item path against real
// Postgres — the per-meter aggregator wraps AggregateByPricingRules, so this
// proves the scan respects the same priority+claim resolution as the cycle.
func TestThresholdScan_ItemUsageCross(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	f := newThresholdFixture(t, "Threshold Item Cross")
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 60*time.Second)
	defer cancel()

	// 100 events × qty 10 = 1000 total quantity on the meter.
	f.ingestUsage(t, ctx, 100, 10)

	if _, err := f.subSvc.SetBillingThresholds(ctx, f.tenantID, f.subID, subscription.BillingThresholdsInput{
		ItemThresholds: []subscription.ItemThresholdInput{
			{SubscriptionItemID: f.itemID, UsageGTE: "500"},
		},
	}); err != nil {
		t.Fatalf("set threshold: %v", err)
	}

	fired, errs := f.engine.ScanThresholds(ctx, 50)
	if len(errs) > 0 {
		t.Fatalf("scan errors: %v", errs)
	}
	if fired != 1 {
		t.Fatalf("fired count: got %d, want 1 (item quantity 1000 > cap 500)", fired)
	}

	invoices := f.listInvoices(t, ctx)
	if len(invoices) != 1 {
		t.Fatalf("expected 1 invoice, got %d", len(invoices))
	}
	if invoices[0].BillingReason != domain.BillingReasonThreshold {
		t.Errorf("billing_reason: got %q, want %q", invoices[0].BillingReason, domain.BillingReasonThreshold)
	}
}

// TestThresholdScan_ItemOnlyKeepsAnchor pins the coverage cell where the
// 2026-07-21 FLOW B14 money bug lived: an ITEM-ONLY threshold (amount_gte
// NULL) with reset_billing_cycle omitted (defaults false). scanSubRow only
// builds the thresholds struct when amount_gte is set, so hydrateThresholds
// allocated it for item-only subs — with a fabricated ResetBillingCycle=true
// that both the engine (re-anchored the cycle on fire) and the API echo
// consumed. Mutation seam: restore the hardcoded `true` in hydrateThresholds
// and both assertions below fail.
func TestThresholdScan_ItemOnlyKeepsAnchor(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	f := newThresholdFixture(t, "Threshold Item Keep Anchor")
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 60*time.Second)
	defer cancel()

	f.ingestUsage(t, ctx, 100, 10)

	// reset_billing_cycle deliberately OMITTED — the documented default is
	// false (Stripe keep-anchor), and item-only subs must honor it.
	if _, err := f.subSvc.SetBillingThresholds(ctx, f.tenantID, f.subID, subscription.BillingThresholdsInput{
		ItemThresholds: []subscription.ItemThresholdInput{
			{SubscriptionItemID: f.itemID, UsageGTE: "500"},
		},
	}); err != nil {
		t.Fatalf("set threshold: %v", err)
	}

	// The hydrated read must report the stored flag, not the column default.
	stored, err := f.subStore.Get(ctx, f.tenantID, f.subID)
	if err != nil {
		t.Fatalf("reload sub: %v", err)
	}
	if stored.BillingThresholds == nil {
		t.Fatal("BillingThresholds should hydrate for item-only thresholds")
	}
	if stored.BillingThresholds.ResetBillingCycle {
		t.Error("item-only threshold hydrated ResetBillingCycle=true; stored value is false")
	}

	fired, errs := f.engine.ScanThresholds(ctx, 50)
	if len(errs) > 0 {
		t.Fatalf("scan errors: %v", errs)
	}
	if fired != 1 {
		t.Fatalf("fired count: got %d, want 1", fired)
	}

	updated, err := f.subStore.Get(ctx, f.tenantID, f.subID)
	if err != nil {
		t.Fatalf("reload sub: %v", err)
	}
	if updated.CurrentBillingPeriodStart == nil {
		t.Fatal("period_start should be set")
	}
	if !updated.CurrentBillingPeriodStart.Equal(f.cycleStart) {
		t.Errorf("item-only cap with default reset=false re-anchored the cycle: period_start %v, want %v",
			*updated.CurrentBillingPeriodStart, f.cycleStart)
	}
}

// TestThresholdScan_BelowThresholdNoFire is the silent-success case: usage
// is non-zero but doesn't cross the cap, so the scan should observe but
// emit nothing. Catches a regression where the threshold logic accidentally
// fires on every tick.
func TestThresholdScan_BelowThresholdNoFire(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	f := newThresholdFixture(t, "Threshold Below Cap")
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 60*time.Second)
	defer cancel()

	// 100 events × qty 10 × 1c = 1000c subtotal.
	f.ingestUsage(t, ctx, 100, 10)

	// Cap at 5000c — well above 1000c.
	if _, err := f.subSvc.SetBillingThresholds(ctx, f.tenantID, f.subID, subscription.BillingThresholdsInput{
		AmountGTE: 5000,
	}); err != nil {
		t.Fatalf("set threshold: %v", err)
	}

	fired, errs := f.engine.ScanThresholds(ctx, 50)
	if len(errs) > 0 {
		t.Fatalf("scan errors: %v", errs)
	}
	if fired != 0 {
		t.Fatalf("fired count: got %d, want 0", fired)
	}
	invoices := f.listInvoices(t, ctx)
	if len(invoices) != 0 {
		t.Fatalf("expected 0 invoices, got %d", len(invoices))
	}
}

// TestThresholdScan_NoConfigSkipped covers the candidate-query side: a sub
// with no billing thresholds configured must not appear in
// ListWithThresholds and thus must not be considered by the scan.
func TestThresholdScan_NoConfigSkipped(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	f := newThresholdFixture(t, "Threshold No Config")
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 60*time.Second)
	defer cancel()

	// Lots of usage but no threshold configured — the candidate query
	// must filter this out.
	f.ingestUsage(t, ctx, 100, 100)

	fired, errs := f.engine.ScanThresholds(ctx, 50)
	if len(errs) > 0 {
		t.Fatalf("scan errors: %v", errs)
	}
	if fired != 0 {
		t.Fatalf("fired count: got %d, want 0 (no threshold configured)", fired)
	}
}

// TestThresholdScan_ResetCycleFalse confirms the cycle stays put when the
// caller explicitly opts out. The threshold invoice still fires, but the
// next natural cycle invoice will pick up where the original cycle ended —
// not where the threshold fired.
func TestThresholdScan_ResetCycleFalse(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	f := newThresholdFixture(t, "Threshold Reset False")
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 60*time.Second)
	defer cancel()

	f.ingestUsage(t, ctx, 100, 10)

	resetFalse := false
	if _, err := f.subSvc.SetBillingThresholds(ctx, f.tenantID, f.subID, subscription.BillingThresholdsInput{
		AmountGTE:         500,
		ResetBillingCycle: &resetFalse,
	}); err != nil {
		t.Fatalf("set threshold: %v", err)
	}

	fired, errs := f.engine.ScanThresholds(ctx, 50)
	if len(errs) > 0 {
		t.Fatalf("scan errors: %v", errs)
	}
	if fired != 1 {
		t.Fatalf("fired count: got %d, want 1", fired)
	}

	updated, err := f.subStore.Get(ctx, f.tenantID, f.subID)
	if err != nil {
		t.Fatalf("reload sub: %v", err)
	}
	if updated.CurrentBillingPeriodStart == nil {
		t.Fatal("period_start should be set")
	}
	// reset_billing_cycle=false means period_start should NOT have advanced.
	if !updated.CurrentBillingPeriodStart.Equal(f.cycleStart) {
		t.Errorf("period_start should remain %v with reset=false, got %v",
			f.cycleStart, *updated.CurrentBillingPeriodStart)
	}
}

// TestThresholdScan_BoundaryDefersToCycle is the T2 money assertion for the
// boundary skip, on real stores: a crossing whose first observation lands
// on/after period_end (scheduler dead across the boundary) must NOT fire a
// threshold invoice — the natural cycle close bills the whole elapsed period,
// exactly once, through the full-fidelity path. Pre-fix the threshold fired a
// [period_start, now) window spilling past period_end; the next cycle's close
// would then bill the spilled usage again. Mutation seam: delete the
// `!now.Before(periodEnd)` guard in scanOneThreshold and the scan fires
// (fired=1) — the zero-threshold-invoice assertion below fails.
func TestThresholdScan_BoundaryDefersToCycle(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	f := newThresholdFixture(t, "Threshold Boundary Cycle")
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 60*time.Second)
	defer cancel()

	// 100 events x qty 10 @ 1c = 1000c, all inside the first hour of the cycle.
	f.ingestUsage(t, ctx, 100, 10)
	resetFalse := false
	if _, err := f.subSvc.SetBillingThresholds(ctx, f.tenantID, f.subID, subscription.BillingThresholdsInput{
		AmountGTE:         500,
		ResetBillingCycle: &resetFalse,
	}); err != nil {
		t.Fatalf("set threshold: %v", err)
	}

	// Rewind the period so wall-clock `now` is past period_end: the crossing is
	// first observed on the far side of the boundary. Usage (first hour of
	// cycleStart = now-72h) stays inside the shortened window. Truncated to
	// microseconds so the value round-trips Postgres timestamptz exactly
	// (Linux clocks are ns-granular; PG stores µs).
	pastEnd := time.Now().UTC().Add(-1 * time.Hour).Truncate(time.Microsecond)
	tx, err := f.db.BeginTx(ctx, postgres.TxTenant, f.tenantID)
	if err != nil {
		t.Fatalf("begin rewind tx: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(ctx, `
		UPDATE subscriptions SET current_billing_period_end = $1, next_billing_at = $1 WHERE id = $2
	`, pastEnd, f.subID); err != nil {
		t.Fatalf("rewind period: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit rewind: %v", err)
	}

	// Threshold tick on the closed window: must skip.
	fired, errs := f.engine.ScanThresholds(ctx, 50)
	if len(errs) > 0 {
		t.Fatalf("scan errors: %v", errs)
	}
	if fired != 0 {
		t.Fatalf("threshold fired across the boundary: fired=%d, want 0 (defer to cycle close)", fired)
	}

	// Same-tick cycle close bills the whole elapsed period once.
	generated, failures := f.engine.RunCycleForTenant(ctx, f.tenantID, 50)
	if len(failures) > 0 {
		t.Fatalf("cycle failures: %v", failures)
	}
	if generated != 1 {
		t.Fatalf("cycle generated = %d, want 1", generated)
	}

	invoices := f.listInvoices(t, ctx)
	if len(invoices) != 1 {
		t.Fatalf("expected exactly 1 invoice (cycle only), got %d", len(invoices))
	}
	inv := invoices[0]
	if inv.BillingReason == domain.BillingReasonThreshold {
		t.Fatalf("the single invoice is a threshold invoice — boundary skip did not defer to the cycle")
	}
	if inv.SubtotalCents != 1000 {
		t.Errorf("cycle invoice subtotal = %d, want 1000 (all usage billed exactly once)", inv.SubtotalCents)
	}
	if !inv.BillingPeriodEnd.Equal(pastEnd) {
		t.Errorf("cycle invoice period_end = %v, want %v (no spill past the boundary)", inv.BillingPeriodEnd, pastEnd)
	}
}

// seedThresholdFleet inserts n active subscriptions on a shared base-fee-only
// plan (no meters, so they cross on the in_arrears base alone — no per-sub
// usage ingestion needed) with an amount threshold configured directly on the
// row. IDs are zero-padded so the drain cursor (ORDER BY id, id > afterID)
// pages deterministically.
func (f *thresholdFixture) seedThresholdFleet(t *testing.T, ctx context.Context, n int) []string {
	t.Helper()
	plan, err := f.pricingSvc.CreatePlan(ctx, f.tenantID, pricing.CreatePlanInput{
		Code: "pln_fleet_base", Name: "Fleet Base Plan",
		Currency: "USD", BillingInterval: domain.BillingMonthly,
		BaseAmountCents: 4900,
	})
	if err != nil {
		t.Fatalf("create fleet plan: %v", err)
	}

	tx, err := f.db.BeginTx(ctx, postgres.TxTenant, f.tenantID)
	if err != nil {
		t.Fatalf("begin fleet tx: %v", err)
	}
	defer postgres.Rollback(tx)

	now := time.Now().UTC()
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		subID := fmt.Sprintf("vlx_sub_fleet_%04d", i)
		itemID := fmt.Sprintf("vlx_si_fleet_%04d", i)
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO subscriptions (
				id, tenant_id, code, display_name, customer_id, status, billing_time,
				current_billing_period_start, current_billing_period_end, next_billing_at,
				billing_threshold_amount_gte, billing_threshold_reset_cycle,
				created_at, updated_at
			) VALUES ($1, $2, $3, $4, $5, 'active', 'anniversary', $6, $7, $7, 100, FALSE, $8, $8)
		`, subID, f.tenantID, "code-fleet-"+subID, "Fleet Sub "+subID, f.customerID, f.cycleStart, f.cycleEnd, now); err != nil {
			t.Fatalf("insert fleet sub %d: %v", i, err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO subscription_items (id, tenant_id, subscription_id, plan_id, quantity, metadata, created_at, updated_at)
			VALUES ($1, $2, $3, $4, 1, '{}'::jsonb, $5, $5)
		`, itemID, f.tenantID, subID, plan.ID, now); err != nil {
			t.Fatalf("insert fleet item %d: %v", i, err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO billing_intervals (tenant_id, subscription_id, subscription_item_id, plan_id, quantity, starts_at, source)
			VALUES ($1, $2, $3, $4, 1, $5, 'create')
		`, f.tenantID, subID, itemID, plan.ID, f.cycleStart); err != nil {
			t.Fatalf("insert fleet interval %d: %v", i, err)
		}
		ids = append(ids, subID)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit fleet: %v", err)
	}
	return ids
}

// TestThresholdScan_DrainsWholeFleet_RealStore is T9+T10 on real Postgres:
// 60 crossed subscriptions with batchSize 50 must ALL fire in a single tick
// (pre-fix, the scan fetched one page and subs #51+ were never scanned —
// spend caps silently disabled past the batch size), and a full re-scan of
// the drained set must burn zero invoice numbers (the fire-once probe skips
// every fired sub with one indexed lookup each). Also EXPLAIN-verifies the
// cursored candidate query is index-servable (subscriptions_pkey provides
// the `id > cursor ORDER BY id` contract without a sort).
func TestThresholdScan_DrainsWholeFleet_RealStore(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	f := newThresholdFixture(t, "Threshold Fleet Drain")
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 120*time.Second)
	defer cancel()

	// The fixture's seeded sub has no thresholds configured, so the fleet of 60
	// is the entire candidate set.
	f.seedThresholdFleet(t, ctx, 60)

	fired, errs := f.engine.ScanThresholds(ctx, 50)
	if len(errs) > 0 {
		t.Fatalf("scan errors: %v", errs)
	}
	if fired != 60 {
		t.Fatalf("drain incomplete on real store: fired=%d, want 60 (subs past the first batch must scan same tick)", fired)
	}

	// T10 — re-scan the drained set: zero fires, zero burned numbers.
	nextBefore, err := f.settings.NextInvoiceNumber(ctx, f.tenantID)
	if err != nil {
		t.Fatalf("number probe: %v", err)
	}
	fired2, errs2 := f.engine.ScanThresholds(ctx, 50)
	if len(errs2) > 0 {
		t.Fatalf("re-scan errors: %v", errs2)
	}
	if fired2 != 0 {
		t.Fatalf("re-scan fired=%d, want 0", fired2)
	}
	nextAfter, err := f.settings.NextInvoiceNumber(ctx, f.tenantID)
	if err != nil {
		t.Fatalf("number probe (after): %v", err)
	}
	if seqOf(t, nextAfter) != seqOf(t, nextBefore)+1 {
		t.Fatalf("re-scan of 60 fired subs burned invoice numbers: %q then %q (want consecutive)", nextBefore, nextAfter)
	}

	// EXPLAIN: with seq scans disabled, the cursored fetch must plan WITHOUT
	// falling back to a (disable-cost) sequential scan — i.e. SOME index can
	// serve the drain. Which index wins (subscriptions_pkey vs the partial
	// threshold index + sort) is a planner choice that varies with stats and
	// PG version — asserting a specific index name was flaky on CI.
	tx, err := f.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin explain tx: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(ctx, `SET LOCAL enable_seqscan = off`); err != nil {
		t.Fatalf("disable seqscan: %v", err)
	}
	rows, err := tx.QueryContext(ctx, `
		EXPLAIN SELECT s.id FROM subscriptions s
		WHERE s.status IN ('active', 'trialing')
		  AND s.livemode = FALSE
		  AND s.test_clock_id IS NULL
		  AND s.id > ''
		  AND (s.billing_threshold_amount_gte IS NOT NULL
		       OR EXISTS (SELECT 1 FROM subscription_item_thresholds sit WHERE sit.subscription_id = s.id))
		ORDER BY s.id ASC
		LIMIT 50
	`)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan plan line: %v", err)
		}
		plan.WriteString(line)
		plan.WriteString("\n")
	}
	if strings.Contains(plan.String(), "Seq Scan on subscriptions") {
		t.Errorf("cursored threshold fetch cannot be index-served (planner forced to a seq scan even with enable_seqscan=off); plan:\n%s", plan.String())
	}
}

// TestThresholdScan_ConcurrentDoubleFire_IndexHolds is T11: two scans racing
// over the same crossed subscription. Both can pass the fire-once probe
// (check-then-act — the probe is an optimization, not the exactly-once
// mechanism); the partial unique index idx_invoices_threshold_unique_per_cycle
// must hold at-most-one invoice, with the loser absorbing ErrAlreadyExists as
// a silent skip, not an error.
func TestThresholdScan_ConcurrentDoubleFire_IndexHolds(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	f := newThresholdFixture(t, "Threshold Concurrent Fire")
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 60*time.Second)
	defer cancel()

	f.ingestUsage(t, ctx, 100, 10)
	resetFalse := false
	if _, err := f.subSvc.SetBillingThresholds(ctx, f.tenantID, f.subID, subscription.BillingThresholdsInput{
		AmountGTE:         500,
		ResetBillingCycle: &resetFalse,
	}); err != nil {
		t.Fatalf("set threshold: %v", err)
	}

	start := make(chan struct{})
	results := make([]int, 2)
	errLists := make([][]error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			<-start
			fired, errs := f.engine.ScanThresholds(ctx, 50)
			results[slot] = fired
			errLists[slot] = errs
		}(i)
	}
	close(start)
	wg.Wait()

	for i, errs := range errLists {
		if len(errs) > 0 {
			t.Fatalf("scan %d errors (loser must absorb ErrAlreadyExists silently): %v", i, errs)
		}
	}
	if results[0]+results[1] != 1 {
		t.Fatalf("combined fired = %d+%d, want exactly 1 across both racers", results[0], results[1])
	}
	invoices := f.listInvoices(t, ctx)
	if len(invoices) != 1 {
		t.Fatalf("unique index failed to hold: %d invoices, want 1", len(invoices))
	}
}

// failableSubAdapter wraps subStoreAdapter with an injectable
// UpdateBillingCycleTx failure — the fault-injection point for the
// fire→reset atomicity test (crash between invoice insert and cycle
// re-anchor, simulated as the second write failing inside the tx).
type failableSubAdapter struct {
	*subStoreAdapter
	updateCycleTxErr error
}

func (a *failableSubAdapter) UpdateBillingCycleTx(ctx context.Context, tx *sql.Tx, tenantID, id string, start, end, next time.Time, anchorDay int) error {
	if a.updateCycleTxErr != nil {
		return a.updateCycleTxErr
	}
	return a.subStoreAdapter.UpdateBillingCycleTx(ctx, tx, tenantID, id, start, end, next, anchorDay)
}

// TestThresholdFire_ResetAtomic_RollsBackOnAdvanceFailure is the ADR-066
// crash-point test on real Postgres: a reset=true fire whose cycle re-anchor
// fails must leave NO invoice behind (single-tx rollback), so the next tick
// retries the whole fire cleanly. The pre-fix two-write shape left the
// invoice committed with the reset stranded forever — the fire-once probe
// blocked every retry, and under base proration (fix 4) the customer's base
// was permanently under-billed.
func TestThresholdFire_ResetAtomic_RollsBackOnAdvanceFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	f := newThresholdFixture(t, "Threshold Atomic Reset")
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 60*time.Second)
	defer cancel()

	f.ingestUsage(t, ctx, 100, 10)
	resetTrue := true
	if _, err := f.subSvc.SetBillingThresholds(ctx, f.tenantID, f.subID, subscription.BillingThresholdsInput{
		AmountGTE:         500,
		ResetBillingCycle: &resetTrue,
	}); err != nil {
		t.Fatalf("set threshold: %v", err)
	}

	// Fault-inject the re-anchor inside the coordinator tx.
	f.subAdapter.updateCycleTxErr = fmt.Errorf("injected: advance failed inside tx")
	fired, errs := f.engine.ScanThresholds(ctx, 50)
	if fired != 0 {
		t.Fatalf("fired = %d, want 0 on a failed reset fire", fired)
	}
	if len(errs) == 0 {
		t.Fatal("scan swallowed the advance failure — want a loud, retryable error")
	}
	if got := len(f.listInvoices(t, ctx)); got != 0 {
		t.Fatalf("invoice survived the rollback: %d invoices, want 0 (the pre-fix stranded-reset shape)", got)
	}
	// The sub's cycle must be untouched.
	sub, err := f.subStore.Get(ctx, f.tenantID, f.subID)
	if err != nil {
		t.Fatalf("get sub: %v", err)
	}
	if !sub.CurrentBillingPeriodStart.Equal(f.cycleStart) {
		t.Fatalf("cycle re-anchored despite rollback: period_start %v, want %v", *sub.CurrentBillingPeriodStart, f.cycleStart)
	}

	// Clear the fault: the next tick retries the WHOLE fire cleanly — invoice
	// lands and the cycle re-anchors, atomically.
	f.subAdapter.updateCycleTxErr = nil
	fired2, errs2 := f.engine.ScanThresholds(ctx, 50)
	if len(errs2) > 0 {
		t.Fatalf("retry scan errors: %v", errs2)
	}
	if fired2 != 1 {
		t.Fatalf("retry fired = %d, want 1 (rollback made the failure retryable)", fired2)
	}
	invoices := f.listInvoices(t, ctx)
	if len(invoices) != 1 {
		t.Fatalf("expected exactly 1 invoice after clean retry, got %d", len(invoices))
	}
	sub, err = f.subStore.Get(ctx, f.tenantID, f.subID)
	if err != nil {
		t.Fatalf("get sub after retry: %v", err)
	}
	if sub.CurrentBillingPeriodStart.Equal(f.cycleStart) {
		t.Fatal("cycle did not re-anchor on the successful retry")
	}
	if !sub.CurrentBillingPeriodStart.Equal(invoices[0].BillingPeriodEnd) {
		t.Errorf("new period_start %v != threshold invoice period_end %v (re-anchor must align with the fire window)",
			*sub.CurrentBillingPeriodStart, invoices[0].BillingPeriodEnd)
	}
}

// TestThresholdFire_ResetProratesBase_RealStore locks fix 4 through the real
// path (previewWithWindow → evaluateThresholds post-processing → persisted
// line items): a reset=true fire on a base-fee plan bills only the elapsed
// fraction of the base, with the standard prorated description and a
// reconciling unit amount, and the cycle re-anchors atomically with the
// insert. The unit tests pin the denominator math; this pins the wiring.
func TestThresholdFire_ResetProratesBase_RealStore(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	f := newThresholdFixture(t, "Threshold Prorated Base")
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 60*time.Second)
	defer cancel()

	// One base-fee sub (fleet helper seeds amount_gte=100, reset=false);
	// flip it to reset=true for the proration arm.
	ids := f.seedThresholdFleet(t, ctx, 1)
	tx, err := f.db.BeginTx(ctx, postgres.TxTenant, f.tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(ctx, `UPDATE subscriptions SET billing_threshold_reset_cycle = TRUE WHERE id = $1`, ids[0]); err != nil {
		t.Fatalf("flip reset: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	fired, errs := f.engine.ScanThresholds(ctx, 50)
	if len(errs) > 0 {
		t.Fatalf("scan errors: %v", errs)
	}
	if fired != 1 {
		t.Fatalf("fired = %d, want 1", fired)
	}

	// Expected proration mirrors the engine convention: segDays = elapsed
	// whole days since cycleStart (fixture anchors 72h ago ⇒ 3), denominator
	// = the plan's full monthly interval from cycleStart (month-length
	// dependent), RoundHalfToEven.
	segDays := int64(3)
	fullDays := int64(math.Round(f.cycleStart.AddDate(0, 1, 0).Sub(f.cycleStart).Hours() / 24))
	wantBase := money.RoundHalfToEven(4900*segDays, fullDays)

	invoices := f.listInvoices(t, ctx)
	if len(invoices) != 1 {
		t.Fatalf("invoices = %d, want 1", len(invoices))
	}
	inv := invoices[0]
	if inv.SubtotalCents != wantBase {
		t.Errorf("subtotal = %d, want %d (prorated %d/%d of 4900)", inv.SubtotalCents, wantBase, segDays, fullDays)
	}
	lines, err := f.invStore.ListLineItems(ctx, f.tenantID, inv.ID)
	if err != nil {
		t.Fatalf("line items: %v", err)
	}
	if len(lines) != 1 {
		t.Fatalf("lines = %d, want 1 (base only)", len(lines))
	}
	li := lines[0]
	if li.AmountCents != wantBase {
		t.Errorf("base line amount = %d, want %d", li.AmountCents, wantBase)
	}
	if li.UnitAmountCents != wantBase {
		t.Errorf("base line unit = %d, want %d (qty 1 — must reconcile qty × unit == amount)", li.UnitAmountCents, wantBase)
	}
	wantDesc := fmt.Sprintf("prorated %d/%d days", segDays, fullDays)
	if !strings.Contains(li.Description, wantDesc) {
		t.Errorf("base line description = %q, want %q suffix", li.Description, wantDesc)
	}

	// The re-anchor committed with the insert: new period starts at the fire.
	sub, err := f.subStore.Get(ctx, f.tenantID, ids[0])
	if err != nil {
		t.Fatalf("get sub: %v", err)
	}
	if !sub.CurrentBillingPeriodStart.Equal(inv.BillingPeriodEnd) {
		t.Errorf("period_start %v != invoice period_end %v (atomic re-anchor)", *sub.CurrentBillingPeriodStart, inv.BillingPeriodEnd)
	}
}

// TestThresholdCycle_DeferredMax_FullWindowOnce is the ADR-066 §4 money lock
// on real Postgres: a reset=false fire (crossed by sum usage + the max
// bucket's committed spend) DROPS the max line; at cycle close the max meter
// must bill over the FULL window — its peak predates the fire, so the
// watermark-clamped residual window contains no peak at all. Pre-fix the
// clamp applied per meter: the deferred max bucket billed from the empty
// residual → billed by NOBODY. The second subscription flips its threshold
// config OFF between fire and close: the exemption must key on the watermark
// invoice's lines (ground truth), not the mutable config.
func TestThresholdCycle_DeferredMax_FullWindowOnce(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	f := newThresholdFixture(t, "Threshold Deferred Max")
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 120*time.Second)
	defer cancel()

	// A max-aggregated meter alongside the fixture's sum meter, on one plan.
	rrvMax, err := f.pricingSvc.CreateRatingRule(ctx, f.tenantID, pricing.CreateRatingRuleInput{
		RuleKey: "gpu_peak", Name: "GPU Peak",
		Mode: domain.PricingFlat, Currency: "USD", FlatAmountCents: decimal.NewFromInt(100),
	})
	if err != nil {
		t.Fatalf("create max rrv: %v", err)
	}
	maxMeter, err := f.pricingSvc.CreateMeter(ctx, f.tenantID, pricing.CreateMeterInput{
		Key: "gpu", Name: "GPU Concurrency", Unit: "gpus",
		Aggregation: "max", RatingRuleVersionID: rrvMax.ID,
	})
	if err != nil {
		t.Fatalf("create max meter: %v", err)
	}
	plan, err := f.pricingSvc.CreatePlan(ctx, f.tenantID, pricing.CreatePlanInput{
		Code: "pln_mixed", Name: "Mixed Plan",
		Currency: "USD", BillingInterval: domain.BillingMonthly,
		BaseAmountCents: 0, MeterIDs: []string{f.meterID, maxMeter.ID},
	})
	if err != nil {
		t.Fatalf("create mixed plan: %v", err)
	}

	// Two isolated customer+sub pairs (usage is customer-scoped; sharing a
	// customer would double-claim events across subs). Sub B flips its
	// threshold config off between fire and close.
	custStore := customer.NewPostgresStore(f.db)
	custSvc := customer.NewService(custStore)
	type pair struct{ custID, subID string }
	pairs := make([]pair, 0, 2)
	now := time.Now().UTC()
	for i := 0; i < 2; i++ {
		cust, err := custSvc.Create(ctx, f.tenantID, customer.CreateInput{
			ExternalID:  fmt.Sprintf("cus_dmax_%d", i),
			DisplayName: fmt.Sprintf("Deferred Max %d", i),
			Email:       fmt.Sprintf("dmax%d@example.test", i),
		})
		if err != nil {
			t.Fatalf("create customer %d: %v", i, err)
		}
		subID := fmt.Sprintf("vlx_sub_dmax_%d", i)
		tx, err := f.db.BeginTx(ctx, postgres.TxTenant, f.tenantID)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO subscriptions (
				id, tenant_id, code, display_name, customer_id, status, billing_time,
				current_billing_period_start, current_billing_period_end, next_billing_at,
				billing_threshold_amount_gte, billing_threshold_reset_cycle,
				created_at, updated_at
			) VALUES ($1, $2, $3, $4, $5, 'active', 'anniversary', $6, $7, $7, 500, FALSE, $8, $8)
		`, subID, f.tenantID, "code-"+subID, "Deferred Max "+subID, cust.ID, f.cycleStart, f.cycleEnd, now); err != nil {
			t.Fatalf("insert sub %d: %v", i, err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO subscription_items (id, tenant_id, subscription_id, plan_id, quantity, metadata, created_at, updated_at)
			VALUES ($1, $2, $3, $4, 1, '{}'::jsonb, $5, $5)
		`, fmt.Sprintf("vlx_si_dmax_%d", i), f.tenantID, subID, plan.ID, f.cycleStart); err != nil {
			t.Fatalf("insert item %d: %v", i, err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO billing_intervals (tenant_id, subscription_id, subscription_item_id, plan_id, quantity, starts_at, source)
			VALUES ($1, $2, $3, $4, 1, $5, 'create')
		`, f.tenantID, subID, fmt.Sprintf("vlx_si_dmax_%d", i), plan.ID, f.cycleStart); err != nil {
			t.Fatalf("insert interval %d: %v", i, err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit %d: %v", i, err)
		}
		// Sum usage crossing the cap (1000 calls @1c = 1000c ≥ 500) + the max
		// peak (100 GPUs, one event EARLY in the cycle — before the fire).
		peakTS := f.cycleStart.Add(30 * time.Minute)
		if _, err := f.usageSvc.Ingest(ctx, f.tenantID, usage.IngestInput{
			CustomerID: cust.ID, MeterID: maxMeter.ID,
			Quantity: decimal.NewFromInt(100), Timestamp: &peakTS,
		}); err != nil {
			t.Fatalf("ingest peak %d: %v", i, err)
		}
		for j := 0; j < 10; j++ {
			ts := f.cycleStart.Add(time.Duration(j+1) * time.Minute)
			if _, err := f.usageSvc.Ingest(ctx, f.tenantID, usage.IngestInput{
				CustomerID: cust.ID, MeterID: f.meterID,
				Quantity: decimal.NewFromInt(100), Timestamp: &ts,
			}); err != nil {
				t.Fatalf("ingest sum %d/%d: %v", i, j, err)
			}
		}
		pairs = append(pairs, pair{cust.ID, subID})
	}

	// Fire: both subs cross (sum 1000c + max committed 10000c ≥ 500).
	// reset=false ⇒ the max line is DROPPED from both threshold invoices.
	fired, errs := f.engine.ScanThresholds(ctx, 50)
	if len(errs) > 0 {
		t.Fatalf("scan errors: %v", errs)
	}
	if fired != 2 {
		t.Fatalf("fired = %d, want 2", fired)
	}

	// Sub B: operator clears the threshold config between fire and close —
	// the close-side exemption must not care.
	txB, err := f.db.BeginTx(ctx, postgres.TxTenant, f.tenantID)
	if err != nil {
		t.Fatalf("begin flip: %v", err)
	}
	if _, err := txB.ExecContext(ctx, `UPDATE subscriptions SET billing_threshold_amount_gte = NULL WHERE id = $1`, pairs[1].subID); err != nil {
		t.Fatalf("clear thresholds: %v", err)
	}
	if err := txB.Commit(); err != nil {
		t.Fatalf("commit flip: %v", err)
	}

	// Rewind both periods so the cycle closes now. The fire billed through
	// wall-now, PAST the rewound period end — the clamped residual window is
	// EMPTY, so pre-fix the deferred max billed from nothing at all.
	pastEnd := time.Now().UTC().Add(-30 * time.Minute).Truncate(time.Microsecond)
	txR, err := f.db.BeginTx(ctx, postgres.TxTenant, f.tenantID)
	if err != nil {
		t.Fatalf("begin rewind: %v", err)
	}
	if _, err := txR.ExecContext(ctx, `
		UPDATE subscriptions SET current_billing_period_end = $1, next_billing_at = $1 WHERE id = $2 OR id = $3
	`, pastEnd, pairs[0].subID, pairs[1].subID); err != nil {
		t.Fatalf("rewind: %v", err)
	}
	if err := txR.Commit(); err != nil {
		t.Fatalf("commit rewind: %v", err)
	}

	generated, failures := f.engine.RunCycleForTenant(ctx, f.tenantID, 50)
	if len(failures) > 0 {
		t.Fatalf("cycle failures: %v", failures)
	}
	if generated != 2 {
		t.Fatalf("generated = %d, want 2 (one close per sub — 0 means the deferred max billed by NOBODY)", generated)
	}

	for i, p := range pairs {
		invs := f.listInvoices(t, ctx)
		var fire, cycle *domain.Invoice
		for k := range invs {
			if invs[k].SubscriptionID != p.subID {
				continue
			}
			if invs[k].BillingReason == domain.BillingReasonThreshold {
				fire = &invs[k]
			} else {
				cycle = &invs[k]
			}
		}
		if fire == nil || cycle == nil {
			t.Fatalf("sub %d: missing fire or cycle invoice", i)
		}
		// Fire billed the sum only (max dropped).
		if fire.SubtotalCents != 1000 {
			t.Errorf("sub %d fire subtotal = %d, want 1000 (sum only; dropped max must not be charged)", i, fire.SubtotalCents)
		}
		// Close bills the max at its FULL-window peak — exactly once, exactly
		// 100 GPUs — and re-bills none of the sum.
		lines, err := f.invStore.ListLineItems(ctx, f.tenantID, cycle.ID)
		if err != nil {
			t.Fatalf("sub %d cycle lines: %v", i, err)
		}
		if len(lines) != 1 {
			t.Fatalf("sub %d cycle invoice lines = %d, want 1 (max only; sum already billed through the fire)", i, len(lines))
		}
		li := lines[0]
		if li.MeterID != maxMeter.ID {
			t.Errorf("sub %d cycle line meter = %s, want the max meter", i, li.MeterID)
		}
		if li.QuantityDecimal.IntPart() != 100 || li.AmountCents != 10000 {
			t.Errorf("sub %d max line qty/amount = %d/%d, want 100/10000 (full-window peak — the clamped residual holds no peak)",
				i, li.QuantityDecimal.IntPart(), li.AmountCents)
		}
	}
}

// TestImmediateCancel_AfterThresholdFire_NoDoubleBill is the ADR-066 §5
// site-set fix on real Postgres: the immediate cancel is the period's SECOND
// closer, and pre-fix it ignored the threshold watermark entirely — after a
// reset=false fire it re-billed the pre-fire sum usage AND the full
// in_arrears base (both already on the fire invoice). The final cancel
// invoice must bill only the post-fire residual sum, zero base, and the
// deferred max bucket over the FULL window.
func TestImmediateCancel_AfterThresholdFire_NoDoubleBill(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	f := newThresholdFixture(t, "Threshold Cancel Double")
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 120*time.Second)
	defer cancel()

	// Max meter + base-fee plan alongside the fixture's sum meter.
	rrvMax, err := f.pricingSvc.CreateRatingRule(ctx, f.tenantID, pricing.CreateRatingRuleInput{
		RuleKey: "gpu_peak_c", Name: "GPU Peak C",
		Mode: domain.PricingFlat, Currency: "USD", FlatAmountCents: decimal.NewFromInt(100),
	})
	if err != nil {
		t.Fatalf("create max rrv: %v", err)
	}
	maxMeter, err := f.pricingSvc.CreateMeter(ctx, f.tenantID, pricing.CreateMeterInput{
		Key: "gpu_c", Name: "GPU C", Unit: "gpus",
		Aggregation: "max", RatingRuleVersionID: rrvMax.ID,
	})
	if err != nil {
		t.Fatalf("create max meter: %v", err)
	}
	plan, err := f.pricingSvc.CreatePlan(ctx, f.tenantID, pricing.CreatePlanInput{
		Code: "pln_cancel_mixed", Name: "Cancel Mixed Plan",
		Currency: "USD", BillingInterval: domain.BillingMonthly,
		BaseAmountCents: 3000, MeterIDs: []string{f.meterID, maxMeter.ID},
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}

	cust, err := customer.NewService(customer.NewPostgresStore(f.db)).Create(ctx, f.tenantID, customer.CreateInput{
		ExternalID: "cus_cancel_fire", DisplayName: "Cancel Fire", Email: "cf@example.test",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	subID := "vlx_sub_cancel_fire"
	now := time.Now().UTC()
	tx, err := f.db.BeginTx(ctx, postgres.TxTenant, f.tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO subscriptions (
			id, tenant_id, code, display_name, customer_id, status, billing_time,
			current_billing_period_start, current_billing_period_end, next_billing_at,
			billing_threshold_amount_gte, billing_threshold_reset_cycle,
			created_at, updated_at
		) VALUES ($1, $2, 'code-cf', 'Cancel Fire Sub', $3, 'active', 'anniversary', $4, $5, $5, 500, FALSE, $6, $6)
	`, subID, f.tenantID, cust.ID, f.cycleStart, f.cycleEnd, now); err != nil {
		t.Fatalf("insert sub: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO subscription_items (id, tenant_id, subscription_id, plan_id, quantity, metadata, created_at, updated_at)
		VALUES ('vlx_si_cancel_fire', $1, $2, $3, 1, '{}'::jsonb, $4, $4)
	`, f.tenantID, subID, plan.ID, f.cycleStart); err != nil {
		t.Fatalf("insert item: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO billing_intervals (tenant_id, subscription_id, subscription_item_id, plan_id, quantity, starts_at, source)
		VALUES ($1, $2, 'vlx_si_cancel_fire', $3, 1, $4, 'create')
	`, f.tenantID, subID, plan.ID, f.cycleStart); err != nil {
		t.Fatalf("insert interval: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Pre-fire usage: 1000c of sum calls + a 100-GPU peak.
	peakTS := f.cycleStart.Add(30 * time.Minute)
	if _, err := f.usageSvc.Ingest(ctx, f.tenantID, usage.IngestInput{
		CustomerID: cust.ID, MeterID: maxMeter.ID,
		Quantity: decimal.NewFromInt(100), Timestamp: &peakTS,
	}); err != nil {
		t.Fatalf("ingest peak: %v", err)
	}
	for j := 0; j < 10; j++ {
		ts := f.cycleStart.Add(time.Duration(j+1) * time.Minute)
		if _, err := f.usageSvc.Ingest(ctx, f.tenantID, usage.IngestInput{
			CustomerID: cust.ID, MeterID: f.meterID,
			Quantity: decimal.NewFromInt(100), Timestamp: &ts,
		}); err != nil {
			t.Fatalf("ingest sum: %v", err)
		}
	}

	// Fire (reset=false): full base 3000 + sum 1000 ride the invoice; max
	// dropped (deferred).
	fired, errs := f.engine.ScanThresholds(ctx, 50)
	if len(errs) > 0 {
		t.Fatalf("scan errors: %v", errs)
	}
	if fired != 1 {
		t.Fatalf("fired = %d, want 1", fired)
	}

	// Immediate cancel mid-period, after the fire.
	canceledAt := time.Now().UTC().Truncate(time.Microsecond)
	txC, err := f.db.BeginTx(ctx, postgres.TxTenant, f.tenantID)
	if err != nil {
		t.Fatalf("begin cancel: %v", err)
	}
	if _, err := txC.ExecContext(ctx, `
		UPDATE subscriptions SET status = 'canceled', canceled_at = $1 WHERE id = $2
	`, canceledAt, subID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if err := txC.Commit(); err != nil {
		t.Fatalf("commit cancel: %v", err)
	}
	sub, err := f.subStore.Get(ctx, f.tenantID, subID)
	if err != nil {
		t.Fatalf("get sub: %v", err)
	}

	finalInv, err := f.engine.BillFinalOnImmediateCancel(ctx, sub)
	if err != nil {
		t.Fatalf("bill final on cancel: %v", err)
	}
	if finalInv.ID == "" {
		t.Fatal("expected a final cancel invoice (the deferred max bucket must bill somewhere)")
	}

	lines, err := f.invStore.ListLineItems(ctx, f.tenantID, finalInv.ID)
	if err != nil {
		t.Fatalf("final lines: %v", err)
	}
	var maxLines, sumLines, baseLines int
	for _, li := range lines {
		switch {
		case li.LineType == domain.LineTypeBaseFee:
			baseLines++
		case li.MeterID == maxMeter.ID:
			maxLines++
			if li.QuantityDecimal.IntPart() != 100 || li.AmountCents != 10000 {
				t.Errorf("max line qty/amount = %d/%d, want 100/10000 (full-window peak)", li.QuantityDecimal.IntPart(), li.AmountCents)
			}
		case li.MeterID == f.meterID:
			sumLines++
		}
	}
	// The fire billed base + pre-fire sum; the cancel bills NEITHER again.
	if baseLines != 0 {
		t.Errorf("final cancel invoice re-billed %d base line(s) — the fire already carried the full base", baseLines)
	}
	if sumLines != 0 {
		t.Errorf("final cancel invoice re-billed %d sum line(s) — pre-fire usage already on the fire invoice, and no post-fire usage exists", sumLines)
	}
	if maxLines != 1 {
		t.Errorf("deferred max lines on final invoice = %d, want exactly 1", maxLines)
	}
	if finalInv.SubtotalCents != 10000 {
		t.Errorf("final invoice subtotal = %d, want 10000 (deferred max only)", finalInv.SubtotalCents)
	}
}
