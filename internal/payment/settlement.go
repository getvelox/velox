package payment

import (
	"context"
	"database/sql"
	"errors"

	"fmt"
	"log/slog"
	"time"

	"github.com/stripe/stripe-go/v82"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
)

// SettlementSource tags which entry point discovered a payment's terminal
// status, for logs/metrics. The settlement side-effects are identical
// regardless of source — that is the whole point of the primitive (ADR-049):
// a dropped-webhook recovery is byte-identical to the webhook it replaces.
type SettlementSource string

const (
	SourceWebhook        SettlementSource = "webhook"         // inbound payment_intent.* event
	SourceReconciler     SettlementSource = "reconciler"      // GetPaymentIntent backstop sweep (Phase 2)
	SourceChargeResponse SettlementSource = "charge_response" // synchronous Confirm:true create response (Phase 3)
	SourceManual         SettlementSource = "manual"          // operator on-demand "Check provider" (Phase 4)
)

// SettleSucceeded transitions an invoice to PAID and fires the complete
// success side-effect set: bind sim-time so paid_at lands on a clock-pinned
// invoice's timeline, MarkPaid (zero amount_due, record PI + paid_at), stamp
// the charged card for the activity timeline, fire payment.succeeded, and
// enqueue the receipt email.
//
// Idempotent and safe to call from any entry point (ADR-049 discover-then-
// settle): MarkPaid is a no-op on an already-paid invoice, and the card-stamp /
// event / receipt steps are best-effort (log-only) so a duplicate call — e.g.
// the webhook arriving after the reconciler already settled — does not fail.
//
// This is the consolidated implementation of what handlePaymentSucceeded did
// inline (ADR-049 Phase 1); the webhook handler now resolves the invoice and
// delegates here.
// capturedCents is the amount Stripe actually captured (0 = unknown/legacy
// caller): the settle compares it against the transitioned row's
// amount_paid and escalates payment.amount_mismatch on drift — a Checkout
// session can legally pay an amount a credit note has since changed
// (ADR-068); silent wrong books would corrupt the refund cap.
//
// The fast-path duplicate check compares against the CALLER's inv snapshot —
// the webhook and reconciler both pass freshly-read rows (documented
// contract); the post-transition check below uses the row RETURNED by the
// FOR-UPDATE transition and needs no such trust.
func (s *Stripe) SettleSucceeded(ctx context.Context, tenantID string, inv domain.Invoice, paymentIntentID string, capturedCents int64, source SettlementSource) error {
	// Idempotency guard, symmetric with SettleFailed's out-of-order guard:
	// skip if the invoice is already settled paid. The webhook path resolves a
	// `processing` invoice (guard passes), but a non-webhook source — the
	// reconciler recovering a success the webhook already delivered — would
	// otherwise re-fire the receipt email + payment.succeeded event. MarkPaid
	// itself is a no-op on a paid invoice; this guard additionally suppresses
	// the duplicate side-effects.
	if inv.Status == domain.InvoicePaid || inv.PaymentStatus == domain.PaymentSucceeded {
		if paymentIntentID != "" && inv.StripePaymentIntentID != paymentIntentID {
			// A SECOND, different PaymentIntent succeeded against an
			// already-paid invoice (two devices; a stale-but-live Checkout
			// session): money was captured twice and exists only in Stripe.
			// Escalate loudly — the operator IS the refund mechanism
			// (auto-refund deferred, ADR-068). An empty recorded PI counts:
			// the invoice settled via credits/offline and a card charge
			// still landed.
			s.escalatePaymentAnomaly(ctx, tenantID, inv, domain.EventPaymentDuplicateCharge, paymentIntentID, capturedCents,
				"second successful charge on an already-paid invoice")
			return nil
		}
		slog.Info("payment already settled; skipping duplicate success settlement",
			"invoice_id", inv.ID,
			"payment_intent_id", paymentIntentID,
			"source", source,
		)
		return nil
	}

	// Bind effective-now from the invoice so paid_at lands in simulated time
	// on clock-pinned invoices. Stripe's webhook fires in wall-clock 2026 even
	// when the invoice belongs to a clock frozen at 2024-04 — without binding,
	// paid_at would leak wall-clock and the dashboard would show "Paid on
	// 2026-05-08" for a simulation-2024 invoice.
	ctx = s.bindForInvoice(ctx, tenantID, inv.ID)
	now := clock.Now(ctx)
	// The charge's CONTRACTED instant beats the bind: the pin resolves the
	// clock's CURRENT frozen_time — during (or after) a wide advance that's
	// the advance TARGET, not the retry/boundary the charge fired at, so a
	// sim-Mar-7 dunning recovery stamped "paid Apr 1". The inline settle
	// hands the anchor over on ctx; the webhook path recovers it from the
	// PI's velox_anchor_at metadata. Wall charges carry no anchor.
	if a, ok := settleAnchorFrom(ctx); ok {
		now = a
	}

	// Single atomic operation: mark paid, zero amount_due, record PI + paid_at,
	// AND enqueue invoice.paid + payment.succeeded in the SAME tx (the card path).
	// transitioned reports whether THIS call did the finalized→paid move.
	// The line-47 guard is a fast path that catches the SERIAL redelivery
	// (re-read sees paid); a truly CONCURRENT redelivery of the same charge
	// slips past it because both readers saw `processing`. The
	// SELECT … FOR UPDATE serializes those two, and exactly one gets
	// transitioned=true — the once-only gate for both the in-tx events and the
	// post-commit best-effort side-effects below (receipt email, card stamp).
	// Detach from the caller's cancellation BEFORE the settle tx, not after
	// it. Since ADR-040's amendment the receipt rides that tx, so the unit
	// that must survive a client disconnect now STARTS at the transition — a
	// webhook client hanging up mid-settle would otherwise abort a
	// transaction that carries a customer-visible effect. WithoutCancel keeps
	// the ctx VALUES (tenant binding, simulated clock, livemode) and drops
	// only the cancel signal; every call below is individually bounded.
	ctx = context.WithoutCancel(ctx)
	// Resolve the recipient BEFORE the settle transaction: the hook below runs
	// inside it, and a customer lookup there would hold the invoice row lock
	// across another query. A duplicate settle (transition lost) pays for one
	// wasted read — the trade for never holding the money lock on a lookup.
	receiptTx := s.buildReceiptHook(ctx, tenantID, inv, capturedCents)
	fresh, transitioned, err := s.invoices.MarkPaidCardSettlementTransition(ctx, tenantID, inv.ID, paymentIntentID, now, receiptTx)
	if err != nil {
		if errors.Is(err, errs.ErrInvalidState) {
			// Non-payable target (voided; the void's session-expire leg
			// failed and the customer paid inside the residual). Retrying the
			// webhook forever is wrong — the transition will never succeed.
			// Escalate the per-cause event (the money is owed BACK, distinct
			// from duplicate_charge) and absorb.
			s.escalatePaymentAnomaly(ctx, tenantID, inv, domain.EventPaymentReceivedOnVoidedInvoice, paymentIntentID, capturedCents,
				"payment succeeded against a non-payable invoice")
			return nil
		}
		return fmt.Errorf("mark invoice paid: %w", err)
	}
	if !transitioned {
		// Compare against the row the FOR-UPDATE transition RETURNED — the
		// post-race truth — never the caller's stale snapshot (a checkout
		// invoice records its PI only at settle, so the stale comparison
		// would false-alarm on every routine concurrent same-PI redelivery).
		if paymentIntentID != "" && fresh.StripePaymentIntentID != paymentIntentID {
			s.escalatePaymentAnomaly(ctx, tenantID, fresh, domain.EventPaymentDuplicateCharge, paymentIntentID, capturedCents,
				"second successful charge lost the settle race to a different PaymentIntent")
			return nil
		}
		slog.Info("payment already settled by a concurrent settler; skipping duplicate side-effects",
			"invoice_id", inv.ID, "payment_intent_id", paymentIntentID, "source", source)
		return nil
	}
	// The settle tx committed. Detach the post-commit side-effect block from
	// the caller's cancellation: this ctx is usually a webhook REQUEST ctx,
	// and a client disconnect / server drain mid-block would kill the
	// remaining enqueues even though the payment is already booked — no

	// DURABILITY TIERING. The consistency-critical events — invoice.paid AND
	// payment.succeeded — are BOTH enqueued INSIDE
	// MarkPaidCardSettlementTransition's tx (invoice/postgres.go), so they are
	// crash-safe and exactly-once with the paid-flip (transactional outbox,
	// ADR-040).
	//
	// Everything below is post-commit + best-effort BY DESIGN, ordered by
	// what a process death costs, cheapest-to-lose LAST:
	//   1. amount truth-check + receipt enqueue: fast DB writes with NO
	//      reconciler behind them — a dropped receipt enqueue is gone for
	//      good (the dispatcher's retry + DLQ only own delivery AFTER the
	//      enqueue lands). They run FIRST, before any network call. This
	//      block was historically last, below two Stripe calls, while its
	//      comment claimed a "sub-ms" crash window — the window was
	//      seconds-wide and grew every time a PR inserted another call
	//      above it (2026-07-05 reassessment).
	//   2. dunning resolve: idempotent AND backstopped — the dunning
	//      sweep's paid-pre-check floor re-resolves it when the run's
	//      next retry comes due (the sweep only processes due runs).
	//   3. checkout-session expire + card stamp: Stripe NETWORK calls with
	//      their own backstops (session ExpiresAt <= 1h; card stamp is a
	//      cosmetic timeline sub-line).
	//
	// RULE for future additions: this block is APPEND-ONLY AT THE END —
	// inserting a call above the receipt enqueue widens the unrecoverable
	// window again. A new step may only go earlier if it is a fast DB write
	// whose loss is MORE expensive than a receipt.
	//
	// Receipt email is deliberately NOT in-tx: strict atomicity is the wrong
	// contract for email, and folding it in would drag customer-email
	// resolution + the suppression-list read under the invoice row lock.
	// DEFERRED UPGRADE (if a design partner needs guaranteed receipts): a
	// receipt-pending marker + reconciler re-fire — tracked in
	// docs/adr/README.md "Open follow-ups".

	// Amount truth-check (ADR-068): the captured amount must equal what the
	// transition booked (amount_paid = amount_due at settle). A drifted
	// Checkout session (credit note changed the due inside the session's
	// window) settles the invoice but books the WRONG figure — detect it,
	// never silently absorb it.
	if capturedCents > 0 && capturedCents != fresh.AmountPaidCents {
		s.escalatePaymentAnomaly(ctx, tenantID, fresh, domain.EventPaymentAmountMismatch, paymentIntentID, capturedCents,
			"captured amount differs from the amount booked at settle")
	}

	// Enqueue payment receipt email. s.emailReceipt is *OutboxSender
	// (ADR-040), so SendPaymentReceipt is a fast DB INSERT and the
	// dispatcher's retry loop owns delivery + backoff. Failure here logs but
	// does not fail the caller — the payment already committed, and returning
	// an error would make the webhook re-fire the whole event (re-MarkPaid +
	// double-firing the customer-facing event).

	slog.Info("payment succeeded",
		"invoice_id", inv.ID,
		"payment_intent_id", paymentIntentID,
		"source", source,
	)

	// Resolve any active dunning run for this now-paid invoice — symmetric with the
	// engine's background-settle DunningResolver (#317). A card success should clear
	// the "in dunning" state promptly instead of waiting for the dunning sweep's
	// paid-pre-check floor to catch it when the run next comes due (which can be
	// days out). Best-effort + nil-tolerant
	// (narrow tests) + idempotent (no-op when there is no active run); on failure the
	// floor still resolves it, so log and continue. Runs in the invoice-bound ctx
	// so it stamps simulated time on clock-pinned invoices.
	//
	// Exactly-once: dunning's resolveRunNow CASes the resolve, so this and
	// processRun's own resolve on a synchronous retry-success emit ONE
	// dunning.resolved. processRun persists the attempt count BEFORE the charge, so a
	// resolver firing synchronously here re-reads the FULL attempt_count (not one low).
	if s.dunningResolver != nil {
		if err := s.dunningResolver.ResolveByInvoice(ctx, tenantID, inv.ID, domain.ResolutionPaymentRecovered); err != nil {
			slog.Warn("payment succeeded: resolve dunning run failed; the run stays visible in dunning until its next scheduled retry, when the sweep's paid check will resolve it",
				"invoice_id", inv.ID, "error", err)
		}
	}

	// Best-effort Stripe-side expire of any still-live checkout sessions for
	// this invoice (ADR-068). The DB rows were already closed IN the settle
	// tx (choke-point close in markPaidReportingTransition) so the reuse
	// path cannot serve them; this network sweep shrinks the window in which
	// a second device could pay a session that is dead in our books but live
	// at Stripe. Gated on transitioned==true (once-only); each session's own
	// ExpiresAt (<=1h) is the backstop when a call fails.
	s.expireCheckoutSessionsBestEffort(ctx, tenantID, fresh)

	// Stamp the card actually charged onto the invoice so the activity
	// timeline can show "Invoice paid · via Visa •••• 4242" (ADR-020).
	// Best-effort — a missing CardFetcher, a non-card PM, or a transient
	// Stripe API error all fall through to "Invoice paid · $29.00" with no
	// sub-line. Lookup goes directly through Stripe (not our paymentmethods
	// table) so one-off Checkout cards the customer never saved still show.
	if s.cardFetcher != nil && paymentIntentID != "" {
		card, cardErr := s.cardFetcher.FetchCardForPaymentIntent(ctx, paymentIntentID)
		if cardErr != nil {
			slog.Warn("payment succeeded: card resolve failed (timeline sub-line will be empty)",
				"invoice_id", inv.ID, "payment_intent_id", paymentIntentID, "error", cardErr)
		} else if card.Brand != "" || card.Last4 != "" {
			if err := s.invoices.SetPaymentCard(ctx, tenantID, inv.ID, card.Brand, card.Last4); err != nil {
				slog.Warn("payment succeeded: persist card details failed",
					"invoice_id", inv.ID, "error", err)
			}
		}
	}

	// payment.succeeded is enqueued IN-TX by MarkPaidCardSettlementTransition
	// (above), so it commits atomically with the paid-flip. Do NOT also fire it
	// here — that would double-fire it (one in-tx, one post-commit).

	// The attempt's outcome is resolved INSIDE the settle tx by
	// MarkPaidCardSettlementTransition (ADR-103) — atomic with the paid
	// flip, so no post-commit write can leave the timeline's payment
	// owner disagreeing with the invoice.

	return nil
}

