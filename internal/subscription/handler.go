package subscription

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/sagarsuperuser/velox/internal/api/respond"
	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/activity"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/platform/timeline"
	"github.com/sagarsuperuser/velox/internal/tax"
)

// PlanReader reads plan data for proration calculations.
type PlanReader interface {
	GetPlan(ctx context.Context, tenantID, id string) (domain.Plan, error)
}

// ProrationInvoiceCreator creates finalized proration invoices and supports
// idempotent retry via a source-of-truth lookup.
//
// GetByProrationSource now takes the subscription_item_id alongside
// plan_changed_at — two items on the same subscription can be changed in the
// same wall-clock moment, and the pre-FEAT-5 key (subscription,
// plan_changed_at) would collide between them. The store's dedup index
// matches this tuple.
type ProrationInvoiceCreator interface {
	// CreateInvoiceWithLineItemsAudited writes the invoice in its own
	// transaction and runs emit on it before commit (nil emit = none). The
	// non-atomic path uses it so the finalize audit row commits with the
	// invoice, as on the atomic path.
	CreateInvoiceWithLineItemsAudited(ctx context.Context, tenantID string, inv domain.Invoice, items []domain.InvoiceLineItem, emit func(tx *sql.Tx, out domain.Invoice) error) (domain.Invoice, error)
	GetByProrationSource(ctx context.Context, tenantID, subscriptionID, subscriptionItemID string, changeType domain.ItemChangeType, changeAt time.Time) (domain.Invoice, error)
	NextInvoiceNumber(ctx context.Context, tenantID string) (string, error)
	// FindBaseInvoiceForPeriod gates immediate in_advance proration on
	// whether the source invoice was actually paid; a written-off one is
	// unpaid (ADR-050 amendment 2026-10-23). Mirrors the engine's
	// cancel-relief paid/unpaid split; same industry rationale (Chargebee
	// Refundable vs Adjustment / Stripe proration_behavior=none).
	FindBaseInvoiceForPeriod(ctx context.Context, tenantID, subscriptionID string, periodStart time.Time) (domain.Invoice, error)
	// FindFundingInvoicesForPeriod returns EVERY invoice that funded the
	// current period — the base invoice AND any mid-period upgrade/qty
	// proration invoice — so a downgrade/qty-decrease/item-removal clawback can
	// be fanned across them (LIFO for a plan downgrade, proportional for
	// fungible qty/item changes) instead of overrunning a single invoice's
	// credit-note cap. ErrNotFound when nothing funded the period.
	FindFundingInvoicesForPeriod(ctx context.Context, tenantID, subscriptionID string, periodStart, periodEnd time.Time) ([]domain.Invoice, error)
	// Tx variants — write the proration invoice + allocate the
	// invoice-number inside a caller-owned transaction. Used by the
	// atomic AddItem-with-proration flow so a failed proration insert
	// rolls back the sub-item insert too. ADR-030 atomic-proration
	// follow-through (2026-05-29).
	CreateInvoiceWithLineItemsTx(ctx context.Context, tx *sql.Tx, tenantID string, inv domain.Invoice, items []domain.InvoiceLineItem) (domain.Invoice, error)
	NextInvoiceNumberTx(ctx context.Context, tx *sql.Tx, tenantID string) (string, error)
	// SetAutoChargePending enrolls a finalized proration CHARGE invoice into the
	// auto-charge sweep so it actually collects — wall-clock RetryPendingCharges
	// for live subs, RetryPendingChargesForClock during catchup for clock-pinned
	// subs. Without it an upgrade/add/qty-increase invoice is finalized but never
	// charged (no creation-site enrollment), unlike engine cycle/create invoices.
	// Idempotent; the sweep's status='finalized' AND payment_status='pending'
	// filter gates when it fires (so enrolling a still-draft tax-pending invoice
	// is safe — it stays parked until tax resolves and it finalizes).
	SetAutoChargePending(ctx context.Context, tenantID, id string, pending bool) error
	// SetAutoChargePendingTx is the tx-aware variant — enrolls inside the caller's
	// tx so enrollment commits atomically with the proration item change.
	SetAutoChargePendingTx(ctx context.Context, tx *sql.Tx, tenantID, id string, pending bool) error
}

// InvoiceFinalizeRecorder records that an invoice was born finalized: the
// finalize audit row, on the transaction that inserts the invoice, and the
// invoice.finalized webhook, after that transaction commits. *billing.Engine
// satisfies it, so a proration invoice gets the row and the payload the engine
// writes for a cycle or first invoice (#1279). Both calls skip a draft, which
// gets them from invoice.Service.Finalize when it finalizes.
type InvoiceFinalizeRecorder interface {
	EmitFinalizeAuditTx(ctx context.Context, tx *sql.Tx, inv domain.Invoice) error
	DispatchInvoiceFinalized(ctx context.Context, tenantID string, inv domain.Invoice)
}

// ProrationTaxResult is what ApplyTaxToLineItems returns: invoice-level tax
// totals plus per-line mutations to the supplied line-item slice. Duplicates
// billing.TaxApplication so subscription package doesn't import billing.
type ProrationTaxResult struct {
	// TaxFacts: the shared 13-field calculation bundle (domain/tax_facts.go).
	// This mirror's flat fields produced THREE documented field-drop bugs
	// (provider provenance never committed; the reverse-charge/exemption
	// legend vanishing; the deferral facts missing so deferred proration
	// invoices were never auto-retried) — the embedded shared type makes a
	// fourth structurally impossible: a new tax fact added to TaxFacts is
	// carried here with zero edits.
	domain.TaxFacts
	SubtotalCents int64
	DiscountCents int64
}

// ProrationTaxApplier resolves and applies tax against a proration invoice's
// single line item, and commits the resulting provider calculation into a
// reportable tax transaction — the same calculate-then-commit contract the
// engine runs for cycle/create invoices, so proration tax is reported and
// remitted, not just charged.
type ProrationTaxApplier interface {
	ApplyTaxToLineItems(ctx context.Context, tenantID, customerID, currency string, subtotal, discount int64, lineItems []domain.InvoiceLineItem) (ProrationTaxResult, error)
	// CommitTax turns a provider tax calculation into a committed tax
	// transaction (Stripe Tax). No-op for manual/none providers. Called
	// after the proration invoice row is durable.
	CommitTax(ctx context.Context, tenantID, invoiceID, calculationID string) error
}

// CreditNoteIssuer issues the tax-reversing adjustment credit notes that
// credit unused prepaid time on a downgrade, quantity decrease or item
// removal (ADR-048, ADR-118). On a paid invoice the note credits the GROSS the
// customer paid for the unused slice (net + the proportional tax) to their
// balance AND reverses the proportional output tax; on an unpaid one it
// reduces what the invoice asks for. *creditnote.Service satisfies it
// directly (same method as billing.CreditNoteAdjuster).
//
// Required for a credit: production always wires it (SetCreditNoteIssuer).
// Unwired, a change that owes a credit fails rather than dropping it.
type CreditNoteIssuer interface {
	CreateAndIssueAdjustment(ctx context.Context, tenantID, invoiceID string, grossCents int64, reason, description string) (domain.CreditNote, error)
	// CreateAdjustmentDraftTx creates the clawback adjustment credit note as a
	// DRAFT inside the caller's tx (coordinator-owned, ADR-056) so it commits
	// atomically with the item change; the caller issues it post-commit via
	// Issue, and RetryPendingClawbackIssue re-issues it if that fails.
	CreateAdjustmentDraftTx(ctx context.Context, tx *sql.Tx, tenantID, invoiceID string, grossCents int64, reason, description string) (domain.CreditNote, error)
	// LockInvoicesForDraftsTx takes every listed invoice's credit-note lock
	// before the first draft, so drafts on several invoices in one tx cannot
	// deadlock with a concurrent credit note on one of them.
	LockInvoicesForDraftsTx(ctx context.Context, tx *sql.Tx, tenantID string, invoiceIDs []string) error
	// Issue finalizes a draft credit note (files the Stripe Tax reversal +
	// applies the customer-balance credit). Idempotent — safe to retry.
	Issue(ctx context.Context, tenantID, id string) (domain.CreditNote, error)
	// CreditedCents reports how much of an invoice has already been credited
	// (sum of non-voided credit notes), so the clawback fan-out caps each
	// funding invoice's piece at its remaining creditable headroom.
	CreditedCents(ctx context.Context, tenantID, invoiceID string) (int64, error)
}

// TenantLocator resolves the tenant's billing timezone so the proration
// denominator (fullBillingCycleDays) advances the cycle in the same zone the
// engine computes period boundaries in (ADR-058). *billing.Engine satisfies it
// (TenantLocation). Optional: when unwired (narrow tests) the day-math falls
// back to UTC — correct for UTC tenants; the only deployments that diverge are
// offset-TZ tenants, which production always wires. A wired locator's read
// failure is an error, never UTC (P24).
type TenantLocator interface {
	TenantLocation(ctx context.Context, tenantID string) (*time.Location, error)
}

// NetTermsReader resolves the tenant's configured Net payment terms so the
// proration invoice stamps the same terms + due date the engine's
// cycle/create invoices do, including 0 ("Due on receipt"). *billing.Engine
// satisfies it. Optional: when unwired (narrow tests) the proration path uses
// Net 30. A wired reader's failure fails the proration, never a Net 30 guess.
type NetTermsReader interface {
	NetPaymentTermDays(ctx context.Context, tenantID string) (int, error)
}

// auditRecorder is the narrow audit interface the subscription handler uses:
// Log to write operator-action rows and Query to read them back for the
// activity timeline. *audit.Logger satisfies it. Declared as an interface (vs
// the concrete logger) so the handler's audit metadata — including the
// ADR-030 sim-time context on clock-pinned actions — is unit-testable with a
// capturing fake.
type auditRecorder interface {
	Log(ctx context.Context, tenantID, action, resourceType, resourceID, resourceLabel string, metadata map[string]any) error
	Query(ctx context.Context, tenantID string, filter audit.QueryFilter) ([]domain.AuditEntry, int, error)
}

type Handler struct {
	svc         *Service
	plans       PlanReader
	invoices    ProrationInvoiceCreator
	creditNotes CreditNoteIssuer
	tzLocator   TenantLocator
	netTerms    NetTermsReader
	tax         ProrationTaxApplier
	events      domain.EventDispatcher
	auditLogger auditRecorder
	// finalizeAudit writes the finalize audit row and sends invoice.finalized
	// for a proration invoice born finalized. Its name holds "audit" so
	// audit.MustWired refuses to boot without it; nil (narrow tests) writes
	// neither.
	finalizeAudit InvoiceFinalizeRecorder
	// Resolver binds effective-now from the sub pin at handler entry
	// for proration math + changeAt stamping (PR-12, ADR-030 follow-
	// through). Without it, mid-cycle plan changes on clock-pinned
	// subs computed proration against wall-clock now — wrong factor
	// for the simulated state. Optional: nil-safe; without it the
	// proration paths fall back to wall-clock (pre-PR-12 behavior).
	resolver clock.Resolver
	// db enables the atomic AddItem-with-proration flow — the handler
	// opens an outer tx that wraps the sub-item insert + the proration
	// invoice/credit insert, so a failure on either side rolls back
	// both. Optional: nil-safe; when unwired (tests, narrow setups)
	// addItem falls back to the legacy non-atomic flow with explicit
	// orphan-item warning in the failure path. ADR-030 atomic-proration
	// follow-through (2026-05-29).
	db *postgres.DB
	// invoiceLabels names the invoices activity rows link to (ADR-128).
	// Optional: unset, rows name no invoice by link.
	invoiceLabels InvoiceLabeler
}

func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// SetAuditLogger configures audit logging for financial operations.
func (h *Handler) SetAuditLogger(l auditRecorder) { h.auditLogger = l }

// SetInvoiceFinalizeRecorder wires the finalize audit row and invoice.finalized
// webhook for proration invoices (the billing engine; router.go).
func (h *Handler) SetInvoiceFinalizeRecorder(r InvoiceFinalizeRecorder) { h.finalizeAudit = r }

// SetResolver wires the clock.Resolver used to bind effective-now at
// proration entry points so wall-clock-time computations on
// clock-pinned subs use simulated time. Implemented by *billing.Engine
// (same resolver the Service uses internally).
func (h *Handler) SetResolver(r clock.Resolver) { h.resolver = r }

// SetDB wires the database handle so the handler can open the outer
// transaction wrapping the atomic AddItem-with-proration flow.
// Without this, addItem falls back to its legacy non-atomic flow
// (item insert + proration insert in separate txs — a proration
// failure leaves an orphan item that requires manual reconciliation).
// Production wires this from router.go; tests can leave it nil.
func (h *Handler) SetDB(db *postgres.DB) { h.db = db }

// bindForSub returns ctx with effective-now bound from the sub pin
// when the resolver is wired. Used at proration handler entries so
// remainingPeriodRatio / changeAt stamps land in simulated time on
// clock-pinned subs. Falls through unchanged when resolver isn't set
// or the sub isn't clock-pinned (resolver returns wall-clock).
func (h *Handler) bindForSub(ctx context.Context, tenantID, subID string) context.Context {
	if h.resolver == nil {
		return ctx
	}
	bound, _ := clock.BindEffectiveNow(ctx, h.resolver, clock.Pin{TenantID: tenantID, SubscriptionID: subID})
	return bound
}

// auditCtx binds the sub's simulated-time context — its test clock and that
// clock's frozen instant — onto ctx for an audit emission, so the row lands
// on the sim axis (audit_log.sim_effective_at / test_clock_id) and the clock
// filter can find it after ADR-086 teardown deletes the sub itself.
//
// It replaces auditMetaForSub, which merged the same pair into the metadata
// bag from TWO sources: the clock id off the entity, the instant off
// sub.UpdatedAt. That pairing is only true when this request is what last
// wrote the row — on subscription.proration_failed (nothing written) and on
// the change-plan rows (sub rewritten later) it stamped a STALE sim instant.
// One resolution of the pin yields both halves, consistent by construction
// (clock.Sim).
//
// Cost is zero on the path that matters: live mode can hold no clock (DB
// CHECK on test_clocks.livemode), so production never resolves. It is also a
// no-op when ctx is already bound — the service-layer entry points
// (bindForSub) bind before we get here on most routes.
//
// ADR-030 still holds: created_at stays WALL-clock ("when the operator
// clicked"). This is the second axis, not a replacement for the first.
func (h *Handler) auditCtx(ctx context.Context, tenantID, subID string) context.Context {
	if h.resolver == nil || subID == "" || postgres.Livemode(ctx) {
		return ctx
	}
	if _, ok := clock.SimOf(ctx); ok {
		return ctx
	}
	bound, _ := clock.BindEffectiveNow(ctx, h.resolver, clock.Pin{TenantID: tenantID, SubscriptionID: subID})
	return bound
}

// planIDsForAudit projects a sub's items into the audit metadata
// shape — same as the cancel handler's existing payload, so audit
// rows from create/cancel/etc carry a consistent plan-ids array.
func planIDsForAudit(sub domain.Subscription) []string {
	ids := make([]string, 0, len(sub.Items))
	for _, it := range sub.Items {
		ids = append(ids, it.PlanID)
	}
	return ids
}

// SetEventDispatcher wires the outbound webhook dispatcher. When nil the
// handler still functions — events just aren't emitted, which is only the
// right behavior in narrow unit tests.
func (h *Handler) SetEventDispatcher(d domain.EventDispatcher) { h.events = d }

// SetProrationDeps sets optional dependencies for proration: the plan reader
// and the invoice side. A proration credit is a credit note
// (SetCreditNoteIssuer).
func (h *Handler) SetProrationDeps(plans PlanReader, invoices ProrationInvoiceCreator) {
	h.plans = plans
	h.invoices = invoices
}

// SetProrationTaxApplier configures tax resolution on proration invoices.
func (h *Handler) SetProrationTaxApplier(a ProrationTaxApplier) {
	h.tax = a
}

// SetTenantLocator wires the tenant-billing-timezone resolver (the billing
// engine) so proration day-math anchors its denominator in the tenant zone
// (ADR-058). When unset, fullBillingCycleDays falls back to UTC.
func (h *Handler) SetTenantLocator(l TenantLocator) { h.tzLocator = l }

// SetNetTermsReader wires the tenant Net-terms resolver (the billing engine)
// so proration invoices stamp the operator-configured terms instead of a
// hardcoded Net 30; wired from router.go.
func (h *Handler) SetNetTermsReader(r NetTermsReader) { h.netTerms = r }

// tenantLoc resolves the tenant billing timezone, UTC when unwired.
func (h *Handler) tenantLoc(ctx context.Context, tenantID string) (*time.Location, error) {
	if h.tzLocator == nil {
		return time.UTC, nil
	}
	return h.tzLocator.TenantLocation(ctx, tenantID)
}

// stampPeriodDisplay populates the computed CurrentBillingPeriodDisplay — the
// inclusive human period range ("Jun 1 – Jun 30"), anchored in the org's
// billing timezone (tenantLoc, ADR-077) so the displayed range matches the
// invoice and renders in the one timezone the org bills in. No-op when the sub
// has no current period. Called for every subscription response via respondSub.
//
// A timezone read failure leaves the field empty rather than rendering the
// range in the wrong zone; the response still goes out, since this runs after
// the handler's write has committed.
func (h *Handler) stampPeriodDisplay(ctx context.Context, sub *domain.Subscription) {
	if sub.CurrentBillingPeriodStart != nil && sub.CurrentBillingPeriodEnd != nil {
		loc, err := h.tenantLoc(ctx, sub.TenantID)
		if err != nil {
			slog.WarnContext(ctx, "subscription period display omitted: tenant timezone unreadable",
				"subscription_id", sub.ID, "error", err)
			return
		}
		sub.CurrentBillingPeriodDisplay = domain.FormatInclusivePeriod(
			*sub.CurrentBillingPeriodStart, *sub.CurrentBillingPeriodEnd, loc)
	}
}

