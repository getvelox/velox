package subscription

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
)

// fakeCNIssuer records CreateAndIssueAdjustment calls so the downgrade-clawback
// tests can assert the GROSS amount, the source invoice id, and the reason
// routed to the tax-reversing credit-note primitive (ADR-048).
type fakeCNIssuer struct {
	calls []cnIssueCall
	err   error
	// credited[invoiceID] = cents already credited against that invoice (prior
	// non-voided credit notes), so headroom-spill clawback tests can shrink one
	// funding invoice's remaining room. Default 0 → full headroom.
	credited map[string]int64
}

func (f *fakeCNIssuer) CreditedCents(_ context.Context, _, invoiceID string) (int64, error) {
	return f.credited[invoiceID], nil
}

type cnIssueCall struct {
	invoiceID string
	gross     int64
	reason    string
	desc      string
}

func (f *fakeCNIssuer) CreateAndIssueAdjustment(_ context.Context, _, invoiceID string, gross int64, reason, desc string) (domain.CreditNote, error) {
	f.calls = append(f.calls, cnIssueCall{invoiceID: invoiceID, gross: gross, reason: reason, desc: desc})
	if f.err != nil {
		return domain.CreditNote{}, f.err
	}
	return domain.CreditNote{ID: "vlx_cn_test", InvoiceID: invoiceID}, nil
}

// CreateAdjustmentDraftTx records like CreateAndIssueAdjustment so f.calls
// assertions hold on both the atomic (draft-in-tx) and non-atomic paths.
func (f *fakeCNIssuer) CreateAdjustmentDraftTx(_ context.Context, _ *sql.Tx, _, invoiceID string, gross int64, reason, desc string) (domain.CreditNote, error) {
	f.calls = append(f.calls, cnIssueCall{invoiceID: invoiceID, gross: gross, reason: reason, desc: desc})
	if f.err != nil {
		return domain.CreditNote{}, f.err
	}
	return domain.CreditNote{ID: "vlx_cn_draft_test", InvoiceID: invoiceID, Status: domain.CreditNoteDraft, IssuePending: true}, nil
}

func (f *fakeCNIssuer) Issue(_ context.Context, _, id string) (domain.CreditNote, error) {
	if f.err != nil {
		return domain.CreditNote{}, f.err
	}
	return domain.CreditNote{ID: id, Status: domain.CreditNoteIssued}, nil
}

