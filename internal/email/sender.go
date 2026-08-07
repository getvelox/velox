package email

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"os"
	"strings"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
)

// ErrSMTPNotConfigured is returned by every send path when SMTP_HOST
// (and creds) are not set. We surface a real error rather than silently
// pretending success — operator-visible failures beat invisible drops.
// Local dev is expected to run MailHog or similar; the path "no SMTP
// at all" is treated as a misconfiguration, not a graceful mode.
var ErrSMTPNotConfigured = errors.New("email: SMTP not configured (SMTP_HOST unset)")

// Sender sends emails via SMTP. If SMTP is not configured, every send
// returns ErrSMTPNotConfigured — callers must treat that as a real
// failure, not a silent drop. Local dev should run MailHog or set
// SMTP_HOST to a real relay.
//
// Customer-facing emails render as multipart/alternative (text + HTML)
// with tenant-branded chrome pulled via SettingsGetter. Operator emails
// (password reset, member invite) stay plain text —
// they carry security-sensitive tokens and have no tenant branding
// context to begin with.
type Sender struct {
	host     string
	port     string
	username string
	password string
	from     string
	// tlsMode controls the transport security model:
	//   "starttls" — connect plain, upgrade with STARTTLS (port 587).
	//                Default; works with SendGrid, Postmark, Mailgun,
	//                AWS SES port 587.
	//   "implicit" — TLS-from-the-handshake (port 465). Required by
	//                AWS SES port 465 and a few legacy providers.
	//   "none"     — plain SMTP, no TLS. ONLY for local dev with a
	//                tool like Mailpit/MailHog. deliver() rejects this
	//                mode when APP_ENV=production (fail-loud at send).
	tlsMode string
	// prodEnv gates the tlsMode=none rejection (APP_ENV=production).
	prodEnv bool

	// hostedInvoiceBaseURL controls where email CTAs point for the
	// Stripe-parity hosted invoice page (T0-17). When unset, emails
	// still render branded HTML but without the pay/view CTA button.
	hostedInvoiceBaseURL string

	// settings, when non-nil, is consulted on every customer-facing
	// send to fetch the tenant's branding (logo, brand color, company
	// name, support URL, from-name). Nil => fall back to Velox defaults.
	settings SettingsGetter

	// bounceReporter, when non-nil, is called on SMTP permanent-failure
	// (5xx) errors so the partner's customer row flips to email_status=
	// 'bounced'. Nil means bounces are still logged but not persisted —
	// acceptable in tests and standalone contexts.
	bounceReporter BounceReporter
	suppression    RecipientSuppressionChecker
}

// SettingsGetter resolves a tenantID to the tenant's public-facing
// settings. Satisfied by *tenant.SettingsStore at runtime; unit tests
// pass a fake.
type SettingsGetter interface {
	Get(ctx context.Context, tenantID string) (domain.TenantSettings, error)
}

// BounceReporter is the callback the Sender invokes when SMTP rejects a
// send with a permanent-failure (5xx) code. The router-side adapter
// resolves email → customer via the blind index and calls
// customer.MarkEmailBounced. Kept behind an interface so the email
// package doesn't import customer.
//
// Called synchronously from the SMTP send path; implementations should
// bound their own latency so a bounce doesn't stall the outbox worker.
type BounceReporter interface {
	ReportBounce(ctx context.Context, tenantID, email, reason string)
}

// RecipientSuppressionChecker reports whether the (tenant, email) is
// known-bounced or known-complained — in which case we soft-skip the
// send instead of pounding a dead inbox. Implementation looks up the
// customer row via email blind index and checks email_status. Wired
// via SetSuppressionChecker on both Sender and OutboxSender so the
// gate covers both direct-SMTP and outbox paths.
//
// The check is best-effort. On error (DB down, blinder misconfigured)
// the send proceeds — failing closed would mean a bouncing recipient
// blocks every outbound send if the lookup ever flakes.
type RecipientSuppressionChecker interface {
	IsSuppressed(ctx context.Context, tenantID, email string) (suppressed bool, reason string, err error)
}

// NewSender creates an email sender from environment variables. When
// SMTP_HOST is unset every send returns ErrSMTPNotConfigured —
// `make dev` should run MailHog (docs/ops/email-setup.md) so the
// dev path exercises the same error contract as production.
func NewSender() *Sender {
	tlsMode := strings.ToLower(strings.TrimSpace(os.Getenv("SMTP_TLS")))
	switch tlsMode {
	case "starttls", "implicit", "none":
		// valid
	case "":
		tlsMode = "starttls"
	default:
		// Invalid value — log and fall back to safe default. The
		// dispatcher would otherwise silently use the wrong transport.
		slog.Warn("SMTP_TLS unrecognized, defaulting to starttls", "got", tlsMode)
		tlsMode = "starttls"
	}
	prodEnv := strings.EqualFold(strings.TrimSpace(os.Getenv("APP_ENV")), "production")
	// One line of boot-time truth about the transport mode: strict
	// STARTTLS is the default, and a stale env pointing at a no-TLS
	// sandbox will now fail loudly at send — this log is how the
	// operator self-diagnoses that flip (P5 rollout rider).
	slog.Info("smtp transport", "mode", tlsMode, "strict_starttls", tlsMode == "starttls", "prod", prodEnv)
	return &Sender{
		host:                 strings.TrimSpace(os.Getenv("SMTP_HOST")),
		port:                 envOr("SMTP_PORT", "587"),
		username:             strings.TrimSpace(os.Getenv("SMTP_USERNAME")),
		password:             strings.TrimSpace(os.Getenv("SMTP_PASSWORD")),
		from:                 envOr("SMTP_FROM", "billing@velox.dev"),
		tlsMode:              tlsMode,
		prodEnv:              prodEnv,
		hostedInvoiceBaseURL: strings.TrimSpace(os.Getenv("HOSTED_INVOICE_BASE_URL")),
	}
}

// NewTestSender builds a Sender with explicit transport parameters —
// test-only constructor (production always goes through NewSender's
// env resolution) used to exercise deliver()'s strict-STARTTLS and
// prod-none refusals against local fake servers.
func NewTestSender(host, port, tlsMode string, prodEnv bool) *Sender {
	return &Sender{host: host, port: port, from: "test@velox.dev", tlsMode: tlsMode, prodEnv: prodEnv}
}

