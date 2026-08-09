package invoice

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/shopspring/decimal"
	"github.com/signintech/gopdf"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/pdffonts"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
)

// CompanyInfo holds tenant company details for the PDF header.
type CompanyInfo struct {
	Name         string
	Email        string
	Phone        string
	AddressLine1 string
	AddressLine2 string
	City         string
	State        string
	PostalCode   string
	Country      string
	// BrandColor is a 7-char hex string (#rrggbb) applied to the company
	// name and a thin accent bar between header and body. Empty falls back
	// to the neutral palette — the exact palette existing PDFs used before
	// this field was introduced.
	BrandColor string
	// TaxID is the SUPPLIER's tax identifier printed in the invoice header
	// (GSTIN, EU VAT, ABN, etc.). Mandatory on Indian B2B invoices under
	// Rule 46 of the CGST Rules; widely expected on EU/AU invoices too.
	// Empty = no header line rendered (legacy behaviour preserved).
	TaxID string
	// TaxIDType is the canonical kind of TaxID: "gstin" / "in_gst" /
	// "in_gstin" for India, "vat" / "eu_vat" for EU, "abn" / "au_abn" for
	// Australia. Drives both the header label ("GSTIN: ..." vs "VAT: ...")
	// and the reverse-charge legend wording. Unknown/empty values fall back
	// to a generic "Tax ID:" label.
	TaxIDType string
}

// SupplierTaxIDTypeFromCountry infers a TaxIDType from the supplier's
// registered country when settings doesn't store the type explicitly. Used
// by handlers that build CompanyInfo from TenantSettings (which currently
// carries TaxID without a separate kind column). Returns the empty string
// for countries we don't have an opinion on — the PDF then prints a
// generic "Tax ID:" label rather than misrepresenting the scheme.
//
// Mapping is intentionally narrow: only India, Australia, and the EU/UK
// have unambiguous single-scheme codes. Multi-scheme countries (US: EIN
// vs SSN vs state IDs) require an explicit type from the operator and are
// left unmapped here.
func SupplierTaxIDTypeFromCountry(country string) string {
	c := strings.ToUpper(strings.TrimSpace(country))
	switch c {
	case "IN", "INDIA":
		return "gstin"
	case "AU", "AUSTRALIA":
		return "abn"
	case "AT", "BE", "BG", "HR", "CY", "CZ", "DK", "EE", "FI", "FR",
		"DE", "GR", "HU", "IE", "IT", "LV", "LT", "LU", "MT", "NL",
		"PL", "PT", "RO", "SK", "SI", "ES", "SE", "GB", "UK":
		return "vat"
	default:
		return ""
	}
}

// supplierTaxIDLabel maps a TaxIDType to the in-PDF label prefix. Unknown or
// empty kinds fall back to a generic "Tax ID" so the value still appears on
// the invoice without misrepresenting the scheme.
func supplierTaxIDLabel(taxIDType string) string {
	switch strings.ToLower(strings.TrimSpace(taxIDType)) {
	case "gstin", "in_gst", "in_gstin":
		return "GSTIN"
	case "vat", "eu_vat":
		return "VAT"
	case "abn", "au_abn":
		return "ABN"
	default:
		return "Tax ID"
	}
}

// reverseChargeLegend resolves the statutory disclosure text for a
// reverse-charge invoice. India GST (Rule 46(p), CGST §9(3)/9(4)) and EU
// VAT (Art. 196 of the VAT Directive) carry materially different mandated
// wording, so the legend branches on jurisdiction. Pure helper —
// unit-testable without rendering a PDF.
func reverseChargeLegend(inv domain.Invoice, supplier CompanyInfo, billTo BillToInfo, lineItems []domain.InvoiceLineItem) string {
	indian := isIndianContext(supplier, billTo.Country) ||
		lineItemsAreIndian(lineItems) ||
		strings.EqualFold(strings.TrimSpace(inv.TaxCountry), "IN")
	if indian {
		return "Tax payable on reverse charge basis: YES — recipient is liable to pay GST under section 9(3)/9(4) of the CGST Act."
	}
	return "Reverse charge — VAT to be accounted for by the recipient."
}

// lineItemsAreIndian reports whether any line item carries an Indian
// tax jurisdiction code. The tax engine stamps line-item TaxJurisdiction
// like "IN-MH" (state-coded) or "IN" — used as a fallback signal when
// neither supplier nor billing-country tells us the legend should be
// India-flavoured.
func lineItemsAreIndian(lineItems []domain.InvoiceLineItem) bool {
	for _, li := range lineItems {
		j := strings.ToUpper(strings.TrimSpace(li.TaxJurisdiction))
		if j == "IN" || strings.HasPrefix(j, "IN-") {
			return true
		}
	}
	return false
}

