package invoice_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// seedClaimableInvoice creates an invoice matching the auto-charge sweep
// predicates exactly (finalized, payment pending, flag set, owing).
func seedClaimableInvoice(t *testing.T, db *postgres.DB, ctx context.Context, tenantID, num string) domain.Invoice {
	t.Helper()
	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_" + num, DisplayName: "Claim " + num,
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	store := invoice.NewPostgresStore(db)
	inv, err := store.Create(ctx, tenantID, domain.Invoice{
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
	if err := store.SetAutoChargePending(ctx, tenantID, inv.ID, true); err != nil {
		t.Fatalf("set auto_charge_pending: %v", err)
	}
	return inv
}

// TestClaimAutoCharge_CollisionExactlyOne is the real-Postgres CAS pin
// (HA hazard #1): N racing claimers on one invoice — exactly one wins
// the lease window. Remove the claim's WHERE lease predicate and this
// fails (mutation-verify).
func TestClaimAutoCharge_CollisionExactlyOne(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Claim Collision")
	inv := seedClaimableInvoice(t, db, ctx, tenantID, "INV-CLAIM-RACE")
	store := invoice.NewPostgresStore(db)

	const racers = 8
	var wg sync.WaitGroup
	wins := make(chan bool, racers)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := store.ClaimAutoCharge(ctx, tenantID, inv.ID)
			if err != nil {
				t.Errorf("claim: %v", err)
				return
			}
			wins <- ok
		}()
	}
	wg.Wait()
	close(wins)
	won := 0
	for ok := range wins {
		if ok {
			won++
		}
	}
	if won != 1 {
		t.Fatalf("claim winners: got %d, want exactly 1 — the charge leg must admit one leader per lease window", won)
	}
}

// TestClaimAutoCharge_LeaseExpiryAndRelease: a held lease refuses
// re-claims; an expired lease (or an explicit release on a pre-Stripe
// skip path) re-admits.
func TestClaimAutoCharge_LeaseExpiryAndRelease(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Claim Lease")
	inv := seedClaimableInvoice(t, db, ctx, tenantID, "INV-CLAIM-LEASE")
	store := invoice.NewPostgresStore(db)

	if ok, _ := store.ClaimAutoCharge(ctx, tenantID, inv.ID); !ok {
		t.Fatal("first claim must succeed")
	}
	if ok, _ := store.ClaimAutoCharge(ctx, tenantID, inv.ID); ok {
		t.Fatal("second claim inside the lease must fail")
	}

	// Expiry: push the lease into the past (simulating a crashed leader)
	// — the next tick re-claims.
	expireLease(t, db, inv.ID)
	if ok, _ := store.ClaimAutoCharge(ctx, tenantID, inv.ID); !ok {
		t.Fatal("claim after lease expiry must succeed (crashed leader self-heals)")
	}

	// Release: explicit clear (provably-pre-Stripe skip path) re-admits
	// immediately, no lease wait.
	if err := store.ReleaseAutoChargeClaim(ctx, tenantID, inv.ID); err != nil {
		t.Fatalf("release: %v", err)
	}
	if ok, _ := store.ClaimAutoCharge(ctx, tenantID, inv.ID); !ok {
		t.Fatal("claim after explicit release must succeed")
	}
}

