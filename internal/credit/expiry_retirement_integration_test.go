package credit_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/credit"
	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// P14 (ADR-071): expiry retires the grant block. The pre-fix sweep
// appended the -remaining expiry entry from a stale candidate snapshot
// and never touched consumed_cents, so (a) a backdated apply landing
// between the candidate list and the append made the sweep over-expire
// — negative ledger — and (b) a backdated apply arriving AFTER the
// expiry could re-drain the "expired" grant's still-open headroom.
// ExpireGrantAtomic recomputes remaining under the same FOR UPDATE row
// lock the apply/adjust paths hold and flips consumed_cents =
// amount_cents atomically with the entry.

// expirySetup seeds a tenant + customer and returns the stores.
func expirySetup(t *testing.T, name string) (*postgres.DB, context.Context, string, domain.Customer, *credit.PostgresStore, *credit.Service, *invoice.PostgresStore) {
	t.Helper()
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, name)
	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_" + name, DisplayName: name,
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	store := credit.NewPostgresStore(db)
	return db, ctx, tenantID, cust, store, credit.NewService(store), invoice.NewPostgresStore(db)
}

// seedExpiredGrant grants amountCents and then back-dates its
// expires_at to `expiresAt` (Grant rejects past expiry at create time,
// so the past stamp is applied directly). Returns the grant's id.
func seedExpiredGrant(t *testing.T, db *postgres.DB, ctx context.Context, svc *credit.Service, tenantID, customerID string, amountCents int64, expiresAt time.Time) string {
	t.Helper()
	future := time.Now().UTC().Add(24 * time.Hour)
	g, err := svc.Grant(ctx, tenantID, credit.GrantInput{
		CustomerID: customerID, AmountCents: amountCents,
		Description: "seed expiring grant", ExpiresAt: &future,
	})
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin bypass: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(
		`UPDATE customer_credit_ledger SET expires_at = $1 WHERE id = $2`,
		expiresAt, g.ID,
	); err != nil {
		t.Fatalf("backdate expires_at: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit backdate: %v", err)
	}
	return g.ID
}

var invoiceSeq atomic.Int64

func createOpenInvoice(t *testing.T, ctx context.Context, invoiceStore *invoice.PostgresStore, tenantID, customerID string, amountCents int64) domain.Invoice {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Microsecond)
	dueAt := now.Add(7 * 24 * time.Hour)
	issuedAt := now
	inv, err := invoiceStore.Create(ctx, tenantID, domain.Invoice{
		InvoiceNumber:      fmt.Sprintf("EXP-%d", invoiceSeq.Add(1)),
		CustomerID:         customerID,
		Status:             domain.InvoiceDraft,
		PaymentStatus:      domain.PaymentPending,
		Currency:           "USD",
		SubtotalCents:      amountCents,
		TotalAmountCents:   amountCents,
		AmountDueCents:     amountCents,
		BillingPeriodStart: now.Add(-30 * 24 * time.Hour),
		BillingPeriodEnd:   now,
		IssuedAt:           &issuedAt,
		DueAt:              &dueAt,
	})
	if err != nil {
		t.Fatalf("create invoice: %v", err)
	}
	return inv
}

func customerLedgerFacts(t *testing.T, db *postgres.DB, ctx context.Context, customerID string) (sum int64, expiryEntries int, expiryTotal int64) {
	t.Helper()
	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin bypass: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := tx.QueryRow(`
		SELECT COALESCE(SUM(amount_cents), 0),
		       COUNT(*) FILTER (WHERE entry_type = 'expiry'),
		       COALESCE(SUM(ABS(amount_cents)) FILTER (WHERE entry_type = 'expiry'), 0)
		FROM customer_credit_ledger WHERE customer_id = $1
	`, customerID).Scan(&sum, &expiryEntries, &expiryTotal); err != nil {
		t.Fatalf("read ledger facts: %v", err)
	}
	return sum, expiryEntries, expiryTotal
}