// respondSub writes a single subscription as JSON, first stamping the computed
// period-display field. Every handler that returns a subscription goes through
// here so the display can't be forgotten on a new path.
func (h *Handler) respondSub(w http.ResponseWriter, r *http.Request, status int, sub domain.Subscription) {
	h.stampPeriodDisplay(r.Context(), &sub)
	respond.JSON(w, r, status, sub)
}

// SetCreditNoteIssuer wires the tax-reversing credit-note primitive that
// credits unused prepaid time (ADR-048, ADR-118). Implemented by
// *creditnote.Service; wired from router.go.
func (h *Handler) SetCreditNoteIssuer(i CreditNoteIssuer) {
	h.creditNotes = i
}

// fireEvent dispatches a subscription lifecycle event. Synchronous by design:
// with the webhook_outbox in place (RES-1), Dispatch is a short DB insert that
// must persist-before-return so a crash between the handler's respond.JSON and
// event emission can't silently lose the event. Logging an error beats
// dropping.
//
// The event carries sub (ADR-131), which the caller read after the change,
// with its items. sub must be that read: an event built from a guess would
// tell receivers something the API does not show.
func (h *Handler) fireEvent(ctx context.Context, tenantID, eventType string, sub domain.Subscription, extra map[string]any) {
	if h.events == nil {
		return
	}
	if err := h.events.Dispatch(ctx, tenantID, eventType, domain.SubscriptionWebhookData(sub, extra)); err != nil {
		slog.ErrorContext(ctx, "dispatch subscription event",
			"event_type", eventType,
			"subscription_id", sub.ID,
			"tenant_id", tenantID,
			"error", err,
		)
	}
}

// fireItemEvent sends an item event: the subscription read after the change,
// and item_id naming the item (gone from items on a removal). When the read
// fails there is no true resource to send, so the event is left out, loudly.
func (h *Handler) fireItemEvent(ctx context.Context, tenantID, eventType, subID, itemID string) {
	sub, err := h.svc.Get(ctx, tenantID, subID)
	if err != nil {
		slog.ErrorContext(ctx, "subscription item event not sent: the subscription could not be read after the change",
			"event_type", eventType, "subscription_id", subID, "item_id", itemID, "error", err)
		return
	}
	h.fireEvent(ctx, tenantID, eventType, sub, map[string]any{"item_id": itemID})
}

func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Post("/", h.create)
	r.Get("/", h.list)
	r.Get("/{id}", h.get)
	r.Post("/{id}/activate", h.activate)
	r.Post("/{id}/cancel", h.cancel)
	r.Post("/{id}/schedule-cancel", h.scheduleCancel)
	r.Delete("/{id}/scheduled-cancel", h.clearScheduledCancel)
	r.Put("/{id}/pause-collection", h.pauseCollection)
	r.Delete("/{id}/pause-collection", h.resumeCollection)
	r.Post("/{id}/end-trial", h.endTrial)
	r.Post("/{id}/extend-trial", h.extendTrial)
	r.Post("/{id}/retry-billing", h.retryBilling)

	// Billing thresholds — early-invoice (billing threshold) config. PUT writes the full
	// (amount, reset, items) triple; DELETE clears it. Idempotent.
	r.Put("/{id}/billing-thresholds", h.setBillingThresholds)
	r.Delete("/{id}/billing-thresholds", h.clearBillingThresholds)

	// Items — Stripe-style per-item mutation. Quantity and plan changes land
	// on the same PATCH (body discriminates), pending-change clear has its own
	// DELETE so client code can target it without a PATCH body shape.
	r.Post("/{id}/items", h.addItem)
	r.Patch("/{id}/items/{itemID}", h.updateItem)
	r.Delete("/{id}/items/{itemID}/pending-change", h.cancelPendingItemChange)
	r.Delete("/{id}/items/{itemID}", h.removeItem)
	r.Get("/{id}/timeline", h.activityTimeline)
	return r
}

// createBody is CreateInput as the API reads it. Its status hides the
// embedded one so the handler can tell a field left out (start now) from one
// sent empty or null (refused).
type createBody struct {
	CreateInput
	Status sentStatus `json:"status"`
}

// sentStatus records whether "status" was in the body at all. encoding/json
// calls UnmarshalJSON for a JSON null too, so null counts as sent.
type sentStatus struct {
	value domain.SubscriptionStatus
	sent  bool
}

func (s *sentStatus) UnmarshalJSON(b []byte) error {
	s.sent = true
	if string(b) == "null" {
		return nil
	}
	return json.Unmarshal(b, &s.value)
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())

	var body createBody
	if !respond.DecodeStrict(w, r, &body) {
		return
	}
	// "status": "" and "status": null are not "left out": a template whose
	// value was unset must not start and bill the subscription.
	if body.Status.sent && body.Status.value == "" {
		respond.FromError(w, r, errs.Invalid("status", createStatusMessage), "subscription")
		return
	}
	input := body.CreateInput
	input.Status = body.Status.value

	sub, err := h.svc.Create(r.Context(), tenantID, input)
	if err != nil {
		respond.FromError(w, r, err, "subscription")
		return
	}

	// Explicit audit row — created_at is wall-clock for forensics
	// (ADR-030 line 131, post-2026-05-28 amendment). For clock-pinned
	// subs, h.auditCtx binds the clock so the writer stamps the sim axis
	// (sim_effective_at + test_clock_id columns, mirrored into metadata for
	// the UI subline).
	if h.auditLogger != nil {
		_ = h.auditLogger.Log(h.auditCtx(r.Context(), tenantID, sub.ID), tenantID, domain.AuditActionCreate, "subscription", sub.ID, sub.Code, map[string]any{
			"customer_id": sub.CustomerID,
			"plan_ids":    planIDsForAudit(sub),
		})
	}

	// subscription.created is enqueued IN the create tx (store-level
	// DispatchTx subset, 2026-07-05) — do not also fire it here.

	h.respondSub(w, r, http.StatusCreated, sub)
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())

	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))

	// ?billing=failing lists the subscriptions whose billing is failing
	// (ADR-119). It is a closed set: an unknown value is a 400, never an
	// unfiltered list that looks like "everything is failing".
	billing := r.URL.Query().Get("billing")
	if billing != "" && billing != ListBillingFailing {
		respond.BadRequest(w, r, `billing must be "failing" when set`)
		return
	}
	// ?collection=paused lists live subscriptions whose collection is
	// paused. Same closed-set rule as ?billing=.
	collection := r.URL.Query().Get("collection")
	if collection != "" && collection != ListCollectionPaused {
		respond.BadRequest(w, r, `collection must be "paused" when set`)
		return
	}
	// ?status= is a closed set too: an unknown value matched no row and
	// read as "none of those". The Paused tab sent status=paused and was
	// always empty (sweep E6).
	status := r.URL.Query().Get("status")
	switch domain.SubscriptionStatus(status) {
	case "", domain.SubscriptionDraft, domain.SubscriptionTrialing, domain.SubscriptionActive,
		domain.SubscriptionCanceled:
	default:
		msg := "status must be one of draft, trialing, active, canceled"
		if status == "paused" {
			msg += `; paused collection is not a status, use collection=paused`
		}
		respond.BadRequest(w, r, msg)
		return
	}

	subs, total, err := h.svc.List(r.Context(), ListFilter{
		TenantID:   tenantID,
		CustomerID: r.URL.Query().Get("customer_id"),
		PlanID:     r.URL.Query().Get("plan_id"),
		Status:     status,
		Billing:    billing,
		Collection: collection,
		Search:     strings.TrimSpace(r.URL.Query().Get("search")),
		Limit:      limit,
		Offset:     offset,
		Sort:       r.URL.Query().Get("sort"),
		SortDir:    r.URL.Query().Get("dir"),
	})
	if err != nil {
		respond.InternalError(w, r)
		slog.ErrorContext(r.Context(), "list subscriptions", "error", err)
		return
	}
	if subs == nil {
		subs = []domain.Subscription{}
	}
	for i := range subs {
		h.stampPeriodDisplay(r.Context(), &subs[i])
	}

	respond.List(w, r, subs, total)
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	id := chi.URLParam(r, "id")

	sub, err := h.svc.Get(r.Context(), tenantID, id)
	if errors.Is(err, errs.ErrNotFound) {
		respond.NotFound(w, r, "subscription")
		return
	}
	if err != nil {
		respond.InternalError(w, r)
		slog.ErrorContext(r.Context(), "get subscription", "error", err)
		return
	}

	h.respondSub(w, r, http.StatusOK, sub)
}

func (h *Handler) activate(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	id := chi.URLParam(r, "id")

	sub, err := h.svc.Activate(r.Context(), tenantID, id)
	if err != nil {
		respond.FromError(w, r, err, "subscription")
		return
	}

	// Wall-clock created_at + sim metadata on clock-pinned subs — same
	// pattern as the create handler above.
	if h.auditLogger != nil {
		_ = h.auditLogger.Log(h.auditCtx(r.Context(), tenantID, sub.ID), tenantID, domain.AuditActionActivate, "subscription", sub.ID, sub.Code, map[string]any{
			"customer_id": sub.CustomerID,
		})
	}

	// subscription.activated is enqueued IN the activation tx (store-level
	// DispatchTx subset) — do not also fire it here.

	h.respondSub(w, r, http.StatusOK, sub)
}

func (h *Handler) cancel(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	id := chi.URLParam(r, "id")

	sub, prorationCreditCents, err := h.svc.Cancel(r.Context(), tenantID, id)
	if err != nil {
		respond.FromError(w, r, err, "subscription")
		return
	}

	// The cancel audit row rides the cancel transaction itself (ADR-090,
	// Service.Cancel emission), with the proration credit in its metadata —
	// dunning-driven cancels record identically instead of being invisible.
	_ = prorationCreditCents

	// subscription.canceled is enqueued IN the cancel tx (store-level
	// DispatchTx subset) — do not also fire it here.

	h.respondSub(w, r, http.StatusOK, sub)
}

// scheduleCancel records a soft-cancel intent. Body must set exactly one of
// {at_period_end:true, cancel_at:<RFC3339>}. On an ACTIVE sub the current
// period is unaffected and the billing engine flips the sub to canceled at
// the boundary. On a TRIALING sub, at_period_end means cancel FREE at trial
// end (Stripe semantics, ADR-069) and cancel_at accepts exactly trial_end_at
// or >= current_billing_period_end — the response's derived
// cancel_effective_at states when the cancel actually lands. Re-calling
// replaces any prior schedule (so a caller can switch modes with a new
// request); a 409 means the sub's status changed mid-flight — re-read.
func (h *Handler) scheduleCancel(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	id := chi.URLParam(r, "id")

	var input ScheduleCancelInput
	if !respond.DecodeStrict(w, r, &input) {
		return
	}

	sub, err := h.svc.ScheduleCancel(r.Context(), tenantID, id, input)
	if err != nil {
		respond.FromError(w, r, err, "subscription")
		return
	}

	if h.auditLogger != nil {
		meta := map[string]any{
			"action":               domain.SubAuditCancelScheduled,
			"customer_id":          sub.CustomerID,
			"cancel_at_period_end": sub.CancelAtPeriodEnd,
		}
		if sub.CancelAt != nil {
			meta["cancel_at"] = sub.CancelAt.UTC()
		}
		// When it lands, and the status it was scheduled in: at period end
		// on a trialing subscription means at trial end (ADR-069), and the
		// activity line says which.
		if sub.CancelEffectiveAt != nil {
			meta["cancel_effective_at"] = sub.CancelEffectiveAt.UTC()
		}
		meta["status"] = string(sub.Status)
		_ = h.auditLogger.Log(h.auditCtx(r.Context(), tenantID, sub.ID), tenantID, domain.AuditActionUpdate, "subscription", sub.ID, sub.Code, meta)
	}

	// The subscription carries cancel_at, cancel_at_period_end and
	// cancel_effective_at (ADR-069: when it actually cancels).
	h.fireEvent(r.Context(), tenantID, domain.EventSubscriptionCancelScheduled, sub, nil)

	h.respondSub(w, r, http.StatusOK, sub)
}

// pauseCollection sets the Stripe-parity collection-pause state. Distinct
// from POST /pause (which hard-pauses the subscription via status). Body
// must include behavior; v1 only accepts "keep_as_draft". resumes_at is
// optional — when set, the cycle scan auto-clears the pause at the start
// of that period.
func (h *Handler) pauseCollection(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	id := chi.URLParam(r, "id")

	var input PauseCollectionInput
	if !respond.DecodeStrict(w, r, &input) {
		return
	}

	sub, err := h.svc.PauseCollection(r.Context(), tenantID, id, input)
	if err != nil {
		respond.FromError(w, r, err, "subscription")
		return
	}

	// The subscription's pause_collection holds behavior and resumes_at.
	h.fireEvent(r.Context(), tenantID, domain.EventSubscriptionCollectionPaused, sub, nil)

	h.respondSub(w, r, http.StatusOK, sub)
}

// endTrial flips a 'trialing' subscription to 'active' immediately,
// regardless of trial_end_at. Operator-driven counterpart to the cycle
// scan's auto-flip. Fires subscription.trial_ended with
// triggered_by="operator" so analytics can distinguish from the
// scheduled transition. Returns 422 if the row is not in 'trialing'.
func (h *Handler) endTrial(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	id := chi.URLParam(r, "id")

	sub, err := h.svc.EndTrial(r.Context(), tenantID, id)
	if err != nil {
		respond.FromError(w, r, err, "subscription")
		return
	}

	if h.auditLogger != nil {
		_ = h.auditLogger.Log(h.auditCtx(r.Context(), tenantID, sub.ID), tenantID, domain.AuditActionUpdate, "subscription", sub.ID, sub.Code, map[string]any{
			"action":      domain.SubAuditTrialEnded,
			"customer_id": sub.CustomerID,
		})
	}

	// subscription.trial_ended (triggered_by=operator) is enqueued IN the
	// end-trial tx (store-level DispatchTx subset) — do not also fire it here.

	h.respondSub(w, r, http.StatusOK, sub)
}

// retryBilling is the operator's retry for a subscription whose billing is
// failing or parked (ADR-119). It clears the saved failure (with its audit row,
// in one transaction) and bills the subscription now.
//
// 200 carries the subscription, invoices_generated and still_due, and has
// three outcomes: billed (no billing_failure, still_due false); failed again
// and saved (billing_failure set); or the attempt did not complete and nothing
// was saved (no billing_failure, still_due true: unbilled, the next billing
// run attempts it). 409 billing_not_failing when there is nothing to retry,
// 409 invalid_state for a subscription on a test clock. The audit row is the
// service's; the handler emits none.
func (h *Handler) retryBilling(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	id := chi.URLParam(r, "id")

	res, err := h.svc.RetryBilling(r.Context(), tenantID, id)
	if err != nil {
		respond.FromError(w, r, err, "subscription")
		return
	}
	h.stampPeriodDisplay(r.Context(), &res.Subscription)
	respond.JSON(w, r, http.StatusOK, map[string]any{
		"subscription":       res.Subscription,
		"invoices_generated": res.InvoicesGenerated,
		"still_due":          res.StillDue,
	})
}

// ExtendTrialInput is the POST /v1/subscriptions/{id}/extend-trial body.
type ExtendTrialInput struct {
	TrialEnd time.Time `json:"trial_end"`
}

// extendTrial pushes a trialing subscription's trial_end_at later. Body:
// {trial_end:<RFC3339>}. Returns 422 if the new value is in the past or
// not strictly after the current trial_end_at, or the sub is not in
// 'trialing'. Fires subscription.trial_extended with
// triggered_by="operator".
func (h *Handler) extendTrial(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	id := chi.URLParam(r, "id")

	var body ExtendTrialInput
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		respond.BadRequest(w, r, "invalid JSON body")
		return
	}
	if body.TrialEnd.IsZero() {
		respond.ValidationField(w, r, "trial_end", "trial_end is required")
		return
	}

	sub, err := h.svc.ExtendTrial(r.Context(), tenantID, id, body.TrialEnd)
	if err != nil {
		respond.FromError(w, r, err, "subscription")
		return
	}

	if h.auditLogger != nil {
		_ = h.auditLogger.Log(h.auditCtx(r.Context(), tenantID, sub.ID), tenantID, domain.AuditActionUpdate, "subscription", sub.ID, sub.Code, map[string]any{
			"action":      domain.SubAuditTrialExtended,
			"customer_id": sub.CustomerID,
			"trial_end":   body.TrialEnd.UTC(),
		})
	}

	// The new trial end is the subscription's trial_end_at.
	h.fireEvent(r.Context(), tenantID, domain.EventSubscriptionTrialExtended, sub, map[string]any{
		"triggered_by": "operator",
	})

	h.respondSub(w, r, http.StatusOK, sub)
}

// resumeCollection clears the collection-pause state. Idempotent —
// clearing a row that has no active pause returns 200 with the unchanged
// subscription. Returns 404 only when the subscription itself doesn't
// exist.
func (h *Handler) resumeCollection(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	id := chi.URLParam(r, "id")

	sub, err := h.svc.ResumeCollection(r.Context(), tenantID, id)
	if err != nil {
		respond.FromError(w, r, err, "subscription")
		return
	}

	// Audit write now lives in Service.ResumeCollection so any future
	// non-handler caller also produces an entry. The service stamps
	// triggered_by="operator" because today's only caller is this
	// handler; engine's cycle-scan auto-resume writes its own audit row
	// with triggered_by="schedule" directly via Engine.SetAuditLogger.

	h.fireEvent(r.Context(), tenantID, domain.EventSubscriptionCollectionResumed, sub, map[string]any{
		"triggered_by": "operator",
	})

	h.respondSub(w, r, http.StatusOK, sub)
}

