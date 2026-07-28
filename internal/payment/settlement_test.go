package payment

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// recordingDunningResolver records ResolveByInvoice calls (and can inject an error)
// so the card-success dunning-resolve path can be asserted.
type dunningResolveCall struct {
	invoiceID  string
	resolution domain.DunningResolution
}

type recordingDunningResolver struct {
	calls []dunningResolveCall
	err   error
}

func (r *recordingDunningResolver) ResolveByInvoice(_ context.Context, _, invoiceID string, resolution domain.DunningResolution) error {
	r.calls = append(r.calls, dunningResolveCall{invoiceID, resolution})
	return r.err
}

// TestSettleSucceeded_ResolvesDunningRun locks the #317 card-success symmetry: when
// a card payment settles a finalized invoice, any active dunning run is resolved
// (payment_recovered) IMMEDIATELY — not left active until the dunning sweep's
// paid-pre-check floor catches it on the next tick.
func TestSettleSucceeded_ResolvesDunningRun(t *testing.T) {
	invoices := newMockInvoiceUpdater()
	invoices.invoices["inv_1"] = domain.Invoice{
		ID: "inv_1", TenantID: "t1", Status: domain.InvoiceFinalized,
		PaymentStatus: domain.PaymentProcessing, StripePaymentIntentID: "pi_abc",
	}
	invoices.byPI["pi_abc"] = "inv_1"
	resolver := &recordingDunningResolver{}
	s := NewStripe(&mockStripeClient{}, invoices, newMockWebhookStore(), nil)
	s.SetDunningResolver(resolver)

	if err := s.SettleSucceeded(context.Background(), "t1", invoices.invoices["inv_1"], "pi_abc", 0, SourceWebhook); err != nil {
		t.Fatalf("SettleSucceeded: %v", err)
	}
	if len(resolver.calls) != 1 {
		t.Fatalf("ResolveByInvoice calls: got %d, want 1 (a card success must resolve the active dunning run)", len(resolver.calls))
	}
	if resolver.calls[0].invoiceID != "inv_1" {
		t.Errorf("resolved invoice: got %q, want inv_1", resolver.calls[0].invoiceID)
	}
	if resolver.calls[0].resolution != domain.ResolutionPaymentRecovered {
		t.Errorf("resolution: got %q, want %q", resolver.calls[0].resolution, domain.ResolutionPaymentRecovered)
	}
}

// TestSettleSucceeded_ResolverErrorDoesNotFailSettle: the resolve is best-effort —
// a resolver failure must NOT fail the settle (the dunning sweep's paid-pre-check
// floor still resolves the run on the next tick).
func TestSettleSucceeded_ResolverErrorDoesNotFailSettle(t *testing.T) {
	invoices := newMockInvoiceUpdater()
	invoices.invoices["inv_1"] = domain.Invoice{
		ID: "inv_1", TenantID: "t1", Status: domain.InvoiceFinalized,
		PaymentStatus: domain.PaymentProcessing, StripePaymentIntentID: "pi_abc",
	}
	invoices.byPI["pi_abc"] = "inv_1"
	resolver := &recordingDunningResolver{err: errors.New("dunning store blip")}
	s := NewStripe(&mockStripeClient{}, invoices, newMockWebhookStore(), nil)
	s.SetDunningResolver(resolver)

	if err := s.SettleSucceeded(context.Background(), "t1", invoices.invoices["inv_1"], "pi_abc", 0, SourceWebhook); err != nil {
		t.Fatalf("SettleSucceeded must succeed even when the resolver errors (best-effort): %v", err)
	}
	if invoices.invoices["inv_1"].Status != domain.InvoicePaid {
		t.Error("invoice must still be marked paid despite the resolver error")
	}
}

// The webhook tests (stripe_test.go) already pin the primitive via the webhook
// entry point. These exercise it DIRECTLY — calling SettleSucceeded /
// SettleFailed with a non-webhook source — to lock the ADR-049 contract that
// the side-effects are source-independent. This is the foundation Phase 2
// relies on: the reconciler will call these same methods, so a backstop-
// recovered settlement must fire the SAME consequences (dunning, mark) as the
// webhook, and inherit the out-of-order guard.

