package creditnote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/sagarsuperuser/velox/internal/api/middleware"
	"github.com/sagarsuperuser/velox/internal/api/respond"
	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
)

// CustomerGetter resolves customer IDs to display names and billing profiles
// for the Bill-To block on the credit note PDF.
type CustomerGetter interface {
	Get(ctx context.Context, tenantID, id string) (domain.Customer, error)
	GetBillingProfile(ctx context.Context, tenantID, customerID string) (domain.CustomerBillingProfile, error)
}

// SettingsGetter reads tenant settings for the company header on the PDF.
type SettingsGetter interface {
	Get(ctx context.Context, tenantID string) (domain.TenantSettings, error)
}

// InvoiceGetter fetches the invoice the credit note was issued against.
// The CN PDF references the invoice number + date, tax country, tax name
// and reverse-charge flag — everything needed to keep the two documents
// legally consistent.
type InvoiceGetter interface {
	Get(ctx context.Context, tenantID, id string) (domain.Invoice, error)
}

// HandlerDeps bundles the PDF-only dependencies so the constructor stays
// small and callers can omit the block when they only need the JSON API.
type HandlerDeps struct {
	Customers CustomerGetter
	Settings  SettingsGetter
	Invoices  InvoiceGetter
}

// EmailSender enqueues the credit-note document email (ADR-082 rider).
// Satisfied by *email.OutboxSender.
type EmailSender interface {
	SendCreditNote(ctx context.Context, tenantID, to string, cc []string, customerName, creditNoteNumber, invoiceNumber string, amountCents int64, currency string, pdfBytes []byte) error
}

type Handler struct {
	svc         *Service
	customers   CustomerGetter
	settings    SettingsGetter
	invoices    InvoiceGetter
	emailSender EmailSender
	auditLogger *audit.Logger
}

func NewHandler(svc *Service, deps ...HandlerDeps) *Handler {
	h := &Handler{svc: svc}
	if len(deps) > 0 {
		h.customers = deps[0].Customers
		h.settings = deps[0].Settings
		h.invoices = deps[0].Invoices
	}
	return h
}

// SetAuditLogger configures audit logging for financial operations.
func (h *Handler) SetAuditLogger(l *audit.Logger) { h.auditLogger = l }

// SetEmailSender wires the outbox-backed CN email path. Without it the
// send endpoint returns 503 email-not-configured semantics (loud, not
// silent).
func (h *Handler) SetEmailSender(s EmailSender) { h.emailSender = s }

func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Post("/", h.create)
	r.Get("/", h.list)
	r.Get("/{id}", h.get)
	r.Get("/{id}/pdf", h.downloadPDF)
	r.Post("/{id}/issue", h.issue)
	r.Post("/{id}/void", h.void)
	r.Post("/{id}/retry-refund", h.retryRefund)
	r.Post("/{id}/send", h.sendEmail)
	return r
}

