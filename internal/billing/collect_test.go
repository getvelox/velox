package billing

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/payment"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
)

// erroringPaymentSetups returns a resolve ERROR — the "can't determine PM
// state" case, distinct from a clean "no PM on file".
type erroringPaymentSetups struct{ err error }

func (f *erroringPaymentSetups) ResolveForCharge(_ context.Context, _, _ string) (string, string, error) {
	return "", "", f.err
}

// collectFixture builds an engine around one finalized $50 invoice, queued for
// collection the way the store writes it at finalize. paymentSetups/charger
// default to wireBaseTax's no-PM/sentinel pair; tests override per arm.
func collectFixture() (*Engine, *mockInvoices, *fakeNoPMNotifier, domain.Invoice) {
	inv := domain.Invoice{
		ID: "inv_c1", TenantID: "t1", CustomerID: "cus_1",
		Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentPending,
		TaxFacts:      domain.TaxFacts{TaxStatus: domain.InvoiceTaxOK},
		SubtotalCents: 5000, TotalAmountCents: 5000, AmountDueCents: 5000,
		AutoChargePending: true,
	}
	invoices := &mockInvoices{invoices: []domain.Invoice{inv}}
	e := wireBaseTax(NewEngine(&mockSubs{}, &mockUsage{}, &mockPricing{}, invoices, nil, &mockSettings{}, nil, nil, billingTestClock()))
	notifier := e.noPMNotifier.(*fakeNoPMNotifier)
	return e, invoices, notifier, inv
}

func autoChargePending(t *testing.T, invoices *mockInvoices, id string) bool {
	t.Helper()
	for _, iv := range invoices.invoices {
		if iv.ID == id {
			return iv.AutoChargePending
		}
	}
	t.Fatalf("invoice %s not in mock", id)
	return false
}

// triggerNoPMNotifier records the trigger label of each setup-link email.
type triggerNoPMNotifier struct{ triggers []string }

func (n *triggerNoPMNotifier) NotifyNoPaymentMethod(_ context.Context, _ string, _ domain.Invoice, trigger string) (domain.NotifyOutcome, error) {
	n.triggers = append(n.triggers, trigger)
	return domain.NotifySent, nil
}

// anchorCreditApplier records the instant credits were applied at.
type anchorCreditApplier struct {
	inv *mockInvoices
	at  []time.Time
}

func (f *anchorCreditApplier) ApplyToInvoiceAt(_ context.Context, _, _, _ string, _ int64, at time.Time, _ ...string) (int64, error) {
	f.at = append(f.at, at)
	return 0, nil
}

