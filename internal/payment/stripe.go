package payment

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	chimw "github.com/go-chi/chi/v5/middleware"

	mw "github.com/sagarsuperuser/velox/internal/api/middleware"
	"github.com/sagarsuperuser/velox/internal/audit"
	veloxauth "github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/payment/breaker"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
)

// Stripe is the payment adapter. It creates PaymentIntents for finalized invoices
// and processes webhook events to update invoice payment status.
//
// This is NOT an abstract "payment provider" interface. Velox is Stripe-native.
// If we ever support another provider, we'll refactor — not speculate now.
// DunningStarter starts dunning for failed payments.
type DunningStarter interface {
	StartDunning(ctx context.Context, tenantID string, invoiceID, customerID string, failureAt time.Time, cause domain.DunningStartCause) (domain.InvoiceDunningRun, error)
}

// DunningResolver closes an active dunning run when a card payment settles —
// symmetric with the engine's DunningResolver for background settles (credits /
// threshold / auto-charge, #317). Optional; nil = skip (the dunning sweep's
// paid-pre-check floor still resolves the run when it next comes due). Wire via
// SetDunningResolver. ResolveByInvoice no-ops when there is no active run, so it
// is safe to call on every card success.
type DunningResolver interface {
	ResolveByInvoice(ctx context.Context, tenantID, invoiceID string, resolution domain.DunningResolution) error
}

// CardDetails holds card info fetched from Stripe for display.
type CardDetails struct {
	PaymentMethodID string
	Brand           string
	Last4           string
	ExpMonth        int
	ExpYear         int
}

// CardFetcher fetches card details from Stripe.
//
// FetchCardDetails returns the customer's *default* PM card. Used
// by dunning to render "we'll retry charging Visa •••• 4242 in 3
// days" copy.
//
// FetchCardForPaymentIntent returns the card actually charged on a
// specific PaymentIntent. Used by the payment-succeeded webhook
// handler to stamp the invoice's `payment_card_brand` /
// `payment_card_last4` columns (ADR-020). Works for one-off
// Checkout cards too — the customer doesn't have to save the PM
// for the timeline to show what was charged.
type CardFetcher interface {
	FetchCardDetails(ctx context.Context, stripeCustomerID string) (CardDetails, error)
	FetchCardForPaymentIntent(ctx context.Context, paymentIntentID string) (CardDetails, error)
}

// EmailReceipt sends payment receipt emails. ctx carries livemode so
// the enqueue/brand lookup runs against the right tenant_settings row.
type EmailReceipt interface {
	SendPaymentReceipt(ctx context.Context, tenantID, to string, cc []string, customerName, invoiceNumber string, amountCents int64, currency, publicToken string) error
}

// CustomerEmailResolver resolves customer contact info for email notifications.
type CustomerEmailResolver interface {
	GetCustomerEmail(ctx context.Context, tenantID, customerID string) (email, displayName string, additionalEmails []string, err error)
}

// EmailPaymentFailed sends "your charge was declined" emails. Hits
// after a Stripe charge attempt actually went through and was
// declined — distinct from EmailPaymentSetup which fires when there's
// no PM on file at finalize (no charge was ever attempted). Same
// signature as dunning.EmailNotifier so OutboxSender satisfies both.
type EmailPaymentFailed interface {
	SendPaymentFailed(ctx context.Context, tenantID, to string, cc []string, customerName, invoiceNumber, reason, publicToken string) error
}

type Stripe struct {
	client             StripeClient
	invoices           InvoiceUpdater
	webhooks           WebhookStore
	dunning            DunningStarter
	dunningResolver    DunningResolver
	paymentSetups      PaymentSetupStore
	cardFetcher        CardFetcher
	events             domain.EventDispatcher
	emailReceipt       EmailReceipt
	customerEmail      CustomerEmailResolver
	emailPaymentFailed EmailPaymentFailed
	breaker            *breaker.Breaker // optional; nil = no breaker
	pmAttacher         PaymentMethodAttacher
	customerResolver   CustomerByStripeIDResolver // optional; resolves SetupIntent.customer → velox id
	refundUpdater      RefundStatusUpdater        // optional; applies async refund-webhook status to a CN
	resolver           clock.Resolver             // optional; binds effective-now from invoice
	anomalies          PaymentAnomalyRecorder     // optional; durable marker for the attention banner (ADR-068)
	checkoutSessions   *CheckoutSessionStore      // optional; claim-ledger truth-sync + settle-time expire (ADR-068)
	sessionClients     *StripeClients             // optional; raw mode-keyed clients for session expire calls
	audit              AuditEmitter               // optional; ADR-090 in-tx emission for the checkout-completed PM flip
}

// AuditEmitter is the narrow in-tx audit seam (ADR-090).
type AuditEmitter interface {
	LogInTx(ctx context.Context, tx *sql.Tx, e audit.Entry) error
}

// SetAuditLogger wires in-tx audit emission for the
// checkout.session.completed payment-setup flip — previously that webhook's
// only durable mutation left no audit trail (and payment-mode+save-card had
// no sibling record at all).
func (s *Stripe) SetAuditLogger(a AuditEmitter) { s.audit = a }

// PaymentAnomalyRecorder persists the durable payment-anomaly marker the
// dashboard attention classifier reads (ADR-068). Implemented by
// *invoice.PostgresStore; wired via SetAnomalyRecorder in router.go.
type PaymentAnomalyRecorder interface {
	RecordPaymentAnomaly(ctx context.Context, tenantID, invoiceID, kind, paymentIntentID string, capturedCents int64) error
}

// SetAnomalyRecorder wires the durable anomaly marker store.
func (s *Stripe) SetAnomalyRecorder(r PaymentAnomalyRecorder) {
	s.anomalies = r
}

// SetCheckoutSessionStore wires the claim ledger for webhook truth-sync and
// the settle-time best-effort Stripe expire (ADR-068). clients resolves the
// mode-keyed raw Stripe client for the expire calls (the narrow
// StripeClient interface deliberately has no session surface).
func (s *Stripe) SetCheckoutSessionStore(cs *CheckoutSessionStore, clients *StripeClients) {
	s.checkoutSessions = cs
	s.sessionClients = clients
}

// SetResolver wires the unified clock.Resolver. Webhook handlers fire
// in their own goroutine with no inherited ctx binding — they bind
// effective-now from the invoice they're processing so MarkPaid /
// UpdatePayment / StartDunning all stamp simulated time on
// clock-pinned invoices. Without this, webhook-driven writes leak
// wall-clock back into the simulation. Optional: nil leaves binding
// off (test default).
func (s *Stripe) SetResolver(r clock.Resolver) {
	s.resolver = r
}

// bindForInvoice resolves effective-now from the invoice and binds
// ctx for downstream stamps. Returns ctx unchanged on resolver error
// or no resolver — webhook handlers must never fail their inbound
// event on a transient resolution issue.
func (s *Stripe) bindForInvoice(ctx context.Context, tenantID, invoiceID string) context.Context {
	bound, _ := clock.BindEffectiveNow(ctx, s.resolver, clock.Pin{TenantID: tenantID, InvoiceID: invoiceID})
	return bound
}

// StripeClient is the interface for Stripe API calls.
// In production this wraps stripe-go; in tests it's a mock.
type StripeClient interface {
	CreatePaymentIntent(ctx context.Context, params PaymentIntentParams) (PaymentIntentResult, error)
	CancelPaymentIntent(ctx context.Context, paymentIntentID string) error
	GetPaymentIntent(ctx context.Context, paymentIntentID string) (PaymentIntentResult, error)
}

