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
			got, err := store.Open(ctx, intentFor(tenantID, inv.ID, key))
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

	first, err := store.Open(ctx, intentFor(tenantID, inv.ID, "key_first"))
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

	got, err := store.Open(ctx, intentFor(tenantID, inv.ID, "key_second"))
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

	first, err := store.Open(ctx, intentFor(tenantID, inv.ID, "key_one"))
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

	second, err := store.Open(ctx, intentFor(tenantID, inv.ID, "key_two"))
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
	if _, err := store.Open(ctx, intentFor(tenantID, inv.ID, "key_late")); err == nil {
		t.Fatal("opening a charge intent on a paid invoice must fail")
	}
}
