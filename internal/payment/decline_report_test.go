package payment

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// The charge call's own decline is a report like the webhook's: it fires
// payment.failed and the customer email once per PaymentIntent, whichever
// arrives first, and with no webhook at all. Before, it stamped the failure
// and left every notification to the webhook — a decline with no delivery
// (lost, or a test clock with no forwarder) told nobody.

func declineClient(pi string) *mockStripeClient {
	return &mockStripeClient{shouldFail: true, failErr: &PaymentError{
		Message: "Your card was declined.", PaymentIntentID: pi, Unknown: false,
	}}
}

func newDeclineStripe(client *mockStripeClient, invoices *mockInvoiceUpdater) (*Stripe, *recordingFailedEmail, *recordingDunningStarter) {
	email := &recordingFailedEmail{}
	dunning := &recordingDunningStarter{}
	s := NewStripe(client, invoices, newMockWebhookStore(), nil, dunning)
	s.SetEmailPaymentFailed(email, staticCustomerEmail{})
	return s, email, dunning
}

func TestChargeDecline_NotifiesWithoutAWebhook(t *testing.T) {
	invoices := newMockInvoiceUpdater()
	snapshot := pendingSnapshot()
	invoices.invoices["inv_1"] = snapshot
	s, email, dunning := newDeclineStripe(declineClient("pi_X"), invoices)

	if _, err := s.ChargeInvoice(context.Background(), "t1", snapshot, "cus_stripe", "pm_test"); err == nil {
		t.Fatal("a decline must return an error to the caller")
	}
	got := invoices.invoices["inv_1"]
	if got.PaymentStatus != domain.PaymentFailed || got.StripePaymentIntentID != "pi_X" || got.ChargeAttemptSeq != 1 {
		t.Fatalf("row %s/%q/seq %d, want failed/pi_X/seq 1", got.PaymentStatus, got.StripePaymentIntentID, got.ChargeAttemptSeq)
	}
	if invoices.failedEventEnqueues != 1 {
		t.Errorf("payment.failed enqueued %d times, want 1 — the decline must announce itself with no webhook", invoices.failedEventEnqueues)
	}
	if email.sends != 1 {
		t.Errorf("decline email sent %d times, want 1", email.sends)
	}
	if len(dunning.calls) != 1 {
		t.Errorf("StartDunning called %d times, want 1 (inline, for a test-clock Advance)", len(dunning.calls))
	}
}

// The webhook that follows the charge call's report is the same PaymentIntent:
// it must not notify again.
func TestChargeDecline_ThenWebhook_NotifiesOnce(t *testing.T) {
	invoices := newMockInvoiceUpdater()
	snapshot := pendingSnapshot()
	invoices.invoices["inv_1"] = snapshot
	s, email, _ := newDeclineStripe(declineClient("pi_X"), invoices)

	_, _ = s.ChargeInvoice(context.Background(), "t1", snapshot, "cus_stripe", "pm_test")
	if err := s.SettleFailed(context.Background(), "t1", invoices.invoices["inv_1"],
		PaymentFailure{PaymentIntentID: "pi_X", Message: "Your card was declined."}, SourceWebhook); err != nil {
		t.Fatalf("webhook SettleFailed: %v", err)
	}
	if invoices.failedEventEnqueues != 1 || email.sends != 1 {
		t.Errorf("events=%d emails=%d after charge response + webhook, want 1 and 1", invoices.failedEventEnqueues, email.sends)
	}
	if got := invoices.invoices["inv_1"].ChargeAttemptSeq; got != 1 {
		t.Errorf("seq %d after the webhook re-reported the recorded decline, want 1 (a re-report must not bump it)", got)
	}
}

