package dunning

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/sagarsuperuser/velox/internal/api/respond"
	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

// PaymentCanceler stops an invoice from being payable at Stripe when a
// dunning resolution voids it (expire live Checkout sessions, then cancel the
// PI where Stripe allows). Satisfied by *payment.Stripe.
type PaymentCanceler interface {
	StopCollection(ctx context.Context, tenantID string, inv domain.Invoice)
}

// InvoiceVoider routes dunning resolution outcomes through the invoice
// SERVICE (not the raw store), so they inherit the service's status guards,
// the in-flight payment guard, the tax reversal, and the single-writer
// webhook events. Before this, resolveRun called the raw store's
// UpdateStatus(Voided) directly — a second, less-guarded void writer that
// reversed no tax and emitted no event (an overlapping-flow hole). The
// payment_recovered leg had the same hole until 2026-07-26: a raw
// MarkPaid("") skipped the out_of_band: marker, the in-flight guard, and
// the invoice.payment_recorded webhook that the Record-offline-payment
// dialog's path emits — two offline-payment writers with different
// evidence trails for the same operator action.
type InvoiceVoider interface {
	Void(ctx context.Context, tenantID, id string) (domain.Invoice, error)
	RecordOfflinePayment(ctx context.Context, tenantID, id, note string) (domain.Invoice, error)
}

type Handler struct {
	svc           *Service
	invoiceVoider InvoiceVoider
	paymentCancel PaymentCanceler
	auditLogger   AuditWriter
	resolver      clock.Resolver
}

// AuditWriter is the narrow audit surface dunning handler uses.
// Decoupled from internal/audit so the handler can be tested with
// a fake; wired in router.go via SetAuditLogger.
type AuditWriter interface {
	Log(ctx context.Context, tenantID, action, resourceType, resourceID, resourceLabel string, metadata map[string]any) error
}

// SetAuditLogger wires the audit logger so dunning policy CRUD and
// run resolution mutations land in audit_log. Without this, operator-
// triggered resolution of a dunning run (a money decision: customer
// no longer pays vs grace extension vs write-off) was invisible.
func (h *Handler) SetAuditLogger(a AuditWriter) {
	h.auditLogger = a
}

// SetResolver wires the clock resolver so resolveRun can bind ctx
// from the invoice's pin before invoices.MarkPaid. Without this,
// `invoice.paid_at` stamps wall-clock on clock-pinned invoices —
// inconsistent with every other invoice timestamp on the same row
// and breaks ADR-030's "no wall-clock leakage on pinned entities"
// guarantee at the dunning-resolution seam.
func (h *Handler) SetResolver(r clock.Resolver) { h.resolver = r }

// SetInvoiceVoider wires the invoice service so a manually-resolved dunning
// run voids through invoice.Service.Void (status guards + in-flight guard +
// tax reversal + single-writer invoice.voided event) instead of the raw
// store's UpdateStatus. Wired post-construction (the invoice service is built
// after the dunning handler), mirroring SetInvoiceUncollectibleMarker.
func (h *Handler) SetInvoiceVoider(v InvoiceVoider) { h.invoiceVoider = v }

// SetPaymentCanceler wires the collection-stop seam (lazy: the payment
// service is built after this handler).
func (h *Handler) SetPaymentCanceler(p PaymentCanceler) { h.paymentCancel = p }

type HandlerDeps struct {
	PaymentCancel PaymentCanceler
}

func NewHandler(svc *Service, deps ...HandlerDeps) *Handler {
	h := &Handler{svc: svc}
	if len(deps) > 0 {
		h.paymentCancel = deps[0].PaymentCancel
	}
	return h
}

func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()

	// Dunning policies (ADR-036 campaigns model — multi-policy-per-
	// tenant). Replaces the prior singleton /policy + per-customer
	// /customers/{id}/override surface. Customers are reassigned via
	// PATCH /v1/customers/{id} { "dunning_policy_id": ... } on the
	// customer handler.
	r.Route("/policies", func(r chi.Router) {
		r.Get("/", h.listPolicies)
		r.Post("/", h.createPolicy)
		r.Get("/{id}", h.getPolicy)
		r.Patch("/{id}", h.updatePolicy)
		r.Delete("/{id}", h.deletePolicy)
		r.Post("/{id}/set-default", h.setDefaultPolicy)
	})

	r.Route("/runs", func(r chi.Router) {
		r.Get("/", h.listRuns)
		r.Get("/{id}", h.getRun)
		r.Post("/{id}/resolve", h.resolveRun)
	})

	// /stats backs the dashboard's stat cards. Aggregate query — no
	// pagination, no client-side derivation from a sliced /runs list.
	r.Get("/stats", h.getStats)

	return r
}

