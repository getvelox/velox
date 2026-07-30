package payment

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

// ErrChargeAttemptInFlight is returned when an EARLIER charge attempt on this
// invoice is still unresolved — a PaymentIntent may be live at Stripe under a
// key we cannot reuse (different amount, different card, different seq).
// Starting a second charge here is the double-charge this ledger exists to
// prevent, so the caller must wait for the recovery sweep to name the first
// attempt. errs.InvalidState-kinded so handlers map it to an honest 409.
var ErrChargeAttemptInFlight = errs.InvalidState("an earlier payment attempt for this invoice is still being resolved").WithCode("payment_in_progress")

const chargeIntentCols = `id, tenant_id, livemode, invoice_id, idempotency_key, amount_cents, currency,
	stripe_customer_id, stripe_payment_method_id, description, metadata, purpose,
	state, stripe_payment_intent_id, recovery_attempts, last_error,
	occurred_at, sim_effective_at, resolved_at, created_at, updated_at`

// ChargeIntentStore persists the pre-call record of a charge attempt (ADR-106).
// It lives in internal/payment rather than internal/invoice deliberately: the
// row is a payment-protocol artifact, not invoice data, and keeping it here
// leaves invoice.Store — and its four test fakes — untouched.
type ChargeIntentStore struct {
	db *postgres.DB
}

func NewChargeIntentStore(db *postgres.DB) *ChargeIntentStore {
	return &ChargeIntentStore{db: db}
}

func scanChargeIntent(row interface{ Scan(...any) error }) (domain.ChargeIntent, error) {
	var ci domain.ChargeIntent
	var metaJSON []byte
	err := row.Scan(&ci.ID, &ci.TenantID, &ci.Livemode, &ci.InvoiceID, &ci.IdempotencyKey,
		&ci.AmountCents, &ci.Currency, &ci.StripeCustomerID, &ci.StripePaymentMethodID,
		&ci.Description, &metaJSON, &ci.Purpose, &ci.State, &ci.StripePaymentIntentID,
		&ci.RecoveryAttempts, &ci.LastError, &ci.OccurredAt, &ci.SimEffectiveAt,
		&ci.ResolvedAt, &ci.CreatedAt, &ci.UpdatedAt)
	if err != nil {
		return domain.ChargeIntent{}, err
	}
	if len(metaJSON) > 0 {
		_ = json.Unmarshal(metaJSON, &ci.Metadata)
	}
	return ci, nil
}

