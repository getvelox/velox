package creditnote

import (
	"context"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// TestRenderPDF_Basic covers the happy path: an issued refund-type credit
// note with taxes and a successful Stripe refund. Asserts the renderer
// produces a valid PDF of reasonable size — mirroring invoice/pdf_test.
func TestRenderPDF_Basic(t *testing.T) {
	t.Parallel()
	issued := time.Date(2026, 4, 15, 0, 0, 0, 0, time.UTC)
	invIssued := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)

	cn := domain.CreditNote{
		ID:                "vlx_cn_test",
		CreditNoteNumber:  "CN-202604-0001",
		InvoiceID:         "vlx_inv_test",
		CustomerID:        "cus_1",
		Status:            domain.CreditNoteIssued,
		Reason:            "Service outage credit for incident 2026-04-10",
		SubtotalCents:     5000,
		TaxAmountCents:    900,
		TotalCents:        5900,
		RefundAmountCents: 5900,
		Currency:          "USD",
		IssuedAt:          &issued,
		RefundStatus:      domain.RefundSucceeded,
		StripeRefundID:    "re_test_1",
	}
	items := []domain.CreditNoteLineItem{
		{Description: "Partial refund — outage credit", Quantity: 1, UnitAmountCents: 5900, AmountCents: 5900},
	}
	orig := OriginalInvoiceInfo{
		Number:     "VLX-202604-0001",
		IssuedAt:   &invIssued,
		Currency:   "USD",
		TaxCountry: "US",
		TaxName:    "Sales Tax",
		TaxRate:    18.00,
	}
	bt := BillToInfo{
		Name:         "Acme Corp",
		Email:        "billing@acme.example",
		AddressLine1: "123 Main St",
		City:         "San Francisco",
		State:        "CA",
		PostalCode:   "94105",
		Country:      "US",
		TaxID:        "US-EIN-12-3456789",
	}
	company := CompanyInfo{
		Name:    "Velox Inc.",
		Email:   "billing@velox.dev",
		TaxID:   "US-EIN-98-7654321",
		Country: "US",
	}

	out, err := RenderPDF(context.Background(), cn, items, orig, bt, company)
	if err != nil {
		t.Fatalf("RenderPDF: %v", err)
	}
	if len(out) < 4 || string(out[:4]) != "%PDF" {
		t.Fatal("output is not a valid PDF (missing %PDF header)")
	}
	if len(out) < 1024 {
		t.Errorf("PDF too small: %d bytes", len(out))
	}
	if len(out) > 1024*1024 {
		t.Errorf("PDF too large: %d bytes", len(out))
	}
	t.Logf("PDF generated: %d bytes", len(out))
}

// TestRenderPDF_ReverseCharge covers the EU B2B / India RCM path where
// tax on the CN is zero but the reverse-charge legend must still appear.
// Regression guard: a CN issued against a reverse-charge invoice that
// silently dropped the legend would leave the buyer's records mismatched
// and fail a VAT audit.
func TestRenderPDF_ReverseCharge(t *testing.T) {
	t.Parallel()
	issued := time.Date(2026, 4, 15, 0, 0, 0, 0, time.UTC)

	cn := domain.CreditNote{
		ID:                "vlx_cn_rc",
		CreditNoteNumber:  "CN-202604-0002",
		InvoiceID:         "vlx_inv_rc",
		CustomerID:        "cus_eu_1",
		Status:            domain.CreditNoteIssued,
		Reason:            "Contract cancellation — EU B2B reverse charge",
		SubtotalCents:     10000,
		TaxAmountCents:    0,
		TotalCents:        10000,
		CreditAmountCents: 10000,
		Currency:          "EUR",
		IssuedAt:          &issued,
	}
	items := []domain.CreditNoteLineItem{
		{Description: "Cancellation credit", Quantity: 1, UnitAmountCents: 10000, AmountCents: 10000},
	}
	orig := OriginalInvoiceInfo{
		Number:        "VLX-202604-0099",
		Currency:      "EUR",
		TaxCountry:    "DE",
		ReverseCharge: true,
	}
	bt := BillToInfo{Name: "Beispiel GmbH", Country: "DE", TaxID: "DE123456789"}
	company := CompanyInfo{Name: "Velox Ltd", Country: "IE", TaxID: "IE1234567T"}

	out, err := RenderPDF(context.Background(), cn, items, orig, bt, company)
	if err != nil {
		t.Fatalf("RenderPDF: %v", err)
	}
	if len(out) < 4 || string(out[:4]) != "%PDF" {
		t.Fatal("output is not a valid PDF")
	}
}

