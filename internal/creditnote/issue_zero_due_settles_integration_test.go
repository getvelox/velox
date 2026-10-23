package creditnote_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/credit"
	"github.com/sagarsuperuser/velox/internal/creditnote"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/tenant"
	"github.com/sagarsuperuser/velox/internal/testutil"
	"github.com/sagarsuperuser/velox/internal/webhook"
)

// A credit note that leaves an unpaid invoice with nothing due marks it paid
// in the note's own transaction (G3, loops review 2026-10-19). The real
// trigger: an operator waives a dunned invoice, one whose card payment failed,
// with a credit note for what is due. Stripe documents the target: a credit
// note that "reduces the balance of an open invoice to 0" makes the invoice
// paid. Before, the invoice stayed finalized with its payment failed and
// nothing due. No charge path takes a $0 invoice, and dunning either closed the
// run as recovered with the invoice unpaid, or ran its final action on it.

// pinnedClock pins every invoice to one simulated instant, standing in for the
// billing engine's clock.Resolver on an invoice under a test clock.
type pinnedClock struct{ at time.Time }

func (pinnedClock) SimForCustomer(context.Context, string, string) (clock.Sim, error) {
	return clock.Sim{}, errs.ErrNotFound
}
func (pinnedClock) SimForSubscription(context.Context, string, string) (clock.Sim, error) {
	return clock.Sim{}, errs.ErrNotFound
}
func (p pinnedClock) SimForInvoice(context.Context, string, string) (clock.Sim, error) {
	return clock.Sim{At: p.at, TestClockID: "vlx_tclk_zero_due"}, nil
}

// bypassExec runs one statement on a transaction RLS does not filter, in test
// mode (the livemode trigger stamps inserted rows from the session setting).
func bypassExec(t *testing.T, ctx context.Context, db *postgres.DB, query string, args ...any) {
	t.Helper()
	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin bypass: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.livemode', 'off', true)`); err != nil {
		t.Fatalf("pin livemode: %v", err)
	}
	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit bypass: %v", err)
	}
}

// bypassCount runs a count(*) query that RLS does not filter.
func bypassCount(t *testing.T, ctx context.Context, db *postgres.DB, query string, args ...any) int {
	t.Helper()
	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin bypass: %v", err)
	}
	defer postgres.Rollback(tx)
	var n int
	if err := tx.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", query, err)
	}
	return n
}

// setChargeHold writes an invoice's charge hold as a claim does. A live hold
// ends in the future (a charge is running); a stale one ended an hour ago (a
// charge whose result was lost, waiting for the stale-hold sweep).
func setChargeHold(t *testing.T, ctx context.Context, db *postgres.DB, invoiceID string, live bool) {
	t.Helper()
	until := "now() - interval '1 hour'"
	if live {
		until = "now() + interval '5 minutes'"
	}
	bypassExec(t, ctx, db, `UPDATE invoices SET auto_charge_claimed_until = `+until+` WHERE id = $1`, invoiceID)
}

// clearChargeHold is what the stale-hold sweep does when Stripe has nothing.
func clearChargeHold(t *testing.T, ctx context.Context, db *postgres.DB, invoiceID string) {
	t.Helper()
	bypassExec(t, ctx, db, `UPDATE invoices SET auto_charge_claimed_until = NULL WHERE id = $1`, invoiceID)
}

// zeroDueRig is one finalized 100.00 invoice with nothing paid, and the real
// credit-note, credit and invoice services over real Postgres. invoice.paid
// goes to the real webhook outbox, as router.go wires it.
type zeroDueRig struct {
	ctx      context.Context
	db       *postgres.DB
	tenantID string
	inv      domain.Invoice
	invStore *invoice.PostgresStore
	notes    *creditnote.Service
	at       time.Time // the invoice's simulated now
}

func newZeroDueRig(t *testing.T, name string, payment domain.InvoicePaymentStatus) *zeroDueRig {
	t.Helper()
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, name)
	_, inv := seedInvoiceForCN(t, db, tenantID, "zero-due", domain.InvoiceFinalized, payment, "")

	invStore := invoice.NewPostgresStore(db)
	invStore.SetOutboxEnqueuer(webhook.NewOutboxStore(db))
	credits := credit.NewService(credit.NewPostgresStore(db))
	settings := tenant.NewSettingsStore(db)
	notes := creditnote.NewService(creditnote.NewPostgresStore(db), invStore, nil, ledgerGranter{credits})
	notes.SetNumberGenerator(settings)
	at := time.Date(2026, 9, 14, 10, 30, 0, 0, time.UTC)
	notes.SetResolver(pinnedClock{at: at})
	return &zeroDueRig{ctx: ctx, db: db, tenantID: tenantID, inv: inv, invStore: invStore, notes: notes, at: at}
}

