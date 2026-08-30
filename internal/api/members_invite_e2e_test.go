package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// ADR-081 e2e over the REAL wiring: bootstrap → login → invite (session
// only; API keys 403) → token extracted from the enqueued outbox email →
// public preview → accept mints an account + session → members list →
// removal kills the removed member's session. This pins the seams the
// package-level dashmembers tests can't: chi route registration (the
// public accept routes vs the /v1/auth Mount("/") wildcard), the
// RequireSession gate, cookie minting on accept, and the env-driven
// DASHBOARD_BASE_URL plumbing through router construction.
func TestE2E_MemberInviteAcceptRemove(t *testing.T) {
	db := testutil.SetupTestDB(t)
	t.Setenv("VELOX_BOOTSTRAP_TOKEN", "invite-e2e-bootstrap-token")
	t.Setenv("DASHBOARD_BASE_URL", "http://dash.e2e.test")

	srv := NewServer(db, clock.NewFake(time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)), nil)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// Bootstrap the workspace + owner.
	resp := doPost(t, ts, "/v1/bootstrap", "", map[string]any{
		"token":          "invite-e2e-bootstrap-token",
		"tenant_name":    "Invite E2E Co",
		"owner_email":    "owner@invite-e2e.test",
		"owner_password": "a-real-12char-pw",
	})
	assertStatus(t, resp, 201)
	boot := readJSON(t, resp)
	secretKey, _ := boot["secret_key_test"].(string)

	ownerCookie := login(t, ts, "owner@invite-e2e.test", "a-real-12char-pw")

	// An API key cannot manage membership — machine credentials carry no
	// human actor.
	keyResp := doPost(t, ts, "/v1/members/invite", "Bearer "+secretKey, map[string]any{
		"email": "teammate@invite-e2e.test",
	})
	assertStatus(t, keyResp, 403)

	// The owner's session invites a teammate.
	inviteResp := postWithCookie(t, ts, "/v1/members/invite", ownerCookie, map[string]any{
		"email": "teammate@invite-e2e.test",
	})
	assertStatus(t, inviteResp, 201)
	inv := readJSON(t, inviteResp)
	if inv["status"] != "pending" {
		t.Fatalf("fresh invite status: %v", inv["status"])
	}

	// The accept link rides the email outbox; pull the raw token from the
	// enqueued row (production sends it via SMTP — the DB row is the same
	// payload the dispatcher would render).
	token := inviteTokenFromOutbox(t, db)
	if !strings.HasPrefix(token, "") || token == "" {
		t.Fatal("no invite token found in the email outbox")
	}

	// Public preview — no auth, token is the credential.
	prevResp := doGet(t, ts, "/v1/auth/invite/"+token, "")
	assertStatus(t, prevResp, 200)
	preview := readJSON(t, prevResp)
	if preview["needs_new_account"] != true {
		t.Fatalf("preview needs_new_account: %v", preview["needs_new_account"])
	}
	if preview["tenant_name"] != "Invite E2E Co" {
		t.Fatalf("preview tenant_name: %v", preview["tenant_name"])
	}

	// Accept: account created, session cookie minted.
	acceptResp := doPost(t, ts, "/v1/auth/accept-invite", "", map[string]any{
		"token":    token,
		"password": "another-12char-pw",
	})
	assertStatus(t, acceptResp, 200)
	accept := readJSON(t, acceptResp)
	if accept["session_minted"] != true {
		t.Fatalf("accept session_minted: %v", accept["session_minted"])
	}
	var memberCookie *http.Cookie
	for _, c := range acceptResp.Cookies() {
		if c.Name == "velox_session" && c.Value != "" {
			memberCookie = c
		}
	}
	if memberCookie == nil {
		t.Fatal("accept-invite did not set a velox_session cookie")
	}
	memberID, _ := accept["user_id"].(string)

	// The minted session works, and the members list shows both humans.
	listResp := getWithCookie(t, ts, "/v1/members/", memberCookie)
	assertStatus(t, listResp, 200)
	list := readJSON(t, listResp)
	if members, _ := list["members"].([]any); len(members) != 2 {
		t.Fatalf("member count: got %d, want 2", len(list["members"].([]any)))
	}

	// Replaying the consumed token fails generically.
	replay := doPost(t, ts, "/v1/auth/accept-invite", "", map[string]any{
		"token": token, "password": "another-12char-pw",
	})
	assertStatus(t, replay, 422)

	// The audit rows actually LAND. Regression pin: every auth-event
	// audit write (login/logout/mode/member.joined) silently failed on
	// the TxTenant livemode guard until 2026-07-06 — the handler logged
	// an error and moved on, so nothing user-visible broke while the
	// compliance trail stayed empty.
	assertAuditAction(t, db, "login")
	assertAuditAction(t, db, "member.invited")
	assertAuditAction(t, db, "member.joined")

	// Owner removes the teammate → 204, and the teammate's session dies
	// with the membership (sessions pin the tenant).
	delResp := deleteWithCookie(t, ts, "/v1/members/"+memberID, ownerCookie)
	assertStatus(t, delResp, 204)
	deadResp := getWithCookie(t, ts, "/v1/members/", memberCookie)
	assertStatus(t, deadResp, 401)
}

