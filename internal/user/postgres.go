package user

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

// PostgresStore is the production persistence for user accounts. All
// queries run under TxBypass — there is no tenant context for users
// (a user *belongs to* tenants via user_tenants but the user row
// itself is global). Auth resolution happens before any tenant ctx is
// available.
type PostgresStore struct {
	db *postgres.DB
}

func NewPostgresStore(db *postgres.DB) *PostgresStore {
	return &PostgresStore{db: db}
}

const userCols = `id, email::text, password_hash, created_at, last_login_at`

func scanUser(row *sql.Row) (domain.User, error) {
	var u domain.User
	err := row.Scan(&u.ID, &u.Email, &u.PasswordHash, &u.CreatedAt, &u.LastLoginAt)
	return u, err
}

// ErrEmailTaken is returned when Create hits the email unique
// violation. Surfaced to the API as 409.
var ErrEmailTaken = errs.AlreadyExists("email", "an account with this email already exists")

func (s *PostgresStore) Create(ctx context.Context, email, passwordHash string) (domain.User, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return domain.User{}, err
	}
	defer postgres.Rollback(tx)

	row := tx.QueryRowContext(ctx, `
		INSERT INTO users (email, password_hash)
		VALUES ($1, $2)
		RETURNING `+userCols, strings.TrimSpace(email), passwordHash)

	u, err := scanUser(row)
	if err != nil {
		if postgres.IsUniqueViolation(err) {
			return domain.User{}, ErrEmailTaken
		}
		return domain.User{}, fmt.Errorf("create user: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return domain.User{}, err
	}
	return u, nil
}

func (s *PostgresStore) GetByEmail(ctx context.Context, email string) (domain.User, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return domain.User{}, err
	}
	defer postgres.Rollback(tx)

	row := tx.QueryRowContext(ctx,
		`SELECT `+userCols+` FROM users WHERE email = $1`, strings.TrimSpace(email))
	u, err := scanUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.User{}, errs.ErrNotFound
	}
	return u, err
}

func (s *PostgresStore) GetByID(ctx context.Context, id string) (domain.User, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return domain.User{}, err
	}
	defer postgres.Rollback(tx)

	row := tx.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE id = $1`, id)
	u, err := scanUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.User{}, errs.ErrNotFound
	}
	return u, err
}

func (s *PostgresStore) TouchLastLogin(ctx context.Context, id string, at time.Time) error {
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return err
	}
	defer postgres.Rollback(tx)
	_, err = tx.ExecContext(ctx,
		`UPDATE users SET last_login_at = $1 WHERE id = $2`, at, id)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *PostgresStore) SetPassword(ctx context.Context, id, passwordHash string) error {
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return err
	}
	defer postgres.Rollback(tx)
	_, err = tx.ExecContext(ctx,
		`UPDATE users SET password_hash = $1 WHERE id = $2`, passwordHash, id)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *PostgresStore) AttachTenant(ctx context.Context, userID, tenantID, role string) error {
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return err
	}
	defer postgres.Rollback(tx)
	_, err = tx.ExecContext(ctx, `
		INSERT INTO user_tenants (user_id, tenant_id, role)
		VALUES ($1, $2, $3)
		ON CONFLICT (user_id, tenant_id) DO NOTHING
	`, userID, tenantID, role)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// CreateInTx inserts a user and their tenant attachment inside the
// CALLER's transaction — the seam tenant.RunBootstrap needs so the
// owner user commits (or rolls back) atomically with the tenant, keys,
// and settings it belongs to (ADR-073). passwordHash must already be a
// bcrypt hash (user.HashPassword validates + hashes). No commit here;
// the caller owns the tx.
func (s *PostgresStore) CreateInTx(ctx context.Context, tx *sql.Tx, email, passwordHash, tenantID, role string) (domain.User, error) {
	row := tx.QueryRowContext(ctx, `
		INSERT INTO users (email, password_hash)
		VALUES ($1, $2)
		RETURNING `+userCols, strings.TrimSpace(email), passwordHash)
	u, err := scanUser(row)
	if err != nil {
		if postgres.IsUniqueViolation(err) {
			return domain.User{}, ErrEmailTaken
		}
		return domain.User{}, fmt.Errorf("create user: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO user_tenants (user_id, tenant_id, role)
		VALUES ($1, $2, $3)
	`, u.ID, tenantID, role); err != nil {
		return domain.User{}, fmt.Errorf("attach tenant: %w", err)
	}
	return u, nil
}

