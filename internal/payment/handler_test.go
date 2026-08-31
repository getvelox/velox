package payment

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/tenantstripe"
)

// TestStripeWebhook_SignatureGate pins the handler's signature gate as WIRED —
// through Routes() with a real endpoint lookup — rather than testing the
// verifier in isolation (the verifier is stripe-go's webhook.ValidatePayload
// since 2026-08-30). Four cases: a valid signature is accepted; a validly
// signed but STALE timestamp is rejected; a tampered body under a valid
// header is rejected; and a timestamp slightly in the FUTURE is accepted —
// the one-sided tolerance that replaced the hand-rolled two-sided check
// (which rejected every delivery when the host clock ran >5 min behind
// Stripe).
func TestStripeWebhook_SignatureGate(t *testing.T) {
	secret := "whsec_gate_test"
	newHandler := func() *Handler {
		invoices := newMockInvoiceUpdaterH()
		stripeAdapter := NewStripe(nil, invoices, newMockWebhookStoreHandler(), nil)
		resolver := &stubResolver{rows: map[string]tenantstripe.EndpointLookup{
			"vlx_spc_gate": {ID: "vlx_spc_gate", TenantID: "t1", Livemode: true, WebhookSecret: secret},
		}}
		return NewHandler(stripeAdapter, resolver)
	}
	body := []byte(`{"id":"evt_gate_1","type":"charge.refunded","created":1700000000,"livemode":true,"data":{"object":{}}}`)
	post := func(t *testing.T, body []byte, header string) int {
		t.Helper()
		req := httptest.NewRequest("POST", "/stripe/vlx_spc_gate", strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Stripe-Signature", header)
		rec := httptest.NewRecorder()
		newHandler().Routes().ServeHTTP(rec, req)
		return rec.Code
	}

	if code := post(t, body, signStripePayloadAt(body, secret, time.Now())); code == http.StatusBadRequest {
		t.Fatalf("valid signature must not be rejected as invalid signature, got %d", code)
	}
	if code := post(t, body, signStripePayloadAt(body, secret, time.Now().Add(-10*time.Minute))); code != http.StatusBadRequest {
		t.Fatalf("stale (10 min old) but validly signed payload must be rejected, got %d", code)
	}
	tampered := []byte(strings.Replace(string(body), `"charge.refunded"`, `"charge.succeeded"`, 1))
	if code := post(t, tampered, signStripePayloadAt(body, secret, time.Now())); code != http.StatusBadRequest {
		t.Fatalf("tampered body under a valid header must be rejected, got %d", code)
	}
	if code := post(t, body, signStripePayloadAt(body, secret, time.Now().Add(2*time.Minute))); code == http.StatusBadRequest {
		t.Fatalf("a timestamp 2 min in the future (host clock behind Stripe) must be accepted — one-sided tolerance, got %d", code)
	}
}

// stubResolver returns a preset endpoint lookup keyed by endpoint id.
type stubResolver struct {
	rows map[string]tenantstripe.EndpointLookup
}

func (s *stubResolver) LookupEndpoint(_ context.Context, id string) (tenantstripe.EndpointLookup, error) {
	row, ok := s.rows[id]
	if !ok {
		return tenantstripe.EndpointLookup{}, errs.ErrNotFound
	}
	return row, nil
}

func TestWebhookHandler_SuccessfulPayment(t *testing.T) {
	invoices := newMockInvoiceUpdaterH()
	invoices.invoices["inv_1"] = mockInvoice{
		id: "inv_1", tenantID: "t1", status: "finalized",
		paymentStatus: "processing", stripePI: "pi_test_abc",
	}
	invoices.byPI["pi_test_abc"] = "inv_1"

	webhooks := newMockWebhookStoreHandler()

	stripeAdapter := NewStripe(nil, invoices, webhooks, nil)
	secret := "whsec_handler_test"
	resolver := &stubResolver{rows: map[string]tenantstripe.EndpointLookup{
		"vlx_spc_abc": {ID: "vlx_spc_abc", TenantID: "t1", Livemode: true, WebhookSecret: secret},
	}}
	handler := NewHandler(stripeAdapter, resolver)

	event := map[string]any{
		"id":       "evt_success_1",
		"type":     "payment_intent.succeeded",
		"created":  time.Now().Unix(),
		"livemode": true,
		"data": map[string]any{
			"object": map[string]any{
				"id":       "pi_test_abc",
				"object":   "payment_intent",
				"status":   "succeeded",
				"amount":   19900,
				"currency": "usd",
				"metadata": map[string]string{
					"velox_tenant_id":  "t1",
					"velox_invoice_id": "inv_1",
				},
			},
		},
	}
	body, _ := json.Marshal(event)

	req := httptest.NewRequest("POST", "/stripe/vlx_spc_abc", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Stripe-Signature", signStripePayload(body, secret))
	rec := httptest.NewRecorder()

	handler.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status: got %d, want 200. body: %s", rec.Code, rec.Body.String())
	}

	var resp map[string]string
	_ = json.NewDecoder(rec.Body).Decode(&resp)
	if resp["status"] != "processed" {
		t.Errorf("response status: got %q, want processed", resp["status"])
	}

	inv := invoices.invoices["inv_1"]
	if inv.paymentStatus != "succeeded" {
		t.Errorf("payment_status: got %q, want succeeded", inv.paymentStatus)
	}
}

// TestWebhookHandler_OversizedBodyIs413NotSignatureFailure locks the size
// diagnostic: a legitimate (validly signed) event whose body exceeds
// maxWebhookBodySize must be rejected as 413 payload_too_large — NOT truncated
// at the cap, HMAC-failed over the truncated bytes, and died as a misleading
// 400 "invalid signature" (the pre-fix behavior, which left the operator
// chasing a phantom signing-secret problem while Stripe retried for ~3 days
// and then dropped the event permanently).
func TestWebhookHandler_OversizedBodyIs413NotSignatureFailure(t *testing.T) {
	stripeAdapter := NewStripe(nil, newMockInvoiceUpdaterH(), newMockWebhookStoreHandler(), nil)
	secret := "whsec_size_test"
	resolver := &stubResolver{rows: map[string]tenantstripe.EndpointLookup{
		"vlx_spc_abc": {ID: "vlx_spc_abc", TenantID: "t1", Livemode: true, WebhookSecret: secret},
	}}
	handler := NewHandler(stripeAdapter, resolver)

	// A valid event inflated past the cap by a padding field, signed over the
	// FULL body — so the only thing wrong with this request is its size.
	event := map[string]any{
		"id":       "evt_huge",
		"type":     "payment_intent.succeeded",
		"created":  time.Now().Unix(),
		"livemode": true,
		"padding":  strings.Repeat("x", maxWebhookBodySize),
		"data":     map[string]any{"object": map[string]any{"id": "pi_huge", "object": "payment_intent"}},
	}
	body, _ := json.Marshal(event)
	if len(body) <= maxWebhookBodySize {
		t.Fatalf("fixture bug: body is %d bytes, need > %d", len(body), maxWebhookBodySize)
	}

	req := httptest.NewRequest("POST", "/stripe/vlx_spc_abc", strings.NewReader(string(body)))
	req.Header.Set("Stripe-Signature", signStripePayload(body, secret))
	rec := httptest.NewRecorder()
	handler.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized signed body: got %d (%s), want 413 — a truncation surfacing as a signature failure is the misleading-diagnostic bug", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "payload_too_large") {
		t.Errorf("response must carry the payload_too_large code so the failure is attributable to size, got: %s", rec.Body.String())
	}
}