func (h *Handler) getStats(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	stats, err := h.svc.GetStats(r.Context(), tenantID)
	if err != nil {
		respond.InternalError(w, r)
		slog.ErrorContext(r.Context(), "get dunning stats", "error", err)
		return
	}
	respond.JSON(w, r, http.StatusOK, stats)
}

func (h *Handler) listPolicies(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	policies, err := h.svc.ListPolicies(r.Context(), tenantID)
	if err != nil {
		respond.InternalError(w, r)
		slog.ErrorContext(r.Context(), "list dunning policies", "error", err)
		return
	}
	if policies == nil {
		policies = []domain.DunningPolicy{}
	}
	// Attach customer-assignment counts so the admin page can render
	// the "N customers assigned" badge without a round-trip per row.
	type policyWithCount struct {
		domain.DunningPolicy
		AssignedCustomers int `json:"assigned_customers"`
	}
	out := make([]policyWithCount, 0, len(policies))
	for _, p := range policies {
		count, _ := h.svc.CountCustomersOnPolicy(r.Context(), tenantID, p.ID)
		out = append(out, policyWithCount{DunningPolicy: p, AssignedCustomers: count})
	}
	respond.JSON(w, r, http.StatusOK, map[string]any{"data": out})
}

func (h *Handler) getPolicy(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	id := chi.URLParam(r, "id")
	policy, err := h.svc.GetPolicyByID(r.Context(), tenantID, id)
	if errors.Is(err, errs.ErrNotFound) {
		respond.NotFound(w, r, "dunning_policy")
		return
	}
	if err != nil {
		respond.InternalError(w, r)
		slog.ErrorContext(r.Context(), "get dunning policy", "id", id, "error", err)
		return
	}
	respond.JSON(w, r, http.StatusOK, policy)
}

// decodePolicyBody decodes a policy payload and REFUSES the pre-ADR-112
// `final_action` key by name.
//
// encoding/json drops unknown fields silently, so a caller still sending
// `final_action: "cancel_subscription"` would have stored a policy that
// pauses and never writes off — a terminal action they did not choose,
// applied to real money, with a 200 on the way out. DisallowUnknownFields
// would be the blunt version and would also reject harmless extra keys.
func decodePolicyBody(r *http.Request) (domain.DunningPolicy, error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return domain.DunningPolicy{}, errors.New("invalid JSON body")
	}
	var legacy struct {
		FinalAction *string `json:"final_action"`
	}
	if err := json.Unmarshal(body, &legacy); err == nil && legacy.FinalAction != nil {
		return domain.DunningPolicy{}, errs.Invalid("final_action",
			"final_action was split in ADR-112 — send final_subscription_action (none|pause|cancel) and final_invoice_action (none|mark_uncollectible)")
	}
	var policy domain.DunningPolicy
	if err := json.Unmarshal(body, &policy); err != nil {
		return domain.DunningPolicy{}, errors.New("invalid JSON body")
	}
	return policy, nil
}

func (h *Handler) createPolicy(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	policy, err := decodePolicyBody(r)
	if err != nil {
		respond.FromError(w, r, err, "dunning_policy")
		return
	}
	policy.ID = "" // server-assigned
	result, err := h.svc.UpsertPolicy(r.Context(), tenantID, policy)
	if err != nil {
		respond.FromError(w, r, err, "dunning_policy")
		return
	}
	if h.auditLogger != nil {
		_ = h.auditLogger.Log(r.Context(), tenantID, domain.AuditActionCreate, "dunning_policy", result.ID, result.Name, nil)
	}
	respond.JSON(w, r, http.StatusCreated, result)
}

func (h *Handler) updatePolicy(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	id := chi.URLParam(r, "id")
	policy, err := decodePolicyBody(r)
	if err != nil {
		respond.FromError(w, r, err, "dunning_policy")
		return
	}
	policy.ID = id
	result, err := h.svc.UpsertPolicy(r.Context(), tenantID, policy)
	if err != nil {
		respond.FromError(w, r, err, "dunning_policy")
		return
	}
	if h.auditLogger != nil {
		_ = h.auditLogger.Log(r.Context(), tenantID, domain.AuditActionUpdate, "dunning_policy", result.ID, result.Name, nil)
	}
	respond.JSON(w, r, http.StatusOK, result)
}

