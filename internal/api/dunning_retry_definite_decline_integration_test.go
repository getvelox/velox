package api

import (
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sagarsuperuser/velox/internal/credit"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/dunning"
	"github.com/sagarsuperuser/velox/internal/email"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/payment"
	"github.com/sagarsuperuser/velox/internal/platform/leader"
	"github.com/sagarsuperuser/velox/internal/platform/leader/leadertest"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
	"github.com/sagarsuperuser/velox/internal/webhook"
)

// A dunning retry counts only a definite answer (G2, loops review
// 2026-10-19). The real trigger: Postgres drops the connection while a retry
// reads or claims the invoice, or settles it from credit. Before, that error
// spent an attempt, its text reached the customer's warning email, and on the
// last attempt the final action ran with no final charge.
//
// Each test runs the real dunning.Service.ProcessDueRuns over the production
// retry adapter, the real dunning, invoice and credit stores, and a scripted
// Stripe. One invoice store call fails once, as a dropped connection fails it.

// errConnDropped is what pgx returns when Postgres ends the connection under
// a query: a failover, a restart, or pg_terminate_backend.
var errConnDropped = &pgconn.PgError{Severity: "FATAL", Code: "57P01", Message: "terminating connection due to administrator command"}

// faultInvoices is the retry adapter's invoice store: the real store, except
// that the nth call of one method fails. With lostAck the real call commits
// first and only its answer is lost.
type faultInvoices struct {
	*invoice.PostgresStore
	method  string
	nth     int
	lostAck bool
	calls   int
}

func (f *faultInvoices) trips(method string) bool {
	if method != f.method {
		return false
	}
	f.calls++
	return f.calls == f.nth
}

func (f *faultInvoices) Get(ctx context.Context, tenantID, id string) (domain.Invoice, error) {
	if f.trips("Get") {
		return domain.Invoice{}, errConnDropped
	}
	return f.PostgresStore.Get(ctx, tenantID, id)
}

func (f *faultInvoices) ClaimChargeForDunningRetry(ctx context.Context, tenantID, id string) (domain.ChargeHold, error) {
	if f.trips("ClaimChargeForDunningRetry") {
		if f.lostAck {
			if _, err := f.PostgresStore.ClaimChargeForDunningRetry(ctx, tenantID, id); err != nil {
				return domain.ChargeHold{}, err
			}
		}
		return domain.ChargeHold{}, errConnDropped
	}
	return f.PostgresStore.ClaimChargeForDunningRetry(ctx, tenantID, id)
}

func (f *faultInvoices) MarkPaid(ctx context.Context, tenantID, id, stripePaymentIntentID string, paidAt time.Time) (domain.Invoice, error) {
	if f.trips("MarkPaid") {
		return domain.Invoice{}, errConnDropped
	}
	return f.PostgresStore.MarkPaid(ctx, tenantID, id, stripePaymentIntentID, paidAt)
}

// faultLedger is the retry's credit ledger: the real store, except that the
// next credit apply fails while failApply is set.
type faultLedger struct {
	*credit.PostgresStore
	failApply bool
}

func (f *faultLedger) ApplyToInvoiceAtomic(ctx context.Context, tenantID, customerID, invoiceID, invoiceDesc string, invoiceAmountCents int64, at time.Time) (int64, error) {
	if f.failApply {
		f.failApply = false
		return 0, errConnDropped
	}
	return f.PostgresStore.ApplyToInvoiceAtomic(ctx, tenantID, customerID, invoiceID, invoiceDesc, invoiceAmountCents, at)
}

// captureSlog sends the default logger to a buffer for the rest of the test.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// settleFaultInvoices is the charge path's invoice store with its first card
// settle failing: Stripe took the money and Velox could not record it.
type settleFaultInvoices struct {
	*invoice.PostgresStore
	failed bool
}

func (s *settleFaultInvoices) MarkPaidCardSettlementTransition(ctx context.Context, tenantID, id, stripePaymentIntentID string, paidAt time.Time, then func(tx *sql.Tx, fresh domain.Invoice) error) (domain.Invoice, bool, error) {
	if !s.failed {
		s.failed = true
		return domain.Invoice{}, false, errConnDropped
	}
	return s.PostgresStore.MarkPaidCardSettlementTransition(ctx, tenantID, id, stripePaymentIntentID, paidAt, then)
}

