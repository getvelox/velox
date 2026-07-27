package invoice_test

import "time"

// ADR-101 fixture stamps: the interval writers refuse an ACTIVE
// subscription with no billing period (an impossible production state —
// every real activation path stamps one), so active-sub fixtures carry
// theirs. bivPE is one month out — superseded by any later
// UpdateBillingCycle a test performs, so it never fights test-specific
// period math.
func bivPE(t time.Time) *time.Time {
	e := t.AddDate(0, 1, 0)
	return &e
}