// setBillingThresholds writes the early-invoice (billing threshold) config onto a
// subscription. Body shape:
//
//	{
//	  "amount_gte": 50000,                    // optional, integer cents
//	  "item_thresholds": [                    // optional, always-array
//	    {"subscription_item_id": "si_xxx", "usage_gte": "1000"}
//	  ]
//	}
//
// At least one of amount_gte or item_thresholds must be supplied. Returns
// 422 on validation failure (terminal sub, unknown item id, negative
// usage_gte, multi-currency item set, an unknown field such as the removed
// reset_billing_cycle, etc). A fire always keeps the billing period.
//
// Replaces the full set on every call: the per-item rows for any item not
// in the new slice are deleted by the store.
func (h *Handler) setBillingThresholds(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	id := chi.URLParam(r, "id")

	var input BillingThresholdsInput
	if !respond.DecodeStrict(w, r, &input) {
		return
	}

	// Multi-currency check happens here because plan currency lookups need
	// h.plans, which the service doesn't have. We hydrate the sub's items,
	// fetch each item's plan, and reject when the set spans more than one
	// currency. A threshold expressed in cents only makes sense against a
	// single-currency line set. The one currency rides the audit row, so the
	// activity line can show the amount as money (ADR-128, B9).
	var thresholdCurrency string
	if h.plans != nil {
		// A read error fails the request: skipping it would let a cents
		// threshold onto a mixed-currency sub.
		sub, gerr := h.svc.Get(r.Context(), tenantID, id)
		if gerr != nil {
			respond.FromError(w, r, gerr, "subscription")
			return
		}
		if len(sub.Items) > 0 {
			seen := make(map[string]struct{}, 2)
			for _, it := range sub.Items {
				p, perr := h.plans.GetPlan(r.Context(), tenantID, it.PlanID)
				if perr != nil {
					respond.FromError(w, r, fmt.Errorf("get plan %s: %w", it.PlanID, perr), "plan")
					return
				}
				if p.Currency != "" {
					seen[p.Currency] = struct{}{}
				}
			}
			if len(seen) > 1 {
				respond.ValidationField(w, r, "billing_thresholds",
					"billing thresholds are not supported on multi-currency subscriptions")
				return
			}
			for c := range seen {
				thresholdCurrency = c
			}
		}
	}

	sub, err := h.svc.SetBillingThresholds(r.Context(), tenantID, id, input)
	if err != nil {
		respond.FromError(w, r, err, "subscription")
		return
	}

	if h.auditLogger != nil {
		meta := map[string]any{
			"action":               domain.SubAuditThresholdsSet,
			"customer_id":          sub.CustomerID,
			"amount_gte":           input.AmountGTE,
			"item_threshold_count": len(input.ItemThresholds),
		}
		if thresholdCurrency != "" {
			meta["currency"] = thresholdCurrency
		}
		_ = h.auditLogger.Log(h.auditCtx(r.Context(), tenantID, sub.ID), tenantID, domain.AuditActionUpdate, "subscription", sub.ID, sub.Code, meta)
	}

	h.respondSub(w, r, http.StatusOK, sub)
}

// clearBillingThresholds removes any threshold configuration on a
// subscription. Idempotent — clearing on a sub that has no threshold returns
// 200 with the unchanged subscription. Returns 404 only when the
// subscription itself doesn't exist.
func (h *Handler) clearBillingThresholds(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	id := chi.URLParam(r, "id")

	sub, err := h.svc.ClearBillingThresholds(r.Context(), tenantID, id)
	if err != nil {
		respond.FromError(w, r, err, "subscription")
		return
	}

	if h.auditLogger != nil {
		_ = h.auditLogger.Log(h.auditCtx(r.Context(), tenantID, sub.ID), tenantID, domain.AuditActionUpdate, "subscription", sub.ID, sub.Code, map[string]any{
			"action":      domain.SubAuditThresholdsCleared,
			"customer_id": sub.CustomerID,
		})
	}

	h.respondSub(w, r, http.StatusOK, sub)
}

// clearScheduledCancel removes any prior schedule. Idempotent — clearing a
// row that has no pending cancel returns 200 with the unchanged subscription.
// Returns 404 only when the subscription itself doesn't exist.
func (h *Handler) clearScheduledCancel(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	id := chi.URLParam(r, "id")

	sub, err := h.svc.ClearScheduledCancel(r.Context(), tenantID, id)
	if err != nil {
		respond.FromError(w, r, err, "subscription")
		return
	}

	if h.auditLogger != nil {
		_ = h.auditLogger.Log(h.auditCtx(r.Context(), tenantID, sub.ID), tenantID, domain.AuditActionUpdate, "subscription", sub.ID, sub.Code, map[string]any{
			"action":      domain.SubAuditCancelCleared,
			"customer_id": sub.CustomerID,
		})
	}

	h.fireEvent(r.Context(), tenantID, domain.EventSubscriptionCancelCleared, sub, nil)

	h.respondSub(w, r, http.StatusOK, sub)
}

// addItem appends a new priced line to a subscription. When the parent
// subscription is mid-period, the new item drives a proration invoice so the
// customer is charged for the partial-period cost of the addition rather than
// getting it free until next cycle close.
func (h *Handler) addItem(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	id := chi.URLParam(r, "id")

	var input AddItemInput
	if !respond.DecodeStrict(w, r, &input) {
		return
	}

	// Bind effective-now from the sub pin so proration math runs in
	// simulated time on clock-pinned subs (ADR-030). Without binding,
	// `remainingPeriodRatio(sub, time.Now())` would compute the
	// wrong ratio for a clock-pinned sub whose current_period is in
	// the simulated future. PR-12.
	ctx := h.bindForSub(r.Context(), tenantID, id)
	r = r.WithContext(ctx)

	// Close an ended, unbilled period first, so the snapshot below is the
	// period that is running (Service.CloseEndedPeriodFirst).
	if _, err := h.svc.CloseEndedPeriodFirst(ctx, tenantID, id); err != nil {
		respond.FromError(w, r, err, "subscription")
		return
	}

	// Read the subscription to decide whether the add prorates. On the
	// atomic path this read only routes the request: the add is priced from
	// the subscription AddItemTx reads under the subscription row lock, so an
	// add that waited on a period close or a plan swap is priced on the
	// period it lands in (#1270). Only the legacy path with no database
	// wired prices from this read.
	var subBefore domain.Subscription
	var prorationRemainingDays, prorationTotalDays int64
	if h.plans != nil && h.invoices != nil {
		// A read error fails the add: carrying on would add the item with
		// proration silently off (no in-advance charge for the period).
		sub, serr := h.svc.Get(ctx, tenantID, id)
		if serr != nil {
			respond.FromError(w, r, serr, "subscription")
			return
		}
		subBefore = sub
		var derr error
		prorationRemainingDays, prorationTotalDays, derr = h.addProrationDays(ctx, tenantID, sub, clock.Now(ctx))
		if derr != nil {
			slog.ErrorContext(ctx, "add item refused: tenant timezone unreadable for proration",
				"subscription_id", id, "error", derr)
			respond.InternalError(w, r)
			return
		}
	}

	// Atomic AddItem-with-proration path (ADR-030 atomic-proration
	// follow-through, 2026-05-29): when proration emission is needed
	// AND the db handle is wired, open one outer tx wrapping the item
	// insert + the proration write so a failure on either side rolls
	// back both. Falls back to the legacy non-atomic path when db
	// isn't wired (test scaffolding) or when no proration emission is
	// needed (zero remaining / no invoices wired). ADR-042 (2026-05-31)
	// converted prorationFactor float64 → integer day-ratio for
	// industry-grade money math precision.
	wantProration := prorationRemainingDays > 0 && h.invoices != nil
	atomic := h.db != nil && wantProration

	var item domain.SubscriptionItem
	var addErr error
	var addProration *ProrationDetail
	if atomic {
		item, addProration, addErr = h.atomicAddItemWithProration(ctx, tenantID, id, input)
	} else {
		item, addErr = h.svc.AddItem(ctx, tenantID, id, input)
	}
	if addErr != nil {
		respond.FromError(w, r, addErr, "subscription")
		return
	}

	if h.auditLogger != nil {
		payload := map[string]any{
			"action":   domain.SubAuditItemAdded,
			"item_id":  item.ID,
			"plan_id":  item.PlanID,
			"quantity": item.Quantity,
		}
		addProrationMeta(payload, addProration, h.planCurrency(ctx, tenantID, item.PlanID))
		_ = h.auditLogger.Log(h.auditCtx(r.Context(), tenantID, id), tenantID, domain.AuditActionUpdate, "subscription", id, subBefore.Code, payload)
	}

	// Re-fetch the subscription so downstream payload/proration paths see the
	// full post-add Items slice. If the read fails the proration path falls
	// back to the snapshot from before the add; item.added is not sent.
	var subAfter domain.Subscription
	subAfterRead := false
	if h.events != nil || (wantProration && !atomic) {
		s, getErr := h.svc.Get(r.Context(), tenantID, id)
		if getErr != nil {
			subAfter = subBefore
			if subAfter.ID == "" {
				subAfter = domain.Subscription{ID: id}
			}
		} else {
			subAfter, subAfterRead = s, true
		}
	}

	// Legacy non-atomic proration path — only when atomic path wasn't
	// taken (h.db unwired). Keeps the prior behavior intact for tests
	// and minimal setups so this PR doesn't force every consumer to
	// thread a db handle through.
	if wantProration && !atomic {
		changeAt := item.CreatedAt
		if changeAt.IsZero() {
			changeAt = clock.Now(ctx)
		}
		spec := itemProrationSpec{
			changeType:    domain.ItemChangeTypeAdd,
			changeAt:      changeAt,
			remainingDays: prorationRemainingDays,
			totalDays:     prorationTotalDays,
			itemID:        item.ID,
			oldPlanID:     "",
			oldQuantity:   0,
			newPlanID:     item.PlanID,
			newQuantity:   item.Quantity,
		}
		prorationResult, prorationErr := h.handleItemProration(r.Context(), tenantID, subAfter, spec, nil)
		if prorationErr != nil {
			// Deliberate ADR-050 block (add charges against an unpaid source) →
			// clean 409, not the internal-failure 500 below.
			if errors.Is(prorationErr, errs.ErrInvalidState) {
				respond.FromError(w, r, prorationErr, "subscription item")
				return
			}
			slog.ErrorContext(r.Context(), "item proration failed after item add committed",
				"subscription_id", id,
				"item_id", item.ID,
				"tenant_id", tenantID,
				"plan_id", item.PlanID,
				"quantity", item.Quantity,
				"proration_remaining_days", prorationRemainingDays,
				"proration_total_days", prorationTotalDays,
				"error", prorationErr,
			)
			if h.auditLogger != nil {
				_ = h.auditLogger.Log(h.auditCtx(r.Context(), tenantID, id), tenantID, domain.AuditActionSubscriptionProrationFailed, "subscription", id, "", map[string]any{
					"item_id":                  item.ID,
					"change_type":              string(domain.ItemChangeTypeAdd),
					"plan_id":                  item.PlanID,
					"quantity":                 item.Quantity,
					"proration_remaining_days": prorationRemainingDays,
					"proration_total_days":     prorationTotalDays,
					"error":                    prorationErr.Error(),
				})
			}
			respond.Error(w, r, http.StatusInternalServerError, "api_error", "proration_failed",
				"item add succeeded but the prorated charge could not be created — the item is on the subscription and no automatic retry will run (re-sending this request is rejected as a duplicate); issue the prorated amount manually via a one-off invoice, or contact support")
			return
		}
		_ = prorationResult
	}

	// The re-read above is the subscription with the new item. When it
	// failed there is no true resource to send, so the event is left out,
	// loudly.
	if h.events != nil {
		if subAfterRead {
			h.fireEvent(r.Context(), tenantID, domain.EventSubscriptionItemAdded, subAfter, map[string]any{
				"item_id": item.ID,
			})
		} else {
			slog.ErrorContext(r.Context(), "subscription.item.added not sent: the subscription could not be read after the add",
				"subscription_id", id, "item_id", item.ID)
		}
	}

	respond.JSON(w, r, http.StatusCreated, item)
}

