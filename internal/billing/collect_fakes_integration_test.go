package billing_test

import (
	"context"
	"errors"
	"time"

	"github.com/sagarsuperuser/velox/internal/billing"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/subscription"
	"github.com/sagarsuperuser/velox/internal/tax"
	"github.com/sagarsuperuser/velox/internal/tenant"
	"github.com/sagarsuperuser/velox/internal/usage"
)

// newRealEngine is the engine most integration tests use: the real Postgres
// stores for subscriptions, usage, pricing, invoices and tenant settings, no
// credit applier, and the defaults every such engine needs:
//   - the subscription store as interval reader (the only segment source);
//   - the no-tax resolver (the engine fails without one, as production always
//     wires one);
//   - the collect fakes below: no payment method on file, a charger that
//     fails if reached, a recording no-PM notifier, a no-op dunning starter.
//
// A test adds what its flow needs with the engine's setters. One that wraps a
// store or wants other fakes calls billing.NewEngine itself. A nil clk means
// the real clock.
func newRealEngine(db *postgres.DB, clk clock.Clock) *billing.Engine {
	subs := subscription.NewPostgresStore(db)
	e := billing.NewEngine(
		&subStoreAdapter{subs}, &usageStoreAdapter{usage.NewPostgresStore(db)},
		&pricingStoreAdapter{pricing.NewPostgresStore(db)}, &invoiceStoreAdapter{invoice.NewPostgresStore(db)},
		nil, tenant.NewSettingsStore(db), testPaymentSetupsNoPM{}, testChargerSentinel{}, clk,
	)
	e.SetIntervalReader(subs)
	e.SetTaxProviderResolver(tax.NewResolver(nil))
	e.SetNoPaymentMethodNotifier(&testNoPMNotifier{})
	e.SetDunningStarter(testDunningStarter{})
	return e
}

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
