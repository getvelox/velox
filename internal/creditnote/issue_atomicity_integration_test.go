package creditnote_test

import (
	"context"
	"database/sql"
	"errors"
	"github.com/sagarsuperuser/velox/internal/tax"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/creditnote"
	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// failingGranterTx fails the in-tx grant — the exact internal-effect failure the
// Issue() coordinator tx must roll the draft→issued CAS back from (ADR-061).
type failingGranterTx struct{}

func (failingGranterTx) Grant(context.Context, string, creditnote.CreditGrantInput) error {
	return nil
}
func (failingGranterTx) GrantForCreditNote(context.Context, string, string, creditnote.CreditGrantInput) error {
	return nil
}
func (failingGranterTx) GrantForCreditNoteTx(context.Context, *sql.Tx, string, string, creditnote.CreditGrantInput) error {
	return errors.New("injected in-tx grant failure")
}
func (failingGranterTx) LockCommitGrantForReliefTx(context.Context, *sql.Tx, string, string) (int64, int64, int64, bool, error) {
	return 0, 0, 0, false, nil
}
func (failingGranterTx) RetireCommitSliceForReliefTx(context.Context, *sql.Tx, string, string, string, int64, int64, int64) (int64, error) {
	return 0, errors.New("not a commit test")
}

// TestIssue_GrantFailure_RollsBackCAS is the real-Postgres proof of the ADR-061
// atomicity guarantee: when the INTERNAL credit grant fails inside Issue()'s
// coordinator tx, the draft→issued CAS rolls back WITH it — so the credit note
// stays 'draft' and the formerly-possible issued-but-ungranted orphan cannot
// exist. Pre-PR2 the CAS committed in its OWN tx, so a grant failure left a CN
// shown as completed with no balance credit and no automatic recovery. The
// in-memory double can't model rollback; this proves it against real tx
// semantics, then confirms a clean re-drive (with a working granter) succeeds.
func TestIssue_GrantFailure_RollsBackCAS(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "CN Issue Atomicity")

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_issueatom", DisplayName: "Issue Atom",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}

	now := time.Now().UTC()
	issued := now
	inv, err := invoice.NewPostgresStore(db).Create(ctx, tenantID, domain.Invoice{
		CustomerID:         cust.ID,
		InvoiceNumber:      "INV-ISSUEATOM-1",
		Status:             domain.InvoicePaid,
		PaymentStatus:      domain.PaymentSucceeded,
		Currency:           "USD",
		SubtotalCents:      10000,
		TotalAmountCents:   10000,
		AmountDueCents:     0,
		AmountPaidCents:    10000,
		BillingPeriodStart: now.Add(-30 * 24 * time.Hour),
		BillingPeriodEnd:   now,
		IssuedAt:           &issued,
	})
	if err != nil {
		t.Fatalf("create invoice: %v", err)
	}

	store := creditnote.NewPostgresStore(db)
	cn, err := store.Create(ctx, tenantID, domain.CreditNote{
		InvoiceID:         inv.ID,
		CustomerID:        cust.ID,
		CreditNoteNumber:  "CN-ISSUEATOM-1",
		Status:            domain.CreditNoteDraft,
		Reason:            "fraudulent",
		SubtotalCents:     5000,
		TotalCents:        5000,
		CreditAmountCents: 5000, // credit-type → Issue() takes the in-tx grant path
		Currency:          "USD",
		RefundStatus:      domain.RefundNone,
	})
	if err != nil {
		t.Fatalf("create credit note: %v", err)
	}

	// --- Failure path: the in-tx grant errors → the CAS must roll back. ---
	failSvc := creditnote.NewService(store, invoice.NewPostgresStore(db), nil, failingGranterTx{})
	if _, err := failSvc.Issue(ctx, tenantID, cn.ID); err == nil {
		t.Fatal("expected Issue to fail when the in-tx grant errors")
	}

	got, err := store.Get(ctx, tenantID, cn.ID)
	if err != nil {
		t.Fatalf("get credit note after failed issue: %v", err)
	}
	if got.Status != domain.CreditNoteDraft {
		t.Fatalf("status after grant failure: got %q, want draft — the CAS must roll back with the failed grant (no issued-but-ungranted orphan)", got.Status)
	}
	if got.IssuedAt != nil {
		t.Errorf("issued_at must be nil after rollback, got %v", got.IssuedAt)
	}

	// --- Recovery path: a clean re-drive (working granter) now succeeds. ---
	okSvc := creditnote.NewService(store, invoice.NewPostgresStore(db), nil, okGranterTx{})
	if _, err := okSvc.Issue(ctx, tenantID, cn.ID); err != nil {
		t.Fatalf("re-issue after a clean rollback should succeed: %v", err)
	}
	got, err = store.Get(ctx, tenantID, cn.ID)
	if err != nil {
		t.Fatalf("get credit note after re-issue: %v", err)
	}
	if got.Status != domain.CreditNoteIssued {
		t.Errorf("status after clean re-drive: got %q, want issued", got.Status)
	}
}

