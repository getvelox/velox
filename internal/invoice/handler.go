package invoice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/sagarsuperuser/velox/internal/api/middleware"
	"github.com/sagarsuperuser/velox/internal/api/respond"
	"github.com/sagarsuperuser/velox/internal/api/timefilter"
	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/payment"
	"github.com/sagarsuperuser/velox/internal/platform/timeline"
)

// CustomerGetter resolves customer IDs to names and billing profiles for PDF rendering.
type CustomerGetter interface {
	Get(ctx context.Context, tenantID, id string) (domain.Customer, error)
	GetBillingProfile(ctx context.Context, tenantID, customerID string) (domain.CustomerBillingProfile, error)
}

// SettingsGetter reads tenant settings for PDF company info.
type SettingsGetter interface {
	Get(ctx context.Context, tenantID string) (domain.TenantSettings, error)
}

// CreditNoteLaneCap is how many credit notes the timeline's CN lane
// fetches — the creditnote store's hard clamp. The lister interface
// carries no total, so the handler discloses `truncated` when a fetch
// comes back AT the cap (it can't know what fell off, only that
// something may have). The adapter must request exactly this many;
// requesting fewer would trip the disclosure early, more gets clamped
// silently.
const CreditNoteLaneCap = 100

// CreditNoteLister fetches credit notes for an invoice.
type CreditNoteLister interface {
	List(ctx context.Context, tenantID, invoiceID string) ([]domain.CreditNote, error)
}

// PaymentCharger creates a Stripe PaymentIntent for a finalized invoice.
type PaymentCharger interface {
	ChargeInvoice(ctx context.Context, tenantID string, inv domain.Invoice, stripeCustomerID, stripePaymentMethodID string) (domain.Invoice, error)
}

// PaymentSetupGetter checks if a customer has a payment method ready.
type PaymentSetupGetter interface {
	GetPaymentSetup(ctx context.Context, tenantID, customerID string) (domain.CustomerPaymentSetup, error)
}

// AuditStampFetcher returns the audit entries for one invoice — the
// wall-clock record of its operator/engine transitions. The timeline
// uses it as READ-TIME enrichment (ADR-104 Invariant A, corrected
// twice): lifecycle rows are derived from invoice state columns, whose
// stamps follow the entity's calendar (ADR-030), so their real-world
// moment lives only in the audit log. Joining it here is the
// subscription timeline's model applied to the invoice page — exact
// keys (action + the frozen-vocabulary metadata discriminator), never
// a time-window match. Consumer-defined; backed by audit.Logger.Query.
type AuditStampFetcher interface {
	ListByInvoice(ctx context.Context, tenantID, invoiceID string) ([]domain.AuditEntry, error)
}

// NoPaymentMethodNotifier emails the customer a payment-update link when a
// finalized invoice can't be auto-charged because no payment method is on
// file. Structurally identical to the billing engine's notifier of the same
// name (wired to the same adapter in router.go) — declared locally so the
// invoice package doesn't import the billing engine (zero cross-domain
// imports). Optional; nil means no-PM finalize just queues for retry.
type NoPaymentMethodNotifier interface {
	NotifyNoPaymentMethod(ctx context.Context, tenantID string, inv domain.Invoice, trigger string) (domain.NotifyOutcome, error)
}

// PaymentCanceler stops an invoice from being payable at Stripe when it is
// voided: expire any live Checkout session, then cancel the PaymentIntent if
// Stripe allows it (it refuses for Checkout-owned PIs — the session expiry is
// what stops those). Satisfied by *payment.Stripe.
type PaymentCanceler interface {
	StopCollection(ctx context.Context, tenantID string, inv domain.Invoice)
}

// BillingProfileGetter reads customer billing profile for PDF.
type BillingProfileGetter interface {
	GetBillingProfile(ctx context.Context, tenantID, customerID string) (domain.CustomerBillingProfile, error)
}

// DunningResolver resolves active dunning runs when an invoice is voided or paid.
type DunningResolver interface {
	ResolveByInvoice(ctx context.Context, tenantID, invoiceID string, resolution domain.DunningResolution) error
}

// WebhookEventLister lists webhook events for payment timeline.
type WebhookEventLister interface {
	ListByInvoice(ctx context.Context, tenantID, invoiceID string) ([]domain.StripeWebhookEvent, error)
}

// DunningTimelineFetcher fetches dunning data for payment timeline.
type DunningTimelineFetcher interface {
	ListRunsByInvoice(ctx context.Context, tenantID, invoiceID string) ([]domain.InvoiceDunningRun, error)
	ListEvents(ctx context.Context, tenantID, runID string) ([]domain.InvoiceDunningEvent, error)
}

// EmailSender sends invoice-related emails. ctx must carry livemode
// (set by auth middleware) so the underlying enqueue / brand lookup
// stamps the right tenant_settings + email_outbox row.
type EmailSender interface {
	SendInvoice(ctx context.Context, tenantID, to string, cc []string, customerName, invoiceNumber string, totalCents int64, currency string, pdfBytes []byte, publicToken string) error
}

// EmailEventLister surfaces customer-notification email rows
// (queued/dispatched/failed) tied to an invoice for the timeline.
// Without this, operators have no signal that the customer was
// notified — the no-PM finalize email goes out asynchronously and
// the only trace is the email_outbox row. Satisfied by
// email.OutboxStore.ListByInvoice.
type EmailEventLister interface {
	ListByInvoice(ctx context.Context, tenantID, invoiceNumber string) ([]EmailEventRow, error)
}

// EmailEventRow is the timeline-friendly view of an email_outbox row.
// Trimmed to the fields the timeline renderer needs.
type EmailEventRow struct {
	// ID is the outbox row id — the timeline's stable per-row key
	// (same-instant rows on a frozen clock made composite timestamp
	// keys collide).
	ID        string
	EmailType string
	Status    string // pending / dispatched / failed / skipped
	// DeliveryState is the provider-confirmed outcome layered over
	// Status (ADR-098): unknown / delivered / bounced / complained.
	DeliveryState string
	CreatedAt     time.Time
	DispatchedAt  *time.Time
	LastError     string
	To            string // resolved from payload
	// AttemptNumber pairs a dunning_warning email with the retry
	// attempt it warns about (payload attempt_number = run.AttemptCount
	// at send time). 0 for non-dunning types and legacy rows.
	AttemptNumber int
	// SimEffectiveAt is the billing-axis anchor stamped at enqueue
	// (ADR-104): the simulated instant that caused the send. Nil for
	// live-mode mail and for legacy rows enqueued before the anchor
	// existed — those render at their wall stamp.
	SimEffectiveAt *time.Time
}

// RefundIssuer issues a direct refund on a paid invoice. Concretely this
// creates + issues a refund credit note atomically; the handler doesn't need
// to know about credit notes as a data model. Backed by creditnote.Service.
type RefundIssuer interface {
	IssueRefund(ctx context.Context, tenantID string, input RefundInput) (domain.CreditNote, error)
}

// RefundInput is the handler-facing refund request. AmountCents=0 means
// "refund the full remaining refundable amount".
type RefundInput struct {
	InvoiceID   string
	AmountCents int64
	Reason      string
	Description string
}

// validRefundReasons matches Stripe's refund reason enum plus "other" as the
// catch-all. Constrained at the edge so the UI can render a dropdown and the
// audit log has a stable vocabulary.
var validRefundReasons = map[string]bool{
	"duplicate":             true,
	"fraudulent":            true,
	"requested_by_customer": true,
	"other":                 true,
}

// getInvoiceServer is the slice of generated.ServerInterface that this
// handler currently implements — the single OpenAPI operation
// `getInvoice` (GET /v1/invoices/{id}). As more operations migrate
// onto the typed surface, this assertion will broaden until the
// handler conforms to the full generated.ServerInterface and the chi
// mount can swap to the generated route helper. The compile-time
// `var _` below catches any drift between the spec's signature and the
// handler's implementation as a build error rather than a runtime
// 404 — same trick Stripe-go and gh-cli use when they conform to
// generated SDK interfaces.
type getInvoiceServer interface {
	GetInvoice(w http.ResponseWriter, r *http.Request, id string)
}

var _ getInvoiceServer = (*Handler)(nil)

type Handler struct {
	svc             *Service
	customers       CustomerGetter
	settings        SettingsGetter
	creditNotes     CreditNoteLister
	charger         PaymentCharger
	paymentSetups   PaymentSetupGetter
	paymentCancel   PaymentCanceler
	dunning         DunningResolver
	webhookEvents   WebhookEventLister
	emailEvents     EmailEventLister
	dunningTimeline DunningTimelineFetcher
	events          domain.EventDispatcher
	emailSender     EmailSender
	refundIssuer    RefundIssuer
	auditLogger     auditWriter
	noPMNotifier    NoPaymentMethodNotifier
	auditStamps     AuditStampFetcher
}

// auditWriter is the narrow audit-write interface the invoice handler uses.
// *audit.Logger satisfies it; declared as an interface (vs the concrete
// logger) so the handler's audit rows — action, label, metadata — are
// unit-testable with a capturing fake.
type auditWriter interface {
	Log(ctx context.Context, tenantID, action, resourceType, resourceID, resourceLabel string, metadata map[string]any) error
}

type HandlerDeps struct {
	CreditNotes     CreditNoteLister
	Charger         PaymentCharger
	PaymentSetups   PaymentSetupGetter
	PaymentCancel   PaymentCanceler
	Dunning         DunningResolver
	WebhookEvents   WebhookEventLister
	EmailEvents     EmailEventLister
	DunningTimeline DunningTimelineFetcher
	Events          domain.EventDispatcher
	RefundIssuer    RefundIssuer
}

func NewHandler(svc *Service, customers CustomerGetter, settings SettingsGetter, deps ...HandlerDeps) *Handler {
	h := &Handler{svc: svc, customers: customers, settings: settings}
	if len(deps) > 0 {
		h.creditNotes = deps[0].CreditNotes
		h.charger = deps[0].Charger
		h.paymentSetups = deps[0].PaymentSetups
		h.paymentCancel = deps[0].PaymentCancel
		h.dunning = deps[0].Dunning
		h.webhookEvents = deps[0].WebhookEvents
		h.emailEvents = deps[0].EmailEvents
		h.dunningTimeline = deps[0].DunningTimeline
		h.events = deps[0].Events
		h.refundIssuer = deps[0].RefundIssuer
	}
	return h
}

// SetEmailSender configures email sending for invoice notifications.
func (h *Handler) SetEmailSender(sender EmailSender) {
	h.emailSender = sender
}

// SetAuditStamps wires the timeline's audit enrichment (ADR-104).
func (h *Handler) SetAuditStamps(f AuditStampFetcher) { h.auditStamps = f }

// SetNoPaymentMethodNotifier wires the customer-notification dispatcher
// used when a manually-finalized invoice can't be auto-charged (no PM on
// file). Mirrors the billing engine's wiring — both receive the same
// adapter instance — so a manual one-off invoice and a cycle invoice notify
// the customer identically at finalize. Optional; nil → no-PM finalize
// still queues for scheduler retry, just without the email.
func (h *Handler) SetNoPaymentMethodNotifier(n NoPaymentMethodNotifier) {
	h.noPMNotifier = n
}

// SetEmailEvents wires the email_outbox lister used by the timeline
// to surface customer-notification events. Optional — when nil, the
// timeline omits the email rows but the rest of it (lifecycle,
// stripe webhooks, dunning) renders unchanged.
func (h *Handler) SetEmailEvents(lister EmailEventLister) {
	h.emailEvents = lister
}

// SetAuditLogger configures audit logging for financial operations.
func (h *Handler) SetAuditLogger(l auditWriter) { h.auditLogger = l }

// fireEvent dispatches a webhook event. Synchronous: with the outbox
// (RES-1) Dispatch is a short DB insert that must persist-before-return,
// and logging any failure is preferred to silently losing the event.
func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Post("/", h.create)
	r.Get("/", h.list)
	r.Get("/{id}", h.get)
	r.Get("/{id}/pdf", h.downloadPDF)
	r.Post("/{id}/finalize", h.finalize)
	r.Post("/{id}/void", h.void)
	r.Post("/{id}/line-items", h.addLineItem)
	r.Post("/{id}/send", h.sendEmail)
	r.Post("/{id}/resend-setup-link", h.resendSetupLink)
	r.Post("/{id}/collect", h.collectPayment)
	r.Post("/{id}/refund", h.refund)
	r.Post("/{id}/retry-tax", h.retryTax)
	r.Post("/{id}/rotate-public-token", h.rotatePublicToken)
	// Stripe-parity offline-payment recovery. Lets the operator mark
	// an unpaid (or uncollectible) invoice as paid without going
	// through a PaymentIntent — for cheque, wire, cash, or any
	// out-of-band collection. Stripe's equivalent is the
	// paid_out_of_band=true flag on POST /v1/invoices/{id}/pay.
	r.Post("/{id}/record-payment", h.recordOfflinePayment)
	// Stripe-parity uncollectible mark — operator-driven path. The
	// dunning automation reaches this same service method via the
	// mark_uncollectible final-action; this endpoint lets the
	// operator do it directly without waiting for retries.
	r.Post("/{id}/mark-uncollectible", h.markUncollectible)
	r.Get("/{id}/payment-timeline", h.paymentTimeline)
	return r
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())

	var input CreateInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respond.BadRequest(w, r, "invalid JSON body")
		return
	}

	// Currency resolve for one-off invoices (FLOW I8, fixed 2026-07-26):
	// explicit input → customer billing-profile currency → tenant default
	// → the service's USD fallback. Pre-fix an empty currency hardcoded
	// USD, so a bare API create for a GBP-profile customer silently
	// minted a USD invoice — the composer masked it by always sending
	// its picker value (which itself defaults to the profile currency).
	// Only this endpoint can see an empty currency: every internal
	// caller (cycle engine, prorations, threshold, BillOnCreate) stamps
	// the plan currency explicitly.
	if strings.TrimSpace(input.Currency) == "" && input.CustomerID != "" {
		if h.customers != nil {
			if bp, err := h.customers.GetBillingProfile(r.Context(), tenantID, input.CustomerID); err == nil && bp.Currency != "" {
				input.Currency = bp.Currency
			}
		}
		if input.Currency == "" && h.settings != nil {
			if ts, err := h.settings.Get(r.Context(), tenantID); err == nil && ts.DefaultCurrency != "" {
				input.Currency = ts.DefaultCurrency
			}
		}
	}

	inv, err := h.svc.Create(r.Context(), tenantID, input)
	if err != nil {
		respond.FromError(w, r, err, "invoice")
		return
	}

	respond.JSON(w, r, http.StatusCreated, inv)
}

