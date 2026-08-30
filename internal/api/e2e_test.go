package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestE2E_FullBillingCycle tests the complete billing flow via HTTP:
//
//	create pricing → create customer → create subscription →
//	ingest usage → trigger billing → verify invoice → download PDF → grant credits
//
// Key design decisions:
//   - Billing period is set to a past window so the billing engine considers it due (arrears billing)
//   - Usage events are timestamped WITHIN the billing period so they're aggregated correctly
//   - Uses the public API contract: external_customer_id + event_name for usage events
func TestE2E_FullBillingCycle(t *testing.T) {
	db := testutil.SetupTestDB(t)

	// Use a fake clock so we control time without SQL backdating hacks.
	// Start at March 1 — subscriptions created "now" get a March billing period.
	march1 := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	clk := clock.NewFake(march1)

	srv := NewServer(db, clk, nil)

	tenantID := testutil.CreateTestTenant(t, db, "E2E Test Corp")
	apiKey := createTestAPIKey(t, db, tenantID)

	ts := httptest.NewServer(srv)
	defer ts.Close()

	auth := "Bearer " + apiKey

	// Usage timestamp mid-period (clock says March so events are within period)
	usageTimestamp := time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC)
	periodStart := march1
	periodEnd := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)

	// 1. Health check
	t.Run("health", func(t *testing.T) {
		resp := doGet(t, ts, "/health", "")
		assertStatus(t, resp, 200)
		body := readJSON(t, resp)
		if body["status"] != "ok" {
			t.Errorf("health: got %v", body)
		}
		if resp.Header.Get("Velox-Version") == "" {
			t.Error("missing Velox-Version header")
		}
	})

	// 2. Create rating rule (flat $10 per unit)
	var ruleID string
	t.Run("create rating rule", func(t *testing.T) {
		resp := doPost(t, ts, "/v1/rating-rules", auth, map[string]any{
			"rule_key":          "api_calls",
			"name":              "API Call Pricing",
			"mode":              "flat",
			"currency":          "USD",
			"flat_amount_cents": 1000,
		})
		assertStatus(t, resp, 201)
		body := readJSON(t, resp)
		ruleID = body["id"].(string)
	})

	// 3. Create meter linked to the rating rule
	var meterID string
	t.Run("create meter", func(t *testing.T) {
		resp := doPost(t, ts, "/v1/meters", auth, map[string]any{
			"key":                    "api_calls",
			"name":                   "API Calls",
			"unit":                   "calls",
			"aggregation":            "sum",
			"rating_rule_version_id": ruleID,
		})
		assertStatus(t, resp, 201)
		body := readJSON(t, resp)
		meterID = body["id"].(string)
	})

	// 4. Create plan with base fee + meter
	var planID string
	t.Run("create plan", func(t *testing.T) {
		resp := doPost(t, ts, "/v1/plans", auth, map[string]any{
			"code":              "starter",
			"name":              "Starter Plan",
			"currency":          "USD",
			"billing_interval":  "monthly",
			"base_amount_cents": 2900,
			"meter_ids":         []string{meterID},
		})
		assertStatus(t, resp, 201)
		body := readJSON(t, resp)
		planID = body["id"].(string)
	})

	// 5. Create customer
	var customerID string
	t.Run("create customer", func(t *testing.T) {
		resp := doPost(t, ts, "/v1/customers", auth, map[string]any{
			"external_id":  "e2e_cust",
			"display_name": "E2E Test Customer",
			"email":        "test@e2e.com",
		})
		assertStatus(t, resp, 201)
		body := readJSON(t, resp)
		customerID = body["id"].(string)
	})

	// 6. Create subscription — clock says March 1, so billing period is March 1 - April 1
	t.Run("create subscription", func(t *testing.T) {
		resp := doPost(t, ts, "/v1/subscriptions", auth, map[string]any{
			"code":         "e2e-sub",
			"display_name": "E2E Subscription",
			"customer_id":  customerID,
			"items": []map[string]any{
				{"plan_id": planID, "quantity": 1},
			},
			"start_now": true,
		})
		assertStatus(t, resp, 201)
		body := readJSON(t, resp)
		if body["id"] == nil || body["id"].(string) == "" {
			t.Fatal("subscription id should be set")
		}
		if body["status"] != "active" {
			t.Errorf("status: got %v, want active", body["status"])
		}
	})

	// 7. Ingest usage events WITH timestamps inside the billing period
	t.Run("ingest usage", func(t *testing.T) {
		for i := 0; i < 3; i++ {
			resp := doPost(t, ts, "/v1/usage-events", auth, map[string]any{
				"external_customer_id": "e2e_cust",
				"event_name":           "api_calls",
				"quantity":             100,
				"timestamp":            usageTimestamp.Add(time.Duration(i) * time.Hour).Format(time.RFC3339),
				"idempotency_key":      fmt.Sprintf("e2e-event-%d", i),
			})
			assertStatus(t, resp, 201)
		}
	})

	// 8. Usage summary — query the billing period where events were ingested
	t.Run("usage summary", func(t *testing.T) {
		url := fmt.Sprintf("/v1/usage-summary/%s?from=%s&to=%s",
			customerID,
			periodStart.Format(time.RFC3339),
			periodEnd.Format(time.RFC3339))
		resp := doGet(t, ts, url, auth)
		assertStatus(t, resp, 200)
		body := readJSON(t, resp)
		if body["total_events"].(float64) != 3 {
			t.Errorf("total_events: got %v, want 3", body["total_events"])
		}
	})

	// 9. Advance clock to April 1 — period closes, engine sees subscription as due
	clk.Set(time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC))

	// Trigger billing — engine finds the due subscription and generates an invoice
	t.Run("trigger billing", func(t *testing.T) {
		resp := doPost(t, ts, "/v1/billing/run", auth, nil)
		assertStatus(t, resp, 200)
		body := readJSON(t, resp)
		generated := body["invoices_generated"].(float64)
		if generated != 1 {
			t.Fatalf("invoices_generated: got %v, want 1", generated)
		}
	})

	// 10. List invoices — verify the generated invoice
	// Expected: base fee $29 + flat $10/unit × 300 calls = $29 + $3000 = $3029
	// Wait — flat pricing is per-unit: 300 quantity × $10 = $3000. Plus $29 base = $3029.
	var invoiceID string
	t.Run("list invoices", func(t *testing.T) {
		resp := doGet(t, ts, "/v1/invoices", auth)
		assertStatus(t, resp, 200)
		body := readJSON(t, resp)
		data, ok := body["data"].([]any)
		if !ok || len(data) == 0 {
			t.Fatalf("expected at least 1 invoice, got %v", body["data"])
		}
		inv := data[0].(map[string]any)
		invoiceID = inv["id"].(string)
		total := int64(inv["total_amount_cents"].(float64))

		// Base $29 + usage (300 × $10 flat = $3000) = $3029
		expectedTotal := int64(2900 + 300*1000)
		if total != expectedTotal {
			t.Errorf("total: got %d cents, want %d cents", total, expectedTotal)
		}
	})

	// 11. Invoice detail with line items
	t.Run("invoice detail", func(t *testing.T) {
		if invoiceID == "" {
			t.Skip("no invoice to check (previous step failed)")
		}
		resp := doGet(t, ts, "/v1/invoices/"+invoiceID, auth)
		assertStatus(t, resp, 200)
		body := readJSON(t, resp)
		items, ok := body["line_items"].([]any)
		if !ok {
			t.Fatalf("line_items missing or not array: %v", body["line_items"])
		}
		if len(items) != 2 {
			t.Errorf("expected 2 line items (base + usage), got %d", len(items))
		}
	})

	// 12. Download PDF
	t.Run("invoice pdf", func(t *testing.T) {
		if invoiceID == "" {
			t.Skip("no invoice to check")
		}
		resp := doGet(t, ts, "/v1/invoices/"+invoiceID+"/pdf", auth)
		assertStatus(t, resp, 200)
		if resp.Header.Get("Content-Type") != "application/pdf" {
			t.Errorf("content-type: got %q", resp.Header.Get("Content-Type"))
		}
	})

	// 13. Auth required — unauthenticated request should be rejected
	t.Run("no auth 401", func(t *testing.T) {
		resp := doGet(t, ts, "/v1/customers", "")
		assertStatus(t, resp, 401)
	})

	// 14. Credits — grant and verify balance
	t.Run("grant credits", func(t *testing.T) {
		resp := doPost(t, ts, "/v1/credits/grant", auth, map[string]any{
			"customer_id":  customerID,
			"amount_cents": 5000,
			"description":  "Welcome credit",
		})
		assertStatus(t, resp, 201)

		resp = doGet(t, ts, "/v1/credits/balance/"+customerID, auth)
		assertStatus(t, resp, 200)
		body := readJSON(t, resp)
		if body["balance_cents"].(float64) != 5000 {
			t.Errorf("balance: got %v, want 5000", body["balance_cents"])
		}
	})

	t.Logf("E2E passed: invoice %s, 2 line items, PDF downloaded", invoiceID)
}

