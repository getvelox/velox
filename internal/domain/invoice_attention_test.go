package domain

import (
	"strings"
	"testing"
	"time"
)

// helper: minimal draft invoice in healthy state
func draft() Invoice {
	return Invoice{
		ID:            "vlx_inv_test",
		Status:        InvoiceDraft,
		PaymentStatus: PaymentPending,
		TaxFacts: TaxFacts{
			TaxStatus: InvoiceTaxOK,
		},
	}
}

func TestClassifyInvoiceAttention_HealthyReturnsNil(t *testing.T) {
	if got := ClassifyInvoiceAttention(draft(), AttentionContext{}); got != nil {
		t.Fatalf("healthy invoice should return nil, got %+v", got)
	}
}

func TestClassifyInvoiceAttention_TerminalStatesReturnNil(t *testing.T) {
	for _, status := range []InvoiceStatus{InvoicePaid, InvoiceVoided} {
		t.Run(string(status), func(t *testing.T) {
			inv := draft()
			inv.Status = status
			// Even with active failure modes, terminal status must suppress.
			inv.TaxStatus = InvoiceTaxFailed
			inv.TaxErrorCode = "customer_data_invalid"
			inv.PaymentStatus = PaymentFailed
			if got := ClassifyInvoiceAttention(inv, AttentionContext{}); got != nil {
				t.Fatalf("terminal status %s should suppress attention, got %+v", status, got)
			}
		})
	}
}

func TestClassifyInvoiceAttention_TaxFailedSubcodes(t *testing.T) {
	cases := []struct {
		errorCode   string
		wantReason  AttentionReason
		wantParam   string
		wantPrimAct AttentionAction
	}{
		{"customer_data_invalid", AttentionReasonTaxLocationRequired, "customer.address.postal_code", AttentionActionEditBillingProfile},
		{"jurisdiction_unsupported", AttentionReasonTaxCalculationFailed, "", AttentionActionReviewRegistration},
		{"provider_outage", AttentionReasonTaxCalculationFailed, "", AttentionActionWaitProvider},
		{"provider_auth", AttentionReasonTaxCalculationFailed, "", AttentionActionRotateAPIKey},
		{"unknown", AttentionReasonTaxCalculationFailed, "", AttentionActionRetryTax},
		{"", AttentionReasonTaxCalculationFailed, "", AttentionActionRetryTax}, // empty falls through to unknown branch
	}
	for _, tc := range cases {
		t.Run(tc.errorCode, func(t *testing.T) {
			inv := draft()
			inv.TaxStatus = InvoiceTaxFailed
			inv.TaxErrorCode = tc.errorCode
			inv.TaxPendingReason = "raw provider response"
			now := time.Now()
			inv.TaxDeferredAt = &now

			att := ClassifyInvoiceAttention(inv, AttentionContext{})
			if att == nil {
				t.Fatalf("expected attention, got nil")
			}
			if att.Severity != AttentionSeverityCritical {
				t.Errorf("severity = %s, want critical", att.Severity)
			}
			if att.Reason != tc.wantReason {
				t.Errorf("reason = %s, want %s", att.Reason, tc.wantReason)
			}
			if att.Param != tc.wantParam {
				t.Errorf("param = %q, want %q", att.Param, tc.wantParam)
			}
			if len(att.Actions) == 0 {
				t.Fatalf("expected at least one action")
			}
			if att.Actions[0].Code != tc.wantPrimAct {
				t.Errorf("primary action = %s, want %s", att.Actions[0].Code, tc.wantPrimAct)
			}
			if att.DocURL == "" {
				t.Errorf("expected DocURL to be set")
			}
			// ADR-025: upstream provider responses go in ProviderResponse,
			// not Detail. Every code in this test fixture except
			// provider_not_configured (not exercised here) is post-flight
			// — the raw string came back from the provider.
			if att.ProviderResponse != "raw provider response" {
				t.Errorf("expected ProviderResponse to carry raw provider response, got %q", att.ProviderResponse)
			}
			if att.Detail != "" {
				t.Errorf("Detail should be empty for tax codes — Velox-internal slot is reserved for our own framing, got %q", att.Detail)
			}
			if att.Since == nil {
				t.Errorf("expected Since to be set from TaxDeferredAt")
			}
			if want := "tax." + tc.errorCode; tc.errorCode != "" && att.Code != want {
				t.Errorf("code = %q, want %q", att.Code, want)
			}
		})
	}
}

// TestClassifyInvoiceAttention_ProviderNotConfiguredEmptyResponse asserts
// the ADR-025 contract for the only pre-flight tax code: provider_not_
// configured fired before any HTTP call to Stripe, so neither slot
// should carry the Velox-internal string ("no client configured for
// livemode=…"). The headline + Connect Stripe action carry the whole
// UI; surfacing the internal string under "Provider response" would
// mislead operators into thinking a 4xx came back from Stripe.
func TestClassifyInvoiceAttention_ProviderNotConfiguredEmptyResponse(t *testing.T) {
	inv := draft()
	inv.TaxStatus = InvoiceTaxFailed
	inv.TaxErrorCode = "provider_not_configured"
	inv.TaxPendingReason = "no client configured for livemode=false" // Velox-internal, NOT a provider response
	now := time.Now()
	inv.TaxDeferredAt = &now

	att := ClassifyInvoiceAttention(inv, AttentionContext{})
	if att == nil {
		t.Fatalf("expected attention, got nil")
	}
	if att.ProviderResponse != "" {
		t.Errorf("ProviderResponse should be empty for pre-flight code (we never called the provider), got %q", att.ProviderResponse)
	}
	if att.Detail != "" {
		t.Errorf("Detail should be empty — headline + Connect Stripe action are the whole UI, got %q", att.Detail)
	}
}

