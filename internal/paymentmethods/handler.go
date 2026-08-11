package paymentmethods

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/sagarsuperuser/velox/internal/api/respond"
	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/errs"
)

// Handler exposes the operator-side payment-method surface under
// /v1/customers/{customer_id}/payment-methods: list / set-default /
// detach, plus a "send setup link" affordance (email or copy-link) that
// mints a Stripe Checkout setup URL the operator hands to the customer —
// card data goes browser → Stripe, never the operator dashboard.
//
// The customer lookup (resolves the recipient email + display name) and
// the email sender are optional: a Handler without them refuses the
// email path with 503 and still serves list/setDefault/detach.
type Handler struct {
	svc            *Service
	customerLookup CustomerLookup
	emailer        SetupLinkEmailer
	auditLogger    AuditWriter
	cooldown       CooldownGate
}

// CustomerLookup resolves the recipient's email + display name for
// the operator "send setup email" flow. Implemented by
// *customer.PostgresStore via a thin adapter in router.go.
type CustomerLookup interface {
	GetForSetupLink(ctx context.Context, tenantID, customerID string) (email, displayName string, err error)
}

// SetupLinkEmailer dispatches the operator-initiated "add a payment
// method" email. Satisfied by *email.Sender. operatorNote may be
// empty — the template falls back to a default body when so.
type SetupLinkEmailer interface {
	SendPaymentSetupLink(ctx context.Context, tenantID, to, customerName, operatorNote, setupURL string) error
}

// CooldownGate enforces a per-customer minimum interval between
// send-setup-email calls. Defends against operator double-click and
// abusive automation. Satisfied by *middleware.RateLimiter via its
// AllowKey method. Optional: nil = no cooldown (local dev / tests).
type CooldownGate interface {
	AllowKey(ctx context.Context, key string) (remaining int, resetAt time.Time, allowed bool)
}

// AuditWriter is declared in service.go — operator handler reuses
// the same narrow interface.

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// SetCustomerLookup wires the customer-lookup dependency. Required
// for the operator send-setup-email endpoint; nil = endpoint refuses
// with 503.
func (h *Handler) SetCustomerLookup(c CustomerLookup) { h.customerLookup = c }

// SetEmailer wires the email sender. Required for operator
// send-setup-email; nil = endpoint refuses with 503.
func (h *Handler) SetEmailer(e SetupLinkEmailer) { h.emailer = e }

// SetAuditLogger wires audit on operator-initiated actions.
func (h *Handler) SetAuditLogger(a AuditWriter) { h.auditLogger = a }

// SetCooldown wires the per-customer cooldown gate on
// send-setup-email. Without it, an operator double-click could send
// two emails ~30s apart. Optional in tests / local dev.
func (h *Handler) SetCooldown(c CooldownGate) { h.cooldown = c }

// OperatorRoutes mounts the operator-side PM surface under
// /v1/customers/{customer_id}/payment-methods. Same Service backs both
// surfaces — the only difference is where customer_id comes from
// (portal session ctx vs URL path) and how auth is gated (portal
// session vs operator API key). Industry parity: Chargebee, Lago, Orb
// all expose operator-side list/setDefault/detach + a "send setup
// link" affordance. Card-data entry stays customer-side via Stripe
// Checkout, keeping every tenant in PCI SAQ-A scope. See
// docs/adr/ for the PCI-scope rationale.
func (h *Handler) OperatorRoutes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", h.operatorList)
	r.Post("/{pmID}/default", h.operatorSetDefault)
	r.Delete("/{pmID}", h.operatorDetach)
	// Mint a Stripe Checkout setup-session URL that the operator can
	// hand to the customer (copy, email, magic-link wrap) so the
	// customer can add a card themselves. The card data never touches
	// the operator's dashboard — it goes browser → Stripe.js → Stripe.
	r.Post("/setup-session", h.operatorCreateSetupSession)
	// Send the setup link as an email from Velox's branded sender to
	// the customer's email on file. Industry-primary path (Stripe
	// "Send Payment Method" + Chargebee "Request Payment Method" both
	// default to email); copy-link via setup-session above stays the
	// secondary path for non-email channels.
	r.Post("/send-setup-email", h.operatorSendSetupEmail)
	return r
}

type pmResponse struct {
	ID        string    `json:"id"`
	Type      string    `json:"type"`
	CardBrand string    `json:"card_brand,omitempty"`
	CardLast4 string    `json:"card_last4,omitempty"`
	CardExpMo int       `json:"card_exp_month,omitempty"`
	CardExpYr int       `json:"card_exp_year,omitempty"`
	IsDefault bool      `json:"is_default"`
	CreatedAt time.Time `json:"created_at"`
}

func toResp(pm PaymentMethod) pmResponse {
	return pmResponse{
		ID: pm.ID, Type: pm.Type,
		CardBrand: pm.CardBrand, CardLast4: pm.CardLast4,
		CardExpMo: pm.CardExpMonth, CardExpYr: pm.CardExpYear,
		IsDefault: pm.IsDefault, CreatedAt: pm.CreatedAt,
	}
}