func (r *zeroDueRig) draft(t *testing.T, cents int64) domain.CreditNote {
	t.Helper()
	cn, err := r.notes.Create(r.ctx, r.tenantID, creditnote.CreateInput{
		InvoiceID: r.inv.ID,
		Reason:    "waived",
		Lines:     []creditnote.CreditLineInput{{Description: "waive the invoice", Quantity: 1, UnitAmountCents: cents}},
	})
	if err != nil {
		t.Fatalf("Create %d: %v", cents, err)
	}
	return cn
}

func (r *zeroDueRig) note(t *testing.T, id string) domain.CreditNote {
	t.Helper()
	cn, err := r.notes.Get(r.ctx, r.tenantID, id)
	if err != nil {
		t.Fatalf("get credit note: %v", err)
	}
	return cn
}

func (r *zeroDueRig) invoiceNow(t *testing.T) domain.Invoice {
	t.Helper()
	inv, err := r.invStore.Get(r.ctx, r.tenantID, r.inv.ID)
	if err != nil {
		t.Fatalf("get invoice: %v", err)
	}
	return inv
}

// paidEvents counts the invoice.paid rows queued for the invoice.
func (r *zeroDueRig) paidEvents(t *testing.T) int {
	t.Helper()
	return bypassCount(t, r.ctx, r.db,
		`SELECT count(*) FROM webhook_outbox WHERE tenant_id = $1 AND event_type = $2 AND payload->'invoice'->>'id' = $3`,
		r.tenantID, domain.EventInvoicePaid, r.inv.ID)
}

// assertOpenAtZero checks the invoice was left as the note made it: finalized,
// its payment as before, nothing due, and no invoice.paid.
func (r *zeroDueRig) assertOpenAtZero(t *testing.T, payment domain.InvoicePaymentStatus) {
	t.Helper()
	inv := r.invoiceNow(t)
	if inv.Status != domain.InvoiceFinalized || inv.PaymentStatus != payment || inv.AmountDueCents != 0 {
		t.Errorf("invoice %s/%s with %d due, want finalized/%s with 0 due: it was marked paid", inv.Status, inv.PaymentStatus, inv.AmountDueCents, payment)
	}
	if n := r.paidEvents(t); n != 0 {
		t.Errorf("%d invoice.paid events queued, want none", n)
	}
}

// Mutation seam: remove the SettleZeroDueTx call in creditnote.Service.Issue →
// "leaves nothing due" goes red.
func TestIssue_ToZeroOnDunnedInvoice_MarksPaidInIssueTx(t *testing.T) {
	t.Run("leaves nothing due", func(t *testing.T) {
		r := newZeroDueRig(t, "CN Zero Due Settles", domain.PaymentFailed)
		// The customer had opened the hosted payment page: an open checkout claim.
		bypassExec(t, r.ctx, r.db, `INSERT INTO checkout_sessions (id, tenant_id, invoice_id, livemode, amount_cents, currency, status, expires_at)
			VALUES ('vlx_cs_zero_due', $1, $2, false, 10000, 'USD', 'open', now() + interval '1 hour')`, r.tenantID, r.inv.ID)
		draft := r.draft(t, 10000)

		issued, err := r.notes.Issue(r.ctx, r.tenantID, draft.ID)
		if err != nil {
			t.Fatalf("Issue: %v", err)
		}
		if issued.Status != domain.CreditNoteIssued {
			t.Fatalf("note %s, want issued", issued.Status)
		}
		inv := r.invoiceNow(t)
		if inv.Status != domain.InvoicePaid || inv.PaymentStatus != domain.PaymentSucceeded {
			t.Errorf("invoice %s/%s, want paid/succeeded: the note left nothing due", inv.Status, inv.PaymentStatus)
		}
		if inv.AmountDueCents != 0 || inv.AmountPaidCents != 0 {
			t.Errorf("invoice due %d / paid %d, want 0 / 0: no money was taken", inv.AmountDueCents, inv.AmountPaidCents)
		}
		if inv.PaidAt == nil || !inv.PaidAt.Equal(r.at) {
			t.Errorf("paid_at %v, want the invoice's own clock %v", inv.PaidAt, r.at)
		}
		if n := r.paidEvents(t); n != 1 {
			t.Errorf("%d invoice.paid events queued, want exactly 1", n)
		}
		if n := bypassCount(t, r.ctx, r.db, `SELECT count(*) FROM checkout_sessions WHERE invoice_id = $1 AND status = 'open'`, r.inv.ID); n != 0 {
			t.Errorf("%d open checkout claims on the settled invoice, want none", n)
		}
	})

	// The settle rides the note's transaction: when the issue fails, nothing
	// of it commits.
	t.Run("the audit row fails: nothing commits", func(t *testing.T) {
		r := newZeroDueRig(t, "CN Zero Due Atomic", domain.PaymentFailed)
		draft := r.draft(t, 10000)
		r.notes.SetAuditLogger(&failingCNEmitter{})

		if _, err := r.notes.Issue(r.ctx, r.tenantID, draft.ID); err == nil {
			t.Fatal("Issue succeeded with a failing audit row")
		}
		if got := r.note(t, draft.ID); got.Status != domain.CreditNoteDraft {
			t.Errorf("note %s, want draft", got.Status)
		}
		inv := r.invoiceNow(t)
		if inv.Status != domain.InvoiceFinalized || inv.AmountDueCents != 10000 {
			t.Errorf("invoice %s with %d due, want finalized with 10000 due", inv.Status, inv.AmountDueCents)
		}
		if n := r.paidEvents(t); n != 0 {
			t.Errorf("%d invoice.paid events queued by an issue that rolled back, want none", n)
		}
	})

	t.Run("leaves one cent due: not settled", func(t *testing.T) {
		r := newZeroDueRig(t, "CN Zero Due One Cent", domain.PaymentFailed)
		draft := r.draft(t, 9999)
		if _, err := r.notes.Issue(r.ctx, r.tenantID, draft.ID); err != nil {
			t.Fatalf("Issue: %v", err)
		}
		inv := r.invoiceNow(t)
		if inv.Status != domain.InvoiceFinalized || inv.PaymentStatus != domain.PaymentFailed || inv.AmountDueCents != 1 {
			t.Errorf("invoice %s/%s with %d due, want finalized/failed with 1 due", inv.Status, inv.PaymentStatus, inv.AmountDueCents)
		}
		if n := r.paidEvents(t); n != 0 {
			t.Errorf("%d invoice.paid events queued, want none", n)
		}
	})
}

