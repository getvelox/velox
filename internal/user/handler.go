package user

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/sagarsuperuser/velox/internal/api/respond"
	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/session"
)

// SessionService is the narrow seam the handler needs from *session.Service.
//
// It exists so the FAILURE paths can be tested. logout used to write its audit row
// BEFORE revoking, log a revoke error to stderr, and return 204 regardless — a
// permanent append-only row asserting a logout that never happened, and a live
// session behind a browser told it was signed out. That survived an end-to-end
// audit because nothing could inject a failing Revoke: the dependency was a
// concrete *session.Service. A guarantee you cannot write a failing test for is a
// guarantee you do not have.
type SessionService interface {
	Issue(ctx context.Context, in session.IssueInput) (string, session.Session, error)
	Resolve(ctx context.Context, rawID string) (session.Session, error)
	Revoke(ctx context.Context, rawID string) error
	RevokeAllForUser(ctx context.Context, userID string) error
	SetLivemode(ctx context.Context, rawID string, livemode bool) error
}

// Handler wires the dashboard auth endpoints. ADR-011.
//
// Routes (mounted under /v1/auth):
//
//	POST /login                     — email + password → mint cookie
//	POST /logout                    — clear cookie + revoke session
//	POST /password-reset/request    — email → send reset link
//	POST /password-reset/confirm    — token + new password → set
type Handler struct {
	users            *Service
	sessions         SessionService
	cookie           session.CookieConfig
	email            EmailSender // required — production always wires the adapter
	dashboardBaseURL string      // canonical dashboard origin for reset links; never from request headers. empty => reset emails disabled
	smtpConfigured   bool        // SMTP wired at boot (email.Sender.IsConfigured); drives the email_delivery hint
	auditLogger      AuditRecorder
}

// AuditRecorder is the narrow audit surface the auth handler needs — kept here
// (not an import of *audit.Logger) so internal/user stays decoupled. Production
// wires *audit.Logger via SetAuditLogger in router.go. Optional: nil = the
// handler skips audit writes (login/logout/reset still work; they just leave no
// row), which is the safe default for the unit tests that build a bare handler.
type AuditRecorder interface {
	Log(ctx context.Context, tenantID, action, resourceType, resourceID, resourceLabel string, metadata map[string]any) error
}

// SetAuditLogger wires the audit recorder used to log authenticated auth events
// (login, logout, mode change, password reset). Without it those events are not
// audited. Failed logins are NOT routed here — they're pre-auth (no tenant to
// scope a per-tenant audit row, and surfacing email-existence in the log would
// be an enumeration oracle), so they go to the structured security log instead.
func (h *Handler) SetAuditLogger(a AuditRecorder) { h.auditLogger = a }

// auditAuthEvent writes one audit row for an authenticated auth event. It
// stamps the actor (the operator's user id, since these endpoints run outside
// session middleware so the ctx carries no identity) and the client IP (the
// TrustedRealIP-corrected r.RemoteAddr) onto the ctx the recorder reads. When
// actorUserID is "" the actor stays unresolved → recorded as 'system' (used for
// a password-reset REQUEST, which any unauthenticated party can trigger).
// Best-effort: a write failure is logged, never surfaced — auth must not fail
// because the audit row didn't land.
//
// livemode stamps the row's RLS mode partition. These endpoints run OUTSIDE session
// middleware, so the ctx carries no livemode and the audit writer's TxTenant would
// refuse to open (every auth audit row silently failed this way until 2026-07-06).
//
// Auth events are ACCOUNT-PLANE — a login is a login, not a "test-mode login" — so
// they are ALL filed in the canonical (test) partition. They used to inherit the
// session's mode, which meant the same event landed in a different partition
// depending on which way the operator's Test/Live toggle happened to be pointing,
// and "when did they last sign in?" had no reliable answer.
//
// Accepted loss, stated rather than glossed: an operator viewing the audit log in
// LIVE mode does not see these rows. See the note in tenant/settings.go for why the
// obvious fix (an OR arm on livemode) is the one thing we must not do.
func (h *Handler) auditAuthEvent(ctx context.Context, r *http.Request, livemode bool, actorUserID, tenantID, action, resourceID, label string, meta map[string]any) {
	if h.auditLogger == nil || tenantID == "" {
		return
	}
	if actorUserID != "" {
		ctx = auth.WithUserID(ctx, actorUserID)
	}
	ctx = audit.WithClientIP(ctx, audit.ExtractClientIP(r))
	ctx = postgres.WithLivemode(ctx, false) // account-plane: canonical partition
	if err := h.auditLogger.Log(ctx, tenantID, action, "user", resourceID, label, meta); err != nil {
		slog.ErrorContext(ctx, "audit: auth event write failed", "action", action, "tenant_id", tenantID, "error", err)
	}
}

