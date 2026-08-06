package doctor_test

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/sagarsuperuser/velox/internal/doctor"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestDoctor_CleanSchemaIsQuiet_AndSeededViolationsAreCaught is the doctor's
// own positive control, in both directions.
//
// Direction 1 (quiet): every check must RUN against the real schema (a check
// that errors is schema drift — the doctor's own rot alarm) and a fresh
// database must produce ZERO violations, because the exactness rule says a
// check that flags legal states does not ship.
//
// Direction 2 (loud): a seeded violation must be caught by NAME. Without
// this arm, a doctor whose SQL silently matched nothing would look
// permanently healthy — the mirror image of crying wolf.
func TestDoctor_CleanSchemaIsQuiet_AndSeededViolationsAreCaught(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// The doctor MUST run as a role that sees every row (the table owner /
	// an admin DSN). This test originally ran it on the RLS-scoped app pool
	// and the doctor reported a spotless bill of health over rows it could
	// not see — the exact silent-blindness failure this comment now warns
	// about. cmd/velox-doctor documents the same requirement.
	adminURL := strings.TrimSpace(os.Getenv("TEST_ADMIN_DATABASE_URL"))
	if adminURL == "" {
		adminURL = "postgres://velox:velox@localhost:5432/velox_test?sslmode=disable"
	}
	admin, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatalf("open admin pool: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close() })

	// Direction 1 — all checks run, zero violations on a fresh DB.
	res := doctor.Run(ctx, admin, doctor.Checks)
	for _, e := range res.Errors {
		t.Errorf("check failed to run (schema drift?): %v", e)
	}
	for _, v := range res.Violations {
		t.Errorf("fresh database flagged by %s: row %s (%s) — either the seed data is illegal or the check is not exact",
			v.Check.Name, v.RowID, v.Detail)
	}

	// Direction 2 — seed two violations from different domains, raw SQL on
	// purpose: the states must be unreachable through any writer.
	tenantID := testutil.CreateTestTenant(t, db, "Doctor Loud")
	mustExec := func(q string, args ...any) {
		t.Helper()
		// TxBypass: the seeds are deliberately-illegal states no writer can
		// produce; RLS on the pool would (correctly) refuse them.
		tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
		if err != nil {
			t.Fatalf("seed begin: %v", err)
		}
		defer postgres.Rollback(tx)
		if _, err := tx.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("seed commit: %v", err)
		}
	}
	mustExec(`INSERT INTO customers (id, tenant_id, external_id, display_name, email)
		VALUES ('cus_doc1', $1, 'doc1', 'Doc', '')`, tenantID)
	// paid/succeeded with amount_due != 0 and paid_at NULL — violates both
	// invoice_paid_settled_coherence and invoice_due_conservation.
	mustExec(`INSERT INTO invoices (id, tenant_id, customer_id, invoice_number, status, payment_status,
			currency, subtotal_cents, total_amount_cents, amount_due_cents, amount_paid_cents, credits_applied_cents,
			billing_period_start, billing_period_end)
		VALUES ('vlx_inv_doc1', $1, 'cus_doc1', 'DOC-1', 'paid', 'succeeded', 'USD', 4000, 4000, 4000, 4000, 0,
			now() - interval '30 days', now())`, tenantID)
	// detached card still default — the charge-a-removed-card hazard.
	mustExec(`INSERT INTO payment_methods (id, tenant_id, livemode, customer_id, stripe_payment_method_id,
			type, is_default, detached_at, created_at, updated_at)
		VALUES ('vlx_pm_doc1', $1, false, 'cus_doc1', 'pm_doc1', 'card', true, now(), now(), now())`, tenantID)

	res = doctor.Run(ctx, admin, doctor.Checks)
	want := map[string]bool{
		"invoice_paid_settled_coherence":             false,
		"payment_methods_detached_but_still_default": false,
	}
	for _, v := range res.Violations {
		if _, ok := want[v.Check.Name]; ok && strings.Contains(v.RowID+v.Detail, "doc1") {
			want[v.Check.Name] = true
		}
	}
	for name, caught := range want {
		if !caught {
			t.Errorf("seeded violation NOT caught by %s — the check's SQL matches nothing", name)
			for _, e := range res.Errors {
				t.Logf("  check error: %v", e)
			}
			for _, v := range res.Violations {
				t.Logf("  saw: %s %s %s", v.Check.Name, v.RowID, v.Detail)
			}
		}
	}
}
