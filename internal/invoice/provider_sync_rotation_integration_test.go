package invoice_test

import (
	"context"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestUnknownSweepRotatesInsteadOfJamming is the liveness proof for migration
// 0167, and it is written as a REPRODUCTION of the bug rather than a spot check.
//
// The sweep is LIMITed and was ordered by updated_at ASC. The reconciler writes
// nothing when it finds a non-terminal PaymentIntent, so such a row's
// updated_at froze and it sorted first forever: once batch-size of them
// accumulated, the sweep returned only stuck rows and a NEW ambiguous charge —
// the kind Stripe resolves in a single call — was never reached. Money that
// could have been collected simply stopped being asked about.
//
// The fix is ordering, not exclusion, and the difference matters: these rows
// MUST keep being polled, because the sweep is what notices when the customer
// finally authenticates. Every exclusion added during this arc risked hiding a
// row from the very code that would end its wait.
func TestUnknownSweepRotatesInsteadOfJamming(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Sweep Rotation")

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_rot", DisplayName: "Rotation",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}

	store := invoice.NewPostgresStore(db)
	now := time.Now().UTC()
	issued := now

	mkUnknown := func(num string, piID string, ageHours int) string {
		inv, err := store.Create(ctx, tenantID, domain.Invoice{
			CustomerID: cust.ID, InvoiceNumber: num, Status: domain.InvoiceFinalized,
			PaymentStatus: domain.PaymentUnknown, Currency: "USD",
			SubtotalCents: 1000, TotalAmountCents: 1000, AmountDueCents: 1000,
			BillingPeriodStart: now.Add(-30 * 24 * time.Hour), BillingPeriodEnd: now, IssuedAt: &issued,
		})
		if err != nil {
			t.Fatalf("create %s: %v", num, err)
		}
		// The PI id is what makes a row reconcilable at all (parked rows are
		// excluded); write it the real way.
		if _, err := store.UpdatePayment(ctx, tenantID, inv.ID, domain.PaymentUnknown, piID, "", nil); err != nil {
			t.Fatalf("attach PI to %s: %v", num, err)
		}
		tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		// Rollback deferred BEFORE any t.Fatalf below: a Fatalf exits this
		// goroutine, so without it a failed statement leaves the transaction
		// open and the pool blocks the whole package until the test binary's
		// timeout — which is how a one-line encoding bug in this helper cost a
		// ten-minute hang rather than a two-second failure.
		defer postgres.Rollback(tx)
		// make_interval() takes the count as an integer parameter; the
		// ($1 || ' hours')::interval form needs a TEXT arg and pgx refuses to
		// encode an int into it.
		if _, err := tx.ExecContext(ctx,
			`UPDATE invoices SET updated_at = now() - make_interval(hours => $1) WHERE id = $2`, ageHours, inv.ID); err != nil {
			t.Fatalf("age %s: %v", num, err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
		return inv.ID
	}

	// The stuck one is the OLDEST by updated_at, which is exactly how it used to
	// monopolise the head of the queue.
	stuck := mkUnknown("INV-STUCK", "pi_stuck", 72)
	fresh := mkUnknown("INV-FRESH", "pi_fresh", 1)

	first := func(t *testing.T) string {
		t.Helper()
		got, err := store.ListUnknownPayments(ctx, time.Now().UTC(), 1)
		if err != nil {
			t.Fatalf("ListUnknownPayments: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("expected 1 invoice at LIMIT 1, got %d", len(got))
		}
		return got[0].ID
	}

	// LIMIT 1 makes the starvation visible with two rows instead of fifty: the
	// queue can serve exactly one invoice per sweep, so whoever holds the head
	// holds it against everyone else.
	if got := first(t); got != stuck {
		t.Fatalf("first sweep should return the least-recently-observed row (never observed, oldest): got %s, want %s", got, stuck)
	}

	// The reconciler polls it, finds a non-terminal PaymentIntent, and records
	// that observation. Under the OLD behaviour this wrote nothing at all.
	if err := store.RecordProviderSync(ctx, tenantID, stuck, domain.ProviderPIRequiresAction); err != nil {
		t.Fatalf("record provider sync: %v", err)
	}

	if got := first(t); got != fresh {
		t.Fatalf("after polling the stuck row the queue must move on to the never-observed one; got %s, want %s. "+
			"This is the starvation bug: a row the reconciler cannot resolve held the head of a LIMITed queue forever, "+
			"so newly-ambiguous charges were never reached", got, fresh)
	}

	// And it round-robins rather than permanently demoting: once the fresh row
	// has also been observed, the stuck one (observed longer ago) comes back up.
	if err := store.RecordProviderSync(ctx, tenantID, fresh, domain.ProviderPIProcessing); err != nil {
		t.Fatalf("record provider sync (fresh): %v", err)
	}
	if got := first(t); got != stuck {
		t.Fatalf("queue must round-robin by least-recently-observed, not demote a row forever: got %s, want %s", got, stuck)
	}
}

// TestRecordProviderSyncTouchesOnlyTheObservation guards the two columns this
// write must NOT move, each for a concrete reason:
//
//   - charge_attempt_seq seeds the Stripe idempotency key (ADR-105). Bumping it
//     from an OBSERVATION would rotate the key with no attempt having happened,
//     so the next retry could open a second PaymentIntent beside a live one —
//     the exact double charge ADR-107 exists to make unreachable.
//   - updated_at feeds the attention banner's "since" and the sweep's own
//     staleness gate. Writing it every five minutes would reset the operator's
//     age signal forever and make a permanently stuck invoice look brand new.
func TestRecordProviderSyncTouchesOnlyTheObservation(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Sync Narrowness")

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_narrow", DisplayName: "Narrow",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}

	store := invoice.NewPostgresStore(db)
	now := time.Now().UTC()
	issued := now
	inv, err := store.Create(ctx, tenantID, domain.Invoice{
		CustomerID: cust.ID, InvoiceNumber: "INV-NARROW", Status: domain.InvoiceFinalized,
		PaymentStatus: domain.PaymentUnknown, Currency: "USD",
		SubtotalCents: 1000, TotalAmountCents: 1000, AmountDueCents: 1000,
		BillingPeriodStart: now.Add(-time.Hour), BillingPeriodEnd: now, IssuedAt: &issued,
	})
	if err != nil {
		t.Fatalf("create invoice: %v", err)
	}
	if _, err := store.UpdatePayment(ctx, tenantID, inv.ID, domain.PaymentUnknown, "pi_narrow", "", nil); err != nil {
		t.Fatalf("attach PI: %v", err)
	}

	before, err := store.Get(ctx, tenantID, inv.ID)
	if err != nil {
		t.Fatalf("get before: %v", err)
	}

	if err := store.RecordProviderSync(ctx, tenantID, inv.ID, domain.ProviderPIRequiresAction); err != nil {
		t.Fatalf("record provider sync: %v", err)
	}

	after, err := store.Get(ctx, tenantID, inv.ID)
	if err != nil {
		t.Fatalf("get after: %v", err)
	}

	if after.ChargeAttemptSeq != before.ChargeAttemptSeq {
		t.Errorf("charge_attempt_seq moved %d → %d on a mere observation — that rotates the Stripe idempotency key with no attempt behind it, so the next retry can open a SECOND PaymentIntent beside a live one",
			before.ChargeAttemptSeq, after.ChargeAttemptSeq)
	}
	if !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Errorf("updated_at moved %s → %s on a mere observation — the banner's age and the sweep's staleness gate both read it, so a permanently stuck invoice would look forever fresh",
			before.UpdatedAt, after.UpdatedAt)
	}
	if after.PaymentStatus != before.PaymentStatus {
		t.Errorf("payment_status changed %q → %q: an observation must never overwrite the authoritative money state", before.PaymentStatus, after.PaymentStatus)
	}
	if after.StripePaymentIntentID != before.StripePaymentIntentID {
		t.Errorf("stripe_payment_intent_id changed %q → %q", before.StripePaymentIntentID, after.StripePaymentIntentID)
	}

	// And it DID record what it was for.
	if after.ProviderPaymentStatus != domain.ProviderPIRequiresAction {
		t.Errorf("provider_payment_status = %q, want %q", after.ProviderPaymentStatus, domain.ProviderPIRequiresAction)
	}
	if after.ProviderSyncedAt == nil {
		t.Error("provider_synced_at not set — without it the sweep cannot order by least-recently-observed and the starvation returns")
	}
}