// recordingEventDispatcher counts outbound events by type.
type recordingEventDispatcher struct{ byType map[string]int }

func (r *recordingEventDispatcher) Dispatch(_ context.Context, _, eventType string, _ map[string]any) error {
	if r.byType == nil {
		r.byType = map[string]int{}
	}
	r.byType[eventType]++
	return nil
}

// recordingReceiptEmail counts payment-receipt enqueues.
type recordingReceiptEmail struct{ sends int }

func (r *recordingReceiptEmail) SendPaymentReceipt(_ context.Context, _, _ string, _ []string, _, _ string, _ int64, _, _ string) error {
	r.sends++
	return nil
}

// recordingFailedEmail counts payment-failed enqueues.
type recordingFailedEmail struct{ sends int }

func (r *recordingFailedEmail) SendPaymentFailed(_ context.Context, _, _ string, _ []string, _, _, _, _ string) error {
	r.sends++
	return nil
}

type staticCustomerEmail struct{}

func (staticCustomerEmail) GetCustomerEmail(_ context.Context, _, _ string) (string, string, []string, error) {
	return "buyer@example.com", "Buyer", nil, nil
}

// TestSettleSucceeded_ConcurrentRedeliveryFiresSideEffectsOnce locks the H7
// fix: two at-least-once deliveries of the SAME payment_intent.succeeded that
// race — both fetch the invoice while it's still `processing`, so both carry a
// stale snapshot that slips past the fast-path already-paid guard — must settle
// the invoice once and fire the side-effects EXACTLY once: payment.succeeded is
// now enqueued IN-TX by the card-settlement transition, the receipt email
// post-commit — both gated on the transition winner. MarkPaid's SELECT … FOR
// UPDATE serializes the two; only the transition-winner fires them. Pre-fix both
// fired, double-emailing the customer and double-firing the outbound webhook.
func TestSettleSucceeded_ConcurrentRedeliveryFiresSideEffectsOnce(t *testing.T) {
	invoices := newMockInvoiceUpdater()
	invoices.invoices["inv_1"] = domain.Invoice{
		ID: "inv_1", TenantID: "t1", CustomerID: "cus_1", InvoiceNumber: "VLX-1",
		Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentProcessing,
		StripePaymentIntentID: "pi_abc", TotalAmountCents: 2900, Currency: "USD",
	}
	invoices.byPI["pi_abc"] = "inv_1"

	events := &recordingEventDispatcher{}
	email := &recordingReceiptEmail{}
	s := NewStripe(&mockStripeClient{}, invoices, newMockWebhookStore(), nil)
	s.SetEventDispatcher(events)
	s.SetEmailReceipt(email, staticCustomerEmail{})

	// The stale snapshot both racing deliveries hold: invoice still
	// `processing`, so it passes SettleSucceeded's already-paid
	// fast-path guard (the Status/PaymentStatus check at the top) each time. The
	// transition gate (MarkPaidReportingTransition) is what de-dupes them.
	stale := invoices.invoices["inv_1"]
	for i := 0; i < 2; i++ {
		if err := s.SettleSucceeded(context.Background(), "t1", stale, "pi_abc", 0, SourceWebhook); err != nil {
			t.Fatalf("settle attempt %d: %v", i+1, err)
		}
	}

	if got := invoices.invoices["inv_1"].PaymentStatus; got != domain.PaymentSucceeded {
		t.Fatalf("invoice not settled: payment_status=%q", got)
	}
	// payment.succeeded is now enqueued IN-TX by the card-settlement transition
	// (gated on transitioned), not via the post-commit dispatcher — so the in-tx
	// path fires it exactly once and the dispatcher sees it zero times.
	if got := invoices.cardEventEnqueues; got != 1 {
		t.Errorf("payment.succeeded enqueued in-tx %d times, want exactly 1 (concurrent redelivery must not double-fire)", got)
	}
	if got := events.byType[domain.EventPaymentSucceeded]; got != 0 {
		t.Errorf("payment.succeeded fired via the post-commit dispatcher %d times, want 0 (it moved in-tx)", got)
	}
	if email.sends != 1 {
		t.Errorf("receipt email enqueued %d times, want exactly 1 (no double-notify)", email.sends)
	}
}

