package invoice

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
)

// Lane fakes: each side-source either errors or returns canned rows.

type cnLaneFake struct {
	err error
	cns []domain.CreditNote
}

func (f *cnLaneFake) List(context.Context, string, string) ([]domain.CreditNote, error) {
	return f.cns, f.err
}

type webhookLaneFake struct{ err error }

func (f *webhookLaneFake) ListByInvoice(context.Context, string, string) ([]domain.StripeWebhookEvent, error) {
	return nil, f.err
}

type emailLaneFake struct{ err error }

func (f *emailLaneFake) ListByInvoice(context.Context, string, string) ([]EmailEventRow, error) {
	return nil, f.err
}

type dunningLaneFake struct {
	runsErr   error
	eventsErr error
	runs      []domain.InvoiceDunningRun
}

func (f *dunningLaneFake) ListRunsByInvoice(context.Context, string, string) ([]domain.InvoiceDunningRun, error) {
	return f.runs, f.runsErr
}

func (f *dunningLaneFake) ListEvents(context.Context, string, string) ([]domain.InvoiceDunningEvent, error) {
	return nil, f.eventsErr
}

type timelineResp struct {
	Events    []map[string]any `json:"events"`
	Degraded  []string         `json:"degraded"`
	Truncated bool             `json:"truncated"`
}

// TestPaymentTimeline_DegradationDisclosure locks the 2026-07-19 audit
// finding 4: side-lane fetch failures were swallowed (`if err == nil`) —
// a timeline missing its dunning rows was indistinguishable from an
// invoice that never saw dunning, and the credit-note lane's store cap
// truncated silently. Lane failures now degrade loudly (`degraded` names
// the lanes, the lifecycle core still renders) and the CN cap discloses
// via `truncated`.
func TestPaymentTimeline_DegradationDisclosure(t *testing.T) {
	seed := func(t *testing.T) (*Handler, domain.Invoice) {
		t.Helper()
		store := newMemStore()
		inv, err := store.Create(context.Background(), "t1", domain.Invoice{
			CustomerID: "cus_1", Status: domain.InvoiceFinalized,
			PaymentStatus: domain.PaymentPending, AmountDueCents: 5000,
		})
		if err != nil {
			t.Fatalf("seed invoice: %v", err)
		}
		return &Handler{svc: NewService(store, nil, nil)}, inv
	}

	fetch := func(t *testing.T, h *Handler, invID string) timelineResp {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", invID)
		ctx := auth.WithTenantID(req.Context(), "t1")
		ctx = context.WithValue(ctx, chi.RouteCtxKey, rctx)
		rr := httptest.NewRecorder()
		h.paymentTimeline(rr, req.WithContext(ctx))
		if rr.Code != http.StatusOK {
			t.Fatalf("status: got %d, body=%s", rr.Code, rr.Body.String())
		}
		var resp timelineResp
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v; body=%s", err, rr.Body.String())
		}
		return resp
	}

	t.Run("every failing lane is named; the lifecycle core still renders", func(t *testing.T) {
		h, inv := seed(t)
		boom := fmt.Errorf("lane down")
		h.creditNotes = &cnLaneFake{err: boom}
		h.emailEvents = &emailLaneFake{err: boom}
		h.webhookEvents = &webhookLaneFake{err: boom}
		h.dunningTimeline = &dunningLaneFake{runsErr: boom}

		resp := fetch(t, h, inv.ID)
		// "stripe" is no longer a lane: webhook rows stopped being a
		// display source at ADR-103 (payments render from charge attempts).
		want := []string{"credit_notes", "emails", "dunning"}
		if len(resp.Degraded) != len(want) {
			t.Fatalf("degraded: got %v, want %v", resp.Degraded, want)
		}
		got := map[string]bool{}
		for _, l := range resp.Degraded {
			got[l] = true
		}
		for _, l := range want {
			if !got[l] {
				t.Errorf("lane %q failed but is not disclosed", l)
			}
		}
		if len(resp.Events) == 0 {
			t.Error("lifecycle rows must survive side-lane failures — got an empty timeline")
		}
	})

	t.Run("a single run's events failing degrades the dunning lane once", func(t *testing.T) {
		h, inv := seed(t)
		h.dunningTimeline = &dunningLaneFake{
			runs:      []domain.InvoiceDunningRun{{ID: "run_1"}, {ID: "run_2"}},
			eventsErr: fmt.Errorf("events down"),
		}
		resp := fetch(t, h, inv.ID)
		if len(resp.Degraded) != 1 || resp.Degraded[0] != "dunning" {
			t.Errorf("degraded: got %v, want exactly [dunning] (disclosed once, not per run)", resp.Degraded)
		}
	})

	t.Run("unwired lanes are a deployment shape, not degradation", func(t *testing.T) {
		h, inv := seed(t)
		resp := fetch(t, h, inv.ID)
		if len(resp.Degraded) != 0 {
			t.Errorf("nil listers must not read as failures: %v", resp.Degraded)
		}
		if resp.Degraded == nil {
			t.Error("degraded must serialize as [], not null")
		}
		if resp.Truncated {
			t.Error("truncated without a CN fetch at cap")
		}
	})

	t.Run("a CN fetch at the cap discloses truncation; below it does not", func(t *testing.T) {
		issuedAt := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
		mkCNs := func(n int) []domain.CreditNote {
			out := make([]domain.CreditNote, n)
			for i := range out {
				out[i] = domain.CreditNote{
					Status: domain.CreditNoteIssued, IssuedAt: &issuedAt,
					CreditNoteNumber: fmt.Sprintf("CN-%03d", i),
				}
			}
			return out
		}
		h, inv := seed(t)
		h.creditNotes = &cnLaneFake{cns: mkCNs(CreditNoteLaneCap)}
		if resp := fetch(t, h, inv.ID); !resp.Truncated {
			t.Error("CN lane at cap: truncated must be true")
		}
		h2, inv2 := seed(t)
		h2.creditNotes = &cnLaneFake{cns: mkCNs(CreditNoteLaneCap - 1)}
		if resp := fetch(t, h2, inv2.ID); resp.Truncated {
			t.Error("CN lane below cap: truncated must be false")
		}
	})
}