// updateItem applies a quantity change or plan change (immediate/scheduled)
// to a single item. Quantity-only edits return the updated item directly;
// immediate plan changes also drive proration (new invoice or credit) keyed
// on the item.
func (h *Handler) updateItem(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	subID := chi.URLParam(r, "id")
	itemID := chi.URLParam(r, "itemID")

	var input UpdateItemInput
	if !respond.DecodeStrict(w, r, &input) {
		return
	}

	// Bind effective-now from the sub pin so proration math + changeAt
	// stamps run in simulated time on clock-pinned subs (PR-12).
	ctx := h.bindForSub(r.Context(), tenantID, subID)
	r = r.WithContext(ctx)

	// Close an ended, unbilled period first, so the change (and its
	// proration snapshot below, and a scheduled change's effective date) is
	// planned from the period that is running (Service.CloseEndedPeriodFirst).
	if _, err := h.svc.CloseEndedPeriodFirst(ctx, tenantID, subID); err != nil {
		respond.FromError(w, r, err, "subscription")
		return
	}

	// Read the item and subscription only when the change may prorate. On the
	// atomic path this read only routes the request (does it prorate, is it a
	// cross-interval swap); the proration is priced from the state
	// UpdateItemTx reads under the subscription row lock (#1270). Only the
	// legacy path with no database wired prices from this read.
	var oldPlanID string
	var oldQuantity int64
	var prorationRemainingDays, prorationTotalDays int64
	var subBefore domain.Subscription
	isImmediatePlanChange := input.NewPlanID != "" && input.Immediate
	isQuantityChange := input.Quantity != nil
	prorationEligible := (isImmediatePlanChange || isQuantityChange) && h.plans != nil
	if prorationEligible {
		// Read errors fail the change: carrying on would apply it with
		// proration silently off (no charge or credit for the period).
		item, gerr := h.svc.store.GetItem(ctx, tenantID, itemID)
		if gerr != nil {
			respond.FromError(w, r, gerr, "subscription item")
			return
		}
		if item.SubscriptionID == subID {
			oldPlanID = item.PlanID
			oldQuantity = item.Quantity
		}
		sub, serr := h.svc.Get(ctx, tenantID, subID)
		if serr != nil {
			respond.FromError(w, r, serr, "subscription")
			return
		}
		subBefore = sub
		prorationRemainingDays, prorationTotalDays = remainingPeriodRatio(sub, clock.Now(ctx))
	}

	// Atomic UpdateItem-with-proration path (ADR-030 atomic-proration
	// follow-through). Tries the atomic flow first when conditions are
	// met (db wired, proration eligible, scheduled / cross-interval /
	// cross-axis-orchestrator NOT in play). svc.UpdateItemTx returns
	// errAtomicNotApplicable for the complex paths (cross-interval
	// swap, scheduled change); on that signal we fall back to the
	// legacy non-atomic flow which handles those cases via the
	// existing multi-write engine orchestration. Quantity changes +
	// same-interval immediate plan changes are now fully atomic with
	// their proration writes.
	wantProration := prorationEligible && prorationRemainingDays > 0 && h.invoices != nil

	// A cross-interval immediate plan swap MUST take the atomic path even at the
	// period boundary (remainingDays==0, where wantProration is false): the swap
	// restructures the cycle (plan write + watermark advance + new-period
	// invoice) and that must be one tx regardless of how much of the OLD period
	// remained. applyCrossIntervalPlanSwapTx derives the new period from `now`,
	// not from remainingDays, so the boundary case is sound — otherwise it would
	// fall to the non-atomic fallback and re-open the silent-revenue-drop window
	// at the boundary. (ADR-056.)
	crossIntervalSwap := false
	if isImmediatePlanChange && h.plans != nil && oldPlanID != "" {
		oldP, e1 := h.plans.GetPlan(ctx, tenantID, oldPlanID)
		if e1 != nil {
			respond.FromError(w, r, fmt.Errorf("get current plan %s: %w", oldPlanID, e1), "plan")
			return
		}
		// A missing new plan is the service's validation error to report;
		// any other read error must not misroute a cross-interval swap onto
		// the non-atomic path (ADR-056).
		newP, e2 := h.plans.GetPlan(ctx, tenantID, input.NewPlanID)
		if e2 != nil && !errors.Is(e2, errs.ErrNotFound) {
			respond.FromError(w, r, fmt.Errorf("get new plan %s: %w", input.NewPlanID, e2), "plan")
			return
		}
		if e2 == nil {
			oi := oldP.BillingInterval
			if oi == "" {
				oi = domain.BillingMonthly
			}
			ni := newP.BillingInterval
			if ni == "" {
				ni = domain.BillingMonthly
			}
			crossIntervalSwap = oi != ni
		}
	}
	atomic := h.db != nil && h.invoices != nil && (wantProration || crossIntervalSwap)
	var (
		result    ItemChangeResult
		atomicErr error
	)
	if atomic {
		result, atomicErr = h.atomicUpdateItemWithProration(ctx, tenantID, subID, itemID, input)
		if errors.Is(atomicErr, errAtomicNotApplicable) {
			atomic = false
			atomicErr = nil
		}
		if atomicErr == nil && result.pricedFrom.item.ID != "" {
			// The plan the change moved from, read under the lock (#1270).
			oldPlanID = result.pricedFrom.item.PlanID
		}
	}
	if !atomic {
		var err error
		result, err = h.svc.UpdateItem(ctx, tenantID, subID, itemID, input)
		if err != nil {
			respond.FromError(w, r, err, "subscription item")
			return
		}
	} else if atomicErr != nil {
		respond.FromError(w, r, atomicErr, "subscription item")
		return
	}

	// Skip the legacy delta-proration emission when the service used
	// the cross-axis orchestrator. The orchestrator already issued
	// the refund credit (for OLD in_advance unused) and — when NEW is
	// in_advance — billed the new in_advance period, with the old period's
	// usage up to the swap, synchronously via BillOnPlanSwapTx. Firing handleItemProration on top would emit a
	// second credit against the same OLD period; the gates inside
	// handleItemProration aren't tight enough (a freshly auto-charged
	// new in_advance invoice would pass the paid-check), so an
	// explicit skip here is the only safe guard.
	if !atomic && prorationEligible && prorationRemainingDays > 0 && h.invoices != nil && !result.OrchestratedCrossAxis {
		// Re-hydrate the subscription post-change so the Items slice reflects
		// the swapped plan/quantity — handleProration walks it to price the
		// change (currency/naming). Fall back to subBefore on error so the
		// handler still responds, but use the fresh Items when available.
		subAfter, getErr := h.svc.Get(ctx, tenantID, subID)
		if getErr != nil {
			subAfter = subBefore
		}
		var spec itemProrationSpec
		if isImmediatePlanChange {
			var changeAt time.Time
			if result.Item.PlanChangedAt != nil {
				changeAt = *result.Item.PlanChangedAt
			} else {
				changeAt = clock.Now(ctx)
			}
			spec = itemProrationSpec{
				changeType:    domain.ItemChangeTypePlan,
				changeAt:      changeAt,
				remainingDays: prorationRemainingDays,
				totalDays:     prorationTotalDays,
				itemID:        result.Item.ID,
				oldPlanID:     oldPlanID,
				oldQuantity:   result.Item.Quantity,
				newPlanID:     result.Item.PlanID,
				newQuantity:   result.Item.Quantity,
			}
		} else {
			// Quantity-only change. Plan is unchanged; store doesn't stamp a
			// dedicated timestamp so we use UpdatedAt (the store bumps it on
			// every item write) — stable across retries of the same in-flight
			// UpdateItemQuantity call.
			changeAt := result.Item.UpdatedAt
			if changeAt.IsZero() {
				changeAt = clock.Now(ctx)
			}
			spec = itemProrationSpec{
				changeType:    domain.ItemChangeTypeQuantity,
				changeAt:      changeAt,
				remainingDays: prorationRemainingDays,
				totalDays:     prorationTotalDays,
				itemID:        result.Item.ID,
				oldPlanID:     oldPlanID,
				oldQuantity:   oldQuantity,
				newPlanID:     result.Item.PlanID,
				newQuantity:   result.Item.Quantity,
			}
		}
		prorationResult, prorationErr := h.handleItemProration(r.Context(), tenantID, subAfter, spec, nil)
		if prorationErr != nil {
			// Deliberate ADR-050 block (upgrade charges against an unpaid
			// source) → clean 409, not the internal-failure 500 below.
			if errors.Is(prorationErr, errs.ErrInvalidState) {
				respond.FromError(w, r, prorationErr, "subscription item")
				return
			}
			slog.ErrorContext(r.Context(), "item proration failed after item change committed",
				"subscription_id", subID,
				"item_id", result.Item.ID,
				"tenant_id", tenantID,
				"change_type", spec.changeType,
				"old_plan_id", oldPlanID,
				"new_plan_id", input.NewPlanID,
				"old_quantity", oldQuantity,
				"new_quantity", spec.newQuantity,
				"proration_remaining_days", prorationRemainingDays,
				"proration_total_days", prorationTotalDays,
				"error", prorationErr,
			)
			if h.auditLogger != nil {
				_ = h.auditLogger.Log(h.auditCtx(r.Context(), tenantID, subID), tenantID, domain.AuditActionSubscriptionProrationFailed, "subscription", subID, "", map[string]any{
					"item_id":                  result.Item.ID,
					"change_type":              string(spec.changeType),
					"old_plan_id":              oldPlanID,
					"new_plan_id":              input.NewPlanID,
					"old_quantity":             oldQuantity,
					"new_quantity":             spec.newQuantity,
					"proration_remaining_days": prorationRemainingDays,
					"proration_total_days":     prorationTotalDays,
					"error":                    prorationErr.Error(),
				})
			}
			respond.Error(w, r, http.StatusInternalServerError, "api_error", "proration_failed",
				"item change succeeded but the prorated adjustment could not be created — the item is on the new state and no automatic retry will run (re-sending this request is rejected as a no-op change); issue the prorated charge or credit manually, or contact support")
			return
		}
		if prorationResult != nil {
			result.Proration = prorationResult
		}
	}

	// Audit the item change AFTER proration resolves, so the outcome
	// (proration invoice / credit / adjustment) is on the row for EVERY
	// path — the atomic wrapper sets result.Proration before returning,
	// the legacy/cross-interval path sets it just above. Written only on
	// success; a proration failure already returned a proration_failed
	// row + 500 above.
	if h.auditLogger != nil {
		payload := map[string]any{
			"item_id":   result.Item.ID,
			"immediate": input.Immediate,
		}
		addProrationMeta(payload, result.Proration, h.planCurrency(ctx, tenantID, result.Item.PlanID))
		// A cross-interval swap's credit for the old period's unused time
		// that its invoices had no room left for (ADR-118).
		if c := result.swapRelief.UncreditedCents; c > 0 {
			payload["uncredited_cents"] = c
		}
		// The item as the change found it under the subscription row lock
		// (#1270): the plan and quantity it moved from. Every path sets it,
		// a scheduled change included, which keeps the item on this plan
		// until the boundary; oldPlanID and oldQuantity are read only for
		// changes that prorate.
		was := result.pricedFrom.item
		if input.Quantity != nil {
			payload["action"] = domain.SubAuditItemQuantityChanged
			payload["quantity"] = *input.Quantity
			payload["plan_id"] = result.Item.PlanID
			if was.Quantity > 0 {
				payload["old_quantity"] = was.Quantity
			}
		} else {
			payload["action"] = domain.SubAuditItemPlanChanged
			payload["old_plan_id"] = was.PlanID
			payload["new_plan_id"] = input.NewPlanID
			// A scheduled change names the boundary it lands on.
			if !input.Immediate && result.Item.PendingPlanEffectiveAt != nil {
				payload["effective_at"] = result.Item.PendingPlanEffectiveAt.UTC()
			}
		}
		// Sim-time context (ADR-030): re-fetch the post-change sub for its
		// UpdatedAt (the change instant) + test_clock_id so a clock-pinned
		// change renders the simulated effect-time subline.
		auditSub := subBefore
		if s, err := h.svc.Get(ctx, tenantID, subID); err == nil {
			auditSub = s
		}
		_ = h.auditLogger.Log(h.auditCtx(ctx, tenantID, subID), tenantID, domain.AuditActionSubscriptionItemUpdated, "subscription", subID, auditSub.Code, payload)
	}

	// Event dispatch. Quantity changes and immediate plan changes are
	// observable-now → subscription.item.updated. A scheduled plan change
	// (Immediate=false, NewPlanID set) is an intent, not a mutation of the
	// current cycle → subscription.pending_change.scheduled; the applied
	// event fires at the cycle boundary from billing.Engine.
	// The item in the subscription's items holds the new quantity or plan,
	// or the scheduled one (pending_plan_id, pending_plan_effective_at).
	if h.events != nil {
		eventType := domain.EventSubscriptionItemUpdated
		if input.NewPlanID != "" && !input.Immediate {
			eventType = domain.EventSubscriptionPendingChangeScheduled
		}
		h.fireItemEvent(r.Context(), tenantID, eventType, subID, result.Item.ID)
	}

	respond.JSON(w, r, http.StatusOK, result)
}

func (h *Handler) cancelPendingItemChange(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	subID := chi.URLParam(r, "id")
	itemID := chi.URLParam(r, "itemID")

	item, err := h.svc.CancelPendingItemChange(r.Context(), tenantID, subID, itemID)
	if err != nil {
		respond.FromError(w, r, err, "subscription item")
		return
	}

	if h.auditLogger != nil {
		_ = h.auditLogger.Log(h.auditCtx(r.Context(), tenantID, subID), tenantID, domain.AuditActionUpdate, "subscription", subID, "", map[string]any{
			"action":  domain.SubAuditPendingPlanChangeCanceled,
			"item_id": item.ID,
		})
	}

	if h.events != nil {
		h.fireItemEvent(r.Context(), tenantID, domain.EventSubscriptionPendingChangeCanceled, subID, item.ID)
	}

	respond.JSON(w, r, http.StatusOK, item)
}

func (h *Handler) removeItem(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	subID := chi.URLParam(r, "id")
	itemID := chi.URLParam(r, "itemID")

	// Bind effective-now from the sub pin (PR-12).
	ctx := h.bindForSub(r.Context(), tenantID, subID)
	r = r.WithContext(ctx)

	// Close an ended, unbilled period first, so the snapshot below is the
	// period that is running (Service.CloseEndedPeriodFirst).
	if _, err := h.svc.CloseEndedPeriodFirst(ctx, tenantID, subID); err != nil {
		respond.FromError(w, r, err, "subscription")
		return
	}

	// Read the item and subscription to decide whether the removal credits
	// the unused portion of what the customer already paid for this item. On
	// the atomic path the credit is priced from the state RemoveItemTx reads
	// under the subscription row lock (#1270); only the legacy path with no
	// database wired prices from this read.
	var removedPlanID string
	var removedQuantity int64
	var subBefore domain.Subscription
	var prorationRemainingDays, prorationTotalDays int64
	if h.plans != nil && h.invoices != nil {
		// Read errors fail the remove: it is a hard delete, so carrying on
		// would lose the unused-portion credit and the data to compute it.
		item, gerr := h.svc.store.GetItem(ctx, tenantID, itemID)
		if gerr != nil {
			respond.FromError(w, r, gerr, "subscription item")
			return
		}
		if item.SubscriptionID == subID {
			removedPlanID = item.PlanID
			removedQuantity = item.Quantity
		}
		sub, serr := h.svc.Get(ctx, tenantID, subID)
		if serr != nil {
			respond.FromError(w, r, serr, "subscription")
			return
		}
		subBefore = sub
		if sub.Status == domain.SubscriptionActive {
			prorationRemainingDays, prorationTotalDays = remainingPeriodRatio(sub, clock.Now(ctx))
		}
	}

	// Atomic RemoveItem-with-proration: delete + credit-note draft in one
	// tx so a failed draft rolls back the delete. Falls back to
	// the legacy non-atomic flow when db isn't wired or no proration
	// emission is needed.
	wantProration := prorationRemainingDays > 0 && removedPlanID != "" && h.invoices != nil
	atomic := h.db != nil && wantProration
	var removeProration *ProrationDetail
	if atomic {
		removed, detail, err := h.atomicRemoveItemWithProration(ctx, tenantID, subID, itemID)
		if err != nil {
			respond.FromError(w, r, err, "subscription item")
			return
		}
		removeProration = detail
		// The plan the removal credited, read under the lock (#1270).
		removedPlanID = removed.PlanID
	} else {
		if err := h.svc.RemoveItem(ctx, tenantID, subID, itemID); err != nil {
			respond.FromError(w, r, err, "subscription item")
			return
		}
	}

	if h.auditLogger != nil {
		payload := map[string]any{
			"action":  domain.SubAuditItemRemoved,
			"item_id": itemID,
			// Which plan left — captured for proration math anyway;
			// without it the feed said "Item removed" with no noun.
			"plan_id": removedPlanID,
		}
		addProrationMeta(payload, removeProration, h.planCurrency(ctx, tenantID, removedPlanID))
		_ = h.auditLogger.Log(h.auditCtx(ctx, tenantID, subID), tenantID, domain.AuditActionUpdate, "subscription", subID, subBefore.Code, payload)
	}

	// Legacy non-atomic proration emission path — only when atomic wasn't taken.
	if !atomic && prorationRemainingDays > 0 && removedPlanID != "" {
		// Re-fetch to price the proration over the remaining items.
		subAfter, getErr := h.svc.Get(ctx, tenantID, subID)
		if getErr != nil {
			subAfter = subBefore
		}
		spec := itemProrationSpec{
			changeType:    domain.ItemChangeTypeRemove,
			changeAt:      clock.Now(ctx),
			remainingDays: prorationRemainingDays,
			totalDays:     prorationTotalDays,
			itemID:        itemID,
			oldPlanID:     removedPlanID,
			oldQuantity:   removedQuantity,
			newPlanID:     "",
			newQuantity:   0,
		}
		prorationResult, prorationErr := h.handleItemProration(ctx, tenantID, subAfter, spec, nil)
		if prorationErr != nil {
			slog.ErrorContext(r.Context(), "item proration failed after item remove committed",
				"subscription_id", subID,
				"item_id", itemID,
				"tenant_id", tenantID,
				"plan_id", removedPlanID,
				"quantity", removedQuantity,
				"proration_remaining_days", prorationRemainingDays,
				"proration_total_days", prorationTotalDays,
				"error", prorationErr,
			)
			if h.auditLogger != nil {
				_ = h.auditLogger.Log(h.auditCtx(r.Context(), tenantID, subID), tenantID, domain.AuditActionSubscriptionProrationFailed, "subscription", subID, "", map[string]any{
					"item_id":                  itemID,
					"change_type":              string(domain.ItemChangeTypeRemove),
					"plan_id":                  removedPlanID,
					"quantity":                 removedQuantity,
					"proration_remaining_days": prorationRemainingDays,
					"proration_total_days":     prorationTotalDays,
					"error":                    prorationErr.Error(),
				})
			}
			respond.Error(w, r, http.StatusInternalServerError, "api_error", "proration_failed",
				"item remove succeeded but the prorated credit could not be created — the item is removed and no automatic retry will run (re-sending this request returns not-found); grant the credit manually, or contact support")
			return
		}
		_ = prorationResult
	}

	if h.events != nil {
		h.fireItemEvent(r.Context(), tenantID, domain.EventSubscriptionItemRemoved, subID, itemID)
	}

	w.WriteHeader(http.StatusNoContent)
}

// itemProrationSpec describes a single item mutation that the proration
// generator should price. The generator computes a per-unit-period delta
// between the before-state amount (oldPlan × oldQty) and the after-state
// amount (newPlan × newQty), scales by prorationFactor, and emits either a
// proration invoice (positive delta) or a proration credit (negative).
//
// Each of the four mutation types fills the struct slightly differently:
//   - plan:     old/new plan differ, old/new qty equal (single item.Quantity)
//   - quantity: old/new plan equal, old/new qty differ
//   - add:      oldPlanID="", oldQuantity=0; new populated (delta = +new)
//   - remove:   newPlanID="", newQuantity=0; old populated (delta = -old)
//
// changeAt is the dedup-key timestamp (kept on invoice.source_plan_changed_at
// for historical reasons — see migration 0027 comment). Callers should pass
// item.PlanChangedAt for plan changes, or a freshly-stamped clock for the
// other three.
type itemProrationSpec struct {
	changeType    domain.ItemChangeType
	changeAt      time.Time
	remainingDays int64 // ADR-042: integer day-ratio for proration math
	totalDays     int64
	itemID        string
	oldPlanID     string
	oldQuantity   int64
	newPlanID     string
	newQuantity   int64
}

// atomicAddItemWithProration opens one outer transaction that wraps
// the subscription-item insert + the proration invoice/credit insert.
// A failure on either side rolls back BOTH via the deferred Rollback —
// so the API never returns success with the item committed but no
// proration recorded. Pre-2026-05-29 the two writes were independent
// txs and a proration failure left an orphan item (the bug surfaced
// during EX3 manual test).
//
// Returns the new item on success; on any error, the entire operation
// is rolled back and the caller sees a failure (no half-committed
// state for them to reconcile).
//
// The add is priced from result.pricedFrom.sub: the subscription AddItemTx
// read under the subscription row lock (#1270), never from addItem's read
// before the tx, whose period a plan swap or a period close that committed
// in between has replaced.
func (h *Handler) atomicAddItemWithProration(
	ctx context.Context,
	tenantID, subID string,
	input AddItemInput,
) (domain.SubscriptionItem, *ProrationDetail, error) {
	tx, err := h.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.SubscriptionItem{}, nil, fmt.Errorf("begin atomic addItem tx: %w", err)
	}
	defer postgres.Rollback(tx)

	result, err := h.svc.AddItemTx(ctx, tx, tenantID, subID, input)
	if err != nil {
		return domain.SubscriptionItem{}, nil, err
	}
	item, locked := result.Item, result.pricedFrom.sub
	remainingDays, totalDays, err := h.addProrationDays(ctx, tenantID, locked, result.EffectiveAt)
	if err != nil {
		return domain.SubscriptionItem{}, nil, fmt.Errorf("proration days in atomic addItem tx: %w", err)
	}

	changeAt := item.CreatedAt
	if changeAt.IsZero() {
		changeAt = clock.Now(ctx)
	}
	spec := itemProrationSpec{
		changeType:    domain.ItemChangeTypeAdd,
		changeAt:      changeAt,
		remainingDays: remainingDays,
		totalDays:     totalDays,
		itemID:        item.ID,
		oldPlanID:     "",
		oldQuantity:   0,
		newPlanID:     item.PlanID,
		newQuantity:   item.Quantity,
	}
	detail, err := h.handleItemProration(ctx, tenantID, locked, spec, tx)
	if err != nil {
		// A deliberate ADR-050 block (charge against an unpaid source) is an
		// operator-facing 409, not an internal failure — pass it through
		// unwrapped so the message stays clean; the deferred Rollback undoes
		// the item add so nothing half-applies.
		if errors.Is(err, errs.ErrInvalidState) {
			return domain.SubscriptionItem{}, nil, err
		}
		return domain.SubscriptionItem{}, nil, fmt.Errorf("proration in atomic addItem tx: %w", err)
	}

	// Create the clawback credit-note DRAFT(s) IN this tx so they commit
	// atomically with the item change (ADR-056) — a create failure rolls the
	// change back rather than leaving the customer un-credited. No-op for the
	// add path (upgrades charge, never claw back). Issued post-commit below.
	if err := h.createClawbackDraftsTx(ctx, tx, tenantID, detail); err != nil {
		return domain.SubscriptionItem{}, nil, fmt.Errorf("create clawback draft in atomic addItem tx: %w", err)
	}
	// Enroll the proration CHARGE invoice for the auto-charge sweep IN this tx, so
	// enrollment commits atomically with the item change — closes the window where
	// a failed post-commit enrollment left a committed charge invoice unenrolled
	// and unpaid (no sweep scans auto_charge_pending=false). No-op for credit paths.
	if err := h.enrollAutoChargeTx(ctx, tx, tenantID, detail); err != nil {
		return domain.SubscriptionItem{}, nil, fmt.Errorf("enroll proration charge in atomic addItem tx: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return domain.SubscriptionItem{}, nil, fmt.Errorf("commit atomic addItem tx: %w", err)
	}
	// invoice.finalized for the charge invoice, now that it is durable (its
	// finalize audit row committed in the tx above).
	h.dispatchProrationFinalized(ctx, tenantID, detail)
	// Commit proration tax AFTER the tx is durable — CommitTax is an external
	// Stripe call and must not ride the DB tx (a rollback would orphan the
	// committed tax transaction). No-op unless a stripe_tax calculation exists.
	h.commitProrationTax(ctx, tenantID, detail)
	// Issue the clawback DRAFTS created above, AFTER the tx is durable (Stripe
	// reversal is external). On failure the draft persists + RetryPendingClawback
	// Issue re-issues it. No-op for the add path (no clawback).
	h.issueClawbackDrafts(ctx, tenantID, detail)
	// (Auto-charge enrollment now happens IN the tx above — see enrollAutoChargeTx.)
	// Return the proration detail so addItem's audit row can show the
	// charge the add produced (see atomicUpdateItemWithProration).
	return item, detail, nil
}

