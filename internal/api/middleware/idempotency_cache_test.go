package middleware

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestIdempotency_Caches5xx is the regression test for COR-6: a transient 500
// retry must replay the cached 500 rather than re-invoke the handler. The
// previous impl cached only 2xx, which meant a retry could re-run side effects
// the first attempt had already committed but failed to confirm back to the
// client (classic "Stripe charged the card but the 200 timed out" scenario).
func TestIdempotency_Caches5xx(t *testing.T) {
	db := testutil.SetupTestDB(t)
	tenantID := testutil.CreateTestTenant(t, db, "Idempotency 5xx")

	var calls atomic.Int64
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, `{"error":"upstream timeout"}`, http.StatusInternalServerError)
	})
	handler := Idempotency(db, nil)(inner)

	// First call: handler runs, returns 500.
	rec1 := invokeWithKey(t, handler, tenantID, "idem-5xx", `{"amount":100}`)
	if rec1.Code != http.StatusInternalServerError {
		t.Fatalf("first call: got %d, want 500", rec1.Code)
	}
	if calls.Load() != 1 {
		t.Fatalf("first call: handler should have run once, got %d", calls.Load())
	}
	if rec1.Header().Get("Idempotent-Replayed") != "" {
		t.Fatal("first call: Idempotent-Replayed must not be set")
	}

	// Second call same key+body: must replay cached 500 without running handler.
	rec2 := invokeWithKey(t, handler, tenantID, "idem-5xx", `{"amount":100}`)
	if rec2.Code != http.StatusInternalServerError {
		t.Fatalf("replay: got %d, want 500", rec2.Code)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("replay: handler must not re-run, got calls=%d", got)
	}
	if rec2.Header().Get("Idempotent-Replayed") != "true" {
		t.Error("replay: Idempotent-Replayed=true must be set")
	}
	if rec2.Body.String() != rec1.Body.String() {
		t.Errorf("replay body mismatch:\n got: %s\nwant: %s", rec2.Body.String(), rec1.Body.String())
	}
}

// TestIdempotency_Caches4xx_ExceptConflictAndUnprocessable pins the nuance
// that 4xx responses ARE cached (to prevent retry-and-succeed after a real
// validation/authorization failure), but 409 and 422 specifically are NOT
// cached because they signal "this isn't the real first response": 409 from
// concurrent contention (retry may succeed after the conflict clears), 422
// typically from input validation (client is expected to fix the body, and
// our fingerprint check will catch the body change as a key-reuse error).
func TestIdempotency_Caches4xx_ExceptConflictAndUnprocessable(t *testing.T) {
	db := testutil.SetupTestDB(t)
	tenantID := testutil.CreateTestTenant(t, db, "Idempotency 4xx")

	cases := []struct {
		name       string
		status     int
		key        string
		wantCached bool
	}{
		{"400 bad request cached", http.StatusBadRequest, "idem-400", true},
		{"401 unauthorized cached", http.StatusUnauthorized, "idem-401", true},
		{"404 not found cached", http.StatusNotFound, "idem-404", true},
		{"409 conflict NOT cached", http.StatusConflict, "idem-409", false},
		{"422 unprocessable NOT cached", http.StatusUnprocessableEntity, "idem-422", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int64
			inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				http.Error(w, `{"error":"x"}`, tc.status)
			})
			handler := Idempotency(db, nil)(inner)

			_ = invokeWithKey(t, handler, tenantID, tc.key, `{}`)
			rec2 := invokeWithKey(t, handler, tenantID, tc.key, `{}`)

			switch {
			case tc.wantCached && calls.Load() != 1:
				t.Errorf("cached status %d: handler should have run once, got %d", tc.status, calls.Load())
			case tc.wantCached && rec2.Header().Get("Idempotent-Replayed") != "true":
				t.Errorf("cached status %d: replay header missing on second call", tc.status)
			case !tc.wantCached && calls.Load() != 2:
				t.Errorf("uncached status %d: handler should run twice, got %d", tc.status, calls.Load())
			case !tc.wantCached && rec2.Header().Get("Idempotent-Replayed") != "":
				t.Errorf("uncached status %d: replay header must not be set", tc.status)
			}
		})
	}
}

