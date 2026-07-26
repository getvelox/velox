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

// dunningEventsLaneFake returns one run with canned events (the
// degradation fake's ListEvents can only error).
type dunningEventsLaneFake struct {
	runs   []domain.InvoiceDunningRun
	events []domain.InvoiceDunningEvent
}

func (f *dunningEventsLaneFake) ListRunsByInvoice(context.Context, string, string) ([]domain.InvoiceDunningRun, error) {
	return f.runs, nil
}

func (f *dunningEventsLaneFake) ListEvents(context.Context, string, string) ([]domain.InvoiceDunningEvent, error) {
	return f.events, nil
}

// TestPaymentTimeline_ResolvedRowFoldsIntoCauseRow pins the ADR-020 fold
// for every operator resolution (found live on FLOW I13, 2026-07-26: a
// write-off rendered "Marked uncollectible — written off as bad debt"
// PLUS a bare "Dunning resolved" twin at the same instant; the void
// path had the same twin). The dunning 'resolved' event folds into the
// lifecycle cause row — and ONLY when that cause row's own field proves
// the invoice actually transitioned, so a failed propagation keeps the
// dunning row as the surviving record.
func TestPaymentTimeline_ResolvedRowFoldsIntoCauseRow(t *testing.T) {
	at := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)

	fetch := func(t *testing.T, h *Handler, invID string) []map[string]any {
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
			t.Fatalf("decode: %v", err)
		}
		return resp.Events
	}

	countResolved := func(events []map[string]any) (resolvedRows int, descriptions []string) {
		for _, e := range events {
			descriptions = append(descriptions, e["description"].(string))
			if e["event_type"] == "resolved" {
				resolvedRows++
			}
		}
		return
	}

	seed := func(t *testing.T, mutate func(*domain.Invoice), reason string) (*Handler, string) {
		t.Helper()
		store := newMemStore()
		inv := domain.Invoice{
			CustomerID: "cus_1", Status: domain.InvoiceFinalized,
			PaymentStatus: domain.PaymentPending, AmountDueCents: 5500,
		}
		mutate(&inv)
		created, err := store.Create(context.Background(), "t1", inv)
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		// Create() may not persist terminal fields — re-apply via update.
		created.Status = inv.Status
		created.UncollectibleAt = inv.UncollectibleAt
		created.VoidedAt = inv.VoidedAt
		created.PaidAt = inv.PaidAt
		store.invoices[created.ID] = created
		h := &Handler{svc: NewService(store, nil, nil)}
		h.dunningTimeline = &dunningEventsLaneFake{
			runs: []domain.InvoiceDunningRun{{ID: "run_1"}},
			events: []domain.InvoiceDunningEvent{
				{EventType: "dunning_started", CreatedAt: at.Add(-48 * time.Hour)},
				{EventType: "resolved", Reason: reason, CreatedAt: at},
			},
		}
		return h, created.ID
	}

	t.Run("write-off: resolved folds into the Marked-uncollectible row", func(t *testing.T) {
		h, id := seed(t, func(i *domain.Invoice) {
			i.Status = domain.InvoiceUncollectible
			i.UncollectibleAt = &at
		}, "invoice_not_collectible")
		n, descs := countResolved(fetch(t, h, id))
		if n != 0 {
			t.Errorf("bare 'Dunning resolved' twin survived the write-off fold: %v", descs)
		}
	})

	t.Run("void: resolved folds into the Invoice-voided row", func(t *testing.T) {
		h, id := seed(t, func(i *domain.Invoice) {
			i.Status = domain.InvoiceVoided
			i.VoidedAt = &at
		}, "manually_resolved")
		n, descs := countResolved(fetch(t, h, id))
		if n != 0 {
			t.Errorf("bare 'Dunning resolved' twin survived the void fold: %v", descs)
		}
	})

	t.Run("sweep-flavored void reason folds too", func(t *testing.T) {
		h, id := seed(t, func(i *domain.Invoice) {
			i.Status = domain.InvoiceVoided
			i.VoidedAt = &at
		}, "invoice_voided")
		if n, descs := countResolved(fetch(t, h, id)); n != 0 {
			t.Errorf("sweep-reason twin survived: %v", descs)
		}
	})

	t.Run("FAILED propagation keeps the dunning row — the only surviving record", func(t *testing.T) {
		// Run resolved as write-off but the invoice never transitioned
		// (MarkUncollectible failed): UncollectibleAt nil → no cause row
		// → the dunning row must stay.
		h, id := seed(t, func(i *domain.Invoice) {}, "invoice_not_collectible")
		n, descs := countResolved(fetch(t, h, id))
		if n != 1 {
			t.Errorf("dunning row must survive when no cause row exists: rows=%d %v", n, descs)
		}
	})
}
