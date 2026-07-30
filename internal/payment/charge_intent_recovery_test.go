package payment

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
)

// idempotentStripe models the ONE Stripe behaviour this whole design rests on:
// "Stripe's idempotency works by saving the resulting status code and body of
// the first request made for any given idempotency key... Subsequent requests
// with the same key return the same result."
//
// A fake that simply returns a fresh PaymentIntent per call would let the
// double-charge tests below pass against a design that double-charges. So this
// keeps a key→PaymentIntent cache and mints a NEW id only for a key it has
// never seen — exactly the property recovery depends on.
type idempotentStripe struct {
	mu       sync.Mutex
	cache    map[string]string // idempotency key → PaymentIntent id
	minted   []string          // every id ever created, in order
	created  int
	failNext error // when set, the next create returns this instead
	// dropResponse simulates the crash/timeout shape: Stripe DID create the
	// PaymentIntent and saved the result, but the caller never saw it.
	dropResponse bool
	// onCreate fires INSIDE the call, modelling a webhook that settles the
	// invoice while the request is in flight — the race the step-6 re-read
	// exists to survive.
	onCreate func()
}

func newIdempotentStripe() *idempotentStripe {
	return &idempotentStripe{cache: map[string]string{}}
}

func (f *idempotentStripe) CreatePaymentIntent(_ context.Context, p PaymentIntentParams) (PaymentIntentResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failNext != nil {
		err := f.failNext
		f.failNext = nil
		return PaymentIntentResult{}, err
	}
	if f.onCreate != nil {
		hook := f.onCreate
		f.onCreate = nil
		f.mu.Unlock()
		hook()
		f.mu.Lock()
	}
	id, seen := f.cache[p.IdempotencyKey]
	if !seen {
		f.created++
		id = "pi_" + p.IdempotencyKey
		f.cache[p.IdempotencyKey] = id
		f.minted = append(f.minted, id)
	}
	if f.dropResponse {
		f.dropResponse = false
		// The PaymentIntent exists and the result is saved under the key; the
		// caller just never learns its id.
		return PaymentIntentResult{}, &PaymentError{Message: "connection reset", Unknown: true}
	}
	return PaymentIntentResult{ID: id, Status: "succeeded"}, nil
}

func (f *idempotentStripe) GetPaymentIntent(_ context.Context, id string) (PaymentIntentResult, error) {
	return PaymentIntentResult{ID: id, Status: "succeeded"}, nil
}

func (f *idempotentStripe) CancelPaymentIntent(_ context.Context, id string) error { return nil }

func (f *idempotentStripe) mintedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.created
}

