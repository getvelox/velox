package billing_test

import (
	"context"
	"testing"

	"github.com/sagarsuperuser/velox/internal/billing"
	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/subscription"
	"github.com/sagarsuperuser/velox/internal/tax"
	"github.com/sagarsuperuser/velox/internal/tenant"
	"github.com/sagarsuperuser/velox/internal/testutil"
	"github.com/sagarsuperuser/velox/internal/usage"
)

// TestManualInvoice_TaxComputedAtFinalize is the e2e for the operator
// composer flow: create a manual (no-subscription) invoice with line items
// atomically, then finalize. The headline assertion is that tax is computed
// AT FINALIZE — the old behaviour left manual invoices at tax=0 because tax
// was only ever computed during cycle billing, never for operator-composed
// drafts.
//
// The line amounts ($33.33 + $33.33 + $33.34 = $100.00 at 7.25%) are chosen
// to exercise the manual provider's residual-on-last-line allocation: the
// sum of the independently-rounded per-line taxes must reconcile EXACTLY to
// the invoice-level tax, with no penny lost or invented. That invariant is
// the whole reason the manual provider rounds at the invoice level and
// absorbs the residual on the last line.
func TestManualInvoice_TaxComputedAtFinalize(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test needs postgres")
	}
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)

	customerStore := customer.NewPostgresStore(db)
	pricingStore := pricing.NewPostgresStore(db)
	subStore := subscription.NewPostgresStore(db)
	invoiceStore := invoice.NewPostgresStore(db)
	usageStore := usage.NewPostgresStore(db)
	settingsStore := tenant.NewSettingsStore(db)

	tenantID := testutil.CreateTestTenant(t, db, "Manual Invoice Tax Corp")

	// Tenant tax = manual flat 7.25%. Get→mutate→Upsert preserves the other
	// NOT NULL settings columns (currency, timezone, prefix, ...).
	ts, err := settingsStore.Get(ctx, tenantID)
	if err != nil {
		t.Fatalf("get settings: %v", err)
	}
	ts.TaxProvider = "manual"
	ts.TaxRate = 7.25
	ts.TaxName = "Sales Tax"
	if _, err := settingsStore.Upsert(ctx, ts); err != nil {
		t.Fatalf("upsert settings: %v", err)
	}

	cust, err := customerStore.Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_manual_tax", DisplayName: "Manual Tax Customer",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}

	// The engine is the TaxRetrier the invoice service calls at finalize. No
	// billing-profile dep wired — the manual provider is flat-rate and needs
	// no jurisdiction lookup.
	engine := billing.NewEngine(
		&subStoreAdapter{subStore},
		&usageStoreAdapter{usageStore},
		&pricingStoreAdapter{pricingStore},
		&invoiceStoreAdapter{invoiceStore},
		nil, settingsStore, testPaymentSetupsNoPM{}, testChargerSentinel{}, clock.Real(),
	)
	engine.SetTaxProviderResolver(tax.NewResolver(nil))
	engine.SetNoPaymentMethodNotifier(&testNoPMNotifier{})
	engine.SetDunningStarter(testDunningStarter{})

	invoiceSvc := invoice.NewService(invoiceStore, clock.Real(), settingsStore)
	invoiceSvc.SetTaxRetrier(engine)

	// Atomic create-with-lines: $33.33 + $33.33 + $33.34 = $100.00.
	inv, err := invoiceSvc.Create(ctx, tenantID, invoice.CreateInput{
		CustomerID: cust.ID,
		Currency:   "USD",
		LineItems: []invoice.AddLineItemInput{
			{Description: "Consulting — part 1", Quantity: 1, UnitAmountCents: 3333},
			{Description: "Consulting — part 2", Quantity: 1, UnitAmountCents: 3333},
			{Description: "Consulting — part 3", Quantity: 1, UnitAmountCents: 3334},
		},
	})
	if err != nil {
		t.Fatalf("create manual invoice: %v", err)
	}
	if inv.BillingReason != domain.BillingReasonManual {
		t.Errorf("billing_reason: got %q, want manual", inv.BillingReason)
	}
	if inv.SubtotalCents != 10000 {
		t.Fatalf("subtotal after atomic create: got %d¢, want 10000¢", inv.SubtotalCents)
	}
	// Draft has no tax yet — tax is a finalize-time concern.
	if inv.TaxAmountCents != 0 {
		t.Errorf("draft tax before finalize: got %d¢, want 0¢", inv.TaxAmountCents)
	}

	// Finalize → tax computed via the manual provider.
	finalized, err := invoiceSvc.Finalize(ctx, tenantID, inv.ID)
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if finalized.Status != domain.InvoiceFinalized {
		t.Fatalf("status after finalize: got %s, want finalized", finalized.Status)
	}
	if finalized.TaxStatus != domain.InvoiceTaxOK {
		t.Fatalf("tax_status after finalize: got %s, want ok", finalized.TaxStatus)
	}
	if finalized.TaxProvider != "manual" {
		t.Errorf("tax_provider: got %q, want manual", finalized.TaxProvider)
	}

	// 10000¢ × 7.25% = 725¢, rounded at the invoice level.
	const wantTax = 725
	if finalized.TaxAmountCents != wantTax {
		t.Errorf("invoice tax: got %d¢, want %d¢", finalized.TaxAmountCents, wantTax)
	}
	if finalized.TotalAmountCents != 10000+wantTax {
		t.Errorf("invoice total: got %d¢, want %d¢", finalized.TotalAmountCents, 10000+wantTax)
	}

	// The reconciliation invariant: independently-rounded per-line taxes must
	// sum EXACTLY to the invoice tax. With three near-equal bases this is the
	// case that breaks naive per-line rounding (242+242+242 = 726 ≠ 725); the
	// manual provider absorbs the −1¢ residual on the last line.
	items, err := invoiceStore.ListLineItems(ctx, tenantID, inv.ID)
	if err != nil {
		t.Fatalf("list line items: %v", err)
	}
	var lineTaxSum int64
	for _, li := range items {
		lineTaxSum += li.TaxAmountCents
	}
	if lineTaxSum != finalized.TaxAmountCents {
		t.Errorf("sum of per-line tax (%d¢) != invoice tax (%d¢) — residual not reconciled. Lines: %+v",
			lineTaxSum, finalized.TaxAmountCents, items)
	}
	t.Logf("manual invoice e2e: $100.00 @ 7.25%% → tax %d¢, per-line sum %d¢ (reconciled)", finalized.TaxAmountCents, lineTaxSum)
}

