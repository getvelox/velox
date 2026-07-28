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
		{"retry_attempted", "no_payment_method", "Payment retry #1 attempted", "scheduled", "No payment method — reminder sent", 1},
		{"retry_attempted", "no payment method for customer", "Payment retry #2 attempted", "scheduled", "No payment method — reminder sent", 2},
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
