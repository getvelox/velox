package payment

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
)

// ---------------------------------------------------------------------------
// Mocks
// ---------------------------------------------------------------------------

type mockStripeClient struct {
	lastParams   PaymentIntentParams
	shouldFail   bool
	failErr      error // when set, overrides the default card_declined failure
	piID         string
	chargeStatus string                         // CreatePaymentIntent result.Status; defaults to "requires_capture"
	piStates     map[string]PaymentIntentResult // reconciler lookups by PI ID
}

func (m *mockStripeClient) CreatePaymentIntent(_ context.Context, params PaymentIntentParams) (PaymentIntentResult, error) {
	m.lastParams = params
	if m.shouldFail {
		if m.failErr != nil {
			return PaymentIntentResult{}, m.failErr
		}
		return PaymentIntentResult{}, &PaymentError{Message: "card_declined", DeclineCode: "card_declined"}
	}
	status := m.chargeStatus
	if status == "" {
		status = "requires_capture"
	}
	return PaymentIntentResult{
		ID:           m.piID,
		Status:       status,
		ClientSecret: "pi_secret_test",
	}, nil
}

func (m *mockStripeClient) CancelPaymentIntent(_ context.Context, _ string) error {
	return nil
}

func (m *mockStripeClient) GetPaymentIntent(_ context.Context, piID string) (PaymentIntentResult, error) {
	if m.piStates != nil {
		if res, ok := m.piStates[piID]; ok {
			return res, nil
		}
	}
	return PaymentIntentResult{ID: piID, Status: "requires_payment_method"}, nil
}

type mockInvoiceUpdater struct {
	markPaidErr error // injected: MarkPaid*Transition returns this (voided-target shape)
	// chargeAttempts records every ADR-102 attempt-fact write (chokepoint
	// insert + settle-path upserts), in call order.
	chargeAttempts []domain.InvoiceChargeAttempt
	invoices       map[string]domain.Invoice
	byPI           map[string]string // PI ID -> invoice ID
	failNotedPI    map[string]string // invoice ID -> PI whose failure notifications fired
	// cardEventEnqueues counts payment.succeeded enqueues — mirrors the real
	// store enqueuing payment.succeeded IN-TX, gated on the transition (so a
	// concurrent redelivery increments it once, not twice).
	cardEventEnqueues int
	// failedEventEnqueues counts payment.failed enqueues — mirrors the real
	// store enqueuing payment.failed IN-TX, gated on firstForThisPI (same-PI
	// redelivery no-ops; a new retry PI fires again).
	failedEventEnqueues int
}

func newMockInvoiceUpdater() *mockInvoiceUpdater {
	return &mockInvoiceUpdater{
		invoices:    make(map[string]domain.Invoice),
		byPI:        make(map[string]string),
		failNotedPI: make(map[string]string),
	}
}

func (m *mockInvoiceUpdater) UpdatePayment(_ context.Context, tenantID, id string, ps domain.InvoicePaymentStatus, piID, errMsg string, paidAt *time.Time) (domain.Invoice, error) {
	inv, ok := m.invoices[id]
	if !ok {
		return domain.Invoice{}, errs.ErrNotFound
	}
	inv.PaymentStatus = ps
	inv.StripePaymentIntentID = piID
	inv.LastPaymentError = errMsg
	inv.PaidAt = paidAt
	m.invoices[id] = inv
	if piID != "" {
		m.byPI[piID] = id
	}
	return inv, nil
}

func (m *mockInvoiceUpdater) UpdateStatus(_ context.Context, _, id string, status domain.InvoiceStatus) (domain.Invoice, error) {
	inv, ok := m.invoices[id]
	if !ok {
		return domain.Invoice{}, errs.ErrNotFound
	}
	inv.Status = status
	m.invoices[id] = inv
	return inv, nil
}

func (m *mockInvoiceUpdater) GetByStripePaymentIntentID(_ context.Context, _, piID string) (domain.Invoice, error) {
	id, ok := m.byPI[piID]
	if !ok {
		return domain.Invoice{}, errs.ErrNotFound
	}
	return m.invoices[id], nil
}

func (m *mockInvoiceUpdater) RecordChargeAttempt(_ context.Context, _ string, a domain.InvoiceChargeAttempt) error {
	m.chargeAttempts = append(m.chargeAttempts, a)
	return nil
}

func (m *mockInvoiceUpdater) Get(_ context.Context, _, id string) (domain.Invoice, error) {
	inv, ok := m.invoices[id]
	if !ok {
		return domain.Invoice{}, errs.ErrNotFound
	}
	return inv, nil
}

func (m *mockInvoiceUpdater) MarkPaid(ctx context.Context, tenantID, id string, stripePI string, paidAt time.Time) (domain.Invoice, error) {
	inv, _, err := m.MarkPaidReportingTransition(ctx, tenantID, id, stripePI, paidAt)
	return inv, err
}

func (m *mockInvoiceUpdater) MarkPaidReportingTransition(_ context.Context, _, id string, stripePI string, paidAt time.Time) (domain.Invoice, bool, error) {
	if m.markPaidErr != nil {
		return domain.Invoice{}, false, m.markPaidErr
	}
	inv, ok := m.invoices[id]
	if !ok {
		return domain.Invoice{}, false, errs.ErrNotFound
	}
	// Mirror the store: already-paid is a no-op (transitioned=false),
	// matching the FOR UPDATE serialized semantics.
	if inv.Status == domain.InvoicePaid {
		return inv, false, nil
	}
	inv.Status = domain.InvoicePaid
	inv.PaymentStatus = domain.PaymentSucceeded
	inv.StripePaymentIntentID = stripePI
	inv.PaidAt = &paidAt
	inv.AmountPaidCents = inv.AmountDueCents // mirror the store: booked = due at settle
	inv.AmountDueCents = 0
	m.invoices[id] = inv
	return inv, true, nil
}

// MarkPaidCardSettlementTransition mirrors the store's card path: same paid-flip
// as MarkPaidReportingTransition PLUS an in-tx payment.succeeded enqueue, gated on
// the transition. We record the enqueue count so tests can assert exactly-once.
func (m *mockInvoiceUpdater) MarkPaidCardSettlementTransition(ctx context.Context, tenantID, id string, stripePI string, paidAt time.Time) (domain.Invoice, bool, error) {
	inv, transitioned, err := m.MarkPaidReportingTransition(ctx, tenantID, id, stripePI, paidAt)
	if err == nil && transitioned {
		m.cardEventEnqueues++
	}
	return inv, transitioned, err
}

