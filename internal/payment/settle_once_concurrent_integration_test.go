package payment_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/email"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/payment"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
	"github.com/sagarsuperuser/velox/internal/webhook"
)

// #989: "a payment is recorded once" on a real database, with the sources
// that learn of one PaymentIntent's success running at the same time.
//
// Every source ends in SettleSucceeded, whose fast-path guard reads the
// CALLER's snapshot. Racers that read the invoice before anyone settled it
// all pass that guard; what keeps the payment single is the store's
// SELECT … FOR UPDATE and its already-paid early return
// (invoice/postgres.go markPaidReportingTransition). The existing settlement
// tests run two calls one after the other on an in-memory fake that
// re-implements that gate, so removing the lock left them green.
//
// Two reachable races, both on the real invoice store, the real webhook
// outbox and the real email outbox, wired as router.go wires them:
//
//   - charge response vs webhook: an auto-charge whose create response says
//     `succeeded` while Stripe's payment_intent.succeeded (redelivered) lands.
//     The row holds no PaymentIntent yet, so the webhook finds the invoice by
//     its velox_invoice_id metadata. The reconciler cannot see this row.
//   - webhook vs reconciler: a charge whose outcome was ambiguous (the row is
//     `unknown` and names the PaymentIntent), then the webhook (redelivered)
//     and two overlapping reconciler sweeps all learn it succeeded. The
//     charge call cannot run again here — every charge claim refuses an
//     `unknown` row.
//
// Each round counts what one payment must leave behind exactly once.

const (
	settleRaceRounds = 10
	settleDue        = int64(5000)
)

// settleRaceStripe answers like Stripe for the one PaymentIntent of a round:
// the create response and GetPaymentIntent both report it succeeded.
type settleRaceStripe struct {
	mu        sync.Mutex
	createRes payment.PaymentIntentResult
	createErr error
	gets      map[string]int
}

func (f *settleRaceStripe) CreatePaymentIntent(context.Context, payment.PaymentIntentParams) (payment.PaymentIntentResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.createRes, f.createErr
}

func (f *settleRaceStripe) CancelPaymentIntent(context.Context, string) error { return nil }

func (f *settleRaceStripe) GetPaymentIntent(_ context.Context, piID string) (payment.PaymentIntentResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets[piID]++
	return payment.PaymentIntentResult{ID: piID, Status: "succeeded", AmountReceivedCents: settleDue}, nil
}

func (f *settleRaceStripe) ListPaymentIntentsForCustomer(context.Context, string, time.Time) ([]payment.PaymentIntentResult, error) {
	return nil, nil
}

func (f *settleRaceStripe) getCount(piID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gets[piID]
}

type settleRaceCard struct{}

func (settleRaceCard) FetchCardDetails(context.Context, string) (payment.CardDetails, error) {
	return payment.CardDetails{Brand: "visa", Last4: "4242"}, nil
}

func (settleRaceCard) FetchCardForPaymentIntent(context.Context, string) (payment.CardDetails, error) {
	return payment.CardDetails{Brand: "visa", Last4: "4242"}, nil
}

// settleRaceEmails resolves the receipt recipient from the real customer row,
// as router.go's customerEmailFetcherAdapter does (customers.email).
type settleRaceEmails struct{ store *customer.PostgresStore }

func (r settleRaceEmails) GetCustomerEmail(ctx context.Context, tenantID, customerID string) (string, string, []string, error) {
	c, err := r.store.Get(ctx, tenantID, customerID)
	if err != nil {
		return "", "", nil, err
	}
	return c.Email, c.DisplayName, c.AdditionalEmails, nil
}

// countingStore passes every call to the real store; it only counts how many
// settles got past SettleSucceeded's snapshot guard and reached the locked
// transition. A round where that is 1 was serialized by the guard alone and
// proves nothing about the lock, so the test requires rounds where it is >1.
type countingStore struct {
	payment.InvoiceUpdater
	reached atomic.Int64
}

func (c *countingStore) MarkPaidCardSettlementTransition(ctx context.Context, tenantID, id, pi string, paidAt time.Time, then func(tx *sql.Tx, fresh domain.Invoice) error) (domain.Invoice, bool, error) {
	c.reached.Add(1)
	return c.InvoiceUpdater.MarkPaidCardSettlementTransition(ctx, tenantID, id, pi, paidAt, then)
}