type PaymentIntentParams struct {
	AmountCents int64
	Currency    string
	CustomerID  string // Stripe customer ID
	// PaymentMethodID is the Stripe PaymentMethod to charge, resolved by
	// the caller from Velox's payment_methods table (the single source of
	// truth for the customer's default card). Required: the client charges
	// this exact PM and never asks Stripe to pick one. Empty → loud failure.
	PaymentMethodID string
	Description     string
	IdempotencyKey  string
	Metadata        map[string]string
}

type PaymentIntentResult struct {
	ID           string
	Status       string
	ClientSecret string
	// Purpose is the velox_purpose PI metadata (e.g. "dunning_retry",
	// "hosted_invoice_pay"). Populated by GetPaymentIntent so the reconciler
	// can replicate the webhook's customer-email suppression when it recovers a
	// failure (ADR-049 Phase 2). Empty for the create response / when unset.
	Purpose string
	// AmountReceivedCents is the amount Stripe actually captured. Populated
	// by GetPaymentIntent so the reconciler's SettleSucceeded carries the
	// real captured figure — it passed a literal 0 pre-2026-07-06, which
	// structurally disabled the ADR-068 payment.amount_mismatch alarm on
	// the webhook-drop backstop path (the settle skips the truth-check
	// when capturedCents == 0).
	AmountReceivedCents int64
}

// InvoiceUpdater updates invoice payment status.
type InvoiceUpdater interface {
	UpdatePayment(ctx context.Context, tenantID, id string, paymentStatus domain.InvoicePaymentStatus, stripePaymentIntentID, lastPaymentError string, paidAt *time.Time) (domain.Invoice, error)
	UpdateStatus(ctx context.Context, tenantID, id string, status domain.InvoiceStatus) (domain.Invoice, error)
	MarkPaid(ctx context.Context, tenantID, id string, stripePaymentIntentID string, paidAt time.Time) (domain.Invoice, error)
	// MarkPaidReportingTransition is MarkPaid plus a `transitioned` flag —
	// true only when THIS call moved the invoice into paid (false on the
	// already-paid no-op). SettleSucceeded gates its post-paid side-effects
	// on it so a concurrent redelivery of the same charge doesn't double-
	// fire the receipt email / payment.succeeded event.
	MarkPaidReportingTransition(ctx context.Context, tenantID, id string, stripePaymentIntentID string, paidAt time.Time) (domain.Invoice, bool, error)
	// MarkPaidCardSettlementTransition is MarkPaidReportingTransition for the
	// card path: it also enqueues payment.succeeded in the SAME tx as the
	// paid-flip, so that event (the only one bearing the Stripe payment_intent_id)
	// is crash-safe instead of fire-and-forget post-commit. SettleSucceeded uses
	// this; non-card settlement paths keep MarkPaidReportingTransition.
	MarkPaidCardSettlementTransition(ctx context.Context, tenantID, id string, stripePaymentIntentID string, paidAt time.Time) (domain.Invoice, bool, error)
	// MarkPaymentFailedReportingTransition records a failure and reports
	// whether THIS call is the first to fire the failure-notification set
	// (payment.failed event + customer email + dunning) for this
	// PaymentIntent. SettleFailed gates those side-effects on the flag so a
	// concurrent at-least-once redelivery of the same
	// payment_intent.payment_failed doesn't double-notify. PI-keyed because
	// failure is non-terminal — one fresh failure per dunning retry — and
	// because the synchronous charge path stamps payment_status='failed'
	// before deferring the notifications here.
	MarkPaymentFailedReportingTransition(ctx context.Context, tenantID, id, paymentIntentID, lastPaymentError string) (domain.Invoice, bool, error)
	// SetPaymentCard stamps the card brand + last4 used to settle
	// an invoice. Optional — empty values render no sub-line in
	// the timeline. Called by handlePaymentSucceeded after MarkPaid
	// lands. ADR-020.
	SetPaymentCard(ctx context.Context, tenantID, id, brand, last4 string) error
	GetByStripePaymentIntentID(ctx context.Context, tenantID, stripePaymentIntentID string) (domain.Invoice, error)
	Get(ctx context.Context, tenantID, id string) (domain.Invoice, error)
}

// WebhookStore persists and queries Stripe webhook events.
//
// Dedup is split across two calls so the dedup marker can be written
// AFTER the business side-effect commits, not before. WasProcessed is
// the read-only pre-check; IngestEvent is the write that both records
// the audit row and claims the dedup slot (ON CONFLICT DO NOTHING).
// HandleWebhook calls IngestEvent only once processing has succeeded
// (or failed permanently) — a transient processing failure persists no
// row, so Stripe's redelivery re-runs the handler instead of being
// short-circuited as a duplicate.
type WebhookStore interface {
	// WasProcessed reports whether an event with this stripe_event_id has
	// already been ingested for the tenant+livemode. Read-only; used as the
	// idempotency pre-check before running side-effects.
	WasProcessed(ctx context.Context, tenantID, stripeEventID string) (bool, error)
	IngestEvent(ctx context.Context, tenantID string, event domain.StripeWebhookEvent) (domain.StripeWebhookEvent, bool, error)
	ListByInvoice(ctx context.Context, tenantID, invoiceID string) ([]domain.StripeWebhookEvent, error)
}

func NewStripe(client StripeClient, invoices InvoiceUpdater, webhooks WebhookStore, paymentSetups PaymentSetupStore, dunning ...DunningStarter) *Stripe {
	s := &Stripe{client: client, invoices: invoices, webhooks: webhooks, paymentSetups: paymentSetups}
	if len(dunning) > 0 {
		s.dunning = dunning[0]
	}
	return s
}

// SetCardFetcher configures card detail fetching from Stripe.
func (s *Stripe) SetCardFetcher(cf CardFetcher) {
	s.cardFetcher = cf
}

// SetDunningResolver wires the resolver that closes an active dunning run when a
// card payment settles (SettleSucceeded). See the DunningResolver doc-comment.
func (s *Stripe) SetDunningResolver(d DunningResolver) {
	s.dunningResolver = d
}

// SetEmailReceipt configures payment receipt email sending.
func (s *Stripe) SetEmailReceipt(receipt EmailReceipt, customerEmail CustomerEmailResolver) {
	s.emailReceipt = receipt
	s.customerEmail = customerEmail
}

// SetEmailPaymentFailed configures the post-decline email sender.
// Mirrors the dunning email path — points the customer at the hosted
// invoice URL where they can update PM and retry. The customer-email
// resolver is shared with the receipt path.
func (s *Stripe) SetEmailPaymentFailed(sender EmailPaymentFailed, customerEmail CustomerEmailResolver) {
	s.emailPaymentFailed = sender
	if s.customerEmail == nil {
		s.customerEmail = customerEmail
	}
}

// SetEventDispatcher configures outbound webhook event firing.
func (s *Stripe) SetEventDispatcher(events domain.EventDispatcher) {
	s.events = events
}

// SetBreaker wires a global circuit breaker around Stripe calls. When
// set, ChargeInvoice short-circuits with breaker.ErrOpen if the breaker
// is rejecting — the Stripe call is not made and the invoice state is
// left untouched so dunning treats it as "try later" without ticking
// attempt_count.
func (s *Stripe) SetBreaker(b *breaker.Breaker) {
	s.breaker = b
}

// PaymentMethodAttacher persists a PM row after a Stripe setup_intent
// succeeds. Optional — if nil, setup_intent.succeeded events are logged
// and ack'd but no payment_methods row is created. In practice the
// router always wires paymentmethods.Service here.
//
// The method name is AttachForWebhook (not AttachFromSetupIntent) so
// paymentmethods.Service can keep its richer AttachFromSetupIntent
// method — returning (PaymentMethod, error) — for tests and direct
// callers, while this package imports nothing from paymentmethods.
type PaymentMethodAttacher interface {
	AttachForWebhook(ctx context.Context, tenantID, customerID, stripePaymentMethodID string) error
}

