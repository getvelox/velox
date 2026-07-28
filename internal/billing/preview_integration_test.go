package billing_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/billing"
	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/subscription"
	"github.com/sagarsuperuser/velox/internal/tenant"
	"github.com/sagarsuperuser/velox/internal/testutil"
	"github.com/sagarsuperuser/velox/internal/usage"
)

// previewFixture wires real per-domain stores against a clean test DB so
// the create_preview path exercises the full SQL → engine → wire-shape
// chain. Tests then ingest events and assert the preview emits the same
// totals the cycle scan would produce — the parity guarantee the whole
// feature rests on.
//
// Mirrors customerUsageFixture (see internal/usage/customer_usage_integration_test.go)
// but stops short of running RunCycle — preview composes off the engine's
// previewWithWindow path which is read-only by construction.
type previewFixture struct {
	db          *postgres.DB
	tenantID    string
	customerSvc *customer.Service
	pricingSvc  *pricing.Service
	subStore    *subscription.PostgresStore
	subSvc      *subscription.Service
	usageSvc    *usage.Service
	preview     *billing.PreviewService
	engine      *billing.Engine
}

func newPreviewFixture(t *testing.T, name string) *previewFixture {
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

	engine := billing.NewEngine(
		&subStoreAdapter{subStore},
		&usageStoreAdapter{usageStore},
		&pricingStoreAdapter{pricingStore},
		&invoiceStoreAdapter{invoiceStore},
		nil, settingsStore, testPaymentSetupsNoPM{}, testChargerSentinel{}, nil,
	)
	engine.SetIntervalReader(subStore)

	preview := billing.NewPreviewService(engine, customerStore, subStore)

	return &previewFixture{
		db:          db,
		tenantID:    tenantID,
		customerSvc: customerSvc,
		pricingSvc:  pricingSvc,
		subStore:    subStore,
		subSvc:      subSvc,
		usageSvc:    usageSvc,
		preview:     preview,
		engine:      engine,
	}
}

// seedSubscription mirrors customer-usage's seedCustomerWithSub. Creates
// a customer, a plan with the supplied meter, and an active subscription
// with a current billing cycle of [from, to). Returns customer / plan /
// sub IDs.
func (f *previewFixture) seedSubscription(
	t *testing.T,
	ctx context.Context,
	externalID, planCode, meterID string,
	cycleStart, cycleEnd time.Time,
) (custID, planID, subID string) {
	t.Helper()

	cust, err := f.customerSvc.Create(ctx, f.tenantID, customer.CreateInput{
		ExternalID:  externalID,
		DisplayName: externalID,
		Email:       externalID + "@example.test",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}

	plan, err := f.pricingSvc.CreatePlan(ctx, f.tenantID, pricing.CreatePlanInput{
		Code:            planCode,
		Name:            planCode,
		Currency:        "USD",
		BillingInterval: domain.BillingMonthly,
		BaseAmountCents: 0,
		MeterIDs:        []string{meterID},
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}

	subID = postgres.NewID("vlx_sub")
	tx, err := f.db.BeginTx(ctx, postgres.TxTenant, f.tenantID)
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
	`, subID, f.tenantID, "code-"+externalID, planCode+"-sub", cust.ID,
		cycleStart, cycleEnd, now)
	if err != nil {
		t.Fatalf("insert sub: %v", err)
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO subscription_items (id, tenant_id, subscription_id, plan_id, quantity, metadata, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 1, '{}'::jsonb, $5, $5)
	`, postgres.NewID("vlx_si"), f.tenantID, subID, plan.ID, now)
	if err != nil {
		t.Fatalf("insert sub item: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit sub: %v", err)
	}
	return cust.ID, plan.ID, subID
}

// countInvoiceRows counts invoice + invoice_line_item rows for the
// fixture's tenant. Used by TestCreatePreview_NoWrites to assert the
// preview path persists nothing — the standout property of this surface
// vs. /v1/invoices/{id}/finalize.
func (f *previewFixture) countInvoiceRows(t *testing.T, ctx context.Context) (invoices, lineItems int) {
	t.Helper()
	tx, err := f.db.BeginTx(ctx, postgres.TxTenant, f.tenantID)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer postgres.Rollback(tx)

	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM invoices WHERE tenant_id = $1`, f.tenantID).Scan(&invoices); err != nil {
		t.Fatalf("count invoices: %v", err)
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM invoice_line_items WHERE tenant_id = $1`, f.tenantID).Scan(&lineItems); err != nil {
		t.Fatalf("count line items: %v", err)
	}
	return invoices, lineItems
}