// The webhook got there first: the charge call finds the invoice already
// holding its PaymentIntent and does not notify again.
func TestChargeDecline_WebhookFirst_NotifiesOnce(t *testing.T) {
	invoices, snapshot := declinedMidCharge()
	s, email, _ := newDeclineStripe(declineClient("pi_X"), invoices)

	_, _ = s.ChargeInvoice(context.Background(), "t1", snapshot, "cus_stripe", "pm_test")
	if invoices.failedEventEnqueues != 1 {
		t.Errorf("payment.failed enqueued %d times in total, want 1 (the webhook's)", invoices.failedEventEnqueues)
	}
	if email.sends != 0 {
		t.Errorf("charge call sent %d decline emails after the webhook already notified, want 0", email.sends)
	}
	if got := invoices.invoices["inv_1"]; got.StripePaymentIntentID != "pi_X" || got.ChargeAttemptSeq != 1 {
		t.Errorf("row %q/seq %d, want pi_X/seq 1 unchanged", got.StripePaymentIntentID, got.ChargeAttemptSeq)
	}
}

// Dunning sends its own email per retry attempt; the generic decline email
// would double-notify. The event still fires.
func TestChargeDecline_DunningRetry_EventButNoEmail(t *testing.T) {
	invoices := newMockInvoiceUpdater()
	snapshot := pendingSnapshot()
	snapshot.PaymentStatus = domain.PaymentFailed
	snapshot.StripePaymentIntentID = "pi_W"
	snapshot.ChargeAttemptSeq = 2
	invoices.invoices["inv_1"] = snapshot
	invoices.failNotedPI["inv_1"] = "pi_W"
	s, email, _ := newDeclineStripe(declineClient("pi_X"), invoices)

	_, _ = s.ChargeInvoiceForDunningRetry(context.Background(), "t1", snapshot, "cus_stripe", "pm_test")
	if invoices.failedEventEnqueues != 1 {
		t.Errorf("payment.failed enqueued %d times, want 1", invoices.failedEventEnqueues)
	}
	if email.sends != 0 {
		t.Errorf("generic decline email sent %d times on a dunning retry, want 0", email.sends)
	}
	if got := invoices.invoices["inv_1"]; got.StripePaymentIntentID != "pi_X" || got.ChargeAttemptSeq != 3 {
		t.Errorf("row %q/seq %d, want pi_X/seq 3", got.StripePaymentIntentID, got.ChargeAttemptSeq)
	}
}

// A failure with no PaymentIntent (the create itself failed — a merchant
// configuration error, a missing card) has nothing to report: it is stamped
// and the customer is not emailed about the merchant's setup.
func TestChargeFailure_NoPaymentIntent_StampedNotAnnounced(t *testing.T) {
	invoices := newMockInvoiceUpdater()
	snapshot := pendingSnapshot()
	invoices.invoices["inv_1"] = snapshot
	client := &mockStripeClient{shouldFail: true, failErr: &PaymentError{Message: "stripe not configured for this mode", Unknown: false}}
	s, email, dunning := newDeclineStripe(client, invoices)

	_, _ = s.ChargeInvoice(context.Background(), "t1", snapshot, "cus_stripe", "pm_test")
	if got := invoices.invoices["inv_1"]; got.PaymentStatus != domain.PaymentFailed || got.ChargeAttemptSeq != 1 {
		t.Fatalf("row %s/seq %d, want failed/seq 1", got.PaymentStatus, got.ChargeAttemptSeq)
	}
	if invoices.failedEventEnqueues != 0 || email.sends != 0 {
		t.Errorf("events=%d emails=%d for a failure with no PaymentIntent, want 0 and 0", invoices.failedEventEnqueues, email.sends)
	}
	if len(dunning.calls) != 1 {
		t.Errorf("StartDunning called %d times, want 1 — a definite failure still dunning", len(dunning.calls))
	}
}

// A void or write-off can land during the charge (neither takes the charge
// lease). The decline is recorded, but the customer is not asked to pay an
// invoice that is no longer owed, and no campaign starts.
func TestChargeDecline_InvoiceVoidedDuringCall_NoEmailNoDunning(t *testing.T) {
	invoices := newMockInvoiceUpdater()
	snapshot := pendingSnapshot()
	row := snapshot
	row.Status = domain.InvoiceVoided
	invoices.invoices["inv_1"] = row
	s, email, dunning := newDeclineStripe(declineClient("pi_X"), invoices)

	_, _ = s.ChargeInvoice(context.Background(), "t1", snapshot, "cus_stripe", "pm_test")
	if email.sends != 0 {
		t.Errorf("decline email sent %d times for a voided invoice, want 0", email.sends)
	}
	if len(dunning.calls) != 0 {
		t.Errorf("dunning started on a voided invoice: %+v", dunning.calls)
	}
}

