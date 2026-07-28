package invoice

import (
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// TestDescribeDunningEvent_StartCauses locks the cause-aware start row
// (the timeline half of the dunning-start-cause fix): a payment-failure
// start renders as a failed row, a card-less enrollment renders as a
// reminder-cycle row, and legacy rows with no recorded cause keep the
// old generic copy.
func TestDescribeDunningEvent_StartCauses(t *testing.T) {
	cases := []struct {
		eventType, reason, wantDesc, wantSeverity, wantDetail string
		attempt                                               int
	}{
		// Uniform machine title; the cause is the subline.
		{"dunning_started", "payment_failed", "Payment recovery started", "failed", "Card was declined — automatic retries scheduled", 0},
		{"dunning_started", "no_payment_method", "Payment recovery started", "scheduled", "No payment method — reminders until a card is added", 0},
		{"dunning_started", "", "Payment recovery started", "scheduled", "", 0},
		// Card-less retry = reminder tick, never a charge failure —
		// both the normalized enum and the legacy sentinel render it.
		// The reminder clause is composed by the timeline builder from
		// the actual email_outbox row, not asserted here.
		{"retry_attempted", "no_payment_method", "Payment retry #1 attempted", "scheduled", "No payment method", 1},
		{"retry_attempted", "no payment method for customer", "Payment retry #2 attempted", "scheduled", "No payment method", 2},
		// A real decline keeps the processing shape (provider reason
		// folds in from the Stripe twin); the recovering attempt is green.
		{"retry_attempted", "card_declined", "Payment retry #3 attempted", "processing", "", 3},
		{"retry_attempted", "succeeded", "Payment retry #3 attempted", "succeeded", "", 3},
	}
	for _, c := range cases {
		desc, sev, detail := describeDunningEvent(c.eventType, c.reason, c.attempt)
		if desc != c.wantDesc || sev != c.wantSeverity || detail != c.wantDetail {
			t.Errorf("%s/%q: got (%q, %q, %q), want (%q, %q, %q)", c.eventType, c.reason, desc, sev, detail, c.wantDesc, c.wantSeverity, c.wantDetail)
		}
	}
}

// TestEmailClause locks the delivery-verdict grammar threaded into
// dunning-row sublines: outbox status decides sent-vs-not, the
// ADR-098 provider verdict layers over a completed handoff — same
// grammar as describeEmailEvent, rendered as a lowercase clause.
func TestEmailClause(t *testing.T) {
	cases := []struct {
		status, delivery, want string
	}{
		{"dispatched", "unknown", "reminder sent"},
		{"dispatched", "delivered", "reminder sent — delivered"},
		{"dispatched", "bounced", "reminder sent — bounced"},
		{"dispatched", "complained", "reminder sent — recipient marked it as spam"},
		{"failed", "unknown", "reminder failed to send"},
		{"pending", "unknown", "reminder queued"},
		{"skipped", "unknown", "reminder skipped — invoice settled first"},
	}
	for _, c := range cases {
		got := emailClause("reminder", EmailEventRow{Status: c.status, DeliveryState: c.delivery})
		if got != c.want {
			t.Errorf("%s/%s: got %q, want %q", c.status, c.delivery, got, c.want)
		}
	}
	if got := capFirst("reminder sent — bounced"); got != "Reminder sent — bounced" {
		t.Errorf("capFirst: got %q", got)
	}
	if got := capFirst("Already capped"); got != "Already capped" {
		t.Errorf("capFirst noop: got %q", got)
	}
}

// TestApplyChargeAttemptPrecedence locks the ADR-102 render rule: every
// charge attempt appears exactly once, via the richest owner available —
// dunning row → attempt row → stripe webhook row — with the attempt
// replacing its webhook echo only on simulated invoices (the invoice's
// own axis prefers its own facts; wall-clock invoices keep the webhook
// row so pre-ADR-102 rendering is unchanged).
func TestApplyChargeAttemptPrecedence(t *testing.T) {
	simT := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	wallT := time.Date(2026, 7, 28, 14, 12, 0, 0, time.UTC)
	failed := func(pi string, sim bool) domain.InvoiceChargeAttempt {
		a := domain.InvoiceChargeAttempt{
			StripePaymentIntentID: pi,
			Outcome:               domain.ChargeAttemptFailed,
			ProviderReason:        "Your card was declined.",
			AmountCents:           440,
			OccurredAt:            wallT,
		}
		if sim {
			a.SimEffectiveAt = &simT
		}
		return a
	}
	stripeRow := func(pi string) timelineEvent {
		return timelineEvent{Source: "stripe", EventType: "payment_intent.payment_failed", PaymentIntentID: pi, Detail: "Customer notified by email", Error: "Your card was declined."}
	}
	dunningRow := func(pi string) timelineEvent {
		return timelineEvent{Source: "dunning", EventType: "dunning_started", PaymentIntentID: pi}
	}
	countBy := func(events []timelineEvent, source string) int {
		n := 0
		for _, e := range events {
			if e.Source == source {
				n++
			}
		}
		return n
	}

	// Dunning owns the PI → attempt suppressed, nothing added or dropped.
	out := applyChargeAttemptPrecedence([]timelineEvent{dunningRow("pi_1")},
		[]domain.InvoiceChargeAttempt{failed("pi_1", true)}, domain.Invoice{}, true)
	if len(out) != 1 || countBy(out, "payment") != 0 {
		t.Fatalf("dunning-owned PI: got %d rows (%d payment), want the 1 dunning row only", len(out), countBy(out, "payment"))
	}

	// Simulated invoice, stripe owns the PI, sim-stamped attempt →
	// attempt REPLACES the webhook row and lifts its folded Detail.
	out = applyChargeAttemptPrecedence([]timelineEvent{stripeRow("pi_2")},
		[]domain.InvoiceChargeAttempt{failed("pi_2", true)}, domain.Invoice{}, true)
	if countBy(out, "stripe") != 0 || countBy(out, "payment") != 1 {
		t.Fatalf("sim replace: got %d stripe / %d payment rows, want 0/1", countBy(out, "stripe"), countBy(out, "payment"))
	}
	if out[0].Detail != "Customer notified by email" {
		t.Fatalf("sim replace must lift the folded Detail, got %q", out[0].Detail)
	}
	if !out[0].IsSimulated || !out[0].sortAt.Equal(simT) {
		t.Fatalf("sim replace must render on the billing axis (is_simulated at simT), got sim=%v at %v", out[0].IsSimulated, out[0].sortAt)
	}

	// Wall-clock invoice, stripe owns the PI → attempt defers (zero churn).
	out = applyChargeAttemptPrecedence([]timelineEvent{stripeRow("pi_3")},
		[]domain.InvoiceChargeAttempt{failed("pi_3", false)}, domain.Invoice{}, false)
	if countBy(out, "stripe") != 1 || countBy(out, "payment") != 0 {
		t.Fatalf("wall defer: got %d stripe / %d payment rows, want 1/0", countBy(out, "stripe"), countBy(out, "payment"))
	}

	// Nothing owns the PI (webhook lost, or dunning off pre-webhook) →
	// the attempt renders itself. Empty-PI attempts (PI create failed)
	// always render — they can never have a twin.
	out = applyChargeAttemptPrecedence(nil,
		[]domain.InvoiceChargeAttempt{failed("pi_4", true), failed("", true)}, domain.Invoice{}, true)
	if countBy(out, "payment") != 2 {
		t.Fatalf("unowned attempts: got %d payment rows, want 2", countBy(out, "payment"))
	}

	// Succeeded attempt on a paid invoice → the invoice.paid lifecycle
	// row owns the story; on a NOT-paid invoice it renders (anomaly).
	paidAt := wallT
	succ := domain.InvoiceChargeAttempt{StripePaymentIntentID: "pi_5", Outcome: domain.ChargeAttemptSucceeded, OccurredAt: wallT}
	out = applyChargeAttemptPrecedence(nil, []domain.InvoiceChargeAttempt{succ}, domain.Invoice{PaidAt: &paidAt}, false)
	if countBy(out, "payment") != 0 {
		t.Fatalf("succeeded+paid: got %d payment rows, want 0", countBy(out, "payment"))
	}
	out = applyChargeAttemptPrecedence(nil, []domain.InvoiceChargeAttempt{succ}, domain.Invoice{}, false)
	if countBy(out, "payment") != 1 || out[0].Description != "Payment collected" {
		t.Fatalf("succeeded+unpaid anomaly: got %d payment rows (%q), want 1 'Payment collected'", countBy(out, "payment"), out[0].Description)
	}

	// Pending attempts never render — the attention banner owns in-flight.
	out = applyChargeAttemptPrecedence(nil,
		[]domain.InvoiceChargeAttempt{{StripePaymentIntentID: "pi_6", Outcome: domain.ChargeAttemptPending, OccurredAt: wallT}}, domain.Invoice{}, false)
	if len(out) != 0 {
		t.Fatalf("pending: got %d rows, want 0", len(out))
	}
}
