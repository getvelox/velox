package invoice

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
)

// TestPaymentTimeline_RefundRowReportsOutcomeNotIntent locks the 2026-08-03
// FLOW C2 finding: the credit-note row was labelled from RefundAmountCents
// alone — what was ALLOCATED to the card — and never consulted RefundStatus,
// whether that money actually moved.
//
// Observed on CN-000027: refund_status=failed, stripe_refund_id empty, nothing
// left the Stripe balance, and the invoice timeline said "Refund issued ·
// $10.00". That is the surface an operator opens to ask what happened to an
// invoice, and it asserted the customer had been repaid when they had not.
// This is NOT specific to the offline-payment case that surfaced it: any card
// refund that fails at Stripe (declined, expired card, network) renders the
// same way, which is an ordinary production path.
//
// Every case below is one status against otherwise IDENTICAL rows, because a
// single-status test passes just as well against the old allocation-only
// labelling.
func TestPaymentTimeline_RefundRowReportsOutcomeNotIntent(t *testing.T) {
	t.Parallel()

	issuedAt := time.Date(2026, 8, 2, 15, 24, 0, 0, time.UTC)

	seed := func(t *testing.T, cn domain.CreditNote) timelineResp {
		t.Helper()
		store := newMemStore()
		inv, err := store.Create(context.Background(), "t1", domain.Invoice{
			CustomerID: "cus_1", Status: domain.InvoicePaid,
			PaymentStatus: domain.PaymentSucceeded, AmountPaidCents: 5362,
			StripePaymentIntentID: "pi_card",
		})
		if err != nil {
			t.Fatalf("seed invoice: %v", err)
		}
		cn.InvoiceID = inv.ID
		cn.TenantID = "t1"
		cn.Status = domain.CreditNoteIssued
		cn.IssuedAt = &issuedAt
		h := &Handler{svc: NewService(store, nil, nil)}
		h.creditNotes = &cnLaneFake{cns: []domain.CreditNote{cn}}

		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", inv.ID)
		ctx := auth.WithTenantID(req.Context(), "t1")
		ctx = context.WithValue(ctx, chi.RouteCtxKey, rctx)
		rr := httptest.NewRecorder()
		h.paymentTimeline(rr, req.WithContext(ctx))
		if rr.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
		}
		var resp timelineResp
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return resp
	}

	rowFor := func(t *testing.T, resp timelineResp, wantDetail string) (string, string) {
		t.Helper()
		for _, e := range resp.Events {
			d, _ := e["detail"].(string)
			if d == wantDetail || (d != "" && len(d) >= len(wantDetail) && d[:len(wantDetail)] == wantDetail) {
				desc, _ := e["description"].(string)
				status, _ := e["status"].(string)
				return desc, status
			}
		}
		t.Fatalf("no timeline row for %q; events=%v", wantDetail, resp.Events)
		return "", ""
	}
	descFor := func(t *testing.T, resp timelineResp, wantDetail string) string {
		t.Helper()
		desc, _ := rowFor(t, resp, wantDetail)
		return desc
	}

	// Full-amount refund: the label is the whole story of the row.
	t.Run("full refund", func(t *testing.T) {
		cases := []struct {
			status     domain.RefundStatus
			want       string
			wantStatus string
		}{
			{domain.RefundSucceeded, "Refund issued", "succeeded"},
			{domain.RefundFailed, "Refund failed", "failed"},
			{domain.RefundPending, "Refund pending", "pending"},
			// Allocated but never executed. Must not borrow the success
			// wording merely because it is not an explicit failure.
			{domain.RefundNone, "Refund not processed", "pending"},
		}
		for _, tc := range cases {
			t.Run(string(tc.status), func(t *testing.T) {
				resp := seed(t, domain.CreditNote{
					CreditNoteNumber: "CN-FULL", TotalCents: 1000,
					RefundAmountCents: 1000, RefundStatus: tc.status,
				})
				got, gotStatus := rowFor(t, resp, "CN-FULL")
				// The status field drives the UI's verdict dot. It was
				// hardcoded "succeeded" for every CN row, so a failed
				// refund wore an emerald success dot beside the words
				// "Refund failed" — the visual channel contradicting the
				// text channel this test already pinned (2026-08-04
				// invoice census). Both channels are asserted together
				// now so they can never diverge again.
				if gotStatus != tc.wantStatus {
					t.Errorf("refund_status=%q: timeline status %q, want %q — the verdict dot must agree with the words", tc.status, gotStatus, tc.wantStatus)
				}
				if got != tc.want {
					t.Errorf("refund_status=%q rendered %q, want %q", tc.status, got, tc.want)
				}
			})
		}
	})

	// Partial refund: the row also carries credit/out-of-band, so the refund
	// leg is a clause rather than the headline — but it must still be honest.
	t.Run("partial refund", func(t *testing.T) {
		cases := []struct {
			status domain.RefundStatus
			want   string
		}{
			{domain.RefundSucceeded, "Credit note issued — part refunded to card"},
			{domain.RefundFailed, "Credit note issued — card refund failed"},
			{domain.RefundPending, "Credit note issued — card refund pending"},
			{domain.RefundNone, "Credit note issued — card refund not processed"},
		}
		for _, tc := range cases {
			t.Run(string(tc.status), func(t *testing.T) {
				resp := seed(t, domain.CreditNote{
					CreditNoteNumber: "CN-PART", TotalCents: 8000,
					RefundAmountCents: 1000, CreditAmountCents: 7000, RefundStatus: tc.status,
				})
				got := descFor(t, resp, "CN-PART")
				if got != tc.want {
					t.Errorf("refund_status=%q rendered %q, want %q", tc.status, got, tc.want)
				}
			})
		}
	})

	// A note with no card leg at all is unaffected by refund status.
	t.Run("no refund leg keeps the plain label", func(t *testing.T) {
		resp := seed(t, domain.CreditNote{
			CreditNoteNumber: "CN-CREDIT", TotalCents: 1000,
			CreditAmountCents: 1000, RefundStatus: domain.RefundNone,
		})
		if got := descFor(t, resp, "CN-CREDIT"); got != "Credit note issued" {
			t.Errorf("credit-only note rendered %q, want %q", got, "Credit note issued")
		}
	})
}
