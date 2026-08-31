package invoice_test

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestMarkPaid_IdempotentPreservesAmountPaid is the regression for the audit
// MarkPaid finding (velox-ops): a DUPLICATE MarkPaid must not corrupt the
// recorded paid amount.
//
// The unknown-charge recovery path can settle the same charge twice under
// different Stripe event ids — the reconciler resolves a PaymentUnknown by
// querying Stripe and calling MarkPaid, and the real payment_intent.succeeded
// webhook then lands with a new event id (so event-id dedup does not catch it)
// and calls MarkPaid again. The buggy UPDATE set
// `amount_paid_cents = amount_due_cents`, and on the second call
// amount_due_cents was already 0, so it zeroed amount_paid_cents — corrupting
// the paid amount and blocking card refunds (refunds size against
// amount_paid_cents). The fix makes a re-mark on an already-paid invoice a
// true no-op on the money fields.
func TestMarkPaid_IdempotentPreservesAmountPaid(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)

	store := invoice.NewPostgresStore(db)
	tenantID := testutil.CreateTestTenant(t, db, "MarkPaid Idempotency")
	invID := seedDraftInvoice(t, db, tenantID)

	// Give the invoice a positive balance and a resolved tax status so it can
	// be finalized and then marked paid.
	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin seed tx: %v", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE invoices
		    SET subtotal_cents = 1000, total_amount_cents = 1000,
		        amount_due_cents = 1000, tax_status = 'ok'
		  WHERE id = $1`, invID); err != nil {
		t.Fatalf("seed amounts: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit seed tx: %v", err)
	}

	if _, err := store.UpdateStatus(ctx, tenantID, invID, domain.InvoiceFinalized); err != nil {
		t.Fatalf("finalize: %v", err)
	}

	now := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)

	first, err := store.MarkPaid(ctx, tenantID, invID, "pi_reconciler", now)
	if err != nil {
		t.Fatalf("first MarkPaid: %v", err)
	}
	if first.AmountPaidCents != 1000 || first.AmountDueCents != 0 {
		t.Fatalf("after first MarkPaid: amount_paid=%d amount_due=%d, want 1000/0",
			first.AmountPaidCents, first.AmountDueCents)
	}
	if first.Status != domain.InvoicePaid {
		t.Fatalf("after first MarkPaid: status=%q, want paid", first.Status)
	}

	// Duplicate MarkPaid — the late webhook with a different PI id resolving
	// the same charge the reconciler already settled. Must be a no-op on the
	// money fields.
	second, err := store.MarkPaid(ctx, tenantID, invID, "pi_late_webhook", now.Add(time.Minute))
	if err != nil {
		t.Fatalf("second (duplicate) MarkPaid: %v", err)
	}
	if second.AmountPaidCents != 1000 {
		t.Errorf("REGRESSION: duplicate MarkPaid zeroed amount_paid_cents: got %d, want 1000 — refunds would be blocked",
			second.AmountPaidCents)
	}
	if second.AmountDueCents != 0 {
		t.Errorf("after duplicate MarkPaid: amount_due_cents=%d, want 0", second.AmountDueCents)
	}
	if second.Status != domain.InvoicePaid {
		t.Errorf("after duplicate MarkPaid: status=%q, want paid", second.Status)
	}
}

// TestMarkPaidReportingTransition_FlagsTheRealTransition locks the store-level
// signal H7 relies on: the first MarkPaidReportingTransition on a finalized
// invoice reports transitioned=true; a second call on the now-paid invoice
// reports transitioned=false (the idempotent no-op branch). The SELECT … FOR
// UPDATE serializes concurrent callers, so for two at-least-once webhook
// deliveries of the same charge exactly one sees true — which is what lets
// SettleSucceeded fire the receipt email / payment.succeeded event once.
func TestMarkPaidReportingTransition_FlagsTheRealTransition(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)

	store := invoice.NewPostgresStore(db)
	tenantID := testutil.CreateTestTenant(t, db, "MarkPaid Transition Flag")
	invID := seedDraftInvoice(t, db, tenantID)

	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin seed tx: %v", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE invoices SET subtotal_cents = 1000, total_amount_cents = 1000,
		    amount_due_cents = 1000, tax_status = 'ok' WHERE id = $1`, invID); err != nil {
		t.Fatalf("seed amounts: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit seed tx: %v", err)
	}
	if _, err := store.UpdateStatus(ctx, tenantID, invID, domain.InvoiceFinalized); err != nil {
		t.Fatalf("finalize: %v", err)
	}

	now := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)

	_, transitioned, err := store.MarkPaidReportingTransition(ctx, tenantID, invID, "pi_1", now)
	if err != nil {
		t.Fatalf("first MarkPaidReportingTransition: %v", err)
	}
	if !transitioned {
		t.Error("first call on a finalized invoice must report transitioned=true")
	}

	_, transitioned2, err := store.MarkPaidReportingTransition(ctx, tenantID, invID, "pi_2", now.Add(time.Minute))
	if err != nil {
		t.Fatalf("second MarkPaidReportingTransition: %v", err)
	}
	if transitioned2 {
		t.Error("second call on an already-paid invoice must report transitioned=false (idempotent no-op)")
	}
}

