package billing_test

import (
	"context"
	"database/sql"
	"github.com/sagarsuperuser/velox/internal/platform/leader"
	"github.com/sagarsuperuser/velox/internal/platform/leader/leadertest"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/billing"
	"github.com/sagarsuperuser/velox/internal/credit"
	"github.com/sagarsuperuser/velox/internal/creditnote"
	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/subscription"
	"github.com/sagarsuperuser/velox/internal/subscription/subscriptiontest"
	"github.com/sagarsuperuser/velox/internal/tenant"
	"github.com/sagarsuperuser/velox/internal/testutil"
	"github.com/sagarsuperuser/velox/internal/usage"
)

// subStoreAdapter wraps subscription.PostgresStore to implement billing.SubscriptionReader
type subStoreAdapter struct {
	store *subscription.PostgresStore
}

func (a *subStoreAdapter) GetDueBilling(ctx context.Context, now, dueBy time.Time, exclude []string, limit int) ([]domain.Subscription, error) {
	return a.store.GetDueBilling(ctx, now, dueBy, exclude, limit)
}

func (a *subStoreAdapter) GetDueBillingForClock(ctx context.Context, tenantID, clockID string, exclude []string, limit int) ([]domain.Subscription, error) {
	return a.store.GetDueBillingForClock(ctx, tenantID, clockID, exclude, limit)
}

func (a *subStoreAdapter) GetDueBillingForTenant(ctx context.Context, tenantID string, now, dueBy time.Time, exclude []string, limit int) ([]domain.Subscription, error) {
	return a.store.GetDueBillingForTenant(ctx, tenantID, now, dueBy, exclude, limit)
}

func (a *subStoreAdapter) Get(ctx context.Context, tenantID, id string) (domain.Subscription, error) {
	return a.store.Get(ctx, tenantID, id)
}

func (a *subStoreAdapter) RecordBillingFailure(ctx context.Context, tenantID, id string, f subscription.BillingFailureWrite) (bool, error) {
	return a.store.RecordBillingFailure(ctx, tenantID, id, f)
}

func (a *subStoreAdapter) WithTenantTx(ctx context.Context, tenantID string, fn func(tx *sql.Tx) error) error {
	return a.store.WithTenantTx(ctx, tenantID, fn)
}

func (a *subStoreAdapter) ClosePeriodTx(ctx context.Context, tx *sql.Tx, tenantID, id string, expected subscription.PeriodSnapshot, start, end, next time.Time, anchorDay int) error {
	return a.store.ClosePeriodTx(ctx, tx, tenantID, id, expected, start, end, next, anchorDay)
}

func (a *subStoreAdapter) ClosePeriod(ctx context.Context, tenantID, id string, expected subscription.PeriodSnapshot, start, end, next time.Time, anchorDay int) error {
	return a.store.ClosePeriod(ctx, tenantID, id, expected, start, end, next, anchorDay)
}

func (a *subStoreAdapter) ApplyDuePendingItemPlansAtomic(ctx context.Context, tenantID, id string, now time.Time) ([]domain.SubscriptionItem, error) {
	return a.store.ApplyDuePendingItemPlansAtomic(ctx, tenantID, id, now)
}

func (a *subStoreAdapter) FireScheduledCancellationTx(ctx context.Context, tx *sql.Tx, tenantID, id string, expected subscription.PeriodSnapshot, at time.Time) (domain.Subscription, error) {
	return a.store.FireScheduledCancellationTx(ctx, tx, tenantID, id, expected, at)
}

func (a *subStoreAdapter) ClearPauseCollection(ctx context.Context, tenantID, id string) (domain.Subscription, error) {
	return a.store.ClearPauseCollection(ctx, tenantID, id)
}

func (a *subStoreAdapter) ListWithThresholds(ctx context.Context, livemode bool, afterID string, limit int) ([]domain.Subscription, error) {
	return a.store.ListWithThresholds(ctx, livemode, afterID, limit)
}

func (a *subStoreAdapter) ListWithThresholdsForClock(ctx context.Context, tenantID, clockID, afterID string, limit int) ([]domain.Subscription, error) {
	return a.store.ListWithThresholdsForClock(ctx, tenantID, clockID, afterID, limit)
}

func (a *subStoreAdapter) ListItemChangesInPeriod(ctx context.Context, tenantID, subscriptionID string, periodStart, periodEnd time.Time) ([]domain.SubscriptionItemChange, error) {
	return a.store.ListItemChangesInPeriod(ctx, tenantID, subscriptionID, periodStart, periodEnd)
}

// pricingStoreAdapter wraps pricing.PostgresStore to implement billing.PricingReader
type pricingStoreAdapter struct {
	store *pricing.PostgresStore
}

func (a *pricingStoreAdapter) GetPlan(ctx context.Context, tenantID, id string) (domain.Plan, error) {
	return a.store.GetPlan(ctx, tenantID, id)
}

func (a *pricingStoreAdapter) GetMeter(ctx context.Context, tenantID, id string) (domain.Meter, error) {
	return a.store.GetMeter(ctx, tenantID, id)
}

func (a *pricingStoreAdapter) GetRatingRule(ctx context.Context, tenantID, id string) (domain.RatingRuleVersion, error) {
	return a.store.GetRatingRule(ctx, tenantID, id)
}

func (a *pricingStoreAdapter) GetRuleByKeyAsOf(ctx context.Context, tenantID, ruleKey string, asOf time.Time) (domain.RatingRuleVersion, error) {
	return a.store.GetRuleByKeyAsOf(ctx, tenantID, ruleKey, asOf)
}

func (a *pricingStoreAdapter) GetOverrideByKeyAsOf(ctx context.Context, tenantID, customerID, ruleKey string, asOf time.Time) (domain.CustomerPriceOverride, error) {
	return a.store.GetOverrideByKeyAsOf(ctx, tenantID, customerID, ruleKey, asOf)
}

