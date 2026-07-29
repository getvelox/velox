package arch

import (
	"regexp"
	"strings"
	"testing"
)

// Emails enqueued from an HTTP handler must run on a ctx bound to the
// entity's clock, not the raw request ctx.
//
// This class has now cost two bugs. ADR-030 says "bind effective-now at
// every operator entry point on a clock-pinned entity", and services do
// — but emails are enqueued from HANDLERS, which sit OUTSIDE the
// service's bound scope. So an invoice's own state changes carried
// simulated stamps while the emails announcing them did not: four
// operator paths (finalize's setup link, the operator resend, "Email
// invoice", "Email credit note") enqueued unanchored. The ADR-104 walk
// caught it live — the anchor column shipped, and the first email sent
// through the UI still wrote NULL.
//
// Prose didn't hold the rule, so this does: a call passing r.Context()
// straight into an email-sending method is the exact shape of the bug.
// Bind first (h.svc.bindForInvoice(r.Context(), …)) and pass the bound
// ctx.
var emailSendCallRe = regexp.MustCompile(
	`\.(Send[A-Za-z]*|Notify[A-Za-z]*)\(\s*r\.Context\(\)`)

// emailBindingAllowlist exempts call sites whose email type reaches NO
// clock-scoped surface, each with its reason. An entry must be removed
// the moment its type becomes invoice- or customer-timeline-scoped.
var emailBindingAllowlist = map[string]string{
	// payment_setup_link is operator-initiated and customer-scoped: it
	// carries no invoice_number, so neither ListByInvoice nor
	// ListByCustomer (both filter to invoice-scoped types) can render it.
	// The only cost of leaving it unbound is that a clock-pinned
	// customer's setup-link row survives clock teardown unanchored —
	// invisible, since nothing lists it. Binding would mean wiring a
	// resolver through paymentmethods (which has none) for a row no
	// surface shows. REMOVE THIS ENTRY if payment_setup_link is ever
	// added to a timeline query.
	"internal/paymentmethods/handler.go": "payment_setup_link renders on no timeline; unbound row is invisible (see comment)",
}

func TestHandlerEmailEnqueuesBindTheEntityClock(t *testing.T) {
	for path, src := range sourceFiles(t) {
		if _, ok := emailBindingAllowlist[path]; ok {
			continue
		}
		if !strings.HasSuffix(path, "handler.go") {
			continue
		}
		for i, line := range strings.Split(src, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			if emailSendCallRe.MatchString(line) {
				t.Errorf("%s:%d passes the raw request ctx to an email/notify call — the "+
					"enqueued row gets no billing anchor and falls off the entity's calendar "+
					"(ADR-030 / ADR-104). Bind first: ctx := h.svc.bindForInvoice(r.Context(), "+
					"tenantID, inv.ID).\n    %s", path, i+1, trimmed)
			}
		}
	}
}
