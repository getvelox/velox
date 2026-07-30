package payment

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	auth "github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/payment/breaker"
)

const (
	// chargeIntentCoolOff is how long an intent may sit open before recovery
	// touches it. Longer than the charge context (30s) plus its retries, so the
	// inline path always wins the common case and this stays a backstop.
	chargeIntentCoolOff = 5 * time.Minute

	// chargeIntentReplayTTL is the most dangerous number in ADR-106. Stripe
	// documents idempotency keys as retained for "at least 24 hours" — after
	// which replaying a key does NOT return the original PaymentIntent, it
	// creates a second one. That is precisely the bug being fixed, so recovery
	// refuses to replay past a deliberate 2x safety margin and quarantines the
	// row instead. Being stuck-and-loud beats being silently double-charged.
	chargeIntentReplayTTL = 12 * time.Hour

	// chargeIntentMaxRecoveryAttempts bounds automatic recovery. Past this the
	// row goes to needs_review — still blocking new charges, which is the point.
	chargeIntentMaxRecoveryAttempts = 5
)

// ChargeIntentLedger is the narrow seam the recovery sweep needs. Declared here
// (consumer-side) so tests can substitute a fake without a database.
type ChargeIntentLedger interface {
	ListOpen(ctx context.Context, olderThan time.Time, limit int) ([]domain.ChargeIntent, error)
	BumpRecoveryAttempt(ctx context.Context, tenantID, id string) (bool, error)
	Resolve(ctx context.Context, tenantID, id, paymentIntentID string) error
	MarkNeedsReview(ctx context.Context, tenantID, id, reason string) error
	// ReleaseRecoveryAttempt un-counts an attempt that provably never reached
	// the provider, so an outage cannot exhaust the budget.
	ReleaseRecoveryAttempt(ctx context.Context, tenantID, id string) error
	HasUnresolved(ctx context.Context, tenantID, invoiceID string) (bool, error)
}

// SetChargeIntents wires the ledger for RecoverChargeIntents and for the
// give-up gate in reconcileOne. Nil disables recovery — and with it the gate,
// which is correct: without the ledger there is no intent to defer to.
func (r *Reconciler) SetChargeIntents(l ChargeIntentLedger) { r.chargeIntents = l }

// RecoverChargeIntents resolves charge attempts whose outcome was lost — the
// crash between the Stripe call and the outcome write, and the ambiguous error
// that carried no PaymentIntent id.
//
// Recovery is IDEMPOTENT REPLAY, not a search. The intent row stores the exact
// Idempotency-Key header and the exact request parameters, so this re-issues
// the identical request. Stripe saves the status code and body of the first
// request per key and returns that same result to subsequent requests, so the
// replay is handed the ORIGINAL PaymentIntent. If Stripe saved nothing (the
// request never began execution), the replay creates the one PaymentIntent we
// always intended — which step 3 has just re-proven is still owed. Both
// outcomes are safe; there is no third.
//
// Signature matches Run so it drops straight into the scheduler's reconciler
// driver with no adapter.
func (r *Reconciler) RecoverChargeIntents(ctx context.Context, limit int) (int, []error) {
	if r.chargeIntents == nil || r.client == nil {
		return 0, nil
	}
	cutoff := time.Now().UTC().Add(-chargeIntentCoolOff)
	intents, err := r.chargeIntents.ListOpen(ctx, cutoff, limit)
	if err != nil {
		return 0, []error{fmt.Errorf("list open charge intents: %w", err)}
	}

	var (
		resolved int
		errs     []error
	)
	for _, ci := range intents {
		changed, err := r.recoverOne(ctx, ci)
		if err != nil {
			errs = append(errs, fmt.Errorf("charge intent %s (invoice %s): %w", ci.ID, ci.InvoiceID, err))
			continue
		}
		if changed {
			resolved++
		}
	}
	return resolved, errs
}