func TestWebhookHandler_NoVeloxMetadata(t *testing.T) {
	stripeAdapter := NewStripe(nil, newMockInvoiceUpdaterH(), newMockWebhookStoreHandler(), nil)
	secret := "whsec_foreign_test"
	resolver := &stubResolver{rows: map[string]tenantstripe.EndpointLookup{
		"vlx_spc_abc": {ID: "vlx_spc_abc", TenantID: "t1", Livemode: true, WebhookSecret: secret},
	}}
	handler := NewHandler(stripeAdapter, resolver)

	event := map[string]any{
		"id":       "evt_foreign",
		"type":     "payment_intent.succeeded",
		"created":  time.Now().Unix(),
		"livemode": true,
		"data": map[string]any{
			"object": map[string]any{
				"id":       "pi_not_ours",
				"object":   "payment_intent",
				"metadata": map[string]string{},
			},
		},
	}
	body, _ := json.Marshal(event)

	req := httptest.NewRequest("POST", "/stripe/vlx_spc_abc", strings.NewReader(string(body)))
	req.Header.Set("Stripe-Signature", signStripePayload(body, secret))
	rec := httptest.NewRecorder()
	handler.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("should return 200 for non-Velox events, got %d", rec.Code)
	}

	var resp map[string]string
	_ = json.NewDecoder(rec.Body).Decode(&resp)
	if resp["status"] != "skipped" {
		t.Errorf("should skip non-Velox events, got %q", resp["status"])
	}
}