// TestClassifyInvoiceAttention_ProviderNotConfigured_StripeConnectedSwapsCopy
// pins the gap-window UX fix: when an invoice has tax_error_code=
// provider_not_configured (stamped at calculation-fail time) AND the
// tenant has now connected Stripe, the banner must NOT keep telling
// the operator to "Connect Stripe in Settings → Payments" — Stripe
// is connected; the only thing the invoice is waiting for is the
// next scheduler tick. Surface the queued-and-retry-now path
// instead.
func TestClassifyInvoiceAttention_ProviderNotConfigured_StripeConnectedSwapsCopy(t *testing.T) {
	inv := draft()
	inv.TaxStatus = InvoiceTaxFailed
	inv.TaxErrorCode = "provider_not_configured"

	t.Run("not connected → connect-stripe action", func(t *testing.T) {
		att := ClassifyInvoiceAttention(inv, AttentionContext{StripeConnected: false})
		if att == nil {
			t.Fatalf("expected attention, got nil")
		}
		if !strings.Contains(att.Message, "Stripe isn't connected") {
			t.Errorf("expected 'Stripe isn't connected' copy, got: %q", att.Message)
		}
		if len(att.Actions) == 0 || att.Actions[0].Code != AttentionActionConnectTaxProvider {
			t.Errorf("expected primary action = connect_tax_provider, got: %+v", att.Actions)
		}
	})

	t.Run("connected → calculation-queued + retry-now", func(t *testing.T) {
		att := ClassifyInvoiceAttention(inv, AttentionContext{StripeConnected: true})
		if att == nil {
			t.Fatalf("expected attention, got nil")
		}
		if strings.Contains(att.Message, "Stripe isn't connected") {
			t.Errorf("must NOT say 'Stripe isn't connected' when Stripe IS connected, got: %q", att.Message)
		}
		if !strings.Contains(att.Message, "queued") && !strings.Contains(att.Message, "retry") {
			t.Errorf("expected queued/retry-shortly copy, got: %q", att.Message)
		}
		if len(att.Actions) != 1 || att.Actions[0].Code != AttentionActionRetryTax {
			t.Errorf("connected branch should expose only Retry now (no Connect Stripe), got: %+v", att.Actions)
		}
		if att.Severity != AttentionSeverityInfo {
			t.Errorf("queued state should be info severity (transient, system will resolve), got: %v", att.Severity)
		}
	})
}

func TestClassifyInvoiceAttention_TaxPendingIsWarning(t *testing.T) {
	inv := draft()
	inv.TaxStatus = InvoiceTaxPending
	inv.TaxErrorCode = "customer_data_invalid"
	att := ClassifyInvoiceAttention(inv, AttentionContext{})
	if att == nil || att.Severity != AttentionSeverityWarning {
		t.Fatalf("pending should be warning, got %+v", att)
	}
	if att.Reason != AttentionReasonTaxLocationRequired {
		t.Errorf("reason = %s, want %s", att.Reason, AttentionReasonTaxLocationRequired)
	}
}

func TestClassifyInvoiceAttention_PaymentFailed(t *testing.T) {
	inv := draft()
	inv.PaymentStatus = PaymentFailed
	inv.LastPaymentError = "card declined: insufficient funds"
	inv.UpdatedAt = time.Now()

	att := ClassifyInvoiceAttention(inv, AttentionContext{})
	if att == nil {
		t.Fatalf("expected attention")
	}
	if att.Severity != AttentionSeverityCritical {
		t.Errorf("severity = %s, want critical", att.Severity)
	}
	if att.Reason != AttentionReasonPaymentFailed {
		t.Errorf("reason = %s, want %s", att.Reason, AttentionReasonPaymentFailed)
	}
	// Operator voice: the banner headline is Velox's sentence, never the
	// provider's cardholder-facing string ("Your card was declined" read
	// as Velox addressing the operator). The provider's exact wording is
	// preserved verbatim in ProviderResponse.
	if att.Message != "The customer's card was declined." {
		t.Errorf("message = %q, want the operator-voice headline", att.Message)
	}
	if att.ProviderResponse != "card declined: insufficient funds" {
		t.Errorf("provider response = %q, want the verbatim provider string", att.ProviderResponse)
	}
	// Primary action is now update_payment_method (the card on file
	// is broken — retrying the same card will decline again).
	// retry_payment remains as the secondary action for transient
	// declines where the operator wants to re-attempt without
	// changing the card.
	if len(att.Actions) < 2 ||
		att.Actions[0].Code != AttentionActionUpdatePaymentMethod ||
		att.Actions[1].Code != AttentionActionRetryPayment {
		t.Errorf("expected actions [update_payment_method, retry_payment], got %v", att.Actions)
	}
	// ADR-025: LastPaymentError is Stripe's last_payment_error body,
	// upstream payload — goes in ProviderResponse, not Detail.
	if att.ProviderResponse != "card declined: insufficient funds" {
		t.Errorf("expected ProviderResponse to carry LastPaymentError, got %q", att.ProviderResponse)
	}
	if att.Detail != "" {
		t.Errorf("Detail should be empty (no Velox-internal context for this code yet), got %q", att.Detail)
	}
}

func TestClassifyInvoiceAttention_PaymentUnknownIsInfo(t *testing.T) {
	inv := draft()
	inv.PaymentStatus = PaymentUnknown
	// A PaymentIntent id is what makes this the RESOLVING case — the reconciler
	// has something to query, so Info is honest. Without one the invoice is
	// parked and will never resolve, which is Critical
	// (TestParkedInvoiceAttentionTellsTheTruth). This fixture predates that
	// split and meant the resolving case.
	inv.StripePaymentIntentID = "pi_resolving"
	att := ClassifyInvoiceAttention(inv, AttentionContext{})
	if att == nil || att.Severity != AttentionSeverityInfo {
		t.Fatalf("payment_unknown should be info, got %+v", att)
	}
	if att.Reason != AttentionReasonPaymentUnconfirmed {
		t.Errorf("reason = %s, want %s", att.Reason, AttentionReasonPaymentUnconfirmed)
	}
}

func TestClassifyInvoiceAttention_PriorityOrder(t *testing.T) {
	// Tax_failed must beat payment_failed must beat payment_unknown.
	inv := draft()
	inv.TaxStatus = InvoiceTaxFailed
	inv.TaxErrorCode = "customer_data_invalid"
	inv.PaymentStatus = PaymentFailed // also bad

	att := ClassifyInvoiceAttention(inv, AttentionContext{})
	if att == nil {
		t.Fatalf("expected attention")
	}
	if att.Reason != AttentionReasonTaxLocationRequired {
		t.Errorf("priority broken: tax_failed should win, got %s", att.Reason)
	}

	// Drop tax — payment_failed should now win over payment_unknown.
	inv.TaxStatus = InvoiceTaxOK
	inv.TaxErrorCode = ""
	att = ClassifyInvoiceAttention(inv, AttentionContext{})
	if att == nil || att.Reason != AttentionReasonPaymentFailed {
		t.Errorf("priority broken: payment_failed should beat payment_unknown, got %+v", att)
	}

	// Drop payment_failed — payment_unknown remains.
	inv.PaymentStatus = PaymentUnknown
	att = ClassifyInvoiceAttention(inv, AttentionContext{})
	if att == nil || att.Reason != AttentionReasonPaymentUnconfirmed {
		t.Errorf("priority broken: payment_unknown should remain, got %+v", att)
	}
}