// TestCreatePreview_SingleMeterFlatParity is the parity guarantee:
// preview math == invoice math for the single-meter flat-pricing case.
// Same fixture as TestCustomerUsage_SingleMeterFlatParity (100 events ×
// qty=10 × 1¢ = 1000c) so a regression in one shows up in the other.
func TestCreatePreview_SingleMeterFlatParity(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	f := newPreviewFixture(t, "Create Preview Single Meter")
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 30*time.Second)
	defer cancel()

	cycleStart := time.Now().UTC().Truncate(time.Hour).Add(-120 * time.Hour) // far enough back that 100 hourly events stay in the past (future live ingest is rejected)
	cycleEnd := cycleStart.Add(30 * 24 * time.Hour)

	rrv, err := f.pricingSvc.CreateRatingRule(ctx, f.tenantID, pricing.CreateRatingRuleInput{
		RuleKey: "tokens_flat", Name: "Tokens Flat",
		Mode: domain.PricingFlat, Currency: "USD", FlatAmountCents: decimal.NewFromInt(1),
	})
	if err != nil {
		t.Fatalf("create rrv: %v", err)
	}

	meter, err := f.pricingSvc.CreateMeter(ctx, f.tenantID, pricing.CreateMeterInput{
		Key: "tokens", Name: "Tokens", Unit: "tokens",
		Aggregation: "sum", RatingRuleVersionID: rrv.ID,
	})
	if err != nil {
		t.Fatalf("create meter: %v", err)
	}

	custID, _, subID := f.seedSubscription(t, ctx, "cus_preview_flat", "pln_flat", meter.ID, cycleStart, cycleEnd)

	// 100 in-cycle events × qty=10 × 1¢ = 1000c.
	for i := 0; i < 100; i++ {
		ts := cycleStart.Add(time.Duration(i) * time.Hour)
		if _, err := f.usageSvc.Ingest(ctx, f.tenantID, usage.IngestInput{
			CustomerID: custID, MeterID: meter.ID,
			Quantity: decimal.NewFromInt(10), Timestamp: &ts,
		}); err != nil {
			t.Fatalf("ingest %d: %v", i, err)
		}
	}

	res, err := f.preview.CreatePreview(ctx, f.tenantID, billing.CreatePreviewRequest{
		CustomerID: custID,
	})
	if err != nil {
		t.Fatalf("CreatePreview: %v", err)
	}
	if res.SubscriptionID != subID {
		t.Errorf("subscription_id: got %q want %q", res.SubscriptionID, subID)
	}
	if !res.BillingPeriodStart.Equal(cycleStart) {
		t.Errorf("billing_period_start: got %v want %v", res.BillingPeriodStart, cycleStart)
	}
	if !res.BillingPeriodEnd.Equal(cycleEnd) {
		t.Errorf("billing_period_end: got %v want %v", res.BillingPeriodEnd, cycleEnd)
	}
	if len(res.Lines) != 1 {
		t.Fatalf("lines: got %d want 1 (single-meter no-base-fee plan)", len(res.Lines))
	}
	if res.Lines[0].LineType != "usage" {
		t.Errorf("line_type: got %q want usage", res.Lines[0].LineType)
	}
	if res.Lines[0].AmountCents != 1000 {
		t.Errorf("amount_cents: got %d want 1000", res.Lines[0].AmountCents)
	}
	if !res.Lines[0].Quantity.Equal(decimal.NewFromInt(1000)) {
		t.Errorf("quantity: got %s want 1000", res.Lines[0].Quantity.String())
	}
	if len(res.Totals) != 1 {
		t.Fatalf("totals: got %d want 1", len(res.Totals))
	}
	if res.Totals[0].Currency != "USD" || res.Totals[0].AmountCents != 1000 {
		t.Errorf("totals[0]: got %+v want USD/1000", res.Totals[0])
	}
}