// TestIdempotency_FingerprintMismatchStill422 confirms COR-6's broader caching
// didn't compromise the fingerprint-mismatch contract: reusing a key with a
// different body must still return 422 idempotency_error (not a replay of the
// first response under the wrong parameters).
func TestIdempotency_FingerprintMismatchStill422(t *testing.T) {
	db := testutil.SetupTestDB(t)
	tenantID := testutil.CreateTestTenant(t, db, "Idempotency Fingerprint")

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"new"}`))
	})
	handler := Idempotency(db, nil)(inner)

	// First body: succeeds, cached.
	rec1 := invokeWithKey(t, handler, tenantID, "idem-fp", `{"amount":100}`)
	if rec1.Code != http.StatusCreated {
		t.Fatalf("first call: got %d, want 201", rec1.Code)
	}

	// Second call: same key, DIFFERENT body — must be 422 idempotency_error,
	// not a replay of the 201.
	rec2 := invokeWithKey(t, handler, tenantID, "idem-fp", `{"amount":200}`)
	if rec2.Code != http.StatusUnprocessableEntity {
		t.Fatalf("mismatch: got %d, want 422", rec2.Code)
	}
	if !strings.Contains(rec2.Body.String(), "idempotency_error") {
		t.Errorf("mismatch: expected idempotency_error in body, got: %s", rec2.Body.String())
	}
}

// TestIdempotency_ConcurrentSameKey_RunsOnce is the regression test for the
// check-then-act race: two concurrent requests with the same idempotency key
// must execute the side effect exactly once. Before the reserve-before-act
// fix, both requests passed the read (no row yet) and both invoked the handler
// — a double credit-grant / double-charge. With the fix, exactly one request
// claims the key and runs the handler; the other either replays the stored
// response or gets a 409 conflict_idempotency, but never re-executes.
func TestIdempotency_ConcurrentSameKey_RunsOnce(t *testing.T) {
	db := testutil.SetupTestDB(t)
	tenantID := testutil.CreateTestTenant(t, db, "Idempotency Concurrent")

	var calls atomic.Int64
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Count the side effect (the "credit grant"). Sleep briefly while
		// holding the claim so the racing request reliably reaches the
		// conflict path and starts polling — exercising the serialization.
		calls.Add(1)
		time.Sleep(150 * time.Millisecond)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"granted"}`))
	})
	handler := Idempotency(db, nil)(inner)

	const n = 2
	var (
		wg      sync.WaitGroup
		start   = make(chan struct{})
		codes   [n]int
		replays [n]string
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // release both goroutines as close to simultaneously as possible
			rec := invokeWithKey(t, handler, tenantID, "idem-concurrent", `{"amount":100}`)
			codes[i] = rec.Code
			replays[i] = rec.Header().Get("Idempotent-Replayed")
		}(i)
	}
	close(start)
	wg.Wait()

	// The side effect must have run exactly once.
	if got := calls.Load(); got != 1 {
		t.Fatalf("concurrent same-key: handler must run exactly once, got %d", got)
	}

	// Exactly one request executed (201, no replay header). The other must be
	// either a replay of that 201, or a 409 conflict_idempotency — never a
	// second fresh execution.
	executed, replayed, conflicted := 0, 0, 0
	for i := 0; i < n; i++ {
		switch {
		case codes[i] == http.StatusCreated && replays[i] == "":
			executed++
		case codes[i] == http.StatusCreated && replays[i] == "true":
			replayed++
		case codes[i] == http.StatusConflict:
			conflicted++
		default:
			t.Errorf("request %d: unexpected outcome code=%d replayed=%q", i, codes[i], replays[i])
		}
	}
	if executed != 1 {
		t.Errorf("exactly one request should have executed fresh, got %d (codes=%v replays=%v)", executed, codes, replays)
	}
	if replayed+conflicted != 1 {
		t.Errorf("the other request should replay or 409, got replayed=%d conflicted=%d (codes=%v)", replayed, conflicted, codes)
	}
}

