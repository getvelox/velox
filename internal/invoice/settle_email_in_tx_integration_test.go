package invoice_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/email"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// The settle transitions carry their customer email INSIDE the transaction
// (ADR-040 amendment): the paid-flip and its receipt commit together, so a
// process death or failover between them can no longer lose the receipt while
// the payment stands — the transition gate would suppress every later attempt.
// Real Postgres because the guarantee is transactional.

func settleFixture(t *testing.T, db *postgres.DB, ctx context.Context, name string) (tenantID string, invID string, store *invoice.PostgresStore) {
	t.Helper()
	tenantID = testutil.CreateTestTenant(t, db, name)
	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{ExternalID: "cus_" + name, DisplayName: name})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	store = invoice.NewPostgresStore(db)
	now := time.Now().UTC()
	inv, err := store.Create(ctx, tenantID, domain.Invoice{
		CustomerID: cust.ID, InvoiceNumber: "INV-" + name, Status: domain.InvoiceFinalized,
		PaymentStatus: domain.PaymentPending, Currency: "USD", SubtotalCents: 5000,
		TotalAmountCents: 5000, AmountDueCents: 5000, BillingPeriodStart: now.Add(-time.Hour),
		BillingPeriodEnd: now, IssuedAt: &now,
	})
	if err != nil {
		t.Fatalf("create invoice: %v", err)
	}
	return tenantID, inv.ID, store
}

func outboxCount(t *testing.T, db *postgres.DB, ctx context.Context, tenantID, emailType string) int {
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

// TestSettleTransitions_EmailRidesTheTx: the receipt and the decline notice
// are committed by the same transaction as the state change, and the hook is
// handed the freshly-written row (the receipt's amount is what was booked).
// Mutation check: move the hook after tx.Commit → the crash window returns and
// the shared-fate assertion below (rollback case) fails.
func TestSettleTransitions_EmailRidesTheTx(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test — requires postgres")
	}
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	outbox := email.NewOutboxStore(db)

	t.Run("paid flip and receipt commit together", func(t *testing.T) {
		tenantID, invID, store := settleFixture(t, db, ctx, "settle-paid")
		var sawAmount int64
		_, transitioned, err := store.MarkPaidCardSettlementTransition(ctx, tenantID, invID, "pi_ok", time.Now().UTC(),
			func(tx *sql.Tx, fresh domain.Invoice) error {
				sawAmount = fresh.AmountPaidCents // the row this tx wrote
				_, e := outbox.Enqueue(ctx, tx, tenantID, email.TypePaymentReceipt, map[string]any{"to": "c@example.test"})
				return e
			})
		if err != nil || !transitioned {
			t.Fatalf("transition: transitioned=%v err=%v", transitioned, err)
		}
		if sawAmount == 0 {
			t.Fatal("the hook must receive the freshly-written row — the receipt amount is what this tx booked")
		}
		if n := outboxCount(t, db, ctx, tenantID, email.TypePaymentReceipt); n != 1 {
			t.Fatalf("payment_receipt outbox rows = %d, want 1 (committed with the paid flip)", n)
		}
	})

	t.Run("hook is skipped when the transition is lost", func(t *testing.T) {
		tenantID, invID, store := settleFixture(t, db, ctx, "settle-dup")
		if _, _, err := store.MarkPaidCardSettlementTransition(ctx, tenantID, invID, "pi_first", time.Now().UTC(), nil); err != nil {
			t.Fatalf("first settle: %v", err)
		}
		ran := false
		_, transitioned, err := store.MarkPaidCardSettlementTransition(ctx, tenantID, invID, "pi_first", time.Now().UTC(),
			func(tx *sql.Tx, _ domain.Invoice) error {
				ran = true
				_, e := outbox.Enqueue(ctx, tx, tenantID, email.TypePaymentReceipt, map[string]any{"to": "c@example.test"})
				return e
			})
		if err != nil {
			t.Fatalf("duplicate settle: %v", err)
		}
		if transitioned || ran {
			t.Fatalf("a duplicate settle must not re-fire the receipt: transitioned=%v hookRan=%v", transitioned, ran)
		}
		if n := outboxCount(t, db, ctx, tenantID, email.TypePaymentReceipt); n != 0 {
			t.Fatalf("duplicate settle enqueued %d receipt(s); want 0", n)
		}
	})

	t.Run("a failing hook never vetoes the money write", func(t *testing.T) {
		tenantID, invID, store := settleFixture(t, db, ctx, "settle-veto")
		fresh, transitioned, err := store.MarkPaidCardSettlementTransition(ctx, tenantID, invID, "pi_veto", time.Now().UTC(),
			func(tx *sql.Tx, _ domain.Invoice) error {
				if _, e := outbox.Enqueue(ctx, tx, tenantID, email.TypePaymentReceipt, map[string]any{"to": "c@example.test"}); e != nil {
					return e
				}
				return errors.New("template render exploded")
			})
		if err != nil {
			t.Fatalf("a failing email hook must not surface as a settle error: %v", err)
		}
		if !transitioned || fresh.Status != domain.InvoicePaid {
			t.Fatalf("the payment must still be booked: transitioned=%v status=%q", transitioned, fresh.Status)
		}
		if n := outboxCount(t, db, ctx, tenantID, email.TypePaymentReceipt); n != 0 {
			t.Fatalf("the failed hook's enqueue survived (%d rows) — it must roll back to the savepoint", n)
		}
	})

	t.Run("decline notice rides the fail stamp", func(t *testing.T) {
		tenantID, invID, store := settleFixture(t, db, ctx, "settle-failed")
		_, first, err := store.MarkPaymentFailedReportingTransition(ctx, tenantID, invID, "pi_decline", "card_declined",
			func(tx *sql.Tx, _ domain.Invoice) error {
				_, e := outbox.Enqueue(ctx, tx, tenantID, email.TypePaymentFailed, map[string]any{"to": "c@example.test"})
				return e
			})
		if err != nil || !first {
			t.Fatalf("fail stamp: first=%v err=%v", first, err)
		}
		if n := outboxCount(t, db, ctx, tenantID, email.TypePaymentFailed); n != 1 {
			t.Fatalf("payment_failed outbox rows = %d, want 1", n)
		}
		// A same-PI redelivery is not the first report: no second notice.
		ran := false
		if _, again, err := store.MarkPaymentFailedReportingTransition(ctx, tenantID, invID, "pi_decline", "card_declined",
			func(tx *sql.Tx, _ domain.Invoice) error { ran = true; return nil }); err != nil || again || ran {
			t.Fatalf("same-PI redelivery must not re-notify: again=%v hookRan=%v err=%v", again, ran, err)
		}
	})
}
