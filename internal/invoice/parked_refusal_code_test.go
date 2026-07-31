package invoice

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
)

// TestParkedRefusalsAnswerOneCode pins the HTTP half of the single-source rule.
//
// domain.PaymentBlocksAction returns a (code, message) pair so every surface
// refuses a parked invoice identically. Collapsing the seven Go call sites into
// it was only half the job: a handler can still pass the message through and
// drop the code. One did — collect answered 422 validation_error while void,
// send and resend-setup-link answered 409 payment_unidentifiable. Same rule,
// same sentence, and an API consumer could not branch on it uniformly.
//
// A walk of all four refusals found that, not a test, because the domain matrix
// test asserts what the FUNCTION returns and nothing asserted what the HANDLERS
// forward. This is that assertion.
func TestParkedRefusalsAnswerOneCode(t *testing.T) {
	// Parked: unknown payment, no PaymentIntent id — the ADR-107 shape.
	parked := domain.Invoice{
		InvoiceNumber:         "INV-PARKED",
		Status:                domain.InvoiceFinalized,
		PaymentStatus:         domain.PaymentUnknown,
		StripePaymentIntentID: "",
		AmountDueCents:        5000,
		CustomerID:            "cus_1",
		Currency:              "USD",
	}

	cases := []struct {
		name    string
		body    string
		handler func(h *Handler) http.HandlerFunc
	}{
		{"void", "{}", func(h *Handler) http.HandlerFunc { return h.void }},
		{"collect", "{}", func(h *Handler) http.HandlerFunc { return h.collectPayment }},
		// send validates its payload before reaching the gate, so it needs one.
		{"send", `{"email":"a@b.test"}`, func(h *Handler) http.HandlerFunc { return h.sendEmail }},
		{"resend-setup-link", "{}", func(h *Handler) http.HandlerFunc { return h.resendSetupLink }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := newMemStore()
			inv, err := store.Create(ctx, "t1", parked)
			if err != nil {
				t.Fatalf("seed invoice: %v", err)
			}

			h := &Handler{
				svc:           NewService(store, nil, nil),
				charger:       &fakeCharger{},
				paymentSetups: &fakePaymentSetups{setup: domain.CustomerPaymentSetup{SetupStatus: domain.PaymentSetupReady, StripeCustomerID: "cus_stripe_1"}},
				auditLogger:   &capturingInvoiceAudit{},
			}

			req := httptest.NewRequest(http.MethodPost, "/v1/invoices/"+inv.ID+"/x", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			rctx := chi.NewRouteContext()
			rctx.URLParams.Add("id", inv.ID)
			reqCtx := context.WithValue(req.Context(), auth.TestTenantIDKey(), "t1")
			reqCtx = context.WithValue(reqCtx, chi.RouteCtxKey, rctx)
			req = req.WithContext(reqCtx)
			rr := httptest.NewRecorder()

			tc.handler(h)(rr, req)

			if rr.Code != http.StatusConflict {
				t.Fatalf("status: got %d, want 409 — every parked refusal is the same conflict. body=%s", rr.Code, rr.Body.String())
			}

			var envelope struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rr.Body.Bytes(), &envelope); err != nil {
				t.Fatalf("decode body %q: %v", rr.Body.String(), err)
			}
			if envelope.Error.Code != "payment_unidentifiable" {
				t.Errorf("code: got %q, want payment_unidentifiable — a consumer branches on this, so one handler answering differently makes the single source useless over HTTP", envelope.Error.Code)
			}
			// The refusal must still name the way out; a parked invoice has
			// exactly one, and an operator who is not told it is stuck.
			if !strings.Contains(envelope.Error.Message, "uncollectible") {
				t.Errorf("message names no way out: %q", envelope.Error.Message)
			}
		})
	}
}

// TestParkedEmailRefusalDescribesTheRightEmail guards a copy split that a walk
// forced. email-invoice and resend-setup-link shared one sentence — "that email
// tells the customer we will collect automatically" — which is true of the
// setup link and false of the invoice email, whose call to action is "View &
// pay invoice". Wrong-email copy on a money surface is the same defect class
// this arc exists to remove, so the distinction is asserted rather than trusted.
func TestParkedEmailRefusalDescribesTheRightEmail(t *testing.T) {
	parked := domain.Invoice{PaymentStatus: domain.PaymentUnknown}

	invoiceMsg := domain.PaymentBlocksAction(parked, domain.ActionEmailInvoice).Message
	setupMsg := domain.PaymentBlocksAction(parked, domain.ActionResendSetupLink).Message

	if invoiceMsg == setupMsg {
		t.Fatal("the invoice email and the setup link get the same refusal — they are different emails making different promises, and one sentence cannot be true of both")
	}
	if strings.Contains(invoiceMsg, "collect automatically") {
		t.Errorf("the invoice-email refusal describes the SETUP LINK's promise; that email asks the customer to pay, it does not promise automatic collection: %q", invoiceMsg)
	}
	if !strings.Contains(setupMsg, "collect automatically") {
		t.Errorf("the setup-link refusal lost the reason it is refused — that email promises collection the engine will never perform: %q", setupMsg)
	}
}
