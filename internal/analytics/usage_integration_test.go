package analytics

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestUsageAnalytics_TenantScoped covers GET /v1/analytics/usage, which had no
// test at all — it is reachable by API key but no dashboard page calls it, so
// a broken query here would have surfaced to an integrator, not to CI.
//
// All three of its scans (timeseries, top-meters, totals) name tenant_id
// explicitly so Postgres can use the index; RLS remains the isolation
// guarantee. This test is the negative control for that pair agreeing: a
// second tenant's events must appear in none of the three.
func TestUsageAnalytics_TenantScoped(t *testing.T) {
	db := testutil.SetupTestDB(t)
	now := time.Now().UTC()

	tenantA := testutil.CreateTestTenant(t, db, "Usage Analytics A")
	custA := seedCustomer(t, db, tenantA, "cus_ua_a")
	meterA := seedMeter(t, db, tenantA, "Meter A", "k_ua_a")
	seedUsageEvent(t, db, tenantA, custA, meterA, 10, now.Add(-2*time.Hour))
	seedUsageEvent(t, db, tenantA, custA, meterA, 15, now.Add(-1*time.Hour))

	tenantB := testutil.CreateTestTenant(t, db, "Usage Analytics B")
	custB := seedCustomer(t, db, tenantB, "cus_ua_b")
	meterB := seedMeter(t, db, tenantB, "Meter B", "k_ua_b")
	seedUsageEvent(t, db, tenantB, custB, meterB, 9999, now.Add(-1*time.Hour))

	h := NewHandler(db)
	req := httptest.NewRequest("GET", "/usage?period=30d", nil)
	req = req.WithContext(auth.WithTenantID(postgres.WithLivemode(req.Context(), false), tenantA))
	rr := httptest.NewRecorder()
	h.usageAnalytics(rr, req)

	if rr.Code != 200 {
		t.Fatalf("status: got %d, body=%s", rr.Code, rr.Body.String())
	}

	// quantity is decimal-string encoded on the wire (NUMERIC(38,12)) —
	// decoding it as int64 is exactly the bug this endpoint shipped with.
	var resp struct {
		Data []struct {
			Events   int64           `json:"events"`
			Quantity decimal.Decimal `json:"quantity"`
		} `json:"data"`
		TopMeters []struct {
			MeterID  string          `json:"meter_id"`
			Events   int64           `json:"events"`
			Quantity decimal.Decimal `json:"quantity"`
		} `json:"top_meters"`
		Totals struct {
			Events   int64           `json:"events"`
			Quantity decimal.Decimal `json:"quantity"`
		} `json:"totals"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v — body=%s", err, rr.Body.String())
	}

	// Totals: tenant A's two events only. 9999 would mean tenant B leaked in.
	if resp.Totals.Events != 2 {
		t.Errorf("totals.events = %d, want 2", resp.Totals.Events)
	}
	if !resp.Totals.Quantity.Equal(decimal.NewFromInt(25)) {
		t.Errorf("totals.quantity = %s, want 25", resp.Totals.Quantity)
	}

	// Timeseries must sum to the same thing.
	var seriesEvents int64
	seriesQty := decimal.Zero
	for _, p := range resp.Data {
		seriesEvents += p.Events
		seriesQty = seriesQty.Add(p.Quantity)
	}
	if seriesEvents != 2 || !seriesQty.Equal(decimal.NewFromInt(25)) {
		t.Errorf("timeseries sums = (%d events, %s qty), want (2, 25)", seriesEvents, seriesQty)
	}

	// Top meters: only tenant A's meter, and never tenant B's.
	if len(resp.TopMeters) != 1 {
		t.Fatalf("top_meters length = %d, want 1 (%+v)", len(resp.TopMeters), resp.TopMeters)
	}
	if resp.TopMeters[0].MeterID != meterA {
		t.Errorf("top_meters[0].meter_id = %s, want %s", resp.TopMeters[0].MeterID, meterA)
	}
	if !resp.TopMeters[0].Quantity.Equal(decimal.NewFromInt(25)) {
		t.Errorf("top_meters[0].quantity = %s, want 25", resp.TopMeters[0].Quantity)
	}
}

func seedCustomer(t *testing.T, db *postgres.DB, tenantID, externalID string) string {
	t.Helper()
	id := postgres.NewID("vlx_cus")
	execInTenantTx(t, db, tenantID, `
		INSERT INTO customers (id, tenant_id, external_id, display_name, email)
		VALUES ($1, $2, $3, $4, $5)
	`, id, tenantID, externalID, "Analytics Customer", externalID+"@example.com")
	return id
}

func seedMeter(t *testing.T, db *postgres.DB, tenantID, name, key string) string {
	t.Helper()
	id := postgres.NewID("vlx_mtr")
	execInTenantTx(t, db, tenantID, `
		INSERT INTO meters (id, tenant_id, name, key, unit, aggregation)
		VALUES ($1, $2, $3, $4, 'requests', 'sum')
	`, id, tenantID, name, key)
	return id
}

func seedUsageEvent(t *testing.T, db *postgres.DB, tenantID, customerID, meterID string, qty int64, ts time.Time) {
	t.Helper()
	execInTenantTx(t, db, tenantID, `
		INSERT INTO usage_events (id, tenant_id, customer_id, meter_id, quantity, properties, timestamp, origin)
		VALUES ($1, $2, $3, $4, $5, '{}'::jsonb, $6, 'api')
	`, postgres.NewID("vlx_evt"), tenantID, customerID, meterID, qty, ts)
}

func execInTenantTx(t *testing.T, db *postgres.DB, tenantID, query string, args ...any) {
	t.Helper()
	ctx := postgres.WithLivemode(context.Background(), false)
	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		t.Fatalf("exec: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}