type dunningRetryRig struct {
	*hardDeclineRig
	runs         *dunning.PostgresStore
	ledger       *credit.PostgresStore
	ledgerFaults *faultLedger
	credits      *credit.Service
	faults       *faultInvoices
	policy       domain.DunningPolicy
	svc          *dunning.Service
}

func newDunningRetryRig(t *testing.T, name string) *dunningRetryRig {
	t.Helper()
	r := newHardDeclineRig(t, name)
	r.ctx = leadertest.Token(t, testutil.AdminPool(t), r.ctx, leader.RoleDunning)
	d := &dunningRetryRig{hardDeclineRig: r, runs: dunning.NewPostgresStore(r.db)}
	d.ledger = credit.NewPostgresStore(r.db)
	d.ledgerFaults = &faultLedger{PostgresStore: d.ledger}
	d.credits = credit.NewService(d.ledgerFaults)
	d.faults = &faultInvoices{PostgresStore: r.invoices}
	policy, err := d.runs.UpsertPolicy(r.ctx, r.tenantID, domain.DunningPolicy{
		Name: "default", Enabled: true, RetryDays: []int{3, 3, 3},
		FinalSubscriptionAction: domain.SubActionNone, FinalInvoiceAction: domain.InvActionMarkUncollectible,
	})
	if err != nil {
		t.Fatalf("upsert policy: %v", err)
	}
	d.policy = policy
	d.useCharger(r.charger)
	return d
}

// useCharger wires the dunning service as router.go does, with the retry
// adapter charging through charger.
func (d *dunningRetryRig) useCharger(charger *payment.Stripe) {
	svc := dunning.NewService(d.runs, &paymentRetrierAdapter{
		charger: charger, invoiceStore: d.faults, paymentSetups: d.retrier.paymentSetups, credits: d.credits,
	}, nil)
	svc.SetSubscriptionPauser(nil, d.invoices)
	svc.SetInvoiceUncollectibleMarker(&invoiceUncollectibleAdapter{svc: d.invSvc})
	svc.SetChargeHoldClaimer(d.invoices)
	svc.SetEmailNotifier(email.NewOutboxSender(email.NewOutboxStore(d.db)))
	svc.SetCustomerEmailFetcher(&customerEmailFetcherAdapter{store: d.customers})
	d.svc = svc
}

// dunnedInvoice is a finalized $50 invoice whose charge was declined.
func (d *dunningRetryRig) dunnedInvoice(custID, num string) domain.Invoice {
	d.t.Helper()
	now := time.Now().UTC()
	inv, err := d.invoices.Create(d.ctx, d.tenantID, domain.Invoice{
		CustomerID: custID, InvoiceNumber: num, Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentFailed,
		Currency: "USD", SubtotalCents: 5000, TotalAmountCents: 5000, AmountDueCents: 5000,
		BillingPeriodStart: now.Add(-30 * 24 * time.Hour), BillingPeriodEnd: now, IssuedAt: &now,
	})
	if err != nil {
		d.t.Fatalf("create invoice: %v", err)
	}
	return inv
}

// dueRun is an active run on the invoice that has used `attempts` retries,
// the last one 72 hours ago, and whose next retry is due now.
func (d *dunningRetryRig) dueRun(invID, custID string, attempts int) domain.InvoiceDunningRun {
	d.t.Helper()
	due := time.Now().UTC().Add(-time.Minute)
	run, err := d.runs.CreateRun(d.ctx, d.tenantID, domain.InvoiceDunningRun{
		InvoiceID: invID, CustomerID: custID, PolicyID: d.policy.ID,
		State: domain.DunningActive, Reason: "payment_failed", AttemptCount: attempts, NextActionAt: &due,
	})
	if err != nil {
		d.t.Fatalf("create run: %v", err)
	}
	last := due.Add(-72 * time.Hour)
	run.LastAttemptAt = &last
	if ok, err := d.runs.UpdateRunIfActive(d.ctx, d.tenantID, run, attempts, nil); err != nil || !ok {
		d.t.Fatalf("record the earlier attempts: applied=%v err=%v", ok, err)
	}
	return d.run(run.ID)
}