// SetPaymentMethodAttacher configures the handler that processes
// setup_intent.succeeded events.
func (s *Stripe) SetPaymentMethodAttacher(a PaymentMethodAttacher) {
	s.pmAttacher = a
}

// CustomerByStripeIDResolver maps a Stripe Customer ID back to the Velox
// customer ID. Optional fallback for setup_intent.succeeded: a Checkout
// session in setup mode only carries velox_customer_id on the SetupIntent
// when its creator set setup_intent_data.metadata — and not every call site
// does. Rather than depend on that discipline at every session-creation
// site, the webhook resolves the customer from the SetupIntent's `customer`
// field, which Stripe always populates for our customer-scoped setup flows.
// This makes PM persistence authoritative-by-construction instead of
// best-effort-by-metadata.
type CustomerByStripeIDResolver interface {
	CustomerIDByStripeID(ctx context.Context, tenantID, stripeCustomerID string) (string, error)
}

// SetCustomerResolver wires the Stripe-customer → velox-customer resolver
// used as the setup_intent.succeeded fallback. Optional: nil leaves the
// handler dependent on SetupIntent metadata alone (test default).
func (s *Stripe) SetCustomerResolver(r CustomerByStripeIDResolver) {
	s.customerResolver = r
}

// RefundStatusUpdater applies an async refund-webhook status (already mapped to a
// Velox refund_status) to the credit note carrying stripeRefundID, monotonically.
// Returns errs.ErrNotFound when no credit note matches (foreign/dashboard refund,
// or the row hasn't committed yet). Optional — nil logs+acks refund events.
// payment imports nothing from creditnote; creditnote.Service satisfies this.
type RefundStatusUpdater interface {
	ApplyRefundWebhook(ctx context.Context, tenantID, stripeRefundID string, status domain.RefundStatus) error
}

// SetRefundStatusUpdater wires the async refund-status reconciler used by the
// refund.* / charge.refund.updated webhook handlers.
func (s *Stripe) SetRefundStatusUpdater(u RefundStatusUpdater) {
	s.refundUpdater = u
}

// IsUnknownPaymentFailure classifies an error returned from ChargeInvoice
// as a Stripe-side unknown outcome (5xx, timeout, network) — the failure
// category that should feed the circuit breaker. Explicit card declines
// and validation errors return false. Exported so the breaker factory can
// reuse the exact same classification used at the charge site.
func IsUnknownPaymentFailure(err error) bool {
	if err == nil {
		return false
	}
	var pe *PaymentError
	if errors.As(err, &pe) {
		return pe.Unknown
	}
	// Plain error with no classification — assume unknown to be conservative.
	// A wrapped non-PaymentError from our own code shouldn't happen; if it
	// does we'd rather trip the breaker than miss a real incident.
	return true
}

// fireEvent dispatches an outbound webhook event. Synchronous by design: when
// the outbox path is enabled (RES-1, the default), Dispatch is a short DB
// insert, and persisting-before-return is what the outbox exists to
// guarantee. Errors are logged but not surfaced — the business op has
// already committed, and we prefer that a missed event shows up in logs
// over failing the end-user request for a webhook side-effect.
func (s *Stripe) fireEvent(ctx context.Context, tenantID, eventType string, payload map[string]any) {
	if s.events == nil {
		return
	}
	if err := s.events.Dispatch(ctx, tenantID, eventType, payload); err != nil {
		slog.Error("dispatch event",
			"event_type", eventType,
			"tenant_id", tenantID,
			"error", err,
		)
	}
}

// ChargeInvoice creates a Stripe PaymentIntent for a finalized invoice.
// The invoice must have a customer with a Stripe customer ID already set up.
func (s *Stripe) ChargeInvoice(ctx context.Context, tenantID string, inv domain.Invoice, stripeCustomerID, stripePaymentMethodID string) (domain.Invoice, error) {
	return s.chargeInvoice(ctx, tenantID, inv, stripeCustomerID, stripePaymentMethodID, "")
}

// ChargeInvoiceForDunningRetry charges an invoice as part of a dunning
// retry attempt. Identical to ChargeInvoice except the resulting
// PaymentIntent carries velox_purpose=dunning_retry metadata, which
// the payment_intent.payment_failed webhook handler reads to suppress
// the duplicate generic payment-failed email (dunning sends its own
// per-attempt warning / escalation inline from processRun / exhaustRun).
//
// Exposed as a separate method rather than a variadic option so the
// engine's InvoiceCharger interface stays narrow (default-mode only)
// and the cross-domain import that a shared options type would force
// (billing → payment) is avoided per the per-domain-isolation rule
// in CLAUDE.md.
func (s *Stripe) ChargeInvoiceForDunningRetry(ctx context.Context, tenantID string, inv domain.Invoice, stripeCustomerID, stripePaymentMethodID string) (domain.Invoice, error) {
	return s.chargeInvoice(ctx, tenantID, inv, stripeCustomerID, stripePaymentMethodID, piPurposeDunningRetry)
}

