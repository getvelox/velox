package invoice

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
)

// TestInFlightGuards pins the in-flight payment guards on the three operator
// status-mutation paths (Void, MarkUncollectible, RecordOfflinePayment). A
// charge "in flight" (payment_status processing/unknown) may still succeed, so
// mutating the invoice now strands captured money on a voided invoice, reverses
// tax on a sale that completes (under-remit), or double-collects an offline
// payment. All three must reject with ErrInvalidState (→409) while in flight,
// and must still allow the legitimate non-in-flight states. Stripe enforces the
// same open-payment rule.
func TestInFlightGuards(t *testing.T) {
	ctx := context.Background()

	// finalized builds a finalized invoice and forces its payment_status.
	finalized := func(ps domain.InvoicePaymentStatus) (*Service, string) {
		store := newMemStore()
		svc := NewService(store, nil, newMemNumberer())
		svc.SetTaxReverser(&fakeTaxReverser{})
		inv, err := svc.Create(ctx, "t1", CreateInput{
			CustomerID: "c", SubscriptionID: "s",
			BillingPeriodStart: time.Now(), BillingPeriodEnd: time.Now().AddDate(0, 1, 0),
		})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if _, err := svc.Finalize(ctx, "t1", inv.ID); err != nil {
			t.Fatalf("finalize: %v", err)
		}
		cur := store.invoices[inv.ID]
		cur.PaymentStatus = ps
		store.invoices[inv.ID] = cur
		return svc, inv.ID
	}

	inFlight := []domain.InvoicePaymentStatus{domain.PaymentProcessing, domain.PaymentUnknown}

	for _, ps := range inFlight {
		t.Run("Void rejected when "+string(ps), func(t *testing.T) {
			svc, id := finalized(ps)
			if _, err := svc.Void(ctx, "t1", id); !errors.Is(err, errs.ErrInvalidState) {
				t.Fatalf("Void on %s: want ErrInvalidState (→409), got %v", ps, err)
			}
		})
		t.Run("MarkUncollectible rejected when "+string(ps), func(t *testing.T) {
			if ps == domain.PaymentUnknown {
				// DELIBERATE carve-out (ADR-107). An 'unknown' payment with no
				// PaymentIntent id is PARKED — excluded from every charge path,
				// so without this exit the invoice can reach no terminal state
				// at all and the operator has literally no action. Writing it
				// off moves no money, and a late webhook still marks it paid.
				// The parked path has its own coverage in
				// TestParkedInvoiceHasExactlyOneWayOut, including the negative
				// control that 'processing' still refuses.
				t.Skip("parked invoices are deliberately write-off-able — see ADR-107")
			}
			svc, id := finalized(ps)
			if _, err := svc.MarkUncollectible(ctx, "t1", id); !errors.Is(err, errs.ErrInvalidState) {
				t.Fatalf("MarkUncollectible on %s: want ErrInvalidState, got %v", ps, err)
			}
		})
		t.Run("RecordOfflinePayment rejected when "+string(ps), func(t *testing.T) {
			svc, id := finalized(ps)
			if _, err := svc.RecordOfflinePayment(ctx, "t1", id, "Cheque #1"); !errors.Is(err, errs.ErrInvalidState) {
				t.Fatalf("RecordOfflinePayment on %s: want ErrInvalidState, got %v", ps, err)
			}
		})
	}

	// The guard must NOT block legitimate non-in-flight states. A finalized
	// invoice still pending (no charge open) and a failed charge are both
	// voidable / markable / offline-recordable.
	for _, ps := range []domain.InvoicePaymentStatus{domain.PaymentPending, domain.PaymentFailed} {
		t.Run("Void allowed when "+string(ps), func(t *testing.T) {
			svc, id := finalized(ps)
			if _, err := svc.Void(ctx, "t1", id); err != nil {
				t.Fatalf("Void on %s should be allowed, got %v", ps, err)
			}
		})
		t.Run("MarkUncollectible allowed when "+string(ps), func(t *testing.T) {
			svc, id := finalized(ps)
			if _, err := svc.MarkUncollectible(ctx, "t1", id); err != nil {
				t.Fatalf("MarkUncollectible on %s should be allowed, got %v", ps, err)
			}
		})
		t.Run("RecordOfflinePayment allowed when "+string(ps), func(t *testing.T) {
			svc, id := finalized(ps)
			if _, err := svc.RecordOfflinePayment(ctx, "t1", id, "Wire"); err != nil {
				t.Fatalf("RecordOfflinePayment on %s should be allowed, got %v", ps, err)
			}
		})
	}

	// Guard precedence: the in-flight check sits BEFORE reverseInvoiceTax, so a
	// blocked Void/MarkUncollectible must NOT have fired a tax reversal.
	t.Run("blocked Void does not reverse tax", func(t *testing.T) {
		store := newMemStore()
		svc := NewService(store, nil, newMemNumberer())
		rev := &fakeTaxReverser{}
		svc.SetTaxReverser(rev)
		inv, _ := svc.Create(ctx, "t1", CreateInput{
			CustomerID: "c", SubscriptionID: "s",
			BillingPeriodStart: time.Now(), BillingPeriodEnd: time.Now().AddDate(0, 1, 0),
		})
		if _, err := svc.Finalize(ctx, "t1", inv.ID); err != nil {
			t.Fatalf("finalize: %v", err)
		}
		cur := store.invoices[inv.ID]
		cur.PaymentStatus = domain.PaymentProcessing
		cur.TaxTransactionID = "txn_1"
		store.invoices[inv.ID] = cur
		if _, err := svc.Void(ctx, "t1", inv.ID); !errors.Is(err, errs.ErrInvalidState) {
			t.Fatalf("want ErrInvalidState, got %v", err)
		}
		if len(rev.calls) != 0 {
			t.Errorf("tax reversal must not fire on an in-flight blocked void; got %d calls", len(rev.calls))
		}
	})
}