// Gap #1: a late decline webhook for an OLDER attempt must not move an
// invoice that already holds a newer one — no relink, no seq bump, no notice.
func TestSettleFailed_StaleOlderAttempt_LeavesInvoiceAlone(t *testing.T) {
	for _, status := range []domain.InvoicePaymentStatus{domain.PaymentFailed, domain.PaymentProcessing, domain.PaymentUnknown} {
		t.Run(string(status), func(t *testing.T) {
			invoices := newMockInvoiceUpdater()
			invoices.invoices["inv_1"] = domain.Invoice{
				ID: "inv_1", TenantID: "t1", CustomerID: "cus_1", Status: domain.InvoiceFinalized,
				PaymentStatus: status, StripePaymentIntentID: "pi_B", ChargeAttemptSeq: 2,
			}
			invoices.failNotedPI["inv_1"] = "pi_B"
			s, email, dunning := newDeclineStripe(&mockStripeClient{}, invoices)

			if err := s.SettleFailed(context.Background(), "t1", invoices.invoices["inv_1"],
				PaymentFailure{PaymentIntentID: "pi_A", Message: "late decline"}, SourceWebhook); err != nil {
				t.Fatalf("SettleFailed: %v", err)
			}
			got := invoices.invoices["inv_1"]
			if got.PaymentStatus != status || got.StripePaymentIntentID != "pi_B" || got.ChargeAttemptSeq != 2 {
				t.Errorf("row %s/%q/seq %d, want %s/pi_B/seq 2 untouched", got.PaymentStatus, got.StripePaymentIntentID, got.ChargeAttemptSeq, status)
			}
			if invoices.failedEventEnqueues != 0 || email.sends != 0 || len(dunning.calls) != 0 {
				t.Errorf("events=%d emails=%d dunning=%d for a stale attempt, want 0/0/0", invoices.failedEventEnqueues, email.sends, len(dunning.calls))
			}
			if invoices.unrecordedFailures != 1 {
				t.Errorf("unrecorded reports %d, want 1", invoices.unrecordedFailures)
			}
		})
	}
}

// A decline on the hosted Checkout page is an attempt Velox never waits on:
// it lands on its attempt row only, whatever the invoice's state.
func TestSettleFailed_HostedAttempt_NeverMovesInvoice(t *testing.T) {
	invoices := newMockInvoiceUpdater()
	invoices.invoices["inv_1"] = domain.Invoice{
		ID: "inv_1", TenantID: "t1", CustomerID: "cus_1", Status: domain.InvoiceFinalized,
		PaymentStatus: domain.PaymentPending,
	}
	s, email, dunning := newDeclineStripe(&mockStripeClient{}, invoices)

	if err := s.SettleFailed(context.Background(), "t1", invoices.invoices["inv_1"],
		PaymentFailure{PaymentIntentID: "pi_H", Message: "declined", Purpose: PurposeHostedInvoicePay}, SourceWebhook); err != nil {
		t.Fatalf("SettleFailed: %v", err)
	}
	got := invoices.invoices["inv_1"]
	if got.PaymentStatus != domain.PaymentPending || got.StripePaymentIntentID != "" || got.ChargeAttemptSeq != 0 {
		t.Errorf("hosted decline moved the invoice: %s/%q/seq %d", got.PaymentStatus, got.StripePaymentIntentID, got.ChargeAttemptSeq)
	}
	if invoices.failedEventEnqueues != 0 || email.sends != 0 || len(dunning.calls) != 0 {
		t.Errorf("events=%d emails=%d dunning=%d for a hosted decline, want 0/0/0", invoices.failedEventEnqueues, email.sends, len(dunning.calls))
	}
}

