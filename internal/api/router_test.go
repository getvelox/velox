package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

func newTestServer() *Server {
	// Create server with nil DB — handlers that don't touch DB will work,
	// auth middleware will reject (no valid keys), which is what we want to test.
	db := &postgres.DB{}
	return NewServer(db, nil, nil)
}

func TestHealthEndpoint(t *testing.T) {
	srv := newTestServer()

	req := httptest.NewRequest("GET", "/health", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status: got %d, want 200", rec.Code)
	}

	var body map[string]string
	_ = json.NewDecoder(rec.Body).Decode(&body)
	if body["status"] != "ok" {
		t.Errorf("body: got %v", body)
	}
}

func TestAuthRequired_Customers(t *testing.T) {
	srv := newTestServer()

	req := httptest.NewRequest("GET", "/v1/customers", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status: got %d, want 401", rec.Code)
	}

	var body map[string]any
	_ = json.NewDecoder(rec.Body).Decode(&body)
	errObj, _ := body["error"].(map[string]any)
	if errObj == nil || errObj["type"] != "authentication_error" {
		t.Errorf("error type: got %v, want authentication_error", body["error"])
	}
}

func TestAuthRequired_AllProtectedRoutes(t *testing.T) {
	srv := newTestServer()

	routes := []struct {
		method string
		path   string
	}{
		{"GET", "/v1/customers"},
		{"POST", "/v1/customers"},
		{"GET", "/v1/meters"},
		{"GET", "/v1/plans"},
		{"GET", "/v1/rating-rules"},
		{"GET", "/v1/subscriptions"},
		{"GET", "/v1/usage-events"},
		{"GET", "/v1/invoices"},
		{"GET", "/v1/api-keys"},
		{"GET", "/v1/dunning/policy"},
		{"GET", "/v1/credits/balance/cus_123"},
	}

	for _, r := range routes {
		t.Run(r.method+" "+r.path, func(t *testing.T) {
			req := httptest.NewRequest(r.method, r.path, nil)
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Errorf("%s %s: got %d, want 401", r.method, r.path, rec.Code)
			}
		})
	}
}

func TestWebhookEndpoint_NoAuthRequired(t *testing.T) {
	srv := newTestServer()

	// Webhook endpoint should not require API key auth
	// (it uses Stripe signature verification instead)
	body := `{"id":"evt_test","type":"payment_intent.succeeded","created":1234567890,"data":{"object":{"id":"pi_test","object":"payment_intent","metadata":{}}}}`
	req := httptest.NewRequest("POST", "/v1/webhooks/stripe", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	// Should return 200 (processed or skipped), not 401
	if rec.Code == http.StatusUnauthorized {
		t.Error("webhook endpoint should not require API key auth")
	}
}

func TestNotFound(t *testing.T) {
	srv := newTestServer()

	req := httptest.NewRequest("GET", "/v1/nonexistent", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	// chi returns 404 for unmatched routes or 401 for auth-protected prefix
	if rec.Code != http.StatusNotFound && rec.Code != http.StatusUnauthorized {
		t.Errorf("status: got %d, want 404 or 401", rec.Code)
	}
}

func TestRateLimitHeaders(t *testing.T) {
	srv := newTestServer()

	// Rate limiting runs after auth — unauthenticated requests get 401 before rate limiter
	req := httptest.NewRequest("GET", "/v1/customers", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	// 401 responses should NOT have rate limit headers (rejected before rate limiter runs)
	if rec.Header().Get("X-RateLimit-Limit") != "" {
		t.Error("X-RateLimit-Limit should not be set on unauthenticated requests")
	}
}

func TestContentTypeJSON(t *testing.T) {
	srv := newTestServer()

	req := httptest.NewRequest("GET", "/health", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	ct := rec.Header().Get("Content-Type")
	if !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type: got %q, want application/json", ct)
	}
}