// addProrationDays is the share of sub's current period an item added at
// `at` pays for in advance: the days left and the period's days, or zero
// when sub is not active or the days left round to zero.
//
// Day-grade clamp (ADR-012 amendment 2026-07-26): an item added ON the
// period-start calendar day (tenant TZ) owes the whole period — the add day
// counts whole, matching the cycle-close segment clamp in
// billing.itemBaseSegments. Without this a post-noon day-1 add pays 30/31
// in_advance while its in_arrears twin bills the full period at close. Adds
// only: plan/quantity/remove changes stay instant-precise per ADR-012's
// Consequences, so their handlers don't clamp.
func (h *Handler) addProrationDays(ctx context.Context, tenantID string, sub domain.Subscription, at time.Time) (remainingDays, totalDays int64, err error) {
	if sub.Status != domain.SubscriptionActive {
		return 0, 0, nil
	}
	remainingDays, totalDays = remainingPeriodRatio(sub, at)
	if remainingDays > 0 && sub.CurrentBillingPeriodStart != nil {
		loc, err := h.tenantLoc(ctx, tenantID)
		if err != nil {
			return 0, 0, err
		}
		if domain.SameCalendarDayIn(at, *sub.CurrentBillingPeriodStart, loc) {
			remainingDays = totalDays
		}
	}
	return remainingDays, totalDays, nil
}

// atomicUpdateItemWithProration mirrors atomicAddItemWithProration for
// the UpdateItem path: open one outer tx, write the item update + the
// proration in the same tx, commit on success. Returns
// errAtomicNotApplicable if the input routes to the cross-interval or
// scheduled path (caller falls back to legacy non-atomic flow).
//
// The proration is priced from result.pricedFrom: the subscription and the
// item as UpdateItemTx read them under the subscription row lock (#1270).
// Never from updateItem's read before the tx, which a change to the same
// item that committed in between makes stale: two overlapping changes would
// both be priced from the state they started from.
func (h *Handler) atomicUpdateItemWithProration(
	ctx context.Context,
	tenantID, subID, itemID string,
	input UpdateItemInput,
) (ItemChangeResult, error) {
	tx, err := h.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return ItemChangeResult{}, fmt.Errorf("begin atomic updateItem tx: %w", err)
	}
	defer postgres.Rollback(tx)

	result, err := h.svc.UpdateItemTx(ctx, tx, tenantID, subID, itemID, input)
	if err != nil {
		return ItemChangeResult{}, err
	}
	base := result.pricedFrom

	// Cross-interval swap: UpdateItemTx restructured the cycle atomically on
	// this tx (plan write + watermark + new in_advance invoice). It did its OWN
	// proration, so handleItemProration MUST NOT run — that would emit a second
	// credit against the OLD period. Commit the swap, then run the OLD-period
	// refund + the new invoice's external steps POST-commit (Stripe must not
	// ride the tx). A failed in-tx bill already rolled the whole swap back above.
	if result.OrchestratedCrossAxis {
		if err := tx.Commit(); err != nil {
			return ItemChangeResult{}, fmt.Errorf("commit atomic cross-interval swap tx: %w", err)
		}
		h.svc.FinalizeCrossIntervalSwap(ctx, tenantID, base.sub, result)
		return result, nil
	}

	remainingDays, totalDays := remainingPeriodRatio(base.sub, result.EffectiveAt)
	var spec itemProrationSpec
	if input.NewPlanID != "" && input.Immediate {
		var changeAt time.Time
		if result.Item.PlanChangedAt != nil {
			changeAt = *result.Item.PlanChangedAt
		} else {
			changeAt = clock.Now(ctx)
		}
		spec = itemProrationSpec{
			changeType:    domain.ItemChangeTypePlan,
			changeAt:      changeAt,
			remainingDays: remainingDays,
			totalDays:     totalDays,
			itemID:        result.Item.ID,
			oldPlanID:     base.item.PlanID,
			oldQuantity:   result.Item.Quantity,
			newPlanID:     result.Item.PlanID,
			newQuantity:   result.Item.Quantity,
		}
	} else {
		changeAt := result.Item.UpdatedAt
		if changeAt.IsZero() {
			changeAt = clock.Now(ctx)
		}
		spec = itemProrationSpec{
			changeType:    domain.ItemChangeTypeQuantity,
			changeAt:      changeAt,
			remainingDays: remainingDays,
			totalDays:     totalDays,
			itemID:        result.Item.ID,
			oldPlanID:     base.item.PlanID,
			oldQuantity:   base.item.Quantity,
			newPlanID:     result.Item.PlanID,
			newQuantity:   result.Item.Quantity,
		}
	}
	detail, err := h.handleItemProration(ctx, tenantID, base.sub, spec, tx)
	if err != nil {
		// A deliberate ADR-050 block (charge against an unpaid source) is an
		// operator-facing 409, not an internal failure — pass it through
		// unwrapped so the message stays clean; the deferred Rollback undoes
		// the item change so nothing half-applies.
		if errors.Is(err, errs.ErrInvalidState) {
			return ItemChangeResult{}, err
		}
		return ItemChangeResult{}, fmt.Errorf("proration in atomic updateItem tx: %w", err)
	}

	// Create the clawback credit-note DRAFT(s) IN this tx so a plan-downgrade /
	// qty-decrease's credit commits atomically with the item change (ADR-056) —
	// a create failure rolls the change back. No-op on upgrades / qty-increases.
	// Issued post-commit below.
	if err := h.createClawbackDraftsTx(ctx, tx, tenantID, detail); err != nil {
		return ItemChangeResult{}, fmt.Errorf("create clawback draft in atomic updateItem tx: %w", err)
	}
	// Enroll the proration CHARGE invoice for the auto-charge sweep IN this tx, so
	// enrollment commits atomically with the item change (see atomicAddItem).
	if err := h.enrollAutoChargeTx(ctx, tx, tenantID, detail); err != nil {
		return ItemChangeResult{}, fmt.Errorf("enroll proration charge in atomic updateItem tx: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return ItemChangeResult{}, fmt.Errorf("commit atomic updateItem tx: %w", err)
	}
	// invoice.finalized for an upgrade or seat-increase invoice, now that it
	// is durable (see atomicAddItemWithProration).
	h.dispatchProrationFinalized(ctx, tenantID, detail)
	// Commit proration tax AFTER the tx is durable (external Stripe call;
	// must not ride the DB tx). No-op unless a stripe_tax calculation exists.
	h.commitProrationTax(ctx, tenantID, detail)
	// Issue the clawback DRAFTS created above, AFTER the tx is durable. On
	// failure the draft persists + RetryPendingClawbackIssue re-issues it. No-op
	// on upgrades / qty-increases (those bill via the invoice path).
	h.issueClawbackDrafts(ctx, tenantID, detail)
	// (Auto-charge enrollment now happens IN the tx above — see enrollAutoChargeTx.)
	// Surface the proration outcome so the caller's audit row (and the
	// activity timeline built from it) can show WHAT the change billed —
	// not just that a change happened. Pre-fix this detail was dropped
	// here and result.Proration stayed nil at audit-write time.
	result.Proration = detail
	return result, nil
}

// atomicRemoveItemWithProration opens one outer tx wrapping the item
// delete + the remove-proration credit grant. A failure on the credit
// side rolls back the item delete — customer keeps the item (and its
// future cycle billing) but at least there's no orphan-credit gap.
// ADR-030 atomic-proration follow-through (2026-05-29).
//
// The credit is priced from the item and subscription RemoveItemTx read
// under the subscription row lock (#1270), so a quantity change that
// committed while this request was reading is credited too. Returns the
// item as it was removed.
func (h *Handler) atomicRemoveItemWithProration(
	ctx context.Context,
	tenantID, subID, itemID string,
) (domain.SubscriptionItem, *ProrationDetail, error) {
	tx, err := h.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.SubscriptionItem{}, nil, fmt.Errorf("begin atomic removeItem tx: %w", err)
	}
	defer postgres.Rollback(tx)

	result, err := h.svc.RemoveItemTx(ctx, tx, tenantID, subID, itemID)
	if err != nil {
		return domain.SubscriptionItem{}, nil, err
	}
	base := result.pricedFrom

	// Only an active subscription's unused prebill is credited.
	var remainingDays, totalDays int64
	if base.sub.Status == domain.SubscriptionActive {
		remainingDays, totalDays = remainingPeriodRatio(base.sub, result.EffectiveAt)
	}
	spec := itemProrationSpec{
		changeType:    domain.ItemChangeTypeRemove,
		changeAt:      result.EffectiveAt,
		remainingDays: remainingDays,
		totalDays:     totalDays,
		itemID:        itemID,
		oldPlanID:     base.item.PlanID,
		oldQuantity:   base.item.Quantity,
		newPlanID:     "",
		newQuantity:   0,
	}
	detail, err := h.handleItemProration(ctx, tenantID, base.sub, spec, tx)
	if err != nil {
		return domain.SubscriptionItem{}, nil, fmt.Errorf("proration in atomic removeItem tx: %w", err)
	}

	// Create the item-removal clawback credit-note DRAFT(s) IN this tx so the
	// clawback obligation commits atomically with the item delete (ADR-056) — a
	// create failure rolls the delete back (this is what makes the function's
	// "a credit failure rolls back the delete" contract actually hold). Issued
	// post-commit below.
	if err := h.createClawbackDraftsTx(ctx, tx, tenantID, detail); err != nil {
		return domain.SubscriptionItem{}, nil, fmt.Errorf("create clawback draft in atomic removeItem tx: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return domain.SubscriptionItem{}, nil, fmt.Errorf("commit atomic removeItem tx: %w", err)
	}
	// Remove proration is a credit (no invoice/tax) so commitProrationTax
	// no-ops, but keep the post-tx commit uniform across the atomic wrappers.
	h.commitProrationTax(ctx, tenantID, detail)
	// Issue the clawback DRAFTS created above, AFTER the tx is durable (ADR-048):
	// removing an item mid-cycle claws back the unused prebill as a tax-reversing
	// CN per paid funding invoice. On failure the draft persists +
	// RetryPendingClawbackIssue re-issues it.
	h.issueClawbackDrafts(ctx, tenantID, detail)
	// Return the credit detail so removeItem's audit row can show the
	// clawback amount (see atomicUpdateItemWithProration).
	return base.item, detail, nil
}

// finalizeAuditEmit returns the emission that writes the finalize audit row
// for a proration invoice born finalized, on the transaction that inserts it.
// nil when no recorder is wired. A draft gets no row here: it gets one from
// invoice.Service.Finalize when it finalizes.
func (h *Handler) finalizeAuditEmit(ctx context.Context, tenantID, subID string) func(tx *sql.Tx, out domain.Invoice) error {
	if h.finalizeAudit == nil {
		return nil
	}
	actx := h.auditCtx(ctx, tenantID, subID)
	return func(tx *sql.Tx, out domain.Invoice) error {
		if out.Status != domain.InvoiceFinalized {
			return nil
		}
		return h.finalizeAudit.EmitFinalizeAuditTx(actx, tx, out)
	}
}

// dispatchProrationFinalized sends invoice.finalized for the proration invoice
// this change created finalized. Called after the row is durable, never inside
// a transaction. No-op for a draft, a credit or a dedup replay.
func (h *Handler) dispatchProrationFinalized(ctx context.Context, tenantID string, detail *ProrationDetail) {
	if h.finalizeAudit == nil || detail == nil || detail.finalizedInvoice == nil {
		return
	}
	h.finalizeAudit.DispatchInvoiceFinalized(ctx, tenantID, *detail.finalizedInvoice)
}

// commitProrationTax turns the proration invoice's provider tax calculation
// into a reportable tax transaction (Stripe Tax), mirroring the engine's
// post-finalize CommitTax for cycle/create invoices. No-op unless a provider
// produced a calculation id (stripe_tax) — manual/none and credit-path
// (downgrade) prorations carry no calculation. Non-fatal: the invoice is
// already authoritative; a transient commit failure is logged, like the
// engine's convention, and can be reconciled later.
func (h *Handler) commitProrationTax(ctx context.Context, tenantID string, detail *ProrationDetail) {
	if h.tax == nil || detail == nil || detail.TaxProvider == "" || detail.TaxCalculationID == "" {
		return
	}
	if err := h.tax.CommitTax(ctx, tenantID, detail.InvoiceID, detail.TaxCalculationID); err != nil {
		slog.WarnContext(ctx, "tax: commit failed after proration invoice",
			"error", err, "tenant_id", tenantID, "invoice_id", detail.InvoiceID)
	}
}

// enrollAutoCharge flags a freshly-created proration CHARGE invoice for the
// auto-charge sweep so it actually collects (parity with engine cycle/create
// invoices, which enroll at finalize). Called AFTER the atomic tx commits — a
// flag UPDATE on a not-yet-durable row would be lost — and inline on the
// non-atomic path, mirroring commitProrationTax. No-op unless the charge branch
// recorded a fresh invoice id. On error the invoice is durable but unenrolled;
// log at error level (the operator can Collect Payment manually) — same
// reconcilable risk profile as the post-commit tax commit.
func (h *Handler) enrollAutoCharge(ctx context.Context, tenantID string, detail *ProrationDetail) {
	if detail == nil || detail.AutoChargeInvoiceID == "" {
		return
	}
	if err := h.invoices.SetAutoChargePending(ctx, tenantID, detail.AutoChargeInvoiceID, true); err != nil {
		slog.ErrorContext(ctx, "proration charge invoice: re-queue for auto-charge failed (its create already queued it if finalized)",
			"error", err, "tenant_id", tenantID, "invoice_id", detail.AutoChargeInvoiceID)
	}
}

// enrollAutoChargeTx is the atomic-path variant of enrollAutoCharge: it enrolls the
// proration CHARGE invoice INSIDE the caller's tx, so enrollment commits atomically
// with the item change. A failure here rolls the whole change back (the operator
// retries) instead of leaving a committed-but-unenrolled charge invoice to sit
// unpaid with no reconciler. No-op for credit/adjustment paths.
func (h *Handler) enrollAutoChargeTx(ctx context.Context, tx *sql.Tx, tenantID string, detail *ProrationDetail) error {
	if detail == nil || detail.AutoChargeInvoiceID == "" {
		return nil
	}
	return h.invoices.SetAutoChargePendingTx(ctx, tx, tenantID, detail.AutoChargeInvoiceID, true)
}

// issueClawbackCreditNote issues the tax-reversing adjustment credit note for a
// downgrade clawback recorded on the detail (ADR-048). Issued AFTER the atomic
// tx is durable — the credit-note service is not tx-aware and its tax reversal
// is an external Stripe call, so it must not ride the DB tx (a rollback would
// orphan a committed CN + balance grant). The downgrade branch calls it inline
// on the non-atomic path. No-op unless that branch recorded credit shares.
//
// On error the item change is already durable but the customer was not
// credited — log at error level so it surfaces for reconciliation. The
// operation still reports success: the downgrade itself committed, and a client
// retry is rejected at Service.UpdateItem's no-op gate (so we never double-
// issue), so returning an error here would be misleading and unrecoverable by
// retry anyway. The source invoice's over-credit cap bounds any aggregate
// over-credit.
func (h *Handler) issueClawbackCreditNote(ctx context.Context, tenantID string, detail *ProrationDetail) {
	if h.creditNotes == nil || detail == nil {
		return
	}
	pieces := detail.ClawbackPieces
	for i, p := range pieces {
		if _, err := h.creditNotes.CreateAndIssueAdjustment(ctx, tenantID, p.InvoiceID, p.GrossCents, detail.ClawbackReason, detail.ClawbackMemo); err != nil {
			slog.ErrorContext(ctx, "downgrade clawback credit note failed after item change committed; customer not yet credited, reconcile manually",
				"error", err,
				"tenant_id", tenantID,
				"source_invoice_id", p.InvoiceID,
				"gross_cents", p.GrossCents,
				"piece_index", i,
				"piece_count", len(pieces),
				"reason", detail.ClawbackReason)
		}
	}
}

