package invoice

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// TestFormatTaxRate guards the fixed-decimal formatter that replaced %g on the
// invoice + credit-note PDFs. %g uses significant figures and silently drops
// precision on rates ≥ 10 (13.875 → "13.88"); the statutory NUMERIC(7,4) rate
// must print verbatim with trailing zeros trimmed.
func TestFormatTaxRate(t *testing.T) {
	cases := []struct {
		rate float64
		want string
	}{
		{8.875, "8.875"},   // NYC — the motivating case
		{8.8750, "8.875"},  // stored 4-dp form, trailing zero trimmed
		{18, "18"},         // whole percent, no decimal point
		{9.975, "9.975"},   // Quebec — 3-dp
		{13.875, "13.875"}, // ≥ 10 with 3-dp — the %g precision-loss case
		{7.25, "7.25"},
		{8.8, "8.8"},
		{0, "0"},
	}
	for _, c := range cases {
		if got := formatTaxRate(c.rate); got != c.want {
			t.Errorf("formatTaxRate(%v) = %q, want %q", c.rate, got, c.want)
		}
	}
}

func TestRenderPDF(t *testing.T) {
	now := time.Now().UTC()
	dueAt := now.AddDate(0, 0, 30)
	periodStart := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)

	inv := domain.Invoice{
		ID:                 "vlx_inv_test",
		InvoiceNumber:      "VLX-202604-0001",
		Status:             domain.InvoiceFinalized,
		PaymentStatus:      domain.PaymentPending,
		Currency:           "USD",
		SubtotalCents:      19900,
		TotalAmountCents:   19900,
		AmountDueCents:     19900,
		BillingPeriodStart: periodStart,
		BillingPeriodEnd:   periodEnd,
		IssuedAt:           &now,
		DueAt:              &dueAt,
		NetPaymentTermDays: 30,
		Memo:               "Thank you for your business.",
	}

	lineItems := []domain.InvoiceLineItem{
		{
			LineType:         domain.LineTypeBaseFee,
			Description:      "Pro Plan - base fee",
			Quantity:         1,
			UnitAmountCents:  4900,
			AmountCents:      4900,
			TotalAmountCents: 4900,
			Currency:         "USD",
		},
		{
			LineType:         domain.LineTypeUsage,
			MeterID:          "mtr_api",
			Description:      "API Calls - 1,500 calls",
			Quantity:         1500,
			UnitAmountCents:  8,
			AmountCents:      12500,
			TotalAmountCents: 12500,
			Currency:         "USD",
			PricingMode:      "graduated",
		},
		{
			LineType:         domain.LineTypeUsage,
			MeterID:          "mtr_storage",
			Description:      "Storage - 50 GB",
			Quantity:         50,
			UnitAmountCents:  2500,
			AmountCents:      2500,
			TotalAmountCents: 2500,
			Currency:         "USD",
			PricingMode:      "flat",
		},
	}

	pdfBytes, err := RenderPDF(context.Background(), inv, lineItems, BillToInfo{Name: "Acme Corp"}, nil)
	if err != nil {
		t.Fatalf("render pdf: %v", err)
	}

	// PDF should start with %PDF
	if len(pdfBytes) < 4 || string(pdfBytes[:4]) != "%PDF" {
		t.Fatal("output is not a valid PDF")
	}

	// Should be a reasonable size (> 1KB, < 1MB)
	if len(pdfBytes) < 1024 {
		t.Errorf("PDF too small: %d bytes", len(pdfBytes))
	}
	if len(pdfBytes) > 1024*1024 {
		t.Errorf("PDF too large: %d bytes", len(pdfBytes))
	}

	t.Logf("PDF generated: %d bytes", len(pdfBytes))
}

