package invoice_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// stampOutcome is the fixture form of the charge path's outcome write: it
// stamps against the row's current charge_attempt_seq and fails the test if
// the stamp was refused, so a seeding step can never silently no-op.
func stampOutcome(t *testing.T, store *invoice.PostgresStore, ctx context.Context, tenantID, id string, ps domain.InvoicePaymentStatus, pi, msg string) {
	t.Helper()
	cur, err := store.Get(ctx, tenantID, id)
	if err != nil {
		t.Fatalf("get before stamp: %v", err)
	}
	if ps == domain.PaymentFailed && pi != "" {
		// A decline naming its PaymentIntent is the charge call's report, not
		// a stamp — the path production takes.
		seq := cur.ChargeAttemptSeq
		if _, res, err := store.MarkPaymentFailedReportingTransition(ctx, tenantID, id,
			domain.PaymentFailureReport{PaymentIntentID: pi, Message: msg, ChargedAtSeq: &seq}, nil); err != nil || !res.Recorded {
			t.Fatalf("report decline %q: recorded=%v err=%v", pi, res.Recorded, err)
		}
		return
	}
	if _, applied, err := store.StampChargeOutcome(ctx, tenantID, id, cur.ChargeAttemptSeq, ps, pi, msg); err != nil || !applied {
		t.Fatalf("stamp %s/%q: applied=%v err=%v", ps, pi, applied, err)
	}
}