// TestWebhookHandler_SetupIntentEmptyMetadata_StillAttaches — the end-to-end
// regression for the hosted "update payment" / operator add-card Checkout
// bug: a setup_intent.succeeded created by Stripe Checkout carries NO metadata
// on the SetupIntent (Checkout doesn't copy session metadata), so it has no
// velox_tenant_id. It must NOT be dropped at the HTTP gate the way a foreign
// payment_intent is — it reaches HandleWebhook, which resolves the customer
// from the SetupIntent's `customer` field and attaches the PM.
func TestWebhookHandler_SetupIntentEmptyMetadata_StillAttaches(t *testing.T) {
	stripeAdapter := NewStripe(nil, newMockInvoiceUpdaterH(), newMockWebhookStoreHandler(), nil)
	attacher := &recordingAttacher{}
	stripeAdapter.SetPaymentMethodAttacher(attacher)
	stripeAdapter.SetCustomerResolver(&recordingCustomerResolver{wantStripeID: "cus_stripe_99", veloxID: "cus_local_7"})

	secret := "whsec_seti_test"
	resolver := &stubResolver{rows: map[string]tenantstripe.EndpointLookup{
		"vlx_spc_abc": {ID: "vlx_spc_abc", TenantID: "t1", Livemode: true, WebhookSecret: secret},
	}}
	handler := NewHandler(stripeAdapter, resolver)

	event := map[string]any{
		"id":       "evt_seti_http",
		"type":     "setup_intent.succeeded",
		"created":  time.Now().Unix(),
		"livemode": true,
		"data": map[string]any{
			"object": map[string]any{
				"id":             "seti_http_1",
				"object":         "setup_intent",
				"payment_method": "pm_stripe_77",
				"customer":       "cus_stripe_99",
				"metadata":       map[string]string{}, // Checkout left this empty
			},
		},
	}
	body, _ := json.Marshal(event)

	req := httptest.NewRequest("POST", "/stripe/vlx_spc_abc", strings.NewReader(string(body)))
	req.Header.Set("Stripe-Signature", signStripePayload(body, secret))
	rec := httptest.NewRecorder()
	handler.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200. body: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]string
	_ = json.NewDecoder(rec.Body).Decode(&resp)
	if resp["status"] != "processed" {
		t.Fatalf("setup_intent with empty metadata must be processed, not %q", resp["status"])
	}
	if attacher.called != 1 || attacher.customerID != "cus_local_7" || attacher.pmID != "pm_stripe_77" {
		t.Fatalf("expected attach(cus_local_7, pm_stripe_77), got called=%d customer=%q pm=%q",
			attacher.called, attacher.customerID, attacher.pmID)
	}
}

func TestWebhookHandler_UnknownEndpoint404(t *testing.T) {
	stripeAdapter := NewStripe(nil, newMockInvoiceUpdaterH(), newMockWebhookStoreHandler(), nil)
	resolver := &stubResolver{rows: map[string]tenantstripe.EndpointLookup{}}
	handler := NewHandler(stripeAdapter, resolver)

	req := httptest.NewRequest("POST", "/stripe/vlx_spc_does_not_exist",
		strings.NewReader(`{"id":"evt_1"}`))
	req.Header.Set("Stripe-Signature", "t=0,v1=00")
	rec := httptest.NewRecorder()
	handler.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown endpoint should 404, got %d", rec.Code)
	}
}

func TestWebhookHandler_SignatureRequired(t *testing.T) {
	stripeAdapter := NewStripe(nil, newMockInvoiceUpdaterH(), newMockWebhookStoreHandler(), nil)
	resolver := &stubResolver{rows: map[string]tenantstripe.EndpointLookup{
		"vlx_spc_abc": {ID: "vlx_spc_abc", TenantID: "t1", Livemode: true, WebhookSecret: "whsec_real"},
	}}
	handler := NewHandler(stripeAdapter, resolver)

	req := httptest.NewRequest("POST", "/stripe/vlx_spc_abc", strings.NewReader(`{"id":"evt_1"}`))
	rec := httptest.NewRecorder()
	handler.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("should reject unsigned webhook, got %d", rec.Code)
	}
}

