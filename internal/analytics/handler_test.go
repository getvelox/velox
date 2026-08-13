package analytics

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/auth"
)

// ctxWithTenant injects a tenant ID into ctx the same way the real auth
// middleware does — the analytics handlers rely on it.
func ctxWithTenant(ctx context.Context, tenantID string) context.Context {
	return context.WithValue(ctx, auth.TestTenantIDKey(), tenantID)
}

// ---------------------------------------------------------------------------
// Route registration
// ---------------------------------------------------------------------------

func TestRoutes_RegistersExpectedEndpoints(t *testing.T) {
	h := NewHandler(nil)
	r := h.Routes()

	chiRouter, ok := r.(*chi.Mux)
	if !ok {
		t.Fatal("Routes() should return a *chi.Mux")
	}

	type route struct{ method, pattern string }
	var routes []route
	_ = chi.Walk(chiRouter, func(method, pattern string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		routes = append(routes, route{method, pattern})
		return nil
	})

	expected := []route{
		{"GET", "/overview"},
		{"GET", "/revenue-chart"},
		{"GET", "/mrr-movement"},
		{"GET", "/usage"},
	}

	for _, want := range expected {
		found := false
		for _, got := range routes {
			if got.method == want.method && got.pattern == want.pattern {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing route: %s %s", want.method, want.pattern)
		}
	}
}

func TestRoutes_MethodNotAllowed(t *testing.T) {
	h := NewHandler(nil)
	r := h.Routes()

	req := httptest.NewRequest("POST", "/overview", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /overview: got %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}

// ---------------------------------------------------------------------------
// Response shapes
// ---------------------------------------------------------------------------

func TestOverviewResponse_JSONShape(t *testing.T) {
	resp := OverviewResponse{
		Period:              "30d",
		Currency:            "USD",
		MRR:                 1200_00,
		MRRPrev:             1100_00,
		ARR:                 14400_00,
		ARRPrev:             13200_00,
		Revenue:             500_000_00,
		RevenuePrev:         450_000_00,
		OutstandingAR:       15_000_00,
		AvgInvoiceValue:     320_00,
		CreditBalance:       8_500_00,
		ActiveCustomers:     42,
		NewCustomers:        5,
		ActiveSubs:          38,
		TrialingSubs:        3,
		PaidInvoices:        156,
		FailedPayments:      3,
		OpenInvoices:        12,
		DunningActive:       2,
		UsageEvents:         12_345,
		LogoChurnRate:       0.05,
		RevenueChurnRate:    0.02,
		NRR:                 1.08,
		DunningRecoveryRate: 0.75,
		MRRMovement: MRRMovementTotals{
			New: 2500, Expansion: 500, Contraction: 100, Churned: 800, Net: 2100,
		},
	}

	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal OverviewResponse: %v", err)
	}

	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	expected := []string{
		"period", "currency", "mrr", "mrr_prev", "arr", "arr_prev",
		"revenue", "revenue_prev", "outstanding_ar", "avg_invoice_value", "credit_balance_total",
		"active_customers", "new_customers", "active_subscriptions", "trialing_subscriptions",
		"paid_invoices", "failed_payments", "open_invoices",
		"dunning_active", "refunds_needing_attention", "usage_events",
		"logo_churn_rate", "revenue_churn_rate", "nrr", "dunning_recovery_rate",
		"mrr_movement",
	}
	for _, field := range expected {
		if _, ok := m[field]; !ok {
			t.Errorf("missing JSON field %q", field)
		}
	}
	if len(m) != len(expected) {
		t.Errorf("OverviewResponse has %d fields, want %d", len(m), len(expected))
	}

	// Spot-check the nested movement object.
	mv, ok := m["mrr_movement"].(map[string]any)
	if !ok {
		t.Fatalf("mrr_movement: expected object, got %T", m["mrr_movement"])
	}
	for _, k := range []string{"new", "expansion", "contraction", "churned", "net"} {
		if _, ok := mv[k]; !ok {
			t.Errorf("mrr_movement missing field %q", k)
		}
	}
}

func TestOverviewResponse_ZeroValues(t *testing.T) {
	var resp OverviewResponse
	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var m map[string]any
	_ = json.Unmarshal(data, &m)

	if m["period"] != "" {
		t.Errorf("period: got %v, want empty", m["period"])
	}
	if m["mrr"] != float64(0) {
		t.Errorf("mrr: got %v, want 0", m["mrr"])
	}
}

func TestRevenueDataPoint_JSONShape(t *testing.T) {
	dp := RevenueDataPoint{
		Date:         "2026-04-01",
		RevenueCents: 150_000,
		InvoiceCount: 42,
	}

	data, err := json.Marshal(dp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var m map[string]any
	_ = json.Unmarshal(data, &m)

	if m["date"] != "2026-04-01" {
		t.Errorf("date: got %v", m["date"])
	}
	if m["revenue_cents"] != float64(150_000) {
		t.Errorf("revenue_cents: got %v", m["revenue_cents"])
	}
	if m["invoice_count"] != float64(42) {
		t.Errorf("invoice_count: got %v", m["invoice_count"])
	}
}

func TestMRRMovementPoint_JSONShape(t *testing.T) {
	p := MRRMovementPoint{Date: "2026-04-01", New: 100, Expansion: 20, Contraction: 5, Churned: 10, Net: 105}
	data, _ := json.Marshal(p)
	var m map[string]any
	_ = json.Unmarshal(data, &m)

	for _, k := range []string{"date", "new", "expansion", "contraction", "churned", "net"} {
		if _, ok := m[k]; !ok {
			t.Errorf("missing field %q", k)
		}
	}
}

func TestUsagePoint_JSONShape(t *testing.T) {
	p := UsagePoint{Date: "2026-04-01", Events: 100, Quantity: decimal.NewFromInt(5000)}
	data, _ := json.Marshal(p)
	var m map[string]any
	_ = json.Unmarshal(data, &m)

	for _, k := range []string{"date", "events", "quantity"} {
		if _, ok := m[k]; !ok {
			t.Errorf("missing field %q", k)
		}
	}
}

// ---------------------------------------------------------------------------
// Period parsing
// ---------------------------------------------------------------------------

func TestParsePeriod(t *testing.T) {
	tests := []struct {
		key       string
		wantKey   string
		wantDays  int
		wantTrunc string
	}{
		{"", "30d", 30, "day"},
		{"7d", "7d", 7, "day"},
		{"30d", "30d", 30, "day"},
		{"90d", "90d", 90, "day"},
		{"12m", "12m", 365, "month"},
		{"bogus", "30d", 30, "day"},
	}

	for _, tt := range tests {
		t.Run("period="+tt.key, func(t *testing.T) {
			p := parsePeriod(tt.key)
			if p.Key != tt.wantKey {
				t.Errorf("Key: got %q, want %q", p.Key, tt.wantKey)
			}
			if p.Trunc != tt.wantTrunc {
				t.Errorf("Trunc: got %q, want %q", p.Trunc, tt.wantTrunc)
			}

			// Window is close to wantDays in length, accounting for rounding.
			length := p.End.Sub(p.Start)
			wantLen := time.Duration(tt.wantDays) * 24 * time.Hour
			if abs(length-wantLen) > time.Hour {
				t.Errorf("length: got %v, want ~%v", length, wantLen)
			}

			// Prior window is the same length and ends where current begins.
			if !p.PrevEnd.Equal(p.Start) {
				t.Errorf("prev window does not abut current: prevEnd=%v start=%v", p.PrevEnd, p.Start)
			}
			if abs(p.PrevEnd.Sub(p.PrevStart)-length) > time.Hour {
				t.Errorf("prev window length mismatch")
			}
		})
	}
}

func abs(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// ---------------------------------------------------------------------------
// Constructor / context helpers
// ---------------------------------------------------------------------------

func TestNewHandler(t *testing.T) {
	h := NewHandler(nil)
	if h == nil {
		t.Fatal("NewHandler returned nil")
	}
	if h.db != nil {
		t.Error("db should be nil when passed nil")
	}
}

func TestTenantIDFromContext(t *testing.T) {
	ctx := ctxWithTenant(context.Background(), "t_test_123")
	if got := auth.TenantID(ctx); got != "t_test_123" {
		t.Errorf("TenantID: got %q, want %q", got, "t_test_123")
	}
}

func TestTenantIDFromContext_Empty(t *testing.T) {
	if got := auth.TenantID(context.Background()); got != "" {
		t.Errorf("TenantID: got %q, want empty", got)
	}
}

// SQL correctness is validated in integration tests (requires a real DB with
// seed data), not here.
