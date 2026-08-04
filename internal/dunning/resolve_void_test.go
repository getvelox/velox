package dunning

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
)

type recordingVoider struct {
	calls       int
	recordCalls int
	recordNotes []string
	err         error
}

func (v *recordingVoider) RecordOfflinePayment(_ context.Context, tenantID, id, note string) (domain.Invoice, error) {
	v.recordCalls++
	v.recordNotes = append(v.recordNotes, note)
	if v.err != nil {
		return domain.Invoice{}, v.err
	}
	return domain.Invoice{ID: id, TenantID: tenantID, CustomerID: "cus_1", Status: domain.InvoicePaid, StripePaymentIntentID: "out_of_band:2026-07-26T00:00:00Z"}, nil
}

func (v *recordingVoider) Void(_ context.Context, tenantID, id string) (domain.Invoice, error) {
	v.calls++
	if v.err != nil {
		return domain.Invoice{}, v.err
	}
	// The PI id rides the Void return (the dunning handler cancels it
	// post-void); the consumed-credit reversal happens inside Void itself.
	return domain.Invoice{ID: id, TenantID: tenantID, CustomerID: "cus_1", Status: domain.InvoiceVoided, StripePaymentIntentID: "pi_1"}, nil
}

type recordingCanceler struct{ calls int }

func (c *recordingCanceler) StopCollection(_ context.Context, _ string, _ domain.Invoice) {
	c.calls++
}

func resolveManually(t *testing.T, h *Handler, runID string) {
	t.Helper()
	body, _ := json.Marshal(resolveInput{Resolution: string(domain.ResolutionInvoiceVoided)})
	r := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", runID)
	ctx := context.WithValue(auth.WithTenantID(r.Context(), "t1"), chi.RouteCtxKey, rctx)
	r = r.WithContext(ctx)
	h.resolveRun(httptest.NewRecorder(), r)
}

// TestResolveRun_ManualVoid_RoutesThroughServiceAndGatesSideEffects pins D5
// (single void writer, ADR-059): a manually-resolved dunning run voids the
// invoice through invoice.Service.Void (which reverses the consumed credits
// atomically + reverses tax + emits the single-writer invoice.voided event +
// enforces the in-flight guard) instead of the raw store. The post-void
// PI-cancel must run ONLY when the void SUCCEEDS — otherwise an in-flight
// invoice (whose void the service refuses) would still get its live
// PaymentIntent canceled, defeating the in-flight guard.
func TestResolveRun_ManualVoid_RoutesThroughServiceAndGatesSideEffects(t *testing.T) {
	ctx := context.Background()
	newHandler := func(voider *recordingVoider) (*Handler, *recordingCanceler, string) {
		svc := NewService(newMemStore(), &noopRetrier{}, nil)
		run, err := svc.StartDunning(ctx, "t1", "inv_1", "cus_1", time.Now(), domain.DunningCausePaymentFailed)
		if err != nil {
			t.Fatalf("StartDunning: %v", err)
		}
		canceler := &recordingCanceler{}
		h := NewHandler(svc, HandlerDeps{
			PaymentCancel: canceler,
		})
		h.SetInvoiceVoider(voider)
		return h, canceler, run.ID
	}

	t.Run("void succeeds → routes through service voider + cancels PI", func(t *testing.T) {
		voider := &recordingVoider{}
		h, canceler, runID := newHandler(voider)
		resolveManually(t, h, runID)
		if voider.calls != 1 {
			t.Errorf("void must route through the invoice service voider; calls=%d, want 1", voider.calls)
		}
		if canceler.calls != 1 {
			t.Errorf("stop-collection should run after a successful void; calls=%d, want 1", canceler.calls)
		}
	})

	t.Run("void refused (in-flight) → post-void PI-cancel SKIPPED", func(t *testing.T) {
		voider := &recordingVoider{err: errs.InvalidState("a charge is in flight on this invoice")}
		h, canceler, runID := newHandler(voider)
		resolveManually(t, h, runID)
		if voider.calls != 1 {
			t.Errorf("voider should be attempted once; calls=%d", voider.calls)
		}
		if canceler.calls != 0 {
			t.Errorf("stop-collection MUST NOT run when the void was refused — the invoice is still collectible; calls=%d, want 0", canceler.calls)
		}
	})
}

