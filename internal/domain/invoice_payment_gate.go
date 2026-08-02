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
	Blocked bool
	// Code is machine-readable, for API consumers and the dashboard.
	Code string
	// Message is operator-facing and must always answer BOTH halves: why this
	// is refused, and what to do instead. A refusal that names no alternative
	// is how an operator ends up stuck with an invoice and no next step.
	Message string
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
			Message: parkedMessage(action),
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
func parkedMessage(action InvoiceAction) string {
	// Bounded since ADR-108: the search sweep can adopt a found PaymentIntent,
	// so "will not resolve on its own" is conditional now — but every refusal
	// below still holds while the invoice IS parked, and the write-off remains
	// the only operator exit.
	const why = "this invoice's charge attempt could not be identified with the payment provider, so we cannot tell whether the customer was charged, and unless the attempt can be found by Velox's provider search it will not resolve on its own"
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
