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

// Bad-debt RECOVERY: an operator charging a written-off invoice when the
// customer comes back. Real Postgres throughout — every guard here is a SQL
// predicate in the claim CAS, and a fake would only prove the fake.

// seedWrittenOff creates a written-off invoice in the recoverable shape, then
// applies `mutate` (raw SQL) so each test can set exactly the one column its
// gate keys on.
func seedWrittenOff(t *testing.T, db *postgres.DB, ctx context.Context, tenantID, num, mutate string) domain.Invoice {
	t.Helper()
	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_" + num, DisplayName: "Recovery " + num,
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	store := invoice.NewPostgresStore(db)
	inv, err := store.Create(ctx, tenantID, domain.Invoice{
		CustomerID: cust.ID, InvoiceNumber: num,
		Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentFailed,
		Currency: "USD", SubtotalCents: 5000, TotalAmountCents: 5000, AmountDueCents: 5000,
		BillingPeriodStart: time.Now().UTC().Add(-30 * 24 * time.Hour),
		BillingPeriodEnd:   time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("create invoice: %v", err)
	}
	tx, err := db.BeginTx(context.Background(), postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(context.Background(),
		`UPDATE invoices SET status='uncollectible', uncollectible_at=now() `+mutate+` WHERE id=$1`, inv.ID); err != nil {
		t.Fatalf("write off %s: %v", num, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return inv
}

// TestRecoveryClaim_OperatorMayMachineMayNot is THE safety boundary of this
// feature, and the reason ClaimChargeForManualCollect stopped sharing
// claimChargeLease.
//
// An operator may charge a written-off invoice — the customer came back. No
// MACHINE may: dunning re-charging a debt the business formally gave up on is
// the failure this whole design exists to avoid. Both directions in one test,
// so collapsing the two claims back onto one shared lease fails here.
func TestRecoveryClaim_OperatorMayMachineMayNot(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Recovery Boundary")
	store := invoice.NewPostgresStore(db)

	writtenOff := seedWrittenOff(t, db, ctx, tenantID, "INV-REC-1", "")

	// The machine must be refused FIRST — if the operator claim ran first it
	// would hold the lease and the dunning refusal would be ambiguous.
	ok, err := store.ClaimChargeForDunningRetry(ctx, tenantID, writtenOff.ID)
	if err != nil {
		t.Fatalf("dunning claim: %v", err)
	}
	if ok {
		t.Fatal("a DUNNING retry claimed a written-off invoice — a machine must never re-charge a debt the business wrote off")
	}

	ok, err = store.ClaimChargeForManualCollect(ctx, tenantID, writtenOff.ID)
	if err != nil {
		t.Fatalf("operator claim: %v", err)
	}
	if !ok {
		t.Fatal("the OPERATOR could not claim a written-off invoice — bad-debt recovery is impossible")
	}

	// Negative control: an ordinary finalized invoice is still claimable by
	// BOTH, so the split did not narrow the normal path.
	open := seedClaimableInvoice(t, db, ctx, tenantID, "INV-REC-OPEN")
	if ok, err := store.ClaimChargeForDunningRetry(ctx, tenantID, open.ID); err != nil || !ok {
		t.Fatalf("regression: dunning can no longer claim a finalized invoice (ok=%v err=%v)", ok, err)
	}
}

// TestRecoveryClaim_GatesLiveInTheCAS pins the two money gates as SQL, not as
// service pre-reads. A pre-read is a TOCTOU against the very sweeps that create
// these states — the tax-reversal sweep especially: read says "safe", sweep
// reverses, charge proceeds, tenant under-remits.
func TestRecoveryClaim_GatesLiveInTheCAS(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Recovery Gates")
	store := invoice.NewPostgresStore(db)

	cases := []struct {
		name      string
		mutate    string
		wantClaim bool
		why       string
	}{
		{
			name: "tax already reversed", mutate: ", tax_transaction_id='tax_tx_123'", wantClaim: false,
			why: "MarkUncollectible reversed this invoice's tax upstream and it cannot be re-committed (23h calc TTL, draft-only computation). Charging collects tax the tenant told the authority it did not collect.",
		},
		{
			name: "threshold usage re-billed", mutate: ", billing_reason='threshold'", wantClaim: false,
			why: "writing off a threshold invoice does not stop the next cycle close re-billing that usage window — collecting this one double-bills it",
		},
		{
			name: "recoverable", mutate: ", billing_reason='subscription_cycle'", wantClaim: true,
			why: "negative control: an ordinary written-off cycle invoice with no committed tax IS recoverable, so the gates above are discriminating rather than blanket refusals",
		},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inv := seedWrittenOff(t, db, ctx, tenantID, "INV-GATE-"+string(rune('A'+i)), tc.mutate)
			got, err := store.ClaimChargeForManualCollect(ctx, tenantID, inv.ID)
			if err != nil {
				t.Fatalf("claim: %v", err)
			}
			if got != tc.wantClaim {
				t.Errorf("claim = %v, want %v — %s", got, tc.wantClaim, tc.why)
			}
		})
	}
}

// TestRecoveryClaim_ParkedIsStillRefused: an ambiguous charge outcome parks the
// invoice at payment_status='unknown' (ADR-107), and writing it off does not
// answer whether the card was charged. Recovery must not blind re-charge it —
// that is the exact double-charge ADR-107 exists to prevent. Free, because the
// claim's payment_status predicate already excludes 'unknown'; pinned so a
// future widening cannot quietly admit it.
func TestRecoveryClaim_ParkedIsStillRefused(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Recovery Parked")
	store := invoice.NewPostgresStore(db)

	inv := seedWrittenOff(t, db, ctx, tenantID, "INV-REC-PARKED", ", payment_status='unknown'")
	ok, err := store.ClaimChargeForManualCollect(ctx, tenantID, inv.ID)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if ok {
		t.Fatal("recovery claimed a PARKED written-off invoice — nobody knows whether that card was already charged, so this is a blind re-charge (ADR-107)")
	}
}

// TestRecoveryClaim_CollisionExactlyOne: two operators clicking at once must
// produce one claim, hence one PaymentIntent. Same shape as the auto-charge
// collision pin, re-run against the recovery predicate because it is new SQL.
func TestRecoveryClaim_CollisionExactlyOne(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Recovery Collision")
	store := invoice.NewPostgresStore(db)
	inv := seedWrittenOff(t, db, ctx, tenantID, "INV-REC-RACE", "")

	const n = 8
	var wg sync.WaitGroup
	results := make([]bool, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			ok, err := store.ClaimChargeForManualCollect(ctx, tenantID, inv.ID)
			if err == nil {
				results[i] = ok
			}
		}(i)
	}
	wg.Wait()

	won := 0
	for _, r := range results {
		if r {
			won++
		}
	}
	if won != 1 {
		t.Fatalf("%d concurrent recovery claims won, want exactly 1 — more than one means more than one card charge", won)
	}
}

// TestRecovery_SettlesToPaidAndKeepsTheWriteOff is the feature's central
// promise, and until now nothing asserted it.
//
// The entire design rests on "the invoice is charged in place, not reopened —
// the write-off is preserved as history". If uncollectible_at were lost on
// settle, the recovery would be an ERASURE, which is exactly the outcome the
// reopen shape was rejected for. A claim test and a markPaid test each passing
// separately does not prove this; only the pair together does.
func TestRecovery_SettlesToPaidAndKeepsTheWriteOff(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Recovery Settle")
	store := invoice.NewPostgresStore(db)

	inv := seedWrittenOff(t, db, ctx, tenantID, "INV-REC-SETTLE", "")

	before, err := store.Get(ctx, tenantID, inv.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if before.UncollectibleAt == nil {
		t.Fatal("precondition: the fixture is not actually written off")
	}
	writtenOffAt := *before.UncollectibleAt

	// The operator claims it (the recovery path), then the charge settles.
	if ok, err := store.ClaimChargeForManualCollect(ctx, tenantID, inv.ID); err != nil || !ok {
		t.Fatalf("recovery claim failed: ok=%v err=%v", ok, err)
	}
	paidAt := time.Now().UTC()
	if _, err := store.MarkPaid(ctx, tenantID, inv.ID, "pi_recovered", paidAt); err != nil {
		t.Fatalf("mark paid: %v", err)
	}

	got, err := store.Get(ctx, tenantID, inv.ID)
	if err != nil {
		t.Fatalf("get after settle: %v", err)
	}

	if got.Status != domain.InvoicePaid {
		t.Errorf("status = %q, want paid — a settled recovery must leave the invoice paid", got.Status)
	}
	if got.PaymentStatus != domain.PaymentSucceeded {
		t.Errorf("payment_status = %q, want succeeded", got.PaymentStatus)
	}
	// THE ASSERTION THIS TEST EXISTS FOR.
	if got.UncollectibleAt == nil {
		t.Fatal("uncollectible_at was CLEARED by the recovery — the write-off has been erased, which is precisely what charging in place exists to avoid")
	}
	if !got.UncollectibleAt.Equal(writtenOffAt) {
		t.Errorf("uncollectible_at moved: %v -> %v; the write-off's own date must not be rewritten by a later recovery", writtenOffAt, *got.UncollectibleAt)
	}
	if got.PaidAt == nil {
		t.Error("paid_at must be set — both stamps carry, which is Stripe's status_transitions shape")
	}
	if got.StripePaymentIntentID != "pi_recovered" {
		t.Errorf("stripe_payment_intent_id = %q, want the recovery's PI", got.StripePaymentIntentID)
	}
}