// EmailSender is the narrow surface this handler uses to dispatch
// password-reset emails. Satisfied in production by an adapter over
// internal/email's Sender / OutboxSender. SMTP misconfiguration
// surfaces as a SendPasswordReset error logged here; the response to
// the user stays generic to prevent enumeration.
type EmailSender interface {
	SendPasswordReset(ctx context.Context, tenantID, email, resetLink string) error
}

// NewHandler wires the dependencies. emailSender is required —
// password-reset requests will nil-panic if it's nil. dashboardBaseURL is
// the canonical dashboard origin used to build password-reset links; it is
// never derived from the request (Host header) to prevent host-header
// poisoning / token theft. When empty, password-reset emails are not sent
// (requestPasswordReset fails safe). Set it to your dashboard URL — in
// split-origin dev (Vite on :5173 vs API on :8080) that's the SPA URL.
func NewHandler(users *Service, sessions SessionService, cookie session.CookieConfig, emailSender EmailSender, dashboardBaseURL string, smtpConfigured bool) *Handler {
	return &Handler{
		users:            users,
		sessions:         sessions,
		cookie:           cookie,
		email:            emailSender,
		dashboardBaseURL: strings.TrimRight(strings.TrimSpace(dashboardBaseURL), "/"),
		smtpConfigured:   smtpConfigured,
	}
}

// Routes returns the dashboard auth surface. Mount under /v1/auth.
// All routes are intentionally outside session middleware: they're
// either pre-session (login, password-reset) or take the cookie via
// r.Cookie directly (logout, mode).
func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Post("/login", h.login)
	r.Post("/logout", h.logout)
	r.Post("/mode", h.setMode)
	r.Post("/password-reset/request", h.requestPasswordReset)
	r.Post("/password-reset/confirm", h.confirmPasswordReset)
	// GET /password-reset/check?token=… — non-consuming validity probe
	// for the reset-password page on mount. 200 = renderable form, 422
	// = link is invalid/expired/used. Idempotent so it's a GET.
	r.Get("/password-reset/check", h.checkPasswordResetToken)
	return r
}

type loginReq struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type loginResp struct {
	UserID    string `json:"user_id"`
	TenantID  string `json:"tenant_id"`
	Email     string `json:"email"`
	Livemode  bool   `json:"livemode"`
	ExpiresAt string `json:"expires_at"`
}