func (h *Handler) addLineItem(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	id := chi.URLParam(r, "id")

	var input AddLineItemInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respond.BadRequest(w, r, "invalid JSON body")
		return
	}

	item, err := h.svc.AddLineItem(r.Context(), tenantID, id, input)
	if err != nil {
		respond.FromError(w, r, err, "invoice")
		return
	}

	respond.JSON(w, r, http.StatusCreated, item)
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())

	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))

	// `ids` filter (comma-separated) lets CreditNotes-and-similar
	// pages fetch the exact invoices their primary rows reference,
	// avoiding the "list-then-client-side-join" pagination bug.
	var ids []string
	if raw := strings.TrimSpace(r.URL.Query().Get("ids")); raw != "" {
		for _, id := range strings.Split(raw, ",") {
			if id = strings.TrimSpace(id); id != "" {
				ids = append(ids, id)
			}
		}
	}

	// Shared ?from / ?to contract (api/timefilter): RFC3339 instants
	// or bare YYYY-MM-DD, inclusive both ends. Malformed input is a
	// loud 400 — silently ignoring it would return an unfiltered list
	// that lies about what the operator asked for.
	createdFrom, createdTo, err := timefilter.ParseRange(r, "from", "to")
	if err != nil {
		respond.FromError(w, r, err, "invoice")
		return
	}

	filter := ListFilter{
		TenantID:       tenantID,
		CustomerID:     r.URL.Query().Get("customer_id"),
		SubscriptionID: r.URL.Query().Get("subscription_id"),
		Status:         r.URL.Query().Get("status"),
		PaymentStatus:  r.URL.Query().Get("payment_status"),
		Search:         strings.TrimSpace(r.URL.Query().Get("search")),
		CreatedFrom:    createdFrom,
		CreatedTo:      createdTo,
		Overdue:        r.URL.Query().Get("overdue") == "true",
		IDs:            ids,
		Limit:          limit,
		Offset:         offset,
		// Sort + direction are validated against a closed set in
		// the store (invoiceOrderBy). Unknown sort keys silently
		// default to created_at; unknown dir defaults to desc.
		Sort:    r.URL.Query().Get("sort"),
		SortDir: r.URL.Query().Get("dir"),
	}
	// Cursor pagination (2026-05-29). Takes precedence over offset.
	// Only supported on the default sort (created_at DESC) — a custom
	// sort + cursor combination would yield inconsistent seek-vs-
	// order pairings.
	if c := r.URL.Query().Get("after"); c != "" && filter.Sort == "" {
		if cur, err := middleware.DecodeCursor(c); err == nil {
			filter.AfterCreatedAt = cur.CreatedAt
			filter.AfterID = cur.ID
		}
	}

	invoices, total, err := h.svc.List(r.Context(), filter)
	if err != nil {
		respond.InternalError(w, r)
		slog.ErrorContext(r.Context(), "list invoices", "error", err)
		return
	}
	if invoices == nil {
		invoices = []domain.Invoice{}
	}

	if !filter.AfterCreatedAt.IsZero() && filter.AfterID != "" {
		l := filter.Limit
		if l <= 0 {
			l = 50
		} else if l > 100 {
			l = 100
		}
		hasMore := len(invoices) > l
		if hasMore {
			invoices = invoices[:l]
		}
		resp := middleware.PageResponse{Data: invoices, HasMore: hasMore}
		if hasMore && len(invoices) > 0 {
			last := invoices[len(invoices)-1]
			resp.NextCursor = middleware.EncodeCursor(last.ID, last.CreatedAt)
		}
		respond.JSON(w, r, http.StatusOK, resp)
		return
	}

	respond.List(w, r, invoices, total)
}

// GetInvoice is the OpenAPI-typed handler for `GET /v1/invoices/{id}`.
// Signature matches generated.ServerInterface so the spec, the handler,
// and the router stay aligned at compile time — see the
// `var _ generated.GetInvoiceServer = (*Handler)(nil)` assertion below.
//
// The chi route still calls h.get (which extracts the id via chi.URLParam
// and delegates here), keeping the routing layer unchanged for now. As
// more endpoints adopt this pattern we'll switch the chi mount to use
// the generated route registration helper, which calls these typed
// methods directly.
func (h *Handler) GetInvoice(w http.ResponseWriter, r *http.Request, id string) {
	tenantID := auth.TenantID(r.Context())

	inv, items, err := h.svc.GetWithLineItems(r.Context(), tenantID, id)
	if errors.Is(err, errs.ErrNotFound) {
		respond.NotFound(w, r, "invoice")
		return
	}
	if err != nil {
		respond.InternalError(w, r)
		slog.ErrorContext(r.Context(), "get invoice", "error", err)
		return
	}
	if items == nil {
		items = []domain.InvoiceLineItem{}
	}

	respond.JSON(w, r, http.StatusOK, map[string]any{
		"invoice":    inv,
		"line_items": items,
	})
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	h.GetInvoice(w, r, chi.URLParam(r, "id"))
}

func (h *Handler) finalize(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	id := chi.URLParam(r, "id")

	inv, err := h.svc.Finalize(r.Context(), tenantID, id)
	if err != nil {
		respond.FromError(w, r, err, "invoice")
		return
	}

	// The finalize audit row AND the invoice.finalized webhook are emitted
	// by service.Finalize — the canonical single writer, covering this
	// endpoint AND the tax-retry auto-finalize. Pre-fix the webhook fired
	// from here only, so the tax-retry path silently skipped it.

	// No automatic "here's your invoice" email on finalize. Velox
	// auto-charges the saved card (Stripe charge_automatically model), so
	// the customer's touchpoint is the payment receipt on success (fired
	// from the Stripe webhook) or the "set up payment method" email below
	// when there's no card on file — matching cycle invoices and Stripe's
	// auto-charge behavior, where finalizing an auto-charged invoice does
	// NOT email the invoice. Operators can still send it explicitly via
	// POST /{id}/send.

	// Bind before the post-finalize side effects (ADR-030 / ADR-104): the
	// service bound ctx for the finalize WRITE, but this call runs on the
	// handler's raw ctx, so the setup-link email it may enqueue was
	// stamped with no billing anchor and fell off the invoice's calendar.
	inv = h.collectAtFinalize(h.svc.bindForInvoice(r.Context(), tenantID, inv.ID), tenantID, inv)

	respond.JSON(w, r, http.StatusOK, inv)
}

// collectAtFinalize runs the post-finalize collection step and returns the
// possibly-updated invoice. It mirrors the billing engine's cycle-invoice
// post-finalize block so a manual one-off invoice collects identically:
//   - payment method ready → auto-charge the saved card (the Stripe webhook
//     fires the receipt on success; a decline starts dunning).
//   - no payment method → queue for the scheduler's auto-charge retry (the
//     RetryPendingCharges sweep picks it up on its next tick after the
//     customer attaches a card — attach itself kicks no charge, so collection
//     can lag attach by up to the billing interval, 1h in prod / 5m local)
//     AND email the customer a payment-update link. Pre-fix the no-PM case did nothing, so a manual
//     invoice silently went overdue — customer never told, scheduler never
//     retried.
func (h *Handler) collectAtFinalize(ctx context.Context, tenantID string, inv domain.Invoice) domain.Invoice {
	// Once the invoice is finalized, collection must not be abortable by the
	// operator's browser: this ctx is the HTTP request's, and a client
	// disconnect mid-charge would cancel the Stripe call at its most
	// ambiguous moment AND kill every write that remembers the failure — the
	// charger's own 'unknown' outcome-persist runs on this same ctx, as do
	// the retry-flag set and the notifier below. One external event would
	// erase the failure and its bookkeeping in the same stroke. WithoutCancel
	// keeps the request's values (tenant, livemode, clock binding) and drops
	// only the cancellation; the charge itself is re-bounded by the 30s
	// deadline below (the engine pipeline's shape: durable parent for
	// bookkeeping, disposable child for the risky call).
	ctx = context.WithoutCancel(ctx)

	// Drain the customer's credit balance first (ADR-088: the balance applies
	// to one-off invoices too — Stripe parity; Lago-style exclusion rejected).
	// The card below is only ever charged the post-credit remainder, and a
	// fully covered invoice falls into the zero-due settle arm. An apply
	// FAILURE queues for the retry sweep and returns WITHOUT charging (trap
	// R1: never a pre-credit card charge — the sweep re-applies atomically
	// before its own charge, so recovery pre-exists).
	if inv.AmountDueCents > 0 {
		refreshed, err := h.svc.ApplyCreditBalance(ctx, tenantID, inv.ID)
		if err != nil {
			slog.WarnContext(ctx, "credit apply failed at manual finalize — queuing for scheduler retry; never charging pre-credit",
				"invoice_id", inv.ID, "error", err)
			if serr := h.svc.SetAutoChargePending(ctx, tenantID, inv.ID, true); serr != nil {
				slog.WarnContext(ctx, "failed to mark invoice for auto-charge retry",
					"invoice_id", inv.ID, "error", serr)
			}
			return inv
		}
		inv = refreshed
	}

	if inv.AmountDueCents <= 0 {
		// Finalized with nothing left to pay (the ADR-066 class): there is no
		// payment to wait for, so the terminal state is PAID — Stripe parity:
		// zero-amount invoices auto-mark paid with no payment attempt.
		// Pre-fix this was a bare early return, which stranded the invoice
		// finalized/payment_pending FOREVER: every charge path gates on
		// amount_due > 0 (correctly), the retry sweep's predicate too, and
		// dunning never starts — it aged into a permanently-overdue attention
		// item nothing could act on. The engine's cycle, threshold, and
		// tax-retry writers all carry this settle arm; the manual writer was
		// the one that imitated the collect block without it. Draft/tax-
		// pending invoices can't slip through: our caller just finalized this
		// invoice, SettleZeroDue re-reads and requires status=finalized, and
		// the store's MarkPaid guard rejects drafts and non-ok tax
		// (DEMO-000906) as the last line.
		settled, err := h.svc.SettleZeroDue(ctx, tenantID, inv.ID)
		if err != nil {
			// Best-effort like the rest of collection: the finalize itself is
			// already authoritative; a transient settle failure leaves the
			// invoice pending for an operator retry rather than failing the
			// request.
			slog.WarnContext(ctx, "zero-due invoice could not be auto-settled at finalize",
				"invoice_id", inv.ID, "error", err)
			return inv
		}
		slog.InfoContext(ctx, "zero-due invoice auto-settled paid at finalize", "invoice_id", inv.ID)
		return settled
	}
	if h.charger == nil || h.paymentSetups == nil {
		return inv
	}
	ps, psErr := h.paymentSetups.GetPaymentSetup(ctx, tenantID, inv.CustomerID)
	// pmReady requires the PM ID itself, not just the "ready" status: the
	// charge below passes ps.StripePaymentMethodID verbatim, and the charger
	// hard-rejects an empty one — an error that lands in the decline arm,
	// which deliberately sets no retry flag (dunning owns real declines), so
	// a ready-status-without-PM-ID row would dead-end with no retry path and
	// no customer email. Routing it to the not-ready arm instead self-heals:
	// flag for the sweep + setup-link email. The engine's ResolveForCharge
	// sites check the PM ID for the same reason; status alone is an
	// implementation invariant of the current payment-setup reader, not a
	// guarantee this call site owns.
	pmReady := psErr == nil && ps.SetupStatus == domain.PaymentSetupReady &&
		ps.StripeCustomerID != "" && ps.StripePaymentMethodID != ""
	if pmReady {
		// Synchronous charge with the same 30s bound as the engine's collect
		// pipeline — without it the request rode the Stripe SDK's default
		// (~80s). The deadline applies to the charge only; the flag/notifier
		// bookkeeping below stays on the durable detached ctx.
		chargeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		charged, err := h.charger.ChargeInvoice(chargeCtx, tenantID, inv, ps.StripeCustomerID, ps.StripePaymentMethodID)
		if err == nil {
			inv = charged
			slog.InfoContext(ctx, "auto-charge initiated", "invoice_id", inv.ID)
			return inv
		}
		var pe *payment.PaymentError
		if errors.As(err, &pe) && !pe.Unknown {
			// Definite decline: the charger persisted payment_status=failed
			// and started dunning inline — dunning is the single retry owner.
			// Deliberately NO auto_charge_pending: a second retry owner
			// minting its own idempotency keys is a double-charge window, and
			// the sweep only lists payment_status='pending' rows anyway.
			slog.WarnContext(ctx, "auto-charge declined, invoice stays finalized; dunning drives collection",
				"invoice_id", inv.ID, "error", err)
			return inv
		}
		// Transient (breaker open — the charger deliberately left the invoice
		// untouched, no PI exists), ambiguous outcome (persisted 'unknown';
		// the reconciler resolves the true state against Stripe), or an
		// unclassified error. NO dunning exists for any of these — nothing
		// definitely failed — so pre-fix nothing ever retried: no flag, no
		// dunning, no email, the invoice silently aged into overdue. Queue
		// for the sweep. Safe by the sweep's own predicate: it lists only
		// payment_status='pending' rows, so the flag re-drives the breaker
		// case on the next tick and stays inert on 'unknown'/'failed' until
		// the reconciler or dunning owns the outcome.
		slog.WarnContext(ctx, "auto-charge did not complete; queuing for scheduler retry",
			"invoice_id", inv.ID, "error", err)
		if err := h.svc.SetAutoChargePending(ctx, tenantID, inv.ID, true); err != nil {
			// A failed set(true) is a liveness sink: the invoice stays
			// invisible to RetryPendingCharges forever (playbook class G).
			slog.WarnContext(ctx, "failed to mark invoice for auto-charge retry",
				"invoice_id", inv.ID, "error", err)
		}
		return inv
	}
	// No payment method on file: no charge is attempted, so dunning never
	// starts — the scheduler flag is the only retry path.
	slog.InfoContext(ctx, "no payment method at finalize, queuing for scheduler retry + notifying customer",
		"invoice_id", inv.ID, "customer_id", inv.CustomerID)
	if err := h.svc.SetAutoChargePending(ctx, tenantID, inv.ID, true); err != nil {
		slog.WarnContext(ctx, "failed to mark invoice for auto-charge retry",
			"invoice_id", inv.ID, "error", err)
	}
	if h.noPMNotifier != nil {
		outcome, err := h.noPMNotifier.NotifyNoPaymentMethod(ctx, tenantID, inv, "finalize_no_pm")
		switch {
		case err != nil:
			slog.WarnContext(ctx, "no-payment-method notification failed",
				"invoice_id", inv.ID, "error", err)
		case outcome == domain.NotifySkippedNoEmail:
			// No stamp: self-heals via the sweep if the customer gains an email.
			slog.InfoContext(ctx, "setup-link email skipped: customer has no email on file",
				"invoice_id", inv.ID)
		default:
			// Send-once marker: the auto-charge sweep revisits this invoice
			// every tick and must not duplicate the email (ADR-087 follow-up).
			if serr := h.svc.SetNoPMNotifiedAt(ctx, tenantID, inv.ID, time.Now().UTC()); serr != nil {
				slog.WarnContext(ctx, "failed to stamp no-PM notified marker",
					"invoice_id", inv.ID, "error", serr)
			}
		}
	}
	return inv
}