// TestCollectInvoice pins the finalize-time nudge: it runs the sweep's own
// collector on one invoice, so every arm below is the sweep's behavior, and a
// failure leaves the invoice queued for the sweep rather than lost.
func TestCollectInvoice(t *testing.T) {
	ctx := context.Background()

	t.Run("PM ready → charged once, flag cleared, no email", func(t *testing.T) {
		e, invoices, notifier, inv := collectFixture()
		e.paymentSetups = &fakePaymentSetups{ready: true, stripeCustomerID: "cus_stripe"}
		charger := &recordingCharger{}
		e.charger = charger

		e.CollectInvoice(ctx, "t1", inv.ID, nil)
		if len(charger.got) != 1 {
			t.Fatalf("charges = %d, want 1", len(charger.got))
		}
		if autoChargePending(t, invoices, inv.ID) {
			t.Error("a successful charge must clear the queue flag")
		}
		if len(notifier.got) != 0 {
			t.Errorf("PM-ready path must not email, got %d", len(notifier.got))
		}
	})

	t.Run("no PM → setup-link email labelled finalize_no_pm, stays queued", func(t *testing.T) {
		e, invoices, _, inv := collectFixture()
		n := &triggerNoPMNotifier{}
		e.noPMNotifier = n

		e.CollectInvoice(ctx, "t1", inv.ID, nil)
		if len(n.triggers) != 1 || n.triggers[0] != noPMTriggerFinalize {
			t.Fatalf("triggers = %v, want [%s]", n.triggers, noPMTriggerFinalize)
		}
		if !autoChargePending(t, invoices, inv.ID) {
			t.Error("a card-less invoice must stay queued so attaching a card collects it")
		}
		got, _ := invoices.GetInvoice(ctx, "t1", inv.ID)
		if got.NoPMNotifiedAt == nil {
			t.Error("send-once marker must be stamped")
		}
	})

	t.Run("then the sweep stays silent: one email across nudge + sweep", func(t *testing.T) {
		e, invoices, _, inv := collectFixture()
		n := &triggerNoPMNotifier{}
		e.noPMNotifier = n

		e.CollectInvoice(ctx, "t1", inv.ID, nil)
		// The sweep lists the same row; hand it the PRE-email snapshot to
		// prove the collector re-reads after claiming instead of trusting it.
		if _, errs := e.processAutoCharge(ctx, []domain.Invoice{inv}, nil, noPMTriggerSweep); len(errs) != 0 {
			t.Fatalf("sweep errs: %v", errs)
		}
		if len(n.triggers) != 1 {
			t.Errorf("emails = %d (%v), want exactly 1", len(n.triggers), n.triggers)
		}
		_ = invoices
	})

	t.Run("PM resolve ERROR → no email (unknown ≠ missing), stays queued", func(t *testing.T) {
		e, invoices, notifier, inv := collectFixture()
		e.paymentSetups = &erroringPaymentSetups{err: errors.New("db blip")}

		e.CollectInvoice(ctx, "t1", inv.ID, nil)
		if len(notifier.got) != 0 {
			t.Errorf("resolver error must NOT email the customer, got %d", len(notifier.got))
		}
		if !autoChargePending(t, invoices, inv.ID) {
			t.Error("must stay queued for the sweep")
		}
	})

	t.Run("reload fails → nothing charged, stays queued for the sweep", func(t *testing.T) {
		e, invoices, _, inv := collectFixture()
		e.paymentSetups = &fakePaymentSetups{ready: true, stripeCustomerID: "cus_stripe"}
		charger := &recordingCharger{}
		e.charger = charger
		invoices.getErr = errors.New("reload blip")

		e.CollectInvoice(ctx, "t1", inv.ID, nil)
		invoices.getErr = nil
		if len(charger.got) != 0 {
			t.Errorf("no charge on a failed reload, got %d", len(charger.got))
		}
		if !autoChargePending(t, invoices, inv.ID) {
			t.Error("the queue flag is the recovery; it must survive")
		}
	})

	t.Run("decline → flag cleared: dunning is the one retry owner", func(t *testing.T) {
		e, invoices, _, inv := collectFixture()
		e.paymentSetups = &fakePaymentSetups{ready: true, stripeCustomerID: "cus_stripe"}
		e.charger = &fakeChargerDecline{}

		e.CollectInvoice(ctx, "t1", inv.ID, nil)
		if autoChargePending(t, invoices, inv.ID) {
			t.Error("a decline hands the invoice to dunning; the sweep must not also retry it")
		}
	})

	t.Run("draft → not collected", func(t *testing.T) {
		e, invoices, notifier, inv := collectFixture()
		invoices.invoices[0].Status = domain.InvoiceDraft
		invoices.invoices[0].TaxStatus = domain.InvoiceTaxPending
		charger := &recordingCharger{}
		e.charger = charger
		e.paymentSetups = &fakePaymentSetups{ready: true, stripeCustomerID: "cus_stripe"}
		applier := &fakeCreditApplier{inv: invoices, applyCents: 5000}
		e.credits = applier

		e.CollectInvoice(ctx, "t1", inv.ID, nil)
		if len(charger.got) != 0 || len(notifier.got) != 0 || applier.calls != 0 {
			t.Errorf("a draft must not be charged, emailed, or have credits applied (charges=%d emails=%d credit applies=%d)",
				len(charger.got), len(notifier.got), applier.calls)
		}
	})

	t.Run("survives caller cancellation; charge keeps its deadline", func(t *testing.T) {
		e, _, _, inv := collectFixture()
		e.paymentSetups = &fakePaymentSetups{ready: true, stripeCustomerID: "cus_stripe"}
		charger := &ctxProbeCharger{}
		e.charger = charger

		callerCtx, cancel := context.WithCancel(context.Background())
		cancel() // the HTTP client is already gone

		e.CollectInvoice(callerCtx, "t1", inv.ID, nil)
		if charger.ctxErrAtCall != nil {
			t.Errorf("charge ctx must be detached from the caller's cancellation, got err=%v", charger.ctxErrAtCall)
		}
		if !charger.hadDeadline {
			t.Error("charge ctx must still carry the 30s deadline after the detach")
		}
	})

	t.Run("credits apply at the caller's anchor (cycle boundary), not now", func(t *testing.T) {
		e, invoices, _, inv := collectFixture()
		applier := &anchorCreditApplier{inv: invoices}
		e.credits = applier
		boundary := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)

		e.CollectInvoice(ctx, "t1", inv.ID, &boundary)
		if len(applier.at) != 1 || !applier.at[0].Equal(boundary) {
			t.Errorf("credit apply at = %v, want the period boundary %v", applier.at, boundary)
		}
	})
}

