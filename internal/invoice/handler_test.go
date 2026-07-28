package invoice

import "testing"

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
