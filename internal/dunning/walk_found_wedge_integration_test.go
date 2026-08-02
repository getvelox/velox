package dunning_test

import (
	"context"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/dunning"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// Two pins from the 2026-08-02 ADR-108 walk, recorded with their honest
// provenance because the walk's first reading of them was WRONG and the
// correction is the valuable part:
//
// A bypass-inserted fixture (a live-mode run on a test-mode invoice+policy —
// trigger 0021 had silently overwritten the intended livemode) wedged in the
// live scheduler pass, ERROR-logging "get bound policy … not found" every
// tick. First reading: "ListDueRuns is missing a livemode predicate" — the
// #13 bug class. That reading was REFUTED by its own mutation check: the
// added predicate's mutation SURVIVED, because invoice_dunning_runs carries
// RLS scoping livemode to the GUC and ListDueRuns runs under TxTenant —
// cross-mode selection is impossible through real flows, and the explicit
// #13-class clauses belong only on TxBypass queries where RLS does not apply.
//
//  1. TestListDueRuns_LivemodeIsolation pins the PROPERTY (mode isolation)
//     without caring that RLS rather than a WHERE clause enforces it — so a
//     future rewrite of this query to TxBypass without adding the explicit
//     clause fails loudly here.
//  2. TestProcessDueRuns_TerminalInvoiceResolvesWithoutPolicy pins the
//     processRun reorder: terminal resolution needs no policy, so an
//     unloadable policy must not strand a terminal-invoice run. No REAL path
//     to an unloadable policy is known today (FK RESTRICT; same-mode
//     binding); the ordering is robustness against the class.

// TestListDueRuns_LivemodeIsolation: a live-mode run must be invisible to the
// test-mode sweep. Enforced by RLS today (TxTenant + the livemode row policy),
// not by a WHERE clause — this test pins the property, not the mechanism.
func TestListDueRuns_LivemodeIsolation(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "DueRuns Mode Isolation")

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_mode_iso", DisplayName: "Mode Iso",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	dstore := dunning.NewPostgresStore(db)
	policy, err := dstore.UpsertPolicy(ctx, tenantID, domain.DunningPolicy{
		Name: "default", Enabled: true, RetrySchedule: []string{"72h"}, MaxRetryAttempts: 3,
		FinalAction: domain.DunningFinalAction("mark_uncollectible"), GracePeriodDays: 3,
	})
	if err != nil {
		t.Fatalf("upsert policy: %v", err)
	}
	istore := invoice.NewPostgresStore(db)
	now := time.Now().UTC()

	mkRun := func(num, runID string, livemode bool) {
		inv, err := istore.Create(ctx, tenantID, domain.Invoice{
			CustomerID: cust.ID, InvoiceNumber: num, Status: domain.InvoiceFinalized,
			PaymentStatus: domain.PaymentFailed, Currency: "USD",
			SubtotalCents: 1000, TotalAmountCents: 1000, AmountDueCents: 1000,
			BillingPeriodStart: now.Add(-time.Hour), BillingPeriodEnd: now, IssuedAt: &now,
		})
		if err != nil {
			t.Fatalf("create %s: %v", num, err)
		}
		tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer postgres.Rollback(tx)
		// Trigger 0021 autosets livemode from the GUC and would silently
		// overwrite $6 — pin it per-row (found the hard way: both fixture runs
		// came out live and the control failed).
		guc := "off"
		if livemode {
			guc = "on"
		}
		if _, err := tx.ExecContext(ctx, `SELECT set_config('app.livemode', $1, true)`, guc); err != nil {
			t.Fatalf("pin livemode GUC: %v", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO invoice_dunning_runs (id, tenant_id, invoice_id, customer_id, policy_id, state, reason, attempt_count, next_action_at, paused, created_at, updated_at, livemode)
			VALUES ($1, $2, $3, $4, $5, 'active', 'payment_failed', 0, now() - interval '1 hour', false, now(), now(), $6)`,
			runID, tenantID, inv.ID, cust.ID, policy.ID, livemode); err != nil {
			t.Fatalf("insert run %s: %v", runID, err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}

	mkRun("INV-MODE-TEST", "run_mode_test", false)
	mkRun("INV-MODE-LIVE", "run_mode_live", true)

	due, err := dstore.ListDueRuns(ctx, tenantID, now.Add(time.Minute), 50)
	if err != nil {
		t.Fatalf("ListDueRuns: %v", err)
	}
	got := map[string]bool{}
	for _, r := range due {
		got[r.ID] = true
	}
	if !got["run_mode_test"] {
		t.Error("the test-mode run was not selected by the test-mode sweep — the control")
	}
	if got["run_mode_live"] {
		t.Error("a LIVE-mode run was selected by the TEST-mode sweep — cross-mode dunning processing; with SKIP LOCKED this wedges the run forever (the wrong pass locks it and errors, the right pass skips it)")
	}
}

// TestProcessDueRuns_TerminalInvoiceResolvesWithoutPolicy: a run whose policy
// row CANNOT be loaded must still resolve when its invoice is terminal — the
// resolve needs no policy, and erroring before the terminal check stranded
// such runs forever.
func TestProcessDueRuns_TerminalInvoiceResolvesWithoutPolicy(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Terminal Without Policy")

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_no_policy", DisplayName: "No Policy",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	istore := invoice.NewPostgresStore(db)
	now := time.Now().UTC()
	inv, err := istore.Create(ctx, tenantID, domain.Invoice{
		CustomerID: cust.ID, InvoiceNumber: "INV-NOPOLICY", Status: domain.InvoiceFinalized,
		PaymentStatus: domain.PaymentFailed, Currency: "USD",
		SubtotalCents: 1000, TotalAmountCents: 1000, AmountDueCents: 1000,
		BillingPeriodStart: now.Add(-time.Hour), BillingPeriodEnd: now, IssuedAt: &now,
	})
	if err != nil {
		t.Fatalf("create invoice: %v", err)
	}
	// Write the invoice off — terminal. (payment_status='failed' here; the
	// walk's original was 'unknown', but the wedge is status-driven.)
	if _, err := istore.UpdateStatus(ctx, tenantID, inv.ID, domain.InvoiceUncollectible); err != nil {
		t.Fatalf("write off: %v", err)
	}

	dstore := dunning.NewPostgresStore(db)
	// The policy_id FK is RESTRICT, so "deleted policy" cannot be seeded
	// directly. Reproduce the EXACT walk shape instead: a policy that EXISTS
	// but is invisible to this pass's mode — created under LIVE, referenced by
	// a TEST run. GetPolicyByID under the test ctx cannot see it (mode-scoped
	// read), which is precisely the not-found the walk hit.
	livePolicy, err := dstore.UpsertPolicy(postgres.WithLivemode(context.Background(), true), tenantID, domain.DunningPolicy{
		Name: "live-only", Enabled: true, RetrySchedule: []string{"72h"}, MaxRetryAttempts: 3,
		FinalAction: domain.DunningFinalAction("mark_uncollectible"), GracePeriodDays: 3,
	})
	if err != nil {
		t.Fatalf("upsert live policy: %v", err)
	}
	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.livemode', 'off', true)`); err != nil {
		t.Fatalf("pin livemode GUC: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO invoice_dunning_runs (id, tenant_id, invoice_id, customer_id, policy_id, state, reason, attempt_count, next_action_at, paused, created_at, updated_at, livemode)
		VALUES ('run_no_policy', $1, $2, $3, $4, 'active', 'payment_failed', 0, now() - interval '1 hour', false, now(), now(), false)`,
		tenantID, inv.ID, cust.ID, livePolicy.ID); err != nil {
		t.Fatalf("insert run: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	svc := dunning.NewService(dstore, nil, nil)
	svc.SetSubscriptionPauser(nil, istore) // wires the InvoiceGetter the terminal check reads

	if _, errs := svc.ProcessDueRuns(ctx, tenantID, 50); len(errs) != 0 {
		t.Fatalf("processing a terminal-invoice run with a missing policy must not error — the resolve needs no policy: %v", errs)
	}

	run, err := dstore.GetRunByInvoice(ctx, tenantID, inv.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if run.State != domain.DunningResolved {
		t.Fatalf("run state = %q, want resolved — a terminal invoice's run must resolve even when its policy row is gone", run.State)
	}
	if run.Resolution != domain.ResolutionManuallyResolved {
		t.Errorf("resolution = %q, want manually_resolved (the invoice_<status> terminal branch)", run.Resolution)
	}
}
