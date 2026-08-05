package invoice

import "testing"

// TestDescribeEmailEvent_DeliveryStateLayering locks the timeline
// grammar for ADR-098: the provider-confirmed outcome layers over a
// completed handoff, the send-lifecycle states keep their renderings,
// and 'skipped' no longer falls through to a "succeeded" row claiming a
// never-sent email was sent.
func TestDescribeEmailEvent_DeliveryStateLayering(t *testing.T) {
	cases := []struct {
		name          string
		status, state string
		wantDesc      string
		wantStatus    string
	}{
		{"dispatched, no verdict", "dispatched", "unknown", "Invoice emailed to customer", "succeeded"},
		{"dispatched, delivered", "dispatched", "delivered", "Invoice emailed to customer — delivered", "succeeded"},
		{"dispatched, bounced", "dispatched", "bounced", "Invoice emailed to customer — bounced", "failed"},
		{"dispatched, complained", "dispatched", "complained", "Invoice emailed to customer — recipient marked it as spam", "failed"},
		{"pending", "pending", "unknown", "Invoice emailed to customer (queued)", "processing"},
		{"failed", "failed", "unknown", "Invoice emailed to customer (delivery failed)", "failed"},
		{"skipped no longer lies", "skipped", "unknown", "Invoice emailed to customer (not sent — the invoice was already closed)", "info"},
	}
	for _, c := range cases {
		desc, status := describeEmailEvent("invoice", c.status, c.state)
		if desc != c.wantDesc || status != c.wantStatus {
			t.Errorf("%s: got (%q, %q), want (%q, %q)", c.name, desc, status, c.wantDesc, c.wantStatus)
		}
	}

	// Non-timeline email types stay off the timeline regardless of state.
	if desc, _ := describeEmailEvent("password_reset", "dispatched", "delivered"); desc != "" {
		t.Errorf("password_reset must stay off the invoice timeline, got %q", desc)
	}
}
