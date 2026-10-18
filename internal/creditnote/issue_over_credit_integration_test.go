package creditnote_test

import (
	"context"
	"database/sql"
	"sync"
	"testing"

	"github.com/sagarsuperuser/velox/internal/credit"
	"github.com/sagarsuperuser/velox/internal/creditnote"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/tenant"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// ledgerGranter is the production bridge from credit-note Issue() to the real
// credit ledger (api.creditGrantAdapter), restated here because that adapter is
// unexported.
type ledgerGranter struct{ svc *credit.Service }

func (g ledgerGranter) in(i creditnote.CreditGrantInput) credit.GrantInput {
	return credit.GrantInput{CustomerID: i.CustomerID, AmountCents: i.AmountCents, Description: i.Description, InvoiceID: i.InvoiceID}
}
func (g ledgerGranter) Grant(ctx context.Context, tenantID string, i creditnote.CreditGrantInput) error {
	_, err := g.svc.Grant(ctx, tenantID, g.in(i))
	return err
}
func (g ledgerGranter) GrantForCreditNote(ctx context.Context, tenantID, cnID string, i creditnote.CreditGrantInput) error {
	_, err := g.svc.GrantForCreditNote(ctx, tenantID, cnID, g.in(i))
	return err
}
func (g ledgerGranter) GrantForCreditNoteTx(ctx context.Context, tx *sql.Tx, tenantID, cnID string, i creditnote.CreditGrantInput) error {
	_, err := g.svc.GrantForCreditNoteTx(ctx, tx, tenantID, cnID, g.in(i))
	return err
}
func (g ledgerGranter) LockCommitGrantForReliefTx(ctx context.Context, tx *sql.Tx, tenantID, invoiceID string) (int64, int64, int64, bool, error) {
	return g.svc.LockCommitGrantForReliefTx(ctx, tx, tenantID, invoiceID)
}
func (g ledgerGranter) RetireCommitSliceForReliefTx(context.Context, *sql.Tx, string, string, string, int64, int64, int64) (int64, error) {
	panic("commit relief is not exercised by these tests")
}

// overCreditRig is one finalized, unpaid 100.00 invoice with the real
// credit-note, credit and invoice services over real Postgres.
type overCreditRig struct {
	ctx      context.Context
	db       *postgres.DB
	tenantID string
	cust     domain.Customer
	inv      domain.Invoice
	invStore *invoice.PostgresStore
	invSvc   *invoice.Service
	credits  *credit.Service
	notes    *creditnote.Service
}

func newOverCreditRig(t *testing.T, name string) *overCreditRig {
	t.Helper()
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, name)
	cust, inv := seedInvoiceForCN(t, db, tenantID, "over-credit", domain.InvoiceFinalized, domain.PaymentPending, "")

	invStore := invoice.NewPostgresStore(db)
	credits := credit.NewService(credit.NewPostgresStore(db))
	settings := tenant.NewSettingsStore(db)
	invSvc := invoice.NewService(invStore, nil, settings)
	invSvc.SetCreditReverser(credits)
	notes := creditnote.NewService(creditnote.NewPostgresStore(db), invStore, nil, ledgerGranter{credits})
	notes.SetNumberGenerator(settings)
	return &overCreditRig{ctx: ctx, db: db, tenantID: tenantID, cust: cust, inv: inv,
		invStore: invStore, invSvc: invSvc, credits: credits, notes: notes}
}

func (r *overCreditRig) draft(t *testing.T, cents int64) domain.CreditNote {
	t.Helper()
	cn, err := r.notes.Create(r.ctx, r.tenantID, creditnote.CreateInput{
		InvoiceID: r.inv.ID,
		Reason:    "order_change",
		Lines:     []creditnote.CreditLineInput{{Description: "credit", Quantity: 1, UnitAmountCents: cents}},
	})
	if err != nil {
		t.Fatalf("Create %d: %v", cents, err)
	}
	return cn
}

