package payment_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/payment"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
	"github.com/sagarsuperuser/velox/internal/webhook"
)

// boundaryStripe is the concurrent-resolver fake: at the external-call
// boundary (inside CreatePaymentIntent, after the charge took its hold and
// re-checked the invoice) it runs onCreate, which commits the racing
// transition on the real database, and then answers.
type boundaryStripe struct {
	mu       sync.Mutex
	onCreate func()
	res      payment.PaymentIntentResult
	err      error
}

func (f *boundaryStripe) CreatePaymentIntent(context.Context, payment.PaymentIntentParams) (payment.PaymentIntentResult, error) {
	f.mu.Lock()
	hook, res, err := f.onCreate, f.res, f.err
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	return res, err
}

func (f *boundaryStripe) CancelPaymentIntent(context.Context, string) error { return nil }

func (f *boundaryStripe) GetPaymentIntent(_ context.Context, piID string) (payment.PaymentIntentResult, error) {
	return payment.PaymentIntentResult{ID: piID, Status: "succeeded", AmountReceivedCents: settleDue}, nil
}

func (f *boundaryStripe) ListPaymentIntentsForCustomer(context.Context, string, time.Time) ([]payment.PaymentIntentResult, error) {
	return nil, nil
}

// An offline payment and a card charge on the same invoice, raced at the
// Stripe call, on the real invoice store and service.
//
//   - While the charge holds the invoice, the offline payment is refused
//     (#975) and the card payment is recorded with its PaymentIntent.
//   - When the hold is gone before the operator records the payment (the
//     stale-hold sweep cleared it), the offline payment wins. The charge's
//     late answer then cannot write its PaymentIntent onto the paid row:
//     the invoice keeps no PaymentIntent and paid_out_of_band, so nothing
//     later offers a card refund for money that came by bank transfer. A
//     success for that charge is a second payment, flagged as one.
func TestOfflinePayment_RacedWithCardChargeAtTheStripeCall(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test — requires postgres")
	}
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	invStore := invoice.NewPostgresStore(db)
	outbox := webhook.NewOutboxStore(db)
	invStore.SetOutboxEnqueuer(outbox)
	invSvc := invoice.NewService(invStore, nil, nil)
	client := &boundaryStripe{}
	s := payment.NewStripe(client, invStore, payment.NewPostgresWebhookStore(db), nil)
	s.SetCardFetcher(settleRaceCard{})
	s.SetEventDispatcher(webhook.NewOutboxDispatcher(outbox))
	s.SetAnomalyRecorder(invStore)
	rig := &settleRaceRig{db: db, ctx: ctx, invStore: invStore, customers: customer.NewPostgresStore(db)}

	// column reads the PaymentIntent column and the out-of-band flag as stored.
	column := func(t *testing.T, tenantID, id string) (pi *string, offline bool) {
		t.Helper()
		tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
		if err != nil {
			t.Fatal(err)
		}
		defer postgres.Rollback(tx)
		var raw *string
		var flag *bool
		// to_jsonb keeps this compiling and failing cleanly on a schema
		// without the column: the key is simply absent.
		if err := tx.QueryRowContext(ctx, `SELECT stripe_payment_intent_id, (to_jsonb(i) ->> 'paid_out_of_band')::boolean FROM invoices i WHERE id = $1`, id).Scan(&raw, &flag); err != nil {
			t.Fatalf("read invoice columns: %v", err)
		}
		return raw, flag != nil && *flag
	}
	charge := func(t *testing.T, name string, onCreate func(tenantID, invoiceID string), res payment.PaymentIntentResult, cerr error) (string, domain.Invoice, domain.Invoice, error) {
		t.Helper()
		tenantID, inv := rig.seed(t, name)
		hold, err := invStore.ClaimChargeForManualCollect(ctx, tenantID, inv.ID)
		if err != nil || !hold.Held() {
			t.Fatalf("claim: held=%v err=%v", hold.Held(), err)
		}
		client.mu.Lock()
		client.onCreate = func() { onCreate(tenantID, inv.ID) }
		client.res, client.err = res, cerr
		client.mu.Unlock()
		got, chargeErr := s.ChargeInvoice(ctx, tenantID, inv, hold, "cus_stripe_"+name, "pm_"+name)
		after, err := invStore.Get(ctx, tenantID, inv.ID)
		if err != nil {
			t.Fatalf("re-read: %v", err)
		}
		return tenantID, got, after, chargeErr
	}
	recordOffline := func(t *testing.T, clearHold bool) func(tenantID, invoiceID string) {
		return func(tenantID, invoiceID string) {
			if clearHold {
				cur, err := invStore.Get(ctx, tenantID, invoiceID)
				if err != nil {
					t.Errorf("read hold: %v", err)
					return
				}
				release(t, db, ctx, tenantID, cur.ID)
			}
			_, err := invSvc.RecordOfflinePayment(ctx, tenantID, invoiceID, "wire")
			if clearHold && err != nil {
				t.Errorf("offline payment after the hold cleared: %v", err)
			}
			if !clearHold && !errors.Is(err, errs.ErrInvalidState) {
				t.Errorf("offline payment while the charge holds the invoice: err %v, want a refusal", err)
			}
		}
	}

	t.Run("charge holds the invoice: offline refused, card payment recorded", func(t *testing.T) {
		tenantID, _, after, err := charge(t, "oob-race-held", recordOffline(t, false),
			payment.PaymentIntentResult{ID: "pi_oob_race_held", Status: "succeeded"}, nil)
		if err != nil {
			t.Fatalf("ChargeInvoice: %v", err)
		}
		if after.Status != domain.InvoicePaid || after.StripePaymentIntentID != "pi_oob_race_held" || after.AmountPaidCents != settleDue {
			t.Errorf("invoice %s PI %q paid %d, want paid by pi_oob_race_held for %d", after.Status, after.StripePaymentIntentID, after.AmountPaidCents, settleDue)
		}
		if _, offline := column(t, tenantID, after.ID); offline {
			t.Error("paid_out_of_band set on a card payment")
		}
	})

	for _, c := range []struct {
		name string
		res  payment.PaymentIntentResult
		err  error
	}{
		{"processing", payment.PaymentIntentResult{ID: "pi_oob_race_proc", Status: "processing"}, nil},
		{"declined", payment.PaymentIntentResult{}, &payment.PaymentError{Message: "Your card was declined.", DeclineCode: "generic_decline", PaymentIntentID: "pi_oob_race_decl"}},
	} {
		t.Run("hold cleared, offline payment wins, then the charge answers "+c.name, func(t *testing.T) {
			tenantID, _, after, _ := charge(t, "oob-race-"+c.name, recordOffline(t, true), c.res, c.err)
			if after.Status != domain.InvoicePaid || after.PaymentStatus != domain.PaymentSucceeded || after.AmountPaidCents != settleDue {
				t.Fatalf("invoice %s/%s paid %d, want the offline payment's paid/succeeded %d", after.Status, after.PaymentStatus, after.AmountPaidCents, settleDue)
			}
			pi, offline := column(t, tenantID, after.ID)
			if pi != nil {
				t.Errorf("invoices.stripe_payment_intent_id = %q, want NULL: the charge's answer must not land on an invoice paid outside Velox", *pi)
			}
			if !offline {
				t.Error("paid_out_of_band = false, want true")
			}
			if after.HasCardPayment() {
				t.Error("HasCardPayment() = true on an invoice paid by bank transfer")
			}
		})
	}

	t.Run("a success for the losing charge is a second payment", func(t *testing.T) {
		tenantID, _, after, _ := charge(t, "oob-race-dup", recordOffline(t, true),
			payment.PaymentIntentResult{ID: "pi_oob_race_dup", Status: "processing"}, nil)
		if err := s.HandleWebhook(ctx, tenantID, succeededEvent("evt_oob_race_dup", "pi_oob_race_dup", after.ID)); err != nil {
			t.Fatalf("webhook: %v", err)
		}
		got, err := invStore.Get(ctx, tenantID, after.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.PaymentAnomalyKind != domain.EventPaymentDuplicateCharge || got.PaymentAnomalyPaymentIntentID != "pi_oob_race_dup" {
			t.Errorf("anomaly %q on %q, want %s on pi_oob_race_dup", got.PaymentAnomalyKind, got.PaymentAnomalyPaymentIntentID, domain.EventPaymentDuplicateCharge)
		}
		if pi, offline := column(t, tenantID, got.ID); pi != nil || !offline {
			t.Errorf("after the second payment: PaymentIntent %v, paid_out_of_band %v; want NULL and true", pi, offline)
		}
		if n := rig.count(t, `SELECT count(*) FROM webhook_outbox WHERE tenant_id=$1 AND event_type=$2 AND payload->'invoice'->>'id'=$3`,
			tenantID, domain.EventInvoicePaid, got.ID); n != 1 {
			t.Errorf("invoice.paid rows = %d, want 1", n)
		}
		// The webhook says the first payment was made outside Velox: its
		// recorded PaymentIntent is empty, as for a credits-paid invoice,
		// and paid_out_of_band tells the two apart.
		if n := rig.count(t, `SELECT count(*) FROM webhook_outbox WHERE tenant_id=$1 AND event_type=$2 AND payload->'invoice'->>'id'=$3
			AND COALESCE(payload->'invoice'->>'stripe_payment_intent_id', '') = '' AND payload->>'incoming_payment_intent_id' = 'pi_oob_race_dup'
			AND (payload->'invoice'->>'paid_out_of_band')::boolean`,
			tenantID, domain.EventPaymentDuplicateCharge, got.ID); n != 1 {
			t.Errorf("payment.duplicate_charge rows with paid_out_of_band true and no recorded PaymentIntent = %d, want 1", n)
		}
	})
}

// release clears the invoice's charge hold, as the stale-hold sweep does
// when Stripe shows no payment for it (ADR-127).
func release(t *testing.T, db *postgres.DB, ctx context.Context, tenantID, id string) {
	t.Helper()
	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatal(err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(ctx, `UPDATE invoices SET auto_charge_claimed_until = NULL WHERE id = $1`, id); err != nil {
		t.Fatalf("clear hold: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(fmt.Errorf("commit hold clear: %w", err))
	}
}
