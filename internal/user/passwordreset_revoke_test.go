package user

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/session"
)

// fakeUserStore is a minimal in-memory Store that lets the reset flow
// run end-to-end through user.Service.ConsumeResetToken without a DB.
// Only the methods the reset path touches are non-trivial; the rest
// satisfy the interface.
type fakeUserStore struct {
	user            domain.User
	resetTokenHash  string // the single valid token hash
	consumed        bool
	setPasswordHash string
	// loginUser, when set, is returned by GetByEmail on a matching email so the
	// login path can run end-to-end; tenants is returned by TenantsForUser. Both
	// default to zero (GetByEmail misses, no tenants) — the value the existing
	// reset tests rely on.
	loginUser *domain.User
	tenants   []domain.UserTenant
	// createTokenErr, when set, is returned by CreateResetToken — the real
	// store returns ErrResetSendCapped there once the account is over its
	// per-window send budget.
	createTokenErr error
}

func (f *fakeUserStore) Create(ctx context.Context, email, passwordHash string) (domain.User, error) {
	return domain.User{}, errs.ErrNotFound
}
func (f *fakeUserStore) GetByEmail(ctx context.Context, email string) (domain.User, error) {
	if f.loginUser != nil && f.loginUser.Email == email {
		return *f.loginUser, nil
	}
	return domain.User{}, errs.ErrNotFound
}
func (f *fakeUserStore) GetByID(ctx context.Context, id string) (domain.User, error) {
	if id == f.user.ID {
		return f.user, nil
	}
	return domain.User{}, errs.ErrNotFound
}
func (f *fakeUserStore) TouchLastLogin(ctx context.Context, id string, at time.Time) error {
	return nil
}
func (f *fakeUserStore) SetPassword(ctx context.Context, id, passwordHash string) error {
	f.setPasswordHash = passwordHash
	return nil
}
func (f *fakeUserStore) AttachTenant(ctx context.Context, userID, tenantID, role string) error {
	return nil
}
func (f *fakeUserStore) TenantsForUser(ctx context.Context, userID string) ([]domain.UserTenant, error) {
	return f.tenants, nil
}
func (f *fakeUserStore) CreateResetToken(ctx context.Context, userID, tokenHash string, expiresAt time.Time) (domain.PasswordResetToken, error) {
	if f.createTokenErr != nil {
		return domain.PasswordResetToken{}, f.createTokenErr
	}
	return domain.PasswordResetToken{}, nil
}
func (f *fakeUserStore) ConsumeResetToken(ctx context.Context, tokenHash string) (string, error) {
	if f.consumed || tokenHash != f.resetTokenHash {
		return "", errs.ErrNotFound
	}
	f.consumed = true
	return f.user.ID, nil
}
func (f *fakeUserStore) LookupResetToken(ctx context.Context, tokenHash string) (string, error) {
	if f.consumed || tokenHash != f.resetTokenHash {
		return "", errs.ErrNotFound
	}
	return f.user.ID, nil
}

// recordingSessionStore satisfies session.Store and records the
// argument passed to RevokeAllForUser so the test can assert the
// reset flow actually fans out the revoke.
type recordingSessionStore struct {
	revokeAllForUser []string
}

func (r *recordingSessionStore) Insert(ctx context.Context, s session.Session) error { return nil }
func (r *recordingSessionStore) GetByIDHash(ctx context.Context, idHash string) (session.Session, error) {
	return session.Session{}, session.ErrNotFound
}
func (r *recordingSessionStore) Revoke(ctx context.Context, idHash string) error { return nil }
func (r *recordingSessionStore) RevokeAllForUser(ctx context.Context, userID string) error {
	r.revokeAllForUser = append(r.revokeAllForUser, userID)
	return nil
}
func (r *recordingSessionStore) UpdateLivemode(ctx context.Context, idHash string, livemode bool) error {
	return nil
}

// stubEmailSender satisfies the handler's EmailSender dependency; the
// reset-confirm path under test never calls it.
type stubEmailSender struct{}

func (stubEmailSender) SendPasswordReset(ctx context.Context, tenantID, email, resetLink string) error {
	return nil
}

// TestConfirmPasswordResetRevokesSessions is the regression guard for
// the defect where a successful password reset left existing sessions
// (including one minted from a stolen cookie) alive for the full TTL.
// Without the RevokeAllForUser fan-out wired into confirmPasswordReset
// this test fails: the session store records zero revoke-all calls.
func TestConfirmPasswordResetRevokesSessions(t *testing.T) {
	const (
		userID    = "usr_reset_target"
		plaintext = "reset-token-plaintext-0123456789abcdef"
	)

	userStore := &fakeUserStore{
		user:           domain.User{ID: userID, Email: "op@example.com"},
		resetTokenHash: hashResetToken(plaintext),
	}
	userSvc := NewService(userStore, clock.Real())

	sessStore := &recordingSessionStore{}
	sessSvc := session.NewService(sessStore)

	h := NewHandler(userSvc, sessSvc, session.DefaultCookieConfig(), stubEmailSender{}, "", false)

	body, _ := json.Marshal(confirmResetReq{
		Token:    plaintext,
		Password: "a-sufficiently-long-password",
	})
	req := httptest.NewRequest(http.MethodPost, "/password-reset/confirm", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()

	h.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 from confirmPasswordReset, got %d: %s", rec.Code, rec.Body.String())
	}
	if !userStore.consumed {
		t.Fatal("expected reset token to be consumed")
	}
	if got := len(sessStore.revokeAllForUser); got != 1 {
		t.Fatalf("expected RevokeAllForUser to be called exactly once, got %d call(s)", got)
	}
	if sessStore.revokeAllForUser[0] != userID {
		t.Fatalf("expected RevokeAllForUser(%q), got %q", userID, sessStore.revokeAllForUser[0])
	}
}