type setupSessionRequest struct {
	ReturnURL string `json:"return_url,omitempty"`
}

type setupSessionResponse struct {
	URL       string `json:"url"`
	SessionID string `json:"session_id"`
}

// operatorIdentity reads tenant from the auth ctx and customer from
// the URL path. The path arg name is "customer_id" to match the
// existing /v1/customers/{customer_id}/... convention used elsewhere
// (coupons, usage). Returns ok=false on either missing piece so the
// handler can 400 rather than silently fall through.
func operatorIdentity(r *http.Request) (tenantID, customerID string, ok bool) {
	tenantID = auth.TenantID(r.Context())
	customerID = chi.URLParam(r, "customer_id")
	return tenantID, customerID, tenantID != "" && customerID != ""
}

func (h *Handler) operatorList(w http.ResponseWriter, r *http.Request) {
	tenantID, customerID, ok := operatorIdentity(r)
	if !ok {
		respond.BadRequest(w, r, "missing tenant or customer id")
		return
	}
	// Existence check: List legally returns empty for an unknown customer,
	// so without this a typo'd or cross-tenant id read as "no cards on
	// file" (200 []) instead of "no such customer" (the walked 200-empty-
	// shell class; post-billing closeout, open item 2). Reuses the same
	// lookup the setup-email path already 404s through.
	if h.customerLookup != nil {
		if _, _, err := h.customerLookup.GetForSetupLink(r.Context(), tenantID, customerID); err != nil {
			if errors.Is(err, errs.ErrNotFound) {
				respond.NotFound(w, r, "customer")
				return
			}
			respond.InternalError(w, r)
			return
		}
	}
	pms, err := h.svc.List(r.Context(), tenantID, customerID)
	if err != nil {
		respond.InternalError(w, r)
		return
	}
	out := make([]pmResponse, 0, len(pms))
	for _, pm := range pms {
		out = append(out, toResp(pm))
	}
	respond.List(w, r, out, len(out))
}

func (h *Handler) operatorSetDefault(w http.ResponseWriter, r *http.Request) {
	tenantID, customerID, ok := operatorIdentity(r)
	if !ok {
		respond.BadRequest(w, r, "missing tenant or customer id")
		return
	}
	pmID := chi.URLParam(r, "pmID")
	if pmID == "" {
		respond.BadRequest(w, r, "missing payment method id")
		return
	}
	pm, err := h.svc.SetDefault(r.Context(), tenantID, customerID, pmID)
	if err != nil {
		if errors.Is(err, errs.ErrNotFound) {
			respond.NotFound(w, r, "payment_method")
			return
		}
		respond.InternalError(w, r)
		return
	}
	respond.JSON(w, r, http.StatusOK, toResp(pm))
}

func (h *Handler) operatorDetach(w http.ResponseWriter, r *http.Request) {
	tenantID, customerID, ok := operatorIdentity(r)
	if !ok {
		respond.BadRequest(w, r, "missing tenant or customer id")
		return
	}
	pmID := chi.URLParam(r, "pmID")
	if pmID == "" {
		respond.BadRequest(w, r, "missing payment method id")
		return
	}
	pm, err := h.svc.Detach(r.Context(), tenantID, customerID, pmID)
	if err != nil {
		if errors.Is(err, errs.ErrNotFound) {
			respond.NotFound(w, r, "payment_method")
			return
		}
		respond.InternalError(w, r)
		return
	}
	respond.JSON(w, r, http.StatusOK, toResp(pm))
}

// sendSetupEmailRequest is the operator-initiated send-email payload.
// Note is optional — when present it prefaces the email body with
// the operator's own context ("Your card expired", "We're moving you
// to monthly billing"); when absent the template uses a generic body.
type sendSetupEmailRequest struct {
	Note string `json:"note,omitempty"`
}

// sendSetupEmailResponse echoes the recipient address back to the
// dashboard so the toast can read "Setup link sent to billing@acme.com"
// — useful confirmation when a customer has multiple addresses on
// different surfaces (profile vs billing_profile vs portal session).
type sendSetupEmailResponse struct {
	To      string `json:"to"`
	Subject string `json:"subject"`
}

