package creditnote

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/tax"
)

// fakeTaxReverser records reversal calls and returns a deterministic
// upstream transaction id so tests can assert idempotency + persistence.
type fakeTaxReverser struct {
	calls    []tax.ReversalRequest
	failWith error
	returnID string
}

func (f *fakeTaxReverser) ReverseTax(_ context.Context, _ string, req tax.ReversalRequest) (*tax.ReversalResult, error) {
	f.calls = append(f.calls, req)
	if f.failWith != nil {
		return nil, f.failWith
	}
	id := f.returnID
	if id == "" {
		id = "tx_reversal_fake"
	}
	return &tax.ReversalResult{TransactionID: id}, nil
}

// TestCreate_ProportionalTaxBreakdown verifies Create() splits the gross
// CN subtotal into net + tax using the invoice's tax ratio. Without this,
// the CN would show the gross as net and claim zero tax was collected —
// wrong for any tax-bearing invoice.
func TestCreate_ProportionalTaxBreakdown(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	invoices := &memInvoiceReader{
		invoices: map[string]domain.Invoice{
			"inv_taxed": {
				ID: "inv_taxed", TenantID: "t1", CustomerID: "cus_1",
				Status: domain.InvoiceFinalized, Currency: "USD",
				SubtotalCents:    10000,
				TaxFacts:         domain.TaxFacts{TaxAmountCents: 1800},
				TotalAmountCents: 11800,
				AmountDueCents:   11800,
			},
		},
	}
	svc := NewService(store, invoices, nil)
	svc.SetNumberGenerator(&fakeCNNumbers{})

	// Half refund → should break out half the tax.
	cn, err := svc.Create(context.Background(), "t1", CreateInput{
		InvoiceID: "inv_taxed",
		Reason:    "partial adjustment",
		Lines: []CreditLineInput{
			{Description: "adj", Quantity: 1, UnitAmountCents: 5900},
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// taxAmount = 5900 * 1800 / 11800 = 900
	if cn.TaxAmountCents != 900 {
		t.Errorf("tax_amount_cents: got %d, want 900 (5900 * 1800 / 11800)", cn.TaxAmountCents)
	}
	if cn.SubtotalCents != 5000 {
		t.Errorf("subtotal_cents (net): got %d, want 5000 (5900 - 900 tax)", cn.SubtotalCents)
	}
	if cn.TotalCents != 5900 {
		t.Errorf("total_cents (gross): got %d, want 5900 (unchanged gross)", cn.TotalCents)
	}
}

// TestCreate_ZeroTaxInvoiceLeavesZeroTaxOnCN verifies the proportional
// block preserves legacy behaviour when the invoice has no tax: the CN
// reports zero tax and subtotal == total == sum of line amounts. Existing
// tests and integrations that assume this invariant continue to work.
func TestCreate_ZeroTaxInvoiceLeavesZeroTaxOnCN(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	invoices := &memInvoiceReader{
		invoices: map[string]domain.Invoice{
			"inv_notax": {
				ID: "inv_notax", TenantID: "t1", CustomerID: "cus_1",
				Status: domain.InvoiceFinalized, Currency: "USD",
				SubtotalCents:    10000,
				TaxFacts:         domain.TaxFacts{TaxAmountCents: 0},
				TotalAmountCents: 10000,
				AmountDueCents:   10000,
			},
		},
	}
	svc := NewService(store, invoices, nil)
	svc.SetNumberGenerator(&fakeCNNumbers{})

	cn, err := svc.Create(context.Background(), "t1", CreateInput{
		InvoiceID: "inv_notax", Reason: "credit",
		Lines: []CreditLineInput{{Description: "x", Quantity: 1, UnitAmountCents: 3000}},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if cn.TaxAmountCents != 0 {
		t.Errorf("tax_amount_cents: got %d, want 0", cn.TaxAmountCents)
	}
	if cn.SubtotalCents != 3000 {
		t.Errorf("subtotal_cents: got %d, want 3000", cn.SubtotalCents)
	}
	if cn.TotalCents != 3000 {
		t.Errorf("total_cents: got %d, want 3000", cn.TotalCents)
	}
}

// TestIssue_TaxReversalOnPaidInvoiceWithTransaction verifies the happy
// path: a paid invoice with a committed tax_transaction triggers a
// partial reversal at the CN's gross total, and the returned upstream id
// is persisted onto the CN.
func TestIssue_TaxReversalOnPaidInvoiceWithTransaction(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	invoices := &memInvoiceReader{
		invoices: map[string]domain.Invoice{
			"inv_paid": {
				ID: "inv_paid", TenantID: "t1", CustomerID: "cus_1",
				Status: domain.InvoicePaid, PaymentStatus: domain.PaymentSucceeded,
				Currency: "USD", SubtotalCents: 10000, TaxFacts: domain.TaxFacts{TaxAmountCents: 1800},
				TotalAmountCents: 11800, AmountPaidCents: 11800,
				StripePaymentIntentID: "pi_1",
				TaxTransactionID:      "tx_upstream_1",
			},
		},
	}
	refunder := &fakeRefunder{}
	rev := &fakeTaxReverser{returnID: "tx_reversal_1"}
	svc := NewService(store, invoices, refunder, &fakeCreditGranter{})
	svc.SetNumberGenerator(&fakeCNNumbers{})
	svc.SetTaxReverser(rev)

	cn, err := svc.CreateRefund(context.Background(), "t1", RefundInput{
		InvoiceID: "inv_paid", Reason: "duplicate",
	})
	if err != nil {
		t.Fatalf("CreateRefund: %v", err)
	}
	if len(rev.calls) != 1 {
		t.Fatalf("reverse calls: got %d, want 1", len(rev.calls))
	}
	call := rev.calls[0]
	if call.OriginalTransactionID != "tx_upstream_1" {
		t.Errorf("OriginalTransactionID: got %q, want tx_upstream_1", call.OriginalTransactionID)
	}
	if call.Mode != tax.ReversalModePartial {
		t.Errorf("Mode: got %q, want partial", call.Mode)
	}
	if call.GrossAmountCents != 11800 {
		t.Errorf("GrossAmountCents: got %d, want 11800", call.GrossAmountCents)
	}
	if call.CreditNoteID == "" {
		t.Error("CreditNoteID should be set for idempotency")
	}
	// Verify reversal id got persisted.
	stored, err := store.Get(context.Background(), "t1", cn.ID)
	if err != nil {
		t.Fatalf("Get stored CN: %v", err)
	}
	if stored.TaxTransactionID != "tx_reversal_1" {
		t.Errorf("stored TaxTransactionID: got %q, want tx_reversal_1", stored.TaxTransactionID)
	}
}

// TestIssue_NoReversalWhenInvoiceHasNoTransactionID covers tenants on
// manual/none providers (or legacy invoices from before the commit
// column existed). The reverser must not be called — invoking it with
// an empty OriginalTransactionID would produce a provider error.
func TestIssue_NoReversalWhenInvoiceHasNoTransactionID(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	invoices := &memInvoiceReader{
		invoices: map[string]domain.Invoice{
			"inv_manual": {
				ID: "inv_manual", TenantID: "t1", CustomerID: "cus_1",
				Status: domain.InvoicePaid, PaymentStatus: domain.PaymentSucceeded,
				Currency: "USD", SubtotalCents: 10000, TaxFacts: domain.TaxFacts{TaxAmountCents: 1800},
				TotalAmountCents: 11800, AmountPaidCents: 11800,
				StripePaymentIntentID: "pi_2",
				// TaxTransactionID deliberately empty.
			},
		},
	}
	refunder := &fakeRefunder{}
	rev := &fakeTaxReverser{}
	svc := NewService(store, invoices, refunder, &fakeCreditGranter{})
	svc.SetNumberGenerator(&fakeCNNumbers{})
	svc.SetTaxReverser(rev)

	_, err := svc.CreateRefund(context.Background(), "t1", RefundInput{
		InvoiceID: "inv_manual", Reason: "duplicate",
	})
	if err != nil {
		t.Fatalf("CreateRefund: %v", err)
	}
	if len(rev.calls) != 0 {
		t.Errorf("reverse calls: got %d, want 0 (no upstream state)", len(rev.calls))
	}
}

// TestIssue_NoReverserWiredDoesNotCall covers the narrow-test and
// no-provider-configured paths. Without SetTaxReverser the CN issues
// normally without crashing.
func TestIssue_NoReverserWiredDoesNotCall(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	invoices := &memInvoiceReader{
		invoices: map[string]domain.Invoice{
			"inv_paid": {
				ID: "inv_paid", TenantID: "t1", CustomerID: "cus_1",
				Status: domain.InvoicePaid, PaymentStatus: domain.PaymentSucceeded,
				Currency: "USD", TotalAmountCents: 10000, AmountPaidCents: 10000,
				StripePaymentIntentID: "pi_3",
				TaxTransactionID:      "tx_upstream_x",
			},
		},
	}
	refunder := &fakeRefunder{}
	svc := NewService(store, invoices, refunder, &fakeCreditGranter{})
	svc.SetNumberGenerator(&fakeCNNumbers{})

	cn, err := svc.CreateRefund(context.Background(), "t1", RefundInput{
		InvoiceID: "inv_paid", Reason: "duplicate",
	})
	if err != nil {
		t.Fatalf("CreateRefund: %v", err)
	}
	if cn.Status != domain.CreditNoteIssued {
		t.Errorf("status: got %q, want issued", cn.Status)
	}
	if cn.TaxTransactionID != "" {
		t.Errorf("TaxTransactionID: got %q, want empty (no reverser)", cn.TaxTransactionID)
	}
}

// TestIssue_ReversalFailureStillIssuesCN — parity with the Stripe-refund
// failure policy: the CN is an accounting document, so a provider error
// on the tax reversal is logged (warn) but the CN still transitions to
// issued. Operators reconcile manually.
func TestIssue_ReversalFailureStillIssuesCN(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	invoices := &memInvoiceReader{
		invoices: map[string]domain.Invoice{
			"inv_paid": {
				ID: "inv_paid", TenantID: "t1", CustomerID: "cus_1",
				Status: domain.InvoicePaid, PaymentStatus: domain.PaymentSucceeded,
				Currency: "USD", SubtotalCents: 10000, TaxFacts: domain.TaxFacts{TaxAmountCents: 1800},
				TotalAmountCents: 11800, AmountPaidCents: 11800,
				StripePaymentIntentID: "pi_4",
				TaxTransactionID:      "tx_upstream_2",
			},
		},
	}
	refunder := &fakeRefunder{}
	rev := &fakeTaxReverser{failWith: errors.New("stripe: api error")}
	svc := NewService(store, invoices, refunder, &fakeCreditGranter{})
	svc.SetNumberGenerator(&fakeCNNumbers{})
	svc.SetTaxReverser(rev)

	cn, err := svc.CreateRefund(context.Background(), "t1", RefundInput{
		InvoiceID: "inv_paid", Reason: "fraudulent",
	})
	if err != nil {
		t.Fatalf("CreateRefund: %v", err)
	}
	if cn.Status != domain.CreditNoteIssued {
		t.Errorf("status: got %q, want issued (CN issues even on reversal failure)", cn.Status)
	}
	if cn.TaxTransactionID != "" {
		t.Errorf("TaxTransactionID: got %q, want empty on failure", cn.TaxTransactionID)
	}
	if !cn.TaxReversalPending {
		t.Error("TaxReversalPending: got false, want true — a failed reversal must be marked so the recovery sweep re-drives it (no silent over-remit)")
	}
	if len(rev.calls) != 1 {
		t.Errorf("reverse calls: got %d, want 1", len(rev.calls))
	}
}

// TestRetryPendingCreditNoteTaxReversal_ReDrivesAndClears proves the CN-scoped
// reconciler closes the over-remit gap #310 can't see: it re-drives a failed
// credit-note tax reversal and clears the marker on success. The first inline
// attempt fails (marked pending); the sweep, once Stripe recovers, reverses and
// stamps the transaction id.
func TestRetryPendingCreditNoteTaxReversal_ReDrivesAndClears(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	invoices := &memInvoiceReader{
		invoices: map[string]domain.Invoice{
			"inv_paid": {
				ID: "inv_paid", TenantID: "t1", CustomerID: "cus_1",
				Status: domain.InvoicePaid, PaymentStatus: domain.PaymentSucceeded,
				Currency: "USD", SubtotalCents: 10000, TaxFacts: domain.TaxFacts{TaxAmountCents: 1800},
				TotalAmountCents: 11800, AmountPaidCents: 11800,
				StripePaymentIntentID: "pi_5",
				TaxTransactionID:      "tx_upstream_3",
			},
		},
	}
	rev := &fakeTaxReverser{failWith: errors.New("stripe: transient")}
	svc := NewService(store, invoices, &fakeRefunder{}, &fakeCreditGranter{})
	svc.SetNumberGenerator(&fakeCNNumbers{})
	svc.SetTaxReverser(rev)

	cn, err := svc.CreateRefund(context.Background(), "t1", RefundInput{InvoiceID: "inv_paid", Reason: "fraudulent"})
	if err != nil {
		t.Fatalf("CreateRefund: %v", err)
	}
	if !cn.TaxReversalPending {
		t.Fatal("precondition: CN must be marked tax_reversal_pending after the failed inline reversal")
	}

	// Stripe recovers; the sweep must re-drive and clear the marker.
	rev.failWith = nil
	rev.returnID = "tx_reversal_recovered"
	n, sweepErrs := svc.RetryPendingCreditNoteTaxReversal(context.Background(), 10)
	if len(sweepErrs) != 0 {
		t.Fatalf("sweep errors: %v", sweepErrs)
	}
	if n != 1 {
		t.Errorf("recovered: got %d, want 1", n)
	}

	got, _ := store.Get(context.Background(), "t1", cn.ID)
	if got.TaxReversalPending {
		t.Error("marker not cleared after a successful re-drive")
	}
	if got.TaxTransactionID != "tx_reversal_recovered" {
		t.Errorf("reversal tx id: got %q, want tx_reversal_recovered", got.TaxTransactionID)
	}
	if len(rev.calls) != 2 {
		t.Errorf("reverse calls: got %d, want 2 (failed inline + recovered sweep)", len(rev.calls))
	}
}

// TestRetryPendingCreditNoteTaxReversal_RecoversMarkerlessOrphan proves the fix
// for the review finding: even when the tax_reversal_pending marker is false
// (its write failed in the compound ReverseTax-fails-AND-marker-fails window),
// the sweep still recovers — eligibility is derived structurally (issued CN, no
// reversal stamped, tax-bearing source), not gated on the marker write landing.
func TestRetryPendingCreditNoteTaxReversal_RecoversMarkerlessOrphan(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	invoices := &memInvoiceReader{
		invoices: map[string]domain.Invoice{
			"inv_tax": {
				ID: "inv_tax", TenantID: "t1", CustomerID: "cus_1",
				Status: domain.InvoicePaid, PaymentStatus: domain.PaymentSucceeded,
				Currency: "USD", SubtotalCents: 10000, TaxFacts: domain.TaxFacts{TaxAmountCents: 1000},
				TotalAmountCents: 11000, AmountPaidCents: 11000,
				TaxTransactionID: "tx_upstream_orphan",
			},
		},
	}
	rev := &fakeTaxReverser{returnID: "tx_reversal_orphan_recovered"}
	svc := NewService(store, invoices, &fakeRefunder{}, &fakeCreditGranter{})
	svc.SetTaxReverser(rev)

	// The orphan: issued, no reversal stamped, marker FALSE (its write failed).
	cn, err := store.Create(context.Background(), "t1", domain.CreditNote{
		InvoiceID: "inv_tax", CustomerID: "cus_1", Status: domain.CreditNoteIssued,
		TotalCents: 5500, TaxReversalPending: false, TaxTransactionID: "",
	})
	if err != nil {
		t.Fatalf("seed orphan: %v", err)
	}

	n, sweepErrs := svc.RetryPendingCreditNoteTaxReversal(context.Background(), 10)
	if len(sweepErrs) != 0 {
		t.Fatalf("sweep errors: %v", sweepErrs)
	}
	if n != 1 {
		t.Errorf("recovered: got %d, want 1 (structural derivation must catch the marker-less orphan)", n)
	}
	got, _ := store.Get(context.Background(), "t1", cn.ID)
	if got.TaxTransactionID != "tx_reversal_orphan_recovered" {
		t.Errorf("reversal tx id: got %q, want recovered", got.TaxTransactionID)
	}
	if len(rev.calls) != 1 {
		t.Errorf("reverse calls: got %d, want 1", len(rev.calls))
	}
}

// fakeCreditGranter satisfies the CreditGranter optional dep so
// CreateRefund doesn't blow up when refund_type is credit. The refund
// happy path doesn't call Grant, but wiring one keeps the service's
// optional-dep surface exercised.
type fakeCreditGranter struct {
	calls                                          []CreditGrantInput
	cnCalls                                        []fakeCreditGranterCNCall
	seenCNIDs                                      map[string]bool
	commitGranted, commitConsumed, commitCNRetired int64
	commitFound                                    bool
	retiredSlices, refundedGross                   []int64
}

type fakeCreditGranterCNCall struct {
	creditNoteID string
	input        CreditGrantInput
}

func (f *fakeCreditGranter) Grant(_ context.Context, _ string, in CreditGrantInput) error {
	f.calls = append(f.calls, in)
	return nil
}

// GrantForCreditNote mirrors the production dedup contract: if the
// same CN id is granted twice, the second call returns nil silently
// (representing "ErrAlreadyExists handled by GrantForCreditNote's
// fetch-and-return path"), but only ONE Grant call is recorded.
func (f *fakeCreditGranter) GrantForCreditNote(_ context.Context, _, creditNoteID string, in CreditGrantInput) error {
	if f.seenCNIDs == nil {
		f.seenCNIDs = make(map[string]bool)
	}
	if f.seenCNIDs[creditNoteID] {
		return nil
	}
	f.seenCNIDs[creditNoteID] = true
	f.cnCalls = append(f.cnCalls, fakeCreditGranterCNCall{creditNoteID: creditNoteID, input: in})
	return nil
}

// GrantForCreditNoteTx ignores the (nil) coordinator tx and delegates — the
// fake records the same way; real in-tx atomicity is covered by the PG test.
// Relief-pair stubs: unit doubles carry a configurable commit grant so the
// relief coordinator's math is unit-testable; lock semantics are proven on
// real Postgres.
func (f *fakeCreditGranter) LockCommitGrantForReliefTx(_ context.Context, _ *sql.Tx, _, _ string) (int64, int64, int64, bool, error) {
	return f.commitGranted, f.commitConsumed, f.commitCNRetired, f.commitFound, nil
}

func (f *fakeCreditGranter) RetireCommitSliceForReliefTx(_ context.Context, _ *sql.Tx, _, _, _ string, slice, refundedGross, _ int64) (int64, error) {
	f.retiredSlices = append(f.retiredSlices, slice)
	f.refundedGross = append(f.refundedGross, refundedGross)
	f.commitConsumed += slice
	f.commitCNRetired += slice
	return f.commitGranted - f.commitConsumed, nil
}

func (f *fakeCreditGranter) GrantForCreditNoteTx(ctx context.Context, _ *sql.Tx, _, creditNoteID string, in CreditGrantInput) error {
	return f.GrantForCreditNote(ctx, "", creditNoteID, in)
}

// TestRetryPendingCreditNoteTaxReversal_ZeroTaxOrphanDoesNotRewriteItself pins
// the convergence property of the sweep.
//
// The scan's second arm selects issued credit notes whose tax_transaction_id is
// empty, bounded by `updated_at > now() - 24h` so a row that cannot be resolved
// eventually ages out to manual reconcile. A zero-tax reversal succeeds but
// returns NO transaction id, so the row's tax_transaction_id stays empty and it
// requalifies on the next tick. That was survivable — until the sweep wrote
// SetTaxReversalPending(false) unconditionally, including when the marker was
// already false. The real store bumps updated_at on that UPDATE, refreshing the
// row's own eligibility window, so the sweep re-filed a reversal at Stripe every
// tick forever. Observed live: advanced=1 on every scheduler tick with zero rows
// genuinely pending.
//
// The memStore cannot model updated_at, so the assertion is the write itself:
// no marker write when there is no marker to clear.
func TestRetryPendingCreditNoteTaxReversal_ZeroTaxOrphanDoesNotRewriteItself(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	invoices := &memInvoiceReader{
		invoices: map[string]domain.Invoice{
			"inv_zero_tax": {
				ID: "inv_zero_tax", TenantID: "t1", CustomerID: "cus_1",
				Status: domain.InvoicePaid, PaymentStatus: domain.PaymentSucceeded,
				Currency: "USD", SubtotalCents: 10000,
				TaxFacts:         domain.TaxFacts{TaxAmountCents: 0},
				TotalAmountCents: 10000, AmountPaidCents: 10000,
				TaxTransactionID: "tx_upstream_zero_tax",
			},
		},
	}
	// A zero-tax reversal: succeeds, returns no transaction id to store.
	rev := &fakeTaxReverser{returnID: ""}
	svc := NewService(store, invoices, &fakeRefunder{}, &fakeCreditGranter{})
	svc.SetTaxReverser(rev)

	cn, err := store.Create(context.Background(), "t1", domain.CreditNote{
		InvoiceID: "inv_zero_tax", CustomerID: "cus_1", Status: domain.CreditNoteIssued,
		TotalCents: 1000, TaxReversalPending: false, TaxTransactionID: "",
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	before := store.setPendingCalls
	if _, sweepErrs := svc.RetryPendingCreditNoteTaxReversal(context.Background(), 10); len(sweepErrs) != 0 {
		t.Fatalf("sweep errors: %v", sweepErrs)
	}

	if got := store.setPendingCalls - before; got != 0 {
		t.Errorf("SetTaxReversalPending writes: got %d, want 0 — the marker was already false, "+
			"so this write only refreshes updated_at and keeps the row inside its own 24h window", got)
	}
	// And no provider call either: a zero-tax transaction has nothing to
	// reverse, so re-driving it is a round-trip that can never change the
	// row. Leaving this in place meant ~288 pointless calls per credit note
	// before the 24h window aged it out.
	if len(rev.calls) != 0 {
		t.Errorf("reverse calls: got %d, want 0 — a zero-tax invoice has nothing to reverse", len(rev.calls))
	}
	if got, _ := store.Get(context.Background(), "t1", cn.ID); got.TaxReversalPending {
		t.Error("marker should remain false")
	}
}

// TestRetryPendingCreditNoteTaxReversal_ClearsMarkerWhenActuallySet is the
// other half: when the marker IS set, the sweep must still clear it, or a
// genuinely pending row would never leave the fast-path arm of the scan.
func TestRetryPendingCreditNoteTaxReversal_ClearsMarkerWhenActuallySet(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	invoices := &memInvoiceReader{
		invoices: map[string]domain.Invoice{
			"inv_tax": {
				ID: "inv_tax", TenantID: "t1", CustomerID: "cus_1",
				Status: domain.InvoicePaid, PaymentStatus: domain.PaymentSucceeded,
				Currency: "USD", SubtotalCents: 10000,
				TaxFacts:         domain.TaxFacts{TaxAmountCents: 1000},
				TotalAmountCents: 11000, AmountPaidCents: 11000,
				TaxTransactionID: "tx_upstream",
			},
		},
	}
	rev := &fakeTaxReverser{returnID: "tx_reversal_ok"}
	svc := NewService(store, invoices, &fakeRefunder{}, &fakeCreditGranter{})
	svc.SetTaxReverser(rev)

	cn, err := store.Create(context.Background(), "t1", domain.CreditNote{
		InvoiceID: "inv_tax", CustomerID: "cus_1", Status: domain.CreditNoteIssued,
		TotalCents: 5500, TaxReversalPending: true, TaxTransactionID: "",
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	before := store.setPendingCalls
	if _, sweepErrs := svc.RetryPendingCreditNoteTaxReversal(context.Background(), 10); len(sweepErrs) != 0 {
		t.Fatalf("sweep errors: %v", sweepErrs)
	}
	if got := store.setPendingCalls - before; got != 1 {
		t.Errorf("SetTaxReversalPending writes: got %d, want 1 — a set marker must be cleared", got)
	}
	got, _ := store.Get(context.Background(), "t1", cn.ID)
	if got.TaxReversalPending {
		t.Error("marker not cleared")
	}
	if got.TaxTransactionID != "tx_reversal_ok" {
		t.Errorf("reversal tx id: got %q, want tx_reversal_ok", got.TaxTransactionID)
	}
}