func (s *Stripe) chargeInvoice(ctx context.Context, tenantID string, inv domain.Invoice, stripeCustomerID, stripePaymentMethodID, purpose string) (domain.Invoice, error) {
	if inv.Status != domain.InvoiceFinalized {
		return domain.Invoice{}, fmt.Errorf("can only charge finalized invoices, current status: %s", inv.Status)
	}
	if inv.AmountDueCents <= 0 {
		return domain.Invoice{}, fmt.Errorf("invoice has no amount due")
	}
	if stripeCustomerID == "" {
		return domain.Invoice{}, fmt.Errorf("stripe customer ID is required")
	}
	// The exact card to charge is resolved upstream from Velox's
	// payment_methods table and passed in explicitly — the charge never
	// defers PM selection to Stripe. A missing PM id is a caller bug
	// (the charge gates require a default PM before reaching here); fail
	// loud rather than letting Stripe pick a card.
	if stripePaymentMethodID == "" {
		return domain.Invoice{}, fmt.Errorf("stripe payment method ID is required")
	}

	// Seed tenantID on ctx so the per-tenant Stripe client resolver
	// (payment.StripeClients) can look it up. Scheduler / billing-engine
	// driven charges arrive on background ctx without tenant stamped.
	// Livemode is already carried on ctx by authenticated callers; for
	// background workers it defaults to live (postgres.Livemode default).
	ctx = veloxauth.WithTenantID(ctx, tenantID)

	// Stable business metadata only — nothing per-HTTP-request. The
	// idempotency key + metadata together form Stripe's dedup contract:
	// same key + same params → cached response; different params → 409
	// conflict. Putting velox_request_id (per-call chi ID) in metadata
	// would make every retry attempt see "same key, different params"
	// and 409. Log correlation goes through stripe_payment_intent_id
	// (recorded back on the invoice) instead.
	metadata := map[string]string{
		"velox_invoice_id":     inv.ID,
		"velox_invoice_number": inv.InvoiceNumber,
		"velox_tenant_id":      tenantID,
		"velox_customer_id":    inv.CustomerID,
	}
	if purpose != "" {
		metadata["velox_purpose"] = purpose
	}
	// velox_anchor_at: the CONTRACTED simulated instant this charge fires
	// at (a dunning retry's next_action_at, a cycle's close boundary).
	// The settle path — inline OR the wall-side webhook, which cannot see
	// the caller's ctx — stamps paid_at from this anchor, so a recovery
	// that happened at sim Mar 7 never records "paid Apr 1" just because
	// the operator advanced a month in one click (ADR-030
	// contracted-instant rule). Absent on wall-clock charges.
	if sim, ok := clock.SimOf(ctx); ok {
		metadata["velox_anchor_at"] = sim.At.UTC().Format(time.RFC3339Nano)
	}
	// Per-attempt idempotency key. inv.UpdatedAt advances every time
	// UpdatePayment runs (which happens on every prior failed attempt
	// recording last_payment_error), so each genuine retry gets a
	// fresh key — Stripe creates a new PI rather than returning the
	// cached failed one. Within a single attempt, two concurrent
	// callers (scheduler + operator click) read the same UpdatedAt
	// and dedupe correctly: Stripe returns the same PI to both, no
	// double-charge.
	//
	// The `purpose` suffix prevents key collisions between the initial
	// finalize-time auto-charge (purpose="") and the first dunning
	// retry (purpose="dunning_retry"). Without it, both calls use
	// inv.UpdatedAt = T1 (the dunning retry reads the inv with
	// updated_at still pinned to the initial-charge-fail moment
	// because no UpdatePayment runs between them), but the metadata
	// differs because the retry path adds velox_purpose=dunning_retry.
	// Stripe sees "same key, different parameters" → 409. Surfaced
	// 2026-05-19 from a clock-pinned dunning workflow; the bug exists
	// on wall-clock too in theory but is masked because invoice
	// metadata typically doesn't change between consecutive
	// chargeInvoice/ChargeInvoiceForDunningRetry calls in production
	// (they sit far enough apart in time for UpdatedAt to drift via
	// other writes).
	idempotencyKey := fmt.Sprintf("velox_inv_%s_%d", inv.ID, inv.UpdatedAt.UnixNano())
	if purpose != "" {
		idempotencyKey += "_" + purpose
	}
	params := PaymentIntentParams{
		AmountCents:     inv.AmountDueCents,
		Currency:        inv.Currency,
		CustomerID:      stripeCustomerID,
		PaymentMethodID: stripePaymentMethodID,
		Description:     fmt.Sprintf("Invoice %s", inv.InvoiceNumber),
		IdempotencyKey:  idempotencyKey,
		Metadata:        metadata,
	}

	var result PaymentIntentResult
	var err error
	if s.breaker != nil {
		var out any
		out, err = s.breaker.Execute(ctx, func(ctx context.Context) (any, error) {
			return s.client.CreatePaymentIntent(ctx, params)
		})
		if errors.Is(err, breaker.ErrOpen) {
			// Breaker short-circuit: the call to Stripe was not made. Do NOT
			// mutate invoice state — leaving payment_status as-is lets the
			// scheduler try again on the next tick once cooldown elapses.
			// Returning a distinct sentinel lets dunning treat this as a
			// transient skip rather than ticking attempt_count.
			return domain.Invoice{}, ErrPaymentTransient
		}
		if out != nil {
			result = out.(PaymentIntentResult)
		}
	} else {
		result, err = s.client.CreatePaymentIntent(ctx, params)
	}
	if err != nil {
		// Categorise the error so ambiguous (5xx/timeout/network) outcomes are
		// recorded as PaymentUnknown, not PaymentFailed. Retrying a failed-
		// but-actually-succeeded charge would double-bill the customer; the
		// reconciler resolves unknowns by querying Stripe later.
		//
		// Dunning for DEFINITE failures is started inline further down this
		// block (!pe.Unknown → startDunningWithRetry — added for test-clock
		// catchup, see that comment). The payment_intent.payment_failed
		// webhook (handlePaymentFailed) is the redundant-but-idempotent
		// second path: with Confirm:true + OffSession:true Stripe creates
		// the PI even on decline and sends the event; StartDunning's
		// per-invoice UNIQUE makes the pair safe. Unknown outcomes start
		// nothing — the reconciler resolves them first.
		var pe *PaymentError
		if !errors.As(err, &pe) {
			pe = &PaymentError{Message: errs.Scrub(err.Error()), Unknown: true}
		}

		status := domain.PaymentFailed
		verb := "payment failed"
		if pe.Unknown {
			status = domain.PaymentUnknown
			verb = "payment state unknown"
			slog.Warn("payment intent outcome unknown — reconciler will resolve",
				"invoice_id", inv.ID,
				"stripe_payment_intent_id", pe.PaymentIntentID,
				"error", pe.Message,
			)
		}
		if perr := persistChargeOutcomeWithRetry(ctx, s.invoices, tenantID, inv.ID, status, pe.PaymentIntentID, pe.Message); perr != nil {
			// LOAD-BEARING, especially on the Unknown path: this is the SOLE
			// write that sets payment_status='unknown' — enrolling the invoice
			// into the reconciler's unknown-payment sweep (ListUnknownPayments)
			// — and persists the PaymentIntent id. Losing it strands a
			// possibly-SUCCEEDED PI: the reconciler never resolves it → silent
			// under-collection. Pre-fix the call swallowed both the return AND
			// the error (`_, _ =`). Fail LOUD with the PI id so an operator can
			// reconcile manually. The charge-error return below is unchanged —
			// the caller still classifies the outcome and defers unknowns.
			slog.Error("CRITICAL: could not persist post-charge payment state — invoice may be orphaned from the reconciler sweep",
				"invoice_id", inv.ID,
				"payment_status", string(status),
				"unknown_outcome", pe.Unknown,
				"stripe_payment_intent_id", pe.PaymentIntentID,
				"error", perr,
			)
		}
		// Count unknown outcomes as failed for the success-rate alert — a
		// Stripe outage genuinely impairs customer charging and should page,
		// even though the reconciler will later resolve the per-invoice state.
		mw.RecordPaymentCharge("failed")

		// Inline StartDunning for known-failed charges so the dunning run
		// exists by the time the orchestrator's Phase 5 queries due runs
		// in the same Advance. Pre-fix this was deferred to the
		// payment_intent.payment_failed webhook — fine on wall-clock, but
		// under test-clock catchup the webhook arrives AFTER Phase 5 has
		// already exited (Phase 3 fires PI, Phase 5 runs immediately,
		// webhook lands later), so the new dunning run sat at
		// attempt_count=0 with no retries fired until the next Advance.
		// Industry parity: Stripe Test Clocks processes the failure
		// synchronously inside the advance.
		//
		// Safe to call alongside the webhook path: StartDunning is
		// idempotent by invoice (migration 0085 UNIQUE), so the
		// subsequent webhook-driven call returns the existing run.
		//
		// Only fires on definitively-failed charges. Unknown outcomes
		// (Stripe outage, ambiguous timeout) defer to the webhook so
		// the reconciler can resolve them without burning a retry on
		// an ambiguous result.
		if !pe.Unknown && s.dunning != nil {
			failureAt := simulatedFailureAt(inv)
			// A definitively-declined charge — the cause is a real payment failure.
			if derr := startDunningWithRetry(ctx, s.dunning, tenantID, inv.ID, inv.CustomerID, failureAt, domain.DunningCausePaymentFailed); derr != nil {
				slog.Error("inline StartDunning after known-failed charge failed — no action needed: the payment-failed webhook or the dunning backfill sweep will start the run automatically",
					"invoice_id", inv.ID, "customer_id", inv.CustomerID, "error", derr)
			}
		}

		// Wrap with %w so respond.FromError can detect *PaymentError
		// via errors.As and surface OperatorSafeMessage() instead of
		// leaking pe.Message (which is the raw Stripe SDK string —
		// includes idempotency-key conflicts, validation errors, etc.).
		// ADR-026.
		return domain.Invoice{}, fmt.Errorf("%s: %w", verb, pe)
	}

	slog.Info("payment intent created",
		"invoice_id", inv.ID,
		"payment_intent_id", result.ID,
		"amount_cents", inv.AmountDueCents,
		"request_id", chimw.GetReqID(ctx),
	)
	mw.RecordPaymentCharge("succeeded")

	// Honor the synchronous confirmation outcome (ADR-049 Phase 3,
	// discover-then-settle). With Confirm:true + OffSession:true, Stripe
	// returns the terminal status in the create RESPONSE for cards — a
	// `succeeded` PI is authoritative WITHOUT waiting for the webhook. Settle
	// inline so the invoice resolves in-request — and, on a test clock, inside
	// the operator's Advance on the simulated timeline — instead of sitting in
	// `processing` until a wall-clock webhook that may never arrive in local
	// dev. The webhook + reconciler stay idempotent backstops (SettleSucceeded
	// skips an already-paid invoice). Genuinely-async statuses (processing /
	// requires_action / requires_confirmation / requires_capture — delayed
	// methods, or rare off-session SCA) stay `processing` and await the webhook.
	if result.Status == "succeeded" {
		settleCtx := ctx
		if sim, ok := clock.SimOf(ctx); ok {
			// Same anchor the PI metadata carries — the inline path can
			// hand it over directly instead of round-tripping via Stripe.
			settleCtx = withSettleAnchor(ctx, sim.At)
		}
		if serr := s.SettleSucceeded(settleCtx, tenantID, inv, result.ID, inv.AmountDueCents, SourceChargeResponse); serr != nil {
			return domain.Invoice{}, fmt.Errorf("settle succeeded inline: %w", serr)
		}
		// Return the freshly-settled row so callers (dunning retrier, portal,
		// operator charge-now) observe the paid state immediately. On a re-read
		// error the settle already committed — return a patched copy rather
		// than a misleading error.
		settled, gerr := s.invoices.Get(ctx, tenantID, inv.ID)
		if gerr != nil {
			inv.PaymentStatus = domain.PaymentSucceeded
			inv.Status = domain.InvoicePaid
			inv.StripePaymentIntentID = result.ID
			return inv, nil
		}
		return settled, nil
	}

	// Still in flight — record the PI and set processing; the webhook (and the
	// reconciler backstop) will settle it.
	return s.invoices.UpdatePayment(ctx, tenantID, inv.ID,
		domain.PaymentProcessing, result.ID, "", nil)
}