// --- helpers ---

func doGet(t *testing.T, ts *httptest.Server, path, auth string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("GET", ts.URL+path, nil)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	return resp
}

func doPost(t *testing.T, ts *httptest.Server, path, auth string, body map[string]any) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req, _ := http.NewRequest("POST", ts.URL+path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	return resp
}

func assertStatus(t *testing.T, resp *http.Response, want int) {
	t.Helper()
	if resp.StatusCode != want {
		var body map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&body)
		t.Fatalf("status: got %d, want %d. body: %v", resp.StatusCode, want, body)
	}
}

func readJSON(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return body
}

func createTestAPIKey(t *testing.T, db *postgres.DB, tenantID string) string {
	t.Helper()

	secret := make([]byte, 32)
	_, _ = rand.Read(secret)
	secretHex := hex.EncodeToString(secret)
	rawKey := "vlx_secret_" + secretHex
	prefix := "vlx_secret_" + secretHex[:12]
	hash := sha256.Sum256([]byte(rawKey))
	hashHex := hex.EncodeToString(hash[:])
	keyID := "vlx_key_e2e_" + secretHex[:8]

	tx, err := db.BeginTx(context.Background(), postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	_, err = tx.ExecContext(context.Background(),
		`INSERT INTO api_keys (id, key_prefix, key_hash, key_type, name, tenant_id)
		VALUES ($1, $2, $3, 'secret', 'E2E Test Key', $4)`,
		keyID, prefix, hashHex, tenantID)
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("create key: %v", err)
	}
	_ = tx.Commit()

	return rawKey
}