// The charge call writes its attempt row before reporting the decline: the
// report's in-tx upsert inserts a missing row as 'external' with no sim
// anchor, and nothing corrects the trigger afterwards.
func TestChargeDecline_AttemptRowCarriesItsTrigger(t *testing.T) {
	invoices := newMockInvoiceUpdater()
	snapshot := pendingSnapshot()
	invoices.invoices["inv_1"] = snapshot
	s, _, _ := newDeclineStripe(declineClient("pi_X"), invoices)

	_, _ = s.ChargeInvoiceForDunningRetry(context.Background(), "t1", snapshot, "cus_stripe", "pm_test")
	if got := invoices.attemptTrigger["pi_X"]; got != domain.ChargeTriggerDunningRetry {
		t.Fatalf("attempt trigger %q, want %q — the attempt row was written after the report", got, domain.ChargeTriggerDunningRetry)
	}
}

// failingOnceDunning fails its first StartDunning, as a DB blip during the
// charge call's inline start would.
type failingOnceDunning struct{ calls int }

func (f *failingOnceDunning) StartDunning(_ context.Context, _, invoiceID, _ string, _ time.Time, _ domain.DunningStartCause) (domain.InvoiceDunningRun, error) {
	f.calls++
	if f.calls <= 3 { // startDunningWithRetry tries three times
		return domain.InvoiceDunningRun{}, errors.New("db blip")
	}
	return domain.InvoiceDunningRun{ID: "drun_1", InvoiceID: invoiceID}, nil
}

// The charge call recorded and announced the decline, but its dunning start
// failed. The webhook that follows is not the first notice, yet it must
// still start dunning: on a test clock nothing else would (the backfill
// skips simulated invoices).
func TestChargeDecline_WebhookRedrivesAFailedDunningStart(t *testing.T) {
	invoices := newMockInvoiceUpdater()
	snapshot := pendingSnapshot()
	invoices.invoices["inv_1"] = snapshot
	dunning := &failingOnceDunning{}
	s := NewStripe(declineClient("pi_X"), invoices, newMockWebhookStore(), nil, dunning)

	_, _ = s.ChargeInvoice(context.Background(), "t1", snapshot, "cus_stripe", "pm_test")
	if dunning.calls != 3 {
		t.Fatalf("inline StartDunning attempts %d, want 3 (all failing)", dunning.calls)
	}
	if err := s.SettleFailed(context.Background(), "t1", invoices.invoices["inv_1"],
		PaymentFailure{PaymentIntentID: "pi_X", Message: "Your card was declined."}, SourceWebhook); err != nil {
		t.Fatalf("webhook SettleFailed: %v", err)
	}
	if dunning.calls != 4 {
		t.Fatalf("StartDunning calls %d, want 4 — the webhook must re-drive the failed start", dunning.calls)
	}
}

// The charge call's own report of its decline was lost (a DB blip, a crash
// after Stripe answered), so the invoice still holds the previous attempt.
// Its attempt row was written first, and it is Velox's newest: the webhook
// records it, which rotates the seq and sends the notice the charge call never
// sent.
func TestSettleFailed_LostChargeReport_WebhookRecordsTheNewestAttempt(t *testing.T) {
	invoices := newMockInvoiceUpdater()
	invoices.invoices["inv_1"] = domain.Invoice{
		ID: "inv_1", TenantID: "t1", CustomerID: "cus_1", Status: domain.InvoiceFinalized,
		PaymentStatus: domain.PaymentFailed, StripePaymentIntentID: "pi_A", ChargeAttemptSeq: 2,
	}
	invoices.failNotedPI["inv_1"] = "pi_A"
	invoices.chargeAttempts = []domain.InvoiceChargeAttempt{
		{InvoiceID: "inv_1", StripePaymentIntentID: "pi_A", Trigger: domain.ChargeTriggerAutoCharge},
		{InvoiceID: "inv_1", StripePaymentIntentID: "pi_B", Trigger: domain.ChargeTriggerAutoCharge},
	}
	s, email, _ := newDeclineStripe(&mockStripeClient{}, invoices)

	if err := s.SettleFailed(context.Background(), "t1", invoices.invoices["inv_1"],
		PaymentFailure{PaymentIntentID: "pi_B", Message: "declined"}, SourceWebhook); err != nil {
		t.Fatalf("SettleFailed: %v", err)
	}
	got := invoices.invoices["inv_1"]
	if got.StripePaymentIntentID != "pi_B" || got.ChargeAttemptSeq != 3 {
		t.Fatalf("row %q/seq %d, want pi_B/seq 3 — the newest attempt's decline must be recorded", got.StripePaymentIntentID, got.ChargeAttemptSeq)
	}
	if invoices.failedEventEnqueues != 1 || email.sends != 1 {
		t.Errorf("events=%d emails=%d, want 1 and 1", invoices.failedEventEnqueues, email.sends)
	}
}

