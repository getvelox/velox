package payment

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/email"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/payment/breaker"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/tenant"
	"github.com/sagarsuperuser/velox/internal/testutil"
	"github.com/sagarsuperuser/velox/internal/webhook"
)

// #976 on a real database (ADR-127). Each test stops a charge at one point —
// a crash before the request leaves, a crash after Stripe charged, the
// caller's deadline ending during the call — or gives Stripe's unclear
// answers, then checks two things: no path charges the invoice a second time
// while its charge hold is set, live or stale (counting the PaymentIntents at
// Stripe, not only the invoice's state), and the reconciler's stale-hold
// sweep ends it the way Stripe says. Only Stripe is faked (fakeStripe).

const shDue = int64(5000)

type shRig struct {
	t         *testing.T
	db        *postgres.DB
	ctx       context.Context
	tenantID  string
	invoices  *invoice.PostgresStore
	customers *customer.PostgresStore
	invSvc    *invoice.Service
	fake      *fakeStripe
	stripe    *Stripe
	rec       *Reconciler
	checkout  *CheckoutSessionStore
	dunning   *shDunning
}

// shDunning counts StartDunning and the recovered resolves, safely across
// goroutines.
type shDunning struct {
	mu       sync.Mutex
	started  []string
	resolved []string
}

func (d *shDunning) StartDunning(_ context.Context, _ string, invoiceID, _ string, _ time.Time, _ domain.DunningStartCause) (domain.InvoiceDunningRun, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.started = append(d.started, invoiceID)
	return domain.InvoiceDunningRun{ID: "drun_" + invoiceID, InvoiceID: invoiceID}, nil
}

func (d *shDunning) ResolveByInvoice(_ context.Context, _, invoiceID string, res domain.DunningResolution) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if res == domain.ResolutionPaymentRecovered {
		d.resolved = append(d.resolved, invoiceID)
	}
	return nil
}

func (d *shDunning) resolvedCount(invoiceID string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, id := range d.resolved {
		if id == invoiceID {
			n++
		}
	}
	return n
}

type shEmails struct{ store *customer.PostgresStore }

func (r shEmails) GetCustomerEmail(ctx context.Context, tenantID, customerID string) (string, string, []string, error) {
	c, err := r.store.Get(ctx, tenantID, customerID)
	if err != nil {
		return "", "", nil, err
	}
	return c.Email, c.DisplayName, c.AdditionalEmails, nil
}

type shCards struct{}

func (shCards) FetchCardDetails(context.Context, string) (CardDetails, error) {
	return CardDetails{Brand: "visa", Last4: "4242"}, nil
}

func (shCards) FetchCardForPaymentIntent(context.Context, string) (CardDetails, error) {
	return CardDetails{Brand: "visa", Last4: "4242"}, nil
}

// newSHRig wires the charge path and the reconciler as router.go does, on the
// real invoice store, webhook outbox and email outbox.
func newSHRig(t *testing.T, name string) *shRig {
	t.Helper()
	db := testutil.SetupTestDB(t)
	r := &shRig{t: t, db: db, ctx: postgres.WithLivemode(context.Background(), false)}
	r.tenantID = testutil.CreateTestTenant(t, db, name)
	r.invoices = invoice.NewPostgresStore(db)
	outbox := webhook.NewOutboxStore(db)
	r.invoices.SetOutboxEnqueuer(outbox)
	r.customers = customer.NewPostgresStore(db)
	r.invSvc = invoice.NewService(r.invoices, clock.Real(), tenant.NewSettingsStore(db))
	sender := email.NewOutboxSender(email.NewOutboxStore(db))
	emails := shEmails{store: r.customers}
	r.fake = newFakeStripe()
	r.dunning = &shDunning{}

	r.stripe = NewStripe(r.fake, r.invoices, NewPostgresWebhookStore(db), nil, r.dunning)
	r.stripe.SetDunningResolver(r.dunning)
	r.stripe.SetCardFetcher(shCards{})
	r.stripe.SetEventDispatcher(webhook.NewOutboxDispatcher(outbox))
	r.stripe.SetAnomalyRecorder(r.invoices)
	r.stripe.SetEmailReceipt(sender, emails)
	r.stripe.SetEmailPaymentFailed(sender, emails)
	r.rec = NewReconciler(r.fake, r.invoices, time.Minute)
	r.rec.SetSettler(r.stripe)
	r.checkout = NewCheckoutSessionStore(db)
	return r
}

// seed creates a customer with an email and a Stripe customer, and a
// finalized, unpaid invoice queued for collection: what every charge starts
// from.
func (r *shRig) seed(num string) domain.Invoice {
	r.t.Helper()
	return r.seedFor(num, true)
}

func (r *shRig) seedFor(num string, withStripeCustomer bool) domain.Invoice {
	r.t.Helper()
	cust, err := r.customers.Create(r.ctx, r.tenantID, domain.Customer{
		ExternalID: "ext_" + num, DisplayName: num, Email: "billing@" + num + ".test",
	})
	if err != nil {
		r.t.Fatalf("create customer: %v", err)
	}
	if withStripeCustomer {
		if err := r.customers.SetStripeCustomerID(r.ctx, r.tenantID, cust.ID, "cus_stripe_"+num); err != nil {
			r.t.Fatalf("set stripe customer: %v", err)
		}
	}
	now := time.Now().UTC()
	inv, err := r.invoices.Create(r.ctx, r.tenantID, domain.Invoice{
		CustomerID: cust.ID, InvoiceNumber: num, Status: domain.InvoiceFinalized,
		PaymentStatus: domain.PaymentPending, Currency: "USD", SubtotalCents: shDue,
		TotalAmountCents: shDue, AmountDueCents: shDue,
		BillingPeriodStart: now.Add(-time.Hour), BillingPeriodEnd: now, IssuedAt: &now,
	})
	if err != nil {
		r.t.Fatalf("create invoice: %v", err)
	}
	return inv
}

func stripeCustomerOf(inv domain.Invoice) string { return "cus_stripe_" + inv.InvoiceNumber }

type chargeKind string

const (
	autoCharge   chargeKind = "auto-charge"
	dunningRetry chargeKind = "dunning retry"
	collect      chargeKind = "Collect payment"
)

// errNotClaimed: the initiator's claim refused the invoice, so it charged
// nothing.
var errNotClaimed = errors.New("claim refused")

// chargeAs runs one charge initiator the way production does: take its claim
// on the invoice, re-read it, then charge the given card under the claim.
func (r *shRig) chargeAs(ctx context.Context, kind chargeKind, invoiceID, card string) (domain.Invoice, error) {
	c, err := r.claim(kind, invoiceID)
	if err != nil {
		return domain.Invoice{}, fmt.Errorf("claim: %w", err)
	}
	if !c.ok() {
		return domain.Invoice{}, errNotClaimed
	}
	inv, err := r.invoices.Get(r.ctx, r.tenantID, invoiceID)
	if err != nil {
		return domain.Invoice{}, err
	}
	return r.chargeUnder(ctx, kind, c, inv, card)
}

// crash runs fn and reports whether it died at a fakeCrash: what a crash
// leaves committed is what the rest of the test sees.
func crash(fn func()) (crashed bool) {
	defer func() {
		if p := recover(); p != nil {
			if _, ok := p.(fakeCrash); !ok {
				panic(p)
			}
			crashed = true
		}
	}()
	fn()
	return false
}

