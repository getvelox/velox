package paymentmethods

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

// StripeAPI is the narrow subset of Stripe we need. Declared here instead
// of depending on internal/payment so the paymentmethods package can be
// tested with a fake and so internal/payment doesn't gain a reverse
// dependency.
type StripeAPI interface {
	// CreateSetupCheckoutSession creates a Stripe Checkout Session in
	// setup mode and returns a hosted URL the customer can redirect to.
	// successURL and cancelURL are separate so the SPA can render
	// different copy based on the outcome (?status=success vs cancel).
	// Used by the default web-v2 portal UI, which redirects rather than
	// embedding Stripe Elements.
	CreateSetupCheckoutSession(ctx context.Context, stripeCustomerID, successURL, cancelURL string, metadata map[string]string) (checkoutURL, sessionID string, err error)

	// EnsureStripeCustomer returns the existing Stripe customer ID from
	// customer_payment_setups, or creates one if absent and writes it back
	// to the setup row. Needed because a customer might not have a Stripe
	// customer yet when they first land on the portal.
	EnsureStripeCustomer(ctx context.Context, tenantID, customerID string) (string, error)

	// DetachPaymentMethod calls Stripe's detach endpoint. Best-effort — if
	// Stripe has already detached (e.g. card expired and Stripe removed
	// it), we still want to mark the local row detached.
	DetachPaymentMethod(ctx context.Context, stripePaymentMethodID string) error

	// SetDefaultPaymentMethod updates the Stripe Customer's
	// invoice_settings.default_payment_method. Required so any Stripe-side
	// off-session auto-charge uses the operator's chosen card, not the one
	// Stripe last had on file. Pre-2026-05-29 SetDefault flipped the local
	// row only; Stripe's default stayed stale. The adapter looks up the
	// Stripe Customer ID via its customerLink (same pattern as
	// EnsureStripeCustomer). Best-effort: returns error if Stripe is
	// unreachable; caller logs + audits and the operator action still
	// commits (local-wins, per Lago / Recurly / Chargebee).
	SetDefaultPaymentMethod(ctx context.Context, tenantID, customerID, stripePaymentMethodID string) error

	// FetchPaymentMethodCard looks up card metadata for a Stripe PM —
	// brand / last4 / exp / fingerprint. Used by the webhook handler
	// when persisting a newly attached PM. Fingerprint drives
	// dedupe-on-attach (see PostgresStore.Upsert).
	FetchPaymentMethodCard(ctx context.Context, stripePaymentMethodID string) (CardMetadata, error)
}

// CardMetadata bundles the card facts the Stripe adapter returns at
// attach time. Fingerprint is the dedupe key — Stripe's stable hash
// of the card number (CVC + expiry don't affect it). Empty when the
// PM isn't a card type (legacy / bank account / etc.).
type CardMetadata struct {
	Brand       string
	Last4       string
	ExpMonth    int
	ExpYear     int
	Fingerprint string
}

// PaymentSetupSummaryWriter — REMOVED. The 1:1 customer_payment_setups
// summary table was retired in migration 0097; paymentmethods.Service
// no longer writes any denorm cache. Single source of truth:
//   - customers.stripe_customer_id (the Stripe Customer mapping)
//   - payment_methods rows (canonical per-PM, is_default flag tracks
//     primary, detached_at marks removal for audit)
//
// Billing engine reads PaymentReadiness via a thin adapter that
// queries customers + payment_methods directly. No more dual-write.

type Service struct {
	store         Store
	stripe        StripeAPI
	portalBaseURL string // SPA base URL; setup-session return URL is portalBaseURL + setupCompletePath when caller doesn't pass one
	auditLogger   AuditWriter
}

// setupCompletePath is the SPA route Stripe Checkout redirects to
// after a successful (or canceled) setup-session. Public, no auth —
// the customer landing here got there from an email link, they don't
// have a portal session. The page reads ?status=success|cancel from
// the query string for the appropriate copy.
const setupCompletePath = "/payment-method-added"

// AuditWriter is the narrow audit surface paymentmethods needs.
// Declared here (not imported from internal/audit) so the package
// stays decoupled and testable with a fake. Production wires
// *audit.Logger via SetAuditLogger in router.go.
type AuditWriter interface {
	Log(ctx context.Context, tenantID, action, resourceType, resourceID, resourceLabel string, metadata map[string]any) error
}