// createRequest is CreateInput plus the ADR-080 commit-relief block. The
// two shapes are mutually exclusive: commit_relief routes to the dedicated
// single-tx relief coordinator (lines are server-built there); everything
// else is the ordinary line-based create.
type createRequest struct {
	CreateInput
	CommitRelief *CommitReliefInput `json:"commit_relief,omitempty"`
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())

	var req createRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respond.BadRequest(w, r, "invalid JSON body")
		return
	}

	if req.CommitRelief != nil {
		if len(req.Lines) > 0 {
			respond.Validation(w, r, "commit_relief and lines are mutually exclusive — the relief derives its single line from the commit's remaining credits")
			return
		}
		if req.CreditAmountCents > 0 {
			respond.Validation(w, r, "credit_amount_cents cannot be used with commit_relief — paying relief from the customer's balance would refund the very credits being retired")
			return
		}
		relief := *req.CommitRelief
		if relief.InvoiceID == "" {
			relief.InvoiceID = req.InvoiceID
		}
		if relief.Reason == "" {
			relief.Reason = req.Reason
		}
		cn, err := h.svc.CreateAndIssueCommitRelief(r.Context(), tenantID, relief)
		if err != nil {
			respond.FromError(w, r, err, "credit_note")
			return
		}
		respond.JSON(w, r, http.StatusCreated, cn)
		return
	}

	input := req.CreateInput
	cn, err := h.svc.Create(r.Context(), tenantID, input)
	if err != nil {
		respond.FromError(w, r, err, "credit_note")
		return
	}

	// Auto-issue if requested (create + issue in one call)
	if input.AutoIssue {
		issued, err := h.svc.Issue(r.Context(), tenantID, cn.ID)
		if err != nil {
			// CN was created but issue failed — void the draft to avoid orphans.
			// The void is itself a CAS-gated audited flip (svc.Void), so the
			// created-then-voided pair leaves a truthful two-row trail.
			_, _ = h.svc.Void(r.Context(), tenantID, cn.ID)
			respond.FromError(w, r, err, "credit_note")
			return
		}
		// Issue() adds its own `credit_note.issued` row on its coordinator tx
		// (or, when it defers, no row at all — the create row above is then the
		// whole and accurate story).
		respond.JSON(w, r, http.StatusCreated, issued)
		return
	}

	respond.JSON(w, r, http.StatusCreated, cn)
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	page := middleware.ParsePageParams(r)

	cns, err := h.svc.List(r.Context(), ListFilter{
		TenantID:     tenantID,
		InvoiceID:    r.URL.Query().Get("invoice_id"),
		CustomerID:   r.URL.Query().Get("customer_id"),
		Status:       r.URL.Query().Get("status"),
		RefundStatus: r.URL.Query().Get("refund_status"),
		Limit:        page.Limit,
		Offset:       page.Offset,
		Sort:         r.URL.Query().Get("sort"),
		SortDir:      r.URL.Query().Get("dir"),
	})
	if err != nil {
		respond.InternalError(w, r)
		slog.ErrorContext(r.Context(), "list credit notes", "error", err)
		return
	}
	if cns == nil {
		cns = []domain.CreditNote{}
	}

	respond.JSON(w, r, http.StatusOK, map[string]any{"data": cns})
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	id := chi.URLParam(r, "id")

	cn, err := h.svc.Get(r.Context(), tenantID, id)
	if errors.Is(err, errs.ErrNotFound) {
		respond.NotFound(w, r, "credit note")
		return
	}
	if err != nil {
		respond.InternalError(w, r)
		return
	}

	respond.JSON(w, r, http.StatusOK, cn)
}

func (h *Handler) issue(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	id := chi.URLParam(r, "id")

	cn, err := h.svc.Issue(r.Context(), tenantID, id)
	if errors.Is(err, errs.ErrNotFound) {
		respond.NotFound(w, r, "credit note")
		return
	}
	if err != nil {
		respond.FromError(w, r, err, "credit_note")
		return
	}

	// Outcome-conditional (ADR-090): the issued and orphan-voided outcomes each
	// carried an in-tx row, which self-accounts for the request. A DEFERRED
	// outcome (in-flight source invoice) mutated nothing and emits nothing —
	// declare that, or the coverage detector reports a 200 that changed no state
	// as an uncovered mutation forever.
	if cn.Status != domain.CreditNoteIssued && cn.Status != domain.CreditNoteVoided {
		audit.MarkSkip(r.Context())
	}

	respond.JSON(w, r, http.StatusOK, cn)
}

// retryRefund re-attempts the Stripe refund leg of an issued CN whose
// refund_status is failed or pending. Operator-triggered via dashboard
// when the CN was issued during a Stripe outage / before Stripe was
// connected. Uses the same idempotency key as Issue() so retries
// converge cleanly.
func (h *Handler) retryRefund(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	id := chi.URLParam(r, "id")

	cn, err := h.svc.RetryRefund(r.Context(), tenantID, id)
	if errors.Is(err, errs.ErrNotFound) {
		respond.NotFound(w, r, "credit note")
		return
	}
	if err != nil {
		respond.FromError(w, r, err, "credit_note")
		return
	}

	// ADR-090: the operator retry rides its own persist tx (svc.RetryRefund →
	// store UpdateRefundStatusAudited) and emits UNCONDITIONALLY — the
	// audit-worthy fact is the OPERATOR ACTION (a real cash-back request against
	// the customer's payment), not the state transition, so a converged re-drive
	// records a row carrying status_changed=false rather than staying silent.
	// The emission self-accounts for the request.

	respond.JSON(w, r, http.StatusOK, cn)
}