func (h *Handler) deletePolicy(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	id := chi.URLParam(r, "id")
	err := h.svc.DeletePolicy(r.Context(), tenantID, id)
	if errors.Is(err, errs.ErrNotFound) {
		respond.NotFound(w, r, "dunning_policy")
		return
	}
	if err != nil {
		respond.FromError(w, r, err, "dunning_policy")
		return
	}
	if h.auditLogger != nil {
		_ = h.auditLogger.Log(r.Context(), tenantID, domain.AuditActionDelete, "dunning_policy", id, "", nil)
	}
	respond.JSON(w, r, http.StatusOK, map[string]string{"status": "deleted"})
}

func (h *Handler) setDefaultPolicy(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	id := chi.URLParam(r, "id")
	if err := h.svc.SetDefaultPolicy(r.Context(), tenantID, id); err != nil {
		if errors.Is(err, errs.ErrNotFound) {
			respond.NotFound(w, r, "dunning_policy")
			return
		}
		respond.FromError(w, r, err, "dunning_policy")
		return
	}
	if h.auditLogger != nil {
		_ = h.auditLogger.Log(r.Context(), tenantID, domain.AuditActionUpdate, "dunning_policy", id, "", map[string]any{
			"action": "set_default",
		})
	}
	respond.JSON(w, r, http.StatusOK, map[string]string{"status": "default_updated"})
}

func (h *Handler) listRuns(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())

	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))

	runs, total, err := h.svc.ListRuns(r.Context(), RunListFilter{
		TenantID:  tenantID,
		InvoiceID: r.URL.Query().Get("invoice_id"),
		State:     r.URL.Query().Get("state"),
		Limit:     limit,
		Offset:    offset,
	})
	if err != nil {
		respond.InternalError(w, r)
		slog.ErrorContext(r.Context(), "list dunning runs", "error", err)
		return
	}
	if runs == nil {
		runs = []domain.InvoiceDunningRun{}
	}

	respond.List(w, r, runs, total)
}

func (h *Handler) getRun(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	id := chi.URLParam(r, "id")

	run, err := h.svc.store.GetRun(r.Context(), tenantID, id)
	if errors.Is(err, errs.ErrNotFound) {
		respond.NotFound(w, r, "dunning run")
		return
	}
	if err != nil {
		respond.InternalError(w, r)
		slog.ErrorContext(r.Context(), "get dunning run", "error", err)
		return
	}

	events, _ := h.svc.store.ListEvents(r.Context(), tenantID, id)
	if events == nil {
		events = []domain.InvoiceDunningEvent{}
	}

	respond.JSON(w, r, http.StatusOK, map[string]any{
		"run":    run,
		"events": events,
	})
}

// Customer override handlers (GetCustomerOverride / UpsertCustomerOverride
// / DeleteCustomerOverride) were removed in ADR-036. Per-customer
// differentiation is now expressed as `customers.dunning_policy_id`
// assignment; mutation goes through PATCH /v1/customers/{id} on the
// customer handler.

type resolveInput struct {
	Resolution string `json:"resolution"`
}

