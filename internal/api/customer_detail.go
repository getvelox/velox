package api

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/sagarsuperuser/velox/internal/api/respond"
	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/subscription"
)

// customerDetailHandler provides the operator dashboard's per-customer
// composed views (overview / invoices / subscriptions on the Customer
// Detail page). Lives in the API package because it crosses domain
// boundaries — domain packages stay independent.
//
// History: born as "customerPortalHandler" at /v1/customer-portal — a
// name that collided with the ADR-051 customer SELF-SERVE portal
// (magic-link login, /v1/me/*), which was removed 2026-06-09. This
// handler was never that feature: it has been operator-auth
// (PermCustomerRead), dashboard-serving from birth. Renamed and moved
// under /v1/customers/{id}/* so "customer portal" means exactly one
// thing in this repo's history — the feature ADR-051 removed.
type customerDetailHandler struct {
	customers *customer.PostgresStore
	subs      *subscription.PostgresStore
	invoices  *invoice.PostgresStore
}

func newCustomerDetailHandler(customers *customer.PostgresStore, subs *subscription.PostgresStore, invoices *invoice.PostgresStore) *customerDetailHandler {
	return &customerDetailHandler{customers: customers, subs: subs, invoices: invoices}
}

// requireCustomer resolves the path's customer id to an existing row and
// writes the 404 itself when there is none. Every route here composes
// tenant-scoped sub-queries that all legally return empty for an unknown
// id — so without this lookup a typo'd or cross-tenant id read as "this
// customer has nothing" (200, empty shell) instead of "no such customer".
// Not a leak and not an existence oracle (a cross-tenant id and a made-up
// one behave identically), but an honest 404 is what a caller can act on.
func (h *customerDetailHandler) requireCustomer(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	tenantID := auth.TenantID(r.Context())
	customerID := chi.URLParam(r, "customer_id")
	if _, err := h.customers.Get(r.Context(), tenantID, customerID); err != nil {
		if errors.Is(err, errs.ErrNotFound) {
			respond.NotFound(w, r, "customer")
		} else {
			respond.InternalError(w, r)
		}
		return "", "", false
	}
	return tenantID, customerID, true
}

func (h *customerDetailHandler) listSubscriptions(w http.ResponseWriter, r *http.Request) {
	tenantID, customerID, ok := h.requireCustomer(w, r)
	if !ok {
		return
	}

	subs, _, err := h.subs.List(r.Context(), subscription.ListFilter{
		TenantID:   tenantID,
		CustomerID: customerID,
	})
	if err != nil {
		respond.InternalError(w, r)
		return
	}
	if subs == nil {
		subs = []domain.Subscription{}
	}
	respond.JSON(w, r, http.StatusOK, map[string]any{"data": subs})
}

func (h *customerDetailHandler) listInvoices(w http.ResponseWriter, r *http.Request) {
	tenantID, customerID, ok := h.requireCustomer(w, r)
	if !ok {
		return
	}

	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 25
	}

	invoices, total, err := h.invoices.List(r.Context(), invoice.ListFilter{
		TenantID:   tenantID,
		CustomerID: customerID,
		Limit:      limit,
	})
	if err != nil {
		respond.InternalError(w, r)
		return
	}
	if invoices == nil {
		invoices = []domain.Invoice{}
	}
	respond.JSON(w, r, http.StatusOK, map[string]any{"data": invoices, "total": total})
}

// overview returns a consolidated view of a customer: active subscriptions,
// the 5 most-recent invoices, and outstanding balance. It does NOT return a
// usage summary — the comment said so for a while and the response never
// carried one. requireCustomer closes the walked "200 empty shell for an
// unknown id" gap (post-billing closeout, open item 2).
func (h *customerDetailHandler) overview(w http.ResponseWriter, r *http.Request) {
	tenantID, customerID, ok := h.requireCustomer(w, r)
	if !ok {
		return
	}

	subs, _, _ := h.subs.List(r.Context(), subscription.ListFilter{
		TenantID:   tenantID,
		CustomerID: customerID,
		Status:     "active",
	})

	invoices, _, _ := h.invoices.List(r.Context(), invoice.ListFilter{
		TenantID:   tenantID,
		CustomerID: customerID,
		Limit:      5,
	})

	if subs == nil {
		subs = []domain.Subscription{}
	}
	if invoices == nil {
		invoices = []domain.Invoice{}
	}

	// Outstanding balance — accounts-receivable exposure for this
	// customer. Aggregate over ALL their unpaid invoices, not just
	// the 5 most-recent in the recent_invoices slice. Industry parity:
	// Stripe / Lago / Chargebee / Recurly all surface this on the
	// customer page so operators see total AR at a glance instead of
	// summing it manually. Read failure surfaces as zero (best-effort);
	// the rest of the overview still renders.
	outstanding, _ := h.invoices.GetOutstandingBalance(r.Context(), tenantID, customerID)

	respond.JSON(w, r, http.StatusOK, map[string]any{
		"customer_id":          customerID,
		"active_subscriptions": subs,
		"recent_invoices":      invoices,
		"outstanding_balance":  outstanding,
	})
}