type settleRaceRig struct {
	db        *postgres.DB
	ctx       context.Context
	invStore  *invoice.PostgresStore
	customers *customer.PostgresStore
	counting  *countingStore
	stripe    *payment.Stripe
	client    *settleRaceStripe
	reconcile *payment.Reconciler
}

func newSettleRaceRig(t *testing.T) *settleRaceRig {
	t.Helper()
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)

	invStore := invoice.NewPostgresStore(db)
	outbox := webhook.NewOutboxStore(db)
	invStore.SetOutboxEnqueuer(outbox)
	customers := customer.NewPostgresStore(db)
	sender := email.NewOutboxSender(email.NewOutboxStore(db))
	emails := settleRaceEmails{store: customers}

	client := &settleRaceStripe{gets: map[string]int{}}
	counting := &countingStore{InvoiceUpdater: invStore}
	s := payment.NewStripe(client, counting, payment.NewPostgresWebhookStore(db), nil)
	s.SetCardFetcher(settleRaceCard{})
	s.SetEventDispatcher(webhook.NewOutboxDispatcher(outbox))
	s.SetAnomalyRecorder(invStore)
	s.SetEmailReceipt(sender, emails)
	s.SetEmailPaymentFailed(sender, emails)

	// The unknown sweep's cool-off is the only knob: 1ns so the row stamped a
	// moment ago is due. The settler is the same adapter, as in router.go.
	rec := payment.NewReconciler(client, invStore, time.Nanosecond)
	rec.SetSettler(s)

	return &settleRaceRig{db: db, ctx: ctx, invStore: invStore, customers: customers,
		counting: counting, stripe: s, client: client, reconcile: rec}
}

// seed creates a tenant, a customer with an email and a finalized,
// unpaid invoice of settleDue — the shape every auto-charge starts from.
func (r *settleRaceRig) seed(t *testing.T, name string) (string, domain.Invoice) {
	t.Helper()
	tenantID := testutil.CreateTestTenant(t, r.db, name)
	cust, err := r.customers.Create(r.ctx, tenantID, domain.Customer{
		ExternalID: "cus_" + name, DisplayName: name, Email: "billing@" + name + ".test",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	now := time.Now().UTC()
	inv, err := r.invStore.Create(r.ctx, tenantID, domain.Invoice{
		CustomerID: cust.ID, InvoiceNumber: "INV-" + name, Status: domain.InvoiceFinalized,
		PaymentStatus: domain.PaymentPending, Currency: "USD", SubtotalCents: settleDue,
		TotalAmountCents: settleDue, AmountDueCents: settleDue,
		BillingPeriodStart: now.Add(-time.Hour), BillingPeriodEnd: now, IssuedAt: &now,
	})
	if err != nil {
		t.Fatalf("create invoice: %v", err)
	}
	return tenantID, inv
}

// succeededEvent is Stripe's payment_intent.succeeded for the PI, carrying
// the velox_invoice_id metadata the handler falls back to.
func succeededEvent(evtID, piID, invoiceID string) domain.StripeWebhookEvent {
	amt := settleDue
	return domain.StripeWebhookEvent{
		StripeEventID: evtID, EventType: "payment_intent.succeeded", ObjectType: "payment_intent",
		InvoiceID: invoiceID, PaymentIntentID: piID, PaymentStatus: "succeeded",
		AmountCents: &amt, Currency: "usd", Payload: map[string]any{},
		ReceivedAt: time.Now().UTC(), OccurredAt: time.Now().UTC(),
	}
}

// race releases every racer at once and returns their errors.
func race(racers []func() error) []error {
	start := make(chan struct{})
	errs := make([]error, len(racers))
	var wg sync.WaitGroup
	for i, fn := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs[i] = fn()
		}()
	}
	close(start)
	wg.Wait()
	return errs
}

func (r *settleRaceRig) count(t *testing.T, q string, args ...any) int {
	t.Helper()
	tx, err := r.db.BeginTx(r.ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatal(err)
	}
	defer postgres.Rollback(tx)
	var n int
	if err := tx.QueryRowContext(r.ctx, q, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", q, err)
	}
	return n
}

