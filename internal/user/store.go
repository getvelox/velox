package user

import (
	"context"
	"errors"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// Store is the persistence boundary for user accounts, the user↔tenant
// join, and password reset tokens. ADR-011.
// ErrResetSendCapped means this account has already been sent
// ResetSendsPerWindow reset links inside ResetSendWindow, so no token was
// issued. It is not an error the caller surfaces: the endpoint's response is
// fixed either way (the fixed body is the account-enumeration defence).
var ErrResetSendCapped = errors.New("user: password-reset send cap reached for this account")

const (
	// ResetSendsPerWindow / ResetSendWindow bound how many reset EMAILS one
	// account can be sent. The per-IP limiter on /v1/auth slows credential
	// stuffing but does nothing against one caller pointing many requests at
	// a single victim's address, each inside the IP budget — flooding their
	// inbox and burning SMTP quota.
	//
	// The budget is derived from password_reset_tokens itself: the rows ARE
	// the record of what was sent, so the cap is cluster-wide by
	// construction, survives restarts, and needs no Redis. It replaced a
	// Redis bucket (ha-14 PR-B) that silently vanished wherever Redis was
	// unreachable — verified 2026-08-31: 8 consecutive sends, 0 refusals —
	// and before that an in-process map that degraded to 3xN per hour at N
	// replicas. Keyed on the ACCOUNT rather than the typed string, so it
	// counts what was actually delivered.
	ResetSendsPerWindow = 3
	ResetSendWindow     = time.Hour
)

type Store interface {
	// Create inserts a user. Returns ErrEmailTaken if the email is
	// already in the table (citext unique violation).
	Create(ctx context.Context, email, passwordHash string) (domain.User, error)

	// GetByEmail loads the user with the given email (case-insensitive
	// via citext). Returns errs.ErrNotFound when no row matches.
	GetByEmail(ctx context.Context, email string) (domain.User, error)

	// GetByID loads the user by id.
	GetByID(ctx context.Context, id string) (domain.User, error)

	// TouchLastLogin updates last_login_at on successful login.
	TouchLastLogin(ctx context.Context, id string, at time.Time) error

	// SetPassword updates the password_hash. Used by the reset flow.
	SetPassword(ctx context.Context, id, passwordHash string) error

	// AttachTenant adds (user_id, tenant_id, role) to user_tenants.
	// Idempotent on (user_id, tenant_id) primary key conflict.
	AttachTenant(ctx context.Context, userID, tenantID, role string) error

	// TenantsForUser returns the tenant memberships for a user. v1 has
	// 1:1 user:tenant; the shape supports growth.
	TenantsForUser(ctx context.Context, userID string) ([]domain.UserTenant, error)

	// CreateResetToken enforces the per-account send cap and inserts a row
	// whose token_hash matches the
	// caller-provided hash. Plaintext token never enters the DB.
	CreateResetToken(ctx context.Context, userID, tokenHash string, expiresAt time.Time) (domain.PasswordResetToken, error)
	// Over the cap it returns ErrResetSendCapped and writes nothing — the
	// caller sends no email and answers with the same fixed generic body.

	// ConsumeResetToken atomically looks up the token by hash, asserts
	// it isn't used or expired, and stamps used_at. Returns the token's
	// owning user_id.
	ConsumeResetToken(ctx context.Context, tokenHash string) (string, error)

	// LookupResetToken is the non-consuming counterpart of
	// ConsumeResetToken — returns the owning user_id if the token is
	// currently valid (not used, not expired) without flipping
	// used_at. Backs the page-mount validity check on the reset-
	// password screen so the operator who clicks an already-used
	// email link gets "this link is no longer valid" instead of a
	// form that rejects them at submit time.
	LookupResetToken(ctx context.Context, tokenHash string) (string, error)
}