// isIndianContext returns true when the invoice is plausibly an Indian tax
// invoice — supplier registered in India, supplier carrying a GSTIN, or
// recipient billed to India. Drives the reverse-charge legend wording so
// Indian invoices use GST terminology and EU/other invoices keep the VAT
// Directive language they had before.
func isIndianContext(company CompanyInfo, customerCountry string) bool {
	if strings.EqualFold(company.Country, "IN") || strings.EqualFold(company.Country, "India") {
		return true
	}
	t := strings.ToLower(strings.TrimSpace(company.TaxIDType))
	if t == "gstin" || t == "in_gst" || t == "in_gstin" {
		return true
	}
	if strings.EqualFold(customerCountry, "IN") || strings.EqualFold(customerCountry, "India") {
		return true
	}
	return false
}

// parseBrandColor converts a #rrggbb string to RGB. Returns ok=false for
// anything malformed so callers can cleanly fall back to the default palette.
func parseBrandColor(hex string) (r, g, b uint8, ok bool) {
	if len(hex) != 7 || hex[0] != '#' {
		return 0, 0, 0, false
	}
	parse := func(s string) (uint8, bool) {
		var v uint8
		for i := 0; i < len(s); i++ {
			c := s[i]
			var n uint8
			switch {
			case c >= '0' && c <= '9':
				n = c - '0'
			case c >= 'a' && c <= 'f':
				n = c - 'a' + 10
			case c >= 'A' && c <= 'F':
				n = c - 'A' + 10
			default:
				return 0, false
			}
			v = v<<4 | n
		}
		return v, true
	}
	rr, ok1 := parse(hex[1:3])
	gg, ok2 := parse(hex[3:5])
	bb, ok3 := parse(hex[5:7])
	if !ok1 || !ok2 || !ok3 {
		return 0, 0, 0, false
	}
	return rr, gg, bb, true
}

// CreditNoteInfo holds credit note data for the totals section.
type CreditNoteInfo struct {
	Number               string
	Reason               string
	Amount               int64
	RefundAmountCents    int64
	CreditAmountCents    int64
	OutOfBandAmountCents int64
	// TaxAmountCents is the proportional tax portion of this CN
	// (back-solved from the invoice's tax ratio at CN create time).
	// Rendered as a sub-fact under the CN row when > 0.
	TaxAmountCents int64
	// TaxTransactionID is the upstream reversal transaction id (Stripe
	// Tax: tx_xxx). Non-empty when the CN issued an upstream reversal;
	// empty for manual/none providers and legacy CNs. Drives the
	// "(Stripe Tax)" vs "(no upstream provider)" suffix.
	TaxTransactionID string
	RefundStatus     string
}

// cnChannelDescription builds the per-channel breakdown line for a
// post-payment credit note in the POST-PAYMENT ADJUSTMENTS section.
//
// A mixed-allocation CN reaches that list via its settled credit /
// out-of-band leg, so its card leg can be present in ANY refund state —
// a non-succeeded leg is annotated rather than printed bare, because
// "1,000.00 to card" on a customer-visible document asserts cash moved
// when refund_status says it didn't. Same rule as the dashboard
// waterfall's channelDescription; keep the two in step.
//
// ASCII-safe on purpose: the embedded Noto Sans subset covers Latin +
// currency symbols but NOT arrows (→, ↳), so the dashboard's arrow
// styling would render as missing-glyph boxes here.
func cnChannelDescription(cn CreditNoteInfo, symbol string) string {
	var channels []string
	if cn.RefundAmountCents > 0 {
		note := ""
		switch cn.RefundStatus {
		case string(domain.RefundSucceeded):
			// settled — no annotation
		case string(domain.RefundFailed):
			note = " (refund failed)"
		default:
			// pending, or empty on legacy rows: either way, not settled.
			note = " (refund pending)"
		}
		channels = append(channels, formatCentsIn(symbol, cn.RefundAmountCents)+" to card"+note)
	}
	if cn.CreditAmountCents > 0 {
		channels = append(channels, formatCentsIn(symbol, cn.CreditAmountCents)+" to credit")
	}
	if cn.OutOfBandAmountCents > 0 {
		channels = append(channels, formatCentsIn(symbol, cn.OutOfBandAmountCents)+" out of band")
	}
	return strings.Join(channels, " | ")
}

