package dunning_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/dunning"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// Real-Postgres proof of the SB-1/SB-2 store predicates (fake-fidelity rule:
// the memStore mirrors these, so the SQL itself must be pinned here).

func exhaustedRunPG(t *testing.T, name string) (context.Context, *dunning.PostgresStore, string, domain.InvoiceDunningRun) {
	t.Helper()
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, name)
	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{ExternalID: "cus_" + name, DisplayName: name})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	store := dunning.NewPostgresStore(db)
	policy, err := store.UpsertPolicy(ctx, tenantID, domain.DunningPolicy{
		Name: "default", Enabled: true, RetrySchedule: []string{"72h", "72h"}, MaxRetryAttempts: 3,
		FinalSubscriptionAction: domain.SubActionNone, FinalInvoiceAction: domain.InvActionMarkUncollectible, GracePeriodDays: 3,
	})
	if err != nil {
		t.Fatalf("upsert policy: %v", err)
	}
	now := time.Now().UTC()
	inv, err := invoice.NewPostgresStore(db).Create(ctx, tenantID, domain.Invoice{
		CustomerID: cust.ID, InvoiceNumber: "INV-" + name, Status: domain.InvoiceFinalized,
		PaymentStatus: domain.PaymentFailed, Currency: "USD", SubtotalCents: 5000,
		TotalAmountCents: 5000, AmountDueCents: 5000, BillingPeriodStart: now.Add(-time.Hour),
		BillingPeriodEnd: now, IssuedAt: &now,
	})
	if err != nil {
		t.Fatalf("create invoice: %v", err)
	}
	last := now.Add(-time.Hour)
	next := now.Add(-time.Minute)
	run, err := store.CreateRun(ctx, tenantID, domain.InvoiceDunningRun{
		InvoiceID: inv.ID, CustomerID: cust.ID, PolicyID: policy.ID, State: domain.DunningActive,
		Reason: "payment_failed", AttemptCount: 3, LastAttemptAt: &last, NextActionAt: &next,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	// Re-read so NextActionAt carries the column's exact (microsecond) value,
	// as a due-run listing would.
	run, err = store.GetRun(ctx, tenantID, run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	return ctx, store, tenantID, run
}

// TestClaimExhaustion_Predicate: each predicate component refuses on its
// own, and the match moves next_action_at to the lease. The match arm also
// pins that a timestamptz scanned from the column round-trips through the
// driver exactly (the claim compares it with =).
// Mutations: drop `next_action_at = $5` (stale arm applies); drop
// `state = 'active'` (escalated arm applies).
func TestClaimExhaustion_Predicate(t *testing.T) {
	ctx, store, tenantID, run := exhaustedRunPG(t, "ClaimPred")
	lease := time.Now().UTC().Add(15 * time.Minute).Truncate(time.Microsecond)

	stale := run.NextActionAt.Add(-time.Second)
	if won, err := store.ClaimExhaustion(ctx, tenantID, run.ID, run.AttemptCount, stale, lease); err != nil || won {
		t.Fatalf("stale next_action_at claimed: won=%v err=%v", won, err)
	}
	if won, err := store.ClaimExhaustion(ctx, tenantID, run.ID, run.AttemptCount-1, *run.NextActionAt, lease); err != nil || won {
		t.Fatalf("stale attempt_count claimed: won=%v err=%v", won, err)
	}
	won, err := store.ClaimExhaustion(ctx, tenantID, run.ID, run.AttemptCount, *run.NextActionAt, lease)
	if err != nil || !won {
		t.Fatalf("the listed snapshot must win the claim: won=%v err=%v", won, err)
	}
	got, err := store.GetRun(ctx, tenantID, run.ID)
	if err != nil || got.NextActionAt == nil || !got.NextActionAt.Equal(lease) {
		t.Fatalf("claim did not write the lease: next=%v err=%v", got.NextActionAt, err)
	}
	// The loser holding the same listing now loses.
	if won, err := store.ClaimExhaustion(ctx, tenantID, run.ID, run.AttemptCount, *run.NextActionAt, lease.Add(time.Minute)); err != nil || won {
		t.Fatalf("a second claim on the same listing won: won=%v err=%v", won, err)
	}

	// An escalated row is never claimable — even one that (illegally) still
	// carries a next_action_at, so the state component is proven on its own:
	// a legal escalated row has NULL next_action_at and would fail the
	// equality anyway.
	if _, err := testutil.AdminPool(t).ExecContext(ctx,
		`UPDATE invoice_dunning_runs SET state = 'escalated' WHERE id = $1`, run.ID); err != nil {
		t.Fatalf("seed escalated-with-next: %v", err)
	}
	if won, err := store.ClaimExhaustion(ctx, tenantID, run.ID, run.AttemptCount, lease, lease.Add(time.Minute)); err != nil || won {
		t.Fatalf("an escalated run was claimed: won=%v err=%v", won, err)
	}
}

// TestUpdateRunIfActive_StateAndLivenessGuards (real Postgres): an escalated
// row is never rewritten by a stale active write (SB-2), and an active write
// without next_action_at is refused before touching the row (SB-1).
// Mutations: `state <> 'resolved'` (the stale write reopens the run); delete
// refuseStrandingWrite (the NULL write lands).
func TestUpdateRunIfActive_StateAndLivenessGuards(t *testing.T) {
	ctx, store, tenantID, run := exhaustedRunPG(t, "StateGuard")

	stranded := run
	stranded.NextActionAt = nil
	if _, err := store.UpdateRunIfActive(ctx, tenantID, stranded, run.AttemptCount, nil); err == nil {
		t.Fatal("an active run was written with next_action_at NULL")
	}
	if got, _ := store.GetRun(ctx, tenantID, run.ID); got.NextActionAt == nil {
		t.Fatal("the refused write still changed the row")
	}

	esc := run
	esc.State = domain.DunningEscalated
	esc.Resolution = domain.ResolutionRetriesExhausted
	now := time.Now().UTC()
	esc.ResolvedAt = &now
	esc.NextActionAt = nil
	if ok, err := store.UpdateRunIfActive(ctx, tenantID, esc, run.AttemptCount, nil); err != nil || !ok {
		t.Fatalf("escalate (legal terminal write): ok=%v err=%v", ok, err)
	}
	hooked := 0
	staleRetry := run
	retryAt := now.Add(24 * time.Hour)
	staleRetry.NextActionAt = &retryAt
	staleRetry.Resolution = domain.ResolutionActionFailed
	ok, err := store.UpdateRunIfActive(ctx, tenantID, staleRetry, run.AttemptCount, func(*sql.Tx) error { hooked++; return nil })
	if err != nil || ok || hooked != 0 {
		t.Fatalf("a stale active write applied to an escalated run: ok=%v err=%v hook=%d", ok, err, hooked)
	}
	if got, _ := store.GetRun(ctx, tenantID, run.ID); got.State != domain.DunningEscalated {
		t.Fatalf("escalated run reopened to %s", got.State)
	}
}