// A charge's result was lost and its hold waits for the stale-hold sweep to
// ask Stripe; the charge may have taken the money. An operator's credit note
// to $0 still issues, but does not mark the invoice paid. Passes before the
// fix too (nothing marked it paid then); it pins the hold check.
//
// Mutation seam: drop the ChargeHeldTx check in SettleZeroDueTx → red.
func TestIssue_ToZeroUnderStaleHold_IssuesWithoutSettling(t *testing.T) {
	r := newZeroDueRig(t, "CN Zero Due Stale Hold", domain.PaymentFailed)
	draft := r.draft(t, 10000)
	setChargeHold(t, r.ctx, r.db, r.inv.ID, false)

	issued, err := r.notes.Issue(r.ctx, r.tenantID, draft.ID)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if issued.Status != domain.CreditNoteIssued {
		t.Fatalf("note %s, want issued", issued.Status)
	}
	r.assertOpenAtZero(t, domain.PaymentFailed)
}

// A credit note to $0 races a charge claim on the same invoice: a dunning
// retry on a failed invoice, the auto-charge sweep on a pending one. They
// serialize on the invoice row, so exactly one wins: the note marks the
// invoice paid and the claim is refused, or the claim takes the hold and the
// note issues without marking it paid. Never both.
//
// Mutation seam: read the hold before LockForCreditNoteTx (outside the row
// lock) → a claim can commit between that read and the settle, and both win.
func TestIssue_ToZeroRacingAChargeClaim_ExactlyOneWins(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payment domain.InvoicePaymentStatus
		claim   func(s *invoice.PostgresStore, ctx context.Context, tenantID, id string) (domain.ChargeHold, error)
	}{
		{"dunning retry", domain.PaymentFailed, (*invoice.PostgresStore).ClaimChargeForDunningRetry},
		{"auto-charge", domain.PaymentPending, (*invoice.PostgresStore).ClaimAutoCharge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var noteWon, claimWon int
			for i := 0; i < 8; i++ {
				r := newZeroDueRig(t, "CN Zero Due Race", tc.payment)
				draft := r.draft(t, 10000)

				var wg sync.WaitGroup
				var hold domain.ChargeHold
				var claimErr, issueErr error
				wg.Add(2)
				go func() {
					defer wg.Done()
					hold, claimErr = tc.claim(r.invStore, r.ctx, r.tenantID, r.inv.ID)
				}()
				go func() {
					defer wg.Done()
					_, issueErr = r.notes.Issue(r.ctx, r.tenantID, draft.ID)
				}()
				wg.Wait()
				if claimErr != nil || issueErr != nil {
					t.Fatalf("round %d: claim err = %v, issue err = %v", i, claimErr, issueErr)
				}
				if got := r.note(t, draft.ID); got.Status != domain.CreditNoteIssued {
					t.Fatalf("round %d: note %s, want issued", i, got.Status)
				}
				inv := r.invoiceNow(t)
				switch {
				case inv.Status == domain.InvoicePaid && !hold.Held():
					noteWon++
				case inv.Status == domain.InvoiceFinalized && inv.AmountDueCents == 0 && hold.Held():
					claimWon++
				default:
					t.Fatalf("round %d: invoice %s/%s with %d due, claim held=%v; want the note to mark it paid and the claim refused, or the claim held and the invoice open at 0",
						i, inv.Status, inv.PaymentStatus, inv.AmountDueCents, hold.Held())
				}
				want := 0
				if inv.Status == domain.InvoicePaid {
					want = 1
				}
				if n := r.paidEvents(t); n != want {
					t.Fatalf("round %d: %d invoice.paid events queued, want %d", i, n, want)
				}
			}
			t.Logf("note won %d rounds, claim won %d", noteWon, claimWon)
		})
	}
}