// TestStampChargeOutcome_OutcomeRecordedDuringTheCallWins (P13): the charge
// path stamps its outcome after a Stripe round-trip, and another outcome can
// be recorded during it. Each case reads the attempt's seq (as the charge
// does when it builds its idempotency key), runs the competing writer through
// the REAL store method, then stamps, and asserts the row the DB kept.
//
// Refused cases must leave the winner's row exactly as it was, seq included.
// Applied cases are the negative controls: writes the guard must not block (a
// dunning retry starting from the previous attempt's failure, a void or
// write-off that landed mid-charge, a park).
func TestStampChargeOutcome_OutcomeRecordedDuringTheCallWins(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Charge Outcome Guard")
	store := invoice.NewPostgresStore(db)
	paidAt := time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)

	type step func(t *testing.T, id string)
	none := func(*testing.T, string) {}
	markPaid := func(pi string) step {
		return func(t *testing.T, id string) {
			if _, err := store.MarkPaid(ctx, tenantID, id, pi, paidAt); err != nil {
				t.Fatalf("MarkPaid: %v", err)
			}
		}
	}
	markFailed := func(pi string) step {
		return func(t *testing.T, id string) {
			if _, _, err := store.MarkPaymentFailedReportingTransition(ctx, tenantID, id, failureReport(pi, "card_declined"), nil); err != nil {
				t.Fatalf("MarkPaymentFailedReportingTransition: %v", err)
			}
		}
	}
	syncDecline := func(pi string) step {
		return func(t *testing.T, id string) {
			stampOutcome(t, store, ctx, tenantID, id, domain.PaymentFailed, pi, "card_declined")
		}
	}
	setStatus := func(s domain.InvoiceStatus) step {
		return func(t *testing.T, id string) {
			if _, err := store.UpdateStatus(ctx, tenantID, id, s); err != nil {
				t.Fatalf("UpdateStatus(%s): %v", s, err)
			}
		}
	}

	cases := []struct {
		name string
		// before runs before the attempt reads its seq; during runs after,
		// i.e. while the Stripe call is in flight.
		before, during step
		status         domain.InvoicePaymentStatus
		pi             string
		applied        bool
	}{
		// Refused — a paid invoice is never un-settled.
		{"own webhook paid, then unknown with the PI", none, markPaid("pi_X"), domain.PaymentUnknown, "pi_X", false},
		{"own webhook paid, then unknown with no PI", none, markPaid("pi_X"), domain.PaymentUnknown, "", false},
		{"paid offline, then processing", none, markPaid("out_of_band:2026-10-10T09:00:00Z"), domain.PaymentProcessing, "pi_X", false},
		{"paid by credits (no PI), then failed with no PI", none, markPaid(""), domain.PaymentFailed, "", false},
		// Refused — a decline its own webhook already recorded stays recorded.
		{"own decline webhook, then unknown with the PI", none, markFailed("pi_X"), domain.PaymentUnknown, "pi_X", false},
		{"own decline webhook, then processing with the PI", none, markFailed("pi_X"), domain.PaymentProcessing, "pi_X", false},
		{"own decline webhook, then unknown with no PI (would park a known decline)", none, markFailed("pi_X"), domain.PaymentUnknown, "", false},

		// Applied — a new attempt starting from the previous attempt's failure.
		{"previous attempt failed, then unknown for a new PI", markFailed("pi_W"), none, domain.PaymentUnknown, "pi_X", true},
		{"previous attempt failed, then unknown with no PI (park)", markFailed("pi_W"), none, domain.PaymentUnknown, "", true},
		// Applied — the previous attempt's decline webhook lands mid-call,
		// re-recording a failure already on the row. It records nothing new,
		// so it must not refuse this attempt's outcome.
		{"previous attempt's decline webhook lands mid-call (after its sync decline)", syncDecline("pi_W"), markFailed("pi_W"), domain.PaymentUnknown, "pi_X", true},
		{"previous attempt's decline webhook redelivered mid-call", markFailed("pi_W"), markFailed("pi_W"), domain.PaymentProcessing, "pi_X", true},
		// Applied — voided / written off mid-charge: the stamp is the
		// reconciler's only handle on a PaymentIntent that may have captured.
		{"voided mid-charge, then unknown", none, setStatus(domain.InvoiceVoided), domain.PaymentUnknown, "pi_X", true},
		{"written off mid-charge, then processing", none, setStatus(domain.InvoiceUncollectible), domain.PaymentProcessing, "pi_X", true},
		{"no interleaving, processing", none, none, domain.PaymentProcessing, "pi_X", true},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inv := seedClaimableInvoice(t, db, ctx, tenantID, "INV-P13-"+string(rune('A'+i)))
			tc.before(t, inv.ID)
			attempt, err := store.Get(ctx, tenantID, inv.ID)
			if err != nil {
				t.Fatalf("attempt read: %v", err)
			}
			tc.during(t, inv.ID)
			before, err := store.Get(ctx, tenantID, inv.ID)
			if err != nil {
				t.Fatalf("get before stamp: %v", err)
			}

			got, applied, err := store.StampChargeOutcome(ctx, tenantID, inv.ID, attempt.ChargeAttemptSeq, tc.status, tc.pi, "charge outcome")
			if err != nil {
				t.Fatalf("StampChargeOutcome: %v — a refused stamp is not an error", err)
			}
			if applied != tc.applied {
				t.Fatalf("applied = %v, want %v", applied, tc.applied)
			}
			after, err := store.Get(ctx, tenantID, inv.ID)
			if err != nil {
				t.Fatalf("get after: %v", err)
			}
			if got.PaymentStatus != after.PaymentStatus || got.StripePaymentIntentID != after.StripePaymentIntentID || got.ChargeAttemptSeq != after.ChargeAttemptSeq {
				t.Errorf("returned row %s/%q/seq %d differs from the row on disk %s/%q/seq %d",
					got.PaymentStatus, got.StripePaymentIntentID, got.ChargeAttemptSeq,
					after.PaymentStatus, after.StripePaymentIntentID, after.ChargeAttemptSeq)
			}

			if tc.applied {
				if after.PaymentStatus != tc.status || after.StripePaymentIntentID != tc.pi {
					t.Fatalf("stamp refused: row is %s/%q, want %s/%q", after.PaymentStatus, after.StripePaymentIntentID, tc.status, tc.pi)
				}
				if after.ChargeAttemptSeq != before.ChargeAttemptSeq+1 {
					t.Errorf("charge_attempt_seq %d → %d, want +1 for a recorded outcome", before.ChargeAttemptSeq, after.ChargeAttemptSeq)
				}
				if after.Status != before.Status {
					t.Errorf("invoice status changed %s → %s", before.Status, after.Status)
				}
				return
			}
			if after.Status != before.Status || after.PaymentStatus != before.PaymentStatus ||
				after.StripePaymentIntentID != before.StripePaymentIntentID ||
				after.ChargeAttemptSeq != before.ChargeAttemptSeq ||
				(before.PaidAt == nil) != (after.PaidAt == nil) {
				t.Fatalf("late stamp changed the winner's row: %s/%s/%q/seq %d/paid_at %v → %s/%s/%q/seq %d/paid_at %v",
					before.Status, before.PaymentStatus, before.StripePaymentIntentID, before.ChargeAttemptSeq, before.PaidAt,
					after.Status, after.PaymentStatus, after.StripePaymentIntentID, after.ChargeAttemptSeq, after.PaidAt)
			}
		})
	}

	t.Run("a failure naming a PaymentIntent is a report, not a stamp", func(t *testing.T) {
		inv := seedClaimableInvoice(t, db, ctx, tenantID, "INV-P13-FAILPI")
		if _, _, err := store.StampChargeOutcome(ctx, tenantID, inv.ID, inv.ChargeAttemptSeq, domain.PaymentFailed, "pi_X", "declined"); err == nil {
			t.Fatal("stamped a named decline; want a refusal — it would skip the once-per-PaymentIntent notice and the attempt check")
		}
	})

	t.Run("succeeded is not a charge outcome — it goes through MarkPaid", func(t *testing.T) {
		inv := seedClaimableInvoice(t, db, ctx, tenantID, "INV-P13-SUCC")
		if _, _, err := store.StampChargeOutcome(ctx, tenantID, inv.ID, inv.ChargeAttemptSeq, domain.PaymentSucceeded, "pi_X", ""); err == nil {
			t.Fatal("stamped succeeded; want a refusal — it would mark payment succeeded without status, amounts or paid events")
		}
	})

	t.Run("missing invoice is ErrNotFound", func(t *testing.T) {
		_, _, err := store.StampChargeOutcome(ctx, tenantID, "vlx_inv_does_not_exist", 0, domain.PaymentUnknown, "pi_X", "")
		if !errors.Is(err, errs.ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	})
}

