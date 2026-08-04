package creditnote

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/signintech/gopdf"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/pdffonts"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
)

// CompanyInfo holds the seller's registered-business details printed at
// the top-left of the credit note. Mirrors the invoice PDF's shape so the
// handler can populate both from a single settings lookup.
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
	// TaxID is the seller's VAT/GSTIN/EIN/ABN, required on legally
	// compliant credit notes (EU VAT Directive, UK VATA, India GST).
	TaxID string
}

// BillToInfo holds the buyer's address and tax id. Required on EU VAT
// credit notes, India GST credit notes, and strongly recommended
// everywhere else — the document reduces the buyer's deductible tax, so
// the buyer's identity must be unambiguous.
type BillToInfo struct {
	Name         string
	Email        string
	AddressLine1 string
	AddressLine2 string
	City         string
	State        string
	PostalCode   string
	Country      string
	// TaxID is the buyer's VAT/GSTIN registration. Printed under the
	// Bill-To block when present.
	TaxID string
}

// OriginalInvoiceInfo holds the minimal snapshot of the invoice this
// credit note references. A CN is always issued against a specific
// invoice, and the invoice number + date must appear on the CN to
// satisfy reverse-lookup audits (EU VAT Implementing Regulation Art. 226,
// India CGST Rule 53, etc.).
type OriginalInvoiceInfo struct {
	Number   string
	IssuedAt *time.Time
	Currency string
	// BillingTimezone is the original invoice's billing-calendar timezone
	// (ADR-077). The CN's Issued/Invoice-Date/Voided civil dates render in this
	// zone (falling back to UTC when empty), so a non-UTC deployment doesn't
	// print a day-shifted date on a customer-retained credit note (ADR-075).
	BillingTimezone string
	// TaxCountry is the ISO-3166 code the invoice's tax was reported
	// under. Used for the tax-row label when the CN doesn't carry a
	// per-line breakdown of its own.
	TaxCountry string
	// TaxName is the label the invoice used for its tax row (VAT, GST,
	// Sales Tax, ...). Preserved on the CN so the two documents read
	// as a pair.
	TaxName string
	// TaxRate is the invoice's aggregate tax rate in percent (4-decimal
	// precision). Shown parenthetically next to the tax label ("VAT (20%)").
	// ADR-042/043.
	TaxRate float64
	// ReverseCharge / ExemptReason surface the invoice's tax treatment
	// so the CN carries the same compliance legend. A CN issued
	// against a reverse-charge invoice must itself display the
	// reverse-charge note; otherwise the buyer's records will mismatch.
	ReverseCharge bool
	ExemptReason  string
}

// Currency symbol map. Kept in sync with the invoice PDF's map.
var cnCurrencySymbols = map[string]string{
	"USD": "$", "EUR": "€", "GBP": "£", "JPY": "¥", "CNY": "¥",
	"INR": "₹", "BRL": "R$", "CAD": "CA$", "AUD": "A$", "CHF": "CHF ",
	"SGD": "S$", "HKD": "HK$", "NZD": "NZ$", "MXN": "MX$",
	"KRW": "₩", "ZAR": "R", "PLN": "zł", "AED": "AED ", "SAR": "SAR ",
	"THB": "฿", "MYR": "RM ", "IDR": "Rp ", "PHP": "₱", "VND": "₫",
}