// TestChargeIntent_AmbiguousOutcomeRecoversToTheSamePaymentIntent is the whole
// point of ADR-106, end to end.
//
// An attempt reaches Stripe, Stripe creates the PaymentIntent, and the response
// is lost — the invoice ends with payment_status='unknown' and NO PaymentIntent
// id, which is the state that used to be unrecoverable. The recovery sweep then
// replays the stored key and must be handed back the ORIGINAL PaymentIntent.
//
// The assertion that matters is Stripe's create count: exactly ONE
// PaymentIntent may exist for this invoice after recovery. Two means the
// customer was charged twice.
func TestChargeIntent_AmbiguousOutcomeRecoversToTheSamePaymentIntent(t *testing.T) {
	client := newIdempotentStripe()
	invoices := newMockInvoiceUpdater()
	invoices.invoices["inv_1"] = finalizedPendingInvoice()
	ledger := newMemChargeIntents()

	s := NewStripe(client, invoices, newMockWebhookStore(), nil)
	s.SetChargeIntents(ledger)

	// The charge reaches Stripe; the response is lost.
	client.dropResponse = true
	if _, err := s.ChargeInvoice(context.Background(), "t1", invoices.invoices["inv_1"], "cus_x", "pm_x"); err == nil {
		t.Fatal("charge with a dropped response must return an error")
	}
	if got := client.mintedCount(); got != 1 {
		t.Fatalf("Stripe created %d PaymentIntents on the first attempt, want 1", got)
	}

	// The invoice is in the unrecoverable-before-ADR-106 state.
	inv := invoices.invoices["inv_1"]
	if inv.StripePaymentIntentID != "" {
		t.Fatalf("precondition: the lost response must leave no PI id, got %q", inv.StripePaymentIntentID)
	}
	if inv.PaymentStatus != domain.PaymentUnknown {
		t.Fatalf("precondition: want payment_status=unknown, got %s", inv.PaymentStatus)
	}

	// The intent stayed OPEN — that omission is the recovery hook.
	open := ledger.all()
	if len(open) != 1 || open[0].State != domain.ChargeIntentOpen {
		t.Fatalf("want exactly one OPEN charge intent after a lost response, got %+v", open)
	}
	if open[0].IdempotencyKey == "" {
		t.Fatal("the intent stored no idempotency key — nothing could be replayed")
	}

	// Age it past the cool-off and recover.
	ledger.rows[open[0].ID].UpdatedAt = time.Now().UTC().Add(-2 * chargeIntentCoolOff)
	store := &recoveryInvoiceStore{mock: invoices}
	rec := NewReconciler(client, store, time.Second)
	rec.SetChargeIntents(ledger)

	n, errs := rec.RecoverChargeIntents(context.Background(), 10)
	if len(errs) > 0 {
		t.Fatalf("recovery errors: %v", errs)
	}
	if n != 1 {
		t.Fatalf("recovered %d intents, want 1", n)
	}

	// THE assertion: still exactly one PaymentIntent in existence.
	if got := client.mintedCount(); got != 1 {
		t.Fatalf("Stripe holds %d PaymentIntents after recovery, want 1 — the customer was charged twice", got)
	}
	if got := invoices.invoices["inv_1"].StripePaymentIntentID; got != client.minted[0] {
		t.Fatalf("invoice names %q, want the original PaymentIntent %q", got, client.minted[0])
	}
	if st := ledger.get(open[0].ID).State; st != domain.ChargeIntentResolved {
		t.Fatalf("intent state after recovery = %s, want resolved", st)
	}
	// A recovered attempt is as real as an inline one: without the ADR-102 fact
	// it is missing from the invoice timeline entirely, and ADR-106 claimed this
	// happened when it did not.
	if len(store.attempts) != 1 {
		t.Fatalf("recovery recorded %d charge-attempt facts, want 1 — the attempt would be invisible on the timeline", len(store.attempts))
	}
	if store.attempts[0].StripePaymentIntentID != client.minted[0] {
		t.Fatalf("the recorded fact names %q, want the recovered PaymentIntent %q", store.attempts[0].StripePaymentIntentID, client.minted[0])
	}
}

// TestChargeIntent_BlocksASecondChargeWhileAnAttemptIsUnresolved covers the
// other half of the guarantee. Recovery names a lost PaymentIntent eventually;
// until then nothing may open a second one — not the sweep, not an operator,
// not a card swap that changes the idempotency key.
func TestChargeIntent_BlocksASecondChargeWhileAnAttemptIsUnresolved(t *testing.T) {
	client := newIdempotentStripe()
	invoices := newMockInvoiceUpdater()
	invoices.invoices["inv_1"] = finalizedPendingInvoice()
	ledger := newMemChargeIntents()

	s := NewStripe(client, invoices, newMockWebhookStore(), nil)
	s.SetChargeIntents(ledger)

	client.dropResponse = true
	_, _ = s.ChargeInvoice(context.Background(), "t1", invoices.invoices["inv_1"], "cus_x", "pm_x")
	if got := client.mintedCount(); got != 1 {
		t.Fatalf("setup: want 1 PaymentIntent, got %d", got)
	}

	// The customer swaps cards, which changes the idempotency key. Pre-ADR-106
	// this sailed through and opened a second PaymentIntent on the new card
	// while the first was still live.
	inv := invoices.invoices["inv_1"]
	inv.PaymentStatus = domain.PaymentPending // as a retry would find it
	_, err := s.ChargeInvoice(context.Background(), "t1", inv, "cus_x", "pm_DIFFERENT")
	if !errors.Is(err, ErrPaymentTransient) {
		t.Fatalf("a charge under a different key while an attempt is unresolved must be deferred, got %v", err)
	}
	if got := client.mintedCount(); got != 1 {
		t.Fatalf("Stripe holds %d PaymentIntents, want 1 — the swapped card opened a second charge", got)
	}
}