// A parked invoice (its newest attempt's id never learned) must not be
// released by a late decline for an older attempt: that attempt has its own
// row, superseded by the parked one.
func TestSettleFailed_ParkedInvoice_StaleOlderDeclineSuperseded(t *testing.T) {
	invoices := newMockInvoiceUpdater()
	invoices.invoices["inv_1"] = domain.Invoice{
		ID: "inv_1", TenantID: "t1", CustomerID: "cus_1", Status: domain.InvoiceFinalized,
		PaymentStatus: domain.PaymentUnknown, ChargeAttemptSeq: 3,
	}
	invoices.chargeAttempts = []domain.InvoiceChargeAttempt{
		{InvoiceID: "inv_1", StripePaymentIntentID: "pi_A", Trigger: domain.ChargeTriggerAutoCharge},
		{InvoiceID: "inv_1", StripePaymentIntentID: "", Trigger: domain.ChargeTriggerDunningRetry},
	}
	s, email, dunning := newDeclineStripe(&mockStripeClient{}, invoices)

	if err := s.SettleFailed(context.Background(), "t1", invoices.invoices["inv_1"],
		PaymentFailure{PaymentIntentID: "pi_A", Message: "late decline"}, SourceWebhook); err != nil {
		t.Fatalf("SettleFailed: %v", err)
	}
	if got := invoices.invoices["inv_1"]; got.PaymentStatus != domain.PaymentUnknown || got.ChargeAttemptSeq != 3 {
		t.Fatalf("parked invoice released by a stale decline: %s/seq %d", got.PaymentStatus, got.ChargeAttemptSeq)
	}
	if invoices.failedEventEnqueues != 0 || email.sends != 0 || len(dunning.calls) != 0 {
		t.Errorf("events=%d emails=%d dunning=%d, want 0/0/0", invoices.failedEventEnqueues, email.sends, len(dunning.calls))
	}
}

// A parked invoice can adopt a hosted Checkout attempt found by the search
// sweep (ADR-108). The invoice then waits on it, so its failure must record —
// before, the hosted check ran first and the invoice sat in 'processing'
// forever.
func TestSettleFailed_HostedAttemptTheInvoiceHolds_Records(t *testing.T) {
	invoices := newMockInvoiceUpdater()
	invoices.invoices["inv_1"] = domain.Invoice{
		ID: "inv_1", TenantID: "t1", CustomerID: "cus_1", Status: domain.InvoiceFinalized,
		PaymentStatus: domain.PaymentProcessing, StripePaymentIntentID: "pi_H", ChargeAttemptSeq: 4,
	}
	s, _, dunning := newDeclineStripe(&mockStripeClient{}, invoices)

	if err := s.SettleFailed(context.Background(), "t1", invoices.invoices["inv_1"],
		PaymentFailure{PaymentIntentID: "pi_H", Message: "authentication abandoned", Purpose: PurposeHostedInvoicePay}, SourceReconciler); err != nil {
		t.Fatalf("SettleFailed: %v", err)
	}
	if got := invoices.invoices["inv_1"]; got.PaymentStatus != domain.PaymentFailed {
		t.Fatalf("payment_status %q, want failed — the invoice was waiting on this attempt", got.PaymentStatus)
	}
	if len(dunning.calls) != 1 {
		t.Errorf("StartDunning calls %d, want 1", len(dunning.calls))
	}
}
