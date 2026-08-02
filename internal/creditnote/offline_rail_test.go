package creditnote

import (
	"context"
	"strings"
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// The invariant under test: refund money may only target a REAL card
// PaymentIntent. RecordOfflinePayment settles an invoice by stamping a
// synthetic "out_of_band:<ts>" marker into the PI field, so AmountPaidCents > 0
// does NOT imply a card was charged, and the field being non-empty does not
// imply it is a Stripe id.
//
// Found by walking FLOW C2 (2026-08-02) on VLX-000068 — $53.62 settled entirely
// by bank transfer. Every site that moved refund money tested only `!= ""`, so
// the dialog offered "Refund to card — max $53.62", Create accepted a card
// allocation, Issue handed the synthetic marker to Stripe as a PaymentIntent
// id, and the note issued with refund_status=failed. That raised a real
// "1 refund needs attention" alert whose only remediation — Retry refund —
// answered HTTP 500 and could never succeed.
//
// Every case below is a CONTROL/TREATMENT pair over the same amounts: the only
// difference is whether the PI field holds a card id or the offline marker. A
// single-sided test would pass against a cap wired to AmountPaidCents alone.

func offlineInvoice() domain.Invoice {
	return domain.Invoice{
		ID: "inv_offline", TenantID: "t1", CustomerID: "cus_1",
		Status: domain.InvoicePaid, PaymentStatus: domain.PaymentSucceeded,
		Currency: "USD", TotalAmountCents: 5362, AmountPaidCents: 5362,
		StripePaymentIntentID: domain.OutOfBandPaymentIntentPrefix + "2026-08-02T15:22:52Z",
	}
}

func cardInvoice() domain.Invoice {
	inv := offlineInvoice()
	inv.ID = "inv_card"
	inv.StripePaymentIntentID = "pi_real_card"
	return inv
}

func mkRailSvc(inv domain.Invoice) (*Service, *fakeRefunder) {
	store := newMemStore()
	invoices := &memInvoiceReader{invoices: map[string]domain.Invoice{inv.ID: inv}}
	refunder := &fakeRefunder{}
	svc := NewService(store, invoices, refunder)
	svc.SetNumberGenerator(&fakeCNNumbers{})
	return svc, refunder
}

func TestDomain_HasCardPayment(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		pi   string
		want bool
	}{
		{"real card PaymentIntent", "pi_3TynLTGcT3wmy5fZ", true},
		{"never charged", "", false},
		{"offline marker", "out_of_band:2026-08-02T15:22:52Z", false},
		// Guards the prefix test against a naive Contains/suffix rewrite.
		{"card id merely containing the word", "pi_out_of_band_lookalike", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inv := domain.Invoice{StripePaymentIntentID: tc.pi, AmountPaidCents: 5000}
			if got := inv.HasCardPayment(); got != tc.want {
				t.Errorf("HasCardPayment(%q) = %v, want %v", tc.pi, got, tc.want)
			}
			// The cap must collapse to zero exactly when there is no card,
			// so the offline case reuses the over-refund guard rather than
			// needing a second parallel check.
			gotCap := inv.CardRefundableCents(0)
			wantCap := int64(0)
			if tc.want {
				wantCap = 5000
			}
			if gotCap != wantCap {
				t.Errorf("CardRefundableCents = %d, want %d", gotCap, wantCap)
			}
		})
	}

	t.Run("prior refunds subtract, never below zero", func(t *testing.T) {
		inv := domain.Invoice{StripePaymentIntentID: "pi_x", AmountPaidCents: 6260}
		if got := inv.CardRefundableCents(6260); got != 0 {
			t.Errorf("fully refunded: got %d, want 0", got)
		}
		if got := inv.CardRefundableCents(9999); got != 0 {
			t.Errorf("over-refunded must clamp at 0, got %d", got)
		}
		if got := inv.CardRefundableCents(1000); got != 5260 {
			t.Errorf("partial: got %d, want 5260", got)
		}
	})
}