// operatorSendSetupEmail mints a Stripe Checkout setup-session and
// enqueues the customer email in one atomic operator action.
// Industry-primary path (matches Stripe "Send Payment Method",
// Chargebee "Request Payment Method"). The secondary copy-link path
// remains via operatorCreateSetupSession.
//
// Resilience: in production the email is routed through the email
// outbox — the operator's request succeeds the moment a row lands in
// the outbox (the dispatcher then handles SMTP delivery + retries +
// DLQ). The operator sees "Setup link queued for {email}", the
// row reaches the customer's inbox seconds later. SMTP outages
// don't fail the operator's action; they show up as stuck-pending
// rows on the email_outbox dashboard. Bringing this path into the
// outbox parity matches every other email producer (invoice /
// dunning / receipt / magic-link / etc.).
//
// Failure modes:
//   - Customer has no email → 422 with code 'missing_email'
//   - Email/customer lookup not wired → 503 service_unavailable
//   - Enqueue (or direct SMTP, when outbox disabled) fails → 502 email_enqueue_failed
func (h *Handler) operatorSendSetupEmail(w http.ResponseWriter, r *http.Request) {
	tenantID, customerID, ok := operatorIdentity(r)
	if !ok {
		respond.BadRequest(w, r, "missing tenant or customer id")
		return
	}
	if h.customerLookup == nil || h.emailer == nil {
		respond.Error(w, r, http.StatusServiceUnavailable, "api_error",
			"email_unavailable",
			"email setup-link sending is not configured on this Velox instance")
		return
	}
	// Cooldown — 1 send per (tenant, customer) per 60s. Defends
	// against operator double-click and abusive automation. Returns
	// 429 with retry-after when fired; the operator's "Sending…" UI
	// state already prevents the common single-page double-click,
	// but a refresh-then-click or two operators on the same customer
	// could still race without this server-side gate.
	if h.cooldown != nil {
		key := "setup-link:" + tenantID + ":" + customerID
		_, resetAt, allowed := h.cooldown.AllowKey(r.Context(), key)
		if !allowed {
			retryIn := int(time.Until(resetAt).Seconds())
			if retryIn < 1 {
				retryIn = 1
			}
			w.Header().Set("Retry-After", strconv.Itoa(retryIn))
			respond.Error(w, r, http.StatusTooManyRequests, "rate_limit_error",
				"cooldown_active",
				"a setup link was sent to this customer recently — wait before sending again")
			return
		}
	}
	var req sendSetupEmailRequest
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}
	note := strings.TrimSpace(req.Note)
	if len(note) > 2000 {
		respond.Error(w, r, http.StatusUnprocessableEntity, "invalid_request_error",
			"note_too_long", "note must be 2000 characters or fewer")
		return
	}

	email, displayName, err := h.customerLookup.GetForSetupLink(r.Context(), tenantID, customerID)
	if err != nil {
		if errors.Is(err, errs.ErrNotFound) {
			respond.NotFound(w, r, "customer")
			return
		}
		slog.ErrorContext(r.Context(), "operator setup email: customer lookup", "error", err)
		respond.InternalError(w, r)
		return
	}
	if email == "" {
		respond.Error(w, r, http.StatusUnprocessableEntity, "invalid_state",
			"missing_email",
			"this customer has no email on file — add one before sending a setup link")
		return
	}
	if displayName == "" {
		displayName = "there" // "Hi there," — fallback that reads naturally.
	}

	url, sessionID, err := h.svc.CreateSetupSession(r.Context(), tenantID, customerID, "")
	if err != nil {
		respond.FromError(w, r, err, "payment_method")
		return
	}

	if err := h.emailer.SendPaymentSetupLink(r.Context(), tenantID, email, displayName, note, url); err != nil {
		// In production this is an outbox enqueue (writes a
		// pending row that the dispatcher drains). Only path that
		// can fail at this layer is a DB write error — SMTP issues
		// surface later as a pending row with last_error populated.
		// When the outbox is disabled (legacy direct-SMTP path),
		// this same error covers SMTP misconfigure / handshake
		// failures.
		slog.ErrorContext(r.Context(), "operator setup email: enqueue",
			"customer_id", customerID, "to", email, "session_id", sessionID, "error", err)
		respond.Error(w, r, http.StatusBadGateway, "api_error",
			"email_enqueue_failed",
			"failed to queue the setup link email — please retry; if it persists, check the email outbox dashboard")
		return
	}

	if h.auditLogger != nil {
		// No recipient address in the append-only row (GDPR erasure) — the
		// row links to the customer, and the email outbox is the delivery
		// record with the actual address.
		_ = h.auditLogger.Log(r.Context(), tenantID, "update", "customer", customerID, displayName, map[string]any{
			"action":     "setup_link_sent",
			"session_id": sessionID,
		})
	}

	respond.JSON(w, r, http.StatusAccepted, sendSetupEmailResponse{
		To:      email,
		Subject: "Add a payment method",
	})
}

// operatorCreateSetupSession mints a Stripe Checkout-mode SetupSession
// URL the operator can hand to the customer. Card capture happens
// browser → Stripe.js → Stripe; the operator's dashboard never sees
// card data. Industry-standard "send setup link" affordance.
// Response: { url, session_id }. The url is single-use, expires per
// Stripe's session policy (~24h).
func (h *Handler) operatorCreateSetupSession(w http.ResponseWriter, r *http.Request) {
	tenantID, customerID, ok := operatorIdentity(r)
	if !ok {
		respond.BadRequest(w, r, "missing tenant or customer id")
		return
	}
	var req setupSessionRequest
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}
	url, sessionID, err := h.svc.CreateSetupSession(r.Context(), tenantID, customerID, req.ReturnURL)
	if err != nil {
		respond.FromError(w, r, err, "payment_method")
		return
	}
	respond.JSON(w, r, http.StatusCreated, setupSessionResponse{URL: url, SessionID: sessionID})
}
