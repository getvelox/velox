package domain

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestPaymentBlocksAction_Matrix makes the whole rule visible in one place, so a
// change to any cell is a deliberate edit rather than a side effect discovered
// later on a customer's invoice.
//
// The distinction the matrix exists to protect: PARKED and IN-FLIGHT look alike
// (both are IsInFlight) but call for opposite advice. One resolves itself and
// waiting is correct; the other never will, and telling someone to wait is the
// bug that took a four-agent sweep to find.
func TestPaymentBlocksAction_Matrix(t *testing.T) {
	settled := Invoice{PaymentStatus: PaymentPending}
	parked := Invoice{PaymentStatus: PaymentUnknown, StripePaymentIntentID: ""}
	resolving := Invoice{PaymentStatus: PaymentProcessing, StripePaymentIntentID: "pi_1"}
	unknownWithPI := Invoice{PaymentStatus: PaymentUnknown, StripePaymentIntentID: "pi_1"}

	every := []InvoiceAction{
		ActionVoid, ActionMarkUncollectible, ActionRecordOfflinePayment,
		ActionIssueCreditNote, ActionCollectPayment, ActionResendSetupLink, ActionEmailInvoice,
	}

	t.Run("a settled payment blocks nothing", func(t *testing.T) {
		for _, a := range every {
			if got := PaymentBlocksAction(settled, a); got.Blocked {
				t.Errorf("%s blocked on a not-in-flight invoice: %s", a, got.Message)
			}
		}
	})

	t.Run("parked allows ONLY mark-uncollectible", func(t *testing.T) {
		for _, a := range every {
			got := PaymentBlocksAction(parked, a)
			if a == ActionMarkUncollectible {
				if got.Blocked {
					t.Fatalf("mark-uncollectible is the ONLY exit from a parked invoice; blocking it leaves the invoice with no terminal state at all: %s", got.Message)
				}
				continue
			}
			if !got.Blocked {
				t.Errorf("%s allowed on a parked invoice — its charge may have succeeded", a)
			}
			if got.Code != "payment_unidentifiable" {
				t.Errorf("%s code = %q, want payment_unidentifiable", a, got.Code)
			}
		}
	})

	t.Run("parked refusals always name the way out", func(t *testing.T) {
		// A refusal that names no alternative is how an operator ends up stuck
		// with an invoice and no next step — which is exactly what shipped.
		for _, a := range every {
			if a == ActionMarkUncollectible {
				continue
			}
			msg := PaymentBlocksAction(parked, a).Message
			if !strings.Contains(msg, "uncollectible") {
				t.Errorf("%s refusal names no way out: %q", a, msg)
			}
			if strings.Contains(msg, "wait for") || strings.Contains(msg, "cancel it") {
				t.Errorf("%s refusal tells the operator to wait or cancel — neither is possible for a parked invoice: %q", a, msg)
			}
		}
	})

	t.Run("a genuinely resolving charge says wait, not write off", func(t *testing.T) {
		for _, inv := range []Invoice{resolving, unknownWithPI} {
			for _, a := range every {
				got := PaymentBlocksAction(inv, a)
				if !got.Blocked {
					t.Errorf("%s allowed while a charge is being confirmed", a)
					continue
				}
				if got.Code != "payment_in_flight" {
					t.Errorf("%s code = %q, want payment_in_flight", a, got.Code)
				}
				if strings.Contains(got.Message, "uncollectible") {
					t.Errorf("%s tells the operator to write off a charge that is still resolving: %q", a, got.Message)
				}
			}
		}
	})
}

// TestNoHandWrittenPaymentInFlightRefusals is the mechanised half, and the
// reason this refactor is worth more than better wording.
//
// This rule was previously re-expressed by hand in eight places. When
// payment_status='unknown' changed meaning, all eight went wrong independently
// and two of them had already drifted apart from each other ("wait for it to
// settle or cancel it" vs "settle or cancel first, or wait for charge
// reconciliation" — same rule, two authors, both naming actions the product
// does not offer).
//
// Nothing stops that recurring except a check. Any new hand-written refusal
// that talks about a charge being in flight fails here and gets pointed at
// PaymentBlocksAction.
func TestNoHandWrittenPaymentInFlightRefusals(t *testing.T) {
	// Phrases that only ever appear in a payment-in-flight refusal.
	banned := regexp.MustCompile(`(?i)(charge is (already )?in flight|payment is in flight|wait for it to settle|charge reconciliation)`)

	root := ".."
	var offenders []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		// The single source is allowed to say it; that is its job.
		if strings.HasSuffix(path, "invoice_payment_gate.go") {
			return nil
		}
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		// Scope the scan to REFUSAL constructs. A banner that DESCRIBES an
		// in-flight payment ("Velox confirms it automatically") is legitimate
		// and true for a resolving charge — it is only the hand-written
		// refusals that must derive from the single source. The constructor and
		// its message often sit on different lines, so look ahead a little.
		refusal := regexp.MustCompile(`(errs\.InvalidState\(|respond\.Validation\(|respond\.Error\()`)
		lines := strings.Split(string(src), "\n")
		for i, line := range lines {
			if !refusal.MatchString(line) {
				continue
			}
			window := strings.Join(lines[i:min(i+3, len(lines))], " ")
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			if banned.MatchString(window) {
				offenders = append(offenders, path+":"+itoa(i+1)+"  "+strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(offenders) > 0 {
		t.Errorf("hand-written payment-in-flight refusal(s) — derive them from domain.PaymentBlocksAction instead, "+
			"so the next change to what a payment state MEANS cannot leave stale copies behind:\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