// TestStampChargeOutcome_WaitsForAnInFlightSettle proves the CAS is evaluated
// against the settle's COMMITTED row, not a snapshot taken before it: the
// stamp's UPDATE blocks on the settle's row lock, and once the settle commits
// (bumping the seq), the UPDATE re-checks its WHERE and refuses. A guard moved
// into a Go read-then-write would read the pre-settle row and overwrite.
func TestStampChargeOutcome_WaitsForAnInFlightSettle(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Charge Outcome Lock Wait")
	store := invoice.NewPostgresStore(db)
	inv := seedClaimableInvoice(t, db, ctx, tenantID, "INV-P13-LOCK")

	settle, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin settle: %v", err)
	}
	defer postgres.Rollback(settle)
	if _, err := settle.ExecContext(ctx, `SELECT 1 FROM invoices WHERE id = $1 FOR UPDATE`, inv.ID); err != nil {
		t.Fatalf("lock row: %v", err)
	}

	type result struct {
		inv     domain.Invoice
		applied bool
		err     error
	}
	done := make(chan result, 1)
	go func() {
		got, applied, err := store.StampChargeOutcome(ctx, tenantID, inv.ID, inv.ChargeAttemptSeq, domain.PaymentUnknown, "", "timeout")
		done <- result{got, applied, err}
	}()

	select {
	case r := <-done:
		t.Fatalf("StampChargeOutcome returned while the settle held the row lock (applied=%v err=%v) — it did not wait", r.applied, r.err)
	case <-time.After(300 * time.Millisecond):
	}

	if _, err := settle.ExecContext(ctx, `
		UPDATE invoices SET status = 'paid', payment_status = 'succeeded',
			stripe_payment_intent_id = 'pi_win', paid_at = now(),
			charge_attempt_seq = charge_attempt_seq + 1
		WHERE id = $1`, inv.ID); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if err := settle.Commit(); err != nil {
		t.Fatalf("commit settle: %v", err)
	}

	r := <-done
	if r.err != nil {
		t.Fatalf("StampChargeOutcome: %v", r.err)
	}
	if r.applied || r.inv.Status != domain.InvoicePaid || r.inv.PaymentStatus != domain.PaymentSucceeded ||
		r.inv.StripePaymentIntentID != "pi_win" || r.inv.PaidAt == nil {
		t.Fatalf("late stamp landed on the settled row: applied=%v %s/%s/%q/paid_at %v",
			r.applied, r.inv.Status, r.inv.PaymentStatus, r.inv.StripePaymentIntentID, r.inv.PaidAt)
	}
}