func (h *Handler) resolveRun(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	id := chi.URLParam(r, "id")

	var input resolveInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respond.BadRequest(w, r, "invalid JSON body")
		return
	}

	// Only the three operator-choosable outcomes are legal here.
	// retries_exhausted / action_failed are engine-written states —
	// accepting them (or any free-text string) from the API fabricates
	// engine outcomes and used to store the bogus value verbatim
	// (resolution has a CHECK constraint since migration 0158, but the
	// column is written by multiple flows, so the operator endpoint
	// rejects early with a teaching message).
	switch domain.DunningResolution(input.Resolution) {
	case domain.ResolutionPaymentRecovered, domain.ResolutionInvoiceVoided, domain.ResolutionInvoiceNotCollectible:
	default:
		// manually_resolved is deliberately NOT accepted: it is the legacy
		// pre-0170 value that meant "voided OR uncollectible", and accepting
		// it would reintroduce exactly the ambiguity the split removed. The
		// message names it explicitly rather than letting an integrator who
		// still sends it read a generic refusal and guess.
		if domain.DunningResolution(input.Resolution) == domain.ResolutionManuallyResolved {
			respond.ValidationField(w, r, "resolution", "manually_resolved is no longer accepted — it meant both 'voided' and 'written off'. Use invoice_voided to annul the invoice, or invoice_not_collectible to write it off.")
			return
		}
		respond.ValidationField(w, r, "resolution", "resolution must be one of payment_recovered, invoice_voided, invoice_not_collectible")
		return
	}

	run, err := h.svc.ResolveRun(r.Context(), tenantID, id, domain.DunningResolution(input.Resolution))
	if err != nil {
		respond.FromError(w, r, err, "dunning_run")
		return
	}

	// Money-decision audit: operator chose how to close out a failing
	// invoice's collection cycle (payment recovered / manual resolve /
	// write-off). Critical for finance reconciliation — "why was this
	// invoice marked recovered when no payment came in?".
	if h.auditLogger != nil {
		// Bind the invoice's clock so an operator resolving a dunning run inside
		// a simulation lands on the sim axis. The dunning_run row itself is
		// hard-deleted by ADR-086 teardown, so this row is the only surviving
		// record of WHY collection was closed out ("marked recovered when no
		// payment came in") — precisely the question the comment above says
		// finance asks. Nothing to resolve in live mode (no clock possible).
		auditCtx := r.Context()
		if h.resolver != nil && run.InvoiceID != "" && !postgres.Livemode(auditCtx) {
			auditCtx, _ = clock.BindEffectiveNow(auditCtx, h.resolver, clock.Pin{TenantID: tenantID, InvoiceID: run.InvoiceID})
		}
		_ = h.auditLogger.Log(auditCtx, tenantID, domain.AuditActionUpdate, "dunning_run", run.ID, "", map[string]any{
			"action":     "resolved",
			"resolution": input.Resolution,
			"invoice_id": run.InvoiceID,
		})
	}

	// Propagate resolution to invoice
	if run.InvoiceID != "" {
		switch domain.DunningResolution(input.Resolution) {
		case domain.ResolutionPaymentRecovered:
			// Route through the invoice service's offline-payment writer —
			// the same path the invoice page's Record-offline-payment dialog
			// uses — so the recovery gets the out_of_band: PaymentIntent
			// marker, the in-flight-charge guard, the payment_recorded audit
			// row, and the invoice.payment_recorded webhook. A raw MarkPaid
			// here was a second offline-payment writer with a thinner
			// evidence trail for the same operator action. The service binds
			// the invoice's clock itself (ADR-030), so sim-pinned invoices
			// land paid_at on the sim axis without a bind here.
			if h.invoiceVoider == nil {
				slog.WarnContext(r.Context(), "invoice service unwired; skipping dunning payment-recovered mark-paid", "invoice_id", run.InvoiceID)
				break
			}
			if _, err := h.invoiceVoider.RecordOfflinePayment(r.Context(), tenantID, run.InvoiceID, "Recovered via dunning resolution"); err != nil {
				slog.WarnContext(r.Context(), "failed to record recovered payment after dunning resolution", "invoice_id", run.InvoiceID, "error", err)
			}
		case domain.ResolutionInvoiceVoided:
			// Void through the invoice SERVICE (single void writer): status flip
			// + atomic consumed-credit reversal + tax reversal + in-flight guard
			// + single-writer invoice.voided event. The PI-cancel below is gated
			// on the void SUCCEEDING — otherwise an in-flight invoice (which the
			// service's guard refuses to void) would still get its live PI
			// canceled, defeating the guard. It runs only after a confirmed void.
			if h.invoiceVoider == nil {
				slog.WarnContext(r.Context(), "invoice voider unwired; skipping dunning manual-resolve void", "invoice_id", run.InvoiceID)
				break
			}
			voided, err := h.invoiceVoider.Void(r.Context(), tenantID, run.InvoiceID)
			if err != nil {
				slog.WarnContext(r.Context(), "failed to void invoice after dunning resolution; skipping PI cancel", "invoice_id", run.InvoiceID, "error", err)
				break
			}
			// Stop collection at Stripe (only after a confirmed void):
			// expire live Checkout sessions, then cancel the PI where
			// cancelable. Credit reversal already happened inside Void.
			if h.paymentCancel != nil {
				h.paymentCancel.StopCollection(r.Context(), tenantID, voided)
			}
		}
	}

	respond.JSON(w, r, http.StatusOK, run)
}