func (r *shRig) exec(q string, args ...any) {
	r.t.Helper()
	tx, err := r.db.BeginTx(r.ctx, postgres.TxBypass, "")
	if err != nil {
		r.t.Fatal(err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(r.ctx, q, args...); err != nil {
		r.t.Fatalf("%s: %v", q, err)
	}
	if err := tx.Commit(); err != nil {
		r.t.Fatal(err)
	}
}

func (r *shRig) count(q string, args ...any) int {
	r.t.Helper()
	tx, err := r.db.BeginTx(r.ctx, postgres.TxBypass, "")
	if err != nil {
		r.t.Fatal(err)
	}
	defer postgres.Rollback(tx)
	var n int
	if err := tx.QueryRowContext(r.ctx, q, args...).Scan(&n); err != nil {
		r.t.Fatalf("count %q: %v", q, err)
	}
	return n
}

// hold reads the invoice's charge hold: nil when none is set.
func (r *shRig) hold(invoiceID string) *time.Time {
	r.t.Helper()
	tx, err := r.db.BeginTx(r.ctx, postgres.TxBypass, "")
	if err != nil {
		r.t.Fatal(err)
	}
	defer postgres.Rollback(tx)
	var h sql.NullTime
	if err := tx.QueryRowContext(r.ctx, `SELECT auto_charge_claimed_until FROM invoices WHERE id = $1`, invoiceID).Scan(&h); err != nil {
		r.t.Fatalf("read hold: %v", err)
	}
	if !h.Valid {
		return nil
	}
	return &h.Time
}

// ageHold moves a set hold into the past by age, past its end: what a
// crashed or timed-out charger leaves once its five minutes are over. It also
// forgets any earlier check of the row.
func (r *shRig) ageHold(invoiceID string, age time.Duration) {
	r.exec(`UPDATE invoices SET auto_charge_claimed_until = now() - make_interval(secs => $2), provider_synced_at = NULL
		WHERE id = $1 AND auto_charge_claimed_until IS NOT NULL`, invoiceID, age.Seconds())
}

// staleSweepAge is past the hold's end plus the sweep's 10-minute wait.
const staleSweepAge = 11 * time.Minute

func (r *shRig) run() []error {
	r.t.Helper()
	_, errs := r.rec.Run(r.ctx, 50)
	return errs
}

func (r *shRig) runClean() {
	r.t.Helper()
	if errs := r.run(); len(errs) > 0 {
		r.t.Fatalf("reconciler: %v", errs)
	}
}

func (r *shRig) get(invoiceID string) domain.Invoice {
	r.t.Helper()
	inv, err := r.invoices.Get(r.ctx, r.tenantID, invoiceID)
	if err != nil {
		r.t.Fatalf("get invoice: %v", err)
	}
	return inv
}

func (r *shRig) events(eventType, invoiceID string) int {
	r.t.Helper()
	return r.count(`SELECT count(*) FROM webhook_outbox WHERE tenant_id = $1 AND event_type = $2 AND payload->'invoice'->>'id' = $3`,
		r.tenantID, eventType, invoiceID)
}

func (r *shRig) receipts() int {
	r.t.Helper()
	return r.count(`SELECT count(*) FROM email_outbox WHERE tenant_id = $1 AND email_type = $2`, r.tenantID, email.TypePaymentReceipt)
}

// assertNoSecondCharge runs every way a second payment could start, with the
// customer's default card changed, and requires each to charge nothing: the
// auto-charge sweep, a dunning retry, Collect payment and the hosted page.
// It counts the PaymentIntents at Stripe, so a claim that wrongly let a
// charge through fails here even if the invoice state looked fine.
func (r *shRig) assertNoSecondCharge(invoiceID string) {
	r.t.Helper()
	before := r.fake.countPIs()
	for _, kind := range []chargeKind{autoCharge, dunningRetry, collect} {
		_, err := r.chargeAs(r.ctx, kind, invoiceID, "pm_card_changed")
		if !errors.Is(err, errNotClaimed) {
			r.t.Errorf("%s with a changed card: err = %v, want its claim refused while the charge hold is set", kind, err)
		}
	}
	inv := r.get(invoiceID)
	if _, _, err := r.checkout.ClaimOpen(r.ctx, r.tenantID, invoiceID, inv.AmountDueCents, inv.Currency, false); !errors.Is(err, ErrChargeInFlight) {
		r.t.Errorf("hosted page claim: err = %v, want ErrChargeInFlight while the charge hold is set", err)
		if err == nil {
			// The customer pays on the hosted page: Checkout's own PaymentIntent.
			r.fake.addPI(stripeCustomerOf(inv), invoiceID, PurposeHostedInvoicePay, "succeeded", inv.AmountDueCents)
		}
	}
	if after := r.fake.countPIs(); after != before {
		r.t.Errorf("PaymentIntents at Stripe went from %d to %d: a second payment started while the first may have charged", before, after)
	}
}

// assertPaidOnce checks everything one payment leaves behind, once.
func (r *shRig) assertPaidOnce(invoiceID, piID string) {
	r.t.Helper()
	got := r.get(invoiceID)
	if got.Status != domain.InvoicePaid || got.PaymentStatus != domain.PaymentSucceeded || got.StripePaymentIntentID != piID {
		r.t.Errorf("invoice %s/%s with PI %q, want paid/succeeded with %q", got.Status, got.PaymentStatus, got.StripePaymentIntentID, piID)
	}
	if got.AmountPaidCents != shDue {
		r.t.Errorf("amount_paid = %d, want %d", got.AmountPaidCents, shDue)
	}
	if h := r.hold(invoiceID); h != nil {
		r.t.Errorf("charge hold still set (%v) on a paid invoice", *h)
	}
	if n := r.events(domain.EventPaymentSucceeded, invoiceID); n != 1 {
		r.t.Errorf("payment.succeeded rows = %d, want 1", n)
	}
	if n := r.events(domain.EventPaymentDuplicateCharge, invoiceID); n != 0 {
		r.t.Errorf("payment.duplicate_charge rows = %d, want 0", n)
	}
	if n := r.receipts(); n != 1 {
		r.t.Errorf("payment receipts = %d, want 1", n)
	}
	if n := r.fake.countPIs(); n != 1 {
		r.t.Errorf("PaymentIntents at Stripe = %d, want exactly 1", n)
	}
}

// crashAfterCharge charges kind and dies after Stripe created the
// PaymentIntent, before anything was saved. It returns that PaymentIntent.
func (r *shRig) crashAfterCharge(inv domain.Invoice, kind chargeKind, status string) fakePI {
	r.t.Helper()
	r.fake.queue(fakeMode{kind: fakeCrashAfterCreate, status: status})
	if !crash(func() { _, _ = r.chargeAs(r.ctx, kind, inv.ID, "pm_card_1") }) {
		r.t.Fatal("the charge did not reach the crash point")
	}
	got := r.get(inv.ID)
	if got.PaymentStatus != domain.PaymentPending || got.StripePaymentIntentID != "" {
		r.t.Fatalf("after the crash: %s with PI %q, want pending with none (nothing saved)", got.PaymentStatus, got.StripePaymentIntentID)
	}
	if r.hold(inv.ID) == nil {
		r.t.Fatal("no charge hold after the crash: nothing says a charge was sent")
	}
	pi := r.fake.onlyPI()
	if pi.id == "" {
		r.t.Fatalf("setup: Stripe should hold exactly one PaymentIntent, has %d", r.fake.countPIs())
	}
	return pi
}

// 1 + 3. Stripe took the money and the process died before the result was
// saved; no webhook ever arrives. None of the ways to a second payment gets
// through — a changed card, another charge kind, a key Stripe no longer
// remembers after 24 hours, the hosted page — and one reconciler run records
// the payment Stripe took: paid, one receipt, one payment.succeeded, the
// dunning run resolved as recovered, still one PaymentIntent.
func TestStaleHold_CrashAfterStripeCharged_NoSecondPayment(t *testing.T) {
	r := newSHRig(t, "SH Crash After Charge")
	inv := r.seed("INV-SH-CRASH")
	charged := r.crashAfterCharge(inv, autoCharge, "")

	r.assertNoSecondCharge(inv.ID) // the hold is still live

	r.ageHold(inv.ID, staleSweepAge) // the hold has ended: a stale hold
	r.assertNoSecondCharge(inv.ID)

	// Stripe forgets the key, and the hold is 25 hours old.
	r.fake.prune()
	r.ageHold(inv.ID, 25*time.Hour)
	r.assertNoSecondCharge(inv.ID)

	r.runClean()
	r.assertPaidOnce(inv.ID, charged.id)
	if n := r.dunning.resolvedCount(inv.ID); n != 1 {
		t.Errorf("dunning resolved as recovered %d times, want 1", n)
	}
}

// 2. The caller's deadline ends while Stripe is still working (a dunning
// retry's 15 s, auto-charge's 30 s), or the caller is cancelled (a Collect
// request whose browser went away). A deadline is a call that timed out:
// Stripe was slow, as in an incident, so the attempt is saved as parked even
// though the caller's context is over, and gets Stripe's 24 hours when Stripe
// shows nothing (ADR-127 amendment 2026-10-20). A cancellation is Velox
// interrupting itself: nothing is saved. Either way the hold stays, nothing
// charges again, and the sweep records the payment.
func TestStaleHold_CallerDeadlineEndsDuringTheCall(t *testing.T) {
	for _, tc := range []struct {
		kind   chargeKind
		cancel bool // the request context is cancelled, not timed out
	}{
		{kind: dunningRetry},
		{kind: autoCharge},
		{kind: collect, cancel: true},
	} {
		t.Run(string(tc.kind), func(t *testing.T) {
			r := newSHRig(t, "SH Timeout "+string(tc.kind))
			inv := r.seed("INV-SH-TIMEOUT")
			r.fake.queue(fakeMode{kind: fakeOutliveCtx})

			var ctx context.Context
			var cancel context.CancelFunc
			if tc.cancel {
				ctx, cancel = context.WithCancel(r.ctx)
				time.AfterFunc(300*time.Millisecond, cancel)
			} else {
				ctx, cancel = context.WithTimeout(r.ctx, 300*time.Millisecond)
			}
			defer cancel()
			_, err := r.chargeAs(ctx, tc.kind, inv.ID, "pm_card_1")
			var pe *PaymentError
			if !errors.As(err, &pe) || !pe.Unknown {
				t.Fatalf("err = %v, want an unclear outcome", err)
			}

			got := r.get(inv.ID)
			if tc.cancel {
				if got.PaymentStatus != domain.PaymentPending || got.StripePaymentIntentID != "" || got.ChargeAttemptSeq != inv.ChargeAttemptSeq {
					t.Errorf("after the cancellation: %s with PI %q seq %d, want pending, none, %d: nothing saved",
						got.PaymentStatus, got.StripePaymentIntentID, got.ChargeAttemptSeq, inv.ChargeAttemptSeq)
				}
			} else {
				if got.PaymentStatus != domain.PaymentUnknown || got.StripePaymentIntentID != "" || got.ChargeAttemptSeq != inv.ChargeAttemptSeq+1 {
					t.Errorf("after the timeout: %s with PI %q seq %d, want parked: unknown, none, %d (saved after the caller's deadline)",
						got.PaymentStatus, got.StripePaymentIntentID, got.ChargeAttemptSeq, inv.ChargeAttemptSeq+1)
				}
				if n := r.attempts(inv.ID, domain.ChargeAttemptUnknown); n != 1 {
					t.Errorf("attempt rows with an unknown outcome = %d, want 1", n)
				}
			}
			if r.hold(inv.ID) == nil {
				t.Fatal("the unclear charge released its hold")
			}
			charged := r.fake.onlyPI()
			if charged.id == "" {
				t.Fatalf("setup: the request outliving its context should have created one PaymentIntent, Stripe has %d", r.fake.countPIs())
			}

			r.ageHold(inv.ID, staleSweepAge)
			r.assertNoSecondCharge(inv.ID)
			r.runClean()
			r.assertPaidOnce(inv.ID, charged.id)
		})
	}
}

// 4. The hosted page. A live hold refuses it, and so does a stale one. After
// the sweep finds nothing at Stripe the page opens; after it finds the
// payment, the invoice is paid and the page refuses as paid.
func TestStaleHold_HostedPage(t *testing.T) {
	r := newSHRig(t, "SH Hosted")

	inv := r.seed("INV-SH-HOSTED")
	c, err := r.claim(autoCharge, inv.ID)
	if err != nil || !c.ok() {
		t.Fatalf("claim: %v", err)
	}
	if _, _, err := r.checkout.ClaimOpen(r.ctx, r.tenantID, inv.ID, shDue, "USD", false); !errors.Is(err, ErrChargeInFlight) {
		t.Fatalf("live hold: hosted claim err = %v, want ErrChargeInFlight", err)
	}
	r.ageHold(inv.ID, staleSweepAge) // the charger died before sending
	if _, _, err := r.checkout.ClaimOpen(r.ctx, r.tenantID, inv.ID, shDue, "USD", false); !errors.Is(err, ErrChargeInFlight) {
		t.Fatalf("stale hold: hosted claim err = %v, want ErrChargeInFlight (its charge may have taken the money)", err)
	}
	r.runClean() // Stripe has nothing
	if r.hold(inv.ID) != nil {
		t.Fatal("the hold survived a sweep that found nothing at Stripe")
	}
	if _, _, err := r.checkout.ClaimOpen(r.ctx, r.tenantID, inv.ID, shDue, "USD", false); err != nil {
		t.Fatalf("after the sweep found nothing: hosted claim err = %v, want the page to open", err)
	}

	paid := r.seed("INV-SH-HOSTED-PAID")
	charged := r.crashAfterCharge(paid, autoCharge, "")
	r.ageHold(paid.ID, staleSweepAge)
	r.runClean()
	if got := r.get(paid.ID); got.Status != domain.InvoicePaid || got.StripePaymentIntentID != charged.id {
		t.Fatalf("after the sweep found the payment: %s with PI %q, want paid with %q", got.Status, got.StripePaymentIntentID, charged.id)
	}
	if _, _, err := r.checkout.ClaimOpen(r.ctx, r.tenantID, paid.ID, shDue, "USD", false); !errors.Is(err, ErrInvoiceNotPayable) {
		t.Fatalf("hosted claim on the paid invoice: err = %v, want ErrInvoiceNotPayable (the page shows paid)", err)
	}
}

// 5. The process died before the request left. The sweep finds nothing,
// clears the hold and moves the counter, and the next charge goes out under
// the next key: one payment in all.
func TestStaleHold_NothingSent_ReleasedAndChargedOnceUnderTheNextKey(t *testing.T) {
	r := newSHRig(t, "SH Crash Before Send")
	inv := r.seed("INV-SH-C2")
	r.fake.queue(fakeMode{kind: fakeCrashBeforeSend})
	if !crash(func() { _, _ = r.chargeAs(r.ctx, autoCharge, inv.ID, "pm_card_1") }) {
		t.Fatal("the charge did not reach the crash point")
	}
	r.ageHold(inv.ID, staleSweepAge)
	r.assertNoSecondCharge(inv.ID)

	r.runClean()
	got := r.get(inv.ID)
	if r.hold(inv.ID) != nil {
		t.Fatal("hold kept after Stripe was shown to have nothing")
	}
	if got.PaymentStatus != domain.PaymentPending || got.ChargeAttemptSeq != inv.ChargeAttemptSeq+1 {
		t.Fatalf("after the sweep: %s seq %d, want pending seq %d (a fresh key for the next charge)", got.PaymentStatus, got.ChargeAttemptSeq, inv.ChargeAttemptSeq+1)
	}

	if _, err := r.chargeAs(r.ctx, autoCharge, inv.ID, "pm_card_1"); err != nil {
		t.Fatalf("the charge after the sweep: %v", err)
	}
	pi := r.fake.onlyPI()
	if want := ChargeIdempotencyKey(inv.ID, inv.ChargeAttemptSeq+1, ""); pi.key != want {
		t.Errorf("the new charge's key = %q, want %q", pi.key, want)
	}
	r.assertPaidOnce(inv.ID, pi.id)
}

// 6. Stripe saved a 500 for the key with no PaymentIntent (results are saved
// "including 500 errors"); the caller's context had ended, so nothing was
// saved on the invoice. The sweep finds nothing and moves the counter; the
// next charge uses the next key, so it does not get the saved 500 back.
func TestStaleHold_Saved500_NextChargeUsesTheNextKey(t *testing.T) {
	r := newSHRig(t, "SH Saved 500")
	inv := r.seed("INV-SH-500")
	r.fake.queue(fakeMode{kind: fakeSave500})

	ctx, cancel := context.WithCancel(r.ctx)
	r.fake.beforeCreate = cancel // the caller is gone by the time Stripe answers
	if _, err := r.chargeAs(ctx, autoCharge, inv.ID, "pm_card_1"); err == nil {
		t.Fatal("want the 500's error")
	}
	r.fake.beforeCreate = nil
	if n := r.fake.countPIs(); n != 0 {
		t.Fatalf("setup: Stripe should hold no PaymentIntent, has %d", n)
	}
	if got := r.get(inv.ID); got.PaymentStatus != domain.PaymentPending {
		t.Fatalf("setup: the 500 should not be saved on the invoice, which is %s", got.PaymentStatus)
	}
	r.ageHold(inv.ID, staleSweepAge)
	r.runClean()
	if got := r.get(inv.ID); got.ChargeAttemptSeq != inv.ChargeAttemptSeq+1 || r.hold(inv.ID) != nil {
		t.Fatalf("after the sweep: seq %d hold %v, want seq %d and no hold", got.ChargeAttemptSeq, r.hold(inv.ID), inv.ChargeAttemptSeq+1)
	}
	if _, err := r.chargeAs(r.ctx, autoCharge, inv.ID, "pm_card_1"); err != nil {
		t.Fatalf("the charge under the next key got: %v (the saved 500 came back?)", err)
	}
	r.assertPaidOnce(inv.ID, r.fake.onlyPI().id)
}

// 7. What the sweep does with each kind of payment it finds.
func TestStaleHold_WhatTheSweepFinds(t *testing.T) {
	t.Run("processing: adopted onto the invoice", func(t *testing.T) {
		r := newSHRig(t, "SH Found Processing")
		inv := r.seed("INV-SH-PROC")
		pi := r.crashAfterCharge(inv, autoCharge, "processing")
		r.ageHold(inv.ID, staleSweepAge)
		r.runClean()
		got := r.get(inv.ID)
		if got.PaymentStatus != domain.PaymentProcessing || got.StripePaymentIntentID != pi.id {
			t.Fatalf("after the sweep: %s with PI %q, want processing with %q", got.PaymentStatus, got.StripePaymentIntentID, pi.id)
		}
		if r.hold(inv.ID) != nil {
			t.Error("hold kept after the processing payment was put on the invoice")
		}
		if n := r.fake.countPIs(); n != 1 {
			t.Errorf("PaymentIntents at Stripe = %d, want 1", n)
		}
	})

	t.Run("requires_action: canceled at Stripe, then the next run clears", func(t *testing.T) {
		r := newSHRig(t, "SH Found Requires Action")
		inv := r.seed("INV-SH-RA")
		pi := r.crashAfterCharge(inv, autoCharge, "requires_action")
		r.ageHold(inv.ID, staleSweepAge)
		r.runClean()
		if got := r.fake.pi(pi.id); got.status != "canceled" {
			t.Fatalf("PaymentIntent is %q at Stripe, want canceled (an off-session one never ends on its own)", got.status)
		}
		if r.hold(inv.ID) == nil {
			t.Fatal("hold cleared in the same run that canceled; the next run decides")
		}
		r.ageHold(inv.ID, staleSweepAge) // the next run, past the re-check wait
		r.runClean()
		got := r.get(inv.ID)
		if r.hold(inv.ID) != nil || got.ChargeAttemptSeq != inv.ChargeAttemptSeq+1 || got.PaymentStatus != domain.PaymentPending {
			t.Fatalf("after the next run: hold %v, %s seq %d; want cleared, pending, seq %d",
				r.hold(inv.ID), got.PaymentStatus, got.ChargeAttemptSeq, inv.ChargeAttemptSeq+1)
		}
	})

	t.Run("two succeeded: paid with the first, the second flagged as a duplicate", func(t *testing.T) {
		r := newSHRig(t, "SH Found Two")
		inv := r.seed("INV-SH-TWO")
		first := r.crashAfterCharge(inv, autoCharge, "")
		second := r.fake.addPI(stripeCustomerOf(inv), inv.ID, "", "succeeded", shDue)
		r.ageHold(inv.ID, staleSweepAge)
		r.runClean()
		got := r.get(inv.ID)
		if got.Status != domain.InvoicePaid || got.StripePaymentIntentID != first.id {
			t.Fatalf("after the sweep: %s with PI %q, want paid with the first, %q", got.Status, got.StripePaymentIntentID, first.id)
		}
		if n := r.events(domain.EventPaymentDuplicateCharge, inv.ID); n != 1 {
			t.Errorf("payment.duplicate_charge rows = %d, want 1 (for %s)", n, second.id)
		}
		if n := r.events(domain.EventPaymentSucceeded, inv.ID); n != 1 {
			t.Errorf("payment.succeeded rows = %d, want 1", n)
		}
		if n := r.receipts(); n != 1 {
			t.Errorf("receipts = %d, want 1", n)
		}
		if r.hold(inv.ID) != nil {
			t.Error("hold kept after the payments were recorded")
		}
	})

	t.Run("succeeded but fully refunded: not counted as paying the invoice", func(t *testing.T) {
		r := newSHRig(t, "SH Found Refunded")
		inv := r.seed("INV-SH-REFUNDED")
		pi := r.crashAfterCharge(inv, autoCharge, "")
		r.fake.refund(pi.id)
		r.ageHold(inv.ID, staleSweepAge)
		r.runClean()
		got := r.get(inv.ID)
		if got.Status != domain.InvoiceFinalized || got.PaymentStatus != domain.PaymentPending {
			t.Fatalf("after the sweep: %s/%s, want finalized/pending: a refunded payment does not pay the invoice", got.Status, got.PaymentStatus)
		}
		if r.hold(inv.ID) != nil || got.ChargeAttemptSeq != inv.ChargeAttemptSeq+1 {
			t.Errorf("after the sweep: hold %v seq %d, want treated as nothing at Stripe: cleared, seq %d",
				r.hold(inv.ID), got.ChargeAttemptSeq, inv.ChargeAttemptSeq+1)
		}
	})
}

// 8. A charger claims and stalls before sending. Its hold ends, the sweep
// clears it (nothing at Stripe), and another charger claims and sends. When
// the first wakes, its re-check refuses, and it sends nothing.
func TestStaleHold_StalledCharger_SendsNothing(t *testing.T) {
	t.Run("another owner took the invoice", func(t *testing.T) {
		r := newSHRig(t, "SH Stalled")
		inv := r.seed("INV-SH-STALL")
		a, err := r.claim(autoCharge, inv.ID)
		if err != nil || !a.ok() {
			t.Fatalf("A's claim: %v", err)
		}
		snapshotA := r.get(inv.ID) // A read the invoice, then stalled

		r.ageHold(inv.ID, staleSweepAge)
		r.runClean()
		if r.hold(inv.ID) != nil {
			t.Fatal("setup: the sweep should clear A's stale hold (nothing at Stripe)")
		}
		if _, err := r.chargeAs(r.ctx, collect, inv.ID, "pm_card_1"); err != nil {
			t.Fatalf("B's charge: %v", err)
		}
		bPI := r.fake.onlyPI()

		_, err = r.chargeUnder(r.ctx, autoCharge, a, snapshotA, "pm_card_1")
		if !errors.Is(err, ErrPaymentTransient) {
			t.Errorf("A after waking: err = %v, want ErrPaymentTransient (it no longer holds the invoice)", err)
		}
		if n := r.fake.countPIs(); n != 1 {
			t.Fatalf("PaymentIntents at Stripe = %d, want 1: the stalled charger sent its stale request", n)
		}
		r.assertPaidOnce(inv.ID, bPI.id)
	})

	t.Run("a webhook settled the invoice during the stall", func(t *testing.T) {
		r := newSHRig(t, "SH Stalled Settled")
		inv := r.seed("INV-SH-STALL-SETTLED")
		a, err := r.claim(autoCharge, inv.ID)
		if err != nil || !a.ok() {
			t.Fatalf("A's claim: %v", err)
		}
		snapshotA := r.get(inv.ID)
		// An older attempt's late success lands while A stalls, its hold
		// still live.
		if _, _, err := r.invoices.MarkPaidCardSettlementTransition(r.ctx, r.tenantID, inv.ID, "pi_older_attempt", time.Now().UTC(), nil); err != nil {
			t.Fatal(err)
		}
		_, err = r.chargeUnder(r.ctx, autoCharge, a, snapshotA, "pm_card_1")
		if !errors.Is(err, ErrPaymentTransient) {
			t.Errorf("A after waking: err = %v, want ErrPaymentTransient (the invoice was settled meanwhile)", err)
		}
		if r.fake.creates != 0 {
			t.Fatalf("Stripe received %d creates: A charged an invoice already paid", r.fake.creates)
		}
	})
}

// 9. A late release: A claims and stalls; its hold ends and the sweep clears
// it; B claims, and B's result is lost (crash after Stripe charged). When A
// finally releases, B's hold must survive, and the sweep later finds B's
// payment. One PaymentIntent.
func TestStaleHold_LateRelease_KeepsTheOtherOwnersHold(t *testing.T) {
	r := newSHRig(t, "SH Late Release")
	inv := r.seed("INV-SH-LATE")
	a, err := r.claim(autoCharge, inv.ID)
	if err != nil || !a.ok() {
		t.Fatalf("A's claim: %v", err)
	}
	r.ageHold(inv.ID, staleSweepAge)
	r.runClean()
	if r.hold(inv.ID) != nil {
		t.Fatal("setup: the sweep should clear A's stale hold")
	}
	bPI := r.crashAfterCharge(r.get(inv.ID), collect, "")

	if err := r.release(a, inv.ID); err != nil {
		t.Fatalf("A's release: %v", err)
	}
	if r.hold(inv.ID) == nil {
		t.Fatal("A's late release erased B's hold: the only trace that B's charge may have been sent")
	}
	r.ageHold(inv.ID, staleSweepAge)
	r.assertNoSecondCharge(inv.ID)
	r.runClean()
	r.assertPaidOnce(inv.ID, bPI.id)
}

// 11. Stripe will not answer: not connected for this mode, the key rejected,
// or the customer not in this account. The open invoice keeps its hold; void
// and offline payment are refused; write-off is allowed (it moves no money).
// After the write-off the sweep clears the hold with an ERROR. And a
// write-off followed by a payment the sweep does find settles uncollectible →
// paid.
func TestStaleHold_StripeCannotAnswer(t *testing.T) {
	for _, tc := range []struct {
		name   string
		break_ func(f *fakeStripe, stripeCustomer string)
	}{
		{"Stripe not connected", func(f *fakeStripe, _ string) {
			f.mu.Lock()
			f.notConfigured = true
			f.mu.Unlock()
		}},
		{"customer not in this account", func(f *fakeStripe, c string) { f.setCustomer(c, "missing") }},
		{"key rejected", func(f *fakeStripe, c string) { f.setCustomer(c, "auth") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newSHRig(t, "SH Cannot Answer")
			inv := r.seed("INV-SH-CANNOT")
			r.crashAfterCharge(inv, autoCharge, "")
			tc.break_(r.fake, stripeCustomerOf(inv))
			r.ageHold(inv.ID, staleSweepAge)

			_ = r.run()
			got := r.get(inv.ID)
			if r.hold(inv.ID) == nil || got.PaymentStatus != domain.PaymentPending || got.ChargeAttemptSeq != inv.ChargeAttemptSeq {
				t.Fatalf("after a check Stripe would not answer: hold %v, %s seq %d; want the hold kept and the invoice unchanged",
					r.hold(inv.ID), got.PaymentStatus, got.ChargeAttemptSeq)
			}
			if got.ProviderSyncedAt == nil {
				t.Error("the check was not stamped (provider_synced_at), so it rotates nowhere")
			}
			r.assertNoSecondCharge(inv.ID)

			if _, err := r.invSvc.Void(r.ctx, r.tenantID, inv.ID); !errors.Is(err, errs.ErrInvalidState) {
				t.Errorf("void on a stale hold: err = %v, want refused (its charge may have taken the money)", err)
			}
			if _, err := r.invSvc.RecordOfflinePayment(r.ctx, r.tenantID, inv.ID, "wire"); !errors.Is(err, errs.ErrInvalidState) {
				t.Errorf("offline payment on a stale hold: err = %v, want refused (the customer could pay twice)", err)
			}
			if _, err := r.invSvc.MarkUncollectible(r.ctx, r.tenantID, inv.ID); err != nil {
				t.Fatalf("write-off on a stale hold: %v, want allowed (it moves no money; the exit when Stripe cannot answer)", err)
			}

			r.ageHold(inv.ID, staleSweepAge) // past the re-check wait
			_ = r.run()
			if r.hold(inv.ID) != nil {
				t.Error("hold kept on a written-off invoice Stripe cannot answer about: nothing can charge it, and the sweep would loop")
			}
		})
	}

	t.Run("written off, then the sweep finds the payment: uncollectible → paid", func(t *testing.T) {
		r := newSHRig(t, "SH Written Off Then Found")
		inv := r.seed("INV-SH-WO-FOUND")
		charged := r.crashAfterCharge(inv, autoCharge, "")
		r.ageHold(inv.ID, staleSweepAge)
		if _, err := r.invSvc.MarkUncollectible(r.ctx, r.tenantID, inv.ID); err != nil {
			t.Fatalf("write-off on a stale hold: %v", err)
		}
		r.runClean()
		got := r.get(inv.ID)
		if got.Status != domain.InvoicePaid || got.StripePaymentIntentID != charged.id {
			t.Fatalf("after the sweep: %s with PI %q, want paid with %q (a write-off moves no money; the found payment settles it)",
				got.Status, got.StripePaymentIntentID, charged.id)
		}
		if r.hold(inv.ID) != nil {
			t.Error("hold kept after the payment was recorded")
		}
	})
}

// 12. A list that fails part-way is a failed check, never "nothing at
// Stripe": the hold stays and the row is stamped. And failing rows rotate: 25
// failing plus one that resolves, at limit 20, resolve within two runs.
func TestStaleHold_ListFailsPartWay_KeptAndRotates(t *testing.T) {
	r := newSHRig(t, "SH Page Error")
	inv := r.seed("INV-SH-PAGE")
	r.crashAfterCharge(inv, autoCharge, "")
	r.fake.setCustomer(stripeCustomerOf(inv), "fail")
	r.ageHold(inv.ID, staleSweepAge)
	_ = r.run()
	got := r.get(inv.ID)
	if r.hold(inv.ID) == nil || got.PaymentStatus != domain.PaymentPending || got.ChargeAttemptSeq != inv.ChargeAttemptSeq {
		t.Fatalf("after a failed list: hold %v, %s seq %d; want the hold kept and the invoice unchanged (a failed read is never 'nothing')",
			r.hold(inv.ID), got.PaymentStatus, got.ChargeAttemptSeq)
	}
	if got.ProviderSyncedAt == nil {
		t.Error("the failed check was not stamped (provider_synced_at)")
	}

	rot := newSHRig(t, "SH Rotation")
	for i := 0; i < 25; i++ {
		failing := rot.seed(fmt.Sprintf("INV-SH-ROT-F%02d", i))
		if c, err := rot.claim(autoCharge, failing.ID); err != nil || !c.ok() {
			t.Fatalf("claim failing %d: %v", i, err)
		}
		rot.fake.setCustomer(stripeCustomerOf(failing), "fail")
		rot.ageHold(failing.ID, 30*time.Minute) // older holds: ahead of the good row
	}
	good := rot.seed("INV-SH-ROT-GOOD")
	if c, err := rot.claim(autoCharge, good.ID); err != nil || !c.ok() {
		t.Fatalf("claim good: %v", err)
	}
	rot.ageHold(good.ID, staleSweepAge)
	for i := 0; i < 2 && rot.hold(good.ID) != nil; i++ {
		_, _ = rot.rec.Run(rot.ctx, 20)
	}
	if rot.hold(good.ID) != nil {
		t.Fatal("a resolvable stale hold was not reached within two runs: failing rows pinned the head of the queue")
	}
}

// 13. Failing checks never open the breaker that guards customer charges:
// the sweep calls Stripe directly, not through it.
func TestStaleHold_FailingChecksLeaveTheChargeBreakerClosed(t *testing.T) {
	r := newSHRig(t, "SH Breaker")
	b := breaker.New(breaker.Config{Countable: IsUnknownPaymentFailure, FailureThreshold: 5})
	r.stripe.SetBreaker(b)
	r.rec.SetBreaker(b)
	var held []string
	for i := 0; i < 6; i++ {
		inv := r.seed(fmt.Sprintf("INV-SH-BRK-%d", i))
		if c, err := r.claim(autoCharge, inv.ID); err != nil || !c.ok() {
			t.Fatalf("claim %d: %v", i, err)
		}
		r.fake.setCustomer(stripeCustomerOf(inv), "fail")
		r.ageHold(inv.ID, staleSweepAge)
		held = append(held, inv.ID)
	}
	_ = r.run()
	if st := b.State(); st != breaker.StateClosed {
		t.Fatalf("charge breaker is %s after failing stale-hold checks, want closed", st)
	}
	if n := r.fake.listCount(); n < 5 {
		t.Fatalf("stale-hold lists = %d, want at least 5: the checks must have run", n)
	}
	for _, id := range held {
		if r.get(id).ProviderSyncedAt == nil {
			t.Errorf("invoice %s: the failed check was not stamped", id)
		}
	}
	fresh := r.seed("INV-SH-BRK-FRESH")
	if _, err := r.chargeAs(r.ctx, autoCharge, fresh.ID, "pm_card_1"); err != nil {
		t.Fatalf("a normal charge after the failing checks: %v (the breaker must not have opened)", err)
	}
}

// 14. A settle that needed no charge (a credit note took the invoice to zero,
// and the zero-due heal marked it paid) while the hold's lost charge had in
// fact succeeded: the sweep flags the second payment as a duplicate.
func TestStaleHold_SettledWithoutACharge_FoundPaymentFlagged(t *testing.T) {
	r := newSHRig(t, "SH Settled No Charge")
	inv := r.seed("INV-SH-ZERO")
	charged := r.crashAfterCharge(inv, autoCharge, "")
	if _, err := r.invoices.MarkPaid(r.ctx, r.tenantID, inv.ID, "", time.Now().UTC()); err != nil {
		t.Fatalf("settle without a charge: %v", err)
	}
	r.ageHold(inv.ID, staleSweepAge)
	r.runClean()
	if n := r.events(domain.EventPaymentDuplicateCharge, inv.ID); n != 1 {
		t.Errorf("payment.duplicate_charge rows = %d, want 1 for the found payment %s", n, charged.id)
	}
	if got := r.get(inv.ID); got.PaymentAnomalyKind != domain.EventPaymentDuplicateCharge {
		t.Errorf("anomaly marker = %q, want %q", got.PaymentAnomalyKind, domain.EventPaymentDuplicateCharge)
	}
	if r.hold(inv.ID) != nil {
		t.Error("hold kept after the found payment was flagged")
	}
}

// 15. No Stripe customer on file: no charge can have been sent, so the sweep
// clears the hold without asking Stripe.
func TestStaleHold_NoStripeCustomer_ClearedWithoutAStripeCall(t *testing.T) {
	r := newSHRig(t, "SH No Stripe Customer")
	inv := r.seedFor("INV-SH-NOCUS", false)
	if c, err := r.claim(autoCharge, inv.ID); err != nil || !c.ok() {
		t.Fatalf("claim: %v", err)
	}
	r.ageHold(inv.ID, staleSweepAge)
	r.runClean()
	got := r.get(inv.ID)
	if r.hold(inv.ID) != nil || got.ChargeAttemptSeq != inv.ChargeAttemptSeq+1 {
		t.Fatalf("after the sweep: hold %v seq %d, want cleared with seq %d", r.hold(inv.ID), got.ChargeAttemptSeq, inv.ChargeAttemptSeq+1)
	}
	if n := r.fake.listCount(); n != 0 {
		t.Errorf("Stripe lists = %d, want 0: with no Stripe customer there is nothing to ask", n)
	}
}

// 16. Concurrency, on real Postgres with goroutines.
func TestStaleHold_Concurrency(t *testing.T) {
	t.Run("every initiator races a stale-held invoice: none charges", func(t *testing.T) {
		r := newSHRig(t, "SH Race Claims")
		inv := r.seed("INV-SH-RACE-CLAIMS")
		r.crashAfterCharge(inv, autoCharge, "")
		r.ageHold(inv.ID, staleSweepAge)

		var wg sync.WaitGroup
		start := make(chan struct{})
		kinds := []chargeKind{autoCharge, dunningRetry, collect, autoCharge, dunningRetry, collect}
		errs := make([]error, len(kinds)+2)
		for i, k := range kinds {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, errs[i] = r.chargeAs(r.ctx, k, inv.ID, "pm_card_changed")
			}()
		}
		for i := len(kinds); i < len(errs); i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, _, errs[i] = r.checkout.ClaimOpen(r.ctx, r.tenantID, inv.ID, shDue, "USD", false)
			}()
		}
		close(start)
		wg.Wait()
		for i, err := range errs {
			if i < len(kinds) && !errors.Is(err, errNotClaimed) {
				t.Errorf("%s: err = %v, want its claim refused", kinds[i], err)
			}
			if i >= len(kinds) && !errors.Is(err, ErrChargeInFlight) {
				t.Errorf("hosted claim: err = %v, want ErrChargeInFlight", err)
			}
		}
		if n := r.fake.countPIs(); n != 1 {
			t.Fatalf("PaymentIntents at Stripe = %d, want 1", n)
		}
	})

	t.Run("the sweep's clear races the claims: exactly one charge, after the clear", func(t *testing.T) {
		r := newSHRig(t, "SH Race Clear")
		inv := r.seed("INV-SH-RACE-CLEAR")
		r.fake.queue(fakeMode{kind: fakeCrashBeforeSend})
		crash(func() { _, _ = r.chargeAs(r.ctx, autoCharge, inv.ID, "pm_card_1") })
		r.ageHold(inv.ID, staleSweepAge)

		var wg sync.WaitGroup
		start := make(chan struct{})
		var runErrs []error
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, runErrs = r.rec.Run(r.ctx, 50)
		}()
		for _, k := range []chargeKind{autoCharge, collect, autoCharge, collect} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, _ = r.chargeAs(r.ctx, k, inv.ID, "pm_card_1")
			}()
		}
		close(start)
		wg.Wait()
		if len(runErrs) > 0 {
			t.Fatalf("reconciler: %v", runErrs)
		}
		// Whoever lost the race tries once more, as the next tick would.
		if r.get(inv.ID).PaymentStatus != domain.PaymentSucceeded {
			if _, err := r.chargeAs(r.ctx, autoCharge, inv.ID, "pm_card_1"); err != nil {
				t.Fatalf("the charge after the clear: %v", err)
			}
		}
		pi := r.fake.onlyPI()
		if pi.id == "" {
			t.Fatalf("PaymentIntents at Stripe = %d, want exactly 1", r.fake.countPIs())
		}
		if want := ChargeIdempotencyKey(inv.ID, inv.ChargeAttemptSeq+1, ""); pi.key != want {
			t.Errorf("the one charge used key %q, want %q (sent only after the clear moved the counter)", pi.key, want)
		}
		r.assertPaidOnce(inv.ID, pi.id)
	})

	t.Run("a webhook settles the payment during the sweep's list: one paid transition", func(t *testing.T) {
		r := newSHRig(t, "SH Race Webhook")
		inv := r.seed("INV-SH-RACE-WEBHOOK")
		charged := r.crashAfterCharge(inv, autoCharge, "")
		var webhookErr error
		var wg sync.WaitGroup
		r.fake.beforeList = func() {
			wg.Add(1)
			go func() {
				defer wg.Done()
				amt := shDue
				webhookErr = r.stripe.HandleWebhook(r.ctx, r.tenantID, domain.StripeWebhookEvent{
					StripeEventID: "evt_" + charged.id, EventType: "payment_intent.succeeded", ObjectType: "payment_intent",
					InvoiceID: inv.ID, PaymentIntentID: charged.id, PaymentStatus: "succeeded",
					AmountCents: &amt, Currency: "usd", Payload: map[string]any{},
					ReceivedAt: time.Now().UTC(), OccurredAt: time.Now().UTC(),
				})
			}()
		}
		r.ageHold(inv.ID, staleSweepAge)
		_ = r.run()
		wg.Wait()
		if webhookErr != nil {
			t.Fatalf("webhook: %v", webhookErr)
		}
		r.fake.beforeList = nil
		if r.hold(inv.ID) != nil {
			r.ageHold(inv.ID, staleSweepAge) // the sweep's clear lost to the webhook's stamp; the next run clears
			r.runClean()
		}
		r.assertPaidOnce(inv.ID, charged.id)
	})

	t.Run("two sweeps on one invoice: one clear wins", func(t *testing.T) {
		r := newSHRig(t, "SH Race Sweeps")
		inv := r.seed("INV-SH-RACE-SWEEPS")
		r.fake.queue(fakeMode{kind: fakeCrashBeforeSend})
		crash(func() { _, _ = r.chargeAs(r.ctx, autoCharge, inv.ID, "pm_card_1") })
		r.ageHold(inv.ID, staleSweepAge)
		other := NewReconciler(r.fake, r.invoices, time.Minute)
		other.SetSettler(r.stripe)

		var wg sync.WaitGroup
		start := make(chan struct{})
		for _, rec := range []*Reconciler{r.rec, other} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, _ = rec.Run(r.ctx, 50)
			}()
		}
		close(start)
		wg.Wait()
		got := r.get(inv.ID)
		if r.hold(inv.ID) != nil || got.ChargeAttemptSeq != inv.ChargeAttemptSeq+1 {
			t.Fatalf("after two sweeps: hold %v seq %d, want cleared once (seq %d, not moved twice)",
				r.hold(inv.ID), got.ChargeAttemptSeq, inv.ChargeAttemptSeq+1)
		}
	})
}