func (a *pricingStoreAdapter) ListMeterPricingRulesByMeter(ctx context.Context, tenantID, meterID string) ([]domain.MeterPricingRule, error) {
	return a.store.ListMeterPricingRulesByMeter(ctx, tenantID, meterID)
}

// usageStoreAdapter wraps usage.PostgresStore to implement billing.UsageAggregator
type usageStoreAdapter struct {
	store *usage.PostgresStore
}

func (a *usageStoreAdapter) AggregateForBillingPeriodByAgg(ctx context.Context, tenantID, customerID string, meters map[string]string, from, to time.Time) (map[string]decimal.Decimal, error) {
	return a.store.AggregateForBillingPeriodByAgg(ctx, tenantID, customerID, meters, from, to)
}

func (a *usageStoreAdapter) AggregateByPricingRules(ctx context.Context, tenantID, customerID, meterID string, defaultMode domain.AggregationMode, from, to time.Time) ([]domain.RuleAggregation, error) {
	return a.store.AggregateByPricingRules(ctx, tenantID, customerID, meterID, defaultMode, from, to)
}

// invoiceStoreAdapter wraps invoice.PostgresStore to implement billing.InvoiceWriter
type invoiceStoreAdapter struct {
	store *invoice.PostgresStore
}

func (a *invoiceStoreAdapter) GetInvoice(ctx context.Context, tenantID, id string) (domain.Invoice, error) {
	return a.store.Get(ctx, tenantID, id)
}

func (a *invoiceStoreAdapter) MarkPaid(ctx context.Context, tenantID, id string, stripePaymentIntentID string, paidAt time.Time) (domain.Invoice, error) {
	return a.store.MarkPaid(ctx, tenantID, id, stripePaymentIntentID, paidAt)
}

func (a *invoiceStoreAdapter) CreateInvoiceWithLineItems(ctx context.Context, tenantID string, inv domain.Invoice, items []domain.InvoiceLineItem) (domain.Invoice, error) {
	return a.store.CreateWithLineItems(ctx, tenantID, inv, items)
}

func (a *invoiceStoreAdapter) CreateInvoiceWithLineItemsAudited(ctx context.Context, tenantID string, inv domain.Invoice, items []domain.InvoiceLineItem, emit func(tx *sql.Tx, out domain.Invoice) error) (domain.Invoice, error) {
	return a.store.CreateWithLineItemsAudited(ctx, tenantID, inv, items, emit)
}

func (a *invoiceStoreAdapter) CreateInvoiceWithLineItemsTx(ctx context.Context, tx *sql.Tx, tenantID string, inv domain.Invoice, items []domain.InvoiceLineItem) (domain.Invoice, error) {
	return a.store.CreateWithLineItemsTx(ctx, tx, tenantID, inv, items)
}

func (a *invoiceStoreAdapter) SetNoPMNotifiedAt(ctx context.Context, tenantID, id string, at time.Time) error {
	return a.store.SetNoPMNotifiedAt(ctx, tenantID, id, at)
}

func (a *invoiceStoreAdapter) SetAutoChargePending(ctx context.Context, tenantID, id string, pending bool) error {
	return a.store.SetAutoChargePending(ctx, tenantID, id, pending)
}

func (a *invoiceStoreAdapter) ClaimAutoCharge(ctx context.Context, tenantID, id string) (bool, error) {
	return a.store.ClaimAutoCharge(ctx, tenantID, id)
}

func (a *invoiceStoreAdapter) ReleaseAutoChargeClaim(ctx context.Context, tenantID, id string) error {
	return a.store.ReleaseAutoChargeClaim(ctx, tenantID, id)
}

func (a *invoiceStoreAdapter) ListAutoChargePending(ctx context.Context, after domain.InvoiceKeyset, limit int) ([]domain.Invoice, error) {
	return a.store.ListAutoChargePending(ctx, after, limit)
}

func (a *invoiceStoreAdapter) ListFailedWithoutDunningRun(ctx context.Context, olderThan time.Time, limit int) ([]domain.Invoice, error) {
	return a.store.ListFailedWithoutDunningRun(ctx, olderThan, limit)
}

func (a *invoiceStoreAdapter) ListAutoChargePendingForClock(ctx context.Context, tenantID, clockID string, after domain.InvoiceKeyset, limit int) ([]domain.Invoice, error) {
	return a.store.ListAutoChargePendingForClock(ctx, tenantID, clockID, after, limit)
}

func (a *invoiceStoreAdapter) SetTaxTransaction(ctx context.Context, tenantID, id string, taxTransactionID string) error {
	return a.store.SetTaxTransaction(ctx, tenantID, id, taxTransactionID)
}

func (a *invoiceStoreAdapter) ListLineItems(ctx context.Context, tenantID, invoiceID string) ([]domain.InvoiceLineItem, error) {
	return a.store.ListLineItems(ctx, tenantID, invoiceID)
}

func (a *invoiceStoreAdapter) UpdateTaxAtomic(ctx context.Context, tenantID, invoiceID string, update domain.InvoiceTaxRetryUpdate, lineItems []domain.InvoiceLineItem) (domain.Invoice, error) {
	return a.store.UpdateTaxAtomic(ctx, tenantID, invoiceID, update, lineItems)
}

func (a *invoiceStoreAdapter) FindFundingInvoicesForPeriod(ctx context.Context, tenantID, subscriptionID string, periodStart, periodEnd time.Time) ([]domain.Invoice, error) {
	return a.store.FindFundingInvoicesForPeriod(ctx, tenantID, subscriptionID, periodStart, periodEnd)
}

func (a *invoiceStoreAdapter) GetLatestThresholdInvoiceForCycle(ctx context.Context, tenantID, subscriptionID string, periodStart, periodEnd time.Time) (domain.Invoice, error) {
	return a.store.GetLatestThresholdInvoiceForCycle(ctx, tenantID, subscriptionID, periodStart, periodEnd)
}