func (m *mockInvoiceUpdater) MarkPaymentFailedReportingTransition(_ context.Context, _, id, piID, errMsg string) (domain.Invoice, bool, error) {
	inv, ok := m.invoices[id]
	if !ok {
		return domain.Invoice{}, false, errs.ErrNotFound
	}
	// Out-of-order failure for an already-settled invoice: no-op, never a
	// fresh notification (mirrors the FOR UPDATE store guard).
	if inv.Status == domain.InvoicePaid || inv.PaymentStatus == domain.PaymentSucceeded {
		return inv, false, nil
	}
	if m.failNotedPI == nil {
		m.failNotedPI = make(map[string]string)
	}
	// firstForThisPI: have we NOT yet fired failure notifications for this PI?
	first := m.failNotedPI[id] != piID
	inv.PaymentStatus = domain.PaymentFailed
	inv.StripePaymentIntentID = piID
	inv.LastPaymentError = errMsg
	m.invoices[id] = inv
	m.failNotedPI[id] = piID
	if piID != "" {
		m.byPI[piID] = id
	}
	// Mirror the real store: payment.failed is enqueued IN-TX, gated on
	// firstForThisPI (crash-safe with the failed-stamp; SettleFailed no longer
	// post-commit-fires it).
	if first {
		m.failedEventEnqueues++
	}
	return inv, first, nil
}

func (m *mockInvoiceUpdater) SetPaymentCard(_ context.Context, _, id, brand, last4 string) error {
	inv, ok := m.invoices[id]
	if !ok {
		return errs.ErrNotFound
	}
	inv.PaymentCardBrand = brand
	inv.PaymentCardLast4 = last4
	m.invoices[id] = inv
	return nil
}

func (m *mockInvoiceUpdater) ApplyCreditNote(_ context.Context, _, id string, amountCents int64) (domain.Invoice, error) {
	inv, ok := m.invoices[id]
	if !ok {
		return domain.Invoice{}, errs.ErrNotFound
	}
	inv.AmountDueCents -= amountCents
	if inv.AmountDueCents < 0 {
		inv.AmountDueCents = 0
	}
	m.invoices[id] = inv
	return inv, nil
}

type mockWebhookStore struct {
	events      map[string]bool // stripe_event_id -> ingested (dedup row present)
	ingestCalls int             // number of IngestEvent invocations
	ingestErr   error           // when set, IngestEvent returns this error
	wasProcErr  error           // when set, WasProcessed returns this error
}

func newMockWebhookStore() *mockWebhookStore {
	return &mockWebhookStore{events: make(map[string]bool)}
}

func (m *mockWebhookStore) WasProcessed(_ context.Context, _, stripeEventID string) (bool, error) {
	if m.wasProcErr != nil {
		return false, m.wasProcErr
	}
	return m.events[stripeEventID], nil
}

func (m *mockWebhookStore) IngestEvent(_ context.Context, _ string, event domain.StripeWebhookEvent) (domain.StripeWebhookEvent, bool, error) {
	m.ingestCalls++
	if m.ingestErr != nil {
		return domain.StripeWebhookEvent{}, false, m.ingestErr
	}
	if m.events[event.StripeEventID] {
		return event, false, nil
	}
	m.events[event.StripeEventID] = true
	return event, true, nil
}

