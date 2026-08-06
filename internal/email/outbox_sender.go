package email

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// Email type tags written into email_outbox.email_type. Kept stable because
// they live in row data; adding new types is append-only.
//
// payment_setup_request: customer needs to set up a payment method on a
//
//	finalized invoice (no PM on file at finalize). Distinct from
//	payment_failed which means a charge attempt actually went to Stripe
//	and was declined.
//
// payment_failed: a Stripe charge attempt failed (decline, insufficient
//
//	funds, etc.). Used for both immediate post-decline notifications
//	AND dunning retry escalations.
const (
	TypeInvoice             = "invoice"
	TypePaymentReceipt      = "payment_receipt"
	TypeDunningWarning      = "dunning_warning"
	TypeDunningEscalation   = "dunning_escalation"
	TypePaymentFailed       = "payment_failed"
	TypePaymentSetupRequest = "payment_setup_request"
	// TypePaymentSetupLink is the operator-initiated "add a payment
	// method" email (operator clicks Send setup email on a customer
	// page). Distinct from TypePaymentSetupRequest which fires from
	// the engine at invoice finalize when no PM is on file — that
	// one carries invoice context, this one doesn't. Both render via
	// the same unified template (renderPaymentSetupLinkHTML); the
	// type tag is purely for routing + observability.
	TypePaymentSetupLink = "payment_setup_link"
	TypePasswordReset    = "password_reset"
	TypeMemberInvite     = "member_invite"
	// TypeCreditNote is the operator-initiated credit-note document
	// email (ADR-082 rider). No CHECK constraint exists on
	// email_outbox.email_type, so appending a type needs no migration.
	TypeCreditNote = "credit_note"
)

// outboxMessage is the union payload persisted to email_outbox.payload. Each
// email_type populates the subset of fields it needs; unused fields stay zero.
// Keeping it a single struct (not a tagged union of N structs) avoids per-type
// serialisation ceremony — the dispatcher reads the type tag and knows which
// fields are meaningful.
type outboxMessage struct {
	To string `json:"to"`
	// Cc carries the additional billing recipients (ADR-082) for the
	// CC-eligible types. Old rows without the key decode to nil —
	// primary-only delivery, degraded-but-correct during binary skew.
	Cc            []string `json:"cc,omitempty"`
	CustomerName  string   `json:"customer_name,omitempty"`
	InvoiceNumber string   `json:"invoice_number,omitempty"`
	// CreditNoteNumber names the CN document on TypeCreditNote rows
	// (InvoiceNumber then carries the APPLIED invoice for the body copy).
	CreditNoteNumber string `json:"credit_note_number,omitempty"`
	AmountCents      int64  `json:"amount_cents,omitempty"`
	Currency         string `json:"currency,omitempty"`
	// Trigger names WHY this row was enqueued, for rows whose cause an
	// operator cannot infer from the row itself. Two setup-link emails on
	// one invoice — the finalize-time one and an operator's Resend — were
	// byte-identical on the timeline, so "why did this happen twice?" had
	// no answer on the page. The cause is already recorded in the audit
	// log; this carries it onto the row that displays it, the same way
	// AttemptNumber carries the pairing key for dunning reminders.
	// Vocabulary matches the notifier's trigger argument
	// (finalize_no_pm / auto_charge_retry_no_pm / operator_resend).
	Trigger       string `json:"trigger,omitempty"`
	AttemptNumber int    `json:"attempt_number,omitempty"`
	MaxAttempts   int    `json:"max_attempts,omitempty"`
	NextRetryDate string `json:"next_retry_date,omitempty"`
	Reason        string `json:"reason,omitempty"`
	// FailureReason carries the latest decline-or-error message for
	// dunning_warning + payment_failed templates. Surfaced inline so
	// the customer can act (insufficient_funds → top up; lost_card →
	// swap card).
	FailureReason string `json:"failure_reason,omitempty"`
	// EscalationOutcome is what dunning's exhaustion actually did to this
	// invoice (ADR-112) — it replaced the single `action` verb, which could
	// not describe a policy that both cancels and writes off. Rows enqueued
	// by the previous binary decode to the zero value, which renders the
	// NEUTRAL sentence: degraded but not false, since neutral copy asserts
	// no state change rather than the wrong one.
	EscalationOutcome domain.DunningEscalationOutcome `json:"escalation_outcome,omitempty"`
	// SetupURL + OperatorNote carry the operator-initiated
	// "add a payment method" email payload (TypePaymentSetupLink).
	// SetupURL is the Stripe Checkout setup-session URL; OperatorNote
	// is the optional free-form prefix the operator typed in the
	// dialog (empty → template falls back to generic copy).
	SetupURL         string `json:"setup_url,omitempty"`
	OperatorNote     string `json:"operator_note,omitempty"`
	UpdateURL        string `json:"update_url,omitempty"`
	PasswordResetURL string `json:"password_reset_url,omitempty"`
	InviteURL        string `json:"invite_url,omitempty"`
	InviterEmail     string `json:"inviter_email,omitempty"`
	TenantName       string `json:"tenant_name,omitempty"`
	// PublicToken is the Stripe-equivalent hosted_invoice_url credential
	// (T0-17). Carried on invoice-related email types so the dispatcher
	// can build the hosted-invoice CTA URL for the rendered HTML body.
	// InvoiceWrittenOff drives the setup-link email's promise: a written-off
	// invoice is never collected by a machine (ADR-110), so the copy says a
	// person will follow up rather than "we'll collect it automatically".
	InvoiceWrittenOff bool   `json:"invoice_written_off,omitempty"`
	PublicToken       string `json:"public_token,omitempty"`
	PDF               []byte `json:"pdf,omitempty"`
}