// TestUpdateItem_Downgrade_RoutesGrossTaxReversingCreditNote locks ADR-048
// Phase B: a mid-cycle plan DOWNGRADE on a PAID, taxed in_advance prebill must
// claw back the GROSS the customer paid for the unused slice (net + the
// proportional tax) via the credit-note primitive — which reverses the
// proportional output tax — NOT the bare net ledger grant that dropped the tax.
func TestUpdateItem_Downgrade_RoutesGrossTaxReversingCreditNote(t *testing.T) {
	ctx := clock.WithEffectiveNow(context.Background(), proNow)
	tenantID := "t1"

	store := newMemStore()
	subID, itemID := seedSubWithItemAt(t, store, tenantID, "cus_1", "plan_pro", proPeriodStart, proPeriodEnd)
	svc := NewService(store, nil)

	// Downgrade Pro ($60) → Basic ($20). 15/30 remain → net credit = $40 × 15/30 = $20 = 2000.
	plans := &plansMock{plans: map[string]domain.Plan{
		"plan_pro":   {ID: "plan_pro", Name: "Pro", BaseAmountCents: 6000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
		"plan_basic": {ID: "plan_basic", Name: "Basic", BaseAmountCents: 2000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
	}}
	// PAID source prebill: net 6000 + 10% tax 600 = 6600 gross.
	invoices := &invoicesMock{
		sourceInvoice: domain.Invoice{
			ID: "src_inv", PaymentStatus: domain.PaymentSucceeded,
			SubtotalCents: 6000, TaxFacts: domain.TaxFacts{TaxAmountCents: 600}, TotalAmountCents: 6600,
		},
	}
	credits := &creditsMock{}
	cn := &fakeCNIssuer{}

	h := NewHandler(svc)
	h.SetProrationDeps(plans, invoices, credits)
	h.SetCreditNoteIssuer(cn)

	body, _ := json.Marshal(UpdateItemInput{NewPlanID: "plan_basic", Immediate: true})
	req := updateItemURL(context.WithValue(ctx, auth.TestTenantIDKey(), tenantID), subID, itemID, body)

	rr := httptest.NewRecorder()
	h.updateItem(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200. body=%s", rr.Code, rr.Body.String())
	}
	// net 2000 grossed up by the source invoice's 6600/6000 ratio = 2200.
	const wantGross = int64(2200)
	if len(cn.calls) != 1 {
		t.Fatalf("CreateAndIssueAdjustment calls: got %d, want 1", len(cn.calls))
	}
	call := cn.calls[0]
	if call.invoiceID != "src_inv" {
		t.Errorf("CN source invoice: got %q, want %q", call.invoiceID, "src_inv")
	}
	if call.gross != wantGross {
		t.Errorf("CN gross: got %d, want %d (net 2000 + 10%% tax slice)", call.gross, wantGross)
	}
	if call.reason != "subscription_downgrade" {
		t.Errorf("CN reason: got %q, want %q", call.reason, "subscription_downgrade")
	}
	if call.desc == "" {
		t.Error("CN description (memo) is empty")
	}
	// The bare net ledger grant must NOT also fire — that would double-credit.
	if len(credits.grantCalls) != 0 {
		t.Errorf("GrantProration calls: got %d, want 0 — the CN replaces the bare net grant", len(credits.grantCalls))
	}

	var resp ItemChangeResult
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Proration == nil || resp.Proration.Type != "credit" {
		t.Fatalf("proration: got %+v, want type=credit", resp.Proration)
	}
	if resp.Proration.AmountCents != wantGross {
		t.Errorf("proration.amount_cents: got %d, want %d (the gross credited)", resp.Proration.AmountCents, wantGross)
	}
}

// TestUpdateItem_Downgrade_LIFOAcrossFundingInvoices is the Scenario-6
// regression: after a mid-period UPGRADE split the period across two invoices,
// a plan DOWNGRADE claws back the removed value LIFO — against the most-recent
// (upgrade) invoice, reversing ITS tax — leaving the still-active base invoice
// untouched, instead of issuing one credit note against the single
// FindBaseInvoiceForPeriod result.
func TestUpdateItem_Downgrade_LIFOAcrossFundingInvoices(t *testing.T) {
	ctx := clock.WithEffectiveNow(context.Background(), proNow)
	tenantID := "t1"

	store := newMemStore()
	subID, itemID := seedSubWithItemAt(t, store, tenantID, "cus_1", "plan_pro", proPeriodStart, proPeriodEnd)
	svc := NewService(store, nil)

	// Downgrade Pro ($60) → Basic ($20). 15/30 remain → net credit = $20 = 2000.
	plans := &plansMock{plans: map[string]domain.Plan{
		"plan_pro":   {ID: "plan_pro", Name: "Pro", BaseAmountCents: 6000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
		"plan_basic": {ID: "plan_basic", Name: "Basic", BaseAmountCents: 2000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
	}}
	baseInv := domain.Invoice{
		ID: "base_inv", PaymentStatus: domain.PaymentSucceeded,
		SubtotalCents: 2000, TaxFacts: domain.TaxFacts{TaxAmountCents: 200}, TotalAmountCents: 2200,
		CreatedAt: proPeriodStart,
	}
	upInv := domain.Invoice{
		ID: "up_inv", PaymentStatus: domain.PaymentSucceeded,
		SubtotalCents: 4000, TaxFacts: domain.TaxFacts{TaxAmountCents: 400}, TotalAmountCents: 4400,
		CreatedAt: proPeriodStart.AddDate(0, 0, 5), // newer → LIFO targets this first
	}
	invoices := &invoicesMock{
		sourceInvoice:   baseInv, // FindBaseInvoiceForPeriod (paid-check gate)
		fundingInvoices: []domain.Invoice{baseInv, upInv},
	}
	credits := &creditsMock{}
	cn := &fakeCNIssuer{}

	h := NewHandler(svc)
	h.SetProrationDeps(plans, invoices, credits)
	h.SetCreditNoteIssuer(cn)

	body, _ := json.Marshal(UpdateItemInput{NewPlanID: "plan_basic", Immediate: true})
	req := updateItemURL(context.WithValue(ctx, auth.TestTenantIDKey(), tenantID), subID, itemID, body)
	rr := httptest.NewRecorder()
	h.updateItem(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200. body=%s", rr.Code, rr.Body.String())
	}
	// net credit 2000 fits entirely in the upgrade invoice's headroom → ONE CN
	// against up_inv, grossed by ITS 4400/4000 ratio = 2200. Base untouched.
	if len(cn.calls) != 1 {
		t.Fatalf("CreateAndIssueAdjustment calls: got %d, want 1: %+v", len(cn.calls), cn.calls)
	}
	if cn.calls[0].invoiceID != "up_inv" {
		t.Errorf("LIFO must target the newest (upgrade) invoice, got %q (want up_inv — NOT the base)", cn.calls[0].invoiceID)
	}
	if cn.calls[0].gross != 2200 {
		t.Errorf("CN gross: got %d, want 2200 (net 2000 × upgrade-invoice 4400/4000 ratio)", cn.calls[0].gross)
	}
}

// TestUpdateItem_Downgrade_LIFOTiebreak_SameInstantFunding pins the tie case
// found live (FLOW B17c walk, 2026-07-21): a clock catchup stamps every
// invoice it generates with the SAME simulated instant (and a same-second
// subscribe+upgrade does it on wall time), so LIFO-by-CreatedAt alone was a
// coin flip resolved by the funding query's oldest-first order — the clawback
// landed on the day-1 invoice, the exact opposite of LIFO. On equal
// timestamps the subscription_update invoice IS the later price level.
func TestUpdateItem_Downgrade_LIFOTiebreak_SameInstantFunding(t *testing.T) {
	ctx := clock.WithEffectiveNow(context.Background(), proNow)
	tenantID := "t1"

	store := newMemStore()
	subID, itemID := seedSubWithItemAt(t, store, tenantID, "cus_1", "plan_pro", proPeriodStart, proPeriodEnd)
	svc := NewService(store, nil)

	plans := &plansMock{plans: map[string]domain.Plan{
		"plan_pro":   {ID: "plan_pro", Name: "Pro", BaseAmountCents: 6000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
		"plan_basic": {ID: "plan_basic", Name: "Basic", BaseAmountCents: 2000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
	}}
	// BOTH funding invoices born at the same instant; the query returns
	// oldest-reason-first (create, then update) — the order a stable sort
	// preserves on a timestamp tie.
	baseInv := domain.Invoice{
		ID: "base_inv", PaymentStatus: domain.PaymentSucceeded,
		BillingReason: domain.BillingReasonSubscriptionCreate,
		SubtotalCents: 2000, TaxFacts: domain.TaxFacts{TaxAmountCents: 200}, TotalAmountCents: 2200,
		CreatedAt: proPeriodStart,
	}
	upInv := domain.Invoice{
		ID: "up_inv", PaymentStatus: domain.PaymentSucceeded,
		BillingReason: domain.BillingReasonSubscriptionUpdate,
		SubtotalCents: 4000, TaxFacts: domain.TaxFacts{TaxAmountCents: 400}, TotalAmountCents: 4400,
		CreatedAt: proPeriodStart, // SAME instant as the base invoice
	}
	invoices := &invoicesMock{
		sourceInvoice:   baseInv,
		fundingInvoices: []domain.Invoice{baseInv, upInv},
	}
	credits := &creditsMock{}
	cn := &fakeCNIssuer{}

	h := NewHandler(svc)
	h.SetProrationDeps(plans, invoices, credits)
	h.SetCreditNoteIssuer(cn)

	body, _ := json.Marshal(UpdateItemInput{NewPlanID: "plan_basic", Immediate: true})
	req := updateItemURL(context.WithValue(ctx, auth.TestTenantIDKey(), tenantID), subID, itemID, body)
	rr := httptest.NewRecorder()
	h.updateItem(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200. body=%s", rr.Code, rr.Body.String())
	}
	if len(cn.calls) != 1 {
		t.Fatalf("CreateAndIssueAdjustment calls: got %d, want 1: %+v", len(cn.calls), cn.calls)
	}
	if cn.calls[0].invoiceID != "up_inv" {
		t.Errorf("same-instant LIFO tiebreak must target the subscription_update invoice, got %q", cn.calls[0].invoiceID)
	}
}

// TestUpdateItem_Downgrade_FallsBackToNetGrantWhenIssuerUnwired proves the
// downgrade path still works (legacy net ledger grant, no tax reversal) when the
// CN issuer isn't wired — narrow setups / tests. Production always wires it.
func TestUpdateItem_Downgrade_FallsBackToNetGrantWhenIssuerUnwired(t *testing.T) {
	ctx := clock.WithEffectiveNow(context.Background(), proNow)
	tenantID := "t1"

	store := newMemStore()
	subID, itemID := seedSubWithItemAt(t, store, tenantID, "cus_1", "plan_pro", proPeriodStart, proPeriodEnd)
	svc := NewService(store, nil)

	plans := &plansMock{plans: map[string]domain.Plan{
		"plan_pro":   {ID: "plan_pro", Name: "Pro", BaseAmountCents: 6000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
		"plan_basic": {ID: "plan_basic", Name: "Basic", BaseAmountCents: 2000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
	}}
	invoices := &invoicesMock{
		sourceInvoice: domain.Invoice{
			ID: "src_inv", PaymentStatus: domain.PaymentSucceeded,
			SubtotalCents: 6000, TaxFacts: domain.TaxFacts{TaxAmountCents: 600}, TotalAmountCents: 6600,
		},
	}
	credits := &creditsMock{}

	h := NewHandler(svc)
	h.SetProrationDeps(plans, invoices, credits) // no SetCreditNoteIssuer

	body, _ := json.Marshal(UpdateItemInput{NewPlanID: "plan_basic", Immediate: true})
	req := updateItemURL(context.WithValue(ctx, auth.TestTenantIDKey(), tenantID), subID, itemID, body)

	rr := httptest.NewRecorder()
	h.updateItem(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200. body=%s", rr.Code, rr.Body.String())
	}
	if len(credits.grantCalls) != 1 {
		t.Fatalf("GrantProration calls: got %d, want 1 (fallback net grant)", len(credits.grantCalls))
	}
	// Fallback credits the bare NET (pre-fix amount), no tax slice.
	if got := credits.grantCalls[0].AmountCents; got != 2000 {
		t.Errorf("fallback grant amount: got %d, want 2000 (net, no tax)", got)
	}
}

// TestUpdateItem_QuantityDecrease_RoutesGrossCreditNote confirms a mid-cycle
// quantity DECREASE on a paid taxed prebill also routes through the CN with the
// quantity-decrease reason.
func TestUpdateItem_QuantityDecrease_RoutesGrossCreditNote(t *testing.T) {
	ctx := clock.WithEffectiveNow(context.Background(), proNow)
	tenantID := "t1"

	store := newMemStore()
	subID, itemID := seedSubWithItemAt(t, store, tenantID, "cus_1", "plan_seats", proPeriodStart, proPeriodEnd)
	svc := NewService(store, nil)

	// Seed starts at qty 1; bump to 3 first so we can then DECREASE to 1.
	plans := &plansMock{plans: map[string]domain.Plan{
		"plan_seats": {ID: "plan_seats", Name: "Seat", BaseAmountCents: 1000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
	}}
	if _, err := svc.UpdateItem(ctx, tenantID, subID, itemID, UpdateItemInput{Quantity: int64Ptr(3)}); err != nil {
		t.Fatalf("seed qty=3: %v", err)
	}
	// Source prebill for 3 seats: net 3000 + 10% tax 300 = 3300 gross.
	invoices := &invoicesMock{
		sourceInvoice: domain.Invoice{
			ID: "src_inv", PaymentStatus: domain.PaymentSucceeded,
			SubtotalCents: 3000, TaxFacts: domain.TaxFacts{TaxAmountCents: 300}, TotalAmountCents: 3300,
		},
	}
	credits := &creditsMock{}
	cn := &fakeCNIssuer{}

	h := NewHandler(svc)
	h.SetProrationDeps(plans, invoices, credits)
	h.SetCreditNoteIssuer(cn)

	// Decrease 3 → 1 seats: delta -2 × $10 = -2000/full period, 15/30 → net 1000.
	newQty := int64(1)
	body, _ := json.Marshal(UpdateItemInput{Quantity: &newQty})
	req := updateItemURL(context.WithValue(ctx, auth.TestTenantIDKey(), tenantID), subID, itemID, body)

	rr := httptest.NewRecorder()
	h.updateItem(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200. body=%s", rr.Code, rr.Body.String())
	}
	if len(cn.calls) != 1 {
		t.Fatalf("CreateAndIssueAdjustment calls: got %d, want 1", len(cn.calls))
	}
	// net 1000 grossed up by 3300/3000 = 1100.
	if got := cn.calls[0].gross; got != 1100 {
		t.Errorf("CN gross: got %d, want 1100 (net 1000 + 10%% tax slice)", got)
	}
	if got := cn.calls[0].reason; got != "subscription_quantity_decrease" {
		t.Errorf("CN reason: got %q, want %q", got, "subscription_quantity_decrease")
	}
	if len(credits.grantCalls) != 0 {
		t.Errorf("GrantProration calls: got %d, want 0", len(credits.grantCalls))
	}
}

func int64Ptr(v int64) *int64 { return &v }

// TestUpdateItem_Downgrade_InFlightSource_DraftsInsteadOfDroppingCredit is the
// control/treatment pair that found the 2026-07-30 silent money-drop while
// walking FLOW C2.
//
// Setup is identical in both cases — a PAID in_advance prebill (amount_due = 0)
// downgraded mid-cycle — and the ONLY difference is the source's payment_status.
// Pre-fix the treatment produced NO credit note at all: `!= PaymentSucceeded`
// routed the in-flight source into the unpaid amount_due-reduction branch, which
// computed min(grossCredit, amount_due) == 0 on an already-paid source and
// returned nil silently. The item change committed, the customer kept the
// cheaper plan, and the credit they were owed existed nowhere.
//
// Post-fix both produce the clawback; deciding WHEN to issue it belongs to
// creditnote.Issue (the single chokepoint, which defers on in-flight sources and
// picks the paid/unpaid channel once the source resolves — ADR-059).
func TestUpdateItem_Downgrade_InFlightSource_DraftsInsteadOfDroppingCredit(t *testing.T) {
	// net 2000 credit grossed up by the source's 6600/6000 ratio.
	const wantGross = int64(2200)

	run := func(t *testing.T, status domain.InvoicePaymentStatus) []cnIssueCall {
		t.Helper()
		ctx := clock.WithEffectiveNow(context.Background(), proNow)
		tenantID := "t1"

		store := newMemStore()
		subID, itemID := seedSubWithItemAt(t, store, tenantID, "cus_1", "plan_pro", proPeriodStart, proPeriodEnd)
		svc := NewService(store, nil)

		plans := &plansMock{plans: map[string]domain.Plan{
			"plan_pro":   {ID: "plan_pro", Name: "Pro", BaseAmountCents: 6000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
			"plan_basic": {ID: "plan_basic", Name: "Basic", BaseAmountCents: 2000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
		}}
		// The invoice is PAID — amount_due is 0 — and only its payment_status
		// varies. That is what makes the unpaid branch a silent no-op.
		invoices := &invoicesMock{
			sourceInvoice: domain.Invoice{
				ID: "src_inv", Status: domain.InvoicePaid, PaymentStatus: status,
				SubtotalCents: 6000, TaxFacts: domain.TaxFacts{TaxAmountCents: 600},
				TotalAmountCents: 6600, AmountDueCents: 0,
			},
		}
		cn := &fakeCNIssuer{}

		h := NewHandler(svc)
		h.SetProrationDeps(plans, invoices, &creditsMock{})
		h.SetCreditNoteIssuer(cn)

		body, _ := json.Marshal(UpdateItemInput{NewPlanID: "plan_basic", Immediate: true})
		req := updateItemURL(context.WithValue(ctx, auth.TestTenantIDKey(), tenantID), subID, itemID, body)
		rr := httptest.NewRecorder()
		h.updateItem(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status: got %d, want 200. body=%s", rr.Code, rr.Body.String())
		}
		return cn.calls
	}

	t.Run("control: settled source credits the customer", func(t *testing.T) {
		calls := run(t, domain.PaymentSucceeded)
		if len(calls) != 1 || calls[0].gross != wantGross {
			t.Fatalf("control: got %+v, want exactly one clawback of %d", calls, wantGross)
		}
	})

	// The treatment. Both in-flight statuses must behave identically — `unknown`
	// is as in-flight as `processing` (domain.IsInFlight), and an ambiguous
	// charge is precisely when under-crediting would be hardest to notice.
	for _, status := range []domain.InvoicePaymentStatus{domain.PaymentProcessing, domain.PaymentUnknown} {
		t.Run("treatment: in-flight source ("+string(status)+") still records the owed credit", func(t *testing.T) {
			calls := run(t, status)
			if len(calls) != 1 {
				t.Fatalf("in-flight source produced %d clawbacks, want 1 — the credit the customer is owed must be RECORDED even though issuing it waits for the source to settle (pre-fix this was 0: silently dropped)", len(calls))
			}
			if calls[0].gross != wantGross {
				t.Errorf("in-flight clawback gross: got %d, want %d — must match the settled-source control exactly; a different amount means the funding cap fell back to amount_due (0 on a paid source)", calls[0].gross, wantGross)
			}
			if calls[0].invoiceID != "src_inv" {
				t.Errorf("clawback source: got %q, want %q", calls[0].invoiceID, "src_inv")
			}
		})
	}
}
