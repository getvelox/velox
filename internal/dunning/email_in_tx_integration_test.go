package dunning_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/dunning"
	"github.com/sagarsuperuser/velox/internal/email"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/leader"
	"github.com/sagarsuperuser/velox/internal/platform/leader/leadertest"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// The two dunning-seam tests of the 2026-08-30 HA program's
// email-money-in-tx item (ADR-040 amendment): the dunning state change and
// its customer email commit in ONE transaction, and an email-side failure
// never vetoes the state change.

type fixedEmailFetcher struct{ email, name string }

func (f fixedEmailFetcher) GetCustomerEmail(context.Context, string, string) (string, string, []string, error) {
	return f.email, f.name, nil, nil
}

type alwaysFailRetrier struct{}

func (alwaysFailRetrier) RetryPayment(context.Context, string, string, string) error {
	return errors.New("card_declined: insufficient_funds")
}

func seedDunningFixture(t *testing.T, db *postgres.DB, ctx context.Context, name string, attempts int) (tenantID string, runID string, store *dunning.PostgresStore) {
	t.Helper()
	tenantID = testutil.CreateTestTenant(t, db, name)
	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{ExternalID: "cus_" + name, DisplayName: name})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	store = dunning.NewPostgresStore(db)
	policy, err := store.UpsertPolicy(ctx, tenantID, domain.DunningPolicy{
		Name: "default", Enabled: true, RetrySchedule: []string{"72h", "72h", "72h"}, MaxRetryAttempts: 3,
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
	due := now.Add(-time.Minute)
	run, err := store.CreateRun(ctx, tenantID, domain.InvoiceDunningRun{
		InvoiceID: inv.ID, CustomerID: cust.ID, PolicyID: policy.ID,
		State: domain.DunningActive, Reason: "payment_failed", AttemptCount: attempts, NextActionAt: &due,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	return tenantID, run.ID, store
}

func countEmailRows(t *testing.T, db *postgres.DB, ctx context.Context, tenantID, emailType string) int {
	t.Helper()
	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatal(err)
	}
	defer postgres.Rollback(tx)
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM email_outbox WHERE tenant_id=$1 AND email_type=$2`, tenantID, emailType).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestDunningWarning_RidesRescheduleTx: a failed retry's reschedule and its
// "we'll try again" warning email commit together — end to end through the
// real service, real dunning store, real email outbox and the real
// OutboxSender. Before ADR-040's amendment the enqueue ran in its own tx
// AFTER the reschedule committed; a failover between the two lost the email
// with the state standing.
func TestDunningWarning_RidesRescheduleTx(t *testing.T) {
	db := testutil.SetupTestDB(t)
	base := postgres.WithLivemode(context.Background(), false)
	ctx := leadertest.Token(t, testutil.AdminPool(t), base, leader.RoleDunning)
	tenantID, runID, store := seedDunningFixture(t, db, ctx, "warn-tx", 0)

	svc := dunning.NewService(store, alwaysFailRetrier{}, nil)
	svc.SetEmailNotifier(email.NewOutboxSender(email.NewOutboxStore(db)))
	svc.SetCustomerEmailFetcher(fixedEmailFetcher{email: "cust@example.test", name: "Warn TX"})

	if n, errs := svc.ProcessDueRuns(ctx, tenantID, 20); len(errs) > 0 {
		t.Fatalf("process due runs: n=%d errs=%v", n, errs)
	}
	got, err := store.GetRun(ctx, tenantID, runID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AttemptCount != 1 || got.NextActionAt == nil {
		t.Fatalf("run not rescheduled: count=%d next=%v", got.AttemptCount, got.NextActionAt)
	}
	if n := countEmailRows(t, db, ctx, tenantID, email.TypeDunningWarning); n != 1 {
		t.Fatalf("dunning_warning outbox rows = %d, want 1 (committed with the reschedule)", n)
	}
}

// TestDunningEscalation_RidesEscalationTx: exhaustion's escalated write and
// the escalation email commit together.
func TestDunningEscalation_RidesEscalationTx(t *testing.T) {
	db := testutil.SetupTestDB(t)
	base := postgres.WithLivemode(context.Background(), false)
	ctx := leadertest.Token(t, testutil.AdminPool(t), base, leader.RoleDunning)
	tenantID, runID, store := seedDunningFixture(t, db, ctx, "esc-tx", 3) // at max

	svc := dunning.NewService(store, alwaysFailRetrier{}, nil)
	svc.SetEmailNotifier(email.NewOutboxSender(email.NewOutboxStore(db)))
	svc.SetCustomerEmailFetcher(fixedEmailFetcher{email: "cust@example.test", name: "Esc TX"})

	if _, errs := svc.ProcessDueRuns(ctx, tenantID, 20); len(errs) > 0 {
		t.Fatalf("process due runs: errs=%v", errs)
	}
	got, err := store.GetRun(ctx, tenantID, runID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != domain.DunningEscalated {
		t.Fatalf("state = %q, want escalated", got.State)
	}
	if n := countEmailRows(t, db, ctx, tenantID, email.TypeDunningEscalation); n != 1 {
		t.Fatalf("dunning_escalation outbox rows = %d, want 1", n)
	}
}

// TestUpdateRunIfActive_HookGatedOnApplied: a stale snapshot's write applies
// nothing AND its email hook never runs — an email describing a transition
// that did not happen must not exist. Mutation check: run the hook
// regardless of RowsAffected → this fails.
func TestUpdateRunIfActive_HookGatedOnApplied(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID, runID, store := seedDunningFixture(t, db, ctx, "hook-gate", 2)
	outbox := email.NewOutboxStore(db)

	run, err := store.GetRun(ctx, tenantID, runID)
	if err != nil {
		t.Fatal(err)
	}
	stale := run
	stale.AttemptCount = 3
	hookRan := false
	applied, err := store.UpdateRunIfActive(ctx, tenantID, stale, 1 /* stale: row holds 2 */, func(tx *sql.Tx) error {
		hookRan = true
		_, e := outbox.Enqueue(ctx, tx, tenantID, email.TypeDunningWarning, map[string]any{"to": "x@example.test"})
		return e
	})
	if err != nil {
		t.Fatal(err)
	}
	if applied || hookRan {
		t.Fatalf("stale write: applied=%v hookRan=%v — both must be false", applied, hookRan)
	}
	if n := countEmailRows(t, db, ctx, tenantID, email.TypeDunningWarning); n != 0 {
		t.Fatalf("an email row exists for a transition that never happened (%d rows)", n)
	}
}

// TestUpdateRunIfActive_HookErrorNeverVetoes: the hook's enqueue lands and
// is then rolled back to the SAVEPOINT when the hook errors — proving the
// enqueue rides the SAME transaction — while the state change itself still
// commits (non-vetoing). Mutation check: propagate the hook error instead of
// rolling back to the savepoint → the state change is lost → this fails.
func TestUpdateRunIfActive_HookErrorNeverVetoes(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID, runID, store := seedDunningFixture(t, db, ctx, "hook-veto", 0)
	outbox := email.NewOutboxStore(db)

	run, err := store.GetRun(ctx, tenantID, runID)
	if err != nil {
		t.Fatal(err)
	}
	run.AttemptCount = 1
	now := time.Now().UTC()
	run.LastAttemptAt = &now
	applied, err := store.UpdateRunIfActive(ctx, tenantID, run, 0, func(tx *sql.Tx) error {
		if _, e := outbox.Enqueue(ctx, tx, tenantID, email.TypeDunningWarning, map[string]any{"to": "x@example.test"}); e != nil {
			return e
		}
		return errors.New("template render exploded")
	})
	if err != nil {
		t.Fatalf("a failing email hook must not surface as a write error: %v", err)
	}
	if !applied {
		t.Fatal("the state change must apply even when the email hook fails")
	}
	got, err := store.GetRun(ctx, tenantID, runID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AttemptCount != 1 {
		t.Fatalf("attempt_count = %d, want 1 — the money write must have committed", got.AttemptCount)
	}
	if n := countEmailRows(t, db, ctx, tenantID, email.TypeDunningWarning); n != 0 {
		t.Fatalf("the failed hook's enqueue survived (%d rows) — it must roll back to the savepoint", n)
	}
}