// login authenticates the operator's email + password and mints a
// session cookie. Failures collapse into a single 401 with a
// constant-time bcrypt check on the not-found path so we don't leak
// account existence via response timing.
func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	var req loginReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respond.BadRequest(w, r, "invalid JSON body")
		return
	}
	if req.Email == "" {
		respond.ValidationField(w, r, "email", "email is required")
		return
	}
	if req.Password == "" {
		respond.ValidationField(w, r, "password", "password is required")
		return
	}

	ip := audit.ExtractClientIP(r)

	u, tenants, err := h.users.Authenticate(r.Context(), req.Email, req.Password)
	if err != nil {
		if errors.Is(err, ErrBadCredentials) {
			// Failed logins are a SOC-2 CC6.1 signal (brute force / credential
			// stuffing). They can't go to the per-tenant audit_log — there's no
			// resolved tenant pre-auth, and recording email-existence there would
			// be an enumeration oracle — so they land in the structured security
			// log (the right home for pre-auth events).
			slog.WarnContext(r.Context(), "auth: failed login attempt",
				"email", req.Email, "ip", ip, "reason", "bad_credentials")
			respond.Unauthorized(w, r, "invalid email or password")
			return
		}
		respond.FromError(w, r, err, "user")
		return
	}

	// V1: each user belongs to exactly one tenant. The Authenticate
	// call already errored out on zero tenants. Pick the first.
	tenant := tenants[0]

	rawID, sess, err := h.sessions.Issue(r.Context(), session.IssueInput{
		UserID:    u.ID,
		TenantID:  tenant.TenantID,
		Livemode:  false, // dashboard sessions default to test mode; bearer for live
		UserAgent: r.UserAgent(),
		IP:        session.ClientIP(r),
	})
	if err != nil {
		slog.Error("session: issue failed", "err", err, "user_id", u.ID)
		respond.InternalError(w, r)
		return
	}

	h.cookie.SetCookie(w, rawID, sess.ExpiresAt)
	// No email in the label: resource_id IS the user id, and the reader resolves the
	// address from the (erasable) users row. audit_log is append-only — an email
	// written here could never be deleted.
	h.auditAuthEvent(r.Context(), r, sess.Livemode, u.ID, tenant.TenantID, "login", u.ID, "",
		map[string]any{"livemode": sess.Livemode})
	respond.JSON(w, r, http.StatusOK, loginResp{
		UserID:    u.ID,
		TenantID:  tenant.TenantID,
		Email:     u.Email,
		Livemode:  sess.Livemode,
		ExpiresAt: sess.ExpiresAt.Format("2006-01-02T15:04:05Z07:00"),
	})
}

type setModeReq struct {
	Livemode bool `json:"livemode"`
}

// setMode flips the active mode (test/live) on the cookie session.
// Same operator switches between modes without re-authenticating.
// Returns 401 if the cookie is missing or the session is gone.
func (h *Handler) setMode(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie(session.CookieName)
	if err != nil || c.Value == "" {
		respond.Unauthorized(w, r, "missing session — sign in at /login")
		return
	}

	var req setModeReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respond.BadRequest(w, r, "invalid JSON body")
		return
	}

	// Resolve BEFORE the mutation. The audit row needs the session's identity,
	// and resolving afterwards made the emission conditional on a second
	// lookup succeeding — so a resolve failure left the mode SWITCHED with no
	// record of who switched it. An unresolvable session is a 401 anyway.
	sess, rerr := h.sessions.Resolve(r.Context(), c.Value)
	if rerr != nil {
		respond.Unauthorized(w, r, "invalid or expired session")
		return
	}

	if err := h.sessions.SetLivemode(r.Context(), c.Value, req.Livemode); err != nil {
		if errors.Is(err, session.ErrNotFound) {
			respond.Unauthorized(w, r, "invalid or expired session")
			return
		}
		slog.Error("session: set livemode failed", "err", err)
		respond.InternalError(w, r)
		return
	}

	// Stamped with the NEW mode — the row lands in the partition the
	// operator just switched into, alongside the actions that follow.
	h.auditAuthEvent(r.Context(), r, req.Livemode, sess.UserID, sess.TenantID, "mode_changed", sess.UserID, "",
		map[string]any{"livemode": req.Livemode})

	respond.JSON(w, r, http.StatusOK, map[string]any{
		"livemode": req.Livemode,
	})
}