// TestCreatePreview_MultiDimDimensionMatchEcho proves a multi-dim meter
// emits one line per (meter, rule) pair with dimension_match echoed.
// Same fixture as TestCustomerUsage_MultiDimDimensionMatchEcho (1000 input
// @3¢ + 100 output @5¢ = 3500c).
func TestCreatePreview_MultiDimDimensionMatchEcho(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	f := newPreviewFixture(t, "Create Preview Multi Dim")
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 30*time.Second)
	defer cancel()

	cycleStart := time.Now().UTC().Truncate(time.Hour).Add(-24 * time.Hour)
	cycleEnd := cycleStart.Add(30 * 24 * time.Hour)

	rrvIn, err := f.pricingSvc.CreateRatingRule(ctx, f.tenantID, pricing.CreateRatingRuleInput{
		RuleKey: "tokens_input", Name: "Input", Mode: domain.PricingFlat, Currency: "USD", FlatAmountCents: decimal.NewFromInt(3),
	})
	if err != nil {
		t.Fatalf("create rrv input: %v", err)
	}
	rrvOut, err := f.pricingSvc.CreateRatingRule(ctx, f.tenantID, pricing.CreateRatingRuleInput{
		RuleKey: "tokens_output", Name: "Output", Mode: domain.PricingFlat, Currency: "USD", FlatAmountCents: decimal.NewFromInt(5),
	})
	if err != nil {
		t.Fatalf("create rrv output: %v", err)
	}

	meter, err := f.pricingSvc.CreateMeter(ctx, f.tenantID, pricing.CreateMeterInput{
		Key: "tokens_dim", Name: "Tokens Dim", Unit: "tokens",
		Aggregation: "sum", RatingRuleVersionID: rrvIn.ID,
	})
	if err != nil {
		t.Fatalf("create meter: %v", err)
	}

	if _, err := f.pricingSvc.UpsertMeterPricingRule(ctx, f.tenantID, pricing.UpsertMeterPricingRuleInput{
		MeterID: meter.ID, RatingRuleVersionID: rrvIn.ID,
		DimensionMatch:  map[string]any{"operation": "input"},
		AggregationMode: domain.AggSum, Priority: 10,
	}); err != nil {
		t.Fatalf("upsert mpr input: %v", err)
	}
	if _, err := f.pricingSvc.UpsertMeterPricingRule(ctx, f.tenantID, pricing.UpsertMeterPricingRuleInput{
		MeterID: meter.ID, RatingRuleVersionID: rrvOut.ID,
		DimensionMatch:  map[string]any{"operation": "output"},
		AggregationMode: domain.AggSum, Priority: 10,
	}); err != nil {
		t.Fatalf("upsert mpr output: %v", err)
	}

	custID, _, _ := f.seedSubscription(t, ctx, "cus_preview_dim", "pln_dim", meter.ID, cycleStart, cycleEnd)

	// 10 input events qty=100 = 1000 input units → 3000c.
	for i := 0; i < 10; i++ {
		ts := cycleStart.Add(time.Duration(i) * time.Hour)
		if _, err := f.usageSvc.Ingest(ctx, f.tenantID, usage.IngestInput{
			CustomerID: custID, MeterID: meter.ID, Quantity: decimal.NewFromInt(100),
			Dimensions: map[string]any{"operation": "input"}, Timestamp: &ts,
		}); err != nil {
			t.Fatalf("ingest input %d: %v", i, err)
		}
	}
	// 5 output events qty=20 = 100 output units → 500c.
	for i := 0; i < 5; i++ {
		ts := cycleStart.Add(time.Duration(i+11) * time.Hour)
		if _, err := f.usageSvc.Ingest(ctx, f.tenantID, usage.IngestInput{
			CustomerID: custID, MeterID: meter.ID, Quantity: decimal.NewFromInt(20),
			Dimensions: map[string]any{"operation": "output"}, Timestamp: &ts,
		}); err != nil {
			t.Fatalf("ingest output %d: %v", i, err)
		}
	}

	res, err := f.preview.CreatePreview(ctx, f.tenantID, billing.CreatePreviewRequest{
		CustomerID: custID,
	})
	if err != nil {
		t.Fatalf("CreatePreview: %v", err)
	}
	if len(res.Lines) < 2 {
		t.Fatalf("lines: got %d want at least 2 (one per rule)", len(res.Lines))
	}

	var totalCents int64
	dimMatchSeen := 0
	for _, line := range res.Lines {
		if line.LineType != "usage" {
			continue
		}
		if line.DimensionMatch == nil {
			t.Errorf("line %q dimension_match must echo the meter pricing rule's filter", line.RuleKey)
			continue
		}
		dimMatchSeen++
		totalCents += line.AmountCents
	}
	if dimMatchSeen < 2 {
		t.Fatalf("expected dimension_match on at least 2 lines, got %d", dimMatchSeen)
	}
	// 1000 input × 3¢ + 100 output × 5¢ = 3500c.
	if totalCents != 3500 {
		t.Errorf("total cents: got %d want 3500 (1000×3 + 100×5)", totalCents)
	}
	if len(res.Totals) != 1 || res.Totals[0].Currency != "USD" || res.Totals[0].AmountCents != 3500 {
		t.Errorf("totals: got %+v want USD/3500", res.Totals)
	}
}