func TestSettleSucceeded_MarksPaidFromAnySource(t *testing.T) {
	invoices := newMockInvoiceUpdater()
	invoices.invoices["inv_1"] = domain.Invoice{
		ID: "inv_1", TenantID: "t1", Status: domain.InvoiceFinalized,
		PaymentStatus: domain.PaymentProcessing, StripePaymentIntentID: "pi_abc",
	}
	invoices.byPI["pi_abc"] = "inv_1"

	s := NewStripe(&mockStripeClient{}, invoices, newMockWebhookStore(), nil)

	// Call as the reconciler would (not via the webhook).
	if err := s.SettleSucceeded(context.Background(), "t1", invoices.invoices["inv_1"], "pi_abc", 0, SourceReconciler); err != nil {
		t.Fatalf("SettleSucceeded: %v", err)
	}

	inv := invoices.invoices["inv_1"]
	if inv.PaymentStatus != domain.PaymentSucceeded || inv.Status != domain.InvoicePaid {
		t.Errorf("status: got payment=%q invoice=%q, want succeeded/paid", inv.PaymentStatus, inv.Status)
	}
	if inv.PaidAt == nil {
		t.Error("paid_at must be set")
	}
}

// TestSettleFailed_ConcurrentRedeliveryFiresSideEffectsOnce is the failed-path
// twin of the H7 success-path test: two at-least-once deliveries of the SAME
// payment_intent.payment_failed that race — both holding a stale `processing`
// snapshot, so both slip past SettleFailed's already-settled fast-path guard —
// must fire the failure-notification set (payment.failed event + customer
// email + dunning) EXACTLY once. The PI-keyed transition gate in
// MarkPaymentFailedReportingTransition de-dupes them. Pre-fix both fired,
// double-emailing the customer and double-firing the outbound webhook.
func TestSettleFailed_ConcurrentRedeliveryFiresSideEffectsOnce(t *testing.T) {
	invoices := newMockInvoiceUpdater()
	invoices.invoices["inv_1"] = domain.Invoice{
		ID: "inv_1", TenantID: "t1", CustomerID: "cus_1", InvoiceNumber: "VLX-1",
		Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentProcessing,
		StripePaymentIntentID: "pi_x", TotalAmountCents: 4200, Currency: "USD",
	}
	invoices.byPI["pi_x"] = "inv_1"

	events := &recordingEventDispatcher{}
	failedEmail := &recordingFailedEmail{}
	dunning := &recordingDunningStarter{}
	s := NewStripe(&mockStripeClient{}, invoices, newMockWebhookStore(), nil, dunning)
	s.SetEventDispatcher(events)
	s.SetEmailPaymentFailed(failedEmail, staticCustomerEmail{})

	// Both racers hold the same stale `processing` snapshot.
	stale := invoices.invoices["inv_1"]
	for i := 0; i < 2; i++ {
		if err := s.SettleFailed(context.Background(), "t1", stale, "pi_x", "Your card was declined.", false, SourceWebhook); err != nil {
			t.Fatalf("settle attempt %d: %v", i+1, err)
		}
	}

	if got := invoices.invoices["inv_1"].PaymentStatus; got != domain.PaymentFailed {
		t.Fatalf("invoice not marked failed: payment_status=%q", got)
	}
	if got := invoices.failedEventEnqueues; got != 1 {
		t.Errorf("payment.failed enqueued %d times, want exactly 1 (concurrent redelivery must not double-fire the webhook)", got)
	}
	if got := events.byType[domain.EventPaymentFailed]; got != 0 {
		t.Errorf("payment.failed dispatched post-commit %d times, want 0 (it is enqueued IN-TX by the store; a post-commit fire would double-emit)", got)
	}
	if failedEmail.sends != 1 {
		t.Errorf("payment-failed email enqueued %d times, want exactly 1 (no double-notify)", failedEmail.sends)
	}
	if len(dunning.calls) != 1 {
		t.Errorf("StartDunning called %d times, want exactly 1", len(dunning.calls))
	}
}

