package invoice

import "testing"

// Two setup-link emails on one invoice — the finalize-time send and an
// operator's Resend — used to render byte-identical timeline rows: same
// title, same status clause, and on a frozen clock the same instant, so
// "why did this happen twice?" had no answer on the page. The enqueue
// trigger was already captured for the audit log (the resend handler's
// own comment records that "finalize_no_pm" used to be hardcoded); this
// pins that it now reaches the row that displays it, as a cause subline
// per the PR #640 pattern (uniform machine title, cause underneath).
func TestEmailTriggerDetail_DisambiguatesSetupLinkRows(t *testing.T) {
	cases := []struct {
		name      string
		emailType string
		trigger   string
		want      string
	}{
		{"finalize send", "payment_setup_request", "finalize_no_pm",
			"Sent automatically — no payment method on file at finalize"},
		{"auto-charge retry send", "payment_setup_request", "auto_charge_retry_no_pm",
			"Sent automatically — a charge attempt found no payment method"},
		{"operator resend", "payment_setup_request", "operator_resend",
			"Resent by an operator"},
		// Legacy rows (enqueued before the trigger existed) degrade to the
		// old ambiguity — never to a guessed cause.
		{"legacy row", "payment_setup_request", "", ""},
		{"unknown future trigger degrades silently", "payment_setup_request", "some_new_trigger", ""},
		// Other email types carry no cause subline even if a payload
		// happens to hold a trigger key.
		{"non-setup type", "invoice", "operator_resend", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := emailTriggerDetail(tc.emailType, tc.trigger); got != tc.want {
				t.Fatalf("emailTriggerDetail(%q,%q) = %q, want %q", tc.emailType, tc.trigger, got, tc.want)
			}
		})
	}
	// The property the fix exists for: the three real triggers produce
	// three DISTINCT sublines — if any two collapse, the rows are
	// indistinguishable again and the fix has silently regressed.
	seen := map[string]string{}
	for _, tr := range []string{"finalize_no_pm", "auto_charge_retry_no_pm", "operator_resend"} {
		d := emailTriggerDetail("payment_setup_request", tr)
		if d == "" {
			t.Fatalf("trigger %q produced no subline", tr)
		}
		if prev, dup := seen[d]; dup {
			t.Fatalf("triggers %q and %q collapse to the same subline %q", prev, tr, d)
		}
		seen[d] = tr
	}
}