func expireLease(t *testing.T, db *postgres.DB, invoiceID string) {
	t.Helper()
	tx, err := db.BeginTx(context.Background(), postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.Exec(
		`UPDATE invoices SET auto_charge_claimed_until = now() - interval '1 second' WHERE id = $1`,
		invoiceID); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// TestClaimAutoCharge_PredicateRecheck: the CAS re-asserts the full
// sweep eligibility, so any state movement between list and claim —
// a webhook settle, a rival leader's failed/unknown outcome, a cleared
// flag, a voided invoice, a zeroed amount_due — fails the claim
// (mutation-verify style: every predicate flipped once).
func TestClaimAutoCharge_PredicateRecheck(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Claim Predicates")
	store := invoice.NewPostgresStore(db)

	mutations := []struct {
		name string
		sql  string
	}{
		{"webhook settled (status=paid, payment=succeeded)", `UPDATE invoices SET status='paid', payment_status='succeeded' WHERE id=$1`},
		{"rival outcome unknown", `UPDATE invoices SET payment_status='unknown' WHERE id=$1`},
		{"rival outcome failed", `UPDATE invoices SET payment_status='failed' WHERE id=$1`},
		{"flag cleared", `UPDATE invoices SET auto_charge_pending=FALSE WHERE id=$1`},
		{"voided", `UPDATE invoices SET status='voided' WHERE id=$1`},
		{"credit-covered (amount_due=0)", `UPDATE invoices SET amount_due_cents=0 WHERE id=$1`},
	}
	for i, m := range mutations {
		t.Run(m.name, func(t *testing.T) {
			inv := seedClaimableInvoice(t, db, ctx, tenantID, "INV-CLAIM-PRED-"+string(rune('A'+i)))
			tx, err := db.BeginTx(context.Background(), postgres.TxBypass, "")
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			if _, err := tx.Exec(m.sql, inv.ID); err != nil {
				_ = tx.Rollback()
				t.Fatalf("mutate: %v", err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatalf("commit: %v", err)
			}
			if ok, _ := store.ClaimAutoCharge(ctx, tenantID, inv.ID); ok {
				t.Fatalf("claim must fail after %q — charging stale state is the double-charge path", m.name)
			}
		})
	}
}

// TestClaimAutoCharge_UpdatedAtStable pins that claim and release must NOT
// touch updated_at.
//
// The original reason — "the Stripe idempotency key derives from it" — expired
// with #678: the key is now seeded from charge_attempt_seq, which the lease
// cannot reach, and TestChargeKey_StableUntilAnOutcomeIsRecorded owns that
// property. What remains is still worth pinning: the sweep lists order by
// `updated_at ASC`, so a lease that bumped it would send an invoice to the back
// of the queue on every claim — an invoice that fails to complete its charge
// could be starved indefinitely behind newer arrivals.
func TestClaimAutoCharge_UpdatedAtStable(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Claim UpdatedAt")
	inv := seedClaimableInvoice(t, db, ctx, tenantID, "INV-CLAIM-UAT")
	store := invoice.NewPostgresStore(db)

	before, err := store.Get(ctx, tenantID, inv.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if ok, _ := store.ClaimAutoCharge(ctx, tenantID, inv.ID); !ok {
		t.Fatal("claim must succeed")
	}
	if err := store.ReleaseAutoChargeClaim(ctx, tenantID, inv.ID); err != nil {
		t.Fatalf("release: %v", err)
	}
	expireLease(t, db, inv.ID)
	if ok, _ := store.ClaimAutoCharge(ctx, tenantID, inv.ID); !ok {
		t.Fatal("re-claim must succeed")
	}
	after, err := store.Get(ctx, tenantID, inv.ID)
	if err != nil {
		t.Fatalf("get after: %v", err)
	}
	if !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("updated_at moved across claim/release/re-claim (%v → %v) — this DIVERGES the Stripe idempotency key and re-opens the dual-leader double charge", before.UpdatedAt, after.UpdatedAt)
	}
}

// TestClaimAutoCharge_ListsStayClaimBlind is the enrollment-starvation
// regression (adversarial-review flaw 2): ListAutoChargePending is
// shared with dunning enrollment (EnrollStalledForDunning), so a held
// claim must NOT hide the invoice from the list — only the charge leg
// consults the lease.
func TestClaimAutoCharge_ListsStayClaimBlind(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Claim Blind Lists")
	inv := seedClaimableInvoice(t, db, ctx, tenantID, "INV-CLAIM-BLIND")
	store := invoice.NewPostgresStore(db)

	if ok, _ := store.ClaimAutoCharge(ctx, tenantID, inv.ID); !ok {
		t.Fatal("claim must succeed")
	}
	listed, err := store.ListAutoChargePending(ctx, 50)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	found := false
	for _, li := range listed {
		if li.ID == inv.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("a claimed invoice vanished from ListAutoChargePending — dunning enrollment shares this list and would be starved (card-less invoices never enter dunning)")
	}
}

// TestClaimChargeForDunningRetry_CrossPathExclusion (HA hazard #11):
// the sweep claim and the dunning-retry claim share ONE lease column —
// whichever path claims first, the other is refused. Their Stripe
// idempotency keys differ by construction (purpose suffix), so this
// mutual exclusion is the only double-charge guard between them.
func TestClaimChargeForDunningRetry_CrossPathExclusion(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Claim CrossPath")
	store := invoice.NewPostgresStore(db)

	// Sweep first, dunning refused.
	a := seedClaimableInvoice(t, db, ctx, tenantID, "INV-XPATH-A")
	if ok, _ := store.ClaimAutoCharge(ctx, tenantID, a.ID); !ok {
		t.Fatal("sweep claim must succeed")
	}
	if ok, _ := store.ClaimChargeForDunningRetry(ctx, tenantID, a.ID); ok {
		t.Fatal("dunning claim must be refused while the sweep holds the lease — concurrent charges have divergent keys Stripe cannot dedupe")
	}

	// Dunning first, sweep refused.
	b := seedClaimableInvoice(t, db, ctx, tenantID, "INV-XPATH-B")
	if ok, _ := store.ClaimChargeForDunningRetry(ctx, tenantID, b.ID); !ok {
		t.Fatal("dunning claim must succeed")
	}
	if ok, _ := store.ClaimAutoCharge(ctx, tenantID, b.ID); ok {
		t.Fatal("sweep claim must be refused while dunning holds the lease")
	}
}

// TestClaimChargeForDunningRetry_StatusMatrix pins the deliberate
// predicate asymmetry between the two claims:
//   - 'pending'  → both paths may claim (card-less enrolled + card attached);
//   - 'failed'   → dunning only (the normal retry state; the sweep's list
//     never returns failed invoices);
//   - 'unknown'  → NEITHER — an ambiguous outcome may be a real payment
//     and must wait for the reconciler. This is the same-tick
//     N=1 window: billing half's unknown outcome followed
//     seconds later by the dunning half's due retry.
func TestClaimChargeForDunningRetry_StatusMatrix(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Claim StatusMatrix")
	store := invoice.NewPostgresStore(db)

	set := func(t *testing.T, id, status string) {
		t.Helper()
		tx, err := db.BeginTx(context.Background(), postgres.TxBypass, "")
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := tx.Exec(`UPDATE invoices SET payment_status=$1, auto_charge_claimed_until=NULL WHERE id=$2`, status, id); err != nil {
			_ = tx.Rollback()
			t.Fatalf("set status: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}

	inv := seedClaimableInvoice(t, db, ctx, tenantID, "INV-STATUS-MATRIX")

	// pending: dunning claimable.
	if ok, _ := store.ClaimChargeForDunningRetry(ctx, tenantID, inv.ID); !ok {
		t.Fatal("dunning claim on 'pending' must succeed (card-less enrolled, card attached)")
	}

	// failed: dunning claimable, sweep NOT.
	set(t, inv.ID, "failed")
	if ok, _ := store.ClaimChargeForDunningRetry(ctx, tenantID, inv.ID); !ok {
		t.Fatal("dunning claim on 'failed' must succeed (the normal retry state)")
	}
	set(t, inv.ID, "failed")
	if ok, _ := store.ClaimAutoCharge(ctx, tenantID, inv.ID); ok {
		t.Fatal("sweep claim on 'failed' must be refused — dunning owns failed invoices")
	}

	// unknown: NEITHER path may charge — reconciler territory.
	set(t, inv.ID, "unknown")
	if ok, _ := store.ClaimChargeForDunningRetry(ctx, tenantID, inv.ID); ok {
		t.Fatal("dunning claim on 'unknown' must be refused — the ambiguous PI may be a real payment; blind re-charge is the same-tick double-charge window")
	}
	if ok, _ := store.ClaimAutoCharge(ctx, tenantID, inv.ID); ok {
		t.Fatal("sweep claim on 'unknown' must be refused")
	}
}

// TestClaimChargeForManualCollect_SharedLeaseAndPredicate pins the
// operator-collect claim added by the 2026-07-21 snapshot-race audit:
// manual collect was the one charge initiator outside the lease ring —
// it charged a handler snapshot with no mutual exclusion against the
// sweep/dunning and no post-credit reload. The claim shares the
// auto_charge_claimed_until lease, so holding it excludes every other
// initiator; 'unknown' is rejected (reconciler-owned); release frees it.
func TestClaimChargeForManualCollect_SharedLeaseAndPredicate(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Manual Collect Claim")
	store := invoice.NewPostgresStore(db)

	inv := seedClaimableInvoice(t, db, ctx, tenantID, "MCC-1")

	// Sweep holds the lease → manual collect must lose.
	claimed, err := store.ClaimAutoCharge(ctx, tenantID, inv.ID)
	if err != nil || !claimed {
		t.Fatalf("sweep claim: claimed=%v err=%v", claimed, err)
	}
	manual, err := store.ClaimChargeForManualCollect(ctx, tenantID, inv.ID)
	if err != nil {
		t.Fatalf("manual claim: %v", err)
	}
	if manual {
		t.Fatal("manual collect must NOT claim while the sweep holds the lease")
	}

	// Released → manual collect wins; a second manual claim then loses.
	if err := store.ReleaseAutoChargeClaim(ctx, tenantID, inv.ID); err != nil {
		t.Fatalf("release: %v", err)
	}
	manual, err = store.ClaimChargeForManualCollect(ctx, tenantID, inv.ID)
	if err != nil || !manual {
		t.Fatalf("manual claim after release: claimed=%v err=%v", manual, err)
	}
	second, err := store.ClaimChargeForManualCollect(ctx, tenantID, inv.ID)
	if err != nil {
		t.Fatalf("second manual claim: %v", err)
	}
	if second {
		t.Fatal("second manual claim must lose while the first holds the lease")
	}

	// 'unknown' payment status is never claimable — the reconciler owns it.
	unknownInv := seedClaimableInvoice(t, db, ctx, tenantID, "MCC-2")
	if _, err := store.UpdatePayment(ctx, tenantID, unknownInv.ID, domain.PaymentUnknown, "pi_amb", "ambiguous outcome", nil); err != nil {
		t.Fatalf("mark unknown: %v", err)
	}
	claimed, err = store.ClaimChargeForManualCollect(ctx, tenantID, unknownInv.ID)
	if err != nil {
		t.Fatalf("claim unknown: %v", err)
	}
	if claimed {
		t.Fatal("an invoice with payment_status='unknown' must never be claimable for manual collect")
	}
}