func invokeWithKey(t *testing.T, h http.Handler, tenantID, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/v1/invoices", strings.NewReader(body))
	req.Header.Set("Idempotency-Key", key)
	req.Header.Set("Content-Type", "application/json")
	// Tenant + livemode pinned on ctx the same way the real auth
	// middleware does in production. Without WithLivemode, BeginTx
	// trips the strict-mode panic ("TxTenant opened without ctx
	// livemode") — that diagnostic is the contract every entry point
	// must satisfy, including test fixtures.
	ctx := context.WithValue(req.Context(), auth.TestTenantIDKey(), tenantID)
	ctx = postgres.WithLivemode(ctx, false)
	req = req.WithContext(ctx)
	req.Body = io.NopCloser(strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestIdempotency_OrphanedReservation_AnswersUnresolved is sweep-2026-08-30
// S2: a reservation whose owner died mid-request (status 0, older than any
// route budget) must not make every retry poll 5 s and hear "please retry"
// for 24 h. It answers at once with a distinct code that says the outcome is
// unknown and how to find out; the handler never runs (the side effect may
// already have committed). Mutation check: drop the age branch in
// replayExistingKey → 5 s poll + the generic conflict_idempotency → fails.
func TestIdempotency_OrphanedReservation_AnswersUnresolved(t *testing.T) {
	db := testutil.SetupTestDB(t)
	tenantID := testutil.CreateTestTenant(t, db, "Idempotency Orphan")

	// Same livemode as invokeWithKey's request (false): the row's livemode is
	// stamped by the trigger from the transaction's GUC.
	seedCtx := postgres.WithLivemode(context.Background(), false)
	tx, err := db.BeginTx(seedCtx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(seedCtx,
		`INSERT INTO idempotency_keys (key, tenant_id, http_method, http_path, request_fingerprint, status_code, response_body, created_at)
		 VALUES ('idem-orphan', $1, 'POST', '/v1/invoices', '\x'::bytea, 0, ''::bytea, now() - interval '10 minutes')`, tenantID); err != nil {
		t.Fatalf("seed orphaned reservation: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	var calls atomic.Int64
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusCreated)
	})
	handler := Idempotency(db, nil)(inner)

	start := time.Now()
	rec := invokeWithKey(t, handler, tenantID, "idem-orphan", `{"amount":100}`)
	took := time.Since(start)
	if rec.Code != http.StatusConflict {
		t.Fatalf("orphaned reservation: got %d, want 409", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "conflict_idempotency_unresolved") {
		t.Fatalf("orphaned reservation must answer conflict_idempotency_unresolved, got: %s", rec.Body.String())
	}
	if calls.Load() != 0 {
		t.Fatalf("handler must not re-run on an orphaned reservation (outcome unknown), ran %d times", calls.Load())
	}
	if took > 2*time.Second {
		t.Fatalf("the answer must be immediate, not the 5 s poll: took %s", took)
	}
}

// TestIdempotency_IngestServerError_ReleasedForRetry is HA work item 11: on
// routes the release predicate names (the dedup-safe ingest surface), a 5xx
// must release the reservation so the client's documented move — retry with
// the SAME key — re-executes the write instead of replaying the failure for
// the 24 h TTL. Everywhere else the 5xx stays cached (the Stripe-charge
// class), pinned by TestIdempotency_Caches5xx above. Mutation check:
// neutralize the release branch → the retry replays the 500.
func TestIdempotency_IngestServerError_ReleasedForRetry(t *testing.T) {
	db := testutil.SetupTestDB(t)
	tenantID := testutil.CreateTestTenant(t, db, "Idempotency Ingest 5xx")

	var calls atomic.Int64
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			http.Error(w, `{"error":"storage blip"}`, http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusCreated)
	})
	handler := Idempotency(db, func(*http.Request) bool { return true })(inner)

	rec1 := invokeWithKey(t, handler, tenantID, "idem-ingest-5xx", `{"events":[1]}`)
	if rec1.Code != http.StatusInternalServerError {
		t.Fatalf("first call: got %d, want 500", rec1.Code)
	}
	// The release runs on a background ctx after the response; poll briefly.
	deadline := time.Now().Add(2 * time.Second)
	rec2code := 0
	for {
		rec2 := invokeWithKey(t, handler, tenantID, "idem-ingest-5xx", `{"events":[1]}`)
		rec2code = rec2.Code
		if rec2code == http.StatusCreated || time.Now().After(deadline) {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if rec2code != http.StatusCreated {
		t.Fatalf("retry with the same key: got %d, want 201 (the pinned 500 must have been released and re-executed)", rec2code)
	}
	if calls.Load() < 2 {
		t.Fatalf("handler ran %d time(s), want >= 2 — the retry must re-execute", calls.Load())
	}
}