// okGranterTx is the working-granter counterpart used by the recovery leg.
type okGranterTx struct{}

func (okGranterTx) Grant(context.Context, string, creditnote.CreditGrantInput) error { return nil }
func (okGranterTx) GrantForCreditNote(context.Context, string, string, creditnote.CreditGrantInput) error {
	return nil
}
func (okGranterTx) GrantForCreditNoteTx(context.Context, *sql.Tx, string, string, creditnote.CreditGrantInput) error {
	return nil
}
func (okGranterTx) LockCommitGrantForReliefTx(context.Context, *sql.Tx, string, string) (int64, int64, int64, bool, error) {
	return 0, 0, 0, false, nil
}
func (okGranterTx) RetireCommitSliceForReliefTx(context.Context, *sql.Tx, string, string, string, int64, int64, int64) (int64, error) {
	return 0, nil
}

// TestListPendingCreditNoteTaxReversal_FindsMarkerlessOrphan is the real-Postgres
// proof of the recovery fix (review finding, 2026-06-25): an issued credit note
// whose tax reversal failed AND whose tax_reversal_pending marker write ALSO
// failed (so the marker is the default false) is the compound-failure orphan —
// status='issued', tax_transaction_id=”, tax_reversal_pending=false — over-
// remitting tax. Recovery eligibility is now DERIVED STRUCTURALLY (issued CN, no
// reversal stamped, tax-bearing stripe_tax source), so the sweep finds it despite
// the missing marker; a CN that DID stamp a reversal is excluded.
func TestListPendingCreditNoteTaxReversal_FindsMarkerlessOrphan(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "CN Reversal Structural")

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_revstruct", DisplayName: "Rev Struct",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}

	invStore := invoice.NewPostgresStore(db)
	now := time.Now().UTC()
	issuedAt := now
	mkInvoice := func(num string) domain.Invoice {
		inv, err := invStore.Create(ctx, tenantID, domain.Invoice{
			CustomerID: cust.ID, InvoiceNumber: num,
			Status: domain.InvoicePaid, PaymentStatus: domain.PaymentSucceeded,
			Currency: "USD", SubtotalCents: 10000,
			TaxFacts:         domain.TaxFacts{TaxAmountCents: 1000, TaxProvider: "stripe_tax"},
			TotalAmountCents: 11000, AmountPaidCents: 11000,
			BillingPeriodStart: now.Add(-30 * 24 * time.Hour), BillingPeriodEnd: now,
			IssuedAt: &issuedAt,
		})
		if err != nil {
			t.Fatalf("create invoice %s: %v", num, err)
		}
		// Stamp the upstream tax transaction (Create doesn't set it).
		if err := invStore.SetTaxTransaction(ctx, tenantID, inv.ID, "tx_upstream_"+num); err != nil {
			t.Fatalf("stamp tax tx %s: %v", num, err)
		}
		return inv
	}

	taxInv := mkInvoice("INV-REVSTRUCT-ORPHAN")

	store := creditnote.NewPostgresStore(db)
	// The orphan: issued, no reversal stamped, marker default-false.
	orphan, err := store.Create(ctx, tenantID, domain.CreditNote{
		InvoiceID: taxInv.ID, CustomerID: cust.ID, CreditNoteNumber: "CN-REVSTRUCT-ORPHAN",
		Status: domain.CreditNoteIssued, Reason: "fraudulent",
		SubtotalCents: 5000, TaxAmountCents: 500, TotalCents: 5500,
		Currency: "USD", RefundStatus: domain.RefundNone,
		// TaxTransactionID empty, TaxReversalPending false (defaults).
	})
	if err != nil {
		t.Fatalf("create orphan CN: %v", err)
	}

	// Control: a CN that DID stamp its reversal — must NOT be eligible. Create
	// doesn't persist tax_transaction_id (it's stamped post-reversal, like the
	// invoice), so set it explicitly via SetTaxTransaction.
	doneInv := mkInvoice("INV-REVSTRUCT-DONE")
	done, err := store.Create(ctx, tenantID, domain.CreditNote{
		InvoiceID: doneInv.ID, CustomerID: cust.ID, CreditNoteNumber: "CN-REVSTRUCT-DONE",
		Status: domain.CreditNoteIssued, Reason: "fraudulent",
		SubtotalCents: 5000, TaxAmountCents: 500, TotalCents: 5500,
		Currency: "USD", RefundStatus: domain.RefundNone,
	})
	if err != nil {
		t.Fatalf("create done CN: %v", err)
	}
	if err := store.SetTaxTransaction(ctx, tenantID, done.ID, "tx_reversal_already_done"); err != nil {
		t.Fatalf("stamp done CN reversal tx: %v", err)
	}

	pending, err := store.ListPendingCreditNoteTaxReversal(ctx, 50, false)
	if err != nil {
		t.Fatalf("ListPendingCreditNoteTaxReversal: %v", err)
	}
	found := map[string]bool{}
	for _, cn := range pending {
		found[cn.ID] = true
	}
	if !found[orphan.ID] {
		t.Error("the marker-less orphan must be found via structural derivation (issued CN, no reversal stamped, tax-bearing source)")
	}
	if found[done.ID] {
		t.Error("a CN that already stamped its reversal must NOT be eligible")
	}
}