// TestCreatePreview_NoWrites is the standout property of this surface:
// the preview composes reads only. Counts invoices + invoice_line_items
// before and after a CreatePreview call and asserts unchanged.
func TestCreatePreview_NoWrites(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	f := newPreviewFixture(t, "Create Preview No Writes")
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 30*time.Second)
	defer cancel()

	cycleStart := time.Now().UTC().Truncate(time.Hour).Add(-12 * time.Hour)
	cycleEnd := cycleStart.Add(30 * 24 * time.Hour)

	rrv, err := f.pricingSvc.CreateRatingRule(ctx, f.tenantID, pricing.CreateRatingRuleInput{
		RuleKey: "x", Name: "x", Mode: domain.PricingFlat, Currency: "USD", FlatAmountCents: decimal.NewFromInt(1),
	})
	if err != nil {
		t.Fatalf("create rrv: %v", err)
	}
	meter, err := f.pricingSvc.CreateMeter(ctx, f.tenantID, pricing.CreateMeterInput{
		Key: "x", Name: "x", Unit: "u", Aggregation: "sum", RatingRuleVersionID: rrv.ID,
	})
	if err != nil {
		t.Fatalf("create meter: %v", err)
	}

	custID, _, _ := f.seedSubscription(t, ctx, "cus_nowrites", "pln_nowrites", meter.ID, cycleStart, cycleEnd)

	for i := 0; i < 5; i++ {
		ts := cycleStart.Add(time.Duration(i) * time.Hour)
		if _, err := f.usageSvc.Ingest(ctx, f.tenantID, usage.IngestInput{
			CustomerID: custID, MeterID: meter.ID,
			Quantity: decimal.NewFromInt(10), Timestamp: &ts,
		}); err != nil {
			t.Fatalf("ingest %d: %v", i, err)
		}
	}

	beforeInvoices, beforeLineItems := f.countInvoiceRows(t, ctx)

	if _, err := f.preview.CreatePreview(ctx, f.tenantID, billing.CreatePreviewRequest{CustomerID: custID}); err != nil {
		t.Fatalf("CreatePreview: %v", err)
	}

	afterInvoices, afterLineItems := f.countInvoiceRows(t, ctx)

	if afterInvoices != beforeInvoices {
		t.Errorf("invoices row count drifted: before=%d after=%d (preview must not persist)", beforeInvoices, afterInvoices)
	}
	if afterLineItems != beforeLineItems {
		t.Errorf("invoice_line_items row count drifted: before=%d after=%d (preview must not persist)", beforeLineItems, afterLineItems)
	}
}

// TestCreatePreview_CrossTenantIsolation confirms RLS hides the customer
// from a different tenant — the lookup surfaces ErrNotFound, which the
// handler maps to 404. Same property customer-usage relies on.
func TestCreatePreview_CrossTenantIsolation(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	f := newPreviewFixture(t, "Create Preview Cross Tenant A")
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 15*time.Second)
	defer cancel()

	rrv, err := f.pricingSvc.CreateRatingRule(ctx, f.tenantID, pricing.CreateRatingRuleInput{
		RuleKey: "x", Name: "x", Mode: domain.PricingFlat, Currency: "USD", FlatAmountCents: decimal.NewFromInt(1),
	})
	if err != nil {
		t.Fatalf("create rrv: %v", err)
	}
	meter, err := f.pricingSvc.CreateMeter(ctx, f.tenantID, pricing.CreateMeterInput{
		Key: "x", Name: "x", Unit: "u", Aggregation: "sum", RatingRuleVersionID: rrv.ID,
	})
	if err != nil {
		t.Fatalf("create meter: %v", err)
	}
	cycleStart := time.Now().UTC().Truncate(time.Hour).Add(-24 * time.Hour)
	cycleEnd := cycleStart.Add(30 * 24 * time.Hour)
	custA, _, _ := f.seedSubscription(t, ctx, "cus_xtenA", "pln_a", meter.ID, cycleStart, cycleEnd)

	tenantB := testutil.CreateTestTenant(t, f.db, "Create Preview Cross Tenant B")
	_, err = f.preview.CreatePreview(ctx, tenantB, billing.CreatePreviewRequest{CustomerID: custA})
	if err == nil {
		t.Fatal("expected not-found error for cross-tenant lookup")
	}
}

