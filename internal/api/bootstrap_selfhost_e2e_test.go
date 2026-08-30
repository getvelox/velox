package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// P7 / ADR-073 e2e: the REAL wiring (router deps → tenant.RunBootstrap →
// user.HashPassword + CreateInTx) walks the exact self-host path the
// docs describe: POST /v1/bootstrap → dashboard login with the returned
// credentials → API call with the returned LIVE key. This is the
// dead-end HIGH the audit flagged: the old HTTP bootstrap minted no
// owner user and no live key, so an HTTP-bootstrapped install could
// never log in to the dashboard nor reach live mode.
func TestE2E_BootstrapToLoginToLiveKey(t *testing.T) {
	db := testutil.SetupTestDB(t)
	t.Setenv("VELOX_BOOTSTRAP_TOKEN", "p7-e2e-bootstrap-token")

	srv := NewServer(db, clock.NewFake(time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)), nil)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// 1. Bootstrap with operator-chosen credentials.
	resp := doPost(t, ts, "/v1/bootstrap", "", map[string]any{
		"token":          "p7-e2e-bootstrap-token",
		"tenant_name":    "SelfHost Co",
		"owner_email":    "owner@selfhost.test",
		"owner_password": "a-real-12char-pw",
	})
	assertStatus(t, resp, 201)
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("bootstrap Cache-Control: %q, want no-store", cc)
	}
	body := readJSON(t, resp)
	liveKey, _ := body["secret_key_live"].(string)
	testKey, _ := body["secret_key_test"].(string)
	if liveKey == "" || testKey == "" {
		t.Fatalf("bootstrap response missing keys: %v", body)
	}

	// 2. Dashboard login with the credentials the bootstrap returned.
	loginBody, _ := json.Marshal(map[string]any{
		"email":    "owner@selfhost.test",
		"password": "a-real-12char-pw",
	})
	loginResp, err := http.Post(ts.URL+"/v1/auth/login", "application/json", bytes.NewReader(loginBody))
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	defer func() { _ = loginResp.Body.Close() }()
	if loginResp.StatusCode != http.StatusOK {
		t.Fatalf("login with bootstrap credentials: got %d — the dashboard dead-end is back", loginResp.StatusCode)
	}
	gotSession := false
	for _, c := range loginResp.Cookies() {
		if c.Name == "velox_session" && c.Value != "" {
			gotSession = true
		}
	}
	if !gotSession {
		t.Error("login response did not set a velox_session cookie")
	}

	// 3. The LIVE key authenticates against the API.
	liveResp := doGet(t, ts, "/v1/customers", "Bearer "+liveKey)
	assertStatus(t, liveResp, 200)
	// And the test key too (mode partitioning is covered elsewhere).
	testResp := doGet(t, ts, "/v1/customers", "Bearer "+testKey)
	assertStatus(t, testResp, 200)

	// 4. A replayed bootstrap is a uniform 409 — even with the valid token.
	replay := doPost(t, ts, "/v1/bootstrap", "", map[string]any{
		"token": "p7-e2e-bootstrap-token",
	})
	assertStatus(t, replay, 409)
}
