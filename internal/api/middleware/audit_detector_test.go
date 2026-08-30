package middleware

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-chi/chi/v5"
	promtest "github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// uncoveredCount reads the counter for one route label.
func uncoveredCount(t *testing.T, route string) float64 {
	t.Helper()
	return promtest.ToFloat64(auditUncoveredMutations.WithLabelValues(route))
}

// detectorRouter builds a router shaped like the real one: the detector at the
// ROOT, routes underneath in a mounted subtree (so RoutePattern() resolution is
// exercised through a real nested Mount, not a flat router).
func detectorRouter(exempt map[string]bool) chi.Router {
	r := chi.NewRouter()
	r.Use(AuditCoverage(func(method, route string) bool {
		return exempt[method+" "+route]
	}))

	v1 := chi.NewRouter()

	// NOTE: the COVERED path (a route whose real audit emission self-marks the
	// request) cannot be faked here — nothing outside package audit can set the
	// "a row landed" mark, by design: the deleted MarkHandled was the exported
	// escape hatch that let a handler ASSERT coverage it didn't have. It is pinned
	// against real Postgres by TestAuditCoverage_RealEmissionIsSilent below.

	// A mutating route that commits and emits NOTHING — the bug the detector exists
	// to find (and the case the old catch-all papered over with a guessed row).
	v1.Post("/silent", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	})

	// 204: a delete that emits nothing. Pins that "2xx" is not "200".
	v1.Delete("/silent-204/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	// A declared-exempt route (machine ingest) that emits nothing, on purpose.
	v1.Post("/usage-events", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})

	// A route whose mutation FAILED: no row is correct, and the detector must not
	// report it.
	v1.Post("/failing", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})

	// A no-op branch of a mutating route: 2xx, nothing mutated, nothing to audit.
	v1.Post("/noop", func(w http.ResponseWriter, r *http.Request) {
		audit.MarkSkip(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	// Read-only.
	v1.Get("/customers", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	r.Mount("/v1", v1)
	return r
}

func TestAuditCoverage_ReportsOnlyUncoveredMutations(t *testing.T) {
	exempt := map[string]bool{"POST /v1/usage-events": true}

	tests := []struct {
		name       string
		method     string
		path       string
		route      string // the route-pattern label the counter uses
		wantStatus int
		wantReport bool
	}{
		{
			name:       "mutating 2xx with no emission and no exemption → REPORTED",
			method:     "POST",
			path:       "/v1/silent",
			route:      "/v1/silent",
			wantStatus: http.StatusCreated,
			wantReport: true,
		},
		{
			name:       "204 counts as 2xx — a silent delete is still an uncovered mutation",
			method:     "DELETE",
			path:       "/v1/silent-204/vlx_cus_123",
			route:      "/v1/silent-204/{id}", // pattern, never the raw path (cardinality)
			wantStatus: http.StatusNoContent,
			wantReport: true,
		},
		{
			name:       "exempt route → silent",
			method:     "POST",
			path:       "/v1/usage-events",
			route:      "/v1/usage-events",
			wantStatus: http.StatusAccepted,
			wantReport: false,
		},
		{
			name:       "non-2xx (the mutation failed) → silent",
			method:     "POST",
			path:       "/v1/failing",
			route:      "/v1/failing",
			wantStatus: http.StatusInternalServerError,
			wantReport: false,
		},
		{
			name:       "handler declared it mutated nothing (MarkSkip) → silent",
			method:     "POST",
			path:       "/v1/noop",
			route:      "/v1/noop",
			wantStatus: http.StatusOK,
			wantReport: false,
		},
		{
			name:       "GET is not a mutation → silent",
			method:     "GET",
			path:       "/v1/customers",
			route:      "/v1/customers",
			wantStatus: http.StatusOK,
			wantReport: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			router := detectorRouter(exempt)
			before := uncoveredCount(t, tc.route)

			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))

			if rec.Code != tc.wantStatus {
				t.Fatalf("status: got %d, want %d", rec.Code, tc.wantStatus)
			}

			delta := uncoveredCount(t, tc.route) - before
			switch {
			case tc.wantReport && delta != 1:
				t.Errorf("velox_audit_uncovered_mutation_total{route=%q}: got +%v, want +1 — an uncovered mutation went unreported", tc.route, delta)
			case !tc.wantReport && delta != 0:
				t.Errorf("velox_audit_uncovered_mutation_total{route=%q}: got +%v, want +0 — the detector cried wolf", tc.route, delta)
			}
		})
	}
}