// NewService — `summary` parameter kept for back-compat with the
// existing router.go wiring (passes customerStore which used to
// satisfy PaymentSetupSummaryWriter). Ignored. Remove this parameter
// once the writer interface is fully retired across all builders.
func NewService(store Store, stripe StripeAPI, _ any) *Service {
	return &Service{store: store, stripe: stripe}
}

// SetPortalBaseURL sets the SPA base URL used to compose Stripe
// Checkout return URLs. Production wires CUSTOMER_PORTAL_URL via
// router.go; local dev falls through to http://localhost:5173 when
// unset. Without this, Stripe redirects the customer to a hardcoded
// default that may not match the deployment's SPA host.
func (s *Service) SetPortalBaseURL(u string) {
	s.portalBaseURL = strings.TrimRight(strings.TrimSpace(u), "/")
}

// SetAuditLogger wires the audit-log writer. Without it, paymentmethods
// mutations (attach via webhook, set-default, detach, setup-session
// creation) succeed silently — operator Activity feed and AuditLog page
// would miss every card-on-file change. Optional: nil = no audit row
// written, all other behavior unchanged.
func (s *Service) SetAuditLogger(a AuditWriter) {
	s.auditLogger = a
}

// List returns active PMs for (tenantID, customerID). Ordered default
// first for UI convenience.
func (s *Service) List(ctx context.Context, tenantID, customerID string) ([]PaymentMethod, error) {
	return s.store.List(ctx, tenantID, customerID)
}

// CustomersWithDefault reports which of customerIDs have an active default
// payment method, in one read (see Store.CustomersWithDefault).
func (s *Service) CustomersWithDefault(ctx context.Context, tenantID string, customerIDs []string) (map[string]bool, error) {
	return s.store.CustomersWithDefault(ctx, tenantID, customerIDs)
}

// CreateSetupSession returns a hosted Stripe Checkout URL the customer can
// be redirected to for adding a new card without being charged. On
// success Stripe fires setup_intent.succeeded, which the webhook handler
// turns into a payment_methods row via AttachForWebhook.
func (s *Service) CreateSetupSession(ctx context.Context, tenantID, customerID, returnURL string) (checkoutURL, sessionID string, err error) {
	if tenantID == "" || customerID == "" {
		return "", "", errs.Required("tenant_id, customer_id")
	}
	stripeCustomerID, err := s.stripe.EnsureStripeCustomer(ctx, tenantID, customerID)
	if err != nil {
		return "", "", fmt.Errorf("ensure stripe customer: %w", err)
	}
	// Build the default return URL: portalBaseURL + setupCompletePath
	// (the public SPA success page). When the SPA base isn't configured
	// (local dev / tests), fall through to the dev SPA host. The path
	// must point at a route that EXISTS in the SPA — the legacy
	// hardcoded "/customer-portal" was a non-existent route, leaving
	// the customer on a blank page after Stripe Checkout success.
	if returnURL == "" {
		base := s.portalBaseURL
		if base == "" {
			base = "http://localhost:5173"
		}
		returnURL = base + setupCompletePath
	}
	// Split success vs. cancel into separate URLs via ?status= so the
	// landing page can render the right copy without a separate route.
	// Stripe Checkout calls one of the two URLs depending on outcome.
	successURL := appendQuery(returnURL, "status=success")
	cancelURL := appendQuery(returnURL, "status=cancel")
	metadata := map[string]string{
		"velox_tenant_id":   tenantID,
		"velox_customer_id": customerID,
		"velox_livemode":    livemodeLabel(ctx),
		"velox_purpose":     "portal_add_payment_method",
	}
	checkoutURL, sessionID, err = s.stripe.CreateSetupCheckoutSession(ctx, stripeCustomerID, successURL, cancelURL, metadata)
	if err != nil {
		// Nothing was minted — emit nothing. A failed Stripe call must not
		// leave evidence of a capability that does not exist.
		return "", "", err
	}
	// ADR-090 residual OWN-TX emission — legitimate here because there is no
	// business transaction to ride: the real mutation is EXTERNAL (the Stripe
	// Checkout session), and the only local write on this path
	// (customers.stripe_customer_id, inside EnsureStripeCustomer) happens on
	// the FIRST call for a customer and never again.
	//
	// So the row is emitted UNCONDITIONALLY for the operator action, NOT gated
	// on that incidental mapping write: minting this URL is a card-capture
	// CAPABILITY GRANT (anyone holding it can attach a payment method to this
	// customer), and gating on the first-call-only write would silently
	// un-audit every subsequent link for the same customer.
	//
	// The error PROPAGATES (never `_ =`): a capability the compliance log
	// cannot record must not be handed back to the caller. The un-returned
	// Stripe session is inert — it charges nothing and expires — and a retry
	// mints a fresh one.
	if s.auditLogger != nil {
		// No label: this service holds no customer reader, and inventing one
		// from request input would put an unverified value in an append-only
		// record. The dashboard falls back to the resource type and
		// deep-links resource_id.
		if err := s.auditLogger.Log(ctx, tenantID, domain.AuditActionUpdate, "customer", customerID, "", map[string]any{
			"action":     "setup_session_created",
			"session_id": sessionID,
		}); err != nil {
			return "", "", fmt.Errorf("audit setup session: %w", err)
		}
	}
	return checkoutURL, sessionID, nil
}