// RenderPDF renders a standalone credit note PDF. The caller supplies the
// credit note header, its line items, the original invoice it references,
// a BillTo snapshot for the buyer, and a CompanyInfo snapshot for the
// seller. Returns the PDF bytes; no filesystem I/O.
//
// Layout mirrors the invoice PDF intentionally — customers receive the
// pair together and the visual parallel makes the relationship obvious.
// Key differences: "CREDIT NOTE" label, an "Original Invoice" reference
// block in place of the period, a "Reason" block above the line items,
// and a refund-destination footer instead of an "amount due" row.
func RenderPDF(
	ctx context.Context,
	cn domain.CreditNote,
	lineItems []domain.CreditNoteLineItem,
	orig OriginalInvoiceInfo,
	billTo BillToInfo,
	company CompanyInfo,
) ([]byte, error) {
	// Resolve the currency symbol once and thread it through the formatters.
	// Keeping this call-scoped (vs a package global) means concurrent
	// RenderPDF calls with different currencies don't stomp on each other.
	currency := cn.Currency
	if currency == "" {
		currency = orig.Currency
	}
	var symbol string
	if sym, ok := cnCurrencySymbols[strings.ToUpper(currency)]; ok {
		symbol = sym
	} else if currency != "" {
		symbol = currency + " "
	} else {
		symbol = "$"
	}

	// Civil dates render in the original invoice's billing timezone (ADR-077),
	// UTC when unknown — never the raw process zone (ADR-075 audit).
	docLoc := domain.LoadLocationOrUTC(orig.BillingTimezone)

	pdf := &gopdf.GoPdf{}
	pdf.Start(gopdf.Config{PageSize: *gopdf.PageSizeA4})
	pdf.AddPage()

	if err := pdffonts.RegisterNotoSans(pdf); err != nil {
		return nil, err
	}

	setFont := func(bold bool, size int) {
		name := pdffonts.FamilyRegular
		if bold {
			name = pdffonts.FamilyBold
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
	if cn.Status == domain.CreditNoteVoided {
		setFont(true, 72)
		setColor(230, 230, 230)
		textAt(55, 400, "VOID")
	}

	pageW := 595.28 // A4 width in points
	margin := 40.0
	contentW := pageW - margin*2
	y := 40.0

	// ── Header: company (left), "CREDIT NOTE" label (right) ──
	companyName := "Velox"
	var companyAddrLines []string
	companyContact := ""
	if company.Name != "" {
		companyName = company.Name
		if company.AddressLine1 != "" {
			companyAddrLines = append(companyAddrLines, company.AddressLine1)
		}
		if company.AddressLine2 != "" {
			companyAddrLines = append(companyAddrLines, company.AddressLine2)
		}
		if cityLine := cnFormatCityStatePostal(company.City, company.State, company.PostalCode); cityLine != "" {
			companyAddrLines = append(companyAddrLines, cityLine)
		}
		if company.Country != "" {
			companyAddrLines = append(companyAddrLines, company.Country)
		}
		parts := []string{}
		if company.Email != "" {
			parts = append(parts, company.Email)
		}
		if company.Phone != "" {
			parts = append(parts, company.Phone)
		}
		if len(parts) > 0 {
			companyContact = strings.Join(parts, "  |  ")
		}
	}

	setFont(true, 18)
	setColor(30, 30, 30)
	textAt(margin, y, companyName)

	setFont(true, 18)
	setColor(100, 100, 100)
	rightAlignAt(margin, y, contentW, "CREDIT NOTE")
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
	if company.TaxID != "" {
		setFont(false, 8)
		setColor(120, 120, 120)
		textAt(margin, y, "Tax ID: "+company.TaxID)
		y += 11
	}

	y += 16

	// ── Credit Note Details (left) + Bill-To (right) ──
	detailStartY := y

	setFont(true, 9)
	setColor(100, 100, 100)
	textAt(margin, y, "CREDIT NOTE DETAILS")
	y += 14

	detailRow := func(label, value string) {
		setFont(false, 9)
		setColor(100, 100, 100)
		textAt(margin, y, label)
		setColor(40, 40, 40)
		textAt(margin+100, y, value)
		y += 14
	}

	detailRow("Number", cn.CreditNoteNumber)
	if cn.IssuedAt != nil {
		detailRow("Issued", cn.IssuedAt.In(docLoc).Format("January 2, 2006"))
	} else {
		detailRow("Status", string(cn.Status))
	}
	if orig.Number != "" {
		detailRow("Original Invoice", orig.Number)
	}
	if orig.IssuedAt != nil {
		detailRow("Invoice Date", orig.IssuedAt.In(docLoc).Format("January 2, 2006"))
	}
	detailRow("Currency", strings.ToUpper(currency))

	leftBottom := y

	// Bill-To (right column)
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
	cityLine := cnFormatCityStatePostal(billTo.City, billTo.State, billTo.PostalCode)
	if cityLine != "" {
		textAt(rightX, by, cityLine)
		by += 12
	}
	if billTo.Country != "" {
		textAt(rightX, by, billTo.Country)
		by += 12
	}
	if billTo.Email != "" {
		setFont(false, 8)
		setColor(100, 100, 100)
		textAt(rightX, by, billTo.Email)
		by += 12
	}
	if billTo.TaxID != "" {
		setFont(false, 8)
		setColor(100, 100, 100)
		textAt(rightX, by, "Tax ID: "+billTo.TaxID)
		by += 12
	}

	if by > leftBottom {
		y = by
	} else {
		y = leftBottom
	}
	y += 16

	// ── Reason block ──
	// Credit notes carry a mandatory reason on every jurisdiction's compliance
	// checklist (EU VAT Implementing Regulation Art. 226(a), India CGST Rule
	// 53(1A)(d)). Printed prominently above the line items so the customer and
	// auditor both see it without scrolling.
	if strings.TrimSpace(cn.Reason) != "" {
		setFont(true, 9)
		setColor(100, 100, 100)
		textAt(margin, y, "REASON FOR CREDIT")
		y += 14
		setFont(false, 10)
		setColor(40, 40, 40)
		reasonLines := wrapText(cn.Reason, 90)
		for _, line := range reasonLines {
			textAt(margin, y, line)
			y += 14
		}
		y += 6
	}

	// ── Line items table ──
	colX := []float64{margin, margin + 240, margin + 320, margin + 400}
	colEnd := margin + contentW

	setFont(true, 9)
	setColor(80, 80, 80)
	pdf.SetFillColor(245, 245, 245)
	pdf.RectFromUpperLeftWithStyle(margin, y-2, contentW, 20, "F")

	textAt(colX[0], y, "Description")
	rightAlignAt(colX[1], y, colX[2]-colX[1], "Qty")
	rightAlignAt(colX[2], y, colX[3]-colX[2], "Unit Price")
	rightAlignAt(colX[3], y, colEnd-colX[3], "Amount")
	y += 22

	setFont(false, 9)
	setColor(40, 40, 40)
	for _, item := range lineItems {
		desc := item.Description
		if len([]rune(desc)) > 50 {
			desc = string([]rune(desc)[:47]) + "..."
		}
		textAt(colX[0], y, desc)
		rightAlignAt(colX[1], y, colX[2]-colX[1], cnFormatNumber(item.Quantity))
		rightAlignAt(colX[2], y, colX[3]-colX[2], cnFormatCents(item.UnitAmountCents, symbol))
		rightAlignAt(colX[3], y, colEnd-colX[3], cnFormatCents(item.AmountCents, symbol))
		y += 18
	}

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

	// Line amounts above are GROSS (tax-inclusive — ADR-048 anchors the
	// reversal on the gross the customer paid), so they sum to Credit Total,
	// not to this net figure. "Subtotal" implied sum-of-lines and made the
	// document look like it didn't add up ($110.00 line vs $100.00
	// "Subtotal"); label the net row for what it is instead — and only
	// render it when there IS tax to exclude (or a reverse-charge row to
	// explain). On a zero-tax CN net == gross, and printing the same figure
	// twice ("Total excluding tax" then "Credit Total") reads as a bug.
	if cn.TaxAmountCents > 0 || orig.ReverseCharge {
		totalsRow("Total excluding tax", cnFormatCents(cn.SubtotalCents, symbol), false, 80, 80, 80, 40, 40, 40)
	}

	if cn.TaxAmountCents > 0 {
		taxLabel := "Tax"
		if orig.TaxName != "" {
			taxLabel = orig.TaxName
		}
		if orig.TaxRate > 0 {
			taxLabel = fmt.Sprintf("%s (%s%%)", taxLabel, cnFormatTaxRate(orig.TaxRate))
		}
		if orig.TaxCountry != "" {
			taxLabel = fmt.Sprintf("%s [%s]", taxLabel, orig.TaxCountry)
		}
		totalsRow(taxLabel, cnFormatCents(cn.TaxAmountCents, symbol), false, 80, 80, 80, 40, 40, 40)
	} else if orig.ReverseCharge {
		// Even with zero-amount tax, surface the reverse-charge row so
		// the CN reads as a deliberate treatment, not a bug.
		totalsRow("Tax (reverse charge)", cnFormatCents(0, symbol), false, 80, 80, 80, 120, 120, 120)
	}

	pdf.SetStrokeColor(220, 220, 220)
	pdf.Line(totalsX, y-4, totalsX+totalsW, y-4)
	y += 2
	totalsRow("Credit Total", cnFormatCents(cn.TotalCents, symbol), true, 30, 30, 30, 30, 30, 30)

	// ── Refund destination ──
	// Tells the reader where the money went. Three cases map to the three
	// ways Velox settles a CN (see service.go Issue()):
	//   - refund_amount > 0:  Stripe refund back to the original payment
	//   - credit_amount > 0:  Added to the customer's prepaid balance
	//   - both zero:          Applied to an unpaid invoice's amount_due
	y += 8
	setFont(true, 9)
	setColor(100, 100, 100)
	textAt(margin, y, "SETTLEMENT")
	y += 14
	setFont(false, 9)
	setColor(60, 60, 60)
	for _, line := range settlementLines(cn, symbol) {
		textAt(margin, y, line)
		y += 12
	}

	// ── Compliance legend ──
	// Mirrors the invoice's legend so the two documents carry matching
	// disclosures. Required for reverse-charge and exempt treatments; any
	// mismatch between the invoice and its credit note flags a compliance
	// error on the buyer's side.
	if orig.ReverseCharge || orig.ExemptReason != "" {
		y += 8
		setFont(true, 8)
		setColor(80, 80, 80)
		if orig.ReverseCharge {
			textAt(margin, y, "Reverse charge — VAT to be accounted for by the recipient.")
			y += 12
		}
		if orig.ExemptReason != "" {
			setFont(false, 8)
			setColor(100, 100, 100)
			textAt(margin, y, "Tax-exempt: "+orig.ExemptReason)
			y += 12
		}
	}

	// ── Footer ──
	y += 16
	setFont(false, 9)
	setColor(120, 120, 120)
	switch cn.Status {
	case domain.CreditNoteVoided:
		voidDate := "N/A"
		if cn.VoidedAt != nil {
			voidDate = cn.VoidedAt.In(docLoc).Format("January 2, 2006")
		}
		setColor(200, 80, 80)
		textAt(margin, y, fmt.Sprintf("This credit note was voided on %s.", voidDate))
	case domain.CreditNoteDraft:
		setColor(180, 120, 0)
		textAt(margin, y, "DRAFT — not yet issued.")
	default:
		textAt(margin, y, "This credit note reduces the taxable supply originally invoiced above. Retain for your records.")
	}

	y += 24
	setFont(false, 7)
	setColor(170, 170, 170)
	footer := fmt.Sprintf("Generated on %s  |  %s", clock.Now(ctx).Format("Jan 2, 2006 15:04 UTC"), cn.ID)
	fw, _ := pdf.MeasureTextWidth(footer)
	textAt((pageW-fw)/2, y, footer)

	var buf bytes.Buffer
	if _, err := pdf.WriteTo(&buf); err != nil {
		return nil, fmt.Errorf("render pdf: %w", err)
	}
	return buf.Bytes(), nil
}

// settlementLines describes where the credited amount went, one line per
// NONZERO channel — composed, not first-match: the old early-return shape
// made a mixed CN describe only its card leg (the credit/out-of-band money
// silently unexplained), and an out-of-band-only CN fell through to the
// "applied to reduce the amount due" fallback — false, since out-of-band
// means the money was handled entirely outside Velox and the invoice's
// amount due was never touched. The fallback is now reachable only by a
// true adjustment CN (all three channels zero). Refund copy stays explicit
// about status so a failed leg never reads as settled.
func settlementLines(cn domain.CreditNote, symbol string) []string {
	var lines []string
	if cn.RefundAmountCents > 0 {
		switch cn.RefundStatus {
		case domain.RefundSucceeded:
			lines = append(lines, fmt.Sprintf("Refunded %s to the original payment method.", cnFormatCents(cn.RefundAmountCents, symbol)))
		case domain.RefundPending:
			lines = append(lines, fmt.Sprintf("%s refund is pending — please allow up to 10 business days.", cnFormatCents(cn.RefundAmountCents, symbol)))
		case domain.RefundFailed:
			lines = append(lines,
				fmt.Sprintf("%s refund attempt failed.", cnFormatCents(cn.RefundAmountCents, symbol)),
				"Our team will reach out to resolve the refund manually.")
		default:
			lines = append(lines, fmt.Sprintf("Refund of %s scheduled to the original payment method.", cnFormatCents(cn.RefundAmountCents, symbol)))
		}
	}
	if cn.CreditAmountCents > 0 {
		lines = append(lines, fmt.Sprintf("%s added to your account credit balance. Applied automatically to future invoices.", cnFormatCents(cn.CreditAmountCents, symbol)))
	}
	if cn.OutOfBandAmountCents > 0 {
		lines = append(lines, fmt.Sprintf("%s settled outside this billing system (recorded for your records).", cnFormatCents(cn.OutOfBandAmountCents, symbol)))
	}
	if len(lines) > 0 {
		return lines
	}
	return []string{fmt.Sprintf("%s applied to reduce the amount due on the original invoice.", cnFormatCents(cn.TotalCents, symbol))}
}

// cnFormatCityStatePostal joins "City, State Postal" gracefully when any
// component is missing.
func cnFormatCityStatePostal(city, state, postal string) string {
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

// cnFormatTaxRate renders a tax-rate percent with up to 4 decimal places,
// trailing zeros trimmed (8.8750 → "8.875"). Mirrors the invoice PDF's
// formatTaxRate — %g uses significant figures and drops precision on rates
// ≥ 10, so fixed-decimal formatting prints the statutory rate verbatim.
func cnFormatTaxRate(rate float64) string {
	s := strconv.FormatFloat(rate, 'f', 4, 64)
	s = strings.TrimRight(s, "0")
	return strings.TrimRight(s, ".")
}

func cnFormatCents(cents int64, symbol string) string {
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
	return fmt.Sprintf("%s%s%s.%02d", sign, symbol, cnFormatNumber(dollars), remainder)
}

func cnFormatNumber(n int64) string {
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

// wrapText splits a string into lines of at most maxChars runes, breaking
// on whitespace when possible. Used for the reason block so long
// free-text explanations don't run off the page.
func wrapText(text string, maxChars int) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	words := strings.Fields(text)
	if len(words) == 0 {
		return nil
	}
	var lines []string
	current := words[0]
	for _, w := range words[1:] {
		if len([]rune(current))+1+len([]rune(w)) > maxChars {
			lines = append(lines, current)
			current = w
			continue
		}
		current += " " + w
	}
	lines = append(lines, current)
	return lines
}