func (m *mockWebhookStore) ListByInvoice(_ context.Context, _, _ string) ([]domain.StripeWebhookEvent, error) {
	return nil, nil
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestChargeInvoice_Success(t *testing.T) {
	client := &mockStripeClient{piID: "pi_test_123"}
	invoices := newMockInvoiceUpdater()
	webhooks := newMockWebhookStore()

	inv := domain.Invoice{
		ID: "inv_1", TenantID: "t1", CustomerID: "cus_1",
		InvoiceNumber: "VLX-202604-0001",
		Status:        domain.InvoiceFinalized, PaymentStatus: domain.PaymentPending,
		Currency: "USD", AmountDueCents: 19900,
	}
	invoices.invoices["inv_1"] = inv

	stripe := NewStripe(client, invoices, webhooks, nil)
	result, err := stripe.ChargeInvoice(context.Background(), "t1", inv, "cus_stripe_abc", "pm_test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify PaymentIntent params
	if client.lastParams.AmountCents != 19900 {
		t.Errorf("amount: got %d, want 19900", client.lastParams.AmountCents)
	}
	if client.lastParams.CustomerID != "cus_stripe_abc" {
		t.Errorf("customer: got %q, want cus_stripe_abc", client.lastParams.CustomerID)
	}
	// The exact PM resolved upstream must be threaded into the PaymentIntent —
	// Velox names the card, Stripe does not pick. (Single-source-of-truth PM
	// selection: the card gated on is the card charged.)
	if client.lastParams.PaymentMethodID != "pm_test" {
		t.Errorf("payment method: got %q, want pm_test", client.lastParams.PaymentMethodID)
	}
	if client.lastParams.Currency != "USD" {
		t.Errorf("currency: got %q, want USD", client.lastParams.Currency)
	}
	if client.lastParams.Metadata["velox_invoice_id"] != "inv_1" {
		t.Errorf("metadata missing velox_invoice_id")
	}

	// Verify invoice updated to processing
	if result.PaymentStatus != domain.PaymentProcessing {
		t.Errorf("payment_status: got %q, want processing", result.PaymentStatus)
	}
	if result.StripePaymentIntentID != "pi_test_123" {
		t.Errorf("stripe_pi: got %q, want pi_test_123", result.StripePaymentIntentID)
	}
}

// A charge with no resolved PaymentMethod must fail loud and never reach
// Stripe — the engine's charge gates require a default PM before calling,
// so an empty PM id here is a wiring bug, not a cue to let Stripe pick a
// card. Guards against regressing back to implicit Stripe-side selection.
func TestChargeInvoice_RequiresPaymentMethod(t *testing.T) {
	client := &mockStripeClient{piID: "pi_should_not_be_used"}
	invoices := newMockInvoiceUpdater()
	webhooks := newMockWebhookStore()

	inv := domain.Invoice{
		ID: "inv_1", TenantID: "t1", CustomerID: "cus_1",
		InvoiceNumber: "VLX-202604-0001",
		Status:        domain.InvoiceFinalized, PaymentStatus: domain.PaymentPending,
		Currency: "USD", AmountDueCents: 19900,
	}
	invoices.invoices["inv_1"] = inv

	stripe := NewStripe(client, invoices, webhooks, nil)
	_, err := stripe.ChargeInvoice(context.Background(), "t1", inv, "cus_stripe_abc", "")
	if err == nil {
		t.Fatal("expected error for empty payment method id, got nil")
	}
	// The Stripe call must not have been made.
	if client.lastParams.CustomerID != "" {
		t.Errorf("Stripe was called despite missing PM: lastParams=%+v", client.lastParams)
	}
}

func TestChargeInvoice_NotFinalized(t *testing.T) {
	stripe := NewStripe(&mockStripeClient{}, newMockInvoiceUpdater(), newMockWebhookStore(), nil)

	inv := domain.Invoice{Status: domain.InvoiceDraft, AmountDueCents: 100}
	_, err := stripe.ChargeInvoice(context.Background(), "t1", inv, "cus_stripe", "pm_test")
	if err == nil {
		t.Fatal("expected error for non-finalized invoice")
	}
}

func TestChargeInvoice_ZeroAmount(t *testing.T) {
	stripe := NewStripe(&mockStripeClient{}, newMockInvoiceUpdater(), newMockWebhookStore(), nil)

	inv := domain.Invoice{Status: domain.InvoiceFinalized, AmountDueCents: 0}
	_, err := stripe.ChargeInvoice(context.Background(), "t1", inv, "cus_stripe", "pm_test")
	if err == nil {
		t.Fatal("expected error for zero amount")
	}
}

func TestChargeInvoice_StripeFailure(t *testing.T) {
	client := &mockStripeClient{shouldFail: true}
	invoices := newMockInvoiceUpdater()
	invoices.invoices["inv_1"] = domain.Invoice{
		ID: "inv_1", TenantID: "t1", Status: domain.InvoiceFinalized,
		AmountDueCents: 5000, Currency: "USD",
	}

	stripe := NewStripe(client, invoices, newMockWebhookStore(), nil)
	_, err := stripe.ChargeInvoice(context.Background(), "t1", invoices.invoices["inv_1"], "cus_stripe", "pm_test")
	if err == nil {
		t.Fatal("expected error when Stripe fails")
	}

	// Definitive failure (card decline) → PaymentFailed.
	updated := invoices.invoices["inv_1"]
	if updated.PaymentStatus != domain.PaymentFailed {
		t.Errorf("payment_status: got %q, want failed", updated.PaymentStatus)
	}
}

// recordingDunningStarter captures StartDunning calls for assertion.
// Returns a stub run — the test asserts on what was passed, not on
// the dunning state machine's downstream behaviour (that's covered
// by internal/dunning tests).
type recordingDunningStarter struct {
	calls []recordedStartDunning
}

type recordedStartDunning struct {
	tenantID   string
	invoiceID  string
	customerID string
	failureAt  time.Time
}

func (r *recordingDunningStarter) StartDunning(_ context.Context, tenantID, invoiceID, customerID string, failureAt time.Time, cause domain.DunningStartCause) (domain.InvoiceDunningRun, error) {
	r.calls = append(r.calls, recordedStartDunning{tenantID, invoiceID, customerID, failureAt})
	return domain.InvoiceDunningRun{ID: "drun_test", InvoiceID: invoiceID}, nil
}

// TestChargeInvoice_DefiniteFailure_InlineStartsDunning locks in the
// Phase 3 → Phase 5 invariant from ADR-035: when ChargeInvoice gets a
// definitively failed PaymentIntent error (card declined, not the
// ambiguous unknown/timeout case), it MUST inline-call StartDunning so
// the run exists by the time the catchup orchestrator's Phase 5
// queries due runs in the same Advance. Without this, a future
// refactor that reverts to webhook-only StartDunning silently brings
// back the race where Phase 5 fires zero retries because the run
// hasn't been created yet.
func TestChargeInvoice_DefiniteFailure_InlineStartsDunning(t *testing.T) {
	client := &mockStripeClient{
		shouldFail: true,
		failErr: &PaymentError{
			Message:         "card_declined",
			PaymentIntentID: "pi_decline",
			Unknown:         false, // DEFINITIVE failure
		},
	}
	invoices := newMockInvoiceUpdater()
	cycleClose := time.Date(2024, 5, 1, 0, 0, 0, 0, time.UTC)
	cycleEnd := cycleClose
	cycleStart := cycleEnd.Add(-30 * 24 * time.Hour)
	invoices.invoices["inv_1"] = domain.Invoice{
		ID: "inv_1", TenantID: "t1", CustomerID: "cus_1",
		Status:             domain.InvoiceFinalized,
		AmountDueCents:     5000,
		Currency:           "USD",
		IssuedAt:           &cycleEnd,
		BillingPeriodStart: cycleStart,
		BillingPeriodEnd:   cycleEnd,
	}

	dunning := &recordingDunningStarter{}
	stripe := NewStripe(client, invoices, newMockWebhookStore(), nil, dunning)

	_, err := stripe.ChargeInvoice(context.Background(), "t1", invoices.invoices["inv_1"], "cus_stripe_abc", "pm_test")
	if err == nil {
		t.Fatal("expected definitive failure error")
	}

	if len(dunning.calls) != 1 {
		t.Fatalf("StartDunning calls: got %d, want 1 (inline trigger on definitive failure — closes Phase 3 → Phase 5 webhook race)", len(dunning.calls))
	}
	call := dunning.calls[0]
	if call.tenantID != "t1" || call.invoiceID != "inv_1" || call.customerID != "cus_1" {
		t.Errorf("StartDunning args: got %+v, want {t1, inv_1, cus_1, ...}", call)
	}
	if !call.failureAt.Equal(cycleEnd) {
		t.Errorf("failureAt: got %v, want %v (= invoice cycle close — anchors dunning chain on simulated time, not advance-end frozen_time)", call.failureAt, cycleEnd)
	}
}

// TestChargeInvoice_UnknownOutcome_NoInlineDunning locks in the
// counterpart invariant: ambiguous failures (5xx/timeout/network) must
// NOT trigger inline StartDunning — the reconciler resolves them after
// the cool-off window, and starting dunning on an unknown outcome
// could fire retries against a charge that actually succeeded.
func TestChargeInvoice_UnknownOutcome_NoInlineDunning(t *testing.T) {
	client := &mockStripeClient{
		shouldFail: true,
		failErr: &PaymentError{
			Message:         "upstream timeout",
			PaymentIntentID: "pi_ambig",
			Unknown:         true,
		},
	}
	invoices := newMockInvoiceUpdater()
	invoices.invoices["inv_1"] = domain.Invoice{
		ID: "inv_1", TenantID: "t1", CustomerID: "cus_1",
		Status:         domain.InvoiceFinalized,
		AmountDueCents: 5000,
		Currency:       "USD",
	}

	dunning := &recordingDunningStarter{}
	stripe := NewStripe(client, invoices, newMockWebhookStore(), nil, dunning)

	_, _ = stripe.ChargeInvoice(context.Background(), "t1", invoices.invoices["inv_1"], "cus_stripe_abc", "pm_test")

	if len(dunning.calls) != 0 {
		t.Errorf("StartDunning calls on ambiguous outcome: got %d, want 0 (reconciler resolves Unknown later)", len(dunning.calls))
	}
}

func TestChargeInvoice_UnknownOutcome(t *testing.T) {
	// Simulates a 5xx / timeout / network drop: the charge may or may not
	// have been processed by Stripe. Marking the invoice failed and retrying
	// here would double-bill the customer.
	client := &mockStripeClient{
		shouldFail: true,
		failErr: &PaymentError{
			Message:         "stripe 500: upstream timeout",
			PaymentIntentID: "pi_partial_999",
			Unknown:         true,
		},
	}
	invoices := newMockInvoiceUpdater()
	invoices.invoices["inv_1"] = domain.Invoice{
		ID: "inv_1", TenantID: "t1", Status: domain.InvoiceFinalized,
		AmountDueCents: 5000, Currency: "USD",
	}

	stripe := NewStripe(client, invoices, newMockWebhookStore(), nil)
	_, err := stripe.ChargeInvoice(context.Background(), "t1", invoices.invoices["inv_1"], "cus_stripe", "pm_test")
	if err == nil {
		t.Fatal("expected error on ambiguous Stripe outcome")
	}

	updated := invoices.invoices["inv_1"]
	if updated.PaymentStatus != domain.PaymentUnknown {
		t.Errorf("payment_status: got %q, want unknown", updated.PaymentStatus)
	}
	if updated.StripePaymentIntentID != "pi_partial_999" {
		t.Errorf("stripe_pi: got %q, want pi_partial_999 (preserved from ambiguous error so reconciler can query Stripe)", updated.StripePaymentIntentID)
	}
	if updated.LastPaymentError == "" {
		t.Error("last_payment_error should be recorded for operator visibility")
	}
}

func TestChargeInvoice_PlainErrorTreatedAsUnknown(t *testing.T) {
	// Any error that isn't a typed *PaymentError must fail safe to Unknown —
	// we cannot prove the request never reached Stripe.
	client := &mockStripeClient{shouldFail: true, failErr: fmt.Errorf("context deadline exceeded")}
	invoices := newMockInvoiceUpdater()
	invoices.invoices["inv_1"] = domain.Invoice{
		ID: "inv_1", TenantID: "t1", Status: domain.InvoiceFinalized,
		AmountDueCents: 5000, Currency: "USD",
	}

	stripe := NewStripe(client, invoices, newMockWebhookStore(), nil)
	_, err := stripe.ChargeInvoice(context.Background(), "t1", invoices.invoices["inv_1"], "cus_stripe", "pm_test")
	if err == nil {
		t.Fatal("expected error")
	}
	if got := invoices.invoices["inv_1"].PaymentStatus; got != domain.PaymentUnknown {
		t.Errorf("payment_status: got %q, want unknown (untyped errors must fail safe)", got)
	}
}

func TestHandleWebhook_PaymentSucceeded(t *testing.T) {
	invoices := newMockInvoiceUpdater()
	invoices.invoices["inv_1"] = domain.Invoice{
		ID: "inv_1", TenantID: "t1", Status: domain.InvoiceFinalized,
		PaymentStatus:         domain.PaymentProcessing,
		StripePaymentIntentID: "pi_abc",
	}
	invoices.byPI["pi_abc"] = "inv_1"

	webhooks := newMockWebhookStore()
	stripe := NewStripe(&mockStripeClient{}, invoices, webhooks, nil)

	err := stripe.HandleWebhook(context.Background(), "t1", domain.StripeWebhookEvent{
		StripeEventID:   "evt_001",
		EventType:       "payment_intent.succeeded",
		PaymentIntentID: "pi_abc",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	inv := invoices.invoices["inv_1"]
	if inv.PaymentStatus != domain.PaymentSucceeded {
		t.Errorf("payment_status: got %q, want succeeded", inv.PaymentStatus)
	}
	if inv.Status != domain.InvoicePaid {
		t.Errorf("status: got %q, want paid", inv.Status)
	}
	if inv.PaidAt == nil {
		t.Error("paid_at should be set")
	}
}

func TestHandleWebhook_PaymentFailed(t *testing.T) {
	invoices := newMockInvoiceUpdater()
	invoices.invoices["inv_1"] = domain.Invoice{
		ID: "inv_1", TenantID: "t1", Status: domain.InvoiceFinalized,
		PaymentStatus:         domain.PaymentProcessing,
		StripePaymentIntentID: "pi_def",
	}
	invoices.byPI["pi_def"] = "inv_1"

	stripe := NewStripe(&mockStripeClient{}, invoices, newMockWebhookStore(), nil)

	err := stripe.HandleWebhook(context.Background(), "t1", domain.StripeWebhookEvent{
		StripeEventID:   "evt_002",
		EventType:       "payment_intent.payment_failed",
		PaymentIntentID: "pi_def",
		FailureMessage:  "Your card was declined.",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	inv := invoices.invoices["inv_1"]
	if inv.PaymentStatus != domain.PaymentFailed {
		t.Errorf("payment_status: got %q, want failed", inv.PaymentStatus)
	}
	if inv.LastPaymentError != "Your card was declined." {
		t.Errorf("error: got %q, want 'Your card was declined.'", inv.LastPaymentError)
	}
}

// TestHandleWebhook_PaymentFailed_IgnoredForPaidInvoice covers the pass-3
// medium audit finding: an out-of-order payment_intent.payment_failed arriving
// after an invoice is already paid must not corrupt it (flip to failed, null
// paid_at, relink stale PI, start dunning). It's a no-op.
func TestHandleWebhook_PaymentFailed_IgnoredForPaidInvoice(t *testing.T) {
	paidAt := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		inv  domain.Invoice
	}{
		{"status paid", domain.Invoice{
			ID: "inv_1", TenantID: "t1", Status: domain.InvoicePaid,
			PaymentStatus: domain.PaymentSucceeded, PaidAt: &paidAt,
			StripePaymentIntentID: "pi_ok",
		}},
		{"payment succeeded, finalized", domain.Invoice{
			ID: "inv_1", TenantID: "t1", Status: domain.InvoiceFinalized,
			PaymentStatus: domain.PaymentSucceeded, PaidAt: &paidAt,
			StripePaymentIntentID: "pi_ok",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			invoices := newMockInvoiceUpdater()
			invoices.invoices["inv_1"] = tc.inv
			invoices.byPI["pi_stale"] = "inv_1"

			stripe := NewStripe(&mockStripeClient{}, invoices, newMockWebhookStore(), nil)

			err := stripe.HandleWebhook(context.Background(), "t1", domain.StripeWebhookEvent{
				StripeEventID:   "evt_ooo",
				EventType:       "payment_intent.payment_failed",
				PaymentIntentID: "pi_stale",
				FailureMessage:  "Your card was declined.",
			})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			got := invoices.invoices["inv_1"]
			if got.PaymentStatus != domain.PaymentSucceeded {
				t.Errorf("payment_status: got %q, want succeeded (out-of-order failure ignored)", got.PaymentStatus)
			}
			if got.PaidAt == nil {
				t.Error("paid_at was nulled by an out-of-order failure")
			}
			if got.StripePaymentIntentID != "pi_ok" {
				t.Errorf("stripe_payment_intent_id: got %q, want pi_ok (stale PI must not relink)", got.StripePaymentIntentID)
			}
		})
	}
}

func TestHandleWebhook_DuplicateEvent(t *testing.T) {
	invoices := newMockInvoiceUpdater()
	invoices.invoices["inv_1"] = domain.Invoice{
		ID: "inv_1", TenantID: "t1", Status: domain.InvoiceFinalized,
		StripePaymentIntentID: "pi_dup",
	}
	invoices.byPI["pi_dup"] = "inv_1"

	webhooks := newMockWebhookStore()
	stripe := NewStripe(&mockStripeClient{}, invoices, webhooks, nil)

	event := domain.StripeWebhookEvent{
		StripeEventID:   "evt_dup",
		EventType:       "payment_intent.succeeded",
		PaymentIntentID: "pi_dup",
	}

	// First call — processes
	err := stripe.HandleWebhook(context.Background(), "t1", event)
	if err != nil {
		t.Fatalf("first call failed: %v", err)
	}

	// Reset invoice to verify second call doesn't change it
	inv := invoices.invoices["inv_1"]
	inv.PaymentStatus = domain.PaymentProcessing
	inv.Status = domain.InvoiceFinalized
	invoices.invoices["inv_1"] = inv

	// Second call — should be idempotent (duplicate event)
	err = stripe.HandleWebhook(context.Background(), "t1", event)
	if err != nil {
		t.Fatalf("second call failed: %v", err)
	}

	// Invoice should NOT have been updated again
	inv = invoices.invoices["inv_1"]
	if inv.PaymentStatus != domain.PaymentProcessing {
		t.Errorf("duplicate event should not update: got %q", inv.PaymentStatus)
	}
}

func TestHandleWebhook_UnhandledEvent(t *testing.T) {
	stripe := NewStripe(&mockStripeClient{}, newMockInvoiceUpdater(), newMockWebhookStore(), nil)

	err := stripe.HandleWebhook(context.Background(), "t1", domain.StripeWebhookEvent{
		StripeEventID: "evt_other",
		EventType:     "charge.refunded",
	})
	if err != nil {
		t.Fatalf("unhandled events should not error: %v", err)
	}
}

// TestHandleWebhook_TransientFailureNotConsumed is the unit-level regression
// guard for the silent-drop bug. A transient processing failure (here: a DB
// blip on MarkPaid surfaced via a missing-then-present invoice) must:
//   - return the error to the caller (so the HTTP layer responds 5xx), and
//   - NOT write the dedup row (so Stripe's redelivery re-processes).
//
// Before the fix the dedup row was committed BEFORE the business write, so a
// failed write left the event marked consumed and the redelivery was dropped.
func TestHandleWebhook_TransientFailureNotConsumed(t *testing.T) {
	invoices := newMockInvoiceUpdater()
	invoices.invoices["inv_1"] = domain.Invoice{
		ID: "inv_1", TenantID: "t1", Status: domain.InvoiceFinalized,
		PaymentStatus:         domain.PaymentProcessing,
		StripePaymentIntentID: "pi_transient",
	}
	invoices.byPI["pi_transient"] = "inv_1"

	webhooks := newMockWebhookStore()
	// Force the audit/dedup insert to fail transiently — stands in for any
	// retryable error on the persistence path.
	stripe := NewStripe(&mockStripeClient{}, &transientMarkPaidInvoices{mockInvoiceUpdater: invoices}, webhooks, nil)

	event := domain.StripeWebhookEvent{
		StripeEventID:   "evt_transient",
		EventType:       "payment_intent.succeeded",
		PaymentIntentID: "pi_transient",
	}

	err := stripe.HandleWebhook(context.Background(), "t1", event)
	if err == nil {
		t.Fatal("expected transient processing error to surface, got nil")
	}
	if webhooks.events["evt_transient"] {
		t.Fatal("transient failure must not mark the event consumed")
	}
	if webhooks.ingestCalls != 0 {
		t.Fatalf("transient failure must not write a dedup row, got %d IngestEvent calls", webhooks.ingestCalls)
	}

	// Stripe redelivers; the blip has cleared. The event must re-process.
	invoices.invoices["inv_1"] = domain.Invoice{
		ID: "inv_1", TenantID: "t1", Status: domain.InvoiceFinalized,
		PaymentStatus:         domain.PaymentProcessing,
		StripePaymentIntentID: "pi_transient",
	}
	invoices.byPI["pi_transient"] = "inv_1"
	stripe = NewStripe(&mockStripeClient{}, invoices, webhooks, nil)
	if err := stripe.HandleWebhook(context.Background(), "t1", event); err != nil {
		t.Fatalf("redelivery should succeed: %v", err)
	}
	if !webhooks.events["evt_transient"] {
		t.Fatal("successful redelivery must record the dedup row")
	}
	if got := invoices.invoices["inv_1"].PaymentStatus; got != domain.PaymentSucceeded {
		t.Fatalf("redelivery should settle the invoice: got %q, want succeeded", got)
	}
}

// transientMarkPaidInvoices wraps the mock to make the settle write fail
// with a transient (non-ErrNotFound) error. SettleSucceeded calls
// MarkPaidReportingTransition, so that's the override that matters; the
// plain MarkPaid is overridden too for any path that still uses it.
type transientMarkPaidInvoices struct {
	*mockInvoiceUpdater
}

func (t *transientMarkPaidInvoices) MarkPaidReportingTransition(_ context.Context, _, _ string, _ string, _ time.Time) (domain.Invoice, bool, error) {
	return domain.Invoice{}, false, fmt.Errorf("connection reset by peer")
}

// MarkPaidCardSettlementTransition must ALSO fail transiently — SettleSucceeded
// now calls this variant, so the override has to be here too (else the embedded
// mock's success path would run).
func (t *transientMarkPaidInvoices) MarkPaidCardSettlementTransition(_ context.Context, _, _ string, _ string, _ time.Time) (domain.Invoice, bool, error) {
	return domain.Invoice{}, false, fmt.Errorf("connection reset by peer")
}

func (t *transientMarkPaidInvoices) MarkPaid(_ context.Context, _, _ string, _ string, _ time.Time) (domain.Invoice, error) {
	return domain.Invoice{}, fmt.Errorf("connection reset by peer")
}

// TestHandleWebhook_PermanentFailureAcked verifies the other half of the
// classification: an event referencing an invoice that isn't in our DB
// (errs.ErrNotFound) is permanent — redelivering it forever is pointless — so
// HandleWebhook acks (returns nil) and records the audit/dedup row.
func TestHandleWebhook_PermanentFailureAcked(t *testing.T) {
	invoices := newMockInvoiceUpdater() // empty — no such invoice
	webhooks := newMockWebhookStore()
	stripe := NewStripe(&mockStripeClient{}, invoices, webhooks, nil)

	event := domain.StripeWebhookEvent{
		StripeEventID:   "evt_orphan",
		EventType:       "payment_intent.succeeded",
		PaymentIntentID: "pi_orphan",
	}

	if err := stripe.HandleWebhook(context.Background(), "t1", event); err != nil {
		t.Fatalf("permanent failure should be ack'd (nil), got: %v", err)
	}
	if !webhooks.events["evt_orphan"] {
		t.Fatal("permanently-unprocessable event should still be recorded for audit + dedup")
	}
}

// recordingAttacher captures calls so the test can assert the attacher was
// invoked with the right (tenant, customer, pm) tuple.
type recordingAttacher struct {
	tenantID, customerID, pmID string
	called                     int
	err                        error
	// actorType/actorID capture what audit.ResolveActor would attribute the
	// attach to, from the ctx the handler hands us — so a test can assert
	// customer-driven attaches carry the customer actor and operator ones
	// stay system.
	actorType, actorID string
}

func (r *recordingAttacher) AttachForWebhook(ctx context.Context, tenantID, customerID, pmID string) error {
	r.called++
	r.tenantID, r.customerID, r.pmID = tenantID, customerID, pmID
	r.actorType, r.actorID = audit.ResolveActor(ctx)
	return r.err
}

// TestHandleWebhook_SetupIntentSucceeded — a setup_intent.succeeded event
// must re-parse the raw payload, pull payment_method + velox_customer_id
// from it, and delegate to the configured attacher.
func TestHandleWebhook_SetupIntentSucceeded(t *testing.T) {
	stripe := NewStripe(&mockStripeClient{}, newMockInvoiceUpdater(), newMockWebhookStore(), nil)
	attacher := &recordingAttacher{}
	stripe.SetPaymentMethodAttacher(attacher)

	rawPayload := `{
		"id": "evt_seti_ok",
		"type": "setup_intent.succeeded",
		"data": { "object": {
			"id": "seti_1",
			"payment_method": "pm_stripe_42",
			"customer": "cus_stripe_99",
			"metadata": {
				"velox_tenant_id": "tnt_x",
				"velox_customer_id": "cus_local_7"
			}
		}}
	}`

	err := stripe.HandleWebhook(context.Background(), "tnt_x", domain.StripeWebhookEvent{
		StripeEventID: "evt_seti_ok",
		EventType:     "setup_intent.succeeded",
		Payload:       map[string]any{"raw": rawPayload},
	})
	if err != nil {
		t.Fatalf("HandleWebhook: %v", err)
	}
	if attacher.called != 1 {
		t.Fatalf("expected attacher called once, got %d", attacher.called)
	}
	if attacher.tenantID != "tnt_x" || attacher.customerID != "cus_local_7" || attacher.pmID != "pm_stripe_42" {
		t.Fatalf("attacher got wrong args: tenant=%q customer=%q pm=%q",
			attacher.tenantID, attacher.customerID, attacher.pmID)
	}
	// No velox_purpose in this payload → operator/system attach, not customer.
	if attacher.actorType != "system" {
		t.Errorf("purpose-less attach must resolve to system actor, got %q/%q", attacher.actorType, attacher.actorID)
	}
}

// TestHandleWebhook_SetupIntent_CustomerActor — an attach whose SetupIntent
// carries velox_purpose=payment_update_token (the customer-driven public
// token link) must be attributed to the CUSTOMER in the audit log, so a
// self-served card is distinguishable from an operator-added one.
func TestHandleWebhook_SetupIntent_CustomerActor(t *testing.T) {
	stripe := NewStripe(&mockStripeClient{}, newMockInvoiceUpdater(), newMockWebhookStore(), nil)
	attacher := &recordingAttacher{}
	stripe.SetPaymentMethodAttacher(attacher)

	rawPayload := `{
		"id": "evt_seti_cust",
		"type": "setup_intent.succeeded",
		"data": { "object": {
			"id": "seti_2",
			"payment_method": "pm_stripe_cust",
			"customer": "cus_stripe_99",
			"metadata": {
				"velox_tenant_id": "tnt_x",
				"velox_customer_id": "cus_local_7",
				"velox_purpose": "payment_update_token"
			}
		}}
	}`

	if err := stripe.HandleWebhook(context.Background(), "tnt_x", domain.StripeWebhookEvent{
		StripeEventID: "evt_seti_cust",
		EventType:     "setup_intent.succeeded",
		Payload:       map[string]any{"raw": rawPayload},
	}); err != nil {
		t.Fatalf("HandleWebhook: %v", err)
	}
	if attacher.actorType != "customer" || attacher.actorID != "cus_local_7" {
		t.Errorf("customer-driven attach must resolve to customer actor cus_local_7, got %q/%q",
			attacher.actorType, attacher.actorID)
	}
}

// TestHandleWebhook_SetupIntent_OperatorPurposeStaysSystem — the operator
// "Add payment method" flow (velox_purpose=portal_add_payment_method) must
// NOT be attributed to the customer.
func TestHandleWebhook_SetupIntent_OperatorPurposeStaysSystem(t *testing.T) {
	stripe := NewStripe(&mockStripeClient{}, newMockInvoiceUpdater(), newMockWebhookStore(), nil)
	attacher := &recordingAttacher{}
	stripe.SetPaymentMethodAttacher(attacher)

	rawPayload := `{
		"id": "evt_seti_op",
		"type": "setup_intent.succeeded",
		"data": { "object": {
			"id": "seti_3",
			"payment_method": "pm_stripe_op",
			"customer": "cus_stripe_99",
			"metadata": {
				"velox_tenant_id": "tnt_x",
				"velox_customer_id": "cus_local_7",
				"velox_purpose": "portal_add_payment_method"
			}
		}}
	}`

	if err := stripe.HandleWebhook(context.Background(), "tnt_x", domain.StripeWebhookEvent{
		StripeEventID: "evt_seti_op",
		EventType:     "setup_intent.succeeded",
		Payload:       map[string]any{"raw": rawPayload},
	}); err != nil {
		t.Fatalf("HandleWebhook: %v", err)
	}
	if attacher.actorType != "system" {
		t.Errorf("operator-purpose attach must stay system, got %q/%q", attacher.actorType, attacher.actorID)
	}
}

// TestHandleWebhook_SetupIntent_NoAttacherNoError — if the operator hasn't
// wired an attacher (e.g. test env without the paymentmethods package),
// setup_intent events should ack silently rather than error.
func TestHandleWebhook_SetupIntent_NoAttacher(t *testing.T) {
	stripe := NewStripe(&mockStripeClient{}, newMockInvoiceUpdater(), newMockWebhookStore(), nil)

	err := stripe.HandleWebhook(context.Background(), "tnt_x", domain.StripeWebhookEvent{
		StripeEventID: "evt_noop",
		EventType:     "setup_intent.succeeded",
		Payload:       map[string]any{"raw": `{"data":{"object":{}}}`},
	})
	if err != nil {
		t.Fatalf("no-attacher path should be a no-op, got %v", err)
	}
}

// TestHandleWebhook_SetupIntent_MissingMetadata — a setup_intent with no
// velox metadata AND no resolvable customer shouldn't error (someone else's
// SI passing through); we just skip.
// TestHandleWebhook_SetupIntent_UnresolvedCustomerRedelivers locks the
// dropped-card fix: a setup_intent.succeeded that HAS a payment method but
// whose customer can't be resolved (no metadata + resolver NotFound — the
// customer↔Stripe-id link isn't written yet) must REDELIVER, not be acked.
// Pre-fix it returned nil → the event was dedup-marked → Stripe never
// redelivered → the saved card was permanently dropped and the invoice
// looped in auto-charge retry forever.
func TestHandleWebhook_SetupIntent_UnresolvedCustomerRedelivers(t *testing.T) {
	store := newMockWebhookStore()
	stripe := NewStripe(&mockStripeClient{}, newMockInvoiceUpdater(), store, nil)
	attacher := &recordingAttacher{}
	stripe.SetPaymentMethodAttacher(attacher)
	// Resolver can't place cus_foreign (link not yet written) → NotFound.
	stripe.SetCustomerResolver(&recordingCustomerResolver{wantStripeID: "cus_known", veloxID: "cus_x"})

	err := stripe.HandleWebhook(context.Background(), "tnt_x", domain.StripeWebhookEvent{
		StripeEventID: "evt_unresolved",
		EventType:     "setup_intent.succeeded",
		Payload: map[string]any{"raw": `{
			"data": {"object": {"id": "seti_2", "payment_method": "pm_1", "customer": "cus_foreign", "metadata": {}}}
		}`},
	})
	if err == nil {
		t.Fatal("unresolved-customer setup_intent with a card must redeliver, got nil (would dedup-drop the card)")
	}
	if !isTransientWebhookError(err) {
		t.Fatalf("error must be transient (redeliver), got %v", err)
	}
	if attacher.called != 0 {
		t.Fatalf("attacher must not be called when the customer is unresolved")
	}
	if store.ingestCalls != 0 {
		t.Fatalf("no dedup row may be written on a redeliverable miss (else the redelivery short-circuits); ingestCalls=%d", store.ingestCalls)
	}
}

// TestHandleWebhook_SetupIntent_NoPaymentMethodAcks verifies the genuinely-
// nothing-to-do case (no payment_method on the SetupIntent) is acked, not
// redelivered — there's no card to attach.
func TestHandleWebhook_SetupIntent_NoPaymentMethodAcks(t *testing.T) {
	store := newMockWebhookStore()
	stripe := NewStripe(&mockStripeClient{}, newMockInvoiceUpdater(), store, nil)
	attacher := &recordingAttacher{}
	stripe.SetPaymentMethodAttacher(attacher)

	err := stripe.HandleWebhook(context.Background(), "tnt_x", domain.StripeWebhookEvent{
		StripeEventID: "evt_no_pm",
		EventType:     "setup_intent.succeeded",
		Payload: map[string]any{"raw": `{
			"data": {"object": {"id": "seti_2", "metadata": {"velox_customer_id": "cus_7"}}}
		}`},
	})
	if err != nil {
		t.Fatalf("no-payment-method setup_intent should ack, got %v", err)
	}
	if attacher.called != 0 {
		t.Fatalf("attacher must not be called with no payment method")
	}
	if store.ingestCalls != 1 {
		t.Fatalf("a no-op ack should still record the dedup row; ingestCalls=%d", store.ingestCalls)
	}
}

// recordingCustomerResolver is a fake CustomerByStripeIDResolver. Maps one
// Stripe customer id → velox id; everything else is NotFound.
type recordingCustomerResolver struct {
	wantStripeID string
	veloxID      string
	called       int
	err          error
}

func (r *recordingCustomerResolver) CustomerIDByStripeID(_ context.Context, _ /*tenantID*/, stripeCustomerID string) (string, error) {
	r.called++
	if r.err != nil {
		return "", r.err
	}
	if stripeCustomerID == r.wantStripeID {
		return r.veloxID, nil
	}
	return "", errs.ErrNotFound
}

// TestHandleWebhook_SetupIntent_ResolvesCustomerByStripeID — the bug fix:
// a setup_intent.succeeded created by Stripe Checkout carries metadata on the
// session, NOT the SetupIntent, so velox_customer_id is absent. The handler
// must fall back to the SetupIntent's `customer` field, resolve it to the
// velox customer, and still attach the PM.
func TestHandleWebhook_SetupIntent_ResolvesCustomerByStripeID(t *testing.T) {
	stripe := NewStripe(&mockStripeClient{}, newMockInvoiceUpdater(), newMockWebhookStore(), nil)
	attacher := &recordingAttacher{}
	stripe.SetPaymentMethodAttacher(attacher)
	resolver := &recordingCustomerResolver{wantStripeID: "cus_stripe_99", veloxID: "cus_local_7"}
	stripe.SetCustomerResolver(resolver)

	// No metadata on the SetupIntent — only the authoritative `customer`.
	rawPayload := `{
		"id": "evt_seti_nometa",
		"type": "setup_intent.succeeded",
		"data": { "object": {
			"id": "seti_3",
			"payment_method": "pm_stripe_77",
			"customer": "cus_stripe_99",
			"metadata": {}
		}}
	}`

	err := stripe.HandleWebhook(context.Background(), "tnt_x", domain.StripeWebhookEvent{
		StripeEventID: "evt_seti_nometa",
		EventType:     "setup_intent.succeeded",
		Payload:       map[string]any{"raw": rawPayload},
	})
	if err != nil {
		t.Fatalf("HandleWebhook: %v", err)
	}
	if resolver.called != 1 {
		t.Fatalf("expected resolver called once, got %d", resolver.called)
	}
	if attacher.called != 1 {
		t.Fatalf("expected attacher called once, got %d", attacher.called)
	}
	if attacher.customerID != "cus_local_7" || attacher.pmID != "pm_stripe_77" {
		t.Fatalf("attacher got wrong args: customer=%q pm=%q", attacher.customerID, attacher.pmID)
	}
}

// TestHandleWebhook_SetupIntent_MetadataWinsOverResolver — when the SetupIntent
// DOES carry velox_customer_id (the paymentmethods portal flow sets it), the
// metadata fast-path is used and the resolver is never consulted.
func TestHandleWebhook_SetupIntent_MetadataWinsOverResolver(t *testing.T) {
	stripe := NewStripe(&mockStripeClient{}, newMockInvoiceUpdater(), newMockWebhookStore(), nil)
	attacher := &recordingAttacher{}
	stripe.SetPaymentMethodAttacher(attacher)
	resolver := &recordingCustomerResolver{wantStripeID: "cus_stripe_99", veloxID: "cus_should_not_be_used"}
	stripe.SetCustomerResolver(resolver)

	rawPayload := `{
		"data": { "object": {
			"id": "seti_4",
			"payment_method": "pm_meta",
			"customer": "cus_stripe_99",
			"metadata": { "velox_customer_id": "cus_from_metadata" }
		}}
	}`

	err := stripe.HandleWebhook(context.Background(), "tnt_x", domain.StripeWebhookEvent{
		StripeEventID: "evt_seti_meta_wins",
		EventType:     "setup_intent.succeeded",
		Payload:       map[string]any{"raw": rawPayload},
	})
	if err != nil {
		t.Fatalf("HandleWebhook: %v", err)
	}
	if resolver.called != 0 {
		t.Fatalf("resolver must not be consulted when metadata is present, called %d", resolver.called)
	}
	if attacher.customerID != "cus_from_metadata" {
		t.Fatalf("metadata customer id should win, got %q", attacher.customerID)
	}
}

// TestHandleWebhook_SetupIntent_ResolverTransientErrorRedelivers — a
// non-NotFound resolver error (e.g. DB blip) must surface so the HTTP layer
// 5xxs and Stripe redelivers, rather than silently dropping a saved card.
func TestHandleWebhook_SetupIntent_ResolverTransientErrorRedelivers(t *testing.T) {
	stripe := NewStripe(&mockStripeClient{}, newMockInvoiceUpdater(), newMockWebhookStore(), nil)
	attacher := &recordingAttacher{}
	stripe.SetPaymentMethodAttacher(attacher)
	resolver := &recordingCustomerResolver{err: errors.New("db unavailable")}
	stripe.SetCustomerResolver(resolver)

	err := stripe.HandleWebhook(context.Background(), "tnt_x", domain.StripeWebhookEvent{
		StripeEventID: "evt_seti_dberr",
		EventType:     "setup_intent.succeeded",
		Payload: map[string]any{"raw": `{
			"data": {"object": {"id": "seti_5", "payment_method": "pm_x", "customer": "cus_y", "metadata": {}}}
		}`},
	})
	if err == nil {
		t.Fatal("expected transient resolver error to surface for redelivery")
	}
	if attacher.called != 0 {
		t.Fatalf("attacher must not run when customer resolution failed")
	}
}

// TestHandleWebhook_SetupIntentFailed — setup_intent.setup_failed is
// logged and ack'd; nothing else needs to happen.
func TestHandleWebhook_SetupIntentFailed(t *testing.T) {
	stripe := NewStripe(&mockStripeClient{}, newMockInvoiceUpdater(), newMockWebhookStore(), nil)
	attacher := &recordingAttacher{}
	stripe.SetPaymentMethodAttacher(attacher)

	err := stripe.HandleWebhook(context.Background(), "tnt_x", domain.StripeWebhookEvent{
		StripeEventID:  "evt_seti_fail",
		EventType:      "setup_intent.setup_failed",
		FailureMessage: "card declined",
		Payload:        map[string]any{"raw": `{"data":{"object":{}}}`},
	})
	if err != nil {
		t.Fatalf("setup_failed should ack: %v", err)
	}
	if attacher.called != 0 {
		t.Fatalf("setup_failed must not attach anything")
	}
}