// TestSettleFailed_InlinePresetThenWebhookStillNotifiesOnce guards the trap that
// makes a naive status-keyed gate WRONG: the synchronous charge path stamps
// payment_status='failed' (same PI) WITHOUT firing notifications, deferring them
// to the payment_intent.payment_failed webhook. The webhook's SettleFailed must
// still fire the notification set exactly once — the PI marker (which the inline
// preset never writes), not the status, is the dedup key.
func TestSettleFailed_InlinePresetThenWebhookStillNotifiesOnce(t *testing.T) {
	invoices := newMockInvoiceUpdater()
	// Inline charge path already flipped payment_status=failed with pi_y but did
	// NOT notify (failNotedPI for inv_1 stays empty).
	invoices.invoices["inv_1"] = domain.Invoice{
		ID: "inv_1", TenantID: "t1", CustomerID: "cus_1", InvoiceNumber: "VLX-2",
		Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentFailed,
		StripePaymentIntentID: "pi_y", LastPaymentError: "declined",
	}
	invoices.byPI["pi_y"] = "inv_1"

	events := &recordingEventDispatcher{}
	failedEmail := &recordingFailedEmail{}
	dunning := &recordingDunningStarter{}
	s := NewStripe(&mockStripeClient{}, invoices, newMockWebhookStore(), nil, dunning)
	s.SetEventDispatcher(events)
	s.SetEmailPaymentFailed(failedEmail, staticCustomerEmail{})

	if err := s.SettleFailed(context.Background(), "t1", invoices.invoices["inv_1"], "pi_y", "Your card was declined.", false, SourceWebhook); err != nil {
		t.Fatalf("SettleFailed: %v", err)
	}
	if got := invoices.failedEventEnqueues; got != 1 {
		t.Errorf("payment.failed enqueued %d times after inline preset, want 1 (status was already failed but notifications had not fired)", got)
	}
	if failedEmail.sends != 1 {
		t.Errorf("payment-failed email enqueued %d times, want 1", failedEmail.sends)
	}
}

// TestSettleFailed_NewRetryPIFiresAgain ensures the gate does NOT suppress a
// genuinely new failure: a later dunning retry uses a fresh PI, so its failure
// is a distinct event and must fire its own notification set.
func TestSettleFailed_NewRetryPIFiresAgain(t *testing.T) {
	invoices := newMockInvoiceUpdater()
	invoices.invoices["inv_1"] = domain.Invoice{
		ID: "inv_1", TenantID: "t1", CustomerID: "cus_1",
		Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentProcessing,
		StripePaymentIntentID: "pi_a",
	}
	events := &recordingEventDispatcher{}
	dunning := &recordingDunningStarter{}
	s := NewStripe(&mockStripeClient{}, invoices, newMockWebhookStore(), nil, dunning)
	s.SetEventDispatcher(events)

	// First failure (pi_a), then a retry's failure on a fresh PI (pi_b).
	// suppressCustomerEmail=true (dunning-retry PIs), so we assert on the event.
	for _, pi := range []string{"pi_a", "pi_b"} {
		cur := invoices.invoices["inv_1"]
		if err := s.SettleFailed(context.Background(), "t1", cur, pi, "declined", true, SourceWebhook); err != nil {
			t.Fatalf("SettleFailed %s: %v", pi, err)
		}
	}
	if got := invoices.failedEventEnqueues; got != 2 {
		t.Errorf("payment.failed enqueued %d times across two distinct PIs, want 2 (a new retry failure is a fresh event)", got)
	}
}