func TestClassifyInvoiceAttention_PaymentProcessing(t *testing.T) {
	inv := draft()
	inv.Status = InvoiceFinalized
	inv.PaymentStatus = PaymentProcessing
	inv.UpdatedAt = time.Now()

	att := ClassifyInvoiceAttention(inv, AttentionContext{})
	if att == nil {
		t.Fatalf("expected attention")
	}
	if att.Severity != AttentionSeverityInfo {
		t.Errorf("severity = %s, want info", att.Severity)
	}
	if att.Reason != AttentionReasonPaymentProcessing {
		t.Errorf("reason = %s, want %s", att.Reason, AttentionReasonPaymentProcessing)
	}
	if len(att.Actions) != 0 {
		t.Errorf("processing should expose no actions (waiting on provider), got %d", len(att.Actions))
	}
}

func TestClassifyInvoiceAttention_PaymentScheduled(t *testing.T) {
	inv := draft()
	inv.Status = InvoiceFinalized
	inv.PaymentStatus = PaymentPending
	inv.AutoChargePending = true
	inv.UpdatedAt = time.Now()

	// payment_scheduled requires HasPaymentMethod=true: when both
	// auto_charge_pending AND no PM, no_payment_method wins (the
	// scheduler retry would skip again until PM is attached, so
	// "engine will retry" would lie to the operator). See
	// TestClassifyInvoiceAttention_NoPaymentMethod_BeatsScheduled.
	att := ClassifyInvoiceAttention(inv, AttentionContext{HasPaymentMethod: true})
	if att == nil {
		t.Fatalf("expected attention")
	}
	if att.Severity != AttentionSeverityInfo {
		t.Errorf("severity = %s, want info", att.Severity)
	}
	if att.Reason != AttentionReasonPaymentScheduled {
		t.Errorf("reason = %s, want %s", att.Reason, AttentionReasonPaymentScheduled)
	}
	if len(att.Actions) == 0 || att.Actions[0].Code != AttentionActionChargeNow {
		t.Errorf("expected primary action charge_now, got %+v", att.Actions)
	}
	// Wall-clock invoice: message points at the scheduler tick.
	if !strings.Contains(att.Message, "next tick") {
		t.Errorf("wall-clock message = %q, want it to mention the scheduler tick", att.Message)
	}

	// Simulated (clock-pinned) invoice: the wall-clock sweep excludes it, so
	// the message must point at advancing the test clock — not "next tick",
	// which would never fire in real time (ADR-028/029 disjoint flows).
	inv.IsSimulated = true
	simAtt := ClassifyInvoiceAttention(inv, AttentionContext{HasPaymentMethod: true})
	if simAtt == nil || simAtt.Reason != AttentionReasonPaymentScheduled {
		t.Fatalf("simulated: expected payment_scheduled attention, got %+v", simAtt)
	}
	if !strings.Contains(simAtt.Message, "test-clock advance") {
		t.Errorf("simulated message = %q, want it to mention the test-clock advance", simAtt.Message)
	}
	if strings.Contains(simAtt.Message, "next tick") {
		t.Errorf("simulated message = %q must NOT promise a wall-clock tick", simAtt.Message)
	}
}

// TestClassifyInvoiceAttention_NoPaymentMethod_BeatsScheduled pins the
// priority order: when an invoice has both auto_charge_pending=true
// AND no PM ready, the classifier surfaces no_payment_method (the
// actionable reason) — surfacing payment_scheduled would tell the
// operator "engine will retry on its next tick" when in fact the
// retry will skip again until a PM is attached.
func TestClassifyInvoiceAttention_NoPaymentMethod_BeatsScheduled(t *testing.T) {
	inv := draft()
	inv.Status = InvoiceFinalized
	inv.PaymentStatus = PaymentPending
	inv.AutoChargePending = true // engine queued for retry post-no-PM-finalize
	inv.UpdatedAt = time.Now()

	att := ClassifyInvoiceAttention(inv, AttentionContext{HasPaymentMethod: false})
	if att == nil {
		t.Fatalf("expected attention")
	}
	if att.Reason != AttentionReasonNoPaymentMethod {
		t.Errorf("reason = %s, want %s (no_payment_method must beat payment_scheduled when PM is missing)", att.Reason, AttentionReasonNoPaymentMethod)
	}
}

func TestClassifyInvoiceAttention_AwaitingPayment(t *testing.T) {
	inv := draft()
	inv.Status = InvoiceFinalized
	inv.PaymentStatus = PaymentPending
	// AutoChargePending = false (default) — no scheduler queue, no charge yet.
	inv.UpdatedAt = time.Now()

	// HasPaymentMethod=true: PM is on file but engine hasn't run yet.
	// This is the race-window case; classifier should pick awaiting_
	// payment (generic info), not no_payment_method.
	att := ClassifyInvoiceAttention(inv, AttentionContext{HasPaymentMethod: true})
	if att == nil {
		t.Fatalf("expected attention")
	}
	if att.Severity != AttentionSeverityInfo {
		t.Errorf("severity = %s, want info", att.Severity)
	}
	if att.Reason != AttentionReasonAwaitingPayment {
		t.Errorf("reason = %s, want %s", att.Reason, AttentionReasonAwaitingPayment)
	}
	codes := make(map[AttentionAction]bool)
	for _, a := range att.Actions {
		codes[a.Code] = true
	}
	if !codes[AttentionActionChargeNow] || !codes[AttentionActionSendReminder] {
		t.Errorf("awaiting_payment should offer charge_now + send_reminder, got %+v", att.Actions)
	}
	if att.NextAttemptAt != nil {
		t.Errorf("awaiting_payment must not set NextAttemptAt — engine has nothing scheduled, got %v", att.NextAttemptAt)
	}
}