// TestAuditCoverage_NeverMutatesTheResponse is the invariant that separates this
// middleware from the one it replaces. The catch-all's fail-closed ancestor
// swapped a committed mutation's response for a 503, which the Idempotency layer
// then cached for 24h (ADR-089). An observer observes.
func TestAuditCoverage_NeverMutatesTheResponse(t *testing.T) {
	body := `{"id":"vlx_inv_1","invoice_number":"INV-0001"}`

	handler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Custom", "kept")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, body)
	}

	// Same handler, with and without the detector — byte-identical results, on the
	// UNCOVERED path (where the detector does the most work).
	bare := httptest.NewRecorder()
	http.HandlerFunc(handler).ServeHTTP(bare, httptest.NewRequest("POST", "/v1/silent", nil))

	observed := httptest.NewRecorder()
	r := chi.NewRouter()
	r.Use(AuditCoverage(func(string, string) bool { return false }))
	r.Post("/v1/silent", handler)
	r.ServeHTTP(observed, httptest.NewRequest("POST", "/v1/silent", nil))

	if observed.Code != bare.Code {
		t.Errorf("status: got %d, want %d (unchanged)", observed.Code, bare.Code)
	}
	if got, want := observed.Body.String(), bare.Body.String(); got != want {
		t.Errorf("body: got %q, want %q (byte-identical)", got, want)
	}
	if got := observed.Header().Get("X-Custom"); got != "kept" {
		t.Errorf("X-Custom header: got %q, want %q", got, "kept")
	}
	if got := observed.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type: got %q, want %q", got, "application/json")
	}
}

// TestAuditCoverage_PreservesStreaming pins the property the deleted buffer broke.
//
// bufferedResponse implemented only Header/Write/WriteHeader — NOT http.Flusher.
// internal/api/exports.go does `if f, ok := w.(http.Flusher); ok { f.Flush() }`,
// so on /v1/exports that assertion FAILED: every CSV export silently accumulated
// in memory and never streamed — on a route given a 5-minute timeout precisely
// BECAUSE it streams. chi's WrapResponseWriter preserves Flusher through a typed
// wrapper, so the detector can sit anywhere without breaking a stream.
func TestAuditCoverage_PreservesStreaming(t *testing.T) {
	flushed := 0

	r := chi.NewRouter()
	r.Use(AuditCoverage(func(string, string) bool { return false }))
	r.Get("/v1/exports/usage-events", func(w http.ResponseWriter, req *http.Request) {
		f, ok := w.(http.Flusher)
		if !ok {
			t.Error("http.Flusher assertion FAILED through the detector — the export would buffer in memory instead of streaming (the exact bug the old bufferedResponse shipped)")
			return
		}
		w.Header().Set("Content-Type", "text/csv")
		for _, chunk := range []string{"id,amount\n", "vlx_ue_1,10\n", "vlx_ue_2,20\n"} {
			_, _ = io.WriteString(w, chunk)
			f.Flush()
			flushed++
		}
	})

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/exports/usage-events", nil))

	if flushed != 3 {
		t.Errorf("flushes: got %d, want 3", flushed)
	}
	if want := "id,amount\nvlx_ue_1,10\nvlx_ue_2,20\n"; rec.Body.String() != want {
		t.Errorf("streamed body: got %q, want %q", rec.Body.String(), want)
	}
	if !rec.Flushed {
		t.Error("recorder never saw a flush — the response was buffered somewhere in the chain")
	}
}