// PaymentFailure is one report that a PaymentIntent failed, as the three
// sources that learn it hand it to SettleFailed: the charge call's own
// decline (SourceChargeResponse), the payment_intent.payment_failed webhook,
// and the reconciler.
type PaymentFailure struct {
	PaymentIntentID string
	Message         string
	// Purpose is the PaymentIntent's velox_purpose metadata. A dunning
	// retry skips the generic customer email (dunning sends its own per
	// attempt); a hosted Checkout attempt never moves the invoice.
	Purpose string
	// ChargedAtSeq is set only by the charge call reporting its own attempt:
	// the charge_attempt_seq it charged with (see
	// domain.PaymentFailureReport).
	ChargedAtSeq *int64
}

// failureEmailSuppressed reports whether a failure of a PaymentIntent with
// this purpose skips the generic "payment failed" email. A dunning-retry
// attempt already sent its own warning or escalation (Stripe Smart Retries /
// Lago shape: one email per attempt). One helper, so the charge response,
// the webhook and the reconciler cannot drift apart on who gets emailed.
func failureEmailSuppressed(purpose string) bool {
	return purpose == piPurposeDunningRetry
}

// SettleFailed records a payment failure and fires its side effects once per
// PaymentIntent, whichever source reports it first: the charge call's own
// decline, the webhook, or the reconciler. It is the failed-path twin of
// SettleSucceeded (ADR-049).
//
// The store decides whether the report moves the invoice: only a report about
// the attempt the invoice is waiting on does (see
// MarkPaymentFailedReportingTransition). A late decline for an older attempt,
// or a decline on the hosted Checkout page, lands on its own attempt row and
// nothing else.
//
// When it does move the invoice, the first report for the PaymentIntent
// enqueues payment.failed and the customer email in the same transaction as
// the failed stamp. The email is skipped for a dunning retry (dunning sends
// its own) and for an invoice that is no longer finalized (voided or written
// off during the charge). Dunning is then started — on every report that
// records the failure, not only the first: the first reporter's start can
// fail after its stamp committed, and StartDunning is idempotent by invoice
// (0085 UNIQUE), so a later report is a free re-drive. Test-clock invoices
// have no other one: the dunning_backfill reconciler skips simulated
// invoices.
func (s *Stripe) SettleFailed(ctx context.Context, tenantID string, inv domain.Invoice, f PaymentFailure, source SettlementSource) error {
	_, _, err := s.settleFailed(ctx, tenantID, inv, f, source)
	return err
}