// TestClassifyInvoiceAttention_NoPaymentMethod pins the operator-
// actionable distinction: a finalized invoice with no PaymentSetup
// surfaces no_payment_method (warning, action: add_payment_method),
// not the generic awaiting_payment. Without this branch, operators
// see "Invoice is finalized and awaiting payment" and have no
// indication that the engine will never auto-charge.
func TestClassifyInvoiceAttention_NoPaymentMethod(t *testing.T) {
	inv := draft()
	inv.Status = InvoiceFinalized
	inv.PaymentStatus = PaymentPending
	inv.UpdatedAt = time.Now()

	// Customer HAS an email → the engine emailed a setup link, so the banner
	// claims it and offers both a resend and the customer-page path.
	att := ClassifyInvoiceAttention(inv, AttentionContext{HasPaymentMethod: false, CustomerHasEmail: true})
	if att == nil {
		t.Fatalf("expected attention")
	}
	if att.Reason != AttentionReasonNoPaymentMethod {
		t.Errorf("reason = %s, want %s", att.Reason, AttentionReasonNoPaymentMethod)
	}
	if att.Severity != AttentionSeverityWarning {
		t.Errorf("severity = %s, want warning", att.Severity)
	}
	codes := make(map[AttentionAction]bool)
	for _, a := range att.Actions {
		codes[a.Code] = true
	}
	if !codes[AttentionActionAddPaymentMethod] {
		t.Errorf("no_payment_method must offer add_payment_method, got %+v", att.Actions)
	}
	if !codes[AttentionActionSendReminder] {
		t.Errorf("has-email no_payment_method must offer a resend, got %+v", att.Actions)
	}
	// Disposition form (2026-07-10): the banner states what the ENGINE DOES
	// and where delivery is verifiable — never "has been emailed", which is
	// unverifiable from the classifier's inputs (false under suppression/DLQ).
	if strings.Contains(att.Message, "has been emailed") {
		t.Errorf("banner must not assert a completed send it cannot observe, got %q", att.Message)
	}
	if !strings.Contains(att.Message, "emails the customer a setup link") {
		t.Errorf("has-email variant must state the engine's send behavior, got %q", att.Message)
	}
	if att.NextAttemptAt != nil {
		t.Errorf("no_payment_method must not set NextAttemptAt — engine won't auto-charge without PM, got %v", att.NextAttemptAt)
	}
}

// TestClassifyInvoiceAttention_NoPaymentMethod_NoEmail pins the honest variant
// for a customer with no email on file: the engine's setup-link email skips
// silently (no address), so the banner must NOT claim it was emailed, must NOT
// offer a resend that can't send, and must point the operator at the copy-a-
// link / add-an-email path (add_payment_method). Zero-value CustomerHasEmail
// (the conservative default) selects this variant.
func TestClassifyInvoiceAttention_NoPaymentMethod_NoEmail(t *testing.T) {
	inv := draft()
	inv.Status = InvoiceFinalized
	inv.PaymentStatus = PaymentPending
	inv.UpdatedAt = time.Now()

	att := ClassifyInvoiceAttention(inv, AttentionContext{HasPaymentMethod: false, CustomerHasEmail: false})
	if att == nil {
		t.Fatalf("expected attention")
	}
	if att.Reason != AttentionReasonNoPaymentMethod {
		t.Errorf("reason = %s, want %s", att.Reason, AttentionReasonNoPaymentMethod)
	}
	if strings.Contains(att.Message, "has been emailed") {
		t.Errorf("no-email variant must NOT claim a setup link was emailed, got %q", att.Message)
	}
	// Honest under uncertainty: states engine behavior ("only when … an email
	// address on file"), never asserts this customer's email state — so it's
	// correct whether the address is confirmably absent or merely undetermined.
	if !strings.Contains(att.Message, "only when the customer has an email address on file") {
		t.Errorf("no-email variant must state the conditional engine behavior, got %q", att.Message)
	}
	codes := make(map[AttentionAction]bool)
	for _, a := range att.Actions {
		codes[a.Code] = true
	}
	if !codes[AttentionActionAddPaymentMethod] {
		t.Errorf("no-email variant must offer add_payment_method (copy link / add email), got %+v", att.Actions)
	}
	if codes[AttentionActionSendReminder] {
		t.Errorf("no-email variant must NOT offer a resend that can't send, got %+v", att.Actions)
	}
}

func TestClassifyInvoiceAttention_DraftSuppressesAttention(t *testing.T) {
	inv := draft()
	// Status=draft, payment_status=pending — should NOT raise attention
	// (the page itself communicates draft state).
	att := ClassifyInvoiceAttention(inv, AttentionContext{})
	if att != nil {
		t.Errorf("draft should suppress info attention, got %+v", att)
	}
}

func TestClassifyInvoiceAttention_EmptyPaymentErrorFallsBackToGeneric(t *testing.T) {
	inv := draft()
	inv.PaymentStatus = PaymentFailed
	inv.LastPaymentError = ""
	att := ClassifyInvoiceAttention(inv, AttentionContext{})
	if att == nil {
		t.Fatalf("expected attention")
	}
	if att.Message == "" {
		t.Errorf("expected non-empty fallback message")
	}
}

// TestClassify_PaymentProcessing_AgeAware locks ADR-049 Phase 4: a fresh
// in-flight payment is Info ("resolves automatically"), but a REAL invoice
// stuck processing past the expected-settle window escalates to Warning and
// points the operator at Stripe — while a SIMULATED invoice never escalates
// (its age is sim-time, not a real-world duration) and a zero Now stays Info.
func TestClassify_PaymentProcessing_AgeAware(t *testing.T) {
	now := time.Date(2026, 6, 7, 12, 0, 0, 0, time.UTC)
	base := func(updatedAgo time.Duration, simulated bool) Invoice {
		return Invoice{
			ID: "vlx_inv_test", Status: InvoiceFinalized,
			PaymentStatus: PaymentProcessing, TaxFacts: TaxFacts{TaxStatus: InvoiceTaxOK},
			UpdatedAt: now.Add(-updatedAgo), IsSimulated: simulated,
		}
	}

	t.Run("fresh real → Info, auto-resolve copy", func(t *testing.T) {
		att := ClassifyInvoiceAttention(base(1*time.Hour, false), AttentionContext{Now: now})
		if att == nil || att.Severity != AttentionSeverityInfo {
			t.Fatalf("att = %+v, want Info", att)
		}
		if !strings.Contains(att.Message, "automatically") {
			t.Errorf("fresh copy = %q, want it to mention automatic confirmation", att.Message)
		}
	})

	t.Run("stale real → Warning, points to Stripe", func(t *testing.T) {
		att := ClassifyInvoiceAttention(base(7*time.Hour, false), AttentionContext{Now: now})
		if att == nil || att.Severity != AttentionSeverityWarning {
			t.Fatalf("att = %+v, want Warning past the stale window", att)
		}
		if !strings.Contains(att.Message, "Stripe") {
			t.Errorf("stale copy = %q, want it to point at Stripe (no false auto-resolve promise)", att.Message)
		}
	})

	t.Run("stale but simulated → stays Info", func(t *testing.T) {
		att := ClassifyInvoiceAttention(base(7*time.Hour, true), AttentionContext{Now: now})
		if att == nil || att.Severity != AttentionSeverityInfo {
			t.Errorf("simulated invoice escalated on a wall-clock age: %+v", att)
		}
	})

	t.Run("zero Now → stays Info", func(t *testing.T) {
		att := ClassifyInvoiceAttention(base(7*time.Hour, false), AttentionContext{})
		if att == nil || att.Severity != AttentionSeverityInfo {
			t.Errorf("zero Now must not escalate: %+v", att)
		}
	})
}

