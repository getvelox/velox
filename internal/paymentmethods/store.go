package paymentmethods

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

// Store is the persistence contract. Kept narrow — each operation maps to
// exactly one handler action so we don't grow a god-store.
type Store interface {
	List(ctx context.Context, tenantID, customerID string) ([]PaymentMethod, error)
	Get(ctx context.Context, tenantID, pmID string) (PaymentMethod, error)

	// Upsert writes a payment_methods row keyed by stripe_payment_method_id.
	// Webhooks can fire more than once for the same setup intent (Stripe
	// retries on 5xx), so the webhook path must be idempotent. If first is
	// true and no existing active default for the customer, the new row is
	// promoted to default atomically in the same tx.
	Upsert(ctx context.Context, tenantID string, pm PaymentMethod) (PaymentMethod, error)

	// SetDefault atomically clears any existing default for (customerID)
	// and flags pmID as the new default. Fails with ErrNotFound if pmID is
	// detached or not owned by customerID.
	SetDefault(ctx context.Context, tenantID, customerID, pmID string) (PaymentMethod, error)

	// Detach marks the PM as detached_at = now(). Idempotent — a second
	// call on an already-detached row is a no-op and returns the row.
	Detach(ctx context.Context, tenantID, customerID, pmID string) (PaymentMethod, error)

	// DetachAndRebalance detaches pmID and, in the SAME transaction,
	// promotes the newest remaining active PM to default when the detached
	// card was the default — so there is never a committed "active cards
	// but no default" window (a transient failure on the promote rolls the
	// detach back too). Returns the detached row plus the promoted default
	// (nil when the detached card wasn't the default, or no active PM
	// remained to promote). Idempotent: re-detaching an already-detached
	// row promotes nothing (a detached row is never the default).
	DetachAndRebalance(ctx context.Context, tenantID, customerID, pmID string) (PaymentMethod, *PaymentMethod, error)
}

type PostgresStore struct {
	db *postgres.DB
}

func NewPostgresStore(db *postgres.DB) *PostgresStore { return &PostgresStore{db: db} }

const pmSelectCols = `id, tenant_id, livemode, customer_id, stripe_payment_method_id,
	type, COALESCE(card_brand,''), COALESCE(card_last4,''),
	COALESCE(card_exp_month,0), COALESCE(card_exp_year,0),
	COALESCE(card_fingerprint,''),
	is_default, detached_at, created_at, updated_at`

func scanPM(row interface {
	Scan(dest ...any) error
}, pm *PaymentMethod) error {
	return row.Scan(&pm.ID, &pm.TenantID, &pm.Livemode, &pm.CustomerID, &pm.StripePaymentMethodID,
		&pm.Type, &pm.CardBrand, &pm.CardLast4, &pm.CardExpMonth, &pm.CardExpYear,
		&pm.CardFingerprint,
		&pm.IsDefault, &pm.DetachedAt, &pm.CreatedAt, &pm.UpdatedAt)
}

func (s *PostgresStore) List(ctx context.Context, tenantID, customerID string) ([]PaymentMethod, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	rows, err := tx.QueryContext(ctx, `
		SELECT `+pmSelectCols+` FROM payment_methods
		WHERE tenant_id = $1 AND customer_id = $2 AND detached_at IS NULL
		ORDER BY is_default DESC, created_at DESC LIMIT 100`, tenantID, customerID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []PaymentMethod
	for rows.Next() {
		var pm PaymentMethod
		if err := scanPM(rows, &pm); err != nil {
			return nil, err
		}
		out = append(out, pm)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, tx.Commit()
}

func (s *PostgresStore) Get(ctx context.Context, tenantID, pmID string) (PaymentMethod, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return PaymentMethod{}, err
	}
	defer postgres.Rollback(tx)

	var pm PaymentMethod
	if err := scanPM(tx.QueryRowContext(ctx, `
		SELECT `+pmSelectCols+` FROM payment_methods
		WHERE tenant_id = $1 AND id = $2`, tenantID, pmID), &pm); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return PaymentMethod{}, errs.ErrNotFound
		}
		return PaymentMethod{}, err
	}
	return pm, tx.Commit()
}

