package domain

// InvoiceAction names an operator action whose availability depends on the
// invoice's PAYMENT state.
type InvoiceAction string

const (
	ActionVoid                 InvoiceAction = "void"
	ActionMarkUncollectible    InvoiceAction = "mark_uncollectible"
	ActionRecordOfflinePayment InvoiceAction = "record_offline_payment"
	ActionIssueCreditNote      InvoiceAction = "issue_credit_note"
	ActionCollectPayment       InvoiceAction = "collect_payment"
	ActionResendSetupLink      InvoiceAction = "resend_setup_link"
	ActionEmailInvoice         InvoiceAction = "email_invoice"
)

// PaymentBlock is the answer to "does this invoice's payment state block this
// action, and what should the operator be told?"
type PaymentBlock struct {
	Blocked bool `json:"blocked"`
	// Code is machine-readable, for API consumers and the dashboard.
	Code string `json:"code,omitempty"`
	// Message is operator-facing and must always answer BOTH halves: why this
	// is refused, and what to do instead. A refusal that names no alternative
	// is how an operator ends up stuck with an invoice and no next step.
	Message string `json:"message,omitempty"`
}

// PaymentBlocksAction is the SINGLE SOURCE for whether an invoice's payment
// state blocks an operator action.
//
// It exists because this one rule used to be re-expressed by hand in eight
// independent places — five service guards, two HTTP handlers and the
// dashboard's own menu conditions — each with its own wording.
//
// CORRECTION (2026-07-31): when that sentence was first written only the seven
// GO sites had been collapsed. The dashboard was named as if it derived from
// here and did not: it went on hand-writing `payment_status !== 'succeeded' &&
// !== 'processing'` at four call sites, every one omitting 'unknown', so a
// parked invoice rendered a green "Collect Payment" button directly beneath a
// Critical banner saying no further charge would be attempted. The scanner
// below could not catch it because it walked only .go files — the mechanisation
// was as good as its SCOPE, and its scope excluded the surface the comment was
// boasting about. The dashboard now derives from a single frontend helper
// (web-v2/src/lib/status.ts paymentIsUnresolved) and the scanner walks .ts/.tsx.
// The durable version is the server returning per-action availability so the
// dashboard derives rather than mirrors; one mirror beats four copies. When ADR-107
// changed what payment_status='unknown' MEANS (from a transient state that
// resolves in seconds to one that may never resolve), all eight became wrong
// independently, and finding them took a four-agent sweep plus a manual walk of
// the UI. Two of them still disagreed with each other afterwards: void said
// "wait for it to settle or cancel it", credit-note said "settle or cancel
// first, or wait for charge reconciliation" — the same rule, two authors, two
// sets of impossible advice.
//
// The fix is not better wording. It is having one statement of the rule that
// every surface derives from, so the next change to what a payment state means
// cannot leave seven stale copies behind.
//
// Three cases:
//
//   - NOT in flight — nothing here blocks; the caller's own status rules
//     (paid/voided/draft) still apply and are deliberately NOT folded in, since
//     those genuinely differ per action.
//   - PARKED (ADR-107: 'unknown' with no PaymentIntent id) — the attempt could
//     not be identified with the provider and never will be. Mark-uncollectible
//     is ALLOWED here and is the only exit; everything else is refused and told
//     to use it.
//   - IN FLIGHT with a known PaymentIntent — genuinely resolving, so waiting is
//     the correct advice.
//
// EXTENDING THIS IF ADR-106's LEDGER IS EVER RESUMED. That design adds a
// charge_intents row whose state answers the same question more precisely, and
// this function is where it plugs in — not into the seven call sites:
//
//	intent open        → an attempt is in flight and recovery is actively
//	                     retrying it, so this is the WAIT case, even though the
//	                     invoice has no PaymentIntent id yet. Today that shape
//	                     reads as parked; the ledger is exactly what lets it be
//	                     told apart.
//	intent needs_review → recovery gave up, so it is the PARKED case: refuse
//	                     everything but mark-uncollectible and say so.
//
// That is one branch here, and every surface — five service guards, two HTTP
// handlers, the attention banner, the dashboard menu — inherits it. The reason
// to say this out loud is that two adversarial review rounds found the ledger's
// needs_review state had NO operator exit and no honest copy, and the cause was
// structural: expressing a give-up state used to mean writing eight strings and
// wiring a menu, so it got written zero times. Extend the function; do not
// re-scatter the rule.
//
// The gate needs ONE more fact, and where that fact enters is the real decision.
// Two ways in, and they are not equally priced:
//
//   - ON THE READ MODEL (preferred): the invoice store LEFT JOINs the unresolved
//     charge_intents row and populates one more field, exactly as the invoice
//     already carries stripe_payment_intent_id, payment_status and
//     charge_attempt_seq. This signature does not change, the seven callers do
//     not change, and no domain boundary is crossed. Costs one join on the
//     invoice read path, hitting at most one row (the partial unique index on
//     unresolved intents already exists).
//   - VIA THE SIGNATURE: pass the intent state as a parameter. The edit here is
//     trivial, but every caller holds only an Invoice, so each must LOAD the
//     intent — and internal/invoice reaching into internal/payment is the
//     cross-domain coupling internal/arch/boundaries_test.go fails the build
//     over. It threads a new dependency through five services and two handlers
//     to answer a question they already believe they can answer.
//
// Neither is pre-built here. Speculative parameters for a parked design are how
// the ledger reached 1,900 lines in the first place.
func PaymentBlocksAction(inv Invoice, action InvoiceAction) PaymentBlock {
	if !inv.PaymentStatus.IsInFlight() {
		return PaymentBlock{}
	}

	// Parked: the outcome is unknowable, not merely unknown-yet.
	if inv.PaymentStatus == PaymentUnknown && inv.StripePaymentIntentID == "" {
		if action == ActionMarkUncollectible {
			// The deliberate carve-out. Writing it off moves no money, and if
			// the charge did succeed the provider webhook still settles the
			// invoice as paid through the ordinary recovery path. Without this
			// the invoice can reach no terminal state at all.
			return PaymentBlock{}
		}
		return PaymentBlock{
			Blocked: true,
			Code:    "payment_unidentifiable",
			Message: parkedMessage(action, inv.Status == InvoiceUncollectible),
		}
	}

	return PaymentBlock{
		Blocked: true,
		Code:    "payment_in_flight",
		Message: inFlightMessage(action),
	}
}

