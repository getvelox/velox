package user

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/session"
)

type denyAllLimiter struct{ keys []string }

func (d *denyAllLimiter) AllowKey(_ context.Context, key string) (int, time.Time, bool) {
	d.keys = append(d.keys, key)
	return 0, time.Now().Add(time.Hour), false
}

type countingResetSender struct{ calls int }

func (c *countingResetSender) SendPasswordReset(context.Context, string, string, string) error {
	c.calls++
	return nil
}

// TestRequestPasswordReset_SharedCapDeniesSilently is ha-14 PR-B: the
// per-address send cap lives in the SHARED limiter (Redis GCRA — one
// 3/hour budget across every replica, where the old per-process map
// degraded to 3×N/hour). Over the cap: the SAME generic 200 (the fixed
// response is the enumeration defence), no token issued, no email sent.
// The key is the normalized address. Mutation check: skip the limiter
// guard → the request proceeds into issuance and the send fires.
func TestRequestPasswordReset_SharedCapDeniesSilently(t *testing.T) {
	sender := &countingResetSender{}
	h := NewHandler(nil /* users: unreachable on the denied path */, nil,
		session.DefaultCookieConfig(), sender, "https://dash.example.test", true)
	deny := &denyAllLimiter{}
	h.SetResetSendLimiter(deny)

	req := httptest.NewRequest("POST", "/v1/auth/password-reset/request",
		strings.NewReader(`{"email":"  Victim@Example.TEST "}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.requestPasswordReset(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("throttled request must keep the generic 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "if that email is registered") {
		t.Fatalf("throttled response must be the fixed generic body, got: %s", rec.Body.String())
	}
	if sender.calls != 0 {
		t.Fatalf("send fired %d time(s) over the cap; want 0", sender.calls)
	}
	if len(deny.keys) != 1 || deny.keys[0] != "victim@example.test" {
		t.Fatalf("limiter key must be the normalized address, got %v", deny.keys)
	}
}