// TestCollect_Credits pins ADR-088 on the one collector: credits are consumed
// before any card charge, a fully covered invoice settles paid with no charge,
// and an apply FAILURE never charges the pre-credit amount (trap R1).
func TestCollect_Credits(t *testing.T) {
	ctx := context.Background()

	t.Run("partial credit → card charged exactly the remainder", func(t *testing.T) {
		e, invoices, _, inv := collectFixture()
		e.paymentSetups = &fakePaymentSetups{ready: true, stripeCustomerID: "cus_stripe"}
		charger := &recordingCharger{}
		e.charger = charger
		e.credits = &fakeCreditApplier{inv: invoices, applyCents: 2000}

		e.CollectInvoice(ctx, "t1", inv.ID, nil)
		if len(charger.got) != 1 {
			t.Fatalf("charges = %d, want 1", len(charger.got))
		}
		if charger.got[0].AmountDueCents != 3000 {
			t.Errorf("charged = %d, want post-credit remainder 3000", charger.got[0].AmountDueCents)
		}
	})

	t.Run("full credit → settled paid, no charge, dunning resolved", func(t *testing.T) {
		e, invoices, notifier, inv := collectFixture()
		e.paymentSetups = &fakePaymentSetups{ready: true, stripeCustomerID: "cus_stripe"}
		charger := &recordingCharger{}
		e.charger = charger
		e.credits = &fakeCreditApplier{inv: invoices, applyCents: 5000}
		resolver := &recordingDunningResolver{}
		e.SetDunningResolver(resolver)

		e.CollectInvoice(ctx, "t1", inv.ID, nil)
		if len(charger.got) != 0 {
			t.Errorf("fully covered → no charge, got %d", len(charger.got))
		}
		got, _ := invoices.GetInvoice(ctx, "t1", inv.ID)
		if got.Status != domain.InvoicePaid || got.AutoChargePending {
			t.Errorf("invoice = %s pending=%v, want paid and dequeued", got.Status, got.AutoChargePending)
		}
		if len(resolver.resolved) != 1 {
			t.Errorf("credit settle must resolve dunning, got %d", len(resolver.resolved))
		}
		if len(notifier.got) != 0 {
			t.Errorf("no email on a settled invoice, got %d", len(notifier.got))
		}
	})

	t.Run("apply FAILS → card NEVER charged pre-credit, stays queued (R1)", func(t *testing.T) {
		e, invoices, _, inv := collectFixture()
		e.paymentSetups = &fakePaymentSetups{ready: true, stripeCustomerID: "cus_stripe"}
		charger := &recordingCharger{}
		e.charger = charger
		e.credits = &fakeCreditApplier{inv: invoices, err: errors.New("ledger blip")}

		e.CollectInvoice(ctx, "t1", inv.ID, nil)
		if len(charger.got) != 0 {
			t.Fatalf("apply failure must NEVER charge the pre-credit amount, got %d charges", len(charger.got))
		}
		if !autoChargePending(t, invoices, inv.ID) {
			t.Error("apply failure must leave the invoice queued (the sweep re-applies)")
		}
	})
}