// appendQuery appends a key=value query fragment to the given URL,
// using '?' if no query exists or '&' otherwise. Used to add
// ?status=success / ?status=cancel without pulling net/url for what's
// a one-token append. We don't need full URL parsing here because
// the inputs come from operator config + the constant setupCompletePath.
func appendQuery(rawURL, kv string) string {
	if strings.Contains(rawURL, "?") {
		return rawURL + "&" + kv
	}
	return rawURL + "?" + kv
}

// SetDefault flips is_default atomically in payment_methods AND refreshes
// the customer_payment_setups summary row so billing sees the new card.
func (s *Service) SetDefault(ctx context.Context, tenantID, customerID, pmID string) (PaymentMethod, error) {
	pm, err := s.store.SetDefault(ctx, tenantID, customerID, pmID)
	if err != nil {
		return pm, err
	}

	// Push to Stripe (2026-05-29). Pre-fix this updated the local DB
	// only; Stripe Customer's `invoice_settings.default_payment_method`
	// stayed stale, and any Stripe-side off-session auto-charge picked
	// whatever Stripe last had — including a PM the operator had just
	// demoted. Best-effort: local save is source of truth (Lago/Recurly
	// /Chargebee), Stripe failure logs + writes a stripe_sync_error to
	// the audit row so ops can spot the divergence and retry.
	stripeSyncErr := ""
	if pm.StripePaymentMethodID != "" {
		if err := s.stripe.SetDefaultPaymentMethod(ctx, tenantID, customerID, pm.StripePaymentMethodID); err != nil {
			slog.WarnContext(ctx, "failed to sync default payment method to Stripe",
				"tenant_id", tenantID, "customer_id", customerID,
				"stripe_payment_method_id", pm.StripePaymentMethodID,
				"error", err)
			stripeSyncErr = err.Error()
		}
	}

	if s.auditLogger != nil {
		meta := map[string]any{
			"action":      "default_changed",
			"customer_id": customerID,
			"card_brand":  pm.CardBrand,
			"card_last4":  pm.CardLast4,
		}
		if stripeSyncErr != "" {
			meta["stripe_sync_error"] = stripeSyncErr
		}
		_ = s.auditLogger.Log(ctx, tenantID, "update", "payment_method", pm.ID, pmLabel(pm), meta)
	}
	return pm, nil
}