// OutboxSender satisfies the four domain email interfaces (invoice.EmailSender,
// dunning.EmailNotifier, payment.EmailReceipt, payment.EmailPaymentUpdate) by
// enqueueing a row into email_outbox instead of calling SMTP directly. The
// dispatcher drains the queue asynchronously.
//
// It requires a tenantID on every call because the outbox is tenant-scoped
// via RLS (and operators need per-tenant visibility on failures). Callers
// already know the tenant at every site — they just weren't passing it
// through.
type OutboxSender struct {
	store       *OutboxStore
	suppression RecipientSuppressionChecker
}

func NewOutboxSender(store *OutboxStore) *OutboxSender {
	return &OutboxSender{store: store}
}

// SetSuppressionChecker wires the recipient-suppression gate. Without
// it, the OutboxSender skips the check and always enqueues. Mirrors
// Sender.SetSuppressionChecker so both code paths gate identically.
func (s *OutboxSender) SetSuppressionChecker(c RecipientSuppressionChecker) { s.suppression = c }

func (s *OutboxSender) enqueue(ctx context.Context, tenantID, emailType string, msg outboxMessage) error {
	if tenantID == "" {
		return fmt.Errorf("email outbox sender: tenant_id required for %s", emailType)
	}
	// Suppression gate. Bounced/complained recipients never get an
	// outbox row written — the dispatcher would just retry-bounce-DLQ
	// otherwise, which wastes provider quota + sender reputation.
	// Same fail-open semantics as Sender.checkSuppression: a flaky
	// lookup logs WARN and proceeds.
	if s.suppression != nil && msg.To != "" {
		suppressed, reason, err := s.suppression.IsSuppressed(ctx, tenantID, msg.To)
		if err != nil {
			slog.Warn("email outbox: suppression check failed; proceeding with enqueue",
				"tenant_id", tenantID, "to", msg.To, "email_type", emailType, "error", err)
		} else if suppressed {
			slog.Info("email outbox: recipient suppressed — skipping enqueue",
				"tenant_id", tenantID, "to", msg.To, "email_type", emailType, "reason", reason)
			return ErrRecipientSuppressed
		}
	}
	// Marshal-then-unmarshal-to-map: drives the wire payload off the
	// outboxMessage struct's `json:` tags so adding a new field to the
	// struct can't silently drop on the way out. Pre-2026-05-29 this
	// was a hand-written map[string]any of 18 keys — when SetupURL +
	// OperatorNote were added to the struct, nobody updated the map,
	// and the operator-initiated "Send setup email" path silently
	// dropped the Stripe Checkout URL → customers received an email
	// promising "use the secure link below" with no link.
	// feedback_no_silent_fallbacks + feedback_audit_overlapping_flows.
	payloadBytes, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal outbox payload: %w", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		return fmt.Errorf("unmarshal outbox payload: %w", err)
	}
	// ctx must carry livemode (set by caller's auth middleware or, for
	// system-initiated emails fired from background workers, derived
	// from the originating row). EnqueueStandalone opens TxTenant; the
	// email_outbox BEFORE INSERT trigger reads app.livemode from the
	// session and stamps it on the row, so emails enqueued in test mode
	// land on test-mode rows and the dispatcher routes them correctly.
	// Pre-fix this used context.Background() and every email_outbox row
	// got stamped livemode=true regardless of the actual mode.
	_, err = s.store.EnqueueStandalone(ctx, tenantID, emailType, payload)
	return err
}