func TestCreate_RefundAllocation_RequiresCardPayment(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	mkInput := func(invID string) CreateInput {
		return CreateInput{
			InvoiceID:         invID,
			Reason:            "Service credit",
			RefundAmountCents: 1000,
			CreditAmountCents: 0,
			Lines:             []CreditLineInput{{Description: "x", Quantity: 1, UnitAmountCents: 1000}},
		}
	}

	t.Run("CONTROL card-paid — identical allocation is accepted", func(t *testing.T) {
		svc, _ := mkRailSvc(cardInvoice())
		cn, err := svc.Create(ctx, "t1", mkInput("inv_card"))
		if err != nil {
			t.Fatalf("card-paid refund allocation must be accepted: %v", err)
		}
		if cn.RefundAmountCents != 1000 {
			t.Errorf("refund_amount_cents: got %d want 1000", cn.RefundAmountCents)
		}
	})

	t.Run("TREATMENT offline-paid — refused, naming the rail", func(t *testing.T) {
		svc, _ := mkRailSvc(offlineInvoice())
		_, err := svc.Create(ctx, "t1", mkInput("inv_offline"))
		if err == nil {
			t.Fatal("a card refund on an offline-paid invoice must be refused")
		}
		// The operator's fix is to RE-ROUTE the money, not lower it, so the
		// message must name the rail rather than read "exceeds 0.00".
		if !strings.Contains(err.Error(), "not paid by card") {
			t.Errorf("error should name the rail: %q", err.Error())
		}
		if strings.Contains(err.Error(), "exceeds payment-method refundable") {
			t.Errorf("over-cap wording misdescribes the offline cause: %q", err.Error())
		}
	})

	t.Run("offline-paid still credits and out-of-bands freely", func(t *testing.T) {
		svc, _ := mkRailSvc(offlineInvoice())
		cn, err := svc.Create(ctx, "t1", CreateInput{
			InvoiceID:            "inv_offline",
			Reason:               "Service credit",
			CreditAmountCents:    600,
			OutOfBandAmountCents: 400,
			Lines:                []CreditLineInput{{Description: "x", Quantity: 1, UnitAmountCents: 1000}},
		})
		if err != nil {
			t.Fatalf("non-card channels must stay available on an offline invoice: %v", err)
		}
		if cn.CreditAmountCents != 600 || cn.OutOfBandAmountCents != 400 {
			t.Errorf("allocation: credit=%d oob=%d, want 600/400", cn.CreditAmountCents, cn.OutOfBandAmountCents)
		}
	})
}

func TestCreateRefund_InvoiceEndpoint_RequiresCardPayment(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("CONTROL card-paid refunds", func(t *testing.T) {
		svc, refunder := mkRailSvc(cardInvoice())
		if _, err := svc.CreateRefund(ctx, "t1", RefundInput{InvoiceID: "inv_card", Reason: "requested_by_customer"}); err != nil {
			t.Fatalf("card-paid full refund must work: %v", err)
		}
		if len(refunder.calls) != 1 {
			t.Fatalf("expected exactly one Stripe call, got %d", len(refunder.calls))
		}
	})

	t.Run("TREATMENT offline-paid refused and Stripe never called", func(t *testing.T) {
		svc, refunder := mkRailSvc(offlineInvoice())
		_, err := svc.CreateRefund(ctx, "t1", RefundInput{InvoiceID: "inv_offline", Reason: "requested_by_customer"})
		if err == nil {
			t.Fatal("refund endpoint must refuse an offline-paid invoice")
		}
		if !strings.Contains(err.Error(), "outside Stripe") {
			t.Errorf("error should explain the rail: %q", err.Error())
		}
		// Mutation-verify: the whole point is that the synthetic marker never
		// reaches the provider as a PaymentIntent id.
		if len(refunder.calls) != 0 {
			t.Fatalf("Stripe must not be called; got %d call(s) with pi=%q",
				len(refunder.calls), refunder.calls[0].paymentIntentID)
		}
	})
}

func TestRetryRefund_OfflinePaid_FailsCleanlyInsteadOfForever(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// A refund leg written BEFORE the validation guard existed: the row is
	// already in the DB with refund_amount > 0 against an offline-paid
	// invoice. Retry is the operator's only button on the "needs attention"
	// queue, so it must answer with an actionable terminal state rather than
	// calling Stripe with a marker and surfacing an opaque 500.
	inv := offlineInvoice()
	store := newMemStore()
	invoices := &memInvoiceReader{invoices: map[string]domain.Invoice{inv.ID: inv}}
	refunder := &fakeRefunder{}
	svc := NewService(store, invoices, refunder)
	svc.SetNumberGenerator(&fakeCNNumbers{})

	legacy := domain.CreditNote{
		ID: "vlx_cn_legacy", TenantID: "t1", InvoiceID: inv.ID, CustomerID: "cus_1",
		Status: domain.CreditNoteIssued, Currency: "USD",
		RefundAmountCents: 1000, RefundStatus: domain.RefundFailed,
	}
	store.notes[legacy.ID] = legacy

	_, err := svc.RetryRefund(ctx, "t1", legacy.ID)
	if err == nil {
		t.Fatal("retry against an offline-paid invoice must not report success")
	}
	if !strings.Contains(err.Error(), "never succeed") {
		t.Errorf("error must tell the operator this is terminal, not transient: %q", err.Error())
	}
	if len(refunder.calls) != 0 {
		t.Fatalf("Stripe must not be called on retry; got %d call(s)", len(refunder.calls))
	}
}