// TestChargeIntent_FailClosedWhenTheAttemptCannotBeRecorded pins the ordering
// rule itself: if the ledger write fails, no Stripe call may happen. An
// unrecorded attempt is precisely what this design makes impossible, so a
// degraded database must stop charges rather than make invisible ones.
func TestChargeIntent_FailClosedWhenTheAttemptCannotBeRecorded(t *testing.T) {
	client := newIdempotentStripe()
	invoices := newMockInvoiceUpdater()
	invoices.invoices["inv_1"] = finalizedPendingInvoice()
	ledger := newMemChargeIntents()
	ledger.openErr = errors.New("database unavailable")

	s := NewStripe(client, invoices, newMockWebhookStore(), nil)
	s.SetChargeIntents(ledger)

	_, err := s.ChargeInvoice(context.Background(), "t1", invoices.invoices["inv_1"], "cus_x", "pm_x")
	if err == nil {
		t.Fatal("a charge whose attempt could not be recorded must fail, not proceed")
	}
	// Assert WHICH guard stopped it. Without this the test passes even when the
	// fail-closed branch is removed, because the zero-value intent then trips
	// the key-mismatch guard instead — right outcome, wrong reason, and no
	// coverage of the rule that actually matters.
	if !errors.Is(err, ledger.openErr) {
		t.Fatalf("want the ledger error surfaced (proving the fail-closed branch ran), got %v", err)
	}
	if got := client.mintedCount(); got != 0 {
		t.Fatalf("Stripe was called %d times despite the attempt being unrecordable — the fail-closed rule is broken", got)
	}
}

// TestChargeIntent_ReconcilerDefersToAnUnresolvedIntent is the COUNTERPART
// half, and it is not optional: recovery without this gate is pointless,
// because payment_unknown re-opens the hole a tick later.
//
// The give-up write (settle failed on an invoice with no PI id) is the single
// thing that un-quarantines the invoice and bumps charge_attempt_seq, handing
// the next retry a key Stripe has never seen — a second PaymentIntent beside
// one that may be live. So while an attempt is unresolved, that write must not
// happen.
func TestChargeIntent_ReconcilerDefersToAnUnresolvedIntent(t *testing.T) {
	invoices := newMockInvoiceUpdater()
	inv := finalizedPendingInvoice()
	inv.PaymentStatus = domain.PaymentUnknown
	inv.StripePaymentIntentID = "" // the unrecoverable-before-ADR-106 shape
	invoices.invoices["inv_1"] = inv

	ledger := newMemChargeIntents()
	if _, _, err := ledger.Open(context.Background(), domain.ChargeIntent{
		TenantID: "t1", InvoiceID: "inv_1", IdempotencyKey: "velox_inv_inv_1_0_pm_x",
		AmountCents: inv.AmountDueCents, Currency: "USD",
		StripeCustomerID: "cus_x", StripePaymentMethodID: "pm_x",
	}); err != nil {
		t.Fatalf("seed intent: %v", err)
	}

	rec := NewReconciler(newIdempotentStripe(), &recoveryInvoiceStore{mock: invoices}, time.Second)
	rec.SetChargeIntents(ledger)

	changed, err := rec.reconcileOne(context.Background(), inv)
	if err != nil {
		t.Fatalf("reconcileOne: %v", err)
	}
	if changed {
		t.Fatal("reconcileOne settled an invoice whose attempt is still unresolved — that write frees a retry to open a SECOND PaymentIntent")
	}
	if got := invoices.invoices["inv_1"].PaymentStatus; got != domain.PaymentUnknown {
		t.Fatalf("payment_status moved to %s while an attempt was unresolved, want it left at unknown", got)
	}

	// Negative control: with NO unresolved intent the legacy give-up must still
	// fire, or pre-ADR-106 invoices would hang forever.
	empty := newMemChargeIntents()
	rec2 := NewReconciler(newIdempotentStripe(), &recoveryInvoiceStore{mock: invoices}, time.Second)
	rec2.SetChargeIntents(empty)
	if _, err := rec2.reconcileOne(context.Background(), inv); err != nil {
		t.Fatalf("legacy path: %v", err)
	}
	if got := invoices.invoices["inv_1"].PaymentStatus; got != domain.PaymentFailed {
		t.Fatalf("with no intent recorded the reconciler must still settle failed, got %s", got)
	}
}