// createClawbackDraftsTx creates the downgrade/removal clawback credit note(s)
// as DRAFTS inside the caller's tx (coordinator-owned *sql.Tx, ADR-056),
// recording their ids on detail.ClawbackDraftIDs for the post-commit
// issueClawbackDrafts. Because the drafts commit WITH the caller's item change,
// a create failure returns an error and rolls the change back — closing the
// post-commit fire-and-forget gap where a removed/downgraded item could leave
// the customer un-credited. No-op when the detail carries no clawback
// (upgrades / qty-increases bill via the invoice path).
func (h *Handler) createClawbackDraftsTx(ctx context.Context, tx *sql.Tx, tenantID string, detail *ProrationDetail) error {
	if h.creditNotes == nil || detail == nil {
		return nil
	}
	pieces := detail.ClawbackPieces
	pieceIDs := make([]string, 0, len(pieces))
	for _, p := range pieces {
		pieceIDs = append(pieceIDs, p.InvoiceID)
	}
	if err := h.creditNotes.LockInvoicesForDraftsTx(ctx, tx, tenantID, pieceIDs); err != nil {
		return fmt.Errorf("lock clawback source invoices: %w", err)
	}
	for _, p := range pieces {
		cn, err := h.creditNotes.CreateAdjustmentDraftTx(ctx, tx, tenantID, p.InvoiceID, p.GrossCents, detail.ClawbackReason, detail.ClawbackMemo)
		if err != nil {
			return fmt.Errorf("create clawback credit note draft (source %s, %d cents): %w", p.InvoiceID, p.GrossCents, err)
		}
		detail.ClawbackDraftIDs = append(detail.ClawbackDraftIDs, cn.ID)
	}
	return nil
}

// issueClawbackDrafts issues the drafts createClawbackDraftsTx recorded, AFTER
// the caller's tx committed (the Stripe Tax reversal is an external call that
// can't ride the DB tx). The draft itself is durable (committed with the item
// change), so an Issue() failure here is never a silent loss of the obligation.
//
// Issue() is a coordinator-tx (ADR-061): the draft→issued CAS AND the internal
// effect (balance credit / amount_due reduction) commit ATOMICALLY in one tx, so
// there is no "issued but un-credited" window. A failure before/during that commit
// leaves status='draft' AND issue_pending=true, which RetryPendingClawbackIssue
// re-issues on the next scheduler tick. The ONLY post-commit step is the EXTERNAL
// Stripe Tax reversal, which self-heals via RetryPendingCreditNoteTaxReversal (it
// sets tax_reversal_pending on failure); the ERROR below is the surfacing signal,
// not a manual-reconciliation requirement.
func (h *Handler) issueClawbackDrafts(ctx context.Context, tenantID string, detail *ProrationDetail) {
	if h.creditNotes == nil || detail == nil {
		return
	}
	for _, id := range detail.ClawbackDraftIDs {
		if _, err := h.creditNotes.Issue(ctx, tenantID, id); err != nil {
			slog.ErrorContext(ctx, "clawback credit note issue failed post-commit; draft persisted — RetryPendingClawbackIssue re-issues while status='draft' (Issue's internal effect is atomic per ADR-061; any external tax reversal self-heals via its own sweep)",
				"error", err,
				"tenant_id", tenantID,
				"credit_note_id", id,
				"reason", detail.ClawbackReason)
		}
	}
}

// handleItemProration generates the invoice or credit for a single item
// mutation. Dedup key is (tenant, subscription, item, change_type, change_at)
// — retries of the same mutation converge on the existing artifact via the
// proration dedup index.
func (h *Handler) handleItemProration(ctx context.Context, tenantID string, sub domain.Subscription, spec itemProrationSpec, tx *sql.Tx) (*ProrationDetail, error) {
	// Resolve plans needed for pricing and naming. The "effective" plan drives
	// currency and pricing — for a remove it's the old plan; for
	// anything else it's the new plan.
	var oldPlan, newPlan domain.Plan
	if spec.oldPlanID != "" {
		p, err := h.plans.GetPlan(ctx, tenantID, spec.oldPlanID)
		if err != nil {
			return nil, fmt.Errorf("get old plan: %w", err)
		}
		oldPlan = p
	}
	if spec.newPlanID != "" {
		p, err := h.plans.GetPlan(ctx, tenantID, spec.newPlanID)
		if err != nil {
			return nil, fmt.Errorf("get new plan: %w", err)
		}
		newPlan = p
	}

	effectivePlan := newPlan
	if spec.newPlanID == "" {
		effectivePlan = oldPlan
	}
	if effectivePlan.ID == "" {
		return nil, fmt.Errorf("proration spec resolved no plan; cannot price item mutation")
	}

	// Two rules shape immediate proration:
	//
	// (1) effective plan is in_arrears: the customer hasn't paid for
	//     the current period yet (in_arrears bills at cycle close).
	//     Emitting an immediate proration invoice + cycle-close full-
	//     period billing double-counts the new rate. Emitting an
	//     immediate credit gives "refund" against money never paid.
	//     Industry-aligned: Stripe `proration_behavior=none` for this
	//     case; Lago defers downgrades entirely. Velox defers the
	//     proration to cycle close, which bills SEGMENT-AWARE (FLOW B20,
	//     itemBaseSegments): each slice of the period is billed at the
	//     plan/qty actually in effect for it — accurate per-segment, no
	//     double-counting and no phantom credit.
	//
	// (2) effective plan is in_advance BUT an invoice that charged the
	//     period is unpaid: a credit must not become a refundable balance
	//     for money the customer never put in (Chargebee "Adjustment"
	//     credit case). The credit reduces what that invoice still asks
	//     for instead (creditUnusedTime), and a net charge against an
	//     unpaid base invoice is refused (ADR-050).
	//
	// Rule (1) is a silent defer (logged at info, no error): the item
	// change itself still applies, with no proration artifact.
	if effectivePlan.BaseBillTiming != domain.BillInAdvance {
		slog.InfoContext(ctx, "item proration deferred: in_arrears plan; cycle close bills segment-aware",
			"subscription_id", sub.ID,
			"item_id", spec.itemID,
			"change_type", spec.changeType,
			"effective_plan_id", effectivePlan.ID,
		)
		return nil, nil
	}
	// Integer day-ratio proration per ADR-042 — pure-integer banker's
	// rounding, no float64 in the cents path. See prorationCents.
	//
	// Computed first because the sign routes the change: a net credit
	// (downgrade / removal / qty-decrease) goes to creditUnusedTime, and only a
	// net charge (upgrade / add / qty-increase) reads the base invoice, whose
	// unpaid state blocks it (ADR-050).
	//
	// Denominator is the FULL billing cycle, NOT spec.totalDays (the current
	// period). On a stub/partial period (mid-cycle signup) spec.totalDays is
	// the short stub length, and dividing by it over-charges upgrades /
	// over-credits downgrades — the customer paid old × stub/fullCycle for the
	// period, so the delta for the remaining portion is (new−old) ×
	// remaining/fullCycle. This matches the engine's day-1 stub base fee and
	// cross-interval swap refund (both divide by fullCycleDays).
	oldAmount := oldPlan.BaseAmountCents * spec.oldQuantity
	newAmount := newPlan.BaseAmountCents * spec.newQuantity
	billingLoc, err := h.tenantLoc(ctx, sub.TenantID)
	if err != nil {
		return nil, err
	}
	denomDays := spec.totalDays
	if sub.CurrentBillingPeriodStart != nil {
		if fc := fullBillingCycleDays(*sub.CurrentBillingPeriodStart, effectivePlan.BillingInterval, billingLoc, sub.BillingAnchorDay); fc > 0 {
			denomDays = fc
		}
	}
	remainingDays := min(spec.remainingDays, denomDays)
	proratedCents := prorationCents(oldAmount, newAmount, remainingDays, denomDays)

	if proratedCents == 0 {
		return nil, nil
	}

	// Display-only factor for the ProrationDetail payload (operator-visible
	// event metadata). Source of truth for the cents math is the integer
	// remainingDays / fullCycle denominator above.
	var displayFactor float64
	if denomDays > 0 {
		displayFactor = float64(remainingDays) / float64(denomDays)
	}
	detail := &ProrationDetail{
		OldPlanID:       spec.oldPlanID,
		NewPlanID:       spec.newPlanID,
		ProrationFactor: displayFactor,
		AmountCents:     proratedCents,
	}

	memo := prorationMemo(spec, oldPlan, newPlan)

	// A net credit (downgrade / removal / qty-decrease) credits the invoices
	// that charged the unused time, whatever their payment state: one rule
	// for every change (ADR-118 amendment 2026-10-22).
	if proratedCents < 0 {
		return h.creditUnusedTime(ctx, tenantID, sub, spec, detail, -proratedCents, memo, tx)
	}

	// A net charge needs the period's base invoice: none yet (a fresh
	// subscription whose first prebill has not been generated) defers to cycle
	// close, and an UNPAID one blocks the change (ADR-050). A written-off base
	// is unpaid: it blocks the same way (ADR-050 amendment 2026-10-23). An
	// in-flight base is not unpaid: the charge goes ahead, and a later
	// decrease's credit is drafted and held until that charge settles
	// (ADR-059). A base with nothing left to pay (no amount due, tax done)
	// blocks nothing: an unpaid one in that state is settled as paid, and a
	// written-off one stays written off (ADR-113) but is owed nothing.
	if h.invoices != nil && sub.CurrentBillingPeriodStart != nil {
		// A single-row lookup is right here: it only routes paid vs unpaid.
		resolved, lookupErr := h.invoices.FindBaseInvoiceForPeriod(ctx, tenantID, sub.ID, *sub.CurrentBillingPeriodStart) //nolint:forbidigo // single-invoice-ok: paid/unpaid routing gate only
		if lookupErr != nil {
			slog.InfoContext(ctx, "item proration deferred: in_advance source invoice not found for current period",
				"subscription_id", sub.ID,
				"item_id", spec.itemID,
				"change_type", spec.changeType,
				"period_start", *sub.CurrentBillingPeriodStart,
				"error", lookupErr,
			)
			return nil, nil
		}
		owesNothing := resolved.AmountDueCents == 0 && resolved.TaxStatus == domain.InvoiceTaxOK
		if !resolved.PaymentInFlight() && resolved.PaymentStatus != domain.PaymentSucceeded && !owesNothing {
			return nil, unpaidSourceBlocksCharge(ctx, sub, spec, resolved)
		}
	}

	if proratedCents > 0 {
		// Honors ctx-bound effective-now (PR-12) so proration invoice
		// IssuedAt/DueAt land in sim-time on clock-pinned subs.
		now := clock.Now(ctx)
		// Tenant-configured Net terms, same resolution as the engine's
		// cycle/create invoices. Pre-fix this hardcoded 30, so a Net-15
		// tenant's proration invoice carried a different due date (and
		// dunning timing) than every sibling invoice.
		netDays := 30
		if h.netTerms != nil {
			d, err := h.netTerms.NetPaymentTermDays(ctx, tenantID)
			if err != nil {
				return nil, err
			}
			netDays = d
		}
		dueAt := domain.AddDaysIn(now, netDays, billingLoc)

		periodStart := spec.changeAt
		var periodEnd time.Time
		if sub.CurrentBillingPeriodEnd != nil {
			periodEnd = *sub.CurrentBillingPeriodEnd
		} else {
			periodEnd = spec.changeAt
		}

		// Line item quantity represents "what was billed" for this charge.
		// For plan changes it's the item quantity; for qty/add it's the
		// new quantity (effectively the delta billed).
		lineQty := spec.newQuantity
		if lineQty == 0 {
			lineQty = 1
		}

		var discountCents int64
		lineItem := domain.InvoiceLineItem{
			LineType:         domain.LineTypeBaseFee,
			Description:      memo,
			Quantity:         lineQty,
			UnitAmountCents:  proratedCents / max64(lineQty, 1),
			AmountCents:      proratedCents,
			TotalAmountCents: proratedCents,
			Currency:         effectivePlan.Currency,
		}
		lineItems := []domain.InvoiceLineItem{lineItem}

		taxResult := ProrationTaxResult{
			SubtotalCents: proratedCents,
			DiscountCents: discountCents,
		}
		if h.tax != nil {
			r, err := h.tax.ApplyTaxToLineItems(ctx, tenantID, sub.CustomerID, effectivePlan.Currency, proratedCents, discountCents, lineItems)
			if err != nil {
				// Transient tax-infra failure. Do NOT finalize with zero tax —
				// that ships an invoice lying about authoritative amounts.
				// Mark pending so InvoiceFinalizationStatus stamps Draft and the
				// tax retry worker re-runs calculation (same contract as the
				// engine's billOnePeriod / BillOnCreate paths).
				slog.WarnContext(ctx, "tax apply failed on proration, deferring invoice to draft for retry",
					"error", err, "subscription_id", sub.ID)
				taxResult.TaxStatus = domain.InvoiceTaxPending
				// Stamp the deferral facts the tax-retry reconciler keys on —
				// classified like the engine's deferral path (engine.go
				// ApplyTaxToLineItems). Without a retryable tax_error_code the
				// reconciler's filter skips the invoice forever.
				taxResult.TaxErrorCode = string(tax.Classify(err))
				reason := tax.CleanMessage(err.Error())
				if len(reason) > 500 {
					reason = reason[:500]
				}
				taxResult.TaxPendingReason = reason
				deferredAt := now
				taxResult.TaxDeferredAt = &deferredAt
			} else {
				taxResult = r
			}
		}
		netProrated := taxResult.SubtotalCents - taxResult.DiscountCents + taxResult.TaxAmountCents

		// ADR-048 Phase C: present a PLAN upgrade as the industry-standard
		// two-line shape (Stripe/Recurly/Chargebee/Orb all converge on it): a
		// NEGATIVE credit for the unused time on the OLD plan + a POSITIVE
		// charge for the remaining time on the NEW plan. A single "$18.00 plan
		// upgrade proration" line is opaque ("I upgraded to $50, why $18?"); the
		// credit-unused + charge-remaining pair shows the arithmetic.
		//
		// Tax was computed ONCE on the net above (the provider never sees a
		// negative line — Stripe Tax rejects negative amounts and manual clamps
		// them, which is why we split AFTER the tax call), so the invoice
		// subtotal/tax/total stay byte-identical to the single-line invoice. We
		// only partition the stored lines: charge = net − credit, chargeTax =
		// T − creditTax, so the two lines reconstruct the net exactly.
		//
		// Gated to: (1) a plan change OR a quantity increase — both have an
		// "unused old" slice to credit (old plan / old seat count). The
		// pre-fix quantity gate kept a single line whose Quantity was the NEW
		// total but whose Amount was the DELTA-only charge, so the derived
		// unit price was a fiction and qty × unit visibly disagreed with the
		// amount (3 × $33.33 ≠ $100.00 — integer truncation). Stripe stamps
		// the same credit/charge pair for quantity updates. Item ADD stays
		// single-line: oldAmount is 0, so there is nothing unused to credit
		// and the split would degenerate to a $0 line. And (2) the net not
		// being carved by inclusive-mode tax (taxResult.SubtotalCents ==
		// proratedCents) — in the rare inclusive case the carved net ≠
		// proratedCents, so we fall back to the single net line rather than
		// risk a subtotal/line mismatch.
		splitEligible := spec.changeType == domain.ItemChangeTypePlan ||
			spec.changeType == domain.ItemChangeTypeQuantity
		if splitEligible && taxResult.SubtotalCents == proratedCents {
			creditCents, chargeCents, creditTax, chargeTax := splitUpgradeProration(
				oldAmount, remainingDays, denomDays, proratedCents, taxResult.TaxAmountCents)

			// Use the tax-applied single line as the template so each split
			// line inherits the per-line tax DISPLAY fields (rate, jurisdiction,
			// code) the provider stamped; override the amounts, quantity,
			// label, and per-line tax with the partitioned values.
			tmpl := lineItems[0]
			oldQty := max64(spec.oldQuantity, 1)
			newQty := max64(spec.newQuantity, 1)

			// Labels: a plan change names the two plans; a quantity change
			// reuses one plan name, so the seat counts carry the contrast
			// (Stripe's "Unused time on 1 × Pro" shape). The boundary date
			// renders in the sub's billing timezone (ADR-077), not the host zone.
			labelLoc := billingLoc
			creditDesc := upgradeCreditLabel(oldPlan, spec.changeAt, labelLoc)
			chargeDesc := upgradeChargeLabel(newPlan, spec.changeAt, labelLoc)
			if spec.changeType == domain.ItemChangeTypeQuantity {
				creditDesc = qtyChangeCreditLabel(newPlan, oldQty, spec.changeAt, labelLoc)
				chargeDesc = qtyChangeChargeLabel(newPlan, newQty, spec.changeAt, labelLoc)
			}

			creditLine := tmpl
			creditLine.Description = creditDesc
			creditLine.Quantity = oldQty
			creditLine.AmountCents = creditCents
			creditLine.UnitAmountCents = creditCents / oldQty
			creditLine.TaxAmountCents = creditTax
			creditLine.TotalAmountCents = creditCents + creditTax

			chargeLine := tmpl
			chargeLine.Description = chargeDesc
			chargeLine.Quantity = newQty
			chargeLine.AmountCents = chargeCents
			chargeLine.UnitAmountCents = chargeCents / newQty
			chargeLine.TaxAmountCents = chargeTax
			chargeLine.TotalAmountCents = chargeCents + chargeTax

			lineItems = []domain.InvoiceLineItem{creditLine, chargeLine}
		}

		changeAt := spec.changeAt
		invoice := domain.Invoice{
			CustomerID:     sub.CustomerID,
			SubscriptionID: sub.ID,
			// Tax-deferred + pause-collection gate (matches
			// engine.billOnePeriod + BillOnCreate). Pre-fix the
			// proration invoice hardcoded Finalized regardless of
			// tax; if Stripe Tax returned customer_data_invalid the
			// invoice finalized with TaxAmountCents=0, lying about
			// authoritative amounts.
			Status:        domain.InvoiceFinalizationStatus(taxResult.TaxStatus, sub.PauseCollection, changeAt),
			PaymentStatus: domain.PaymentPending,
			Currency:      effectivePlan.Currency,
			SubtotalCents: taxResult.SubtotalCents,
			DiscountCents: taxResult.DiscountCents,
			// ONE struct assignment carries every tax fact — the 13
			// hand-copied lines this replaces produced three documented
			// field-drop bugs on this writer alone (see domain/tax_facts.go).
			TaxFacts: taxResult.TaxFacts,
			// Denormalized zone at issue (ADR-077) — every engine writer
			// stamps this; the proration writer was the one omission.
			BillingTimezone:    billingLoc.String(),
			TotalAmountCents:   netProrated,
			AmountDueCents:     netProrated,
			BillingPeriodStart: periodStart,
			BillingPeriodEnd:   periodEnd,
			IssuedAt:           &now,
			DueAt:              &dueAt,
			// CreatedAt on the same `now` so test-clock-driven plan
			// changes have created_at == issued_at on simulation time.
			CreatedAt: now,
			// Stamp the authoritative simulated flag from the sub's test
			// clock — matching every engine invoice path (billOnePeriod /
			// BillOnCreate / threshold). Without it a clock-pinned
			// plan-change proration invoice persisted is_simulated=false
			// despite its dates being on simulation time, so the dashboard
			// showed no "Simulated" marker (while the sibling cycle invoice
			// did). The frontend badge reads this field authoritatively and
			// deliberately does NOT infer simulation from a future date.
			IsSimulated:        sub.TestClockID != "",
			NetPaymentTermDays: netDays,
			// Stripe stamps subscription_update for every mid-period
			// change invoice (plan / quantity / item add). Pre-fix this
			// path was the only invoice writer that left the reason
			// NULL, so the dashboard couldn't label the trigger.
			BillingReason:            domain.BillingReasonSubscriptionUpdate,
			Memo:                     memo,
			SourcePlanChangedAt:      &changeAt,
			SourceSubscriptionItemID: spec.itemID,
			SourceChangeType:         spec.changeType,
		}

		// Tx-aware allocation + insert. When called inside the atomic
		// AddItem-with-proration flow (tx != nil), both operations share
		// the caller's tx so a rollback frees the invoice number and
		// rolls back the item add. When called via the legacy non-atomic
		// path (tx == nil), the underlying methods open their own txs.
		var (
			invoiceNumber string
			err           error
		)
		if tx != nil {
			invoiceNumber, err = h.invoices.NextInvoiceNumberTx(ctx, tx, tenantID)
		} else {
			invoiceNumber, err = h.invoices.NextInvoiceNumber(ctx, tenantID)
		}
		if err != nil {
			return nil, fmt.Errorf("allocate proration invoice number: %w", err)
		}
		invoice.InvoiceNumber = invoiceNumber

		// The finalize audit row commits with the invoice (ADR-090 shared
		// fate): on the caller's tx, or inside the store's own tx on the
		// non-atomic path. Its failure rolls the item change back.
		emit := h.finalizeAuditEmit(ctx, tenantID, sub.ID)
		var inv domain.Invoice
		if tx != nil {
			inv, err = h.invoices.CreateInvoiceWithLineItemsTx(ctx, tx, tenantID, invoice, lineItems)
			if err == nil && emit != nil {
				if aerr := emit(tx, inv); aerr != nil {
					return nil, fmt.Errorf("finalize audit for proration invoice: %w", aerr)
				}
			}
		} else {
			inv, err = h.invoices.CreateInvoiceWithLineItemsAudited(ctx, tenantID, invoice, lineItems, emit)
		}
		if err != nil {
			// Only the proration-dedup constraint maps to an idempotent
			// replay (look up the pre-existing row, return its id). Other
			// unique violations — billing-period collision, invoice-number
			// collision, etc. — are distinct bugs and must surface as
			// errors, not be silently squashed into "this exact proration
			// already ran." Pre-2026-05-28 the handler caught any
			// ErrAlreadyExists and tried the proration-source lookup, which
			// returned "not found" whenever the actual violation was on
			// `idx_invoices_billing_idempotency` (cycle-dedup) — confusing
			// "proration dedup lookup: not found" error masked the real
			// constraint that fired. Migration 0101 also exempts proration
			// invoices from the billing-idempotency index so the collision
			// no longer triggers there.
			if errs.Code(err) == "invoice_proration_source_taken" {
				// Proration-dedup replay — returns the prior invoice
				// for THIS exact (item, change_type, change_at) to avoid double-issuing.
				// An idempotency lookup, not a period-wide funding decision.
				existing, lookupErr := h.invoices.GetByProrationSource(ctx, tenantID, sub.ID, spec.itemID, spec.changeType, spec.changeAt) //nolint:forbidigo // single-invoice-ok: proration-dedup replay
				if lookupErr != nil {
					return nil, fmt.Errorf("proration dedup lookup: %w", lookupErr)
				}
				slog.InfoContext(ctx, "proration invoice already exists; retry dedup",
					"invoice_id", existing.ID,
					"subscription_id", sub.ID,
					"item_id", spec.itemID,
					"change_type", spec.changeType,
					"change_at", spec.changeAt,
				)
				detail.InvoiceID = existing.ID
				detail.Type = "invoice"
				detail.AmountCents = existing.TotalAmountCents
				return detail, nil
			}
			return nil, fmt.Errorf("create proration invoice: %w", err)
		}

		detail.InvoiceID = inv.ID
		detail.Type = "invoice"
		// GROSS, like the credit/adjustment branches: amount_cents is "the
		// money amount of the outcome artifact" in every proration type. It
		// previously carried the pre-tax net delta here, so the activity
		// timeline read "Proration invoice $18.00" beside an invoice whose
		// own page said $19.80 — two reference frames in one feed (adjudicated
		// in the FLOW B12 walk; credits always showed the gross).
		detail.AmountCents = inv.TotalAmountCents
		// Enroll the freshly-created charge invoice into the auto-charge sweep so
		// it actually collects (ADR — proration parity). Without this an upgrade /
		// add / qty-increase invoice finalizes but is never charged, because
		// (unlike engine cycle/create invoices) the subscription handler has no
		// finalize-time collection step. The sweep does the timeline-correct
		// charge — wall-clock cron for live subs, test-clock catchup on advance
		// for clock-pinned — so we deliberately do NOT charge inline here (that
		// would duplicate the charge path and wall-clock-charge a simulated
		// invoice). Set only on this fresh-creation path, never the dedup-replay
		// above (that invoice may already be paid). Issued post-commit like the
		// tax commit (a flag UPDATE on a not-yet-durable row would be lost).
		detail.AutoChargeInvoiceID = inv.ID
		// Carry the provider provenance so tax can be committed once the
		// invoice row is durable. On the non-atomic path (tx == nil) the
		// row is already committed by CreateInvoiceWithLineItems, so commit
		// here; on the atomic path (tx != nil) the row isn't durable until
		// the caller's tx.Commit, so the atomic wrapper commits afterward
		// (committing a Stripe Tax transaction for a row that might roll
		// back would orphan it). Mirrors the engine's post-finalize commit.
		detail.TaxProvider = inv.TaxProvider
		detail.TaxCalculationID = inv.TaxCalculationID
		// invoice.finalized goes out once the row is durable: here on the
		// non-atomic path, after the caller's commit on the atomic one. Only
		// this fresh creation sets it. The dedup replay above sends nothing:
		// the attempt that created the invoice sent it, unless the process
		// died between that commit and the send (#974, as for the engine).
		if inv.Status == domain.InvoiceFinalized {
			created := inv
			detail.finalizedInvoice = &created
		}
		if tx == nil {
			h.dispatchProrationFinalized(ctx, tenantID, detail)
			h.commitProrationTax(ctx, tenantID, detail)
			h.enrollAutoCharge(ctx, tenantID, detail)
		}

		slog.InfoContext(ctx, "proration invoice created",
			"invoice_id", inv.ID,
			"subscription_id", sub.ID,
			"item_id", spec.itemID,
			"change_type", spec.changeType,
			"amount_cents", proratedCents,
		)
	}

	return detail, nil
}