// DeliverForTest exposes deliver() to tests in this module.
func (s *Sender) DeliverForTest(ctx context.Context, to string, body []byte) error {
	return s.deliver(ctx, "", to, nil, body)
}

func (s *Sender) IsConfigured() bool { return s.host != "" }

// SMTPHost returns the configured SMTP relay host (no port) for
// startup-log diagnostics. Empty string when unconfigured. Only the
// host name is exposed — never the credentials.
func (s *Sender) SMTPHost() string { return s.host }

// SetSettingsGetter wires the tenant settings store so Send* methods can
// look up branding per email. Called from router.go; optional for tests.
func (s *Sender) SetSettingsGetter(g SettingsGetter) { s.settings = g }

// SetBounceReporter wires the bounce-capture callback. Router-side
// adapter resolves email → customer ID → customer.MarkEmailBounced.
func (s *Sender) SetBounceReporter(r BounceReporter) { s.bounceReporter = r }

// SetSuppressionChecker wires the recipient-suppression gate. Without
// it, the Sender skips the check and always attempts SMTP — same
// behavior as before this gate existed (back-compat for tests).
func (s *Sender) SetSuppressionChecker(c RecipientSuppressionChecker) { s.suppression = c }

// checkSuppression returns true if the send should be soft-skipped.
// Logs the skip at INFO so operators can see it in `make dev` output.
// Errors from the checker fail-open (proceed with the send) so a flaky
// lookup doesn't block all outbound mail. ErrRecipientSuppressed is
// returned to the caller so producers can distinguish "skipped" from
// "delivered" if they care; most callers swallow it.
var ErrRecipientSuppressed = errors.New("email: recipient suppressed (bounced or complained)")

func (s *Sender) checkSuppression(ctx context.Context, tenantID, to string) error {
	if s.suppression == nil || tenantID == "" || to == "" {
		return nil
	}
	// Bounded: this DB lookup is part of the outbox per-row budget —
	// unbounded it invalidated the whole lease arithmetic (P5 panel).
	supCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	suppressed, reason, err := s.suppression.IsSuppressed(supCtx, tenantID, to)
	if err != nil {
		// Fail-open: log + proceed. A bouncing-recipient lookup that
		// errors out shouldn't block legitimate sends to other addresses.
		slog.Warn("email suppression check failed; proceeding with send",
			"tenant_id", tenantID, "to", to, "error", err)
		return nil
	}
	if suppressed {
		slog.Info("email suppressed — recipient is bounced or complained",
			"tenant_id", tenantID, "to", to, "reason", reason)
		return ErrRecipientSuppressed
	}
	return nil
}

// isPermanentSMTPBounce checks an SMTP send error for a
// recipient-rejection permanent-failure reply — the recipient address
// itself is bad, so flagging the customer's email as bounced is the
// correct response. Sender-side 5xx codes (authentication failure,
// policy reject, server refusal) are explicitly excluded: those mean
// MY side has a problem, not the recipient's, and flipping the
// customer to bounced would block future legitimate sends as soon as
// the operator fixes their config.
//
// Classification per RFC 5321 + RFC 3463:
//
//	Basic 5xx codes that ARE recipient bounces:
//	  550 - mailbox unavailable / no such user
//	  551 - user not local (rare)
//	  552 - storage quota exceeded
//	  553 - mailbox name not allowed
//
//	Basic 5xx codes that are NOT recipient bounces (sender/server):
//	  521 - server doesn't accept mail
//	  530 - access denied / auth required
//	  534 - auth too weak
//	  535 - authentication failed  (Postmark token mismatch lands here)
//	  538 - encryption required
//	  554 - transaction failed (ambiguous; conservatively excluded)
//
//	Enhanced status (X.Y.Z) interpretation:
//	  5.1.x - addressing (recipient)  → bounce
//	  5.2.x - mailbox (recipient)     → bounce
//	  5.7.x - security/policy (sender) → not a bounce
//	  5.3.x - mail system (server)     → not a bounce
//
// Pre-2026-05-29 every 5xx was treated as a recipient bounce.
// Surfaced live during Postmark token misconfiguration: a wrong
// Server Token returned 535 → Velox flagged the recipient as bounced
// → next send to the same recipient was suppression-gated → operator
// had to manually `UPDATE customers SET email_status='unknown'` to
// recover. The auth failure was a sender-side error all along.
//
// Heuristic string-match because stdlib net/smtp wraps the underlying
// textproto.Error by the time callers see it. Bias: false negatives
// (real bounces missed) cost a wasted retry; false positives
// (sender errors marked as recipient bounces) cost the operator a
// manual recovery — much worse, so the classifier is conservative
// toward "not a bounce."
func isPermanentSMTPBounce(err error) (bool, string) {
	if err == nil {
		return false, ""
	}
	msg := err.Error()

	// Enhanced status code in the addressing/mailbox classes wins
	// over the basic code's classification — 5.1.x and 5.2.x are
	// unambiguously recipient-side per RFC 3463. Some MX servers
	// send only the enhanced form (no leading 550 token), so this
	// check is independent of the basic-code scan below.
	if hasEnhancedRecipientStatus(msg) {
		return true, msg
	}

	for i := 0; i <= len(msg)-3; i++ {
		if msg[i] != '5' {
			continue
		}
		if !isDigit(msg[i+1]) || !isDigit(msg[i+2]) {
			continue
		}
		if i > 0 {
			prev := msg[i-1]
			if isDigit(prev) || isLetter(prev) {
				continue
			}
		}
		// Found a 5xx token. Only the recipient-rejection codes
		// (550-553) classify as a bounce. Everything else is sender
		// or server-side and should be surfaced to the operator
		// without flipping recipient state.
		code := msg[i : i+3]
		if code == "550" || code == "551" || code == "552" || code == "553" {
			return true, msg
		}
		// Sender-side or ambiguous 5xx — surface but don't bounce
		// the recipient.
		return false, ""
	}
	return false, ""
}

