package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestE2E_TestModeIsolation validates FEAT-8's core contract end-to-end: a
// test-mode API key and a live-mode API key, both belonging to the same
// tenant, see completely disjoint data. Customers, webhook endpoints, and
// test clocks created in one mode are invisible to the other. This is the
// guarantee a customer relies on to run synthetic load against their test
// keys without risk to the live production partition.
func TestE2E_TestModeIsolation(t *testing.T) {
	db := testutil.SetupTestDB(t)

	// Use a fake clock so created-at timestamps are deterministic.
	clk := clock.NewFake(time.Date(2026, 4, 20, 0, 0, 0, 0, time.UTC))
	srv := NewServer(db, clk, nil)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	tenantID := testutil.CreateTestTenant(t, db, "TestMode Isolation Corp")
	liveKey := createAPIKeyWithMode(t, db, tenantID, true)
	testKey := createAPIKeyWithMode(t, db, tenantID, false)

	liveAuth := "Bearer " + liveKey
	testAuth := "Bearer " + testKey

	// --- customers ---
	var liveCustID, testCustID string

	t.Run("create live customer", func(t *testing.T) {
		resp := doPost(t, ts, "/v1/customers", liveAuth, map[string]any{
			"external_id":  "cust_live",
			"display_name": "Live Customer",
			"email":        "live@example.com",
		})
		assertStatus(t, resp, 201)
		liveCustID = readJSON(t, resp)["id"].(string)
	})

	t.Run("create test customer", func(t *testing.T) {
		resp := doPost(t, ts, "/v1/customers", testAuth, map[string]any{
			"external_id":  "cust_test",
			"display_name": "Test Customer",
			"email":        "test@example.com",
		})
		assertStatus(t, resp, 201)
		testCustID = readJSON(t, resp)["id"].(string)
	})

	// Same tenant, different modes — IDs must be distinct.
	if liveCustID == testCustID {
		t.Fatal("live and test customer must not share ID")
	}

	t.Run("live key sees only live customer", func(t *testing.T) {
		resp := doGet(t, ts, "/v1/customers", liveAuth)
		assertStatus(t, resp, 200)
		ids := extractListIDs(t, resp)
		if len(ids) != 1 || ids[0] != liveCustID {
			t.Errorf("live key customer list: got %v, want [%s]", ids, liveCustID)
		}
	})

	t.Run("test key sees only test customer", func(t *testing.T) {
		resp := doGet(t, ts, "/v1/customers", testAuth)
		assertStatus(t, resp, 200)
		ids := extractListIDs(t, resp)
		if len(ids) != 1 || ids[0] != testCustID {
			t.Errorf("test key customer list: got %v, want [%s]", ids, testCustID)
		}
	})

	// Direct cross-key probes by ID. List endpoints are scoped by RLS, but
	// we also need to prove that a caller who *knows or guesses* the other
	// mode's resource ID cannot sidestep isolation with a fetch-by-id. These
	// must 404, not 200. If a future change loses the livemode predicate in
	// an RLS policy, this is the test that catches it.
	t.Run("test key probing live customer ID → 404", func(t *testing.T) {
		resp := doGet(t, ts, "/v1/customers/"+liveCustID, testAuth)
		assertStatus(t, resp, 404)
	})

	t.Run("live key probing test customer ID → 404", func(t *testing.T) {
		resp := doGet(t, ts, "/v1/customers/"+testCustID, liveAuth)
		assertStatus(t, resp, 404)
	})

	// --- webhook endpoints ---
	var liveEPID, testEPID string

	t.Run("create live webhook endpoint", func(t *testing.T) {
		resp := doPost(t, ts, "/v1/webhook-endpoints/endpoints", liveAuth, map[string]any{
			"url":    "http://localhost:9001/live",
			"events": []string{"*"},
		})
		assertStatus(t, resp, 201)
		ep := readJSON(t, resp)["endpoint"].(map[string]any)
		liveEPID = ep["id"].(string)
		if ep["livemode"] != true {
			t.Errorf("live endpoint livemode: got %v, want true", ep["livemode"])
		}
	})

	t.Run("create test webhook endpoint", func(t *testing.T) {
		resp := doPost(t, ts, "/v1/webhook-endpoints/endpoints", testAuth, map[string]any{
			"url":    "http://localhost:9002/test",
			"events": []string{"*"},
		})
		assertStatus(t, resp, 201)
		ep := readJSON(t, resp)["endpoint"].(map[string]any)
		testEPID = ep["id"].(string)
		if ep["livemode"] != false {
			t.Errorf("test endpoint livemode: got %v, want false", ep["livemode"])
		}
	})

	t.Run("live key sees only live endpoint", func(t *testing.T) {
		resp := doGet(t, ts, "/v1/webhook-endpoints/endpoints", liveAuth)
		assertStatus(t, resp, 200)
		ids := extractListIDs(t, resp)
		if len(ids) != 1 || ids[0] != liveEPID {
			t.Errorf("live key endpoint list: got %v, want [%s]", ids, liveEPID)
		}
	})

	t.Run("test key sees only test endpoint", func(t *testing.T) {
		resp := doGet(t, ts, "/v1/webhook-endpoints/endpoints", testAuth)
		assertStatus(t, resp, 200)
		ids := extractListIDs(t, resp)
		if len(ids) != 1 || ids[0] != testEPID {
			t.Errorf("test key endpoint list: got %v, want [%s]", ids, testEPID)
		}
	})

	// --- test clocks: write gated to secret + test mode (testclock handler
	//     also refuses live-mode secret keys with 403). ---
	t.Run("live key cannot create test clock (403)", func(t *testing.T) {
		resp := doPost(t, ts, "/v1/test-clocks", liveAuth, map[string]any{
			"name":        "should not be allowed",
			"frozen_time": "2026-01-01T00:00:00Z",
		})
		assertStatus(t, resp, 403)
	})

	var clockID string
	t.Run("test key creates test clock", func(t *testing.T) {
		resp := doPost(t, ts, "/v1/test-clocks", testAuth, map[string]any{
			"name":        "fy26",
			"frozen_time": "2026-01-01T00:00:00Z",
		})
		assertStatus(t, resp, 201)
		clk := readJSON(t, resp)
		clockID = clk["id"].(string)
		if clk["status"] != "ready" {
			t.Errorf("new clock status: got %v, want ready", clk["status"])
		}
	})

	t.Run("live key cannot see test clock", func(t *testing.T) {
		resp := doGet(t, ts, "/v1/test-clocks/"+clockID, liveAuth)
		// 403 from the requireTestMode gate — the clock is invisible to live.
		assertStatus(t, resp, 403)
	})
}

