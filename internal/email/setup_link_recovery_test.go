package email

import (
	"strings"
	"testing"
)

// TestSetupLinkServesRecovery_ExemptsOnlyWrittenOff guards the staleness
// gate's second exemption.
//
// The blanket gate reads any terminal invoice as "settled, so stop asking",
// which is right for a payment demand and wrong for a payment-METHOD link on
// a written-off invoice: attaching a card is the on-ramp to bad-debt recovery,
// not payment of the invoice.
//
// This was found the expensive way. The endpoint was widened to admit
// uncollectible first, and the request then answered 200, wrote its outbox
// row, and had it silently dropped as obsolete — a no-op behind a success
// response, strictly worse than the 409 it replaced. The negative rows are
// what keep the exemption narrow: paid and voided must STILL mute, because
// there no recovery is possible and the ask really is dead.
func TestSetupLinkServesRecovery_ExemptsOnlyWrittenOff(t *testing.T) {
	for _, tc := range []struct {
		emailType, state string
		want             bool
		why              string
	}{
		{TypePaymentSetupRequest, "uncollectible", true,
			"the card is the on-ramp to recovery on normal rails (fresh recovery invoice, ADR-113) and future billing"},
		{TypePaymentSetupRequest, "paid", false,
			"asking for a card after the invoice is paid is the exact trust-eroding mail the gate exists to stop"},
		{TypePaymentSetupRequest, "voided", false,
			"a voided invoice is annulled — there is nothing to recover"},
		{TypePaymentSetupRequest, "", false,
			"a live invoice never reaches the exemption; the gate does not fire at all"},
		{TypePaymentFailed, "uncollectible", false,
			"only the setup link serves recovery — a payment-failed notice on a written-off invoice is still stale"},
		{TypeDunningWarning, "uncollectible", false,
			"a 'we'll retry on <date>' warning is false once the invoice is written off"},
	} {
		if got := setupLinkServesRecovery(tc.emailType, tc.state); got != tc.want {
			t.Errorf("setupLinkServesRecovery(%q, %q) = %v, want %v — %s",
				tc.emailType, tc.state, got, tc.want, tc.why)
		}
	}
}

// TestSetupLinkCopy_DoesNotPromiseAutoCollectOnWriteOff is the honesty half.
//
// NOTHING collects a written-off invoice — ADR-113 pins every charge claim
// to `finalized`. The
// stock copy promises "we'll collect it automatically", which would be a
// commitment the engine cannot keep, sent to the customer, triggered by an
// operator button.
//
// The control is the point: the SAME template on a live invoice must still
// make the automatic promise, because there it is true.
func TestSetupLinkCopy_DoesNotPromiseAutoCollectOnWriteOff(t *testing.T) {
	const autoPromise = "we'll collect it automatically"
	const humanPromise = "we'll be in touch to settle it"

	_, writtenOff, _, _ := renderPaymentSetupLinkHTML(paymentSetupLinkContext{
		CustomerName: "Ada", SetupURL: "https://example.test/s",
		InvoiceNumber: "VLX-000003", AmountDueLabel: "$60.00", WrittenOff: true,
	})
	if strings.Contains(writtenOff, autoPromise) {
		t.Errorf("written-off setup link promises automatic collection — no machine charges a written-off invoice (ADR-110), so this is a promise the engine cannot keep")
	}
	if !strings.Contains(writtenOff, humanPromise) {
		t.Errorf("written-off setup link should say a person will follow up; got:\n%s", writtenOff)
	}

	_, live, _, _ := renderPaymentSetupLinkHTML(paymentSetupLinkContext{
		CustomerName: "Ada", SetupURL: "https://example.test/s",
		InvoiceNumber: "VLX-000006", AmountDueLabel: "$10.00", WrittenOff: false,
	})
	if !strings.Contains(live, autoPromise) {
		t.Errorf("CONTROL: a live invoice must still promise automatic collection — the auto-charge sweep really does collect it once a card is attached; got:\n%s", live)
	}
}
