package subscription

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/tax"
)

// ---------------------------------------------------------------------------
// Mocks for handler-level tests. Kept in this file because the service-level
// mocks in service_test.go don't cover the proration dependency set.
// ---------------------------------------------------------------------------

type plansMock struct{ plans map[string]domain.Plan }

func (m *plansMock) GetPlan(_ context.Context, _, id string) (domain.Plan, error) {
	p, ok := m.plans[id]
	if !ok {
		return domain.Plan{}, errors.New("plan not found")
	}
	return p, nil
}

type invoicesMock struct {
	createInvoiceErr    error
	setAutoChargeErr    error
	nextNumberErr       error
	nextNumberCalls     int
	createdInvoices     []domain.Invoice
	createdLineItems    [][]domain.InvoiceLineItem
	lookupInvoice       domain.Invoice
	lookupInvoiceErr    error
	existingCreateCalls int
	// sourceInvoice is what FindBaseInvoiceForPeriod returns. Tests
	// that exercise the paid-check gate pre-seed this with a paid
	// in_advance invoice; tests that don't care leave it zero — the
	// gate will skip emission (matching production-safe default).
	sourceInvoice    domain.Invoice
	sourceInvoiceErr error
	// fundingInvoices is what FindFundingInvoicesForPeriod returns (the
	// downgrade clawback fan-out). When unset, it defaults to [sourceInvoice]
	// so single-source clawback tests keep working.
	fundingInvoices []domain.Invoice
	// autoChargeEnrolled records invoice IDs passed to SetAutoChargePending(true)
	// so tests can assert a proration CHARGE invoice was enrolled for collection.
	autoChargeEnrolled []string
}

func (m *invoicesMock) SetAutoChargePending(_ context.Context, _, id string, pending bool) error {
	if pending {
		m.autoChargeEnrolled = append(m.autoChargeEnrolled, id)
	}
	return nil
}

func (m *invoicesMock) SetAutoChargePendingTx(_ context.Context, _ *sql.Tx, _, id string, pending bool) error {
	if m.setAutoChargeErr != nil {
		return m.setAutoChargeErr
	}
	if pending {
		m.autoChargeEnrolled = append(m.autoChargeEnrolled, id)
	}
	return nil
}

func (m *invoicesMock) createInvoice(tenantID string, inv domain.Invoice, items []domain.InvoiceLineItem) (domain.Invoice, error) {
	if m.createInvoiceErr != nil {
		return domain.Invoice{}, m.createInvoiceErr
	}
	inv.ID = fmt.Sprintf("vlx_inv_test_%d", len(m.createdInvoices)+1)
	inv.TenantID = tenantID
	m.createdInvoices = append(m.createdInvoices, inv)
	m.createdLineItems = append(m.createdLineItems, items)
	return inv, nil
}

// CreateInvoiceWithLineItemsAudited runs emit as the store does, after the
// insert; an emit error discards the invoice, as the store's rollback would.
// The fake has no transaction, so emit gets a nil tx.
func (m *invoicesMock) CreateInvoiceWithLineItemsAudited(_ context.Context, tenantID string, inv domain.Invoice, items []domain.InvoiceLineItem, emit func(tx *sql.Tx, out domain.Invoice) error) (domain.Invoice, error) {
	out, err := m.createInvoice(tenantID, inv, items)
	if err != nil || emit == nil {
		return out, err
	}
	if err := emit(nil, out); err != nil {
		m.createdInvoices = m.createdInvoices[:len(m.createdInvoices)-1]
		m.createdLineItems = m.createdLineItems[:len(m.createdLineItems)-1]
		return domain.Invoice{}, err
	}
	return out, nil
}

func (m *invoicesMock) GetByProrationSource(_ context.Context, _, _, _ string, _ domain.ItemChangeType, _ time.Time) (domain.Invoice, error) {
	m.existingCreateCalls++
	if m.lookupInvoiceErr != nil {
		return domain.Invoice{}, m.lookupInvoiceErr
	}
	return m.lookupInvoice, nil
}

func (m *invoicesMock) NextInvoiceNumber(_ context.Context, _ string) (string, error) {
	m.nextNumberCalls++
	if m.nextNumberErr != nil {
		return "", m.nextNumberErr
	}
	return "VLX-000042", nil
}

func (m *invoicesMock) FindBaseInvoiceForPeriod(_ context.Context, _, _ string, _ time.Time) (domain.Invoice, error) {
	if m.sourceInvoiceErr != nil {
		return domain.Invoice{}, m.sourceInvoiceErr
	}
	return m.sourceInvoice, nil
}

func (m *invoicesMock) FindFundingInvoicesForPeriod(_ context.Context, _, _ string, _, _ time.Time) ([]domain.Invoice, error) {
	if m.sourceInvoiceErr != nil {
		return nil, m.sourceInvoiceErr
	}
	if len(m.fundingInvoices) > 0 {
		return m.fundingInvoices, nil
	}
	if m.sourceInvoice.ID != "" {
		return []domain.Invoice{m.sourceInvoice}, nil
	}
	return nil, errs.ErrNotFound
}

// CreateInvoiceWithLineItemsTx + NextInvoiceNumberTx mirror their
// non-Tx counterparts for tx-aware callers. Fakes ignore the tx; the
// tests exercise business logic, not transactional atomicity (a
// dedicated integration test covers that path against a real DB).
func (m *invoicesMock) CreateInvoiceWithLineItemsTx(_ context.Context, _ *sql.Tx, tenantID string, inv domain.Invoice, items []domain.InvoiceLineItem) (domain.Invoice, error) {
	return m.createInvoice(tenantID, inv, items)
}

func (m *invoicesMock) NextInvoiceNumberTx(ctx context.Context, _ *sql.Tx, tenantID string) (string, error) {
	return m.NextInvoiceNumber(ctx, tenantID)
}

// Deterministic reference window for proration-AMOUNT tests: a full 30-day
// calendar month (June 2026) with "now" at the midpoint, so day-counts and the
// full-cycle proration denominator are exact regardless of wall-clock. (The old
// now±15d seed silently depended on the current month's length — once proration
// divides by the full cycle, that made the amounts vary 28–31 days.) Pair these
// with a ctx bound via clock.WithEffectiveNow(proNow).
var (
	proPeriodStart = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	proPeriodEnd   = time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)  // exact 30-day month
	proNow         = time.Date(2026, 6, 16, 0, 0, 0, 0, time.UTC) // 15 of 30 days remain
)

// seedSubWithItem creates an active subscription on `initialPlan` with a
// 30-day period centered on now. Use for behavioral tests that don't assert an
// exact proration amount; for amount assertions use seedSubWithItemAt with the
// deterministic pro* window above.
func seedSubWithItem(t *testing.T, store *memStore, tenantID, custID, initialPlan string) (string, string) {
	t.Helper()
	now := time.Now().UTC()
	return seedSubWithItemAt(t, store, tenantID, custID, initialPlan, now.Add(-15*24*time.Hour), now.Add(15*24*time.Hour))
}

// seedSubWithItemAt is seedSubWithItem with an explicit billing period, for
// deterministic proration-amount assertions. Pair with a ctx bound to a fixed
// instant inside [periodStart, periodEnd] via clock.WithEffectiveNow.
func seedSubWithItemAt(t *testing.T, store *memStore, tenantID, custID, initialPlan string, periodStart, periodEnd time.Time) (string, string) {
	t.Helper()
	sub, err := store.Create(context.Background(), tenantID, domain.Subscription{
		Code:        fmt.Sprintf("sub-%d", len(store.subs)+1),
		DisplayName: "Test Sub", CustomerID: custID,
		Status:                    domain.SubscriptionActive,
		BillingTime:               domain.BillingTimeCalendar,
		CurrentBillingPeriodStart: &periodStart,
		CurrentBillingPeriodEnd:   &periodEnd,
		Items:                     []domain.SubscriptionItem{{PlanID: initialPlan, Quantity: 1}},
	})
	if err != nil {
		t.Fatalf("seed subscription: %v", err)
	}
	return sub.ID, sub.Items[0].ID
}