// applyCredits grants the customer `cents` of credit and applies it to the invoice.
func (r *overCreditRig) applyCredits(t *testing.T, cents int64) {
	t.Helper()
	if _, err := r.credits.Grant(r.ctx, r.tenantID, credit.GrantInput{CustomerID: r.cust.ID, AmountCents: cents, Description: "goodwill"}); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	applied, err := r.credits.ApplyToInvoice(r.ctx, r.tenantID, r.cust.ID, r.inv.ID, 10000)
	if err != nil || applied != cents {
		t.Fatalf("ApplyToInvoice = %d, %v; want %d applied", applied, err, cents)
	}
}

func (r *overCreditRig) invoiceNow(t *testing.T) domain.Invoice {
	t.Helper()
	inv, err := r.invStore.Get(r.ctx, r.tenantID, r.inv.ID)
	if err != nil {
		t.Fatalf("get invoice: %v", err)
	}
	return inv
}

func (r *overCreditRig) balance(t *testing.T) int64 {
	t.Helper()
	b, err := r.credits.GetBalance(r.ctx, r.tenantID, r.cust.ID)
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	return b.BalanceCents
}

// conserved asserts the invoice's total is accounted for exactly once:
// what is still due + paid + credits applied + issued credit notes = total.
func (r *overCreditRig) conserved(t *testing.T) {
	t.Helper()
	inv := r.invoiceNow(t)
	issued, err := r.notes.IssuedCreditedCents(r.ctx, r.tenantID, r.inv.ID)
	if err != nil {
		t.Fatalf("issued credit notes: %v", err)
	}
	if got := inv.AmountDueCents + inv.AmountPaidCents + inv.CreditsAppliedCents + issued; got != inv.TotalAmountCents {
		t.Fatalf("invoice total %d accounted as %d (due %d + paid %d + credits applied %d + credit notes %d)",
			inv.TotalAmountCents, got, inv.AmountDueCents, inv.AmountPaidCents, inv.CreditsAppliedCents, issued)
	}
}

// A draft credit note is sized against the amount due when it is created. When
// customer credits are applied to the invoice before the draft is issued, the
// note reduces what is still due and the credits it displaces go back to the
// customer's balance. Before this the invoice was relieved by 160.00 on a
// 100.00 total and the customer's 60.00 of credits bought nothing.
func TestIssue_DraftSizedBeforeCreditsApplied_ReturnsDisplacedCredits(t *testing.T) {
	r := newOverCreditRig(t, "CN Over Credit")
	draft := r.draft(t, 10000)
	r.applyCredits(t, 6000)

	issued, err := r.notes.Issue(r.ctx, r.tenantID, draft.ID)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if issued.Status != domain.CreditNoteIssued {
		t.Fatalf("status = %s, want issued", issued.Status)
	}
	if issued.RefundAmountCents != 0 || issued.CreditAmountCents != 0 || issued.OutOfBandAmountCents != 0 {
		t.Fatalf("note allocation = refund %d / credit %d / out-of-band %d, want all zero (it reduces the invoice)",
			issued.RefundAmountCents, issued.CreditAmountCents, issued.OutOfBandAmountCents)
	}
	inv := r.invoiceNow(t)
	if inv.AmountDueCents != 0 || inv.CreditsAppliedCents != 0 {
		t.Fatalf("invoice due %d / credits applied %d, want 0 / 0", inv.AmountDueCents, inv.CreditsAppliedCents)
	}
	if got := r.balance(t); got != 6000 {
		t.Fatalf("customer balance = %d, want the 6000 of displaced credits back", got)
	}
	r.conserved(t)
}