// TestMarkPaymentFailedReportingTransition_FlagsTheRealNotification locks the
// store-level signal the SettleFailed concurrent-dedup relies on. Failure is
// non-terminal, so the dedup key is the PaymentIntent id (failure_notified_pi),
// not the status: the first failure for a PI reports firstForThisPI=true; a
// duplicate of the SAME PI reports false (so the customer isn't emailed twice
// and integrators don't get two payment.failed events); a fresh PI from a later
// retry reports true again; and an out-of-order failure for an already-paid
// invoice leaves it paid and reports false.
func TestMarkPaymentFailedReportingTransition_FlagsTheRealNotification(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)

	store := invoice.NewPostgresStore(db)
	tenantID := testutil.CreateTestTenant(t, db, "MarkFailed Transition Flag")
	invID := seedDraftInvoice(t, db, tenantID)

	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin seed tx: %v", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE invoices SET subtotal_cents = 1000, total_amount_cents = 1000,
		    amount_due_cents = 1000, tax_status = 'ok' WHERE id = $1`, invID); err != nil {
		t.Fatalf("seed amounts: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit seed tx: %v", err)
	}
	if _, err := store.UpdateStatus(ctx, tenantID, invID, domain.InvoiceFinalized); err != nil {
		t.Fatalf("finalize: %v", err)
	}

	// First failure for pi_a → first notification for this PI.
	inv, first, err := store.MarkPaymentFailedReportingTransition(ctx, tenantID, invID, "pi_a", "card declined", nil)
	if err != nil {
		t.Fatalf("first failure: %v", err)
	}
	if !first {
		t.Error("first failure for pi_a must report firstForThisPI=true")
	}
	if inv.PaymentStatus != domain.PaymentFailed || inv.LastPaymentError != "card declined" {
		t.Errorf("after first failure: payment_status=%q error=%q, want failed/'card declined'", inv.PaymentStatus, inv.LastPaymentError)
	}

	// Duplicate delivery of the SAME PI → not a fresh notification.
	_, dup, err := store.MarkPaymentFailedReportingTransition(ctx, tenantID, invID, "pi_a", "card declined", nil)
	if err != nil {
		t.Fatalf("duplicate failure: %v", err)
	}
	if dup {
		t.Error("duplicate failure for pi_a must report firstForThisPI=false (no double-notify)")
	}

	// A later retry fails on a fresh PI → a genuinely new event.
	_, retry, err := store.MarkPaymentFailedReportingTransition(ctx, tenantID, invID, "pi_b", "card declined again", nil)
	if err != nil {
		t.Fatalf("retry failure: %v", err)
	}
	if !retry {
		t.Error("a new retry PI (pi_b) must report firstForThisPI=true (a distinct failure event)")
	}

	// Settle the invoice, then deliver a stale failure: the out-of-order guard
	// must leave it paid and report no fresh notification.
	now := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := store.MarkPaid(ctx, tenantID, invID, "pi_b", now); err != nil {
		t.Fatalf("mark paid: %v", err)
	}
	stale, staleFirst, err := store.MarkPaymentFailedReportingTransition(ctx, tenantID, invID, "pi_stale", "late decline", nil)
	if err != nil {
		t.Fatalf("stale failure: %v", err)
	}
	if staleFirst {
		t.Error("out-of-order failure on a paid invoice must report firstForThisPI=false")
	}
	if stale.PaymentStatus != domain.PaymentSucceeded || stale.Status != domain.InvoicePaid {
		t.Errorf("out-of-order failure corrupted a paid invoice: payment_status=%q status=%q", stale.PaymentStatus, stale.Status)
	}
}

// TestMarkPaymentFailed_EnqueuesPaymentFailedInTx locks the failed-path mirror of
// the payment.succeeded in-tx fix: payment.failed is enqueued INSIDE the
// failed-stamp tx (crash-safe with the transition; SettleFailed no longer fires it
// post-commit), gated on firstForThisPI — a same-PI redelivery does not re-enqueue,
// a NEW retry PI is a distinct failure and fires again, a stale failure on a
// settled invoice never emits, and a failed ENQUEUE rolls the whole failed-stamp
// back (atomicity: no event ⇒ no stamp).
func TestMarkPaymentFailed_EnqueuesPaymentFailedInTx(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	now := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)

	t.Run("in-tx enqueue, gated on firstForThisPI", func(t *testing.T) {
		tenantID := testutil.CreateTestTenant(t, db, "MarkFailed InTx Event")
		store := invoice.NewPostgresStore(db)
		rec := &recordingOutbox{}
		store.SetOutboxEnqueuer(rec)
		invID := seedFinalizedInvoice(t, db, store, ctx, tenantID)

		// First failure for pi_a → one payment.failed enqueued in the same tx.
		if _, first, err := store.MarkPaymentFailedReportingTransition(ctx, tenantID, invID, "pi_a", "card declined", nil); err != nil || !first {
			t.Fatalf("first failure: first=%v err=%v", first, err)
		}
		if want := []string{domain.EventPaymentFailed}; !reflect.DeepEqual(rec.events, want) {
			t.Fatalf("after first failure: events=%v, want %v", rec.events, want)
		}

		// Same-PI redelivery → no second enqueue.
		if _, dup, err := store.MarkPaymentFailedReportingTransition(ctx, tenantID, invID, "pi_a", "card declined", nil); err != nil || dup {
			t.Fatalf("duplicate failure: dup=%v err=%v", dup, err)
		}
		if len(rec.events) != 1 {
			t.Fatalf("same-PI redelivery re-enqueued: events=%v, want exactly 1 (double-notify)", rec.events)
		}

		// A retry's failure on a fresh PI is a genuinely new event → fires again.
		if _, retry, err := store.MarkPaymentFailedReportingTransition(ctx, tenantID, invID, "pi_b", "declined again", nil); err != nil || !retry {
			t.Fatalf("retry failure: retry=%v err=%v", retry, err)
		}
		if len(rec.events) != 2 {
			t.Fatalf("new retry PI must enqueue a fresh payment.failed: events=%v, want 2", rec.events)
		}

		// Settle, then a stale out-of-order failure → guard returns before the
		// enqueue; a paid invoice never emits payment.failed.
		if _, err := store.MarkPaid(ctx, tenantID, invID, "pi_b", now); err != nil {
			t.Fatalf("mark paid: %v", err)
		}
		preStale := len(rec.events)
		if _, staleFirst, err := store.MarkPaymentFailedReportingTransition(ctx, tenantID, invID, "pi_stale", "late decline", nil); err != nil || staleFirst {
			t.Fatalf("stale failure: first=%v err=%v", staleFirst, err)
		}
		// preStale includes the invoice.paid enqueue from MarkPaid.
		if len(rec.events) != preStale {
			t.Errorf("stale failure on a paid invoice enqueued payment.failed: events=%v", rec.events)
		}
	})

	t.Run("failed enqueue rolls the failed-stamp back", func(t *testing.T) {
		tenantID := testutil.CreateTestTenant(t, db, "MarkFailed InTx Rollback")
		store := invoice.NewPostgresStore(db)
		store.SetOutboxEnqueuer(&failingOutbox{failOn: domain.EventPaymentFailed})
		invID := seedFinalizedInvoice(t, db, store, ctx, tenantID)

		if _, _, err := store.MarkPaymentFailedReportingTransition(ctx, tenantID, invID, "pi_x", "declined", nil); err == nil {
			t.Fatal("a failed in-tx enqueue must surface an error")
		}
		after, err := store.Get(ctx, tenantID, invID)
		if err != nil {
			t.Fatalf("get after rollback: %v", err)
		}
		if after.PaymentStatus == domain.PaymentFailed {
			t.Fatal("the failed-stamp must roll back with the failed enqueue (atomic: no event ⇒ no stamp)")
		}
		if after.LastPaymentError != "" {
			t.Errorf("last_payment_error must be unset after rollback, got %q", after.LastPaymentError)
		}
	})
}

// recordingOutbox captures the events MarkPaid enqueues, ignoring the tx.
type recordingOutbox struct{ events []string }

func (r *recordingOutbox) Enqueue(_ context.Context, _ *sql.Tx, _, eventType string, _ map[string]any) (string, error) {
	r.events = append(r.events, eventType)
	return "vlx_whob_test", nil
}

// failingOutbox records enqueues but errors on one target event type — used to
// prove a failed IN-TX enqueue rolls the paid-flip back (atomicity).
type failingOutbox struct {
	events []string
	failOn string
}

func (f *failingOutbox) Enqueue(_ context.Context, _ *sql.Tx, _, eventType string, _ map[string]any) (string, error) {
	if eventType == f.failOn {
		return "", fmt.Errorf("simulated outbox enqueue failure for %s", eventType)
	}
	f.events = append(f.events, eventType)
	return "vlx_whob_test", nil
}

// seedFinalizedInvoice seeds a finalized, tax-ok invoice with non-zero amounts.
func seedFinalizedInvoice(t *testing.T, db *postgres.DB, store *invoice.PostgresStore, ctx context.Context, tenantID string) string {
	t.Helper()
	invID := seedDraftInvoice(t, db, tenantID)
	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin seed tx: %v", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE invoices SET subtotal_cents=1000, total_amount_cents=1000, amount_due_cents=1000, tax_status='ok' WHERE id=$1`, invID); err != nil {
		t.Fatalf("seed amounts: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit seed tx: %v", err)
	}
	if _, err := store.UpdateStatus(ctx, tenantID, invID, domain.InvoiceFinalized); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	return invID
}

