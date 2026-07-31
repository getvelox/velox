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

// TestListDueRuns_ParkedSourceGate pins both halves of the ADR-107 parked-source
// exclusion, because each half fixes a bug the other one would cause.
//
// EXCLUDE while the invoice is open: a parked invoice ('unknown' with no
// PaymentIntent id) is admitted by no charge path, so the retry adapter returns
// ErrTransientSkip, and the transient handler deliberately does not reschedule —
// correct for a Stripe blip, endless for an invoice that will never be
// chargeable. The run was re-selected every tick forever, and because
// next_action_at is frozen in the past while this scan is next_action_at ASC
// with a LIMIT, parked runs permanently headed the queue: at LIMIT of them real
// dunning stops entirely. Same starvation shape ADR-107 already hit in
// ListUnknownPayments.
//
// KEEP once the invoice is terminal: the run is resolved by the processing
// path's own terminal branch, and markUncollectible's ResolveByInvoice is
// best-effort, documented as relying on dunning scanning invoice status "on next
// tick anyway". An exclusion that also hid written-off invoices would delete
// that backstop and strand the run active forever — which is precisely the bug
// the clawback scan had (a draft excluded on payment state alone could never
// reach the code that voids it). Fixing one starvation by creating another is
// the failure mode this test exists to prevent.
func TestListDueRuns_ParkedSourceGate(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "DueRuns Parked Gate")

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_parked_due", DisplayName: "Parked Due",
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

	// mkDueRun builds a finalized invoice in the given payment shape plus an
	// active dunning run forced due, then applies the invoice status (a write-off
	// is a status change only — payment_status deliberately stays 'unknown',
	// since nobody ever learned whether the card was charged).
	mkDueRun := func(num string, ps domain.InvoicePaymentStatus, piID string, status domain.InvoiceStatus) string {
		inv, err := istore.Create(ctx, tenantID, domain.Invoice{
			CustomerID: cust.ID, InvoiceNumber: num, Status: domain.InvoiceFinalized,
			PaymentStatus: ps, Currency: "USD",
			SubtotalCents: 5000, TotalAmountCents: 5000, AmountDueCents: 5000,
			BillingPeriodStart: now.Add(-time.Hour), BillingPeriodEnd: now, IssuedAt: &now,
		})
		if err != nil {
			t.Fatalf("create invoice %s: %v", num, err)
		}
		// Create does NOT persist stripe_payment_intent_id — that column is only
		// ever written by the payment path. Setting it on the struct silently did
		// nothing, which made the "unknown WITH a PI" control indistinguishable
		// from a parked invoice and the control vacuous. Write it the real way.
		if piID != "" {
			if _, err := istore.UpdatePayment(ctx, tenantID, inv.ID, ps, piID, "", nil); err != nil {
				t.Fatalf("attach payment intent to %s: %v", num, err)
			}
			after, gerr := istore.Get(ctx, tenantID, inv.ID)
			if gerr != nil {
				t.Fatalf("get invoice %s: %v", num, gerr)
			}
			if after.StripePaymentIntentID != piID {
				t.Fatalf("%s: PaymentIntent id did not persist (got %q) — the control below would pass for the wrong reason", num, after.StripePaymentIntentID)
			}
		}
		run, err := dstore.CreateRun(ctx, tenantID, domain.InvoiceDunningRun{
			InvoiceID: inv.ID, CustomerID: cust.ID, PolicyID: policy.ID,
			State: domain.DunningActive, Reason: "payment_failed",
		})
		if err != nil {
			t.Fatalf("create run %s: %v", num, err)
		}
		if status != domain.InvoiceFinalized {
			if _, err := istore.UpdateStatus(ctx, tenantID, inv.ID, status); err != nil {
				t.Fatalf("set invoice %s status %s: %v", num, status, err)
			}
			after, gerr := istore.Get(ctx, tenantID, inv.ID)
			if gerr != nil {
				t.Fatalf("get invoice %s: %v", num, gerr)
			}
			if after.PaymentStatus != ps {
				t.Fatalf("%s: status change moved payment_status %q → %q; a write-off closes the invoice, it does not answer whether the card was charged", num, ps, after.PaymentStatus)
			}
		}
		tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE invoice_dunning_runs SET next_action_at = now() - interval '1 hour' WHERE id = $1`, run.ID); err != nil {
			t.Fatalf("set next_action_at: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
		return run.ID
	}

	parkedOpen := mkDueRun("INV-PARKED-OPEN", domain.PaymentUnknown, "", domain.InvoiceFinalized)
	parkedWrittenOff := mkDueRun("INV-PARKED-WROFF", domain.PaymentUnknown, "", domain.InvoiceUncollectible)
	unknownWithPI := mkDueRun("INV-UNKNOWN-PI", domain.PaymentUnknown, "pi_live_1", domain.InvoiceFinalized)
	ordinaryFailed := mkDueRun("INV-FAILED", domain.PaymentFailed, "", domain.InvoiceFinalized)

	due, err := dstore.ListDueRuns(ctx, tenantID, now.Add(time.Minute), 200)
	if err != nil {
		t.Fatalf("ListDueRuns: %v", err)
	}
	got := map[string]bool{}
	for _, r := range due {
		got[r.ID] = true
	}

	if got[parkedOpen] {
		t.Error("a run whose invoice is PARKED and still open must be excluded — no charge path admits it, so every tick burns an attempt and rewinds it, and its frozen next_action_at permanently heads this LIMITed queue")
	}
	if !got[parkedWrittenOff] {
		t.Error("a run whose parked invoice was WRITTEN OFF must still be selected — the processing path's terminal branch is what resolves it, and mark-uncollectible's own resolve is best-effort by design; excluding it strands the run active forever")
	}
	if !got[unknownWithPI] {
		t.Error("'unknown' WITH a PaymentIntent id is not parked — the provider can still name that attempt and resolve it, so the run must stay due")
	}
	if !got[ordinaryFailed] {
		t.Error("an ordinary failed invoice must remain due — this is the normal dunning path and the control for the assertions above")
	}
}

// TestListDueRunsForClock_ParkedSourceGate is the sim-time twin. It gets its own
// test rather than a shared assertion because the two scans express the same
// rule DIFFERENTLY — the wall-clock scan uses a second NOT EXISTS, the catchup
// scan a NOT(...) over its already-joined invoice row — and a rule written twice
// is a rule that can be right once.
//
// The spin matters more here, not less: a clock advance drives many ticks in a
// burst against a LIMITed, next_action_at-ordered scan, so one parked sim
// invoice can consume the whole catchup budget and stall every other run on the
// clock.
func TestListDueRunsForClock_ParkedSourceGate(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := "tnt_parked_clock"
	const clockID = "vlx_tclk_parked"
	frozen := time.Date(2027, 6, 1, 0, 0, 0, 0, time.UTC)

	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := tx.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	// 0021 livemode-autoset trigger reads the GUC; pin the tx to test mode.
	mustExec(`SELECT set_config('app.livemode', 'off', true)`)
	mustExec(`INSERT INTO tenants (id, name, status) VALUES ($1, 'Parked Clock', 'active')`, tenantID)
	mustExec(`INSERT INTO test_clocks (id, tenant_id, name, frozen_time, status, livemode)
		VALUES ($1, $2, 'parked', $3, 'ready', false)`, clockID, tenantID, frozen)
	mustExec(`INSERT INTO customers (id, tenant_id, external_id, display_name, email, test_clock_id, created_at, updated_at)
		VALUES ('cus_parked_clk', $1, 'parked_clk', 'Parked', '', $2, $3, $3)`, tenantID, clockID, frozen)
	mustExec(`INSERT INTO dunning_policies (id, tenant_id, name, enabled, is_default, retry_schedule, max_retry_attempts, final_action, grace_period_days, livemode)
		VALUES ('dpol_parked', $1, 'P', true, true, '["72h"]', 3, 'pause', 3, false)`, tenantID)

	// payStatus/piID/invStatus reproduce the three shapes; the invoices are
	// simulated so they belong to the catchup scan rather than the wall sweep.
	seedInvRun := func(invID, runID, payStatus, piID, invStatus string) {
		mustExec(`INSERT INTO invoices (id, tenant_id, customer_id, invoice_number,
			status, payment_status, stripe_payment_intent_id, is_simulated, currency,
			subtotal_cents, total_amount_cents, amount_due_cents, tax_status,
			billing_period_start, billing_period_end, created_at, updated_at)
		VALUES ($1, $2, 'cus_parked_clk', $1, $3, $4, NULLIF($5,''), true, 'USD', 2500, 2500, 2500, 'ok', $6, $6, $6, $6)`,
			invID, tenantID, invStatus, payStatus, piID, frozen.Add(-24*time.Hour))
		mustExec(`INSERT INTO invoice_dunning_runs (id, tenant_id, invoice_id, customer_id, policy_id, state, reason, attempt_count, next_action_at, paused, created_at, updated_at, livemode)
		VALUES ($1, $2, $3, 'cus_parked_clk', 'dpol_parked', 'active', 'payment_failed', 0, $4, false, $5, $5, false)`,
			runID, tenantID, invID, frozen.Add(-time.Hour), frozen.Add(-24*time.Hour))
	}
	seedInvRun("inv_clk_parked", "run_clk_parked", "unknown", "", "finalized")
	seedInvRun("inv_clk_wroff", "run_clk_wroff", "unknown", "", "uncollectible")
	seedInvRun("inv_clk_pi", "run_clk_pi", "unknown", "pi_sim_1", "finalized")
	seedInvRun("inv_clk_failed", "run_clk_failed", "failed", "", "finalized")
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	due, err := dunning.NewPostgresStore(db).ListDueRunsForClock(ctx, tenantID, clockID, frozen, 50)
	if err != nil {
		t.Fatalf("ListDueRunsForClock: %v", err)
	}
	got := map[string]bool{}
	for _, r := range due {
		got[r.ID] = true
	}

	if got["run_clk_parked"] {
		t.Error("a PARKED simulated invoice must be excluded from the catchup scan — a clock advance would otherwise spin it every tick and starve the rest of the clock's runs")
	}
	if !got["run_clk_wroff"] {
		t.Error("a written-off parked invoice must STILL be selected in sim time — the processing path's terminal branch is what resolves the run")
	}
	if !got["run_clk_pi"] {
		t.Error("'unknown' WITH a PaymentIntent id is not parked — it can still be named and resolved, so it stays due")
	}
	if !got["run_clk_failed"] {
		t.Error("an ordinary failed simulated invoice must remain due — the control")
	}
}
