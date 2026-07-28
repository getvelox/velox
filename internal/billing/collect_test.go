package billing

import (
	"context"
	"errors"
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// erroringPaymentSetups returns a resolve ERROR — the "can't determine PM
// state" case, distinct from a clean "no PM on file".
type erroringPaymentSetups struct{ err error }

func (f *erroringPaymentSetups) ResolveForCharge(_ context.Context, _, _ string) (string, string, error) {
	return "", "", f.err
}

// collectFixture builds an engine around one finalized $50 invoice and
// returns the pieces the pipeline touches. paymentSetups/charger default to
// wireBaseTax's no-PM/sentinel pair; tests override per arm.
func collectFixture() (*Engine, *mockInvoices, *fakeNoPMNotifier, domain.Subscription, domain.Invoice) {
	inv := domain.Invoice{
		ID: "inv_c1", TenantID: "t1", CustomerID: "cus_1",
		Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentPending,
		TaxFacts:      domain.TaxFacts{TaxStatus: domain.InvoiceTaxOK},
		SubtotalCents: 5000, TotalAmountCents: 5000, AmountDueCents: 5000,
	}
	invoices := &mockInvoices{invoices: []domain.Invoice{inv}}
	e := wireBaseTax(NewEngine(&mockSubs{}, &mockUsage{}, &mockPricing{}, invoices, nil, &mockSettings{}, nil, nil, billingTestClock()))
	notifier := e.noPMNotifier.(*fakeNoPMNotifier)
	sub := domain.Subscription{ID: "sub_1", TenantID: "t1", CustomerID: "cus_1"}
	return e, invoices, notifier, sub, inv
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

// TestCollectAfterFinalize pins the shared post-finalize collection pipeline
// (2026-07-11 extraction of the four hand-copied site blocks) — in particular
// the three error-path behaviors that were silent per-site drift before:
// resolver-error ≠ no-PM (no false "payment method needed" email), reload
// failure queues instead of vanishing, and every downgrade lands on
// auto_charge_pending=true so the sweep re-drives it.
func TestCollectAfterFinalize(t *testing.T) {
	ctx := context.Background()

	t.Run("no PM on file → queued for sweep + setup-link email", func(t *testing.T) {
		e, invoices, notifier, sub, inv := collectFixture()

		e.collectAfterFinalize(ctx, sub, inv, "test")
		if !autoChargePending(t, invoices, inv.ID) {
			t.Error("no-PM arm must set auto_charge_pending=true")
		}
		if len(notifier.got) != 1 {
			t.Fatalf("notifier calls = %d, want 1", len(notifier.got))
		}
		if notifier.got[0].ID != inv.ID {
			t.Errorf("notified invoice = %s, want %s", notifier.got[0].ID, inv.ID)
		}
	})

	t.Run("resolver ERROR → queued, NO email (unknown ≠ missing)", func(t *testing.T) {
		// Pre-extraction all four sites conflated a transient resolve error
		// with "no payment method" and emailed a card-having customer a
		// setup link (design-census D10). The pipeline queues for the sweep
		// (which re-resolves each tick) and stays quiet.
		e, invoices, notifier, sub, inv := collectFixture()
		e.paymentSetups = &erroringPaymentSetups{err: errors.New("db blip")}

		e.collectAfterFinalize(ctx, sub, inv, "test")
		if !autoChargePending(t, invoices, inv.ID) {
			t.Error("resolver-error arm must set auto_charge_pending=true")
		}
		if len(notifier.got) != 0 {
			t.Errorf("resolver error must NOT email the customer, got %d notifies", len(notifier.got))
		}
	})

	t.Run("PM ready + reload fails → queued, no charge (was a silent vanish)", func(t *testing.T) {
		// Pre-extraction: no charge, no flag, no log — the invoice never
		// entered the retry path (design-census D7).
		e, invoices, notifier, sub, inv := collectFixture()
		e.paymentSetups = &fakePaymentSetups{ready: true, stripeCustomerID: "cus_stripe"}
		charger := &recordingCharger{}
		e.charger = charger
		invoices.getErr = errors.New("reload blip")

		e.collectAfterFinalize(ctx, sub, inv, "test")
		invoices.getErr = nil // let the flag assertion read the mock
		if len(charger.got) != 0 {
			t.Errorf("charge must not fire on a failed reload, got %d charges", len(charger.got))
		}
		if !autoChargePending(t, invoices, inv.ID) {
			t.Error("reload failure must queue for the sweep, not vanish")
		}
		if len(notifier.got) != 0 {
			t.Errorf("reload failure is not a no-PM state; got %d notifies", len(notifier.got))
		}
	})

	t.Run("PM ready → charges the RELOADED amount (post-credit truth)", func(t *testing.T) {
		// The caller's inv snapshot can carry a stale pre-credit amount_due;
		// the charge must see the store's current value.
		e, invoices, notifier, sub, inv := collectFixture()
		e.paymentSetups = &fakePaymentSetups{ready: true, stripeCustomerID: "cus_stripe"}
		charger := &recordingCharger{}
		e.charger = charger
		invoices.invoices[0].AmountDueCents = 1200 // credits shrank it after the snapshot

		e.collectAfterFinalize(ctx, sub, inv, "test") // inv still says 5000
		if len(charger.got) != 1 {
			t.Fatalf("charges = %d, want 1", len(charger.got))
		}
		if charger.got[0].AmountDueCents != 1200 {
			t.Errorf("charged amount_due = %d, want reloaded 1200 (never the stale snapshot 5000)", charger.got[0].AmountDueCents)
		}
		if autoChargePending(t, invoices, inv.ID) {
			t.Error("successful charge must not queue a retry")
		}
		if len(notifier.got) != 0 {
			t.Errorf("PM-ready path must not email, got %d", len(notifier.got))
		}
	})

	t.Run("PM ready + amount_due drained to 0 since snapshot → no charge, no flag", func(t *testing.T) {
		e, invoices, _, sub, inv := collectFixture()
		e.paymentSetups = &fakePaymentSetups{ready: true, stripeCustomerID: "cus_stripe"}
		charger := &recordingCharger{}
		e.charger = charger
		invoices.invoices[0].AmountDueCents = 0

		e.collectAfterFinalize(ctx, sub, inv, "test")
		if len(charger.got) != 0 {
			t.Errorf("nothing due → no charge, got %d", len(charger.got))
		}
		if autoChargePending(t, invoices, inv.ID) {
			t.Error("nothing due → no retry flag")
		}
	})

	t.Run("PM ready + decline → queued for sweep", func(t *testing.T) {
		e, invoices, _, sub, inv := collectFixture()
		e.paymentSetups = &fakePaymentSetups{ready: true, stripeCustomerID: "cus_stripe"}
		e.charger = &fakeChargerDecline{}

		e.collectAfterFinalize(ctx, sub, inv, "test")
		if !autoChargePending(t, invoices, inv.ID) {
			t.Error("decline must queue for the sweep (dunning starts inline in the charger)")
		}
	})

	t.Run("no PM + notify reload fails → still queued, notifier skipped, no panic", func(t *testing.T) {
		e, invoices, notifier, sub, inv := collectFixture()
		invoices.getErr = errors.New("reload blip")

		e.collectAfterFinalize(ctx, sub, inv, "test")
		invoices.getErr = nil
		if !autoChargePending(t, invoices, inv.ID) {
			t.Error("flag must be set before the notify reload")
		}
		if len(notifier.got) != 0 {
			t.Errorf("failed reload must skip the notifier, got %d", len(notifier.got))
		}
	})
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

// TestCollectAfterFinalize_SurvivesCallerCancellation pins the pipeline's
// ctx-detach: two of its callers (subscription_create day-1, final-on-cancel)
// arrive on HTTP request ctxs, where a client disconnect mid-charge would
// otherwise abort the Stripe call at its most ambiguous moment and kill the
// charger's 'unknown' outcome-persist plus the retry-flag write in the same
// stroke. Once finalize has happened, collection runs to completion (bounded
// by the 30s charge deadline), regardless of the caller's fate.
func TestCollectAfterFinalize_SurvivesCallerCancellation(t *testing.T) {
	e, invoices, _, sub, inv := collectFixture()
	e.paymentSetups = &fakePaymentSetups{ready: true, stripeCustomerID: "cus_stripe"}
	charger := &ctxProbeCharger{}
	e.charger = charger

	callerCtx, cancel := context.WithCancel(context.Background())
	cancel() // the HTTP client is already gone

	e.collectAfterFinalize(callerCtx, sub, inv, "test")

	if charger.ctxErrAtCall != nil {
		t.Errorf("charge ctx must be detached from the caller's cancellation, got err=%v", charger.ctxErrAtCall)
	}
	if !charger.hadDeadline {
		t.Error("charge ctx must still carry the 30s deadline after the detach")
	}
	if autoChargePending(t, invoices, inv.ID) {
		t.Error("successful charge must not queue a retry")
	}
}

// TestSweepNoPM_SetupEmailSentExactlyOnce pins the sweep-side no-PM email
// (ADR-087 follow-up): a card-less invoice that reaches the auto-charge sweep
// WITHOUT a finalize-time email — a sweep-mediated proration invoice, or a
// finalize whose PM resolve errored (#449 queues without emailing) — gets the
// setup-link email exactly ONCE across ticks, gated by the durable
// no_pm_notified_at stamp. A resolve ERROR sends nothing (unknown ≠ missing),
// and a skipped-no-email outcome stays unstamped so it self-heals when the
// customer gains an address.
func TestSweepNoPM_SetupEmailSentExactlyOnce(t *testing.T) {
	ctx := context.Background()

	t.Run("no PM, never emailed → one email, stamped; second tick silent", func(t *testing.T) {
		e, invoices, notifier, _, inv := collectFixture()
		// The sweep only visits flagged invoices; the claim CAS re-asserts it.
		invoices.invoices[0].AutoChargePending = true
		inv.AutoChargePending = true
		// collectFixture's paymentSetups default to no-PM; drive the sweep body.
		if n, errs := e.processAutoCharge(ctx, []domain.Invoice{inv}); n != 0 || len(errs) != 0 {
			t.Fatalf("tick 1: charged=%d errs=%v", n, errs)
		}
		if len(notifier.got) != 1 {
			t.Fatalf("tick 1 notifies = %d, want 1", len(notifier.got))
		}
		stamped, _ := invoices.GetInvoice(ctx, "t1", inv.ID)
		if stamped.NoPMNotifiedAt == nil {
			t.Fatal("send-once marker must be stamped after the email")
		}
		// Tick 2 re-lists the same still-unpaid invoice (fresh read, as the
		// real sweep would).
		if _, errs := e.processAutoCharge(ctx, []domain.Invoice{stamped}); len(errs) != 0 {
			t.Fatalf("tick 2 errs: %v", errs)
		}
		if len(notifier.got) != 1 {
			t.Errorf("tick 2 must NOT re-email, total notifies = %d", len(notifier.got))
		}
	})

	t.Run("finalize-time email already sent → sweep stays silent", func(t *testing.T) {
		e, invoices, notifier, sub, inv := collectFixture()
		// Finalize-time pipeline sends + stamps...
		e.collectAfterFinalize(ctx, sub, inv, "test")
		if len(notifier.got) != 1 {
			t.Fatalf("pipeline notifies = %d, want 1", len(notifier.got))
		}
		stamped, _ := invoices.GetInvoice(ctx, "t1", inv.ID)
		if stamped.NoPMNotifiedAt == nil {
			t.Fatal("pipeline must stamp the send-once marker")
		}
		// ...so the sweep's next tick must not double-send.
		if _, errs := e.processAutoCharge(ctx, []domain.Invoice{stamped}); len(errs) != 0 {
			t.Fatalf("sweep errs: %v", errs)
		}
		if len(notifier.got) != 1 {
			t.Errorf("sweep after finalize-time email must NOT re-email, total = %d", len(notifier.got))
		}
	})

	t.Run("PM resolve ERROR → no email (unknown ≠ missing), no stamp", func(t *testing.T) {
		e, invoices, notifier, _, inv := collectFixture()
		invoices.invoices[0].AutoChargePending = true
		inv.AutoChargePending = true
		e.paymentSetups = &erroringPaymentSetups{err: errors.New("db blip")}
		if _, errs := e.processAutoCharge(ctx, []domain.Invoice{inv}); len(errs) != 0 {
			t.Fatalf("errs: %v", errs)
		}
		if len(notifier.got) != 0 {
			t.Errorf("resolve error must not email, got %d", len(notifier.got))
		}
		got, _ := invoices.GetInvoice(ctx, "t1", inv.ID)
		if got.NoPMNotifiedAt != nil {
			t.Error("resolve error must not stamp the marker")
		}
	})

	t.Run("customer has no email → unstamped, retried next tick (self-heal)", func(t *testing.T) {
		e, invoices, _, _, inv := collectFixture()
		invoices.invoices[0].AutoChargePending = true
		inv.AutoChargePending = true
		skipper := &skippingNoPMNotifier{}
		e.noPMNotifier = skipper
		if _, errs := e.processAutoCharge(ctx, []domain.Invoice{inv}); len(errs) != 0 {
			t.Fatalf("errs: %v", errs)
		}
		if skipper.calls != 1 {
			t.Fatalf("notifier attempts = %d, want 1", skipper.calls)
		}
		got, _ := invoices.GetInvoice(ctx, "t1", inv.ID)
		if got.NoPMNotifiedAt != nil {
			t.Fatal("skipped-no-email must NOT stamp — it must retry when an address appears")
		}
		if _, errs := e.processAutoCharge(ctx, []domain.Invoice{got}); len(errs) != 0 {
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

// TestApplyCreditsAndCollect pins ADR-088: day-1 and final-on-cancel invoices
// consume the customer's credit balance before any card charge — the card is
// charged only the remainder, a fully covered invoice settles paid with no
// payment attempt, and an apply FAILURE queues for the sweep WITHOUT charging
// (trap R1: the 2026-05-30 overcharge class — the sweep re-applies credits
// atomically before its own charge, so recovery pre-exists).
func TestApplyCreditsAndCollect(t *testing.T) {
	ctx := context.Background()

	t.Run("partial credit → card charged exactly the remainder", func(t *testing.T) {
		e, invoices, _, sub, inv := collectFixture()
		e.paymentSetups = &fakePaymentSetups{ready: true, stripeCustomerID: "cus_stripe"}
		charger := &recordingCharger{}
		e.charger = charger
		e.credits = &fakeCreditApplier{inv: invoices, applyCents: 2000}

		e.applyCreditsAndCollect(ctx, sub, inv, "test")
		if len(charger.got) != 1 {
			t.Fatalf("charges = %d, want 1", len(charger.got))
		}
		if charger.got[0].AmountDueCents != 3000 {
			t.Errorf("charged = %d, want post-credit remainder 3000 (5000 - 2000)", charger.got[0].AmountDueCents)
		}
	})

	t.Run("full credit → settled paid, no charge, dunning resolved", func(t *testing.T) {
		e, invoices, notifier, sub, inv := collectFixture()
		e.paymentSetups = &fakePaymentSetups{ready: true, stripeCustomerID: "cus_stripe"}
		charger := &recordingCharger{}
		e.charger = charger
		e.credits = &fakeCreditApplier{inv: invoices, applyCents: 5000}
		resolver := &recordingDunningResolver{}
		e.SetDunningResolver(resolver)

		e.applyCreditsAndCollect(ctx, sub, inv, "test")
		if len(charger.got) != 0 {
			t.Errorf("fully covered → no charge, got %d", len(charger.got))
		}
		got, _ := invoices.GetInvoice(ctx, "t1", inv.ID)
		if got.Status != domain.InvoicePaid || got.PaymentStatus != domain.PaymentSucceeded {
			t.Errorf("invoice = %s/%s, want paid/succeeded (Stripe parity: no payment attempted)", got.Status, got.PaymentStatus)
		}
		if len(resolver.resolved) != 1 {
			t.Errorf("credit settle must resolve dunning, got %d", len(resolver.resolved))
		}
		if len(notifier.got) != 0 {
			t.Errorf("no email on a settled invoice, got %d", len(notifier.got))
		}
	})

	t.Run("apply FAILS → queued for sweep, card NEVER charged pre-credit (R1)", func(t *testing.T) {
		e, invoices, _, sub, inv := collectFixture()
		e.paymentSetups = &fakePaymentSetups{ready: true, stripeCustomerID: "cus_stripe"}
		charger := &recordingCharger{}
		e.charger = charger
		e.credits = &fakeCreditApplier{inv: invoices, err: errors.New("ledger blip")}

		e.applyCreditsAndCollect(ctx, sub, inv, "test")
		if len(charger.got) != 0 {
			t.Fatalf("apply failure must NEVER charge the pre-credit amount, got %d charges", len(charger.got))
		}
		if !autoChargePending(t, invoices, inv.ID) {
			t.Error("apply failure must queue for the sweep (which re-applies atomically)")
		}
	})

	t.Run("draft invoice → credits not applied (waits for tax-retry + sweep)", func(t *testing.T) {
		e, invoices, _, sub, inv := collectFixture()
		invoices.invoices[0].Status = domain.InvoiceDraft
		inv.Status = domain.InvoiceDraft
		applier := &fakeCreditApplier{inv: invoices, applyCents: 5000}
		e.credits = applier

		e.applyCreditsAndCollect(ctx, sub, inv, "test")
		if applier.calls != 0 {
			t.Errorf("credits must not apply to a draft, got %d applies", applier.calls)
		}
	})
}

// TestCollectAfterFinalize_RefusesDraft pins the pipeline's no-drafts
// precondition. It was prose-only, and billOnePeriod violated it: a
// tax-pending cycle invoice is born draft, and the cycle's collect arm gates
// on amount + pause but not status. The no-PM arm then emailed the customer a
// setup link quoting a PRE-TAX amount_due and stamped no_pm_notified_at, so
// the corrected total was never sent; the charge arm just logged a rejection
// (the charger refuses non-finalized invoices).
//
// Collection for those drafts is owned at finalize instead — RetryTax queues
// them for the sweep when tax resolves, and the sweep emails the final total.
func TestCollectAfterFinalize_RefusesDraft(t *testing.T) {
	ctx := context.Background()
	e, invoices, notifier, sub, inv := collectFixture()
	inv.Status = domain.InvoiceDraft
	inv.TaxStatus = domain.InvoiceTaxPending

	e.collectAfterFinalize(ctx, sub, inv, "test")

	if len(notifier.got) != 0 {
		t.Errorf("draft must not trigger the setup-link email (it would quote a pre-tax total), got %d notifies", len(notifier.got))
	}
	if autoChargePending(t, invoices, inv.ID) {
		t.Error("draft must not be queued here; the tax-retry finalize owns that handoff")
	}
}
