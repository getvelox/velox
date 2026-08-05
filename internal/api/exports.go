package api

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/sagarsuperuser/velox/internal/api/respond"
	"github.com/sagarsuperuser/velox/internal/api/timefilter"
	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/subscription"
	"github.com/sagarsuperuser/velox/internal/usage"
)

// Sprint 2 — data export. Streaming CSV endpoints so a tenant can
// take their data out, clearing the "I take my data" review during
// procurement. RLS-scoped via the standard Bearer-key auth chain.
//
// Each endpoint issues its OWN query (StreamForExport on its store), NOT the
// store's List method. Borrowing List is what this code used to do, and it is
// how two columns the CSVs promised — customers.email_status and
// usage_events.origin — came to be blank in every export ever taken: List's
// SELECT does not carry them. An export's columns are its own contract.
//
// Streaming: ONE snapshot transaction per export, one query, rows handed to the
// csv.Writer as they are scanned and flushed every exportPageSize rows — so the
// HTTP buffer never holds the full result set and operators can export millions
// of rows without OOM.
//
// The snapshot is the correctness property, not an optimization. These exports
// used to page each store with LIMIT/OFFSET, every page in its OWN transaction,
// over a newest-first ordering. A row inserted while the export ran entered at
// the HEAD and shifted every later row down by one — so the row on each page
// boundary was written to the CSV TWICE, and a delete symmetrically SKIPPED one.
// The usage export is given a five-minute timeout precisely BECAUSE it streams a
// lot of rows, which is exactly the window in which concurrent ingest is
// guaranteed; a duplicated usage row in a finance CSV reads as usage that did
// not happen. Under a single MVCC snapshot the question cannot arise. (#475)
//
// Date filters: the four DATA exports accept ?from=<RFC3339>&to=<RFC3339>, and
// usage_events REQUIRES both (the table is unbounded — without a range, an
// export would walk the entire history).
//
// The audit-log export is the exception: it takes ?date_from / ?date_to, because
// its params mirror the audit-log READ route so the dashboard's on-screen filters
// and its Export produce the same set. Passing ?from/?to there is silently
// ignored and you get the whole log — a footgun worth knowing about rather than
// papering over, since the two routes sit side by side in the same handler.
//
// AUDIT (ADR-090 §7): every export here emits an action=export audit row
// BEFORE the first byte streams, and fails closed if it cannot. This is the
// only READ path in Velox that writes to audit_log — see auditExport for why
// the ordering is load-bearing.

const (
	// exportPageSize is the FLUSH INTERVAL — rows written to the csv.Writer
	// between flushes to the wire. It is no longer a page size: nothing
	// paginates any more. Every export (including audit-log.csv) walks one
	// snapshot cursor, so this number now only trades HTTP buffer size against
	// syscalls, and no correctness depends on it.
	exportPageSize = 100

	// usageExportMaxSpanDays caps the date range for usage_events
	// exports. Larger spans should be split into multiple calls so a
	// single export doesn't lock the table.
	usageExportMaxSpanDays = 366
)

// exportAuditor writes the export's own audit row. Own-tx by necessity: an
// export is a READ — there is no business transaction for ADR-090's LogInTx to
// ride. *audit.Logger satisfies it (its Log detaches from the caller's
// cancellation, which is exactly the property this path needs).
type exportAuditor interface {
	Log(ctx context.Context, tenantID, action, resourceType, resourceID, resourceLabel string, metadata map[string]any) error
}

// auditStreamer reads the audit log itself, unbounded, for the audit-log CSV
// export. *audit.Logger satisfies it.
type auditStreamer interface {
	Stream(ctx context.Context, tenantID string, filter audit.QueryFilter, fn func(domain.AuditEntry) error) error
}

type exportsHandler struct {
	customers     *customer.PostgresStore
	invoices      *invoice.PostgresStore
	subscriptions *subscription.PostgresStore
	usage         *usage.PostgresStore
	audit         exportAuditor
	auditLog      auditStreamer
}

func newExportsHandler(
	c *customer.PostgresStore,
	i *invoice.PostgresStore,
	s *subscription.PostgresStore,
	u *usage.PostgresStore,
	auditor exportAuditor,
	auditLog auditStreamer,
) *exportsHandler {
	return &exportsHandler{
		customers:     c,
		invoices:      i,
		subscriptions: s,
		usage:         u,
		audit:         auditor,
		auditLog:      auditLog,
	}
}