// Detach marks the PM detached both in Stripe and locally and, if it was
// the default, promotes the newest remaining active PM to default — the
// detach + promote run in a single store transaction so there is never a
// committed "active cards but no default" window. The promoted default is
// then best-effort synced to Stripe for display parity.
func (s *Service) Detach(ctx context.Context, tenantID, customerID, pmID string) (PaymentMethod, error) {
	pm, err := s.store.Get(ctx, tenantID, pmID)
	if err != nil {
		return PaymentMethod{}, err
	}
	if pm.CustomerID != customerID {
		return PaymentMethod{}, errs.ErrNotFound
	}

	if err := s.stripe.DetachPaymentMethod(ctx, pm.StripePaymentMethodID); err != nil {
		// Stripe returns 404 if already detached — treat that as success.
		// Any other error bubbles up. We use errors.Is on a best-effort
		// basis; most implementations wrap with enough context.
		if !errors.Is(err, errs.ErrNotFound) {
			return PaymentMethod{}, fmt.Errorf("stripe detach: %w", err)
		}
	}

	// Detach and promote a replacement default atomically. Previously these
	// were two separate transactions, so a crash — or, more plausibly, a
	// transient DB error on the promote — could leave the customer with
	// active cards but no default, silently gating auto-charge off until
	// the next card action. Now the detach rolls back too if the promote
	// fails: the "≥1 active card ⇒ exactly one default" invariant always
	// holds across the operation.
	detached, newDefault, err := s.store.DetachAndRebalance(ctx, tenantID, customerID, pmID)
	if err != nil {
		return PaymentMethod{}, err
	}

	if s.auditLogger != nil {
		_ = s.auditLogger.Log(ctx, tenantID, "delete", "payment_method", detached.ID, pmLabel(detached), map[string]any{
			"action":      "detached",
			"customer_id": customerID,
			"card_brand":  detached.CardBrand,
			"card_last4":  detached.CardLast4,
			"was_default": pm.IsDefault,
		})
	}

	// Sync the auto-promoted default to Stripe's
	// invoice_settings.default_payment_method. Best-effort + cosmetic: the
	// charge path reads Velox's default directly (ADR-053), so this no
	// longer decides which card is charged — it keeps Stripe-hosted
	// surfaces (dashboard, Checkout) showing the same default the operator
	// sees. A failure logs + audits a stripe_sync_error; the detach +
	// promotion still stand (local-wins, same posture as SetDefault).
	if newDefault != nil && newDefault.StripePaymentMethodID != "" {
		stripeSyncErr := ""
		if err := s.stripe.SetDefaultPaymentMethod(ctx, tenantID, customerID, newDefault.StripePaymentMethodID); err != nil {
			slog.WarnContext(ctx, "failed to sync auto-promoted default payment method to Stripe",
				"tenant_id", tenantID, "customer_id", customerID,
				"stripe_payment_method_id", newDefault.StripePaymentMethodID, "error", err)
			stripeSyncErr = err.Error()
		}
		if s.auditLogger != nil {
			meta := map[string]any{
				"action":      "default_changed",
				"customer_id": customerID,
				"card_brand":  newDefault.CardBrand,
				"card_last4":  newDefault.CardLast4,
				"trigger":     "auto_promote_on_detach",
			}
			if stripeSyncErr != "" {
				meta["stripe_sync_error"] = stripeSyncErr
			}
			_ = s.auditLogger.Log(ctx, tenantID, "update", "payment_method", newDefault.ID, pmLabel(*newDefault), meta)
		}
	}

	return detached, nil
}

// AttachForWebhook is AttachFromSetupIntent for payment.Stripe.HandleWebhook:
// it returns the card as the API shows it (domain.PaymentMethodView), which
// the payment_method.* webhooks carry, so payment/ does not have to know
// about PaymentMethod. It returns nil for a card already removed, which
// stays removed: the API does not list it, so no event may carry it.
func (s *Service) AttachForWebhook(ctx context.Context, tenantID, customerID, stripePaymentMethodID string, makeDefault bool) (*domain.PaymentMethodView, error) {
	pm, err := s.AttachFromSetupIntent(ctx, tenantID, customerID, stripePaymentMethodID, makeDefault)
	if err != nil {
		return nil, err
	}
	if !pm.IsActive() {
		return nil, nil
	}
	view := pm.View()
	return &view, nil
}

