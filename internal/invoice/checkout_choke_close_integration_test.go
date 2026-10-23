package invoice_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/credit"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/subscription"
	"github.com/sagarsuperuser/velox/internal/testutil"
	"github.com/sagarsuperuser/velox/internal/webhook"
)

// seedOpenClaim inserts an open checkout claim for the invoice (raw SQL —
// the payment package owns the store; these tests lock the INVOICE store's
// choke-point behavior).
func seedOpenClaim(t *testing.T, db *postgres.DB, ctx context.Context, tenantID, invoiceID string) string {
	t.Helper()
	id := postgres.NewID("vlx_cks")
	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO checkout_sessions (id, tenant_id, invoice_id, livemode, amount_cents, currency, status)
		VALUES ($1, $2, $3, false, 1000, 'USD', 'open')
	`, id, tenantID, invoiceID); err != nil {
		t.Fatalf("seed claim: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return id
}

func claimStatus(t *testing.T, db *postgres.DB, ctx context.Context, tenantID, claimID string) string {
	t.Helper()
	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	var status string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM checkout_sessions WHERE id = $1`, claimID).Scan(&status); err != nil {
		t.Fatalf("read claim: %v", err)
	}
	return status
}

// TestChokePointCloses locks the ADR-068 rule that EVERY exit from the
// payable state closes open checkout claims IN the exiting transaction —
// at the store choke points, not per-call-site hooks. Mutation seam: strip
// any of the three close statements and its subtest fails with status
// 'open' (the pre-fix "stored open session for a settled invoice" race).
func TestChokePointCloses(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	now := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)

	t.Run("mark paid settles claims", func(t *testing.T) {
		tenantID := testutil.CreateTestTenant(t, db, "Choke MarkPaid")
		store := invoice.NewPostgresStore(db)
		invID := seedFinalizedInvoice(t, db, store, ctx, tenantID)
		claimID := seedOpenClaim(t, db, ctx, tenantID, invID)
		if _, err := store.MarkPaid(ctx, tenantID, invID, "pi_x", now); err != nil {
			t.Fatalf("mark paid: %v", err)
		}
		if got := claimStatus(t, db, ctx, tenantID, claimID); got != "invoice_settled" {
			t.Fatalf("claim after MarkPaid = %q, want invoice_settled (in-tx choke close)", got)
		}
	})

	t.Run("void settles claims", func(t *testing.T) {
		tenantID := testutil.CreateTestTenant(t, db, "Choke Void")
		store := invoice.NewPostgresStore(db)
		invID := seedFinalizedInvoice(t, db, store, ctx, tenantID)
		claimID := seedOpenClaim(t, db, ctx, tenantID, invID)
		if _, err := store.UpdateStatus(ctx, tenantID, invID, domain.InvoiceVoided); err != nil {
			t.Fatalf("void: %v", err)
		}
		if got := claimStatus(t, db, ctx, tenantID, claimID); got != "invoice_settled" {
			t.Fatalf("claim after void = %q, want invoice_settled", got)
		}
	})

	// Every exit to paid releases a dunning collection pause in the paid
	// transaction (ADR-036 amendment 2026-10-22), through the same choke
	// point (markPaidLockedTx). Each exit is the store call its path makes:
	// the card settle (payment.SettleSucceeded), the reconciler's bare
	// MarkPaid, an offline payment, the credits-cover settle (Collect and
	// a dunning retry with nothing due), and a credit note that leaves
	// nothing due (SettleZeroDueTx on the note's transaction). Mutation:
	// drop the release call in markPaidLockedTx and every subtest fails
	// with resumes_at NULL.
	t.Run("every paid exit releases a dunning pause", func(t *testing.T) {
		exits := []struct {
			name    string
			zeroDue bool
			pay     func(store *invoice.PostgresStore, tenantID, invID string, at time.Time) error
		}{
			{"card settlement", false, func(store *invoice.PostgresStore, tenantID, invID string, at time.Time) error {
				_, _, err := store.MarkPaidCardSettlementTransition(ctx, tenantID, invID, "pi_card", at, nil)
				return err
			}},
			{"reconciler mark paid", false, func(store *invoice.PostgresStore, tenantID, invID string, at time.Time) error {
				_, err := store.MarkPaid(ctx, tenantID, invID, "pi_reconciled", at)
				return err
			}},
			{"offline payment", false, func(store *invoice.PostgresStore, tenantID, invID string, at time.Time) error {
				_, err := store.MarkPaidOffline(ctx, tenantID, invID, at, nil)
				return err
			}},
			{"credits cover it", true, func(store *invoice.PostgresStore, tenantID, invID string, at time.Time) error {
				_, _, err := store.SettleZeroDue(ctx, tenantID, invID, at)
				return err
			}},
			{"credit note leaves nothing due", true, func(store *invoice.PostgresStore, tenantID, invID string, at time.Time) error {
				tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
				if err != nil {
					return err
				}
				defer postgres.Rollback(tx)
				var one int
				if err := tx.QueryRowContext(ctx, `SELECT 1 FROM invoices WHERE id = $1 FOR NO KEY UPDATE`, invID).Scan(&one); err != nil {
					return err
				}
				if _, _, err := store.SettleZeroDueTx(ctx, tx, tenantID, invID, at); err != nil {
					return err
				}
				return tx.Commit()
			}},
		}
		for _, ex := range exits {
			t.Run(ex.name, func(t *testing.T) {
				tenantID := testutil.CreateTestTenant(t, db, "Choke Pause "+ex.name)
				store := invoice.NewPostgresStore(db)
				subs := subscription.NewPostgresStore(db)
				outbox := webhook.NewOutboxStore(db)
				store.SetOutboxEnqueuer(outbox)
				store.SetDunningPauseReleaser(subs)
				invID := seedFinalizedInvoice(t, db, store, ctx, tenantID)
				inv, err := store.Get(ctx, tenantID, invID)
				if err != nil {
					t.Fatalf("get invoice: %v", err)
				}
				subID := inv.SubscriptionID
				if _, outcome, err := subs.SetDunningPause(ctx, tenantID, subID, invID); err != nil || outcome != subscription.DunningPauseWritten {
					t.Fatalf("dunning pause: outcome %v, err %v", outcome, err)
				}
				if ex.zeroDue {
					setDue(t, db, ctx, tenantID, invID, 0)
				}

				paidAt := time.Now().UTC().Truncate(time.Microsecond)
				if err := ex.pay(store, tenantID, invID, paidAt); err != nil {
					t.Fatalf("pay: %v", err)
				}
				sub, err := subs.Get(ctx, tenantID, subID)
				if err != nil {
					t.Fatalf("get subscription: %v", err)
				}
				pc := sub.PauseCollection
				if pc == nil || pc.ResumesAt == nil || pc.SetBy != domain.PauseSetByDunning {
					t.Fatalf("pause after the payment = %+v, want the dunning pause timed at the payment", pc)
				}
				if d := pc.ResumesAt.Sub(paidAt); d < 0 || d > 5*time.Second {
					t.Errorf("resumes_at %v, want the payment's instant (%v)", pc.ResumesAt, paidAt)
				}
				if pc.InForceAt(time.Now()) {
					t.Error("the pause still holds after the invoice it was set for was paid")
				}

				svc := subscription.NewService(subs, clock.Real())
				svc.SetAuditLogger(audit.NewLogger(db))
				svc.SetEventDispatcher(webhook.NewOutboxDispatcher(outbox))
				if _, errs := svc.ProcessExpiredPauseCollections(ctx, 100); len(errs) > 0 {
					t.Fatalf("resumer: %v", errs)
				}
				if sub, err = subs.Get(ctx, tenantID, subID); err != nil || sub.PauseCollection != nil {
					t.Fatalf("pause after the resumer = %+v (err %v), want none", sub.PauseCollection, err)
				}
				if n := countRows(t, db, ctx, tenantID, `SELECT count(*) FROM audit_log WHERE resource_id = $1
					AND metadata->>'action' = 'collection_resumed' AND metadata->>'triggered_by' = 'payment'`, subID); n != 1 {
					t.Errorf("%d collection_resumed rows triggered_by=payment, want 1", n)
				}
				if n := countRows(t, db, ctx, tenantID, `SELECT count(*) FROM webhook_outbox WHERE event_type = 'subscription.collection_resumed'
					AND payload->'subscription'->>'id' = $1 AND payload->>'triggered_by' = 'payment'`, subID); n != 1 {
					t.Errorf("%d subscription.collection_resumed events, want 1", n)
				}
			})
		}
	})

	t.Run("credit application supersedes claims", func(t *testing.T) {
		tenantID := testutil.CreateTestTenant(t, db, "Choke Credit")
		store := invoice.NewPostgresStore(db)
		invID := seedFinalizedInvoice(t, db, store, ctx, tenantID)
		claimID := seedOpenClaim(t, db, ctx, tenantID, invID)

		// Partial credit through the LIVE balance-credit writer
		// (credit.ApplyToInvoiceAtomic): the open claim's minted amount is
		// now stale — it must supersede in the same tx (ADR-068). Pre-fix
		// (2026-07-21 derived-data audit) the supersede lived only on the
		// CN-issue writer and the since-deleted invoice.ApplyCredits, so a
		// customer paying the stale session after a credit overpaid.
		inv, err := store.Get(ctx, tenantID, invID)
		if err != nil {
			t.Fatalf("get invoice: %v", err)
		}
		creditStore := credit.NewPostgresStore(db)
		if _, err := creditStore.AppendEntry(ctx, tenantID, domain.CreditLedgerEntry{
			Currency:    "USD",
			CustomerID:  inv.CustomerID,
			EntryType:   domain.CreditGrant,
			AmountCents: 400,
			Description: "choke-close test grant",
		}); err != nil {
			t.Fatalf("grant credit: %v", err)
		}
		applied, err := creditStore.ApplyToInvoiceAtomic(ctx, tenantID, inv.CustomerID, invID, inv.InvoiceNumber, inv.AmountDueCents, time.Now())
		if err != nil {
			t.Fatalf("apply credits: %v", err)
		}
		if applied != 400 {
			t.Fatalf("applied = %d, want 400 (full balance)", applied)
		}
		if got := claimStatus(t, db, ctx, tenantID, claimID); got != "superseded" {
			t.Fatalf("claim after partial credit = %q, want superseded", got)
		}
	})
}

// setDue sets the invoice's amount due, as an applied credit or a credit
// note would have left it.
func setDue(t *testing.T, db *postgres.DB, ctx context.Context, tenantID, invoiceID string, cents int64) {
	t.Helper()
	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(ctx, `UPDATE invoices SET amount_due_cents = $1 WHERE id = $2`, cents, invoiceID); err != nil {
		t.Fatalf("set amount due: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// countRows runs a count query on the tenant's rows.
func countRows(t *testing.T, db *postgres.DB, ctx context.Context, tenantID, query string, args ...any) int {
	t.Helper()
	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	var n int
	if err := tx.QueryRowContext(ctx, query, args...).Scan(&n); err != nil && err != sql.ErrNoRows {
		t.Fatalf("count: %v", err)
	}
	return n
}
