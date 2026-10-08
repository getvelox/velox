package billing

import (
	"context"
	"log/slog"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// Labels for the no-payment-method email, read back by the invoice email
// timeline ("Sent automatically — …"). The collector is the same either way;
// the label says what woke it.
const (
	noPMTriggerFinalize = "finalize_no_pm"
	noPMTriggerSweep    = "auto_charge_retry_no_pm"
)

// CollectInvoice collects one just-finalized invoice now, in the caller's
// request or tick, instead of waiting for the next auto-charge sweep.
//
// It is a nudge, not a second collector. The finalize write already queued
// the invoice (auto_charge_pending is set in the same statement that makes it
// finalized), and this runs the sweep's own processAutoCharge on that one row
// under the same per-invoice lease. So a crash before or during this call
// loses nothing: the sweep finds the flag on its next tick and finishes the
// job. Errors are logged, never returned, for the same reason.
//
// at anchors the credit application and a credit-covered settle. Cycle close
// and threshold fires pass their own period instant (the credits that were
// live at the boundary are the ones that apply, and paid_at lands there); a
// nil at uses the invoice's clock now, which is right for request-time
// finalizes.
//
// A draft (tax pending, collection paused) is not collected: the claim
// requires status=finalized, so the call is a no-op until it finalizes.
//
// Collection is not abortable by the caller's cancellation. Request-time
// callers would otherwise abort the Stripe call at its most ambiguous moment
// on a client disconnect, along with the charger's 'unknown' outcome write.
// The charge itself is still bounded by processAutoCharge's 30s deadline.
func (e *Engine) CollectInvoice(ctx context.Context, tenantID, invoiceID string, at *time.Time) {
	if e.charger == nil || e.paymentSetups == nil {
		return
	}
	ctx = context.WithoutCancel(ctx)
	inv, err := e.invoices.GetInvoice(ctx, tenantID, invoiceID)
	if err != nil {
		slog.WarnContext(ctx, "collect: invoice reload failed; the auto-charge sweep will collect it",
			"invoice_id", invoiceID, "error", err)
		return
	}
	if inv.Status != domain.InvoiceFinalized {
		return
	}
	if _, errs := e.processAutoCharge(ctx, []domain.Invoice{inv}, at, noPMTriggerFinalize); len(errs) > 0 {
		for _, cerr := range errs {
			slog.WarnContext(ctx, "collect: did not complete; the auto-charge sweep will retry",
				"invoice_id", invoiceID, "error", cerr)
		}
	}
}

// collectionAt is the instant processAutoCharge applies credits and settles
// at: the caller's anchor when it has one, else the invoice's clock now.
func (e *Engine) collectionAt(ctx context.Context, inv domain.Invoice, at *time.Time) time.Time {
	if at != nil {
		return *at
	}
	sim, err := e.SimForInvoice(ctx, inv.TenantID, inv.ID)
	if err != nil {
		return e.clock.Now(ctx) // ADR-030: injected clock, never bare wall-clock
	}
	return sim.At
}