// TestManualInvoice_TaxInclusive_TotalEqualsGross guards the tax-inclusive
// finalize fix. In tax-inclusive mode the operator enters a GROSS amount and
// the provider carves tax OUT of it, so the invoice total must equal the
// gross the operator entered — NOT gross + tax. Pre-fix
// computeAndPersistInvoiceTax computed the total from the stored (gross)
// header subtotal and added the carved tax back on top, double-counting and
// overstating the total by ~one tax amount (here $136 instead of $118). The
// fix reads the net subtotal/discount off the tax application (mirroring the
// cycle build path) and persists them, restoring subtotal − discount + tax ==
// gross.
func TestManualInvoice_TaxInclusive_TotalEqualsGross(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test needs postgres")
	}
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)

	customerStore := customer.NewPostgresStore(db)
	pricingStore := pricing.NewPostgresStore(db)
	subStore := subscription.NewPostgresStore(db)
	invoiceStore := invoice.NewPostgresStore(db)
	usageStore := usage.NewPostgresStore(db)
	settingsStore := tenant.NewSettingsStore(db)

	tenantID := testutil.CreateTestTenant(t, db, "Inclusive Tax Corp")

	// Tenant tax = manual flat 18%, TAX-INCLUSIVE.
	ts, err := settingsStore.Get(ctx, tenantID)
	if err != nil {
		t.Fatalf("get settings: %v", err)
	}
	ts.TaxProvider = "manual"
	ts.TaxRate = 18.0
	ts.TaxName = "GST"
	ts.TaxInclusive = true
	if _, err := settingsStore.Upsert(ctx, ts); err != nil {
		t.Fatalf("upsert settings: %v", err)
	}

	cust, err := customerStore.Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_incl_tax", DisplayName: "Inclusive Tax Customer",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}

	engine := billing.NewEngine(
		&subStoreAdapter{subStore},
		&usageStoreAdapter{usageStore},
		&pricingStoreAdapter{pricingStore},
		&invoiceStoreAdapter{invoiceStore},
		nil, settingsStore, testPaymentSetupsNoPM{}, testChargerSentinel{}, clock.Real(),
	)
	engine.SetTaxProviderResolver(tax.NewResolver(nil))
	engine.SetNoPaymentMethodNotifier(&testNoPMNotifier{})
	engine.SetDunningStarter(testDunningStarter{})

	invoiceSvc := invoice.NewService(invoiceStore, clock.Real(), settingsStore)
	invoiceSvc.SetTaxRetrier(engine)

	// Operator enters a GROSS $118.00 line; at 18% inclusive that decomposes
	// to $100.00 net + $18.00 tax embedded in the price.
	inv, err := invoiceSvc.Create(ctx, tenantID, invoice.CreateInput{
		CustomerID: cust.ID,
		Currency:   "USD",
		LineItems: []invoice.AddLineItemInput{
			{Description: "All-inclusive service", Quantity: 1, UnitAmountCents: 11800},
		},
	})
	if err != nil {
		t.Fatalf("create manual invoice: %v", err)
	}

	finalized, err := invoiceSvc.Finalize(ctx, tenantID, inv.ID)
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}

	const wantGross = 11800
	const wantNet = 10000
	const wantTax = 1800
	if finalized.TaxAmountCents != wantTax {
		t.Errorf("tax: got %d¢, want %d¢ (carved out of the gross)", finalized.TaxAmountCents, wantTax)
	}
	if finalized.SubtotalCents != wantNet {
		t.Errorf("subtotal: got %d¢, want net %d¢ (carved out of the gross)", finalized.SubtotalCents, wantNet)
	}
	if finalized.TotalAmountCents != wantGross {
		t.Errorf("total: got %d¢, want %d¢ (== gross). Pre-fix this was gross+tax=%d¢ (double-count)",
			finalized.TotalAmountCents, wantGross, wantGross+wantTax)
	}
	// The customer-pays invariant the cycle path also maintains.
	if got := finalized.SubtotalCents - finalized.DiscountCents + finalized.TaxAmountCents; got != wantGross {
		t.Errorf("invariant subtotal−discount+tax: got %d¢, want %d¢", got, wantGross)
	}
	t.Logf("tax-inclusive manual invoice: gross $118.00 @ 18%% → net %d¢ + tax %d¢ = total %d¢",
		finalized.SubtotalCents, finalized.TaxAmountCents, finalized.TotalAmountCents)
}