func TestSettleFailed_FiresDunningFromAnySource(t *testing.T) {
	invoices := newMockInvoiceUpdater()
	invoices.invoices["inv_1"] = domain.Invoice{
		ID: "inv_1", TenantID: "t1", CustomerID: "cus_1", Status: domain.InvoiceFinalized,
		PaymentStatus: domain.PaymentProcessing, StripePaymentIntentID: "pi_def",
	}
	invoices.byPI["pi_def"] = "inv_1"
	dunning := &recordingDunningStarter{}

	s := NewStripe(&mockStripeClient{}, invoices, newMockWebhookStore(), nil, dunning)

	// A reconciler-style direct call (suppressCustomerEmail=false): this is the
	// convergence Phase 2 depends on — the primitive fires dunning regardless
	// of who discovered the failure, so a dropped-webhook recovery is not a
	// silent under-collection.
	if err := s.SettleFailed(context.Background(), "t1", invoices.invoices["inv_1"], "pi_def", "Your card was declined.", false, SourceReconciler); err != nil {
		t.Fatalf("SettleFailed: %v", err)
	}

	inv := invoices.invoices["inv_1"]
	if inv.PaymentStatus != domain.PaymentFailed {
		t.Errorf("payment_status: got %q, want failed", inv.PaymentStatus)
	}
	if inv.LastPaymentError != "Your card was declined." {
		t.Errorf("error: got %q, want the decline message", inv.LastPaymentError)
	}
	if len(dunning.calls) != 1 || dunning.calls[0].invoiceID != "inv_1" {
		t.Fatalf("StartDunning calls: got %+v, want exactly one for inv_1 (dunning must fire from any source)", dunning.calls)
	}
}

func TestSettleFailed_OutOfOrderGuardLivesInPrimitive(t *testing.T) {
	paidAt := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	invoices := newMockInvoiceUpdater()
	invoices.invoices["inv_1"] = domain.Invoice{
		ID: "inv_1", TenantID: "t1", Status: domain.InvoicePaid,
		PaymentStatus: domain.PaymentSucceeded, PaidAt: &paidAt,
		StripePaymentIntentID: "pi_ok",
	}
	dunning := &recordingDunningStarter{}

	s := NewStripe(&mockStripeClient{}, invoices, newMockWebhookStore(), nil, dunning)

	// A stale failure for an already-paid invoice, arriving via ANY source,
	// must be a no-op — the guard lives in the primitive, so every settler
	// (reconciler included) inherits it.
	if err := s.SettleFailed(context.Background(), "t1", invoices.invoices["inv_1"], "pi_stale", "Your card was declined.", false, SourceReconciler); err != nil {
		t.Fatalf("SettleFailed: %v", err)
	}

	inv := invoices.invoices["inv_1"]
	if inv.PaymentStatus != domain.PaymentSucceeded || inv.PaidAt == nil {
		t.Errorf("out-of-order failure corrupted a paid invoice: payment=%q paid_at=%v", inv.PaymentStatus, inv.PaidAt)
	}
	if inv.StripePaymentIntentID != "pi_ok" {
		t.Errorf("stale PI relinked: got %q, want pi_ok", inv.StripePaymentIntentID)
	}
	if len(dunning.calls) != 0 {
		t.Errorf("dunning started on an already-paid invoice: %+v", dunning.calls)
	}
}

// ---------------------------------------------------------------------------
// Post-commit durability tiering (2026-07-05): the receipt enqueue runs FIRST
// post-commit — before any Stripe network call — and the whole side-effect
// block is detached from the caller's cancellation via context.WithoutCancel.
// Pre-fix the receipt sat at the very bottom, below the checkout-session
// expire sweep and the card fetch: a process death or a webhook-client
// disconnect during those seconds-wide network calls silently dropped the
// receipt enqueue (which has NO reconciler behind it), while the comment
// above it claimed the window was "sub-ms".
// ---------------------------------------------------------------------------

