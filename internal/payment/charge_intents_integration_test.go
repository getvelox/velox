package payment_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/payment"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

func seedChargeableInvoice(t *testing.T, db *postgres.DB, ctx context.Context, tenantID, num string) domain.Invoice {
	t.Helper()
	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_" + num, DisplayName: "Intent " + num,
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	inv, err := invoice.NewPostgresStore(db).Create(ctx, tenantID, domain.Invoice{
		CustomerID:         cust.ID,
		InvoiceNumber:      num,
		Status:             domain.InvoiceFinalized,
		PaymentStatus:      domain.PaymentPending,
		Currency:           "USD",
		SubtotalCents:      5000,
		TotalAmountCents:   5000,
		AmountDueCents:     5000,
		BillingPeriodStart: time.Now().UTC().Add(-30 * 24 * time.Hour),
		BillingPeriodEnd:   time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("create invoice: %v", err)
	}
	return inv
}

func intentFor(tenantID, invoiceID, key string) domain.ChargeIntent {
	return domain.ChargeIntent{
		TenantID:              tenantID,
		InvoiceID:             invoiceID,
		IdempotencyKey:        key,
		AmountCents:           5000,
		Currency:              "USD",
		StripeCustomerID:      "cus_stripe_x",
		StripePaymentMethodID: "pm_x",
		OccurredAt:            time.Now().UTC(),
	}
}

// TestChargeIntent_CollisionExactlyOneUnresolved is the real-Postgres proof of
// the guarantee. The in-memory ledger used by the unit tests MODELS the partial
// unique index; this asserts the database actually enforces it, because that
// index is what makes a second concurrent charge unrecordable — and, since the
// pre-call write is fail-closed, unmakeable.
//
// N goroutines race to open an intent on one invoice under DIFFERENT keys (the
// shape a card swap or an amount change produces). Exactly one row may exist,
// and every caller must be handed that same row.
func TestChargeIntent_CollisionExactlyOneUnresolved(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Charge Intent Collision")
	inv := seedChargeableInvoice(t, db, ctx, tenantID, "INV-CIN-RACE")
	store := payment.NewChargeIntentStore(db)

	const racers = 8
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		ids  = map[string]int{}
		errs []error
	)
	wg.Add(racers)
	for i := 0; i < racers; i++ {
		go func(i int) {
			defer wg.Done()
			key := "velox_inv_" + inv.ID + "_0_pm_" + string(rune('a'+i))
			got, _, err := store.Open(ctx, intentFor(tenantID, inv.ID, key))
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			ids[got.ID]++
		}(i)
	}
	wg.Wait()

	if len(errs) > 0 {
		t.Fatalf("no racer may fail outright (each must be handed the winning row): %v", errs)
	}
	if len(ids) != 1 {
		t.Fatalf("%d distinct charge intents exist for one invoice, want 1 — a second PaymentIntent could be opened", len(ids))
	}
	for id, n := range ids {
		if n != racers {
			t.Fatalf("intent %s returned to %d of %d racers", id, n, racers)
		}
	}
}