func (h *Handler) void(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	id := chi.URLParam(r, "id")

	inv, err := h.svc.Void(r.Context(), tenantID, id)
	if err != nil {
		respond.FromError(w, r, err, "invoice")
		return
	}

	// Stop collection at Stripe: expire live Checkout sessions, then cancel
	// the PI where cancelable. Best-effort by design (see StopCollection).
	if h.paymentCancel != nil {
		h.paymentCancel.StopCollection(r.Context(), tenantID, inv)
	}

	// Consumed-credit reversal now happens atomically inside svc.Void (status
	// flip + reversal in one tx) — single-writer, no separate best-effort step.

	// Resolve any active dunning runs for this invoice. The resolution names
	// the outcome — this path voids, so the run records invoice_voided (0170).
	if h.dunning != nil {
		if err := h.dunning.ResolveByInvoice(r.Context(), tenantID, id, domain.ResolutionInvoiceVoided); err != nil {
			slog.WarnContext(r.Context(), "failed to resolve dunning on void", "invoice_id", id, "error", err)
		} else {
			slog.InfoContext(r.Context(), "dunning resolved on invoice void", "invoice_id", id)
		}
	}

	// The void audit row rides the void transaction itself (ADR-090,
	// Service.Void emission — one canonical row for this endpoint AND
	// engine-triggered voids, which previously left no trail).

	// invoice.voided is emitted by service.Void (single-writer — covers
	// this endpoint AND engine-triggered voids via InvoiceVoider).

	respond.JSON(w, r, http.StatusOK, inv)
}

// resendSetupLink re-emails the customer the payment-METHOD setup link for a
// finalized, unpaid invoice with no card on file — the "Resend setup link"
// nudge on the no_payment_method attention card. It re-sends the SAME email the
// engine auto-sent at finalize (NotifyNoPaymentMethod → Stripe Checkout in
// SETUP mode → engine auto-charges once a card is attached), which matches that
// state's auto-charge-on-attach model. This is deliberately distinct from
// POST /{id}/send, which emails the hosted-invoice "pay this invoice" link
// (Checkout in PAYMENT mode) — a different collection path for states where the
// operator wants the customer to pay directly.
func (h *Handler) resendSetupLink(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	id := chi.URLParam(r, "id")

	inv, err := h.svc.Get(r.Context(), tenantID, id)
	if errors.Is(err, errs.ErrNotFound) {
		respond.NotFound(w, r, "invoice")
		return
	}
	if err != nil {
		respond.FromError(w, r, err, "invoice")
		return
	}
	// Finalized-unpaid, or WRITTEN OFF and unpaid. Draft/voided/paid invoices
	// have no setup link to resend.
	//
	// `uncollectible` is admitted because attaching a card is account-scoped
	// payment-method capture, not payment of this invoice — the same
	// distinction ADR-110 drew when it removed the customer's Pay button but
	// deliberately KEPT the "add a payment method" button beside it. Without
	// it, bad-debt recovery had no on-ramp: the operator's charge answers 422
	// "customer has no payment method set up", terminal invoices expose no
	// `attention` so the banner offering this action never renders, and this
	// endpoint 409'd on status — a closed loop around the exact scenario
	// recovery exists for, a customer returning with a NEW card (walked
	// 2026-08-05, FLOW D6).
	//
	// The copy MUST differ: see notifyRecoveryScoped below.
	collectible := (inv.Status == domain.InvoiceFinalized || inv.Status == domain.InvoiceUncollectible) &&
		inv.PaymentStatus != domain.PaymentSucceeded
	if !collectible {
		respond.Error(w, r, http.StatusConflict, "invalid_state", "invoice_not_collectible",
			"setup link can only be resent for an unpaid invoice that is finalized or written off")
		return
	}
	// The setup-link email tells the CUSTOMER "add a payment method and we'll
	// collect it automatically". No charge path will ever collect an invoice
	// whose payment is in flight — and for a parked one (ADR-107) that is
	// permanent. Sending it would be an outbound promise the engine cannot
	// keep, triggerable by an operator button (found by the honesty sweep,
	// 2026-07-31).
	if b := domain.PaymentBlocksAction(inv, domain.ActionResendSetupLink); b.Blocked {
		respond.Error(w, r, http.StatusConflict, "invalid_state", b.Code, b.Message)
		return
	}
	if h.noPMNotifier == nil {
		slog.ErrorContext(r.Context(), "resend setup link: no-PM notifier not wired", "invoice_id", inv.ID)
		respond.InternalError(w, r)
		return
	}
	// The TRUE cause: an operator clicked Resend. The row used to say
	// "finalize_no_pm" — a finalize that never ran.
	// Operator entry point on a clock-pinned entity — bind so the resent
	// setup-link email carries the invoice's billing anchor (ADR-030).
	resendCtx := h.svc.bindForInvoice(r.Context(), tenantID, inv.ID)
	outcome, err := h.noPMNotifier.NotifyNoPaymentMethod(resendCtx, tenantID, inv, "operator_resend")
	if err != nil {
		respond.FromError(w, r, err, "invoice")
		return
	}
	if outcome == domain.NotifySkippedNoEmail {
		// Pre-fix this fell through to 200 {"status":"sent"} — a success
		// toast for a send that never happened (the notifier's no-email
		// skip was a silent nil). The typed outcome makes the endpoint
		// honest: nothing was sent, tell the operator what works instead.
		respond.Error(w, r, http.StatusConflict, "invalid_state", "no_email_on_file",
			"customer has no email on file — add one on the customer record, or copy the setup link from the customer page and share it directly")
		return
	}

	if h.auditLogger != nil {
		_ = h.auditLogger.Log(h.svc.AuditCtx(r.Context(), inv), tenantID, domain.AuditActionSend, "invoice", inv.ID, inv.InvoiceNumber, map[string]any{
			"action":         "resend_setup_link",
			"invoice_number": inv.InvoiceNumber,
			"customer_id":    inv.CustomerID,
		})
	}

	respond.JSON(w, r, http.StatusOK, map[string]string{"status": "sent"})
}

// markUncollectible is the operator-driven Stripe-parity bad-debt
// write-off. Service-layer write + event already happen inside
// invoice.Service.MarkUncollectible (so the dunning automated path
// and ResolveRun(invoice_not_collectible) get the same contract);
// this handler adds the side-effects that only fire on the direct
// operator action: resolve any active dunning run so retry
// automation halts immediately.
//
// Industry parity: Stripe + Recurly both halt all dunning activity
// when an invoice is marked uncollectible / failed. We resolve the
// active dunning run with ResolutionRetriesExhausted-shape semantics
// (NOT invoice_not_collectible, which would loop back into the
// "also mark invoice uncollectible" branch we just executed).
func (h *Handler) markUncollectible(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	id := chi.URLParam(r, "id")

	inv, err := h.svc.MarkUncollectible(r.Context(), tenantID, id)
	if err != nil {
		respond.FromError(w, r, err, "invoice")
		return
	}

	// The marked_uncollectible audit row is written by service.MarkUncollectible
	// (the canonical writer, with richer metadata). The handler-level write that
	// used to live here — added under the mistaken belief no row existed — made
	// every operator mark-uncollectible produce TWO identical audit rows.

	// Halt dunning automation. Best-effort — failure is logged not
	// surfaced; the invoice transition is the authoritative state
	// change, and dunning runs scan the invoice status on next tick
	// anyway.
	//
	// The resolution NAMES this outcome (0170 split). This site used to pass
	// ResolutionManuallyResolved on the written grounds that "passing the
	// matching resolution would recurse via ResolveRun's cross-flow branch" —
	// which was false, and cost the column its precision for months: the
	// cross-flow branch that calls MarkUncollectible lives on ResolveRun, and
	// this calls ResolveByInvoice, which has no such branch and cannot
	// recurse. Pinned by TestMarkUncollectible_ResolvesRunAsNotCollectible.
	if h.dunning != nil {
		if err := h.dunning.ResolveByInvoice(r.Context(), tenantID, id, domain.ResolutionInvoiceNotCollectible); err != nil {
			slog.WarnContext(r.Context(), "failed to resolve dunning on mark-uncollectible", "invoice_id", id, "error", err)
		}
	}

	respond.JSON(w, r, http.StatusOK, inv)
}

// recordOfflinePayment flips an unpaid (or uncollectible) invoice to
// paid based on operator-recorded out-of-band collection (cheque,
// wire, cash). Stripe-parity: their paid_out_of_band=true flag on
// POST /v1/invoices/{id}/pay surfaces the same recovery path.
//
// Body shape: { "note": "Cheque #1234" } — single optional string.
// Amount is implicit (full amount_due); partial payments deferred to
// when a customer asks. Date stamps as clock.Now (sim-time on
// clock-pinned invoices).
//
// Side-effects: resolves any active dunning run with
// ResolutionPaymentRecovered so the dashboard reflects the recovery
// in the dunning history view.
func (h *Handler) recordOfflinePayment(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	id := chi.URLParam(r, "id")

	var input struct {
		Note string `json:"note"`
	}
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			respond.BadRequest(w, r, "invalid JSON body")
			return
		}
	}

	inv, err := h.svc.RecordOfflinePayment(r.Context(), tenantID, id, input.Note)
	if err != nil {
		respond.FromError(w, r, err, "invoice")
		return
	}

	// The payment_recorded audit row is written by service.RecordOfflinePayment
	// (the canonical writer — its row also carries recovered_from_status). The
	// handler-level write that used to live here duplicated it on every call.

	if h.dunning != nil {
		if err := h.dunning.ResolveByInvoice(r.Context(), tenantID, id, domain.ResolutionPaymentRecovered); err != nil {
			slog.WarnContext(r.Context(), "failed to resolve dunning on record-payment", "invoice_id", id, "error", err)
		}
	}

	respond.JSON(w, r, http.StatusOK, inv)
}