// callSequencer records the order fakes fire in and the ctx cancellation
// state each observed at call time.
type callSequencer struct {
	seq     []string
	ctxErrs map[string]error
}

func (c *callSequencer) record(name string, ctx context.Context) {
	if c.ctxErrs == nil {
		c.ctxErrs = map[string]error{}
	}
	c.seq = append(c.seq, name)
	c.ctxErrs[name] = ctx.Err()
}

type sequencedReceiptEmail struct{ seq *callSequencer }

func (r sequencedReceiptEmail) SendPaymentReceipt(ctx context.Context, _, _ string, _ []string, _, _ string, _ int64, _, _ string) error {
	r.seq.record("receipt_enqueue", ctx)
	return nil
}

type sequencedCardFetcher struct{ seq *callSequencer }

func (f sequencedCardFetcher) FetchCardDetails(context.Context, string) (CardDetails, error) {
	return CardDetails{}, nil
}
func (f sequencedCardFetcher) FetchCardForPaymentIntent(ctx context.Context, _ string) (CardDetails, error) {
	f.seq.record("card_fetch", ctx)
	return CardDetails{Brand: "visa", Last4: "4242"}, nil
}

type sequencedDunningResolver struct{ seq *callSequencer }

func (d sequencedDunningResolver) ResolveByInvoice(ctx context.Context, _, _ string, _ domain.DunningResolution) error {
	d.seq.record("dunning_resolve", ctx)
	return nil
}

func TestSettleSucceeded_ReceiptEnqueueFirstPostCommit_AndDetachedFromCancel(t *testing.T) {
	invoices := newMockInvoiceUpdater()
	invoices.invoices["inv_1"] = domain.Invoice{
		ID: "inv_1", TenantID: "t1", CustomerID: "cus_1", InvoiceNumber: "VLX-1",
		Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentProcessing,
		StripePaymentIntentID: "pi_abc", TotalAmountCents: 2900, Currency: "USD",
	}
	invoices.byPI["pi_abc"] = "inv_1"

	seq := &callSequencer{}
	s := NewStripe(&mockStripeClient{}, invoices, newMockWebhookStore(), nil)
	s.SetEmailReceipt(sequencedReceiptEmail{seq}, staticCustomerEmail{})
	s.SetCardFetcher(sequencedCardFetcher{seq})
	s.SetDunningResolver(sequencedDunningResolver{seq})

	// The caller's ctx is ALREADY canceled — the shape of a webhook client
	// disconnect / server drain. The settle's post-commit block must run
	// on a detached ctx so the enqueues still land. (The mocks don't check
	// ctx themselves; what's asserted is the ctx.Err() each fake RECEIVED.)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.SettleSucceeded(ctx, "t1", invoices.invoices["inv_1"], "pi_abc", 0, SourceWebhook); err != nil {
		t.Fatalf("SettleSucceeded: %v", err)
	}

	want := []string{"receipt_enqueue", "dunning_resolve", "card_fetch"}
	if len(seq.seq) != len(want) {
		t.Fatalf("post-commit call sequence: got %v, want %v", seq.seq, want)
	}
	for i, name := range want {
		if seq.seq[i] != name {
			t.Fatalf("post-commit call sequence: got %v, want %v — the receipt enqueue (unrecoverable) must run before every network call", seq.seq, want)
		}
	}
	for name, err := range seq.ctxErrs {
		if err != nil {
			t.Errorf("%s observed a canceled ctx (%v) — post-commit side effects must run on context.WithoutCancel", name, err)
		}
	}
}