// unpaidSourceBlocksCharge is the ADR-050 block: a net charge (upgrade / add /
// qty-increase) against an in_advance period whose base invoice is finalized
// but NOT paid is refused. Velox never stacks a second receivable on an unpaid
// current-period invoice (Recurly blocks past-due changes; Stripe recommends
// proration_behavior=none on an unpaid latest invoice); the operator settles or
// voids the outstanding invoice first. errs.InvalidState rolls the item change
// back and the operator sees a clean 409.
func unpaidSourceBlocksCharge(ctx context.Context, sub domain.Subscription, spec itemProrationSpec, src domain.Invoice) error {
	slog.InfoContext(ctx, "item change blocked: net charge against an unpaid in_advance source invoice (ADR-050)",
		"subscription_id", sub.ID,
		"item_id", spec.itemID,
		"change_type", spec.changeType,
		"source_invoice_id", src.ID,
		"source_payment_status", src.PaymentStatus,
		"amount_due_cents", src.AmountDueCents,
	)
	return errs.InvalidState(fmt.Sprintf(
		"invoice %s for the current period is unpaid (%.2f %s outstanding); settle or void it before changing this subscription",
		src.InvoiceNumber, float64(src.AmountDueCents)/100, src.Currency,
	)).WithCode("unpaid_invoice_blocks_change")
}

// creditUnusedTime credits netCredit (net, tax excluded) of unused prepaid
// time for a downgrade, quantity decrease or item removal, by the one
// crediting rule (domain.AllocateUnusedCredit, ADR-118): the invoices that
// charged this item, newest first, each capped at its room. A paid invoice's
// share goes to the customer's balance; an unpaid one's reduces what it still
// asks for (ADR-050: never a refundable credit for money not paid); an
// in-flight one's is held by Issue until the charge settles (ADR-059) and
// counts as a balance credit, the usual outcome. Each
// share is a tax-reversing adjustment credit note on its own invoice (ADR-048).
//
// Read-only: it records the shares on the detail. The atomic caller writes
// them as drafts on its tx (createClawbackDraftsTx) and issues them after
// commit; the non-atomic path (tx == nil, tests and the legacy UpdateItem)
// issues inline. A returned error rolls the change back before it commits.
//
// What does not fit is not owed (every invoice is credited up to its total,
// or relieved of all it asks for), so the change goes ahead and records it on
// the detail (UncreditedCents → the audit row's uncredited_cents).
func (h *Handler) creditUnusedTime(ctx context.Context, tenantID string, sub domain.Subscription, spec itemProrationSpec, detail *ProrationDetail, netCredit int64, memo string, tx *sql.Tx) (*ProrationDetail, error) {
	if h.creditNotes == nil || h.invoices == nil {
		return nil, fmt.Errorf("credit for unused time: the credit-note issuer is not wired")
	}
	if sub.CurrentBillingPeriodStart == nil || sub.CurrentBillingPeriodEnd == nil {
		return nil, nil
	}
	funding, err := h.invoices.FindFundingInvoicesForPeriod(ctx, tenantID, sub.ID, *sub.CurrentBillingPeriodStart, *sub.CurrentBillingPeriodEnd)
	if errors.Is(err, errs.ErrNotFound) {
		// No in_advance invoice funded this period (a fresh subscription whose
		// first prebill has not been generated): nothing was charged, so
		// nothing is credited. Cycle close bills the period as it now stands.
		slog.InfoContext(ctx, "item proration: no in_advance invoice funded the current period; nothing to credit",
			"subscription_id", sub.ID, "item_id", spec.itemID, "change_type", spec.changeType)
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("credit for unused time: find funding invoices: %w", err)
	}
	// Headroom is read OFF the change's tx: it counts drafts, so an on-tx read
	// would let this change's own drafts shrink their room. The draft write
	// re-checks the cap under the invoice lock.
	credited := make(map[string]int64, len(funding))
	for _, f := range funding {
		c, cerr := h.creditNotes.CreditedCents(ctx, tenantID, f.ID)
		if cerr != nil {
			return nil, fmt.Errorf("credit for unused time: read creditable headroom for %s: %w", f.ID, cerr)
		}
		credited[f.ID] = c
	}
	shares, unplaced := domain.AllocateUnusedCredit(funding, credited, spec.itemID, netCredit)
	if src, pending := domain.TaxPendingShare(funding, shares); pending {
		return nil, errs.InvalidState(fmt.Sprintf(
			"funding invoice %s is a draft whose tax is still %s; retry once it is calculated", src.ID, src.TaxStatus)).
			WithCode("funding_invoice_tax_pending")
	}

	// "credit" when any share goes to the balance (paid, or a charge in
	// flight), "adjustment" when every share only reduces what an open invoice
	// asks for.
	detail.Type = "adjustment"
	var gross int64
	for _, sh := range shares {
		gross += sh.GrossCents
		if sh.ToBalance {
			detail.Type = "credit"
		}
	}
	if len(shares) == 0 {
		detail.Type = "credit" // nothing placed: the invoices had no room left
	}
	detail.AmountCents = gross
	detail.UncreditedCents = unplaced
	detail.ClawbackPieces = shares
	detail.ClawbackReason = clawbackReason(spec.changeType)
	detail.ClawbackMemo = memo
	if tx == nil {
		h.issueClawbackCreditNote(ctx, tenantID, detail)
	}
	slog.InfoContext(ctx, "item proration: unused time credited to the invoices that charged it",
		"subscription_id", sub.ID,
		"item_id", spec.itemID,
		"change_type", spec.changeType,
		"invoices", len(shares),
		"net_cents", netCredit,
		"gross_cents", gross,
		"uncredited_cents", unplaced,
	)
	return detail, nil
}

// prorationMemo picks a human-readable description per change type. Kept
// separate from handleItemProration so the math and the wording don't tangle.
func prorationMemo(spec itemProrationSpec, oldPlan, newPlan domain.Plan) string {
	switch spec.changeType {
	case domain.ItemChangeTypePlan:
		verb := "upgrade"
		if newPlan.BaseAmountCents < oldPlan.BaseAmountCents {
			verb = "downgrade"
		}
		return fmt.Sprintf("Plan %s proration: %s -> %s (qty %d)", verb, oldPlan.Name, newPlan.Name, spec.newQuantity)
	case domain.ItemChangeTypeQuantity:
		return fmt.Sprintf("Quantity change proration: %s (%d -> %d seats)", newPlan.Name, spec.oldQuantity, spec.newQuantity)
	case domain.ItemChangeTypeAdd:
		return fmt.Sprintf("Item add proration: %s (qty %d)", newPlan.Name, spec.newQuantity)
	case domain.ItemChangeTypeRemove:
		return fmt.Sprintf("Item remove proration: %s (qty %d)", oldPlan.Name, spec.oldQuantity)
	}
	return "Item change proration"
}

// upgradeCreditLabel / upgradeChargeLabel are the customer-facing line labels
// for the two-line PLAN upgrade proration (ADR-048 Phase C), matching the
// Stripe "Unused time on … / Remaining time on …" convention with the
// proration boundary date. The date is the change instant; finance/ops read
// these on the invoice, so no engineering jargon.
// The proration-boundary date in these labels renders in loc — the sub's
// billing timezone (ADR-077) — not the raw process zone, so the persisted,
// customer-facing invoice-line description doesn't shift a calendar day on a
// non-UTC deployment (ADR-075 audit).
func upgradeCreditLabel(oldPlan domain.Plan, at time.Time, loc *time.Location) string {
	return fmt.Sprintf("Unused time on %s (after %s)", oldPlan.Name, at.In(loc).Format("Jan 2, 2006"))
}

func upgradeChargeLabel(newPlan domain.Plan, at time.Time, loc *time.Location) string {
	return fmt.Sprintf("Remaining time on %s (after %s)", newPlan.Name, at.In(loc).Format("Jan 2, 2006"))
}

// qtyChangeCreditLabel / qtyChangeChargeLabel are the quantity-increase
// variants of the pair above. Both lines name the SAME plan, so the seat
// counts carry the contrast — Stripe's "Unused time on 1 × Pro" shape.
func qtyChangeCreditLabel(plan domain.Plan, oldQty int64, at time.Time, loc *time.Location) string {
	return fmt.Sprintf("Unused time on %d × %s (after %s)", oldQty, plan.Name, at.In(loc).Format("Jan 2, 2006"))
}

func qtyChangeChargeLabel(plan domain.Plan, newQty int64, at time.Time, loc *time.Location) string {
	return fmt.Sprintf("Remaining time on %d × %s (after %s)", newQty, plan.Name, at.In(loc).Format("Jan 2, 2006"))
}

// clawbackReason maps a downgrade change type to the credit-note `reason`
// (free-text, machine-categorical — same style as the engine's cancel/swap
// clawback reasons in ADR-048). Plan-downgrade, quantity-decrease, and
// item-remove each get a distinct reason; the human-readable detail lives in
// the CN description (the proration memo).
func clawbackReason(ct domain.ItemChangeType) string {
	switch ct {
	case domain.ItemChangeTypeRemove:
		return "subscription_item_removed"
	case domain.ItemChangeTypeQuantity:
		return "subscription_quantity_decrease"
	default:
		return "subscription_downgrade"
	}
}

