package email

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/platform/scheduler"
)

// DispatcherConfig controls the outbox dispatcher loop.
type DispatcherConfig struct {
	// Interval is the poll cadence between ProcessBatch calls. Default 5s if zero.
	// A little slower than webhooks (2s) because SMTP latency + provider rate
	// limits make sub-second reactions pointless for email.
	Interval time.Duration
	// BatchSize bounds how many rows are claimed per tick. Default 5 if
	// zero — sized with the P5 lease arithmetic (see outbox.go
	// PerRowBudget/ClaimLease): every claimed row must fit inside
	// BatchTimeout, and the claim lease must exceed BatchTimeout.
	BatchSize int
	// BatchTimeout bounds how long a single tick may run. Defaults to
	// BatchSize×PerRowBudget. Row locks are NOT held for the batch —
	// the claim tx commits immediately; the lease owns exclusion.
	BatchTimeout time.Duration
}

// DispatchLock is a held cluster-wide lock the dispatcher must release.
type DispatchLock interface {
	Release()
}

// DispatchLocker gates the dispatcher tick on a cluster-wide advisory lock.
// Row-level FOR UPDATE SKIP LOCKED already prevents double-delivery when two
// dispatchers race, but the lock avoids both replicas issuing the same claim
// query every tick when only one drain worker is actually needed — less churn
// on the connection pool and on email_outbox's index scan. Nil Locker
// disables gating (single-replica / test mode).
type DispatchLocker interface {
	TryDispatcherLock(ctx context.Context) (DispatchLock, bool, error)
}

// EmailDeliverer is the SMTP-sending narrow interface the dispatcher calls.
// Satisfied by *email.Sender. Defined here so tests can inject fakes without
// touching the production Sender. ctx carries the row's livemode (set by
// Dispatcher.handle from the email_outbox row) so brand lookups inside
// the Sender resolve against the correct tenant_settings row.
type EmailDeliverer interface {
	SendInvoice(ctx context.Context, tenantID, to string, cc []string, customerName, invoiceNumber string, totalCents int64, currency string, pdfBytes []byte, publicToken string) error
	SendPaymentReceipt(ctx context.Context, tenantID, to string, cc []string, customerName, invoiceNumber string, amountCents int64, currency, publicToken string) error
	SendDunningWarning(ctx context.Context, tenantID, to string, cc []string, customerName, invoiceNumber string, attemptNumber, maxAttempts int, nextRetryDate, failureReason, publicToken string) error
	SendDunningEscalation(ctx context.Context, tenantID, to string, cc []string, customerName, invoiceNumber, action, publicToken string) error
	SendPaymentFailed(ctx context.Context, tenantID, to string, cc []string, customerName, invoiceNumber, reason, publicToken string) error
	SendCreditNote(ctx context.Context, tenantID, to string, cc []string, customerName, creditNoteNumber, invoiceNumber string, amountCents int64, currency string, pdfBytes []byte) error
	SendPaymentSetupRequest(ctx context.Context, tenantID, to, customerName, invoiceNumber string, amountDueCents int64, currency, updateURL string) error
	SendPaymentSetupLink(ctx context.Context, tenantID, to, customerName, operatorNote, setupURL string) error
	SendPasswordReset(ctx context.Context, tenantID, to, displayName, resetURL string) error
	SendMemberInvite(ctx context.Context, tenantID, to, inviterEmail, tenantName, acceptURL string) error
}

// Dispatcher drains the email_outbox by deserialising each row's payload and
// invoking the matching Send* method on the wrapped EmailDeliverer (the real
// SMTP Sender in production). Handler semantics: a row is marked 'dispatched'
// once the Send* call returns nil.
type Dispatcher struct {
	settled InvoiceSettledChecker
	outbox  *OutboxStore
	sender  EmailDeliverer
	cfg     DispatcherConfig
	locker  DispatchLocker
}

func NewDispatcher(outbox *OutboxStore, sender EmailDeliverer, cfg DispatcherConfig) *Dispatcher {
	if cfg.Interval <= 0 {
		cfg.Interval = 5 * time.Second
	}
	if cfg.BatchSize <= 0 {
		// 5 rows/tick: with PerRowBudget=60s the invariant chain
		// BatchSize×PerRowBudget ≤ BatchTimeout < ClaimLease holds as
		// 300s ≤ 300s < 360s (P5 panel lease arithmetic). Throughput 5
		// per 5s tick = 60/min — ample at current scale.
		cfg.BatchSize = 5
	}
	if cfg.BatchTimeout <= 0 {
		cfg.BatchTimeout = time.Duration(cfg.BatchSize) * PerRowBudget
	}
	return &Dispatcher{outbox: outbox, sender: sender, cfg: cfg}
}