// assertRecordedOnce checks everything one successful payment leaves behind.
// seqBefore is the invoice's charge_attempt_seq before the race: the settle
// UPDATE bumps it, so exactly one settle write leaves it at seqBefore+1.
func (r *settleRaceRig) assertRecordedOnce(t *testing.T, round int, tenantID, invID, piID, evtID string, seqBefore int64) {
	t.Helper()
	got, err := r.invStore.Get(r.ctx, tenantID, invID)
	if err != nil {
		t.Fatalf("round %d: get invoice: %v", round, err)
	}
	if got.Status != domain.InvoicePaid || got.PaymentStatus != domain.PaymentSucceeded {
		t.Errorf("round %d: invoice %s/%s, want paid/succeeded", round, got.Status, got.PaymentStatus)
	}
	if got.StripePaymentIntentID != piID {
		t.Errorf("round %d: invoice records PI %q, want %q", round, got.StripePaymentIntentID, piID)
	}
	if got.AmountPaidCents != settleDue || got.AmountDueCents != 0 {
		t.Errorf("round %d: amount_paid=%d amount_due=%d, want %d/0 (a second settle UPDATE re-reads the zeroed due and books amount_paid=0)",
			round, got.AmountPaidCents, got.AmountDueCents, settleDue)
	}
	if got.ChargeAttemptSeq != seqBefore+1 {
		t.Errorf("round %d: charge_attempt_seq=%d, want %d (one settle write)", round, got.ChargeAttemptSeq, seqBefore+1)
	}
	if got.PaymentCardLast4 != "4242" {
		t.Errorf("round %d: card last4=%q, want 4242 stamped by the winner", round, got.PaymentCardLast4)
	}

	events := func(eventType string) int {
		return r.count(t, `SELECT count(*) FROM webhook_outbox WHERE tenant_id=$1 AND event_type=$2 AND payload->'invoice'->>'id'=$3`,
			tenantID, eventType, invID)
	}
	for _, want := range []struct {
		event string
		n     int
	}{
		{domain.EventInvoicePaid, 1},
		{domain.EventPaymentSucceeded, 1},
		{domain.EventPaymentDuplicateCharge, 0}, // a same-PI race is not a duplicate charge
		{domain.EventPaymentAmountMismatch, 0},
	} {
		if n := events(want.event); n != want.n {
			t.Errorf("round %d: %s outbox rows = %d, want %d", round, want.event, n, want.n)
		}
	}
	if n := r.count(t, `SELECT count(*) FROM email_outbox WHERE tenant_id=$1 AND email_type=$2`,
		tenantID, email.TypePaymentReceipt); n != 1 {
		t.Errorf("round %d: payment_receipt email rows = %d, want 1", round, n)
	}
	if n := r.count(t, `SELECT count(*) FROM invoice_charge_attempts WHERE tenant_id=$1 AND stripe_payment_intent_id=$2`,
		tenantID, piID); n != 1 {
		t.Errorf("round %d: charge-attempt rows for the PI = %d, want 1", round, n)
	}
	if n := r.count(t, `SELECT count(*) FROM invoice_charge_attempts WHERE tenant_id=$1 AND stripe_payment_intent_id=$2 AND outcome='succeeded' AND amount_cents=$3`,
		tenantID, piID, settleDue); n != 1 {
		t.Errorf("round %d: the PI's attempt row is not succeeded for %d", round, settleDue)
	}
	if n := r.count(t, `SELECT count(*) FROM stripe_webhook_events WHERE tenant_id=$1 AND stripe_event_id=$2`,
		tenantID, evtID); n != 1 {
		t.Errorf("round %d: stored webhook events for %s = %d, want 1", round, evtID, n)
	}
}