func grantConsumed(t *testing.T, db *postgres.DB, ctx context.Context, grantID string) int64 {
	t.Helper()
	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin bypass: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	var consumed int64
	if err := tx.QueryRow(
		`SELECT consumed_cents FROM customer_credit_ledger WHERE id = $1`, grantID,
	).Scan(&consumed); err != nil {
		t.Fatalf("read consumed_cents: %v", err)
	}
	return consumed
}

// TestExpireGrantAtomic_RecomputesUnderLock is the deterministic
// stale-snapshot regression (the audit's headline negative-ledger
// mechanism): candidates are listed showing remaining=$100, a
// backdated apply then drains $60, and the retirement runs against the
// stale snapshot. The pre-fix sweep appended -$100 → SUM = -$60. The
// fix recomputes remaining under the row lock → expiry entry is
// exactly -$40 and the ledger lands at $0.
//
// Mutation-verify: make ExpireGrantAtomic use the caller snapshot's
// remaining (or the pre-fix AppendEntry shape) — this test fails.
func TestExpireGrantAtomic_RecomputesUnderLock(t *testing.T) {
	db, ctx, tenantID, cust, store, svc, invoiceStore := expirySetup(t, "ExpiryStaleSnap")
	expiresAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	grantID := seedExpiredGrant(t, db, ctx, svc, tenantID, cust.ID, 10000, expiresAt)

	// Candidate snapshot BEFORE the backdated apply — remaining $100.
	candidates, err := store.ListExpiredGrants(ctx, 0)
	if err != nil {
		t.Fatalf("list expired grants: %v", err)
	}
	var found bool
	for _, c := range candidates {
		if c.ID == grantID {
			found = true
			if c.AmountCents-c.ConsumedCents != 10000 {
				t.Fatalf("snapshot remaining: got %d, want 10000", c.AmountCents-c.ConsumedCents)
			}
		}
	}
	if !found {
		t.Fatal("expired grant not listed as candidate")
	}

	// Backdated apply (at < expires_at → the grant is eligible) drains $60.
	inv := createOpenInvoice(t, ctx, invoiceStore, tenantID, cust.ID, 6000)
	backdatedAt := expiresAt.Add(-time.Minute)
	applied, err := svc.ApplyToInvoiceAt(ctx, tenantID, cust.ID, inv.ID, 6000, backdatedAt)
	if err != nil {
		t.Fatalf("backdated apply: %v", err)
	}
	if applied != 6000 {
		t.Fatalf("backdated apply: got %d, want 6000 (grant not yet retired — still drainable)", applied)
	}

	// Retirement runs AFTER the drain, holding the stale snapshot's grant.
	retired, err := store.ExpireGrantAtomic(ctx, tenantID, cust.ID, grantID)
	if err != nil {
		t.Fatalf("ExpireGrantAtomic: %v", err)
	}
	if retired != 4000 {
		t.Errorf("retired: got %d, want 4000 (remaining recomputed under lock, not the snapshot's 10000)", retired)
	}

	sum, expiryEntries, expiryTotal := customerLedgerFacts(t, db, ctx, cust.ID)
	if sum != 0 {
		t.Errorf("ledger SUM: got %d, want 0 (pre-fix stale-snapshot expiry drove this to -6000)", sum)
	}
	if expiryEntries != 1 || expiryTotal != 4000 {
		t.Errorf("expiry entries: got %d totalling %d, want exactly 1 totalling 4000", expiryEntries, expiryTotal)
	}
	if consumed := grantConsumed(t, db, ctx, grantID); consumed != 10000 {
		t.Errorf("grant consumed_cents: got %d, want 10000 (retired)", consumed)
	}
}