// Config exposes the resolved (defaulted) dispatcher configuration —
// the lease-invariant test asserts the constants relation against it.
func (d *Dispatcher) Config() DispatcherConfig { return d.cfg }

// SetLocker enables leader gating on the dispatcher tick.
func (d *Dispatcher) SetLocker(locker DispatchLocker) {
	d.locker = locker
}

// Start runs the dispatcher loop until ctx is cancelled. Intended to be
// launched as a goroutine from cmd/velox during boot.
func (d *Dispatcher) Start(ctx context.Context) {
	slog.Info("email outbox dispatcher started",
		"interval", d.cfg.Interval.String(),
		"batch_size", d.cfg.BatchSize,
	)
	scheduler.Run(ctx, "email_outbox", d.cfg.Interval, d.tick)
}

// tick drains one batch. Errors are logged and swallowed — the next tick will
// retry. A per-tick timeout ensures a stuck handler can't hold row locks
// indefinitely if the dispatcher ctx is long-lived.
func (d *Dispatcher) tick(ctx context.Context) {
	batchCtx, cancel := context.WithTimeout(ctx, d.cfg.BatchTimeout)
	defer cancel()

	if d.locker != nil {
		lock, acquired, err := d.locker.TryDispatcherLock(batchCtx)
		if err != nil {
			slog.Error("email outbox dispatcher: lock acquire failed", "error", err)
			return
		}
		if !acquired {
			return
		}
		defer lock.Release()
	}

	n, err := d.outbox.ProcessBatch(batchCtx, d.cfg.BatchSize, d.handle)
	if err != nil {
		slog.Error("email outbox dispatcher: batch error", "error", err)
		return
	}
	if n > 0 {
		slog.Debug("email outbox dispatcher: batch processed", "count", n)
	}
}

// InvoiceSettledChecker answers whether an action-required email
// referencing an invoice is obsolete (invoice settled or gone). Satisfied
// by the invoice store through a composition-root adapter — the email
// package never imports a peer domain. Optional: nil skips the gate.
type InvoiceSettledChecker interface {
	// InvoiceTerminalState returns "" while the invoice is still live,
	// else "paid" / "voided" / "uncollectible" / "gone". The dispatcher
	// needs the specific state because ONE email type survives one of
	// them — see handle().
	InvoiceTerminalState(ctx context.Context, tenantID, invoiceNumber string) (string, error)
}

// SetSettledChecker wires the staleness gate (see handle).
func (d *Dispatcher) SetSettledChecker(c InvoiceSettledChecker) { d.settled = c }

// actionRequiredTypes are the email types whose ENTIRE purpose collapses
// once the invoice settles: "update your payment method", "payment
// failed", "we'll retry on <date>", "action taken — balance due". The
// informational types (invoice document, receipt, credit note) stay
// deliverable regardless — a paid invoice's document is still the
// customer's record.
// escalationAnnounces reports the one case where a terminal invoice must
// NOT mute its email: a dunning escalation whose final action is
// mark_uncollectible marks the invoice uncollectible in the same breath
// as it enqueues the email that ANNOUNCES that outcome. The blanket
// settled-gate therefore skipped it every single time — the customer
// was never told, and the template's uncollectible branch ("View
// invoice" instead of a Pay CTA that would dead-end) was unreachable in
// production. paid/voided still mute it: there the escalation genuinely
// no longer describes reality.
func escalationAnnounces(emailType, terminalState string) bool {
	return emailType == TypeDunningEscalation && terminalState == "uncollectible"
}

var actionRequiredTypes = map[string]bool{
	TypePaymentSetupRequest: true,
	TypePaymentFailed:       true,
	TypeDunningWarning:      true,
	TypeDunningEscalation:   true,
}