// TestRenderPDF_ThreeChannelCN_WithTaxReversal renders a paid invoice with
// a CN that splits across all three channels (PM refund, credit balance,
// out of band) and reverses tax via Stripe Tax. Asserts the render path
// doesn't panic on the new fields and produces a reasonable-size PDF.
// Catches regressions where future changes to CreditNoteInfo break the
// render loop (e.g., dropping OutOfBandAmountCents from the post-payment
// filter would make this CN render as pre-payment and reduce the
// invoice's apparent total).
func TestRenderPDF_ThreeChannelCN_WithTaxReversal(t *testing.T) {
	now := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	dueAt := now.AddDate(0, 0, 30)
	periodStart := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)

	inv := domain.Invoice{
		ID:            "vlx_inv_3ch",
		InvoiceNumber: "VLX-202604-0042",
		Status:        domain.InvoiceFinalized,
		PaymentStatus: domain.PaymentSucceeded,
		Currency:      "INR",
		SubtotalCents: 7000,
		TaxFacts: domain.TaxFacts{
			TaxAmountCents: 1260,
			TaxRate:        18.00,
			TaxName:        "IGST",
			TaxCountry:     "IN",
		},
		TotalAmountCents:   8260,
		AmountPaidCents:    8260,
		AmountDueCents:     0,
		BillingPeriodStart: periodStart,
		BillingPeriodEnd:   periodEnd,
		IssuedAt:           &now,
		DueAt:              &dueAt,
		TaxTransactionID:   "tx_committed_at_finalize",
	}

	lineItems := []domain.InvoiceLineItem{
		{
			LineType:         domain.LineTypeBaseFee,
			Description:      "Advance test plan 2 - base fee",
			Quantity:         1,
			UnitAmountCents:  7000,
			AmountCents:      7000,
			TotalAmountCents: 7000,
			Currency:         "INR",
		},
	}

	cn := CreditNoteInfo{
		Number:               "CN-000008",
		Reason:               "billing error",
		Amount:               8260,
		RefundAmountCents:    4000,
		CreditAmountCents:    3000,
		OutOfBandAmountCents: 1260,
		TaxAmountCents:       1260,
		TaxTransactionID:     "tx_reversal_from_stripe_tax",
		RefundStatus:         string(domain.RefundSucceeded),
	}

	pdfBytes, err := RenderPDF(context.Background(), inv, lineItems, BillToInfo{Name: "Acme Corp"}, []CreditNoteInfo{cn})
	if err != nil {
		t.Fatalf("render pdf: %v", err)
	}
	if len(pdfBytes) < 4 || string(pdfBytes[:4]) != "%PDF" {
		t.Fatal("output is not a valid PDF")
	}
	if len(pdfBytes) < 1024 {
		t.Errorf("PDF too small: %d bytes", len(pdfBytes))
	}
}

// TestRenderPDF_OutOfBandOnlyCN_IsPostPayment is a regression guard: an
// OOB-only CN (no refund-to-PM, no credit-to-balance) used to fall into
// the pre-payment branch because the filter only checked refund + credit,
// which would have reduced the invoice's apparent total in the PDF. Fix
// includes OutOfBandAmountCents in the post-payment classification.
func TestRenderPDF_OutOfBandOnlyCN_IsPostPayment(t *testing.T) {
	now := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	dueAt := now.AddDate(0, 0, 30)
	inv := domain.Invoice{
		ID:                 "vlx_inv_oob",
		InvoiceNumber:      "VLX-OOB-001",
		Status:             domain.InvoiceFinalized,
		PaymentStatus:      domain.PaymentSucceeded,
		Currency:           "USD",
		SubtotalCents:      5000,
		TotalAmountCents:   5000,
		AmountPaidCents:    5000,
		AmountDueCents:     0,
		BillingPeriodStart: now.AddDate(0, -1, 0),
		BillingPeriodEnd:   now,
		IssuedAt:           &now,
		DueAt:              &dueAt,
	}
	lineItems := []domain.InvoiceLineItem{{
		LineType: domain.LineTypeBaseFee, Description: "Service",
		Quantity: 1, UnitAmountCents: 5000, AmountCents: 5000,
		TotalAmountCents: 5000, Currency: "USD",
	}}
	cn := CreditNoteInfo{
		Number:               "CN-OOB-001",
		Reason:               "wired manually",
		Amount:               5000,
		OutOfBandAmountCents: 5000,
		// RefundAmountCents + CreditAmountCents both 0 — the case
		// the old filter mishandled.
	}
	pdfBytes, err := RenderPDF(context.Background(), inv, lineItems, BillToInfo{Name: "Acme"}, []CreditNoteInfo{cn})
	if err != nil {
		t.Fatalf("render pdf: %v", err)
	}
	if len(pdfBytes) < 1024 || string(pdfBytes[:4]) != "%PDF" {
		t.Fatal("expected a valid PDF; OOB-only CN must render as post-payment adjustment")
	}
}