// rotatePublicToken mints a fresh hosted-invoice-URL token for an invoice,
// invalidating the previous one. Defensive rotation for the case where the
// public URL is ever shared where it shouldn't be (accidentally cc'd on a
// wider thread, pasted into a ticketing system, scraped from an email
// archive leak). Only finalized/paid/voided invoices carry tokens — draft
// invoices return 422, matching the store-level guard in SetPublicToken.
func (h *Handler) rotatePublicToken(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	id := chi.URLParam(r, "id")

	inv, err := h.svc.Get(r.Context(), tenantID, id)
	if err != nil {
		respond.FromError(w, r, err, "invoice")
		return
	}
	if inv.Status == domain.InvoiceDraft {
		respond.Error(w, r, http.StatusUnprocessableEntity, "invalid_request_error", "invalid_state",
			"draft invoices do not have a public token — finalize first")
		return
	}

	previousToken := inv.PublicToken
	token, err := GeneratePublicToken()
	if err != nil {
		slog.ErrorContext(r.Context(), "rotate public token: generate", "invoice_id", id, "error", err)
		respond.InternalError(w, r)
		return
	}
	if err := h.svc.SetPublicToken(r.Context(), tenantID, id, token); err != nil {
		respond.FromError(w, r, err, "invoice")
		return
	}
	inv.PublicToken = token

	if h.auditLogger != nil {
		// Audit the rotation but NOT the token values themselves —
		// plaintext tokens in the audit log would turn the log into an
		// attractive target for credential harvesting. Record only that
		// a rotation happened.
		_ = h.auditLogger.Log(h.svc.AuditCtx(r.Context(), inv), tenantID, domain.AuditActionRotate, "invoice", inv.ID, inv.InvoiceNumber, map[string]any{
			"invoice_number":           inv.InvoiceNumber,
			"customer_id":              inv.CustomerID,
			"field":                    "public_token",
			"previous_token_was_unset": previousToken == "",
		})
	}

	respond.JSON(w, r, http.StatusOK, inv)
}

func (h *Handler) sendEmail(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	id := chi.URLParam(r, "id")

	var body struct {
		Email string `json:"email"`
		// AdditionalEmails overrides the CC list for THIS send
		// (ADR-082): absent → the customer's stored additional_emails;
		// explicit [] → primary only; explicit list → validated exact
		// override. Legacy {email}-only bodies therefore now CC the
		// stored list — the Orb-parity default.
		AdditionalEmails *[]string `json:"additional_emails,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Email == "" {
		respond.BadRequest(w, r, "email is required")
		return
	}

	inv, items, err := h.svc.GetWithLineItems(r.Context(), tenantID, id)
	if errors.Is(err, errs.ErrNotFound) {
		respond.NotFound(w, r, "invoice")
		return
	}

	// This email carries a "View & pay invoice" button. Sending it for an
	// invoice whose payment is unresolved asks the customer to pay something we
	// may already have taken from them — and for a parked invoice (ADR-107) the
	// hosted page will not even offer a Pay button when they arrive.
	if b := domain.PaymentBlocksAction(inv, domain.ActionEmailInvoice); b.Blocked {
		respond.Error(w, r, http.StatusConflict, "invalid_state", b.Code, b.Message)
		return
	}
	if err != nil {
		respond.InternalError(w, r)
		return
	}
	// Operator entry point on a clock-pinned entity (ADR-030): bind once
	// here so the enqueued email carries the invoice's billing anchor and
	// lands on the invoice's own calendar (ADR-104). Without this the row
	// enqueues unanchored and sorts by real send time — the exact gap the
	// 2026-07-29 walk caught after the anchor shipped.
	ctx := h.svc.bindForInvoice(r.Context(), tenantID, inv.ID)

	cc, err := resolveSendCC(ctx, h.customers, tenantID, inv.CustomerID, body.Email, body.AdditionalEmails)
	if err != nil {
		respond.FromError(w, r, err, "invoice_email")
		return
	}

	// One shared context builder across emailed/downloaded/hosted PDFs —
	// this path previously hand-rolled a THINNER context (no buyer
	// address/tax id, no credit notes), so the emailed document diverged
	// from the downloaded one.
	bt, ci, cnInfos := BuildPDFContext(ctx, h.customers, h.settings, h.creditNotes, tenantID, &inv)

	pdfBytes, err := RenderPDF(ctx, inv, items, bt, cnInfos, ci)
	if err != nil {
		respond.InternalError(w, r)
		return
	}

	// AmountDueCents, not Total: the email template labels this figure
	// "Amount due", and credits/partial payments make the two differ —
	// telling a customer they owe the pre-credit total is wrong.
	if err := h.emailSender.SendInvoice(ctx, tenantID, body.Email, cc, bt.Name, inv.InvoiceNumber, inv.AmountDueCents, inv.Currency, pdfBytes, inv.PublicToken); err != nil {
		// Sanitize at the boundary — SMTP errors / outbox-store errors
		// would otherwise leak to the operator toast. ADR-026.
		respond.FromError(w, r, err, "invoice_email")
		return
	}

	// Explicit audit row so an operator-initiated send is recorded as
	// "Emailed INV-NNN". (It used to also displace the catch-all middleware's
	// generic "create" row; that middleware is deleted — ADR-090 — so this row is
	// now the only record.)
	// No recipient address in the append-only row (GDPR erasure) — the email
	// outbox is the delivery record; the row links the invoice + customer.
	if h.auditLogger != nil {
		_ = h.auditLogger.Log(h.svc.AuditCtx(r.Context(), inv), tenantID, domain.AuditActionSend, "invoice", inv.ID, inv.InvoiceNumber, map[string]any{
			"invoice_number": inv.InvoiceNumber,
			"customer_id":    inv.CustomerID,
		})
	}

	respond.JSON(w, r, http.StatusOK, map[string]string{"status": "sent"})
}

// resolveSendCC resolves the CC list for an operator-initiated document
// send (ADR-082 tri-state): nil override → the customer's stored
// additional_emails; explicit list (incl. empty) → validated exact
// override against the To address. Exported to the creditnote handler's
// twin via the shared domain.NormalizeAdditionalEmails; kept here as a
// helper because both send handlers in this package family need it.
func resolveSendCC(ctx context.Context, customers CustomerGetter, tenantID, customerID, to string, override *[]string) ([]string, error) {
	if override != nil {
		return domain.NormalizeAdditionalEmails(*override, to)
	}
	if customers == nil {
		// JSON-only handler wiring (tests) — no stored list to default
		// from. Production always wires the customer getter.
		return nil, nil
	}
	cust, err := customers.Get(ctx, tenantID, customerID)
	if err != nil {
		// The stored list is a default, not a hard dependency — but a
		// failed lookup means we can't honor the operator's configured
		// recipients, and silently sending primary-only would be a
		// silent drop. Fail loud; the operator retries.
		return nil, fmt.Errorf("resolve customer additional emails: %w", err)
	}
	// Stored entries equal to the To address are skipped (the operator
	// may have typed one of the CC addresses as the To override).
	kept := cust.AdditionalEmails[:0:0]
	for _, a := range cust.AdditionalEmails {
		if !strings.EqualFold(a, strings.TrimSpace(to)) {
			kept = append(kept, a)
		}
	}
	return kept, nil
}

func (h *Handler) collectPayment(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	id := chi.URLParam(r, "id")

	inv, err := h.svc.Get(r.Context(), tenantID, id)
	if errors.Is(err, errs.ErrNotFound) {
		respond.NotFound(w, r, "invoice")
		return
	}
	if err != nil {
		respond.InternalError(w, r)
		return
	}

	// `uncollectible` is admitted for BAD-DEBT RECOVERY: the customer came back
	// and the operator is running their card. The invoice is NOT reopened — it
	// stays written off until money arrives, then settles uncollectible -> paid,
	// keeping uncollectible_at in its history.
	//
	// finalized only. A written-off invoice is deliberately NOT chargeable
	// here (ADR-113): charge-the-written-off-object is a Stripe-only pattern,
	// and its three refusal gates existed only because the charged object
	// carried stale state. Recovery of a returned customer runs on NORMAL
	// rails — a fresh recovery invoice — and a written-off invoice is settled
	// by RECORDING writers only (offline payment, ADR-108 adoption).
	if inv.Status != domain.InvoiceFinalized {
		respond.Validation(w, r, "can only collect payment on a finalized invoice — for a written-off debt, issue a recovery invoice or record an offline payment")
		return
	}
	if inv.PaymentStatus == domain.PaymentSucceeded {
		respond.Validation(w, r, "invoice is already paid")
		return
	}
	if inv.PaymentStatus == domain.PaymentUnknown {
		// A possibly-succeeded payment is the reconciler's to resolve —
		// blind re-charging an ambiguous outcome is how double charges
		// happen. The claim below also excludes 'unknown'; this gate
		// exists to say WHY instead of a generic conflict.
		//
		// Carries the gate's CODE, not just its message. respond.Validation
		// flattens everything to 422/validation_error, so a walk of all four
		// parked refusals found this one alone answering a different code than
		// void/send/resend — same rule, same sentence, and an API consumer
		// still could not branch on it uniformly. The single source exists to
		// stop exactly that divergence; it only works if callers pass both
		// halves through.
		b := domain.PaymentBlocksAction(inv, domain.ActionCollectPayment)
		respond.Error(w, r, http.StatusConflict, "invalid_state", b.Code, b.Message)
		return
	}
	if inv.AmountDueCents <= 0 {
		respond.Validation(w, r, "invoice has no amount due")
		return
	}

	if h.charger == nil || h.paymentSetups == nil {
		respond.Validation(w, r, "payment provider not configured")
		return
	}

	// Per-invoice charge lease (2026-07-21 snapshot-race audit): manual
	// collect was the one charge initiator outside the mutual-exclusion
	// ring — a concurrent sweep/dunning charge, or a credit apply landing
	// between the read above and the charge below, meant a double charge
	// or an overcharge at the stale pre-credit amount. The claim's CAS
	// re-asserts the chargeable predicate against committed truth.
	claimed, err := h.svc.ClaimChargeForManualCollect(r.Context(), tenantID, id)
	if err != nil {
		respond.InternalError(w, r)
		return
	}
	if !claimed {
		respond.Validation(w, r, "a charge for this invoice is already in progress or it is no longer chargeable — refresh and retry in a few minutes")
		return
	}

	// Post-credit truth (ADR-088): drain the balance, then charge the RELOADED
	// remainder — never the handler snapshot. Charging pre-credit would
	// consummate exactly the overcharge the sweep exists to avoid.
	//
	// This runs BEFORE the payment-method check, which is the order the engine
	// sweep has always used (billing/engine.go applies credit, then settles a
	// fully-covered invoice with MarkPaid and no payment method involved). This
	// handler's comment claimed to "mirror the sweep" while doing the opposite:
	// it demanded a card first, so an invoice a customer's balance covered
	// ENTIRELY was refused for want of a card it never needed — and the
	// zero-due branch below, whose whole premise is "settle without a card
	// charge", was unreachable without one.
	//
	// Bad-debt recovery is where that bites hardest: goodwill credit issued
	// during dunning is exactly how a written-off invoice gets covered, and the
	// customer who was written off is the least likely to have a working card
	// on file.
	inv, err = h.svc.ApplyCreditBalance(r.Context(), tenantID, id)
	if err != nil {
		_ = h.svc.ReleaseChargeClaim(r.Context(), tenantID, id)
		respond.FromError(w, r, err, "credit")
		return
	}
	if inv.AmountDueCents <= 0 {
		// Credits covered it — settle without a card charge, and without ever
		// asking whether there was a card.
		settled, serr := h.svc.SettleZeroDue(r.Context(), tenantID, id)
		if serr != nil {
			respond.FromError(w, r, serr, "invoice")
			return
		}
		respond.JSON(w, r, http.StatusOK, settled)
		return
	}

	ps, err := h.paymentSetups.GetPaymentSetup(r.Context(), tenantID, inv.CustomerID)
	if err != nil || ps.SetupStatus != domain.PaymentSetupReady || ps.StripeCustomerID == "" {
		// Provably pre-Stripe — free the lease so sweep/dunning aren't
		// locked out for the window. Any credit already applied STAYS applied:
		// it was legitimately owed to this invoice, and amount_due is now
		// correct for whatever settles it later.
		_ = h.svc.ReleaseChargeClaim(r.Context(), tenantID, id)
		respond.Validation(w, r, "customer has no payment method set up")
		return
	}

	charged, err := h.charger.ChargeInvoice(r.Context(), tenantID, inv, ps.StripeCustomerID, ps.StripePaymentMethodID)
	if err != nil {
		// ADR-026 boundary sanitization: ChargeInvoice wraps a
		// *payment.PaymentError which respond.FromError detects and
		// surfaces via OperatorSafeMessage() — humanized decline
		// reason or "Payment provider rejected the request" instead
		// of raw Stripe SDK strings (idempotency conflicts, etc.).
		respond.FromError(w, r, err, "payment")
		return
	}

	// Resolve any active dunning run — manual collect payment bypasses dunning retry
	if h.dunning != nil {
		if err := h.dunning.ResolveByInvoice(r.Context(), tenantID, id, domain.ResolutionPaymentRecovered); err != nil {
			slog.WarnContext(r.Context(), "failed to resolve dunning after collect payment", "invoice_id", id, "error", err)
		}
	}

	// Explicit audit row for the money-movement action — "Collected payment on
	// INV-NNN", the row an auditor looks for when money left the customer's card
	// outside a billing run.
	if h.auditLogger != nil {
		_ = h.auditLogger.Log(h.svc.AuditCtx(r.Context(), charged), tenantID, domain.AuditActionCollect, "invoice", charged.ID, charged.InvoiceNumber, map[string]any{
			"invoice_number": charged.InvoiceNumber,
			"amount_cents":   inv.AmountDueCents,
			"currency":       inv.Currency,
		})
	}

	respond.JSON(w, r, http.StatusOK, charged)
}

// refund issues a direct refund on a paid invoice. Convenience wrapper around
// creditnote.Service.CreateRefund — the caller passes a reason + optional
// amount and gets back the issued credit note (which carries the Stripe
// refund ID and status). For partial refunds, amount_cents < amount_paid;
// default (amount_cents=0) refunds the full remaining refundable balance.
func (h *Handler) refund(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	id := chi.URLParam(r, "id")

	if h.refundIssuer == nil {
		respond.Validation(w, r, "refund provider not configured")
		return
	}

	var body struct {
		AmountCents int64  `json:"amount_cents"`
		Reason      string `json:"reason"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		respond.BadRequest(w, r, "invalid JSON body")
		return
	}

	if body.AmountCents < 0 {
		respond.Validation(w, r, "amount_cents must be non-negative")
		return
	}
	if body.Reason == "" {
		respond.Validation(w, r, "reason is required")
		return
	}
	if !validRefundReasons[body.Reason] {
		respond.Validation(w, r, "reason must be one of: duplicate, fraudulent, requested_by_customer, other")
		return
	}

	cn, err := h.refundIssuer.IssueRefund(r.Context(), tenantID, RefundInput{
		InvoiceID:   id,
		AmountCents: body.AmountCents,
		Reason:      body.Reason,
		Description: body.Description,
	})
	if errors.Is(err, errs.ErrNotFound) {
		respond.NotFound(w, r, "invoice")
		return
	}
	if err != nil {
		respond.FromError(w, r, err, "invoice")
		return
	}

	if h.auditLogger != nil {
		// Label the row with the invoice number so it reads "Refunded
		// INV-NNN" (a money-out action), matching finalize/void rows. The same
		// read supplies the sim axis (AuditCtx) — a refund issued inside a
		// simulation belongs to that clock's timeline.
		refundLabel := ""
		auditCtx := r.Context()
		if refInv, gErr := h.svc.Get(r.Context(), tenantID, id); gErr == nil {
			refundLabel = refInv.InvoiceNumber
			auditCtx = h.svc.AuditCtx(auditCtx, refInv)
		} else {
			// The refund ALREADY happened — money moved — so the row is written
			// either way; dropping it would be worse than an unlabelled one. But
			// without the invoice we cannot resolve its clock, so a refund issued
			// inside a simulation lands with NULL sim columns and silently
			// disappears from ?test_clock_id=. Say so, loudly: a row missing from
			// a filtered timeline is indistinguishable from an action that never
			// happened, and this is the only place that knows it went missing.
			slog.WarnContext(r.Context(), "refund audit row will be unstamped and unlabelled: invoice re-read failed",
				"invoice_id", id, "credit_note_id", cn.ID, "error", gErr)
		}
		_ = h.auditLogger.Log(auditCtx, tenantID, domain.AuditActionRefund, "invoice", id, refundLabel, map[string]any{
			"invoice_id":          id,
			"credit_note_id":      cn.ID,
			"credit_note_number":  cn.CreditNoteNumber,
			"refund_amount_cents": cn.RefundAmountCents,
			"stripe_refund_id":    cn.StripeRefundID,
			"refund_status":       string(cn.RefundStatus),
			"reason":              cn.Reason,
			"currency":            cn.Currency,
		})
	}

	respond.JSON(w, r, http.StatusOK, cn)
}

