package creditnote_test

import (
	"context"
	"strings"
	"testing"

	"github.com/sagarsuperuser/velox/internal/creditnote"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/tenant"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// A cancel or swap relieves several funding invoices on one transaction, one
// credit note each. A credit note created on the second invoice at the same
// time must wait for that transaction, not deadlock with it.
func TestCreditNoteNumber_MultiInvoiceReliefDoesNotDeadlockWithCreate(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "CN Lock Order")
	_, invA := seedInvoiceForCN(t, db, tenantID, "lock-order-a", domain.InvoiceFinalized, domain.PaymentPending, "")
	_, invB := seedInvoiceForCN(t, db, tenantID, "lock-order-b", domain.InvoiceFinalized, domain.PaymentPending, "")

	store := creditnote.NewPostgresStore(db)
	svc := creditnote.NewService(store, invoice.NewPostgresStore(db), nil)
	svc.SetNumberGenerator(tenant.NewSettingsStore(db))

	relief, err := store.BeginTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(relief)
	if err := svc.LockInvoicesForDraftsTx(ctx, relief, tenantID, []string{invB.ID, invA.ID}); err != nil {
		t.Fatalf("lock funding invoices: %v", err)
	}
	if _, err := svc.CreateAdjustmentDraftTx(ctx, relief, tenantID, invA.ID, 100, "cancel_relief", "unused time"); err != nil {
		t.Fatalf("relief on first invoice: %v", err)
	}

	createErr := make(chan error, 1)
	go func() {
		_, cerr := svc.Create(ctx, tenantID, creditnote.CreateInput{
			InvoiceID: invB.ID,
			Reason:    "order_change",
			Lines:     []creditnote.CreditLineInput{{Description: "over-billed seat", Quantity: 1, UnitAmountCents: 100}},
		})
		createErr <- cerr
	}()
	// The concurrent create must be waiting for the second invoice's lock,
	// which the relief took up front, and not for the number row.
	testutil.WaitForLockWaiter(t, ctx, testutil.AdminPool(t), "pg_advisory_xact_lock")

	_, reliefErr := svc.CreateAdjustmentDraftTx(ctx, relief, tenantID, invB.ID, 100, "cancel_relief", "unused time")
	if reliefErr == nil {
		if err := relief.Commit(); err != nil {
			t.Fatalf("commit relief: %v", err)
		}
	} else {
		postgres.Rollback(relief)
	}
	cerr := <-createErr

	for name, e := range map[string]error{"relief": reliefErr, "create": cerr} {
		if e != nil && strings.Contains(e.Error(), "deadlock detected") {
			t.Fatalf("%s hit a deadlock: %v", name, e)
		}
	}
	if reliefErr != nil {
		t.Fatalf("relief on second invoice: %v", reliefErr)
	}
}