// settleFailed is SettleFailed returning the invoice as it now stands and what
// the report did, for the charge call, which logs a refused report and
// releases its lease from them.
func (s *Stripe) settleFailed(ctx context.Context, tenantID string, inv domain.Invoice, f PaymentFailure, source SettlementSource) (domain.Invoice, domain.PaymentFailureResult, error) {
	// Ignore an out-of-order failure for an already-settled invoice. Webhooks
	// arrive at-least-once and without ordering guarantees, so a stale
	// payment_failed can land AFTER the invoice was marked paid. The store
	// re-checks this under its row lock; this early return only saves the
	// recipient lookup.
	if inv.Status == domain.InvoicePaid || inv.PaymentStatus == domain.PaymentSucceeded {
		slog.Info("ignoring out-of-order payment failure for already-settled invoice",
			"invoice_id", inv.ID,
			"invoice_status", inv.Status,
			"payment_status", inv.PaymentStatus,
			"payment_intent_id", f.PaymentIntentID,
			"source", source,
		)
		return inv, domain.PaymentFailureResult{}, nil
	}

	// Bind effective-now so the failed stamp and StartDunning land in
	// simulated time on clock-pinned invoices. Inside an Advance the ctx
	// already carries the clock; binding refines it, never erases it.
	ctx = s.bindForInvoice(ctx, tenantID, inv.ID)
	// Detached before the tx: the stamp, payment.failed and the email commit
	// together, and a webhook disconnect must not cut the dunning start that
	// follows them. Values (the simulated clock) survive WithoutCancel.
	ctx = context.WithoutCancel(ctx)

	if f.Message == "" {
		f.Message = "payment failed"
	}
	report := domain.PaymentFailureReport{
		PaymentIntentID: f.PaymentIntentID,
		Message:         f.Message,
		ChargedAtSeq:    f.ChargedAtSeq,
		External:        f.Purpose == PurposeHostedInvoicePay,
	}
	// Recipient resolved before the tx, the notice enqueued inside it, gated
	// on the first notice by the store — the same shape as the paid twin.
	var failedTx func(tx *sql.Tx, fresh domain.Invoice) error
	if !report.External && !failureEmailSuppressed(f.Purpose) {
		failedTx = s.buildPaymentFailedHook(ctx, tenantID, inv, f.Message)
	}
	fresh, res, err := s.invoices.MarkPaymentFailedReportingTransition(ctx, tenantID, inv.ID, report, failedTx)
	if err != nil {
		return domain.Invoice{}, domain.PaymentFailureResult{}, fmt.Errorf("update payment status: %w", err)
	}
	if !res.Recorded {
		slog.Info("payment failure recorded on its attempt only — it is not the attempt this invoice is waiting on (an older attempt, a hosted-page attempt, or another outcome was recorded first)",
			"invoice_id", inv.ID,
			"payment_intent_id", f.PaymentIntentID,
			"invoice_payment_intent_id", fresh.StripePaymentIntentID,
			"invoice_payment_status", fresh.PaymentStatus,
			"source", source,
		)
		return fresh, res, nil
	}
	if res.FirstNotice {
		slog.Info("payment failed",
			"invoice_id", inv.ID,
			"payment_intent_id", f.PaymentIntentID,
			"failure_message", f.Message,
			"source", source,
		)
	} else {
		slog.Info("payment failure already recorded for this payment intent; notification skipped",
			"invoice_id", inv.ID, "payment_intent_id", f.PaymentIntentID, "source", source)
	}

	if s.dunning == nil {
		return fresh, res, nil
	}
	// A charge that fails against a voided or WRITTEN-OFF invoice must not open
	// a fresh campaign. A written-off invoice already ran dunning to its final
	// action; re-enrolling would restart escalation emails (and, under a
	// cancel-subscription final action, cancel a subscription) on a debt the
	// business gave up on. Read from the row the store just returned, not the
	// caller's snapshot: the charge call's snapshot predates the Stripe call,
	// and a void or write-off can land during it (neither takes the charge
	// lease). StartDunning has no status opinion of its own.
	if fresh.Status != domain.InvoiceFinalized {
		slog.InfoContext(ctx, "dunning not started for failed charge on a non-finalized invoice — there is nothing to collect",
			"invoice_id", fresh.ID, "status", fresh.Status)
		return fresh, res, nil
	}
	// failureAt is the simulated cycle-close instant — the moment in the
	// invoice's own time domain when this charge "should" have happened — so
	// dunning's next_action_at lands inside the operator's Advance window for
	// clock-pinned invoices. See simulatedFailureAt.
	//
	// Post-commit and best-effort, NOT folded into the fail-tx: that would hold
	// the invoice FOR UPDATE across StartDunning's ~600ms retry sleep plus a
	// cross-domain policy read on every failed charge (dunning-start is a
	// schedule, not a money artifact). A crash here leaves the invoice failed
	// with no run; the next report of this PaymentIntent re-drives it (see
	// above), and for wall-clock invoices the dunning_backfill reconciler
	// (billing.Engine.EnrollFailedWithoutDunning) does too.
	failureAt := simulatedFailureAt(fresh)
	if started, err := startDunningWithRetry(ctx, s.dunning, tenantID, fresh.ID, fresh.CustomerID, failureAt, domain.DunningCausePaymentFailed); err != nil {
		slog.Error("payment failure StartDunning failed after retries — the next report of this payment intent retries it, and for a wall-clock invoice the dunning backfill sweep does on a later scheduler tick",
			"invoice_id", fresh.ID, "customer_id", fresh.CustomerID, "error", err)
	} else if started {
		slog.Info("dunning started for failed payment", "invoice_id", fresh.ID)
	} else {
		slog.Info("dunning skipped for failed payment — policy disabled or not configured", "invoice_id", fresh.ID)
	}
	return fresh, res, nil
}