func TestWebhookHandler_TenantMismatchRejects(t *testing.T) {
	// Defense in depth: endpoint belongs to t1 but payload metadata claims t2.
	// Accepting this would let a misconfigured tenant write webhook events
	// into another tenant's data space.
	stripeAdapter := NewStripe(nil, newMockInvoiceUpdaterH(), newMockWebhookStoreHandler(), nil)
	secret := "whsec_mismatch"
	resolver := &stubResolver{rows: map[string]tenantstripe.EndpointLookup{
		"vlx_spc_abc": {ID: "vlx_spc_abc", TenantID: "t1", Livemode: true, WebhookSecret: secret},
	}}
	handler := NewHandler(stripeAdapter, resolver)

	event := map[string]any{
		"id":       "evt_mismatch",
		"type":     "payment_intent.succeeded",
		"created":  time.Now().Unix(),
		"livemode": true,
		"data": map[string]any{
			"object": map[string]any{
				"id":       "pi_other",
				"object":   "payment_intent",
				"metadata": map[string]string{"velox_tenant_id": "t2"},
			},
		},
	}
	body, _ := json.Marshal(event)

	req := httptest.NewRequest("POST", "/stripe/vlx_spc_abc", strings.NewReader(string(body)))
	req.Header.Set("Stripe-Signature", signStripePayload(body, secret))
	rec := httptest.NewRecorder()
	handler.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("tenant mismatch should reject, got %d", rec.Code)
	}
}

// TestWebhookHandler_TransientFailureRedelivers is the regression test for the
// silent-drop bug: a transient processing failure used to commit the dedup row
// FIRST and then return HTTP 200, so Stripe never redelivered and the dedup row
// short-circuited any retry — the event was lost forever.
//
// The fix: on a transient failure the handler writes NO dedup row and returns
// 5xx, so Stripe redelivers and the (now healthy) handler re-processes.
func TestWebhookHandler_TransientFailureRedelivers(t *testing.T) {
	invoices := newMockInvoiceUpdaterH()
	invoices.invoices["inv_1"] = mockInvoice{
		id: "inv_1", tenantID: "t1", status: "finalized",
		paymentStatus: "processing", stripePI: "pi_test_abc",
	}
	invoices.byPI["pi_test_abc"] = "inv_1"
	// First delivery hits a transient DB blip on MarkPaid.
	invoices.markPaidErr = fmt.Errorf("connection reset by peer")

	webhooks := newMockWebhookStoreHandler()
	stripeAdapter := NewStripe(nil, invoices, webhooks, nil)
	secret := "whsec_transient_test"
	resolver := &stubResolver{rows: map[string]tenantstripe.EndpointLookup{
		"vlx_spc_abc": {ID: "vlx_spc_abc", TenantID: "t1", Livemode: true, WebhookSecret: secret},
	}}
	handler := NewHandler(stripeAdapter, resolver)

	event := map[string]any{
		"id":       "evt_transient_1",
		"type":     "payment_intent.succeeded",
		"created":  time.Now().Unix(),
		"livemode": true,
		"data": map[string]any{
			"object": map[string]any{
				"id":       "pi_test_abc",
				"object":   "payment_intent",
				"status":   "succeeded",
				"amount":   19900,
				"currency": "usd",
				"metadata": map[string]string{
					"velox_tenant_id":  "t1",
					"velox_invoice_id": "inv_1",
				},
			},
		},
	}
	body, _ := json.Marshal(event)

	send := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/stripe/vlx_spc_abc", strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Stripe-Signature", signStripePayload(body, secret))
		rec := httptest.NewRecorder()
		handler.Routes().ServeHTTP(rec, req)
		return rec
	}

	// First delivery: transient failure must surface as 5xx so Stripe retries.
	rec := send()
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("transient failure: got status %d, want 500. body: %s", rec.Code, rec.Body.String())
	}
	// And it must NOT have been recorded as seen — otherwise the redelivery
	// is short-circuited as a duplicate and the event is dropped forever.
	if webhooks.seen["evt_transient_1"] {
		t.Fatalf("transient failure must not mark the event consumed (would block Stripe redelivery)")
	}
	if invoices.invoices["inv_1"].paymentStatus == "succeeded" {
		t.Fatalf("invoice should not be paid after a failed MarkPaid")
	}

	// Stripe redelivers the SAME event_id; the blip has cleared.
	invoices.markPaidErr = nil
	rec = send()
	if rec.Code != http.StatusOK {
		t.Fatalf("redelivery: got status %d, want 200. body: %s", rec.Code, rec.Body.String())
	}
	if !webhooks.seen["evt_transient_1"] {
		t.Fatalf("successful redelivery must record the dedup row")
	}
	if got := invoices.invoices["inv_1"].paymentStatus; got != "succeeded" {
		t.Fatalf("redelivery should settle the invoice: payment_status got %q, want succeeded", got)
	}
}