// parkedMessage always ends by naming mark-uncollectible, because it is the one
// action that works on a parked invoice and an operator who is not told that
// has no way out.
// alreadyWrittenOff swaps the tail advice. Every arm below steers the operator
// to "mark the invoice uncollectible" as the exit — which is impossible advice
// on an invoice that already IS uncollectible, and that is a reachable state:
// writing off a parked invoice is the carve-out immediately above, and since
// 2026-08-05 such a row stays visible to the ADR-108 search and can be the
// subject of a bad-debt recovery attempt.
func parkedMessage(action InvoiceAction, alreadyWrittenOff bool) string {
	// Bounded since ADR-108: the search sweep can adopt a found PaymentIntent,
	// so "will not resolve on its own" is conditional now — but every refusal
	// below still holds while the invoice IS parked, and the write-off remains
	// the only operator exit.
	const why = "this invoice's charge attempt could not be identified with the payment provider, so we cannot tell whether the customer was charged, and unless the attempt can be found by Velox's provider search it will not resolve on its own"
	if alreadyWrittenOff {
		// It is already written off, so there is no status left to advise. What
		// is still true is the reason: nobody knows whether that card was
		// charged, and only the provider can answer it.
		return why + " — it is already written off, so nothing further will be attempted automatically. Resolve the attempt in Stripe; if money was taken it will settle here when the provider reports it"
	}
	switch action {
	case ActionVoid:
		return why + " — voiding it could annul an invoice that was in fact paid. Check the attempt in Stripe; if nothing was charged, mark the invoice uncollectible instead"
	case ActionRecordOfflinePayment:
		return why + " — recording an offline payment would label a card charge as out-of-band. Check the attempt in Stripe; if nothing was charged, mark the invoice uncollectible instead"
	case ActionIssueCreditNote:
		return why + " — crediting it could refund money that was never collected. Check the attempt in Stripe; if nothing was charged, mark the invoice uncollectible instead"
	case ActionCollectPayment:
		return why + " — charging again risks a second charge. Check the attempt in Stripe; if nothing was charged, mark the invoice uncollectible instead"
	case ActionResendSetupLink:
		return why + " — that email tells the customer we will collect automatically, which will not happen. Resolve the attempt in Stripe first, or mark the invoice uncollectible"
	case ActionEmailInvoice:
		// Split from resend-setup-link after a walk read this refusal back:
		// the invoice email's call to action is "View & pay invoice", not a
		// promise to collect, so the shared sentence described the wrong
		// email. What makes sending it wrong is that it asks for money we may
		// already have taken, and lands the customer on a page with no Pay
		// button.
		return why + " — that email asks the customer to pay an invoice we may already have charged them for, and the hosted page will not offer them a Pay button when they arrive. Check the attempt in Stripe; if nothing was charged, mark the invoice uncollectible instead"
	default:
		return why + " — resolve the attempt in Stripe, or mark the invoice uncollectible"
	}
}