func TestSettleSucceeded_ConcurrentSources_RecordsOnce(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test — requires postgres")
	}
	r := newSettleRaceRig(t)

	t.Run("charge response vs redelivered webhook", func(t *testing.T) {
		contended := 0
		for round := range settleRaceRounds {
			tenantID, inv := r.seed(t, fmt.Sprintf("settle-inline-%d", round))
			piID := fmt.Sprintf("pi_inline_race_%d", round)
			evtID := "evt_" + piID
			r.client.mu.Lock()
			r.client.createRes, r.client.createErr = payment.PaymentIntentResult{ID: piID, Status: "succeeded"}, nil
			r.client.mu.Unlock()
			r.counting.reached.Store(0)

			webhookRacer := func() error {
				return r.stripe.HandleWebhook(r.ctx, tenantID, succeededEvent(evtID, piID, inv.ID))
			}
			// The charge runs under its claim, as every initiator's does.
			hold, err := r.invStore.ClaimChargeForManualCollect(r.ctx, tenantID, inv.ID)
			if err != nil || !hold.Held() {
				t.Fatalf("round %d: claim: held=%v err=%v", round, hold.Held(), err)
			}
			errs := race([]func() error{
				func() error {
					_, err := r.stripe.ChargeInvoice(r.ctx, tenantID, inv, hold, "cus_stripe_race", "pm_race_4242")
					if errors.Is(err, payment.ErrPaymentTransient) {
						// A webhook racer settled the invoice before the charge was
						// sent, so its re-check refused to send (ADR-127): the
						// payment is still recorded once, by the webhook.
						return nil
					}
					return err
				},
				webhookRacer, webhookRacer, webhookRacer,
			})
			for i, err := range errs {
				if err != nil {
					t.Fatalf("round %d: racer %d: %v", round, i, err)
				}
			}
			if r.counting.reached.Load() > 1 {
				contended++
			}
			r.assertRecordedOnce(t, round, tenantID, inv.ID, piID, evtID, inv.ChargeAttemptSeq)
		}
		if contended == 0 {
			t.Fatalf("no round had two settles reach the locked transition — the snapshot guard serialized every round, so the row lock was never exercised")
		}
		t.Logf("%d/%d rounds had ≥2 settles reach the locked transition", contended, settleRaceRounds)
	})

	t.Run("redelivered webhook vs overlapping reconciler sweeps", func(t *testing.T) {
		contended, reconTookPart := 0, 0
		for round := range settleRaceRounds {
			tenantID, inv := r.seed(t, fmt.Sprintf("settle-recon-%d", round))
			piID := fmt.Sprintf("pi_recon_race_%d", round)
			evtID := "evt_" + piID

			// The real charge path parks the outcome: Stripe named the PI but
			// the answer was ambiguous, so the row is `unknown` with the PI.
			r.client.mu.Lock()
			r.client.createRes = payment.PaymentIntentResult{}
			r.client.createErr = &payment.PaymentError{Message: "stripe: timeout", PaymentIntentID: piID, Unknown: true}
			r.client.mu.Unlock()
			hold, err := r.invStore.ClaimChargeForManualCollect(r.ctx, tenantID, inv.ID)
			if err != nil || !hold.Held() {
				t.Fatalf("round %d: claim: held=%v err=%v", round, hold.Held(), err)
			}
			if _, err := r.stripe.ChargeInvoice(r.ctx, tenantID, inv, hold, "cus_stripe_race", "pm_race_4242"); err == nil {
				t.Fatalf("round %d: ambiguous charge returned no error", round)
			}
			parked, err := r.invStore.Get(r.ctx, tenantID, inv.ID)
			if err != nil {
				t.Fatal(err)
			}
			if parked.PaymentStatus != domain.PaymentUnknown || parked.StripePaymentIntentID != piID {
				t.Fatalf("round %d: setup: invoice %s with PI %q, want unknown with %q", round, parked.PaymentStatus, parked.StripePaymentIntentID, piID)
			}
			time.Sleep(2 * time.Millisecond) // past the reconciler's 1ns cool-off on updated_at
			r.counting.reached.Store(0)
			getsBefore := r.client.getCount(piID)

			webhookRacer := func() error {
				return r.stripe.HandleWebhook(r.ctx, tenantID, succeededEvent(evtID, piID, inv.ID))
			}
			reconRacer := func() error {
				_, errs := r.reconcile.Run(r.ctx, 50)
				return errors.Join(errs...)
			}
			errs := race([]func() error{webhookRacer, reconRacer, webhookRacer, reconRacer, webhookRacer})
			for i, err := range errs {
				if err != nil {
					t.Fatalf("round %d: racer %d: %v", round, i, err)
				}
			}
			// A sweep that lists after a webhook settled finds nothing to do,
			// so a single round may legitimately have no reconciler in it.
			if r.client.getCount(piID) > getsBefore {
				reconTookPart++
			}
			if r.counting.reached.Load() > 1 {
				contended++
			}
			r.assertRecordedOnce(t, round, tenantID, inv.ID, piID, evtID, parked.ChargeAttemptSeq)
		}
		if contended == 0 {
			t.Fatalf("no round had two settles reach the locked transition — the snapshot guard serialized every round, so the row lock was never exercised")
		}
		if reconTookPart == 0 {
			t.Fatalf("no reconciler sweep asked Stripe about its PI in any round — the reconciler never took part")
		}
		t.Logf("%d/%d rounds had ≥2 settles reach the locked transition; the reconciler queried Stripe in %d", contended, settleRaceRounds, reconTookPart)
	})
}
