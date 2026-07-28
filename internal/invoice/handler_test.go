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

// TestRenderChargeAttempts locks the ADR-103 single-source rule: payment
// rows come from charge attempts alone, with exactly two exact-keyed
// suppressions — a dunning row carrying the same PaymentIntent absorbs
// the attempt (and inherits its provider facts), and a succeeded attempt
// defers to the "Invoice paid" lifecycle row, which is the superset
// (credits / offline / $0 pay an invoice with no charge).
func TestRenderChargeAttempts(t *testing.T) {
	wallT := time.Date(2026, 7, 28, 14, 12, 0, 0, time.UTC)
	simT := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	failed := func(pi string, sim bool) domain.InvoiceChargeAttempt {
		a := domain.InvoiceChargeAttempt{
			StripePaymentIntentID: pi, Outcome: domain.ChargeAttemptFailed,
			ProviderReason: "Your card was declined.", AmountCents: 440, OccurredAt: wallT,
		}
		if sim {
			a.SimEffectiveAt = &simT
		}
		return a
	}
	dunningRow := func(pi string) timelineEvent {
		return timelineEvent{Source: "dunning", EventType: "retry_attempted", PaymentIntentID: pi}
	}
	count := func(evts []timelineEvent, src string) int {
		n := 0
		for _, e := range evts {
			if e.Source == src {
				n++
			}
		}
		return n
	}

	// Dunning owns the PI → attempt absorbed, and its provider facts
	// land on the dunning row.
	out := renderChargeAttempts([]timelineEvent{dunningRow("pi_1")},
		[]domain.InvoiceChargeAttempt{failed("pi_1", true)}, domain.Invoice{Currency: "USD"})
	if len(out) != 1 || count(out, "payment") != 0 {
		t.Fatalf("dunning-owned PI: got %d rows (%d payment), want 1 dunning row only", len(out), count(out, "payment"))
	}
	if out[0].Error != "Your card was declined." || out[0].AmountCents == nil || *out[0].AmountCents != 440 {
		t.Fatalf("dunning row must inherit the attempt's provider facts: %+v", out[0])
	}

	// No dunning row → the attempt renders itself, on the billing axis
	// when it carries a sim anchor.
	out = renderChargeAttempts(nil, []domain.InvoiceChargeAttempt{failed("pi_2", true)}, domain.Invoice{})
	if count(out, "payment") != 1 {
		t.Fatalf("unowned attempt must render: %+v", out)
	}
	if !out[0].IsSimulated || !out[0].sortAt.Equal(simT) {
		t.Fatalf("sim-anchored attempt must render on the billing axis: sim=%v at %v", out[0].IsSimulated, out[0].sortAt)
	}
	// A wall-stamped attempt keeps wall time.
	out = renderChargeAttempts(nil, []domain.InvoiceChargeAttempt{failed("pi_3", false)}, domain.Invoice{})
	if out[0].IsSimulated || !out[0].sortAt.Equal(wallT) {
		t.Fatalf("wall attempt must keep wall time: sim=%v at %v", out[0].IsSimulated, out[0].sortAt)
	}
	// Empty-PI attempts (the PI create itself failed) still render —
	// they can never have a dunning twin to absorb them.
	out = renderChargeAttempts([]timelineEvent{dunningRow("pi_9")},
		[]domain.InvoiceChargeAttempt{failed("", true)}, domain.Invoice{})
	if count(out, "payment") != 1 {
		t.Fatalf("empty-PI attempt must render: %+v", out)
	}

	// Succeeded: deferred on a paid invoice, rendered on an unpaid one.
	paidAt := wallT
	succ := domain.InvoiceChargeAttempt{StripePaymentIntentID: "pi_4", Outcome: domain.ChargeAttemptSucceeded, OccurredAt: wallT}
	out = renderChargeAttempts(nil, []domain.InvoiceChargeAttempt{succ}, domain.Invoice{PaidAt: &paidAt, StripePaymentIntentID: "pi_4"})
	if count(out, "payment") != 0 {
		t.Fatalf("succeeded+paid must defer to the lifecycle row: %+v", out)
	}
	out = renderChargeAttempts(nil, []domain.InvoiceChargeAttempt{succ}, domain.Invoice{})
	if count(out, "payment") != 1 || out[0].Description != "Payment collected" {
		t.Fatalf("succeeded+unpaid is an anomaly and must render: %+v", out)
	}

	// Pending never renders — the attention banner owns in-flight.
	out = renderChargeAttempts(nil,
		[]domain.InvoiceChargeAttempt{{StripePaymentIntentID: "pi_5", Outcome: domain.ChargeAttemptPending, OccurredAt: wallT}}, domain.Invoice{})
	if len(out) != 0 {
		t.Fatalf("pending must not render: %+v", out)
	}
}
