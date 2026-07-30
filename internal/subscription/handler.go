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
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/sagarsuperuser/velox/internal/api/respond"
	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/money"
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
	CreateInvoiceWithLineItems(ctx context.Context, tenantID string, inv domain.Invoice, items []domain.InvoiceLineItem) (domain.Invoice, error)
	GetByProrationSource(ctx context.Context, tenantID, subscriptionID, subscriptionItemID string, changeType domain.ItemChangeType, changeAt time.Time) (domain.Invoice, error)
	NextInvoiceNumber(ctx context.Context, tenantID string) (string, error)
	// FindBaseInvoiceForPeriod gates immediate in_advance proration on
	// whether the source invoice was actually paid. Mirrors the engine's
	// BillOnCancel paid-check; same industry rationale (Chargebee
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

// ProrationCreditGranter grants credits for downgrade proration. Dedup key is
// (subscription, item, plan_changed_at) — see ProrationInvoiceCreator comment.
type ProrationCreditGranter interface {
	GrantProration(ctx context.Context, tenantID string, input ProrationGrantInput) error
	GetByProrationSource(ctx context.Context, tenantID, subscriptionID, subscriptionItemID string, changeType domain.ItemChangeType, changeAt time.Time) (domain.CreditLedgerEntry, error)
	// Tx variant — append the credit-ledger entry inside a caller-owned
	// transaction. Same atomicity story as the invoice creator's Tx
	// methods above.
	GrantProrationTx(ctx context.Context, tx *sql.Tx, tenantID string, input ProrationGrantInput) error
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

// CreditNoteIssuer issues a tax-reversing adjustment credit note against a
// PAID source invoice for a downgrade clawback (ADR-048). On a paid invoice the
// credit-note primitive credits the GROSS the customer paid for the unused
// slice (net + the proportional tax) to their balance AND reverses the
// proportional output tax against the invoice's committed tax transaction —
// neither of which the bare net ledger grant did. *creditnote.Service
// satisfies it directly (same method as billing.CreditNoteAdjuster).
//
// Optional: when unwired (narrow unit tests; production always wires it via
// SetCreditNoteIssuer) or when no paid source invoice was resolved, the
// downgrade path falls back to the legacy net ledger grant.
type CreditNoteIssuer interface {
	CreateAndIssueAdjustment(ctx context.Context, tenantID, invoiceID string, grossCents int64, reason, description string) (domain.CreditNote, error)
	// CreateAdjustmentDraftTx creates the clawback adjustment credit note as a
	// DRAFT inside the caller's tx (coordinator-owned, ADR-056) so it commits
	// atomically with the item change; the caller issues it post-commit via
	// Issue, and RetryPendingClawbackIssue re-issues it if that fails.
	CreateAdjustmentDraftTx(ctx context.Context, tx *sql.Tx, tenantID, invoiceID string, grossCents int64, reason, description string) (domain.CreditNote, error)
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
// offset-TZ tenants, which production always wires.
type TenantLocator interface {
	TenantLocation(ctx context.Context, tenantID string) *time.Location
}

// NetTermsReader resolves the tenant's configured Net payment terms so the
// proration invoice stamps the same terms + due date the engine's
// cycle/create invoices do. *billing.Engine satisfies it. Optional: when
// unwired (narrow tests) the proration path falls back to Net 30 — the
// pre-wiring hardcode, kept as the fallback only.
type NetTermsReader interface {
	NetPaymentTermDays(ctx context.Context, tenantID string) int
}

// ProrationGrantInput carries the downgrade/removal/reduction credit payload
// plus the provenance fields required for dedup. SourceChangeType
// distinguishes plan-downgrade from qty-reduction from item-remove when the
// same item is mutated multiple ways within the same billing period.
type ProrationGrantInput struct {
	CustomerID               string
	AmountCents              int64
	Description              string
	SourceSubscriptionID     string
	SourceSubscriptionItemID string
	SourcePlanChangedAt      time.Time
	SourceChangeType         domain.ItemChangeType
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
	credits     ProrationCreditGranter
	creditNotes CreditNoteIssuer
	tzLocator   TenantLocator
	netTerms    NetTermsReader
	tax         ProrationTaxApplier
	events      domain.EventDispatcher
	auditLogger auditRecorder
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
}

func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// SetAuditLogger configures audit logging for financial operations.
func (h *Handler) SetAuditLogger(l auditRecorder) { h.auditLogger = l }

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
// remainingPeriodFactor / changeAt stamps land in simulated time on
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

// SetProrationDeps sets optional dependencies for proration invoice generation.
func (h *Handler) SetProrationDeps(plans PlanReader, invoices ProrationInvoiceCreator, credits ProrationCreditGranter) {
	h.plans = plans
	h.invoices = invoices
	h.credits = credits
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
func (h *Handler) tenantLoc(ctx context.Context, tenantID string) *time.Location {
	if h.tzLocator == nil {
		return time.UTC
	}
	return h.tzLocator.TenantLocation(ctx, tenantID)
}

// stampPeriodDisplay populates the computed CurrentBillingPeriodDisplay — the
// inclusive human period range ("Jun 1 – Jun 30"), anchored in the org's
// billing timezone (tenantLoc, ADR-077) so the displayed range matches the
// invoice and renders in the one timezone the org bills in. No-op when the sub
// has no current period. Called for every subscription response via respondSub.
func (h *Handler) stampPeriodDisplay(ctx context.Context, sub *domain.Subscription) {
	if sub.CurrentBillingPeriodStart != nil && sub.CurrentBillingPeriodEnd != nil {
		sub.CurrentBillingPeriodDisplay = domain.FormatInclusivePeriod(
			*sub.CurrentBillingPeriodStart, *sub.CurrentBillingPeriodEnd, h.tenantLoc(ctx, sub.TenantID))
	}
}

// respondSub writes a single subscription as JSON, first stamping the computed
// period-display field. Every handler that returns a subscription goes through
// here so the display can't be forgotten on a new path.
func (h *Handler) respondSub(w http.ResponseWriter, r *http.Request, status int, sub domain.Subscription) {
	h.stampPeriodDisplay(r.Context(), &sub)
	respond.JSON(w, r, status, sub)
}

// SetCreditNoteIssuer wires the tax-reversing credit-note primitive used by the
// downgrade clawback path (ADR-048). When unset, downgrade credits fall back to
// the legacy net ledger grant (no tax reversal). Implemented by
// *creditnote.Service; wired from router.go.
func (h *Handler) SetCreditNoteIssuer(i CreditNoteIssuer) {
	h.creditNotes = i
}

// fireEvent dispatches a subscription lifecycle event. Synchronous by design:
// with the webhook_outbox in place (RES-1), Dispatch is a short DB insert that
// must persist-before-return so a crash between the handler's respond.JSON and
// event emission can't silently lose the event. Logging an error beats
// dropping.
func (h *Handler) fireEvent(ctx context.Context, tenantID, eventType string, sub domain.Subscription, extra map[string]any) {
	if h.events == nil {
		return
	}
	payload := map[string]any{
		"subscription_id": sub.ID,
		"customer_id":     sub.CustomerID,
		"status":          string(sub.Status),
		"item_count":      len(sub.Items),
	}
	if sub.CurrentBillingPeriodStart != nil {
		payload["current_period_start"] = sub.CurrentBillingPeriodStart.UTC()
	}
	if sub.CurrentBillingPeriodEnd != nil {
		payload["current_period_end"] = sub.CurrentBillingPeriodEnd.UTC()
	}
	for k, v := range extra {
		payload[k] = v
	}
	if err := h.events.Dispatch(ctx, tenantID, eventType, payload); err != nil {
		slog.ErrorContext(ctx, "dispatch subscription event",
			"event_type", eventType,
			"subscription_id", sub.ID,
			"tenant_id", tenantID,
			"error", err,
		)
	}
}

// itemPayload projects a SubscriptionItem into an event payload. Stable keys
// — consumers depend on the shape, so we don't echo the full domain struct.
func itemPayload(item domain.SubscriptionItem) map[string]any {
	p := map[string]any{
		"item_id":  item.ID,
		"plan_id":  item.PlanID,
		"quantity": item.Quantity,
	}
	if item.PendingPlanID != "" {
		p["pending_plan_id"] = item.PendingPlanID
	}
	if item.PendingPlanEffectiveAt != nil {
		p["pending_plan_effective_at"] = item.PendingPlanEffectiveAt.UTC()
	}
	return p
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

	// Billing thresholds — Stripe-parity hard-cap config. PUT writes the full
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

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())

	var input CreateInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respond.BadRequest(w, r, "invalid JSON body")
		return
	}

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

	subs, total, err := h.svc.List(r.Context(), ListFilter{
		TenantID:   tenantID,
		CustomerID: r.URL.Query().Get("customer_id"),
		PlanID:     r.URL.Query().Get("plan_id"),
		Status:     r.URL.Query().Get("status"),
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
	// Service.Cancel emission) — same row content, plus dunning-driven cancels
	// record identically instead of being invisible. The fallback-path proration
	// credit lands post-commit as its own audited grant row rather than a field
	// here.
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
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respond.BadRequest(w, r, "invalid JSON body")
		return
	}

	sub, err := h.svc.ScheduleCancel(r.Context(), tenantID, id, input)
	if err != nil {
		respond.FromError(w, r, err, "subscription")
		return
	}

	if h.auditLogger != nil {
		meta := map[string]any{
			"action":               "cancel_scheduled",
			"customer_id":          sub.CustomerID,
			"cancel_at_period_end": sub.CancelAtPeriodEnd,
		}
		if sub.CancelAt != nil {
			meta["cancel_at"] = sub.CancelAt.UTC()
		}
		_ = h.auditLogger.Log(h.auditCtx(r.Context(), tenantID, sub.ID), tenantID, domain.AuditActionUpdate, "subscription", sub.ID, sub.Code, meta)
	}

	extra := map[string]any{"cancel_at_period_end": sub.CancelAtPeriodEnd}
	if sub.CancelAt != nil {
		extra["cancel_at"] = sub.CancelAt.UTC()
	}
	// ADR-069: the authoritative "when does it actually cancel" — consumers
	// must not re-derive it from the status-polymorphic flag.
	if sub.CancelEffectiveAt != nil {
		extra["cancel_effective_at"] = sub.CancelEffectiveAt.UTC()
	}
	h.fireEvent(r.Context(), tenantID, domain.EventSubscriptionCancelScheduled, sub, extra)

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
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respond.BadRequest(w, r, "invalid JSON body")
		return
	}

	// triggered_by tag distinguishes operator-driven pauses from
	// dunning-driven pauses in the Activity timeline. The audit write
	// itself now happens inside Service.PauseCollection so the dunning
	// adapter path (which bypasses this handler) also produces an entry.
	input.TriggeredBy = "operator"

	sub, err := h.svc.PauseCollection(r.Context(), tenantID, id, input)
	if err != nil {
		respond.FromError(w, r, err, "subscription")
		return
	}

	extra := map[string]any{"behavior": string(input.Behavior)}
	if sub.PauseCollection != nil && sub.PauseCollection.ResumesAt != nil {
		extra["resumes_at"] = sub.PauseCollection.ResumesAt.UTC()
	}
	h.fireEvent(r.Context(), tenantID, domain.EventSubscriptionCollectionPaused, sub, extra)

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
			"action":      "trial_ended",
			"customer_id": sub.CustomerID,
		})
	}

	// subscription.trial_ended (triggered_by=operator) is enqueued IN the
	// end-trial tx (store-level DispatchTx subset) — do not also fire it here.

	h.respondSub(w, r, http.StatusOK, sub)
}