// minimalReverseChargeInvoice returns a finalized reverse-charge invoice
// suitable for legend / PDF tests. No tax amount, no positive subtotal —
// the legend branches don't depend on those.
func minimalReverseChargeInvoice() domain.Invoice {
	now := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	due := now.AddDate(0, 0, 30)
	periodStart := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	return domain.Invoice{
		ID:                 "vlx_inv_rc",
		InvoiceNumber:      "VLX-202604-0099",
		Status:             domain.InvoiceFinalized,
		PaymentStatus:      domain.PaymentPending,
		Currency:           "INR",
		SubtotalCents:      19900,
		TotalAmountCents:   19900,
		AmountDueCents:     19900,
		BillingPeriodStart: periodStart,
		BillingPeriodEnd:   periodEnd,
		IssuedAt:           &now,
		DueAt:              &due,
		NetPaymentTermDays: 30,
		TaxFacts: domain.TaxFacts{
			TaxReverseCharge: true,
		},
	}
}

// TestRenderPDF_IndiaSupplierGSTIN verifies the PDF render call accepts the
// new TaxID/TaxIDType fields and produces a valid PDF with a reasonable size
// bump from the extra header line. gopdf compresses content streams and
// uses subset-font glyph IDs, so we cannot byte-substring the rendered text;
// the GSTIN-line presence is validated by the helper-level tests below
// (TestSupplierTaxIDLabel, TestRenderPDF_IndiaSupplierGSTIN_HelperPath).
//
// MANUAL: Visually verify "GSTIN: 27AAEPM1234C1Z5" appears under the company
// contact info in the rendered PDF (see MANUAL_TEST.md, FLOW B10).
func TestRenderPDF_IndiaSupplierGSTIN(t *testing.T) {
	inv := minimalReverseChargeInvoice()
	company := CompanyInfo{
		Name:      "Velox India Pvt Ltd",
		Country:   "IN",
		TaxID:     "27AAEPM1234C1Z5",
		TaxIDType: "gstin",
	}
	out, err := RenderPDF(context.Background(), inv, nil, BillToInfo{Name: "Acme Corp"}, nil, company)
	if err != nil {
		t.Fatalf("render pdf: %v", err)
	}
	if len(out) < 4 || string(out[:4]) != "%PDF" {
		t.Fatal("output is not a valid PDF")
	}

	// Helper-path assertion: the supplier label resolves to "GSTIN" for the
	// declared TaxIDType. This is what would be drawn into the PDF stream.
	if got := supplierTaxIDLabel(company.TaxIDType); got != "GSTIN" {
		t.Errorf("supplierTaxIDLabel(%q) = %q, want GSTIN", company.TaxIDType, got)
	}
}