// updateItemURL builds the route URL for the per-item PATCH handler and wires
// the chi context with {id, itemID} params so chi.URLParam resolves them.
func updateItemURL(ctx context.Context, subID, itemID string, body []byte) *http.Request {
	url := fmt.Sprintf("/subscriptions/%s/items/%s", subID, itemID)
	req := httptest.NewRequest(http.MethodPatch, url, bytes.NewReader(body))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", subID)
	rctx.URLParams.Add("itemID", itemID)
	return req.WithContext(context.WithValue(ctx, chi.RouteCtxKey, rctx))
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestUpdateItem_ProrationFailureSurfacesAs500 locks in the fix for a silent
// data-loss bug: if proration generation fails after the plan change has
// already committed, the error must surface as a 500 with the distinct code
// "proration_failed". Prior behaviour was a 200 OK, leaving the customer on
// the new plan but never billed (or credited) for the mid-cycle swap.
func TestUpdateItem_ProrationFailureSurfacesAs500(t *testing.T) {
	ctx := context.Background()
	tenantID := "t1"

	store := newMemStore()
	subID, itemID := seedSubWithItem(t, store, tenantID, "cus_1", "plan_old")
	svc := NewService(store, nil)

	plans := &plansMock{plans: map[string]domain.Plan{
		"plan_old": {ID: "plan_old", Name: "Basic", BaseAmountCents: 1000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
		"plan_new": {ID: "plan_new", Name: "Pro", BaseAmountCents: 3000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
	}}
	// NextInvoiceNumber fails → proration generation fails after the plan
	// change has already committed.
	invoices := &invoicesMock{
		nextNumberErr: errors.New("sequence unavailable"),
		sourceInvoice: domain.Invoice{ID: "src_inv", PaymentStatus: domain.PaymentSucceeded},
	}

	h := NewHandler(svc)
	h.SetProrationDeps(plans, invoices)

	body, _ := json.Marshal(UpdateItemInput{NewPlanID: "plan_new", Immediate: true})
	req := updateItemURL(context.WithValue(ctx, auth.TestTenantIDKey(), tenantID), subID, itemID, body)

	rr := httptest.NewRecorder()
	h.updateItem(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500. body=%s", rr.Code, rr.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	errObj, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("missing error object in response: %s", rr.Body.String())
	}
	if code, _ := errObj["code"].(string); code != "proration_failed" {
		t.Errorf("error.code: got %q, want %q", code, "proration_failed")
	}

	// The plan change itself must still have committed — that's the whole
	// point of surfacing the error: state is divergent, and the client has to
	// know so they can reconcile.
	storedItem, err := store.GetItem(ctx, tenantID, itemID)
	if err != nil {
		t.Fatalf("reload item: %v", err)
	}
	if storedItem.PlanID != "plan_new" {
		t.Errorf("item plan_id: got %q, want %q (plan change should commit even when proration fails)",
			storedItem.PlanID, "plan_new")
	}
}

// TestUpdateItem_ProrationDedup_UpgradeReturnsExisting exercises the
// idempotent-retry path: if a proration invoice for the same (subscription,
// item, plan_changed_at) already exists, the store returns errs.ErrAlreadyExists
// and the handler re-fetches the existing invoice rather than creating a duplicate.
//
// Without this, an operator-triggered retry (or any worker that recomputes
// proration from subscription state) would silently double-bill on the
// upgrade side, or grant double credits on the downgrade side.
func TestUpdateItem_ProrationDedup_UpgradeReturnsExisting(t *testing.T) {
	ctx := context.Background()
	tenantID := "t1"

	store := newMemStore()
	subID, itemID := seedSubWithItem(t, store, tenantID, "cus_1", "plan_old")
	svc := NewService(store, nil)

	plans := &plansMock{plans: map[string]domain.Plan{
		"plan_old": {ID: "plan_old", Name: "Basic", BaseAmountCents: 1000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
		"plan_new": {ID: "plan_new", Name: "Pro", BaseAmountCents: 3000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
	}}

	existingAt := time.Now().UTC().Add(-time.Minute)
	existingInvoice := domain.Invoice{
		ID:                       "vlx_inv_prior",
		TenantID:                 tenantID,
		SubscriptionID:           subID,
		InvoiceNumber:            "VLX-000001",
		Status:                   domain.InvoiceFinalized,
		SourcePlanChangedAt:      &existingAt,
		SourceSubscriptionItemID: itemID,
	}
	// CreateInvoiceWithLineItems returns the proration-source-taken
	// constraint-coded error to emulate idx_invoices_proration_dedup
	// firing. The handler must then call GetByProrationSource and
	// surface that result instead of bubbling up the error. Other
	// unique-violation codes (billing_period_taken, etc.) intentionally
	// do NOT trigger the dedup-lookup branch — they're distinct bugs.
	// See ADR-030 cross-flow audit 2026-05-28.
	invoices := &invoicesMock{
		createInvoiceErr: errs.AlreadyExists("proration_source",
			"proration invoice already exists for this item change").WithCode("invoice_proration_source_taken"),
		lookupInvoice: existingInvoice,
		sourceInvoice: domain.Invoice{ID: "src_inv", PaymentStatus: domain.PaymentSucceeded},
	}

	h := NewHandler(svc)
	h.SetProrationDeps(plans, invoices)

	body, _ := json.Marshal(UpdateItemInput{NewPlanID: "plan_new", Immediate: true})
	req := updateItemURL(context.WithValue(ctx, auth.TestTenantIDKey(), tenantID), subID, itemID, body)

	rr := httptest.NewRecorder()
	h.updateItem(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (dedup should succeed as idempotent retry), body=%s",
			rr.Code, rr.Body.String())
	}
	if invoices.existingCreateCalls != 1 {
		t.Errorf("GetByProrationSource call count: got %d, want 1 (must be called on ErrAlreadyExists)",
			invoices.existingCreateCalls)
	}
	if len(invoices.createdInvoices) != 0 {
		t.Errorf("createdInvoices: got %d, want 0 (dedup must not create a new invoice)", len(invoices.createdInvoices))
	}

	var resp ItemChangeResult
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Proration == nil {
		t.Fatal("response missing proration detail")
	}
	if resp.Proration.InvoiceID != existingInvoice.ID {
		t.Errorf("proration.invoice_id: got %q, want %q (existing id)",
			resp.Proration.InvoiceID, existingInvoice.ID)
	}
	if resp.Proration.Type != "invoice" {
		t.Errorf("proration.type: got %q, want %q", resp.Proration.Type, "invoice")
	}
}

// prorationTaxApplierMock records calls and returns a canned result. Used to
// verify the proration handler threads tax fields onto the invoice when an
// applier is wired.
type prorationTaxApplierMock struct {
	calls           int
	result          ProrationTaxResult
	err             error
	commitCalls     int
	commitInvoiceID string
	commitCalcID    string
	// receivedLineCount / receivedNegative capture what the handler passed to
	// the provider at call time — the ADR-048 Phase C two-line split must run
	// AFTER tax, so the provider always sees a SINGLE positive net line (never
	// a negative credit line, which Stripe Tax rejects).
	receivedLineCount int
	receivedNegative  bool
}

func (m *prorationTaxApplierMock) CommitTax(_ context.Context, _, invoiceID, calculationID string) error {
	m.commitCalls++
	m.commitInvoiceID = invoiceID
	m.commitCalcID = calculationID
	return nil
}

func (m *prorationTaxApplierMock) ApplyTaxToLineItems(_ context.Context, _, _, _ string, subtotal, discount int64, lineItems []domain.InvoiceLineItem) (ProrationTaxResult, error) {
	m.calls++
	m.receivedLineCount = len(lineItems)
	for _, li := range lineItems {
		if li.AmountCents < 0 {
			m.receivedNegative = true
		}
	}
	if m.err != nil {
		return ProrationTaxResult{}, m.err
	}
	// Mutate line items like the real engine: split invoice tax into first line.
	if len(lineItems) > 0 && m.result.TaxAmountCents > 0 {
		lineItems[0].TaxRate = m.result.TaxRate
		lineItems[0].TaxAmountCents = m.result.TaxAmountCents
		lineItems[0].TotalAmountCents = lineItems[0].AmountCents + m.result.TaxAmountCents
	}
	// Mirror the real engine adapter, which seeds app.SubtotalCents = subtotal
	// (the net it was asked to tax) on the exclusive path. Tests that want the
	// inclusive carve-out can set result.SubtotalCents explicitly.
	r := m.result
	if r.SubtotalCents == 0 {
		r.SubtotalCents = subtotal
	}
	r.DiscountCents = discount
	return r, nil
}

// TestUpdateItem_ProrationAppliesTax locks in COR-2: proration invoices must
// carry tax. Prior behaviour was tax-free proration even when the customer
// had a configured rate.
func TestUpdateItem_ProrationAppliesTax(t *testing.T) {
	ctx := context.Background()
	tenantID := "t1"

	store := newMemStore()
	subID, itemID := seedSubWithItem(t, store, tenantID, "cus_1", "plan_old")
	svc := NewService(store, nil)

	plans := &plansMock{plans: map[string]domain.Plan{
		"plan_old": {ID: "plan_old", Name: "Basic", BaseAmountCents: 1000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
		"plan_new": {ID: "plan_new", Name: "Pro", BaseAmountCents: 3000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
	}}

	invoices := &invoicesMock{
		sourceInvoice: domain.Invoice{ID: "src_inv", PaymentStatus: domain.PaymentSucceeded},
	}
	taxMock := &prorationTaxApplierMock{
		result: ProrationTaxResult{
			TaxFacts: domain.TaxFacts{
				TaxAmountCents: 185,
				TaxRate:        18.50,
				TaxName:        "VAT",
				TaxCountry:     "GB",
			},
		},
	}

	h := NewHandler(svc)
	h.SetProrationDeps(plans, invoices)
	h.SetProrationTaxApplier(taxMock)

	body, _ := json.Marshal(UpdateItemInput{NewPlanID: "plan_new", Immediate: true})
	req := updateItemURL(context.WithValue(ctx, auth.TestTenantIDKey(), tenantID), subID, itemID, body)

	rr := httptest.NewRecorder()
	h.updateItem(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200. body=%s", rr.Code, rr.Body.String())
	}
	if taxMock.calls != 1 {
		t.Errorf("tax applier called %d times, want 1", taxMock.calls)
	}
	if len(invoices.createdInvoices) != 1 {
		t.Fatalf("got %d invoices, want 1", len(invoices.createdInvoices))
	}

	inv := invoices.createdInvoices[0]
	if inv.TaxAmountCents != 185 {
		t.Errorf("invoice TaxAmountCents = %d, want 185", inv.TaxAmountCents)
	}
	if inv.TaxRate != 18.50 {
		t.Errorf("invoice TaxRate = %g, want 18.50", inv.TaxRate)
	}
	if inv.TaxName != "VAT" {
		t.Errorf("invoice TaxName = %q, want VAT", inv.TaxName)
	}
	// SubtotalCents should remain the pre-discount proration amount;
	// TotalAmountCents should add tax on top of discounted subtotal (no
	// discount here → subtotal + tax).
	wantTotal := inv.SubtotalCents - inv.DiscountCents + inv.TaxAmountCents
	if inv.TotalAmountCents != wantTotal {
		t.Errorf("invoice total = %d, want %d (subtotal - discount + tax)", inv.TotalAmountCents, wantTotal)
	}
	if inv.AmountDueCents != wantTotal {
		t.Errorf("invoice amount_due = %d, want %d", inv.AmountDueCents, wantTotal)
	}

	// FLOW B12 adjudication: proration.amount_cents is GROSS in every type —
	// for "invoice" it's the invoice total including tax, matching what the
	// credit branch always reported (the gross credited). Pre-fix it carried
	// the pre-tax net delta, so the activity timeline read "Proration invoice
	// $18.00" beside an invoice whose own page said $19.80.
	var resp ItemChangeResult
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Proration == nil || resp.Proration.Type != "invoice" {
		t.Fatalf("proration: got %+v, want type=invoice", resp.Proration)
	}
	if resp.Proration.AmountCents != wantTotal {
		t.Errorf("proration.amount_cents: got %d, want %d (the invoice TOTAL incl. tax, not the net delta)",
			resp.Proration.AmountCents, wantTotal)
	}
}

// TestUpdateItem_ProrationTaxErrorDefersToDraft covers the medium-severity
// audit finding: when ApplyTaxToLineItems returns a non-nil (transient infra)
// error, the proration invoice must NOT finalize with zero tax — it would ship
// an invoice lying about authoritative amounts. Instead the invoice is stamped
// Draft with tax_status=pending so the tax retry worker re-runs calculation.
func TestUpdateItem_ProrationTaxErrorDefersToDraft(t *testing.T) {
	ctx := context.Background()
	tenantID := "t1"

	store := newMemStore()
	subID, itemID := seedSubWithItem(t, store, tenantID, "cus_1", "plan_old")
	svc := NewService(store, nil)

	plans := &plansMock{plans: map[string]domain.Plan{
		"plan_old": {ID: "plan_old", Name: "Basic", BaseAmountCents: 1000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
		"plan_new": {ID: "plan_new", Name: "Pro", BaseAmountCents: 3000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
	}}
	invoices := &invoicesMock{
		sourceInvoice: domain.Invoice{ID: "src_inv", PaymentStatus: domain.PaymentSucceeded},
	}
	taxMock := &prorationTaxApplierMock{err: errors.New("stripe tax: 503 service unavailable")}

	h := NewHandler(svc)
	h.SetProrationDeps(plans, invoices)
	h.SetProrationTaxApplier(taxMock)

	body, _ := json.Marshal(UpdateItemInput{NewPlanID: "plan_new", Immediate: true})
	req := updateItemURL(context.WithValue(ctx, auth.TestTenantIDKey(), tenantID), subID, itemID, body)

	rr := httptest.NewRecorder()
	h.updateItem(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200. body=%s", rr.Code, rr.Body.String())
	}
	if taxMock.calls != 1 {
		t.Errorf("tax applier called %d times, want 1", taxMock.calls)
	}
	if len(invoices.createdInvoices) != 1 {
		t.Fatalf("got %d invoices, want 1", len(invoices.createdInvoices))
	}
	inv := invoices.createdInvoices[0]
	if inv.TaxStatus != domain.InvoiceTaxPending {
		t.Errorf("invoice TaxStatus = %q, want %q (defer for retry)", inv.TaxStatus, domain.InvoiceTaxPending)
	}
	if inv.Status != domain.InvoiceDraft {
		t.Errorf("invoice Status = %q, want %q (not finalized with zero tax)", inv.Status, domain.InvoiceDraft)
	}
	if inv.TaxAmountCents != 0 {
		t.Errorf("invoice TaxAmountCents = %d, want 0 (no tax computed yet)", inv.TaxAmountCents)
	}
	// Deferral facts (2026-07-10 fix): without a retryable tax_error_code the
	// tax-retry reconciler's filter (billing/tax_retry.go taxRetryableCodes)
	// skips the invoice FOREVER — deferred-to-draft was only half the fix.
	if inv.TaxErrorCode != string(tax.ErrCodeProviderOutage) {
		t.Errorf("invoice TaxErrorCode = %q, want %q (503 classifies as outage; must be retryable)", inv.TaxErrorCode, tax.ErrCodeProviderOutage)
	}
	if inv.TaxDeferredAt == nil {
		t.Error("invoice TaxDeferredAt must be stamped on deferral")
	}
	if inv.TaxPendingReason == "" {
		t.Error("invoice TaxPendingReason must carry the provider error")
	}
}

// TestUpdateItem_ProrationDeferredTaxCarriesRetryFacts pins the OTHER deferral
// route: the engine's ApplyTaxToLineItems defers internally (returns nil error
// with tax_status=pending and the deferral facts populated). Pre-fix the
// ProrationTaxResult mirror lacked TaxDeferredAt/TaxPendingReason/TaxErrorCode
// entirely — the adapter dropped them, the row landed tax_error_code=”, and
// the tax-retry reconciler never picked the invoice up (third documented
// field-drop on this mirror). Also pins BillingTimezone, which every engine
// writer stamps and this writer omitted.
func TestUpdateItem_ProrationDeferredTaxCarriesRetryFacts(t *testing.T) {
	ctx := context.Background()
	tenantID := "t1"

	store := newMemStore()
	subID, itemID := seedSubWithItem(t, store, tenantID, "cus_1", "plan_old")
	svc := NewService(store, nil)

	plans := &plansMock{plans: map[string]domain.Plan{
		"plan_old": {ID: "plan_old", Name: "Basic", BaseAmountCents: 1000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
		"plan_new": {ID: "plan_new", Name: "Pro", BaseAmountCents: 3000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
	}}
	invoices := &invoicesMock{
		sourceInvoice: domain.Invoice{ID: "src_inv", PaymentStatus: domain.PaymentSucceeded},
	}
	deferredAt := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	taxMock := &prorationTaxApplierMock{
		result: ProrationTaxResult{
			TaxFacts: domain.TaxFacts{
				TaxStatus:        domain.InvoiceTaxPending,
				TaxErrorCode:     string(tax.ErrCodeProviderOutage),
				TaxPendingReason: "stripe tax: request timed out",
				TaxDeferredAt:    &deferredAt,
			},
		},
	}

	h := NewHandler(svc)
	h.SetProrationDeps(plans, invoices)
	h.SetProrationTaxApplier(taxMock)

	body, _ := json.Marshal(UpdateItemInput{NewPlanID: "plan_new", Immediate: true})
	req := updateItemURL(context.WithValue(ctx, auth.TestTenantIDKey(), tenantID), subID, itemID, body)

	rr := httptest.NewRecorder()
	h.updateItem(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200. body=%s", rr.Code, rr.Body.String())
	}
	if len(invoices.createdInvoices) != 1 {
		t.Fatalf("got %d invoices, want 1", len(invoices.createdInvoices))
	}
	inv := invoices.createdInvoices[0]
	if inv.Status != domain.InvoiceDraft {
		t.Errorf("invoice Status = %q, want draft", inv.Status)
	}
	if inv.TaxErrorCode != string(tax.ErrCodeProviderOutage) {
		t.Errorf("TaxErrorCode = %q, want %q (dropped by the pre-fix DTO mirror)", inv.TaxErrorCode, tax.ErrCodeProviderOutage)
	}
	if inv.TaxDeferredAt == nil || !inv.TaxDeferredAt.Equal(deferredAt) {
		t.Errorf("TaxDeferredAt = %v, want %v", inv.TaxDeferredAt, deferredAt)
	}
	if inv.TaxPendingReason != "stripe tax: request timed out" {
		t.Errorf("TaxPendingReason = %q, want the provider reason", inv.TaxPendingReason)
	}
	if inv.BillingTimezone != "UTC" {
		t.Errorf("BillingTimezone = %q, want %q (unwired settings resolve UTC; engine writers all stamp this)", inv.BillingTimezone, "UTC")
	}
}

// ---------------------------------------------------------------------------
// Event dispatch tests — P0 #2. Design partners building on Velox need a
// webhook stream for subscription lifecycle the same way Stripe customers
// depend on customer.subscription.*. These tests pin the dispatch contract
// for every mutating handler so a silent regression can't ship.
// ---------------------------------------------------------------------------

type capturedEvent struct {
	tenantID  string
	eventType string
	payload   map[string]any
}

type capturingDispatcher struct {
	events []capturedEvent
	err    error
}

func (d *capturingDispatcher) Dispatch(_ context.Context, tenantID, eventType string, payload map[string]any) error {
	d.events = append(d.events, capturedEvent{tenantID: tenantID, eventType: eventType, payload: payload})
	return d.err
}

func (d *capturingDispatcher) firstOfType(eventType string) (capturedEvent, bool) {
	for _, e := range d.events {
		if e.eventType == eventType {
			return e, true
		}
	}
	return capturedEvent{}, false
}

func TestHandler_Cancel_FiresSubscriptionCanceled(t *testing.T) {
	ctx := context.Background()
	tenantID := "t1"
	store := newMemStore()
	subID, _ := seedSubWithItem(t, store, tenantID, "cus_1", "plan_old")
	svc := NewService(store, nil)

	dispatcher := &capturingDispatcher{}
	h := NewHandler(svc)
	h.SetEventDispatcher(dispatcher)

	req := httptest.NewRequest(http.MethodPost, "/subscriptions/"+subID+"/cancel", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", subID)
	req = req.WithContext(context.WithValue(
		context.WithValue(ctx, chi.RouteCtxKey, rctx),
		auth.TestTenantIDKey(), tenantID,
	))

	rr := httptest.NewRecorder()
	h.cancel(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200. body=%s", rr.Code, rr.Body.String())
	}

	// subscription.canceled moved IN-TX to the store's cancel writer
	// (DispatchTx subscription subset, 2026-07-05) — the handler must NOT
	// also dispatch it (double-emit). In-tx enqueue + payload proven by
	// lifecycle_outbox_integration_test.go.
	if _, ok := dispatcher.firstOfType(domain.EventSubscriptionCanceled); ok {
		t.Fatalf("%s must not be dispatched by the handler — it is enqueued in the cancel tx", domain.EventSubscriptionCanceled)
	}
}

func TestHandler_UpdateItem_ImmediatePlanChangeFiresItemUpdated(t *testing.T) {
	ctx := context.Background()
	tenantID := "t1"

	store := newMemStore()
	subID, itemID := seedSubWithItem(t, store, tenantID, "cus_1", "plan_old")
	svc := NewService(store, nil)

	plans := &plansMock{plans: map[string]domain.Plan{
		"plan_old": {ID: "plan_old", Name: "Basic", BaseAmountCents: 1000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
		"plan_new": {ID: "plan_new", Name: "Pro", BaseAmountCents: 3000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
	}}
	// Paid current-period prebill so the immediate upgrade proceeds (an upgrade
	// against an UNPAID source is blocked per ADR-050).
	invoices := &invoicesMock{sourceInvoice: domain.Invoice{ID: "src_inv", PaymentStatus: domain.PaymentSucceeded}}

	dispatcher := &capturingDispatcher{}
	h := NewHandler(svc)
	h.SetProrationDeps(plans, invoices)
	h.SetEventDispatcher(dispatcher)

	body, _ := json.Marshal(UpdateItemInput{NewPlanID: "plan_new", Immediate: true})
	req := updateItemURL(context.WithValue(ctx, auth.TestTenantIDKey(), tenantID), subID, itemID, body)
	rr := httptest.NewRecorder()
	h.updateItem(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200. body=%s", rr.Code, rr.Body.String())
	}

	ev, ok := dispatcher.firstOfType(domain.EventSubscriptionItemUpdated)
	if !ok {
		t.Fatalf("expected %s, got types=%v", domain.EventSubscriptionItemUpdated, eventTypes(dispatcher.events))
	}
	item := eventItem(t, ev.eventType, ev.payload, itemID)
	if item["plan_id"] != "plan_new" {
		t.Errorf("plan_id: got %v, want plan_new (post-change plan expected)", item["plan_id"])
	}
	// Scheduled event must NOT fire on immediate plan change.
	if _, fired := dispatcher.firstOfType(domain.EventSubscriptionPendingChangeScheduled); fired {
		t.Errorf("pending_change.scheduled fired on immediate plan change")
	}
}

func TestHandler_UpdateItem_ScheduledPlanChangeFiresPendingChangeScheduled(t *testing.T) {
	ctx := context.Background()
	tenantID := "t1"

	store := newMemStore()
	subID, itemID := seedSubWithItem(t, store, tenantID, "cus_1", "plan_old")
	svc := NewService(store, nil)

	plans := &plansMock{plans: map[string]domain.Plan{
		"plan_old": {ID: "plan_old", Name: "Basic", BaseAmountCents: 1000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
		"plan_new": {ID: "plan_new", Name: "Pro", BaseAmountCents: 3000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
	}}
	invoices := &invoicesMock{}

	dispatcher := &capturingDispatcher{}
	h := NewHandler(svc)
	h.SetProrationDeps(plans, invoices)
	h.SetEventDispatcher(dispatcher)

	body, _ := json.Marshal(UpdateItemInput{NewPlanID: "plan_new", Immediate: false})
	req := updateItemURL(context.WithValue(ctx, auth.TestTenantIDKey(), tenantID), subID, itemID, body)
	rr := httptest.NewRecorder()
	h.updateItem(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200. body=%s", rr.Code, rr.Body.String())
	}

	ev, ok := dispatcher.firstOfType(domain.EventSubscriptionPendingChangeScheduled)
	if !ok {
		t.Fatalf("expected %s, got types=%v", domain.EventSubscriptionPendingChangeScheduled, eventTypes(dispatcher.events))
	}
	if item := eventItem(t, ev.eventType, ev.payload, itemID); item["pending_plan_id"] != "plan_new" {
		t.Errorf("pending_plan_id: got %v, want plan_new", item["pending_plan_id"])
	}
	// item.updated must NOT also fire for a scheduled change — the intent is
	// observable but the item hasn't mutated yet.
	if _, fired := dispatcher.firstOfType(domain.EventSubscriptionItemUpdated); fired {
		t.Errorf("item.updated fired on scheduled (non-immediate) plan change")
	}
}

func TestHandler_RemoveItem_FiresItemRemoved(t *testing.T) {
	ctx := context.Background()
	tenantID := "t1"

	store := newMemStore()
	// seed a sub with 2 items so RemoveItem doesn't trip the "last item"
	// guard.
	subID, firstItemID := seedSubWithItem(t, store, tenantID, "cus_1", "plan_a")
	svc := NewService(store, nil)
	if _, err := svc.AddItem(ctx, tenantID, subID, AddItemInput{PlanID: "plan_b", Quantity: 1}); err != nil {
		t.Fatalf("seed second item: %v", err)
	}

	dispatcher := &capturingDispatcher{}
	h := NewHandler(svc)
	h.SetEventDispatcher(dispatcher)

	req := httptest.NewRequest(http.MethodDelete, "/subscriptions/"+subID+"/items/"+firstItemID, nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", subID)
	rctx.URLParams.Add("itemID", firstItemID)
	req = req.WithContext(context.WithValue(
		context.WithValue(ctx, chi.RouteCtxKey, rctx),
		auth.TestTenantIDKey(), tenantID,
	))
	rr := httptest.NewRecorder()
	h.removeItem(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status: got %d, want 204. body=%s", rr.Code, rr.Body.String())
	}

	ev, ok := dispatcher.firstOfType(domain.EventSubscriptionItemRemoved)
	if !ok {
		t.Fatalf("expected %s, got types=%v", domain.EventSubscriptionItemRemoved, eventTypes(dispatcher.events))
	}
	if ev.payload["item_id"] != firstItemID {
		t.Errorf("item_id: got %v, want %q", ev.payload["item_id"], firstItemID)
	}
}

// TestHandler_CancelPendingItemChange_FiresPendingChangeCanceled locks in that
// clearing a scheduled plan change surfaces as its own event type, so a
// webhook consumer listening for upgrade/downgrade flows can distinguish
// "scheduled but rolled back" from "applied at cycle boundary".
func TestHandler_CancelPendingItemChange_FiresPendingChangeCanceled(t *testing.T) {
	ctx := context.Background()
	tenantID := "t1"

	store := newMemStore()
	subID, itemID := seedSubWithItem(t, store, tenantID, "cus_1", "plan_old")
	svc := NewService(store, nil)

	plans := &plansMock{plans: map[string]domain.Plan{
		"plan_old": {ID: "plan_old", Name: "Basic", BaseAmountCents: 1000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
		"plan_new": {ID: "plan_new", Name: "Pro", BaseAmountCents: 3000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
	}}

	// Stage a scheduled change first so CancelPendingItemChange has something
	// to clear. The UpdateItem path drives this via svc so the scheduled fields
	// land on the item row.
	if _, err := svc.UpdateItem(ctx, tenantID, subID, itemID, UpdateItemInput{NewPlanID: "plan_new", Immediate: false}); err != nil {
		t.Fatalf("stage scheduled change: %v", err)
	}

	dispatcher := &capturingDispatcher{}
	h := NewHandler(svc)
	h.SetProrationDeps(plans, &invoicesMock{})
	h.SetEventDispatcher(dispatcher)

	req := httptest.NewRequest(http.MethodDelete, "/subscriptions/"+subID+"/items/"+itemID+"/pending-change", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", subID)
	rctx.URLParams.Add("itemID", itemID)
	req = req.WithContext(context.WithValue(
		context.WithValue(ctx, chi.RouteCtxKey, rctx),
		auth.TestTenantIDKey(), tenantID,
	))
	rr := httptest.NewRecorder()
	h.cancelPendingItemChange(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200. body=%s", rr.Code, rr.Body.String())
	}

	if _, ok := dispatcher.firstOfType(domain.EventSubscriptionPendingChangeCanceled); !ok {
		t.Fatalf("expected %s, got types=%v", domain.EventSubscriptionPendingChangeCanceled, eventTypes(dispatcher.events))
	}
}

// ---------------------------------------------------------------------------
// Mid-cycle proration tests — P1 gap #188. Plan changes were the only mutation
// that drove proration; qty changes, item adds, and item removes silently
// skipped it. These tests pin the new behaviour so the invoice/credit is
// produced even when the billing engine doesn't touch the row.
// ---------------------------------------------------------------------------

// addItemURL builds the POST /subscriptions/{id}/items request with chi params.
func addItemURL(ctx context.Context, subID string, body []byte) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/subscriptions/"+subID+"/items", bytes.NewReader(body))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", subID)
	return req.WithContext(context.WithValue(ctx, chi.RouteCtxKey, rctx))
}

// removeItemURL builds the DELETE /subscriptions/{id}/items/{itemID} request.
func removeItemURL(ctx context.Context, subID, itemID string) *http.Request {
	url := fmt.Sprintf("/subscriptions/%s/items/%s", subID, itemID)
	req := httptest.NewRequest(http.MethodDelete, url, nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", subID)
	rctx.URLParams.Add("itemID", itemID)
	return req.WithContext(context.WithValue(ctx, chi.RouteCtxKey, rctx))
}

// TestAddItem_ProratesMidCycle verifies that adding a priced line to an active
// subscription mid-period emits a proration invoice for the partial-period
// portion of the new item's cost on an in_advance sub with a paid source
// invoice. Prior behaviour was silent skip — the customer got the new item
// for free until next cycle close.
//
// Uses in_advance plans + a paid source invoice because the post-fix
// proration gates require both (in_arrears defers to cycle close;
// in_advance unpaid also defers — see TestAddItem_DefersProration_*).
func TestAddItem_ProratesMidCycle(t *testing.T) {
	ctx := clock.WithEffectiveNow(context.Background(), proNow)
	tenantID := "t1"

	store := newMemStore()
	subID, _ := seedSubWithItemAt(t, store, tenantID, "cus_1", "plan_existing", proPeriodStart, proPeriodEnd)
	svc := NewService(store, nil)

	plans := &plansMock{plans: map[string]domain.Plan{
		"plan_existing": {ID: "plan_existing", Name: "Basic", BaseAmountCents: 1000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
		"plan_new":      {ID: "plan_new", Name: "Add-on", BaseAmountCents: 2000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
	}}
	invoices := &invoicesMock{
		sourceInvoice: domain.Invoice{ID: "src_inv", PaymentStatus: domain.PaymentSucceeded},
	}

	h := NewHandler(svc)
	h.SetProrationDeps(plans, invoices)

	body, _ := json.Marshal(AddItemInput{PlanID: "plan_new", Quantity: 1})
	req := addItemURL(context.WithValue(ctx, auth.TestTenantIDKey(), tenantID), subID, body)

	rr := httptest.NewRecorder()
	h.addItem(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status: got %d, want 201. body=%s", rr.Code, rr.Body.String())
	}
	if len(invoices.createdInvoices) != 1 {
		t.Fatalf("createdInvoices: got %d, want 1 — proration invoice must be emitted for mid-cycle add",
			len(invoices.createdInvoices))
	}
	inv := invoices.createdInvoices[0]
	if inv.SourceChangeType != domain.ItemChangeTypeAdd {
		t.Errorf("invoice SourceChangeType: got %q, want %q", inv.SourceChangeType, domain.ItemChangeTypeAdd)
	}
	if inv.SubscriptionID != subID {
		t.Errorf("invoice subscription_id: got %q, want %q", inv.SubscriptionID, subID)
	}
	// Period is an exact 30-day span (now ± 15 days) with now at the midpoint,
	// so remainingPeriodRatio is exactly 15/30: RoundHalfToEven(2000×15, 30) =
	// 1000. Deterministic — no midnight snapping, and sub-second clock skew
	// can't move the 15-day rounding.
	if inv.SubtotalCents != 1000 {
		t.Errorf("invoice subtotal: got %d, want 1000 ($20 × 15/30)", inv.SubtotalCents)
	}
	if inv.AmountDueCents <= 0 {
		t.Errorf("invoice amount_due: got %d, want > 0", inv.AmountDueCents)
	}
}

// TestUpdateItem_QuantityIncreaseProratesAsInvoice verifies a mid-cycle qty
// increase emits an invoice for the partial-period delta. Prior behaviour was
// silent skip, leaving the customer on the new qty with no delta billed.
func TestUpdateItem_QuantityIncreaseProratesAsInvoice(t *testing.T) {
	ctx := clock.WithEffectiveNow(context.Background(), proNow)
	tenantID := "t1"

	store := newMemStore()
	subID, itemID := seedSubWithItemAt(t, store, tenantID, "cus_1", "plan_seats", proPeriodStart, proPeriodEnd)
	svc := NewService(store, nil)

	plans := &plansMock{plans: map[string]domain.Plan{
		"plan_seats": {ID: "plan_seats", Name: "Seat", BaseAmountCents: 1000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
	}}
	invoices := &invoicesMock{
		sourceInvoice: domain.Invoice{ID: "src_inv", PaymentStatus: domain.PaymentSucceeded},
	}

	h := NewHandler(svc)
	h.SetProrationDeps(plans, invoices)

	// Seed starts at qty=1; bump to 3 → delta = 2 seats × $10 = $20 per full
	// period, half-period → ~$10.
	newQty := int64(3)
	body, _ := json.Marshal(UpdateItemInput{Quantity: &newQty})
	req := updateItemURL(context.WithValue(ctx, auth.TestTenantIDKey(), tenantID), subID, itemID, body)

	rr := httptest.NewRecorder()
	h.updateItem(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200. body=%s", rr.Code, rr.Body.String())
	}
	if len(invoices.createdInvoices) != 1 {
		t.Fatalf("createdInvoices: got %d, want 1 — qty increase must invoice the delta",
			len(invoices.createdInvoices))
	}
	inv := invoices.createdInvoices[0]
	if inv.SourceChangeType != domain.ItemChangeTypeQuantity {
		t.Errorf("invoice SourceChangeType: got %q, want %q", inv.SourceChangeType, domain.ItemChangeTypeQuantity)
	}
	if inv.SourceSubscriptionItemID != itemID {
		t.Errorf("invoice item_id: got %q, want %q", inv.SourceSubscriptionItemID, itemID)
	}
	if inv.SubtotalCents != 1000 {
		t.Errorf("invoice subtotal: got %d, want 1000 (2000 delta × 15/30)", inv.SubtotalCents)
	}
}

// TestUpdateItem_PlanChange_StubPeriod_UsesFullCycleDenominator is the
// end-to-end regression guard for the stub-period proration overcharge found
// in production (DEMO-012094). A mid-month upgrade on a 14-day stub of a 30-day
// monthly cycle must prorate the $30 delta against the FULL 30-day cycle —
// $13.00 for the 13 remaining days — NOT the 14-day stub ($27.86).
func TestUpdateItem_PlanChange_StubPeriod_UsesFullCycleDenominator(t *testing.T) {
	tenantID := "t1"
	// 14-day stub: Apr 16 18:30 → Apr 30 18:30 (2027); the upgrade fires at
	// Apr 17 06:36, leaving 13 days of the 30-day monthly cycle.
	periodStart := time.Date(2027, 4, 16, 18, 30, 0, 0, time.UTC)
	periodEnd := time.Date(2027, 4, 30, 18, 30, 0, 0, time.UTC)
	changeAt := time.Date(2027, 4, 17, 6, 36, 0, 0, time.UTC)
	ctx := clock.WithEffectiveNow(context.Background(), changeAt)

	store := newMemStore()
	subID, itemID := seedSubWithItemAt(t, store, tenantID, "cus_1", "plan_starter", periodStart, periodEnd)
	svc := NewService(store, nil)

	plans := &plansMock{plans: map[string]domain.Plan{
		"plan_starter": {ID: "plan_starter", Name: "Start 20", BaseAmountCents: 2000, Currency: "USD", BaseBillTiming: domain.BillInAdvance, BillingInterval: domain.BillingMonthly},
		"plan_pro":     {ID: "plan_pro", Name: "Pro 50", BaseAmountCents: 5000, Currency: "USD", BaseBillTiming: domain.BillInAdvance, BillingInterval: domain.BillingMonthly},
	}}
	invoices := &invoicesMock{sourceInvoice: domain.Invoice{ID: "src_inv", PaymentStatus: domain.PaymentSucceeded}}

	h := NewHandler(svc)
	h.SetProrationDeps(plans, invoices)

	body, _ := json.Marshal(UpdateItemInput{NewPlanID: "plan_pro", Immediate: true})
	req := updateItemURL(context.WithValue(ctx, auth.TestTenantIDKey(), tenantID), subID, itemID, body)

	rr := httptest.NewRecorder()
	h.updateItem(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200. body=%s", rr.Code, rr.Body.String())
	}
	if len(invoices.createdInvoices) != 1 {
		t.Fatalf("createdInvoices: got %d, want 1", len(invoices.createdInvoices))
	}
	// (5000-2000) × 13 / 30 = 1300. The stub-denominator bug billed 2786.
	if got := invoices.createdInvoices[0].SubtotalCents; got != 1300 {
		t.Errorf("stub-period upgrade proration: got %d, want 1300 ($30 delta × 13/30, full cycle); stub-denominator bug = 2786", got)
	}
}

// TestUpdateItem_QuantityDecreaseProratesAsCredit verifies a mid-cycle qty
// reduction issues a credit for the unused portion of the removed seats.
func TestUpdateItem_QuantityDecreaseProratesAsCredit(t *testing.T) {
	ctx := clock.WithEffectiveNow(context.Background(), proNow)
	tenantID := "t1"

	store := newMemStore()
	subID, itemID := seedSubWithItemAt(t, store, tenantID, "cus_1", "plan_seats", proPeriodStart, proPeriodEnd)
	svc := NewService(store, nil)
	// Start at qty=3 so we can drop to 1.
	startQty := int64(3)
	if _, err := svc.UpdateItem(ctx, tenantID, subID, itemID, UpdateItemInput{Quantity: &startQty}); err != nil {
		t.Fatalf("seed qty=3: %v", err)
	}

	plans := &plansMock{plans: map[string]domain.Plan{
		"plan_seats": {ID: "plan_seats", Name: "Seat", BaseAmountCents: 1000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
	}}
	invoices := &invoicesMock{
		sourceInvoice: domain.Invoice{ID: "src_inv", PaymentStatus: domain.PaymentSucceeded, SubtotalCents: 3000, TotalAmountCents: 3000},
	}
	cn := &fakeCNIssuer{}

	h := NewHandler(svc)
	h.SetProrationDeps(plans, invoices)
	h.SetCreditNoteIssuer(cn)

	// Drop 3 → 1 = 2 seats × $10 = $20 full-period, half-period → ~$10 credit.
	newQty := int64(1)
	body, _ := json.Marshal(UpdateItemInput{Quantity: &newQty})
	req := updateItemURL(context.WithValue(ctx, auth.TestTenantIDKey(), tenantID), subID, itemID, body)

	rr := httptest.NewRecorder()
	h.updateItem(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200. body=%s", rr.Code, rr.Body.String())
	}
	if len(invoices.createdInvoices) != 0 {
		t.Errorf("createdInvoices: got %d, want 0 — qty decrease should credit not invoice",
			len(invoices.createdInvoices))
	}
	if len(cn.calls) != 1 {
		t.Fatalf("credit notes: got %d, want 1 — qty decrease must credit the unused portion", len(cn.calls))
	}
	call := cn.calls[0]
	if call.reason != "subscription_quantity_decrease" || call.invoiceID != "src_inv" {
		t.Errorf("credit note: got %+v, want reason subscription_quantity_decrease on src_inv", call)
	}
	if call.gross != 1000 {
		t.Errorf("credit amount: got %d, want 1000 ($20 × 15/30)", call.gross)
	}
}

// TestRemoveItem_ProratesAsCredit verifies mid-cycle item removal credits the
// customer for the unused portion of the already-paid period. Seeds a 2-item
// subscription so RemoveItem's "last item" guard doesn't trip.
func TestRemoveItem_ProratesAsCredit(t *testing.T) {
	ctx := clock.WithEffectiveNow(context.Background(), proNow)
	tenantID := "t1"

	store := newMemStore()
	subID, firstItemID := seedSubWithItemAt(t, store, tenantID, "cus_1", "plan_a", proPeriodStart, proPeriodEnd)
	svc := NewService(store, nil)
	if _, err := svc.AddItem(ctx, tenantID, subID, AddItemInput{PlanID: "plan_b", Quantity: 1}); err != nil {
		t.Fatalf("seed second item: %v", err)
	}

	plans := &plansMock{plans: map[string]domain.Plan{
		"plan_a": {ID: "plan_a", Name: "Base A", BaseAmountCents: 2000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
		"plan_b": {ID: "plan_b", Name: "Base B", BaseAmountCents: 1000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
	}}
	invoices := &invoicesMock{
		sourceInvoice: domain.Invoice{ID: "src_inv", PaymentStatus: domain.PaymentSucceeded, SubtotalCents: 3000, TotalAmountCents: 3000},
	}
	cn := &fakeCNIssuer{}

	h := NewHandler(svc)
	h.SetProrationDeps(plans, invoices)
	h.SetCreditNoteIssuer(cn)

	req := removeItemURL(context.WithValue(ctx, auth.TestTenantIDKey(), tenantID), subID, firstItemID)
	rr := httptest.NewRecorder()
	h.removeItem(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status: got %d, want 204. body=%s", rr.Code, rr.Body.String())
	}
	if len(invoices.createdInvoices) != 0 {
		t.Errorf("createdInvoices: got %d, want 0 — remove should credit not invoice",
			len(invoices.createdInvoices))
	}
	if len(cn.calls) != 1 {
		t.Fatalf("credit notes: got %d, want 1 — removed item must be credited", len(cn.calls))
	}
	call := cn.calls[0]
	if call.reason != "subscription_item_removed" || call.invoiceID != "src_inv" {
		t.Errorf("credit note: got %+v, want reason subscription_item_removed on src_inv", call)
	}
	// plan_a is $20/period; exact 15/30 half-period → RoundHalfToEven(2000×15, 30) = 1000.
	if call.gross != 1000 {
		t.Errorf("credit amount: got %d, want 1000 ($20 × 15/30)", call.gross)
	}
}

// TestHandleItemProration_DefersOnInArrears locks in the industry-standard
// gate: in_arrears items must NOT emit an immediate proration invoice or
// credit at mutation time. Pre-fix Velox emitted both, then also billed
// full-period at cycle close → ~half-plan over/undercharge per change.
// Post-fix: silent defer with info log; cycle close handles the math.
func TestHandleItemProration_DefersOnInArrears(t *testing.T) {
	ctx := context.Background()
	tenantID := "t1"

	store := newMemStore()
	subID, itemID := seedSubWithItem(t, store, tenantID, "cus_1", "plan_old")
	svc := NewService(store, nil)

	// Both plans in_arrears (empty BaseBillTiming would also work — defaults
	// to in_arrears in the gate's lenient fallback).
	plans := &plansMock{plans: map[string]domain.Plan{
		"plan_old": {ID: "plan_old", Name: "Basic", BaseAmountCents: 5000, Currency: "USD", BaseBillTiming: domain.BillInArrears},
		"plan_new": {ID: "plan_new", Name: "Pro", BaseAmountCents: 10000, Currency: "USD", BaseBillTiming: domain.BillInArrears},
	}}
	invoices := &invoicesMock{}

	h := NewHandler(svc)
	h.SetProrationDeps(plans, invoices)

	body, _ := json.Marshal(UpdateItemInput{NewPlanID: "plan_new", Immediate: true})
	req := updateItemURL(context.WithValue(ctx, auth.TestTenantIDKey(), tenantID), subID, itemID, body)
	rr := httptest.NewRecorder()
	h.updateItem(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200. body=%s", rr.Code, rr.Body.String())
	}
	if len(invoices.createdInvoices) != 0 {
		t.Errorf("in_arrears upgrade must NOT emit immediate proration invoice; got %d", len(invoices.createdInvoices))
	}
}

// TestUpdateItem_QuantityNoOpRejected ensures a qty-change request that doesn't
// actually change the qty returns 400 rather than silently emitting a zero
// proration invoice. Mirrors the same guard on plan changes.
func TestUpdateItem_QuantityNoOpRejected(t *testing.T) {
	ctx := context.Background()
	tenantID := "t1"

	store := newMemStore()
	subID, itemID := seedSubWithItem(t, store, tenantID, "cus_1", "plan_seats")
	svc := NewService(store, nil)

	plans := &plansMock{plans: map[string]domain.Plan{
		"plan_seats": {ID: "plan_seats", Name: "Seat", BaseAmountCents: 1000, Currency: "USD"},
	}}
	invoices := &invoicesMock{}

	h := NewHandler(svc)
	h.SetProrationDeps(plans, invoices)

	sameQty := int64(1) // seeded qty is 1
	body, _ := json.Marshal(UpdateItemInput{Quantity: &sameQty})
	req := updateItemURL(context.WithValue(ctx, auth.TestTenantIDKey(), tenantID), subID, itemID, body)

	rr := httptest.NewRecorder()
	h.updateItem(rr, req)

	if rr.Code != http.StatusBadRequest && rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want 400/422 for no-op qty. body=%s", rr.Code, rr.Body.String())
	}
	if len(invoices.createdInvoices) != 0 {
		t.Errorf("no-op qty must not trigger proration")
	}
}

func eventTypes(events []capturedEvent) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, e.eventType)
	}
	return out
}

// TestHandler_ScheduleCancel_FiresEvent covers the soft-cancel handler surface
// end-to-end: the at_period_end body is accepted, the row picks up
// CancelAtPeriodEnd=true, status remains active (schedule must not flip
// status), and the dispatcher receives subscription.cancel_scheduled with the
// right payload. Audit logging is wired in handler.go and exercised in the
// integration tests; here we focus on the contract a UI client depends on.
func TestHandler_ScheduleCancel_FiresEvent(t *testing.T) {
	ctx := context.Background()
	tenantID := "t1"
	store := newMemStore()
	subID, _ := seedSubWithItem(t, store, tenantID, "cus_1", "plan_old")
	svc := NewService(store, nil)

	dispatcher := &capturingDispatcher{}
	h := NewHandler(svc)
	h.SetEventDispatcher(dispatcher)

	body := bytes.NewBufferString(`{"at_period_end": true}`)
	req := httptest.NewRequest(http.MethodPost, "/subscriptions/"+subID+"/schedule-cancel", body)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", subID)
	req = req.WithContext(context.WithValue(
		context.WithValue(ctx, chi.RouteCtxKey, rctx),
		auth.TestTenantIDKey(), tenantID,
	))

	rr := httptest.NewRecorder()
	h.scheduleCancel(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200. body=%s", rr.Code, rr.Body.String())
	}

	var got domain.Subscription
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !got.CancelAtPeriodEnd {
		t.Error("response must reflect cancel_at_period_end=true")
	}
	if got.Status != domain.SubscriptionActive {
		t.Errorf("status: got %q, want active (schedule must not flip status)", got.Status)
	}

	ev, ok := dispatcher.firstOfType(domain.EventSubscriptionCancelScheduled)
	if !ok {
		t.Fatalf("expected %s, got types=%v", domain.EventSubscriptionCancelScheduled, eventTypes(dispatcher.events))
	}
	if err := domain.CheckWebhookData(ev.eventType, ev.payload); err != nil {
		t.Fatal(err)
	}
	if sub := ev.payload[domain.WebhookResourceSubscription].(map[string]any); sub["cancel_at_period_end"] != true {
		t.Errorf("subscription.cancel_at_period_end: got %v, want true", sub["cancel_at_period_end"])
	}
}

// TestHandler_ScheduleCancel_RejectsBadInput verifies the validation chain
// surfaces 400 cleanly: empty body and both fields together both fail at the
// service layer, and the handler must translate that into a 4xx — never a
// silent 200 with an unset schedule.
func TestHandler_ScheduleCancel_RejectsBadInput(t *testing.T) {
	ctx := context.Background()
	tenantID := "t1"
	store := newMemStore()
	subID, _ := seedSubWithItem(t, store, tenantID, "cus_1", "plan_old")
	svc := NewService(store, nil)
	h := NewHandler(svc)

	cases := []struct {
		name string
		body string
	}{
		{"empty input", `{}`},
		{"both fields set", `{"at_period_end": true, "cancel_at": "2099-01-01T00:00:00Z"}`},
		{"past timestamp", `{"cancel_at": "2020-01-01T00:00:00Z"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/subscriptions/"+subID+"/schedule-cancel", bytes.NewBufferString(tc.body))
			rctx := chi.NewRouteContext()
			rctx.URLParams.Add("id", subID)
			req = req.WithContext(context.WithValue(
				context.WithValue(ctx, chi.RouteCtxKey, rctx),
				auth.TestTenantIDKey(), tenantID,
			))
			rr := httptest.NewRecorder()
			h.scheduleCancel(rr, req)
			if rr.Code < 400 || rr.Code >= 500 {
				t.Errorf("status: got %d, want 4xx. body=%s", rr.Code, rr.Body.String())
			}
		})
	}
}

// TestHandler_PauseCollection_FiresEvent covers the pause-collection PUT
// surface: the keep_as_draft body is accepted, the row picks up
// PauseCollection, status remains active, and the dispatcher receives
// subscription.collection_paused with the right payload.
func TestHandler_PauseCollection_FiresEvent(t *testing.T) {
	ctx := context.Background()
	tenantID := "t1"
	store := newMemStore()
	subID, _ := seedSubWithItem(t, store, tenantID, "cus_1", "plan_old")
	svc := NewService(store, nil)

	dispatcher := &capturingDispatcher{}
	h := NewHandler(svc)
	h.SetEventDispatcher(dispatcher)

	body := bytes.NewBufferString(`{"behavior": "keep_as_draft"}`)
	req := httptest.NewRequest(http.MethodPut, "/subscriptions/"+subID+"/pause-collection", body)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", subID)
	req = req.WithContext(context.WithValue(
		context.WithValue(ctx, chi.RouteCtxKey, rctx),
		auth.TestTenantIDKey(), tenantID,
	))

	rr := httptest.NewRecorder()
	h.pauseCollection(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200. body=%s", rr.Code, rr.Body.String())
	}

	var got domain.Subscription
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.PauseCollection == nil {
		t.Fatal("PauseCollection must be set on response")
	}
	if got.PauseCollection.Behavior != domain.PauseCollectionKeepAsDraft {
		t.Errorf("behavior: got %q, want %q", got.PauseCollection.Behavior, domain.PauseCollectionKeepAsDraft)
	}
	if got.Status != domain.SubscriptionActive {
		t.Errorf("status: got %q, want active (collection-pause must not flip status)", got.Status)
	}

	ev, ok := dispatcher.firstOfType(domain.EventSubscriptionCollectionPaused)
	if !ok {
		t.Fatalf("expected %s, got types=%v", domain.EventSubscriptionCollectionPaused, eventTypes(dispatcher.events))
	}
	if err := domain.CheckWebhookData(ev.eventType, ev.payload); err != nil {
		t.Fatal(err)
	}
	sub := ev.payload[domain.WebhookResourceSubscription].(map[string]any)
	if pc, _ := sub["pause_collection"].(map[string]any); pc["behavior"] != "keep_as_draft" {
		t.Errorf("subscription.pause_collection: got %v, want behavior keep_as_draft", sub["pause_collection"])
	}
}

// TestHandler_PauseCollection_RejectsBadInput verifies the validation chain
// surfaces 400 cleanly: empty behavior and unsupported behavior both fail at
// the service layer.
func TestHandler_PauseCollection_RejectsBadInput(t *testing.T) {
	ctx := context.Background()
	tenantID := "t1"
	store := newMemStore()
	subID, _ := seedSubWithItem(t, store, tenantID, "cus_1", "plan_old")
	svc := NewService(store, nil)
	h := NewHandler(svc)

	cases := []struct {
		name string
		body string
	}{
		{"empty body", `{}`},
		{"unsupported behavior", `{"behavior": "mark_uncollectible"}`},
		{"past resumes_at", `{"behavior": "keep_as_draft", "resumes_at": "2020-01-01T00:00:00Z"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPut, "/subscriptions/"+subID+"/pause-collection", bytes.NewBufferString(tc.body))
			rctx := chi.NewRouteContext()
			rctx.URLParams.Add("id", subID)
			req = req.WithContext(context.WithValue(
				context.WithValue(ctx, chi.RouteCtxKey, rctx),
				auth.TestTenantIDKey(), tenantID,
			))
			rr := httptest.NewRecorder()
			h.pauseCollection(rr, req)
			if rr.Code < 400 || rr.Code >= 500 {
				t.Errorf("status: got %d, want 4xx. body=%s", rr.Code, rr.Body.String())
			}
		})
	}
}

// TestHandler_ResumeCollection_FiresEvent covers the inverse: a sub with
// pause_collection set can have it cleared via DELETE, and the resumed event
// fires.
func TestHandler_ResumeCollection_FiresEvent(t *testing.T) {
	ctx := context.Background()
	tenantID := "t1"
	store := newMemStore()
	subID, _ := seedSubWithItem(t, store, tenantID, "cus_1", "plan_old")
	svc := NewService(store, nil)
	if _, err := svc.PauseCollection(ctx, tenantID, subID, PauseCollectionInput{
		Behavior: domain.PauseCollectionKeepAsDraft,
	}); err != nil {
		t.Fatalf("seed pause: %v", err)
	}

	dispatcher := &capturingDispatcher{}
	h := NewHandler(svc)
	h.SetEventDispatcher(dispatcher)

	req := httptest.NewRequest(http.MethodDelete, "/subscriptions/"+subID+"/pause-collection", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", subID)
	req = req.WithContext(context.WithValue(
		context.WithValue(ctx, chi.RouteCtxKey, rctx),
		auth.TestTenantIDKey(), tenantID,
	))
	rr := httptest.NewRecorder()
	h.resumeCollection(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200. body=%s", rr.Code, rr.Body.String())
	}

	var got domain.Subscription
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.PauseCollection != nil {
		t.Errorf("PauseCollection should be cleared, got %+v", got.PauseCollection)
	}

	if _, ok := dispatcher.firstOfType(domain.EventSubscriptionCollectionResumed); !ok {
		t.Fatalf("expected %s, got types=%v", domain.EventSubscriptionCollectionResumed, eventTypes(dispatcher.events))
	}
}

// TestHandler_ClearScheduledCancel_FiresEvent covers the inverse: a sub with a
// scheduled cancel can have it cleared via DELETE, and the cleared event fires.
func TestHandler_ClearScheduledCancel_FiresEvent(t *testing.T) {
	ctx := context.Background()
	tenantID := "t1"
	store := newMemStore()
	subID, _ := seedSubWithItem(t, store, tenantID, "cus_1", "plan_old")
	svc := NewService(store, nil)
	if _, err := svc.ScheduleCancel(ctx, tenantID, subID, ScheduleCancelInput{AtPeriodEnd: true}); err != nil {
		t.Fatalf("seed schedule: %v", err)
	}

	dispatcher := &capturingDispatcher{}
	h := NewHandler(svc)
	h.SetEventDispatcher(dispatcher)

	req := httptest.NewRequest(http.MethodDelete, "/subscriptions/"+subID+"/scheduled-cancel", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", subID)
	req = req.WithContext(context.WithValue(
		context.WithValue(ctx, chi.RouteCtxKey, rctx),
		auth.TestTenantIDKey(), tenantID,
	))
	rr := httptest.NewRecorder()
	h.clearScheduledCancel(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200. body=%s", rr.Code, rr.Body.String())
	}

	var got domain.Subscription
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.CancelAtPeriodEnd {
		t.Error("cancel_at_period_end should be cleared")
	}

	if _, ok := dispatcher.firstOfType(domain.EventSubscriptionCancelCleared); !ok {
		t.Fatalf("expected %s, got types=%v", domain.EventSubscriptionCancelCleared, eventTypes(dispatcher.events))
	}
}

// TestHandler_EndTrial_FiresEvent covers the operator-driven trial end:
// POST /end-trial flips a trialing sub to active, fires
// subscription.trial_ended with triggered_by="operator".
func TestHandler_EndTrial_FiresEvent(t *testing.T) {
	ctx := context.Background()
	tenantID := "t1"
	store := newMemStore()
	now := time.Now().UTC()
	trialEnd := now.Add(14 * 24 * time.Hour)
	periodStart := now
	periodEnd := trialEnd.Add(30 * 24 * time.Hour)
	sub, err := store.Create(ctx, tenantID, domain.Subscription{
		Code: "sub-trial", DisplayName: "Trial Sub", CustomerID: "cus_1",
		Status:                    domain.SubscriptionTrialing,
		BillingTime:               domain.BillingTimeCalendar,
		TrialStartAt:              &periodStart,
		TrialEndAt:                &trialEnd,
		StartedAt:                 &periodStart,
		CurrentBillingPeriodStart: &periodStart,
		CurrentBillingPeriodEnd:   &periodEnd,
		Items:                     []domain.SubscriptionItem{{PlanID: "plan_pro", Quantity: 1}},
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	svc := NewService(store, nil)
	dispatcher := &capturingDispatcher{}
	h := NewHandler(svc)
	h.SetEventDispatcher(dispatcher)

	req := httptest.NewRequest(http.MethodPost, "/subscriptions/"+sub.ID+"/end-trial", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", sub.ID)
	req = req.WithContext(context.WithValue(
		context.WithValue(ctx, chi.RouteCtxKey, rctx),
		auth.TestTenantIDKey(), tenantID,
	))
	rr := httptest.NewRecorder()
	h.endTrial(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200. body=%s", rr.Code, rr.Body.String())
	}

	var got domain.Subscription
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.Status != domain.SubscriptionActive {
		t.Errorf("status: got %q, want active", got.Status)
	}
	if got.ActivatedAt == nil {
		t.Error("activated_at should be stamped")
	}

	// subscription.trial_ended moved IN-TX to the store's EndTrialEarly
	// writer (DispatchTx subscription subset) — the handler must NOT also
	// dispatch it. Proven in-tx by lifecycle_outbox_integration_test.go.
	if _, ok := dispatcher.firstOfType(domain.EventSubscriptionTrialEnded); ok {
		t.Fatalf("%s must not be dispatched by the handler — it is enqueued in the end-trial tx", domain.EventSubscriptionTrialEnded)
	}
}

// TestHandler_EndTrial_RejectsNonTrialingSub locks in the precondition: the
// store atomic guard rejects calls on subs that aren't trialing (active,
// canceled, paused, etc.) so operators don't accidentally mutate state.
func TestHandler_EndTrial_RejectsNonTrialingSub(t *testing.T) {
	ctx := context.Background()
	tenantID := "t1"
	store := newMemStore()
	subID, _ := seedSubWithItem(t, store, tenantID, "cus_1", "plan_old") // active
	svc := NewService(store, nil)
	h := NewHandler(svc)

	req := httptest.NewRequest(http.MethodPost, "/subscriptions/"+subID+"/end-trial", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", subID)
	req = req.WithContext(context.WithValue(
		context.WithValue(ctx, chi.RouteCtxKey, rctx),
		auth.TestTenantIDKey(), tenantID,
	))
	rr := httptest.NewRecorder()
	h.endTrial(rr, req)
	if rr.Code == http.StatusOK {
		t.Errorf("expected non-2xx for end-trial on active sub, got %d. body=%s", rr.Code, rr.Body.String())
	}
}

// TestHandler_ExtendTrial_FiresEvent covers the happy path: POST
// /extend-trial pushes trial_end_at later, fires
// subscription.trial_extended with triggered_by="operator".
func TestHandler_ExtendTrial_FiresEvent(t *testing.T) {
	ctx := context.Background()
	tenantID := "t1"
	store := newMemStore()
	now := time.Now().UTC()
	trialEnd := now.Add(14 * 24 * time.Hour)
	periodStart := now
	periodEnd := trialEnd.Add(30 * 24 * time.Hour)
	sub, err := store.Create(ctx, tenantID, domain.Subscription{
		Code: "sub-trial-ext", DisplayName: "Trial Sub", CustomerID: "cus_1",
		Status:                    domain.SubscriptionTrialing,
		BillingTime:               domain.BillingTimeCalendar,
		TrialStartAt:              &periodStart,
		TrialEndAt:                &trialEnd,
		StartedAt:                 &periodStart,
		CurrentBillingPeriodStart: &periodStart,
		CurrentBillingPeriodEnd:   &periodEnd,
		Items:                     []domain.SubscriptionItem{{PlanID: "plan_pro", Quantity: 1}},
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	svc := NewService(store, nil)
	dispatcher := &capturingDispatcher{}
	h := NewHandler(svc)
	h.SetEventDispatcher(dispatcher)

	// RFC3339 strips sub-second precision on round-trip; truncate so the
	// equality assertion below holds.
	newEnd := trialEnd.Add(7 * 24 * time.Hour).Truncate(time.Second)
	body, _ := json.Marshal(map[string]any{"trial_end": newEnd.Format(time.RFC3339)})
	req := httptest.NewRequest(http.MethodPost, "/subscriptions/"+sub.ID+"/extend-trial", bytes.NewReader(body))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", sub.ID)
	req = req.WithContext(context.WithValue(
		context.WithValue(ctx, chi.RouteCtxKey, rctx),
		auth.TestTenantIDKey(), tenantID,
	))
	rr := httptest.NewRecorder()
	h.extendTrial(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200. body=%s", rr.Code, rr.Body.String())
	}

	var got domain.Subscription
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Status != domain.SubscriptionTrialing {
		t.Errorf("status should remain trialing, got %q", got.Status)
	}
	if got.TrialEndAt == nil || !got.TrialEndAt.Equal(newEnd) {
		t.Errorf("trial_end_at: got %v, want %v", got.TrialEndAt, newEnd)
	}

	ev, ok := dispatcher.firstOfType(domain.EventSubscriptionTrialExtended)
	if !ok {
		t.Fatalf("expected %s, got types=%v", domain.EventSubscriptionTrialExtended, eventTypes(dispatcher.events))
	}
	if ev.payload["triggered_by"] != "operator" {
		t.Errorf("triggered_by: got %v, want operator", ev.payload["triggered_by"])
	}
}

// TestHandler_ExtendTrial_RejectsShorten locks in the "extend-only"
// guard: passing a trial_end before the existing trial_end_at returns
// non-2xx — operators must use end-trial to shorten.
func TestHandler_ExtendTrial_RejectsShorten(t *testing.T) {
	ctx := context.Background()
	tenantID := "t1"
	store := newMemStore()
	now := time.Now().UTC()
	trialEnd := now.Add(14 * 24 * time.Hour)
	periodStart := now
	periodEnd := trialEnd.Add(30 * 24 * time.Hour)
	sub, err := store.Create(ctx, tenantID, domain.Subscription{
		Code: "sub-shrink", CustomerID: "cus_1",
		Status:                    domain.SubscriptionTrialing,
		BillingTime:               domain.BillingTimeCalendar,
		TrialStartAt:              &periodStart,
		TrialEndAt:                &trialEnd,
		CurrentBillingPeriodStart: &periodStart,
		CurrentBillingPeriodEnd:   &periodEnd,
		Items:                     []domain.SubscriptionItem{{PlanID: "plan_pro", Quantity: 1}},
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	svc := NewService(store, nil)
	h := NewHandler(svc)

	earlier := trialEnd.Add(-time.Hour) // before current trial_end_at
	body, _ := json.Marshal(map[string]any{"trial_end": earlier.Format(time.RFC3339)})
	req := httptest.NewRequest(http.MethodPost, "/subscriptions/"+sub.ID+"/extend-trial", bytes.NewReader(body))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", sub.ID)
	req = req.WithContext(context.WithValue(
		context.WithValue(ctx, chi.RouteCtxKey, rctx),
		auth.TestTenantIDKey(), tenantID,
	))
	rr := httptest.NewRecorder()
	h.extendTrial(rr, req)
	if rr.Code == http.StatusOK {
		t.Errorf("expected non-2xx when shrinking trial, got %d. body=%s", rr.Code, rr.Body.String())
	}
}

// TestHandler_Create_RefusesEmptyOrNullStatus: status takes only "draft".
// "status": "" and "status": null are not a way to say "start now"; a caller
// whose template left the value unset is told (422 on status), not started
// and billed. Leaving the field out still starts the subscription.
func TestHandler_Create_RefusesEmptyOrNullStatus(t *testing.T) {
	ctx := context.WithValue(context.Background(), auth.TestTenantIDKey(), "t1")
	store := newMemStore()
	h := NewHandler(NewService(store, nil))

	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/subscriptions", bytes.NewBufferString(body)).WithContext(ctx)
		rr := httptest.NewRecorder()
		h.create(rr, req)
		return rr
	}
	const base = `"code":"%s","display_name":"S","customer_id":"cus_1","items":[{"plan_id":"pln_1"}]`

	for _, tc := range []struct{ name, status string }{
		{"empty", `""`},
		{"null", `null`},
	} {
		rr := post(fmt.Sprintf(`{`+base+`,"status":%s}`, "sub-"+tc.name, tc.status))
		var body struct {
			Error struct {
				Param string `json:"param"`
			} `json:"error"`
		}
		_ = json.Unmarshal(rr.Body.Bytes(), &body)
		if rr.Code != http.StatusUnprocessableEntity || body.Error.Param != "status" {
			t.Errorf("status %s: got %d %s, want 422 naming status", tc.status, rr.Code, rr.Body.String())
		}
	}
	if len(store.subs) != 0 {
		t.Fatalf("refused creates wrote %d subscriptions, want none", len(store.subs))
	}

	rr := post(fmt.Sprintf(`{`+base+`}`, "sub-absent"))
	if rr.Code != http.StatusCreated {
		t.Fatalf("status left out: got %d %s, want 201", rr.Code, rr.Body.String())
	}
	var sub domain.Subscription
	_ = json.Unmarshal(rr.Body.Bytes(), &sub)
	if sub.Status != domain.SubscriptionActive {
		t.Fatalf("status left out: got %q, want active", sub.Status)
	}
}

// eventItem checks an item event against the payload rule (ADR-131) and
// returns the item it names from the subscription it carries.
func eventItem(t *testing.T, eventType string, data map[string]any, itemID string) map[string]any {
	t.Helper()
	if err := domain.CheckWebhookData(eventType, data); err != nil {
		t.Fatal(err)
	}
	if data["item_id"] != itemID {
		t.Fatalf("item_id: got %v, want %q", data["item_id"], itemID)
	}
	sub := data[domain.WebhookResourceSubscription].(map[string]any)
	items, _ := sub["items"].([]any)
	for _, it := range items {
		if m := it.(map[string]any); m["id"] == itemID {
			return m
		}
	}
	t.Fatalf("item %s is not in the subscription's items: %v", itemID, items)
	return nil
}
