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
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
	"github.com/sagarsuperuser/velox/internal/webhook"
)

// failingEnqueuer stands in for an outbox insert that fails inside the tx.
type failingEnqueuer struct{}

func (failingEnqueuer) Enqueue(context.Context, *sql.Tx, string, string, map[string]any) (string, error) {
	return "", errors.New("outbox unavailable")
}

// TestStartRun_RunEventAndWebhookAreAllOrNone pins that starting dunning is
// one transaction: the run, its dunning_started timeline event, and the
// dunning.started outbox row all commit, or none do. Before, they were three
// separate commits, so a crash after the first left a run with no timeline row
// and no webhook — and nothing re-sent them, since StartDunning is idempotent
// on the existing run. A second StartDunning on the same invoice adds nothing.
func TestStartRun_RunEventAndWebhookAreAllOrNone(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Start Run Atomic")
	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{ExternalID: "cus_sra", DisplayName: "SRA"})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	store := dunning.NewPostgresStore(db)
	if _, err := store.UpsertPolicy(ctx, tenantID, domain.DunningPolicy{
		Name: "default", Enabled: true, RetrySchedule: []string{"72h"}, MaxRetryAttempts: 3,
		FinalSubscriptionAction: domain.SubActionNone, FinalInvoiceAction: domain.InvActionNone, GracePeriodDays: 3,
	}); err != nil {
		t.Fatalf("upsert policy: %v", err)
	}
	newInvoice := func(t *testing.T, num string) domain.Invoice {
		t.Helper()
		now := time.Now().UTC()
		inv, err := invoice.NewPostgresStore(db).Create(ctx, tenantID, domain.Invoice{
			CustomerID: cust.ID, InvoiceNumber: num, Status: domain.InvoiceFinalized,
			PaymentStatus: domain.PaymentPending, Currency: "USD", SubtotalCents: 5000,
			TotalAmountCents: 5000, AmountDueCents: 5000, BillingPeriodStart: now.Add(-time.Hour),
			BillingPeriodEnd: now, IssuedAt: &now,
		})
		if err != nil {
			t.Fatalf("create invoice: %v", err)
		}
		return inv
	}
	count := func(t *testing.T, query, invoiceID string) int {
		t.Helper()
		tx, err := db.BeginTx(context.Background(), postgres.TxBypass, "")
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer postgres.Rollback(tx)
		var n int
		if err := tx.QueryRow(query, invoiceID).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}
	runs := func(t *testing.T, id string) int {
		return count(t, `SELECT count(*) FROM invoice_dunning_runs WHERE invoice_id = $1`, id)
	}
	events := func(t *testing.T, id string) int {
		return count(t, `SELECT count(*) FROM invoice_dunning_events WHERE invoice_id = $1 AND event_type = 'dunning_started'`, id)
	}
	hooks := func(t *testing.T, id string) int {
		return count(t, `SELECT count(*) FROM webhook_outbox WHERE event_type = 'dunning.started' AND payload->>'invoice_id' = $1`, id)
	}

	t.Run("enqueue fails → nothing persisted", func(t *testing.T) {
		inv := newInvoice(t, "INV-SRA-FAIL")
		store.SetOutboxEnqueuer(failingEnqueuer{})
		svc := dunning.NewService(store, nil, nil)
		if _, err := svc.StartDunning(ctx, tenantID, inv.ID, cust.ID, time.Now().UTC(), domain.DunningCauseNoPaymentMethod); err == nil {
			t.Fatal("StartDunning must fail when its webhook can't be enqueued")
		}
		if r, e, h := runs(t, inv.ID), events(t, inv.ID), hooks(t, inv.ID); r+e+h != 0 {
			t.Fatalf("runs=%d events=%d hooks=%d after a failed start, want all 0", r, e, h)
		}
	})

	t.Run("success → run, event and webhook together; a repeat adds nothing", func(t *testing.T) {
		inv := newInvoice(t, "INV-SRA-OK")
		store.SetOutboxEnqueuer(webhook.NewOutboxStore(db))
		svc := dunning.NewService(store, nil, nil)
		failureAt := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC) // the failure instant, not now
		var run domain.InvoiceDunningRun
		for i := 0; i < 2; i++ {
			got, err := svc.StartDunning(ctx, tenantID, inv.ID, cust.ID, failureAt, domain.DunningCauseNoPaymentMethod)
			if err != nil {
				t.Fatalf("StartDunning #%d: %v", i+1, err)
			}
			run = got
		}
		if r, e, h := runs(t, inv.ID), events(t, inv.ID), hooks(t, inv.ID); r != 1 || e != 1 || h != 1 {
			t.Fatalf("runs=%d events=%d hooks=%d, want exactly 1 each", r, e, h)
		}

		// What the rows say, not just that they exist: the timeline row keeps
		// the cause and sits at the failure instant (not catchup's
		// frozen_time), and the webhook carries the run and customer.
		tx, err := db.BeginTx(context.Background(), postgres.TxBypass, "")
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer postgres.Rollback(tx)
		var reason, state string
		var createdAt time.Time
		if err := tx.QueryRow(`SELECT COALESCE(reason,''), state, created_at FROM invoice_dunning_events
			WHERE invoice_id = $1 AND event_type = 'dunning_started'`, inv.ID).Scan(&reason, &state, &createdAt); err != nil {
			t.Fatalf("read started event: %v", err)
		}
		if reason != string(domain.DunningCauseNoPaymentMethod) || state != string(domain.DunningActive) || !createdAt.Equal(failureAt) {
			t.Errorf("started event = reason %q state %q at %v, want %q / active / %v",
				reason, state, createdAt, domain.DunningCauseNoPaymentMethod, failureAt)
		}
		var runID, customerID string
		if err := tx.QueryRow(`SELECT payload->>'run_id', payload->>'customer_id' FROM webhook_outbox
			WHERE event_type = 'dunning.started' AND payload->>'invoice_id' = $1`, inv.ID).Scan(&runID, &customerID); err != nil {
			t.Fatalf("read outbox payload: %v", err)
		}
		if runID != run.ID || customerID != cust.ID {
			t.Errorf("dunning.started payload run_id=%q customer_id=%q, want %q / %q", runID, customerID, run.ID, cust.ID)
		}
	})
}