// TestClassify_PaymentUnconfirmed_NoDeadAction: the unconfirmed banner no longer
// ships a non-functional "Check provider" button — the reconciler resolves it
// automatically (ADR-049 Phase 2); an on-demand action is deferred.
func TestClassify_PaymentUnconfirmed_NoDeadAction(t *testing.T) {
	inv := Invoice{
		ID: "vlx_inv_test", Status: InvoiceFinalized,
		PaymentStatus: PaymentUnknown, TaxFacts: TaxFacts{TaxStatus: InvoiceTaxOK},
		// The resolving case — see the note in
		// TestClassifyInvoiceAttention_PaymentUnknownIsInfo.
		StripePaymentIntentID: "pi_resolving",
	}
	att := ClassifyInvoiceAttention(inv, AttentionContext{})
	if att == nil || att.Code != "payment.unconfirmed" {
		t.Fatalf("att = %+v, want payment.unconfirmed", att)
	}
	if len(att.Actions) != 0 {
		t.Errorf("unconfirmed banner carries %d actions, want 0 (dead disabled button removed)", len(att.Actions))
	}
}

// TestClassifyTaxAttention_RetryPolicyTruth locks the 2026-07-19 truth-audit
// fixes: the banner's retry claims mirror the reconciler's actual policy.
// Pre-fix: "will retry automatically" rendered forever after the cap
// excluded the invoice; provider_not_configured promised scheduler retries
// the predicate never made; NextAttemptAt was populated by no code path.
func TestClassifyTaxAttention_RetryPolicyTruth(t *testing.T) {
	next := time.Date(2027, 6, 2, 9, 0, 0, 0, time.UTC)
	base := Invoice{Status: InvoiceFinalized, TaxFacts: TaxFacts{TaxStatus: InvoiceTaxPending}}

	t.Run("retries remaining: Warning, attempts-used copy, real NextAttemptAt", func(t *testing.T) {
		inv := base
		inv.TaxErrorCode = "provider_outage"
		inv.TaxRetryCount = 3
		inv.TaxNextRetryAt = &next
		att := ClassifyInvoiceAttention(inv, AttentionContext{})
		if att == nil || att.Severity != AttentionSeverityWarning {
			t.Fatalf("want Warning while retries remain, got %+v", att)
		}
		if !strings.Contains(att.Message, "3 of 8 attempts used") {
			t.Errorf("message must state real attempt usage, got %q", att.Message)
		}
		if att.NextAttemptAt == nil || !att.NextAttemptAt.Equal(next) {
			t.Errorf("NextAttemptAt must surface the reconciler's tax_next_retry_at, got %v", att.NextAttemptAt)
		}
	})

	t.Run("exhausted: escalates to Critical, no NextAttemptAt, no retry promise", func(t *testing.T) {
		inv := base
		inv.TaxErrorCode = "provider_outage"
		inv.TaxRetryCount = MaxTaxRetryAttempts
		inv.TaxNextRetryAt = &next // stale row value must NOT surface
		att := ClassifyInvoiceAttention(inv, AttentionContext{})
		if att == nil || att.Severity != AttentionSeverityCritical {
			t.Fatalf("exhausted retries must escalate to Critical, got %+v", att)
		}
		if att.NextAttemptAt != nil {
			t.Error("no next attempt exists after exhaustion — surfacing one is a lie")
		}
		if strings.Contains(att.Message, "retries automatically") {
			t.Errorf("exhausted banner must not promise automatic retries: %q", att.Message)
		}
	})

	t.Run("not_configured + connected: Info with truthful queue copy", func(t *testing.T) {
		inv := base
		inv.TaxErrorCode = "provider_not_configured"
		inv.TaxRetryCount = 1
		att := ClassifyInvoiceAttention(inv, AttentionContext{StripeConnected: true})
		if att == nil || att.Severity != AttentionSeverityInfo {
			t.Fatalf("want Info for queued post-connect recompute, got %+v", att)
		}
		if strings.Contains(att.Message, "scheduler tick") {
			t.Errorf("copy must not reference internals; got %q", att.Message)
		}
	})

	t.Run("not_configured is genuinely retryable by the policy the banner asserts", func(t *testing.T) {
		if !TaxErrorCodeRetryable("provider_not_configured") {
			t.Error("banner promises automatic recompute; the policy must actually include provider_not_configured")
		}
	})
}

// TestClassifyInvoiceAttention_NoPaymentMethod_Simulated pins the clock-aware
// auto-charge reassurance (2026-07-22 payment-surfacing audit, P1-1): the
// wall-clock pending-charge sweep excludes clock-pinned subs, so on a
// simulated invoice the banner must promise collection on the next
// test-clock advance — not "on the next tick", which never comes in real
// time. classifyPaymentScheduled got this carve-out first;
// no_payment_method must match. Mutation seam: drop the IsSimulated branch
// in classifyNoPaymentMethod and both assertions fail.
func TestClassifyInvoiceAttention_NoPaymentMethod_Simulated(t *testing.T) {
	inv := draft()
	inv.Status = InvoiceFinalized
	inv.PaymentStatus = PaymentPending
	inv.IsSimulated = true
	inv.UpdatedAt = time.Now()

	att := ClassifyInvoiceAttention(inv, AttentionContext{HasPaymentMethod: false, CustomerHasEmail: true})
	if att == nil || att.Reason != AttentionReasonNoPaymentMethod {
		t.Fatalf("expected no_payment_method, got %+v", att)
	}
	if !strings.Contains(att.Message, "next test-clock advance") {
		t.Errorf("simulated no-PM banner must promise collection on the next clock advance, got %q", att.Message)
	}
	if strings.Contains(att.Message, "next billing tick") {
		t.Errorf("simulated no-PM banner must not promise a wall-clock tick that excludes clock-pinned subs, got %q", att.Message)
	}
}