// TestRenderPDF_VoidedWatermark ensures a voided credit note is visibly
// marked so a stale reprint can't be confused for an active document.
func TestRenderPDF_VoidedWatermark(t *testing.T) {
	t.Parallel()
	voided := time.Date(2026, 4, 20, 0, 0, 0, 0, time.UTC)
	cn := domain.CreditNote{
		ID:               "vlx_cn_voided",
		CreditNoteNumber: "CN-202604-0003",
		InvoiceID:        "vlx_inv_v",
		CustomerID:       "cus_2",
		Status:           domain.CreditNoteVoided,
		Reason:           "Issued in error",
		SubtotalCents:    1000,
		TotalCents:       1000,
		Currency:         "USD",
		VoidedAt:         &voided,
	}
	items := []domain.CreditNoteLineItem{
		{Description: "x", Quantity: 1, UnitAmountCents: 1000, AmountCents: 1000},
	}
	out, err := RenderPDF(context.Background(), cn, items, OriginalInvoiceInfo{}, BillToInfo{Name: "cus_2"}, CompanyInfo{})
	if err != nil {
		t.Fatalf("RenderPDF: %v", err)
	}
	if len(out) < 4 || string(out[:4]) != "%PDF" {
		t.Fatal("output is not a valid PDF")
	}
}

// TestSettlementLines documents the mapping from credit-note settlement
// state to the PDF footer copy. The copy must stay honest about pending
// and failed refunds rather than always claiming "refunded".
func TestSettlementLines(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		cn      domain.CreditNote
		want    string
		wantAll int // number of lines expected
	}{
		{
			name:    "succeeded refund",
			cn:      domain.CreditNote{RefundAmountCents: 1000, RefundStatus: domain.RefundSucceeded, Currency: "USD"},
			want:    "Refunded",
			wantAll: 1,
		},
		{
			name:    "pending refund",
			cn:      domain.CreditNote{RefundAmountCents: 1000, RefundStatus: domain.RefundPending, Currency: "USD"},
			want:    "pending",
			wantAll: 1,
		},
		{
			name:    "failed refund",
			cn:      domain.CreditNote{RefundAmountCents: 1000, RefundStatus: domain.RefundFailed, Currency: "USD"},
			want:    "failed",
			wantAll: 2,
		},
		{
			name:    "credit grant",
			cn:      domain.CreditNote{CreditAmountCents: 1000, Currency: "USD"},
			want:    "account credit",
			wantAll: 1,
		},
		{
			name:    "unpaid invoice reduction",
			cn:      domain.CreditNote{TotalCents: 1000, Currency: "USD"},
			want:    "amount due",
			wantAll: 1,
		},
		{
			// An out-of-band-only CN used to fall through to the
			// "applied to reduce the amount due" line — false: OOB means
			// the money was handled entirely outside Velox and the
			// invoice's amount due was never touched (2026-08-04 census).
			name:    "out-of-band only says outside, not amount-due",
			cn:      domain.CreditNote{OutOfBandAmountCents: 1000, TotalCents: 1000, Currency: "USD"},
			want:    "outside this billing system",
			wantAll: 1,
		},
		{
			// A mixed CN used to describe ONLY its card leg (first-match
			// early return); each nonzero channel now gets its own line —
			// and the failed card leg keeps its honest wording beside the
			// settled credit line.
			name:    "mixed failed-card + credit describes both channels",
			cn:      domain.CreditNote{RefundAmountCents: 1000, RefundStatus: domain.RefundFailed, CreditAmountCents: 2000, TotalCents: 3000, Currency: "USD"},
			want:    "account credit",
			wantAll: 3, // failed line + reach-out line + credit line
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := settlementLines(tc.cn, "$")
			if len(got) != tc.wantAll {
				t.Fatalf("lines: got %d, want %d (%v)", len(got), tc.wantAll, got)
			}
			joined := ""
			for _, l := range got {
				joined += l + " "
			}
			if !containsFold(joined, tc.want) {
				t.Errorf("expected %q in output, got %q", tc.want, joined)
			}
		})
	}
}

func containsFold(haystack, needle string) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := 0; j < len(needle); j++ {
			hc := haystack[i+j]
			nc := needle[j]
			if hc >= 'A' && hc <= 'Z' {
				hc += 32
			}
			if nc >= 'A' && nc <= 'Z' {
				nc += 32
			}
			if hc != nc {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