// TestAuditCoverage_SeedsRequestStateOnce guards the shadowing trap. The detector
// seeds the audit request-state at the root; if any middleware BELOW it seeded a
// fresh cell, downstream marks would land on the inner cell while the detector
// read the outer — and every covered route would be reported as uncovered. The
// contract that prevents it is WithRequestState's idempotency, pinned here with a
// second seeder between the detector and the handler.
func TestAuditCoverage_SeedsRequestStateOnce(t *testing.T) {
	r := chi.NewRouter()
	r.Use(AuditCoverage(func(string, string) bool { return false }))

	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(audit.WithRequestState(req.Context())))
		})
	})

	r.Post("/v1/credits/grant", func(w http.ResponseWriter, req *http.Request) {
		audit.MarkSkip(req.Context()) // marks whatever cell the handler can see
		w.WriteHeader(http.StatusCreated)
	})

	before := uncoveredCount(t, "/v1/credits/grant")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/credits/grant", nil))

	if delta := uncoveredCount(t, "/v1/credits/grant") - before; delta != 0 {
		t.Errorf("a re-seeded request state shadowed the detector's cell: got +%v reports for an accounted-for route, want +0", delta)
	}
}

// TestAuditCoverage_RealEmissionIsSilent pins the COVERED path with a real audit
// row, against real Postgres: a handler that emits through audit.Logger (the
// self-mark inside Log/LogInTx is the ONLY thing that can claim coverage now) must
// leave the detector silent. This is the case the unit table cannot fake, and the
// one that would break loudest if the self-mark were ever removed — every emitting
// route in the product would start reporting as an uncovered mutation.
func TestAuditCoverage_RealEmissionIsSilent(t *testing.T) {
	db := testutil.SetupTestDB(t)
	tenantID := testutil.CreateTestTenant(t, db, "Audit Detector Real Emission")
	logger := audit.NewLogger(db)

	r := chi.NewRouter()
	r.Use(AuditCoverage(func(string, string) bool { return false }))
	r.Post("/v1/invoices", func(w http.ResponseWriter, req *http.Request) {
		if err := logger.Log(req.Context(), tenantID, domain.AuditActionCreate,
			"invoice", "vlx_inv_real", "INV-0001", nil); err != nil {
			t.Errorf("emit: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
	})

	before := uncoveredCount(t, "/v1/invoices")
	rec := invokeWithKeylessTenant(t, r, tenantID)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status: got %d, want 201", rec.Code)
	}

	if delta := uncoveredCount(t, "/v1/invoices") - before; delta != 0 {
		t.Errorf("a route with a REAL audit row was reported as an uncovered mutation: got +%v, want +0 — the emission's self-mark is not reaching the detector's cell", delta)
	}
	if n := auditRowCount(t, db, tenantID); n != 1 {
		t.Errorf("audit rows: got %d, want 1", n)
	}
}

// invokeWithKeylessTenant drives a POST with the tenant + livemode ctx the auth
// middleware would have stamped, and no idempotency key.
func invokeWithKeylessTenant(t *testing.T, h http.Handler, tenantID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/v1/invoices", strings.NewReader(`{}`))
	ctx := context.WithValue(req.Context(), auth.TestTenantIDKey(), tenantID)
	ctx = postgres.WithLivemode(ctx, false)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req.WithContext(ctx))
	return rec
}

// auditRowCount counts this tenant's audit rows — the append-only record itself,
// not a test double.
func auditRowCount(t *testing.T, db *postgres.DB, tenantID string) int {
	t.Helper()
	ctx := postgres.WithLivemode(context.Background(), false)
	rows, _, err := audit.NewLogger(db).Query(ctx, tenantID, audit.QueryFilter{})
	if err != nil {
		t.Fatalf("query audit log: %v", err)
	}
	return len(rows)
}