// TestClassifyInvoiceAttention_DunningExhausted pins the escalated-run state
// (2026-07-22 audit, P1-4): once the invoice's dunning run has escalated,
// the banner must stop implying recovery is still running — it names the
// exhaustion, keeps charge-on-attach as the fix, and keeps the resend
// action only when an email exists. Mutation seam: drop the
// atc.DunningEscalated branch and every assertion here fails.
func TestClassifyInvoiceAttention_DunningExhausted(t *testing.T) {
	inv := draft()
	inv.Status = InvoiceFinalized
	inv.PaymentStatus = PaymentPending
	inv.UpdatedAt = time.Now()

	att := ClassifyInvoiceAttention(inv, AttentionContext{
		HasPaymentMethod: false, CustomerHasEmail: true, DunningEscalated: true,
	})
	if att == nil {
		t.Fatal("expected attention")
	}
	if att.Reason != AttentionReasonDunningExhausted {
		t.Fatalf("reason = %s, want %s", att.Reason, AttentionReasonDunningExhausted)
	}
	if att.Code != "payment.dunning_exhausted" {
		t.Errorf("code = %s, want payment.dunning_exhausted", att.Code)
	}
	if !strings.Contains(att.Message, "ended without collecting") {
		t.Errorf("escalated banner must state recovery ended, got %q", att.Message)
	}
	if !strings.Contains(att.Message, "Attaching a card") {
		t.Errorf("escalated banner must keep charge-on-attach as the fix, got %q", att.Message)
	}
	codes := make(map[AttentionAction]bool)
	for _, a := range att.Actions {
		codes[a.Code] = true
	}
	if !codes[AttentionActionSendReminder] || !codes[AttentionActionAddPaymentMethod] {
		t.Errorf("escalated (has-email) must offer resend + customer page, got %+v", att.Actions)
	}

	// No email: the resend action must drop, same rule as the base branch.
	att = ClassifyInvoiceAttention(inv, AttentionContext{
		HasPaymentMethod: false, CustomerHasEmail: false, DunningEscalated: true,
	})
	if att.Reason != AttentionReasonDunningExhausted {
		t.Fatalf("no-email escalated reason = %s, want dunning_exhausted", att.Reason)
	}
	for _, a := range att.Actions {
		if a.Code == AttentionActionSendReminder {
			t.Errorf("no-email escalated must not offer a resend that cannot send, got %+v", att.Actions)
		}
	}
}

// TestClassifyInvoiceAttention_SinceIsSimDomainOnSimulatedInvoices pins
// the 2026-07-26 fix (found live, FLOW I12 fixture NIM-000258): the
// banner's `since` was sourced from UpdatedAt — a wall-clock DB stamp —
// while a simulated invoice's page anchors relative time to the sim
// axis, so a one-off invoice finalized at sim-today rendered "since
// 70d ago" (the wall→sim distance). Simulated invoices anchor since to
// IssuedAt (sim-domain); wall-clock invoices keep UpdatedAt.
func TestClassifyInvoiceAttention_SinceIsSimDomainOnSimulatedInvoices(t *testing.T) {
	simIssued := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	wallUpdated := time.Date(2026, 7, 26, 17, 53, 0, 0, time.UTC)

	inv := draft()
	inv.Status = InvoiceFinalized
	inv.PaymentStatus = PaymentPending
	inv.AmountDueCents = 4300
	inv.IsSimulated = true
	inv.IssuedAt = &simIssued
	inv.UpdatedAt = wallUpdated

	att := ClassifyInvoiceAttention(inv, AttentionContext{})
	if att == nil || att.Since == nil {
		t.Fatalf("expected an attention with Since, got %+v", att)
	}
	if !att.Since.Equal(simIssued) {
		t.Errorf("simulated invoice Since = %v, want the sim-domain IssuedAt %v (wall UpdatedAt %v leaks cross-domain deltas)", att.Since, simIssued, wallUpdated)
	}

	inv.IsSimulated = false
	att = ClassifyInvoiceAttention(inv, AttentionContext{})
	if att == nil || att.Since == nil {
		t.Fatalf("expected an attention with Since on the wall twin")
	}
	if !att.Since.Equal(wallUpdated) {
		t.Errorf("wall-clock invoice Since = %v, want UpdatedAt %v", att.Since, wallUpdated)
	}
}

// TestClassify_CollectionPaused_BeatsPaymentScheduled pins that a queued
// invoice on a paused subscription does not claim a charge is coming.
//
// Both auto-charge sweeps skip paused subscriptions, so the queue flag alone
// no longer means "the engine will charge this". The state is reachable
// whenever a subscription is paused AFTER its invoice was queued — which is
// exactly what dunning's `pause` final action does to every in-flight invoice.
func TestClassify_CollectionPaused_BeatsPaymentScheduled(t *testing.T) {
	inv := Invoice{
		Status:            InvoiceFinalized,
		PaymentStatus:     PaymentPending,
		AutoChargePending: true,
		TaxFacts:          TaxFacts{TaxStatus: InvoiceTaxOK},
	}

	scheduled := ClassifyInvoiceAttention(inv, AttentionContext{HasPaymentMethod: true})
	if scheduled == nil || scheduled.Reason != AttentionReasonPaymentScheduled {
		t.Fatalf("unpaused: reason = %v, want payment_scheduled", scheduled)
	}

	paused := ClassifyInvoiceAttention(inv, AttentionContext{HasPaymentMethod: true, CollectionPaused: true})
	if paused == nil {
		t.Fatal("paused: got no attention, want collection_paused")
	}
	if paused.Reason != AttentionReasonCollectionPaused {
		t.Errorf("paused: reason = %s, want collection_paused", paused.Reason)
	}
	if strings.Contains(paused.Message, "next tick") || strings.Contains(paused.Message, "next test-clock advance") {
		t.Errorf("paused copy must not promise an automatic charge, got %q", paused.Message)
	}
	// Charge now survives: a manual collect is an explicit operator override
	// and has no pause filter.
	var hasChargeNow bool
	for _, a := range paused.Actions {
		if a.Code == AttentionActionChargeNow {
			hasChargeNow = true
		}
	}
	if !hasChargeNow {
		t.Error("paused banner must still offer Charge now — the pause governs automation, not the operator")
	}
}

// ---------------------------------------------------------------------------
// Commit exposure (ADR-078 D3 fast-follow)
//
// The property under test is the FOLD: exposure must enrich whatever cause the
// chain found without replacing it. A commit invoice is unpaid-with-live-credit
// for its whole Net-30 term, so a version of this that won the priority chain
// would both cry wolf on healthy deals and mask the declined card underneath.
// ---------------------------------------------------------------------------