// TestChargeIntent_ResolvedOnASuccessfulCharge is the negative control for the
// blocking test above: the quarantine must LIFT on a normal charge, or every
// invoice would be chargeable exactly once, forever.
func TestChargeIntent_ResolvedOnASuccessfulCharge(t *testing.T) {
	client := newIdempotentStripe()
	invoices := newMockInvoiceUpdater()
	invoices.invoices["inv_1"] = finalizedPendingInvoice()
	ledger := newMemChargeIntents()

	s := NewStripe(client, invoices, newMockWebhookStore(), nil)
	s.SetChargeIntents(ledger)

	if _, err := s.ChargeInvoice(context.Background(), "t1", invoices.invoices["inv_1"], "cus_x", "pm_x"); err != nil {
		t.Fatalf("charge: %v", err)
	}
	rows := ledger.all()
	if len(rows) != 1 || rows[0].State != domain.ChargeIntentResolved {
		t.Fatalf("a successful charge must close its intent, got %+v", rows)
	}
	if rows[0].StripePaymentIntentID == "" {
		t.Fatal("the closed intent must name the PaymentIntent it produced")
	}
	unresolved, _ := ledger.HasUnresolved(context.Background(), "t1", "inv_1")
	if unresolved {
		t.Fatal("a settled invoice must not stay quarantined")
	}
}

// TestChargeIntent_SimAnchorRecorded: an attempt made under a test clock stores
// its billing-axis instant, so a wall-plane recovery can still stamp the
// contracted instant rather than the executor's now (ADR-030).
func TestChargeIntent_SimAnchorRecorded(t *testing.T) {
	client := newIdempotentStripe()
	invoices := newMockInvoiceUpdater()
	invoices.invoices["inv_1"] = finalizedPendingInvoice()
	ledger := newMemChargeIntents()

	s := NewStripe(client, invoices, newMockWebhookStore(), nil)
	s.SetChargeIntents(ledger)

	simAt := time.Date(2027, 3, 7, 12, 0, 0, 0, time.UTC)
	ctx := clock.WithSim(context.Background(), clock.Sim{At: simAt, TestClockID: "clk_1"})
	if _, err := s.ChargeInvoice(ctx, "t1", invoices.invoices["inv_1"], "cus_x", "pm_x"); err != nil {
		t.Fatalf("charge: %v", err)
	}
	rows := ledger.all()
	if len(rows) != 1 || rows[0].SimEffectiveAt == nil {
		t.Fatalf("a clock-pinned attempt must record its simulated instant, got %+v", rows)
	}
	if !rows[0].SimEffectiveAt.Equal(simAt) {
		t.Fatalf("sim_effective_at = %v, want %v", rows[0].SimEffectiveAt, simAt)
	}
}