// Upsert inserts a row keyed by (tenant_id, livemode, stripe_payment_method_id).
// On conflict we refresh card metadata but leave is_default alone — the
// customer's default choice shouldn't flip just because Stripe resent the
// webhook. If no active default exists for the customer, the new row is
// promoted to default in the same tx (enforces "first PM is default").
//
// Dedupe-by-fingerprint (ADR-0099): when a customer re-runs Add and
// produces a new pm_xxx with a fingerprint that already exists as an
// active row for this customer, the old row is detached in the same
// transaction and the new row inherits its is_default flag. Industry
// standard — Stripe explicitly recommends this pattern because each
// Checkout completion mints a fresh PaymentMethod even for the same
// physical card. Without dedupe, the customer sees "Visa ····4242"
// twice (or more) and the default-PM semantics drift across the
// duplicates. Skipped when fingerprint is empty (legacy rows /
// non-card types) — those keep current behavior.
func (s *PostgresStore) Upsert(ctx context.Context, tenantID string, pm PaymentMethod) (PaymentMethod, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return PaymentMethod{}, err
	}
	defer postgres.Rollback(tx)

	// Dedupe-by-fingerprint: detach any active row for this customer
	// with the same fingerprint. Capture is_default off the detached
	// row so the new attach can inherit it — otherwise re-adding the
	// default card would unset its default flag (regression hazard).
	// Skip when fingerprint is empty (no key to dedupe on).
	carryDefault := false
	if pm.CardFingerprint != "" {
		var oldDefault bool
		var oldID string
		switch err := tx.QueryRowContext(ctx, `
			SELECT id, is_default FROM payment_methods
			WHERE tenant_id = $1 AND customer_id = $2
			  AND card_fingerprint = $3
			  AND detached_at IS NULL
			  AND stripe_payment_method_id <> $4
			LIMIT 1`, tenantID, pm.CustomerID, pm.CardFingerprint, pm.StripePaymentMethodID,
		).Scan(&oldID, &oldDefault); {
		case err == nil:
			// Match found — detach it. Same-tx so a concurrent List
			// never observes both the old and new active.
			if _, err := tx.ExecContext(ctx, `
				UPDATE payment_methods
				SET detached_at = now(), is_default = false, updated_at = now()
				WHERE tenant_id = $1 AND id = $2`, tenantID, oldID); err != nil {
				return PaymentMethod{}, fmt.Errorf("dedupe-detach: %w", err)
			}
			carryDefault = oldDefault
		case errors.Is(err, sql.ErrNoRows):
			// No prior row with this fingerprint — clean path.
		default:
			return PaymentMethod{}, fmt.Errorf("dedupe lookup: %w", err)
		}
	}

	// Does this customer already have an active default after the
	// dedupe pass? If not, promote the new row.
	var hasDefault bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM payment_methods
			WHERE tenant_id = $1 AND customer_id = $2
			  AND is_default = true AND detached_at IS NULL
		)`, tenantID, pm.CustomerID).Scan(&hasDefault); err != nil {
		return PaymentMethod{}, fmt.Errorf("check default: %w", err)
	}
	promote := !hasDefault || carryDefault

	var out PaymentMethod
	if err := scanPM(tx.QueryRowContext(ctx, `
		INSERT INTO payment_methods
			(tenant_id, customer_id, stripe_payment_method_id, type,
			 card_brand, card_last4, card_exp_month, card_exp_year,
			 card_fingerprint,
			 is_default, created_at, updated_at)
		VALUES ($1, $2, $3, $4, NULLIF($5,''), NULLIF($6,''),
		        NULLIF($7,0), NULLIF($8,0), NULLIF($9,''),
		        $10, now(), now())
		ON CONFLICT (tenant_id, livemode, stripe_payment_method_id) DO UPDATE
		SET card_brand       = EXCLUDED.card_brand,
		    card_last4       = EXCLUDED.card_last4,
		    card_exp_month   = EXCLUDED.card_exp_month,
		    card_exp_year    = EXCLUDED.card_exp_year,
		    card_fingerprint = COALESCE(EXCLUDED.card_fingerprint, payment_methods.card_fingerprint),
		    detached_at      = NULL,
		    updated_at       = now()
		RETURNING `+pmSelectCols,
		tenantID, pm.CustomerID, pm.StripePaymentMethodID, defaultString(pm.Type, "card"),
		pm.CardBrand, pm.CardLast4, pm.CardExpMonth, pm.CardExpYear,
		pm.CardFingerprint,
		promote), &out); err != nil {
		return PaymentMethod{}, fmt.Errorf("upsert payment method: %w", err)
	}

	return out, tx.Commit()
}

// SetDefault does the atomic swap inside one tx so RLS + partial unique
// index see both writes consistently.
func (s *PostgresStore) SetDefault(ctx context.Context, tenantID, customerID, pmID string) (PaymentMethod, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return PaymentMethod{}, err
	}
	defer postgres.Rollback(tx)

	// Clear any other default for this customer first. Postgres validates
	// the partial unique index per-row; clearing before setting avoids a
	// transient violation that would rollback the whole tx.
	if _, err := tx.ExecContext(ctx, `
		UPDATE payment_methods SET is_default = false, updated_at = now()
		WHERE tenant_id = $1 AND customer_id = $2
		  AND is_default = true AND detached_at IS NULL
		  AND id <> $3`, tenantID, customerID, pmID); err != nil {
		return PaymentMethod{}, fmt.Errorf("clear existing default: %w", err)
	}

	var out PaymentMethod
	if err := scanPM(tx.QueryRowContext(ctx, `
		UPDATE payment_methods SET is_default = true, updated_at = now()
		WHERE tenant_id = $1 AND customer_id = $2 AND id = $3
		  AND detached_at IS NULL
		RETURNING `+pmSelectCols,
		tenantID, customerID, pmID), &out); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return PaymentMethod{}, errs.ErrNotFound
		}
		return PaymentMethod{}, fmt.Errorf("set default: %w", err)
	}

	return out, tx.Commit()
}

// Detach is idempotent by design. Re-detaching a row is a no-op write
// (detached_at is kept at its original timestamp).
func (s *PostgresStore) Detach(ctx context.Context, tenantID, customerID, pmID string) (PaymentMethod, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return PaymentMethod{}, err
	}
	defer postgres.Rollback(tx)

	var out PaymentMethod
	if err := scanPM(tx.QueryRowContext(ctx, `
		UPDATE payment_methods
		SET detached_at = COALESCE(detached_at, now()),
		    is_default  = false,
		    updated_at  = now()
		WHERE tenant_id = $1 AND customer_id = $2 AND id = $3
		RETURNING `+pmSelectCols,
		tenantID, customerID, pmID), &out); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return PaymentMethod{}, errs.ErrNotFound
		}
		return PaymentMethod{}, fmt.Errorf("detach: %w", err)
	}

	return out, tx.Commit()
}

// DetachAndRebalance does the detach and the replacement-default promotion
// in one transaction so the invariant "≥1 active PM ⇒ exactly one default"
// holds across the whole operation — no committed window with active cards
// but no default. See the Store interface for the contract.
func (s *PostgresStore) DetachAndRebalance(ctx context.Context, tenantID, customerID, pmID string) (PaymentMethod, *PaymentMethod, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return PaymentMethod{}, nil, err
	}
	defer postgres.Rollback(tx)

	// Lock + read the target to learn whether it was the default BEFORE we
	// clear the flag (RETURNING gives the post-update value). FOR UPDATE
	// serialises concurrent detaches of the same customer's cards.
	var wasDefault bool
	if err := tx.QueryRowContext(ctx, `
		SELECT is_default FROM payment_methods
		WHERE tenant_id = $1 AND customer_id = $2 AND id = $3
		FOR UPDATE`, tenantID, customerID, pmID).Scan(&wasDefault); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return PaymentMethod{}, nil, errs.ErrNotFound
		}
		return PaymentMethod{}, nil, fmt.Errorf("lock payment method: %w", err)
	}

	var detached PaymentMethod
	if err := scanPM(tx.QueryRowContext(ctx, `
		UPDATE payment_methods
		SET detached_at = COALESCE(detached_at, now()),
		    is_default  = false,
		    updated_at  = now()
		WHERE tenant_id = $1 AND customer_id = $2 AND id = $3
		RETURNING `+pmSelectCols,
		tenantID, customerID, pmID), &detached); err != nil {
		return PaymentMethod{}, nil, fmt.Errorf("detach: %w", err)
	}

	var newDefault *PaymentMethod
	if wasDefault {
		// Promote the newest remaining active card. The just-detached row is
		// detached_at IS NULL appears in BOTH the subquery and the OUTER
		// WHERE, and the outer copy is load-bearing: under READ COMMITTED a
		// concurrent Detach of the chosen id commits between snapshot and
		// row lock, and EvalPlanQual re-evaluates only the OUTER predicate —
		// without it the promote lands is_default=true on a detached card
		// (the charge-a-removed-card hazard; found by the ADR-113 doctor
		// mining pass, and exactly what the doctor's
		// payment_methods_detached_but_still_default check flags).
		// excluded (detached_at IS NOT NULL). The prior default is already
		// cleared, so no other is_default row conflicts with the partial
		// unique index — a single targeted set is safe. ErrNoRows = no
		// active card left, leave the customer with no default.
		var nd PaymentMethod
		err := scanPM(tx.QueryRowContext(ctx, `
			UPDATE payment_methods SET is_default = true, updated_at = now()
			WHERE tenant_id = $1 AND customer_id = $2 AND detached_at IS NULL AND id = (
			    SELECT id FROM payment_methods
			    WHERE tenant_id = $1 AND customer_id = $2 AND detached_at IS NULL
			    ORDER BY created_at DESC, id DESC
			    LIMIT 1
			)
			RETURNING `+pmSelectCols,
			tenantID, customerID), &nd)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return PaymentMethod{}, nil, fmt.Errorf("promote replacement default: %w", err)
		}
		if err == nil {
			newDefault = &nd
		}
	}

	if err := tx.Commit(); err != nil {
		return PaymentMethod{}, nil, err
	}
	return detached, newDefault, nil
}

func defaultString(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// Compile-time assertion that PostgresStore satisfies Store.
var _ Store = (*PostgresStore)(nil)