func (s *PostgresStore) TenantsForUser(ctx context.Context, userID string) ([]domain.UserTenant, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	// Ordered by join time: login picks tenants[0] (v1 has no tenant
	// switcher), so the FIRST-joined membership — the user's original
	// workspace — must win deterministically once invites (ADR-081)
	// create multi-membership users.
	rows, err := tx.QueryContext(ctx,
		`SELECT user_id, tenant_id, role FROM user_tenants WHERE user_id = $1
		 ORDER BY created_at ASC, tenant_id ASC`, userID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []domain.UserTenant
	for rows.Next() {
		var ut domain.UserTenant
		if err := rows.Scan(&ut.UserID, &ut.TenantID, &ut.Role); err != nil {
			return nil, err
		}
		out = append(out, ut)
	}
	return out, rows.Err()
}

func (s *PostgresStore) CreateResetToken(ctx context.Context, userID, tokenHash string, expiresAt time.Time) (domain.PasswordResetToken, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return domain.PasswordResetToken{}, err
	}
	defer postgres.Rollback(tx)

	var t domain.PasswordResetToken
	row := tx.QueryRowContext(ctx, `
		INSERT INTO password_reset_tokens (user_id, token_hash, expires_at)
		VALUES ($1, $2, $3)
		RETURNING id, user_id, token_hash, expires_at, used_at, created_at
	`, userID, tokenHash, expiresAt)
	err = row.Scan(&t.ID, &t.UserID, &t.TokenHash, &t.ExpiresAt, &t.UsedAt, &t.CreatedAt)
	if err != nil {
		return domain.PasswordResetToken{}, fmt.Errorf("create reset token: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return domain.PasswordResetToken{}, err
	}
	return t, nil
}

func (s *PostgresStore) ConsumeResetToken(ctx context.Context, tokenHash string) (string, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return "", err
	}
	defer postgres.Rollback(tx)

	// Single UPDATE-RETURNING: stamps used_at and returns user_id only
	// if the token row is currently un-used and un-expired. Atomic so
	// concurrent redeems both can't succeed.
	row := tx.QueryRowContext(ctx, `
		UPDATE password_reset_tokens
		SET used_at = now()
		WHERE token_hash = $1
		  AND used_at IS NULL
		  AND expires_at > now()
		RETURNING user_id
	`, tokenHash)
	var userID string
	if err := row.Scan(&userID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", errs.ErrNotFound
		}
		return "", err
	}

	// Void every OTHER outstanding token for this user, in the same tx.
	// Single-use per token was never enough: reset is requestable by
	// anyone who knows the email, so a user can hold several live tokens
	// at once. Redeeming one left the siblings redeemable for the rest of
	// their hour — meaning whoever obtained an earlier token (a since-
	// recovered mailbox, a forwarded link) could re-flip the password of
	// the account that authorizes charges and refunds, AFTER the rightful
	// owner had just reset it. Voiding here rather than after SetPassword
	// keeps the failure direction safe: if the hash write then fails, every
	// token is dead and the user must request a fresh one — no window is
	// left open by a partial failure. (OWASP: invalidate all outstanding
	// reset tokens on password change.)
	if _, err := tx.ExecContext(ctx, `
		UPDATE password_reset_tokens
		SET used_at = now()
		WHERE user_id = $1
		  AND token_hash <> $2
		  AND used_at IS NULL
	`, userID, tokenHash); err != nil {
		return "", err
	}

	if err := tx.Commit(); err != nil {
		return "", err
	}
	return userID, nil
}

// LookupResetToken is the read-only counterpart — same validity gate
// (un-used, un-expired) but no UPDATE. Used by the reset-password
// page on mount to decide between rendering the form vs. an "this
// link is no longer valid" message.
func (s *PostgresStore) LookupResetToken(ctx context.Context, tokenHash string) (string, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return "", err
	}
	defer postgres.Rollback(tx)
	var userID string
	row := tx.QueryRowContext(ctx, `
		SELECT user_id FROM password_reset_tokens
		WHERE token_hash = $1
		  AND used_at IS NULL
		  AND expires_at > now()
	`, tokenHash)
	if err := row.Scan(&userID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", errs.ErrNotFound
		}
		return "", err
	}
	return userID, nil
}