// piPurposeDunningRetry tags a PaymentIntent created by the dunning
// retrier so the payment_intent.payment_failed webhook can suppress
// its generic payment-failed email — dunning's warning/escalation is
// the canonical notification for retry attempts. Stripe-parity (Smart
// Retries sends one email per attempt, not one webhook-email plus one
// engine-email).
const piPurposeDunningRetry = "dunning_retry"

// purposePaymentUpdateToken tags a SetupIntent created by the customer-driven
// payment-update token link (public_handler.createCheckoutSession). It is the
// signal that the resulting setup_intent.succeeded attach was CUSTOMER-driven
// — so the audit row is attributed to the customer, not "System". Operator
// attach flows leave the purpose empty or "portal_add_payment_method".
const purposePaymentUpdateToken = "payment_update_token"

// simulatedFailureAt derives the cycle-close instant for a failed
// charge on the given invoice. Used by both the inline charge-failure
// path and the async webhook handler so dunning anchors next_action_at
// on simulated time even when running under test-clock catchup.
//
// Returns the latest invoice period boundary at or before IssuedAt:
// - in_arrears: BillingPeriodEnd (= elapsed period close = cycle fire)
// - in_advance: BillingPeriodStart (= upcoming period start = cycle fire)
// Both directions converge on "the cycle close instant in simulated
// time." Manual invoices with no period fields fall back to IssuedAt,
// and ultimately to time.Now() — both wall-clock-equivalent in
// production.
// startDunningWithRetry calls StartDunning with bounded inline retry to
// absorb transient blips (DB connection hiccup, brief contention) without
// silently losing the dunning start. StartDunning is idempotent by
// invoice (migration 0085 UNIQUE on dunning_runs), so the retry is safe
// — a successful first attempt followed by a slow concurrent caller
// resolves to the existing row, not a duplicate.
//
// Three attempts with 100ms / 500ms backoff covers the typical transient
// case. Persistent failure (Stripe outage, DB unavailable for seconds)
// surfaces as the returned error; caller upgrades the slog level to
// ERROR so operators have an alertable signal, and the operator path
// (start dunning manually from the invoice attention banner) stays
// available as the last resort.
//
// 2026-05-30 design-debt audit (Tier 1 #5) replaced two log-and-swallow
// sites here and at the inline charge-failure path with this retry.
func startDunningWithRetry(ctx context.Context, dunning DunningStarter, tenantID, invoiceID, customerID string, failureAt time.Time, cause domain.DunningStartCause) error {
	delays := []time.Duration{0, 100 * time.Millisecond, 500 * time.Millisecond}
	var lastErr error
	for i, d := range delays {
		if d > 0 {
			select {
			case <-time.After(d):
			case <-ctx.Done():
				return fmt.Errorf("ctx canceled during StartDunning retry (attempt %d): %w", i+1, ctx.Err())
			}
		}
		if _, err := dunning.StartDunning(ctx, tenantID, invoiceID, customerID, failureAt, cause); err != nil {
			if errors.Is(err, errs.ErrInvalidState) {
				// Dunning disabled or not-configured (no policy) is a DELIBERATE
				// skip, not a transient failure — return success immediately rather
				// than burning all 3 retries and emitting the misleading "operator
				// must start manually" ERROR for a tenant that never wanted
				// automated retries. A real transient error still retries below.
				return nil
			}
			lastErr = err
			continue
		}
		return nil
	}
	return fmt.Errorf("StartDunning failed after %d attempts: %w", len(delays), lastErr)
}

// persistChargeOutcomeWithRetry records the post-charge payment state (the
// PaymentUnknown/PaymentFailed status + PaymentIntent id) with a bounded
// retry, mirroring startDunningWithRetry. This write is LOAD-BEARING on the
// Unknown path: it is the sole thing that sets payment_status='unknown'
// (enrolling the invoice into the reconciler's ListUnknownPayments sweep) and
// persists the PaymentIntent id, so a transient loss must be retried before
// it strands a possibly-succeeded PI. The caller fails loud if every attempt
// fails. Pre-fix the call site swallowed both the return and the error.
func persistChargeOutcomeWithRetry(ctx context.Context, invoices InvoiceUpdater, tenantID, invoiceID string, status domain.InvoicePaymentStatus, piID, message string) error {
	delays := []time.Duration{0, 100 * time.Millisecond, 500 * time.Millisecond}
	var lastErr error
	for i, d := range delays {
		if d > 0 {
			select {
			case <-time.After(d):
			case <-ctx.Done():
				return fmt.Errorf("ctx canceled persisting charge outcome (attempt %d): %w", i+1, ctx.Err())
			}
		}
		if _, err := invoices.UpdatePayment(ctx, tenantID, invoiceID, status, piID, message, nil); err != nil {
			lastErr = err
			continue
		}
		return nil
	}
	return fmt.Errorf("persist charge outcome failed after %d attempts: %w", len(delays), lastErr)
}

func simulatedFailureAt(inv domain.Invoice) time.Time {
	failureAt := time.Now()
	if inv.IssuedAt != nil {
		failureAt = *inv.IssuedAt
	}
	var candidate time.Time
	if !inv.BillingPeriodStart.IsZero() && !inv.BillingPeriodStart.After(failureAt) && inv.BillingPeriodStart.After(candidate) {
		candidate = inv.BillingPeriodStart
	}
	if !inv.BillingPeriodEnd.IsZero() && !inv.BillingPeriodEnd.After(failureAt) && inv.BillingPeriodEnd.After(candidate) {
		candidate = inv.BillingPeriodEnd
	}
	if !candidate.IsZero() {
		failureAt = candidate
	}
	return failureAt
}