func (h *Handler) void(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	id := chi.URLParam(r, "id")

	cn, err := h.svc.Void(r.Context(), tenantID, id)
	if errors.Is(err, errs.ErrNotFound) {
		respond.NotFound(w, r, "credit note")
		return
	}
	if err != nil {
		respond.FromError(w, r, err, "credit_note")
		return
	}

	// ADR-090: the draft→voided CAS carried its `void` row on the flip's own tx
	// (svc.Void → store TransitionStatusAudited) — a 200 here means the CAS won,
	// so the row exists and self-accounts for the request.

	respond.JSON(w, r, http.StatusOK, cn)
}

// assemblePDFContext gathers the Bill-To, company, and original-invoice
// blocks shared by the download and email paths — extracted (ADR-082)
// so the emailed document can never diverge from the downloaded one,
// the exact drift the invoice send path once had.
func (h *Handler) assemblePDFContext(ctx context.Context, tenantID string, cn domain.CreditNote) (BillToInfo, CompanyInfo, OriginalInvoiceInfo) {
	// Bill-To: prefer billing profile (legal name + full address for tax
	// compliance) and fall back to the customer record.
	bt := BillToInfo{Name: cn.CustomerID}
	if h.customers != nil {
		if cust, err := h.customers.Get(ctx, tenantID, cn.CustomerID); err == nil {
			bt.Name = cust.DisplayName
			bt.Email = cust.Email
		}
		if bp, err := h.customers.GetBillingProfile(ctx, tenantID, cn.CustomerID); err == nil {
			if bp.LegalName != "" {
				bt.Name = bp.LegalName
			}
			// bp.Email removed in migration 0100 — bill-to email tracks
			// customers.email (set above).
			bt.AddressLine1 = bp.AddressLine1
			bt.AddressLine2 = bp.AddressLine2
			bt.City = bp.City
			bt.State = bp.State
			bt.PostalCode = bp.PostalCode
			bt.Country = bp.Country
			bt.TaxID = bp.TaxID
		}
	}

	// Company header from tenant settings.
	var ci CompanyInfo
	if h.settings != nil {
		if ts, err := h.settings.Get(ctx, tenantID); err == nil {
			ci = CompanyInfo{
				Name:         ts.CompanyName,
				Email:        ts.CompanyEmail,
				Phone:        ts.CompanyPhone,
				AddressLine1: ts.CompanyAddressLine1,
				AddressLine2: ts.CompanyAddressLine2,
				City:         ts.CompanyCity,
				State:        ts.CompanyState,
				PostalCode:   ts.CompanyPostalCode,
				Country:      ts.CompanyCountry,
				TaxID:        ts.TaxID,
			}
		}
	}

	// Original invoice reference. Absent the invoice getter we still render
	// the CN — the number lives on the CN itself, so only date/tax metadata
	// is lost in that path.
	var orig OriginalInvoiceInfo
	if h.invoices != nil {
		if inv, err := h.invoices.Get(ctx, tenantID, cn.InvoiceID); err == nil {
			orig = OriginalInvoiceInfo{
				Number:          inv.InvoiceNumber,
				IssuedAt:        inv.IssuedAt,
				Currency:        inv.Currency,
				BillingTimezone: inv.BillingTimezone,
				TaxCountry:      inv.TaxCountry,
				TaxName:         inv.TaxName,
				TaxRate:         inv.TaxRate,
				ReverseCharge:   inv.TaxReverseCharge,
				ExemptReason:    inv.TaxExemptReason,
			}
		}
	}
	return bt, ci, orig
}

func (h *Handler) downloadPDF(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	id := chi.URLParam(r, "id")

	cn, items, err := h.svc.GetWithLineItems(r.Context(), tenantID, id)
	if errors.Is(err, errs.ErrNotFound) {
		respond.NotFound(w, r, "credit note")
		return
	}
	if err != nil {
		respond.InternalError(w, r)
		return
	}

	bt, ci, orig := h.assemblePDFContext(r.Context(), tenantID, cn)

	pdfBytes, err := RenderPDF(r.Context(), cn, items, orig, bt, ci)
	if err != nil {
		slog.ErrorContext(r.Context(), "render credit note pdf", "credit_note_id", id, "error", err)
		respond.InternalError(w, r)
		return
	}

	filename := cn.CreditNoteNumber
	if filename == "" {
		filename = cn.ID
	}
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", "inline; filename=\""+filename+".pdf\"")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(pdfBytes)
}