// Open records the attempt BEFORE Stripe is called and returns the row that
// owns this invoice's charge slot.
//
// Three outcomes, and the caller must branch on which:
//
//   - the returned row's IdempotencyKey EQUALS the caller's — this is our slot
//     (a fresh insert, or the same attempt re-driven after a crash at the same
//     seq). Proceed: sending the same key again is exactly what makes Stripe
//     return the original PaymentIntent instead of a second one.
//   - the returned row's key DIFFERS — an earlier attempt is unresolved under a
//     key we cannot reuse. The caller must NOT call Stripe.
//   - an error — the caller must NOT call Stripe. Fail-closed is the entire
//     guarantee: a charge that is not recorded first must not happen at all.
//
// The payability re-check under FOR SHARE serialises against the settle path's
// FOR UPDATE paid-flip, so an intent cannot be opened on an invoice that just
// got paid. It is a READ of invoices, never an UPDATE — charge_attempt_seq must
// not move at claim time (ADR-105 rejected that explicitly: a crash after a
// claim-time bump would present a different key and mint a second PI).
func (s *ChargeIntentStore) Open(ctx context.Context, in domain.ChargeIntent) (domain.ChargeIntent, bool, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, in.TenantID)
	if err != nil {
		return domain.ChargeIntent{}, false, err
	}
	defer postgres.Rollback(tx)

	var status, paymentStatus string
	var amountDue int64
	if err := tx.QueryRowContext(ctx,
		`SELECT status, payment_status, amount_due_cents FROM invoices WHERE id = $1 FOR SHARE`,
		in.InvoiceID).Scan(&status, &paymentStatus, &amountDue); err != nil {
		if err == sql.ErrNoRows {
			return domain.ChargeIntent{}, false, errs.ErrNotFound
		}
		return domain.ChargeIntent{}, false, fmt.Errorf("payable re-check: %w", err)
	}
	if status != "finalized" || amountDue <= 0 {
		return domain.ChargeIntent{}, false, ErrInvoiceNotPayable
	}
	// payment_status was read and then ignored (adversarial review, 2026-07-30).
	// It matters: an invoice already in flight has a PaymentIntent live at
	// Stripe, and one that predates this ledger has NO intent row — so the
	// one-unresolved index cannot block it and a second charge would open
	// unopposed. IsInFlight semantics, matching the checkout guard.
	if domain.InvoicePaymentStatus(paymentStatus).IsInFlight() {
		return domain.ChargeIntent{}, false, ErrChargeAttemptInFlight
	}

	metaJSON, err := json.Marshal(in.Metadata)
	if err != nil {
		return domain.ChargeIntent{}, false, fmt.Errorf("marshal charge intent metadata: %w", err)
	}

	// Untargeted ON CONFLICT DO NOTHING: a conflict is a no-op rather than a
	// 25P02 abort, so the read-back below runs on THIS transaction. (The
	// checkout_sessions claim predates this trick and pays for it with a
	// three-attempt fresh-tx loser protocol.)
	res, err := tx.ExecContext(ctx, `
		INSERT INTO charge_intents (
			id, tenant_id, invoice_id, idempotency_key, amount_cents, currency,
			stripe_customer_id, stripe_payment_method_id, description, metadata,
			purpose, occurred_at, sim_effective_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,
			-- occurred_at is the KEY's first use, not this row's birth. A
			-- re-attempt under a recurring key (which fix 3 deliberately
			-- permits) would otherwise reset the clock and let the replay TTL
			-- re-authorise a key the provider has already forgotten — the two
			-- fixes cancelling each other out (found by adversarial review,
			-- 2026-07-30). Inherit the earliest use of this exact key.
			COALESCE((SELECT MIN(occurred_at) FROM charge_intents k
			          WHERE k.tenant_id = $2 AND k.idempotency_key = $4), $12),
			$13)
		ON CONFLICT DO NOTHING`,
		postgres.NewID("vlx_cin"), in.TenantID, in.InvoiceID, in.IdempotencyKey,
		in.AmountCents, in.Currency, in.StripeCustomerID, in.StripePaymentMethodID,
		in.Description, metaJSON, in.Purpose, in.OccurredAt,
		postgres.NullableTime(in.SimEffectiveAt),
	)
	if err != nil {
		return domain.ChargeIntent{}, false, fmt.Errorf("open charge intent: %w", err)
	}
	// Did WE create the row? Only the creator may delete it on a provably-no-call
	// skip; a caller handed someone else's in-flight row must never erase their
	// pre-effect marker (found by adversarial review, 2026-07-30).
	inserted, err := res.RowsAffected()
	if err != nil {
		return domain.ChargeIntent{}, false, fmt.Errorf("open charge intent rows: %w", err)
	}

	// Read back whatever holds the slot — ours or an earlier attempt's.
	row := tx.QueryRowContext(ctx, `SELECT `+chargeIntentCols+`
		FROM charge_intents
		WHERE tenant_id = $1 AND invoice_id = $2 AND state <> 'resolved'
		FOR UPDATE`, in.TenantID, in.InvoiceID)
	held, err := scanChargeIntent(row)
	if err != nil {
		if err == sql.ErrNoRows {
			// Fail closed rather than charge blind.
			return domain.ChargeIntent{}, false, fmt.Errorf("charge intent vanished after insert for invoice %s", in.InvoiceID)
		}
		return domain.ChargeIntent{}, false, fmt.Errorf("read back charge intent: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return domain.ChargeIntent{}, false, err
	}
	return held, inserted == 1, nil
}

// Resolve closes an intent, naming the PaymentIntent it produced. CAS on
// state <> 'resolved' so a concurrent recovery and the inline path converge
// instead of fighting.
func (s *ChargeIntentStore) Resolve(ctx context.Context, tenantID, id, paymentIntentID string) error {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return err
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(ctx, `
		UPDATE charge_intents
		SET state = 'resolved', stripe_payment_intent_id = $1, resolved_at = now(), updated_at = now()
		WHERE id = $2 AND state <> 'resolved'`, paymentIntentID, id); err != nil {
		return fmt.Errorf("resolve charge intent: %w", err)
	}
	return tx.Commit()
}