// HandleWebhook processes a Stripe webhook event and updates the
// corresponding invoice.
//
// Dedup ordering (the fix): the dedup marker is written AFTER the
// business side-effect commits, not before. The pre-2026-06 flow
// committed the dedup row first (IngestEvent's own tx) and then ran the
// handler — so a transient handler failure left a committed dedup row,
// the HTTP layer ack'd 200, and Stripe's redelivery short-circuited as
// a duplicate. The event was silently and permanently dropped.
//
// Now:
//  1. WasProcessed is a read-only idempotency pre-check.
//  2. processEvent runs the side-effect (MarkPaid / dunning / etc.).
//  3. On a TRANSIENT failure we persist NO row and return the error —
//     the HTTP handler turns that into a 5xx so Stripe redelivers and
//     the handler re-runs. "dedup row exists" now strictly implies
//     "side-effect committed."
//  4. On success OR a PERMANENT failure (the event references an entity
//     that isn't in our DB and never will be — errs.ErrNotFound) we
//     IngestEvent to persist the audit row + claim the dedup slot, then
//     ack. Acking a permanent failure is deliberate: redelivering it
//     forever achieves nothing.
//
// Business side-effects on these paths are idempotent (MarkPaid is a
// no-op on an already-paid invoice; StartDunning is UNIQUE per invoice),
// so a redelivery that races a slow first delivery and double-processes
// converges to the same state — the ON CONFLICT DO NOTHING in
// IngestEvent collapses the duplicate audit row.
func (s *Stripe) HandleWebhook(ctx context.Context, tenantID string, event domain.StripeWebhookEvent) error {
	seen, err := s.webhooks.WasProcessed(ctx, tenantID, event.StripeEventID)
	if err != nil {
		return fmt.Errorf("webhook dedup pre-check: %w", err)
	}
	if seen {
		slog.Info("duplicate webhook event, skipping", "stripe_event_id", event.StripeEventID)
		return nil
	}

	if procErr := s.processEvent(ctx, tenantID, event); procErr != nil {
		if isTransientWebhookError(procErr) {
			// No dedup row written — return the error so the HTTP layer
			// responds 5xx and Stripe redelivers the event.
			return procErr
		}
		// Permanent: record the audit row (and claim the dedup slot) so the
		// event shows up in history with its outcome, then ack.
		slog.Warn("webhook permanently unprocessable — acking without retry",
			"stripe_event_id", event.StripeEventID,
			"event_type", event.EventType,
			"error", procErr)
	}

	// Side-effect committed (or permanently un-processable) — claim the
	// dedup slot + persist the audit row.
	if _, _, err := s.webhooks.IngestEvent(ctx, tenantID, event); err != nil {
		return fmt.Errorf("ingest webhook event: %w", err)
	}
	return nil
}

// processEvent runs the side-effect for one webhook event. Returns nil on
// success (including no-op event types). A non-nil error is classified by
// HandleWebhook as transient (redeliver) or permanent (ack) via
// isTransientWebhookError.
func (s *Stripe) processEvent(ctx context.Context, tenantID string, event domain.StripeWebhookEvent) error {
	switch event.EventType {
	case "payment_intent.succeeded":
		return s.handlePaymentSucceeded(ctx, tenantID, event)
	case "payment_intent.payment_failed":
		return s.handlePaymentFailed(ctx, tenantID, event)
	case "checkout.session.completed":
		return s.handleCheckoutCompleted(ctx, tenantID, event)
	case "setup_intent.succeeded":
		return s.handleSetupIntentSucceeded(ctx, tenantID, event)
	case "setup_intent.setup_failed":
		// We don't persist anything for setup_failed — Stripe Elements
		// already surfaced the error to the customer browser. Just log
		// so operators can tell this happened from event history.
		slog.Info("setup_intent.setup_failed",
			"stripe_event_id", event.StripeEventID,
			"failure_message", event.FailureMessage)
		return nil
	case "charge.refund.updated", "refund.updated", "refund.failed":
		// One status-driven handler for all three: each carries a Refund object;
		// the source event is irrelevant once we read Refund.status.
		return s.handleRefundUpdated(ctx, tenantID, event)
	default:
		slog.Debug("unhandled webhook event type", "type", event.EventType)
		return nil
	}
}

// isTransientWebhookError reports whether a webhook processing error is
// worth a Stripe redelivery. A missing target entity (errs.ErrNotFound)
// is permanent — the event references an invoice/customer that isn't in
// our DB, and redelivering won't conjure it — so we ack it. Everything
// else (DB write failure, contention, a downstream timeout) is treated
// as transient: return it, respond 5xx, let Stripe redeliver.
func isTransientWebhookError(err error) bool {
	return !errors.Is(err, errs.ErrNotFound)
}