// TestMarkPaidCardSettlement_EnqueuesPaymentSucceededInTx is the regression for
// the 2026-06-22 hybrid fix: on the CARD path, payment.succeeded is enqueued
// IN-TX alongside invoice.paid (so it's crash-safe with the paid-flip), exactly
// once (gated on the transition); the non-card path emits ONLY invoice.paid.
func TestMarkPaidCardSettlement_EnqueuesPaymentSucceededInTx(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	now := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)

	t.Run("card path enqueues invoice.paid + payment.succeeded, once", func(t *testing.T) {
		// Own tenant per sub-test: seedDraftInvoice creates a fixed-external_id
		// customer, so two seeds in one tenant would collide.
		tenantID := testutil.CreateTestTenant(t, db, "MarkPaid Card Settlement (card)")
		store := invoice.NewPostgresStore(db)
		rec := &recordingOutbox{}
		store.SetOutboxEnqueuer(rec)
		invID := seedFinalizedInvoice(t, db, store, ctx, tenantID)

		_, transitioned, err := store.MarkPaidCardSettlementTransition(ctx, tenantID, invID, "pi_card", now, nil)
		if err != nil {
			t.Fatalf("MarkPaidCardSettlementTransition: %v", err)
		}
		if !transitioned {
			t.Fatal("first call must report transitioned=true")
		}
		want := []string{domain.EventInvoicePaid, domain.EventPaymentSucceeded}
		if !reflect.DeepEqual(rec.events, want) {
			t.Fatalf("card path events: got %v, want %v (both enqueued in the same tx)", rec.events, want)
		}

		// Duplicate (already paid) → no new enqueues.
		_, transitioned2, err := store.MarkPaidCardSettlementTransition(ctx, tenantID, invID, "pi_card2", now.Add(time.Minute), nil)
		if err != nil {
			t.Fatalf("duplicate MarkPaidCardSettlementTransition: %v", err)
		}
		if transitioned2 {
			t.Error("duplicate must report transitioned=false")
		}
		if !reflect.DeepEqual(rec.events, want) {
			t.Errorf("duplicate re-enqueued events: got %v, want unchanged %v", rec.events, want)
		}
	})

	t.Run("non-card path enqueues ONLY invoice.paid", func(t *testing.T) {
		tenantID := testutil.CreateTestTenant(t, db, "MarkPaid Card Settlement (non-card)")
		store := invoice.NewPostgresStore(db)
		rec := &recordingOutbox{}
		store.SetOutboxEnqueuer(rec)
		invID := seedFinalizedInvoice(t, db, store, ctx, tenantID)

		if _, _, err := store.MarkPaidReportingTransition(ctx, tenantID, invID, "pi_noncard", now); err != nil {
			t.Fatalf("MarkPaidReportingTransition: %v", err)
		}
		want := []string{domain.EventInvoicePaid}
		if !reflect.DeepEqual(rec.events, want) {
			t.Fatalf("non-card path events: got %v, want %v (payment.succeeded must NOT fire here)", rec.events, want)
		}
	})
}