// retryTax re-runs tax calculation against a draft invoice in
// tax_status pending or failed. Backs the "Retry tax" action surfaced
// by the unified Attention shape. Idempotent — each call increments
// tax_retry_count and rewrites the per-line + invoice-level tax fields.
//
// 200 with the updated invoice (carrying the new Attention) on
// success or post-retry failure (a "still failing" retry is not an
// HTTP error — the dashboard wants the new code to render). 409 when
// the gate fails (status != draft, or tax_status not retryable). 404
// when the invoice doesn't exist.
func (h *Handler) retryTax(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	id := chi.URLParam(r, "id")

	// Snapshot the pre-retry attention reason for the audit trail so
	// post-mortems can answer "did the retry change anything?".
	before, _ := h.svc.Get(r.Context(), tenantID, id)

	inv, err := h.svc.RetryTax(r.Context(), tenantID, id)
	if errors.Is(err, errs.ErrNotFound) {
		respond.NotFound(w, r, "invoice")
		return
	}
	if err != nil {
		respond.FromError(w, r, err, "invoice")
		return
	}

	if h.auditLogger != nil {
		var beforeReason, afterReason string
		if before.Attention != nil {
			beforeReason = string(before.Attention.Reason)
		}
		if inv.Attention != nil {
			afterReason = string(inv.Attention.Reason)
		}
		_ = h.auditLogger.Log(h.svc.AuditCtx(r.Context(), inv), tenantID, domain.AuditActionRetryTax, "invoice", inv.ID, inv.InvoiceNumber, map[string]any{
			"invoice_number":   inv.InvoiceNumber,
			"customer_id":      inv.CustomerID,
			"tax_status":       inv.TaxStatus,
			"tax_retry_count":  inv.TaxRetryCount,
			"before_attention": beforeReason,
			"after_attention":  afterReason,
			"tax_error_code":   inv.TaxErrorCode,
		})
	}

	respond.JSON(w, r, http.StatusOK, inv)
}

// Causal tie-ranks for same-instant timeline cascades (frozen-clock
// closes stamp finalize → dunning start → retries → escalation →
// write-off → resolve at ONE instant; the rank encodes their causal
// order so ties never render effect-before-cause). Gaps left for
// future event kinds.
const (
	rankInvoiceCreated   = 10
	rankInvoiceFinalized = 20
	// A charge attempt CAUSES the dunning campaign that follows it, so a
	// same-instant decline must sort above "Payment recovery started"
	// (ADR-103 — with one payment owner the two are separate rows, and
	// effect-before-cause would be visible).
	rankChargeAttempt     = 25
	rankDunningStarted    = 30 // dunning_started, retry_scheduled
	rankRetryAttempted    = 40
	rankEscalated         = 50
	rankLifecycleTerminal = 60 // paid / voided / uncollectible
	rankCreditNote        = 70
	rankDunningResolved   = 80
	rankEmail             = 90 // external lane; rank is a formality
)

func dunningEventRank(eventType domain.DunningEventType) int {
	switch eventType {
	case domain.DunningEventRetryAttempted:
		return rankRetryAttempted
	case domain.DunningEventEscalated:
		return rankEscalated
	case domain.DunningEventResolved:
		return rankDunningResolved
	default: // dunning_started, retry_scheduled, future kinds
		return rankDunningStarted
	}
}

// rfc3339OrEmpty serializes a wall stamp for the Recorded subline;
// zero (unknown) renders nothing.
func rfc3339OrEmpty(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}

// lifecycleRecordedStamps joins an invoice's audit entries to the
// lifecycle event types they transition (ADR-104 Invariant A, corrected
// boundary №2). EXACT keys only: the top-level action for
// create/finalize/void, the frozen-vocabulary metadata discriminator for
// the two update-flavored transitions (ADR-090 froze the action set, so
// mark-uncollectible and record-payment ride action=update). Earliest
// row wins — each transition happens once, and Query returns
// newest-first, so the walk runs backwards. A transition with no audit
// row is simply absent: its timeline row renders bare, honest for
// pre-audit history.
func lifecycleRecordedStamps(entries []domain.AuditEntry) map[string]time.Time {
	out := map[string]time.Time{}
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		key := ""
		switch e.Action {
		case domain.AuditActionCreate:
			key = "invoice.created"
		case domain.AuditActionFinalize:
			key = "invoice.finalized"
		case domain.AuditActionVoid:
			key = "invoice.voided"
		case domain.AuditActionUpdate:
			switch e.Metadata["action"] {
			case "marked_uncollectible":
				key = "invoice.marked_uncollectible"
			case "payment_recorded":
				key = "invoice.paid"
			}
		}
		if key != "" && out[key].IsZero() {
			out[key] = e.CreatedAt
		}
	}
	return out
}

// sortInvoiceTimeline orders events by their full-precision source
// instant, breaking true same-instant ties by causal rank, and residual
// ties by insertion order (SliceStable; callers append sources oldest-
// first). Sorting the serialized second-truncated Timestamp string was
// the 2026-07-19 inversion class: same-second pairs kept whatever
// orientation their source query happened to use.
func sortInvoiceTimeline(events []timelineEvent) {
	timeline.SortStable(events,
		func(e timelineEvent) time.Time { return e.sortAt },
		func(a, b timelineEvent) bool { return a.tieRank < b.tieRank })

	// Reality pass (ADR-104 ordering amendment, 2026-07-29): within one
	// story-instant group, order by the REAL sequence — the recorded
	// stamps — so a frozen-clock invoice reads exactly like a wall-clock
	// one whose displayed dates happen to coincide. Gated on GROUP
	// COMPLETENESS: only when every row in the group carries a recorded
	// stamp. A partially-stamped group is legacy data (pre-0162/0163/0164
	// rows, or an engine path with no audit row), and placing bare rows
	// against stamped ones would FABRICATE a sequence — those groups keep
	// the causal ladder above, which is correct-but-canonical, never
	// wrong. Equal recorded stamps (same-tx rows share Postgres's
	// tx-stable now()) fall back to the ladder inside the group; the
	// pre-pass ordering makes SliceStable's preserved order exactly
	// ladder-then-insertion.
	for lo := 0; lo < len(events); {
		hi := lo + 1
		for hi < len(events) && events[hi].sortAt.Equal(events[lo].sortAt) {
			hi++
		}
		complete := true
		for i := lo; i < hi; i++ {
			if events[i].recordedSort.IsZero() {
				complete = false
				break
			}
		}
		if complete && hi-lo > 1 {
			group := events[lo:hi]
			sort.SliceStable(group, func(a, b int) bool {
				return group[a].recordedSort.Before(group[b].recordedSort)
			})
		}
		lo = hi
	}
}

type timelineEvent struct {
	Timestamp string `json:"timestamp"`
	// sortAt/tieRank are ordering-only, never serialized. sortAt is the
	// FULL-PRECISION source instant (the serialized Timestamp truncates
	// to seconds — sorting it inverted same-second pairs, the 2026-07-19
	// subscription-timeline class). tieRank orders true same-instant
	// cascades causally: on a frozen clock an entire close cascade
	// (finalize → dunning start → retries → escalate → write-off →
	// resolve) legitimately shares one instant, and source-major
	// insertion rendered "Marked uncollectible" above the escalation
	// that caused it. See sortInvoiceTimeline.
	// ID is a stable per-row key for the SPA. Source rows use their own
	// row id (outbox id, attempt id, dunning event id, credit-note id);
	// synthesized lifecycle rows derive a deterministic one from the
	// invoice id + kind. Composite timestamp keys collided the moment
	// rows shared a frozen-clock instant (ADR-104).
	ID              string `json:"id"`
	Source          string `json:"source"` // "stripe" / "dunning" / "lifecycle" / "email" / "credit_note"
	EventType       string `json:"event_type"`
	Status          string `json:"status"`
	Description     string `json:"description"`
	Error           string `json:"error,omitempty"`
	AmountCents     *int64 `json:"amount_cents,omitempty"`
	Currency        string `json:"currency,omitempty"`
	PaymentIntentID string `json:"payment_intent_id,omitempty"`
	AttemptCount    int    `json:"attempt_count,omitempty"`
	sortAt          time.Time
	tieRank         int
	// recordedSort is the row's real-world instant as a sortable value
	// (zero when unknown). Within one story-instant group the timeline
	// orders by it — but ONLY when every row in the group has one (the
	// group-completeness gate): a partially-stamped group is legacy data,
	// and any placement of its bare rows against stamped ones fabricates
	// a sequence, so those groups keep the causal ladder. See
	// sortInvoiceTimeline.
	recordedSort time.Time
	// Detail is a sub-line rendered beneath the row's main
	// description. Used today on invoice.paid for the payment
	// instrument ("via Visa •••• 4242"); generic enough that
	// future event types can attach their own context (e.g.
	// "after 3 retry attempts" on the same row in the dunning-
	// recovered case). Empty = no sub-line. ADR-020.
	Detail string `json:"detail,omitempty"`
	// IsSimulated marks events whose PRIMARY timestamp is on the
	// entity's simulated calendar. Post-ADR-104 every row type on a
	// clock-pinned invoice is anchored there at write time (lifecycle,
	// dunning, attempts, credit notes, and emails via the outbox
	// anchor); false only on live-mode invoices and on legacy rows
	// written before their anchor existed.
	// SPA reads this flag directly — no client-side heuristic.
	IsSimulated bool `json:"is_simulated,omitempty"`
	// RecordedAt is the REAL-WORLD instant of a row whose primary
	// timestamp is simulated — the operator contract's Invariant A
	// ("any row whose two calendars differ shows both"). Rendered as a
	// muted "Recorded <wall>" subline, mirroring the subscription
	// timeline. Empty when the calendars coincide.
	RecordedAt string `json:"recorded_at,omitempty"`
}

func piFromEventMetadata(meta map[string]any) string {
	pi, _ := meta["payment_intent_id"].(string)
	return pi
}

