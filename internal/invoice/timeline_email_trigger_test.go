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

type emailRowsFake struct{ rows []EmailEventRow }

func (f *emailRowsFake) ListByInvoice(context.Context, string, string) ([]EmailEventRow, error) {
	return f.rows, nil
}

// TestPaymentTimeline_SetupLinkRowsCarryTheirCause pins the WIRE, not just
// the helper: two setup-link email rows on one invoice must reach the
// timeline with distinct cause sublines. The helper alone passing is not
// enough — the first mutation check on this fix cut the render-site call
// (Detail: "") and every existing test stayed green, which is exactly the
// silent regression this test exists to catch.
func TestPaymentTimeline_SetupLinkRowsCarryTheirCause(t *testing.T) {
	store := newMemStore()
	inv, err := store.Create(context.Background(), "t1", domain.Invoice{
		CustomerID: "cus_1", Status: domain.InvoiceFinalized,
		PaymentStatus: domain.PaymentPending, AmountDueCents: 5000,
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	at := time.Now().UTC()
	h := &Handler{
		svc: NewService(store, nil, nil),
		emailEvents: &emailRowsFake{rows: []EmailEventRow{
			{ID: "em1", EmailType: "payment_setup_request", Status: "dispatched", CreatedAt: at, Trigger: "finalize_no_pm"},
			{ID: "em2", EmailType: "payment_setup_request", Status: "skipped", CreatedAt: at.Add(time.Minute), Trigger: "operator_resend"},
		}},
	}

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
	var resp struct {
		Events []map[string]any `json:"events"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	details := map[string]string{}
	for _, e := range resp.Events {
		if e["event_type"] == "email.payment_setup_request" {
			d, _ := e["detail"].(string)
			id, _ := e["id"].(string)
			details[id] = d
		}
	}
	if len(details) != 2 {
		t.Fatalf("want 2 setup-link rows on the timeline, got %d (%v)", len(details), details)
	}
	if details["email:em1"] != "Sent automatically — no payment method on file at finalize" {
		t.Errorf("finalize row detail = %q", details["email:em1"])
	}
	if details["email:em2"] != "Resent by an operator" {
		t.Errorf("resend row detail = %q", details["email:em2"])
	}
	if details["email:em1"] == details["email:em2"] {
		t.Fatal("the two rows are indistinguishable again — the fix has regressed")
	}
}