// TestMarkPaidCardSettlement_RollsBackPaidFlipIfEventEnqueueFails proves the
// atomicity the fix buys: because payment.succeeded is enqueued INSIDE the
// paid-flip tx, a failure of that enqueue rolls the whole tx back — the invoice
// stays finalized, never stranded as paid-without-its-event.
func TestMarkPaidCardSettlement_RollsBackPaidFlipIfEventEnqueueFails(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "MarkPaid Card Rollback")
	store := invoice.NewPostgresStore(db)
	store.SetOutboxEnqueuer(&failingOutbox{failOn: domain.EventPaymentSucceeded})
	invID := seedFinalizedInvoice(t, db, store, ctx, tenantID)
	now := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)

	if _, _, err := store.MarkPaidCardSettlementTransition(ctx, tenantID, invID, "pi_card", now, nil); err == nil {
		t.Fatal("expected an error when the in-tx payment.succeeded enqueue fails")
	}

	got, err := store.Get(ctx, tenantID, invID)
	if err != nil {
		t.Fatalf("get after failed settle: %v", err)
	}
	if got.Status != domain.InvoiceFinalized {
		t.Errorf("REGRESSION: status=%q after a failed in-tx payment.succeeded enqueue, want still 'finalized' (paid-flip must roll back atomically)", got.Status)
	}
	if got.PaymentStatus == domain.PaymentSucceeded {
		t.Error("REGRESSION: payment_status=succeeded after rollback — the paid-flip did not roll back with the failed enqueue")
	}
}