// createAPIKeyWithMode inserts an api_keys row with the Stripe-style mode
// infix (vlx_secret_live_… or vlx_secret_test_…) and the correct livemode
// column value for the trigger to preserve. Uses empty salt so the
// hash-verification path in ValidateKey equals sha256(rawKey).
func createAPIKeyWithMode(t *testing.T, db *postgres.DB, tenantID string, livemode bool) string {
	t.Helper()

	mode := "test"
	if livemode {
		mode = "live"
	}
	secret := make([]byte, 32)
	_, _ = rand.Read(secret)
	secretHex := hex.EncodeToString(secret)
	fullPrefix := "vlx_secret_" + mode + "_"
	rawKey := fullPrefix + secretHex
	dbPrefix := fullPrefix + secretHex[:12]
	hash := sha256.Sum256([]byte(rawKey))
	hashHex := hex.EncodeToString(hash[:])
	keyID := "vlx_key_e2e_" + secretHex[:8] + "_" + mode

	tx, err := db.BeginTx(context.Background(), postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	// The 0021 trigger reads app.livemode on INSERT; in TxBypass we set it
	// explicitly for the row being created. Without this, every bypass-mode
	// insert would land with livemode=true.
	sessionVal := "on"
	if !livemode {
		sessionVal = "off"
	}
	if _, err := tx.ExecContext(context.Background(),
		`SELECT set_config('app.livemode', $1, true)`, sessionVal); err != nil {
		_ = tx.Rollback()
		t.Fatalf("set app.livemode: %v", err)
	}
	if _, err := tx.ExecContext(context.Background(),
		`INSERT INTO api_keys (id, key_prefix, key_hash, key_type, name, tenant_id)
		 VALUES ($1, $2, $3, 'secret', $4, $5)`,
		keyID, dbPrefix, hashHex, "E2E "+mode+" key", tenantID); err != nil {
		_ = tx.Rollback()
		t.Fatalf("insert api key: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return rawKey
}

// extractListIDs pulls the "id" field from each element of a chi list
// response body. The current list handlers return a JSON object with "data"
// or "items" or "customers" arrays depending on the handler — the helper
// tries the common keys and also handles the bare-array shape.
func extractListIDs(t *testing.T, resp *http.Response) []string {
	t.Helper()
	var out []string
	body := readJSON(t, resp)
	list, ok := findListField(body)
	if !ok {
		t.Fatalf("list response has no recognisable array field: %v", body)
	}
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		id, _ := m["id"].(string)
		if id == "" {
			// webhook-endpoints nests the endpoint under "endpoint"
			if inner, ok := m["endpoint"].(map[string]any); ok {
				id, _ = inner["id"].(string)
			}
		}
		if id != "" {
			out = append(out, id)
		}
	}
	return out
}

func findListField(body map[string]any) ([]any, bool) {
	for _, key := range []string{"data", "items", "customers", "endpoints", "results"} {
		if v, ok := body[key].([]any); ok {
			return v, true
		}
	}
	return nil, false
}