// TestCreatePreview_CustomerHasNoSubscription asserts the documented
// coded error for the empty-customer case — symmetric with the
// customer-usage surface so the dashboard's empty-state branch covers
// both reads.
func TestCreatePreview_CustomerHasNoSubscription(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	f := newPreviewFixture(t, "Create Preview No Sub")
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 15*time.Second)
	defer cancel()

	cust, err := f.customerSvc.Create(ctx, f.tenantID, customer.CreateInput{
		ExternalID: "cus_nosub", DisplayName: "No Sub", Email: "nosub@example.test",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}

	_, err = f.preview.CreatePreview(ctx, f.tenantID, billing.CreatePreviewRequest{CustomerID: cust.ID})
	if err == nil {
		t.Fatal("expected error when customer has no subscription")
	}
	if errs.Code(err) != "customer_has_no_subscription" {
		t.Errorf("error code: got %q want customer_has_no_subscription", errs.Code(err))
	}
}

// TestCreatePreview_ExplicitSubscriptionWrongCustomer is the cross-customer
// safety check: passing an ID that belongs to a different customer in the
// same tenant surfaces as 422 invalid_request, not as a silent preview of
// the wrong sub.
func TestCreatePreview_ExplicitSubscriptionWrongCustomer(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	f := newPreviewFixture(t, "Create Preview Wrong Customer")
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 15*time.Second)
	defer cancel()

	rrv, err := f.pricingSvc.CreateRatingRule(ctx, f.tenantID, pricing.CreateRatingRuleInput{
		RuleKey: "x", Name: "x", Mode: domain.PricingFlat, Currency: "USD", FlatAmountCents: decimal.NewFromInt(1),
	})
	if err != nil {
		t.Fatalf("create rrv: %v", err)
	}
	meter, err := f.pricingSvc.CreateMeter(ctx, f.tenantID, pricing.CreateMeterInput{
		Key: "x", Name: "x", Unit: "u", Aggregation: "sum", RatingRuleVersionID: rrv.ID,
	})
	if err != nil {
		t.Fatalf("create meter: %v", err)
	}
	cycleStart := time.Now().UTC().Truncate(time.Hour).Add(-24 * time.Hour)
	cycleEnd := cycleStart.Add(30 * 24 * time.Hour)

	_, _, subA := f.seedSubscription(t, ctx, "cus_wrong_a", "pln_wrong_a", meter.ID, cycleStart, cycleEnd)
	custB, _, _ := f.seedSubscription(t, ctx, "cus_wrong_b", "pln_wrong_b", meter.ID, cycleStart, cycleEnd)

	// Customer B asking for Customer A's subscription — must reject.
	_, err = f.preview.CreatePreview(ctx, f.tenantID, billing.CreatePreviewRequest{
		CustomerID:     custB,
		SubscriptionID: subA,
	})
	if err == nil {
		t.Fatal("expected error when subscription does not belong to customer")
	}
	if got := errs.Field(err); got != "subscription_id" {
		t.Errorf("field: got %q want subscription_id", got)
	}
}