// logout revokes the session row matching the cookie and clears the
// cookie on the response. Idempotent — missing cookie is a no-op.
// logout revokes the session, THEN records it.
//
// The order is the whole point. This used to write the audit row first, revoke
// second, log a revoke failure to stderr, and return 204 either way. So a failed
// revoke produced: a browser told it was logged out, a session still LIVE on the
// server, and a permanent append-only row asserting a logout that never happened.
// Two bugs wearing one coat — a false compliance record, and a silent
// security-relevant failure on the one action a user takes when they no longer
// trust the machine they are on.
//
// Now: resolve (identity is unreadable after revoke), revoke, and emit ONLY if the
// revoke actually succeeded. A revoke that fails is a 500 — the caller must not be
// told they are logged out when they are not.
func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie(session.CookieName)
	if err != nil || c.Value == "" {
		// No cookie: this logout revoked no session, so there is no server-side
		// mutation to audit. Say so explicitly. To an observer at the transport a
		// 204 with no audit row is otherwise indistinguishable from a real logout
		// that LOST its row, and the audit-coverage detector would (correctly, on
		// the evidence available to it) report it as an uncovered mutation.
		audit.MarkSkip(r.Context())
		// Deliberately NO ClearCookie here. "No cookie arrived" does not mean the
		// browser holds none — SameSite=Lax withholds it from every cross-site
		// request, so this branch is exactly what an attacker's cross-site POST
		// lands in. Clearing here emitted Set-Cookie: velox_session=; Max-Age=0,
		// which the browser honours: any website could force-logout any operator
		// (drive-by, via an auto-submitting form) while the session it could not
		// see stayed LIVE server-side for its full TTL — an orphaned credential
		// and a repeatable nuisance-DoS. Lax already blocks the real revoke; this
		// branch must not mutate client state for a request it cannot authenticate.
		// When a cookie genuinely is absent, clearing it was a no-op anyway.
		// The real sign-out clears below, on the far side of a revoke that worked.
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Resolve BEFORE revoke: after it, the token no longer names anyone, and an
	// audit row with no actor is barely a row at all.
	sess, rerr := h.sessions.Resolve(r.Context(), c.Value)

	if revokeErr := h.sessions.Revoke(r.Context(), c.Value); revokeErr != nil {
		// The session is still live. Clearing the cookie here would hand the user a
		// 204 and a cleared browser while the token they were trying to kill keeps
		// working server-side — the exact failure the user was defending against.
		slog.ErrorContext(r.Context(), "session revoke failed; logout NOT performed",
			"error", revokeErr)
		respond.InternalError(w, r)
		return
	}

	if rerr != nil {
		// A stale or already-expired cookie: the revoke was a no-op on a session
		// that was not there. Nothing mutated, nothing to record.
		audit.MarkSkip(r.Context())
	} else {
		// The row is written only now, on the far side of a revoke that WORKED.
		h.auditAuthEvent(r.Context(), r, sess.Livemode, sess.UserID, sess.TenantID, "logout", sess.UserID, "", nil)
	}

	h.cookie.ClearCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

type requestResetReq struct {
	Email string `json:"email"`
}

// requestPasswordReset issues a reset token (if the email matches a
// user) and emails the operator a reset link. Always returns 200 with
// a generic message — never confirms whether the email matched a
// user, to avoid account enumeration.
func (h *Handler) requestPasswordReset(w http.ResponseWriter, r *http.Request) {
	var req requestResetReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respond.BadRequest(w, r, "invalid JSON body")
		return
	}
	if req.Email == "" {
		respond.ValidationField(w, r, "email", "email is required")
		return
	}

	plaintext, tenantID, targetUserID, err := h.users.IssueResetToken(r.Context(), req.Email)
	switch {
	case errors.Is(err, ErrResetSendCapped):
		// Per-account send cap (store-enforced): no token was issued, so no
		// email goes out. Deliberately NO early return and no bespoke body —
		// it falls through to the one shared response every outcome uses.
		//
		// That matters more here than it did for the old per-address budget:
		// this cap is keyed on the ACCOUNT, so an address with no account can
		// never be capped. A distinguishable "you're throttled" reply would
		// therefore be a reliable account-existence oracle — four requests
		// and an attacker knows. WARN, not ERROR: hitting a 3-per-hour cap is
		// routine, and error-level noise here would read as a system failure.
		slog.Warn("password reset throttled — send skipped", "reason", "per-account cap")
	case err != nil:
		slog.Error("password reset issue failed", "err", err)
		// Generic response — don't expose internal failures
	}

	if plaintext != "" && tenantID != "" {
		// Send is best-effort: failure is logged but never surfaces to
		// the response, since the response shape is fixed at "if your
		// email is on file, you'll get a link" to avoid email
		// enumeration. SMTP misconfiguration shows up as a logged
		// SendPasswordReset error.
		resetLink, ok := h.buildResetLink(plaintext)
		if !ok {
			// DASHBOARD_BASE_URL is unset. Refuse to derive the link origin
			// from the request Host header — a poisoned Host would email the
			// victim a reset link pointing at an attacker domain, leaking the
			// token. Fail safe: don't send a poisonable link. Operator must
			// set DASHBOARD_BASE_URL to enable password-reset emails.
			slog.Error("password reset email not sent: DASHBOARD_BASE_URL is unset; refusing to build a reset link from request headers", "tenant_id", tenantID)
		} else {
			// This handler runs pre-auth (the /v1/auth group has no session
			// middleware), so ctx carries no livemode. The reset email is
			// account-plane, and the production email path is the outbox, whose
			// TxTenant refuses to open without a livemode — so without this the
			// send failed and the operator never got the link. Same gap the audit
			// write below already handles (auditAuthEvent sets it too). Canonical
			// (test) partition, like every account-plane row.
			sendCtx := postgres.WithLivemode(r.Context(), false)
			if sendErr := h.email.SendPasswordReset(sendCtx, tenantID, req.Email, resetLink); sendErr != nil {
				slog.Error("password reset email send failed", "err", sendErr)
			}
		}
		// Audit the reset request against the matched account's tenant. The actor is
		// anonymous (any unauthenticated party can trigger a reset email) so it
		// records as 'system'. Only written on a match, so it neither leaks to the
		// requester (the response is identical match-or-not) nor pollutes the log
		// with spray attempts.
		//
		// The row POINTS AT the account (resource_id = the user id) and does not
		// STORE its address — neither in the label nor in metadata, where it used
		// to sit twice. audit_log is append-only, so an email written here could
		// never be erased; the reader resolves it from the users row, which can.
		h.auditAuthEvent(r.Context(), r, false, "", tenantID, "password_reset_requested", targetUserID, "", nil)
	} else {
		// No account matched (or issuance failed): no token exists, nothing was
		// mutated, and deliberately nothing is written — a row here would turn
		// the audit log into the account-existence oracle the fixed 200 exists to
		// deny. Declared so the coverage detector reads "nothing to audit" rather
		// than "a mutation lost its row".
		audit.MarkSkip(r.Context())
	}

	// Whether reset emails can actually be DELIVERED on this deployment —
	// server-global (SMTP wired AND DASHBOARD_BASE_URL set), computed
	// independently of whether the email matched a user, so it leaks no
	// account existence (it's deployment posture, not account state). Lets the
	// UI tell a self-hoster their email isn't configured rather than promising
	// a link that can never arrive.
	emailDelivery := "ok"
	if h.dashboardBaseURL == "" || !h.smtpConfigured {
		emailDelivery = "not_configured"
	}
	respond.JSON(w, r, http.StatusOK, map[string]string{
		"message":        "if an account exists for that email, a password-reset link has been sent",
		"email_delivery": emailDelivery,
	})
}