// staleHolds seeds n invoices, each with its own Stripe customer and a stale
// hold whose charge never reached Stripe, the i-th one ages[i] old (or
// staleSweepAge when ages is short).
func (r *shRig) staleHolds(prefix string, n int, ages ...time.Duration) []string {
	r.t.Helper()
	var ids []string
	for i := 0; i < n; i++ {
		inv := r.seed(fmt.Sprintf("%s-%02d", prefix, i))
		if c, err := r.claim(autoCharge, inv.ID); err != nil || !c.ok() {
			r.t.Fatalf("claim %d: %v", i, err)
		}
		age := staleSweepAge
		if i < len(ages) {
			age = ages[i]
		}
		r.ageHold(inv.ID, age)
		ids = append(ids, inv.ID)
	}
	return ids
}

func (r *shRig) holdsLeft(ids []string) int {
	r.t.Helper()
	n := 0
	for _, id := range ids {
		if r.hold(id) != nil {
			n++
		}
	}
	return n
}

// 17. One run drains every stale hold, page by page, each checked once: 45
// holds at a page of 20 are all cleared by one run. The sweep used to read
// one page per tick, so a hold waited an extra hour for every page queued
// ahead of it, and the refusal's "within about an hour" was not a bound.
func TestStaleHold_OneRunDrainsEveryPage(t *testing.T) {
	r := newSHRig(t, "SH Drain")
	ids := r.staleHolds("INV-SH-DRAIN", 45)
	if _, errs := r.rec.Run(r.ctx, 20); len(errs) > 0 {
		t.Fatalf("reconciler: %v", errs)
	}
	if left := r.holdsLeft(ids); left != 0 {
		t.Fatalf("%d of %d stale holds left after one run at a page of 20: the rest would wait an hour a page", left, len(ids))
	}
	if n := r.fake.listCount(); n != len(ids) {
		t.Errorf("Stripe lists = %d, want %d: one per hold, none checked twice", n, len(ids))
	}
}