// sendEmail emails the credit-note document with PDF attached (ADR-082
// rider — CNs previously had NO send surface at all: engine-auto-issued
// clawback CNs and card-refund CNs moved real money with no document
// reaching the customer). Issued-only: drafts aren't final documents
// and voided CNs are retracted ones.
func (h *Handler) sendEmail(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	id := chi.URLParam(r, "id")

	if h.emailSender == nil {
		respond.FromError(w, r, errs.InvalidState("credit-note email is not configured on this deployment"), "credit_note_email")
		return
	}

	var body struct {
		Email string `json:"email"`
		// AdditionalEmails: tri-state override, same contract as the
		// invoice send (absent → customer's stored list; [] → primary
		// only; list → validated exact override).
		AdditionalEmails *[]string `json:"additional_emails,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Email == "" {
		respond.BadRequest(w, r, "email is required")
		return
	}

	cn, items, err := h.svc.GetWithLineItems(r.Context(), tenantID, id)
	if errors.Is(err, errs.ErrNotFound) {
		respond.NotFound(w, r, "credit note")
		return
	}
	if err != nil {
		respond.InternalError(w, r)
		return
	}
	switch cn.Status {
	case domain.CreditNoteIssued:
		// sendable
	case domain.CreditNoteDraft:
		respond.Validation(w, r, "issue the credit note before sending — drafts are not final documents")
		return
	default:
		respond.Validation(w, r, "a voided credit note cannot be sent")
		return
	}

	// CC resolution (ADR-082 tri-state).
	var cc []string
	if body.AdditionalEmails != nil {
		cc, err = domain.NormalizeAdditionalEmails(*body.AdditionalEmails, body.Email)
		if err != nil {
			respond.FromError(w, r, err, "credit_note_email")
			return
		}
	} else if h.customers != nil {
		cust, err := h.customers.Get(r.Context(), tenantID, cn.CustomerID)
		if err != nil {
			respond.FromError(w, r, fmt.Errorf("resolve customer additional emails: %w", err), "credit_note_email")
			return
		}
		for _, a := range cust.AdditionalEmails {
			if !strings.EqualFold(a, strings.TrimSpace(body.Email)) {
				cc = append(cc, a)
			}
		}
	}

	// Operator entry point on a clock-pinned entity (ADR-030): bind from
	// the SOURCE INVOICE — the same rule the credit note's own timestamps
	// follow — so the enqueued email carries the invoice's billing anchor
	// (ADR-104) instead of enqueuing unanchored.
	ctx := h.svc.bindForInvoice(r.Context(), tenantID, cn.InvoiceID)

	bt, ci, orig := h.assemblePDFContext(ctx, tenantID, cn)
	pdfBytes, err := RenderPDF(ctx, cn, items, orig, bt, ci)
	if err != nil {
		slog.ErrorContext(ctx, "render credit note pdf for email", "credit_note_id", id, "error", err)
		respond.InternalError(w, r)
		return
	}

	if err := h.emailSender.SendCreditNote(ctx, tenantID, body.Email, cc,
		bt.Name, cn.CreditNoteNumber, orig.Number, cn.TotalCents, cn.Currency, pdfBytes); err != nil {
		respond.FromError(w, r, err, "credit_note_email")
		return
	}

	// Same GDPR convention as the invoice send: record THAT it was sent,
	// never the recipient addresses (audit_log is append-only).
	if h.auditLogger != nil {
		_ = h.auditLogger.Log(r.Context(), tenantID, domain.AuditActionSend, "credit_note", cn.ID, cn.CreditNoteNumber, map[string]any{
			"credit_note_number": cn.CreditNoteNumber,
			"invoice_id":         cn.InvoiceID,
			"customer_id":        cn.CustomerID,
		})
	}

	respond.JSON(w, r, http.StatusOK, map[string]string{"status": "sent"})
}