// TestMarkPaid_FiresInvoicePaidOnceOnTransition asserts invoice.paid is emitted
// exactly once — on the real finalized→paid transition — and NOT on a duplicate
// MarkPaid of an already-paid invoice (the already-paid branch returns before
// the emit). This is what lets the single MarkPaid emit point cover every
// settlement path (card, credits, offline, dunning, reconciler fallback)
// without double-firing.
func TestMarkPaid_FiresInvoicePaidOnceOnTransition(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)

	store := invoice.NewPostgresStore(db)
	rec := &recordingOutbox{}
	store.SetOutboxEnqueuer(rec)
	tenantID := testutil.CreateTestTenant(t, db, "MarkPaid InvoicePaid Event")
	invID := seedDraftInvoice(t, db, tenantID)

	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin seed tx: %v", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE invoices
		    SET subtotal_cents = 1000, total_amount_cents = 1000,
		        amount_due_cents = 1000, tax_status = 'ok'
		  WHERE id = $1`, invID); err != nil {
		t.Fatalf("seed amounts: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit seed tx: %v", err)
	}
	if _, err := store.UpdateStatus(ctx, tenantID, invID, domain.InvoiceFinalized); err != nil {
		t.Fatalf("finalize: %v", err)
	}

	now := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)

	// Real transition → exactly one invoice.paid.
	if _, err := store.MarkPaid(ctx, tenantID, invID, "pi_1", now); err != nil {
		t.Fatalf("first MarkPaid: %v", err)
	}
	if len(rec.events) != 1 || rec.events[0] != domain.EventInvoicePaid {
		t.Fatalf("after transition: got events %v, want exactly [%s]", rec.events, domain.EventInvoicePaid)
	}

	// Duplicate MarkPaid (already paid) → no new event.
	if _, err := store.MarkPaid(ctx, tenantID, invID, "pi_2", now.Add(time.Minute)); err != nil {
		t.Fatalf("duplicate MarkPaid: %v", err)
	}
	if len(rec.events) != 1 {
		t.Errorf("REGRESSION: duplicate MarkPaid re-emitted invoice.paid: got %v, want exactly one", rec.events)
	}
}

// TestListPendingTaxCommit_FiltersToOrphans locks the commit-reconciler SQL
// filter against real Postgres: it returns ONLY finalized stripe_tax invoices
// with a calculation id but no transaction id (the orphan left when CommitTax
// succeeded at Stripe but the local persist failed), and excludes already-
// committed and non-stripe invoices.
func TestListPendingTaxCommit_FiltersToOrphans(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	store := invoice.NewPostgresStore(db)
	tenantID := testutil.CreateTestTenant(t, db, "Tax Commit Reconciler")
	invID := seedDraftInvoice(t, db, tenantID)

	exec := func(q string, args ...any) {
		t.Helper()
		tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
		if err != nil {
			t.Fatalf("begin tx: %v", err)
		}
		defer postgres.Rollback(tx)
		if _, err := tx.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("exec: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}

	// The orphan: finalized stripe_tax, calc id set, transaction id NULL.
	exec(`UPDATE invoices SET status='finalized', tax_status='ok', tax_provider='stripe_tax',
	         tax_calculation_id='taxcalc_orphan', tax_transaction_id=NULL, updated_at=now() WHERE id=$1`, invID)
	got, err := store.ListPendingTaxCommit(ctx, 50, false)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0].ID != invID {
		t.Fatalf("orphan not returned: got %d rows %+v", len(got), got)
	}

	// Once the transaction id is persisted → recovered, excluded.
	exec(`UPDATE invoices SET tax_transaction_id='tx_committed' WHERE id=$1`, invID)
	got, err = store.ListPendingTaxCommit(ctx, 50, false)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("committed invoice still returned: %d rows", len(got))
	}

	// A manual-provider invoice (no calc to commit) is never an orphan.
	exec(`UPDATE invoices SET tax_provider='manual', tax_calculation_id='', tax_transaction_id=NULL WHERE id=$1`, invID)
	got, err = store.ListPendingTaxCommit(ctx, 50, false)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("manual invoice returned: %d rows", len(got))
	}
}