// 17b. A run is bounded in time. Once its budget is spent the rest wait for
// the next tick, and the next tick reaches them: the auto-charge sweep and the
// billing cycle run after this sweep in the same tick.
func TestStaleHold_RunStopsAtItsBudget_RestNextTick(t *testing.T) {
	r := newSHRig(t, "SH Budget")
	ids := r.staleHolds("INV-SH-BUDGET", 10)
	r.fake.beforeList = func() { time.Sleep(150 * time.Millisecond) }
	setStaleHoldBudget(r.rec, 400*time.Millisecond)
	r.runClean()
	cleared := len(ids) - r.holdsLeft(ids)
	if cleared == 0 || cleared > 4 {
		t.Fatalf("first run cleared %d of %d holds with a 400ms budget at 150ms a check, want 1 to 4", cleared, len(ids))
	}
	setStaleHoldBudget(r.rec, time.Minute)
	r.runClean()
	if left := r.holdsLeft(ids); left != 0 {
		t.Fatalf("%d holds left after the next run: the ones the budget cut were not reached", left)
	}
}

// 18. Stripe cannot be reached. Each check's Stripe read is cut at its time
// limit, and after three checks in a row get no answer the sweep stops for
// this mode until the next tick: it runs first in the billing tick, and
// stripe-go alone could take about 90 s a page (30 s a request, two resends).
// The holds it did not reach keep their place. An answer starts the count
// again, and a 5xx is an answer: it comes back at once.
func TestStaleHold_StripeUnreachable_ChecksBoundedAndSweepStops(t *testing.T) {
	t.Run("a partition: three reads, each cut at its limit, then the sweep stops", func(t *testing.T) {
		r := newSHRig(t, "SH Partition")
		ids := r.staleHolds("INV-SH-PART", 5, 60*time.Minute, 55*time.Minute, 50*time.Minute, 45*time.Minute, 40*time.Minute)
		r.fake.mu.Lock()
		r.fake.hang = 3 * time.Second
		r.fake.mu.Unlock()
		setStaleHoldCheckTimeout(r.rec, 200*time.Millisecond)

		start := time.Now()
		_ = r.run()
		elapsed := time.Since(start)

		if n := r.fake.listCount(); n != 3 {
			t.Errorf("Stripe lists = %d, want 3: the sweep stops after three failed reads in a row", n)
		}
		if elapsed > 2*time.Second {
			t.Errorf("the run took %v: each read must be cut at its time limit", elapsed)
		}
		if left := r.holdsLeft(ids); left != len(ids) {
			t.Fatalf("%d holds cleared while Stripe could not be read", len(ids)-left)
		}
		for i, id := range ids {
			stamped := r.get(id).ProviderSyncedAt != nil
			if want := i < 3; stamped != want {
				t.Errorf("hold %d: checked-stamp %v, want %v (the holds not reached keep their place)", i, stamped, want)
			}
		}
	})

	t.Run("an answer starts the count again", func(t *testing.T) {
		r := newSHRig(t, "SH Partition Reset")
		ids := r.staleHolds("INV-SH-RESET", 8,
			60*time.Minute, 55*time.Minute, 50*time.Minute, 45*time.Minute, 40*time.Minute, 35*time.Minute, 30*time.Minute, 25*time.Minute)
		const good, page500 = 2, 3 // Stripe answers: nothing there; a 500
		for i, id := range ids {
			switch i {
			case good:
			case page500:
				r.fake.setCustomer(stripeCustomerOf(r.get(id)), "fail")
			default:
				r.fake.setCustomer(stripeCustomerOf(r.get(id)), "unreachable")
			}
		}
		_ = r.run()
		if n := r.fake.listCount(); n != 7 {
			t.Errorf("Stripe lists = %d, want 7: two without an answer, an answer, a 500, then three in a row without an answer stop the sweep", n)
		}
		if r.hold(ids[good]) != nil {
			t.Error("the hold Stripe answered about was not cleared")
		}
		if r.get(ids[7]).ProviderSyncedAt != nil {
			t.Error("the hold after the stop was checked")
		}
	})
}