// hasEnhancedRecipientStatus returns true when msg contains an RFC
// 3463 enhanced status code in the 5.1.x (addressing) or 5.2.x
// (mailbox) classes — both indicate the recipient is the problem.
// 5.7.x (security/policy) and 5.3.x (mail system) are excluded as
// sender/server-side.
func hasEnhancedRecipientStatus(msg string) bool {
	for i := 0; i <= len(msg)-5; i++ {
		if msg[i] != '5' || msg[i+1] != '.' {
			continue
		}
		if msg[i+2] != '1' && msg[i+2] != '2' {
			continue
		}
		if msg[i+3] != '.' || !isDigit(msg[i+4]) {
			continue
		}
		// Boundary check on the left so "x5.1.x" doesn't match.
		if i > 0 {
			prev := msg[i-1]
			if isDigit(prev) || isLetter(prev) {
				continue
			}
		}
		return true
	}
	return false
}

func isDigit(c byte) bool  { return c >= '0' && c <= '9' }
func isLetter(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }

// reportBounceIfPermanent forwards a permanent-failure SMTP error to
// the configured BounceReporter. ctx must carry livemode (caller
// inherits it from the request middleware or the dispatcher's per-row
// pin) — bounceReporter eventually opens a TxTenant on customers,
// without livemode the row update would default to live mode and
// either trip the propagation WARN or flip status on the wrong row.
//
// the configured BounceReporter. 5s timeout derived from the caller's
// ctx so a slow DB lookup doesn't starve the next send and the
// livemode pin propagates into customers.MarkEmailBounced.
func (s *Sender) reportBounceIfPermanent(ctx context.Context, tenantID, to string, err error) {
	if s.bounceReporter == nil {
		return
	}
	permanent, reason := isPermanentSMTPBounce(err)
	if !permanent {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	s.bounceReporter.ReportBounce(ctx, tenantID, to, reason)
}

// brandingFor resolves branding for tenantID. Never errors — a missing
// tenant or a cold-start settings row just gets Velox defaults. ctx
// must carry livemode (set by the request middleware for direct-send,
// or constructed from the email_outbox row's livemode by the
// dispatcher for the durable path).
func (s *Sender) brandingFor(ctx context.Context, tenantID string) Branding {
	b := Branding{}
	if s.settings == nil || tenantID == "" {
		return b.fill()
	}
	// 5s bound on settings query so a slow tenant lookup doesn't stall
	// the send path. Inherits livemode + tenant from the caller's ctx.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ts, err := s.settings.Get(ctx, tenantID)
	if err != nil {
		if !errors.Is(err, errs.ErrNotFound) {
			slog.Warn("email: branding lookup failed — falling back to defaults",
				"tenant_id", tenantID, "error", err)
		}
		return b.fill()
	}
	b = Branding{
		CompanyName: ts.CompanyName,
		LogoURL:     ts.LogoURL,
		BrandColor:  ts.BrandColor,
		SupportURL:  ts.SupportURL,
		FromAddress: ts.CompanyEmail,
	}
	return b.fill()
}

// hostedInvoiceURL returns the shareable hosted-invoice URL for the
// given public_token, or "" if either the token or the base URL is
// missing. Empty string means "no CTA button" at the render layer.
func (s *Sender) hostedInvoiceURL(publicToken string) string {
	if publicToken == "" || s.hostedInvoiceBaseURL == "" {
		return ""
	}
	return strings.TrimRight(s.hostedInvoiceBaseURL, "/") + "/invoice/" + publicToken
}

// ---- Customer-facing emails (HTML + plaintext multipart) ----

// SendInvoice sends an invoice email with PDF attachment. publicToken
// is the hosted_invoice_url credential (T0-17) — empty means no pay CTA.
// ctx must carry livemode for the branding lookup; the request handler
// sets it via auth middleware, the outbox dispatcher derives it from
// the email_outbox row.
func (s *Sender) SendInvoice(ctx context.Context, tenantID, to string, cc []string, customerName, invoiceNumber string, totalCents int64, currency string, pdfBytes []byte, publicToken string) error {
	brand := s.brandingFor(ctx, tenantID)
	hostedURL := s.hostedInvoiceURL(publicToken)
	amount := formatAmount(totalCents, currency)

	subject, contentHTML, ctaURL, ctaLabel := renderInvoiceHTML(customerName, invoiceNumber, amount, hostedURL, totalCents == 0)
	htmlBody, err := renderLayout(layoutInputs{
		Subject: subject, CompanyName: brand.CompanyName, LogoURL: brand.LogoURL,
		BrandColor: brand.BrandColor, SupportURL: brand.SupportURL,
		Content: template.HTML(contentHTML),
		CTAURL:  ctaURL, CTALabel: ctaLabel,
	})
	if err != nil {
		return fmt.Errorf("render invoice html: %w", err)
	}
	textBody := invoiceTextBody(customerName, invoiceNumber, amount, currency, hostedURL, totalCents == 0)

	return s.sendRich(ctx, richMessage{
		TenantID:       tenantID,
		To:             to,
		Cc:             cc,
		Subject:        subject,
		TextBody:       textBody,
		HTMLBody:       htmlBody,
		FromName:       brand.CompanyName,
		AttachmentName: invoiceNumber + ".pdf",
		AttachmentData: pdfBytes,
	})
}

// SendPaymentReceipt sends a receipt after successful payment.
func (s *Sender) SendPaymentReceipt(ctx context.Context, tenantID, to string, cc []string, customerName, invoiceNumber string, amountCents int64, currency, publicToken string) error {
	brand := s.brandingFor(ctx, tenantID)
	hostedURL := s.hostedInvoiceURL(publicToken)
	amount := formatAmount(amountCents, currency)

	subject, contentHTML, ctaURL, ctaLabel := renderReceiptHTML(customerName, invoiceNumber, amount, hostedURL)
	htmlBody, err := renderLayout(layoutInputs{
		Subject: subject, CompanyName: brand.CompanyName, LogoURL: brand.LogoURL,
		BrandColor: brand.BrandColor, SupportURL: brand.SupportURL,
		Content: template.HTML(contentHTML), CTAURL: ctaURL, CTALabel: ctaLabel,
	})
	if err != nil {
		return fmt.Errorf("render receipt html: %w", err)
	}
	textBody := receiptTextBody(customerName, invoiceNumber, amount, hostedURL)

	return s.sendRich(ctx, richMessage{
		TenantID: tenantID, To: to, Cc: cc, Subject: subject,
		TextBody: textBody, HTMLBody: htmlBody, FromName: brand.CompanyName,
	})
}

// SendDunningWarning notifies a customer about a failed retry. Body
// includes the latest decline reason (so the customer can act —
// insufficient_funds → top up; lost_card → swap card). There is no
// final-attempt tone: the dunning service skips the warning on the
// exhausting attempt (N-1 warnings, then ONE escalation email — see
// willExhaustThisAttempt in internal/dunning/service.go).
// CTA is a "Pay invoice" button on the hosted invoice page (Stripe
// Checkout there handles both PM update and pay).
func (s *Sender) SendDunningWarning(ctx context.Context, tenantID, to string, cc []string, customerName, invoiceNumber string, attemptNumber, maxAttempts int, nextRetryDate, failureReason, publicToken string) error {
	brand := s.brandingFor(ctx, tenantID)
	hostedURL := s.hostedInvoiceURL(publicToken)

	subject, contentHTML, ctaURL, ctaLabel := renderDunningWarningHTML(customerName, invoiceNumber, attemptNumber, maxAttempts, nextRetryDate, failureReason, hostedURL)
	htmlBody, err := renderLayout(layoutInputs{
		Subject: subject, CompanyName: brand.CompanyName, LogoURL: brand.LogoURL,
		BrandColor: brand.BrandColor, SupportURL: brand.SupportURL,
		Content: template.HTML(contentHTML), CTAURL: ctaURL, CTALabel: ctaLabel,
	})
	if err != nil {
		return fmt.Errorf("render dunning warning html: %w", err)
	}
	textBody := dunningWarningTextBody(customerName, invoiceNumber, attemptNumber, maxAttempts, nextRetryDate, failureReason, hostedURL)

	return s.sendRich(ctx, richMessage{
		TenantID: tenantID, To: to, Cc: cc, Subject: subject,
		TextBody: textBody, HTMLBody: htmlBody, FromName: brand.CompanyName,
	})
}

// SendDunningEscalation emails the "retries exhausted" escalation.
func (s *Sender) SendDunningEscalation(ctx context.Context, tenantID, to string, cc []string, customerName, invoiceNumber string, outcome domain.DunningEscalationOutcome, publicToken string) error {
	brand := s.brandingFor(ctx, tenantID)
	hostedURL := s.hostedInvoiceURL(publicToken)

	subject, contentHTML, ctaURL, ctaLabel := renderDunningEscalationHTML(customerName, invoiceNumber, outcome, hostedURL)
	htmlBody, err := renderLayout(layoutInputs{
		Subject: subject, CompanyName: brand.CompanyName, LogoURL: brand.LogoURL,
		BrandColor: brand.BrandColor, SupportURL: brand.SupportURL,
		Content: template.HTML(contentHTML), CTAURL: ctaURL, CTALabel: ctaLabel,
	})
	if err != nil {
		return fmt.Errorf("render dunning escalation html: %w", err)
	}
	textBody := dunningEscalationTextBody(customerName, invoiceNumber, outcome, hostedURL)

	return s.sendRich(ctx, richMessage{
		TenantID: tenantID, To: to, Cc: cc, Subject: subject,
		TextBody: textBody, HTMLBody: htmlBody, FromName: brand.CompanyName,
	})
}

// SendPaymentFailed notifies a customer about a failed charge.
func (s *Sender) SendPaymentFailed(ctx context.Context, tenantID, to string, cc []string, customerName, invoiceNumber, reason, publicToken string) error {
	brand := s.brandingFor(ctx, tenantID)
	hostedURL := s.hostedInvoiceURL(publicToken)

	subject, contentHTML, ctaURL, ctaLabel := renderPaymentFailedHTML(customerName, invoiceNumber, reason, hostedURL)
	htmlBody, err := renderLayout(layoutInputs{
		Subject: subject, CompanyName: brand.CompanyName, LogoURL: brand.LogoURL,
		BrandColor: brand.BrandColor, SupportURL: brand.SupportURL,
		Content: template.HTML(contentHTML), CTAURL: ctaURL, CTALabel: ctaLabel,
	})
	if err != nil {
		return fmt.Errorf("render payment failed html: %w", err)
	}
	textBody := paymentFailedTextBody(customerName, invoiceNumber, reason, hostedURL)

	return s.sendRich(ctx, richMessage{
		TenantID: tenantID, To: to, Cc: cc, Subject: subject,
		TextBody: textBody, HTMLBody: htmlBody, FromName: brand.CompanyName,
	})
}

// SendPaymentSetupLink emails the operator-initiated "add a payment
// method" link. Distinct from SendPaymentSetupRequest (which fires
// at invoice finalize with no PM on file and carries the failing
// invoice context). This variant is for an operator clicking "Send
// Setup Link" on a customer page when there's no specific invoice
// pressure — the customer just needs a card on file. operatorNote
// is an optional free-form prefix the operator typed into the
// dashboard dialog (e.g. "Your card on file expired last week —
// here's a link to update it.").
func (s *Sender) SendPaymentSetupLink(ctx context.Context, tenantID, to, customerName, operatorNote, setupURL string) error {
	// Required: the email body promises "use the secure link below" —
	// without the URL we'd render a CTA-less email with that copy and
	// no actionable button (silent template fallback via
	// `{{if .CTAURL}}` in renderLayout). Surfaced 2026-05-29: an
	// older outbox row written by a pre-SetupURL binary dispatched
	// with empty CTA and the customer received a useless email.
	// Per feedback_no_silent_fallbacks — fail loud at the boundary
	// instead of producing a broken email.
	if setupURL == "" {
		return fmt.Errorf("setup_url required: refusing to send payment-setup email with no link")
	}
	brand := s.brandingFor(ctx, tenantID)

	subject, contentHTML, ctaURL, ctaLabel := renderPaymentSetupLinkHTML(paymentSetupLinkContext{
		CustomerName: customerName,
		OperatorNote: operatorNote,
		SetupURL:     setupURL,
	})
	htmlBody, err := renderLayout(layoutInputs{
		Subject: subject, CompanyName: brand.CompanyName, LogoURL: brand.LogoURL,
		BrandColor: brand.BrandColor, SupportURL: brand.SupportURL,
		Content: template.HTML(contentHTML), CTAURL: ctaURL, CTALabel: ctaLabel,
	})
	if err != nil {
		return fmt.Errorf("render payment setup link html: %w", err)
	}
	textBody := paymentSetupLinkTextBody(customerName, operatorNote, setupURL)

	return s.sendRich(ctx, richMessage{
		TenantID: tenantID, To: to, Subject: subject,
		TextBody: textBody, HTMLBody: htmlBody, FromName: brand.CompanyName,
	})
}

// SendPaymentSetupRequest emails the tokenized payment-method-setup
// link. CTA points at updateURL (NOT the hosted invoice URL — this
// flow sets up the saved payment method, a separate concern from
// charging an invoice). Sent at finalize when the customer has no PM
// on file. Distinct from SendPaymentFailed which fires after a charge
// has already been attempted and declined.
func (s *Sender) SendPaymentSetupRequest(ctx context.Context, tenantID, to, customerName, invoiceNumber string, amountDueCents int64, currency, updateURL string, writtenOff bool, _ string) error {
	if updateURL == "" {
		return fmt.Errorf("update_url required: refusing to send payment-setup-request email with no link")
	}
	brand := s.brandingFor(ctx, tenantID)
	amount := formatAmount(amountDueCents, currency)

	subject, contentHTML, ctaURL, ctaLabel := renderPaymentUpdateRequestHTML(customerName, invoiceNumber, amount, updateURL, writtenOff)
	htmlBody, err := renderLayout(layoutInputs{
		Subject: subject, CompanyName: brand.CompanyName, LogoURL: brand.LogoURL,
		BrandColor: brand.BrandColor, SupportURL: brand.SupportURL,
		Content: template.HTML(contentHTML), CTAURL: ctaURL, CTALabel: ctaLabel,
	})
	if err != nil {
		return fmt.Errorf("render payment update request html: %w", err)
	}
	textBody := paymentUpdateRequestTextBody(customerName, invoiceNumber, amount, updateURL)

	return s.sendRich(ctx, richMessage{
		TenantID: tenantID, To: to, Subject: subject,
		TextBody: textBody, HTMLBody: htmlBody, FromName: brand.CompanyName,
	})
}

// SendCreditNote emails the credit-note document (ADR-082 rider).
// invoiceNumber is the APPLIED invoice, named in the body so finance
// can match the credit against the bill it adjusts. No CTA — no hosted
// CN page exists; the attached PDF is the document.
func (s *Sender) SendCreditNote(ctx context.Context, tenantID, to string, cc []string, customerName, creditNoteNumber, invoiceNumber string, amountCents int64, currency string, pdfBytes []byte) error {
	brand := s.brandingFor(ctx, tenantID)
	amount := formatAmount(amountCents, currency)

	subject, contentHTML := renderCreditNoteHTML(customerName, creditNoteNumber, invoiceNumber, amount, brand.CompanyName)
	htmlBody, err := renderLayout(layoutInputs{
		Subject: subject, CompanyName: brand.CompanyName, LogoURL: brand.LogoURL,
		BrandColor: brand.BrandColor, SupportURL: brand.SupportURL,
		Content: template.HTML(contentHTML),
	})
	if err != nil {
		return fmt.Errorf("render credit note html: %w", err)
	}
	textBody := creditNoteTextBody(customerName, creditNoteNumber, invoiceNumber, amount)

	return s.sendRich(ctx, richMessage{
		TenantID: tenantID, To: to, Cc: cc, Subject: subject,
		TextBody: textBody, HTMLBody: htmlBody, FromName: brand.CompanyName,
		AttachmentName: creditNoteNumber + ".pdf",
		AttachmentData: pdfBytes,
	})
}

// ---- Operator-facing emails (plain text; carry security tokens, no
// tenant branding context) ----

func (s *Sender) SendPasswordReset(ctx context.Context, tenantID, to, displayName, resetURL string) error {
	if resetURL == "" {
		return fmt.Errorf("reset_url required: refusing to send password-reset email with no link")
	}
	subject := "Reset your Velox password"
	body := fmt.Sprintf(`Hi %s,

We received a request to reset the password on your Velox account. Use the link below within the next hour to choose a new one:

%s

If you didn't request this, you can safely ignore this email — your current password keeps working, and nobody can sign in without the link.

— Velox
`, displayName, resetURL)
	return s.sendPlain(ctx, tenantID, to, "", subject, body)
}

func (s *Sender) SendMemberInvite(ctx context.Context, tenantID, to, inviterEmail, tenantName, acceptURL string) error {
	if acceptURL == "" {
		return fmt.Errorf("accept_url required: refusing to send member-invite email with no link")
	}
	// The invite service WARNs "falls back to generic" when the tenant-name
	// lookup fails and hands us "" — this guard IS that fallback (2026-07-19
	// truth audit: the claim predated the mechanism; a "" interpolation
	// produced 'invited to  on Velox' in a real customer inbox).
	if strings.TrimSpace(tenantName) == "" {
		tenantName = "your team"
	}
	subject := fmt.Sprintf("You've been invited to %s on Velox", tenantName)
	body := fmt.Sprintf(`Hi,

%s invited you to join %s on Velox. Click the link below within the next 7 days to set up your account:

%s

If you weren't expecting this invitation, you can safely ignore this email.

— Velox
`, inviterEmail, tenantName, acceptURL)
	return s.sendPlain(ctx, tenantID, to, "", subject, body)
}

// ---- Plaintext fallback bodies (multipart/alternative text part) ----

func invoiceTextBody(customerName, invoiceNumber, amount, currency, hostedURL string, zeroDue bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Hi %s,\n\n", customerName)
	if zeroDue {
		// Plumbed amount_due==0: same fact the HTML twin adapts on —
		// never ask the customer to pay a settled invoice.
		fmt.Fprintf(&b, "Invoice %s is ready. It is settled — no payment is needed.\n\n", invoiceNumber)
		if hostedURL != "" {
			fmt.Fprintf(&b, "View online: %s\n\n", hostedURL)
		}
	} else {
		fmt.Fprintf(&b, "Invoice %s is ready.\n\nAmount due: %s\nCurrency: %s\n\n", invoiceNumber, amount, strings.ToUpper(currency))
		if hostedURL != "" {
			fmt.Fprintf(&b, "View or pay online: %s\n\n", hostedURL)
		}
	}
	b.WriteString("The invoice PDF is attached for your records.\n")
	return b.String()
}

func receiptTextBody(customerName, invoiceNumber, amount, hostedURL string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Hi %s,\n\nThanks — we received your payment of %s for invoice %s.\n\n", customerName, amount, invoiceNumber)
	if hostedURL != "" {
		fmt.Fprintf(&b, "View receipt: %s\n", hostedURL)
	}
	return b.String()
}

func dunningWarningTextBody(customerName, invoiceNumber string, attemptNumber, maxAttempts int, nextRetryDate, failureReason, hostedURL string) string {
	// No final-attempt branch — mirrors renderDunningWarningHTML: the
	// dunning service never sends a warning on the exhausting attempt.
	var b strings.Builder
	fmt.Fprintf(&b, "Hi %s,\n\n", customerName)
	fmt.Fprintf(&b, "Attempt %d of %d to charge invoice %s was declined.\n", attemptNumber, maxAttempts, invoiceNumber)
	if strings.TrimSpace(failureReason) != "" {
		fmt.Fprintf(&b, "Reason: %s\n", failureReason)
	}
	fmt.Fprintf(&b, "\nWe'll try again on %s.\n\n", nextRetryDate)
	if hostedURL != "" {
		fmt.Fprintf(&b, "Pay invoice: %s\n", hostedURL)
	}
	return b.String()
}

func dunningEscalationTextBody(customerName, invoiceNumber string, outcome domain.DunningEscalationOutcome, hostedURL string) string {
	// Same customer copy as the HTML alternative (dunningEscalationCopy).
	// Pre-fix this text part still rendered the raw internal enum —
	// "Action taken: mark_uncollectible", the exact P13 symptom, shown to
	// every text-only client — and offered a "Resolve invoice" link even
	// for invoices the hosted page refuses payment on.
	sentence, stillPayable := dunningEscalationCopy(outcome)
	var b strings.Builder
	fmt.Fprintf(&b, "Hi %s,\n\nAll payment attempts for invoice %s were unsuccessful.\n\n%s\n\n", customerName, invoiceNumber, sentence)
	if hostedURL != "" {
		if stillPayable {
			fmt.Fprintf(&b, "Pay invoice: %s\n", hostedURL)
		} else {
			fmt.Fprintf(&b, "View invoice: %s\n", hostedURL)
		}
	}
	return b.String()
}

func paymentFailedTextBody(customerName, invoiceNumber, reason, hostedURL string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Hi %s,\n\nWe couldn't process payment for invoice %s.\n", customerName, invoiceNumber)
	if strings.TrimSpace(reason) != "" {
		fmt.Fprintf(&b, "Reason: %s\n", reason)
	}
	if hostedURL != "" {
		fmt.Fprintf(&b, "\nUpdate payment method: %s\n", hostedURL)
	}
	return b.String()
}

func paymentUpdateRequestTextBody(customerName, invoiceNumber, amount, updateURL string) string {
	// No-PM path only (see renderPaymentSetupLinkHTML) — never claim a
	// decline the customer didn't have.
	return fmt.Sprintf(`Hi %s,

Invoice %s (%s) is ready. Add a payment method using the secure link below and we'll collect it automatically.

The link expires in 24 hours and can only be used once.

%s
`, customerName, invoiceNumber, amount, updateURL)
}

func paymentSetupLinkTextBody(customerName, operatorNote, setupURL string) string {
	intro := operatorNote
	if intro == "" {
		intro = "Please add a payment method on file so we can process your billing. Use the secure link below — your card details go directly to our payment processor and never touch our servers."
	}
	return fmt.Sprintf(`Hi %s,

%s

%s

The link expires in 24 hours and can only be used once.
`, customerName, intro, setupURL)
}

// ---- Low-level send helpers ----

// richMessage is a multipart/alternative-or-mixed email payload. When
// AttachmentData is set the envelope becomes multipart/mixed containing
// a multipart/alternative (text+html) part plus the attachment; otherwise
// it's multipart/alternative directly.
type richMessage struct {
	TenantID string
	To       string
	// Cc are the additional billing recipients (ADR-082) — rendered as a
	// visible Cc: header and RCPT'd in the same SMTP transaction. Only
	// billing-document/billing-state emails populate it; the
	// credential-bearing types have no cc parameter by construction.
	Cc             []string
	Subject        string
	TextBody       string
	HTMLBody       string
	FromName       string
	AttachmentName string
	AttachmentData []byte
}

func (s *Sender) sendRich(ctx context.Context, msg richMessage) error {
	if !s.IsConfigured() {
		return ErrSMTPNotConfigured
	}
	if err := s.checkSuppression(ctx, msg.TenantID, msg.To); err != nil {
		// Primary suppressed = the whole send is blocked, never
		// downgraded to a CC-only delivery — the CC list is an audience,
		// not an alternate recipient.
		return err
	}
	cc := s.filterSuppressedCC(ctx, msg.TenantID, msg.Cc)

	var body strings.Builder
	fmt.Fprintf(&body, "From: %s\r\n", fromHeaderValue(msg.FromName, s.from))
	fmt.Fprintf(&body, "To: %s\r\n", msg.To)
	if len(cc) > 0 {
		// Visible CC (Orb/Stripe semantics). Each address rendered via
		// mail.Address so a CRLF-bearing value can never smuggle a raw
		// header line — same neutralization as fromHeaderValue.
		rendered := make([]string, len(cc))
		for i, a := range cc {
			rendered[i] = (&mail.Address{Address: a}).String()
		}
		fmt.Fprintf(&body, "Cc: %s\r\n", strings.Join(rendered, ", "))
	}
	fmt.Fprintf(&body, "Subject: %s\r\n", encodeHeader(msg.Subject))
	body.WriteString("MIME-Version: 1.0\r\n")
	body.WriteString(correlationHeader(ctx))

	altBoundary := "velox-alt-" + nonce()
	mixedBoundary := "velox-mix-" + nonce()

	if len(msg.AttachmentData) > 0 {
		fmt.Fprintf(&body, "Content-Type: multipart/mixed; boundary=\"%s\"\r\n\r\n", mixedBoundary)
		fmt.Fprintf(&body, "--%s\r\n", mixedBoundary)
	}

	// multipart/alternative wrapper for text + html
	fmt.Fprintf(&body, "Content-Type: multipart/alternative; boundary=\"%s\"\r\n\r\n", altBoundary)

	// text part
	fmt.Fprintf(&body, "--%s\r\n", altBoundary)
	body.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	body.WriteString("Content-Transfer-Encoding: 7bit\r\n\r\n")
	body.WriteString(msg.TextBody)
	body.WriteString("\r\n")

	// html part
	fmt.Fprintf(&body, "--%s\r\n", altBoundary)
	body.WriteString("Content-Type: text/html; charset=utf-8\r\n")
	body.WriteString("Content-Transfer-Encoding: 7bit\r\n\r\n")
	body.WriteString(msg.HTMLBody)
	body.WriteString("\r\n")

	fmt.Fprintf(&body, "--%s--\r\n", altBoundary)

	if len(msg.AttachmentData) > 0 {
		// PDF attachment part
		fmt.Fprintf(&body, "--%s\r\n", mixedBoundary)
		fmt.Fprintf(&body, "Content-Type: application/pdf; name=\"%s\"\r\n", msg.AttachmentName)
		body.WriteString("Content-Transfer-Encoding: base64\r\n")
		fmt.Fprintf(&body, "Content-Disposition: attachment; filename=\"%s\"\r\n\r\n", msg.AttachmentName)
		body.WriteString(wrapBase64(base64.StdEncoding.EncodeToString(msg.AttachmentData), 76))
		fmt.Fprintf(&body, "\r\n--%s--\r\n", mixedBoundary)
	}

	if err := s.deliver(ctx, msg.TenantID, msg.To, cc, []byte(body.String())); err != nil {
		// deliver never returns a CC-only rejection (those are handled
		// and bounce-attributed internally), so attributing this error's
		// bounce to the PRIMARY recipient stays correct.
		slog.Error("send email failed", "to", msg.To, "subject", msg.Subject, "error", err)
		s.reportBounceIfPermanent(ctx, msg.TenantID, msg.To, err)
		return fmt.Errorf("send email: %w", err)
	}
	slog.Info("email sent", "to", msg.To, "cc_count", len(cc), "subject", msg.Subject, "html", true)
	return nil
}

// filterSuppressedCC drops suppressed addresses from the CC list —
// dispatch-time so a bounce recorded between enqueue and send is still
// honored. Drops are logged, never errors: a dead CC must not block the
// primary recipient's billing email (and failing the outbox row would
// re-deliver to the primary on retry). One shared timeout bounds the
// whole set so a slow lookup can't multiply into the per-row budget.
func (s *Sender) filterSuppressedCC(ctx context.Context, tenantID string, cc []string) []string {
	if len(cc) == 0 || s.suppression == nil || tenantID == "" {
		return cc
	}
	supCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	kept := make([]string, 0, len(cc))
	for _, addr := range cc {
		suppressed, reason, err := s.suppression.IsSuppressed(supCtx, tenantID, addr)
		if err != nil {
			// Fail-open per address, same posture as the primary check.
			slog.Warn("cc suppression check failed; keeping recipient",
				"tenant_id", tenantID, "cc", addr, "error", err)
			kept = append(kept, addr)
			continue
		}
		if suppressed {
			slog.Info("cc recipient suppressed — dropped from send",
				"tenant_id", tenantID, "cc", addr, "reason", reason)
			continue
		}
		kept = append(kept, addr)
	}
	return kept
}

// deliver dispatches the rendered RFC-5322 message via SMTP. ONE code
// path for all three transport modes (P5 — the previous split routed
// starttls/none through smtp.SendMail, which had NO dial timeout, NO
// exchange deadline, and — critically — upgraded to TLS only
// OPPORTUNISTICALLY: a MITM stripping the STARTTLS advertisement
// silently downgraded invoice traffic to plaintext):
//
//   - dial: DialContext with a 10s cap (honors ctx cancellation);
//   - deadline: the whole SMTP exchange bounded by min(30s, ctx
//     remaining) so a stalled server can't out-live the per-row budget;
//   - starttls: STRICT — a server that doesn't advertise STARTTLS is a
//     hard error (set SMTP_TLS=none explicitly for local sandboxes);
//   - none: forbidden in production (fails loudly at send; the config
//     comment used to CLAIM this check existed);
//   - AUTH: only when credentials are configured AND the server
//     advertises AUTH (Mailpit and friends advertise none — the old
//     implicit path always authed and broke no-auth sandboxes).
//
// Per-recipient policy (ADR-082): the primary `to` RCPT keeps today's
// semantics — any rejection is a hard error (sendRich attributes its
// bounce to the primary). A CC RCPT *protocol* rejection (a
// *textproto.Error reply) is handled INTERNALLY: WARN + bounce-report
// for the FAILING address, drop it, continue — it must never surface as
// deliver's return error or the primary customer would be wrongly
// flipped to bounced. A CC *transport* error (dial/deadline/IO) aborts
// the whole send: DATA hasn't been sent yet, so the retry is safe and
// nothing was delivered.
func (s *Sender) deliver(ctx context.Context, tenantID, to string, cc []string, body []byte) error {
	if s.tlsMode == "none" && s.prodEnv {
		return fmt.Errorf("SMTP_TLS=none is forbidden in production — use starttls or implicit")
	}
	addr := fmt.Sprintf("%s:%s", s.host, s.port)

	dialer := &net.Dialer{Timeout: 10 * time.Second}
	var conn net.Conn
	var err error
	if s.tlsMode == "implicit" {
		// TLS-from-handshake (port 465 / SMTPS).
		conn, err = (&tls.Dialer{NetDialer: dialer, Config: &tls.Config{ServerName: s.host}}).DialContext(ctx, "tcp", addr)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("smtp dial (%s): %w", s.tlsMode, err)
	}

	// Bound the whole exchange: 30s, clamped to the caller's remaining
	// budget so the per-row ctx (outbox dispatcher) is authoritative.
	deadline := time.Now().Add(30 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetDeadline(deadline)

	// Close the raw conn unconditionally: Quit's command fails on a
	// dead/deadline-expired server BEFORE the transport closes, leaking
	// one fd per stalled row per tick (verifier catch). Close after a
	// clean Quit is an idempotent no-op error.
	defer func() { _ = conn.Close() }()
	c, err := smtp.NewClient(conn, s.host)
	if err != nil {
		return fmt.Errorf("smtp client: %w", err)
	}
	defer func() { _ = c.Quit() }()

	if s.tlsMode == "starttls" {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return fmt.Errorf("smtp server %s does not advertise STARTTLS (strict mode) — for local no-TLS sandboxes set SMTP_TLS=none explicitly", s.host)
		}
		if err := c.StartTLS(&tls.Config{ServerName: s.host}); err != nil {
			return fmt.Errorf("smtp starttls: %w", err)
		}
	}

	if s.username != "" || s.password != "" {
		if ok, _ := c.Extension("AUTH"); ok {
			if err := c.Auth(smtp.PlainAuth("", s.username, s.password, s.host)); err != nil {
				return fmt.Errorf("smtp auth: %w", err)
			}
		}
	}
	if err := c.Mail(s.from); err != nil {
		return fmt.Errorf("smtp mail from: %w", err)
	}
	if err := c.Rcpt(to); err != nil {
		return fmt.Errorf("smtp rcpt to: %w", err)
	}
	for _, addr := range cc {
		if err := c.Rcpt(addr); err != nil {
			var proto *textproto.Error
			if errors.As(err, &proto) {
				// The server REPLIED rejecting this one recipient — the
				// transaction is intact and the remaining recipients
				// still receive. Attribute the bounce to the CC address,
				// never to the primary.
				slog.Warn("smtp rcpt rejected for cc recipient — dropped from this send",
					"cc", addr, "code", proto.Code, "error", err)
				s.reportBounceIfPermanent(ctx, tenantID, addr, err)
				continue
			}
			// Transport/IO failure: nothing has been delivered (DATA not
			// sent) — abort so the outbox retry re-drives the whole send.
			return fmt.Errorf("smtp rcpt cc: %w", err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("smtp data: %w", err)
	}
	if _, err := w.Write(body); err != nil {
		_ = w.Close()
		return fmt.Errorf("smtp write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp data close: %w", err)
	}
	return nil
}

// sendPlain is the text-only path for operator emails that stay simple.
func (s *Sender) sendPlain(ctx context.Context, tenantID, to, fromName, subject, body string) error {
	if !s.IsConfigured() {
		return ErrSMTPNotConfigured
	}
	if err := s.checkSuppression(ctx, tenantID, to); err != nil {
		return err
	}
	var msg strings.Builder
	fmt.Fprintf(&msg, "From: %s\r\n", fromHeaderValue(fromName, s.from))
	fmt.Fprintf(&msg, "To: %s\r\n", to)
	fmt.Fprintf(&msg, "Subject: %s\r\n", encodeHeader(subject))
	msg.WriteString("MIME-Version: 1.0\r\n")
	msg.WriteString(correlationHeader(ctx))
	msg.WriteString("Content-Type: text/plain; charset=utf-8\r\n\r\n")
	msg.WriteString(body)

	if err := s.deliver(ctx, tenantID, to, nil, []byte(msg.String())); err != nil {
		slog.Error("send email failed", "to", to, "subject", subject, "error", err)
		s.reportBounceIfPermanent(ctx, tenantID, to, err)
		return fmt.Errorf("send email: %w", err)
	}
	slog.Info("email sent", "to", to, "subject", subject, "html", false)
	return nil
}

// ---- Utilities ----

// fromHeaderValue renders the From header. mail.Address.String()
// RFC-2047-encodes the display name and never emits raw CR/LF, so a tenant
// CompanyName carrying a header-injection payload (e.g. "Acme\r\nBcc: …")
// cannot smuggle extra SMTP headers through the From line.
func fromHeaderValue(fromName, addr string) string {
	if fromName == "" {
		return addr
	}
	return (&mail.Address{Name: fromName, Address: addr}).String()
}

// encodeHeader Q-encodes any header value (e.g. Subject) that contains
// non-ASCII or control characters — CR/LF included — neutralizing header
// injection regardless of which upstream field flowed in.
func encodeHeader(v string) string {
	return mime.QEncoding.Encode("utf-8", v)
}

func formatAmount(cents int64, currency string) string {
	return fmt.Sprintf("%s %d.%02d", strings.ToUpper(currency), cents/100, cents%100)
}

// wrapBase64 inserts CRLF every n chars. RFC 2045 recommends ≤76 per
// line; most clients accept longer but some classical relays don't.
func wrapBase64(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i += n {
		end := i + n
		if end > len(s) {
			end = len(s)
		}
		b.WriteString(s[i:end])
		b.WriteString("\r\n")
	}
	return strings.TrimRight(b.String(), "\r\n")
}

// nonce produces a short MIME-boundary-safe token. No cryptographic
// requirement — just uniqueness within a single message.
func nonce() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}

func envOr(key, fallback string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	return v
}

func creditNoteTextBody(customerName, creditNoteNumber, invoiceNumber, amount string) string {
	return fmt.Sprintf(`Hi %s,

Credit note %s has been issued against invoice %s.

Amount credited: %s

The credit note PDF is attached for your records.
`, customerName, creditNoteNumber, invoiceNumber, amount)
}
