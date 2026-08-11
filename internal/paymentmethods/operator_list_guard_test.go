package paymentmethods

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/errs"
)

type notFoundLookup struct{}

func (notFoundLookup) GetForSetupLink(context.Context, string, string) (string, string, error) {
	return "", "", errs.ErrNotFound
}

// TestOperatorList_UnknownCustomer404s pins the existence check on
// GET /v1/customers/{customer_id}/payment-methods. Service.List legally
// returns an empty slice for an unknown customer, so pre-fix a typo'd or
// cross-tenant id answered 200 [] — "no cards on file" — instead of 404
// "no such customer" (the walked 200-empty-shell class). The guard must
// answer BEFORE the service is consulted: the handler here is built with
// a nil service, so reaching it would panic — the 404 proves the lookup
// short-circuits.
func TestOperatorList_UnknownCustomer404s(t *testing.T) {
	h := NewHandler(nil)
	h.SetCustomerLookup(notFoundLookup{})

	r := chi.NewRouter()
	r.Mount("/customers/{customer_id}/payment-methods", h.OperatorRoutes())

	req := httptest.NewRequest("GET", "/customers/vlx_cus_nope/payment-methods/", nil).
		WithContext(auth.WithTenantID(context.Background(), "vlx_ten_t1"))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown customer: status = %d, want 404", rec.Code)
	}
}