// commitInv is a finalized invoice that funded a commit grant.
func commitInv() Invoice {
	inv := draft()
	inv.Status = InvoiceFinalized
	inv.Currency = "USD"
	return inv
}

// liveGrant is the context for a grant with `drawable` unspent and `spent` gone.
func liveGrant(drawable, spent int64) AttentionContext {
	return AttentionContext{
		CommitGrantDrawableCents: drawable,
		CommitGrantConsumedCents: spent,
	}
}

func TestCommitExposure_NoGrantLeavesChainUntouched(t *testing.T) {
	// Negative control: the same invoice, with and without grant context, must
	// classify identically. Without this the fold could be firing on every
	// invoice and the other tests would still pass.
	inv := commitInv()
	inv.PaymentStatus = PaymentFailed

	bare := ClassifyInvoiceAttention(inv, AttentionContext{})
	withCtx := ClassifyInvoiceAttention(inv, liveGrant(0, 0))
	if bare == nil || withCtx == nil {
		t.Fatalf("expected a banner in both cases, got %+v / %+v", bare, withCtx)
	}
	if bare.Message != withCtx.Message || bare.Severity != withCtx.Severity ||
		len(bare.Actions) != len(withCtx.Actions) {
		t.Fatalf("zero-grant context changed the banner:\n bare = %+v\n with = %+v", bare, withCtx)
	}
}

func TestCommitExposure_FoldsOntoPaymentFailedWithoutMasking(t *testing.T) {
	inv := commitInv()
	inv.PaymentStatus = PaymentFailed
	inv.LastPaymentError = "Your card was declined"

	got := ClassifyInvoiceAttention(inv, liveGrant(10000000, 0))
	if got == nil {
		t.Fatal("expected a banner")
	}
	// The cause survives — this is the masking regression the fold exists to
	// prevent.
	if got.Reason != AttentionReasonPaymentFailed {
		t.Fatalf("exposure masked the cause: reason = %q, want %q", got.Reason, AttentionReasonPaymentFailed)
	}
	// The primary CTA is still "fix the card", not "void the invoice".
	if len(got.Actions) == 0 || got.Actions[0].Code != AttentionActionUpdatePaymentMethod {
		t.Fatalf("primary action displaced: %+v", got.Actions)
	}
	if got.Actions[len(got.Actions)-1].Code != AttentionActionVoidInvoice {
		t.Fatalf("void action not appended: %+v", got.Actions)
	}
	// Both facts are in the operator-visible message.
	if !strings.Contains(got.Message, "card was declined") {
		t.Fatalf("cause sentence lost: %q", got.Message)
	}
	if !strings.Contains(got.Message, "100,000.00 USD") {
		t.Fatalf("exposure amount missing or unformatted: %q", got.Message)
	}
}

func TestCommitExposure_SpentCreditEscalatesSeverity(t *testing.T) {
	inv := commitInv()
	inv.PaymentStatus = PaymentPending // → no_payment_method, a Warning

	unspent := ClassifyInvoiceAttention(inv, liveGrant(10000000, 0))
	if unspent == nil || unspent.Severity != AttentionSeverityWarning {
		t.Fatalf("nothing spent should stay Warning, got %+v", unspent)
	}
	spent := ClassifyInvoiceAttention(inv, liveGrant(5620000, 4380000))
	if spent == nil || spent.Severity != AttentionSeverityCritical {
		t.Fatalf("spent credit should escalate to Critical, got %+v", spent)
	}
	// Escalation must not rewrite the reason.
	if spent.Reason != unspent.Reason {
		t.Fatalf("escalation changed the reason: %q → %q", unspent.Reason, spent.Reason)
	}
	if !strings.Contains(spent.Message, "43,800.00 USD already spent") {
		t.Fatalf("spent amount missing: %q", spent.Message)
	}
}

func TestCommitExposure_NeverLowersSeverity(t *testing.T) {
	// A Critical cause with an unspent grant (whose floor is only Warning)
	// must stay Critical.
	inv := commitInv()
	inv.PaymentStatus = PaymentFailed
	got := ClassifyInvoiceAttention(inv, liveGrant(10000000, 0))
	if got == nil || got.Severity != AttentionSeverityCritical {
		t.Fatalf("severity was lowered: %+v", got)
	}
}

func TestCommitExposure_SynthesizesOnUncollectible(t *testing.T) {
	// Write-off does NOT retire the grant (ADR-078 D3) — the chain returns nil
	// for uncollectible, so this is the state that would otherwise be entirely
	// unlit while credit keeps going out the door.
	inv := commitInv()
	inv.Status = InvoiceUncollectible

	if bare := ClassifyInvoiceAttention(inv, AttentionContext{}); bare != nil {
		t.Fatalf("precondition: uncollectible without a grant should be silent, got %+v", bare)
	}
	got := ClassifyInvoiceAttention(inv, liveGrant(10000000, 0))
	if got == nil {
		t.Fatal("uncollectible with a live grant must raise a banner")
	}
	if got.Reason != AttentionReasonCommitExposure {
		t.Fatalf("reason = %q, want %q", got.Reason, AttentionReasonCommitExposure)
	}
	if len(got.Actions) != 1 || got.Actions[0].Code != AttentionActionVoidInvoice {
		t.Fatalf("void should be the only action: %+v", got.Actions)
	}
}

func TestCommitExposure_FullySpentOffersNoVoid(t *testing.T) {
	// Void retires only the unspent balance. With nothing left, the button
	// would destroy the invoice and recover nothing.
	inv := commitInv()
	inv.Status = InvoiceUncollectible
	got := ClassifyInvoiceAttention(inv, liveGrant(0, 10000000))
	if got == nil {
		t.Fatal("a fully-spent unpaid grant is still worth reporting")
	}
	if len(got.Actions) != 0 {
		t.Fatalf("no action should be offered when void recovers nothing: %+v", got.Actions)
	}
	if !strings.Contains(got.Message, "cannot recover it") {
		t.Fatalf("message should say void cannot recover: %q", got.Message)
	}
	if got.Severity != AttentionSeverityCritical {
		t.Fatalf("fully-spent-and-unpaid is the worst case, want Critical, got %q", got.Severity)
	}
}