// TestCollect_ZeroDueSettles pins G4: an invoice owing nothing — born $0, or
// brought to $0 by a credit note — is settled paid by the collector with no
// charge. Pre-fix every collection path required amount_due > 0, so these sat
// finalized and "awaiting payment" forever.
func TestCollect_ZeroDueSettles(t *testing.T) {
	ctx := context.Background()
	e, invoices, notifier, inv := collectFixture()
	invoices.invoices[0].AmountDueCents = 0 // e.g. a credit note covered it
	e.SetDunningResolver(&recordingDunningResolver{})
	e.paymentSetups = &fakePaymentSetups{ready: true, stripeCustomerID: "cus_stripe"}
	charger := &recordingCharger{}
	e.charger = charger

	if n, errs := e.processAutoCharge(ctx, []domain.Invoice{inv}, nil, noPMTriggerSweep); n != 1 || len(errs) != 0 {
		t.Fatalf("settled=%d errs=%v, want 1 settle", n, errs)
	}
	got, _ := invoices.GetInvoice(ctx, "t1", inv.ID)
	if got.Status != domain.InvoicePaid {
		t.Errorf("status = %s, want paid", got.Status)
	}
	if len(charger.got) != 0 || len(notifier.got) != 0 {
		t.Errorf("a $0 invoice is neither charged nor emailed (charges=%d emails=%d)", len(charger.got), len(notifier.got))
	}
}

// ctxProbeCharger records ctx liveness + deadline at charge time.
type ctxProbeCharger struct {
	ctxErrAtCall error
	hadDeadline  bool
}

func (c *ctxProbeCharger) ChargeInvoice(ctx context.Context, _ string, inv domain.Invoice, _, _ string) (domain.Invoice, error) {
	c.ctxErrAtCall = ctx.Err()
	_, c.hadDeadline = ctx.Deadline()
	return inv, nil
}

// TestSweepNoPM_SetupEmailSentExactlyOnce pins the sweep-side no-PM email
// (ADR-087 follow-up): a card-less queued invoice gets the setup-link email
// exactly ONCE across ticks, gated by the durable no_pm_notified_at stamp. A
// resolve ERROR sends nothing (unknown ≠ missing), and a skipped-no-email
// outcome stays unstamped so it self-heals when the customer gains an address.
func TestSweepNoPM_SetupEmailSentExactlyOnce(t *testing.T) {
	ctx := context.Background()

	t.Run("no PM, never emailed → one email, stamped; second tick silent", func(t *testing.T) {
		e, invoices, notifier, inv := collectFixture()
		if n, errs := e.processAutoCharge(ctx, []domain.Invoice{inv}, nil, noPMTriggerSweep); n != 0 || len(errs) != 0 {
			t.Fatalf("tick 1: charged=%d errs=%v", n, errs)
		}
		if len(notifier.got) != 1 {
			t.Fatalf("tick 1 notifies = %d, want 1", len(notifier.got))
		}
		stamped, _ := invoices.GetInvoice(ctx, "t1", inv.ID)
		if stamped.NoPMNotifiedAt == nil {
			t.Fatal("send-once marker must be stamped after the email")
		}
		if _, errs := e.processAutoCharge(ctx, []domain.Invoice{stamped}, nil, noPMTriggerSweep); len(errs) != 0 {
			t.Fatalf("tick 2 errs: %v", errs)
		}
		if len(notifier.got) != 1 {
			t.Errorf("tick 2 must NOT re-email, total notifies = %d", len(notifier.got))
		}
	})

	t.Run("customer has no email → unstamped, retried next tick (self-heal)", func(t *testing.T) {
		e, invoices, _, inv := collectFixture()
		skipper := &skippingNoPMNotifier{}
		e.noPMNotifier = skipper
		if _, errs := e.processAutoCharge(ctx, []domain.Invoice{inv}, nil, noPMTriggerSweep); len(errs) != 0 {
			t.Fatalf("errs: %v", errs)
		}
		got, _ := invoices.GetInvoice(ctx, "t1", inv.ID)
		if got.NoPMNotifiedAt != nil {
			t.Fatal("skipped-no-email must NOT stamp — it must retry when an address appears")
		}
		if _, errs := e.processAutoCharge(ctx, []domain.Invoice{got}, nil, noPMTriggerSweep); len(errs) != 0 {
			t.Fatalf("tick 2 errs: %v", errs)
		}
		if skipper.calls != 2 {
			t.Errorf("tick 2 must re-attempt (self-heal), attempts = %d", skipper.calls)
		}
	})
}