// AttachFromSetupIntent is the entry point the P5 webhook handler uses:
// after setup_intent.succeeded, we know the PM and customer, and we
// persist the row here. Called with an RLS ctx already staged to the
// right tenant+livemode by the webhook handler.
//
// makeDefault is set for a card saved through a Velox setup flow (the
// operator's Add payment method link, the customer's payment-update link):
// that card becomes the default, the card Velox charges next (ADR-021).
// Without it the store's rule applies: the first card, or a card replacing
// a default the bank refused for good, becomes the default; any other is
// added beside it.
//
// When the saved card is the default, Stripe's copy
// (invoice_settings.default_payment_method) is set to it after the commit.
// Best-effort and cosmetic, as in SetDefault: charges read Velox's default
// (ADR-053), so a Stripe failure logs, lands as stripe_sync_error on the
// audit row, and never fails the attach (or the webhook behind it).
//
// A card already removed stays removed (a late webhook, see
// PostgresStore.Upsert): the removed row is returned, and no Stripe call or
// audit row follows. When the attach moved the default from another card,
// a default_changed audit row follows the attached one, as Set as default
// and Remove's auto-promote write, so the log shows the charged card changed.
func (s *Service) AttachFromSetupIntent(ctx context.Context, tenantID, customerID, stripePaymentMethodID string, makeDefault bool) (PaymentMethod, error) {
	card, err := s.stripe.FetchPaymentMethodCard(ctx, stripePaymentMethodID)
	if err != nil {
		return PaymentMethod{}, fmt.Errorf("fetch card metadata: %w", err)
	}
	pm, replacedDefault, err := s.store.Upsert(ctx, tenantID, PaymentMethod{
		CustomerID:            customerID,
		StripePaymentMethodID: stripePaymentMethodID,
		Type:                  "card",
		CardBrand:             card.Brand,
		CardLast4:             card.Last4,
		CardExpMonth:          card.ExpMonth,
		CardExpYear:           card.ExpYear,
		CardFingerprint:       card.Fingerprint,
	}, makeDefault)
	if err != nil {
		return PaymentMethod{}, err
	}
	if !pm.IsActive() {
		slog.InfoContext(ctx, "card already removed; the late attach leaves it removed",
			"tenant_id", tenantID, "customer_id", customerID,
			"stripe_payment_method_id", stripePaymentMethodID)
		return pm, nil
	}

	stripeSyncErr := ""
	if pm.IsDefault && pm.StripePaymentMethodID != "" {
		if err := s.stripe.SetDefaultPaymentMethod(ctx, tenantID, customerID, pm.StripePaymentMethodID); err != nil {
			slog.WarnContext(ctx, "failed to sync the attached default payment method to Stripe",
				"tenant_id", tenantID, "customer_id", customerID,
				"stripe_payment_method_id", pm.StripePaymentMethodID, "error", err)
			stripeSyncErr = err.Error()
		}
	}

	if s.auditLogger != nil {
		meta := map[string]any{
			"action":      "attached",
			"customer_id": customerID,
			"card_brand":  card.Brand,
			"card_last4":  card.Last4,
			"is_default":  pm.IsDefault,
		}
		if stripeSyncErr != "" {
			meta["stripe_sync_error"] = stripeSyncErr
		}
		_ = s.auditLogger.Log(ctx, tenantID, "create", "payment_method", pm.ID, pmLabel(pm), meta)

		if replacedDefault {
			trigger := "replaced_refused_default"
			if makeDefault {
				trigger = "setup_link"
			}
			changed := map[string]any{
				"action":      "default_changed",
				"customer_id": customerID,
				"card_brand":  pm.CardBrand,
				"card_last4":  pm.CardLast4,
				"trigger":     trigger,
			}
			if stripeSyncErr != "" {
				changed["stripe_sync_error"] = stripeSyncErr
			}
			_ = s.auditLogger.Log(ctx, tenantID, "update", "payment_method", pm.ID, pmLabel(pm), changed)
		}
	}
	return pm, nil
}

// pmLabel renders an operator-friendly resource label for audit_log.
// "VISA ····4242" reads correctly in the AuditLog table without
// leaking the full stripe_payment_method_id token.
func pmLabel(pm PaymentMethod) string {
	if pm.CardBrand == "" && pm.CardLast4 == "" {
		return pm.Type
	}
	return fmt.Sprintf("%s ····%s", pm.CardBrand, pm.CardLast4)
}

func livemodeLabel(ctx context.Context) string {
	if postgres.Livemode(ctx) {
		return "true"
	}
	return "false"
}