// inFlightMessage covers the genuinely-resolving case, where waiting IS the
// right advice — deliberately different from the parked case, which is the
// distinction that kept being lost.
func inFlightMessage(action InvoiceAction) string {
	const why = "a charge on this invoice is still being confirmed by the payment provider"
	switch action {
	case ActionVoid:
		return why + " — wait for it to report an outcome before voiding, so a paid invoice is not annulled"
	case ActionMarkUncollectible:
		return why + " — wait for it to report an outcome before writing this off"
	case ActionRecordOfflinePayment:
		return why + " — wait for it to report an outcome before recording an offline payment"
	case ActionIssueCreditNote:
		return why + " — wait for it to report an outcome before crediting, so money that was never collected is not refunded"
	case ActionCollectPayment:
		return why + " — retry once it resolves"
	case ActionResendSetupLink, ActionEmailInvoice:
		return why + " — wait for it to report an outcome before asking the customer to pay again"
	default:
		return why + " — wait for it to report an outcome"
	}
}

// RecoveryBlocksCharge is the SINGLE SOURCE for whether a WRITTEN-OFF invoice
// can be charged — bad-debt recovery, when the customer comes back.
//
// It exists for the same reason PaymentBlocksAction does, and was extracted the
// moment the rule needed a second reader. The three refusals below started life
// as inline `if`s in the collect handler; the dashboard then needed them too,
// to disable its "Charge customer" button with a reason instead of letting an
// operator confirm a charge that answers 409. Two hand-written copies of a
// money rule is exactly the drift this file's older sibling was written to end.
//
// These are ALSO enforced in ClaimChargeForManualCollect's SQL, and that is not
// redundancy: a Go-side read is a TOCTOU against the very sweeps that create
// these states (the tax-reversal sweep especially). The CAS is what makes the
// refusal true; this is what makes it EXPLICABLE — to a handler answering with
// a typed code, and to a UI deciding whether to offer the action at all.
//
// unreliefedClawbackCents is passed in rather than read here because it is a
// cross-domain sum (credit notes) and this package holds no stores. Callers
// that cannot compute it pass 0 — which is the permissive direction, so the CAS
// stays the backstop rather than this being the only guard.
func RecoveryBlocksCharge(inv Invoice, unreliefedClawbackCents int64) PaymentBlock {
	if inv.Status != InvoiceUncollectible {
		return PaymentBlock{} // not a recovery — ordinary rules apply
	}
	// Keys on tax_reversed_at — the reversal HAVING HAPPENED — not on
	// tax_transaction_id, which is set on every taxed invoice and never
	// cleared. Under ADR-111 a write-off no longer reverses, so this fires
	// only on invoices reversed BEFORE that change, or reversed by a future
	// operator-initiated bad-debt-relief claim. Keyed on the transaction id it
	// would refuse every taxed invoice forever and make recovery structurally
	// unavailable to tax-registered tenants — the defect this replaces.
	if inv.TaxReversedAt != nil {
		return PaymentBlock{
			Blocked: true,
			Code:    "tax_reversed_unrecoverable",
			Message: "This invoice's tax was reversed with the tax provider when it was written off, and it cannot be re-reported automatically. Charging now would collect tax that has already been reported as not collected. Record the payment as an offline payment instead, and correct the tax with your provider.",
		}
	}
	if inv.BillingReason == BillingReasonThreshold {
		return PaymentBlock{
			Blocked: true,
			Code:    "recovery_superseded",
			Message: "This invoice was billed on a usage threshold, and writing it off did not stop that usage being re-billed on a later invoice. Charging it now would bill the same usage twice — collect the newer invoice instead.",
		}
	}
	if unreliefedClawbackCents > 0 {
		return PaymentBlock{
			Blocked: true,
			Code:    "relief_not_reissued",
			Message: "This invoice is owed a credit that was never applied, so the amount shown is higher than what the customer owes. Re-issue the credit before charging, or the customer will be over-collected.",
		}
	}
	return PaymentBlock{}
}