// remainingPeriodRatio returns the integer day counts that drive proration
// math. Per ADR-042 (2026-05-31) Velox switched from float64 prorationFactor
// to integer day-ratio (`amount * remaining / total`) matching engine.go's
// segment-aware billing pattern and Stripe / Lago / Orb proration semantics.
// Pre-fix: prorationFactor was float64 hours/24 — introduced ULP error on
// large amounts and was non-deterministic across architectures.
//
// Returns (0, 0) when proration doesn't apply (period boundaries missing,
// already past end of period, or period of zero length). When `remaining`
// exceeds `total` (clock-skew edge case), both clamp to `total` for full-
// period proration (factor 1).
func remainingPeriodRatio(sub domain.Subscription, now time.Time) (remainingDays, totalDays int64) {
	if sub.CurrentBillingPeriodStart == nil || sub.CurrentBillingPeriodEnd == nil {
		return 0, 0
	}
	total := int64(math.Round(sub.CurrentBillingPeriodEnd.Sub(*sub.CurrentBillingPeriodStart).Hours() / 24))
	remaining := int64(math.Round(sub.CurrentBillingPeriodEnd.Sub(now).Hours() / 24))
	if total <= 0 || remaining <= 0 {
		return 0, 0
	}
	if remaining > total {
		return total, total
	}
	return remaining, total
}

func planIDsFromItems(items []domain.SubscriptionItem) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.PlanID)
	}
	return out
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// --- Activity timeline (T0-18) ---
//
// Industry-standard subscription detail view (Stripe, Lago, Orb) shows
// a chronological feed of everything that happened to a subscription so
// a CS rep responding to "why was my sub cancelled?" lands on the right
// page with the right story already written. We source it from the
// audit_log — every lifecycle mutation (create/activate/pause/resume/
// cancel, item add/remove/update) is already captured there with the
// actor, timestamp, and a metadata blob. No new instrumentation needed,
// and no RLS gap since audit.Logger.Query already runs under the
// tenant-scoped tx.

// timelineEvent is the wire shape the SPA's timeline component consumes.
// Kept structurally compatible with the invoice payment-timeline event
// so the React renderer can handle both without branching logic.
type timelineEvent struct {
	Timestamp   string `json:"timestamp"`
	Source      string `json:"source"`
	EventType   string `json:"event_type"`
	Status      string `json:"status"`
	Description string `json:"description"`
	// sortAt / recordedAt are the ORDERING axes, never serialized.
	// sortAt is the instant the surface DISPLAYS (sim_effective_at on
	// clock-pinned rows, wall created_at otherwise — the ADR-030
	// narrative-surface rule); recordedAt is the full-precision wall
	// write instant used as the causal tie-break. Sorting on the
	// serialized Timestamp string was the 2026-07-19 inversion bug:
	// second-truncated wall stamps collided (a pause PUT and the
	// catchup resume 36ms later), the tie kept the audit query's DESC
	// orientation, and "auto-resumed (sim Nov 10)" rendered above
	// "paused (sim Nov 2)".
	sortAt     time.Time
	recordedAt time.Time
	// Detail renders as a sub-line beneath the description (mirrors
	// invoice timeline's same-named field). Used for the
	// human-meaningful context that doesn't belong in the main line:
	// "At end of current period", "New trial end: …", "Amount ≥ …",
	// "Plan: … → …", "After next cycle close", etc.
	Detail string `json:"detail,omitempty"`
	// DetailTimestamp ships the RFC3339 timestamp that the detail
	// prefix refers to (e.g. "Auto-resumes" → resumes_at). When set,
	// the frontend formats it in tenant TZ via formatDateTime so the
	// sub-line stays consistent with the main row timestamp. Pre-fix
	// the backend formatted the timestamp inline in UTC, producing
	// rows that mixed the operator's TZ (main row) with UTC (sub-line)
	// — confusing on first read.
	DetailTimestamp string `json:"detail_timestamp,omitempty"`
	ActorType       string `json:"actor_type,omitempty"`
	ActorName       string `json:"actor_name,omitempty"`
	ActorID         string `json:"actor_id,omitempty"`
	// IsSimulated marks events whose effect landed on the simulated
	// timeline. After ADR-030's 2026-05-28 amendment, audit_log
	// created_at is wall-clock for every row — the timestamp this
	// event renders with IS wall-clock. The is_simulated flag now
	// indicates a separate fact: the operator action affected a
	// clock-pinned entity, and its simulated effect-time is in
	// SimEffectiveAt below. Authoritative per row (driven off
	// metadata.sim_effective_at), not a sub-level heuristic — wall-
	// clock-active subs and one-off operator actions on a clock-
	// pinned sub both report false / true correctly.
	IsSimulated bool `json:"is_simulated,omitempty"`
	// SimEffectiveAt is the simulated effect time of an operator
	// action on a clock-pinned entity — what the test clock's
	// frozen_time was when the action landed on the simulated
	// timeline. Empty for events on wall-clock entities. SPA renders
	// it as a subline ("Effect on test clock X at <simulated time>")
	// under the wall-clock primary timestamp.
	SimEffectiveAt string `json:"sim_effective_at,omitempty"`
	// TestClockID identifies which test clock the SimEffectiveAt
	// belongs to. Empty when SimEffectiveAt is empty.
	TestClockID string `json:"test_clock_id,omitempty"`
	// Error is the line's problem: what went wrong on a failure row, what it
	// means on a warning row.
	Error string `json:"error,omitempty"`
	// Line is the row as the activity line standard words it (ADR-128):
	// keyed by its audit row, with the actor from that row. Description,
	// Detail, DetailTimestamp, Error and Status above are filled from it
	// (fillFromLine), so a client that reads only the older fields shows the
	// same words.
	Line activity.Line `json:"line"`
}

// fillFromLine stamps the line's id, actor and times and copies its words
// into the older fields: the title is the description, the meta line
// without the actor is the detail (the card shows the actor from
// actor_name), the problem is the error and the tone is the status.
func (e *timelineEvent) fillFromLine(id string, actor *activity.Actor) {
	e.Line.ID = id
	e.Line.Actor = actor
	e.Line = activity.Stamp(e.Line, e.sortAt, e.recordedAt, e.IsSimulated)
	e.Description = e.Line.Title
	e.Detail, e.DetailTimestamp = legacyDetail(e.Line)
	e.Error = e.Line.Problem
	e.Status = legacyStatus(e.Line.Tone)
}

// planLabel renders an operator-facing plan reference: prefer the
// human-readable plan name from planNames, fall back to "Plan {id}"
// only when the lookup is empty (deleted plan, lookup failure, or
// the description is generated outside the timeline handler where
// the map isn't populated). Industry standard — Stripe / Lago /
// Chargebee / Orb all show plan names in activity feeds, never raw
// "plan_xxx" tokens.
func planLabel(planID string, planNames map[string]string) string {
	if name, ok := planNames[planID]; ok && name != "" {
		return name
	}
	return planID
}

// addProrationMeta stamps the proration OUTCOME (the invoice/credit/
// adjustment a mid-period change produced) into a subscription audit-row
// payload, so the activity timeline can show "Plan changed · … · Proration
// invoice $50.00" instead of just the intent. Single source for the
// add / update / remove audit writers so the metadata shape can't drift.
// No-op when detail is nil (no proration — e.g. zero remaining days).
func addProrationMeta(payload map[string]any, detail *ProrationDetail, currency string) {
	if detail == nil {
		return
	}
	payload["proration_type"] = detail.Type
	payload["proration_amount_cents"] = detail.AmountCents
	if detail.InvoiceID != "" {
		payload["proration_invoice_id"] = detail.InvoiceID
	}
	// The part that reduced open invoices rather than reaching the balance.
	// The activity line links the invoice when there is one (ADR-128 rule 8),
	// and a credit that went both ways shows both parts.
	var adjustedCents int64
	var adjusted []string
	for _, p := range detail.ClawbackPieces {
		if !p.ToBalance {
			adjustedCents += p.GrossCents
			adjusted = append(adjusted, p.InvoiceID)
		}
	}
	switch {
	case len(adjusted) == 1:
		payload["adjusted_invoice_id"] = adjusted[0]
	case len(adjusted) > 1:
		payload["adjusted_invoice_count"] = len(adjusted)
	}
	if detail.Type == "credit" && adjustedCents > 0 {
		payload["adjusted_cents"] = adjustedCents
	}
	// Unused time the invoices had no room left for: not owed, recorded so the
	// line can say the credit was limited (ADR-118 amendment 2026-10-22).
	if detail.UncreditedCents > 0 {
		payload["uncredited_cents"] = detail.UncreditedCents
	}
	if currency != "" {
		payload["currency"] = currency
	}
}

// planCurrency is a best-effort plan-currency lookup for audit metadata —
// empty string on any miss (unwired reader, deleted plan), which
// addProrationMeta treats as "omit currency".
func (h *Handler) planCurrency(ctx context.Context, tenantID, planID string) string {
	if h.plans == nil || planID == "" {
		return ""
	}
	if pl, err := h.plans.GetPlan(ctx, tenantID, planID); err == nil {
		return pl.Currency
	}
	return ""
}

// plural returns "s" for n != 1, else "". Tiny helper to avoid the
// "1 thresholds" / "2 threshold" pluralization mistake.
func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// InvoiceLabeler names an invoice a subscription activity row links to: a
// proration invoice, the open invoice an adjustment reduced, or a spend
// threshold's early invoice (ADR-128 rule 8). An error (the invoice no longer
// exists, or could not be read) leaves the row's invoice as plain text.
type InvoiceLabeler interface {
	InvoiceNumber(ctx context.Context, tenantID, invoiceID string) (string, error)
}

// SetInvoiceLabeler wires the invoice numbers the timeline's links show.
// Unset, rows name no invoice by link.
func (h *Handler) SetInvoiceLabeler(l InvoiceLabeler) { h.invoiceLabels = l }

// invoiceNumbers looks up the number of every invoice the entries name, once
// each. An invoice that cannot be read is left out, so its row links nothing.
func (h *Handler) invoiceNumbers(ctx context.Context, tenantID string, entries []domain.AuditEntry) map[string]string {
	out := map[string]string{}
	if h.invoiceLabels == nil {
		return out
	}
	tried := map[string]bool{}
	for _, e := range entries {
		for _, key := range []string{"proration_invoice_id", "adjusted_invoice_id", "invoice_id"} {
			id := metaString(e.Metadata, key)
			if id == "" || tried[id] {
				continue
			}
			tried[id] = true
			if num, err := h.invoiceLabels.InvoiceNumber(ctx, tenantID, id); err == nil && num != "" {
				out[id] = num
			}
		}
	}
	return out
}

func (h *Handler) activityTimeline(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	id := chi.URLParam(r, "id")

	// Verify the subscription exists + belongs to this tenant before
	// leaking a 200 with empty events — otherwise a bad id returns the
	// same shape as a real sub that just has no audit yet.
	if _, err := h.svc.Get(r.Context(), tenantID, id); err != nil {
		if errors.Is(err, errs.ErrNotFound) {
			respond.NotFound(w, r, "subscription")
			return
		}
		respond.InternalError(w, r)
		return
	}
	// audit_log.created_at is WALL-CLOCK on every row, including clock-pinned subs
	// (ADR-030: it answers "when did the operator click"). It is NOT sim-time — an
	// earlier version of this comment said it was. The SIMULATED instant is the
	// SECOND axis (sim_effective_at), which is what the per-row values below read.
	// Marking every audit-sourced row
	// IsSimulated + SimEffectiveAt are computed PER ROW from
	// metadata.sim_effective_at — the authoritative signal mirrored by
	// the audit writer from the ctx clock binding (ADR-090 §5) at write
	// time. Pre-2026-05-28 this was a sub-
	// level heuristic (every audit row flagged simulated when the
	// sub was clock-pinned), which fired the chip on operator
	// actions whose audit timestamp was actually wall-clock — the
	// 2026-05-28 ADR-030 amendment made audit created_at wall-
	// clock everywhere, so the heuristic became actively wrong.
	// Per-row metadata check below.

	events := []timelineEvent{}
	truncated := false

	if h.auditLogger != nil {
		// Pull a generous slice of audit entries for this sub — the UI
		// shows the most recent first anyway, and subs rarely have more
		// than a few dozen mutations over their lifetime. 100 is the
		// store's hard clamp (audit.Logger.Query). The offset path's total
		// count tells us whether the sub outgrew it; the response carries
		// an explicit `truncated` flag so >100-row histories can't
		// silently start mid-life (the DESC query keeps the NEWEST rows,
		// so it's the earliest events — create/activate — that drop).
		//
		// The filter is deliberately pinned to this one subscription
		// (ResourceType+ResourceID): the timeline is an object-scoped
		// activity view under PermSubscriptionRead, NOT a general audit
		// reader — cross-resource querying stays behind the audit-log
		// route's stricter permission. TestActivityTimeline_* pin both
		// this scope and the error/truncation contracts.
		entries, total, err := h.auditLogger.Query(r.Context(), tenantID, audit.QueryFilter{
			ResourceType: "subscription",
			ResourceID:   id,
			Limit:        100,
		})
		if err == nil {
			truncated = total > len(entries)
			// Plan-name lookup so the timeline shows "Pro Monthly" instead
			// of "vlx_pln_d83g2obmajdtlif0mk00". Collect every plan_id
			// referenced in metadata (plan_id, old_plan_id, new_plan_id)
			// across all entries, batch-fetch via the wired PlanReader,
			// hand the map to describeSubscriptionAction. Missing plans
			// (deleted, RLS gap, unwired reader) fall back to the raw
			// ID per planLabel's contract — operators still see
			// something rather than a blank.
			planNames := map[string]string{}
			if h.plans != nil {
				wanted := map[string]struct{}{}
				for _, e := range entries {
					for _, key := range []string{"plan_id", "old_plan_id", "new_plan_id"} {
						if v, ok := e.Metadata[key].(string); ok && v != "" {
							wanted[v] = struct{}{}
						}
					}
					if arr, ok := e.Metadata["plan_ids"].([]any); ok {
						for _, v := range arr {
							if s, ok := v.(string); ok && s != "" {
								wanted[s] = struct{}{}
							}
						}
					}
				}
				for pid := range wanted {
					if p, err := h.plans.GetPlan(r.Context(), tenantID, pid); err == nil {
						planNames[pid] = p.Name
					}
				}
			}
			labels := lineLabels{plans: planNames, invoices: h.invoiceNumbers(r.Context(), tenantID, entries)}
			for _, e := range entries {
				line := describeSubscriptionAction(e.Action, e.Metadata, labels)
				// Per-row sim context. metadata.sim_effective_at is
				// mirrored by the audit writer from the ctx clock binding
				// on writes affecting clock-pinned subs (ADR-090 §5; the
				// columns are the queryable copy). Empty for everything else.
				simAt, _ := e.Metadata["sim_effective_at"].(string)
				clockID, _ := e.Metadata["test_clock_id"].(string)
				// The display/sort instant: the simulated effect time on
				// clock-pinned rows, wall write-time otherwise. Parse
				// failure (malformed metadata) falls back to wall rather
				// than sorting a garbage zero-time to the top.
				sortAt := e.CreatedAt
				if simAt != "" {
					if t, perr := time.Parse(time.RFC3339, simAt); perr == nil {
						sortAt = t
					}
				}
				ev := timelineEvent{
					Timestamp:      e.CreatedAt.UTC().Format(time.RFC3339),
					Source:         "audit",
					EventType:      e.Action,
					ActorType:      e.ActorType,
					ActorName:      e.ActorName,
					ActorID:        e.ActorID,
					IsSimulated:    simAt != "",
					SimEffectiveAt: simAt,
					TestClockID:    clockID,
					sortAt:         sortAt,
					recordedAt:     e.CreatedAt,
					Line:           line,
				}
				ev.fillFromLine(e.ID, activity.ActorFromAudit(e.ActorType, e.ActorName))
				events = append(events, ev)
			}
		} else {
			// Audit rows are this endpoint's ONLY event source — an empty
			// 200 on query failure was indistinguishable from "no history",
			// on a surface whose purpose is forensic reconstruction. Fail
			// loud instead.
			slog.ErrorContext(r.Context(), "subscription timeline: audit query",
				"subscription_id", id, "error", err)
			respond.InternalError(w, r)
			return
		}
	}

	// Ascending order — CS reps read a timeline top-down, earliest first.
	// The sort axis is the axis the surface DISPLAYS (ADR-030 narrative
	// amendment): the simulated instant on clock-pinned rows, wall
	// otherwise. Sorting the serialized wall Timestamp string under a
	// sim-primary display inverted causally-ordered pairs whose
	// second-truncated wall stamps collided (2026-07-19 live find:
	// "auto-resumed, sim Nov 10" rendered above "paused, sim Nov 2" —
	// both wall-stamped 19:06:37, tie kept the query's DESC orientation).
	//
	// Equal display instants tie-break on full-precision wall recordedAt
	// (causal write order); events sharing BOTH are pre-reversed below so
	// the stable sort preserves true insertion order, not the audit
	// query's DESC.
	slices.Reverse(events)
	timeline.SortStable(events,
		func(e timelineEvent) time.Time { return e.sortAt },
		func(a, b timelineEvent) bool { return a.recordedAt.Before(b.recordedAt) })

	respond.JSON(w, r, http.StatusOK, map[string]any{
		"events": events,
		// True when the sub has more audit rows than the 100 fetched —
		// the timeline then starts mid-history (earliest events dropped).
		"truncated": truncated,
	})
}
