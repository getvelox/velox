package email

import (
	"bytes"
	"html"
	"html/template"
	"strings"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// Branding is the per-tenant visual identity a sender pulls from
// TenantSettings before rendering an outbound email. Every field is
// optional — NewSender falls back to Velox defaults when a tenant
// hasn't set their own (T0-11 onboarding surfaces this in the
// dashboard, but cold-start tenants will still send branded-enough
// email).
type Branding struct {
	CompanyName string
	LogoURL     string
	BrandColor  string // 7-char hex, e.g. #1f6feb. Empty = fall back to neutral.
	SupportURL  string
	FromName    string // Appears as "From: FromName <from@address>" when set.
	FromAddress string // Defaults to SMTP_FROM at send time.
}

// fill fills in Velox defaults so every rendered email has something in
// each slot; keeps the templates simple (no "if empty" branches) at the
// cost of one method call per send.
func (b Branding) fill() Branding {
	if b.CompanyName == "" {
		b.CompanyName = "Velox Billing"
	}
	// BrandColor / LogoURL / SupportURL stay empty when unset — templates
	// {{if}}-gate their renders so "no brand color" means no accent bar,
	// and "no logo" means no <img> tag. Defensive fallbacks would look
	// weirder than absence.
	return b
}

// layoutTemplate is the shared chrome every customer-facing HTML email
// wraps around its specific content. Mirrors the hosted invoice page
// aesthetic (T0-17.5) so a customer landing on the page from an email
// CTA sees visual continuity. Inline styles throughout because email
// clients (Outlook especially) strip <style> blocks inconsistently —
// industry best practice is inline everything.
//
// Structure:
//   - optional 3px brand_color accent bar at the very top
//   - header: tenant logo + company name
//   - content: per-email HTML (injected as template.HTML, pre-rendered)
//   - optional CTA button
//   - footer: support link + "Powered by Velox Billing" micro-credit
const layoutHTML = `<!DOCTYPE html>
<html>
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>{{.Subject}}</title>
</head>
<body style="margin:0;padding:0;background-color:#f5f6f8;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,Helvetica,Arial,sans-serif;color:#1f2937;">
  <table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="background-color:#f5f6f8;">
    <tr>
      <td align="center" style="padding:32px 16px;">
        <table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="max-width:600px;background:#ffffff;border-radius:8px;overflow:hidden;box-shadow:0 1px 3px rgba(0,0,0,0.04);">
          {{if .BrandColor}}<tr><td style="height:3px;background-color:{{.BrandColor}};line-height:0;font-size:0;">&nbsp;</td></tr>{{end}}
          <tr>
            <td style="padding:24px 32px;border-bottom:1px solid #e5e7eb;">
              <table role="presentation" cellpadding="0" cellspacing="0">
                <tr>
                  {{if .LogoURL}}<td style="padding-right:12px;vertical-align:middle;"><img src="{{.LogoURL}}" alt="" height="32" style="display:block;max-height:32px;"></td>{{end}}
                  <td style="vertical-align:middle;font-size:16px;font-weight:600;color:#111827;">{{.CompanyName}}</td>
                </tr>
              </table>
            </td>
          </tr>
          <tr>
            <td style="padding:32px;font-size:15px;line-height:1.6;color:#1f2937;">
              {{.Content}}
              {{if .CTAURL}}
              <table role="presentation" cellpadding="0" cellspacing="0" style="margin:24px 0 8px;">
                <tr>
                  <td style="border-radius:6px;background-color:{{if .BrandColor}}{{.BrandColor}}{{else}}#111827{{end}};">
                    <a href="{{.CTAURL}}" style="display:inline-block;padding:12px 24px;color:#ffffff;text-decoration:none;font-weight:500;font-size:15px;">{{.CTALabel}}</a>
                  </td>
                </tr>
              </table>
              {{end}}
            </td>
          </tr>
          <tr>
            <td style="padding:20px 32px;border-top:1px solid #e5e7eb;font-size:13px;color:#6b7280;">
              {{if .SupportURL}}Need help? <a href="{{.SupportURL}}" style="color:#6b7280;text-decoration:underline;">Contact support</a><br>{{end}}
              <span style="color:#9ca3af;">Powered by Velox Billing</span>
            </td>
          </tr>
        </table>
      </td>
    </tr>
  </table>
</body>
</html>`

// layoutInputs is the view model fed into layoutHTML. Content is
// pre-rendered HTML (template.HTML marks it pre-escaped) so per-email
// templates can inject their own safe markup without html/template
// double-escaping it.
type layoutInputs struct {
	Subject     string
	CompanyName string
	LogoURL     string
	BrandColor  string
	SupportURL  string
	Content     template.HTML
	CTAURL      string
	CTALabel    string
}

var layoutTmpl = template.Must(template.New("layout").Parse(layoutHTML))

// renderLayout assembles the final HTML email by wrapping pre-rendered
// content in the shared chrome.
func renderLayout(inputs layoutInputs) (string, error) {
	var buf bytes.Buffer
	if err := layoutTmpl.Execute(&buf, inputs); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// escape wraps html.EscapeString so per-email helpers can build
// template.HTML content without dragging in the full html/template
// pipeline for single-field interpolation.
func escape(s string) string { return html.EscapeString(s) }

// --- Per-email content builders ---

// Each builder returns a (subject, html-content, cta-url, cta-label)
// tuple. The caller renders through renderLayout then ships via the
// multipart pipeline alongside the plaintext fallback.

// zeroDue is the plumbed fact `amount_due_cents == 0` (paid or fully
// credit-covered at send time). The copy adapts: the old rendering titled
// a settled invoice "Invoice X — USD 0.00" with a "View & pay" CTA —
// asking the customer to pay nothing (2026-07-19 FLOW E copy finding).
func renderInvoiceHTML(customerName, invoiceNumber, amount string, hostedURL string, zeroDue bool) (subject, contentHTML, ctaURL, ctaLabel string) {
	subject = "Invoice " + invoiceNumber + " — " + amount
	if zeroDue {
		subject = "Invoice " + invoiceNumber + " — no payment due"
	}
	var b strings.Builder
	b.WriteString(`<h1 style="margin:0 0 12px;font-size:20px;color:#111827;">Your invoice is ready</h1>`)
	b.WriteString(`<p style="margin:0 0 8px;color:#4b5563;">Hi ` + escape(customerName) + `,</p>`)
	b.WriteString(`<p style="margin:0 0 20px;color:#4b5563;">Invoice <strong style="color:#111827;">` + escape(invoiceNumber) + `</strong> is now available.</p>`)
	b.WriteString(`<div style="background:#f9fafb;border-radius:8px;padding:16px 20px;margin:8px 0 16px;">`)
	if zeroDue {
		b.WriteString(`<div style="font-size:13px;color:#6b7280;text-transform:uppercase;letter-spacing:0.05em;">Amount due</div>`)
		b.WriteString(`<div style="font-size:24px;font-weight:600;color:#111827;margin-top:4px;font-variant-numeric:tabular-nums;">` + escape(amount) + ` — settled</div>`)
	} else {
		b.WriteString(`<div style="font-size:13px;color:#6b7280;text-transform:uppercase;letter-spacing:0.05em;">Amount due</div>`)
		b.WriteString(`<div style="font-size:24px;font-weight:600;color:#111827;margin-top:4px;font-variant-numeric:tabular-nums;">` + escape(amount) + `</div>`)
	}
	b.WriteString(`</div>`)
	switch {
	case zeroDue && hostedURL != "":
		b.WriteString(`<p style="margin:0 0 16px;color:#4b5563;">This invoice is settled — no payment is needed. The PDF is attached for your records.</p>`)
		ctaURL = hostedURL
		ctaLabel = "View invoice"
	case zeroDue:
		b.WriteString(`<p style="margin:0 0 16px;color:#4b5563;">This invoice is settled — no payment is needed. The PDF is attached for your records.</p>`)
	case hostedURL != "":
		b.WriteString(`<p style="margin:0 0 16px;color:#4b5563;">Tap the button below to view or pay your invoice. The PDF is also attached for your records.</p>`)
		ctaURL = hostedURL
		ctaLabel = "View & pay invoice"
	default:
		b.WriteString(`<p style="margin:0 0 16px;color:#4b5563;">Please find the invoice PDF attached.</p>`)
	}
	return subject, b.String(), ctaURL, ctaLabel
}

func renderReceiptHTML(customerName, invoiceNumber, amount, hostedURL string) (subject, contentHTML, ctaURL, ctaLabel string) {
	subject = "Payment received for invoice " + invoiceNumber
	var b strings.Builder
	b.WriteString(`<h1 style="margin:0 0 12px;font-size:20px;color:#111827;">Payment received</h1>`)
	b.WriteString(`<p style="margin:0 0 8px;color:#4b5563;">Hi ` + escape(customerName) + `,</p>`)
	b.WriteString(`<p style="margin:0 0 20px;color:#4b5563;">Thanks — we've received your payment of <strong style="color:#111827;">` + escape(amount) + `</strong> for invoice <strong style="color:#111827;">` + escape(invoiceNumber) + `</strong>.</p>`)
	if hostedURL != "" {
		ctaURL = hostedURL
		ctaLabel = "View receipt"
	}
	return subject, b.String(), ctaURL, ctaLabel
}

func renderDunningWarningHTML(customerName, invoiceNumber string, attemptNumber, maxAttempts int, nextRetryDate, failureReason, hostedURL string) (subject, contentHTML, ctaURL, ctaLabel string) {
	// No final-attempt variant: the dunning service deliberately skips
	// the warning on the attempt that exhausts the retry budget (N-1
	// warnings during retries 1..N-1, then ONE escalation email — see
	// willExhaustThisAttempt in internal/dunning/service.go), so every
	// warning rendered here has a real next retry ahead of it.
	subject = "Action required — payment retry for invoice " + invoiceNumber
	var b strings.Builder
	b.WriteString(`<h1 style="margin:0 0 12px;font-size:20px;color:#111827;">We weren't able to process your payment</h1>`)
	b.WriteString(`<p style="margin:0 0 8px;color:#4b5563;">Hi ` + escape(customerName) + `,</p>`)
	b.WriteString(`<p style="margin:0 0 12px;color:#4b5563;">Attempt <strong style="color:#111827;">` + escape(itoa(attemptNumber)) + ` of ` + escape(itoa(maxAttempts)) + `</strong> to charge invoice <strong style="color:#111827;">` + escape(invoiceNumber) + `</strong> was declined.</p>`)
	// Surface the decline reason inline so customers can act
	// (insufficient_funds → top up; lost_card → swap card). Empty
	// reason = no callout, no false noise.
	if strings.TrimSpace(failureReason) != "" {
		b.WriteString(`<div style="background:#fef2f2;border-left:3px solid #fca5a5;padding:10px 14px;margin:0 0 16px;color:#7f1d1d;font-size:13px;">`)
		b.WriteString(`Reason: ` + escape(failureReason))
		b.WriteString(`</div>`)
	}
	b.WriteString(`<p style="margin:0 0 16px;color:#4b5563;">We'll try again on <strong style="color:#111827;">` + escape(nextRetryDate) + `</strong>. To avoid further retries, please pay the invoice or update your payment method.</p>`)
	if hostedURL != "" {
		ctaURL = hostedURL
		ctaLabel = "Pay invoice"
	}
	return subject, b.String(), ctaURL, ctaLabel
}

func renderDunningEscalationHTML(customerName, invoiceNumber string, outcome domain.DunningEscalationOutcome, hostedURL string) (subject, contentHTML, ctaURL, ctaLabel string) {
	subject = "Action required for invoice " + invoiceNumber
	var b strings.Builder
	b.WriteString(`<h1 style="margin:0 0 12px;font-size:20px;color:#111827;">We couldn't collect payment</h1>`)
	b.WriteString(`<p style="margin:0 0 8px;color:#4b5563;">Hi ` + escape(customerName) + `,</p>`)
	b.WriteString(`<p style="margin:0 0 16px;color:#4b5563;">All payment attempts for invoice <strong style="color:#111827;">` + escape(invoiceNumber) + `</strong> were unsuccessful.</p>`)

	// Never render the internal enum — that leak (P13) showed the debtor
	// "Action taken: mark_uncollectible". dunningEscalationCopy composes
	// the customer sentence from the OUTCOME and is shared with the
	// plaintext part.
	sentence, stillPayable := dunningEscalationCopy(outcome)
	b.WriteString(`<p style="margin:0 0 16px;color:#4b5563;">` + escape(sentence) + `</p>`)
	if hostedURL != "" {
		ctaURL = hostedURL
		ctaLabel = "Pay invoice"
		if !stillPayable {
			ctaLabel = "View invoice"
		}
	}
	return subject, b.String(), ctaURL, ctaLabel
}

// dunningEscalationCopy composes the customer-facing sentence used by BOTH
// multipart/alternative parts of the escalation email. Single source on
// purpose — the P13 raw-enum leak was fixed in the HTML part only, and the
// plaintext twin kept showing text-only clients "Action taken:
// mark_uncollectible" for another two months.
//
// It reads the OUTCOME, never the policy (ADR-112): exhaustion now settles
// two independent things, and only the outcome knows which of them applied
// to this invoice. A one-off invoice under a cancel policy cancels nothing,
// so it must get the neutral sentence.
//
// stillPayable=false once the invoice is written off — a written-off
// invoice has no Pay button on the hosted page (ADR-110), so callers offer
// the link as "View invoice" and point at support rather than a dead end.
// It is the invoice half that decides this; the subscription half never
// changes whether THIS invoice can be paid online.
func dunningEscalationCopy(o domain.DunningEscalationOutcome) (sentence string, stillPayable bool) {
	// Subscription clause. Pause is collection-only (keep_as_draft):
	// service continues and only automatic charging stops, so no "service
	// is paused" / "resume service" claim is made.
	var subClause string
	switch {
	case o.SubscriptionCanceled:
		subClause = "Your subscription has been canceled."
	case o.SubscriptionPaused:
		subClause = "Automatic payment collection on your account has been paused."
	}

	if o.InvoiceWrittenOff {
		// Closed for online payment, and the balance is NOT described as
		// still collectible through the link — it isn't.
		s := "This invoice has been closed for online payment. Please contact support to arrange payment."
		if subClause != "" {
			s = subClause + " " + s
		}
		return s, false
	}

	if subClause != "" {
		return subClause + " The outstanding balance remains due — you can settle it using the link below.", true
	}

	// Nothing applied: a (none, none) policy, a one-off invoice whose
	// policy only acts on subscriptions, or an unwired mover. Neutral — no
	// state change is asserted.
	return "The invoice remains due. Please settle it using the link below or reach out to support.", true
}

func renderPaymentFailedHTML(customerName, invoiceNumber, reason, hostedURL string) (subject, contentHTML, ctaURL, ctaLabel string) {
	subject = "Payment failed for invoice " + invoiceNumber
	var b strings.Builder
	b.WriteString(`<h1 style="margin:0 0 12px;font-size:20px;color:#111827;">Payment unsuccessful</h1>`)
	b.WriteString(`<p style="margin:0 0 8px;color:#4b5563;">Hi ` + escape(customerName) + `,</p>`)
	b.WriteString(`<p style="margin:0 0 16px;color:#4b5563;">We weren't able to process the charge for invoice <strong style="color:#111827;">` + escape(invoiceNumber) + `</strong>.</p>`)
	if strings.TrimSpace(reason) != "" {
		b.WriteString(`<div style="background:#fef2f2;border-left:3px solid #fca5a5;padding:12px 16px;margin:8px 0 16px;color:#7f1d1d;font-size:14px;">` + escape(reason) + `</div>`)
	}
	b.WriteString(`<p style="margin:0 0 16px;color:#4b5563;">Pay the invoice or update your payment method to avoid service interruption.</p>`)
	if hostedURL != "" {
		ctaURL = hostedURL
		ctaLabel = "Pay invoice"
	}
	return subject, b.String(), ctaURL, ctaLabel
}

// renderPaymentUpdateRequestHTML is the invoice-context thin wrapper
// retained for back-compat with the SendPaymentSetupRequest call
// site. Delegates to the unified renderPaymentSetupLinkHTML so the
// template stays single-source.
func renderPaymentUpdateRequestHTML(customerName, invoiceNumber, amountDue, updateURL string, writtenOff bool) (subject, contentHTML, ctaURL, ctaLabel string) {
	return renderPaymentSetupLinkHTML(paymentSetupLinkContext{
		CustomerName:   customerName,
		SetupURL:       updateURL,
		InvoiceNumber:  invoiceNumber,
		AmountDueLabel: amountDue,
		WrittenOff:     writtenOff,
	})
}

// paymentSetupLinkContext is the unified shape for both
// operator-initiated and engine-fired "add a payment method" emails.
// Invoice fields are optional: when InvoiceNumber is empty the
// template renders the generic operator-initiated copy; when set
// it embeds invoice context ("for invoice X — $Y") so the customer
// reads exactly why they need to add a card.
type paymentSetupLinkContext struct {
	CustomerName   string
	OperatorNote   string // optional free-form operator note
	SetupURL       string
	InvoiceNumber  string // optional — engine-fired no-PM-at-finalize path
	AmountDueLabel string // formatted ($X.XX); empty when InvoiceNumber empty
	// WrittenOff marks the invoice as already written off as bad debt. It
	// changes ONE thing and it is the honesty of the whole email: no machine
	// ever collects a written-off invoice (ADR-110 — the auto-charge and
	// dunning claims both pin `finalized`; only an operator may charge one),
	// so the ordinary "we'll collect it automatically" promise would be a
	// commitment the engine cannot keep. Adding a card here is genuinely
	// useful — it is what lets the operator complete the recovery — but the
	// customer must be told a person will do it, not a machine.
	WrittenOff bool
}

// renderPaymentSetupLinkHTML is the single template used by every
// "add a payment method" email send (operator-initiated +
// engine-fired no-PM-at-finalize). Replaces the previous two-
// template split (renderPaymentUpdateRequestHTML +
// renderPaymentSetupLinkHTML) so subject + body stay consistent
// across send paths. Industry parity: Stripe "Send Payment Method",
// Chargebee "Request Payment Method" — both use one template
// internally with optional invoice context.
func renderPaymentSetupLinkHTML(ctx paymentSetupLinkContext) (subject, contentHTML, ctaURL, ctaLabel string) {
	hasInvoice := ctx.InvoiceNumber != ""
	// Invoice context means the NO-PAYMENT-METHOD path (finalize-time or
	// operator resend) — never a decline. Real declines are emailed by the
	// dunning templates and link to the hosted invoice, not here. The
	// previous copy ("We couldn't process payment… update your card")
	// asserted a decline to customers who were never charged and had no
	// card to update — born-false for 100% of this template's audience
	// (2026-07-22 payment-surfacing audit, P1-2).
	if hasInvoice {
		subject = "Set up payment for invoice " + ctx.InvoiceNumber
	} else {
		subject = "Add a payment method"
	}
	var b strings.Builder
	heading := "Add a payment method"
	if hasInvoice {
		heading = "Set up your payment method"
	}
	b.WriteString(`<h1 style="margin:0 0 12px;font-size:20px;color:#111827;">` + escape(heading) + `</h1>`)
	b.WriteString(`<p style="margin:0 0 8px;color:#4b5563;">Hi ` + escape(ctx.CustomerName) + `,</p>`)
	switch {
	case ctx.OperatorNote != "":
		// Operator's custom note renders verbatim — they may have
		// added billing context ("your card on file expired") that
		// the template shouldn't try to autocompose.
		b.WriteString(`<p style="margin:0 0 16px;color:#4b5563;">` + escape(ctx.OperatorNote) + `</p>`)
	case hasInvoice:
		b.WriteString(`<p style="margin:0 0 8px;color:#4b5563;">Invoice <strong style="color:#111827;">` + escape(ctx.InvoiceNumber) + `</strong> (<strong style="color:#111827;">` + escape(ctx.AmountDueLabel) + `</strong>) is ready.</p>`)
		if ctx.WrittenOff {
			b.WriteString(`<p style="margin:0 0 16px;color:#4b5563;">Add a payment method with the secure link below and we'll be in touch to settle it — your card details go directly to our payment processor and never touch our servers.</p>`)
		} else {
			b.WriteString(`<p style="margin:0 0 16px;color:#4b5563;">Add a payment method with the secure link below and we'll collect it automatically — your card details go directly to our payment processor and never touch our servers.</p>`)
		}
	default:
		b.WriteString(`<p style="margin:0 0 16px;color:#4b5563;">Please add a payment method on file so we can process your billing. Use the secure link below — your card details go directly to our payment processor and never touch our servers.</p>`)
	}
	b.WriteString(`<p style="margin:0 0 16px;color:#6b7280;font-size:13px;">The link expires in 24 hours and can only be used once.</p>`)
	ctaURL = ctx.SetupURL
	ctaLabel = "Add payment method"
	return subject, b.String(), ctaURL, ctaLabel
}

// itoa is a local shim to avoid pulling strconv just for escape() input.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

// renderCreditNoteHTML is the credit-note document email (ADR-082
// rider). No CTA — no hosted CN page exists; the PDF is the document.
// invoiceNumber names the APPLIED invoice so finance can match the
// credit to the bill it adjusts.
func renderCreditNoteHTML(customerName, creditNoteNumber, invoiceNumber, amount, companyName string) (subject, contentHTML string) {
	from := companyName
	if from == "" {
		from = "your billing team"
	}
	subject = "Credit note " + creditNoteNumber + " from " + from
	var b strings.Builder
	b.WriteString(`<h1 style="margin:0 0 12px;font-size:20px;color:#111827;">Credit note issued</h1>`)
	b.WriteString(`<p style="margin:0 0 8px;color:#4b5563;">Hi ` + escape(customerName) + `,</p>`)
	b.WriteString(`<p style="margin:0 0 20px;color:#4b5563;">Credit note <strong style="color:#111827;">` + escape(creditNoteNumber) + `</strong> has been issued against invoice <strong style="color:#111827;">` + escape(invoiceNumber) + `</strong>.</p>`)
	b.WriteString(`<div style="background:#f9fafb;border-radius:8px;padding:16px 20px;margin:8px 0 16px;">`)
	b.WriteString(`<div style="font-size:13px;color:#6b7280;text-transform:uppercase;letter-spacing:0.05em;">Amount credited</div>`)
	b.WriteString(`<div style="font-size:24px;font-weight:600;color:#111827;margin-top:4px;font-variant-numeric:tabular-nums;">` + escape(amount) + `</div>`)
	b.WriteString(`</div>`)
	b.WriteString(`<p style="margin:0 0 16px;color:#4b5563;">The credit note PDF is attached for your records.</p>`)
	return subject, b.String()
}