// signStripePayload produces a valid Stripe-Signature header for payload+secret.
func signStripePayload(payload []byte, secret string) string {
	return signStripePayloadAt(payload, secret, time.Now())
}

// signStripePayloadAt signs payload with the given timestamp — the shape
// Stripe's own webhook.ComputeSignature produces (HMAC-SHA256 over
// "<unix ts>.<payload>").
func signStripePayloadAt(payload []byte, secret string, at time.Time) string {
	ts := fmt.Sprintf("%d", at.Unix())
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "." + string(payload)))
	return fmt.Sprintf("t=%s,v1=%s", ts, hex.EncodeToString(mac.Sum(nil)))
}

// --- Mock helpers for handler tests ---

type mockInvoice struct {
	id, tenantID, status, paymentStatus, stripePI, lastError string
	paidAt                                                   *time.Time
}

type mockInvoiceUpdaterHandler struct {
	invoices    map[string]mockInvoice
	byPI        map[string]string
	failNotedPI map[string]string // invoice ID -> PI whose failure notifications fired
	markPaidErr error             // when set, MarkPaid returns this (simulates a transient DB failure)
}

func (m *mockInvoiceUpdaterHandler) RecordChargeAttempt(_ context.Context, _ string, _ domain.InvoiceChargeAttempt) error {
	return nil
}

func (m *mockInvoiceUpdaterHandler) ReleaseAutoChargeClaim(_ context.Context, _, _ string) error {
	return nil
}

func newMockInvoiceUpdaterH() *mockInvoiceUpdaterHandler {
	return &mockInvoiceUpdaterHandler{
		invoices:    make(map[string]mockInvoice),
		byPI:        make(map[string]string),
		failNotedPI: make(map[string]string),
	}
}

func (m *mockInvoiceUpdaterHandler) UpdatePayment(_ context.Context, tenantID, id string, ps domain.InvoicePaymentStatus, piID, errMsg string, paidAt *time.Time) (domain.Invoice, error) {
	inv, ok := m.invoices[id]
	if !ok {
		return domain.Invoice{}, fmt.Errorf("not found")
	}
	inv.paymentStatus = string(ps)
	inv.stripePI = piID
	inv.lastError = errMsg
	inv.paidAt = paidAt
	m.invoices[id] = inv
	if piID != "" {
		m.byPI[piID] = id
	}
	return domain.Invoice{ID: id, TenantID: tenantID, PaymentStatus: ps}, nil
}

func (m *mockInvoiceUpdaterHandler) UpdateStatus(_ context.Context, _, id string, status domain.InvoiceStatus) (domain.Invoice, error) {
	inv, ok := m.invoices[id]
	if !ok {
		return domain.Invoice{}, fmt.Errorf("not found")
	}
	inv.status = string(status)
	m.invoices[id] = inv
	return domain.Invoice{ID: id, Status: status}, nil
}

func (m *mockInvoiceUpdaterHandler) GetByStripePaymentIntentID(_ context.Context, _, piID string) (domain.Invoice, error) {
	id, ok := m.byPI[piID]
	if !ok {
		return domain.Invoice{}, fmt.Errorf("not found")
	}
	inv := m.invoices[id]
	return domain.Invoice{ID: id, TenantID: inv.tenantID}, nil
}

func (m *mockInvoiceUpdaterHandler) Get(_ context.Context, _, id string) (domain.Invoice, error) {
	inv, ok := m.invoices[id]
	if !ok {
		return domain.Invoice{}, fmt.Errorf("not found")
	}
	return domain.Invoice{ID: id, TenantID: inv.tenantID}, nil
}