// Delete removes an intent for the ONE case where the request provably never
// reached Stripe: the circuit breaker short-circuited before the call. Deleting
// rather than resolving keeps the invoice immediately chargeable — a breaker
// skip is a transient, not an attempt.
func (s *ChargeIntentStore) Delete(ctx context.Context, tenantID, id string) error {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return err
	}
	defer postgres.Rollback(tx)
	// recovery_attempts = 0 is the other half of the ownership check. The
	// creator flag proves nobody ELSE created the row, but not that nobody else
	// has since picked it up: once recovery has touched it, a breaker skip on
	// the original caller must not erase a marker that sweep is working from
	// (found by adversarial review, 2026-07-30).
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM charge_intents
		 WHERE id = $1 AND state = 'open' AND recovery_attempts = 0
		   AND COALESCE(stripe_payment_intent_id,'') = ''`, id); err != nil {
		return fmt.Errorf("delete charge intent: %w", err)
	}
	return tx.Commit()
}

// ListOpen returns open intents past the cool-off for the recovery sweep,
// cross-tenant (TxBypass) and scoped to the ctx livemode, mirroring the
// payment reconciler's own sweeps.
//
// needs_review rows ARE listed, and that is a correction: excluding them made
// quarantine permanent. The documented operator exit — re-send the event from
// the Stripe dashboard, the webhook settles the invoice — only closes the intent
// through the sweep's ADOPTION step, which a filtered-out row never reaches. So
// the invoice stayed blocked forever with no path back (found by adversarial
// review, 2026-07-30). The caller compensates by allowing quarantined rows the
// adoption check ONLY: no replay, no Stripe call, so they still cost nothing per
// tick while regaining an exit.
//
// NOT gated on is_simulated, for the same reason listInflightPayments isn't:
// PaymentIntent truth lives at Stripe on real time even for clock-pinned
// invoices, and the catchup worker has no payment-sync phase, so excluding
// simulated rows would strand a simulated attempt forever.
func (s *ChargeIntentStore) ListOpen(ctx context.Context, olderThan time.Time, limit int) ([]domain.ChargeIntent, error) {
	if limit <= 0 {
		limit = 50
	}
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	// ORDER BY state DESC puts 'open' before 'needs_review' (o < r), which is
	// load-bearing rather than cosmetic. A quarantined row never advances its
	// updated_at — nothing writes to it — so under a pure updated_at ordering it
	// becomes the permanent head of a LIMITed queue and starves every
	// recoverable attempt behind it, one row of starvation per quarantine
	// (found by adversarial review, 2026-07-30). Quarantined rows still need
	// listing for their adoption exit, but they must never crowd out work that
	// can actually be finished.
	rows, err := tx.QueryContext(ctx, `SELECT `+chargeIntentCols+`
		FROM charge_intents
		WHERE state <> 'resolved' AND livemode = $1 AND updated_at < $2
		ORDER BY state DESC, updated_at ASC
		LIMIT $3`, postgres.Livemode(ctx), olderThan, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []domain.ChargeIntent
	for rows.Next() {
		ci, err := scanChargeIntent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ci)
	}
	return out, rows.Err()
}

// BumpRecoveryAttempt increments the attempt counter and returns whether this
// caller won the row. The bump happens BEFORE the Stripe replay, on the
// webhook-outbox principle: a crash-loop must still advance toward
// needs_review instead of retrying forever.
func (s *ChargeIntentStore) BumpRecoveryAttempt(ctx context.Context, tenantID, id string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return false, err
	}
	defer postgres.Rollback(tx)
	res, err := tx.ExecContext(ctx, `
		UPDATE charge_intents
		SET recovery_attempts = recovery_attempts + 1, updated_at = now()
		WHERE id = $1 AND state = 'open'`, id)
	if err != nil {
		return false, fmt.Errorf("bump recovery attempt: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return n == 1, nil
}

// ReleaseRecoveryAttempt un-counts an attempt that provably never reached
// Stripe (the circuit breaker short-circuited before the call). Mirrors
// ReleaseAutoChargeClaim: a skip is not an attempt, and counting it would let
// an outage exhaust the recovery budget for work that never happened.
func (s *ChargeIntentStore) ReleaseRecoveryAttempt(ctx context.Context, tenantID, id string) error {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return err
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(ctx, `
		UPDATE charge_intents
		SET recovery_attempts = GREATEST(recovery_attempts - 1, 0)
		WHERE id = $1 AND state = 'open'`, id); err != nil {
		return fmt.Errorf("release recovery attempt: %w", err)
	}
	return tx.Commit()
}

// MarkNeedsReview quarantines an intent that automatic recovery cannot resolve.
// The row keeps blocking new charges by design.
func (s *ChargeIntentStore) MarkNeedsReview(ctx context.Context, tenantID, id, reason string) error {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return err
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(ctx, `
		UPDATE charge_intents
		SET state = 'needs_review', last_error = $1, updated_at = now()
		WHERE id = $2 AND state = 'open'`, errs.Scrub(reason), id); err != nil {
		return fmt.Errorf("mark charge intent needs_review: %w", err)
	}
	return tx.Commit()
}

// HasUnresolved reports whether any attempt on this invoice is still
// unresolved. Used to gate the reconciler's settle-failed give-up: that write
// is what un-quarantines the invoice and hands the next retry a key Stripe has
// never seen, so it must not run while an attempt is outstanding.
func (s *ChargeIntentStore) HasUnresolved(ctx context.Context, tenantID, invoiceID string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return false, err
	}
	defer postgres.Rollback(tx)
	var exists bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM charge_intents
			WHERE tenant_id = $1 AND invoice_id = $2 AND state <> 'resolved')`,
		tenantID, invoiceID).Scan(&exists); err != nil {
		return false, fmt.Errorf("check unresolved charge intent: %w", err)
	}
	return exists, tx.Commit()
}