func (h *exportsHandler) Routes(customerRead, invoiceRead, subscriptionRead, usageRead, auditRead func(http.Handler) http.Handler) chi.Router {
	r := chi.NewRouter()
	r.With(customerRead).Get("/customers.csv", h.exportCustomers)
	r.With(invoiceRead).Get("/invoices.csv", h.exportInvoices)
	r.With(subscriptionRead).Get("/subscriptions.csv", h.exportSubscriptions)
	r.With(usageRead).Get("/usage-events.csv", h.exportUsageEvents)
	// The audit log exports itself. Same permission as the audit-log READ route
	// (auth.PermAPIKeyRead — see the /v1/audit-log mount in router.go): the file
	// is the same data, so gating it differently would either lock out operators
	// who can already read it on screen or hand it to ones who can't.
	r.With(auditRead).Get("/audit-log.csv", h.exportAuditLog)
	return r
}

// parseDateRange reads ?from / ?to off the request. Thin wrapper around
// timefilter.ParseRange so exports share the same date-filter contract as
// audit-log queries + usage queries — RFC3339 OR YYYY-MM-DD, both accepted
// everywhere. Returns zero time.Time when missing.
//
// The range is applied in SQL, by each store's StreamForExport. It used to be
// applied in Go AFTER the page was fetched, which meant a filtered export still
// walked every row in the tenant; nothing filters post-load any more.
func parseDateRange(r *http.Request) (from, to time.Time, err error) {
	return timefilter.ParseRange(r, "from", "to")
}

// exportFilename is the download name, timestamped so re-exports don't clobber.
// Built BEFORE the audit row so the row records the exact filename the operator
// received — that string is how a file found later gets traced back to the
// export that produced it.
func exportFilename(stem string) string {
	// UTC stamp — must be stable/unambiguous, not vary by operator TZ.
	return fmt.Sprintf("%s-%s.csv", stem, time.Now().UTC().Format("20060102-150405")) //tz:ok
}