// setUsageCap turns on a blocking per-period usage cap on an existing sub.
// usage_cap_units lives on the subscription (not the item), so this UPDATE
// doesn't fire the subscription_items change trigger.
func (f *previewFixture) setUsageCap(t *testing.T, ctx context.Context, subID string, capUnits int64) {
	t.Helper()
	tx, err := f.db.BeginTx(ctx, postgres.TxTenant, f.tenantID)
	if err != nil {
		t.Fatalf("begin cap tx: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(ctx,
		`UPDATE subscriptions SET usage_cap_units = $2, overage_action = 'block' WHERE id = $1`,
		subID, capUnits); err != nil {
		t.Fatalf("set usage cap: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit cap: %v", err)
	}
}

// seedItemChange records a mid-period plan change in subscription_item_changes,
// the row ListItemChangesInPeriod reads to trigger segment-aware billing.
// livemode=false matches the WithLivemode(false) test ctx and the sub's row
// under the table's RLS policy.
func (f *previewFixture) seedItemChange(t *testing.T, ctx context.Context, subID, toPlanID string, changedAt time.Time) {
	t.Helper()
	tx, err := f.db.BeginTx(ctx, postgres.TxTenant, f.tenantID)
	if err != nil {
		t.Fatalf("begin change tx: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO subscription_item_changes
			(tenant_id, livemode, subscription_id, change_type, to_plan_id, changed_at, created_at)
		VALUES ($1, false, $2, 'plan', $3, $4, now())
	`, f.tenantID, subID, toPlanID, changedAt); err != nil {
		t.Fatalf("insert item change: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit change: %v", err)
	}
}

// TestCreatePreview_EstimateScopeWarnings pins the Tier-1 honesty warnings:
// create_preview is a full-period estimate that doesn't replicate usage-cap
// scaling or mid-period segment proration (ADR-045), so it must SAY SO on the
// warnings channel rather than silently hand back a number the cycle won't
// match. Clean subs (the common/wedge case) stay warning-free.
func TestCreatePreview_EstimateScopeWarnings(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	f := newPreviewFixture(t, "Create Preview Warnings")
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 60*time.Second)
	defer cancel()

	cycleStart := time.Now().UTC().Truncate(time.Hour).Add(-120 * time.Hour)
	cycleEnd := cycleStart.Add(30 * 24 * time.Hour)

	rrv, err := f.pricingSvc.CreateRatingRule(ctx, f.tenantID, pricing.CreateRatingRuleInput{
		RuleKey: "warn_flat", Name: "Warn Flat",
		Mode: domain.PricingFlat, Currency: "USD", FlatAmountCents: decimal.NewFromInt(1),
	})
	if err != nil {
		t.Fatalf("create rrv: %v", err)
	}
	meter, err := f.pricingSvc.CreateMeter(ctx, f.tenantID, pricing.CreateMeterInput{
		Key: "warn_tokens", Name: "Warn Tokens", Unit: "tokens",
		Aggregation: "sum", RatingRuleVersionID: rrv.ID,
	})
	if err != nil {
		t.Fatalf("create meter: %v", err)
	}

	ingest := func(custID string, n int) {
		t.Helper()
		for i := 0; i < n; i++ {
			ts := cycleStart.Add(time.Duration(i) * time.Hour)
			if _, err := f.usageSvc.Ingest(ctx, f.tenantID, usage.IngestInput{
				CustomerID: custID, MeterID: meter.ID,
				Quantity: decimal.NewFromInt(10), Timestamp: &ts,
			}); err != nil {
				t.Fatalf("ingest: %v", err)
			}
		}
	}
	warns := func(ws []string, substr string) bool {
		for _, w := range ws {
			if strings.Contains(w, substr) {
				return true
			}
		}
		return false
	}
	preview := func(custID string) billing.PreviewResult {
		t.Helper()
		res, err := f.preview.CreatePreview(ctx, f.tenantID, billing.CreatePreviewRequest{CustomerID: custID})
		if err != nil {
			t.Fatalf("CreatePreview: %v", err)
		}
		return res
	}

	t.Run("clean sub (no cap, no change) has no scope warnings", func(t *testing.T) {
		custID, _, _ := f.seedSubscription(t, ctx, "cus_clean", "pln_clean", meter.ID, cycleStart, cycleEnd)
		ingest(custID, 100)
		res := preview(custID)
		if len(res.Warnings) != 0 {
			t.Errorf("clean sub must have no scope warnings, got %v", res.Warnings)
		}
	})

	t.Run("blocking usage cap surfaces the cap warning", func(t *testing.T) {
		custID, _, subID := f.seedSubscription(t, ctx, "cus_cap", "pln_cap", meter.ID, cycleStart, cycleEnd)
		f.setUsageCap(t, ctx, subID, 500) // 100×10 = 1000 units > 500 cap
		ingest(custID, 100)
		res := preview(custID)
		if !warns(res.Warnings, "usage cap") {
			t.Errorf("capped sub must warn the estimate excludes the cap, got %v", res.Warnings)
		}
		if warns(res.Warnings, "mid-period") {
			t.Errorf("capped sub with no change must NOT emit the segment warning, got %v", res.Warnings)
		}
	})

	t.Run("mid-period item change surfaces the segment warning", func(t *testing.T) {
		custID, planID, subID := f.seedSubscription(t, ctx, "cus_seg", "pln_seg", meter.ID, cycleStart, cycleEnd)
		f.seedItemChange(t, ctx, subID, planID, cycleStart.Add(24*time.Hour))
		ingest(custID, 10)
		res := preview(custID)
		if !warns(res.Warnings, "mid-period proration") {
			t.Errorf("mid-period-changed sub must warn about segment proration, got %v", res.Warnings)
		}
		if warns(res.Warnings, "usage cap") {
			t.Errorf("uncapped sub must NOT emit the cap warning, got %v", res.Warnings)
		}
	})
}