// RecoveryWarnsOnOfflinePayment is the OFFLINE twin of RecoveryBlocksCharge,
// and the difference between them is the whole rule for this area:
//
//	refuse to CREATE a bad money event; never refuse to RECORD one that
//	already happened.
//
// A card charge is Velox choosing to move money, so those conditions BLOCK. An
// offline payment means the wire already landed — refusing to record it would
// only make the books wrong too, on top of a payment that exists either way.
//
// But "do not refuse" is not "say nothing". Both conditions below mean the
// recorded payment does not reconcile with what the tenant has told the tax
// authority, or with what was already billed elsewhere — and nothing
// downstream will ever notice, because MarkPaid flips status to 'paid', which
// drops the invoice out of the tax-reversal sweep's own predicate
// (status IN ('voided','uncollectible')). The discrepancy becomes permanently
// invisible at the instant it is created. So the operator has to be told
// BEFORE they act, on the invoice, not in a log line an engineer reads later.
//
// Returns nil when there is nothing to say — the overwhelming majority.
func RecoveryWarnsOnOfflinePayment(inv Invoice, unreliefedClawbackCents int64) *PaymentBlock {
	if inv.Status != InvoiceUncollectible {
		return nil
	}
	// Tax was reversed at write-off and cannot be re-reported automatically.
	// Keyed on the reversal having HAPPENED (stamp set, or a stripe_tax invoice
	// carrying tax whose committed transaction is gone), not on the invoice
	// merely having tax.
	// Same key as the block above: the reversal having happened. The old
	// `|| TaxTransactionID == ""` arm fired on invoices whose tax was never
	// COMMITTED (a commit orphan) — a different defect with a different fix,
	// and warning about a reversal that never happened is a false statement.
	if inv.TaxProvider == "stripe_tax" && inv.TaxAmountCents > 0 && inv.TaxReversedAt != nil {
		return &PaymentBlock{
			Code:    "tax_reversed_unrecoverable",
			Message: "This invoice's tax was reversed with the tax provider when it was written off. Recording this payment does not re-report it, so the tax collected here will not appear in your provider's records — correct it with your provider.",
		}
	}
	if inv.BillingReason == BillingReasonThreshold {
		return &PaymentBlock{
			Code:    "recovery_superseded",
			Message: "This invoice was billed on a usage threshold, and writing it off did not stop that usage being re-billed on a later invoice. Check that this payment is not settling usage the customer has already been billed for.",
		}
	}
	if unreliefedClawbackCents > 0 {
		return &PaymentBlock{
			Code: "relief_not_reissued",
			// Phrased for the moment it is READ. By the time an operator is
			// recording a wire the customer has already sent whatever they were
			// invoiced — so the over-collection has happened and cannot be
			// prevented here. What IS still actionable is the debt it creates in
			// the other direction, which is why the sentence ends on issuing the
			// credit rather than on stopping the payment.
			Message: "This invoice is owed a credit that was never applied, so the amount shown is higher than what the customer actually owed. If they paid this amount they have overpaid — issue the credit so it is returned or offset against their next invoice.",
		}
	}
	return nil
}