func (a *invoiceStoreAdapter) GetLatestThresholdInvoiceForCycleTx(ctx context.Context, tx *sql.Tx, tenantID, subscriptionID string, periodStart, periodEnd time.Time) (domain.Invoice, error) {
	return a.store.GetLatestThresholdInvoiceForCycleTx(ctx, tx, tenantID, subscriptionID, periodStart, periodEnd)
}

func (a *invoiceStoreAdapter) GetInvoiceForPeriod(ctx context.Context, tenantID, subscriptionID string, periodStart, periodEnd time.Time) (domain.Invoice, error) {
	return a.store.GetInvoiceForPeriod(ctx, tenantID, subscriptionID, periodStart, periodEnd)
}

// TestFullBillingCycle_E2E tests the complete flow against real Postgres:
// tenant → customer → meter → rating rule → plan → subscription → usage → billing engine → invoice
func TestFullBillingCycle_E2E(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := leadertest.Token(t, testutil.AdminPool(t), postgres.WithLivemode(context.Background(), false), leader.RoleBilling, leader.RoleDunning)

	// Stores
	customerStore := customer.NewPostgresStore(db)
	pricingStore := pricing.NewPostgresStore(db)
	subStore := subscription.NewPostgresStore(db)
	usageStore := usage.NewPostgresStore(db)
	invoiceStore := invoice.NewPostgresStore(db)

	// 1. Create tenant
	tenantID := testutil.CreateTestTenant(t, db, "Billing Test Corp")

	// 2. Create customer
	cust, err := customerStore.Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_billing_test", DisplayName: "Test Customer",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}

	// 3. Create rating rules
	apiRule, err := pricingStore.CreateRatingRule(ctx, tenantID, domain.RatingRuleVersion{
		RuleKey: "api_calls", Name: "API Calls Pricing", Version: 1,
		LifecycleState: domain.RatingRuleActive, Mode: domain.PricingGraduated,
		Currency: "USD",
		GraduatedTiers: []domain.RatingTier{
			{UpTo: 1000, UnitAmountCents: decimal.NewFromInt(10)}, // $0.10 per call up to 1000
			{UpTo: 0, UnitAmountCents: decimal.NewFromInt(5)},     // $0.05 per call after 1000
		},
	})
	if err != nil {
		t.Fatalf("create api rule: %v", err)
	}

	storageRule, err := pricingStore.CreateRatingRule(ctx, tenantID, domain.RatingRuleVersion{
		RuleKey: "storage_gb", Name: "Storage Pricing", Version: 1,
		LifecycleState: domain.RatingRuleActive, Mode: domain.PricingFlat,
		Currency: "USD", FlatAmountCents: decimal.NewFromInt(2500), // $25 flat
	})
	if err != nil {
		t.Fatalf("create storage rule: %v", err)
	}

	// 4. Create meters
	apiMeter, err := pricingStore.CreateMeter(ctx, tenantID, domain.Meter{
		Key: "api_calls", Name: "API Calls", Unit: "calls",
		Aggregation: "sum", RatingRuleVersionID: apiRule.ID,
	})
	if err != nil {
		t.Fatalf("create api meter: %v", err)
	}

	storageMeter, err := pricingStore.CreateMeter(ctx, tenantID, domain.Meter{
		Key: "storage_gb", Name: "Storage", Unit: "GB",
		Aggregation: "sum", RatingRuleVersionID: storageRule.ID,
	})
	if err != nil {
		t.Fatalf("create storage meter: %v", err)
	}

	// 5. Create plan
	plan, err := pricingStore.CreatePlan(ctx, tenantID, domain.Plan{
		Code: "pro", Name: "Pro Plan", Currency: "USD",
		BillingInterval: domain.BillingMonthly, Status: domain.PlanActive,
		BaseAmountCents: 4900, // $49 base
		MeterIDs:        []string{apiMeter.ID, storageMeter.ID},
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}

	// 6. Create subscription with billing period set
	periodStart := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	nextBilling := periodEnd

	sub, err := subStore.Create(ctx, tenantID, domain.Subscription{
		Code: "sub-e2e-001", DisplayName: "Pro Monthly",
		CustomerID: cust.ID,
		Items:      []domain.SubscriptionItem{{PlanID: plan.ID, Quantity: 1}},
		Status:     domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar,
		StartedAt:                 &periodStart,
		CurrentBillingPeriodStart: &periodStart, CurrentBillingPeriodEnd: bivPE(periodStart),
	})
	if err != nil {
		t.Fatalf("create subscription: %v", err)
	}

	// Set billing period
	subscriptiontest.SetBillingCycle(t, ctx, db, tenantID, sub.ID, periodStart, periodEnd, nextBilling, 0)

	// 7. Ingest usage events
	for i := 0; i < 5; i++ {
		ts := periodStart.Add(time.Duration(i) * 24 * time.Hour)
		_, err := usageStore.Ingest(ctx, tenantID, domain.UsageEvent{
			CustomerID: cust.ID, MeterID: apiMeter.ID,
			Quantity: decimal.NewFromInt(300), Timestamp: ts,
		})
		if err != nil {
			t.Fatalf("ingest api usage %d: %v", i, err)
		}
	}
	// Total API calls: 5 * 300 = 1500

	_, err = usageStore.Ingest(ctx, tenantID, domain.UsageEvent{
		CustomerID: cust.ID, MeterID: storageMeter.ID,
		Quantity: decimal.NewFromInt(50), Timestamp: periodStart.Add(48 * time.Hour),
	})
	if err != nil {
		t.Fatalf("ingest storage usage: %v", err)
	}
	// Total storage: 50 GB

	// 8. Run billing engine with a real settings store (exercises the
	// UPSERT path in NextInvoiceNumber for a tenant with no prior settings row).
	// Pin the engine clock just past period_end so only one period
	// is due — the post-ADR-028 per-sub period loop would otherwise
	// catch up multiple cycles using wall-clock time and the test's
	// 2026-03/04 fixture period.
	fakeClk := clock.NewFake(periodEnd.Add(time.Nanosecond))
	engine := newRealEngine(db, fakeClk)

	count, errs := engine.RunCycle(ctx, 50, nil)
	if len(errs) > 0 {
		t.Fatalf("billing cycle errors: %v", errs)
	}
	if count != 1 {
		t.Fatalf("expected 1 invoice, got %d", count)
	}

	// 9. Verify invoice
	invoices, total, err := invoiceStore.List(ctx, invoice.ListFilter{TenantID: tenantID})
	if err != nil {
		t.Fatalf("list invoices: %v", err)
	}
	if total != 1 {
		t.Fatalf("expected 1 invoice, got %d", total)
	}

	inv := invoices[0]
	if inv.CustomerID != cust.ID {
		t.Errorf("customer_id: got %q, want %q", inv.CustomerID, cust.ID)
	}
	if inv.SubscriptionID != sub.ID {
		t.Errorf("subscription_id: got %q, want %q", inv.SubscriptionID, sub.ID)
	}
	if inv.Status != domain.InvoiceFinalized {
		t.Errorf("status: got %q, want finalized", inv.Status)
	}

	// Expected totals:
	// Base fee: $49.00 = 4900 cents
	// API: graduated — 1000 × $0.10 + 500 × $0.05 = 12500 cents = $125.00
	// Storage: flat $25/unit × 50 GB = 125000 cents = $1250.00
	// Total: 4900 + 12500 + 125000 = 142400 cents = $1424.00
	expectedTotal := int64(4900 + 12500 + 125000)
	if inv.TotalAmountCents != expectedTotal {
		t.Errorf("total: got %d cents ($%.2f), want %d cents ($%.2f)",
			inv.TotalAmountCents, float64(inv.TotalAmountCents)/100,
			expectedTotal, float64(expectedTotal)/100)
	}

	// 10. Verify line items
	items, err := invoiceStore.ListLineItems(ctx, tenantID, inv.ID)
	if err != nil {
		t.Fatalf("list line items: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("expected 3 line items (base + 2 usage), got %d", len(items))
	}

	for _, item := range items {
		switch item.LineType {
		case domain.LineTypeBaseFee:
			if item.AmountCents != 4900 {
				t.Errorf("base fee: got %d, want 4900", item.AmountCents)
			}
		case domain.LineTypeUsage:
			if item.MeterID == apiMeter.ID {
				if item.Quantity != 1500 {
					t.Errorf("API quantity: got %d, want 1500", item.Quantity)
				}
				if item.AmountCents != 12500 {
					t.Errorf("API amount: got %d, want 12500", item.AmountCents)
				}
			}
			if item.MeterID == storageMeter.ID {
				if item.AmountCents != 125000 {
					t.Errorf("storage amount: got %d, want 125000", item.AmountCents)
				}
			}
		}
	}

	// 11. Verify billing cycle advanced
	updatedSub, err := subStore.Get(ctx, tenantID, sub.ID)
	if err != nil {
		t.Fatalf("get updated sub: %v", err)
	}
	if updatedSub.CurrentBillingPeriodStart == nil || updatedSub.CurrentBillingPeriodStart.Month() != time.April {
		t.Errorf("next period should start April, got %v", updatedSub.CurrentBillingPeriodStart)
	}
	if updatedSub.CurrentBillingPeriodEnd == nil || updatedSub.CurrentBillingPeriodEnd.Month() != time.May {
		t.Errorf("next period should end May, got %v", updatedSub.CurrentBillingPeriodEnd)
	}

	t.Logf("E2E billing cycle passed: Invoice %s, Total: $%.2f (%d line items)",
		inv.InvoiceNumber, float64(inv.TotalAmountCents)/100, len(items))
}

// TestBillTiming_InAdvance_E2E exercises ADR-031 slices 2, 3, and 4:
//   - slice 2: BillOnCreate emits a subscription_create invoice for an
//     in_advance plan, covering the upcoming period's base only.
//   - slice 4: the cycle-close invoice for an in_advance plan bills
//     the NEXT period's base (line.billing_period = next period),
//     not the just-elapsed one — closes the double-bill window.
//   - slice 3: BillOnCancel issues a credit grant for the unused
//     portion when a sub is canceled mid-period on an in_advance plan.
//
// Single fixture, three asserts. Existing TestFullBillingCycle_E2E
// remains the in_arrears regression — this test is the in_advance
// counterpart.
func TestBillTiming_InAdvance_E2E(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := leadertest.Token(t, testutil.AdminPool(t), postgres.WithLivemode(context.Background(), false), leader.RoleBilling, leader.RoleDunning)

	customerStore := customer.NewPostgresStore(db)
	pricingStore := pricing.NewPostgresStore(db)
	subStore := subscription.NewPostgresStore(db)
	invoiceStore := invoice.NewPostgresStore(db)
	creditStore := credit.NewPostgresStore(db)
	creditSvc := credit.NewService(creditStore)

	tenantID := testutil.CreateTestTenant(t, db, "In-Advance Test Corp")

	cust, err := customerStore.Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_advance_test", DisplayName: "Advance Customer",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}

	// in_advance plan, no meters — keeps the assert surface small.
	plan, err := pricingStore.CreatePlan(ctx, tenantID, domain.Plan{
		Code: "pro-advance", Name: "Pro Advance",
		Currency: "USD", BillingInterval: domain.BillingMonthly,
		Status: domain.PlanActive, BaseAmountCents: 4900,
		BaseBillTiming: domain.BillInAdvance,
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}

	// Sub covering exactly one full month (no proration noise).
	periodStart := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	sub, err := subStore.Create(ctx, tenantID, domain.Subscription{
		Code: "sub-advance-001", DisplayName: "Pro Advance Monthly",
		CustomerID: cust.ID,
		Items:      []domain.SubscriptionItem{{PlanID: plan.ID, Quantity: 1}},
		Status:     domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar,
		StartedAt:                 &periodStart,
		CurrentBillingPeriodStart: &periodStart, CurrentBillingPeriodEnd: bivPE(periodStart),
	})
	if err != nil {
		t.Fatalf("create subscription: %v", err)
	}
	subscriptiontest.SetBillingCycle(t, ctx, db, tenantID, sub.ID, periodStart, periodEnd, periodEnd, 0)
	sub.CurrentBillingPeriodStart = &periodStart
	sub.CurrentBillingPeriodEnd = &periodEnd

	settingsStore := tenant.NewSettingsStore(db)
	fakeClk := clock.NewFake(periodStart.Add(time.Hour))
	engine := newRealEngine(db, fakeClk)
	engine.SetCreditGranter(creditSvc)
	// Issuers are REQUIRED post-#442: the paid-source cancel credit routes
	// through the real creditnote.Service (Issue grants the balance via the
	// cnGrantAdapter), matching production wiring.
	invoiceSvc := invoice.NewService(invoiceStore, clock.Real(), settingsStore)
	creditNoteSvc := creditnote.NewService(creditnote.NewPostgresStore(db), invoiceStore, nil, &cnGrantAdapter{creditSvc})
	creditNoteSvc.SetNumberGenerator(settingsStore)
	engine.SetInvoiceVoider(invoiceSvc)
	engine.SetCreditNoteAdjuster(creditNoteSvc)
	engine.SetCreditHeadroomReader(creditNoteSvc)

	// --- Slice 2: BillOnCreate emits day-1 invoice ---
	dayOneInv, err := engine.BillOnCreate(ctx, sub)
	if err != nil {
		t.Fatalf("BillOnCreate: %v", err)
	}
	if dayOneInv.ID == "" {
		t.Fatal("BillOnCreate returned empty invoice for in_advance plan")
	}
	if dayOneInv.BillingReason != domain.BillingReasonSubscriptionCreate {
		t.Errorf("day-1 billing_reason: got %q, want subscription_create", dayOneInv.BillingReason)
	}
	if dayOneInv.TotalAmountCents != 4900 {
		t.Errorf("day-1 total: got %d, want 4900", dayOneInv.TotalAmountCents)
	}
	dayOneItems, err := invoiceStore.ListLineItems(ctx, tenantID, dayOneInv.ID)
	if err != nil {
		t.Fatalf("list day-1 line items: %v", err)
	}
	if len(dayOneItems) != 1 {
		t.Fatalf("day-1 line items: got %d, want 1 (base only)", len(dayOneItems))
	}
	if dayOneItems[0].LineType != domain.LineTypeBaseFee {
		t.Errorf("day-1 line type: got %q, want base_fee", dayOneItems[0].LineType)
	}
	// Day-1 line period == current period (sub create covers period 1).
	if dayOneItems[0].BillingPeriodStart == nil || !dayOneItems[0].BillingPeriodStart.Equal(periodStart) {
		t.Errorf("day-1 line period_start: got %v, want %v", dayOneItems[0].BillingPeriodStart, periodStart)
	}

	// Idempotency: re-calling BillOnCreate returns zero invoice (unique
	// constraint catches the replay).
	replay, err := engine.BillOnCreate(ctx, sub)
	if err != nil {
		t.Fatalf("BillOnCreate replay: %v", err)
	}
	if replay.ID != "" {
		t.Errorf("BillOnCreate replay produced an invoice: %s (idempotent skip expected)", replay.ID)
	}

	// --- Slice 4: cycle-close invoice's base covers NEXT period ---
	// Advance the engine's clock just past period_end so the cycle
	// path picks this sub up. The base line should describe period 2.
	fakeClk.Set(periodEnd.Add(time.Nanosecond))
	// RunCycle scans across all tenants in the shared test DB; we
	// can't assert on the global count. Filter by tenantID below.
	_, runErrs := engine.RunCycle(ctx, 50, nil)
	if len(runErrs) > 0 {
		t.Fatalf("RunCycle errors: %v", runErrs)
	}

	// Find the cycle invoice (not the day-1 invoice).
	invs, _, err := invoiceStore.List(ctx, invoice.ListFilter{TenantID: tenantID})
	if err != nil {
		t.Fatalf("list invoices: %v", err)
	}
	var cycleInv domain.Invoice
	for _, i := range invs {
		if i.BillingReason == domain.BillingReasonSubscriptionCycle {
			cycleInv = i
			break
		}
	}
	if cycleInv.ID == "" {
		t.Fatal("no subscription_cycle invoice produced")
	}
	cycleItems, err := invoiceStore.ListLineItems(ctx, tenantID, cycleInv.ID)
	if err != nil {
		t.Fatalf("list cycle line items: %v", err)
	}
	// Find the base line; assert its period is JULY (period 2), not JUNE (period 1).
	expectedNextStart := periodEnd                                 // 2026-07-01
	expectedNextEnd := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC) // 2026-08-01
	foundBase := false
	for _, li := range cycleItems {
		if li.LineType != domain.LineTypeBaseFee {
			continue
		}
		foundBase = true
		if li.BillingPeriodStart == nil || !li.BillingPeriodStart.Equal(expectedNextStart) {
			t.Errorf("cycle base line period_start: got %v, want %v (next period)", li.BillingPeriodStart, expectedNextStart)
		}
		if li.BillingPeriodEnd == nil || !li.BillingPeriodEnd.Equal(expectedNextEnd) {
			t.Errorf("cycle base line period_end: got %v, want %v (next period)", li.BillingPeriodEnd, expectedNextEnd)
		}
		if li.AmountCents != 4900 {
			t.Errorf("cycle base amount: got %d, want 4900 (full upcoming period, no proration)", li.AmountCents)
		}
	}
	if !foundBase {
		t.Error("cycle invoice had no base_fee line")
	}

	// --- Slice 3: BillOnCancel issues a credit grant ---
	// Reload sub (UpdateBillingCycle moved it to July), then cancel
	// halfway through July and assert the credit grant amount.
	//
	// Mark the July in-advance invoice PAID first. BillOnCancel only issues a
	// credit *balance* for unused time the customer actually paid for — when
	// the source invoice is unpaid it (correctly) skips the grant and the
	// open invoice is relieved another way. This matches the industry split
	// (Stripe / Orb / Lago / Chargebee): paid → credit balance, unpaid →
	// reduce/void the invoice, never a credit. Without this step the test was
	// asserting a grant the engine rightly refuses.
	if _, err := invoiceStore.MarkPaid(ctx, tenantID, cycleInv.ID, "pi_test_cancel", periodEnd); err != nil {
		t.Fatalf("mark July invoice paid: %v", err)
	}

	subRefreshed, err := subStore.Get(ctx, tenantID, sub.ID)
	if err != nil {
		t.Fatalf("reload sub: %v", err)
	}
	cancelAt := time.Date(2026, 7, 16, 0, 0, 0, 0, time.UTC) // ≈ half of July
	subRefreshed.Status = domain.SubscriptionCanceled
	subRefreshed.CanceledAt = &cancelAt

	// The cancel relief as Cancel runs it: written on a real tx, committed,
	// then its post-commit legs.
	reliefTx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin relief tx: %v", err)
	}
	relief, err := engine.BillOnCancelDraftsTx(ctx, reliefTx, subRefreshed)
	if err != nil {
		_ = reliefTx.Rollback()
		t.Fatalf("BillOnCancelDraftsTx: %v", err)
	}
	if err := reliefTx.Commit(); err != nil {
		t.Fatalf("commit relief: %v", err)
	}
	engine.FinishRelief(ctx, subRefreshed, relief)

	// Read the customer's credit ledger; should have a grant entry for
	// the unused portion. July has 31 days; unused = 15 days =
	// 4900 * 15/31 ≈ 2370 cents.
	entries, err := creditStore.ListEntries(ctx, credit.ListFilter{TenantID: tenantID, CustomerID: cust.ID, Limit: 50})
	if err != nil {
		t.Fatalf("list credit entries: %v", err)
	}
	var grant domain.CreditLedgerEntry
	for _, e := range entries {
		if e.EntryType == domain.CreditGrant {
			grant = e
			break
		}
	}
	if grant.ID == "" {
		t.Fatal("BillOnCancel produced no credit grant entry")
	}
	// July 16 cancel of a July 1 - August 1 period: 16 unused days out of
	// 31. RoundHalfToEven(4900×16, 31) = RoundHalfToEven(78400, 31) = 2529
	// (78400 = 31×2529 + 1; 2×1 = 2 < 31 → round down). Exact, no tolerance.
	if grant.AmountCents != 2529 {
		t.Errorf("cancel proration credit: got %d cents, want 2529 (4900 × 16/31, banker's)", grant.AmountCents)
	}

	t.Logf("in_advance E2E passed — day-1 inv: $%.2f, cycle base period: %v, cancel credit: $%.2f",
		float64(dayOneInv.TotalAmountCents)/100,
		expectedNextStart.Format("2006-01-02"),
		float64(grant.AmountCents)/100)
}

// TestBillOnCancel_UnpaidPrebillRelief_E2E exercises the fully-wired #22 path
// against postgres: an UNPAID in_advance prebill is settled at mid-period
// cancel by the real invoice.Service (void) and creditnote.Service (reduce
// amount_due), not left full-amount in dunning and not turned into an unfunded
// credit. Two scenarios, each on its own subscription + day-1 invoice:
//   - partial consumption → adjustment credit note reduces amount_due to the
//     consumed portion
//   - no consumption (cancel at period start) → invoice voided
//
// reliefLedgerGranter bridges credit-note Issue() to the real credit ledger, as
// api.creditGrantAdapter does in production (that adapter is unexported).
type reliefLedgerGranter struct{ svc *credit.Service }

func (g reliefLedgerGranter) in(i creditnote.CreditGrantInput) credit.GrantInput {
	return credit.GrantInput{CustomerID: i.CustomerID, AmountCents: i.AmountCents, Description: i.Description, InvoiceID: i.InvoiceID}
}
func (g reliefLedgerGranter) Grant(ctx context.Context, tenantID string, i creditnote.CreditGrantInput) error {
	_, err := g.svc.Grant(ctx, tenantID, g.in(i))
	return err
}
func (g reliefLedgerGranter) GrantForCreditNote(ctx context.Context, tenantID, cnID string, i creditnote.CreditGrantInput) error {
	_, err := g.svc.GrantForCreditNote(ctx, tenantID, cnID, g.in(i))
	return err
}
func (g reliefLedgerGranter) GrantForCreditNoteTx(ctx context.Context, tx *sql.Tx, tenantID, cnID string, i creditnote.CreditGrantInput) error {
	_, err := g.svc.GrantForCreditNoteTx(ctx, tx, tenantID, cnID, g.in(i))
	return err
}
func (g reliefLedgerGranter) LockCommitGrantForReliefTx(ctx context.Context, tx *sql.Tx, tenantID, invoiceID string) (int64, int64, int64, bool, error) {
	return g.svc.LockCommitGrantForReliefTx(ctx, tx, tenantID, invoiceID)
}
func (g reliefLedgerGranter) RetireCommitSliceForReliefTx(context.Context, *sql.Tx, string, string, string, int64, int64, int64) (int64, error) {
	panic("commit relief is not exercised by this test")
}

func TestBillOnCancel_UnpaidPrebillRelief_E2E(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)

	customerStore := customer.NewPostgresStore(db)
	pricingStore := pricing.NewPostgresStore(db)
	subStore := subscription.NewPostgresStore(db)
	invoiceStore := invoice.NewPostgresStore(db)
	creditStore := credit.NewPostgresStore(db)
	creditSvc := credit.NewService(creditStore)
	creditNoteStore := creditnote.NewPostgresStore(db)
	settingsStore := tenant.NewSettingsStore(db)

	tenantID := testutil.CreateTestTenant(t, db, "Unpaid Relief Corp")

	plan, err := pricingStore.CreatePlan(ctx, tenantID, domain.Plan{
		Code: "pro-advance", Name: "Pro Advance",
		Currency: "USD", BillingInterval: domain.BillingMonthly,
		Status: domain.PlanActive, BaseAmountCents: 4900,
		BaseBillTiming: domain.BillInAdvance,
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}

	periodStart := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC) // 30-day cycle

	// real services the engine settles the unpaid invoice through.
	invoiceSvc := invoice.NewService(invoiceStore, clock.Real(), settingsStore)
	creditNoteSvc := creditnote.NewService(creditNoteStore, invoiceStore, nil)
	creditNoteSvc.SetNumberGenerator(settingsStore)

	newEngine := func() *billing.Engine {
		e := newRealEngine(db, clock.NewFake(periodStart.Add(time.Hour)))
		e.SetCreditGranter(creditSvc)
		e.SetInvoiceVoider(invoiceSvc)
		e.SetCreditNoteAdjuster(creditNoteSvc)
		e.SetCreditHeadroomReader(creditNoteSvc)
		return e
	}

	// freshUnpaidSub creates a customer + in_advance sub, emits the (unpaid,
	// finalized) day-1 invoice, and returns the sub and that invoice's id.
	freshUnpaidSub := func(extID string) (domain.Subscription, string) {
		cust, err := customerStore.Create(ctx, tenantID, domain.Customer{ExternalID: extID, DisplayName: extID})
		if err != nil {
			t.Fatalf("create customer: %v", err)
		}
		sub, err := subStore.Create(ctx, tenantID, domain.Subscription{
			Code: "sub-" + extID, DisplayName: extID, CustomerID: cust.ID,
			Items:  []domain.SubscriptionItem{{PlanID: plan.ID, Quantity: 1}},
			Status: domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar,
			StartedAt:                 &periodStart,
			CurrentBillingPeriodStart: &periodStart, CurrentBillingPeriodEnd: bivPE(periodStart),
		})
		if err != nil {
			t.Fatalf("create sub: %v", err)
		}
		subscriptiontest.SetBillingCycle(t, ctx, db, tenantID, sub.ID, periodStart, periodEnd, periodEnd, 0)
		sub.CurrentBillingPeriodStart = &periodStart
		sub.CurrentBillingPeriodEnd = &periodEnd
		inv, err := newEngine().BillOnCreate(ctx, sub)
		if err != nil {
			t.Fatalf("BillOnCreate: %v", err)
		}
		if inv.Status != domain.InvoiceFinalized || inv.PaymentStatus == domain.PaymentSucceeded {
			t.Fatalf("day-1 invoice should be finalized + unpaid, got status=%s payment=%s", inv.Status, inv.PaymentStatus)
		}
		return sub, inv.ID
	}

	countGrants := func(customerID string) int {
		entries, err := creditStore.ListEntries(ctx, credit.ListFilter{TenantID: tenantID, CustomerID: customerID, Limit: 50})
		if err != nil {
			t.Fatalf("list credit entries: %v", err)
		}
		n := 0
		for _, e := range entries {
			if e.EntryType == domain.CreditGrant {
				n++
			}
		}
		return n
	}

	t.Run("partial consumption reduces amount_due to consumed portion", func(t *testing.T) {
		sub, invID := freshUnpaidSub("cus_reduce")
		cancelAt := time.Date(2026, 6, 16, 0, 0, 0, 0, time.UTC) // 15 unused / 30 days
		sub.Status = domain.SubscriptionCanceled
		sub.CanceledAt = &cancelAt

		cancelRelief(t, ctx, db, tenantID, newEngine(), sub)

		got, err := invoiceStore.Get(ctx, tenantID, invID)
		if err != nil {
			t.Fatalf("reload invoice: %v", err)
		}
		// unused = round(4900 * 15/30) = 2450; amount_due 4900 → 2450.
		if got.Status == domain.InvoiceVoided {
			t.Fatalf("partial consumption must reduce, not void")
		}
		if got.AmountDueCents != 2450 {
			t.Errorf("amount_due after reduce: got %d, want 2450 (consumed half)", got.AmountDueCents)
		}
		if n := countGrants(sub.CustomerID); n != 0 {
			t.Errorf("credit grants = %d, want 0 (unpaid → no balance credit)", n)
		}
	})

	t.Run("no consumption voids the unpaid invoice", func(t *testing.T) {
		sub, invID := freshUnpaidSub("cus_void")
		cancelAt := periodStart.Add(time.Hour) // whole period unused
		sub.Status = domain.SubscriptionCanceled
		sub.CanceledAt = &cancelAt

		cancelRelief(t, ctx, db, tenantID, newEngine(), sub)

		got, err := invoiceStore.Get(ctx, tenantID, invID)
		if err != nil {
			t.Fatalf("reload invoice: %v", err)
		}
		if got.Status != domain.InvoiceVoided {
			t.Errorf("invoice status: got %s, want voided (nothing consumed)", got.Status)
		}
		if n := countGrants(sub.CustomerID); n != 0 {
			t.Errorf("credit grants = %d, want 0", n)
		}
	})

	// Customer credits were applied to the unpaid invoice BEFORE the cancel.
	// The relief is still the unused half: it clears what is due and the rest
	// of it hands applied credits back. Sized against the amount due alone
	// (before 2026-10-12) the relief stopped at 1900 and 550 of the customer's
	// credits stayed spent on time they never used.
	t.Run("credits applied before the cancel: the unused share of them comes back", func(t *testing.T) {
		sub, invID := freshUnpaidSub("cus_credits_first")
		if _, err := creditSvc.Grant(ctx, tenantID, credit.GrantInput{CustomerID: sub.CustomerID, AmountCents: 3000, Description: "goodwill"}); err != nil {
			t.Fatalf("grant: %v", err)
		}
		if applied, err := creditSvc.ApplyToInvoice(ctx, tenantID, sub.CustomerID, invID, 4900); err != nil || applied != 3000 {
			t.Fatalf("apply credits = %d, %v; want 3000", applied, err)
		}
		cancelAt := time.Date(2026, 6, 16, 0, 0, 0, 0, time.UTC) // 15 unused / 30 days
		sub.Status = domain.SubscriptionCanceled
		sub.CanceledAt = &cancelAt

		withLedger := creditnote.NewService(creditNoteStore, invoiceStore, nil, reliefLedgerGranter{creditSvc})
		withLedger.SetNumberGenerator(settingsStore)
		e := newEngine()
		e.SetCreditNoteAdjuster(withLedger)
		e.SetCreditHeadroomReader(withLedger)
		cancelRelief(t, ctx, db, tenantID, e, sub)

		got, err := invoiceStore.Get(ctx, tenantID, invID)
		if err != nil {
			t.Fatalf("reload invoice: %v", err)
		}
		// Used half = 2450, all of it covered by credits. Unused half = 2450:
		// 1900 was still due, 550 was covered by credits and goes back.
		if got.AmountDueCents != 0 || got.CreditsAppliedCents != 2450 {
			t.Errorf("invoice due %d / credits applied %d, want 0 / 2450", got.AmountDueCents, got.CreditsAppliedCents)
		}
		bal, err := creditSvc.GetBalance(ctx, tenantID, sub.CustomerID)
		if err != nil {
			t.Fatalf("balance: %v", err)
		}
		if bal.BalanceCents != 550 {
			t.Errorf("customer balance = %d, want 550 (the credits spent on unused time)", bal.BalanceCents)
		}
	})

	// ADR-118: the relief is written in the cancel tx, so a crash after the
	// commit and before its post-commit legs loses nothing — the clawback
	// reconciler issues the committed draft on its own.
	t.Run("crash before FinishRelief: the reconciler issues the relief", func(t *testing.T) {
		sub, invID := freshUnpaidSub("cus_crash")
		cancelAt := time.Date(2026, 6, 16, 0, 0, 0, 0, time.UTC)
		sub.Status = domain.SubscriptionCanceled
		sub.CanceledAt = &cancelAt

		tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		r, err := newEngine().BillOnCancelDraftsTx(ctx, tx, sub)
		if err != nil {
			_ = tx.Rollback()
			t.Fatalf("BillOnCancelDraftsTx: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
		if len(r.DraftIDs) != 1 {
			t.Fatalf("relief drafts = %v, want 1 (the unpaid reduce)", r.DraftIDs)
		}
		// The crash: FinishRelief never runs.
		if got, _ := invoiceStore.Get(ctx, tenantID, invID); got.AmountDueCents != 4900 {
			t.Fatalf("amount_due = %d before the draft is issued, want 4900", got.AmountDueCents)
		}
		if _, errs := creditNoteSvc.RetryPendingClawbackIssue(ctx, 50); len(errs) != 0 {
			t.Fatalf("RetryPendingClawbackIssue: %v", errs)
		}
		got, err := invoiceStore.Get(ctx, tenantID, invID)
		if err != nil {
			t.Fatalf("reload: %v", err)
		}
		if got.AmountDueCents != 2450 {
			t.Errorf("amount_due after the reconciler = %d, want 2450 (relief recovered)", got.AmountDueCents)
		}
	})

	// The engine decides to void from a snapshot read before the cancel tx; if
	// the invoice changed since (a payment started, an amount moved), the
	// void refuses under the row lock and the cancel rolls back — the retry
	// decides again from fresh data.
	t.Run("a void refuses when the invoice changed since it was read", func(t *testing.T) {
		_, invID := freshUnpaidSub("cus_stale")
		snap, err := invoiceStore.Get(ctx, tenantID, invID)
		if err != nil {
			t.Fatalf("snapshot: %v", err)
		}
		// A partial payment lands after the snapshot. Void's own state checks
		// allow it (finalized, nothing in flight) — only the snapshot
		// comparison stops a void that would annul collected money.
		btx, err := db.BeginTx(ctx, postgres.TxBypass, "")
		if err != nil {
			t.Fatalf("begin bypass: %v", err)
		}
		res, err := btx.ExecContext(ctx, `UPDATE invoices SET amount_paid_cents = 100, amount_due_cents = amount_due_cents - 100 WHERE id = $1`, invID)
		if err != nil {
			_ = btx.Rollback()
			t.Fatalf("partial payment: %v", err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			_ = btx.Rollback()
			t.Fatalf("partial payment updated %d rows, want 1", n)
		}
		if err := btx.Commit(); err != nil {
			t.Fatalf("commit partial payment: %v", err)
		}
		tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		_, verr := invoiceSvc.VoidUnchangedTx(ctx, tx, tenantID, snap)
		_ = tx.Rollback()
		if verr == nil {
			t.Fatal("VoidUnchangedTx voided an invoice that was partly paid after the snapshot")
		}
		got, err := invoiceStore.Get(ctx, tenantID, invID)
		if err != nil {
			t.Fatalf("reload: %v", err)
		}
		if got.Status == domain.InvoiceVoided {
			t.Error("invoice voided despite the refusal")
		}
	})
}

func (a *subStoreAdapter) ActivateAfterTrialWithBill(ctx context.Context, tenantID, id string, at time.Time, billFn func(tx *sql.Tx, activated domain.Subscription) error) (domain.Subscription, error) {
	return a.store.ActivateAfterTrialWithBill(ctx, tenantID, id, at, billFn)
}

func (a *subStoreAdapter) CancelAtTrialEnd(ctx context.Context, tenantID, id string, observedTrialEnd time.Time) (domain.Subscription, error) {
	return a.store.CancelAtTrialEnd(ctx, tenantID, id, observedTrialEnd)
}

// cancelRelief runs the cancel relief the way Cancel does: written on a real
// tenant tx, committed, then its post-commit legs.
func cancelRelief(t *testing.T, ctx context.Context, db *postgres.DB, tenantID string, e *billing.Engine, sub domain.Subscription) domain.Relief {
	t.Helper()
	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin relief tx: %v", err)
	}
	r, err := e.BillOnCancelDraftsTx(ctx, tx, sub)
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("BillOnCancelDraftsTx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit relief: %v", err)
	}
	e.FinishRelief(ctx, sub, r)
	return r
}