// TestSettleFailed_EmailEnqueueBeforeDunningStart pins the failed-path twin:
// the payment-failed email enqueue (no reconciler) runs before the dunning
// start (recovered by the dunning_backfill sweep), and suppression skips ONLY
// the email — dunning still runs.
func TestSettleFailed_EmailEnqueueBeforeDunningStart(t *testing.T) {
	run := func(suppress bool) (seq *callSequencer, dunning *mockDunningStarter) {
		invoices := newMockInvoiceUpdater()
		invoices.invoices["inv_1"] = domain.Invoice{
			ID: "inv_1", TenantID: "t1", CustomerID: "cus_1", InvoiceNumber: "VLX-1",
			Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentProcessing,
		}
		invoices.byPI["pi_abc"] = "inv_1"
		seq = &callSequencer{}
		dunning = &mockDunningStarter{seq: seq}
		s := NewStripe(&mockStripeClient{}, invoices, newMockWebhookStore(), nil, dunning)
		s.SetEmailPaymentFailed(sequencedFailedEmail{seq}, staticCustomerEmail{})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := s.SettleFailed(ctx, "t1", invoices.invoices["inv_1"], "pi_abc", "card declined", suppress, SourceWebhook); err != nil {
			t.Fatalf("SettleFailed(suppress=%v): %v", suppress, err)
		}
		return seq, dunning
	}

	seq, _ := run(false)
	want := []string{"failed_email_enqueue", "dunning_start"}
	if len(seq.seq) != 2 || seq.seq[0] != want[0] || seq.seq[1] != want[1] {
		t.Fatalf("failed-path sequence: got %v, want %v — the email enqueue (unrecoverable) must run before the dunning start (reconciler-backstopped)", seq.seq, want)
	}
	for name, err := range seq.ctxErrs {
		if err != nil {
			t.Errorf("%s observed a canceled ctx (%v) — post-commit side effects must run on context.WithoutCancel", name, err)
		}
	}

	// Suppression skips only the email; dunning must still start.
	seq, _ = run(true)
	if len(seq.seq) != 1 || seq.seq[0] != "dunning_start" {
		t.Fatalf("suppressed sequence: got %v, want [dunning_start] — suppression is about duplicate comms, not collections", seq.seq)
	}
}

type sequencedFailedEmail struct{ seq *callSequencer }

func (r sequencedFailedEmail) SendPaymentFailed(ctx context.Context, _, _ string, _ []string, _, _, _, _ string) error {
	r.seq.record("failed_email_enqueue", ctx)
	return nil
}

// mockDunningStarter records StartDunning into the shared sequencer.
type mockDunningStarter struct{ seq *callSequencer }

func (m *mockDunningStarter) StartDunning(ctx context.Context, _, _, _ string, _ time.Time, _ domain.DunningStartCause) (domain.InvoiceDunningRun, error) {
	m.seq.record("dunning_start", ctx)
	return domain.InvoiceDunningRun{}, nil
}

// TestStopCollection_ExpiresSessionsBeforeCancelingPI locks the FLOW D2
// finding: Stripe REFUSES to cancel a PaymentIntent it created for a
// Checkout Session, so a void whose only cleanup was the PI cancel left the
// session live (and the voided invoice payable) until its ExpiresAt. Session
// expiry must run FIRST and unconditionally — including when there is no PI
// recorded at all — and a refused PI cancel must not abort it.
func TestStopCollection_ExpiresSessionsBeforeCancelingPI(t *testing.T) {
	inv := finalizedPendingInvoice()
	inv.StripePaymentIntentID = "pi_checkout"

	// Client whose cancel always fails the way Stripe fails for
	// checkout-owned PIs.
	client := &mockStripeClient{}
	s := NewStripe(client, newMockInvoiceUpdater(), newMockWebhookStore(), nil, &recordingDunningStarter{})

	// No checkout-session store wired (narrow fixture): StopCollection must
	// still attempt the PI cancel and must not panic.
	s.StopCollection(context.Background(), "t1", inv)

	// With no PI recorded, the call is still safe — the session-expiry leg
	// is the load-bearing one for hosted-page attempts.
	noPI := finalizedPendingInvoice()
	noPI.StripePaymentIntentID = ""
	s.StopCollection(context.Background(), "t1", noPI)
}