// A note larger than what is due returns only the difference.
func TestIssue_NotePartlyAboveAmountDue_ReturnsOnlyTheDifference(t *testing.T) {
	r := newOverCreditRig(t, "CN Partial Split")
	draft := r.draft(t, 5000)
	r.applyCredits(t, 6000) // due 4000

	if _, err := r.notes.Issue(r.ctx, r.tenantID, draft.ID); err != nil {
		t.Fatalf("Issue: %v", err)
	}
	inv := r.invoiceNow(t)
	if inv.AmountDueCents != 0 || inv.CreditsAppliedCents != 5000 {
		t.Fatalf("invoice due %d / credits applied %d, want 0 / 5000", inv.AmountDueCents, inv.CreditsAppliedCents)
	}
	if got := r.balance(t); got != 1000 {
		t.Fatalf("customer balance = %d, want 1000", got)
	}
	r.conserved(t)
}

// Voiding the invoice afterwards hands back only the credits still applied to
// it: the ones a credit note already returned must not come back twice.
func TestVoid_AfterCreditNoteReturnedCredits_HandsBackOnlyTheRest(t *testing.T) {
	for _, tc := range []struct {
		name        string
		noteCents   int64
		wantBalance int64 // after the void: every credit the customer was granted, once
	}{
		{"note returned every applied credit", 10000, 6000},
		{"note returned part of the applied credits", 5000, 6000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newOverCreditRig(t, "CN Void Netting")
			draft := r.draft(t, tc.noteCents)
			r.applyCredits(t, 6000)
			if _, err := r.notes.Issue(r.ctx, r.tenantID, draft.ID); err != nil {
				t.Fatalf("Issue: %v", err)
			}
			if _, err := r.invSvc.Void(r.ctx, r.tenantID, r.inv.ID); err != nil {
				t.Fatalf("Void: %v", err)
			}
			if got := r.balance(t); got != tc.wantBalance {
				t.Fatalf("customer balance after void = %d, want %d (6000 granted, none spent)", got, tc.wantBalance)
			}
		})
	}
}

// Two drafts that each fit the amount due when created: the second finds
// nothing left due and is returned to the balance whole.
func TestIssue_SecondDraftFindsNothingDue_ReturnsItsWholeAmount(t *testing.T) {
	r := newOverCreditRig(t, "CN Two Drafts")
	r.applyCredits(t, 6000) // due 4000
	first, second := r.draft(t, 4000), r.draft(t, 4000)

	for _, d := range []domain.CreditNote{first, second} {
		if _, err := r.notes.Issue(r.ctx, r.tenantID, d.ID); err != nil {
			t.Fatalf("Issue %s: %v", d.CreditNoteNumber, err)
		}
	}
	inv := r.invoiceNow(t)
	if inv.AmountDueCents != 0 || inv.CreditsAppliedCents != 2000 {
		t.Fatalf("invoice due %d / credits applied %d, want 0 / 2000", inv.AmountDueCents, inv.CreditsAppliedCents)
	}
	if got := r.balance(t); got != 4000 {
		t.Fatalf("customer balance = %d, want 4000", got)
	}
	r.conserved(t)
}

// With no credit ledger wired, a note that must return credits fails and stays
// a draft; it must not issue and drop them.
func TestIssue_NoGranterAndCreditsToReturn_FailsAndStaysDraft(t *testing.T) {
	r := newOverCreditRig(t, "CN No Granter")
	draft := r.draft(t, 10000)
	r.applyCredits(t, 6000)

	bare := creditnote.NewService(creditnote.NewPostgresStore(r.db), r.invStore, nil)
	if _, err := bare.Issue(r.ctx, r.tenantID, draft.ID); err == nil {
		t.Fatal("Issue succeeded with credits to return and no granter")
	}
	after, err := r.notes.Get(r.ctx, r.tenantID, draft.ID)
	if err != nil {
		t.Fatalf("get note: %v", err)
	}
	if after.Status != domain.CreditNoteDraft {
		t.Fatalf("status = %s, want draft", after.Status)
	}
	if inv := r.invoiceNow(t); inv.AmountDueCents != 4000 || inv.CreditsAppliedCents != 6000 {
		t.Fatalf("invoice due %d / credits applied %d, want it untouched at 4000 / 6000", inv.AmountDueCents, inv.CreditsAppliedCents)
	}
}