// SendInvoice enqueues an invoice email. Satisfies invoice.EmailSender.
func (s *OutboxSender) SendInvoice(ctx context.Context, tenantID, to string, cc []string, customerName, invoiceNumber string, totalCents int64, currency string, pdfBytes []byte, publicToken string) error {
	return s.enqueue(ctx, tenantID, TypeInvoice, outboxMessage{
		To:            to,
		Cc:            cc,
		CustomerName:  customerName,
		InvoiceNumber: invoiceNumber,
		AmountCents:   totalCents,
		Currency:      currency,
		PublicToken:   publicToken,
		PDF:           pdfBytes,
	})
}

// SendPaymentReceipt enqueues a receipt email. Satisfies payment.EmailReceipt.
func (s *OutboxSender) SendPaymentReceipt(ctx context.Context, tenantID, to string, cc []string, customerName, invoiceNumber string, amountCents int64, currency, publicToken string) error {
	return s.enqueue(ctx, tenantID, TypePaymentReceipt, outboxMessage{
		To:            to,
		Cc:            cc,
		CustomerName:  customerName,
		InvoiceNumber: invoiceNumber,
		AmountCents:   amountCents,
		Currency:      currency,
		PublicToken:   publicToken,
	})
}

// SendDunningWarning enqueues a dunning-warning email. Satisfies dunning.EmailNotifier.
func (s *OutboxSender) SendDunningWarning(ctx context.Context, tenantID, to string, cc []string, customerName, invoiceNumber string, attemptNumber, maxAttempts int, nextRetryDate, failureReason, publicToken string) error {
	return s.enqueue(ctx, tenantID, TypeDunningWarning, outboxMessage{
		To:            to,
		Cc:            cc,
		CustomerName:  customerName,
		InvoiceNumber: invoiceNumber,
		AttemptNumber: attemptNumber,
		MaxAttempts:   maxAttempts,
		NextRetryDate: nextRetryDate,
		FailureReason: failureReason,
		PublicToken:   publicToken,
	})
}

// SendDunningEscalation enqueues a dunning-escalation email. Satisfies dunning.EmailNotifier.
func (s *OutboxSender) SendDunningEscalation(ctx context.Context, tenantID, to string, cc []string, customerName, invoiceNumber string, outcome domain.DunningEscalationOutcome, publicToken string) error {
	return s.enqueue(ctx, tenantID, TypeDunningEscalation, outboxMessage{
		To:                to,
		Cc:                cc,
		CustomerName:      customerName,
		InvoiceNumber:     invoiceNumber,
		EscalationOutcome: outcome,
		PublicToken:       publicToken,
	})
}

// SendPaymentFailed enqueues a payment-failed email. Satisfies dunning.EmailNotifier.
func (s *OutboxSender) SendPaymentFailed(ctx context.Context, tenantID, to string, cc []string, customerName, invoiceNumber, reason, publicToken string) error {
	return s.enqueue(ctx, tenantID, TypePaymentFailed, outboxMessage{
		To:            to,
		Cc:            cc,
		CustomerName:  customerName,
		InvoiceNumber: invoiceNumber,
		Reason:        reason,
		PublicToken:   publicToken,
	})
}