// TestAuditCoverage_IdempotentReplayIsNotAnUncoveredMutation drives the REAL
// idempotency middleware (real Postgres, real cached replay) underneath the
// detector.
//
// The catch-all this detector replaces was mounted INSIDE the idempotency layer,
// so it never saw a replay. The detector mounts at the ROOT — it wraps it. A
// replay hands back the FIRST request's response without running the handler, so
// no audit emission happens on the replay: without the MarkSkip that
// replayExistingKey now makes, every idempotent retry of a successful mutation
// would be reported as an uncovered mutation (and a detector that cries wolf on
// a normal, correct client retry is a detector nobody keeps).
//
// The other half of the invariant is just as important: a replay must NOT write a
// second audit row either — it would record a mutation that never happened, in an
// append-only log.
func TestAuditCoverage_IdempotentReplayIsNotAnUncoveredMutation(t *testing.T) {
	db := testutil.SetupTestDB(t)
	tenantID := testutil.CreateTestTenant(t, db, "Audit Detector Replay")
	logger := audit.NewLogger(db)

	var handlerCalls atomic.Int64

	r := chi.NewRouter()
	r.Use(AuditCoverage(func(string, string) bool { return false }))
	r.Use(Idempotency(db, nil))
	r.Post("/v1/invoices", func(w http.ResponseWriter, req *http.Request) {
		handlerCalls.Add(1)
		// A REAL audit row — the emission self-marks the request (Logger.Log).
		if err := logger.Log(req.Context(), tenantID, domain.AuditActionCreate,
			"invoice", "vlx_inv_1", "INV-0001", nil); err != nil {
			t.Errorf("emit: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"vlx_inv_1"}`)
	})

	before := uncoveredCount(t, "/v1/invoices")

	rec1 := invokeWithKey(t, r, tenantID, "idem-audit-replay", `{"amount":100}`)
	if rec1.Code != http.StatusCreated {
		t.Fatalf("first call: got %d, want 201", rec1.Code)
	}

	rec2 := invokeWithKey(t, r, tenantID, "idem-audit-replay", `{"amount":100}`)
	if rec2.Code != http.StatusCreated {
		t.Fatalf("replay: got %d, want 201", rec2.Code)
	}
	if rec2.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatal("replay: expected the cached response, got a fresh handler run — the test isn't exercising a replay")
	}

	if got := handlerCalls.Load(); got != 1 {
		t.Fatalf("handler ran %d times, want 1 (the replay must not re-execute the mutation)", got)
	}
	// The append-only log itself is the assertion: one mutation, one row.
	if n := auditRowCount(t, db, tenantID); n != 1 {
		t.Errorf("audit rows after the original + its replay: got %d, want 1 — a replay must not record a mutation that never happened", n)
	}
	if delta := uncoveredCount(t, "/v1/invoices") - before; delta != 0 {
		t.Errorf("velox_audit_uncovered_mutation_total{route=\"/v1/invoices\"}: got +%v, want +0 — the detector reported an idempotent replay as an uncovered mutation", delta)
	}
}

// TestAuditCoverage_UnmatchedPathIsNotSilentlyExempt pins the canonicalization
// negative from the API side: a 404 inside a mounted subtree resolves to
// RoutePattern() "/v1/usage-events/*". If canonicalRoute trimmed the trailing
// "/*", that would collapse onto the exempt "/v1/usage-events" key and hand a
// free pass to anything unmatched under an exempt subtree.
func TestAuditCoverage_UnmatchedPathIsNotSilentlyExempt(t *testing.T) {
	seen := ""
	r := chi.NewRouter()
	r.Use(AuditCoverage(func(_, route string) bool {
		seen = route
		return false
	}))

	sub := chi.NewRouter()
	sub.Post("/", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusAccepted) })
	// Anything else under the subtree 404s, but the subtree still matched.
	r.Mount("/v1/usage-events", sub)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/usage-events/nope", bytes.NewReader(nil)))

	if rec.Code == http.StatusAccepted {
		t.Fatalf("expected the unmatched sub-path to 404, got %d", rec.Code)
	}
	// A 404 is not a 2xx, so nothing is reported and isExempt is never consulted —
	// which is the point: the detector cannot be tricked into treating an
	// unmatched path as a covered (or exempt) route.
	if seen != "" {
		t.Errorf("isExempt consulted for a non-2xx request (route %q) — the detector should only classify successful mutations", seen)
	}
}