// writeCSVHeaders sets the response headers for a CSV download.
func writeCSVHeaders(w http.ResponseWriter, filename string) *csv.Writer {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename=%q`, filename))
	w.Header().Set("Cache-Control", "no-store")
	return csv.NewWriter(w)
}

// auditExport records a bulk data egress — and is the gate the export must pass
// before it may stream a single byte.
//
// ORDER IS THE WHOLE POINT (and it is why this is not a defer):
//
//   - Emit-then-stream can only ever OVER-record: an export that is audited and
//     then fails mid-file leaves a row for a file the operator never fully got.
//     A reader can reconcile that against the EXPORT_INCOMPLETE marker.
//   - Stream-then-emit UNDER-records, and that is unrecoverable. Killing the
//     connection mid-stream (or a 5-minute timeout firing, or the process being
//     restarted) means pages of customer PII have already left the building and
//     the completion row is never written. audit_log would say nothing happened.
//     In an append-only compliance log there is no second chance to write it.
//
// So the row records the ATTEMPT — an operator asked this system to hand them
// the tenant's customers/invoices/subscriptions/usage/audit log — which is the
// fact a chain of custody actually needs. FAIL CLOSED: if the row cannot be
// written, the handler returns 5xx and streams NOTHING. An export we cannot
// record is an export we do not perform.
//
// Own-tx: an export is a read, so there is no business transaction to join
// (ADR-090's shared-fate LogInTx needs one). audit.Logger.Log detaches from the
// caller's cancellation (context.WithoutCancel), so a client that hangs up the
// instant the export starts still leaves the row behind.
//
// The row deliberately does NOT carry a row count: the count is unknowable
// before the first page is fetched, and a number we cannot honour is a lie in a
// permanent record. resource_id is empty for the same reason — a bulk export has
// no single subject.
func (h *exportsHandler) auditExport(r *http.Request, tenantID, resourceType, filename string, filters map[string]any) error {
	return h.audit.Log(r.Context(), tenantID, domain.AuditActionExport, resourceType, "", "",
		map[string]any{
			"format":   "csv",
			"filename": filename,
			"filters":  filters,
		})
}

// exportScope describes WHAT the export selected, for the audit row's metadata.
// An empty date filter is recorded explicitly as "all" — "the operator took the
// entire table" is precisely the fact worth having, and an absent key would read
// as a missing detail rather than an unfiltered dump.
func exportScope(from, to time.Time) map[string]any {
	if from.IsZero() && to.IsZero() {
		return map[string]any{"date_range": "all"}
	}
	m := map[string]any{}
	if !from.IsZero() {
		m["from"] = from.UTC().Format(time.RFC3339)
	}
	if !to.IsZero() {
		m["to"] = to.UTC().Format(time.RFC3339)
	}
	return m
}

// csvSafe neutralizes spreadsheet (CSV) formula injection. A cell whose first
// character is one of = + - @ TAB CR is interpreted as a formula by Excel,
// Google Sheets, and LibreOffice — so a customer-controlled value like
// "=HYPERLINK(...)" or "@SUM(...)" executes when the operator opens the export.
// Prefixing such values with a single quote forces them to render as text.
// Empty strings pass through. Applied to free-text, externally-controlled
// columns (display names, external IDs, emails, codes, idempotency keys).
//
// The property being protected: the CSV is the artifact an operator hands an
// AUDITOR. A file that executes code when opened is not evidence — and the
// audit-log export makes this sharper still, because customer display names ride
// into it through resource_label.
func csvSafe(s string) string {
	if s == "" {
		return s
	}
	switch s[0] {
	case '=', '+', '-', '@', 0x09, 0x0d:
		return "'" + s
	}
	return s
}

// flushAndContinue flushes the csv buffer to the underlying response
// so streaming clients see partial output. Without the flush, large
// exports buffer until completion.
func flushAndContinue(cw *csv.Writer, w http.ResponseWriter) error {
	cw.Flush()
	if err := cw.Error(); err != nil {
		return err
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}

// exportAbort makes a mid-stream failure visible IN the file. The HTTP
// status is already 200 by the time a store error or ctx timeout hits
// (headers went out with the first row), so the stream used to just
// END — a silently truncated CSV indistinguishable from a complete
// export, feeding partial books into whatever the operator reconciles
// against. The trailing EXPORT_INCOMPLETE record is the honest,
// machine-checkable signal (documented contract: a row whose first
// cell is EXPORT_INCOMPLETE means discard the file and retry), plus a
// server-side error log. Best-effort by construction: if the client
// connection itself died, the marker write fails silently — that
// client isn't reading anyway.
//
// The audit row is already written by this point (auditExport runs before the
// stream), so an aborted export still leaves evidence that data was requested
// and partially handed over — which is the honest record of what happened.
func exportAbort(ctx context.Context, cw *csv.Writer, resource string, err error) {
	slog.ErrorContext(ctx, "csv export aborted mid-stream — emitted EXPORT_INCOMPLETE marker",
		"resource", resource, "error", err)
	_ = cw.Write([]string{"EXPORT_INCOMPLETE", "the export aborted before completion — discard this file and retry"})
	cw.Flush()
}

// exportAuditFailed answers a request whose audit row could not be written. No
// CSV headers have been set and no bytes written, so this is a clean 500 — the
// export simply does not happen. audit.Logger.Log has already logged the cause
// and incremented velox_audit_write_errors_total.
func exportAuditFailed(w http.ResponseWriter, r *http.Request, resourceType string, err error) {
	slog.ErrorContext(r.Context(), "export REFUSED — its audit row could not be written (fail-closed)",
		"resource", resourceType, "error", err)
	respond.InternalError(w, r)
}

// timePtrCSV formats a *time.Time as RFC3339 or empty string. Used
// for nullable columns (canceled_at, paid_at, voided_at, etc).
func timePtrCSV(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// ---- Customers ----

func (h *exportsHandler) exportCustomers(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	if tenantID == "" {
		respond.Unauthorized(w, r, "missing tenant context")
		return
	}
	from, to, err := parseDateRange(r)
	if err != nil {
		respond.FromError(w, r, err, "export")
		return
	}

	filename := exportFilename("customers")
	if err := h.auditExport(r, tenantID, "customer", filename, exportScope(from, to)); err != nil {
		exportAuditFailed(w, r, "customer", err)
		return
	}

	cw := writeCSVHeaders(w, filename)
	defer cw.Flush()

	if err := cw.Write([]string{
		"id", "external_id", "display_name", "email", "status",
		"email_status", "created_at", "updated_at",
	}); err != nil {
		return
	}

	n := 0
	err = h.customers.StreamForExport(r.Context(), tenantID, from, to, func(c domain.Customer) error {
		if err := cw.Write([]string{
			c.ID, csvSafe(c.ExternalID), csvSafe(c.DisplayName), csvSafe(c.Email),
			string(c.Status),
			string(c.EmailStatus),
			c.CreatedAt.UTC().Format(time.RFC3339),
			c.UpdatedAt.UTC().Format(time.RFC3339),
		}); err != nil {
			return err
		}
		n++
		if n%exportPageSize == 0 {
			return flushAndContinue(cw, w)
		}
		return nil
	})
	if err != nil {
		exportAbort(r.Context(), cw, "customers", err)
		return
	}
	// Flush the final partial batch to the SOCKET. The periodic flush above only
	// fires on an exportPageSize boundary, so without this an export smaller than
	// one batch would never reach http.Flusher — and the deferred cw.Flush() only
	// pushes the csv.Writer into the ResponseWriter, not the ResponseWriter onto
	// the wire.
	_ = flushAndContinue(cw, w)
}

// ---- Invoices ----

func (h *exportsHandler) exportInvoices(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	if tenantID == "" {
		respond.Unauthorized(w, r, "missing tenant context")
		return
	}
	from, to, err := parseDateRange(r)
	if err != nil {
		respond.FromError(w, r, err, "export")
		return
	}

	filename := exportFilename("invoices")
	if err := h.auditExport(r, tenantID, "invoice", filename, exportScope(from, to)); err != nil {
		exportAuditFailed(w, r, "invoice", err)
		return
	}

	cw := writeCSVHeaders(w, filename)
	defer cw.Flush()

	if err := cw.Write([]string{
		"id", "invoice_number", "customer_id", "subscription_id",
		"status", "payment_status", "currency",
		"subtotal_cents", "tax_amount_cents", "discount_cents",
		"total_amount_cents", "amount_due_cents", "amount_paid_cents",
		"credits_applied_cents",
		"billing_period_start", "billing_period_end",
		// uncollectible_at sits beside its siblings: a written-off invoice that
		// is later RECOVERED reaches status='paid', and without this column the
		// row is byte-identical to one that was never written off. Bad-debt
		// recovery is a distinct accounting event; the export was the only
		// durable place an accountant could locate one, and it could not.
		// tax_provider + tax_transaction_id are the RELIEF HATCH (ADR-111).
		// Velox no longer reverses tax on write-off, so claiming bad-debt
		// relief is the tenant's own conditioned claim (UK: 6 months + written
		// off to a separate bad-debt account; CA/NY/TX: charged off for
		// federal income tax purposes). Every other input those tests need is
		// already here — status, tax_amount_cents, uncollectible_at, due_at,
		// paid_at, amount_paid_cents. What was missing is whether Velox had
		// ALREADY reversed at the provider, which is the one fact deciding
		// whether the tenant's own claim would be a double claim.
		"tax_provider", "tax_transaction_id", "tax_reversed_at",
		"issued_at", "due_at", "paid_at", "voided_at", "uncollectible_at",
		"created_at",
	}); err != nil {
		return
	}

	n := 0
	err = h.invoices.StreamForExport(r.Context(), tenantID, from, to, func(inv domain.Invoice) error {
		if err := cw.Write([]string{
			inv.ID, csvSafe(inv.InvoiceNumber), inv.CustomerID, inv.SubscriptionID,
			string(inv.Status), string(inv.PaymentStatus), inv.Currency,
			strconv.FormatInt(inv.SubtotalCents, 10),
			strconv.FormatInt(inv.TaxAmountCents, 10),
			strconv.FormatInt(inv.DiscountCents, 10),
			strconv.FormatInt(inv.TotalAmountCents, 10),
			strconv.FormatInt(inv.AmountDueCents, 10),
			strconv.FormatInt(inv.AmountPaidCents, 10),
			strconv.FormatInt(inv.CreditsAppliedCents, 10),
			inv.BillingPeriodStart.UTC().Format(time.RFC3339),
			inv.BillingPeriodEnd.UTC().Format(time.RFC3339),
			csvSafe(inv.TaxProvider),
			csvSafe(inv.TaxTransactionID),
			timePtrCSV(inv.TaxReversedAt),
			timePtrCSV(inv.IssuedAt),
			timePtrCSV(inv.DueAt),
			timePtrCSV(inv.PaidAt),
			timePtrCSV(inv.VoidedAt),
			timePtrCSV(inv.UncollectibleAt),
			inv.CreatedAt.UTC().Format(time.RFC3339),
		}); err != nil {
			return err
		}
		n++
		if n%exportPageSize == 0 {
			return flushAndContinue(cw, w)
		}
		return nil
	})
	if err != nil {
		exportAbort(r.Context(), cw, "invoices", err)
		return
	}
	// Flush the final partial batch to the SOCKET. The periodic flush above only
	// fires on an exportPageSize boundary, so without this an export smaller than
	// one batch would never reach http.Flusher — and the deferred cw.Flush() only
	// pushes the csv.Writer into the ResponseWriter, not the ResponseWriter onto
	// the wire.
	_ = flushAndContinue(cw, w)
}

// ---- Subscriptions ----

func (h *exportsHandler) exportSubscriptions(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	if tenantID == "" {
		respond.Unauthorized(w, r, "missing tenant context")
		return
	}
	from, to, err := parseDateRange(r)
	if err != nil {
		respond.FromError(w, r, err, "export")
		return
	}

	filename := exportFilename("subscriptions")
	if err := h.auditExport(r, tenantID, "subscription", filename, exportScope(from, to)); err != nil {
		exportAuditFailed(w, r, "subscription", err)
		return
	}

	cw := writeCSVHeaders(w, filename)
	defer cw.Flush()

	if err := cw.Write([]string{
		"id", "code", "display_name", "customer_id",
		"status", "billing_time",
		"trial_start_at", "trial_end_at",
		"started_at", "activated_at", "canceled_at",
		"current_billing_period_start", "current_billing_period_end",
		"next_billing_at",
		"plan_ids", // pipe-delimited list of plans on the sub
		"created_at",
	}); err != nil {
		return
	}

	// planIDs arrives pipe-joined from the query's aggregate — the store no
	// longer hydrates every subscription's items just so the export can join
	// their plan ids into one string.
	n := 0
	err = h.subscriptions.StreamForExport(r.Context(), tenantID, from, to, func(sub domain.Subscription, planIDs string) error {
		if err := cw.Write([]string{
			sub.ID, csvSafe(sub.Code), csvSafe(sub.DisplayName), sub.CustomerID,
			string(sub.Status), string(sub.BillingTime),
			timePtrCSV(sub.TrialStartAt), timePtrCSV(sub.TrialEndAt),
			timePtrCSV(sub.StartedAt), timePtrCSV(sub.ActivatedAt),
			timePtrCSV(sub.CanceledAt),
			timePtrCSV(sub.CurrentBillingPeriodStart),
			timePtrCSV(sub.CurrentBillingPeriodEnd),
			timePtrCSV(sub.NextBillingAt),
			planIDs,
			sub.CreatedAt.UTC().Format(time.RFC3339),
		}); err != nil {
			return err
		}
		n++
		if n%exportPageSize == 0 {
			return flushAndContinue(cw, w)
		}
		return nil
	})
	if err != nil {
		exportAbort(r.Context(), cw, "subscriptions", err)
		return
	}
	// Flush the final partial batch to the SOCKET. The periodic flush above only
	// fires on an exportPageSize boundary, so without this an export smaller than
	// one batch would never reach http.Flusher — and the deferred cw.Flush() only
	// pushes the csv.Writer into the ResponseWriter, not the ResponseWriter onto
	// the wire.
	_ = flushAndContinue(cw, w)
}

// ---- Usage events ----

func (h *exportsHandler) exportUsageEvents(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	if tenantID == "" {
		respond.Unauthorized(w, r, "missing tenant context")
		return
	}
	from, to, err := parseDateRange(r)
	if err != nil {
		respond.FromError(w, r, err, "export")
		return
	}
	// Required range — this table is unbounded.
	if from.IsZero() || to.IsZero() {
		respond.BadRequest(w, r, "usage-events export requires `from` and `to` query params (RFC3339)")
		return
	}
	if to.Sub(from) > usageExportMaxSpanDays*24*time.Hour {
		respond.BadRequest(w, r, fmt.Sprintf("date range exceeds %d days — split into multiple calls", usageExportMaxSpanDays))
		return
	}

	filename := exportFilename("usage-events")
	if err := h.auditExport(r, tenantID, "usage_event", filename, exportScope(from, to)); err != nil {
		exportAuditFailed(w, r, "usage_event", err)
		return
	}

	cw := writeCSVHeaders(w, filename)
	defer cw.Flush()

	// provider_cost_* columns (ADR-079): the operator-warehouse margin
	// join is the verified peer workflow (Metronome exports events with
	// cost metadata for exactly this) — the in-app margin card covers the
	// common case; the CSV keeps finance reconciliation possible.
	if err := cw.Write([]string{
		"id", "customer_id", "meter_id",
		"quantity", "timestamp",
		"idempotency_key", "origin",
		"dimensions_json",
		"provider_cost_micros", "provider_cost_source",
	}); err != nil {
		return
	}

	n := 0
	err = h.usage.StreamForExport(r.Context(), tenantID, from, to, func(ev domain.UsageEvent) error {
		dimsJSON := ""
		if len(ev.Dimensions) > 0 {
			if b, err := json.Marshal(ev.Dimensions); err == nil {
				dimsJSON = string(b)
			}
		}
		costMicros := ""
		if ev.ProviderCostMicros != nil {
			costMicros = fmt.Sprintf("%d", *ev.ProviderCostMicros)
		}
		if err := cw.Write([]string{
			ev.ID, ev.CustomerID, ev.MeterID,
			ev.Quantity.String(),
			ev.Timestamp.UTC().Format(time.RFC3339),
			csvSafe(ev.IdempotencyKey),
			string(ev.Origin),
			dimsJSON,
			costMicros, ev.ProviderCostSource,
		}); err != nil {
			return err
		}
		n++
		if n%exportPageSize == 0 {
			return flushAndContinue(cw, w)
		}
		return nil
	})
	if err != nil {
		exportAbort(r.Context(), cw, "usage_events", err)
		return
	}
	// Flush the final partial batch to the SOCKET. The periodic flush above only
	// fires on an exportPageSize boundary, so without this an export smaller than
	// one batch would never reach http.Flusher — and the deferred cw.Flush() only
	// pushes the csv.Writer into the ResponseWriter, not the ResponseWriter onto
	// the wire.
	_ = flushAndContinue(cw, w)
}

// ---- Audit log ----

// exportAuditLog streams the tenant's audit log as CSV — the compliance evidence
// pack, produced SERVER-SIDE.
//
// It replaces a dashboard Export button that paged /v1/audit-log in the browser
// and stopped at 50,000 rows: a silent truncation of the evidence itself. The
// operator got a file that looked complete, and nothing in it said otherwise.
// audit.Logger.Stream applies no cap — the export is bounded by its filters and
// nothing else.
//
// The export audits itself: the row is written BEFORE the stream begins, so it is
// always in the LOG. "Who took a copy of the audit log" is the one question a
// tamper-evidence system must never be unable to answer about itself.
//
// Whether it is in the FILE is a different claim, and a weaker one: an UNFILTERED
// export contains its own row (the snapshot is taken after the write, newest
// first, so it is the first data row) — but any filter that does not match it
// excludes it, exactly as it would exclude any other row. A clock-scoped export
// (?test_clock_id=) NEVER contains it: the export row is a wall-clock row with
// NULL sim columns. That is correct behavior, not a hole — the row is in the log
// either way — but the file is not self-certifying in general, and three places
// used to say it was.
func (h *exportsHandler) exportAuditLog(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	if tenantID == "" {
		respond.Unauthorized(w, r, "missing tenant context")
		return
	}
	// Same param names + parsing as the audit-log READ route (audit.Handler.list),
	// so the dashboard's on-screen filters and its Export produce the same set.
	dateFrom, dateTo, err := timefilter.ParseRange(r, "date_from", "date_to")
	if err != nil {
		respond.FromError(w, r, err, "export")
		return
	}
	// The sim axis (ADR-090 §5) is part of the on-screen slice: an operator
	// looking at one simulation and clicking Export must get THAT simulation,
	// not the whole log. An export that silently dropped the clock filter would
	// hand an auditor a file that looks like one clock's history and is actually
	// every tenant action — the worst kind of wrong, because nothing in the file
	// says so.
	simFrom, simTo, err := timefilter.ParseRange(r, "sim_from", "sim_to")
	if err != nil {
		respond.FromError(w, r, err, "export")
		return
	}
	q := r.URL.Query()
	filter := audit.QueryFilter{
		ResourceType: q.Get("resource_type"),
		ResourceID:   q.Get("resource_id"),
		Action:       q.Get("action"),
		ActorID:      q.Get("actor_id"),
		DateFrom:     dateFrom,
		DateTo:       dateTo,
		TestClockID:  q.Get("test_clock_id"),
		SimFrom:      simFrom,
		SimTo:        simTo,
	}

	scope := exportScope(dateFrom, dateTo)
	for k, v := range map[string]string{
		"resource_type": filter.ResourceType,
		"resource_id":   filter.ResourceID,
		"action":        filter.Action,
		"actor_id":      filter.ActorID,
		// Recorded on the export's OWN audit row: "which slice left the
		// building" is the question this row exists to answer.
		"test_clock_id": filter.TestClockID,
	} {
		if v != "" {
			scope[k] = v
		}
	}
	if !simFrom.IsZero() {
		scope["sim_from"] = simFrom.UTC().Format(time.RFC3339)
	}
	if !simTo.IsZero() {
		scope["sim_to"] = simTo.UTC().Format(time.RFC3339)
	}

	filename := exportFilename("audit-log")
	if err := h.auditExport(r, tenantID, "audit_log", filename, scope); err != nil {
		exportAuditFailed(w, r, "audit_log", err)
		return
	}

	cw := writeCSVHeaders(w, filename)
	defer cw.Flush()

	// sim_effective_at / test_clock_id: a simulation's rows all share one
	// wall-clock created_at (an advance settles everything at one moment), so in
	// the CSV of a simulated tenant created_at cannot tell the reader WHEN, in
	// the simulation, anything happened. These two columns are the only ones
	// that can, and after ADR-086 teardown they are the only surviving record
	// that the clock existed at all. Empty on wall-clock rows.
	if err := cw.Write([]string{
		"id", "created_at", "actor_type", "actor_id", "actor_name",
		"action", "resource_type", "resource_id", "resource_label",
		"ip_address", "request_id", "sim_effective_at", "test_clock_id",
		"metadata_json",
	}); err != nil {
		return
	}

	n := 0
	streamErr := h.auditLog.Stream(r.Context(), tenantID, filter, func(e domain.AuditEntry) error {
		simAt := ""
		if e.SimEffectiveAt != nil {
			simAt = e.SimEffectiveAt.UTC().Format(time.RFC3339)
		}
		metaJSON := ""
		if len(e.Metadata) > 0 {
			if b, err := json.Marshal(e.Metadata); err == nil {
				metaJSON = string(b)
			}
		}
		// csvSafe every column that is not a Velox-minted id or a closed
		// vocabulary. resource_label carries customer display names and
		// plan/subscription codes; actor_name is a customer's display name on
		// customer-actor rows; metadata is a free-form bag.
		//
		// request_id is neutralized too, and deliberately so. It is
		// server-minted TODAY (ADR-090 §6 replaced chi's middleware, which
		// copied an inbound X-Request-Id verbatim) — but audit_log is
		// APPEND-ONLY, so every row written before that change still carries
		// whatever string its caller chose, including a live formula. The
		// column cannot be cleaned retroactively; it can only be rendered
		// safely.
		//
		// ip_address gets the SAME treatment, for the SAME reason — an earlier
		// version of this comment exempted it ("TrustedRealIP only ever yields a
		// net.ParseIP-validated value"), which is an argument about what the code
		// writes TODAY and therefore exactly the argument the append-only rule
		// defeats. The log outlives its writers. csvSafe on a well-formed IP is a
		// no-op (no address begins with = + - @), so the exemption bought nothing
		// and cost the one guarantee that has to hold for a permanent record.
		if err := cw.Write([]string{
			e.ID,
			e.CreatedAt.UTC().Format(time.RFC3339),
			e.ActorType, e.ActorID, csvSafe(e.ActorName),
			e.Action, e.ResourceType, e.ResourceID, csvSafe(e.ResourceLabel),
			csvSafe(e.IPAddress), csvSafe(e.RequestID),
			simAt, e.TestClockID,
			csvSafe(metaJSON),
		}); err != nil {
			return err
		}
		n++
		if n%exportPageSize == 0 {
			return flushAndContinue(cw, w)
		}
		return nil
	})
	if streamErr != nil {
		exportAbort(r.Context(), cw, "audit_log", streamErr)
		return
	}
	_ = flushAndContinue(cw, w)
}