func (r *Reconciler) recoverOne(ctx context.Context, ci domain.ChargeIntent) (bool, error) {
	ctx = auth.WithTenantID(ctx, ci.TenantID)

	// ADR-030 contracted instant. Recovery runs on the wall plane, but the
	// attempt it is completing was CONTRACTED at a simulated instant on a
	// clock-pinned invoice — so the settle must stamp that instant, not the
	// sweep's now. The column was being written and never read, while the
	// migration, the domain type and the tests all asserted this anchoring
	// happened (found by adversarial review, 2026-07-30): the code did not do
	// what three comments said it did.
	if ci.SimEffectiveAt != nil {
		ctx = withSettleAnchor(ctx, *ci.SimEffectiveAt)
	}

	// (0) Count the attempt BEFORE doing anything that can crash, so a
	// crash-loop still advances toward needs_review instead of cycling forever.
	// Losing the CAS means another path already closed the row.
	// A quarantined row is not re-attempted, so it must not be counted either —
	// and counting it would also refuse the row (the CAS is state='open').
	if ci.State == domain.ChargeIntentOpen {
		won, err := r.chargeIntents.BumpRecoveryAttempt(ctx, ci.TenantID, ci.ID)
		if err != nil {
			return false, fmt.Errorf("bump recovery attempt: %w", err)
		}
		if !won {
			return false, nil
		}
	}

	// (1) Adoption. Something else may have learned the outcome meanwhile — a
	// webhook, or an operator re-sending the event from the Stripe dashboard.
	// Close the intent against that PaymentIntent and make no API call.
	inv, err := r.invoices.Get(ctx, ci.TenantID, ci.InvoiceID)
	if err != nil {
		return false, fmt.Errorf("load invoice: %w", err)
	}
	if inv.StripePaymentIntentID != "" {
		if err := r.chargeIntents.Resolve(ctx, ci.TenantID, ci.ID, inv.StripePaymentIntentID); err != nil {
			return false, fmt.Errorf("adopt existing payment intent: %w", err)
		}
		slog.InfoContext(ctx, "charge intent closed by adoption — another path already named the PaymentIntent",
			"invoice_id", ci.InvoiceID, "charge_intent_id", ci.ID,
			"stripe_payment_intent_id", inv.StripePaymentIntentID)
		return true, nil
	}

	// (1b) A QUARANTINED row gets the adoption check above and nothing else. It
	// is listed solely so that exit exists — replaying it is precisely what must
	// never happen — so stop here rather than spending a Stripe call.
	if ci.State == domain.ChargeIntentNeedsReview {
		return false, nil
	}

	// (2) Replay TTL. Past Stripe's retention floor a replay would MINT a second
	// PaymentIntent rather than return the first — the exact bug. Quarantine.
	if time.Since(ci.OccurredAt) >= chargeIntentReplayTTL {
		return r.quarantine(ctx, ci, fmt.Sprintf(
			"idempotency key is older than the %s replay window; replaying it could create a second PaymentIntent", chargeIntentReplayTTL))
	}

	// (3) Attempt budget.
	if ci.RecoveryAttempts >= chargeIntentMaxRecoveryAttempts {
		return r.quarantine(ctx, ci, fmt.Sprintf("automatic recovery gave up after %d attempts: %s", ci.RecoveryAttempts, ci.LastError))
	}

	// (4) Params must still match. If the invoice moved under the attempt (a
	// credit landed, it was paid offline, it was voided), a replay that finds
	// nothing saved at Stripe would create a PaymentIntent for the wrong money.
	if inv.Status != domain.InvoiceFinalized || inv.AmountDueCents != ci.AmountCents {
		return r.quarantine(ctx, ci, fmt.Sprintf(
			"invoice moved under the attempt (status=%s amount_due=%d, intent was for %d) — replay is no longer safe",
			inv.Status, inv.AmountDueCents, ci.AmountCents))
	}

	// (5) REPLAY. Byte-identical to the original request by construction: same
	// key, same params, and Confirm/OffSession are client-side constants.
	params := PaymentIntentParams{
		AmountCents:     ci.AmountCents,
		Currency:        ci.Currency,
		CustomerID:      ci.StripeCustomerID,
		PaymentMethodID: ci.StripePaymentMethodID,
		Description:     ci.Description,
		IdempotencyKey:  ci.IdempotencyKey,
		Metadata:        ci.Metadata,
	}
	piID, err := r.replay(ctx, params)
	if err != nil {
		var pe *PaymentError
		if errors.As(err, &pe) && pe.PaymentIntentID != "" {
			// A cached decline still NAMES the PaymentIntent, which is all we
			// need — the settle below reads its real status from Stripe.
			piID = pe.PaymentIntentID
		} else if errors.Is(err, breaker.ErrOpen) {
			// The breaker collapses BEFORE the call runs, so no attempt was made
			// — refund the budget. Without this, a Stripe outage burns every
			// intent's five attempts in ~25 minutes and quarantines the whole
			// queue for outages that touched nothing (found by adversarial
			// review, 2026-07-30).
			if rerr := r.chargeIntents.ReleaseRecoveryAttempt(ctx, ci.TenantID, ci.ID); rerr != nil {
				slog.WarnContext(ctx, "could not refund a recovery attempt after a breaker skip",
					"charge_intent_id", ci.ID, "error", rerr)
			}
			return false, nil
		} else {
			return false, fmt.Errorf("replay charge: %w", err)
		}
	}
	if piID == "" {
		return false, fmt.Errorf("replay returned no payment intent id")
	}

	// (6) Stamp the discovered id through the EXISTING outcome writer, so
	// charge_attempt_seq stays coupled to PI stamping (ADR-105) and no fourth
	// UPDATE-invoices statement appears. 'processing' is IsInFlight, so every
	// in-flight mutation guard still applies while the settle resolves.
	//
	// RE-READ FIRST. The replay is a network round-trip, and a webhook can settle
	// the invoice inside it. UpdatePayment writes paid_at unconditionally, so
	// stamping 'processing' with paidAt=nil over a just-paid invoice would NULL
	// its paid_at and walk a settled invoice backwards into in-flight (found by
	// adversarial review, 2026-07-30). If it settled, adopt and stop — the
	// outcome we went looking for has arrived by another route.
	fresh, ferr := r.invoices.Get(ctx, ci.TenantID, ci.InvoiceID)
	if ferr != nil {
		return false, fmt.Errorf("re-read invoice before stamping recovered intent: %w", ferr)
	}
	if !fresh.PaymentStatus.IsInFlight() && fresh.PaymentStatus != domain.PaymentPending {
		if err := r.chargeIntents.Resolve(ctx, ci.TenantID, ci.ID, piID); err != nil {
			return false, fmt.Errorf("close charge intent settled under recovery: %w", err)
		}
		slog.InfoContext(ctx, "charge intent closed — the invoice settled while its recovery replay was in flight",
			"invoice_id", ci.InvoiceID, "charge_intent_id", ci.ID,
			"payment_status", string(fresh.PaymentStatus), "stripe_payment_intent_id", piID)
		return true, nil
	}
	if _, err := r.invoices.UpdatePayment(ctx, ci.TenantID, ci.InvoiceID,
		domain.PaymentProcessing, piID, "", nil); err != nil {
		return false, fmt.Errorf("stamp recovered payment intent: %w", err)
	}

	// (7) Close the intent — the attempt now has a name, so the quarantine has
	// done its job.
	if err := r.chargeIntents.Resolve(ctx, ci.TenantID, ci.ID, piID); err != nil {
		return false, fmt.Errorf("close recovered charge intent: %w", err)
	}
	slog.WarnContext(ctx, "recovered a charge attempt whose outcome was lost — replayed its idempotency key and Stripe returned the original PaymentIntent",
		"invoice_id", ci.InvoiceID, "charge_intent_id", ci.ID,
		"stripe_payment_intent_id", piID, "recovery_attempts", ci.RecoveryAttempts+1,
		"idempotency_key", ci.IdempotencyKey)

	// (8) Settle in the same tick via the existing path. The replayed body is
	// the FIRST response and its status is frozen, so it is never trusted —
	// reconcileOne re-reads the live PaymentIntent and routes the terminal
	// outcome. If this crashes, the stale-'processing' sweep is the backstop.
	inv.StripePaymentIntentID = piID
	inv.PaymentStatus = domain.PaymentProcessing
	if _, err := r.reconcileOne(ctx, inv); err != nil {
		slog.WarnContext(ctx, "recovered payment intent stamped, but the immediate settle failed — the processing sweep will retry",
			"invoice_id", ci.InvoiceID, "stripe_payment_intent_id", piID, "error", err)
	}
	return true, nil
}

