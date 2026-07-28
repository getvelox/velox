package payment

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/stripe/stripe-go/v82"

	"github.com/sagarsuperuser/velox/internal/api/respond"
	"github.com/sagarsuperuser/velox/internal/audit"
	veloxauth "github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

// PublicPaymentCustomerReader is the narrow hook the public handler
// uses to fetch a customer with PII fields decrypted. The customer
// package owns the encrypt-at-rest wrapper around display_name /
// email; reading through this interface (instead of joining the
// customers table directly) keeps decryption centralised and stops
// the public payment page from rendering raw `enc:…` ciphertext.
type PublicPaymentCustomerReader interface {
	Get(ctx context.Context, tenantID, id string) (domain.Customer, error)
}

// StripeCustomerEnsurer resolves — or lazily creates — the Stripe Customer for
// a Velox customer. The public first-payment-method flow needs this: a customer
// setting up their first card legitimately has no Stripe Customer yet, so
// requiring a pre-existing customers.stripe_customer_id dead-ends exactly the
// case this email exists to serve. Every sibling payment-setup path (operator
// "Add payment method", hosted-invoice Pay) already creates it on demand via
// this same helper. Satisfied by paymentmethods.StripeAdapter.EnsureStripeCustomer
// (which is built to run on public token-authenticated requests).
type StripeCustomerEnsurer interface {
	EnsureStripeCustomer(ctx context.Context, tenantID, customerID string) (string, error)
}

// PublicPaymentHandler serves tokenized payment update endpoints.
// These routes require NO auth — the token itself is the credential.
//
// Mode routing: these endpoints are anonymous by design, so there's no
// API-key livemode on the incoming request. The token's referenced
// invoice determines the mode — Token.Validate JOINs invoices to
// resolve livemode in the same query, so the validated token already
// carries everything we need to open a properly-scoped TxTenant for
// follow-on reads. RLS stays as defense-in-depth past the token gate.
type PublicPaymentHandler struct {
	tokens    *TokenService
	db        *postgres.DB
	clients   *StripeClients
	customers PublicPaymentCustomerReader
	ensurer   StripeCustomerEnsurer
	returnURL string
	audit     AuditEmitter
}

// SetAuditLogger wires ADR-090 in-tx emission for the token consume/restore
// pair — the public payment-update flow's durable mutations.
func (h *PublicPaymentHandler) SetAuditLogger(a AuditEmitter) { h.audit = a }

func NewPublicPaymentHandler(tokens *TokenService, db *postgres.DB, clients *StripeClients, customers PublicPaymentCustomerReader, ensurer StripeCustomerEnsurer, returnURL string) *PublicPaymentHandler {
	if tokens == nil || !clients.Has() {
		return nil
	}
	return &PublicPaymentHandler{
		tokens:    tokens,
		db:        db,
		clients:   clients,
		customers: customers,
		ensurer:   ensurer,
		returnURL: returnURL,
	}
}

func (h *PublicPaymentHandler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/{token}", h.validateToken)
	r.Post("/{token}/checkout", h.createCheckoutSession)
	return r
}

type tokenValidateResponse struct {
	CustomerName  string `json:"customer_name"`
	InvoiceNumber string `json:"invoice_number"`
	// InvoiceStatus lets the page tell the truth about a terminal
	// invoice. The token outlives the invoice's collectible life (24h
	// validity, and the operator can void/settle at any moment), so
	// without it the page asked a customer to pay an invoice we had
	// already annulled — amount_due_cents survives a void by design
	// (the void reverses collection, it doesn't rewrite the figure).
	// Found on the FLOW D4 walk (VLX-000005, voided).
	InvoiceStatus  string               `json:"invoice_status"`
	AmountDueCents int64                `json:"amount_due_cents"`
	Currency       string               `json:"currency"`
	Branding       publicUpdateBranding `json:"branding"`
}

// publicUpdateBranding mirrors the hosted-invoice page's safe branding
// projection. The payment-update page is the RECOVERY funnel — a
// failed-payment customer lands here from an email; a page branded
// "Velox" instead of the company they actually pay reads as phishing
// and kills the recovery (audit: fe-operator + fe-enduser dupe).
type publicUpdateBranding struct {
	CompanyName string `json:"company_name,omitempty"`
	LogoURL     string `json:"logo_url,omitempty"`
	BrandColor  string `json:"brand_color,omitempty"`
	SupportURL  string `json:"support_url,omitempty"`
}

