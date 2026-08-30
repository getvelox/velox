package dunning_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/dunning"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestUpdateRunIfActive_AttemptCountCAS is the real-Postgres proof of the
// ha-8 attempt-count CAS: of N processors that all read attempt_count=0 and
// try to record attempt 1, exactly ONE applies; and a stale processor's
// rewind (derived from the pre-race count) is refused rather than clobbering
// the winner's recorded attempt back down — the clobber under-counted a
// tenant's retry budget by one charge per overlap. Mutation check: drop the
// `attempt_count = $11` predicate → every racer applies.
func TestUpdateRunIfActive_AttemptCountCAS(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Attempt CAS")
	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{ExternalID: "cus_acas", DisplayName: "ACAS"})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	store := dunning.NewPostgresStore(db)
	policy, err := store.UpsertPolicy(ctx, tenantID, domain.DunningPolicy{
		Name: "default", Enabled: true, RetrySchedule: []string{"72h"}, MaxRetryAttempts: 3,
		FinalSubscriptionAction: domain.SubActionNone, FinalInvoiceAction: domain.InvActionMarkUncollectible, GracePeriodDays: 3,
	})
	if err != nil {
		t.Fatalf("upsert policy: %v", err)
	}
	now := time.Now().UTC()
	inv, err := invoice.NewPostgresStore(db).Create(ctx, tenantID, domain.Invoice{
		CustomerID: cust.ID, InvoiceNumber: "INV-ACAS", Status: domain.InvoiceFinalized,
		PaymentStatus: domain.PaymentFailed, Currency: "USD", SubtotalCents: 5000,
		TotalAmountCents: 5000, AmountDueCents: 5000, BillingPeriodStart: now.Add(-time.Hour),
		BillingPeriodEnd: now, IssuedAt: &now,
	})
	if err != nil {
		t.Fatalf("create invoice: %v", err)
	}
	run, err := store.CreateRun(ctx, tenantID, domain.InvoiceDunningRun{
		InvoiceID: inv.ID, CustomerID: cust.ID, PolicyID: policy.ID,
		State: domain.DunningActive, Reason: "payment_failed",
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	// N processors, all planned from the same read (attempt_count=0).
	const racers = 12
	var wg sync.WaitGroup
	applied := make(chan int, racers)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := run
			r.AttemptCount = 1
			at := now.Add(time.Duration(i) * time.Millisecond)
			r.LastAttemptAt = &at
			ok, err := store.UpdateRunIfActive(ctx, tenantID, r, 0)
			if err != nil {
				t.Errorf("racer %d: %v", i, err)
				return
			}
			if ok {
				applied <- i
			}
		}(i)
	}
	wg.Wait()
	close(applied)
	winners := 0
	for range applied {
		winners++
	}
	if winners != 1 {
		t.Fatalf("%d racers applied the same attempt; want exactly 1", winners)
	}

	// The stale loser's transient-skip rewind: it believes it wrote attempt 1
	// and rewinds to 0, proving expected=1 — but the winner owns attempt 1,
	// and the loser never charged. Before ha-8 this write applied blindly and
	// erased the winner's attempt. It must still be refused when the count
	// has moved on (the winner charged again → count=2).
	{
		tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE invoice_dunning_runs SET attempt_count = 2 WHERE id = $1`, run.ID); err != nil {
			t.Fatalf("advance count: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	stale := run
	stale.AttemptCount = 0
	if ok, err := store.UpdateRunIfActive(ctx, tenantID, stale, 1); err != nil {
		t.Fatalf("stale rewind: %v", err)
	} else if ok {
		t.Fatal("a rewind derived from a superseded attempt applied — it just erased another processor's recorded attempt")
	}
	got, err := store.GetRun(ctx, tenantID, run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.AttemptCount != 2 {
		t.Fatalf("attempt_count = %d, want 2 (the stale write must change nothing)", got.AttemptCount)
	}
}