func TestCommitExposure_SuppressedOnResolvedStatuses(t *testing.T) {
	// paid    — the cash arrived; that is the whole point.
	// voided  — the operator already took the one recovery action there is.
	// draft   — grants are funded at finalize; a draft cannot hold one.
	for _, status := range []InvoiceStatus{InvoicePaid, InvoiceVoided, InvoiceDraft} {
		t.Run(string(status), func(t *testing.T) {
			inv := commitInv()
			inv.Status = status
			if got := ClassifyInvoiceAttention(inv, liveGrant(10000000, 4380000)); got != nil {
				t.Fatalf("status %s must suppress exposure, got %+v", status, got)
			}
		})
	}
}

func TestCommitExposure_ExpiredGrantReportsNothing(t *testing.T) {
	// Expiry is applied by the READER (it mirrors drainPositiveBlocks' liveness
	// predicate), so an expired-but-unswept grant reaches the classifier as
	// drawable=0. With nothing ever spent there is no exposure to report — and
	// critically, no banner claiming spendable credit the drain would refuse.
	inv := commitInv()
	inv.PaymentStatus = PaymentFailed
	got := ClassifyInvoiceAttention(inv, liveGrant(0, 0))
	if got == nil {
		t.Fatal("expected the underlying payment_failed banner")
	}
	if strings.Contains(got.Message, "available for the customer to spend") {
		t.Fatalf("expired grant claimed spendable credit: %q", got.Message)
	}
}

// TestRecoveryInFlight_NarrowByDesign pins the bad-debt-recovery banner and,
// more importantly, its SILENCE.
//
// The banner exists so a second operator cannot charge a card that is already
// being charged. But it must fire ONLY while a recovery is in flight — a
// written-off invoice sitting at `failed` is every dunning-exhausted write-off
// in the system, and lighting those up would put a permanent banner on the
// most common terminal state there is.
func TestRecoveryInFlight_NarrowByDesign(t *testing.T) {
	cases := []struct {
		name    string
		status  InvoiceStatus
		payment InvoicePaymentStatus
		want    bool
	}{
		{"written off + recovery in flight", InvoiceUncollectible, PaymentProcessing, true},
		{"written off + failed (every dunning exhaustion)", InvoiceUncollectible, PaymentFailed, false},
		{"written off + pending", InvoiceUncollectible, PaymentPending, false},
		{"written off + parked", InvoiceUncollectible, PaymentUnknown, false},
		{"voided + processing", InvoiceVoided, PaymentProcessing, false},
		{"paid + processing", InvoicePaid, PaymentProcessing, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inv := draft()
			inv.Status = tc.status
			inv.PaymentStatus = tc.payment
			got := ClassifyInvoiceAttention(inv, AttentionContext{})
			fired := got != nil && got.Code == "payment.recovery_processing"
			if fired != tc.want {
				t.Fatalf("recovery banner fired=%v, want %v (attention=%+v)", fired, tc.want, got)
			}
		})
	}
}

// TestRecoveryWarnsOnOfflinePayment pins the offline-recording warning. Its
// charge-blocking twin (RecoveryBlocksCharge) was removed by ADR-113; recording
// money that already arrived is never refused, only warned about. Refuse to
// create a bad money event; never refuse to record one that already happened.
func TestRecoveryWarnsOnOfflinePayment(t *testing.T) {
	writtenOff := func() Invoice {
		return Invoice{Status: InvoiceUncollectible, BillingReason: BillingReasonSubscriptionCycle}
	}
	taxReversed := func() Invoice {
		i := writtenOff()
		i.TaxProvider, i.TaxAmountCents = "stripe_tax", 725
		i.TaxReversedAt = &time.Time{}
		return i
	}

	t.Run("tax reversed warns", func(t *testing.T) {
		w := RecoveryWarnsOnOfflinePayment(taxReversed(), 0)
		if w == nil || w.Code != "tax_reversed_unrecoverable" {
			t.Fatalf("got %+v, want a tax_reversed_unrecoverable warning", w)
		}
	})

	t.Run("threshold warns", func(t *testing.T) {
		i := writtenOff()
		i.BillingReason = BillingReasonThreshold
		w := RecoveryWarnsOnOfflinePayment(i, 0)
		if w == nil || w.Code != "recovery_superseded" {
			t.Fatalf("got %+v, want a recovery_superseded warning", w)
		}
	})

	t.Run("ordinary written-off invoice says nothing", func(t *testing.T) {
		if w := RecoveryWarnsOnOfflinePayment(writtenOff(), 0); w != nil {
			t.Fatalf("warned on a clean recovery: %+v — a warning on every offline payment is a warning nobody reads", w)
		}
	})

	t.Run("a FINALIZED invoice never warns", func(t *testing.T) {
		i := taxReversed()
		i.Status = InvoiceFinalized
		if w := RecoveryWarnsOnOfflinePayment(i, 0); w != nil {
			t.Fatalf("warned on an ordinary invoice: %+v", w)
		}
	})

	t.Run("tax-free written-off invoice says nothing", func(t *testing.T) {
		// Only a REVERSED provider transaction is unreconciled. An invoice with
		// no tax, or a manual-provider one, has no provider ledger to disagree
		// with — warning there would train the operator to ignore this.
		i := writtenOff()
		i.TaxProvider, i.TaxAmountCents = "stripe_tax", 0
		if w := RecoveryWarnsOnOfflinePayment(i, 0); w != nil {
			t.Fatalf("warned with zero tax: %+v", w)
		}
		i.TaxProvider, i.TaxAmountCents = "manual", 725
		if w := RecoveryWarnsOnOfflinePayment(i, 0); w != nil {
			t.Fatalf("warned on a manual-provider invoice: %+v — the tenant files that tax themselves", w)
		}
	})

	t.Run("unapplied credit warns — the arm that was UNSATISFIABLE until 2026-08-05", func(t *testing.T) {
		// The predicate behind this sum used to test `issue_pending AND
		// status='voided'`, which no row can satisfy: the status transition
		// clears issue_pending in the same statement. The gate built on it
		// could never fire, and it was written from a domain comment claiming
		// the flag is "NEVER cleared" — itself false. Corrected to
		// `voided AND issued_at IS NULL`.
		w := RecoveryWarnsOnOfflinePayment(writtenOff(), 2500)
		if w == nil || w.Code != "relief_not_reissued" {
			t.Fatalf("got %+v, want a relief_not_reissued warning", w)
		}
		if RecoveryWarnsOnOfflinePayment(writtenOff(), 0) != nil {
			t.Error("warned with zero unapplied relief — the negative control")
		}
	})

}