// TestExpireGrant_RetirementWinsOverBackdatedApply locks ADR-071's
// decision: once the retirement has committed, a backdated apply (at <
// expires_at, which WOULD have been eligible pre-retirement) drains
// $0 from the retired grant — other active blocks drain normally and
// the block/balance attribution stays drift-free.
//
// Mutation-verify: remove the consumed_cents flip from
// ExpireGrantAtomic — the retired grant re-drains and this test fails.
func TestExpireGrant_RetirementWinsOverBackdatedApply(t *testing.T) {
	db, ctx, tenantID, cust, store, svc, invoiceStore := expirySetup(t, "ExpiryRetireWins")
	expiresAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	grantID := seedExpiredGrant(t, db, ctx, svc, tenantID, cust.ID, 10000, expiresAt)

	retired, err := store.ExpireGrantAtomic(ctx, tenantID, cust.ID, grantID)
	if err != nil {
		t.Fatalf("ExpireGrantAtomic: %v", err)
	}
	if retired != 10000 {
		t.Fatalf("retired: got %d, want 10000", retired)
	}

	// Active second block: the backdated apply must draw ONLY from this.
	if _, err := svc.Grant(ctx, tenantID, credit.GrantInput{
		CustomerID: cust.ID, AmountCents: 5000, Description: "active block",
	}); err != nil {
		t.Fatalf("grant active block: %v", err)
	}

	inv := createOpenInvoice(t, ctx, invoiceStore, tenantID, cust.ID, 8000)
	backdatedAt := expiresAt.Add(-time.Minute)
	applied, err := svc.ApplyToInvoiceAt(ctx, tenantID, cust.ID, inv.ID, 8000, backdatedAt)
	if err != nil {
		t.Fatalf("backdated apply after retirement: %v", err)
	}
	if applied != 5000 {
		t.Errorf("applied: got %d, want 5000 (only the active block — the retired grant is never re-admitted)", applied)
	}
	if consumed := grantConsumed(t, db, ctx, grantID); consumed != 10000 {
		t.Errorf("retired grant consumed_cents: got %d, want 10000 (untouched by the backdated apply)", consumed)
	}
	sum, _, _ := customerLedgerFacts(t, db, ctx, cust.ID)
	if sum != 0 {
		t.Errorf("ledger SUM: got %d, want 0 (10000 - 10000 expiry + 5000 - 5000 usage)", sum)
	}
}

// TestExpireCredits_ReplayAndConcurrentSweeps: the consumed_cents flip
// is the exactly-once gate — a replayed sweep and two racing sweeps
// converge on ONE expiry entry per grant with no zero-amount entries.
// Also exercises the candidate queries after the description-LIKE
// dedup removal: a retired grant (consumed == amount) is structurally
// excluded from re-listing.
func TestExpireCredits_ReplayAndConcurrentSweeps(t *testing.T) {
	db, ctx, tenantID, cust, _, svc, _ := expirySetup(t, "ExpiryReplay")
	expiresAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	grantID := seedExpiredGrant(t, db, ctx, svc, tenantID, cust.ID, 7000, expiresAt)

	if _, errs := svc.ExpireCredits(ctx); len(errs) != 0 {
		t.Fatalf("first sweep errors: %v", errs)
	}
	if _, errs := svc.ExpireCredits(ctx); len(errs) != 0 {
		t.Fatalf("replayed sweep errors: %v", errs)
	}
	sum, expiryEntries, expiryTotal := customerLedgerFacts(t, db, ctx, cust.ID)
	if expiryEntries != 1 || expiryTotal != 7000 {
		t.Fatalf("after sequential replay: %d expiry entries totalling %d, want exactly 1 totalling 7000", expiryEntries, expiryTotal)
	}
	if sum != 0 {
		t.Fatalf("ledger SUM after replay: got %d, want 0", sum)
	}
	if consumed := grantConsumed(t, db, ctx, grantID); consumed != 7000 {
		t.Fatalf("grant consumed_cents: got %d, want 7000", consumed)
	}

	// Concurrent sweeps on a fresh expired grant (same customer).
	grant2 := seedExpiredGrant(t, db, ctx, svc, tenantID, cust.ID, 3000, expiresAt)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, errs := svc.ExpireCredits(ctx); len(errs) != 0 {
				t.Errorf("concurrent sweep errors: %v", errs)
			}
		}()
	}
	close(start)
	wg.Wait()

	sum, expiryEntries, expiryTotal = customerLedgerFacts(t, db, ctx, cust.ID)
	if expiryEntries != 2 || expiryTotal != 10000 {
		t.Errorf("after concurrent sweeps: %d expiry entries totalling %d, want exactly 2 totalling 10000 (one per grant, never doubled)", expiryEntries, expiryTotal)
	}
	if sum != 0 {
		t.Errorf("ledger SUM after concurrent sweeps: got %d, want 0", sum)
	}
	if consumed := grantConsumed(t, db, ctx, grant2); consumed != 3000 {
		t.Errorf("grant2 consumed_cents: got %d, want 3000", consumed)
	}
}

