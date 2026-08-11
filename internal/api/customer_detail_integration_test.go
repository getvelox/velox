package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/subscription"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestCustomerDetail_UnknownCustomer404s pins the fix for the walked
// "200 empty shell" class (post-billing closeout, open item 2): the
// per-customer composed views used to skip the customer lookup, so a
// typo'd or cross-tenant id answered 200 with everything empty — "this
// customer has nothing" — instead of 404 "no such customer". All three
// routes now resolve the customer first. Cross-tenant behaves exactly
// like nonexistent (RLS hides the row), so there is still no existence
// oracle — just an honest 404.
func TestCustomerDetail_UnknownCustomer404s(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test — requires postgres")
	}
	db := testutil.SetupTestDB(t)
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 15*time.Second)
	defer cancel()

	tenantA := testutil.CreateTestTenant(t, db, "Detail 404 A")
	tenantB := testutil.CreateTestTenant(t, db, "Detail 404 B")

	customers := customer.NewPostgresStore(db)
	subs := subscription.NewPostgresStore(db)
	invoices := invoice.NewPostgresStore(db)
	h := newCustomerDetailHandler(customers, subs, invoices)

	// A real customer in tenant B — used to prove cross-tenant ids 404
	// under tenant A, same as made-up ids.
	cB, err := customers.Create(ctx, tenantB, domain.Customer{DisplayName: "Tenant B Cust"})
	if err != nil {
		t.Fatalf("create tenant-B customer: %v", err)
	}
	// A real customer in tenant A — the 200 control: the same routes
	// must still answer for a customer that exists (an existence check
	// that 404s everything would also "pass" the negative cases).
	cA, err := customers.Create(ctx, tenantA, domain.Customer{DisplayName: "Tenant A Cust"})
	if err != nil {
		t.Fatalf("create tenant-A customer: %v", err)
	}

	r := chi.NewRouter()
	r.Get("/customers/{customer_id}/overview", h.overview)
	r.Get("/customers/{customer_id}/invoices", h.listInvoices)
	r.Get("/customers/{customer_id}/subscriptions", h.listSubscriptions)

	routes := []string{"overview", "invoices", "subscriptions"}
	do := func(customerID, route string) int {
		req := httptest.NewRequest("GET", "/customers/"+customerID+"/"+route, nil).
			WithContext(auth.WithTenantID(ctx, tenantA))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec.Code
	}

	for _, route := range routes {
		if got := do("vlx_cus_does_not_exist", route); got != http.StatusNotFound {
			t.Errorf("%s: made-up id = %d, want 404", route, got)
		}
		if got := do(cB.ID, route); got != http.StatusNotFound {
			t.Errorf("%s: cross-tenant id = %d, want 404 (indistinguishable from nonexistent)", route, got)
		}
		if got := do(cA.ID, route); got != http.StatusOK {
			t.Errorf("%s: real customer = %d, want 200 — the existence check must not 404 the world", route, got)
		}
	}
}
