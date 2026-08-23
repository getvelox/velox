package billing

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/sagarsuperuser/velox/internal/api/respond"
	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/errs"
)

// CreatePreviewHandler exposes POST /v1/invoices/create_preview. Kept
// distinct from the existing billing.Handler (which owns the engine
// trigger and the in-app debug preview) because this surface composes
// across customer / subscription / pricing — the dependency set is
// materially different and the route lives under /invoices/* rather than
// /billing/*.
type CreatePreviewHandler struct {
	svc *PreviewService
}

// NewCreatePreviewHandler wires a handler around a PreviewService.
func NewCreatePreviewHandler(svc *PreviewService) *CreatePreviewHandler {
	return &CreatePreviewHandler{svc: svc}
}

// CreatePreviewRoutes returns the sub-router. Mount at
// /v1/invoices/create_preview as a sibling of /v1/invoices so chi picks
// the more-specific pattern; the parent /invoices Mount uses /{id}
// patterns that would otherwise capture "create_preview" as an invoice
// ID. Auth is gated by PermInvoiceRead — same level as GET /invoices
// (read-only operation, no DB writes).
func (h *CreatePreviewHandler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Post("/", h.create)
	return r
}

// createPreviewWireRequest is the JSON shape the wire sends. Decoupled
// from CreatePreviewRequest so we can parse RFC 3339 timestamps from the
// body without leaking the time.Time type into the wire contract.
//
// Snake-case keys, struct-tag enforced. Both subscription_id and period
// are optional — service defaults the former to the customer's primary
// active sub and the latter to that sub's current cycle.
type createPreviewWireRequest struct {
	CustomerID     string                   `json:"customer_id"`
	SubscriptionID string                   `json:"subscription_id"`
	Period         *createPreviewWirePeriod `json:"period"`
}

// createPreviewWirePeriod is the optional explicit window. Both bounds
// must be supplied together; partial windows are rejected by the service
// layer with a 422.
type createPreviewWirePeriod struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// create handles the POST. JSON body in, PreviewResult out.
//
// Error mapping:
//   - Empty / unparseable body → 400 invalid_request.
//   - customer_id blank → 422 with field=customer_id (errs.Required).
//   - period bounds missing-pair / from >= to / unparseable → 422.
//   - Customer not found / subscription not found → 404.
//   - customer_has_no_subscription → 422 with code (no active sub).
func (h *CreatePreviewHandler) create(w http.ResponseWriter, r *http.Request) {
	// Read-only: this POST computes a dry-run preview and writes nothing. MarkSkip
	// DECLARES that — "this 2xx mutated nothing" — so the coverage detector does
	// not report it as an uncovered mutation. (It used to opt out of a catch-all
	// middleware that would have invented a "Created invoice" row with
	// resource_id="create_preview"; that middleware is gone (ADR-090), but the
	// route is exempt(non_mutating_preview) in the registry, so the detector
	// already skips it. MarkSkip is kept as the LOCAL declaration the registry
	// note cites — the two agree, and if the exemption is ever lifted this call is
	// what keeps the route from reporting as an uncovered mutation.) Fired automatically by the dashboard's
	// upcoming-invoice card on every customer page-open.
	audit.MarkSkip(r.Context())

	tenantID := auth.TenantID(r.Context())

	body, err := io.ReadAll(r.Body)
	if err != nil {
		slog.ErrorContext(r.Context(), "create_preview: read body", "error", err)
		respond.BadRequest(w, r, "could not read request body")
		return
	}

	req, err := decodeCreatePreviewRequest(body)
	if err != nil {
		respond.FromError(w, r, err, "create_preview")
		return
	}

	result, err := h.svc.CreatePreview(r.Context(), tenantID, req)
	if err != nil {
		// errs.ErrNotFound from the customer / subscription store routes
		// to 404 by FromError. The "subscription" resource label is the
		// less common case; "customer" is the more common 404 here, and
		// FromError uses the resource label for the 404 message body
		// rather than for routing.
		if errors.Is(err, errs.ErrNotFound) {
			respond.NotFound(w, r, "customer or subscription")
			return
		}
		respond.FromError(w, r, err, "create_preview")
		return
	}

	respond.JSON(w, r, http.StatusOK, result)
}

// decodeCreatePreviewRequest parses the JSON body and converts wire
// timestamps to time.Time. Returns DomainError-wrapped failures so the
// handler's FromError path can map them to 400/422 with the right field.
//
// Empty body is acceptable as a degenerate {}, but customer_id is required
// by the service layer (Required("customer_id")) so the error surfaces
// uniformly whether the body was {}, {"customer_id":""}, or absent.
func decodeCreatePreviewRequest(body []byte) (CreatePreviewRequest, error) {
	wire := createPreviewWireRequest{}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &wire); err != nil {
			return CreatePreviewRequest{}, errs.Invalid("body", "request body is not valid JSON")
		}
	}

	out := CreatePreviewRequest{
		CustomerID:     wire.CustomerID,
		SubscriptionID: wire.SubscriptionID,
	}

	if wire.Period != nil {
		from, to, err := parseWirePeriod(*wire.Period)
		if err != nil {
			return CreatePreviewRequest{}, err
		}
		out.Period = CreatePreviewPeriod{From: from, To: to}
	}
	return out, nil
}

// parseWirePeriod turns the JSON period {from, to} into time.Time bounds.
// Empty strings stay zero (service treats both-zero as "use sub's current
// cycle"); any non-empty string must be RFC 3339 or we 422 with a
// field-tagged error so the dashboard can route the message.
func parseWirePeriod(p createPreviewWirePeriod) (time.Time, time.Time, error) {
	var from, to time.Time
	if p.From != "" {
		t, err := time.Parse(time.RFC3339, p.From)
		if err != nil {
			return time.Time{}, time.Time{}, errs.Invalid("period.from", "must be RFC 3339 (e.g. 2026-04-01T00:00:00Z)")
		}
		from = t
	}
	if p.To != "" {
		t, err := time.Parse(time.RFC3339, p.To)
		if err != nil {
			return time.Time{}, time.Time{}, errs.Invalid("period.to", "must be RFC 3339 (e.g. 2026-05-01T00:00:00Z)")
		}
		to = t
	}
	return from, to, nil
}