// handleSetupIntentSucceeded re-parses the raw payload to pull out the
// velox_customer_id + payment_method fields, then delegates to the
// PaymentMethodAttacher. Re-parsing (instead of adding fields to
// StripeWebhookEvent) keeps the event struct narrow — other consumers
// of the event don't need to know about setup-intent-specific fields.
func (s *Stripe) handleSetupIntentSucceeded(ctx context.Context, tenantID string, event domain.StripeWebhookEvent) error {
	if s.pmAttacher == nil {
		slog.Debug("setup_intent.succeeded: no PaymentMethodAttacher wired, skipping")
		return nil
	}

	raw, ok := event.Payload["raw"].(string)
	if !ok {
		return fmt.Errorf("setup_intent.succeeded: missing raw payload")
	}
	var parsed struct {
		Data struct {
			Object struct {
				ID            string            `json:"id"`
				PaymentMethod string            `json:"payment_method"`
				Customer      string            `json:"customer"`
				Metadata      map[string]string `json:"metadata"`
			} `json:"object"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return fmt.Errorf("setup_intent.succeeded: parse payload: %w", err)
	}

	customerID := parsed.Data.Object.Metadata["velox_customer_id"]
	pmID := parsed.Data.Object.PaymentMethod

	// Metadata is best-effort: Stripe Checkout doesn't copy a session's
	// metadata onto the underlying SetupIntent unless the creator set
	// setup_intent_data.metadata, and the hosted "update payment" and
	// operator add-card Checkout flows don't. Fall back to the SetupIntent's
	// authoritative `customer` field — always set for our customer-scoped
	// setup flows — so a saved card lands in payment_methods regardless of
	// which flow created the session. A non-NotFound resolver error is
	// transient (return → Stripe redelivers); NotFound (the Stripe customer
	// isn't ours) leaves customerID empty and falls through to the skip below.
	if customerID == "" && s.customerResolver != nil {
		if stripeCustomerID := parsed.Data.Object.Customer; stripeCustomerID != "" {
			resolved, rerr := s.customerResolver.CustomerIDByStripeID(ctx, tenantID, stripeCustomerID)
			if rerr != nil && !errors.Is(rerr, errs.ErrNotFound) {
				return fmt.Errorf("resolve customer by stripe id %s: %w", stripeCustomerID, rerr)
			}
			customerID = resolved
		}
	}

	if pmID == "" {
		// No payment method on the SetupIntent — nothing to attach. Ack.
		slog.Warn("setup_intent.succeeded with no payment_method — skipping",
			"stripe_event_id", event.StripeEventID)
		return nil
	}
	if customerID == "" {
		// The card was saved at Stripe but we can't yet resolve which Velox
		// customer it belongs to (no velox_customer_id in metadata AND the
		// SetupIntent.customer isn't linked locally yet — a race against the
		// customer↔Stripe-id link-write, or a session created before the
		// setup_intent_data.metadata stamp shipped). Return a NON-ErrNotFound
		// error so HandleWebhook treats it as TRANSIENT: no dedup row is
		// written, the HTTP layer returns 5xx, and Stripe REDELIVERS — by
		// which point the link (or a sibling checkout.session.completed) has
		// resolved the customer. Pre-fix this returned nil → the event was
		// dedup-marked → the saved card was permanently dropped (the invoice
		// then looped in auto-charge retry forever). Stripe bounds redelivery
		// to ~3 days, so a genuinely-foreign event can't redeliver unbounded.
		return fmt.Errorf("setup_intent.succeeded: customer not yet resolvable for stripe customer %q (pm %s) — retry",
			parsed.Data.Object.Customer, pmID)
	}

	// Attribute the attach to the CUSTOMER when this SetupIntent came from the
	// customer-driven payment-update token link (velox_purpose set on the
	// public flow's setup_intent_data.metadata). Operator-driven attaches
	// (portal "Add payment method" → "portal_add_payment_method", or the
	// inline-Elements flow → empty purpose) are left as-is so ResolveActor
	// keeps them as system/operator. Without the gate every webhook-attach
	// row — operator and customer alike — resolved to "System", so a
	// customer self-serving a card was indistinguishable from an operator
	// adding one in the audit log.
	if parsed.Data.Object.Metadata["velox_purpose"] == purposePaymentUpdateToken {
		ctx = veloxauth.WithCustomerActor(ctx, customerID)
	}

	if err := s.pmAttacher.AttachForWebhook(ctx, tenantID, customerID, pmID); err != nil {
		return fmt.Errorf("attach payment method: %w", err)
	}

	slog.Info("payment method attached via setup_intent",
		"stripe_event_id", event.StripeEventID,
		"customer_id", customerID,
		"stripe_pm_id", pmID,
	)

	s.fireEvent(ctx, tenantID, domain.EventPaymentMethodAttached, map[string]any{
		"customer_id":              customerID,
		"stripe_payment_method_id": pmID,
	})

	return nil
}

// refundWebhookRaceWindow bounds redelivery for a refund webhook that arrives
// before its credit note's stripe_refund_id row commits. The create→persist is
// synchronous (Issue/RetryRefund) and beats the async webhook, so this is rare;
// the window just has to comfortably exceed Stripe's first redelivery backoff
// while keeping a genuinely-foreign refund from looping for days.
const refundWebhookRaceWindow = 15 * time.Minute

// handleRefundUpdated reconciles the ASYNC outcome of a refund. A refund can
// return pending at create then later flip to succeeded or FAILED (bank reject /
// insufficient platform balance → money back to the platform, customer
// un-refunded), so the create-call status is not final — this webhook is the
// source of truth. Re-parses the raw payload (data.object is a Refund, NOT the
// PI-shaped struct HandleWebhook pre-parses), maps the status, and applies it
// monotonically to the matching credit note.
func (s *Stripe) handleRefundUpdated(ctx context.Context, tenantID string, event domain.StripeWebhookEvent) error {
	if s.refundUpdater == nil {
		slog.Debug("refund webhook: no RefundStatusUpdater wired, skipping", "type", event.EventType)
		return nil
	}
	raw, ok := event.Payload["raw"].(string)
	if !ok {
		return fmt.Errorf("%s: missing raw payload", event.EventType)
	}
	var parsed struct {
		Data struct {
			Object struct {
				ID      string `json:"id"`
				Status  string `json:"status"`
				Created int64  `json:"created"`
			} `json:"object"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return fmt.Errorf("%s: parse payload: %w", event.EventType, err)
	}
	rf := parsed.Data.Object
	if rf.ID == "" {
		return fmt.Errorf("%s: refund payload missing id", event.EventType)
	}
	status := mapStripeRefundStatus(rf.Status)

	err := s.refundUpdater.ApplyRefundWebhook(ctx, tenantID, rf.ID, status)
	if errors.Is(err, errs.ErrNotFound) {
		// No Velox credit note carries this refund id. Either a refund created
		// OUTSIDE Velox (dashboard / direct API) — ack and ignore, never fabricate
		// a credit note — OR our own refund whose CN row hasn't committed yet
		// (rare; the create→persist is synchronous). Disambiguate by age: a very
		// recent refund gets a bounded redelivery so the row can land; an older
		// one is foreign and is ack'd permanently.
		// Fail toward redelivery when the age is recent OR unknown (Created==0,
		// a malformed/partial payload) — better a bounded retry than silently
		// dropping a reconciliation for our own not-yet-committed refund.
		if rf.Created == 0 || time.Now().Unix()-rf.Created < int64(refundWebhookRaceWindow.Seconds()) {
			return fmt.Errorf("refund %s: no matching credit note yet (recent — retry)", rf.ID)
		}
		slog.Info("refund webhook acknowledged, no matching credit note (external/dashboard refund)",
			"refund_id", rf.ID, "stripe_status", rf.Status, "type", event.EventType)
		return nil
	}
	if err != nil {
		return fmt.Errorf("%s: apply refund status: %w", event.EventType, err)
	}
	slog.Info("refund status reconciled from webhook",
		"refund_id", rf.ID, "stripe_status", rf.Status, "velox_status", status, "type", event.EventType)
	return nil
}

func (s *Stripe) handlePaymentSucceeded(ctx context.Context, tenantID string, event domain.StripeWebhookEvent) error {
	if event.PaymentIntentID == "" {
		return fmt.Errorf("payment_intent.succeeded event missing payment_intent_id")
	}

	inv, err := s.invoices.GetByStripePaymentIntentID(ctx, tenantID, event.PaymentIntentID)
	if err != nil && event.InvoiceID != "" {
		inv, err = s.invoices.Get(ctx, tenantID, event.InvoiceID)
	}
	if err != nil {
		return fmt.Errorf("find invoice for PI %s: %w", event.PaymentIntentID, err)
	}

	// Webhook is one of the discover-then-settle entry points (ADR-049): it
	// resolves the invoice, then hands the terminal outcome to the shared
	// settlement primitive so the side-effects (mark paid, card stamp,
	// payment.succeeded event, receipt email) are identical to every other
	// settler.
	var captured int64
	if event.AmountCents != nil {
		captured = *event.AmountCents
	}
	if a, ok := anchorFromEventPayload(event); ok {
		ctx = withSettleAnchor(ctx, a)
	}
	// Currency truth-check (ADR-100): the webhook is the ONE settle entry
	// point carrying Stripe's independently-reported charge currency, so
	// compare it against the invoice header here — case-folded (Stripe
	// reports lowercase). Velox creates the PaymentIntent FROM the header,
	// so a mismatch means the charge was made under a different currency
	// than the invoice now claims — report-only tripwire via the standard
	// anomaly channel; the settle itself proceeds (the money already moved,
	// absorbing silently is the one wrong option).
	if event.Currency != "" && !strings.EqualFold(event.Currency, inv.Currency) {
		s.escalatePaymentAnomaly(ctx, tenantID, inv, domain.EventPaymentCurrencyMismatch, event.PaymentIntentID, captured,
			fmt.Sprintf("charge settled in %s but the invoice is labeled %s", strings.ToUpper(event.Currency), strings.ToUpper(inv.Currency)))
	}
	return s.SettleSucceeded(ctx, tenantID, inv, event.PaymentIntentID, captured, SourceWebhook)
}

func (s *Stripe) handlePaymentFailed(ctx context.Context, tenantID string, event domain.StripeWebhookEvent) error {
	if event.PaymentIntentID == "" {
		return fmt.Errorf("payment_intent.payment_failed event missing payment_intent_id")
	}

	// Try to find invoice by PI ID first, fall back to invoice ID from metadata
	inv, err := s.invoices.GetByStripePaymentIntentID(ctx, tenantID, event.PaymentIntentID)
	if err != nil && event.InvoiceID != "" {
		inv, err = s.invoices.Get(ctx, tenantID, event.InvoiceID)
	}
	if err != nil {
		return fmt.Errorf("find invoice for PI %s: %w", event.PaymentIntentID, err)
	}

	// Suppress the customer-facing email for two flows; the event + dunning
	// still fire (they happen inside the primitive before the email step):
	//   - hosted_invoice_pay: an interactive Pay flow — the customer is on the
	//     hosted invoice page and already saw "Your card was declined" inline;
	//     an email telling them what they just saw is noise.
	//   - dunning_retry: this PI was created by the dunning retrier, which
	//     already sent its own per-attempt warning/escalation email; firing the
	//     generic payment-failed email too would double-notify for the same
	//     attempt (Stripe Smart Retries / Lago shape: one email per attempt).
	purpose := piPurposeFromPayload(event.Payload)
	suppressEmail := purpose == "hosted_invoice_pay" || purpose == "dunning_retry"

	// Discover-then-settle (ADR-049): hand the terminal outcome to the shared
	// primitive, which owns the out-of-order guard, mark-failed, payment.failed
	// event, dunning auto-start, and the (possibly suppressed) failure email.
	return s.SettleFailed(ctx, tenantID, inv, event.PaymentIntentID, event.FailureMessage, suppressEmail, SourceWebhook)
}

// piPurposeFromPayload extracts data.object.metadata.velox_purpose from
// a Stripe webhook event payload. Returns "" if not present. The
// payload["raw"] field is the JSON body of the Stripe event; we keep
// the parse narrow to just the field we need so unrelated schema
// drift doesn't break extraction.
func piPurposeFromPayload(payload map[string]any) string {
	raw, ok := payload["raw"]
	if !ok {
		return ""
	}
	rawStr, ok := raw.(string)
	if !ok {
		return ""
	}
	var parsed struct {
		Data struct {
			Object struct {
				Metadata map[string]string `json:"metadata"`
			} `json:"object"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(rawStr), &parsed); err != nil {
		return ""
	}
	return parsed.Data.Object.Metadata["velox_purpose"]
}

func (s *Stripe) handleCheckoutCompleted(ctx context.Context, tenantID string, event domain.StripeWebhookEvent) error {
	// Extract velox_customer_id from metadata (set during checkout session creation)
	customerID := ""
	sessionID := ""
	veloxPurpose := ""
	if payload, ok := event.Payload["raw"]; ok {
		var raw struct {
			Data struct {
				Object struct {
					ID       string            `json:"id"`
					Customer string            `json:"customer"`
					Metadata map[string]string `json:"metadata"`
				} `json:"object"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(payload.(string)), &raw); err == nil {
			customerID = raw.Data.Object.Metadata["velox_customer_id"]
			sessionID = raw.Data.Object.ID
			veloxPurpose = raw.Data.Object.Metadata["velox_purpose"]
			if event.CustomerExternalID == "" {
				event.CustomerExternalID = raw.Data.Object.Customer
			}
		}
	}
	// Truth-sync the claim ledger (ADR-068): the session finished at Stripe.
	// Without this, a dropped payment_intent.succeeded past Stripe's retry
	// horizon leaves the row 'open' and the reuse path serves a completed
	// session's URL (fail-safe but dead) until the TTL. Best-effort.
	if sessionID != "" && s.checkoutSessions != nil {
		if err := s.checkoutSessions.MarkCompleted(ctx, sessionID); err != nil {
			slog.WarnContext(ctx, "checkout claim truth-sync failed", "session_id", sessionID, "error", err)
		}
	}

	if customerID == "" {
		slog.Debug("checkout.session.completed missing velox_customer_id, skipping")
		return nil
	}

	if s.paymentSetups == nil {
		slog.Warn("payment setup store not configured, cannot update payment status")
		return nil
	}

	// A checkout.session.completed event fires for BOTH save-PM and
	// no-save-PM flows. We only want to mark the customer's setup
	// as 'ready' when a PM was actually attached to the Stripe
	// customer (= can be auto-charged for future invoices).
	// FetchCardDetails lists PMs attached to the customer; success
	// confirms attachment, failure means no PM saved (a one-off
	// hosted-invoice payment without ticking "save card").
	//
	// Pre-fix the upsert ran unconditionally with status='ready'
	// + empty card details on no-save flows — operator-reported
	// inconsistency: dashboard showed "Payment method active" with
	// no card on file. Now no-save flows leave the setup row at
	// its prior state.
	if s.cardFetcher == nil || event.CustomerExternalID == "" {
		slog.Info("checkout.session.completed: no cardFetcher or stripe customer — skipping setup update",
			"customer_id", customerID)
		return nil
	}
	card, err := s.cardFetcher.FetchCardDetails(ctx, event.CustomerExternalID)
	if err != nil {
		// No PM attached to the Stripe customer — operator chose
		// not to save. The PI on this checkout session still
		// succeeded (payment_intent.succeeded handles invoice
		// state); we just don't update the customer's PM record.
		slog.Info("checkout.session.completed: no saved PM (one-off payment) — leaving customer setup unchanged",
			"customer_id", customerID,
			"stripe_customer_id", event.CustomerExternalID,
			"reason", err.Error())
		return nil
	}

	now := time.Now().UTC()
	setup := domain.CustomerPaymentSetup{
		CustomerID:                  customerID,
		TenantID:                    tenantID,
		SetupStatus:                 domain.PaymentSetupReady,
		DefaultPaymentMethodPresent: true,
		PaymentMethodType:           "card",
		StripeCustomerID:            event.CustomerExternalID,
		LastVerifiedAt:              &now,
		UpdatedAt:                   now,
		CardBrand:                   card.Brand,
		CardLast4:                   card.Last4,
		CardExpMonth:                card.ExpMonth,
		CardExpYear:                 card.ExpYear,
		StripePaymentMethodID:       card.PaymentMethodID,
	}

	// Attribute customer-driven completions to the CUSTOMER actor — the
	// same velox_purpose gate as the setup_intent attach path (#461); an
	// operator-driven or purposeless session stays system-attributed.
	auditCtx := ctx
	if veloxPurpose == "hosted_invoice_pay" || veloxPurpose == purposePaymentUpdateToken {
		auditCtx = veloxauth.WithCustomerActor(ctx, customerID)
	}
	var emit func(tx *sql.Tx) error
	if s.audit != nil {
		emit = func(tx *sql.Tx) error {
			return s.audit.LogInTx(auditCtx, tx, audit.Entry{
				Action:       domain.AuditActionUpdate,
				ResourceType: "customer",
				ResourceID:   customerID,
				Metadata: map[string]any{
					"action":             "payment_setup_completed",
					"card_brand":         setup.CardBrand,
					"card_last4":         setup.CardLast4,
					"stripe_customer_id": event.CustomerExternalID,
					"velox_purpose":      veloxPurpose,
				},
			})
		}
	}
	if _, err := s.paymentSetups.UpsertPaymentSetupAudited(ctx, tenantID, setup, emit); err != nil {
		// The customer isn't there to map — hard-deleted by a test-clock
		// teardown, or on the other livemode plane than this webhook. There is
		// nothing to record and nothing a retry would fix, so ACK the event:
		// returning an error here would loop Stripe's redelivery for days
		// against a customer that no longer exists.
		if errors.Is(err, errs.ErrNotFound) {
			slog.WarnContext(ctx, "checkout.session.completed: customer not found for payment-setup mapping — acking event",
				"customer_id", customerID, "stripe_customer_id", event.CustomerExternalID)
			return nil
		}
		return fmt.Errorf("update payment setup: %w", err)
	}

	slog.Info("payment method setup completed",
		"customer_id", customerID,
		"stripe_customer_id", event.CustomerExternalID,
		"card_brand", setup.CardBrand,
		"card_last4", setup.CardLast4,
	)

	s.fireEvent(ctx, tenantID, domain.EventPaymentMethodUpdated, map[string]any{
		"customer_id":        customerID,
		"stripe_customer_id": event.CustomerExternalID,
		"card_brand":         setup.CardBrand,
		"card_last4":         setup.CardLast4,
	})

	return nil
}
