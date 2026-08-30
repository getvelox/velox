package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestReadiness_DrainingIs503LivenessStays200 pins the readiness-first
// drain (2026-08-30): on SIGTERM main.go calls SetDraining and keeps
// listening for SHUTDOWN_DRAIN_DELAY, so a health-check-driven load
// balancer stops routing to this replica BEFORE the listener closes —
// otherwise every rolling deploy ended with connection resets on the
// draining replica. Liveness must stay 200 throughout, or an orchestrator
// would kill the replica mid-drain. Mutation-verify: delete the draining
// branch in handleDeepHealth → the 503 assertion fails.
func TestReadiness_DrainingIs503LivenessStays200(t *testing.T) {
	db := testutil.SetupTestDB(t)
	srv := NewServer(db, nil, nil)
	t.Cleanup(resetDrainingForTest)

	get := func(path string) (int, map[string]any) {
		t.Helper()
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return rec.Code, body
	}

	if code, body := get("/health/ready"); code != http.StatusOK || body["status"] != "ok" {
		t.Fatalf("before drain: /health/ready = %d %v, want 200 ok", code, body)
	}
	if code, _ := get("/health"); code != http.StatusOK {
		t.Fatalf("before drain: /health = %d, want 200", code)
	}

	SetDraining()

	code, body := get("/health/ready")
	if code != http.StatusServiceUnavailable || body["status"] != "draining" {
		t.Fatalf("draining: /health/ready = %d %v, want 503 draining", code, body)
	}
	if checks, _ := body["checks"].(map[string]any); checks["api"] != "draining" {
		t.Fatalf("draining: checks.api = %v, want draining", checks["api"])
	}
	if code, _ := get("/health"); code != http.StatusOK {
		t.Fatalf("draining: /health = %d, want 200 (liveness must not fail during drain)", code)
	}
}