// BillToInfo holds the customer's billing address for the PDF.
type BillToInfo struct {
	Name         string
	Email        string
	AddressLine1 string
	AddressLine2 string
	City         string
	State        string
	PostalCode   string
	Country      string
	// TaxID is the buyer's VAT/GSTIN/ABN registration. Required on
	// legally compliant B2B invoices (EU VAT Directive Art. 226, India
	// GST) — the credit-note PDF already carried it; invoices drifted
	// without it.
	TaxID string
}

// Currency symbol map
var currencySymbols = map[string]string{
	"USD": "$", "EUR": "€", "GBP": "£", "JPY": "¥", "CNY": "¥",
	"INR": "₹", "BRL": "R$", "CAD": "CA$", "AUD": "A$", "CHF": "CHF ",
	"SGD": "S$", "HKD": "HK$", "NZD": "NZ$", "MXN": "MX$",
	"KRW": "₩", "ZAR": "R", "PLN": "zł", "AED": "AED ", "SAR": "SAR ",
	"THB": "฿", "MYR": "RM ", "IDR": "Rp ", "PHP": "₱", "VND": "₫",
}

// currencySymbolFor maps an ISO currency to its display symbol,
// falling back to "CUR " prefixing for unmapped codes.
func currencySymbolFor(currency string) string {
	if sym, ok := currencySymbols[strings.ToUpper(currency)]; ok {
		return sym
	}
	if currency != "" {
		return currency + " "
	}
	return "$"
}