// escalatePaymentAnomaly is the shared loud channel for money anomalies the
// settle path DETECTS but must not absorb silently (ADR-068): a duplicate
// charge, a captured-amount mismatch, a payment on a voided invoice. It
// slog.Errors (ops), dispatches the per-cause outbound event (integrators),
// and stamps the durable anomaly marker the dashboard attention banner reads
// (operators — the load-bearing surface: with auto-refund deferred, the
// operator IS the refund mechanism). Best-effort by design: detection must
// never fail the settlement that triggered it.
// receiptAmountCents is the figure the payment receipt calls "your
// payment": the amount Stripe actually captured for THIS charge, or the
// invoice's recorded amount_paid when the webhook predates captured-
// amount threading. It was TotalAmountCents — wrong whenever credits or
// partial payments made the charge smaller than the invoice total.
func receiptAmountCents(capturedCents int64, fresh domain.Invoice) int64 {
	if capturedCents > 0 {
		return capturedCents
	}
	if fresh.AmountPaidCents > 0 {
		return fresh.AmountPaidCents
	}
	return fresh.TotalAmountCents
}

func (s *Stripe) escalatePaymentAnomaly(ctx context.Context, tenantID string, inv domain.Invoice, eventType, incomingPI string, capturedCents int64, msg string) {
	slog.ErrorContext(ctx, "payment anomaly: "+msg,
		"event", eventType,
		"invoice_id", inv.ID,
		"tenant_id", tenantID,
		"recorded_payment_intent_id", inv.StripePaymentIntentID,
		"incoming_payment_intent_id", incomingPI,
		"captured_cents", capturedCents,
		"amount_paid_cents", inv.AmountPaidCents,
	)
	if s.events != nil {
		if err := s.events.Dispatch(ctx, tenantID, eventType, map[string]any{
			"invoice_id":                 inv.ID,
			"invoice_number":             inv.InvoiceNumber,
			"customer_id":                inv.CustomerID,
			"recorded_payment_intent_id": inv.StripePaymentIntentID,
			"incoming_payment_intent_id": incomingPI,
			"captured_cents":             capturedCents,
			"amount_paid_cents":          inv.AmountPaidCents,
			"currency":                   inv.Currency,
		}); err != nil {
			slog.ErrorContext(ctx, "payment anomaly: event dispatch failed", "event", eventType, "invoice_id", inv.ID, "error", err)
		}
	}
	if s.anomalies != nil {
		if err := s.anomalies.RecordPaymentAnomaly(ctx, tenantID, inv.ID, eventType, incomingPI, capturedCents); err != nil {
			slog.ErrorContext(ctx, "payment anomaly: durable marker write failed", "event", eventType, "invoice_id", inv.ID, "error", err)
		}
	}
}

