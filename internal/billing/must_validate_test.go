package billing

import (
	"strings"
	"testing"

	"github.com/sagarsuperuser/velox/internal/tax"
)

// TestMustValidate_NamesEveryNilCollaborator pins the boot-fail-closed
// contract (2026-07-10 design review, redesign #3 stage 1): a partially
// wired engine panics at MustValidate with EVERY missing collaborator named
// — not just the first — so one boot failure yields one complete fix.
func TestMustValidate_NamesEveryNilCollaborator(t *testing.T) {
	// Construct WITHOUT wireBaseTax: that helper now defaults the collect
	// collaborators (paymentSetups/charger/noPMNotifier), and this test needs
	// them nil to assert they are named.
	e := NewEngine(&mockSubs{}, &mockUsage{}, &mockPricing{}, &mockInvoices{}, nil, &mockSettings{}, nil, nil, billingTestClock())
	e.SetTaxProviderResolver(tax.NewResolver(nil))
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected MustValidate to panic on a partially wired engine")
		}
		msg, _ := r.(string)
		// The deliberately-unwired deps must ALL be named.
		for _, want := range []string{"credits", "paymentSetups", "charger", "events", "auditLogger", "creditGranter"} {
			if !strings.Contains(msg, want) {
				t.Errorf("panic must name nil collaborator %q, got: %s", want, msg)
			}
		}
		// Wired deps must NOT be named.
		for _, wired := range []string{"subs", "usage", "pricing", "invoices", "settings", "taxProviders"} {
			if strings.Contains(msg, wired+",") || strings.HasSuffix(msg, wired) {
				t.Errorf("panic must not name wired collaborator %q, got: %s", wired, msg)
			}
		}
	}()
	e.MustValidate()
}

// TestMustValidate_CoversEveryCollaborator is the drift guard: every
// interface-typed field on Engine participates in validation, so adding a
// 26th collaborator cannot silently escape the boot check. It asserts the
// interface-field count matches the panic's nil list on a zero engine
// (everything nil except what NewEngine defaults).
func TestMustValidate_CoversEveryCollaborator(t *testing.T) {
	e := &Engine{} // nothing wired at all — even clock
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on a zero engine")
		}
		msg, _ := r.(string)
		nilCount := strings.Count(msg, ",") + 1
		// Count Engine's interface fields via the same mechanism a
		// maintainer would: keep this number in lockstep with the struct.
		// If this fails, a collaborator was added/removed — update BOTH
		// the wiring in router.go and this count deliberately.
		// 25th: ScheduledCancelExecutor (ADR-097 mid-period cancel fire).
		// 26th: IntervalSnapshotter (ADR-101 segment-source seam — the
		// producer always wires the real subscription store, even in
		// reader mode "off", so the kill switch can never dangle unwired).
		// Back to 25 at ADR-115: TxRunner is gone — the subscription store
		// owns the period closer's tx (SubscriptionReader.WithTenantTx).
		const collaborators = 25
		if nilCount != collaborators {
			t.Errorf("zero engine names %d nil collaborators, expected %d — Engine's dep set changed; update router wiring + this count deliberately", nilCount, collaborators)
		}
	}()
	e.MustValidate()
}