// 19. A processing payment the invoice does not follow keeps the hold until it
// ends. Clearing the hold dropped the only thing still watching that payment:
// if it later succeeded and its webhook was lost, nothing recorded the money.
func TestStaleHold_ProcessingTheInvoiceDoesNotFollow_KeptUntilItEnds(t *testing.T) {
	t.Run("on a written-off invoice: kept, then recorded once it succeeds", func(t *testing.T) {
		r := newSHRig(t, "SH Processing Written Off")
		inv := r.seed("INV-SH-PROC-WO")
		pi := r.crashAfterCharge(inv, autoCharge, "processing")
		r.ageHold(inv.ID, staleSweepAge)
		if _, err := r.invSvc.MarkUncollectible(r.ctx, r.tenantID, inv.ID); err != nil {
			t.Fatalf("write-off on a stale hold: %v", err)
		}
		r.runClean()
		if r.hold(inv.ID) == nil {
			t.Fatal("hold cleared while a payment the invoice does not follow is still processing at Stripe")
		}

		r.fake.setStatus(pi.id, "succeeded") // days later; its webhook is lost
		r.ageHold(inv.ID, staleSweepAge)
		r.runClean()
		got := r.get(inv.ID)
		if got.Status != domain.InvoicePaid || got.StripePaymentIntentID != pi.id {
			t.Fatalf("after it succeeded: %s with PI %q, want paid with %q (uncollectible → paid)", got.Status, got.StripePaymentIntentID, pi.id)
		}
		if r.hold(inv.ID) != nil {
			t.Error("hold kept after the payment was recorded")
		}
	})

	t.Run("next to a succeeded payment: flagged as a duplicate once it succeeds", func(t *testing.T) {
		r := newSHRig(t, "SH Processing Next To Succeeded")
		inv := r.seed("INV-SH-PROC-DUP")
		first := r.crashAfterCharge(inv, autoCharge, "")
		second := r.fake.addPI(stripeCustomerOf(inv), inv.ID, "", "processing", shDue)
		r.ageHold(inv.ID, staleSweepAge)
		r.runClean()
		if got := r.get(inv.ID); got.Status != domain.InvoicePaid || got.StripePaymentIntentID != first.id {
			t.Fatalf("after the sweep: %s with PI %q, want paid with %q", got.Status, got.StripePaymentIntentID, first.id)
		}
		if r.hold(inv.ID) == nil {
			t.Fatal("hold cleared while a second payment is still processing at Stripe")
		}

		r.fake.setStatus(second.id, "succeeded")
		r.ageHold(inv.ID, staleSweepAge)
		r.runClean()
		if n := r.events(domain.EventPaymentDuplicateCharge, inv.ID); n != 1 {
			t.Errorf("payment.duplicate_charge rows = %d, want 1 for %s", n, second.id)
		}
		if n := r.events(domain.EventPaymentSucceeded, inv.ID); n != 1 {
			t.Errorf("payment.succeeded rows = %d, want 1", n)
		}
		if r.hold(inv.ID) != nil {
			t.Error("hold kept after both payments were recorded")
		}
	})

	t.Run("the invoice's own processing payment: cleared (control)", func(t *testing.T) {
		r := newSHRig(t, "SH Processing Own")
		inv := r.seed("INV-SH-PROC-OWN")
		r.fake.queue(fakeMode{kind: fakeCreate, status: "processing"})
		if _, err := r.chargeAs(r.ctx, autoCharge, inv.ID, "pm_card_1"); err != nil {
			t.Fatalf("charge: %v", err)
		}
		pi := r.fake.onlyPI()
		if got := r.get(inv.ID); got.PaymentStatus != domain.PaymentProcessing || got.StripePaymentIntentID != pi.id {
			t.Fatalf("setup: %s with PI %q, want processing with %q", got.PaymentStatus, got.StripePaymentIntentID, pi.id)
		}
		r.ageHold(inv.ID, staleSweepAge)
		r.runClean()
		if r.hold(inv.ID) != nil {
			t.Error("hold kept although the invoice follows its processing payment (the processing sweep and the webhook do)")
		}
	})
}