func RenderPDF(ctx context.Context, inv domain.Invoice, lineItems []domain.InvoiceLineItem, billTo BillToInfo, creditNotes []CreditNoteInfo, company ...CompanyInfo) ([]byte, error) {
	// Currency symbol is LOCAL to this render. It was a package-level
	// var mutated at the top of every call — two concurrent renders
	// (email + download, two operators, hosted + dashboard) with
	// different currencies raced, and an invoice could print with the
	// OTHER render's symbol on every money field. The shadowing
	// closures below keep the 19 call sites unchanged.
	symbol := currencySymbolFor(inv.Currency)
	formatCents := func(c int64) string { return formatCentsIn(symbol, c) }
	formatRate := func(d decimal.Decimal) string { return formatRateIn(symbol, d) }

	pdf := &gopdf.GoPdf{}
	pdf.Start(gopdf.Config{PageSize: *gopdf.PageSizeA4})
	pdf.AddPage()

	if err := pdffonts.RegisterNotoSans(pdf); err != nil {
		return nil, err
	}

	// Helper functions
	setFont := func(bold bool, size int) {
		name := "noto"
		if bold {
			name = "noto-bold"
		}
		_ = pdf.SetFont(name, "", size)
	}

	setColor := func(r, g, b uint8) {
		pdf.SetTextColor(r, g, b)
	}

	textAt := func(x, y float64, text string) {
		pdf.SetXY(x, y)
		_ = pdf.Cell(nil, text)
	}

	rightAlignAt := func(x, y, width float64, text string) {
		tw, _ := pdf.MeasureTextWidth(text)
		pdf.SetXY(x+width-tw, y)
		_ = pdf.Cell(nil, text)
	}

	// ── VOID watermark ──
	if inv.Status == domain.InvoiceVoided {
		setFont(true, 72)
		setColor(230, 230, 230)
		textAt(55, 400, "VOID")
	}

	pageW := 595.28 // A4 width in points
	margin := 40.0
	contentW := pageW - margin*2
	y := 40.0

	// ── Header ──
	companyName := "Velox"
	var companyAddrLines []string
	companyContact := ""
	var brandR, brandG, brandB uint8
	brandSet := false
	if len(company) > 0 && company[0].Name != "" {
		c := company[0]
		companyName = c.Name
		if c.AddressLine1 != "" {
			companyAddrLines = append(companyAddrLines, c.AddressLine1)
		}
		if c.AddressLine2 != "" {
			companyAddrLines = append(companyAddrLines, c.AddressLine2)
		}
		if cityLine := formatCityStatePostal(c.City, c.State, c.PostalCode); cityLine != "" {
			companyAddrLines = append(companyAddrLines, cityLine)
		}
		if c.Country != "" {
			companyAddrLines = append(companyAddrLines, c.Country)
		}
		parts := []string{}
		if c.Email != "" {
			parts = append(parts, c.Email)
		}
		if c.Phone != "" {
			parts = append(parts, c.Phone)
		}
		if len(parts) > 0 {
			companyContact = strings.Join(parts, "  |  ")
		}
		if r, g, b, ok := parseBrandColor(c.BrandColor); ok {
			brandR, brandG, brandB = r, g, b
			brandSet = true
		}
	}

	setFont(true, 18)
	if brandSet {
		setColor(brandR, brandG, brandB)
	} else {
		setColor(30, 30, 30)
	}
	textAt(margin, y, companyName)

	setFont(true, 18)
	setColor(100, 100, 100)
	rightAlignAt(margin, y, contentW, "INVOICE")
	y += 24

	if len(companyAddrLines) > 0 {
		setFont(false, 8)
		setColor(100, 100, 100)
		for _, line := range companyAddrLines {
			textAt(margin, y, line)
			y += 11
		}
	}
	if companyContact != "" {
		setFont(false, 8)
		setColor(120, 120, 120)
		textAt(margin, y, companyContact)
		y += 11
	}

	// Supplier tax ID line. Mandatory header field on Indian B2B invoices
	// (Rule 46, CGST Rules) and widely expected on EU/AU invoices too.
	// Rendered in the same muted style as contact info so it reads as
	// header metadata, not a primary visual element.
	if len(company) > 0 && strings.TrimSpace(company[0].TaxID) != "" {
		setFont(false, 8)
		setColor(120, 120, 120)
		textAt(margin, y, supplierTaxIDLabel(company[0].TaxIDType)+": "+company[0].TaxID)
		y += 11
	}

	// Thin accent bar under the header — visible branding without overwhelming
	// the document. Skipped when no brand color is set so existing tenants'
	// PDFs are byte-identical to their pre-0046 output.
	if brandSet {
		y += 8
		pdf.SetFillColor(brandR, brandG, brandB)
		pdf.RectFromUpperLeftWithStyle(margin, y, contentW, 2, "F")
		y += 8
	} else {
		y += 16
	}

	// ── Invoice Details (left) + Bill To (right) ──
	detailStartY := y

	setFont(true, 9)
	setColor(100, 100, 100)
	textAt(margin, y, "INVOICE DETAILS")
	y += 14

	detailRow := func(label, value string) {
		setFont(false, 9)
		setColor(100, 100, 100)
		textAt(margin, y, label)
		setColor(40, 40, 40)
		textAt(margin+70, y, value)
		y += 14
	}

	// Civil dates render in the invoice's own denormalized billing timezone (ADR-077),
	// falling back to UTC for legacy/ad-hoc invoices — NOT the raw process zone.
	// The document's Period row already uses this zone; the Issued/Due/Paid/Void
	// dates must match, or a non-UTC deployment prints a day-shifted calendar
	// date on a customer-retained financial document (ADR-075 audit).
	docLoc := domain.LoadLocationOrUTC(inv.BillingTimezone)

	detailRow("Number", inv.InvoiceNumber)
	if inv.IssuedAt != nil {
		detailRow("Issued", inv.IssuedAt.In(docLoc).Format("January 2, 2006"))
	}
	if inv.DueAt != nil {
		detailRow("Due Date", inv.DueAt.In(docLoc).Format("January 2, 2006"))
	}
	// Inclusive last-day period ("Jun 1 – Jun 30"), authored once via
	// domain.FormatInclusivePeriod (ADR-058 follow-up) and carried on
	// inv.BillingPeriodDisplay so the PDF, hosted page, and dashboard all show
	// the identical string. Empty for one-off / no-period invoices → omit the
	// row. Callers that fetch via GetByPublicToken (hosted/portal, bypassing the
	// service read decorator) populate this field before RenderPDF.
	if inv.BillingPeriodDisplay != "" {
		detailRow("Period", inv.BillingPeriodDisplay)
	}
	detailRow("Currency", strings.ToUpper(inv.Currency))

	leftBottom := y

	// Bill To (right column)
	rightX := 340.0
	by := detailStartY

	setFont(true, 9)
	setColor(100, 100, 100)
	textAt(rightX, by, "BILL TO")
	by += 14

	setFont(true, 10)
	setColor(40, 40, 40)
	textAt(rightX, by, billTo.Name)
	by += 14

	setFont(false, 9)
	setColor(80, 80, 80)
	if billTo.AddressLine1 != "" {
		textAt(rightX, by, billTo.AddressLine1)
		by += 12
	}
	if billTo.AddressLine2 != "" {
		textAt(rightX, by, billTo.AddressLine2)
		by += 12
	}
	cityLine := formatCityStatePostal(billTo.City, billTo.State, billTo.PostalCode)
	if cityLine != "" {
		textAt(rightX, by, cityLine)
		by += 12
	}
	if billTo.Country != "" {
		textAt(rightX, by, billTo.Country)
		by += 12
	}
	if billTo.TaxID != "" {
		// Buyer registration — same plain label the credit-note PDF
		// uses (the buyer supplies the id; guessing a jurisdiction
		// label from their country risks mislabeling it).
		textAt(rightX, by, "Tax ID: "+billTo.TaxID)
		by += 12
	}
	if billTo.Email != "" {
		setFont(false, 8)
		setColor(100, 100, 100)
		textAt(rightX, by, billTo.Email)
		by += 12
	}

	if by > leftBottom {
		y = by
	} else {
		y = leftBottom
	}
	y += 16

	// ── Line items table ──
	colX := []float64{margin, margin + 240, margin + 320, margin + 400}
	colEnd := margin + contentW

	// Header row
	setFont(true, 9)
	setColor(80, 80, 80)
	pdf.SetFillColor(245, 245, 245)
	pdf.RectFromUpperLeftWithStyle(margin, y-2, contentW, 20, "F")

	textAt(colX[0], y, "Description")
	rightAlignAt(colX[1], y, colX[2]-colX[1], "Qty")
	rightAlignAt(colX[2], y, colX[3]-colX[2], "Unit Price")
	rightAlignAt(colX[3], y, colEnd-colX[3], "Amount")
	y += 22

	// Line items
	setFont(false, 9)
	setColor(40, 40, 40)
	for _, item := range lineItems {
		desc := item.Description
		if len([]rune(desc)) > 50 {
			desc = string([]rune(desc)[:47]) + "..."
		}
		textAt(colX[0], y, desc)
		rightAlignAt(colX[1], y, colX[2]-colX[1], formatQuantity(item))
		// Token lines quote per 1M on every provider rate card — mirror the
		// dashboard's convention (domain.MeterUnitDisplayScale) so the PDF,
		// the hosted page, and the operator UI all read the same number.
		unitPrice := formatRate(item.DisplayUnitAmountDecimal())
		if shift, per := domain.MeterUnitDisplayScale(item.MeterUnit); shift > 0 {
			// Compact label ("/ 1M", not "/ 1M tokens"): the unit word is
			// already in the description, and the full form overflows the
			// unit-price column into Qty on wide rows (seen on render).
			unitPrice = formatRate(item.DisplayUnitAmountDecimal().Shift(shift)) + " / " + strings.Fields(per)[0]
		}
		rightAlignAt(colX[2], y, colX[3]-colX[2], unitPrice)
		rightAlignAt(colX[3], y, colEnd-colX[3], formatCents(item.AmountCents))
		y += 18
	}

	// Separator
	y += 4
	pdf.SetLineWidth(0.5)
	pdf.SetStrokeColor(200, 200, 200)
	pdf.Line(margin, y, margin+contentW, y)
	y += 8

	// ── Totals ──
	totalsX := 340.0
	totalsW := 140.0
	labelW := 90.0

	totalsRow := func(label string, amount string, bold bool, labelR, labelG, labelB, valR, valG, valB uint8) {
		if bold {
			setFont(true, 10)
		} else {
			setFont(false, 10)
		}
		setColor(labelR, labelG, labelB)
		textAt(totalsX, y, label)
		setColor(valR, valG, valB)
		rightAlignAt(totalsX+labelW, y, totalsW-labelW, amount)
		y += 16
	}

	totalsRow("Subtotal", formatCents(inv.SubtotalCents), false, 80, 80, 80, 40, 40, 40)

	if inv.DiscountCents > 0 {
		totalsRow("Discount", "-"+formatCents(inv.DiscountCents), false, 80, 80, 80, 40, 40, 40)
	}

	if inv.TaxAmountCents > 0 {
		taxLabel := "Tax"
		if inv.TaxName != "" {
			taxLabel = inv.TaxName
		}
		if inv.TaxRate > 0 {
			taxLabel = fmt.Sprintf("%s (%s%%)", taxLabel, formatTaxRate(inv.TaxRate))
		}
		if inv.TaxCountry != "" {
			taxLabel = fmt.Sprintf("%s [%s]", taxLabel, inv.TaxCountry)
		}
		totalsRow(taxLabel, formatCents(inv.TaxAmountCents), false, 80, 80, 80, 40, 40, 40)

		// Per-jurisdiction breakdown. Rendered indented under the aggregate
		// Tax row when lines span multiple jurisdictions (EU cross-state,
		// India CGST+SGST, US state+local). Single-jurisdiction invoices
		// skip this — the aggregate row already tells the whole story.
		jurisdictionAgg := aggregateTaxByJurisdiction(lineItems)
		if len(jurisdictionAgg) > 1 {
			setFont(false, 8)
			setColor(120, 120, 120)
			for _, row := range jurisdictionAgg {
				textAt(totalsX+12, y, row.label)
				rightAlignAt(totalsX+labelW, y, totalsW-labelW, formatCents(row.amount))
				y += 12
			}
			y += 4
		}

		// Show customer's Tax ID below the tax line
		if inv.TaxID != "" {
			setFont(false, 7)
			setColor(120, 120, 120)
			textAt(totalsX, y-12, inv.TaxID)
			y += 2
		}
	} else if inv.TaxReverseCharge {
		// Even with zero-amount tax, surface the reverse-charge row so the
		// invoice reads as a deliberate tax treatment rather than a bug.
		totalsRow("Tax (reverse charge)", formatCents(0), false, 80, 80, 80, 120, 120, 120)
	}

	// Total line
	pdf.SetStrokeColor(220, 220, 220)
	pdf.Line(totalsX, y-4, totalsX+totalsW, y-4)
	y += 2
	totalsRow("Total", formatCents(inv.TotalAmountCents), true, 30, 30, 30, 30, 30, 30)

	// Credit notes (pre-payment). A CN is "post-payment" if any of
	// the three channels is populated — refund-to-PM, credit-balance,
	// or out-of-band. Pre-payment CNs reduce the invoice subtotal
	// directly (unpaid-invoice path). Out-of-band-only CNs must NOT
	// be treated as pre-payment, otherwise they'd reduce the invoice
	// total even though the operator already handled the refund
	// externally.
	var postPaymentCNs []CreditNoteInfo
	for _, cn := range creditNotes {
		if cn.RefundAmountCents > 0 || cn.CreditAmountCents > 0 || cn.OutOfBandAmountCents > 0 {
			postPaymentCNs = append(postPaymentCNs, cn)
			continue
		}
		setFont(false, 8)
		setColor(0, 128, 80)
		label := cn.Number
		if cn.Reason != "" {
			r := []rune(cn.Reason)
			if len(r) > 20 {
				r = append(r[:17], []rune("...")...)
			}
			label += " - " + string(r)
		}
		textAt(totalsX, y, label)
		rightAlignAt(totalsX+labelW, y, totalsW-labelW, "-"+formatCents(cn.Amount))
		y += 16
	}

	if inv.Status == domain.InvoiceVoided {
		pdf.SetStrokeColor(200, 200, 200)
		pdf.Line(totalsX, y-4, totalsX+totalsW, y-4)
		y += 4
		setFont(true, 12)
		setColor(30, 30, 30)
		textAt(totalsX, y, "Amount Due")
		rightAlignAt(totalsX+labelW, y, totalsW-labelW, formatCents(0))
		y += 20
	} else {
		if inv.CreditsAppliedCents > 0 {
			setFont(false, 9)
			setColor(0, 128, 80)
			textAt(totalsX, y, "Prepaid credits")
			rightAlignAt(totalsX+labelW, y, totalsW-labelW, "-"+formatCents(inv.CreditsAppliedCents))
			y += 16
		}

		if inv.AmountPaidCents > 0 {
			totalsRow("Amount Paid", "-"+formatCents(inv.AmountPaidCents), false, 80, 80, 80, 40, 40, 40)
		}

		pdf.SetStrokeColor(200, 200, 200)
		pdf.Line(totalsX, y-4, totalsX+totalsW, y-4)
		y += 4
		setFont(true, 12)
		setColor(30, 30, 30)
		textAt(totalsX, y, "Amount Due")
		rightAlignAt(totalsX+labelW, y, totalsW-labelW, formatCents(inv.AmountDueCents))
		y += 20

		// Post-payment adjustments. A CN is "completed" when at least
		// one channel has settled — credit-balance and out-of-band
		// are immediate at Issue time; the Stripe refund leg is
		// considered settled only on refund_status=succeeded so a
		// pending/failed refund doesn't render as if it landed.
		var completedCNs []CreditNoteInfo
		for _, cn := range postPaymentCNs {
			switch {
			case cn.CreditAmountCents > 0:
				completedCNs = append(completedCNs, cn)
			case cn.OutOfBandAmountCents > 0:
				completedCNs = append(completedCNs, cn)
			case cn.RefundAmountCents > 0 && cn.RefundStatus == string(domain.RefundSucceeded):
				completedCNs = append(completedCNs, cn)
			}
		}
		if len(completedCNs) > 0 {
			y += 8
			setFont(false, 7)
			setColor(150, 150, 150)
			textAt(margin, y, "POST-PAYMENT ADJUSTMENTS")
			y += 12
			for _, cn := range completedCNs {
				channelDesc := cnChannelDescription(cn, symbol)
				reason := cn.Reason
				if len([]rune(reason)) > 40 {
					reason = string([]rune(reason)[:37]) + "..."
				}
				setFont(false, 8)
				setColor(120, 120, 120)
				textAt(margin, y, fmt.Sprintf("%s - %s", cn.Number, reason))
				rightAlignAt(totalsX+labelW, y, totalsW-labelW, formatCents(cn.Amount))
				y += 11
				// Channel breakdown line — smaller, indented, matches
				// dashboard layout.
				setFont(false, 7)
				setColor(140, 140, 140)
				textAt(margin+8, y, channelDesc)
				y += 10
				// Tax reversal sub-fact — same content as the dashboard
				// row but ASCII-safe (no ↳ — Noto Sans subset doesn't
				// cover the Arrows block).
				if cn.TaxAmountCents > 0 {
					var taxLine string
					if cn.TaxTransactionID != "" {
						taxLine = fmt.Sprintf("Tax reversed %s (Stripe Tax)", formatCents(cn.TaxAmountCents))
					} else {
						taxLine = fmt.Sprintf("Tax: %s (no upstream provider)", formatCents(cn.TaxAmountCents))
					}
					setColor(160, 160, 160)
					textAt(margin+12, y, taxLine)
					y += 10
				}
				y += 2
			}
		}
	}

	// Tax treatment legend. Compliance-sensitive disclosure: reverse-charge
	// invoices must state who accounts for the tax, and exempt invoices
	// must carry the reason text (EU OSS, nonprofit certificate, etc.).
	// The per-line `taxability_reason` data persisted from Stripe Tax
	// (issue #4) lets us distinguish customer-exempt and product-exempt
	// lines from reverse-charge lines and append a separate legend; the
	// reverse-charge wording itself still comes from the calc-level
	// inv.TaxReverseCharge so issue #9's India/EU split is preserved
	// untouched.
	exemption := exemptionLegend(lineItems)
	if inv.TaxReverseCharge || inv.TaxExemptReason != "" || exemption != "" {
		y += 8
		setFont(true, 8)
		setColor(80, 80, 80)
		if inv.TaxReverseCharge {
			var supplier CompanyInfo
			if len(company) > 0 {
				supplier = company[0]
			}
			textAt(margin, y, reverseChargeLegend(inv, supplier, billTo, lineItems))
			y += 12
		}
		if exemption != "" {
			// Small vertical gap above the per-line exemption legend when a
			// reverse-charge legend was just rendered, so the two read as
			// distinct disclosures rather than one wrapped paragraph.
			if inv.TaxReverseCharge {
				y += 4
			}
			setFont(false, 8)
			setColor(100, 100, 100)
			for _, line := range strings.Split(exemption, "\n") {
				textAt(margin, y, line)
				y += 12
			}
		}
		if inv.TaxExemptReason != "" {
			setFont(false, 8)
			setColor(100, 100, 100)
			textAt(margin, y, "Tax-exempt: "+inv.TaxExemptReason)
			y += 12
		}
	}

	// ── Footer ──
	y += 16
	setFont(false, 9)
	setColor(120, 120, 120)
	if inv.Status == domain.InvoiceVoided {
		voidDate := "N/A"
		if inv.VoidedAt != nil {
			voidDate = inv.VoidedAt.In(docLoc).Format("January 2, 2006")
		}
		setColor(200, 80, 80)
		textAt(margin, y, fmt.Sprintf("This invoice was voided on %s. No payment is due.", voidDate))
	} else if inv.PaymentStatus == domain.PaymentSucceeded && inv.PaidAt != nil {
		textAt(margin, y, fmt.Sprintf("Paid on %s - Thank you!", inv.PaidAt.In(docLoc).Format("January 2, 2006")))
	} else {
		textAt(margin, y, fmt.Sprintf("Payment due within %d days of issue date.", inv.NetPaymentTermDays))
	}

	if inv.Memo != "" {
		y += 12
		setFont(false, 9)
		setColor(120, 120, 120)
		textAt(margin, y, inv.Memo)
	}

	y += 24
	setFont(false, 7)
	setColor(170, 170, 170)
	footer := fmt.Sprintf("Generated on %s  |  %s", clock.Now(ctx).Format("Jan 2, 2006 15:04 UTC"), inv.ID)
	fw, _ := pdf.MeasureTextWidth(footer)
	textAt((pageW-fw)/2, y, footer)

	var buf bytes.Buffer
	if _, err := pdf.WriteTo(&buf); err != nil {
		return nil, fmt.Errorf("render pdf: %w", err)
	}
	return buf.Bytes(), nil
}

