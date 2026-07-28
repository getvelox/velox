package billing

import (
	"context"
	"log/slog"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// collectAfterFinalize is the ONE post-finalize collection pipeline, shared by
// every engine finalize path: cycle close (billOnePeriod), day-1
// subscription_create (FinalizeOnCreateInvoice), the immediate-cancel final
// invoice, and a finalized threshold fire. Before the 2026-07-11 extraction the
// pipeline was hand-copied at those four sites and had already drifted (missing
// success/decline logs, and the error-path bugs fixed below); the #434
// typed-notify-outcome change had to be hand-edited four times. Copy-drift on
// this shape is the design review's redesign #4 finding — this method is the
// dissolution.
//
// Contract: best-effort, never returns an error. Every failure downgrades to
// auto_charge_pending=true so RetryPendingCharges re-drives collection on its
// next tick (re-applying credits atomically and re-resolving the PM). The
// caller owns its GATES — credit-apply + creditApplyOK suppression,
// pause-collection, amount-due>0, and status splits are genuinely divergent
// per-site policies (deliberate, ADR-cited — see
// docs/dev/design-review-2026-07-10.md D1-D5), NOT part of the shared pipeline.
// Callers must not pass a draft invoice: the charger refuses non-finalized
// invoices and the no-PM email must not quote non-final totals (the threshold
// path's tax-deferred drafts take its queue-only arm instead).
//
// Three error-path behaviors are deliberately unified to the conservative arm
// (pre-extraction each was a silent per-site drift):
//   - A payment-setup RESOLVE error is not "no payment method": queue for the
//     sweep but do NOT email — pre-extraction all four sites emailed
//     "payment method needed" to card-having customers on a transient read
//     error (design-census D10).
//   - A pre-charge invoice reload error queues for the sweep — pre-extraction
//     it skipped silently (no charge, no flag, no log) and the invoice never
//     entered the retry path (design-census D7).
//   - A pre-notify invoice reload error is logged — pre-extraction it silently
//     dropped the setup-link email (design-census D9).
//
// The charge is synchronous with a 30s timeout. Charge idempotency lives in
// the charger (per-attempt Stripe key); decline-path dunning starts inline in
// the charger, so the decline arm here only queues the retry flag. logTag
// prefixes every log line with the calling site's identity.
func (e *Engine) collectAfterFinalize(ctx context.Context, sub domain.Subscription, inv domain.Invoice, logTag string) {
	// The no-drafts contract above, enforced rather than asserted. A
	// tax-pending cycle invoice is born DRAFT
	// (domain.InvoiceFinalizationStatus) and billOnePeriod's collect arm
	// gates on amount + pause, not status — so drafts reached here and took
	// the no-PM arm, emailing the customer a setup link quoting a PRE-TAX
	// amount_due, then stamping no_pm_notified_at so the corrected figure
	// was never sent. The charge arm fared no better: the charger refuses a
	// non-finalized invoice, so the attempt logged as a failure.
	//
	// Returning early is safe because collection is now owned at the moment
	// of finalize: the tax-retry chain queues the invoice for the sweep when
	// it auto-finalizes (invoice.Service.RetryTax), and the sweep emails the
	// final total. A draft that never resolves its tax is not owed by anyone
	// yet — the operator sees it on the tax banner, which is the actionable
	// surface for that state.
	if inv.Status != domain.InvoiceFinalized {
		slog.WarnContext(ctx, logTag+": refusing to collect a non-finalized invoice; collection resumes when it finalizes",
			"invoice_id", inv.ID,
			"status", string(inv.Status),
		)
		return
	}
	// Collection is not abortable by the caller's cancellation. Two of this
	// pipeline's callers (subscription_create day-1, final-on-cancel) arrive
	// on HTTP request ctxs — a client disconnect mid-charge would otherwise
	// abort the Stripe call at its most ambiguous moment and kill the
	// charger's 'unknown' outcome-persist plus the retry-flag write in the
	// same stroke, leaving no record that a charge was ever attempted. For
	// background callers (cycle close, threshold) this is a no-op, except
	// during shutdown — where finishing an in-flight collect (bounded by the
	// 30s charge deadline below) beats aborting a money operation midway.
	ctx = context.WithoutCancel(ctx)
	stripeCusID, stripePMID, psErr := e.paymentSetups.ResolveForCharge(ctx, sub.TenantID, sub.CustomerID)
	if psErr != nil {
		slog.WarnContext(ctx, logTag+": payment-setup resolve failed; queuing for scheduler retry",
			"invoice_id", inv.ID,
			"customer_id", sub.CustomerID,
			"error", psErr,
		)
		e.queueForChargeRetry(ctx, sub.TenantID, inv.ID)
		return
	}

	if stripePMID != "" && stripeCusID != "" {
		// PM ready → synchronous charge. Reload first: the caller's inv can
		// carry a stale pre-credit amount_due (billOnePeriod applies credits
		// after create), and the charge must see post-credit truth.
		chargeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		chargeInv, err := e.invoices.GetInvoice(chargeCtx, sub.TenantID, inv.ID)
		if err != nil {
			slog.WarnContext(ctx, logTag+": pre-charge invoice reload failed; queuing for scheduler retry",
				"invoice_id", inv.ID,
				"error", err,
			)
			e.queueForChargeRetry(ctx, sub.TenantID, inv.ID)
			return
		}
		if chargeInv.AmountDueCents <= 0 {
			return // credits covered it since the caller's snapshot; nothing to collect
		}
		if _, err := e.charger.ChargeInvoice(chargeCtx, sub.TenantID, chargeInv, stripeCusID, stripePMID); err != nil {
			slog.WarnContext(ctx, logTag+": auto-charge failed, marking for retry",
				"invoice_id", inv.ID,
				"error", err,
			)
			e.queueForChargeRetry(ctx, sub.TenantID, inv.ID)
			return
		}
		slog.InfoContext(ctx, logTag+": auto-charge succeeded", "invoice_id", inv.ID)
		return
	}

	// No PM on file: queue for the sweep (charges the moment a card is
	// attached — Chargebee's "Collect Invoice on Card Update") AND send the
	// setup-link email so the customer learns about the gap now, not when the
	// invoice ages into overdue.
	slog.InfoContext(ctx, logTag+": no payment method at finalize, queuing for scheduler retry",
		"invoice_id", inv.ID,
		"customer_id", sub.CustomerID,
	)
	e.queueForChargeRetry(ctx, sub.TenantID, inv.ID)

	// Reload so the notifier sees the just-finalized state (invoice number,
	// totals).
	notifyInv, err := e.invoices.GetInvoice(ctx, sub.TenantID, inv.ID)
	if err != nil {
		slog.WarnContext(ctx, logTag+": no-PM notify reload failed; setup-link email not sent",
			"invoice_id", inv.ID,
			"error", err,
		)
		return
	}
	outcome, err := e.noPMNotifier.NotifyNoPaymentMethod(ctx, sub.TenantID, notifyInv, "finalize_no_pm")
	switch {
	case err != nil:
		slog.WarnContext(ctx, logTag+": no-payment-method notification failed",
			"invoice_id", inv.ID,
			"error", err,
		)
	case outcome == domain.NotifySkippedNoEmail:
		// No stamp: if the customer adds an email later, the sweep's
		// send-once check should still let the notification go out.
		slog.InfoContext(ctx, logTag+": setup-link email skipped: customer has no email on file",
			"invoice_id", inv.ID)
	default:
		// Stamp the send-once marker so the auto-charge sweep (which visits
		// this still-unpaid invoice every tick) doesn't send a duplicate.
		// Best-effort: a failed stamp risks one extra email, never a lost one.
		if serr := e.invoices.SetNoPMNotifiedAt(ctx, sub.TenantID, inv.ID, e.clock.Now(ctx)); serr != nil {
			slog.WarnContext(ctx, logTag+": failed to stamp no-PM notified marker", "invoice_id", inv.ID, "error", serr)
		}
		slog.InfoContext(ctx, logTag+": setup-link email queued", "invoice_id", inv.ID)
	}
}

// applyCreditsAndCollect is the finalize-tail for sites whose invoice should
// consume the customer's credit balance before any card charge (ADR-088:
// day-1 subscription_create and the final-on-cancel invoice — industry
// parity is unanimous that first/final invoices are not excluded from
// automatic application; cycle close and threshold keep their own in-place
// blocks, whose $0 arm is entangled with cycle control flow, ADR-087 trap
// R2). Sequence mirrors the retry sweep's proven shape: apply → reload →
// settle-if-zero → collect.
//
// Trap R1 (the 2026-05-30 overcharge class) is honored by construction: an
// apply FAILURE queues for the sweep and returns — never a pre-credit card
// charge — and the sweep re-applies credits atomically before charging, so
// recovery pre-exists. Credits apply only to a FINALIZED invoice: a
// tax-pending day-1 draft waits for the tax-retry chain, whose sweep
// collection re-applies credits itself. The e.credits nil-guard matches the
// sweep's existing guard (narrow unit fixtures); production boot requires
// the collaborator (#442).
func (e *Engine) applyCreditsAndCollect(ctx context.Context, sub domain.Subscription, inv domain.Invoice, logTag string) {
	ctx = context.WithoutCancel(ctx) // same contract as collectAfterFinalize; also shields the apply/settle writes
	if e.credits != nil && inv.AmountDueCents > 0 && inv.Status == domain.InvoiceFinalized {
		sim, nowErr := e.SimForInvoice(ctx, sub.TenantID, inv.ID)
		at := sim.At
		if nowErr != nil {
			at = e.clock.Now(ctx) // ADR-030: injected clock, never bare wall-clock
		}
		if _, err := e.credits.ApplyToInvoiceAt(ctx, sub.TenantID, sub.CustomerID, inv.ID, inv.AmountDueCents, at, inv.InvoiceNumber); err != nil {
			slog.WarnContext(ctx, logTag+": credit apply failed — queuing for scheduler retry; never charging pre-credit",
				"invoice_id", inv.ID, "error", err)
			e.queueForChargeRetry(ctx, sub.TenantID, inv.ID)
			return
		}
		refreshed, err := e.invoices.GetInvoice(ctx, sub.TenantID, inv.ID)
		if err != nil {
			slog.WarnContext(ctx, logTag+": post-credit invoice reload failed; queuing for scheduler retry",
				"invoice_id", inv.ID, "error", err)
			e.queueForChargeRetry(ctx, sub.TenantID, inv.ID)
			return
		}
		inv = refreshed
		if inv.AmountDueCents <= 0 {
			// Fully covered by balance: settle without a card charge (Stripe
			// parity: no payment is attempted). Finalized-only by the gate
			// above, so the DEMO-000906 draft guard holds by construction.
			if _, err := e.invoices.MarkPaid(ctx, sub.TenantID, inv.ID, "", at); err != nil {
				// Mirrors the cycle site's failure surface: loud warn; the
				// credits are durably applied, an operator retry settles it.
				slog.WarnContext(ctx, logTag+": fully-credited invoice could not be marked paid",
					"invoice_id", inv.ID, "error", err)
				return
			}
			e.resolveDunningRecovered(ctx, sub.TenantID, inv.ID)
			slog.InfoContext(ctx, logTag+": invoice fully covered by credits, marked as paid", "invoice_id", inv.ID)
			return
		}
	}
	if inv.AmountDueCents > 0 {
		e.collectAfterFinalize(ctx, sub, inv, logTag)
	}
}

// queueForChargeRetry sets auto_charge_pending=true so RetryPendingCharges
// picks the invoice up on its next tick. Best-effort: a failed set(true) is a
// liveness sink — the invoice stays invisible to the sweep forever (playbook
// class G) — so it is loudly logged, but the caller's flow never fails on it.
func (e *Engine) queueForChargeRetry(ctx context.Context, tenantID, invoiceID string) {
	if err := e.invoices.SetAutoChargePending(ctx, tenantID, invoiceID, true); err != nil {
		slog.WarnContext(ctx, "failed to queue invoice for charge retry",
			"invoice_id", invoiceID,
			"error", err,
		)
	}
}