// login posts credentials and returns the velox_session cookie.
func login(t *testing.T, ts *httptest.Server, email, password string) *http.Cookie {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"email": email, "password": password})
	resp, err := http.Post(ts.URL+"/v1/auth/login", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login status: %d", resp.StatusCode)
	}
	for _, c := range resp.Cookies() {
		if c.Name == "velox_session" && c.Value != "" {
			return c
		}
	}
	t.Fatal("login set no velox_session cookie")
	return nil
}

func requestWithCookie(t *testing.T, ts *httptest.Server, method, path string, cookie *http.Cookie, body map[string]any) *http.Response {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, ts.URL+path, rdr)
	if err != nil {
		t.Fatalf("build %s %s: %v", method, path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

func postWithCookie(t *testing.T, ts *httptest.Server, path string, cookie *http.Cookie, body map[string]any) *http.Response {
	return requestWithCookie(t, ts, http.MethodPost, path, cookie, body)
}

func getWithCookie(t *testing.T, ts *httptest.Server, path string, cookie *http.Cookie) *http.Response {
	return requestWithCookie(t, ts, http.MethodGet, path, cookie, nil)
}

func deleteWithCookie(t *testing.T, ts *httptest.Server, path string, cookie *http.Cookie) *http.Response {
	return requestWithCookie(t, ts, http.MethodDelete, path, cookie, nil)
}

// assertAuditAction fails unless at least one audit_log row with the
// action exists (read under bypass — the assertion is "the row landed",
// not a mode-visibility check).
func assertAuditAction(t *testing.T, db *postgres.DB, action string) {
	t.Helper()
	ctx := t.Context()
	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer postgres.Rollback(tx)
	var n int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM audit_log WHERE action = $1`, action).Scan(&n); err != nil {
		t.Fatalf("count audit %s: %v", action, err)
	}
	if n == 0 {
		t.Errorf("no audit_log row for action %q — the auth-event audit write is failing again (livemode guard?)", action)
	}
}

// inviteTokenFromOutbox reads the newest member_invite outbox row and
// extracts the raw token from its accept URL.
func inviteTokenFromOutbox(t *testing.T, db *postgres.DB) string {
	t.Helper()
	ctx := t.Context()
	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer postgres.Rollback(tx)
	var payload []byte
	err = tx.QueryRowContext(ctx, `
		SELECT payload FROM email_outbox
		WHERE email_type = 'member_invite'
		ORDER BY created_at DESC LIMIT 1`).Scan(&payload)
	if err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	var msg struct {
		InviteURL string `json:"invite_url"`
	}
	if err := json.Unmarshal(payload, &msg); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	_, token, found := strings.Cut(msg.InviteURL, "token=")
	if !found {
		t.Fatalf("no token in invite URL %q", msg.InviteURL)
	}
	return token
}