// StopCollection is the void path's twin of the settle path's cleanup: an
// invoice that will never be collected must stop being PAYABLE at Stripe.
//
// Order matters. Session expiry comes FIRST because Stripe REFUSES to cancel
// a PaymentIntent it created for a Checkout Session ("You cannot perform this
// action on PaymentIntents created by Checkout. Try expiring the Checkout
// Session instead") — so for every hosted-page attempt, PI cancel alone was
// a no-op that logged a warning while the session stayed live until its
// ExpiresAt (<=1h), leaving a voided invoice payable. Found on the FLOW D2
// walk (VLX-000004): void → PI cancel refused → session still open.
// Expiring the session is what actually closes that window; the PI then
// terminates with it.
//
// The PI cancel still runs afterwards for engine-minted PIs (auto-charge,
// dunning retry) — those are cancelable and have no session. Both legs are
// best-effort: the DB-side claim close (in the void tx) already stops OUR
// reuse path, ExpiresAt bounds the Stripe-side window, and the ADR-068
// payment-received-on-voided-invoice anomaly remains the last backstop.
func (s *Stripe) StopCollection(ctx context.Context, tenantID string, inv domain.Invoice) {
	s.expireCheckoutSessionsBestEffort(ctx, tenantID, inv)
	if inv.StripePaymentIntentID == "" {
		return
	}
	if err := s.client.CancelPaymentIntent(ctx, inv.StripePaymentIntentID); err != nil {
		// Checkout-owned PIs are expected to refuse; the expire above is
		// the real stop for those. Anything else is worth a line.
		slog.InfoContext(ctx, "stop collection: payment intent not canceled (checkout-owned PIs expire with their session)",
			"invoice_id", inv.ID, "payment_intent_id", inv.StripePaymentIntentID, "error", err)
		return
	}
	slog.InfoContext(ctx, "stop collection: payment intent canceled", "invoice_id", inv.ID)
}