type confirmResetReq struct {
	Token    string `json:"token"`
	Password string `json:"password"`
}

// checkPasswordResetToken probes a reset token's validity without
// consuming it. 200 with {"valid": true} when the token is currently
// usable; 422 with the same shape (valid: false) when it's expired,
// already used, or unknown. The reset-password page hits this on
// mount so it can render "this link is no longer valid" instead of
// a form the user fills in only to be rejected at submit.
func (h *Handler) checkPasswordResetToken(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if err := h.users.CheckResetToken(r.Context(), token); err != nil {
		respond.JSON(w, r, http.StatusUnprocessableEntity, map[string]any{
			"valid":  false,
			"reason": "invalid_or_expired",
		})
		return
	}
	respond.JSON(w, r, http.StatusOK, map[string]any{"valid": true})
}

// confirmPasswordReset validates the reset token, applies the new
// password, and revokes every active session for the user (forces a
// fresh login). Revoking matters for the threat the reset is most
// often run against: a thief who already has a live velox_session
// cookie. Without the fan-out revoke the stolen cookie would ride out
// its 7-day TTL even after the operator resets. Surface the
// password-validation error inline (e.g. "must be at least 12
// characters") so the dashboard can highlight the field; collapse
// token failures into a single 422.
func (h *Handler) confirmPasswordReset(w http.ResponseWriter, r *http.Request) {
	var req confirmResetReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respond.BadRequest(w, r, "invalid JSON body")
		return
	}
	if req.Token == "" {
		respond.ValidationField(w, r, "token", "token is required")
		return
	}
	if req.Password == "" {
		respond.ValidationField(w, r, "password", "password is required")
		return
	}

	u, err := h.users.ConsumeResetToken(r.Context(), req.Token, req.Password)
	if err != nil {
		// errs.Invalid (password validation) and errs.NotFound (token
		// invalid/expired/used) both map cleanly; let respond.FromError
		// do the routing.
		respond.FromError(w, r, err, "password_reset")
		return
	}

	// Revoke any sessions minted before the reset — including one a
	// thief opened from a stolen cookie, which is exactly the case the
	// operator is resetting to shut down. Failure here is logged but
	// not surfaced: the password is already changed, so the reset
	// succeeded; we don't want a transient session-store error to make
	// the operator think it didn't.
	if revokeErr := h.sessions.RevokeAllForUser(r.Context(), u.ID); revokeErr != nil {
		slog.Error("session: revoke-all-for-user after password reset failed",
			"user_id", u.ID, "err", revokeErr)
	}

	// Audit the completed reset — a password change + all-session revocation is
	// a high-value account-takeover signal. Actor is the account owner (they
	// held the token); tenant resolved separately since domain.User carries none.
	if tenantID, terr := h.users.TenantForUser(r.Context(), u.ID); terr == nil {
		// resource_id = the user id; the reader resolves the address. Not stored.
		h.auditAuthEvent(r.Context(), r, false, u.ID, tenantID, "password_reset_completed", u.ID, "", nil)
	}

	respond.JSON(w, r, http.StatusOK, map[string]string{
		"message": "password updated — sign in with your new password",
	})
}

// buildResetLink constructs the operator-facing URL for the reset
// confirmation page from the configured DASHBOARD_BASE_URL. It never
// derives the origin from the request (Host header, X-Forwarded-Proto):
// a password-reset link is a security-token carrier, and a poisoned Host
// header would let an attacker email the victim a link pointing at their
// own domain to capture the token. Returns ok=false when
// DASHBOARD_BASE_URL is unset so the caller can fail safe rather than
// send a poisonable link. Single-origin prod deployments still set
// DASHBOARD_BASE_URL to their canonical dashboard URL.
func (h *Handler) buildResetLink(token string) (string, bool) {
	if h.dashboardBaseURL == "" {
		return "", false
	}
	return h.dashboardBaseURL + "/reset-password?token=" + token, true
}