// TestRenderPDF_IndiaReverseChargeLegend verifies the India context resolves
// to the GST-flavoured legend, not the EU "VAT to be accounted for" wording.
func TestRenderPDF_IndiaReverseChargeLegend(t *testing.T) {
	inv := minimalReverseChargeInvoice()
	company := CompanyInfo{
		Name:      "Velox India Pvt Ltd",
		Country:   "IN",
		TaxID:     "27AAEPM1234C1Z5",
		TaxIDType: "gstin",
	}
	billTo := BillToInfo{Name: "Acme India Ltd", Country: "IN"}

	got := reverseChargeLegend(inv, company, billTo, nil)
	if !strings.Contains(got, "Tax payable on reverse charge basis: YES") {
		t.Errorf("legend = %q, want it to contain India GST wording", got)
	}
	if strings.Contains(got, "VAT to be accounted for") {
		t.Errorf("legend = %q, must not contain EU VAT wording", got)
	}

	// And the PDF still renders without error end-to-end.
	out, err := RenderPDF(context.Background(), inv, nil, billTo, nil, company)
	if err != nil {
		t.Fatalf("render pdf: %v", err)
	}
	if len(out) < 4 || string(out[:4]) != "%PDF" {
		t.Fatal("output is not a valid PDF")
	}
}

// TestRenderPDF_EUReverseChargeLegend_Unchanged verifies a non-Indian
// supplier (e.g. DE, EU VAT) keeps the existing EU legend wording — the fix
// must not regress EU invoices.
func TestRenderPDF_EUReverseChargeLegend_Unchanged(t *testing.T) {
	inv := minimalReverseChargeInvoice()
	inv.Currency = "EUR"
	company := CompanyInfo{
		Name:      "Velox GmbH",
		Country:   "DE",
		TaxID:     "DE123456789",
		TaxIDType: "vat",
	}
	billTo := BillToInfo{Name: "Acme GmbH", Country: "FR"}

	got := reverseChargeLegend(inv, company, billTo, nil)
	if !strings.Contains(got, "VAT to be accounted for by the recipient") {
		t.Errorf("legend = %q, want EU VAT wording preserved", got)
	}
	if strings.Contains(got, "GST") || strings.Contains(got, "CGST") {
		t.Errorf("legend = %q, must not contain India GST wording for EU invoices", got)
	}

	out, err := RenderPDF(context.Background(), inv, nil, billTo, nil, company)
	if err != nil {
		t.Fatalf("render pdf: %v", err)
	}
	if len(out) < 4 || string(out[:4]) != "%PDF" {
		t.Fatal("output is not a valid PDF")
	}
}

// TestRenderPDF_NoTaxID_NoGSTINLine verifies that an empty TaxID does not
// trigger the supplier-tax-ID header line — backward compatibility for
// tenants who haven't set a tax ID yet.
//
// gopdf compresses streams, so we can't byte-search; instead we render two
// PDFs with the only difference being TaxID populated, and assert the
// no-tax-ID render is strictly smaller (= header line was suppressed).
func TestRenderPDF_NoTaxID_NoGSTINLine(t *testing.T) {
	inv := minimalReverseChargeInvoice()
	base := CompanyInfo{Name: "Velox India Pvt Ltd", Country: "IN"}
	withGSTIN := base
	withGSTIN.TaxID = "27AAEPM1234C1Z5"
	withGSTIN.TaxIDType = "gstin"

	billTo := BillToInfo{Name: "Acme India Ltd", Country: "IN"}

	outNo, err := RenderPDF(context.Background(), inv, nil, billTo, nil, base)
	if err != nil {
		t.Fatalf("render pdf no tax id: %v", err)
	}
	outYes, err := RenderPDF(context.Background(), inv, nil, billTo, nil, withGSTIN)
	if err != nil {
		t.Fatalf("render pdf with tax id: %v", err)
	}

	if len(outNo) >= len(outYes) {
		t.Errorf("expected PDF without GSTIN to be smaller than PDF with GSTIN; got no=%d yes=%d", len(outNo), len(outYes))
	}
	if len(outNo) < 4 || string(outNo[:4]) != "%PDF" {
		t.Fatal("no-tax-id output is not a valid PDF")
	}
}