// formatCityStatePostal joins "City, State Postal" with graceful handling
// when any component is missing. Shared between the company "From" block
// and the "Bill To" block for consistent formatting across the PDF.
func formatCityStatePostal(city, state, postal string) string {
	line := city
	if state != "" {
		if line != "" {
			line += ", "
		}
		line += state
	}
	if postal != "" {
		if line != "" {
			line += " "
		}
		line += postal
	}
	return line
}

// formatTaxRate renders a tax-rate percent with up to 4 decimal places,
// trailing zeros trimmed: 8.8750 → "8.875", 18 → "18", 9.975 → "9.975".
// fmt's %g uses *significant figures*, which silently drops precision on rates
// ≥ 10 (13.875 → "13.88"); the NUMERIC(7,4) tax rate needs fixed-decimal
// formatting so the statutory rate prints verbatim.
func formatTaxRate(rate float64) string {
	s := strconv.FormatFloat(rate, 'f', 4, 64)
	s = strings.TrimRight(s, "0")
	return strings.TrimRight(s, ".")
}

// formatRateIn renders a per-unit price carried as DECIMAL CENTS at full
// precision (e.g. 0.3 cents → "$0.003"), the Stripe unit_amount_decimal model
// — mirrors web-v2 formatRate. Unlike formatCentsIn (whole cents) it never
// collapses a sub-cent rate to "$0.00"; it keeps a minimum of 2 fractional
// digits and trims trailing zeros beyond that. Only the per-unit column uses
// this; line amounts/totals stay whole cents (formatCentsIn). ADR-054.
// The symbol is threaded per render (see RenderPDF) — never global state.
func formatRateIn(symbol string, cents decimal.Decimal) string {
	dollars := cents.Shift(-2) // ÷100 exactly, no rounding
	neg := dollars.Sign() < 0
	abs := dollars.Abs()
	intPart := abs.Truncate(0)
	fracStr := ""
	if frac := abs.Sub(intPart); !frac.IsZero() {
		if fs := frac.String(); strings.ContainsRune(fs, '.') {
			fracStr = strings.TrimRight(strings.SplitN(fs, ".", 2)[1], "0")
		}
	}
	if len(fracStr) < 2 {
		fracStr += strings.Repeat("0", 2-len(fracStr))
	}
	sign := ""
	if neg && !abs.IsZero() {
		sign = "-"
	}
	return fmt.Sprintf("%s%s%s.%s", sign, symbol, formatNumber(intPart.IntPart()), fracStr)
}