// SendPaymentSetupRequest enqueues a payment-setup-request email
// (customer must set up a PM on a finalized invoice).
func (s *OutboxSender) SendPaymentSetupRequest(ctx context.Context, tenantID, to, customerName, invoiceNumber string, amountDueCents int64, currency, updateURL string, writtenOff bool, trigger string) error {
	if updateURL == "" {
		return fmt.Errorf("update_url required: refusing to enqueue payment-setup-request email with no link")
	}
	return s.enqueue(ctx, tenantID, TypePaymentSetupRequest, outboxMessage{
		To:                to,
		CustomerName:      customerName,
		InvoiceNumber:     invoiceNumber,
		AmountCents:       amountDueCents,
		Currency:          currency,
		UpdateURL:         updateURL,
		InvoiceWrittenOff: writtenOff,
		Trigger:           trigger,
	})
}

// SendPaymentSetupLink enqueues the operator-initiated "add a payment
// method" email. Distinct from SendPaymentSetupRequest (invoice
// context, engine-fired); this one is the operator's "Send setup
// email" dashboard action. Routes through the unified template
// (renderPaymentSetupLinkHTML) with operatorNote prefacing the body
// when set. Industry standard — every other operator-action email
// goes through the outbox for resilience; this brings the setup-link
// path into parity.
func (s *OutboxSender) SendPaymentSetupLink(ctx context.Context, tenantID, to, customerName, operatorNote, setupURL string) error {
	// Required at enqueue time — see Sender.SendPaymentSetupLink for
	// rationale. A row written with empty SetupURL would dispatch
	// successfully (the outbox layer doesn't know what each email type
	// needs) and produce a button-less email at render time. Reject
	// here so the operator sees a clear error on their dashboard click
	// instead of the customer receiving broken mail.
	if setupURL == "" {
		return fmt.Errorf("setup_url required: refusing to enqueue payment-setup email with no link")
	}
	return s.enqueue(ctx, tenantID, TypePaymentSetupLink, outboxMessage{
		To:           to,
		CustomerName: customerName,
		OperatorNote: operatorNote,
		SetupURL:     setupURL,
	})
}

// SendCreditNote enqueues a credit-note document email (ADR-082
// rider). invoiceNumber is the APPLIED invoice — named in the body so
// finance can match the credit to the bill it adjusts. No CTA link: no
// hosted CN page exists, the PDF is the document.
func (s *OutboxSender) SendCreditNote(ctx context.Context, tenantID, to string, cc []string, customerName, creditNoteNumber, invoiceNumber string, amountCents int64, currency string, pdfBytes []byte) error {
	return s.enqueue(ctx, tenantID, TypeCreditNote, outboxMessage{
		To:               to,
		Cc:               cc,
		CustomerName:     customerName,
		CreditNoteNumber: creditNoteNumber,
		InvoiceNumber:    invoiceNumber,
		AmountCents:      amountCents,
		Currency:         currency,
		PDF:              pdfBytes,
	})
}

// SendPasswordReset enqueues a password-reset email. The URL carries the
// one-time-use raw reset token that lands the dashboard user on the
// frontend password-reset confirmation page.
func (s *OutboxSender) SendPasswordReset(ctx context.Context, tenantID, to, displayName, resetURL string) error {
	if resetURL == "" {
		return fmt.Errorf("reset_url required: refusing to enqueue password-reset email with no link")
	}
	return s.enqueue(ctx, tenantID, TypePasswordReset, outboxMessage{
		To:               to,
		CustomerName:     displayName,
		PasswordResetURL: resetURL,
	})
}

// SendMemberInvite enqueues a team-invite email. The URL carries the
// raw invite token. Unlike the other flows this is tenant-initiated
// (inviter sits inside tenantID) rather than system-initiated.
func (s *OutboxSender) SendMemberInvite(ctx context.Context, tenantID, to, inviterEmail, tenantName, acceptURL string) error {
	if acceptURL == "" {
		return fmt.Errorf("accept_url required: refusing to enqueue member-invite email with no link")
	}
	return s.enqueue(ctx, tenantID, TypeMemberInvite, outboxMessage{
		To:           to,
		InviterEmail: inviterEmail,
		TenantName:   tenantName,
		InviteURL:    acceptURL,
	})
}