// skippingNoPMNotifier always reports the customer has no email on file.
type skippingNoPMNotifier struct{ calls int }

func (n *skippingNoPMNotifier) NotifyNoPaymentMethod(_ context.Context, _ string, _ domain.Invoice, trigger string) (domain.NotifyOutcome, error) {
	n.calls++
	return domain.NotifySkippedNoEmail, nil
}

// A catch-up cycle close (several periods closed in one run, as a test-clock
// Advance or a late tick does) collects each invoice at ITS period boundary,
// not at the run's now. Credits are applied as of the boundary — a grant that
// expired after it still covers that period — and a credit-covered invoice's
// paid_at lands there too. Applying at now would skip those grants and charge
// the card for what they covered.
func TestCycleClose_CollectsAtThePeriodBoundary(t *testing.T) {
	periodStart := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	nextBilling := periodEnd
	subs := &mockSubs{
		subs: map[string]domain.Subscription{
			"sub_1": {
				ID: "sub_1", TenantID: "t1", CustomerID: "cus_1",
				Items:                     []domain.SubscriptionItem{{PlanID: "pln_1", Quantity: 1}},
				Status:                    domain.SubscriptionActive,
				BillingTime:               domain.BillingTimeCalendar,
				CurrentBillingPeriodStart: &periodStart, CurrentBillingPeriodEnd: &periodEnd,
				NextBillingAt: &nextBilling,
			},
		},
		cycleUpdated: make(map[string]bool),
	}
	pricing := &mockPricing{plans: map[string]domain.Plan{
		"pln_1": {ID: "pln_1", Currency: "USD", BillingInterval: domain.BillingMonthly, BaseAmountCents: 1000},
	}}
	invoices := &mockInvoices{}
	applier := &anchorCreditApplier{inv: invoices}
	runAt := time.Date(2026, 7, 1, 0, 0, 1, 0, time.UTC) // three periods due: May 1, Jun 1, Jul 1
	engine := wireBaseTax(NewEngine(subs, &mockUsage{totals: map[string]int64{}}, pricing, invoices, applier, &mockSettings{}, nil, nil, clock.NewFake(runAt)))

	if _, errs := engine.RunCycle(context.Background(), 50); len(errs) > 0 {
		t.Fatalf("RunCycle: %v", errs)
	}
	want := []time.Time{periodEnd, periodEnd.AddDate(0, 1, 0), periodEnd.AddDate(0, 2, 0)}
	if len(applier.at) != len(want) {
		t.Fatalf("credit applies = %d (%v), want %d", len(applier.at), applier.at, len(want))
	}
	for i, at := range applier.at {
		if !at.Equal(want[i]) {
			t.Errorf("apply %d at %v, want the period boundary %v (not the run's now %v)", i, at, want[i], runAt)
		}
	}
}

// refusingCharger fails definitely with no decline code (a Stripe
// invalid_request, or a card_error that carries only a code).
type refusingCharger struct{}

func (refusingCharger) ChargeInvoice(_ context.Context, _ string, inv domain.Invoice, _, _ string) (domain.Invoice, error) {
	return inv, &payment.PaymentError{Message: "No such PaymentMethod"}
}

// Any DEFINITE failure hands the invoice to dunning, not only a decline with a
// decline_code: the charger has already marked it failed and started dunning,
// so the queue flag must not stay set beside it.
func TestCollect_DefiniteFailureWithoutDeclineCode_ClearsFlag(t *testing.T) {
	e, invoices, _, inv := collectFixture()
	e.paymentSetups = &fakePaymentSetups{ready: true, stripeCustomerID: "cus_stripe"}
	e.charger = refusingCharger{}

	_, errs := e.processAutoCharge(context.Background(), []domain.Invoice{inv}, nil, noPMTriggerSweep)
	if len(errs) != 1 {
		t.Errorf("an unusual refusal is surfaced to the caller, got errs=%v", errs)
	}
	if autoChargePending(t, invoices, inv.ID) {
		t.Error("a definite failure must clear the queue flag: dunning owns the retries")
	}
}
