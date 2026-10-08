package billing_test

import (
	"context"
	"time"

	"errors"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// Collect-pipeline collaborators for integration engines. REQUIRED post-#442
// (the charger/paymentSetups/noPMNotifier nil guards are deleted): every
// engine whose flow can reach the collector (CollectInvoice) wires these. The defaults
// steer flows into the no-PM arm — queue for the sweep + notify — which only
// writes auto_charge_pending on the test's own tenant.

// testPaymentSetupsNoPM reports "no payment method on file" for everyone.
type testPaymentSetupsNoPM struct{}

func (testPaymentSetupsNoPM) ResolveForCharge(_ context.Context, _, _ string) (string, string, error) {
	return "", "", nil
}

// testChargerSentinel errors loudly if a flow somehow reaches a charge —
// integration fixtures that want charging wire their own charger.
type testChargerSentinel struct{}

func (testChargerSentinel) ChargeInvoice(_ context.Context, _ string, inv domain.Invoice, _, _ string) (domain.Invoice, error) {
	return inv, errors.New("testChargerSentinel: flow reached ChargeInvoice without a real charger fake")
}

// testDunningResolver satisfies the required DunningResolver for engines
// whose flows can settle an invoice (credit-cover settles resolve dunning).
type testDunningResolver struct{ resolved []string }

func (d *testDunningResolver) ResolveByInvoice(_ context.Context, _, invoiceID string, _ domain.DunningResolution) error {
	d.resolved = append(d.resolved, invoiceID)
	return nil
}

// testNoPMNotifier records sends (queue+notify arm).
type testNoPMNotifier struct{ got []domain.Invoice }

func (n *testNoPMNotifier) NotifyNoPaymentMethod(_ context.Context, _ string, inv domain.Invoice, trigger string) (domain.NotifyOutcome, error) {
	n.got = append(n.got, inv)
	return domain.NotifySent, nil
}

// testDunningStarter is a no-op starter for integration engines whose flow can
// reach the collector's no-card arm (it starts no-payment dunning there).
// Tests that assert on dunning wire their own.
type testDunningStarter struct{}

func (testDunningStarter) StartDunning(context.Context, string, string, string, time.Time, domain.DunningStartCause) error {
	return nil
}