func (h *PublicPaymentHandler) validateToken(w http.ResponseWriter, r *http.Request) {
	rawToken := chi.URLParam(r, "token")
	if rawToken == "" {
		respond.BadRequest(w, r, "token is required")
		return
	}

	token, err := h.tokens.Validate(r.Context(), rawToken)
	if err != nil {
		// Validate's JOIN to invoices means a deleted-invoice token
		// also lands here as "invalid or expired" — operator-friendly
		// 401 instead of a downstream 500.
		respond.Error(w, r, http.StatusUnauthorized, "authentication_error", "invalid_token",
			"invalid or expired token")
		return
	}

	// All follow-on reads run under TxTenant scoped to the token's
	// resolved (tenant, livemode). RLS stays as defense-in-depth past
	// the token gate — a bug in a WHERE clause cannot return another
	// tenant's row. The token resolves the (tenant, livemode) pair
	// authoritatively at validate time (livemode comes from invoices
	// via the JOIN), so no second bypass-required lookup is needed.
	scopedCtx := postgres.WithLivemode(r.Context(), token.Livemode)
	tx, err := h.db.BeginTx(scopedCtx, postgres.TxTenant, token.TenantID)
	if err != nil {
		slog.ErrorContext(scopedCtx, "public payment: begin tx", "error", err)
		respond.InternalError(w, r)
		return
	}
	defer postgres.Rollback(tx)

	var invoiceNumber, currency, invoiceStatus string
	var amountDueCents int64
	err = tx.QueryRowContext(scopedCtx, `
		SELECT invoice_number, amount_due_cents, currency, status FROM invoices WHERE id = $1
	`, token.InvoiceID).Scan(&invoiceNumber, &amountDueCents, &currency, &invoiceStatus)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Validate's JOIN already proved the invoice exists, so
			// ErrNoRows here means it was deleted between Validate and
			// this read — race. Treat as stale link.
			respond.Error(w, r, http.StatusNotFound, "invalid_request_error", "invoice_not_found",
				"this payment link is no longer valid — the invoice it references is unavailable")
			return
		}
		slog.ErrorContext(scopedCtx, "public payment: failed to fetch invoice", "invoice_id", token.InvoiceID, "error", err)
		respond.InternalError(w, r)
		return
	}

	// Customer name comes from the customer service so display_name
	// arrives decrypted. Reading customers.display_name directly here
	// would surface the encrypted ciphertext (`enc:…`) to the public
	// payment page — same architectural issue we fixed for the
	// testclock attached-customers panel.
	cust, err := h.customers.Get(scopedCtx, token.TenantID, token.CustomerID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			respond.Error(w, r, http.StatusNotFound, "invalid_request_error", "customer_not_found",
				"this payment link is no longer valid — the customer it references is unavailable")
			return
		}
		slog.ErrorContext(scopedCtx, "public payment: failed to fetch customer", "customer_id", token.CustomerID, "error", err)
		respond.InternalError(w, r)
		return
	}

	// Tenant branding for the page header — same safe projection the
	// hosted invoice exposes. Best-effort: a missing settings row just
	// renders the neutral fallback header.
	var branding publicUpdateBranding
	_ = tx.QueryRowContext(scopedCtx, `
		SELECT COALESCE(company_name,''), COALESCE(logo_url,''), COALESCE(brand_color,''), COALESCE(support_url,'')
		FROM tenant_settings WHERE tenant_id = $1
	`, token.TenantID).Scan(&branding.CompanyName, &branding.LogoURL, &branding.BrandColor, &branding.SupportURL)

	respond.JSON(w, r, http.StatusOK, tokenValidateResponse{
		CustomerName:   cust.DisplayName,
		InvoiceNumber:  invoiceNumber,
		InvoiceStatus:  invoiceStatus,
		AmountDueCents: amountDueCents,
		Currency:       currency,
		Branding:       branding,
	})
}

type checkoutResponse struct {
	URL string `json:"url"`
}