// renderChargeAttempts renders the charge-attempt facts — the single
// source of payment rows on the timeline (ADR-103).
//
// Exactly two suppressions, both exact-keyed, no heuristics:
//
//   - a dunning row already carrying this attempt's PaymentIntent
//     absorbs it: the campaign row ("Payment retry #2 attempted" with
//     its cause subline) is the richer telling of the same charge, and
//     it lifts the attempt's provider reason + amount onto itself;
//   - a SUCCEEDED attempt on a paid invoice defers to the "Invoice
//     paid" lifecycle row. That row is the superset — an invoice can
//     also become paid with no charge at all (credits, an offline
//     payment, a $0 total) — so it owns the success story.
//
// `pending` attempts never render (in flight; the attention banner owns
// that), and a succeeded attempt on a NOT-paid invoice does render —
// that shape is an anomaly worth seeing.
func renderChargeAttempts(events []timelineEvent, attempts []domain.InvoiceChargeAttempt, inv domain.Invoice) []timelineEvent {
	dunningIdxByPI := map[string]int{}
	for i, e := range events {
		if e.Source == "dunning" && e.PaymentIntentID != "" {
			if _, dup := dunningIdxByPI[e.PaymentIntentID]; !dup {
				dunningIdxByPI[e.PaymentIntentID] = i
			}
		}
	}
	sawSucceeded := false
	for _, a := range attempts {
		if a.Outcome == domain.ChargeAttemptSucceeded {
			sawSucceeded = true
		}
		switch a.Outcome {
		case domain.ChargeAttemptFailed, domain.ChargeAttemptUnknown:
		case domain.ChargeAttemptSucceeded:
			if inv.PaidAt != nil {
				// The paid lifecycle row owns this success's story — and
				// this attempt owns its WALL moment (a webhook settle
				// writes no operator audit row, so the attempt's
				// occurred_at is the only real-world stamp the paid row
				// can honestly carry; ADR-104 Invariant A). Lift it when
				// the audit join left the row bare.
				if a.SimEffectiveAt != nil {
					for i := range events {
						if events[i].EventType == "invoice.paid" && events[i].Source == "lifecycle" && events[i].RecordedAt == "" {
							events[i].RecordedAt = a.OccurredAt.Format(time.RFC3339)
							events[i].recordedSort = a.OccurredAt
							break
						}
					}
				}
				continue
			}
		default: // pending
			continue
		}
		// The dunning row tells this charge better — hand it the
		// provider facts and drop the attempt row.
		if idx, ok := dunningIdxByPI[a.StripePaymentIntentID]; ok && a.StripePaymentIntentID != "" {
			d := &events[idx]
			// The attempt owns payment truth, so its verbatim provider
			// reason replaces the dunning event's internally-wrapped copy
			// ("payment failed: Your card was declined." → "Your card was
			// declined.").
			if a.ProviderReason != "" {
				d.Error = a.ProviderReason
			}
			if d.AmountCents == nil && a.AmountCents > 0 {
				amt := a.AmountCents
				d.AmountCents = &amt
				d.Currency = inv.Currency
			}
			continue
		}
		events = append(events, chargeAttemptRow(a, inv))
	}
	// Tripwire (ADR-103): a card-paid invoice with no succeeded attempt
	// means the one owner of payment rows is missing its fact — the
	// timeline will simply show no charge. Out-of-band settlements
	// (offline payment, credit-covered, $0) legitimately have none.
	if inv.PaidAt != nil && !sawSucceeded &&
		inv.StripePaymentIntentID != "" && !strings.HasPrefix(inv.StripePaymentIntentID, "out_of_band:") {
		slog.Warn("invoice is paid by card but has no succeeded charge attempt — payment row will be missing (ADR-103 single-source invariant)",
			"invoice_id", inv.ID, "payment_intent_id", inv.StripePaymentIntentID)
	}
	return events
}

// chargeAttemptRow renders one attempt fact. A simulated attempt's
// PRIMARY position is its billing-axis instant, with the wall instant
// kept as RecordedAt (Invariant A); an unanchored attempt keeps its
// wall stamp — same fallback as legacy emails (ADR-104).
func chargeAttemptRow(a domain.InvoiceChargeAttempt, inv domain.Invoice) timelineEvent {
	ts := a.OccurredAt
	sim := false
	var recorded time.Time
	if a.SimEffectiveAt != nil {
		ts = *a.SimEffectiveAt
		recorded = a.OccurredAt
		sim = true
	}
	desc, status := "Payment failed", "failed"
	switch a.Outcome {
	case domain.ChargeAttemptUnknown:
		desc, status = "Charge attempted — outcome unconfirmed", "processing"
	case domain.ChargeAttemptSucceeded:
		desc, status = "Payment collected", "succeeded"
	}
	var amtPtr *int64
	if a.AmountCents > 0 {
		amt := a.AmountCents
		amtPtr = &amt
	}
	return timelineEvent{
		ID:              "attempt:" + a.ID,
		Timestamp:       ts.Format(time.RFC3339),
		sortAt:          ts,
		tieRank:         rankChargeAttempt,
		Source:          "payment",
		EventType:       "charge_attempt." + string(a.Outcome),
		Status:          status,
		Description:     desc,
		Error:           a.ProviderReason,
		AmountCents:     amtPtr,
		Currency:        inv.Currency,
		PaymentIntentID: a.StripePaymentIntentID,
		IsSimulated:     sim,
		RecordedAt:      rfc3339OrEmpty(recorded),
		recordedSort:    recorded,
	}
}

// formatPaymentCardDetail produces the sub-line shown under the
// "Invoice paid" row, e.g. "via Visa •••• 4242". Returns empty
// when card details aren't on the invoice — graceful: no
// sub-line. Brand titlecased per Stripe convention
// (visa→Visa, mastercard→Mastercard). ADR-020.
func formatPaymentCardDetail(brand, last4 string) string {
	if last4 == "" && brand == "" {
		return ""
	}
	display := brandDisplayName(brand)
	if display == "" {
		display = "card"
	}
	if last4 == "" {
		return "via " + display
	}
	return "via " + display + " •••• " + last4
}

// brandDisplayName converts Stripe's enum-form card brand to the
// title-cased form operators read on the dashboard. Mirrors
// Stripe's own display names so the timeline matches what
// operators see in the Stripe dashboard.
//
// Stripe's PaymentMethodCard.DisplayBrand returns one of: visa,
// mastercard, american_express, cartes_bancaires, diners_club,
// discover, eftpos_australia, interac, jcb, union_pay, other —
// "and may contain more values in the future" per the SDK
// comment. Unknown values fall through to a defensive
// title-case so a future-Stripe addition doesn't render as
// "cartes_bancaires" in the dashboard.
func brandDisplayName(brand string) string {
	switch strings.ToLower(brand) {
	case "visa":
		return "Visa"
	case "mastercard":
		return "Mastercard"
	case "amex", "american_express", "american express":
		return "American Express"
	case "discover":
		return "Discover"
	case "jcb":
		return "JCB"
	case "diners", "diners_club":
		return "Diners Club"
	case "unionpay", "union_pay":
		return "UnionPay"
	case "cartes_bancaires":
		return "Cartes Bancaires"
	case "eftpos_australia":
		return "Eftpos Australia"
	case "interac":
		return "Interac"
	case "other":
		return "Card"
	case "":
		return ""
	default:
		// Unknown brand — title-case each underscore-separated
		// segment so a future-Stripe value renders legibly without
		// requiring a Velox release.
		return titleCaseSnake(brand)
	}
}

