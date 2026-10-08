package invoice

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
)

// fakeCharger records whether the auto-charge was attempted.
type fakeCharger struct {
	called bool
	err    error
}

func (f *fakeCharger) ChargeInvoice(_ context.Context, _ string, inv domain.Invoice, _, _ string) (domain.Invoice, error) {
	f.called = true
	if f.err != nil {
		return domain.Invoice{}, f.err
	}
	return inv, nil
}

// fakePaymentSetups returns a canned payment-setup snapshot.
type fakePaymentSetups struct {
	setup domain.CustomerPaymentSetup
	err   error
}

func (f *fakePaymentSetups) GetPaymentSetup(_ context.Context, _, _ string) (domain.CustomerPaymentSetup, error) {
	return f.setup, f.err
}

// recordingCollector stands in for the billing engine's collector. It records
// the queue flag it saw, then settles the invoice the way a successful charge
// would, so the handler's response can be checked for post-collection state.
type recordingCollector struct {
	store     *memStore
	calls     int
	sawQueued bool
	sawStatus domain.InvoiceStatus
}

func (c *recordingCollector) CollectInvoice(ctx context.Context, tenantID, invoiceID string, at *time.Time) {
	c.calls++
	inv, _ := c.store.Get(ctx, tenantID, invoiceID)
	c.sawQueued = inv.AutoChargePending
	c.sawStatus = inv.Status
	_, _ = c.store.MarkPaid(ctx, tenantID, invoiceID, "pi_test", time.Now().UTC())
}

// Manual finalize records the collection intent in the finalize write, then
// hands the invoice to the one collector and responds with what it did. The
// collector must find the invoice already queued: that is what makes a crash
// between finalize and collection recoverable by the auto-charge sweep.
func TestFinalize_QueuesThenCollectsThroughTheOneCollector(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	inv, err := store.Create(context.Background(), "t1", domain.Invoice{
		CustomerID: "cus_1", Status: domain.InvoiceDraft, PaymentStatus: domain.PaymentPending,
		AmountDueCents: 5000, Currency: "USD", BillingReason: domain.BillingReasonManual,
	})
	if err != nil {
		t.Fatalf("seed invoice: %v", err)
	}
	collector := &recordingCollector{store: store}
	h := &Handler{svc: NewService(store, nil, nil)}
	h.SetCollector(collector)

	req := httptest.NewRequest(http.MethodPost, "/v1/invoices/"+inv.ID+"/finalize", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", inv.ID)
	reqCtx := context.WithValue(req.Context(), auth.TestTenantIDKey(), "t1")
	reqCtx = context.WithValue(reqCtx, chi.RouteCtxKey, rctx)
	rr := httptest.NewRecorder()
	h.finalize(rr, req.WithContext(reqCtx))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	if collector.calls != 1 {
		t.Fatalf("collector calls = %d, want 1", collector.calls)
	}
	if collector.sawStatus != domain.InvoiceFinalized || !collector.sawQueued {
		t.Errorf("collector saw status=%s queued=%v; the finalize write must queue the invoice before collection runs",
			collector.sawStatus, collector.sawQueued)
	}
	var body domain.Invoice
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Status != domain.InvoicePaid {
		t.Errorf("response status = %s, want paid (the post-collection state)", body.Status)
	}
}

// A DRAFT zero-due invoice must never settle through SettleZeroDue — the
// DEMO-000906 class: a tax-pending draft jumping to paid blocks tax retry
// forever. Both the service re-read (status must be finalized) and the
// store's MarkPaid guard enforce it; this pins the service half.
func TestSettleZeroDue_DraftIsNotSettled(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	inv, err := store.Create(context.Background(), "t1", domain.Invoice{
		CustomerID: "cus_1", Status: domain.InvoiceDraft,
		PaymentStatus: domain.PaymentPending, AmountDueCents: 0,
	})
	if err != nil {
		t.Fatalf("seed invoice: %v", err)
	}
	svc := NewService(store, nil, nil)

	out, err := svc.SettleZeroDue(context.Background(), "t1", inv.ID)
	if err != nil {
		t.Fatalf("SettleZeroDue on a draft must no-op, not error: %v", err)
	}
	if out.Status != domain.InvoiceDraft {
		t.Errorf("draft must stay draft, got %s", out.Status)
	}
	got, _ := store.Get(context.Background(), "t1", inv.ID)
	if got.Status != domain.InvoiceDraft || got.PaymentStatus != domain.PaymentPending {
		t.Errorf("stored draft mutated to %s/%s — must be untouched", got.Status, got.PaymentStatus)
	}
}

// fakeInvCreditApplier drains up to applyCents from the memStore invoice.
type fakeInvCreditApplier struct {
	store      *memStore
	applyCents int64
	err        error
	calls      int
}

func (f *fakeInvCreditApplier) ApplyToInvoiceAt(_ context.Context, tenantID, _, invoiceID string, amountCents int64, _ time.Time, _ ...string) (int64, error) {
	f.calls++
	if f.err != nil {
		return 0, f.err
	}
	inv, ok := f.store.invoices[invoiceID]
	if !ok {
		return 0, errs.ErrNotFound
	}
	deduct := f.applyCents
	if amountCents < deduct {
		deduct = amountCents
	}
	inv.AmountDueCents -= deduct
	inv.CreditsAppliedCents += deduct
	f.store.invoices[invoiceID] = inv
	return deduct, nil
}

// TestApplyCreditBalance_FinalizedOnly pins the ADR-113 status universe
// through the SERVICE, the only caller on the collect path.
//
// History matters here: this test's ancestor pinned the OPPOSITE for
// uncollectible (the charge-in-place recovery era, where credit had to drain
// before the card was charged the remainder). ADR-113 removed that feature,
// so credit draining against a written-off invoice would now be a ledger
// debit nothing consumes — the uncollectible row asserts refusal, and the
// finalized control keeps a refuse-everything predicate from passing.
func TestApplyCreditBalance_FinalizedOnly(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		status domain.InvoiceStatus
		want   bool
	}{
		{"finalized (control)", domain.InvoiceFinalized, true},
		{"uncollectible must NOT drain the balance (ADR-113)", domain.InvoiceUncollectible, false},
		{"voided must NOT drain the balance", domain.InvoiceVoided, false},
		{"paid must NOT drain the balance", domain.InvoicePaid, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newMemStore()
			inv, err := store.Create(context.Background(), "t1", domain.Invoice{
				AmountDueCents: 5000, CustomerID: "cus_1", Status: tc.status,
			})
			if err != nil {
				t.Fatalf("seed: %v", err)
			}
			applier := &fakeInvCreditApplier{store: store, applyCents: 2000}
			svc := NewService(store, nil, nil)
			svc.SetCreditApplier(applier)

			if _, err := svc.ApplyCreditBalance(context.Background(), "t1", inv.ID); err != nil {
				t.Fatalf("ApplyCreditBalance: %v", err)
			}
			got := applier.calls > 0
			if got != tc.want {
				t.Fatalf("credit applied = %v, want %v — draining credit against a non-chargeable invoice books a debit nothing consumes", got, tc.want)
			}
		})
	}
}
