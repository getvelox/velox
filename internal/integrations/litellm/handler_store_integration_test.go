package litellm

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/testutil"
	"github.com/sagarsuperuser/velox/internal/usage"
)

// TestSpend_RealStoreContract proves the classification against the REAL
// stores (fake-fidelity rule): an unmapped customer on a live database is
// errs.ErrNotFound → 200 + errors[]; the same lookup against a closed pool
// is a raw driver error → 503. If a store ever returned ErrNotFound for an
// infrastructure failure, the second half goes red.
func TestSpend_RealStoreContract(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "LiteLLM 503 Contract")

	h := New(customer.NewPostgresStore(db), pricing.NewService(pricing.NewPostgresStore(db)), usage.NewService(usage.NewPostgresStore(db)))
	post := func(t *testing.T) (int, string) {
		t.Helper()
		code, body := postSpendWithCtx(t, h, ctx, tenantID, spendPayload("real-1"))
		return code, body
	}

	code, body := post(t)
	if code != http.StatusOK || !strings.Contains(body, "not found") {
		t.Fatalf("unmapped customer on a live DB: status %d body %s, want 200 with a not-found row", code, body)
	}

	_ = db.Pool.Close() // the store is now unreachable — every lookup is a raw driver error
	code, body = post(t)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("closed pool: status %d body %s, want 503 (a store failure must not masquerade as not-found)", code, body)
	}
	if strings.Contains(body, "not found") || strings.Contains(body, "closed") {
		t.Fatalf("closed pool: body leaks or misclassifies: %s", body)
	}
}
