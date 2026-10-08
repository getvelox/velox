package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestE2E_ManualFinalize_CollectsThroughEngine pins the router wiring behind
// ADR-116: the invoice handler's collector is the billing engine. A $0 manual
// invoice finalized through the REAL router must come back paid in the same
// response. If the router stops wiring the collector, finalize still succeeds
// but the invoice comes back finalized/pending and waits for the hourly sweep,
// which this test catches.
func TestE2E_ManualFinalize_CollectsThroughEngine(t *testing.T) {
	db := testutil.SetupTestDB(t)
	srv := NewServer(db, clock.NewFake(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)), nil)
	tenantID := testutil.CreateTestTenant(t, db, "Finalize Collect Corp")
	apiKey := createTestAPIKey(t, db, tenantID)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	call := func(method, path, body string, want int) map[string]any {
		t.Helper()
		req, err := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatalf("build %s %s: %v", method, path, err)
		}
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer func() { _ = resp.Body.Close() }()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		if resp.StatusCode != want {
			t.Fatalf("%s %s: status %d, want %d, body=%v", method, path, resp.StatusCode, want, out)
		}
		return out
	}

	cust := call(http.MethodPost, "/v1/customers", `{"external_id":"cus_fin_zero","display_name":"Zero"}`, http.StatusCreated)
	inv := call(http.MethodPost, "/v1/invoices", `{"customer_id":"`+cust["id"].(string)+`"}`, http.StatusCreated)
	id, _ := inv["id"].(string)
	if id == "" {
		if nested, ok := inv["invoice"].(map[string]any); ok {
			id, _ = nested["id"].(string)
		}
	}
	if id == "" {
		t.Fatalf("no invoice id in create response: %v", inv)
	}

	out := call(http.MethodPost, "/v1/invoices/"+id+"/finalize", ``, http.StatusOK)
	if out["status"] != "paid" {
		t.Errorf("finalize response status = %v, want paid (a $0 invoice settles through the engine's collector)", out["status"])
	}
}