// TestChargeIntent_QuarantineOutlivesTheOpenState pins the predicate choice
// that a reasonable reviewer would get wrong: the one-unresolved index uses
// `state <> 'resolved'`, not `state = 'open'`.
//
// A needs_review row is exactly the case where nobody knows whether a
// PaymentIntent is live, so it must keep blocking. If the index were keyed on
// 'open', quarantining an unrecoverable attempt would UNBLOCK the invoice —
// turning the safety valve into the double-charge trigger.
func TestChargeIntent_QuarantineOutlivesTheOpenState(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Charge Intent Quarantine")
	inv := seedChargeableInvoice(t, db, ctx, tenantID, "INV-CIN-QUAR")
	store := payment.NewChargeIntentStore(db)

	first, _, err := store.Open(ctx, intentFor(tenantID, inv.ID, "key_first"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := store.MarkNeedsReview(ctx, tenantID, first.ID, "cannot resolve"); err != nil {
		t.Fatalf("quarantine: %v", err)
	}

	unresolved, err := store.HasUnresolved(ctx, tenantID, inv.ID)
	if err != nil {
		t.Fatalf("has unresolved: %v", err)
	}
	if !unresolved {
		t.Fatal("a needs_review intent must still count as unresolved — otherwise quarantine unblocks the invoice")
	}

	got, _, err := store.Open(ctx, intentFor(tenantID, inv.ID, "key_second"))
	if err != nil {
		t.Fatalf("open after quarantine: %v", err)
	}
	if got.ID != first.ID {
		t.Fatalf("a second attempt was allowed alongside a quarantined one (%s vs %s)", got.ID, first.ID)
	}
}

// TestChargeIntent_ResolvedFreesTheInvoice is the negative control for both
// tests above: the guard must LIFT, or an invoice would be chargeable exactly
// once in its lifetime.
func TestChargeIntent_ResolvedFreesTheInvoice(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Charge Intent Release")
	inv := seedChargeableInvoice(t, db, ctx, tenantID, "INV-CIN-FREE")
	store := payment.NewChargeIntentStore(db)

	first, _, err := store.Open(ctx, intentFor(tenantID, inv.ID, "key_one"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := store.Resolve(ctx, tenantID, first.ID, "pi_123"); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	unresolved, _ := store.HasUnresolved(ctx, tenantID, inv.ID)
	if unresolved {
		t.Fatal("a resolved intent must not keep blocking")
	}

	second, _, err := store.Open(ctx, intentFor(tenantID, inv.ID, "key_two"))
	if err != nil {
		t.Fatalf("open after resolve: %v", err)
	}
	if second.ID == first.ID {
		t.Fatal("a new attempt must get its own intent row once the previous one resolved")
	}
	if second.IdempotencyKey != "key_two" {
		t.Fatalf("second attempt key = %q, want its own", second.IdempotencyKey)
	}
}

// TestChargeIntent_SweepListsQuarantinedRowsAndRanksOpenFirst pins the sweep's
// SQL predicate and ordering directly, because the unit tests exercise only the
// in-memory ledger — which had silently kept the PRE-FIX predicate, leaving the
// quarantine-exit fix with no executing coverage anywhere.
//
// Two properties: a quarantined row must be LISTED (or its adoption exit can
// never run), and it must never OUTRANK an open one. A quarantined row's
// updated_at never advances, so under a pure updated_at ordering it would
// permanently head a LIMITed queue and starve every recoverable attempt.
func TestChargeIntent_SweepListsQuarantinedRowsAndRanksOpenFirst(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Charge Intent Sweep Order")
	store := payment.NewChargeIntentStore(db)

	// The quarantined row is OLDER, so updated_at alone would put it first.
	oldInv := seedChargeableInvoice(t, db, ctx, tenantID, "INV-CIN-OLD")
	old, _, err := store.Open(ctx, intentFor(tenantID, oldInv.ID, "key_old"))
	if err != nil {
		t.Fatalf("open old: %v", err)
	}
	if err := store.MarkNeedsReview(ctx, tenantID, old.ID, "unresolvable"); err != nil {
		t.Fatalf("quarantine: %v", err)
	}

	newInv := seedChargeableInvoice(t, db, ctx, tenantID, "INV-CIN-NEW")
	fresh, _, err := store.Open(ctx, intentFor(tenantID, newInv.ID, "key_new"))
	if err != nil {
		t.Fatalf("open new: %v", err)
	}

	rows, err := store.ListOpen(ctx, time.Now().UTC().Add(time.Minute), 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("want both rows listed (a quarantined row must stay visible for its adoption exit), got %d", len(rows))
	}
	if rows[0].ID != fresh.ID {
		t.Fatalf("the quarantined row outranked the open one — it never advances updated_at, so it would permanently head a LIMITed queue and starve recoverable work")
	}

	// And with a batch of one, the recoverable row is still the one returned.
	head, err := store.ListOpen(ctx, time.Now().UTC().Add(time.Minute), 1)
	if err != nil {
		t.Fatalf("list head: %v", err)
	}
	if len(head) != 1 || head[0].ID != fresh.ID {
		t.Fatalf("a single-row batch returned the quarantined row — every tick would do no useful work")
	}
}

// TestChargeIntent_KeyAgeSurvivesAReAttempt pins the interaction between two
// fixes that would otherwise cancel out. Re-attempting under a recurring key is
// permitted (the key index is partial), but the replay TTL measures
// occurred_at — so a fresh row carrying an OLD key would reset the clock and
// re-authorise a key the provider has already forgotten.
func TestChargeIntent_KeyAgeSurvivesAReAttempt(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Charge Intent Key Age")
	inv := seedChargeableInvoice(t, db, ctx, tenantID, "INV-CIN-KEYAGE")
	store := payment.NewChargeIntentStore(db)

	in := intentFor(tenantID, inv.ID, "velox_inv_recurring")
	in.OccurredAt = time.Now().UTC().Add(-20 * time.Hour) // the key's first use
	first, _, err := store.Open(ctx, in)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := store.Resolve(ctx, tenantID, first.ID, "pi_1"); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	// A later attempt recomputes the SAME key (the settle failed, so the seq
	// never moved) and would otherwise stamp occurred_at = now.
	again := intentFor(tenantID, inv.ID, "velox_inv_recurring")
	again.OccurredAt = time.Now().UTC()
	row, _, err := store.Open(ctx, again)
	if err != nil {
		t.Fatalf("re-attempt: %v", err)
	}
	if time.Since(row.OccurredAt) < 19*time.Hour {
		t.Fatalf("occurred_at reset to %v on a re-attempt under a recurring key — the replay TTL would re-authorise a key the provider has already forgotten", row.OccurredAt)
	}
}

// TestChargeIntent_RefusesWhileAPaymentIsAlreadyInFlight closes the gap the
// review found in the payability re-check: it read payment_status and then
// ignored it. That matters most for invoices predating this ledger — they have
// a PaymentIntent live at Stripe and NO intent row, so the one-unresolved index
// cannot block them and a second charge would open unopposed.
func TestChargeIntent_RefusesWhileAPaymentIsAlreadyInFlight(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Charge Intent InFlight")
	store := payment.NewChargeIntentStore(db)
	invStore := invoice.NewPostgresStore(db)

	for _, ps := range []domain.InvoicePaymentStatus{domain.PaymentProcessing, domain.PaymentUnknown} {
		t.Run(string(ps), func(t *testing.T) {
			inv := seedChargeableInvoice(t, db, ctx, tenantID, "INV-CIN-INFLIGHT-"+string(ps))
			if _, err := invStore.UpdatePayment(ctx, tenantID, inv.ID, ps, "pi_live", "", nil); err != nil {
				t.Fatalf("set %s: %v", ps, err)
			}
			if _, _, err := store.Open(ctx, intentFor(tenantID, inv.ID, "key_"+string(ps))); err == nil {
				t.Fatalf("opening a charge intent while payment_status=%s must be refused — a PaymentIntent is already live", ps)
			}
		})
	}
}

// TestChargeIntent_AResolvedRowDoesNotBlockItsKeyForever is the regression for
// a permanent-block defect the adversarial review found: the key uniqueness
// index was TOTAL, so once a resolved row held key K, a later attempt that
// recomputed K could not insert, the read-back (unresolved rows only) found
// nothing, and the charge was refused forever.
//
// K recurs legitimately: charge_attempt_seq moves only when an outcome is
// RECORDED, so a create that succeeded and whose settle then failed leaves the
// seq — and therefore the key — unchanged. The mechanism meant to prevent a
// double charge would have bricked the invoice instead. Re-attempting under a
// recurring key is safe: Stripe returns the original PaymentIntent for it.
func TestChargeIntent_AResolvedRowDoesNotBlockItsKeyForever(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Charge Intent Key Reuse")
	inv := seedChargeableInvoice(t, db, ctx, tenantID, "INV-CIN-REUSE")
	store := payment.NewChargeIntentStore(db)

	first, _, err := store.Open(ctx, intentFor(tenantID, inv.ID, "velox_inv_same_key"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := store.Resolve(ctx, tenantID, first.ID, "pi_1"); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	// The settle failed, so the seq never moved and the next attempt computes
	// the SAME key.
	again, created, err := store.Open(ctx, intentFor(tenantID, inv.ID, "velox_inv_same_key"))
	if err != nil {
		t.Fatalf("re-attempt under a recurring key must be allowed, got: %v", err)
	}
	if !created || again.ID == first.ID {
		t.Fatalf("want a fresh row for the re-attempt (created=%v, id=%s vs %s)", created, again.ID, first.ID)
	}
	if again.State != domain.ChargeIntentOpen {
		t.Fatalf("re-attempt state = %s, want open", again.State)
	}
}

// TestChargeIntent_RefusesAnUnpayableInvoice: the payability re-check runs
// under FOR SHARE inside the claim tx, so it serialises against the settle
// path's paid-flip. An intent must never be opened on an invoice that just got
// paid — that is the mint-vs-settle race, and opening one would both quarantine
// a settled invoice and authorise a charge against nothing owed.
func TestChargeIntent_RefusesAnUnpayableInvoice(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Charge Intent Payable")
	inv := seedChargeableInvoice(t, db, ctx, tenantID, "INV-CIN-PAID")
	store := payment.NewChargeIntentStore(db)

	if _, err := invoice.NewPostgresStore(db).MarkPaid(ctx, tenantID, inv.ID, "pi_paid", time.Now().UTC()); err != nil {
		t.Fatalf("mark paid: %v", err)
	}
	if _, _, err := store.Open(ctx, intentFor(tenantID, inv.ID, "key_late")); err == nil {
		t.Fatal("opening a charge intent on a paid invoice must fail")
	}
}