// TestChargeIntent_RefusesToReplayAnExpiredKey guards the most dangerous number
// in ADR-106. Stripe retains idempotency keys for "at least 24 hours"; past
// that, replaying does NOT return the original PaymentIntent, it creates a
// second one — the exact bug this design exists to prevent. So an attempt older
// than the replay TTL must be quarantined WITHOUT calling Stripe, and must keep
// blocking the invoice.
func TestChargeIntent_RefusesToReplayAnExpiredKey(t *testing.T) {
	client := newIdempotentStripe()
	invoices := newMockInvoiceUpdater()
	inv := finalizedPendingInvoice()
	inv.PaymentStatus = domain.PaymentUnknown
	invoices.invoices["inv_1"] = inv

	ledger := newMemChargeIntents()
	seeded, _, err := ledger.Open(context.Background(), domain.ChargeIntent{
		TenantID: "t1", InvoiceID: "inv_1", IdempotencyKey: "velox_inv_inv_1_0_pm_x",
		AmountCents: inv.AmountDueCents, Currency: "USD",
		StripeCustomerID: "cus_x", StripePaymentMethodID: "pm_x",
		OccurredAt: time.Now().UTC().Add(-chargeIntentReplayTTL - time.Hour),
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	ledger.rows[seeded.ID].UpdatedAt = time.Now().UTC().Add(-2 * chargeIntentCoolOff)

	rec := NewReconciler(client, &recoveryInvoiceStore{mock: invoices}, time.Second)
	rec.SetChargeIntents(ledger)
	if _, errs := rec.RecoverChargeIntents(context.Background(), 10); len(errs) > 0 {
		t.Fatalf("recovery errors: %v", errs)
	}

	if got := client.mintedCount(); got != 0 {
		t.Fatalf("Stripe was called %d times for an expired key — the replay would have created a SECOND PaymentIntent", got)
	}
	got := ledger.get(seeded.ID)
	if got.State != domain.ChargeIntentNeedsReview {
		t.Fatalf("expired intent state = %s, want needs_review", got.State)
	}
	// Quarantine must keep BLOCKING: "we do not know" is the last state in
	// which another charge would be safe.
	unresolved, _ := ledger.HasUnresolved(context.Background(), "t1", "inv_1")
	if !unresolved {
		t.Fatal("a needs_review intent must still block further charges")
	}
}

// TestChargeIntent_QuarantineBlocksTheChargePathUnderTheSAMEKey is the
// regression test for the worst defect the adversarial review found in this
// design (2026-07-30). The guard checked ONLY key equality — and the key
// recomputes identically in exactly the dangerous case, because quarantine
// means no outcome was recorded and therefore charge_attempt_seq never moved.
//
// So a quarantined attempt sailed straight through: the operator clicks Collect
// days later, the same key is re-sent, Stripe has long since expired it, and a
// SECOND PaymentIntent is created beside one that may have succeeded.
//
// The existing quarantine test missed this because it re-opened with a
// DIFFERENT key, exercising the one branch that already worked.
func TestChargeIntent_QuarantineBlocksTheChargePathUnderTheSAMEKey(t *testing.T) {
	client := newIdempotentStripe()
	invoices := newMockInvoiceUpdater()
	invoices.invoices["inv_1"] = finalizedPendingInvoice()
	ledger := newMemChargeIntents()

	s := NewStripe(client, invoices, newMockWebhookStore(), nil)
	s.SetChargeIntents(ledger)

	// An attempt reaches Stripe and its outcome is lost, then recovery gives up.
	client.dropResponse = true
	_, _ = s.ChargeInvoice(context.Background(), "t1", invoices.invoices["inv_1"], "cus_x", "pm_x")
	rows := ledger.all()
	if len(rows) != 1 {
		t.Fatalf("setup: want 1 intent, got %d", len(rows))
	}
	if err := ledger.MarkNeedsReview(context.Background(), "t1", rows[0].ID, "unresolvable"); err != nil {
		t.Fatalf("quarantine: %v", err)
	}
	before := client.mintedCount()

	// The operator retries. Nothing recorded an outcome, so the key is IDENTICAL
	// — same invoice, same seq, same payment method.
	inv := invoices.invoices["inv_1"]
	inv.PaymentStatus = domain.PaymentPending
	_, err := s.ChargeInvoice(context.Background(), "t1", inv, "cus_x", "pm_x")
	if err == nil {
		t.Fatal("charging over a QUARANTINED attempt must be refused")
	}
	if !errors.Is(err, ErrChargeAttemptInFlight) {
		t.Errorf("want ErrChargeAttemptInFlight so the operator gets a 409, got %v", err)
	}
	// And it must NOT be transient. Dunning maps ErrPaymentTransient to a skip
	// that REWINDS the attempt count, so a quarantined invoice would spin every
	// tick forever without ever exhausting — uncollectible, un-escalatable and
	// silent. Quarantine needs a human, which is the opposite of a wait.
	if errors.Is(err, ErrPaymentTransient) {
		t.Error("a QUARANTINED attempt was reported as a transient wait — dunning will rewind its attempt count every tick and never escalate")
	}
	if got := client.mintedCount(); got != before {
		t.Fatalf("Stripe was called over a quarantined attempt (%d -> %d) — with an expired key that creates a SECOND PaymentIntent", before, got)
	}
}

// TestChargeIntent_OpenIntentDefersAsATransientWait is the counterpart: while
// an attempt is genuinely OPEN, the deferral IS a wait, so it must stay
// transient or dunning burns a retry attempt on an invoice nobody has failed to
// charge yet.
func TestChargeIntent_OpenIntentDefersAsATransientWait(t *testing.T) {
	client := newIdempotentStripe()
	invoices := newMockInvoiceUpdater()
	invoices.invoices["inv_1"] = finalizedPendingInvoice()
	ledger := newMemChargeIntents()

	s := NewStripe(client, invoices, newMockWebhookStore(), nil)
	s.SetChargeIntents(ledger)

	client.dropResponse = true
	_, _ = s.ChargeInvoice(context.Background(), "t1", invoices.invoices["inv_1"], "cus_x", "pm_x")

	// A different card ⇒ a different key ⇒ deferral against an OPEN intent.
	inv := invoices.invoices["inv_1"]
	inv.PaymentStatus = domain.PaymentPending
	_, err := s.ChargeInvoice(context.Background(), "t1", inv, "cus_x", "pm_OTHER")
	if !errors.Is(err, ErrPaymentTransient) {
		t.Errorf("deferring behind an OPEN attempt must be transient so dunning does not burn a retry, got %v", err)
	}
	if !errors.Is(err, ErrChargeAttemptInFlight) {
		t.Errorf("want ErrChargeAttemptInFlight for an honest 409, got %v", err)
	}
}

// TestChargeIntent_DefiniteRejectionClosesTheIntent: a provider refusing the
// request outright is proof that no PaymentIntent was created, so the invoice
// must NOT be quarantined. Leaving it open bricked the invoice: the failure
// write rotates the key, the guard then refuses every later attempt, recovery
// reproduces the same rejection until it quarantines, and quarantine's only
// exit is adopting a PaymentIntent that will never exist.
func TestChargeIntent_DefiniteRejectionClosesTheIntent(t *testing.T) {
	client := newIdempotentStripe()
	client.failNext = &PaymentError{Message: "No such PaymentMethod: pm_x", Unknown: false}
	invoices := newMockInvoiceUpdater()
	invoices.invoices["inv_1"] = finalizedPendingInvoice()
	ledger := newMemChargeIntents()

	s := NewStripe(client, invoices, newMockWebhookStore(), nil)
	s.SetChargeIntents(ledger)

	if _, err := s.ChargeInvoice(context.Background(), "t1", invoices.invoices["inv_1"], "cus_x", "pm_x"); err == nil {
		t.Fatal("a rejected charge must return an error")
	}
	rows := ledger.all()
	if len(rows) != 1 {
		t.Fatalf("want 1 intent, got %d", len(rows))
	}
	if rows[0].State != domain.ChargeIntentResolved {
		t.Fatalf("a definite rejection left the intent %s — nothing was created, so quarantining protects against nothing and the invoice becomes permanently unchargeable", rows[0].State)
	}
	unresolved, _ := ledger.HasUnresolved(context.Background(), "t1", "inv_1")
	if unresolved {
		t.Fatal("the invoice is still blocked after a rejection that created no PaymentIntent")
	}
}

// TestChargeIntent_StaleOpenIntentIsNotReSent: past the replay window the same
// key no longer dedups at Stripe, so re-sending it CREATES a second
// PaymentIntent. The TTL guarded only the recovery sweep; this path runs far
// more often and had none.
func TestChargeIntent_StaleOpenIntentIsNotReSent(t *testing.T) {
	client := newIdempotentStripe()
	invoices := newMockInvoiceUpdater()
	invoices.invoices["inv_1"] = finalizedPendingInvoice()
	ledger := newMemChargeIntents()

	s := NewStripe(client, invoices, newMockWebhookStore(), nil)
	s.SetChargeIntents(ledger)

	client.dropResponse = true
	_, _ = s.ChargeInvoice(context.Background(), "t1", invoices.invoices["inv_1"], "cus_x", "pm_x")
	rows := ledger.all()
	ledger.rows[rows[0].ID].OccurredAt = time.Now().UTC().Add(-chargeIntentReplayTTL - time.Hour)
	before := client.mintedCount()

	inv := invoices.invoices["inv_1"]
	inv.PaymentStatus = domain.PaymentPending
	if _, err := s.ChargeInvoice(context.Background(), "t1", inv, "cus_x", "pm_x"); err == nil {
		t.Fatal("re-sending a key past the replay window must be refused")
	}
	if got := client.mintedCount(); got != before {
		t.Fatalf("an expired key was re-sent (%d -> %d PaymentIntents)", before, got)
	}
}

// TestChargeIntent_BreakerSkipOnlyDeletesOurOwnRow: two callers computing the
// same key are handed the SAME row. A caller whose breaker opens must not erase
// a marker it merely inherited — that is another caller's in-flight attempt, and
// deleting it re-opens the double charge.
func TestChargeIntent_BreakerSkipOnlyDeletesOurOwnRow(t *testing.T) {
	ledger := newMemChargeIntents()
	first, created, err := ledger.Open(context.Background(), domain.ChargeIntent{
		TenantID: "t1", InvoiceID: "inv_1", IdempotencyKey: "k", AmountCents: 100,
	})
	if err != nil || !created {
		t.Fatalf("first Open must create: created=%v err=%v", created, err)
	}
	second, created2, err := ledger.Open(context.Background(), domain.ChargeIntent{
		TenantID: "t1", InvoiceID: "inv_1", IdempotencyKey: "k", AmountCents: 100,
	})
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	if created2 {
		t.Fatal("the second caller must NOT be told it created the row — it would then delete another attempt's marker on a breaker skip")
	}
	if second.ID != first.ID {
		t.Fatalf("both callers must be handed the same row, got %s and %s", first.ID, second.ID)
	}
}

// TestChargeIntent_RecoveryDoesNotClobberAnInvoiceSettledMidReplay: the replay
// is a network round-trip and a webhook can settle the invoice inside it.
// UpdatePayment writes paid_at unconditionally, so stamping 'processing' with
// paidAt=nil over a just-paid invoice would NULL its paid_at and walk a settled
// invoice backwards into in-flight.
func TestChargeIntent_RecoveryDoesNotClobberAnInvoiceSettledMidReplay(t *testing.T) {
	client := newIdempotentStripe()
	invoices := newMockInvoiceUpdater()
	invoices.invoices["inv_1"] = finalizedPendingInvoice()

	ledger := newMemChargeIntents()
	seeded, _, err := ledger.Open(context.Background(), domain.ChargeIntent{
		TenantID: "t1", InvoiceID: "inv_1", IdempotencyKey: "velox_inv_inv_1_0_pm_x",
		AmountCents: invoices.invoices["inv_1"].AmountDueCents, Currency: "USD",
		StripeCustomerID: "cus_x", StripePaymentMethodID: "pm_x",
		OccurredAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	ledger.rows[seeded.ID].UpdatedAt = time.Now().UTC().Add(-2 * chargeIntentCoolOff)

	// The webhook lands INSIDE the replay round-trip — after the sweep's
	// pre-checks passed, before it stamps.
	paidAt := time.Now().UTC()
	client.onCreate = func() {
		inv := invoices.invoices["inv_1"]
		inv.PaymentStatus = domain.PaymentSucceeded
		inv.Status = domain.InvoicePaid
		inv.PaidAt = &paidAt
		inv.StripePaymentIntentID = "pi_from_webhook"
		invoices.invoices["inv_1"] = inv
	}

	rec := NewReconciler(client, &recoveryInvoiceStore{mock: invoices}, time.Second)
	rec.SetChargeIntents(ledger)
	if _, errs := rec.RecoverChargeIntents(context.Background(), 10); len(errs) > 0 {
		t.Fatalf("recovery errors: %v", errs)
	}

	got := invoices.invoices["inv_1"]
	if got.PaidAt == nil {
		t.Fatal("recovery NULLed paid_at on an invoice settled during its replay")
	}
	if got.PaymentStatus != domain.PaymentSucceeded {
		t.Fatalf("a settled invoice was walked backwards to %s", got.PaymentStatus)
	}
	if st := ledger.get(seeded.ID).State; st != domain.ChargeIntentResolved {
		t.Fatalf("the intent must close when the invoice settled mid-replay, got %s", st)
	}
}

// TestChargeIntent_QuarantineRegainsAnExitAndIsNotReplayed covers fix 2, which
// until now had NO executing coverage: the in-memory ledger still filtered
// state=='open', so the sweep never saw a quarantined row in any test and
// reverting the fix broke nothing.
//
// Two properties, and they pull against each other: a quarantined row must be
// VISIBLE to the sweep (or its documented operator exit — re-send the event,
// the webhook settles the invoice, adoption closes the intent — can never run),
// and it must never be REPLAYED (replaying is what the quarantine exists to
// prevent).
func TestChargeIntent_QuarantineRegainsAnExitAndIsNotReplayed(t *testing.T) {
	client := newIdempotentStripe()
	invoices := newMockInvoiceUpdater()
	invoices.invoices["inv_1"] = finalizedPendingInvoice()
	ledger := newMemChargeIntents()

	seeded, _, err := ledger.Open(context.Background(), domain.ChargeIntent{
		TenantID: "t1", InvoiceID: "inv_1", IdempotencyKey: "velox_inv_inv_1_0_pm_x",
		AmountCents: invoices.invoices["inv_1"].AmountDueCents, Currency: "USD",
		StripeCustomerID: "cus_x", StripePaymentMethodID: "pm_x", OccurredAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := ledger.MarkNeedsReview(context.Background(), "t1", seeded.ID, "unresolvable"); err != nil {
		t.Fatalf("quarantine: %v", err)
	}
	ledger.rows[seeded.ID].UpdatedAt = time.Now().UTC().Add(-2 * chargeIntentCoolOff)

	rec := NewReconciler(client, &recoveryInvoiceStore{mock: invoices}, time.Second)
	rec.SetChargeIntents(ledger)

	// Pass 1: no PaymentIntent known yet — visible, but never replayed.
	if _, errs := rec.RecoverChargeIntents(context.Background(), 10); len(errs) > 0 {
		t.Fatalf("pass 1: %v", errs)
	}
	if got := client.mintedCount(); got != 0 {
		t.Fatalf("a quarantined intent was REPLAYED (%d provider calls) — past the key's retention that creates a second PaymentIntent", got)
	}
	if ledger.get(seeded.ID).RecoveryAttempts != 0 {
		t.Error("a quarantined row burned a recovery attempt without doing any work")
	}

	// Pass 2: the operator re-sends the event, the webhook settles the invoice,
	// and adoption must now close the intent — the exit the ADR documents.
	inv := invoices.invoices["inv_1"]
	inv.StripePaymentIntentID = "pi_found_by_operator"
	invoices.invoices["inv_1"] = inv

	if _, errs := rec.RecoverChargeIntents(context.Background(), 10); len(errs) > 0 {
		t.Fatalf("pass 2: %v", errs)
	}
	if st := ledger.get(seeded.ID).State; st != domain.ChargeIntentResolved {
		t.Fatalf("quarantine has no exit: state still %s after the PaymentIntent became known", st)
	}
	if got := client.mintedCount(); got != 0 {
		t.Fatalf("adoption made %d provider calls; it must make none", got)
	}
}

// TestChargeIntent_BreakerSkipRefundsTheRecoveryBudget covers fix 6, which had
// no test. A breaker skip provably makes no call, so counting it would let an
// outage burn every intent's budget and quarantine the whole queue for work
// that never happened.
func TestChargeIntent_BreakerSkipRefundsTheRecoveryBudget(t *testing.T) {
	ledger := newMemChargeIntents()
	row, _, err := ledger.Open(context.Background(), domain.ChargeIntent{
		TenantID: "t1", InvoiceID: "inv_1", IdempotencyKey: "k", AmountCents: 100,
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := ledger.BumpRecoveryAttempt(context.Background(), "t1", row.ID); err != nil {
		t.Fatalf("bump: %v", err)
	}
	if got := ledger.get(row.ID).RecoveryAttempts; got != 1 {
		t.Fatalf("attempts after bump = %d, want 1", got)
	}
	if err := ledger.ReleaseRecoveryAttempt(context.Background(), "t1", row.ID); err != nil {
		t.Fatalf("release: %v", err)
	}
	if got := ledger.get(row.ID).RecoveryAttempts; got != 0 {
		t.Fatalf("a provably-skipped attempt still counted (attempts=%d) — an outage would quarantine the queue", got)
	}
	// Never below zero, or a row could out-run its budget forever.
	if err := ledger.ReleaseRecoveryAttempt(context.Background(), "t1", row.ID); err != nil {
		t.Fatalf("second release: %v", err)
	}
	if got := ledger.get(row.ID).RecoveryAttempts; got != 0 {
		t.Fatalf("attempts underflowed to %d", got)
	}
}

// recoveryInvoiceStore adapts the charge-path mock to the reconciler's store
// interface, so recovery can be exercised against the same in-memory invoices
// the charge wrote to.
type recoveryInvoiceStore struct {
	mock     *mockInvoiceUpdater
	attempts []domain.InvoiceChargeAttempt
}

func (r *recoveryInvoiceStore) ListUnknownPayments(context.Context, time.Time, int) ([]domain.Invoice, error) {
	return nil, nil
}

func (r *recoveryInvoiceStore) ListProcessingPayments(context.Context, time.Time, int) ([]domain.Invoice, error) {
	return nil, nil
}

func (r *recoveryInvoiceStore) Get(_ context.Context, _, id string) (domain.Invoice, error) {
	return r.mock.invoices[id], nil
}

// RecordChargeAttempt captures the ADR-102 fact so a test can assert a
// recovered attempt actually reaches the timeline.
func (r *recoveryInvoiceStore) RecordChargeAttempt(_ context.Context, _ string, a domain.InvoiceChargeAttempt) error {
	r.attempts = append(r.attempts, a)
	return nil
}

func (r *recoveryInvoiceStore) MarkPaid(ctx context.Context, tenantID, id, piID string, paidAt time.Time) (domain.Invoice, error) {
	return r.mock.MarkPaid(ctx, tenantID, id, piID, paidAt)
}

func (r *recoveryInvoiceStore) UpdatePayment(ctx context.Context, tenantID, id string, ps domain.InvoicePaymentStatus, piID, lastErr string, paidAt *time.Time) (domain.Invoice, error) {
	return r.mock.UpdatePayment(ctx, tenantID, id, ps, piID, lastErr, paidAt)
}