func (m *mockInvoiceUpdaterHandler) MarkPaid(ctx context.Context, tenantID, id string, piID string, paidAt time.Time) (domain.Invoice, error) {
	inv, _, err := m.MarkPaidReportingTransition(ctx, tenantID, id, piID, paidAt)
	return inv, err
}

func (m *mockInvoiceUpdaterHandler) MarkPaidReportingTransition(_ context.Context, _, id string, piID string, paidAt time.Time) (domain.Invoice, bool, error) {
	if m.markPaidErr != nil {
		return domain.Invoice{}, false, m.markPaidErr
	}
	inv, ok := m.invoices[id]
	if !ok {
		return domain.Invoice{}, false, fmt.Errorf("not found")
	}
	alreadyPaid := inv.paymentStatus == "succeeded"
	inv.paymentStatus = "succeeded"
	inv.stripePI = piID
	inv.paidAt = &paidAt
	m.invoices[id] = inv
	if piID != "" {
		m.byPI[piID] = id
	}
	return domain.Invoice{ID: id, TenantID: inv.tenantID, Status: domain.InvoicePaid}, !alreadyPaid, nil
}

// The hook is INVOKED when the transition is won, exactly as the real store
// does inside its transaction (nil tx: these fakes have none). A fake that
// accepted the hook and dropped it would make every settlement-email
// assertion in this package read zero and pass forever.
func (m *mockInvoiceUpdaterHandler) MarkPaidCardSettlementTransition(ctx context.Context, tenantID, id, piID string, paidAt time.Time, then func(tx *sql.Tx, fresh domain.Invoice) error) (domain.Invoice, bool, error) {
	inv, transitioned, err := m.MarkPaidReportingTransition(ctx, tenantID, id, piID, paidAt)
	if err == nil && transitioned && then != nil {
		_ = then(nil, inv)
	}
	return inv, transitioned, err
}

func (m *mockInvoiceUpdaterHandler) MarkPaymentFailedReportingTransition(_ context.Context, _, id, piID, errMsg string, then func(tx *sql.Tx, fresh domain.Invoice) error) (domain.Invoice, bool, error) {
	inv, ok := m.invoices[id]
	if !ok {
		return domain.Invoice{}, false, fmt.Errorf("not found")
	}
	if inv.paymentStatus == "succeeded" {
		return domain.Invoice{ID: id, TenantID: inv.tenantID}, false, nil
	}
	if m.failNotedPI == nil {
		m.failNotedPI = make(map[string]string)
	}
	first := m.failNotedPI[id] != piID
	inv.paymentStatus = "failed"
	inv.stripePI = piID
	inv.lastError = errMsg
	m.invoices[id] = inv
	m.failNotedPI[id] = piID
	if piID != "" {
		m.byPI[piID] = id
	}
	return domain.Invoice{ID: id, TenantID: inv.tenantID, PaymentStatus: domain.PaymentFailed}, first, nil
}

func (m *mockInvoiceUpdaterHandler) SetPaymentCard(_ context.Context, _, _ string, _, _ string) error {
	return nil
}

func (m *mockInvoiceUpdaterHandler) ApplyCreditNote(_ context.Context, _, id string, _ int64) (domain.Invoice, error) {
	inv, ok := m.invoices[id]
	if !ok {
		return domain.Invoice{}, fmt.Errorf("not found")
	}
	return domain.Invoice{ID: id, TenantID: inv.tenantID}, nil
}

type mockWebhookStoreH struct {
	seen map[string]bool
}

func newMockWebhookStoreHandler() *mockWebhookStoreH {
	return &mockWebhookStoreH{seen: make(map[string]bool)}
}

func (m *mockWebhookStoreH) WasProcessed(_ context.Context, _, stripeEventID string) (bool, error) {
	return m.seen[stripeEventID], nil
}

func (m *mockWebhookStoreH) IngestEvent(_ context.Context, _ string, event domain.StripeWebhookEvent) (domain.StripeWebhookEvent, bool, error) {
	if m.seen[event.StripeEventID] {
		return event, false, nil
	}
	m.seen[event.StripeEventID] = true
	return event, true, nil
}

func (m *mockWebhookStoreH) ListByInvoice(_ context.Context, _, _ string) ([]domain.StripeWebhookEvent, error) {
	return nil, nil
}