// failingReverser is the always-failing provider for the HA-13 proof.
type failingReverser struct{}

func (failingReverser) ReverseTax(_ context.Context, _ string, _ tax.ReversalRequest) (*tax.ReversalResult, error) {
	return nil, errors.New("stripe: 503 service unavailable")
}

// TestCreditNoteTaxReversal_PromotedOrphanOutlivesTheWindow is the
// real-Postgres proof of HA-13 (2026-08-30): a marker-less orphan whose first
// re-drive fails is promoted onto the marker branch, and — unlike the
// structural branch — that branch has no 24h freshness window, so the row is
// still eligible after being backdated a day. Before the promote, a >24h
// provider outage coinciding with the compound failure silently aged the
// obligation out. Mutation-verify: skip promoteOrphan → the backdated row
// is not returned.
func TestCreditNoteTaxReversal_PromotedOrphanOutlivesTheWindow(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "CN Reversal Promote")

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_promote", DisplayName: "Promote",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	invStore := invoice.NewPostgresStore(db)
	now := time.Now().UTC()
	issuedAt := now
	inv, err := invStore.Create(ctx, tenantID, domain.Invoice{
		CustomerID: cust.ID, InvoiceNumber: "INV-PROMOTE",
		Status: domain.InvoicePaid, PaymentStatus: domain.PaymentSucceeded,
		Currency: "USD", SubtotalCents: 10000,
		TaxFacts:         domain.TaxFacts{TaxAmountCents: 1000, TaxProvider: "stripe_tax"},
		TotalAmountCents: 11000, AmountPaidCents: 11000,
		BillingPeriodStart: now.Add(-30 * 24 * time.Hour), BillingPeriodEnd: now,
		IssuedAt: &issuedAt,
	})
	if err != nil {
		t.Fatalf("create invoice: %v", err)
	}
	if err := invStore.SetTaxTransaction(ctx, tenantID, inv.ID, "tx_upstream_promote"); err != nil {
		t.Fatalf("stamp tax tx: %v", err)
	}
	store := creditnote.NewPostgresStore(db)
	orphan, err := store.Create(ctx, tenantID, domain.CreditNote{
		InvoiceID: inv.ID, CustomerID: cust.ID, CreditNoteNumber: "CN-PROMOTE",
		Status: domain.CreditNoteIssued, Reason: "fraudulent",
		SubtotalCents: 5000, TaxAmountCents: 500, TotalCents: 5500,
		Currency: "USD", RefundStatus: domain.RefundNone,
	})
	if err != nil {
		t.Fatalf("create orphan: %v", err)
	}

	svc := creditnote.NewService(store, invStore, nil, nil)
	svc.SetTaxReverser(failingReverser{})
	n, sweepErrs := svc.RetryPendingCreditNoteTaxReversal(ctx, 50)
	if n != 0 || len(sweepErrs) != 1 {
		t.Fatalf("first sweep: reversed=%d errs=%v — want 0 and exactly the re-drive error", n, sweepErrs)
	}
	got, err := store.Get(ctx, tenantID, orphan.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !got.TaxReversalPending {
		t.Fatal("orphan was not promoted onto the marker branch by its failed re-drive")
	}

	// A day passes with the provider still down: the structural window is
	// long gone — the marker branch must still carry the row.
	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE credit_notes SET updated_at = now() - interval '25 hours' WHERE id = $1`, orphan.ID); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	pending, err := store.ListPendingCreditNoteTaxReversal(ctx, 50, false)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	found := false
	for _, cn := range pending {
		found = found || cn.ID == orphan.ID
	}
	if !found {
		t.Fatal("promoted orphan fell out of the sweep after 25h — the obligation, not just detection, was bounded by the window")
	}
}