// The store refuses a reduction larger than what is due, or a return of more
// credits than are applied, and leaves the row as it was.
func TestApplyCreditNoteTx_RefusesMoreThanTheInvoiceHolds(t *testing.T) {
	r := newOverCreditRig(t, "CN Store Refusal")
	r.applyCredits(t, 6000) // due 4000, credits applied 6000

	for _, tc := range []struct {
		name             string
		reduce, returned int64
	}{
		{"reduce more than is due", 4001, 0},
		{"return more credits than are applied", 4000, 6001},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx, err := r.db.BeginTx(r.ctx, postgres.TxTenant, r.tenantID)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer postgres.Rollback(tx)
			_, err = r.invStore.ApplyCreditNoteTx(r.ctx, tx, r.tenantID, r.inv.ID, tc.reduce, tc.returned)
			if errs.Code(err) != "credit_note_exceeds_invoice" {
				t.Fatalf("err = %v, want code credit_note_exceeds_invoice", err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatalf("commit: %v", err)
			}
			if inv := r.invoiceNow(t); inv.AmountDueCents != 4000 || inv.CreditsAppliedCents != 6000 {
				t.Fatalf("invoice due %d / credits applied %d, want unchanged 4000 / 6000", inv.AmountDueCents, inv.CreditsAppliedCents)
			}
		})
	}
}

// Issue decides from the invoice row it locks, not from its earlier read. Here
// the invoice is paid while Issue waits for the row: the note must take the
// paid-invoice path (credit to the balance), not reduce an amount due that no
// longer exists.
func TestIssue_InvoicePaidWhileWaitingForItsRow_TakesThePaidPath(t *testing.T) {
	r := newOverCreditRig(t, "CN Locked Row")
	draft := r.draft(t, 3000)

	// Hold the invoice row in an open transaction that marks it paid.
	holder, err := r.db.BeginTx(r.ctx, postgres.TxTenant, r.tenantID)
	if err != nil {
		t.Fatalf("begin holder: %v", err)
	}
	defer postgres.Rollback(holder)
	if _, err := holder.ExecContext(r.ctx, `
		UPDATE invoices SET status = 'paid', payment_status = 'succeeded',
			amount_paid_cents = amount_due_cents, amount_due_cents = 0, paid_at = now()
		WHERE id = $1`, r.inv.ID); err != nil {
		t.Fatalf("mark paid in holder: %v", err)
	}

	type result struct {
		cn  domain.CreditNote
		err error
	}
	done := make(chan result, 1)
	go func() {
		cn, ierr := r.notes.Issue(r.ctx, r.tenantID, draft.ID)
		done <- result{cn, ierr}
	}()
	// Issue has read "unpaid" and is waiting on the row in LockForCreditNoteTx.
	// That statement is longer than the 1 KiB pg_stat_activity keeps, so match
	// its start: SELECT and the invoice column list. A plain SELECT never waits
	// on a row lock, so a blocked one is the locking read. The column list alone
	// is not enough: the UPDATE that applies the note returns the same columns.
	testutil.WaitForLockWaiter(t, r.ctx, testutil.AdminPool(t), "SELECT id, tenant_id, customer_id, COALESCE(subscription_id,'')")
	if err := holder.Commit(); err != nil {
		t.Fatalf("commit holder: %v", err)
	}
	res := <-done
	if res.err != nil {
		t.Fatalf("Issue: %v", res.err)
	}
	if res.cn.Status != domain.CreditNoteIssued || res.cn.CreditAmountCents != 3000 {
		t.Fatalf("note status %s / credit %d, want issued with 3000 credited to the balance", res.cn.Status, res.cn.CreditAmountCents)
	}
	if got := r.balance(t); got != 3000 {
		t.Fatalf("customer balance = %d, want 3000", got)
	}
	if inv := r.invoiceNow(t); inv.AmountDueCents != 0 || inv.AmountPaidCents != 10000 {
		t.Fatalf("invoice due %d / paid %d, want 0 / 10000 (the payment untouched)", inv.AmountDueCents, inv.AmountPaidCents)
	}
}