// handle is the per-row handler. It demarshals the payload based on
// email_type and dispatches to the matching Send* method. Returning nil
// marks the row 'dispatched'; returning an error schedules a retry (or DLQ
// after MaxOutboxAttempts); returning ErrEmailObsolete marks it 'skipped'.
func (d *Dispatcher) handle(ctx context.Context, row OutboxRow) error {
	msg, err := decodeMessage(row.EmailType, row.Payload)
	if err != nil {
		// Non-retryable — identical failure on every attempt. The
		// ErrPayloadDecode sentinel makes ProcessBatch DLQ it NOW
		// instead of riding 15 no-op cycles over ~72h (P5 panel).
		return fmt.Errorf("%w: %v", ErrPayloadDecode, err)
	}

	// Staleness gate: a queued action-required email whose invoice
	// settled while the row waited (SMTP outage, paused dispatcher) must
	// not deliver — "action required: update your payment method" landing
	// AFTER the customer paid erodes trust (found live, FLOW E
	// 2026-07-19). FAIL-OPEN on checker errors: a DB blip must never eat
	// customer mail; the pre-gate behavior (send) is the fallback.
	if d.settled != nil && actionRequiredTypes[row.EmailType] && msg.InvoiceNumber != "" {
		livemodeCtx := postgres.WithLivemode(ctx, row.Livemode)
		state, cerr := d.settled.InvoiceTerminalState(livemodeCtx, row.TenantID, msg.InvoiceNumber)
		if cerr != nil {
			slog.Warn("email staleness check failed — sending anyway (fail-open)",
				"outbox_id", row.ID, "email_type", row.EmailType,
				"invoice_number", msg.InvoiceNumber, "error", cerr)
		} else if state != "" && !escalationAnnounces(row.EmailType, state) {
			return fmt.Errorf("%w: invoice %s reached %s before delivery", ErrEmailObsolete, msg.InvoiceNumber, state)
		}
	}

	// Pin the row's livemode on the per-call ctx so the Sender's
	// settings.Get for branding hits the right tenant_settings row
	// (TxTenant under the hood). Without this, dispatcher-driven
	// sends would default to live regardless of which mode the
	// email was enqueued under.
	ctx = postgres.WithLivemode(ctx, row.Livemode)
	// Stamp the row id for provider-metadata correlation: the MIME
	// builders emit it as an X-PM-Metadata-* header so the provider's
	// Delivery/Bounce/SpamComplaint webhooks can name the exact outbox
	// row they concern (ADR-098).
	ctx = WithCorrelation(ctx, row.ID)

	switch row.EmailType {
	case TypeInvoice:
		return d.sender.SendInvoice(ctx, row.TenantID, msg.To, msg.Cc, msg.CustomerName, msg.InvoiceNumber,
			msg.AmountCents, msg.Currency, msg.PDF, msg.PublicToken)
	case TypePaymentReceipt:
		return d.sender.SendPaymentReceipt(ctx, row.TenantID, msg.To, msg.Cc, msg.CustomerName, msg.InvoiceNumber,
			msg.AmountCents, msg.Currency, msg.PublicToken)
	case TypeDunningWarning:
		return d.sender.SendDunningWarning(ctx, row.TenantID, msg.To, msg.Cc, msg.CustomerName, msg.InvoiceNumber,
			msg.AttemptNumber, msg.MaxAttempts, msg.NextRetryDate, msg.FailureReason, msg.PublicToken)
	case TypeDunningEscalation:
		return d.sender.SendDunningEscalation(ctx, row.TenantID, msg.To, msg.Cc, msg.CustomerName, msg.InvoiceNumber,
			msg.Action, msg.PublicToken)
	case TypePaymentFailed:
		return d.sender.SendPaymentFailed(ctx, row.TenantID, msg.To, msg.Cc, msg.CustomerName, msg.InvoiceNumber,
			msg.Reason, msg.PublicToken)
	case TypePaymentSetupRequest:
		return d.sender.SendPaymentSetupRequest(ctx, row.TenantID, msg.To, msg.CustomerName, msg.InvoiceNumber,
			msg.AmountCents, msg.Currency, msg.UpdateURL)
	case TypePaymentSetupLink:
		return d.sender.SendPaymentSetupLink(ctx, row.TenantID, msg.To, msg.CustomerName,
			msg.OperatorNote, msg.SetupURL)
	case TypePasswordReset:
		return d.sender.SendPasswordReset(ctx, row.TenantID, msg.To, msg.CustomerName, msg.PasswordResetURL)
	case TypeMemberInvite:
		return d.sender.SendMemberInvite(ctx, row.TenantID, msg.To, msg.InviterEmail, msg.TenantName, msg.InviteURL)
	case TypeCreditNote:
		return d.sender.SendCreditNote(ctx, row.TenantID, msg.To, msg.Cc, msg.CustomerName,
			msg.CreditNoteNumber, msg.InvoiceNumber, msg.AmountCents, msg.Currency, msg.PDF)
	default:
		return fmt.Errorf("%w: unknown email_type %q", ErrPayloadDecode, row.EmailType)
	}
}

// decodeMessage turns the jsonb payload into a strongly-typed message. The
// schema is intentionally a single union struct (rather than per-type
// structs) because the outbox is a log and we care more about simple
// read-back than strict schema segmentation.
func decodeMessage(emailType string, payload map[string]any) (outboxMessage, error) {
	var msg outboxMessage
	if payload == nil {
		return msg, fmt.Errorf("empty payload")
	}
	// Re-marshal and unmarshal so field parsing uses json tags and numeric
	// normalisation (jsonb comes back as map[string]any with float64 numbers).
	raw, err := json.Marshal(payload)
	if err != nil {
		return msg, err
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		return msg, err
	}
	if msg.To == "" {
		return msg, fmt.Errorf("%s: missing 'to'", emailType)
	}
	return msg, nil
}