// expireCheckoutSessionsBestEffort expires, at Stripe, every session for the
// invoice not yet confirmed terminal — including superseded rows whose
// earlier expire call failed (their sessions stay payable at Stripe). Errors
// classify idempotently: already-expired = success; completed = the webhook
// escalation owns the money consequence; anything else = rely on ExpiresAt.
func (s *Stripe) expireCheckoutSessionsBestEffort(ctx context.Context, tenantID string, inv domain.Invoice) {
	if s.checkoutSessions == nil {
		return
	}
	claims, err := s.checkoutSessions.ListUnresolvedForInvoice(ctx, tenantID, inv.ID)
	if err != nil {
		slog.WarnContext(ctx, "settle: list checkout claims for expire failed", "invoice_id", inv.ID, "error", err)
		return
	}
	if s.sessionClients == nil {
		return
	}
	for _, c := range claims {
		sc := s.sessionClients.For(ctx, tenantID, c.Livemode)
		if sc == nil {
			continue
		}
		if _, err := sc.V1CheckoutSessions.Expire(ctx, c.StripeSessionID, &stripe.CheckoutSessionExpireParams{}); err != nil {
			slog.WarnContext(ctx, "settle: best-effort session expire failed (ExpiresAt backstop applies)",
				"invoice_id", inv.ID, "claim_id", c.ID, "session_id", c.StripeSessionID, "error", err)
			continue
		}
		if err := s.checkoutSessions.MarkExpired(ctx, tenantID, c.ID); err != nil {
			slog.WarnContext(ctx, "settle: mark claim expired failed", "claim_id", c.ID, "error", err)
		}
	}
}