func (h *PublicPaymentHandler) createCheckoutSession(w http.ResponseWriter, r *http.Request) {
	rawToken := chi.URLParam(r, "token")
	if rawToken == "" {
		respond.BadRequest(w, r, "token is required")
		return
	}

	token, err := h.tokens.Validate(r.Context(), rawToken)
	if err != nil {
		respond.Error(w, r, http.StatusUnauthorized, "authentication_error", "invalid_token",
			"invalid or expired token")
		return
	}

	// Scope ctx to the token's resolved (tenant, livemode) for all
	// downstream tenant-scoped work, including the Stripe client lookup.
	scopedCtx := postgres.WithLivemode(r.Context(), token.Livemode)

	// Resolve — or lazily create — the customer's Stripe Customer. A customer
	// setting up their FIRST payment method (the whole point of this email) has
	// no customers.stripe_customer_id yet; EnsureStripeCustomer creates one on
	// demand and persists it, the same as the operator "Add payment method" and
	// hosted-invoice Pay paths. Pre-fix this read stripe_customer_id directly
	// and dead-ended with "customer does not have a Stripe payment setup" — for
	// the one flow whose job is to create that setup.
	//
	// This runs BEFORE the token is consumed (below), so a recoverable failure
	// here — e.g. Stripe not yet connected for this mode — does NOT burn the
	// single-use link: the customer retries the same email once it's fixed.
	// EnsureStripeCustomer is idempotent (short-circuits on the persisted id),
	// so a replay/retry doesn't mint duplicate Stripe Customers.
	stripeCustomerID, err := h.ensurer.EnsureStripeCustomer(scopedCtx, token.TenantID, token.CustomerID)
	if err != nil {
		slog.ErrorContext(scopedCtx, "public payment: ensure stripe customer",
			"customer_id", token.CustomerID, "error", err)
		// Customer-facing endpoint — TIER 2 sanitization (ADR-026): neutral
		// copy + reference id only, never operator/config detail.
		respond.Error(w, r, http.StatusBadGateway, "api_error", "stripe_error",
			"We couldn't start the payment update right now. Please contact your billing administrator if the problem persists.")
		return
	}

	sc := h.clients.For(scopedCtx, token.TenantID, token.Livemode)
	if sc == nil {
		// EnsureStripeCustomer just succeeded for this (tenant, mode), so a nil
		// client here would be a config change between the two calls.
		respond.Error(w, r, http.StatusBadGateway, "api_error", "stripe_error",
			"We couldn't start the payment update right now. Please contact your billing administrator if the problem persists.")
		return
	}

	// Atomically consume the single-use token NOW — immediately before the
	// side-effecting Checkout-session create. Validate's `used_at IS NULL` is
	// only a read, so two concurrent requests both pass it; this conditional
	// UPDATE is the compare-and-swap that lets exactly one open a setup session
	// (a loser or a replay sees consumed=false → invalid-token). Consuming here
	// rather than up-front means only a genuine external Stripe failure past
	// this point burns the token — not a recoverable precondition.
	// The token IS a customer credential — stamp the customer actor so the
	// consume/restore audit rows attribute to the customer (ADR-090 D16).
	scopedCtx = veloxauth.WithCustomerActor(scopedCtx, token.CustomerID)
	var consumeEmit func(tx *sql.Tx) error
	if h.audit != nil {
		consumeEmit = func(tx *sql.Tx) error {
			return h.audit.LogInTx(scopedCtx, tx, audit.Entry{
				Action:       domain.AuditActionUpdate,
				ResourceType: "customer",
				ResourceID:   token.CustomerID,
				Metadata: map[string]any{
					"action":     "payment_update_checkout_started",
					"invoice_id": token.InvoiceID,
				},
			})
		}
	}
	consumed, err := h.tokens.ConsumeAudited(scopedCtx, token.TenantID, rawToken, consumeEmit)
	if err != nil {
		slog.ErrorContext(scopedCtx, "public payment: consume token", "error", err)
		respond.InternalError(w, r)
		return
	}
	if !consumed {
		respond.Error(w, r, http.StatusUnauthorized, "authentication_error", "invalid_token",
			"invalid or expired token")
		return
	}

	// Build the return URL: the public, unauthenticated /payment-method-added
	// SPA page — the same terminal the operator-driven "Add payment method"
	// flow uses (paymentmethods.setupCompletePath). A customer arriving from an
	// email link has NO portal session, so the fallback MUST be a public route;
	// the legacy /customers/{id} fallback was an operator ProtectedRoute that
	// bounced the customer to login.
	returnURL := h.returnURL
	if returnURL == "" {
		returnURL = "http://localhost:5173/payment-method-added"
	}
	// Success vs. cancel get distinct ?status= so the landing page renders the
	// right copy (mirrors the paymentmethods setup-session flow); Stripe calls
	// one of the two depending on outcome.
	successURL := appendStatusQuery(returnURL, "success")
	cancelURL := appendStatusQuery(returnURL, "cancel")

	// Create a Checkout Session in setup mode for new payment method
	sess, err := sc.V1CheckoutSessions.Create(r.Context(), &stripe.CheckoutSessionCreateParams{
		Customer:           stripe.String(stripeCustomerID),
		Mode:               stripe.String(string(stripe.CheckoutSessionModeSetup)),
		PaymentMethodTypes: stripe.StringSlice([]string{"card"}),
		SuccessURL:         stripe.String(successURL),
		CancelURL:          stripe.String(cancelURL),
		// Stamp velox_customer_id on the underlying SetupIntent, not just the
		// session: Stripe Checkout does NOT copy session metadata onto the
		// SetupIntent, so without this the setup_intent.succeeded webhook
		// arrives with empty metadata and must resolve the customer by Stripe
		// id — which races the customer↔Stripe-id link-write and can drop the
		// saved card. setup_intent_data.metadata makes the event self-resolving.
		SetupIntentData: &stripe.CheckoutSessionCreateSetupIntentDataParams{
			Metadata: map[string]string{
				"velox_customer_id": token.CustomerID,
				"velox_tenant_id":   token.TenantID,
				"velox_invoice_id":  token.InvoiceID,
				"velox_purpose":     purposePaymentUpdateToken,
			},
		},
		Params: stripe.Params{
			Metadata: map[string]string{
				"velox_customer_id": token.CustomerID,
				"velox_tenant_id":   token.TenantID,
				"velox_invoice_id":  token.InvoiceID,
				"velox_purpose":     purposePaymentUpdateToken,
			},
		},
	})
	if err != nil {
		slog.ErrorContext(r.Context(), "public payment: failed to create checkout session",
			"customer_id", token.CustomerID, "error", err)
		// The token was consumed above (the CAS is what makes two
		// concurrent clicks yield ONE session) — but the side effect it
		// was consumed FOR just failed. Restore it so a transient Stripe
		// error doesn't permanently kill the customer's emailed link;
		// the recency guard inside Restore bounds this to the
		// consume→create window of this request.
		var restoreEmit func(tx *sql.Tx) error
		if h.audit != nil {
			restoreEmit = func(tx *sql.Tx) error {
				return h.audit.LogInTx(scopedCtx, tx, audit.Entry{
					Action:       domain.AuditActionUpdate,
					ResourceType: "customer",
					ResourceID:   token.CustomerID,
					Metadata: map[string]any{
						"action":     "payment_update_checkout_restored",
						"invoice_id": token.InvoiceID,
					},
				})
			}
		}
		// The paired restore row keeps the log truthful: without it the
		// consume row would claim a checkout that never produced a session.
		if restoreErr := h.tokens.RestoreAudited(scopedCtx, token.TenantID, rawToken, restoreEmit); restoreErr != nil {
			slog.ErrorContext(scopedCtx, "public payment: restore token after create failure", "error", restoreErr)
		}
		// Customer-facing endpoint — TIER 2 sanitization (ADR-026):
		// even "Stripe SDK error" framing leaks operator context.
		// End customers should see only neutral copy + reference ID.
		respond.Error(w, r, http.StatusBadGateway, "api_error", "stripe_error",
			"We couldn't start the payment update right now. Please try the link again in a few minutes, or contact support if the problem persists.")
		return
	}

	// Token was already atomically consumed above (before the session was
	// created), so there's no post-success mark step here.

	slog.InfoContext(r.Context(), "public payment update session created",
		"customer_id", token.CustomerID,
		"invoice_id", token.InvoiceID,
		"session_id", sess.ID,
	)

	respond.JSON(w, r, http.StatusCreated, checkoutResponse{
		URL: sess.URL,
	})
}

// appendStatusQuery adds ?status=<v> (or &status=<v>) to the Stripe return URL
// so the public /payment-method-added page renders success vs. cancel copy.
// Inlined rather than pulling net/url: inputs are operator config + a constant.
// Mirrors paymentmethods.appendQuery.
func appendStatusQuery(rawURL, status string) string {
	sep := "?"
	if strings.Contains(rawURL, "?") {
		sep = "&"
	}
	return rawURL + sep + "status=" + status
}
