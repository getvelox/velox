package user

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/session"
)

// countingResetEmail records sends so a test can prove the capped path sends
// nothing.
type countingResetEmail struct{ calls int }

func (c *countingResetEmail) SendPasswordReset(context.Context, string, string, string) error {
	c.calls++
	return nil
}

// TestRequestPasswordReset_CappedAccountIsSilentlySkipped: when the store
// reports the account is over its send budget, the endpoint sends nothing,
// declares the request accounted-for, and returns the SAME fixed generic 200
// it returns for a delivered link and for an address with no account — that
// fixedness is the enumeration defence.
//
// It also asserts the LOG LEVEL, which is the one thing the capped arm
// uniquely owns: hitting a 3-per-hour cap is routine and expected, so it logs
// WARN. Without the arm the error falls through to `slog.Error("password
// reset issue failed")` and every capped send looks like a system failure to
// anyone alerting on error rate. Mutation check: disable the
// ErrResetSendCapped arm → an ERROR record appears → this fails.
func TestRequestPasswordReset_CappedAccountIsSilentlySkipped(t *testing.T) {
	const email = "victim@example.com"
	target := domain.User{ID: "usr_capped", Email: email}
	userStore := &fakeUserStore{
		user:           target,
		loginUser:      &target,
		tenants:        []domain.UserTenant{{TenantID: "ten_1"}},
		createTokenErr: ErrResetSendCapped,
	}
	sender := &countingResetEmail{}
	h := NewHandler(NewService(userStore, clock.Real()), session.NewService(&recordingSessionStore{}),
		session.DefaultCookieConfig(), sender, "https://dash.example.test", true)

	// Capture logs for the level assertion; restore the default after.
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	body, _ := json.Marshal(map[string]string{"email": email})
	req := httptest.NewRequest(http.MethodPost, "/password-reset/request", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	// Audit-coverage state on the ctx, as the root middleware provides in
	// production: the capped arm must DECLARE the request accounted-for
	// (MarkSkip), or the coverage detector reports this mutating 2xx as a
	// mutation that lost its audit row.
	reqCtx := audit.WithRequestState(req.Context())
	req = req.WithContext(reqCtx)
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("capped request must keep the generic 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// The response must be byte-identical to the one an address with NO
	// account gets. This cap is keyed on the account, so only a real account
	// can be capped — any distinguishable reply would be a reliable
	// account-existence oracle (four requests and an attacker knows).
	unknownStore := &fakeUserStore{}
	unknownH := NewHandler(NewService(unknownStore, clock.Real()), session.NewService(&recordingSessionStore{}),
		session.DefaultCookieConfig(), &countingResetEmail{}, "https://dash.example.test", true)
	unknownBody, _ := json.Marshal(map[string]string{"email": "nobody@example.com"})
	unknownReq := httptest.NewRequest(http.MethodPost, "/password-reset/request", strings.NewReader(string(unknownBody)))
	unknownReq.Header.Set("Content-Type", "application/json")
	unknownReq = unknownReq.WithContext(audit.WithRequestState(unknownReq.Context()))
	unknownRec := httptest.NewRecorder()
	unknownH.Routes().ServeHTTP(unknownRec, unknownReq)

	if rec.Code != unknownRec.Code || rec.Body.String() != unknownRec.Body.String() {
		t.Fatalf("capped reply is distinguishable from the no-account reply — that is an account-existence oracle\n capped:  %d %s\n unknown: %d %s",
			rec.Code, rec.Body.String(), unknownRec.Code, unknownRec.Body.String())
	}
	if sender.calls != 0 {
		t.Fatalf("capped account: %d email(s) sent, want 0", sender.calls)
	}
	var sawWarn, sawError bool
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if line == "" {
			continue
		}
		var rec struct {
			Level string `json:"level"`
			Msg   string `json:"msg"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if rec.Level == "ERROR" {
			sawError = true
			t.Logf("unexpected ERROR record: %s", rec.Msg)
		}
		if rec.Level == "WARN" && strings.Contains(rec.Msg, "throttled") {
			sawWarn = true
		}
	}
	if sawError {
		t.Error("a routine per-account cap must not log at ERROR — it would read as a system failure to anyone alerting on error rate")
	}
	if !sawWarn {
		t.Error("a capped send must leave a WARN record naming the throttle")
	}
	if !audit.WasHandled(reqCtx) {
		t.Fatal("the capped path must MarkSkip — otherwise the audit-coverage detector reads this 2xx as a mutation that lost its row")
	}
}