// extendTrial pushes a trialing subscription's trial_end_at later. Body:
// {trial_end:<RFC3339>}. Returns 422 if the new value is in the past or
// not strictly after the current trial_end_at, or the sub is not in
// 'trialing'. Fires subscription.trial_extended with
// triggered_by="operator".
func (h *Handler) extendTrial(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	id := chi.URLParam(r, "id")

	var body struct {
		TrialEnd time.Time `json:"trial_end"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		respond.Error(w, r, http.StatusBadRequest, "invalid_body", "invalid request body", "")
		return
	}
	if body.TrialEnd.IsZero() {
		respond.Error(w, r, http.StatusUnprocessableEntity, "validation_error", "trial_end is required", "trial_end")
		return
	}

	sub, err := h.svc.ExtendTrial(r.Context(), tenantID, id, body.TrialEnd)
	if err != nil {
		respond.FromError(w, r, err, "subscription")
		return
	}

	if h.auditLogger != nil {
		_ = h.auditLogger.Log(h.auditCtx(r.Context(), tenantID, sub.ID), tenantID, domain.AuditActionUpdate, "subscription", sub.ID, sub.Code, map[string]any{
			"action":      "trial_extended",
			"customer_id": sub.CustomerID,
			"trial_end":   body.TrialEnd.UTC(),
		})
	}

	h.fireEvent(r.Context(), tenantID, domain.EventSubscriptionTrialExtended, sub, map[string]any{
		"triggered_by": "operator",
		"trial_end":    body.TrialEnd.UTC(),
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

	h.fireEvent(r.Context(), tenantID, domain.EventSubscriptionCollectionResumed, sub, nil)

	h.respondSub(w, r, http.StatusOK, sub)
}

// setBillingThresholds writes the Stripe-parity hard-cap config onto a
// subscription. Body shape:
//
//	{
//	  "amount_gte": 50000,                    // optional, integer cents
//	  "reset_billing_cycle": true,            // optional, defaults FALSE (Stripe keep-anchor; see BillingThresholdsInput)
//	  "item_thresholds": [                    // optional, always-array
//	    {"subscription_item_id": "si_xxx", "usage_gte": "1000"}
//	  ]
//	}
//
// At least one of amount_gte or item_thresholds must be supplied. Returns
// 422 on validation failure (terminal sub, unknown item id, negative
// usage_gte, multi-currency item set, etc).
//
// Replaces the full set on every call: the per-item rows for any item not
// in the new slice are deleted by the store.
func (h *Handler) setBillingThresholds(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	id := chi.URLParam(r, "id")

	var input BillingThresholdsInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respond.BadRequest(w, r, "invalid JSON body")
		return
	}

	// Multi-currency check happens here because plan currency lookups need
	// h.plans, which the service doesn't have. We hydrate the sub's items,
	// fetch each item's plan, and reject when the set spans more than one
	// currency. A threshold expressed in cents only makes sense against a
	// single-currency line set.
	if h.plans != nil {
		sub, gerr := h.svc.Get(r.Context(), tenantID, id)
		if gerr == nil && len(sub.Items) > 0 {
			seen := make(map[string]struct{}, 2)
			for _, it := range sub.Items {
				p, perr := h.plans.GetPlan(r.Context(), tenantID, it.PlanID)
				if perr != nil {
					continue
				}
				if p.Currency != "" {
					seen[p.Currency] = struct{}{}
				}
			}
			if len(seen) > 1 {
				respond.Error(w, r, http.StatusUnprocessableEntity, "validation_error",
					"billing thresholds are not supported on multi-currency subscriptions",
					"billing_thresholds")
				return
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
			"action":               "billing_thresholds_set",
			"customer_id":          sub.CustomerID,
			"amount_gte":           input.AmountGTE,
			"item_threshold_count": len(input.ItemThresholds),
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
			"action":      "billing_thresholds_cleared",
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
			"action":      "cancel_cleared",
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
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respond.BadRequest(w, r, "invalid JSON body")
		return
	}

	// Bind effective-now from the sub pin so proration math runs in
	// simulated time on clock-pinned subs (ADR-030). Without binding,
	// `remainingPeriodFactor(sub, time.Now())` would compute the
	// wrong factor for a clock-pinned sub whose current_period is in
	// the simulated future. PR-12.
	ctx := h.bindForSub(r.Context(), tenantID, id)
	r = r.WithContext(ctx)

	// Snapshot the pre-add subscription for proration. Factor is computed
	// from the period boundaries which don't change when an item is added
	// mid-cycle, so a pre-add read is equivalent to a post-add read here.
	var subBefore domain.Subscription
	var prorationRemainingDays, prorationTotalDays int64
	if h.plans != nil && h.invoices != nil {
		sub, serr := h.svc.Get(ctx, tenantID, id)
		if serr == nil {
			subBefore = sub
			if sub.Status == domain.SubscriptionActive {
				prorationRemainingDays, prorationTotalDays = remainingPeriodRatio(sub, clock.Now(ctx))
				// Day-grade clamp (ADR-012 amendment 2026-07-26): an item
				// added ON the period-start calendar day (tenant TZ) owes
				// the whole period — the add day counts whole, matching
				// the cycle-close segment clamp in billing.itemBaseSegments.
				// Without this a post-noon day-1 add pays 30/31 in_advance
				// while its in_arrears twin bills the full period at close.
				// Adds only: plan/quantity/remove changes stay instant-
				// precise per ADR-012's Consequences, so their handlers
				// don't clamp.
				if prorationRemainingDays > 0 && sub.CurrentBillingPeriodStart != nil &&
					domain.SameCalendarDayIn(clock.Now(ctx), *sub.CurrentBillingPeriodStart, h.tenantLoc(ctx, tenantID)) {
					prorationRemainingDays = prorationTotalDays
				}
			}
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
		item, addProration, addErr = h.atomicAddItemWithProration(ctx, tenantID, id, input, subBefore, prorationRemainingDays, prorationTotalDays)
	} else {
		item, addErr = h.svc.AddItem(ctx, tenantID, id, input)
	}
	if addErr != nil {
		respond.FromError(w, r, addErr, "subscription")
		return
	}

	if h.auditLogger != nil {
		payload := map[string]any{
			"action":   "item_added",
			"item_id":  item.ID,
			"plan_id":  item.PlanID,
			"quantity": item.Quantity,
		}
		addProrationMeta(payload, addProration, h.planCurrency(ctx, tenantID, item.PlanID))
		_ = h.auditLogger.Log(h.auditCtx(r.Context(), tenantID, id), tenantID, domain.AuditActionUpdate, "subscription", id, subBefore.Code, payload)
	}

	// Re-fetch the subscription so downstream payload/proration paths see the
	// full post-add Items slice. Silent fallback: if the read fails we use a
	// minimal struct for events — item.added is still useful without the
	// enclosing snapshot.
	var subAfter domain.Subscription
	if h.events != nil || (wantProration && !atomic) {
		s, getErr := h.svc.Get(r.Context(), tenantID, id)
		if getErr != nil {
			subAfter = subBefore
			if subAfter.ID == "" {
				subAfter = domain.Subscription{ID: id}
			}
		} else {
			subAfter = s
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
				_ = h.auditLogger.Log(h.auditCtx(r.Context(), tenantID, id), tenantID, "subscription.proration_failed", "subscription", id, "", map[string]any{
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

	if h.events != nil {
		h.fireEvent(r.Context(), tenantID, domain.EventSubscriptionItemAdded, subAfter, map[string]any{
			"item": itemPayload(item),
		})
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
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respond.BadRequest(w, r, "invalid JSON body")
		return
	}

	// Bind effective-now from the sub pin so proration math + changeAt
	// stamps run in simulated time on clock-pinned subs (PR-12).
	ctx := h.bindForSub(r.Context(), tenantID, subID)
	r = r.WithContext(ctx)

	// Capture the pre-change item/plan only when we're about to drive proration
	// — the old plan id, old quantity, and the subscription's remaining period
	// all come from a snapshot taken before UpdateItem mutates the row.
	var oldPlanID string
	var oldQuantity int64
	var prorationRemainingDays, prorationTotalDays int64
	var subBefore domain.Subscription
	isImmediatePlanChange := input.NewPlanID != "" && input.Immediate
	isQuantityChange := input.Quantity != nil
	prorationEligible := (isImmediatePlanChange || isQuantityChange) && h.plans != nil
	if prorationEligible {
		item, gerr := h.svc.store.GetItem(ctx, tenantID, itemID)
		if gerr == nil && item.SubscriptionID == subID {
			oldPlanID = item.PlanID
			oldQuantity = item.Quantity
		}
		sub, serr := h.svc.Get(ctx, tenantID, subID)
		if serr == nil {
			subBefore = sub
			prorationRemainingDays, prorationTotalDays = remainingPeriodRatio(sub, clock.Now(ctx))
		}
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
		newP, e2 := h.plans.GetPlan(ctx, tenantID, input.NewPlanID)
		if e1 == nil && e2 == nil {
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
		result, atomicErr = h.atomicUpdateItemWithProration(ctx, tenantID, subID, itemID, input, subBefore, oldPlanID, oldQuantity, prorationRemainingDays, prorationTotalDays)
		if errors.Is(atomicErr, errAtomicNotApplicable) {
			atomic = false
			atomicErr = nil
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
	// in_advance — billed the new in_advance period synchronously via
	// BillOnCreate. Firing handleItemProration on top would emit a
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
				_ = h.auditLogger.Log(h.auditCtx(r.Context(), tenantID, subID), tenantID, "subscription.proration_failed", "subscription", subID, "", map[string]any{
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
		if input.Quantity != nil {
			payload["action"] = "item_quantity_changed"
			payload["quantity"] = *input.Quantity
		} else {
			payload["action"] = "item_plan_changed"
			payload["old_plan_id"] = oldPlanID
			payload["new_plan_id"] = input.NewPlanID
		}
		// Sim-time context (ADR-030): re-fetch the post-change sub for its
		// UpdatedAt (the change instant) + test_clock_id so a clock-pinned
		// change renders the simulated effect-time subline.
		auditSub := subBefore
		if s, err := h.svc.Get(ctx, tenantID, subID); err == nil {
			auditSub = s
		}
		_ = h.auditLogger.Log(h.auditCtx(ctx, tenantID, subID), tenantID, "subscription.item_updated", "subscription", subID, auditSub.Code, payload)
	}

	// Event dispatch. Quantity changes and immediate plan changes are
	// observable-now → subscription.item.updated. A scheduled plan change
	// (Immediate=false, NewPlanID set) is an intent, not a mutation of the
	// current cycle → subscription.pending_change.scheduled; the applied
	// event fires at the cycle boundary from billing.Engine.
	if h.events != nil {
		sub, getErr := h.svc.Get(r.Context(), tenantID, subID)
		if getErr != nil {
			sub = domain.Subscription{ID: subID}
		}
		extra := map[string]any{"item": itemPayload(result.Item)}
		eventType := domain.EventSubscriptionItemUpdated
		if input.NewPlanID != "" && !input.Immediate {
			eventType = domain.EventSubscriptionPendingChangeScheduled
			extra["new_plan_id"] = input.NewPlanID
			if result.Item.PendingPlanEffectiveAt != nil {
				extra["effective_at"] = result.Item.PendingPlanEffectiveAt.UTC()
			}
		}
		h.fireEvent(r.Context(), tenantID, eventType, sub, extra)
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
			"action":  "cancel_pending_item_plan_change",
			"item_id": item.ID,
		})
	}

	if h.events != nil {
		sub, getErr := h.svc.Get(r.Context(), tenantID, subID)
		if getErr != nil {
			sub = domain.Subscription{ID: subID}
		}
		h.fireEvent(r.Context(), tenantID, domain.EventSubscriptionPendingChangeCanceled, sub, map[string]any{
			"item": itemPayload(item),
		})
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

	// Capture the pre-delete item + sub for proration. Removing mid-period
	// should credit back the unused portion of what the customer already paid
	// for this item. RemoveItem is a hard delete so the snapshot must be
	// taken before the call.
	var removedPlanID string
	var removedQuantity int64
	var subBefore domain.Subscription
	var prorationRemainingDays, prorationTotalDays int64
	if h.plans != nil && h.credits != nil {
		item, gerr := h.svc.store.GetItem(ctx, tenantID, itemID)
		if gerr == nil && item.SubscriptionID == subID {
			removedPlanID = item.PlanID
			removedQuantity = item.Quantity
		}
		sub, serr := h.svc.Get(ctx, tenantID, subID)
		if serr == nil {
			subBefore = sub
			if sub.Status == domain.SubscriptionActive {
				prorationRemainingDays, prorationTotalDays = remainingPeriodRatio(sub, clock.Now(ctx))
			}
		}
	}

	// Atomic RemoveItem-with-proration: delete + credit grant in one
	// tx so a failed credit grant rolls back the delete. Falls back to
	// the legacy non-atomic flow when db isn't wired or no proration
	// emission is needed.
	wantProration := prorationRemainingDays > 0 && removedPlanID != "" && h.credits != nil
	atomic := h.db != nil && wantProration
	var removeProration *ProrationDetail
	if atomic {
		detail, err := h.atomicRemoveItemWithProration(ctx, tenantID, subID, itemID, subBefore, removedPlanID, removedQuantity, prorationRemainingDays, prorationTotalDays)
		if err != nil {
			respond.FromError(w, r, err, "subscription item")
			return
		}
		removeProration = detail
	} else {
		if err := h.svc.RemoveItem(ctx, tenantID, subID, itemID); err != nil {
			respond.FromError(w, r, err, "subscription item")
			return
		}
	}

	if h.auditLogger != nil {
		payload := map[string]any{
			"action":  "item_removed",
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
				_ = h.auditLogger.Log(h.auditCtx(r.Context(), tenantID, subID), tenantID, "subscription.proration_failed", "subscription", subID, "", map[string]any{
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
		sub, getErr := h.svc.Get(r.Context(), tenantID, subID)
		if getErr != nil {
			sub = domain.Subscription{ID: subID}
		}
		h.fireEvent(r.Context(), tenantID, domain.EventSubscriptionItemRemoved, sub, map[string]any{
			"item_id": itemID,
		})
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

// handleItemProration generates the invoice or credit for a single item
// mutation. Dedup key is (tenant, subscription, item, change_type, change_at)
// — retries of the same mutation converge on the existing artifact via the
// proration dedup index.
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
func (h *Handler) atomicAddItemWithProration(
	ctx context.Context,
	tenantID, subID string,
	input AddItemInput,
	subBefore domain.Subscription,
	remainingDays, totalDays int64,
) (domain.SubscriptionItem, *ProrationDetail, error) {
	tx, err := h.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.SubscriptionItem{}, nil, fmt.Errorf("begin atomic addItem tx: %w", err)
	}
	defer postgres.Rollback(tx)

	item, err := h.svc.AddItemTx(ctx, tx, tenantID, subID, input)
	if err != nil {
		return domain.SubscriptionItem{}, nil, err
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
	detail, err := h.handleItemProration(ctx, tenantID, subBefore, spec, tx)
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

// atomicUpdateItemWithProration mirrors atomicAddItemWithProration for
// the UpdateItem path: open one outer tx, write the item update + the
// proration in the same tx, commit on success. Returns
// errAtomicNotApplicable if the input routes to the cross-interval or
// scheduled path (caller falls back to legacy non-atomic flow).
func (h *Handler) atomicUpdateItemWithProration(
	ctx context.Context,
	tenantID, subID, itemID string,
	input UpdateItemInput,
	subBefore domain.Subscription,
	oldPlanID string, oldQuantity int64,
	remainingDays, totalDays int64,
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
		h.svc.FinalizeCrossIntervalSwap(ctx, tenantID, subBefore, result)
		return result, nil
	}

	// Build the proration spec mirroring the legacy handler logic.
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
			oldPlanID:     oldPlanID,
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
			oldPlanID:     oldPlanID,
			oldQuantity:   oldQuantity,
			newPlanID:     result.Item.PlanID,
			newQuantity:   result.Item.Quantity,
		}
	}
	detail, err := h.handleItemProration(ctx, tenantID, subBefore, spec, tx)
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
func (h *Handler) atomicRemoveItemWithProration(
	ctx context.Context,
	tenantID, subID, itemID string,
	subBefore domain.Subscription,
	oldPlanID string, oldQuantity int64,
	remainingDays, totalDays int64,
) (*ProrationDetail, error) {
	tx, err := h.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, fmt.Errorf("begin atomic removeItem tx: %w", err)
	}
	defer postgres.Rollback(tx)

	if err := h.svc.RemoveItemTx(ctx, tx, tenantID, subID, itemID); err != nil {
		return nil, err
	}

	spec := itemProrationSpec{
		changeType:    domain.ItemChangeTypeRemove,
		changeAt:      clock.Now(ctx),
		remainingDays: remainingDays,
		totalDays:     totalDays,
		itemID:        itemID,
		oldPlanID:     oldPlanID,
		oldQuantity:   oldQuantity,
		newPlanID:     "",
		newQuantity:   0,
	}
	detail, err := h.handleItemProration(ctx, tenantID, subBefore, spec, tx)
	if err != nil {
		return nil, fmt.Errorf("proration in atomic removeItem tx: %w", err)
	}

	// Create the item-removal clawback credit-note DRAFT(s) IN this tx so the
	// clawback obligation commits atomically with the item delete (ADR-056) — a
	// create failure rolls the delete back (this is what makes the function's
	// "a credit failure rolls back the delete" contract actually hold). Issued
	// post-commit below.
	if err := h.createClawbackDraftsTx(ctx, tx, tenantID, detail); err != nil {
		return nil, fmt.Errorf("create clawback draft in atomic removeItem tx: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit atomic removeItem tx: %w", err)
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
	return detail, nil
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
		slog.ErrorContext(ctx, "proration charge invoice not enrolled for auto-charge; collect manually or it will sit unpaid",
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
// on the non-atomic path. No-op unless that branch recorded a clawback (CN
// issuer wired + PAID source invoice resolved).
//
// On error the item change is already durable but the customer was not
// credited — log at error level so it surfaces for reconciliation. The
// operation still reports success: the downgrade itself committed, and a client
// retry is rejected at Service.UpdateItem's no-op gate (so we never double-
// issue), so returning an error here would be misleading and unrecoverable by
// retry anyway. The source invoice's over-credit cap bounds any aggregate
// over-credit.
// resolveClawbackPieces fans a downgrade/qty-decrease/item-removal clawback of
// netCredit (net, the unconsumed removed value) across EVERY invoice that funded
// the current period, each piece capped at that invoice's remaining headroom
// (creditable for a paid invoice / amount_due for an unpaid one):
//   - PLAN downgrade → LIFO: reverse the most-recent funding invoice (the
//     upgrade) first, so the latest price level is undone against its own tax
//     and the still-active base plan is left untouched; spill to older invoices
//     only when the slice exceeds the newest invoice's headroom.
//   - QUANTITY decrease / item REMOVAL → proportional: the removed units are
//     fungible (no principled "newest seat"), so split across funding invoices
//     in subtotal proportion.
//
// Read-only (it issues nothing) — called inside the atomic item-change tx, so a
// returned error (headroom genuinely exhausted, or a lookup failure) rolls the
// change back BEFORE it commits rather than leaving a post-commit "reconcile
// manually" gap. The credit notes themselves are issued post-commit by
// issueClawbackCreditNote.
func (h *Handler) resolveClawbackPieces(ctx context.Context, tenantID string, sub domain.Subscription, changeType domain.ItemChangeType, netCredit int64) ([]ClawbackPiece, error) {
	if netCredit <= 0 || sub.CurrentBillingPeriodStart == nil || sub.CurrentBillingPeriodEnd == nil {
		return nil, nil
	}
	funding, err := h.invoices.FindFundingInvoicesForPeriod(ctx, tenantID, sub.ID, *sub.CurrentBillingPeriodStart, *sub.CurrentBillingPeriodEnd)
	if err != nil {
		return nil, err
	}

	type fundingCap struct {
		inv      domain.Invoice
		grossCap int64
		netCap   int64
	}
	caps := make([]fundingCap, 0, len(funding))
	for _, f := range funding {
		var grossCap int64
		// An IN-FLIGHT funding invoice is capped like a PAID one, not like an
		// unpaid one (2026-07-30): a paid-then-in-flight source has amount_due=0,
		// so the unpaid arm would cap the piece at 0 and the draft would be worth
		// nothing — the same silent-drop this fix exists to remove. The headroom
		// that matters is what the source could still be credited for
		// (total − already credited); whether the in-flight charge ultimately
		// succeeds is decided later, by creditnote.Issue's paid/unpaid branch.
		if f.PaymentStatus == domain.PaymentSucceeded || f.PaymentStatus.IsInFlight() {
			credited, cerr := h.creditNotes.CreditedCents(ctx, tenantID, f.ID)
			if cerr != nil {
				return nil, fmt.Errorf("read creditable headroom for %s: %w", f.ID, cerr)
			}
			grossCap = f.TotalAmountCents - credited
		} else {
			grossCap = f.AmountDueCents
		}
		if grossCap <= 0 {
			continue // no room left on this invoice
		}
		netCap := grossCap
		if f.SubtotalCents > 0 && f.TotalAmountCents > 0 {
			netCap = money.RoundHalfToEven(grossCap*f.SubtotalCents, f.TotalAmountCents)
		}
		caps = append(caps, fundingCap{inv: f, grossCap: grossCap, netCap: netCap})
	}
	if len(caps) == 0 {
		return nil, fmt.Errorf("no creditable funding invoice for period")
	}

	netCaps := make([]int64, len(caps))
	var nets []int64
	var remainder int64
	if changeType == domain.ItemChangeTypePlan {
		// LIFO — newest funding invoice first (undo the latest price level).
		// Equal timestamps are REAL, not theoretical: a clock catchup stamps
		// every invoice it generates with the same simulated instant, and a
		// same-second subscribe+upgrade does it on wall time. On a tie the
		// subscription_update invoice IS the later price level (the funding
		// query encodes the same tiebreak, ASC) — without this clause the
		// stable sort kept the query's oldest-first order and the clawback
		// landed on the day-1 invoice, the exact opposite of LIFO (found
		// live, FLOW B17c walk 2026-07-21).
		sort.SliceStable(caps, func(a, b int) bool {
			if caps[a].inv.CreatedAt.Equal(caps[b].inv.CreatedAt) {
				return caps[a].inv.BillingReason == domain.BillingReasonSubscriptionUpdate &&
					caps[b].inv.BillingReason != domain.BillingReasonSubscriptionUpdate
			}
			return caps[a].inv.CreatedAt.After(caps[b].inv.CreatedAt)
		})
		for i := range caps {
			netCaps[i] = caps[i].netCap
		}
		nets, remainder = money.AllocateLIFO(netCredit, netCaps)
	} else {
		// Fungible qty/item change → proportional by what each invoice charged.
		weights := make([]int64, len(caps))
		for i := range caps {
			weights[i] = caps[i].inv.SubtotalCents
			netCaps[i] = caps[i].netCap
		}
		nets, remainder = money.AllocateByWeightCapped(netCredit, weights, netCaps)
	}
	if remainder > 0 {
		return nil, fmt.Errorf("clawback %d net exceeds funding headroom by %d across %d invoice(s)", netCredit, remainder, len(caps))
	}

	pieces := make([]ClawbackPiece, 0, len(caps))
	for i := range caps {
		net := nets[i]
		if net <= 0 {
			continue
		}
		gross := grossUpByInvoiceRatio(net, caps[i].inv.SubtotalCents, caps[i].inv.TotalAmountCents)
		if gross > caps[i].grossCap {
			gross = caps[i].grossCap // clamp net→gross rounding to headroom
		}
		if gross <= 0 {
			continue
		}
		pieces = append(pieces, ClawbackPiece{InvoiceID: caps[i].inv.ID, GrossCents: gross})
	}
	return pieces, nil
}

func (h *Handler) issueClawbackCreditNote(ctx context.Context, tenantID string, detail *ProrationDetail) {
	if h.creditNotes == nil || detail == nil {
		return
	}
	pieces := detail.ClawbackPieces
	if len(pieces) == 0 && detail.ClawbackInvoiceID != "" && detail.ClawbackGrossCents > 0 {
		// Single-piece carrier (the unpaid-source amount_due relief path).
		pieces = []ClawbackPiece{{InvoiceID: detail.ClawbackInvoiceID, GrossCents: detail.ClawbackGrossCents}}
	}
	for i, p := range pieces {
		if p.InvoiceID == "" || p.GrossCents <= 0 {
			continue
		}
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

// clawbackPieces resolves the clawback fan-out pieces for a detail, including
// the single-piece carrier (the unpaid-source amount_due relief path), and
// drops empty/zero pieces. Shared by the in-tx create and the inline path.
func clawbackPieces(detail *ProrationDetail) []ClawbackPiece {
	pieces := detail.ClawbackPieces
	if len(pieces) == 0 && detail.ClawbackInvoiceID != "" && detail.ClawbackGrossCents > 0 {
		pieces = []ClawbackPiece{{InvoiceID: detail.ClawbackInvoiceID, GrossCents: detail.ClawbackGrossCents}}
	}
	out := make([]ClawbackPiece, 0, len(pieces))
	for _, p := range pieces {
		if p.InvoiceID != "" && p.GrossCents > 0 {
			out = append(out, p)
		}
	}
	return out
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
	for _, p := range clawbackPieces(detail) {
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

	// Industry-standard gates on immediate proration emission. Two
	// scenarios must NOT produce an immediate invoice or credit grant:
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
	// (2) effective plan is in_advance BUT the source invoice for the
	//     current period was not paid: the would-be credit is against
	//     money the customer never put in (Chargebee "Adjustment" credit
	//     case; Stripe explicitly warns about this). Skip immediate
	//     emission; if the operator wants to settle, void the unpaid
	//     invoice or wait for dunning.
	//
	// Both gates are silent-defer (logged at info, no error). The
	// downstream item change itself still applies — just no proration
	// artifact.
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
	// Computed BEFORE resolving the source invoice so the unpaid-source branch
	// (ADR-050) can route by sign: a net charge (upgrade / add / qty-increase)
	// is blocked, a net credit (downgrade / removal / qty-decrease) settles as
	// an adjustment against the open invoice.
	//
	// Denominator is the FULL billing cycle, NOT spec.totalDays (the current
	// period). On a stub/partial period (mid-cycle signup) spec.totalDays is
	// the short stub length, and dividing by it over-charges upgrades /
	// over-credits downgrades — the customer paid old × stub/fullCycle for the
	// period, so the delta for the remaining portion is (new−old) ×
	// remaining/fullCycle. This matches the engine's day-1 stub base fee and
	// BillOnPlanSwapImmediate (both divide by fullCycleDays).
	oldAmount := oldPlan.BaseAmountCents * spec.oldQuantity
	newAmount := newPlan.BaseAmountCents * spec.newQuantity
	denomDays := spec.totalDays
	if sub.CurrentBillingPeriodStart != nil {
		if fc := fullBillingCycleDays(*sub.CurrentBillingPeriodStart, effectivePlan.BillingInterval, h.tenantLoc(ctx, sub.TenantID), sub.BillingAnchorDay); fc > 0 {
			denomDays = fc
		}
	}
	remainingDays := min(spec.remainingDays, denomDays)
	proratedCents := prorationCents(oldAmount, newAmount, remainingDays, denomDays)

	if proratedCents == 0 {
		return nil, nil
	}

	// Resolve the current-period in_advance source invoice and branch on its
	// payment status. src/haveSrc carry a PAID source into the happy path
	// below — the downgrade-credit branch grosses the clawback up against this
	// invoice's tax ratio and anchors the tax-reversing credit note to it
	// (ADR-048). In production h.invoices is always wired and this gate runs.
	var haveSrc bool
	if h.invoices != nil && sub.CurrentBillingPeriodStart != nil {
		// single-invoice-ok: paid/unpaid routing gate only — the per-invoice
		// credit MATH fans across FindFundingInvoicesForPeriod (resolveClawbackPieces).
		// Audit 2026-06-15 cleared the gate (0/3); base-paid implies the period was funded.
		resolved, lookupErr := h.invoices.FindBaseInvoiceForPeriod(ctx, tenantID, sub.ID, *sub.CurrentBillingPeriodStart)
		if lookupErr != nil {
			// No source invoice for the period (e.g. a freshly created sub whose
			// first prebill hasn't been generated). Nothing to anchor an
			// adjustment or clawback to — defer to cycle close.
			slog.InfoContext(ctx, "item proration deferred: in_advance source invoice not found for current period",
				"subscription_id", sub.ID,
				"item_id", spec.itemID,
				"change_type", spec.changeType,
				"period_start", *sub.CurrentBillingPeriodStart,
				"error", lookupErr,
			)
			return nil, nil
		}
		// THREE-way split, not two (fixed 2026-07-30, found walking FLOW C2).
		// `!= PaymentSucceeded` used to lump IN-FLIGHT sources in with genuinely
		// unpaid ones and send them down the amount_due-reduction branch. On a
		// source that is already PAID but has a charge in flight (amount_due=0,
		// payment_status=processing/unknown), that branch computes
		// min(grossCredit, amount_due) == 0 and returns nil SILENTLY — the item
		// change committed, the customer kept the cheaper plan, and the credit
		// they were owed existed nowhere. Proven with a control: the identical
		// downgrade on a `succeeded` source credited $32.18; on a `processing`
		// source it produced no row at all.
		//
		// domain.IsInFlight's own contract says such a mutation must "refuse or
		// defer", and DEFER is already built end to end: creditnote.Issue is the
		// single chokepoint for every issue trigger and leaves the note
		// draft+issue_pending while the source is in flight, after which the
		// ADR-059 reconciler re-drives it and picks the paid/unpaid channel from
		// how the source actually resolved. All of that needs one thing from us:
		// a DRAFT must exist. So an in-flight source falls through to the
		// draft-creating path below instead of being settled inline.
		if resolved.PaymentStatus.IsInFlight() {
			slog.InfoContext(ctx, "item proration: source payment in flight — clawback will be drafted and issued once it settles (ADR-059)",
				"subscription_id", sub.ID,
				"item_id", spec.itemID,
				"source_invoice_id", resolved.ID,
				"payment_status", string(resolved.PaymentStatus),
			)
		} else if resolved.PaymentStatus != domain.PaymentSucceeded {
			// UNPAID source. Industry-convergent rule (Chargebee Adjustment
			// Credit / Lago credit-note wallet / Orb adjustment / Stripe
			// proration_behavior=none on unpaid / Recurly block-on-past-due):
			// never mint a refundable credit or stack a second charge against
			// money the customer hasn't paid (ADR-050).
			return h.settleUnpaidSourceProration(ctx, tenantID, sub, spec, oldPlan, newPlan, resolved, proratedCents, tx)
		}
		haveSrc = true
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
			if d := h.netTerms.NetPaymentTermDays(ctx, tenantID); d > 0 {
				netDays = d
			}
		}
		dueAt := now.AddDate(0, 0, netDays)

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
			labelLoc := h.tenantLoc(ctx, sub.TenantID)
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
			Status:        domain.InvoiceFinalizationStatus(taxResult.TaxStatus, sub.PauseCollection),
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
			BillingTimezone:    h.tenantLoc(ctx, sub.TenantID).String(),
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

		var inv domain.Invoice
		if tx != nil {
			inv, err = h.invoices.CreateInvoiceWithLineItemsTx(ctx, tx, tenantID, invoice, lineItems)
		} else {
			inv, err = h.invoices.CreateInvoiceWithLineItems(ctx, tenantID, invoice, lineItems)
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
				// single-invoice-ok: proration-dedup replay — returns the prior invoice
				// for THIS exact (item, change_type, change_at) to avoid double-issuing.
				// An idempotency lookup, not a period-wide funding decision.
				existing, lookupErr := h.invoices.GetByProrationSource(ctx, tenantID, sub.ID, spec.itemID, spec.changeType, spec.changeAt)
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
		if tx == nil {
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
	} else {
		creditAmount := -proratedCents

		// ADR-048: on a PAID, taxed in_advance source invoice with the CN
		// issuer wired, credit the GROSS the customer paid for the unused
		// slice (net + the proportional tax) via the tax-reversing credit-note
		// primitive — which also reverses the proportional output tax against
		// the original invoice's committed tax transaction — rather than the
		// bare net ledger grant that dropped the tax. The CN service's tax
		// reversal is an external Stripe call, so it can't ride this tx; this
		// stashes the clawback pieces on the detail and the caller routes them:
		//   - atomic path (tx != nil): the atomic wrapper calls
		//     createClawbackDraftsTx (drafts committed WITH the item change, so a
		//     create failure rolls the change back) then issueClawbackDrafts
		//     post-commit; ADR-057.
		//   - non-atomic fallback (tx == nil): issueClawbackCreditNote inline
		//     below (create+issue, fire-and-forget — used only when h.db is
		//     unwired, e.g. narrow unit tests).
		if h.creditNotes != nil && haveSrc {
			// Fan the clawback across EVERY funding invoice of the period (LIFO
			// for a plan downgrade, proportional for fungible qty/item changes),
			// each capped at its own remaining headroom — so a clawback that
			// exceeds the single resolved invoice (e.g. after a prior upgrade
			// split the period across two invoices) is placed in full instead of
			// overrunning one invoice's credit-note cap.
			pieces, perr := h.resolveClawbackPieces(ctx, tenantID, sub, spec.changeType, creditAmount)
			if perr != nil {
				// Resolve runs read-only inside the atomic item-change tx, so a
				// returned error rolls the change back BEFORE it commits — nothing
				// half-applies. Cleaner than the old post-commit reconcile gap.
				return nil, fmt.Errorf("downgrade clawback: %w", perr)
			}
			var grossCredit int64
			for _, p := range pieces {
				grossCredit += p.GrossCents
			}
			detail.Type = "credit"
			detail.AmountCents = grossCredit
			detail.ClawbackPieces = pieces
			detail.ClawbackReason = clawbackReason(spec.changeType)
			detail.ClawbackMemo = memo
			if tx == nil {
				h.issueClawbackCreditNote(ctx, tenantID, detail)
			}
			slog.InfoContext(ctx, "downgrade clawback routed to tax-reversing credit note(s)",
				"subscription_id", sub.ID,
				"item_id", spec.itemID,
				"change_type", spec.changeType,
				"funding_invoices", len(pieces),
				"net_cents", creditAmount,
				"gross_cents", grossCredit,
			)
			return detail, nil
		}

		// Fallback: CN issuer unwired (narrow unit tests) or no paid source
		// invoice resolved — grant the bare net amount to the credit ledger as
		// before (no tax reversal).
		if h.credits != nil {
			grantInput := ProrationGrantInput{
				CustomerID:               sub.CustomerID,
				AmountCents:              creditAmount,
				Description:              memo,
				SourceSubscriptionID:     sub.ID,
				SourceSubscriptionItemID: spec.itemID,
				SourcePlanChangedAt:      spec.changeAt,
				SourceChangeType:         spec.changeType,
			}
			var err error
			if tx != nil {
				err = h.credits.GrantProrationTx(ctx, tx, tenantID, grantInput)
			} else {
				err = h.credits.GrantProration(ctx, tenantID, grantInput)
			}
			// Same pattern as the invoice path above: only the
			// proration-source code maps to an idempotent replay. Other
			// AlreadyExists (e.g. credit_note_source) are distinct bugs
			// and must propagate up. ADR-030 cross-flow audit 2026-05-28.
			if errs.Code(err) == "credit_proration_source_taken" {
				// single-invoice-ok: credit-ledger proration-dedup replay — same
				// idempotency lookup as the invoice path, not a funding decision.
				existing, lookupErr := h.credits.GetByProrationSource(ctx, tenantID, sub.ID, spec.itemID, spec.changeType, spec.changeAt)
				if lookupErr != nil {
					return nil, fmt.Errorf("proration credit dedup lookup: %w", lookupErr)
				}
				slog.InfoContext(ctx, "proration credit already granted; retry dedup",
					"entry_id", existing.ID,
					"subscription_id", sub.ID,
					"item_id", spec.itemID,
					"change_type", spec.changeType,
					"change_at", spec.changeAt,
				)
				detail.AmountCents = existing.AmountCents
				detail.Type = "credit"
				return detail, nil
			}
			if err != nil {
				return nil, fmt.Errorf("grant proration credit: %w", err)
			}

			detail.AmountCents = creditAmount
			detail.Type = "credit"

			slog.InfoContext(ctx, "proration credit granted",
				"subscription_id", sub.ID,
				"item_id", spec.itemID,
				"change_type", spec.changeType,
				"credit_cents", creditAmount,
			)
		}
	}

	return detail, nil
}

// settleUnpaidSourceProration applies the ADR-050 unpaid-source policy when a
// mid-period item change lands on an in_advance period whose source invoice is
// finalized but NOT paid. Industry-convergent across Stripe / Chargebee /
// Recurly / Lago / Orb: never mint a refundable credit, and never stack a
// second charge, against money the customer hasn't paid.
//
//   - NET CHARGE (upgrade / add / qty-increase, proratedCents > 0) → BLOCK.
//     We refuse to pile a second receivable onto an unpaid current-period
//     invoice (Recurly blocks past-due changes; Stripe recommends
//     proration_behavior=none on an unpaid latest invoice). The operator must
//     settle or void the outstanding invoice first. Returned as
//     errs.InvalidState so the atomic tx rolls the item change back and the
//     operator sees a clean 409 — nothing half-applies.
//
//   - NET CREDIT (downgrade / removal / qty-decrease, proratedCents < 0) →
//     reduce the open invoice's amount_due via a tax-reversing ADJUSTMENT
//     credit note, capped at amount_due. No refundable balance credit is
//     granted (no cash was funded). This is the same CreateAndIssueAdjustment
//     primitive the engine's relieveUnpaidPrebill uses on an unpaid cancel —
//     Chargebee's "Adjustment Credit", Lago's credit-note, Orb's adjustment.
//
// The adjustment is carried on the detail's Clawback* fields and issued by the
// caller AFTER the tx commits (the CN service is not tx-aware), exactly like
// the PAID downgrade clawback (ADR-048); the non-atomic path issues inline.
func (h *Handler) settleUnpaidSourceProration(ctx context.Context, tenantID string, sub domain.Subscription, spec itemProrationSpec, oldPlan, newPlan domain.Plan, src domain.Invoice, proratedCents int64, tx *sql.Tx) (*ProrationDetail, error) {
	if proratedCents > 0 {
		slog.InfoContext(ctx, "item change blocked: net charge against an unpaid in_advance source invoice (ADR-050)",
			"subscription_id", sub.ID,
			"item_id", spec.itemID,
			"change_type", spec.changeType,
			"source_invoice_id", src.ID,
			"source_payment_status", src.PaymentStatus,
			"amount_due_cents", src.AmountDueCents,
		)
		return nil, errs.InvalidState(fmt.Sprintf(
			"invoice %s for the current period is unpaid (%.2f %s outstanding); settle or void it before changing this subscription",
			src.InvoiceNumber, float64(src.AmountDueCents)/100, src.Currency,
		)).WithCode("unpaid_invoice_blocks_change")
	}

	// Net credit: settle the unused slice against the open invoice as an
	// adjustment, capped at what's still owed (CreateAndIssueAdjustment errors
	// above amount_due — same clamp the engine's unpaid-cancel relief applies).
	creditAmount := -proratedCents
	grossCredit := grossUpByInvoiceRatio(creditAmount, src.SubtotalCents, src.TotalAmountCents)
	reduceBy := min(grossCredit, src.AmountDueCents)
	if reduceBy <= 0 {
		// Nothing left to reduce on the source, so this branch cannot place the
		// credit. Reaching here with a credit owed used to `return nil, nil` —
		// the change committed and the money owed vanished with no row, no error
		// and no log (2026-07-30). The in-flight case that made it reachable now
		// routes to the draft path upstream; anything ELSE landing here is a
		// state we have not modelled, so say so LOUDLY rather than silently
		// under-crediting (no-silent-fallbacks).
		slog.ErrorContext(ctx, "downgrade credit could not be placed: unpaid source has no remaining amount_due to reduce — customer may be under-credited, reconcile manually",
			"subscription_id", sub.ID,
			"item_id", spec.itemID,
			"source_invoice_id", src.ID,
			"source_status", string(src.Status),
			"source_payment_status", string(src.PaymentStatus),
			"source_amount_due_cents", src.AmountDueCents,
			"gross_credit_cents", grossCredit,
		)
		return nil, nil
	}

	detail := &ProrationDetail{
		OldPlanID:          spec.oldPlanID,
		NewPlanID:          spec.newPlanID,
		Type:               "adjustment",
		AmountCents:        reduceBy,
		ClawbackInvoiceID:  src.ID,
		ClawbackGrossCents: reduceBy,
		ClawbackReason:     clawbackReason(spec.changeType),
		ClawbackMemo:       prorationMemo(spec, oldPlan, newPlan),
	}
	// Non-atomic path issues inline; the atomic wrappers issue after tx.Commit.
	if tx == nil {
		h.issueClawbackCreditNote(ctx, tenantID, detail)
	}
	slog.InfoContext(ctx, "unpaid-source downgrade settled via adjustment credit note (amount_due reduced; no refundable credit)",
		"subscription_id", sub.ID,
		"item_id", spec.itemID,
		"change_type", spec.changeType,
		"source_invoice_id", src.ID,
		"net_cents", creditAmount,
		"reduce_by_cents", reduceBy,
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

// remainingPeriodFactor returns the fraction of the current billing period
// that is still ahead of `now`, clamped to [0, 1]. Used to scale a proration
// charge/credit against the per-period price.
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

// describeSubscriptionAction maps audit_log action + metadata to a
// human-readable sentence + sub-line + status tag the UI colors by.
// Unknown actions pass through with a neutral info tag rather than
// hiding — the feed should never silently drop an event.
//
// Detail is the optional sub-line beneath the main description. Used
// for human-meaningful context (dates, counts, plan names) that
// bloats the title if inlined. Mirrors the invoice timeline shape.
//
// planNames is a lookup from plan_id → plan.Name used to resolve
// raw IDs in metadata to operator-friendly labels. The caller
// (activityTimeline) batch-fetches every plan_id referenced in the
// audit entries before invoking this function. Missing entries fall
// back to the raw ID (deleted plan / lookup miss).
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

// prorationPartLabel renders the proration OUTCOME segment of a timeline row
// from the audit metadata stamped by addProrationMeta — "Proration invoice
// $50.00" / "Credit $19.80" / "Open invoice adjusted $54.44". Empty when no
// proration metadata is present. Shared across the plan-change, quantity-
// change, item-add, and item-remove renderers so the wording can't drift.
func prorationPartLabel(meta map[string]any) string {
	// Tolerate both int64 (an in-memory map straight from addProrationMeta)
	// and float64 (the same value after an audit-store JSON round-trip).
	// Production reads come back float64; this keeps the helper correct if
	// ever called on an un-round-tripped map.
	var amt int64
	switch v := meta["proration_amount_cents"].(type) {
	case float64:
		amt = int64(v)
	case int64:
		amt = v
	case int:
		amt = int64(v)
	default:
		return ""
	}
	cur, _ := meta["currency"].(string)
	switch t, _ := meta["proration_type"].(string); t {
	case "invoice":
		return "Proration invoice " + formatAmountForTimeline(amt, cur)
	case "credit":
		return "Credit " + formatAmountForTimeline(amt, cur)
	case "adjustment":
		return "Open invoice adjusted " + formatAmountForTimeline(amt, cur)
	}
	return ""
}

// formatAmountForTimeline renders cents for the activity feed with the
// right currency marker — the previous hardcoded "$" mislabeled every
// non-USD tenant's amounts. Symbols for the common cases, ISO code
// suffix otherwise.
func formatAmountForTimeline(cents int64, currency string) string {
	v := float64(cents) / 100
	switch strings.ToUpper(currency) {
	case "USD", "":
		return fmt.Sprintf("$%.2f", v)
	case "EUR":
		return fmt.Sprintf("€%.2f", v)
	case "GBP":
		return fmt.Sprintf("£%.2f", v)
	case "INR":
		return fmt.Sprintf("₹%.2f", v)
	default:
		return fmt.Sprintf("%.2f %s", v, strings.ToUpper(currency))
	}
}

func describeSubscriptionAction(action string, meta map[string]any, planNames map[string]string) (desc, detail, detailTimestamp, status string) {
	switch action {
	case domain.AuditActionCreate:
		return "Subscription created", "", "", "info"
	case domain.AuditActionActivate:
		return "Subscription activated", "", "", "succeeded"
	case domain.AuditActionCancel:
		by := ""
		if v, ok := meta["canceled_by"].(string); ok && v != "" {
			by = " by " + v
		}
		// Surface cancel-proration credit (in_advance unused-base
		// refund) on the timeline. Pre-fix the credit was only
		// visible on the customer's credit ledger; operators had to
		// cross-reference to learn how much was refunded for this
		// specific cancel. Industry standard — Stripe / Lago /
		// Chargebee / Orb link the credit to the subscription event.
		d := ""
		if v, ok := meta["prorated_credit_cents"].(float64); ok && v > 0 {
			cur, _ := meta["currency"].(string)
			d = "Prorated credit issued: " + formatAmountForTimeline(int64(v), cur)
		}
		return "Subscription canceled" + by, d, "", "canceled"
	case domain.AuditActionUpdate:
		// AuditActionUpdate is a catch-all bucket; the meaningful
		// discriminator is meta["action"]. Every operator-driven
		// mutation that doesn't have its own audit action (cancel,
		// activate, create) routes through here with a sub-action tag.
		a, _ := meta["action"].(string)
		switch a {
		case "cancel_scheduled":
			if v, ok := meta["cancel_at_period_end"].(bool); ok && v {
				return "Cancellation scheduled", "At end of current period", "", "warning"
			}
			if t, ok := meta["cancel_at"].(string); ok && t != "" {
				return "Cancellation scheduled", "On", t, "warning"
			}
			return "Cancellation scheduled", "", "", "warning"
		case "cancel_cleared":
			return "Scheduled cancellation cleared", "", "", "info"
		case "collection_paused":
			d := ""
			ts := ""
			if r, ok := meta["resumes_at"].(string); ok && r != "" {
				d = "Auto-resumes"
				ts = r
			} else {
				d = "Cycle keeps drafting; no charge until resumed"
			}
			title := "Collection paused"
			if tb, _ := meta["triggered_by"].(string); tb == "dunning" {
				title = "Collection paused by dunning"
			}
			return title, d, ts, "warning"
		case "collection_resumed":
			if tb, _ := meta["triggered_by"].(string); tb == "schedule" {
				return "Collection auto-resumed", "Scheduled resume date reached", "", "succeeded"
			}
			return "Collection resumed", "", "", "succeeded"
		case "trial_ended":
			return "Trial ended early", "", "", "info"
		case "trial_extended":
			d := ""
			ts := ""
			if t, ok := meta["trial_end"].(string); ok && t != "" {
				d = "New trial end:"
				ts = t
			}
			return "Trial extended", d, ts, "info"
		case "billing_thresholds_set":
			parts := []string{}
			if v, ok := meta["amount_gte"].(float64); ok && v > 0 {
				parts = append(parts, fmt.Sprintf("Amount ≥ %d¢", int64(v)))
			}
			if v, ok := meta["item_threshold_count"].(float64); ok && v > 0 {
				parts = append(parts, fmt.Sprintf("%d item threshold%s", int(v), plural(int(v))))
			}
			return "Billing thresholds set", strings.Join(parts, " · "), "", "info"
		case "billing_thresholds_cleared":
			return "Billing thresholds cleared", "", "", "info"
		case "item_added":
			parts := []string{}
			if v, ok := meta["plan_id"].(string); ok && v != "" {
				parts = append(parts, planLabel(v, planNames))
			}
			if q, ok := meta["quantity"].(float64); ok && q > 0 {
				parts = append(parts, fmt.Sprintf("qty %d", int(q)))
			}
			if pp := prorationPartLabel(meta); pp != "" {
				parts = append(parts, pp)
			}
			return "Item added", strings.Join(parts, " · "), "", "info"
		case "item_removed":
			// Plan NAME (resolved via planNames) is the operator-legible
			// noun; the vlx_si_ item id stays out of the row.
			parts := []string{}
			if pid, _ := meta["plan_id"].(string); pid != "" {
				parts = append(parts, planLabel(pid, planNames))
			}
			if pp := prorationPartLabel(meta); pp != "" {
				parts = append(parts, pp)
			}
			return "Item removed", strings.Join(parts, " · "), "", "info"
		case "cancel_pending_item_plan_change":
			return "Pending plan change canceled", "", "", "info"
		}
		// Unknown sub-action — surface the bucket label rather than
		// hiding the row. Better than silently dropping audit context.
		return "Subscription updated", "", "", "info"
	case "subscription.item_updated":
		// Item-level plan + quantity changes go through this dedicated
		// action (not AuditActionUpdate) so the metadata discriminator
		// lives in meta["action"]: "item_plan_changed" or
		// "item_quantity_changed".
		a, _ := meta["action"].(string)
		immediate, _ := meta["immediate"].(bool)
		when := "At next period"
		if immediate {
			when = "Immediate"
		}
		switch a {
		case "item_plan_changed":
			parts := []string{}
			oldPlan, _ := meta["old_plan_id"].(string)
			newPlan, _ := meta["new_plan_id"].(string)
			if oldPlan != "" && newPlan != "" {
				parts = append(parts, planLabel(oldPlan, planNames)+" → "+planLabel(newPlan, planNames))
			}
			parts = append(parts, when)
			if pp := prorationPartLabel(meta); pp != "" {
				parts = append(parts, pp)
			}
			return "Plan changed", strings.Join(parts, " · "), "", "info"
		case "item_quantity_changed":
			parts := []string{}
			if q, ok := meta["quantity"].(float64); ok {
				parts = append(parts, fmt.Sprintf("To qty %d", int(q)))
			}
			parts = append(parts, when)
			if pp := prorationPartLabel(meta); pp != "" {
				parts = append(parts, pp)
			}
			return "Quantity changed", strings.Join(parts, " · "), "", "info"
		}
		return "Item updated", "", "", "info"
	case "subscription.pending_change_applied":
		oldPlan, _ := meta["old_plan_id"].(string)
		newPlan, _ := meta["new_plan_id"].(string)
		d := ""
		if oldPlan != "" && newPlan != "" {
			d = planLabel(oldPlan, planNames) + " → " + planLabel(newPlan, planNames)
		}
		return "Scheduled plan change applied", d, "", "success"
	case "subscription.threshold_crossed":
		d := ""
		if num, _ := meta["invoice_number"].(string); num != "" {
			d = "Invoice " + num + " issued early"
			if amt, ok := meta["amount_cents"].(float64); ok {
				cur, _ := meta["currency"].(string)
				d += " — " + formatAmountForTimeline(int64(amt), cur)
			}
		}
		return "Spending threshold crossed", d, "", "warning"
	case "subscription.threshold_deferred":
		// ADR-066 §4b loudness floor. Peak/last-style charges can't be split
		// across an early invoice and the cycle close, so the fire waits.
		return "Spending threshold reached — invoice deferred",
			"This subscription's spend is priced on peak usage, which bills once at the end of the cycle. The invoice will be issued at cycle close.",
			"", "info"
	case "subscription.proration_failed":
		d := ""
		if e, ok := meta["error"].(string); ok && e != "" {
			d = e
		}
		return "Proration failed", d, "", "warning"
	default:
		return action, "", "", "info"
	}
}

// plural returns "s" for n != 1, else "". Tiny helper to avoid the
// "1 thresholds" / "2 threshold" pluralization mistake.
func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
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
			for _, e := range entries {
				desc, detail, detailTS, status := describeSubscriptionAction(e.Action, e.Metadata, planNames)
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
				events = append(events, timelineEvent{
					Timestamp:       e.CreatedAt.UTC().Format(time.RFC3339),
					Source:          "audit",
					EventType:       e.Action,
					Status:          status,
					Description:     desc,
					Detail:          detail,
					DetailTimestamp: detailTS,
					ActorType:       e.ActorType,
					ActorName:       e.ActorName,
					ActorID:         e.ActorID,
					IsSimulated:     simAt != "",
					SimEffectiveAt:  simAt,
					TestClockID:     clockID,
					sortAt:          sortAt,
					recordedAt:      e.CreatedAt,
				})
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
