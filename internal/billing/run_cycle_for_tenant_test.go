package billing

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
)

func dueSubFixture(id string) domain.Subscription {
	periodStart := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	return domain.Subscription{
		ID: id, TenantID: "t1", CustomerID: "cus_" + id,
		Items:                     []domain.SubscriptionItem{{PlanID: "pln_1", Quantity: 1}},
		Status:                    domain.SubscriptionActive,
		BillingTime:               domain.BillingTimeCalendar,
		CurrentBillingPeriodStart: &periodStart, CurrentBillingPeriodEnd: &periodEnd,
		NextBillingAt: &periodEnd,
	}
}

func tenantRunEngine(subs *mockSubs) *Engine {
	return tenantRunEngineWith(subs, &mockInvoices{})
}

// tenantRunEngineWith lets a test supply the invoice mock — e.g. a db-backed one
// (`&mockInvoices{db: db}`) so the engine's in-tx finalize emission (ADR-090) can
// ride a real tx when the test also wires the real audit logger.
func tenantRunEngineWith(subs *mockSubs, invoices *mockInvoices) *Engine {
	pricing := &mockPricing{plans: map[string]domain.Plan{
		"pln_1": {ID: "pln_1", Currency: "USD", BillingInterval: domain.BillingMonthly, BaseAmountCents: 1000},
	}}
	return wireBaseTax(NewEngine(subs, &mockUsage{totals: map[string]int64{}}, pricing, invoices, nil, &mockSettings{}, nil, nil, billingTestClock()))
}

// TestRunCycleForTenant_BillsDueSubs: the happy path — the manual run bills the
// tenant's due, non-clock-pinned subs and reports no failures.
func TestRunCycleForTenant_BillsDueSubs(t *testing.T) {
	subs := &mockSubs{
		subs:         map[string]domain.Subscription{"sub_a": dueSubFixture("sub_a")},
		cycleUpdated: make(map[string]bool),
	}
	engine := tenantRunEngine(subs)

	generated, failures := engine.RunCycleForTenant(context.Background(), "t1", 50)
	if len(failures) != 0 {
		t.Fatalf("unexpected failures: %v", failures)
	}
	if generated < 1 {
		t.Errorf("expected >=1 invoice generated, got %d", generated)
	}
}

// TestRunCycleForTenant_FailingSubDoesNotLoopForever locks the no-progress
// guard: a sub that fails to bill never advances next_billing_at and stays
// "due", so a naive re-fetch loop would spin forever. RunCycleForTenant must
// attempt it at most once and return exactly one SubBillError carrying the
// sub id (the id the handler surfaces; the raw cause is server-logged only).
func TestRunCycleForTenant_FailingSubDoesNotLoopForever(t *testing.T) {
	subs := &mockSubs{
		subs:           map[string]domain.Subscription{"sub_x": dueSubFixture("sub_x")},
		cycleUpdated:   make(map[string]bool),
		closePeriodErr: errors.New("simulated period close failure"),
	}
	engine := tenantRunEngine(subs)

	done := make(chan struct{})
	var failures []SubBillError
	go func() {
		_, failures = engine.RunCycleForTenant(context.Background(), "t1", 50)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunCycleForTenant did not terminate — the no-progress guard is broken (failing sub re-billed forever)")
	}

	if len(failures) != 1 {
		t.Fatalf("expected exactly 1 failure (attempted once), got %d: %v", len(failures), failures)
	}
	if failures[0].SubscriptionID != "sub_x" {
		t.Errorf("failure must carry the subscription id, got %q", failures[0].SubscriptionID)
	}
}

// TestTriggerCycle_ForbidsUnscopedKey is the fail-closed lock: a request with no
// tenant scope (a platform key) must be rejected 403 — never fall through to the
// unscoped, cross-tenant RunCycle (the pre-fix leak). RunCycle stays
// scheduler-only.
func TestTriggerCycle_ForbidsUnscopedKey(t *testing.T) {
	subs := &mockSubs{subs: map[string]domain.Subscription{}, cycleUpdated: make(map[string]bool)}
	h := NewHandler(tenantRunEngine(subs), subs)

	req := httptest.NewRequest(http.MethodPost, "/run", nil) // no tenant in ctx
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("unscoped billing run must be 403, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestTriggerCycle_SanitizesErrorDetail locks that a per-sub billing failure is
// surfaced as sub-id + a generic class only — raw DB/Stripe error text (the
// pre-fix leak) never reaches the response body.
func TestTriggerCycle_SanitizesErrorDetail(t *testing.T) {
	const secret = "pq: duplicate key value violates unique constraint idx_secret"
	subs := &mockSubs{
		subs:           map[string]domain.Subscription{"sub_y": dueSubFixture("sub_y")},
		cycleUpdated:   make(map[string]bool),
		closePeriodErr: errors.New(secret),
	}
	h := NewHandler(tenantRunEngine(subs), subs)

	ctx := auth.WithTenantID(context.Background(), "t1")
	req := httptest.NewRequest(http.MethodPost, "/run", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusPartialContent {
		t.Fatalf("a run with failures must be 206, got %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, secret) || strings.Contains(body, "constraint") {
		t.Fatalf("raw error detail leaked into the response body: %s", body)
	}
	var resp struct {
		Errors []string `json:"errors"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Errors) != 1 || !strings.Contains(resp.Errors[0], "sub_y") {
		t.Errorf("sanitized error must name the subscription, got %v", resp.Errors)
	}
}
