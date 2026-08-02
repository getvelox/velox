package domain

import (
	"strings"
	"testing"
)

// TestParkedInvoiceAttentionTellsTheTruth pins the banner for an invoice whose
// charge attempt could not be identified at the provider (ADR-107).
//
// Before this, such an invoice rendered the same muted Info banner as a charge
// that really is being confirmed, saying "Velox re-checks automatically and
// resolves once Stripe reports a final outcome" — on day 1 and on day 400. For
// a parked invoice there is nothing to re-check: the reconciler early-returns
// because there is no PaymentIntent id, and the invoice is excluded from every
// charge path. The banner was instructing an operator to wait for something
// that cannot happen, at the lowest severity, with no action offered.
func TestParkedInvoiceAttentionTellsTheTruth(t *testing.T) {
	parked := Invoice{
		Status:                InvoiceFinalized,
		PaymentStatus:         PaymentUnknown,
		StripePaymentIntentID: "", // the defining property
		AmountDueCents:        5000,
	}

	att := classifyPaymentUnconfirmed(parked)
	if att == nil {
		t.Fatal("a parked invoice must raise an attention")
	}

	if att.Severity != AttentionSeverityCritical {
		t.Errorf("severity = %v, want critical — a permanently stuck invoice that needs a human is not an FYI", att.Severity)
	}
	if strings.Contains(att.Message, "re-checks automatically") {
		t.Error("the banner still promises automatic resolution for an invoice that will never resolve automatically")
	}
	// It must say the three things an operator needs (ADR-108): that Velox is
	// actively searching the provider, that an UNFOUND attempt will not
	// resolve on its own, and what to do instead. The bound matters in both
	// directions — an absolute "will not resolve" is a lie now that search
	// adopts found PIs, and an unconditional "resolves automatically" would be
	// the pre-#682 lie again for the rows search never finds.
	if !strings.Contains(att.Message, "searching the provider") {
		t.Error("the banner does not say Velox is searching — an operator would go do the search by hand")
	}
	if !strings.Contains(att.Message, "will not resolve on its own") {
		t.Error("the banner does not tell the operator that an unfound attempt stays stuck")
	}
	if !strings.Contains(att.Message, "if it cannot be found") {
		t.Error("the banner states futility unconditionally — false since ADR-108's search sweep adopts found PaymentIntents")
	}
	if !strings.Contains(strings.ToLower(att.Message), "uncollectible") {
		t.Error("the banner names no way out — mark-uncollectible is the one action the product allows here")
	}
	// And it must explain the deliberate no-charge choice, or the operator will
	// read the silence as a bug and go looking for a retry button.
	if !strings.Contains(strings.ToLower(att.Message), "twice") {
		t.Error("the banner does not explain WHY no further charge is attempted, so the silence looks like a defect")
	}
}

// TestUnconfirmedWithAPaymentIntentKeepsTheWaitingCopy is the negative control.
// A charge that IS being confirmed genuinely resolves itself, so escalating it
// to critical and telling the operator to write it off would be its own lie —
// and would train them to ignore the critical case.
func TestUnconfirmedWithAPaymentIntentKeepsTheWaitingCopy(t *testing.T) {
	confirming := Invoice{
		Status:                InvoiceFinalized,
		PaymentStatus:         PaymentUnknown,
		StripePaymentIntentID: "pi_live_123",
		AmountDueCents:        5000,
	}

	att := classifyPaymentUnconfirmed(confirming)
	if att == nil {
		t.Fatal("want an attention")
	}
	if att.Severity != AttentionSeverityInfo {
		t.Errorf("severity = %v, want info — this one really is resolving itself", att.Severity)
	}
	if !strings.Contains(att.Message, "re-checks automatically") {
		t.Error("a resolvable charge should still tell the operator to wait")
	}
}