func formatCentsIn(symbol string, cents int64) string {
	if cents == 0 {
		return symbol + "0.00"
	}
	sign := ""
	if cents < 0 {
		sign = "-"
		cents = -cents
	}
	dollars := cents / 100
	remainder := cents % 100
	return fmt.Sprintf("%s%s%s.%02d", sign, symbol, formatNumber(dollars), remainder)
}

func formatQuantity(item domain.InvoiceLineItem) string {
	if item.LineType == domain.LineTypeBaseFee {
		return "1"
	}
	// Usage lines may carry a fractional quantity (e.g. 1.5 GPU-hours). Show the
	// exact decimal when set so quantity × unit reconciles to the amount; whole
	// quantities keep thousands separators. Falls back to the integer for rows
	// written before quantity_decimal existed.
	if !item.QuantityDecimal.IsZero() {
		if item.QuantityDecimal.IsInteger() {
			return formatNumber(item.QuantityDecimal.IntPart())
		}
		return item.QuantityDecimal.String()
	}
	return formatNumber(item.Quantity)
}

func formatNumber(n int64) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var result []byte
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			result = append(result, ',')
		}
		result = append(result, byte(c))
	}
	return string(result)
}

type jurisdictionTaxRow struct {
	label  string
	amount int64
}

// aggregateTaxByJurisdiction sums tax by (jurisdiction, rate_bp) across line
// items so the PDF can render a multi-row breakdown when the invoice spans
// more than one jurisdiction. Lines without a jurisdiction or without tax
// are ignored. Results are sorted by label for deterministic output.
func aggregateTaxByJurisdiction(lineItems []domain.InvoiceLineItem) []jurisdictionTaxRow {
	type key struct {
		jurisdiction string
		rate         float64
	}
	agg := make(map[key]int64)
	order := make([]key, 0)
	for _, li := range lineItems {
		if li.TaxAmountCents == 0 || li.TaxJurisdiction == "" {
			continue
		}
		k := key{jurisdiction: li.TaxJurisdiction, rate: li.TaxRate}
		if _, seen := agg[k]; !seen {
			order = append(order, k)
		}
		agg[k] += li.TaxAmountCents
	}
	rows := make([]jurisdictionTaxRow, 0, len(order))
	for _, k := range order {
		label := k.jurisdiction
		if k.rate > 0 {
			label = fmt.Sprintf("%s (%s%%)", label, formatTaxRate(k.rate))
		}
		rows = append(rows, jurisdictionTaxRow{label: label, amount: agg[k]})
	}
	return rows
}