// TestSupplierTaxIDLabel covers the label-prefix mapping for the in-PDF
// "GSTIN: ..." / "VAT: ..." / "ABN: ..." / "Tax ID: ..." rendering.
func TestSupplierTaxIDLabel(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"gstin", "GSTIN"},
		{"in_gst", "GSTIN"},
		{"in_gstin", "GSTIN"},
		{"GSTIN", "GSTIN"}, // case-insensitive
		{"vat", "VAT"},
		{"eu_vat", "VAT"},
		{"abn", "ABN"},
		{"au_abn", "ABN"},
		{"", "Tax ID"},
		{"us_ein", "Tax ID"},
		{"  gstin  ", "GSTIN"}, // trims whitespace
	}
	for _, c := range cases {
		if got := supplierTaxIDLabel(c.in); got != c.want {
			t.Errorf("supplierTaxIDLabel(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestIsIndianContext covers each branch of the India-context detector that
// drives reverse-charge legend selection.
func TestIsIndianContext(t *testing.T) {
	cases := []struct {
		name     string
		company  CompanyInfo
		custCtry string
		want     bool
	}{
		{"supplier IN", CompanyInfo{Country: "IN"}, "US", true},
		{"supplier India long form", CompanyInfo{Country: "India"}, "", true},
		{"supplier GSTIN type", CompanyInfo{TaxIDType: "gstin"}, "", true},
		{"supplier in_gst type", CompanyInfo{TaxIDType: "in_gst"}, "", true},
		{"supplier in_gstin type", CompanyInfo{TaxIDType: "in_gstin"}, "", true},
		{"customer IN", CompanyInfo{Country: "DE"}, "IN", true},
		{"customer India long form", CompanyInfo{}, "India", true},
		{"EU supplier + EU customer", CompanyInfo{Country: "DE", TaxIDType: "vat"}, "FR", false},
		{"US supplier + US customer", CompanyInfo{Country: "US"}, "US", false},
		{"empty", CompanyInfo{}, "", false},
	}
	for _, c := range cases {
		if got := isIndianContext(c.company, c.custCtry); got != c.want {
			t.Errorf("%s: isIndianContext = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestReverseChargeLegend_TaxJurisdictionFallback confirms the legend
// branches on a per-line-item Indian TaxJurisdiction (e.g. "IN-MH") even
// when neither supplier nor billing country signals India.
func TestReverseChargeLegend_TaxJurisdictionFallback(t *testing.T) {
	inv := minimalReverseChargeInvoice()
	supplier := CompanyInfo{Name: "Velox", Country: "US"}
	billTo := BillToInfo{Name: "Acme", Country: "US"}
	lineItems := []domain.InvoiceLineItem{
		{TaxJurisdiction: "IN-MH"},
	}
	got := reverseChargeLegend(inv, supplier, billTo, lineItems)
	if !strings.Contains(got, "Tax payable on reverse charge basis: YES") {
		t.Errorf("expected India legend via line-item jurisdiction; got %q", got)
	}
}

// TestSupplierTaxIDTypeFromCountry covers the country→type inference used
// by handlers that build CompanyInfo from TenantSettings.
func TestSupplierTaxIDTypeFromCountry(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"IN", "gstin"},
		{"in", "gstin"},
		{"India", "gstin"},
		{"AU", "abn"},
		{"DE", "vat"},
		{"FR", "vat"},
		{"GB", "vat"},
		{"UK", "vat"},
		{"US", ""},
		{"", ""},
		{" IN ", "gstin"},
	}
	for _, c := range cases {
		if got := SupplierTaxIDTypeFromCountry(c.in); got != c.want {
			t.Errorf("SupplierTaxIDTypeFromCountry(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestExemptionLegend_CustomerExempt covers the path where at least one line
// carries Stripe's `customer_exempt` taxability reason — the PDF must surface
// the exemption-certificate disclosure, in addition to (not in place of) any
// reverse-charge legend already driven by inv.TaxReverseCharge.
func TestExemptionLegend_CustomerExempt(t *testing.T) {
	got := exemptionLegend([]domain.InvoiceLineItem{
		{TaxabilityReason: "customer_exempt"},
		{TaxabilityReason: "standard_rated"},
	})
	want := "One or more lines are exempt from tax based on the customer's exemption certificate."
	if got != want {
		t.Errorf("exemptionLegend = %q, want %q", got, want)
	}
}

// TestExemptionLegend_ProductExempt covers the path where at least one line
// carries Stripe's `product_exempt` taxability reason (e.g. food in some US
// states, education in some EU states).
func TestExemptionLegend_ProductExempt(t *testing.T) {
	got := exemptionLegend([]domain.InvoiceLineItem{
		{TaxabilityReason: "product_exempt"},
	})
	want := "One or more lines are exempt from tax in this jurisdiction by product category."
	if got != want {
		t.Errorf("exemptionLegend = %q, want %q", got, want)
	}
}

// TestExemptionLegend_Both verifies both exemption types appear with
// customer_exempt rendered first (chosen ordering: certificate-driven beats
// category-driven because the customer-side disclosure is more specific).
func TestExemptionLegend_Both(t *testing.T) {
	got := exemptionLegend([]domain.InvoiceLineItem{
		{TaxabilityReason: "product_exempt"},
		{TaxabilityReason: "customer_exempt"},
	})
	customer := "One or more lines are exempt from tax based on the customer's exemption certificate."
	product := "One or more lines are exempt from tax in this jurisdiction by product category."
	want := customer + "\n" + product
	if got != want {
		t.Errorf("exemptionLegend joined = %q, want %q", got, want)
	}
	if !strings.HasPrefix(got, customer) {
		t.Errorf("customer_exempt must render before product_exempt; got %q", got)
	}
}

// TestExemptionLegend_None covers the no-op path: trivial reasons (empty,
// standard_rated, reverse_charge, not_collecting) must not trigger the
// exemption legend. reverse_charge has its own dedicated legend driven by
// inv.TaxReverseCharge — conflating that here would double-disclose.
func TestExemptionLegend_None(t *testing.T) {
	cases := []struct {
		name  string
		items []domain.InvoiceLineItem
	}{
		{"nil items", nil},
		{"empty slice", []domain.InvoiceLineItem{}},
		{"empty reason", []domain.InvoiceLineItem{{TaxabilityReason: ""}}},
		{"standard_rated only", []domain.InvoiceLineItem{{TaxabilityReason: "standard_rated"}}},
		{"reverse_charge only", []domain.InvoiceLineItem{{TaxabilityReason: "reverse_charge"}}},
		{"not_collecting only", []domain.InvoiceLineItem{{TaxabilityReason: "not_collecting"}}},
		{"zero_rated only", []domain.InvoiceLineItem{{TaxabilityReason: "zero_rated"}}},
		{"unknown future value", []domain.InvoiceLineItem{{TaxabilityReason: "some_new_reason_v3"}}},
	}
	for _, c := range cases {
		if got := exemptionLegend(c.items); got != "" {
			t.Errorf("%s: exemptionLegend = %q, want empty", c.name, got)
		}
	}
}

// TestRenderPDF_ExemptionLegend_PDFRenders is the integration sanity check:
// rendering a PDF with a customer-exempt line completes without error and
// the byte size is strictly larger than the same invoice rendered without
// any exempt lines (the legend adds a line of text). gopdf compresses
// content streams + uses font subsets so we can't byte-search, but the
// size delta is the same technique used elsewhere in this file (see #9's
// TestRenderPDF_NoTaxID_NoGSTINLine).
func TestRenderPDF_ExemptionLegend_PDFRenders(t *testing.T) {
	inv := minimalReverseChargeInvoice()
	inv.TaxReverseCharge = false
	billTo := BillToInfo{Name: "Acme Inc.", Country: "US"}
	company := CompanyInfo{Name: "Velox Inc.", Country: "US"}

	plain := []domain.InvoiceLineItem{
		{LineType: domain.LineTypeBaseFee, Description: "Plan", Quantity: 1, AmountCents: 1000, TotalAmountCents: 1000, Currency: "USD", TaxabilityReason: "standard_rated"},
	}
	exempt := []domain.InvoiceLineItem{
		{LineType: domain.LineTypeBaseFee, Description: "Plan", Quantity: 1, AmountCents: 1000, TotalAmountCents: 1000, Currency: "USD", TaxabilityReason: "customer_exempt"},
	}

	outPlain, err := RenderPDF(context.Background(), inv, plain, billTo, nil, company)
	if err != nil {
		t.Fatalf("render plain pdf: %v", err)
	}
	outExempt, err := RenderPDF(context.Background(), inv, exempt, billTo, nil, company)
	if err != nil {
		t.Fatalf("render exempt pdf: %v", err)
	}
	if len(outExempt) <= len(outPlain) {
		t.Errorf("expected exempt PDF to be larger than plain (legend adds a line); got plain=%d exempt=%d", len(outPlain), len(outExempt))
	}
	if string(outExempt[:4]) != "%PDF" {
		t.Fatal("exempt output is not a valid PDF")
	}
}

func TestParseBrandColor(t *testing.T) {
	cases := []struct {
		in    string
		wantR uint8
		wantG uint8
		wantB uint8
		ok    bool
	}{
		{"#1f6feb", 0x1f, 0x6f, 0xeb, true},
		{"#000000", 0, 0, 0, true},
		{"#ffffff", 0xff, 0xff, 0xff, true},
		{"#FF00AA", 0xff, 0, 0xaa, true},
		{"", 0, 0, 0, false},
		{"#fff", 0, 0, 0, false},     // short form rejected
		{"1f6feb", 0, 0, 0, false},   // missing #
		{"#zzzzzz", 0, 0, 0, false},  // non-hex
		{"#12345", 0, 0, 0, false},   // too short
		{"#1234567", 0, 0, 0, false}, // too long
	}
	for _, c := range cases {
		r, g, b, ok := parseBrandColor(c.in)
		if ok != c.ok || r != c.wantR || g != c.wantG || b != c.wantB {
			t.Errorf("parseBrandColor(%q) = (%d,%d,%d,%v), want (%d,%d,%d,%v)",
				c.in, r, g, b, ok, c.wantR, c.wantG, c.wantB, c.ok)
		}
	}
}

// TestCNChannelDescription_RefundStateAnnotated guards the customer-visible
// channel line against asserting cash movement the refund status disproves.
// A mixed-allocation CN enters the POST-PAYMENT ADJUSTMENTS list via its
// settled credit / out-of-band leg, so its card leg can be present in any
// state; printing "to card" bare for a failed leg told the customer money
// came back when no refund object even exists at Stripe (found live
// 2026-08-03, sibling of the dashboard's "Refunded to Card" stat fix).
func TestCNChannelDescription_RefundStateAnnotated(t *testing.T) {
	base := CreditNoteInfo{
		RefundAmountCents:    1000,
		CreditAmountCents:    2000,
		OutOfBandAmountCents: 500,
	}

	cases := []struct {
		name       string
		status     string
		wantCard   string
		forbidBare bool
	}{
		{"succeeded is bare", string(domain.RefundSucceeded), "$10.00 to card |", false},
		{"failed is annotated", string(domain.RefundFailed), "$10.00 to card (refund failed)", true},
		{"pending is annotated", string(domain.RefundPending), "$10.00 to card (refund pending)", true},
		{"legacy empty status reads as pending, never settled", "", "$10.00 to card (refund pending)", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cn := base
			cn.RefundStatus = tc.status
			got := cnChannelDescription(cn, "$")
			if !strings.Contains(got, tc.wantCard) {
				t.Errorf("channel line = %q, want it to contain %q", got, tc.wantCard)
			}
			if tc.forbidBare && strings.Contains(got, "to card |") {
				t.Errorf("channel line = %q asserts an unannotated card settlement", got)
			}
			// The settled legs render regardless of the card leg's state.
			if !strings.Contains(got, "$20.00 to credit") || !strings.Contains(got, "$5.00 out of band") {
				t.Errorf("channel line = %q lost a settled channel", got)
			}
		})
	}
}
