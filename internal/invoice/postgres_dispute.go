package invoice

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

const disputeCols = `id, invoice_id, stripe_dispute_id, stripe_charge_id, stripe_payment_intent_id,
	status, reason, amount_cents, currency, evidence_due_by, opened_at, closed_at,
	last_event_at, last_stripe_event_id, livemode, created_at, updated_at`

func scanDisputeDest(d *domain.InvoiceDispute) []any {
	return []any{
		&d.ID, &d.InvoiceID, &d.StripeDisputeID, &d.StripeChargeID, &d.StripePaymentIntentID,
		(*string)(&d.Status), &d.Reason, &d.AmountCents, &d.Currency, &d.EvidenceDueBy, &d.OpenedAt, &d.ClosedAt,
		&d.LastEventAt, &d.LastStripeEventID, &d.Livemode, &d.CreatedAt, &d.UpdatedAt,
	}
}

// RecordStripeDispute records one charge.dispute.* snapshot against the
// invoice whose payment was disputed (#979), in one transaction:
//
//   - the invoice is found from the dispute's own row when Velox already
//     recorded it, otherwise from the disputed PaymentIntent (the invoice's
//     charge attempts, then the invoice's own payment and anomaly columns).
//     No invoice holds it: errs.ErrNotFound, the charge was not Velox's;
//   - the invoice row is locked first, then the dispute row (lock order rule,
//     playbook §5.6), so two events for one dispute serialize;
//   - the snapshot is written only when it is newer than the row
//     (InvoiceDispute.SupersededBy), so a redelivery or an older event that
//     arrives late changes nothing and queues nothing;
//   - payment.dispute_opened is queued when this call creates the row, and
//     payment.dispute_closed when it moves the dispute to a closed status it
//     did not have, both in this transaction (ADR-040 outbox): the events
//     commit with the record or not at all.
//
// The invoice itself is never written: its status, payment status and
// amounts stay as they are.
func (s *PostgresStore) RecordStripeDispute(ctx context.Context, tenantID string, snap domain.DisputeSnapshot) (domain.DisputeRecordResult, error) {
	if snap.StripeDisputeID == "" || snap.StripePaymentIntentID == "" {
		return domain.DisputeRecordResult{}, fmt.Errorf("dispute snapshot needs a dispute id and a payment intent id")
	}
	if !snap.Status.Valid() {
		return domain.DisputeRecordResult{}, fmt.Errorf("dispute %s: unknown status %q", snap.StripeDisputeID, snap.Status)
	}
	currency := strings.ToUpper(strings.TrimSpace(snap.Currency))

	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.DisputeRecordResult{}, err
	}
	defer postgres.Rollback(tx)

	invoiceID, err := disputedInvoiceTx(ctx, tx, snap.StripeDisputeID, snap.StripePaymentIntentID)
	if err != nil {
		return domain.DisputeRecordResult{}, err
	}
	// The whole row: the events below carry the invoice (ADR-131).
	var inv domain.Invoice
	if err := tx.QueryRowContext(ctx,
		`SELECT `+invCols+` FROM invoices WHERE id = $1 FOR UPDATE`, invoiceID,
	).Scan(s.scanInvDest(&inv)...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.DisputeRecordResult{}, errs.ErrNotFound
		}
		return domain.DisputeRecordResult{}, fmt.Errorf("lock disputed invoice: %w", err)
	}

	var cur domain.InvoiceDispute
	err = tx.QueryRowContext(ctx,
		`SELECT `+disputeCols+` FROM invoice_disputes WHERE stripe_dispute_id = $1 FOR UPDATE`,
		snap.StripeDisputeID,
	).Scan(scanDisputeDest(&cur)...)
	found := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return domain.DisputeRecordResult{}, fmt.Errorf("read dispute %s: %w", snap.StripeDisputeID, err)
	}

	var res domain.DisputeRecordResult
	switch {
	case !found:
		var closedAt *time.Time
		if snap.Status.Closed() {
			closedAt = &snap.EventAt
		}
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO invoice_disputes
				(tenant_id, invoice_id, stripe_dispute_id, stripe_charge_id, stripe_payment_intent_id,
				 status, reason, amount_cents, currency, evidence_due_by, opened_at, closed_at,
				 last_event_at, last_stripe_event_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
			RETURNING `+disputeCols,
			tenantID, invoiceID, snap.StripeDisputeID, snap.StripeChargeID, snap.StripePaymentIntentID,
			string(snap.Status), snap.Reason, snap.AmountCents, currency, snap.EvidenceDueBy,
			snap.OpenedAt, closedAt, snap.EventAt, snap.StripeEventID,
		).Scan(scanDisputeDest(&res.Dispute)...); err != nil {
			return domain.DisputeRecordResult{}, fmt.Errorf("insert dispute %s: %w", snap.StripeDisputeID, err)
		}
		res.Applied, res.Opened, res.Closed = true, true, snap.Status.Closed()

	case !cur.SupersededBy(snap):
		// A redelivery, or an event older than the one the row holds.
		res.Dispute = cur
		return res, tx.Commit()

	default:
		// The close time is the first event that carried this closed status.
		// A later snapshot with the same status keeps it; a new closed status
		// (a late win after a loss) restarts it; an open status clears it.
		var closedAt *time.Time
		if snap.Status.Closed() {
			closedAt = &snap.EventAt
			if cur.Status == snap.Status && cur.ClosedAt != nil {
				closedAt = cur.ClosedAt
			}
		}
		chargeID := snap.StripeChargeID
		if chargeID == "" {
			chargeID = cur.StripeChargeID
		}
		if err := tx.QueryRowContext(ctx, `
			UPDATE invoice_disputes SET
				stripe_charge_id = $2, status = $3, reason = $4, amount_cents = $5, currency = $6,
				evidence_due_by = $7, closed_at = $8, last_event_at = $9, last_stripe_event_id = $10,
				updated_at = now()
			WHERE id = $1
			RETURNING `+disputeCols,
			cur.ID, chargeID, string(snap.Status), snap.Reason, snap.AmountCents, currency,
			snap.EvidenceDueBy, closedAt, snap.EventAt, snap.StripeEventID,
		).Scan(scanDisputeDest(&res.Dispute)...); err != nil {
			return domain.DisputeRecordResult{}, fmt.Errorf("update dispute %s: %w", snap.StripeDisputeID, err)
		}
		res.Applied = true
		res.Closed = snap.Status.Closed() && snap.Status != cur.Status
	}

	if s.outbox != nil {
		payload := domain.InvoiceWebhookData(inv, map[string]any{"dispute": res.Dispute})
		if res.Opened {
			if _, err := s.outbox.Enqueue(ctx, tx, tenantID, domain.EventPaymentDisputeOpened, payload); err != nil {
				return domain.DisputeRecordResult{}, fmt.Errorf("enqueue %s: %w", domain.EventPaymentDisputeOpened, err)
			}
		}
		if res.Closed {
			if _, err := s.outbox.Enqueue(ctx, tx, tenantID, domain.EventPaymentDisputeClosed, payload); err != nil {
				return domain.DisputeRecordResult{}, fmt.Errorf("enqueue %s: %w", domain.EventPaymentDisputeClosed, err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return domain.DisputeRecordResult{}, err
	}
	return res, nil
}

// disputedInvoiceTx finds the invoice a dispute belongs to. A recorded
// dispute keeps the invoice it was first recorded against. A new one is
// matched by its PaymentIntent: every payment a settle path saw has a charge
// attempt row (the success upserts it in the settle transaction), and the
// invoice columns cover rows from before attempts were recorded and payments
// flagged as anomalies.
func disputedInvoiceTx(ctx context.Context, tx *sql.Tx, stripeDisputeID, paymentIntentID string) (string, error) {
	var id string
	err := tx.QueryRowContext(ctx,
		`SELECT invoice_id FROM invoice_disputes WHERE stripe_dispute_id = $1`, stripeDisputeID,
	).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("read dispute %s: %w", stripeDisputeID, err)
	}
	err = tx.QueryRowContext(ctx,
		`SELECT invoice_id FROM invoice_charge_attempts WHERE stripe_payment_intent_id = $1`, paymentIntentID,
	).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("find attempt for payment intent %s: %w", paymentIntentID, err)
	}
	err = tx.QueryRowContext(ctx, `
		SELECT id FROM invoices
		WHERE stripe_payment_intent_id = $1 OR payment_anomaly_payment_intent_id = $1
		ORDER BY (stripe_payment_intent_id = $1) DESC, created_at
		LIMIT 1`, paymentIntentID,
	).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", errs.ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("find invoice for payment intent %s: %w", paymentIntentID, err)
	}
	return id, nil
}

// ListDisputesByInvoice returns the invoice's disputes, oldest first.
func (s *PostgresStore) ListDisputesByInvoice(ctx context.Context, tenantID, invoiceID string) ([]domain.InvoiceDispute, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	rows, err := tx.QueryContext(ctx,
		`SELECT `+disputeCols+` FROM invoice_disputes WHERE invoice_id = $1 ORDER BY opened_at, id`, invoiceID)
	if err != nil {
		return nil, fmt.Errorf("list disputes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []domain.InvoiceDispute{}
	for rows.Next() {
		var d domain.InvoiceDispute
		if err := rows.Scan(scanDisputeDest(&d)...); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, tx.Commit()
}

// LostDisputesByInvoices returns, for a page of invoices, the newest dispute
// on each that closed as lost: invoice id → dispute. Invoices without one are
// absent. One read for the page; the attention notice reads it.
func (s *PostgresStore) LostDisputesByInvoices(ctx context.Context, tenantID string, invoiceIDs []string) (map[string]domain.InvoiceDispute, error) {
	out := make(map[string]domain.InvoiceDispute)
	if len(invoiceIDs) == 0 {
		return out, nil
	}
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	rows, err := tx.QueryContext(ctx, `
		SELECT DISTINCT ON (invoice_id) `+disputeCols+`
		FROM invoice_disputes
		WHERE invoice_id = ANY($1::text[]) AND status = $2
		ORDER BY invoice_id, closed_at DESC, id DESC`,
		postgres.StringArray(invoiceIDs), string(domain.DisputeLost))
	if err != nil {
		return nil, fmt.Errorf("read lost disputes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var d domain.InvoiceDispute
		if err := rows.Scan(scanDisputeDest(&d)...); err != nil {
			return nil, err
		}
		out[d.InvoiceID] = d
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, tx.Commit()
}