// titleCaseSnake turns "cartes_bancaires" into "Cartes Bancaires"
// for unrecognised brands. Defensive default so new Stripe
// networks render passably.
func titleCaseSnake(s string) string {
	parts := strings.Split(s, "_")
	for i, p := range parts {
		if p == "" {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + strings.ToLower(p[1:])
	}
	return strings.Join(parts, " ")
}

// relevantDunningEvents filters to only operator-meaningful events.
var relevantDunningEvents = map[string]bool{
	"dunning_started": true,
	"retry_attempted": true,
	"resolved":        true,
	"escalated":       true,
}

// emailRowInstant resolves an email row's position on the timeline
// (ADR-104). The PRIMARY instant is the billing instant that caused the
// send — the outbox anchor stamped at enqueue from bound ctx; rankEmail
// then places the row after the money event it announces, since a
// notification is only ever an effect. The real-world send instant
// (dispatched_at once the relay took it, enqueue time until then)
// survives as `recorded` — the operator contract's Invariant A: a row
// whose two calendars differ shows both. Legacy rows without an anchor
// keep their wall stamp — misplaced but honest; inferring an anchor at
// read time is the heuristic the no-heuristic-proxies rule bans.
func emailRowInstant(evt EmailEventRow) (ts time.Time, recorded time.Time, sim bool) {
	wall := evt.CreatedAt
	if evt.DispatchedAt != nil {
		wall = *evt.DispatchedAt
	}
	if evt.SimEffectiveAt != nil {
		return *evt.SimEffectiveAt, wall, true
	}
	return wall, time.Time{}, false
}

// describeEmailEvent maps an email_outbox row to a timeline-friendly
// description + status. Returns empty description for email types
// that don't belong on the invoice timeline (catch-all so adding new
// templates doesn't accidentally surface them). Status maps to the
// existing dot-color grammar: succeeded (emerald), processing (blue),
// failed (red).
func describeEmailEvent(emailType, outboxStatus, deliveryState string) (string, string) {
	desc := ""
	switch emailType {
	case "invoice":
		desc = "Invoice emailed to customer"
	case "payment_receipt":
		desc = "Payment receipt emailed"
	case "payment_failed":
		desc = "Payment-failed email sent to customer"
	case "payment_setup_request":
		desc = "Customer notified — set up payment method"
	case "credit_note":
		desc = "Credit note emailed to customer"
	case "dunning_warning":
		desc = "Dunning reminder emailed"
	case "dunning_escalation":
		desc = "Dunning escalation emailed"
	default:
		return "", ""
	}
	// Map outbox row status to timeline-status grammar, layering the
	// provider-confirmed outcome (ADR-098) over a completed handoff:
	// 'dispatched' alone only means the relay accepted the message —
	// delivered/bounced/complained is what the provider then learned.
	switch outboxStatus {
	case "dispatched":
		switch deliveryState {
		case "delivered":
			return desc + " — delivered", "succeeded"
		case "bounced":
			return desc + " — bounced", "failed"
		case "complained":
			return desc + " — recipient marked it as spam", "failed"
		}
		return desc, "succeeded"
	case "failed":
		return desc + " (delivery failed)", "failed"
	case "pending":
		return desc + " (queued)", "processing"
	case "skipped":
		// Deliberately not sent — the invoice reached a TERMINAL state while
		// the row sat queued (0155). Pre-ADR-098 this fell through to the
		// default "succeeded" rendering, showing a never-sent email as sent.
		//
		// Says "closed", not "settled": the dispatcher skips on ANY terminal
		// state (dispatcher.go reads InvoiceTerminalState, which answers paid /
		// voided / uncollectible / gone). "Settled first" was true only for the
		// paid case and asserted a payment on invoices that were written off or
		// annulled — found on a written-off invoice whose row read "not sent —
		// invoice settled first" beside a "Marked uncollectible" row two lines
		// above it. The real state IS recorded (outbox last_error carries
		// "reached <state> before delivery") but is not plumbed to the
		// timeline, so this says only what it can know.
		return desc + " (not sent — the invoice was already closed)", "info"
	}
	return desc, "succeeded"
}

// emailClause renders a suppressed dunning email row as a lowercase
// clause for threading into the matching dunning row's detail subline
// ("reminder sent — bounced"). Same status/verdict grammar as
// describeEmailEvent — the row is real outbox data, just displayed on
// the retry row instead of as its own timeline entry (the entry lives
// on the customer page's "Sent emails" card, Stripe shape).
func emailClause(noun string, em EmailEventRow) string {
	switch em.Status {
	case "failed":
		return noun + " failed to send"
	case "pending":
		return noun + " queued"
	case "skipped":
		return noun + " skipped — the invoice was already closed"
	}
	switch em.DeliveryState {
	case "delivered":
		return noun + " sent — delivered"
	case "bounced":
		return noun + " sent — bounced"
	case "complained":
		return noun + " sent — recipient marked it as spam"
	}
	return noun + " sent"
}

// capFirst uppercases the leading ASCII letter so an emailClause can
// stand alone as a detail subline.
func capFirst(s string) string {
	if s == "" || s[0] < 'a' || s[0] > 'z' {
		return s
	}
	return string(s[0]-'a'+'A') + s[1:]
}

// describeDunningEvent renders one dunning lifecycle row: uniform
// machine-event TITLES ("Payment recovery started", "Payment retry #N
// attempted" — the timeline's title=event, subline=detail grammar, and
// the same "Payment recovery" vocabulary the Diagnostic card uses),
// with the row's CAUSE as the detail subline. The cause is recorded at
// write time by the caller (never re-derived): pre-fix the start row
// was cause-blind over a hardcoded payment_failed reason (repaired in
// 0161), and card-less retry rows read as real charge attempts.
func describeDunningEvent(eventType, reason string, attemptCount int) (desc, status, detail string) {
	switch eventType {
	case "dunning_started":
		switch reason {
		case string(domain.DunningCausePaymentFailed):
			return "Payment recovery started", "failed", "Card was declined — automatic retries scheduled"
		case string(domain.DunningCauseNoPaymentMethod):
			return "Payment recovery started", "scheduled", "No payment method — reminders until a card is added"
		}
		// Legacy rows with no recorded cause: the start is still true,
		// the cause is honestly absent.
		return "Payment recovery started", "scheduled", ""
	case "retry_attempted":
		desc := fmt.Sprintf("Payment retry #%d attempted", attemptCount)
		switch reason {
		case "succeeded":
			// The recovering attempt (its own row since the I5 walk).
			return desc, "succeeded", ""
		case string(domain.DunningCauseNoPaymentMethod), domain.ErrNoPaymentMethodOnRetry.Error():
			// Nothing was charged — there was no card. The tick counted
			// the attempt and reminded the customer. Rendering it as a
			// bare "attempted" read as a repeat charge failure (legacy
			// sentinel matched for pre-normalization rows: our own
			// single-writer string, not an operator spelling). The
			// reminder clause ("reminder sent — bounced") is composed
			// by the timeline builder from the actual email_outbox row
			// — claimed here it would assert a send this function
			// can't see.
			return desc, "scheduled", "No payment method"
		}
		return desc, "processing", ""
	case "resolved":
		// Substring, not equality: this one reason field carries three
		// spellings by construction — ResolveRun writes the bare resolution
		// ("invoice_voided"), ResolveByInvoice prefixes it ("invoice
		// invoice_voided"), and the engine floor derives its own from the
		// invoice status ("invoice_voided" / "invoice_uncollectible"). The
		// equality version below silently fell through to the generic label
		// for two of the three. Unlike the ADR-020 FOLD — which must never
		// key on this string — a missed spelling here only costs a less
		// specific label, and the default arm catches it.
		switch {
		case strings.Contains(reason, "payment_recovered"):
			return "Payment recovered via retry", "succeeded", ""
		case strings.Contains(reason, "not_collectible"), strings.Contains(reason, "uncollectible"):
			return "Invoice written off", "resolved", ""
		case strings.Contains(reason, "voided"), reason == string(domain.ResolutionManuallyResolved):
			return "Invoice voided", "resolved", ""
		default:
			return "Dunning resolved", "resolved", ""
		}
	case "escalated":
		// reason carries the policy.final_action that fired. ADR-036
		// amendment aligned the enum with Stripe/Lago/Recurly: pause
		// now means pause-collection (keep_as_draft), not hard pause;
		// write_off_later → mark_uncollectible; new cancel_subscription.
		switch reason {
		case "pause":
			return "Collection paused — retries exhausted", "escalated", ""
		case "mark_uncollectible":
			return "Marked uncollectible — retries exhausted", "escalated", ""
		case "cancel_subscription":
			return "Subscription canceled — retries exhausted", "escalated", ""
		default:
			return "Escalated for manual review", "escalated", ""
		}
	default:
		return eventType, "info", ""
	}
}

func (h *Handler) paymentTimeline(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	id := chi.URLParam(r, "id")

	inv, err := h.svc.Get(r.Context(), tenantID, id)
	if errors.Is(err, errs.ErrNotFound) {
		respond.NotFound(w, r, "invoice")
		return
	}
	if err != nil {
		respond.InternalError(w, r)
		return
	}

	// Draft invoices have no payment activity
	if inv.Status == domain.InvoiceDraft {
		respond.JSON(w, r, http.StatusOK, map[string]any{"events": []timelineEvent{}, "degraded": []string{}, "truncated": false})
		return
	}

	// is_simulated is the invoice's persisted, authoritative flag — stamped at
	// write time when the creating context was bound to a frozen test clock
	// (engine: the subscription is pinned; manual composer: the customer is
	// pinned). Reading it here, instead of re-deriving from the subscription's
	// CURRENT test_clock_id, fixes two defects: (1) manual one-off invoices
	// have no subscription to look through, so the old lookup always returned
	// false and dropped their badge despite simulated timestamps; (2) the badge
	// now survives a later clock unpin, since the old sub.Get was a mutable
	// read-time snapshot (the heuristic feedback_no_heuristic_proxies bans).
	// Stripe-webhook + email events stay wall-clock either way (real-world
	// dispatch time), so they don't carry this flag.
	isSimulated := inv.IsSimulated

	var events []timelineEvent

	// lifecycle rows come from the invoice row itself (a failure there
	// 500s above); side lanes degrade LOUDLY instead: named in `degraded`,
	// logged, and bannered by the UI. `truncated` discloses the one capped
	// lane (credit notes, CreditNoteLaneCap). Declared up here because the
	// audit-stamp join below is the first fallible lane.
	degraded := []string{}
	truncated := false
	degrade := func(lane string, err error) {
		degraded = append(degraded, lane)
		slog.ErrorContext(r.Context(), "payment timeline: lane degraded", "lane", lane, "invoice_id", inv.ID, "error", err)
	}

	// Wall-clock stamps for the lifecycle rows (ADR-104 Invariant A,
	// corrected boundary №2 — operator nudge, same afternoon as the CN
	// one): lifecycle rows derive from invoice state columns whose stamps
	// follow the entity's calendar, so their real-world moment lives only
	// in the audit log. Exact-keyed join; earliest row wins; a missing
	// entry leaves the row bare — honest for pre-audit history. Fetch
	// failure degrades the lane like every other source.
	lifecycleRecorded := map[string]time.Time{}
	if isSimulated && h.auditStamps != nil {
		if entries, err := h.auditStamps.ListByInvoice(r.Context(), tenantID, inv.ID); err != nil {
			degrade("audit", err)
		} else {
			lifecycleRecorded = lifecycleRecordedStamps(entries)
		}
	}

	// Lifecycle events synthesised from invoice columns. Without these,
	// freshly-finalized invoices that haven't seen a Stripe charge yet
	// render an empty timeline — operators have no chronology to read.
	// Mirrors Stripe's "Invoice activity" section which always anchors
	// on Created → Finalized regardless of payment progress.
	events = append(events, timelineEvent{
		ID:           "lifecycle:created:" + inv.ID,
		Timestamp:    inv.CreatedAt.Format(time.RFC3339),
		sortAt:       inv.CreatedAt,
		tieRank:      rankInvoiceCreated,
		Source:       "lifecycle",
		EventType:    "invoice.created",
		Status:       "succeeded",
		Description:  "Invoice created",
		IsSimulated:  isSimulated,
		RecordedAt:   rfc3339OrEmpty(lifecycleRecorded["invoice.created"]),
		recordedSort: lifecycleRecorded["invoice.created"],
	})
	if inv.IssuedAt != nil {
		amt := inv.AmountDueCents
		events = append(events, timelineEvent{
			ID:           "lifecycle:finalized:" + inv.ID,
			Timestamp:    inv.IssuedAt.Format(time.RFC3339),
			sortAt:       *inv.IssuedAt,
			tieRank:      rankInvoiceFinalized,
			Source:       "lifecycle",
			EventType:    "invoice.finalized",
			Status:       "succeeded",
			Description:  "Invoice finalized",
			RecordedAt:   rfc3339OrEmpty(lifecycleRecorded["invoice.finalized"]),
			recordedSort: lifecycleRecorded["invoice.finalized"],
			AmountCents:  &amt,
			Currency:     inv.Currency,
			IsSimulated:  isSimulated,
		})
	}
	// (Removed: synthetic "Payment deadline" event keyed off due_at.
	// Activity is for things that happened — charges, state
	// transitions, dunning attempts. A future deadline isn't an
	// activity. The deadline is already surfaced honestly in the
	// invoice header and the InvoiceAttention banner's `DueBy` line.)
	if inv.VoidedAt != nil {
		events = append(events, timelineEvent{
			ID:           "lifecycle:voided:" + inv.ID,
			Timestamp:    inv.VoidedAt.Format(time.RFC3339),
			sortAt:       *inv.VoidedAt,
			tieRank:      rankLifecycleTerminal,
			Source:       "lifecycle",
			EventType:    "invoice.voided",
			Status:       "canceled",
			Description:  "Invoice voided",
			RecordedAt:   rfc3339OrEmpty(lifecycleRecorded["invoice.voided"]),
			recordedSort: lifecycleRecorded["invoice.voided"],
			IsSimulated:  isSimulated,
		})
	}
	if inv.UncollectibleAt != nil {
		events = append(events, timelineEvent{
			ID:           "lifecycle:uncollectible:" + inv.ID,
			Timestamp:    inv.UncollectibleAt.Format(time.RFC3339),
			sortAt:       *inv.UncollectibleAt,
			tieRank:      rankLifecycleTerminal,
			Source:       "lifecycle",
			EventType:    "invoice.marked_uncollectible",
			Status:       "canceled",
			Description:  "Marked uncollectible — written off as bad debt",
			RecordedAt:   rfc3339OrEmpty(lifecycleRecorded["invoice.marked_uncollectible"]),
			recordedSort: lifecycleRecorded["invoice.marked_uncollectible"],
			IsSimulated:  isSimulated,
		})
	}
	if inv.PaidAt != nil {
		amt := inv.AmountPaidCents
		desc := "Invoice paid"
		detail := formatPaymentCardDetail(inv.PaymentCardBrand, inv.PaymentCardLast4)
		// Operator-recorded offline payments (cheque/wire/cash) stamp a
		// synthetic out_of_band: payment-intent id — surface them as what
		// they are instead of rendering identically to a card payment.
		if strings.HasPrefix(inv.StripePaymentIntentID, "out_of_band:") {
			// One word, one sense (operator-ambiguity tiebreaker,
			// 2026-07-29): this row once said "recorded" three ways —
			// title, agent line, timestamp label. The timestamp label
			// owns the word product-wide (Invariant C), so the title
			// states the FACT (payment received out of band) and the
			// agent line uses "entered".
			desc = "Payment received (offline)"
			detail = "Entered by an operator — cheque, wire, or other out-of-band payment"
		}
		events = append(events, timelineEvent{
			ID:           "lifecycle:paid:" + inv.ID,
			Timestamp:    inv.PaidAt.Format(time.RFC3339),
			sortAt:       *inv.PaidAt,
			tieRank:      rankLifecycleTerminal,
			Source:       "lifecycle",
			EventType:    "invoice.paid",
			Status:       "succeeded",
			RecordedAt:   rfc3339OrEmpty(lifecycleRecorded["invoice.paid"]),
			recordedSort: lifecycleRecorded["invoice.paid"],
			Description:  desc,
			AmountCents:  &amt,
			Currency:     inv.Currency,
			Detail:       detail,
			IsSimulated:  isSimulated,
		})
	}

	// Lane-degradation disclosure (2026-07-19 audit, finding 4): a
	// side-lane fetch failure used to be swallowed (`if err == nil`) — the
	// lane silently vanished, and a timeline missing its dunning rows is
	// indistinguishable from an invoice that never saw dunning. The core

	// Credit-note chronology rows. The settlement waterfall on the page
	// already shows credit notes channel-by-channel; these rows give the
	// SAME facts a place in the chronology ("Invoice paid" then silence
	// after a refund read as nothing-happened). Issued notes only —
	// drafts aren't activity yet, voided notes vanish from the story the
	// same way Stripe's do. Each row carries the CN's OWN is_simulated:
	// operator-HTTP CNs stamp WALL-CLOCK issued_at (the HTTP path doesn't
	// bind the customer clock) → is_simulated=false → real-time lane;
	// engine clawbacks (downgrade/cancel proration) issue under the
	// clock-pinned sub's bound time → is_simulated=true → billing
	// (Activity) lane, sorted with the other simulated rows. Pre-fix all
	// CNs were tagged with the INVOICE's flag and routed to the real-time
	// lane, so an engine CN showed a simulated timestamp in the wall-clock
	// lane (migration 0117 added the per-CN flag).
	if h.creditNotes != nil {
		if cns, err := h.creditNotes.List(r.Context(), tenantID, inv.ID); err != nil {
			degrade("credit_notes", err)
		} else {
			// The lister fetches at most CreditNoteLaneCap notes; at the
			// cap we can't know what fell off, only that something may
			// have (the interface carries no total).
			if len(cns) >= CreditNoteLaneCap {
				truncated = true
			}
			// Store returns created_at DESC — reverse so residual
			// exact-tie insertion order is causal (oldest first).
			slices.Reverse(cns)
			for _, cn := range cns {
				if cn.Status != domain.CreditNoteIssued || cn.IssuedAt == nil {
					continue
				}
				total := cn.TotalCents
				// RefundAmountCents is what was ALLOCATED to the card;
				// RefundStatus is whether that money actually moved. Labelling
				// from the allocation alone reported an intention as an outcome:
				// a credit note whose Stripe refund failed still read "Refund
				// issued" on the invoice an operator opens to ask what happened
				// (found walking FLOW C2 2026-08-03 on CN-000027 —
				// refund_status=failed, no stripe_refund_id, no money moved,
				// timeline said "Refund issued · $10.00"). The credit note row
				// and the needs-attention queue both already carry the truth;
				// this surface simply wasn't reading it.
				desc := "Credit note issued"
				if cn.RefundAmountCents > 0 {
					full := cn.RefundAmountCents == cn.TotalCents
					// Exhaustive on purpose: "money moved" is the ONLY state
					// that may render as issued. `none` with an allocation is a
					// real stored state (a leg that never executed — e.g. no
					// refunder configured), and it must not borrow the
					// success wording just because it isn't an explicit failure.
					switch cn.RefundStatus {
					case domain.RefundSucceeded, "":
						desc = "Refund issued"
						if !full {
							desc = "Credit note issued — part refunded to card"
						}
					case domain.RefundFailed:
						desc = "Refund failed"
						if !full {
							desc = "Credit note issued — card refund failed"
						}
					case domain.RefundPending:
						desc = "Refund pending"
						if !full {
							desc = "Credit note issued — card refund pending"
						}
					default:
						desc = "Refund not processed"
						if !full {
							desc = "Credit note issued — card refund not processed"
						}
					}
				}
				// The row's STATUS drives the UI's verdict dot, and it must
				// agree with the words beside it: the description switch
				// above tells the truth per refund_status, but the status
				// was hardcoded "succeeded" — so a CN whose card refund
				// FAILED rendered an emerald success dot next to the words
				// "Refund failed" (2026-08-04 failed-refund invoice census;
				// the text channel was fixed 2026-08-03, the visual channel
				// was not). Refund-less CNs (credit / out-of-band / pure
				// adjustment) settle inside the Issue transaction, so
				// "succeeded" stays true for them.
				cnStatus := "succeeded"
				if cn.RefundAmountCents > 0 {
					switch cn.RefundStatus {
					case domain.RefundSucceeded, "":
						cnStatus = "succeeded"
					case domain.RefundFailed:
						cnStatus = "failed"
					default:
						// pending, and none-with-allocation ("not
						// processed"): money has not moved — never green.
						cnStatus = "pending"
					}
				}
				detail := cn.CreditNoteNumber
				if cn.Reason != "" {
					detail = cn.CreditNoteNumber + " — " + cn.Reason
				}
				// Invariant A (boundary corrected post-walk): a credit
				// note on a clock-pinned invoice is dated on the entity's
				// calendar, so its real-world insert instant renders as
				// the same "Recorded" subline emails and attempts carry.
				// Nil RecordedAt (pre-0164 rows) → no subline, honestly.
				var cnRecorded time.Time
				if cn.IsSimulated && cn.RecordedAt != nil {
					cnRecorded = *cn.RecordedAt
				}
				events = append(events, timelineEvent{
					ID:           "cn:" + cn.ID,
					Timestamp:    cn.IssuedAt.Format(time.RFC3339),
					sortAt:       *cn.IssuedAt,
					tieRank:      rankCreditNote,
					Source:       "credit_note",
					RecordedAt:   rfc3339OrEmpty(cnRecorded),
					recordedSort: cnRecorded,
					EventType:    "credit_note.issued",
					Status:       cnStatus,
					Description:  desc,
					AmountCents:  &total,
					Currency:     cn.Currency,
					Detail:       detail,
					IsSimulated:  cn.IsSimulated,
				})
			}
		}
	}

	// Customer-notification email events. Surfaces "Customer notified
	// — payment method required" / "Receipt sent" / "Dunning warning
	// emailed" alongside the Stripe + dunning rows. Without this,
	// operators have no signal that the customer was actually told
	// about the issue — the email outbox is the durable trace.
	//
	// Suppressed dunning emails are captured here and threaded into
	// the matching dunning rows' detail sublines below, so a bounced
	// reminder is visible from the invoice without re-introducing the
	// wall-clock rows. Warnings pair by payload attempt_number =
	// run.AttemptCount at send time (exact key, no index heuristics);
	// the payload carries no run id, so across multiple runs on one
	// invoice a repeated attempt number keeps the newest email —
	// accepted display nuance on an already-rare shape.
	reminderByAttempt := map[int]EmailEventRow{}
	var escalationEmail *EmailEventRow
	if h.emailEvents != nil {
		emailEvts, err := h.emailEvents.ListByInvoice(r.Context(), tenantID, inv.InvoiceNumber)
		if err != nil {
			degrade("emails", err)
		} else {
			for _, evt := range emailEvts {
				// Dunning warning + escalation emails are surfaced in the
				// per-customer "Sent emails" section on CustomerDetail
				// (Stripe shape — `docs.stripe.com/invoicing/send-email`
				// lists the email log on the customer page, not the
				// invoice page). Suppressing the rows here avoids the
				// wall-clock-vs-simulated-time visual mismatch in the
				// invoice activity timeline — those rows would show
				// "May 16, 2026" send times next to dunning state rows
				// at simulated cycle dates like "Mar 4, 2025."
				//
				// payment_failed (initial charge) still flows through as
				// its own row. It used to be folded onto the charge
				// failure as a "Customer notified by email" sub-line;
				// ADR-103 dropped that fold because pairing the two
				// needed a time window, and telling the customer is a
				// distinct fact with its own timestamp.
				if evt.EmailType == "dunning_warning" || evt.EmailType == "dunning_escalation" {
					if evt.EmailType == "dunning_warning" && evt.AttemptNumber > 0 {
						reminderByAttempt[evt.AttemptNumber] = evt
					} else if evt.EmailType == "dunning_escalation" {
						e := evt
						escalationEmail = &e
					}
					continue
				}
				desc, status := describeEmailEvent(evt.EmailType, evt.Status, evt.DeliveryState)
				if desc == "" {
					continue
				}
				ts, recorded, sim := emailRowInstant(evt)
				events = append(events, timelineEvent{
					ID:           "email:" + evt.ID,
					Timestamp:    ts.Format(time.RFC3339),
					sortAt:       ts,
					tieRank:      rankEmail,
					Source:       "email",
					EventType:    "email." + evt.EmailType,
					Status:       status,
					Description:  desc,
					Error:        evt.LastError,
					IsSimulated:  sim,
					RecordedAt:   rfc3339OrEmpty(recorded),
					recordedSort: recorded,
				})
			}
		}
	}

	// Stripe webhook rows are NOT a display source (ADR-103). The
	// webhook table is ingestion infrastructure — idempotency + replay —
	// and it emits 2-3 rows of provider chatter per charge (created,
	// then succeeded/failed). Payments are rendered from the ONE thing
	// Velox owns about them: invoice_charge_attempts, resolved inside
	// the settle transaction. That deleted three separate reconciliation
	// mechanisms (lifecycle dedup, failed-twin folding by PI-then-index,
	// and email folding by a 2-minute time window) along with their
	// heuristics.

	// Fetch dunning events. Track the max attempt count across the
	// run so we can attach it to the lifecycle invoice.paid row
	// when this run resolved into payment success — the operator
	// then sees "Invoice paid · after 3 retry attempts" in one row
	// instead of separate paid + dunning-resolved entries.
	maxAttemptCount := 0
	if h.dunningTimeline != nil {
		runs, err := h.dunningTimeline.ListRunsByInvoice(r.Context(), tenantID, id)
		if err != nil {
			degrade("dunning", err)
		} else {
			// ListRuns returns created_at DESC — reverse so multi-run
			// invoices append run 1 before run 2 (residual exact-tie
			// insertion order stays causal).
			slices.Reverse(runs)
			dunningDegraded := false
			for _, run := range runs {
				runEvents, err := h.dunningTimeline.ListEvents(r.Context(), tenantID, run.ID)
				if err != nil {
					// One run's events failing still loses a slice of the
					// lane — disclose it (once), don't just skip the run.
					if !dunningDegraded {
						dunningDegraded = true
						degrade("dunning", err)
					}
					continue
				}
				for _, evt := range runEvents {
					if !relevantDunningEvents[string(evt.EventType)] {
						continue
					}
					if evt.AttemptCount > maxAttemptCount {
						maxAttemptCount = evt.AttemptCount
					}
					// Suppress dunning 'resolved' when a lifecycle cause row
					// already says it (ADR-020 fold): "Invoice paid" owns a
					// recovered run, "Marked uncollectible" owns a write-off,
					// "Invoice voided" owns a void-flavored resolution. Gated
					// on the invoice's own transition FIELDS — never the
					// event's reason string, because each resolver spells its
					// reason differently ("manually_resolved",
					// "invoice_voided", "invoice manually_resolved" from
					// ResolveByInvoice, …) and a reason-matched fold silently
					// misses the next spelling (found live twice on FLOW I13:
					// first the write-off twin, then NIM-000233's void twin
					// surviving the reason-matched version of this fold). A
					// failed propagation (run resolved, invoice never
					// transitioned) sets none of the fields, so the dunning
					// row stays as the only surviving record.
					if string(evt.EventType) == "resolved" &&
						(inv.PaidAt != nil || inv.UncollectibleAt != nil || inv.VoidedAt != nil) {
						continue
					}
					desc, status, detail := describeDunningEvent(string(evt.EventType), evt.Reason, evt.AttemptCount)
					// Reason is machine vocabulary (cause enums, our no-PM
					// sentinel) once a detail subline carries the operator
					// copy — don't ship it as red error text. Decline
					// retries keep their provider reason; the start row's
					// provider message lifts in via the Stripe fold.
					rowErr := evt.Reason
					if detail != "" || evt.EventType == domain.DunningEventStarted {
						rowErr = ""
					}
					// Thread the suppressed dunning email's delivery
					// verdict (ADR-098) into the row it belongs to.
					// Skipped for the succeeded retry: warnings are only
					// enqueued after FAILED attempts, so a same-numbered
					// email would belong to another run's failure.
					if status != "succeeded" {
						switch evt.EventType {
						case domain.DunningEventRetryAttempted:
							if em, ok := reminderByAttempt[evt.AttemptCount]; ok {
								clause := emailClause("reminder", em)
								if detail != "" {
									detail += " — " + clause
								} else {
									detail = capFirst(clause)
								}
							}
						case domain.DunningEventEscalated:
							if escalationEmail != nil {
								detail = capFirst(emailClause("escalation email", *escalationEmail))
							}
						}
					}
					var dunRecorded time.Time
					if isSimulated && evt.RecordedAt != nil {
						dunRecorded = *evt.RecordedAt
					}
					events = append(events, timelineEvent{
						ID:           "dunning:" + evt.ID,
						Timestamp:    evt.CreatedAt.Format(time.RFC3339),
						sortAt:       evt.CreatedAt,
						tieRank:      dunningEventRank(evt.EventType),
						Source:       "dunning",
						RecordedAt:   rfc3339OrEmpty(dunRecorded),
						recordedSort: dunRecorded,
						EventType:    string(evt.EventType),
						Status:       status,
						Description:  desc,
						Detail:       detail,
						Error:        rowErr,
						AttemptCount: evt.AttemptCount,
						IsSimulated:  isSimulated,
						// Exact-attribution key written by the dunning
						// retry path (RetryError); empty on legacy rows.
						PaymentIntentID: piFromEventMetadata(evt.Metadata),
					})
				}
			}
		}
	}

	// Attach attempt count to the lifecycle invoice.paid row when
	// the invoice was collected via dunning retry. The frontend
	// renders "after N retry attempts" as a sub-line, replacing
	// the now-suppressed dunning 'resolved' row.
	//
	// NOT on a bad-debt recovery. A written-off invoice's retries all FAILED —
	// that is why it was written off, and its run resolved
	// invoice_not_collectible. Crediting "paid after N retry attempts" to a
	// recovery would hand the operator's manual collection to the campaign that
	// gave up on it, and it is the count of failures being shown as the cause
	// of success. The write-off stamp is the discriminator: it is set only on
	// invoices whose collection was abandoned, and it survives the recovery
	// (which is exactly why it can be used here).
	if inv.PaidAt != nil && inv.UncollectibleAt == nil && maxAttemptCount > 0 {
		for i := range events {
			if events[i].Source == "lifecycle" && events[i].EventType == "invoice.paid" {
				events[i].AttemptCount = maxAttemptCount
				break
			}
		}
	}

	// Payments render from ONE owner: the charge-attempt facts
	// (ADR-103). A dunning row that already carries the attempt's
	// PaymentIntent absorbs it (the campaign row is the richer telling
	// of the same charge) — an EXACT id match, not the old
	// PI-then-positional-index guess. Everything else renders as its
	// own row.
	if attempts, aerr := h.svc.ListChargeAttempts(r.Context(), tenantID, inv.ID); aerr != nil {
		degrade("charge_attempts", aerr)
	} else {
		events = renderChargeAttempts(events, attempts, inv)
	}

	// Full-precision ascending with CAUSAL tie ranks — see
	// sortInvoiceTimeline. The prior string sort compared second-
	// truncated timestamps and fell back to source-major insertion
	// order, which was anti-causal for every same-instant pair the
	// TIMELINE-ORDER flow didn't pin (escalation vs write-off, failed
	// retry vs same-second settle) and preserved the DESC orientation
	// of the runs/credit-note source queries within collided seconds.
	sortInvoiceTimeline(events)

	respond.JSON(w, r, http.StatusOK, map[string]any{"events": events, "degraded": degraded, "truncated": truncated})
}

func (h *Handler) downloadPDF(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	id := chi.URLParam(r, "id")

	inv, items, err := h.svc.GetWithLineItems(r.Context(), tenantID, id)
	if errors.Is(err, errs.ErrNotFound) {
		respond.NotFound(w, r, "invoice")
		return
	}
	if err != nil {
		respond.InternalError(w, r)
		return
	}

	// One shared context builder across emailed/downloaded/hosted PDFs.
	bt, ci, cnInfos := BuildPDFContext(r.Context(), h.customers, h.settings, h.creditNotes, tenantID, &inv)

	pdfBytes, err := RenderPDF(r.Context(), inv, items, bt, cnInfos, ci)
	if err != nil {
		respond.InternalError(w, r)
		return
	}

	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", "inline; filename=\""+inv.InvoiceNumber+".pdf\"")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(pdfBytes)
}