// tick is one scheduler pass of dunning for this tenant.
func (d *dunningRetryRig) tick() {
	d.t.Helper()
	if _, errs := d.svc.ProcessDueRuns(d.ctx, d.tenantID, 20); len(errs) > 0 {
		d.t.Errorf("ProcessDueRuns: %v", errs)
	}
}

func (d *dunningRetryRig) run(id string) domain.InvoiceDunningRun {
	d.t.Helper()
	run, err := d.runs.GetRun(d.ctx, d.tenantID, id)
	if err != nil {
		d.t.Fatalf("get run: %v", err)
	}
	return run
}

func (d *dunningRetryRig) events(runID string, typ domain.DunningEventType) []domain.InvoiceDunningEvent {
	d.t.Helper()
	all, err := d.runs.ListEvents(d.ctx, d.tenantID, runID)
	if err != nil {
		d.t.Fatalf("list events: %v", err)
	}
	var out []domain.InvoiceDunningEvent
	for _, e := range all {
		if e.EventType == typ {
			out = append(out, e)
		}
	}
	return out
}

func (d *dunningRetryRig) warningEmails() int {
	d.t.Helper()
	tx, err := d.db.BeginTx(d.ctx, postgres.TxBypass, "")
	if err != nil {
		d.t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	var n int
	if err := tx.QueryRowContext(d.ctx, `SELECT count(*) FROM email_outbox WHERE tenant_id = $1 AND email_type = $2`,
		d.tenantID, email.TypeDunningWarning).Scan(&n); err != nil {
		d.t.Fatalf("count warning emails: %v", err)
	}
	return n
}

func (d *dunningRetryRig) clearHold(invID string) {
	d.t.Helper()
	tx, err := d.db.BeginTx(d.ctx, postgres.TxBypass, "")
	if err != nil {
		d.t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(d.ctx, `UPDATE invoices SET auto_charge_claimed_until = NULL WHERE id = $1`, invID); err != nil {
		d.t.Fatalf("clear hold: %v", err)
	}
	if err := tx.Commit(); err != nil {
		d.t.Fatalf("commit: %v", err)
	}
}

// paidEvents counts the invoice.paid rows queued for the invoice.
func (d *dunningRetryRig) paidEvents(invID string) int {
	d.t.Helper()
	tx, err := d.db.BeginTx(d.ctx, postgres.TxBypass, "")
	if err != nil {
		d.t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	var n int
	if err := tx.QueryRowContext(d.ctx, `SELECT count(*) FROM webhook_outbox WHERE tenant_id = $1 AND event_type = $2 AND payload->'invoice'->>'id' = $3`,
		d.tenantID, domain.EventInvoicePaid, invID).Scan(&n); err != nil {
		d.t.Fatalf("count invoice.paid: %v", err)
	}
	return n
}

func (d *dunningRetryRig) isDue(runID string) bool {
	d.t.Helper()
	due, err := d.runs.ListDueRuns(d.ctx, d.tenantID, time.Now().UTC(), 20)
	if err != nil {
		d.t.Fatalf("list due runs: %v", err)
	}
	for _, r := range due {
		if r.ID == runID {
			return true
		}
	}
	return false
}

func (d *dunningRetryRig) grant(custID string, cents int64) {
	d.t.Helper()
	if _, err := d.credits.Grant(d.ctx, d.tenantID, credit.GrantInput{Currency: "USD", CustomerID: custID, AmountCents: cents, Description: "goodwill"}); err != nil {
		d.t.Fatalf("grant credit: %v", err)
	}
}

func (d *dunningRetryRig) creditUsage(custID, invID string) []domain.CreditLedgerEntry {
	d.t.Helper()
	entries, err := d.ledger.ListEntries(d.ctx, credit.ListFilter{
		TenantID: d.tenantID, CustomerID: custID, InvoiceID: invID, EntryType: string(domain.CreditUsage),
	})
	if err != nil {
		d.t.Fatalf("list credit usage: %v", err)
	}
	return entries
}

// assertNotCounted checks that the pass left the run as it found it: same
// attempt count, last attempt and next retry, no retry event, no warning
// email.
func (d *dunningRetryRig) assertNotCounted(before domain.InvoiceDunningRun) {
	d.t.Helper()
	got := d.run(before.ID)
	if got.State != domain.DunningActive || got.AttemptCount != before.AttemptCount {
		d.t.Errorf("run %s with %d attempts, want active with %d: the retry was counted", got.State, got.AttemptCount, before.AttemptCount)
	}
	if !sameInstant(got.LastAttemptAt, before.LastAttemptAt) || !sameInstant(got.NextActionAt, before.NextActionAt) {
		d.t.Errorf("last attempt %s, next retry %s; want %s, %s unchanged",
			instant(got.LastAttemptAt), instant(got.NextActionAt), instant(before.LastAttemptAt), instant(before.NextActionAt))
	}
	if evs := d.events(before.ID, domain.DunningEventRetryAttempted); len(evs) != 0 {
		d.t.Errorf("retry_attempted events %+v, want none", evs)
	}
	if n := d.warningEmails(); n != 0 {
		d.t.Errorf("%d warning emails queued, want none", n)
	}
}

func sameInstant(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

func instant(t *time.Time) string {
	if t == nil {
		return "<nil>"
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// The invoice read fails: the retry is not counted, and the next pass charges
// and counts the decline once.
func TestDunningRetry_InvoiceReadError_NotCounted(t *testing.T) {
	d := newDunningRetryRig(t, "Retry Read Error")
	custID := d.customerWithCard("readerr", "pm_readerr")
	inv := d.dunnedInvoice(custID, "INV-G2-READ")
	before := d.dueRun(inv.ID, custID, 1)
	d.charges.answers = []scriptedCharge{{err: stripeDeclined("pi_readerr_2", "insufficient_funds", "Your card has insufficient funds.")}}
	d.faults.method, d.faults.nth = "Get", 1

	d.tick()
	d.assertNotCounted(before)
	if n := len(d.charges.charged); n != 0 {
		t.Errorf("Stripe received %d charges, want none", n)
	}
	if at := d.holdAt(inv.ID); at != nil {
		t.Errorf("charge hold set (%v) by a retry that failed before its claim", *at)
	}

	// The connection is back. The next pass charges, and the decline counts once.
	d.tick()
	if got := d.run(before.ID); got.AttemptCount != 2 {
		t.Errorf("attempt count %d after the decline, want 2", got.AttemptCount)
	}
	if n := len(d.charges.charged); n != 1 {
		t.Errorf("Stripe received %d charges, want 1", n)
	}
	evs := d.events(before.ID, domain.DunningEventRetryAttempted)
	if len(evs) != 1 || evs[0].Metadata["payment_intent_id"] != "pi_readerr_2" {
		t.Errorf("retry_attempted events %+v, want one for pi_readerr_2", evs)
	}
	if n := d.warningEmails(); n != 1 {
		t.Errorf("%d warning emails queued, want 1 for the decline", n)
	}
}

// The claim fails. When the claim committed and only its answer was lost, the
// hold it left keeps the run out of the due list until the stale-hold sweep
// clears it.
func TestDunningRetry_ClaimError_NotCounted(t *testing.T) {
	t.Run("the claim fails", func(t *testing.T) {
		d := newDunningRetryRig(t, "Retry Claim Error")
		custID := d.customerWithCard("claimerr", "pm_claimerr")
		inv := d.dunnedInvoice(custID, "INV-G2-CLAIM")
		before := d.dueRun(inv.ID, custID, 1)
		d.faults.method, d.faults.nth = "ClaimChargeForDunningRetry", 1

		d.tick()
		d.assertNotCounted(before)
		if n := len(d.charges.charged); n != 0 {
			t.Errorf("Stripe received %d charges, want none", n)
		}
		if at := d.holdAt(inv.ID); at != nil {
			t.Errorf("charge hold set (%v) by a claim that failed", *at)
		}
	})

	t.Run("the claim commits and its answer is lost", func(t *testing.T) {
		d := newDunningRetryRig(t, "Retry Claim Lost Ack")
		custID := d.customerWithCard("claimack", "pm_claimack")
		inv := d.dunnedInvoice(custID, "INV-G2-CLAIM-ACK")
		before := d.dueRun(inv.ID, custID, 1)
		d.faults.method, d.faults.nth, d.faults.lostAck = "ClaimChargeForDunningRetry", 1, true

		d.tick()
		d.assertNotCounted(before)
		if n := len(d.charges.charged); n != 0 {
			t.Errorf("Stripe received %d charges, want none", n)
		}
		if d.holdAt(inv.ID) == nil {
			t.Fatal("the committed claim's hold is gone; only the stale-hold sweep may clear it")
		}
		if d.isDue(before.ID) {
			t.Error("the run is due while its invoice's hold is set")
		}
		d.clearHold(inv.ID) // what the stale-hold sweep does when Stripe has nothing
		if !d.isDue(before.ID) {
			t.Error("the run is not due after the hold cleared")
		}
	})
}

// Credits cover the invoice and marking it paid fails: the retry is not
// counted, the hold is released, and the credit is applied once. The next pass
// finds nothing due and marks the invoice paid (G3). Before, it closed the run
// as recovered and left the invoice unpaid at $0, with no invoice.paid.
//
// Mutation seam: in retryPayment, return nil for a $0 invoice again → the
// second pass leaves the invoice unpaid.
func TestDunningRetry_CreditCoveredMarkPaidError_NotCounted_HoldReleased(t *testing.T) {
	d := newDunningRetryRig(t, "Retry Covered MarkPaid Error")
	d.invoices.SetOutboxEnqueuer(webhook.NewOutboxStore(d.db))
	custID := d.customerWithCard("covered", "pm_covered")
	inv := d.dunnedInvoice(custID, "INV-G2-COVERED")
	d.grant(custID, 5000)
	before := d.dueRun(inv.ID, custID, 1)
	d.faults.method, d.faults.nth = "MarkPaid", 1

	d.tick()
	d.assertNotCounted(before)
	if n := len(d.charges.charged); n != 0 {
		t.Errorf("Stripe received %d charges for an invoice credits covered", n)
	}
	if at := d.holdAt(inv.ID); at != nil {
		t.Errorf("charge hold still set (%v) after a settle that sent no charge", *at)
	}
	if usage := d.creditUsage(custID, inv.ID); len(usage) != 1 || usage[0].AmountCents != -5000 {
		t.Errorf("credit usage entries %+v, want one of -5000", usage)
	}

	// The next pass marks it paid.
	d.tick()
	if got := d.invoice(inv.ID); got.Status != domain.InvoicePaid || got.AmountDueCents != 0 {
		t.Errorf("invoice %s/%s with %d due, want paid: credits covered it", got.Status, got.PaymentStatus, got.AmountDueCents)
	}
	if n := d.paidEvents(inv.ID); n != 1 {
		t.Errorf("%d invoice.paid events queued, want exactly 1", n)
	}
	if got := d.run(before.ID); got.State != domain.DunningResolved || got.Resolution != domain.ResolutionPaymentRecovered {
		t.Errorf("run %s/%q, want resolved as payment_recovered", got.State, got.Resolution)
	}
	if usage := d.creditUsage(custID, inv.ID); len(usage) != 1 || usage[0].AmountCents != -5000 {
		t.Errorf("credit usage entries %+v, want still one of -5000", usage)
	}
	if n := len(d.charges.charged); n != 0 {
		t.Errorf("Stripe received %d charges for an invoice credits covered", n)
	}
}

// Applying credit fails: the retry is not counted, the hold is released, and a
// WARN names the invoice and the error. Before, this step skipped with no log
// line, so an operator could not see why the retry did not run.
func TestDunningRetry_CreditApplyError_NotCounted_Logged(t *testing.T) {
	d := newDunningRetryRig(t, "Retry Credit Apply Error")
	custID := d.customerWithCard("applyerr", "pm_applyerr")
	inv := d.dunnedInvoice(custID, "INV-G2-APPLY")
	before := d.dueRun(inv.ID, custID, 1)
	d.ledgerFaults.failApply = true
	logs := captureSlog(t)

	d.tick()
	d.assertNotCounted(before)
	if n := len(d.charges.charged); n != 0 {
		t.Errorf("Stripe received %d charges, want none", n)
	}
	if at := d.holdAt(inv.ID); at != nil {
		t.Errorf("charge hold still set (%v) after a retry that sent no charge", *at)
	}
	warned := false
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "level=WARN") && strings.Contains(line, inv.ID) &&
			strings.Contains(line, "apply credits") && strings.Contains(line, "57P01") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("no WARN names invoice %s and the credit apply's error; logs:\n%s", inv.ID, logs.String())
	}
}

// Credits cover part of the invoice and the read after applying them fails:
// the retry is not counted and the hold is released. The next pass charges
// the rest once, without applying the credit again.
func TestDunningRetry_RefreshAfterCreditError_NotCounted_HoldReleased(t *testing.T) {
	d := newDunningRetryRig(t, "Retry Refresh Error")
	custID := d.customerWithCard("partial", "pm_partial")
	inv := d.dunnedInvoice(custID, "INV-G2-PARTIAL")
	d.grant(custID, 2000)
	before := d.dueRun(inv.ID, custID, 1)
	d.charges.answers = []scriptedCharge{{pi: "pi_partial_paid"}}
	d.faults.method, d.faults.nth = "Get", 2 // the read after the credit apply

	d.tick()
	d.assertNotCounted(before)
	if n := len(d.charges.charged); n != 0 {
		t.Errorf("Stripe received %d charges, want none", n)
	}
	if at := d.holdAt(inv.ID); at != nil {
		t.Errorf("charge hold still set (%v) after a retry that sent no charge", *at)
	}

	d.tick()
	if len(d.charges.amounts) != 1 || d.charges.amounts[0] != 3000 {
		t.Errorf("Stripe charges %v cents, want one of 3000 (the invoice less the credit)", d.charges.amounts)
	}
	if usage := d.creditUsage(custID, inv.ID); len(usage) != 1 || usage[0].AmountCents != -2000 {
		t.Errorf("credit usage entries %+v, want one of -2000", usage)
	}
	if got := d.invoice(inv.ID); got.Status != domain.InvoicePaid {
		t.Errorf("invoice %s, want paid", got.Status)
	}
	if got := d.run(before.ID); got.State != domain.DunningResolved {
		t.Errorf("run %s, want resolved", got.State)
	}
}

// The money test. The invoice read fails on the final retry: no final action
// runs, because no final charge was sent. The next pass sends it.
func TestDunningRetry_FinalAttemptDBError_NoFinalAction(t *testing.T) {
	d := newDunningRetryRig(t, "Retry Final Attempt Error")
	custID := d.customerWithCard("final", "pm_final")
	inv := d.dunnedInvoice(custID, "INV-G2-FINAL")
	before := d.dueRun(inv.ID, custID, d.policy.Retries()-1)
	d.charges.answers = []scriptedCharge{{pi: "pi_final_paid"}}
	d.faults.method, d.faults.nth = "Get", 1

	d.tick()
	if got := d.invoice(inv.ID); got.Status != domain.InvoiceFinalized {
		t.Errorf("invoice %s, want finalized: the final action ran with no final charge", got.Status)
	}
	if evs := d.events(before.ID, domain.DunningEventEscalated); len(evs) != 0 {
		t.Errorf("escalated events %+v, want none", evs)
	}
	d.assertNotCounted(before)
	if n := len(d.charges.charged); n != 0 {
		t.Errorf("Stripe received %d charges, want none", n)
	}

	d.tick()
	if n := len(d.charges.charged); n != 1 {
		t.Fatalf("Stripe received %d charges on the next pass, want the final charge", n)
	}
	if got := d.invoice(inv.ID); got.Status != domain.InvoicePaid {
		t.Errorf("invoice %s, want paid", got.Status)
	}
	if got := d.run(before.ID); got.State != domain.DunningResolved {
		t.Errorf("run %s, want resolved", got.State)
	}
}

// Stripe took the money and recording it failed. The retry is not counted,
// and the hold stays for the stale-hold sweep, which asks Stripe and records
// the payment. Passed before the fix too; it pins the settle error as a skip
// now that the adapter, not the payment package's classifier, decides.
func TestDunningRetry_StripeSucceeded_InlineSettleError_Skipped(t *testing.T) {
	d := newDunningRetryRig(t, "Retry Settle Error")
	d.useCharger(payment.NewStripe(d.charges, &settleFaultInvoices{PostgresStore: d.invoices},
		payment.NewPostgresWebhookStore(d.db), d.retrier.paymentSetups))
	custID := d.customerWithCard("settle", "pm_settle")
	inv := d.dunnedInvoice(custID, "INV-G2-SETTLE")
	before := d.dueRun(inv.ID, custID, 1)
	d.charges.answers = []scriptedCharge{{pi: "pi_settle_lost"}}

	d.tick()
	if n := len(d.charges.charged); n != 1 {
		t.Fatalf("Stripe received %d charges, want 1", n)
	}
	d.assertNotCounted(before)
	if d.holdAt(inv.ID) == nil {
		t.Error("the hold was released; it must stay until the stale-hold sweep has asked Stripe")
	}
	if got := d.invoice(inv.ID); got.Status != domain.InvoiceFinalized {
		t.Errorf("invoice %s, want finalized (the settle failed)", got.Status)
	}
}

// R3 (ADR-129): a customer holds USD credit, has an unpaid USD invoice in
// dunning from a canceled USD subscription, and a EUR invoice from the EUR
// subscription that replaced it. The EUR close's credit apply draws nothing;
// the dunning retry applies the USD credit to the USD invoice.
func TestDunningRetry_AppliesOnlyTheInvoiceCurrency(t *testing.T) {
	d := newDunningRetryRig(t, "Retry Credit Currency")
	custID := d.customerWithCard("ccy", "pm_ccy")
	if _, err := d.credits.Grant(d.ctx, d.tenantID, credit.GrantInput{
		CustomerID: custID, Currency: "USD", AmountCents: 5000, Description: "USD goodwill",
	}); err != nil {
		t.Fatalf("grant: %v", err)
	}
	now := time.Now().UTC()
	eur, err := d.invoices.Create(d.ctx, d.tenantID, domain.Invoice{
		CustomerID: custID, InvoiceNumber: "INV-CCY-EUR", Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentPending,
		Currency: "EUR", SubtotalCents: 3000, TotalAmountCents: 3000, AmountDueCents: 3000,
		BillingPeriodStart: now.Add(-30 * 24 * time.Hour), BillingPeriodEnd: now, IssuedAt: &now,
	})
	if err != nil {
		t.Fatalf("create EUR invoice: %v", err)
	}
	if applied, err := d.credits.ApplyToInvoiceAt(d.ctx, d.tenantID, custID, eur.ID, 3000, now, eur.InvoiceNumber); err != nil || applied != 0 {
		t.Errorf("EUR close credit apply = %d, %v; want 0", applied, err)
	}

	inv := d.dunnedInvoice(custID, "INV-CCY-USD")
	d.dueRun(inv.ID, custID, 1)
	d.tick()
	if usage := d.creditUsage(custID, inv.ID); len(usage) != 1 || usage[0].AmountCents != -5000 {
		t.Errorf("USD invoice credit usage %+v, want one entry of -5000", usage)
	}
	if got := d.invoice(inv.ID); got.Status != domain.InvoicePaid || got.CreditsAppliedCents != 5000 {
		t.Errorf("USD invoice %s with %d credits applied, want paid with 5000", got.Status, got.CreditsAppliedCents)
	}
	if got := d.invoice(eur.ID); got.CreditsAppliedCents != 0 || got.AmountDueCents != 3000 {
		t.Errorf("EUR invoice credits_applied %d, amount_due %d; want 0 and 3000", got.CreditsAppliedCents, got.AmountDueCents)
	}
	if n := len(d.charges.charged); n != 0 {
		t.Errorf("Stripe received %d charges, want none (credit covered the USD invoice)", n)
	}
}