// replay re-issues the stored request. Breaker-wrapped like every other call.
func (r *Reconciler) replay(ctx context.Context, params PaymentIntentParams) (string, error) {
	if r.breaker != nil {
		out, err := r.breaker.Execute(ctx, func(ctx context.Context) (any, error) {
			return r.client.CreatePaymentIntent(ctx, params)
		})
		if err != nil {
			return "", err
		}
		if res, ok := out.(PaymentIntentResult); ok {
			return res.ID, nil
		}
		return "", fmt.Errorf("replay returned an unexpected result type")
	}
	res, err := r.client.CreatePaymentIntent(ctx, params)
	if err != nil {
		return "", err
	}
	return res.ID, nil
}

// quarantine stops automatic recovery while KEEPING the invoice blocked. The
// row still satisfies the one-unresolved index, so no second charge can be
// opened — which is correct precisely because we do not know whether the first
// one is live.
func (r *Reconciler) quarantine(ctx context.Context, ci domain.ChargeIntent, reason string) (bool, error) {
	if err := r.chargeIntents.MarkNeedsReview(ctx, ci.TenantID, ci.ID, reason); err != nil {
		return false, fmt.Errorf("quarantine charge intent: %w", err)
	}
	slog.ErrorContext(ctx, "CRITICAL: a charge attempt cannot be resolved automatically — a PaymentIntent may be live at Stripe under this key. Look it up in the Stripe dashboard (searchable by idempotency key) before charging this invoice again; the invoice is blocked from further charges until then",
		"invoice_id", ci.InvoiceID, "tenant_id", ci.TenantID, "charge_intent_id", ci.ID,
		"idempotency_key", ci.IdempotencyKey, "amount_cents", ci.AmountCents,
		"occurred_at", ci.OccurredAt, "reason", reason)
	return false, nil
}