// TestExpireGrant_ConcurrentBackdatedApply is the playbook collision
// test: a sweep and a backdated apply race on the same grant across
// repeated rounds. Whichever side wins the row lock, the money
// invariants hold: the ledger SUM never goes negative, the expired
// grant ends fully retired, block attribution equals the balance (no
// drift), and expiry+usage+remaining always account for exactly the
// granted total.
func TestExpireGrant_ConcurrentBackdatedApply(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "ExpiryApplyRace")
	custStore := customer.NewPostgresStore(db)
	store := credit.NewPostgresStore(db)
	svc := credit.NewService(store)
	invoiceStore := invoice.NewPostgresStore(db)

	const rounds = 8
	for i := 0; i < rounds; i++ {
		cust, err := custStore.Create(ctx, tenantID, domain.Customer{
			ExternalID: "cus_race_" + string(rune('a'+i)), DisplayName: "Race",
		})
		if err != nil {
			t.Fatalf("round %d: create customer: %v", i, err)
		}
		expiresAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
		grantID := seedExpiredGrant(t, db, ctx, svc, tenantID, cust.ID, 10000, expiresAt)
		// Second, active block so the apply has an eligible target when
		// it loses the race for the expiring grant.
		if _, err := svc.Grant(ctx, tenantID, credit.GrantInput{
			CustomerID: cust.ID, AmountCents: 5000, Description: "active block",
		}); err != nil {
			t.Fatalf("round %d: grant active block: %v", i, err)
		}
		inv := createOpenInvoice(t, ctx, invoiceStore, tenantID, cust.ID, 12000)

		var applied int64
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			if _, errs := svc.ExpireCredits(ctx); len(errs) != 0 {
				t.Errorf("round %d: sweep errors: %v", i, errs)
			}
		}()
		go func() {
			defer wg.Done()
			<-start
			a, err := svc.ApplyToInvoiceAt(ctx, tenantID, cust.ID, inv.ID, 12000, expiresAt.Add(-time.Minute))
			if err != nil {
				t.Errorf("round %d: backdated apply: %v", i, err)
			}
			applied = a
		}()
		close(start)
		wg.Wait()

		sum, expiryEntries, expiryTotal := customerLedgerFacts(t, db, ctx, cust.ID)
		if sum < 0 {
			t.Fatalf("round %d: NEGATIVE ledger SUM %d (applied=%d expired=%d)", i, sum, applied, expiryTotal)
		}
		// Conservation: granted == applied + expired + what's left.
		if got := 15000 - applied - expiryTotal; got != sum {
			t.Errorf("round %d: conservation broken: 15000 - applied(%d) - expired(%d) = %d, ledger says %d",
				i, applied, expiryTotal, got, sum)
		}
		if expiryEntries > 1 {
			t.Errorf("round %d: %d expiry entries, want at most 1", i, expiryEntries)
		}
		if consumed := grantConsumed(t, db, ctx, grantID); consumed != 10000 {
			t.Errorf("round %d: expiring grant consumed_cents: got %d, want 10000 (fully retired: drained, expired, or both)", i, consumed)
		}
	}
}