// settleAnchorKey carries the charge's contracted instant from the charge
// site (inline) or the webhook's PI metadata into SettleSucceeded, which
// rebinds ctx internally (bindForInvoice resolves the clock's CURRENT
// frozen_time and would otherwise win). Package-private: the anchor is a
// payment-internal contract between the charge and its settle.
type settleAnchorKeyType struct{}

var settleAnchorKey = settleAnchorKeyType{}

func withSettleAnchor(ctx context.Context, at time.Time) context.Context {
	return context.WithValue(ctx, settleAnchorKey, at.UTC())
}

func settleAnchorFrom(ctx context.Context) (time.Time, bool) {
	t, ok := ctx.Value(settleAnchorKey).(time.Time)
	return t, ok && !t.IsZero()
}

// anchorFromEventPayload extracts data.object.metadata.velox_anchor_at from
// a parsed Stripe webhook event. Returns false on absence or parse failure —
// the settle then falls back to the invoice-pin binding (the pre-anchor
// behavior), never errors: a malformed anchor must not block a settlement.
func anchorFromEventPayload(event domain.StripeWebhookEvent) (time.Time, bool) {
	data, _ := event.Payload["data"].(map[string]any)
	obj, _ := data["object"].(map[string]any)
	meta, _ := obj["metadata"].(map[string]any)
	raw, _ := meta["velox_anchor_at"].(string)
	if raw == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// buildReceiptHook resolves the recipient (plain reads, before the settle
// transaction opens) and returns the hook that enqueues the payment receipt on
// that transaction — so the paid-flip and the receipt commit together
// (ADR-040 amendment; before it a failover between the two lost the receipt
// with the payment standing, and the transition gate suppressed every retry).
// nil when there is nothing to send: no sender wired, or no resolvable
// recipient. The amount comes from the row the transition actually wrote.
func (s *Stripe) buildReceiptHook(ctx context.Context, tenantID string, inv domain.Invoice, capturedCents int64) func(tx *sql.Tx, fresh domain.Invoice) error {
	if s.emailReceipt == nil || s.customerEmail == nil {
		return nil
	}
	email, name, cc, err := s.customerEmail.GetCustomerEmail(ctx, tenantID, inv.CustomerID)
	if err != nil || email == "" {
		slog.Warn("skip payment receipt email — cannot resolve customer email",
			"invoice_id", inv.ID, "customer_id", inv.CustomerID, "error", err)
		return nil
	}
	return func(tx *sql.Tx, fresh domain.Invoice) error {
		return s.emailReceipt.SendPaymentReceiptTx(ctx, tx, tenantID, email, cc, name,
			inv.InvoiceNumber, receiptAmountCents(capturedCents, fresh), inv.Currency, inv.PublicToken)
	}
}

// buildPaymentFailedHook is the decline-notice counterpart of
// buildReceiptHook. SettleFailed skips it for a dunning retry
// (failureEmailSuppressed) and for a hosted attempt — dunning still runs;
// only this email is skipped.
func (s *Stripe) buildPaymentFailedHook(ctx context.Context, tenantID string, inv domain.Invoice, failureMsg string) func(tx *sql.Tx, fresh domain.Invoice) error {
	if s.emailPaymentFailed == nil {
		return nil
	}
	if s.customerEmail == nil {
		slog.Error("payment failed email — customer email resolver not wired", "invoice_id", inv.ID)
		return nil
	}
	email, name, cc, err := s.customerEmail.GetCustomerEmail(ctx, tenantID, inv.CustomerID)
	if err != nil || email == "" {
		slog.Warn("skip payment failed email — cannot resolve customer email",
			"invoice_id", inv.ID, "customer_id", inv.CustomerID, "error", err)
		return nil
	}
	return func(tx *sql.Tx, fresh domain.Invoice) error {
		// Asking a customer to pay an invoice that was voided or written off
		// during the charge is wrong; the row under the settle lock decides.
		if fresh.Status != domain.InvoiceFinalized {
			return nil
		}
		return s.emailPaymentFailed.SendPaymentFailedTx(ctx, tx, tenantID, email, cc, name,
			inv.InvoiceNumber, failureMsg, inv.PublicToken)
	}
}