// TestResolveRun_RejectsUnknownResolution pins the operator-input contract
// (2026-07-26, found live on FLOW I5): the endpoint stored ANY string
// verbatim — a typo'd "payment_received" landed in
// invoice_dunning_runs.resolution, silently skipped the mark-paid
// propagation (the switch matched nothing), and rendered as a raw slug in
// the dashboard. Only the three operator-choosable outcomes are legal;
// engine-written states (retries_exhausted, action_failed) and free text
// are 422s.
func TestResolveRun_RejectsUnknownResolution(t *testing.T) {
	ctx := context.Background()
	svc := NewService(newMemStore(), &noopRetrier{}, nil)
	run, err := svc.StartDunning(ctx, "t1", "inv_1", "cus_1", time.Now(), domain.DunningCausePaymentFailed)
	if err != nil {
		t.Fatalf("StartDunning: %v", err)
	}
	h := NewHandler(svc, HandlerDeps{})

	for _, bad := range []string{"payment_received", "retries_exhausted", "action_failed", "", "whatever"} {
		body, _ := json.Marshal(resolveInput{Resolution: bad})
		r := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", run.ID)
		r = r.WithContext(context.WithValue(auth.WithTenantID(r.Context(), "t1"), chi.RouteCtxKey, rctx))
		rec := httptest.NewRecorder()
		h.resolveRun(rec, r)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("resolution %q: got %d, want 422", bad, rec.Code)
		}
	}

	// The run must be untouched by the rejected attempts.
	got, err := svc.store.GetRun(ctx, "t1", run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.State != domain.DunningActive || got.Resolution != "" {
		t.Errorf("run mutated by rejected resolution: state=%s resolution=%q", got.State, got.Resolution)
	}
}

// TestResolveRun_PaymentRecovered_RoutesThroughOfflinePaymentWriter pins the
// single-offline-payment-writer contract (2026-07-26): payment_recovered
// resolves through invoice.Service.RecordOfflinePayment — the same path as
// the invoice page's Record-offline-payment dialog — so the recovery carries
// the out_of_band: PaymentIntent marker, the in-flight guard, and the
// invoice.payment_recorded webhook. Pre-fix it called a raw MarkPaid("")
// with none of those.
func TestResolveRun_PaymentRecovered_RoutesThroughOfflinePaymentWriter(t *testing.T) {
	ctx := context.Background()
	svc := NewService(newMemStore(), &noopRetrier{}, nil)
	run, err := svc.StartDunning(ctx, "t1", "inv_1", "cus_1", time.Now(), domain.DunningCausePaymentFailed)
	if err != nil {
		t.Fatalf("StartDunning: %v", err)
	}
	voider := &recordingVoider{}
	h := NewHandler(svc, HandlerDeps{})
	h.SetInvoiceVoider(voider)

	body, _ := json.Marshal(resolveInput{Resolution: string(domain.ResolutionPaymentRecovered)})
	r := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", run.ID)
	r = r.WithContext(context.WithValue(auth.WithTenantID(r.Context(), "t1"), chi.RouteCtxKey, rctx))
	rec := httptest.NewRecorder()
	h.resolveRun(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("resolve: got %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if voider.recordCalls != 1 {
		t.Fatalf("RecordOfflinePayment calls = %d, want 1 (raw MarkPaid is the dual-writer hole)", voider.recordCalls)
	}
	if len(voider.recordNotes) != 1 || voider.recordNotes[0] != "Recovered via dunning resolution" {
		t.Errorf("note = %v, want the dunning-resolution memo", voider.recordNotes)
	}
	if voider.calls != 0 {
		t.Errorf("Void must not run on payment_recovered; calls=%d", voider.calls)
	}
}
