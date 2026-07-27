package invoice

import "testing"

// TestDescribeDunningEvent_StartCauses locks the cause-aware start row
// (the timeline half of the dunning-start-cause fix): a payment-failure
// start renders as a failed row, a card-less enrollment renders as a
// reminder-cycle row, and legacy rows with no recorded cause keep the
// old generic copy.
func TestDescribeDunningEvent_StartCauses(t *testing.T) {
	cases := []struct {
		reason, wantDesc, wantSeverity string
	}{
		{"payment_failed", "Payment failed — automatic retry scheduled", "failed"},
		{"no_payment_method", "No payment method — reminder cycle started", "scheduled"},
		{"", "Automatic retry scheduled", "scheduled"},
	}
	for _, c := range cases {
		desc, sev := describeDunningEvent("dunning_started", c.reason, 0)
		if desc != c.wantDesc || sev != c.wantSeverity {
			t.Errorf("reason %q: got (%q, %q), want (%q, %q)", c.reason, desc, sev, c.wantDesc, c.wantSeverity)
		}
	}
}