// TestAdjustAtomic_ClawbackRequiresEligibleBlocks: the raw-SUM balance
// gate counts expired-but-unswept headroom that drainPositiveBlocks
// refuses to drain. Pre-fix the -$60 clawback discarded the drain
// result and inserted anyway — SUM dropped to $40 while block
// remaining said $100, and the later sweep (-$100) drove the ledger to
// -$60. Post-fix the clawback fails loudly; after the sweep it fails
// on the balance gate; with an active block present it succeeds.
func TestAdjustAtomic_ClawbackRequiresEligibleBlocks(t *testing.T) {
	db, ctx, tenantID, cust, _, svc, _ := expirySetup(t, "AdjustExpiredUnswept")
	expiresAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	seedExpiredGrant(t, db, ctx, svc, tenantID, cust.ID, 10000, expiresAt)

	// Only headroom is expired-unswept: clawback must fail loudly, not
	// book a deduction nothing drained.
	if _, err := svc.Adjust(ctx, tenantID, credit.AdjustInput{
		CustomerID: cust.ID, AmountCents: -6000, Description: "clawback vs expired",
	}); err == nil {
		t.Fatal("clawback against expired-unswept headroom succeeded; want loud failure")
	}
	sum, _, _ := customerLedgerFacts(t, db, ctx, cust.ID)
	if sum != 10000 {
		t.Fatalf("ledger SUM after rejected clawback: got %d, want 10000 (nothing booked)", sum)
	}

	// After the sweep retires the grant, the same clawback fails the
	// plain balance gate (balance is now 0).
	if _, errs := svc.ExpireCredits(ctx); len(errs) != 0 {
		t.Fatalf("sweep errors: %v", errs)
	}
	if _, err := svc.Adjust(ctx, tenantID, credit.AdjustInput{
		CustomerID: cust.ID, AmountCents: -6000, Description: "clawback vs zero",
	}); err == nil {
		t.Fatal("clawback against zero balance succeeded; want failure")
	}

	// Positive control: an active block absorbs the clawback fully.
	if _, err := svc.Grant(ctx, tenantID, credit.GrantInput{
		CustomerID: cust.ID, AmountCents: 8000, Description: "active block",
	}); err != nil {
		t.Fatalf("grant active block: %v", err)
	}
	if _, err := svc.Adjust(ctx, tenantID, credit.AdjustInput{
		CustomerID: cust.ID, AmountCents: -6000, Description: "clawback vs active",
	}); err != nil {
		t.Fatalf("clawback against active block: %v", err)
	}
	sum, _, _ = customerLedgerFacts(t, db, ctx, cust.ID)
	if sum != 2000 {
		t.Errorf("ledger SUM: got %d, want 2000 (10000 expired + 8000 - 6000)", sum)
	}
}

// TestAdjust_CapsPositiveAmount: positive adjustments are grant-shaped
// inflows and carry Grant's $1M cap (pre-fix they were unbounded).
func TestAdjust_CapsPositiveAmount(t *testing.T) {
	_, ctx, tenantID, cust, _, svc, _ := expirySetup(t, "AdjustCap")
	if _, err := svc.Adjust(ctx, tenantID, credit.AdjustInput{
		CustomerID: cust.ID, AmountCents: 100_000_001, Description: "fat finger",
	}); err == nil {
		t.Fatal("positive adjustment above $1M cap succeeded; want 422")
	}
	if _, err := svc.Adjust(ctx, tenantID, credit.AdjustInput{
		CustomerID: cust.ID, AmountCents: 100_000_000, Description: "at cap",
	}); err != nil {
		t.Fatalf("positive adjustment at cap: %v", err)
	}
}