// A credit application and an issue racing on one invoice, in either order,
// account for the invoice total exactly once and lose none of the customer's
// credits.
func TestIssue_RacingCreditApplication_ConservesInvoiceAndCredits(t *testing.T) {
	for i := 0; i < 8; i++ {
		r := newOverCreditRig(t, "CN Race")
		draft := r.draft(t, 10000)
		if _, err := r.credits.Grant(r.ctx, r.tenantID, credit.GrantInput{CustomerID: r.cust.ID, AmountCents: 6000, Description: "goodwill"}); err != nil {
			t.Fatalf("Grant: %v", err)
		}

		var wg sync.WaitGroup
		var applyErr, issueErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, applyErr = r.credits.ApplyToInvoice(r.ctx, r.tenantID, r.cust.ID, r.inv.ID, 10000)
		}()
		go func() {
			defer wg.Done()
			_, issueErr = r.notes.Issue(r.ctx, r.tenantID, draft.ID)
		}()
		wg.Wait()
		if applyErr != nil || issueErr != nil {
			t.Fatalf("round %d: apply err = %v, issue err = %v", i, applyErr, issueErr)
		}
		inv := r.invoiceNow(t)
		if inv.AmountDueCents != 0 || inv.CreditsAppliedCents != 0 {
			t.Fatalf("round %d: invoice due %d / credits applied %d, want 0 / 0", i, inv.AmountDueCents, inv.CreditsAppliedCents)
		}
		if got := r.balance(t); got != 6000 {
			t.Fatalf("round %d: customer balance = %d, want all 6000 (the note covers the whole invoice)", i, got)
		}
		r.conserved(t)
	}
}

// A note may be as large as what the invoice can still be relieved of: the
// amount due plus the credits applied to it. Credits applied BEFORE the note is
// written come back the same way as credits applied after. Before 2026-10-12
// create refused anything above the amount due, so those credits could not be
// returned by a credit note at all.
func TestCreate_NoteMayCoverAppliedCredits_AndIssueReturnsThem(t *testing.T) {
	r := newOverCreditRig(t, "CN Sizing")
	r.applyCredits(t, 6000) // due 4000, credits applied 6000

	draft := r.draft(t, 10000) // the whole invoice
	if _, err := r.notes.Issue(r.ctx, r.tenantID, draft.ID); err != nil {
		t.Fatalf("Issue: %v", err)
	}
	inv := r.invoiceNow(t)
	if inv.AmountDueCents != 0 || inv.CreditsAppliedCents != 0 {
		t.Fatalf("invoice due %d / credits applied %d, want 0 / 0", inv.AmountDueCents, inv.CreditsAppliedCents)
	}
	if got := r.balance(t); got != 6000 {
		t.Fatalf("customer balance = %d, want 6000", got)
	}
	r.conserved(t)
}

// The ceiling is the invoice: one cent more than due plus applied credits is refused.
func TestCreate_NoteAboveDuePlusAppliedCredits_Refused(t *testing.T) {
	r := newOverCreditRig(t, "CN Sizing Cap")
	r.applyCredits(t, 6000)
	// Take 1000 of the amount due away so due + applied credits (9000) is below the total.
	first := r.draft(t, 1000)
	if _, err := r.notes.Issue(r.ctx, r.tenantID, first.ID); err != nil {
		t.Fatalf("Issue first: %v", err)
	}
	_, err := r.notes.Create(r.ctx, r.tenantID, creditnote.CreateInput{
		InvoiceID: r.inv.ID,
		Reason:    "order_change",
		Lines:     []creditnote.CreditLineInput{{Description: "credit", Quantity: 1, UnitAmountCents: 9001}},
	})
	if err == nil {
		t.Fatal("a 9001 note on an invoice with 3000 due and 6000 of applied credits was accepted")
	}
	if ok := r.draft(t, 9000); ok.TotalCents != 9000 {
		t.Fatalf("a 9000 note was not created at 9000: %d", ok.TotalCents)
	}
}
