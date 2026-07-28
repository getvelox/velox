package invoice

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/tax"
)

type memStore struct {
	invoices    map[string]domain.Invoice
	lineItems   map[string][]domain.InvoiceLineItem
	failNotedPI map[string]string // invoice ID -> PI whose failure notifications fired
}

func (m *memStore) ActionRequiredObsolete(_ context.Context, _, invoiceNumber string) (bool, error) {
	for _, inv := range m.invoices {
		if inv.InvoiceNumber == invoiceNumber {
			settled := inv.PaymentStatus == domain.PaymentSucceeded ||
				inv.Status == domain.InvoicePaid || inv.Status == domain.InvoiceVoided ||
				inv.Status == domain.InvoiceUncollectible
			return settled, nil
		}
	}
	return true, nil
}

func newMemStore() *memStore {
	return &memStore{
		invoices:    make(map[string]domain.Invoice),
		lineItems:   make(map[string][]domain.InvoiceLineItem),
		failNotedPI: make(map[string]string),
	}
}

func (m *memStore) Create(_ context.Context, tenantID string, inv domain.Invoice) (domain.Invoice, error) {
	inv.ID = fmt.Sprintf("vlx_inv_%d", len(m.invoices)+1)
	inv.TenantID = tenantID
	now := time.Now().UTC()
	inv.CreatedAt = now
	inv.UpdatedAt = now
	m.invoices[inv.ID] = inv
	return inv, nil
}

// CreateAudited mirrors the store contract (ADR-090): emit runs after the row
// exists and before "commit"; an emit error rolls the write back and surfaces.
// The fake has no tx, so the closure gets a nil *sql.Tx — enough to prove the
// service passes an emission and propagates its error.
func (m *memStore) CreateAudited(ctx context.Context, tenantID string, inv domain.Invoice, emit func(tx *sql.Tx, out domain.Invoice) error) (domain.Invoice, error) {
	out, err := m.Create(ctx, tenantID, inv)
	if err != nil {
		return domain.Invoice{}, err
	}
	if emit != nil {
		if err := emit(nil, out); err != nil {
			delete(m.invoices, out.ID) // shared fate: roll the write back
			return domain.Invoice{}, fmt.Errorf("audit emission: %w", err)
		}
	}
	return out, nil
}

func (m *memStore) Get(_ context.Context, tenantID, id string) (domain.Invoice, error) {
	inv, ok := m.invoices[id]
	if !ok || inv.TenantID != tenantID {
		return domain.Invoice{}, errs.ErrNotFound
	}
	return inv, nil
}

func (m *memStore) GetByProrationSource(_ context.Context, tenantID, subscriptionID, subscriptionItemID string, changeType domain.ItemChangeType, changeAt time.Time) (domain.Invoice, error) {
	for _, inv := range m.invoices {
		if inv.TenantID == tenantID && inv.SubscriptionID == subscriptionID &&
			inv.SourceSubscriptionItemID == subscriptionItemID &&
			inv.SourceChangeType == changeType &&
			inv.SourcePlanChangedAt != nil && inv.SourcePlanChangedAt.Equal(changeAt) {
			return inv, nil
		}
	}
	return domain.Invoice{}, errs.ErrNotFound
}

func (m *memStore) GetByNumber(_ context.Context, tenantID, number string) (domain.Invoice, error) {
	for _, inv := range m.invoices {
		if inv.TenantID == tenantID && inv.InvoiceNumber == number {
			return inv, nil
		}
	}
	return domain.Invoice{}, errs.ErrNotFound
}

func (m *memStore) List(_ context.Context, filter ListFilter) ([]domain.Invoice, int, error) {
	var result []domain.Invoice
	for _, inv := range m.invoices {
		if inv.TenantID != filter.TenantID {
			continue
		}
		if filter.Status != "" && string(inv.Status) != filter.Status {
			continue
		}
		result = append(result, inv)
	}
	return result, len(result), nil
}

func (m *memStore) GetOutstandingBalance(_ context.Context, tenantID, customerID string) (OutstandingBalance, error) {
	var out OutstandingBalance
	for _, inv := range m.invoices {
		if inv.TenantID != tenantID || inv.CustomerID != customerID {
			continue
		}
		if inv.Status == domain.InvoiceVoided || inv.Status == domain.InvoiceUncollectible || inv.Status == domain.InvoiceDraft {
			continue
		}
		if inv.PaymentStatus == domain.PaymentPending || inv.PaymentStatus == domain.PaymentFailed || inv.PaymentStatus == domain.PaymentUnknown {
			out.TotalCents += inv.AmountDueCents
			out.UnpaidCount++
		}
	}
	return out, nil
}

func (m *memStore) SetPublicToken(_ context.Context, tenantID, invoiceID, token string) error {
	inv, ok := m.invoices[invoiceID]
	if !ok || inv.TenantID != tenantID {
		return errs.ErrNotFound
	}
	if inv.Status == domain.InvoiceDraft {
		return errs.ErrNotFound
	}
	inv.PublicToken = token
	inv.UpdatedAt = time.Now().UTC()
	m.invoices[invoiceID] = inv
	return nil
}

func (m *memStore) GetByPublicToken(_ context.Context, token string) (domain.Invoice, bool, error) {
	if token == "" {
		return domain.Invoice{}, false, errs.ErrNotFound
	}
	for _, inv := range m.invoices {
		if inv.PublicToken == token {
			return inv, false, nil
		}
	}
	return domain.Invoice{}, false, errs.ErrNotFound
}

func (m *memStore) GetByStripeInvoiceID(_ context.Context, tenantID, stripeInvoiceID string) (domain.Invoice, error) {
	if stripeInvoiceID == "" {
		return domain.Invoice{}, errs.ErrNotFound
	}
	for _, inv := range m.invoices {
		if inv.TenantID == tenantID && inv.StripeInvoiceID == stripeInvoiceID {
			return inv, nil
		}
	}
	return domain.Invoice{}, errs.ErrNotFound
}

func (m *memStore) FindBaseInvoiceForPeriod(_ context.Context, tenantID, subscriptionID string, periodStart time.Time) (domain.Invoice, error) {
	var best domain.Invoice
	found := false
	for _, inv := range m.invoices {
		if inv.TenantID != tenantID || inv.SubscriptionID != subscriptionID {
			continue
		}
		if inv.Status == domain.InvoiceVoided || inv.Status == domain.InvoiceUncollectible {
			continue
		}
		for _, li := range m.lineItems[inv.ID] {
			if li.LineType == domain.LineTypeBaseFee && li.BillingPeriodStart != nil && li.BillingPeriodStart.Equal(periodStart) {
				if !found || inv.CreatedAt.After(best.CreatedAt) {
					best = inv
					found = true
				}
				break
			}
		}
	}
	if !found {
		return domain.Invoice{}, errs.ErrNotFound
	}
	return best, nil
}

func (m *memStore) RecordChargeAttempt(_ context.Context, _ string, _ domain.InvoiceChargeAttempt) error {
	return nil
}

func (m *memStore) ListChargeAttemptsByInvoice(_ context.Context, _, _ string) ([]domain.InvoiceChargeAttempt, error) {
	return nil, nil
}

func (m *memStore) LatestThresholdPeriodEnd(_ context.Context, tenantID, subscriptionID string, periodStart, periodEnd time.Time) (time.Time, error) {
	var latest time.Time
	found := false
	for _, inv := range m.invoices {
		if inv.TenantID != tenantID || inv.SubscriptionID != subscriptionID {
			continue
		}
		if inv.BillingReason != domain.BillingReasonThreshold {
			continue
		}
		if inv.Status == domain.InvoiceVoided || inv.Status == domain.InvoiceUncollectible {
			continue
		}
		if inv.BillingPeriodStart.Before(periodStart) || !inv.BillingPeriodStart.Before(periodEnd) {
			continue
		}
		if !found || inv.BillingPeriodEnd.After(latest) {
			latest = inv.BillingPeriodEnd
			found = true
		}
	}
	if !found {
		return time.Time{}, errs.ErrNotFound
	}
	return latest, nil
}

func (m *memStore) UpdateStatus(_ context.Context, tenantID, id string, status domain.InvoiceStatus) (domain.Invoice, error) {
	inv, ok := m.invoices[id]
	if !ok || inv.TenantID != tenantID {
		return domain.Invoice{}, errs.ErrNotFound
	}
	inv.Status = status
	inv.UpdatedAt = time.Now().UTC()
	m.invoices[id] = inv
	return inv, nil
}

// Prior variant: the fake's "prior" is the current in-memory status —
// exact, since the fake is single-threaded.
func (m *memStore) UpdateStatusWithReversalPrior(ctx context.Context, tenantID, id string, status domain.InvoiceStatus, reverseFn func(tx *sql.Tx, prior domain.InvoiceStatus) error) (domain.Invoice, error) {
	prior := domain.InvoiceStatus("")
	if inv, ok := m.invoices[id]; ok {
		prior = inv.Status
	}
	var fn func(tx *sql.Tx) error
	if reverseFn != nil {
		fn = func(tx *sql.Tx) error { return reverseFn(tx, prior) }
	}
	return m.UpdateStatusWithReversal(ctx, tenantID, id, status, fn)
}

func (m *memStore) UpdateStatusWithReversal(ctx context.Context, tenantID, id string, status domain.InvoiceStatus, reverseFn func(tx *sql.Tx) error) (domain.Invoice, error) {
	prev, ok := m.invoices[id]
	if !ok || prev.TenantID != tenantID {
		return domain.Invoice{}, errs.ErrNotFound
	}
	inv, err := m.UpdateStatus(ctx, tenantID, id, status)
	if err != nil {
		return domain.Invoice{}, err
	}
	if reverseFn != nil {
		// In-memory: no real tx — pass nil and, on error, restore the pre-flip
		// invoice so the test store mirrors the all-or-nothing store contract.
		if err := reverseFn(nil); err != nil {
			m.invoices[id] = prev
			return domain.Invoice{}, err
		}
	}
	return inv, nil
}

func (m *memStore) FinalizeWithDates(_ context.Context, tenantID, id string, issuedAt, dueAt time.Time) (domain.Invoice, error) {
	inv, ok := m.invoices[id]
	if !ok || inv.TenantID != tenantID {
		return domain.Invoice{}, errs.ErrNotFound
	}
	inv.Status = domain.InvoiceFinalized
	inv.IssuedAt = &issuedAt
	inv.DueAt = &dueAt
	inv.UpdatedAt = time.Now().UTC()
	m.invoices[id] = inv
	return inv, nil
}

func (m *memStore) UpdatePayment(_ context.Context, tenantID, id string, ps domain.InvoicePaymentStatus, stripeID, errMsg string, paidAt *time.Time) (domain.Invoice, error) {
	inv, ok := m.invoices[id]
	if !ok || inv.TenantID != tenantID {
		return domain.Invoice{}, errs.ErrNotFound
	}
	inv.PaymentStatus = ps
	inv.StripePaymentIntentID = stripeID
	inv.LastPaymentError = errMsg
	inv.PaidAt = paidAt
	m.invoices[id] = inv
	return inv, nil
}

func (m *memStore) MarkPaymentFailedReportingTransition(_ context.Context, tenantID, id, piID, errMsg string) (domain.Invoice, bool, error) {
	inv, ok := m.invoices[id]
	if !ok || inv.TenantID != tenantID {
		return domain.Invoice{}, false, errs.ErrNotFound
	}
	if inv.Status == domain.InvoicePaid || inv.PaymentStatus == domain.PaymentSucceeded {
		return inv, false, nil
	}
	if m.failNotedPI == nil {
		m.failNotedPI = make(map[string]string)
	}
	first := m.failNotedPI[id] != piID
	inv.PaymentStatus = domain.PaymentFailed
	inv.StripePaymentIntentID = piID
	inv.LastPaymentError = errMsg
	m.invoices[id] = inv
	m.failNotedPI[id] = piID
	return inv, first, nil
}

func (m *memStore) SetNoPMNotifiedAt(_ context.Context, tenantID, id string, at time.Time) error {
	inv, ok := m.invoices[id]
	if !ok || inv.TenantID != tenantID {
		return errs.ErrNotFound
	}
	inv.NoPMNotifiedAt = &at
	m.invoices[id] = inv
	return nil
}

func (m *memStore) MarkPaid(_ context.Context, tenantID, id, stripeID string, paidAt time.Time) (domain.Invoice, error) {
	inv, ok := m.invoices[id]
	if !ok || inv.TenantID != tenantID {
		return domain.Invoice{}, errs.ErrNotFound
	}
	// Mirror PostgresStore.MarkPaid's state-machine guard (added
	// 2026-05-22): reject draft (finalize first) and voided
	// (terminal); reject tax_status != ok (authoritative amounts).
	switch inv.Status {
	case domain.InvoiceFinalized, domain.InvoiceUncollectible, domain.InvoicePaid:
		// ok
	default:
		return domain.Invoice{}, errs.InvalidState("cannot mark invoice paid from status " + string(inv.Status))
	}
	if inv.TaxStatus != "" && inv.TaxStatus != domain.InvoiceTaxOK {
		return domain.Invoice{}, errs.InvalidState("cannot mark invoice paid with tax_status=" + string(inv.TaxStatus))
	}
	inv.Status = domain.InvoicePaid
	inv.PaymentStatus = domain.PaymentSucceeded
	inv.StripePaymentIntentID = stripeID
	inv.AmountPaidCents = inv.AmountDueCents
	inv.AmountDueCents = 0
	inv.PaidAt = &paidAt
	m.invoices[id] = inv
	return inv, nil
}

func (m *memStore) ApplyCreditNote(_ context.Context, tenantID, id string, amountCents int64) (domain.Invoice, error) {
	inv, ok := m.invoices[id]
	if !ok || inv.TenantID != tenantID {
		return domain.Invoice{}, errs.ErrNotFound
	}
	inv.AmountDueCents -= amountCents
	if inv.AmountDueCents < 0 {
		inv.AmountDueCents = 0
	}
	m.invoices[id] = inv
	return inv, nil
}

func (m *memStore) CreateLineItem(_ context.Context, tenantID string, item domain.InvoiceLineItem) (domain.InvoiceLineItem, error) {
	item.ID = fmt.Sprintf("vlx_ili_%d", len(m.lineItems[item.InvoiceID])+1)
	item.TenantID = tenantID
	m.lineItems[item.InvoiceID] = append(m.lineItems[item.InvoiceID], item)
	return item, nil
}

func (m *memStore) ListLineItems(_ context.Context, _, invoiceID string) ([]domain.InvoiceLineItem, error) {
	return m.lineItems[invoiceID], nil
}

func (m *memStore) AddLineItemAtomic(_ context.Context, tenantID, invoiceID string, item domain.InvoiceLineItem) (domain.InvoiceLineItem, domain.Invoice, error) {
	inv, ok := m.invoices[invoiceID]
	if !ok || inv.TenantID != tenantID {
		return domain.InvoiceLineItem{}, domain.Invoice{}, errs.ErrNotFound
	}
	if inv.Status != domain.InvoiceDraft {
		return domain.InvoiceLineItem{}, domain.Invoice{},
			fmt.Errorf("can only add line items to draft invoices, current status: %s", inv.Status)
	}

	item.InvoiceID = invoiceID
	item.TenantID = tenantID
	item.Currency = inv.Currency
	item.ID = fmt.Sprintf("vlx_ili_%d", len(m.lineItems[invoiceID])+1)
	m.lineItems[invoiceID] = append(m.lineItems[invoiceID], item)

	var subtotal int64
	for _, li := range m.lineItems[invoiceID] {
		subtotal += li.AmountCents
	}
	total := subtotal + inv.TaxAmountCents - inv.DiscountCents
	amountDue := total - inv.AmountPaidCents - inv.CreditsAppliedCents
	if amountDue < 0 {
		amountDue = 0
	}
	inv.SubtotalCents = subtotal
	inv.TotalAmountCents = total
	inv.AmountDueCents = amountDue
	inv.UpdatedAt = time.Now().UTC()
	m.invoices[invoiceID] = inv
	return item, inv, nil
}

func (m *memStore) UpdateTaxAtomic(_ context.Context, tenantID, invoiceID string, update domain.InvoiceTaxRetryUpdate, lineItems []domain.InvoiceLineItem) (domain.Invoice, error) {
	inv, ok := m.invoices[invoiceID]
	if !ok || inv.TenantID != tenantID {
		return domain.Invoice{}, errs.ErrNotFound
	}
	if inv.Status != domain.InvoiceDraft {
		return domain.Invoice{}, errs.InvalidState("not draft")
	}
	if inv.TaxStatus != domain.InvoiceTaxPending && inv.TaxStatus != domain.InvoiceTaxFailed {
		return domain.Invoice{}, errs.InvalidState("tax not retryable")
	}
	byID := make(map[string]domain.InvoiceLineItem, len(lineItems))
	for _, li := range lineItems {
		byID[li.ID] = li
	}
	for i, existing := range m.lineItems[invoiceID] {
		if updated, ok := byID[existing.ID]; ok {
			m.lineItems[invoiceID][i].TaxRate = updated.TaxRate
			m.lineItems[invoiceID][i].TaxAmountCents = updated.TaxAmountCents
			m.lineItems[invoiceID][i].TotalAmountCents = updated.TotalAmountCents
		}
	}
	inv.TaxAmountCents = update.TaxAmountCents
	inv.TaxRate = update.TaxRate
	inv.TaxName = update.TaxName
	inv.TaxCountry = update.TaxCountry
	inv.TaxID = update.TaxID
	inv.TaxProvider = update.TaxProvider
	inv.TaxCalculationID = update.TaxCalculationID
	inv.TaxReverseCharge = update.TaxReverseCharge
	inv.TaxExemptReason = update.TaxExemptReason
	inv.TaxStatus = update.TaxStatus
	inv.TaxDeferredAt = update.TaxDeferredAt
	inv.TaxPendingReason = update.TaxPendingReason
	inv.TaxErrorCode = update.TaxErrorCode
	inv.TaxNextRetryAt = update.TaxNextRetryAt
	if !update.InitialAttempt {
		inv.TaxRetryCount++
	}
	inv.TotalAmountCents = update.TotalAmountCents
	due := inv.TotalAmountCents - inv.AmountPaidCents - inv.CreditsAppliedCents
	if due < 0 {
		due = 0
	}
	inv.AmountDueCents = due
	inv.UpdatedAt = time.Now().UTC()
	m.invoices[invoiceID] = inv
	return inv, nil
}

// TestRetryProviderConfigErrors_FlushesStuckRows covers ADR-019.
// After a successful Stripe re-connect, the service should flush
// every invoice in the (tenant, livemode) partition that was
// stuck on provider_not_configured / provider_auth — and skip
// rows in unrelated states.
func TestRetryProviderConfigErrors_FlushesStuckRows(t *testing.T) {
	store := newMemStore()
	now := time.Now().UTC()

	// Three invoices: stuck-provider-not-configured, stuck-provider-auth,
	// and tax-OK (already healthy — should be skipped).
	store.invoices["inv_stuck_a"] = domain.Invoice{
		ID: "inv_stuck_a", TenantID: "t1", CustomerID: "cus_a",
		Status: domain.InvoiceDraft,
		TaxFacts: domain.TaxFacts{
			TaxStatus: domain.InvoiceTaxPending, TaxErrorCode: "provider_not_configured",
		},
		BillingReason: "subscription_cycle",
		Currency:      "USD", CreatedAt: now,
	}
	store.invoices["inv_stuck_b"] = domain.Invoice{
		ID: "inv_stuck_b", TenantID: "t1", CustomerID: "cus_b",
		Status: domain.InvoiceDraft,
		TaxFacts: domain.TaxFacts{
			TaxStatus: domain.InvoiceTaxFailed, TaxErrorCode: "provider_auth",
		},
		BillingReason: "subscription_cycle",
		Currency:      "USD", CreatedAt: now,
	}
	store.invoices["inv_healthy"] = domain.Invoice{
		ID: "inv_healthy", TenantID: "t1", CustomerID: "cus_c",
		Status: domain.InvoiceDraft, TaxFacts: domain.TaxFacts{TaxStatus: domain.InvoiceTaxOK},
		BillingReason: "subscription_cycle", Currency: "USD", CreatedAt: now,
	}

	// Stub TaxRetrier that just flips status to ok — equivalent to
	// the engine succeeding after fresh creds land.
	stub := &stubTaxRetrier{store: store}
	svc := NewService(store, nil, nil)
	svc.SetTaxRetrier(stub)

	processed, errs := svc.RetryProviderConfigErrors(context.Background(), "t1", false)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if processed != 2 {
		t.Errorf("processed: got %d, want 2 (the two stuck rows)", processed)
	}
	if stub.calls != 2 {
		t.Errorf("RetryTaxForInvoice calls: got %d, want 2", stub.calls)
	}
	// Healthy invoice was never iterated.
	for _, id := range stub.calledOn {
		if id == "inv_healthy" {
			t.Errorf("retry should not touch healthy invoice")
		}
	}
}

// stubTaxRetrier mimics a successful engine retry: flips
// tax_status to ok on the row.
type stubTaxRetrier struct {
	store    *memStore
	calls    int
	calledOn []string
}

func (s *stubTaxRetrier) RetryTaxForInvoice(_ context.Context, tenantID, invoiceID string) (domain.Invoice, error) {
	s.calls++
	s.calledOn = append(s.calledOn, invoiceID)
	inv, ok := s.store.invoices[invoiceID]
	if !ok || inv.TenantID != tenantID {
		return domain.Invoice{}, errs.ErrNotFound
	}
	inv.TaxStatus = domain.InvoiceTaxOK
	inv.TaxPendingReason = ""
	inv.TaxErrorCode = ""
	s.store.invoices[invoiceID] = inv
	return inv, nil
}

// ComputeTaxForInvoice mirrors the finalize-time compute: resolve tax to ok
// on a draft regardless of incoming tax_status. The stub leaves the amount
// untouched (the real engine runs ApplyTaxToLineItems); tests that assert on
// computed tax amounts use the real engine via integration coverage.
func (s *stubTaxRetrier) ComputeTaxForInvoice(_ context.Context, tenantID, invoiceID string) (domain.Invoice, error) {
	inv, ok := s.store.invoices[invoiceID]
	if !ok || inv.TenantID != tenantID {
		return domain.Invoice{}, errs.ErrNotFound
	}
	inv.TaxStatus = domain.InvoiceTaxOK
	s.store.invoices[invoiceID] = inv
	return inv, nil
}

// ListCustomerDataInvalidErrors — per-customer flush stub mirroring
// ListProviderConfigErrors. Filters on customer_id + status=draft +
// tax_status pending|failed + tax_error_code=customer_data_invalid.
func (m *memStore) ListCustomerDataInvalidErrors(_ context.Context, tenantID, customerID string) ([]domain.Invoice, error) {
	var out []domain.Invoice
	for _, inv := range m.invoices {
		if inv.TenantID != tenantID || inv.CustomerID != customerID {
			continue
		}
		if inv.Status != domain.InvoiceDraft {
			continue
		}
		if inv.TaxStatus != domain.InvoiceTaxPending && inv.TaxStatus != domain.InvoiceTaxFailed {
			continue
		}
		if inv.TaxErrorCode != "customer_data_invalid" {
			continue
		}
		out = append(out, inv)
	}
	return out, nil
}

func (m *memStore) ListProviderConfigErrors(_ context.Context, tenantID string, _ bool) ([]domain.Invoice, error) {
	// memStore doesn't track livemode (mock fixtures are single-mode);
	// the real PostgresStore impl filters by livemode in the WHERE.
	var out []domain.Invoice
	for _, inv := range m.invoices {
		if inv.TenantID != tenantID {
			continue
		}
		if inv.Status != domain.InvoiceDraft {
			continue
		}
		if inv.TaxStatus != domain.InvoiceTaxPending && inv.TaxStatus != domain.InvoiceTaxFailed {
			continue
		}
		if inv.TaxErrorCode != "provider_not_configured" && inv.TaxErrorCode != "provider_auth" {
			continue
		}
		out = append(out, inv)
	}
	return out, nil
}

// ListPendingTaxRetryForClock — ADR-029 Phase 2 stub for the in-memory
// test store. The narrow service tests in this file don't exercise the
// catchup path; integration tests cover it. Returning nothing here
// satisfies the interface without faking sub→clock joins in memory.
func (m *memStore) ListPendingTaxRetryForClock(_ context.Context, _, _ string, _ []string, _, _ int) ([]domain.Invoice, error) {
	return nil, nil
}

func (m *memStore) ListPendingTaxRetry(_ context.Context, batch int, retryableCodes []string, maxAttempts int, _ bool) ([]domain.Invoice, error) {
	if batch <= 0 {
		batch = 50
	}
	allowed := make(map[string]struct{}, len(retryableCodes))
	for _, c := range retryableCodes {
		allowed[c] = struct{}{}
	}
	now := time.Now().UTC()
	var out []domain.Invoice
	for _, inv := range m.invoices {
		if inv.Status != domain.InvoiceDraft {
			continue
		}
		if inv.TaxStatus != domain.InvoiceTaxPending && inv.TaxStatus != domain.InvoiceTaxFailed {
			continue
		}
		if _, ok := allowed[inv.TaxErrorCode]; !ok {
			continue
		}
		if inv.TaxRetryCount >= maxAttempts {
			continue
		}
		if inv.TaxNextRetryAt != nil && inv.TaxNextRetryAt.After(now) {
			continue
		}
		out = append(out, inv)
		if len(out) >= batch {
			break
		}
	}
	return out, nil
}

func (m *memStore) ListPendingTaxCommit(_ context.Context, batch int, _ bool) ([]domain.Invoice, error) {
	if batch <= 0 {
		batch = 50
	}
	var out []domain.Invoice
	for _, inv := range m.invoices {
		if inv.Status != domain.InvoiceFinalized || inv.TaxStatus != domain.InvoiceTaxOK {
			continue
		}
		if inv.TaxProvider != "stripe_tax" || inv.TaxCalculationID == "" || inv.TaxTransactionID != "" {
			continue
		}
		out = append(out, inv)
		if len(out) >= batch {
			break
		}
	}
	return out, nil
}

func (m *memStore) ListPendingTaxReversal(_ context.Context, batch int, _ bool) ([]domain.Invoice, error) {
	if batch <= 0 {
		batch = 50
	}
	var out []domain.Invoice
	for _, inv := range m.invoices {
		if inv.Status != domain.InvoiceVoided && inv.Status != domain.InvoiceUncollectible {
			continue
		}
		if inv.TaxProvider != "stripe_tax" || inv.TaxTransactionID == "" || inv.TaxReversedAt != nil {
			continue
		}
		if inv.IsSimulated {
			continue
		}
		out = append(out, inv)
		if len(out) >= batch {
			break
		}
	}
	return out, nil
}

func (m *memStore) MarkTaxReversed(_ context.Context, tenantID, id string) error {
	// Match the real store: the UPDATE is RLS-scoped + guarded by
	// `tax_reversed_at IS NULL`, so a missing/wrong-tenant id or an
	// already-stamped row is a silent no-op (zero rows), never an error.
	inv, ok := m.invoices[id]
	if !ok || inv.TenantID != tenantID || inv.TaxReversedAt != nil {
		return nil
	}
	now := time.Now().UTC()
	inv.TaxReversedAt = &now
	m.invoices[id] = inv
	return nil
}

func (m *memStore) CreateWithLineItems(_ context.Context, tenantID string, inv domain.Invoice, items []domain.InvoiceLineItem) (domain.Invoice, error) {
	// Emulate the proration dedup partial unique index. Without this, tests
	// that exercise retry-after-partial-failure paths silently double-insert
	// rows the real DB would have rejected.
	if inv.SourcePlanChangedAt != nil {
		for _, existing := range m.invoices {
			if existing.TenantID == tenantID && existing.SubscriptionID == inv.SubscriptionID &&
				existing.SourcePlanChangedAt != nil && existing.SourcePlanChangedAt.Equal(*inv.SourcePlanChangedAt) {
				return domain.Invoice{}, errs.ErrAlreadyExists
			}
		}
	}

	inv.ID = fmt.Sprintf("vlx_inv_%d", len(m.invoices)+1)
	inv.TenantID = tenantID
	now := time.Now().UTC()
	inv.CreatedAt = now
	inv.UpdatedAt = now
	m.invoices[inv.ID] = inv
	for _, item := range items {
		item.InvoiceID = inv.ID
		item.TenantID = tenantID
		m.lineItems[inv.ID] = append(m.lineItems[inv.ID], item)
	}
	return inv, nil
}

func (m *memStore) CreateWithLineItemsAudited(ctx context.Context, tenantID string, inv domain.Invoice, items []domain.InvoiceLineItem, emit func(tx *sql.Tx, out domain.Invoice) error) (domain.Invoice, error) {
	out, err := m.CreateWithLineItems(ctx, tenantID, inv, items)
	if err != nil {
		return domain.Invoice{}, err
	}
	if emit != nil {
		if err := emit(nil, out); err != nil {
			delete(m.invoices, out.ID) // shared fate: roll the write back
			delete(m.lineItems, out.ID)
			return domain.Invoice{}, fmt.Errorf("audit emission: %w", err)
		}
	}
	return out, nil
}

func (m *memStore) AddLineItemAtomicAudited(ctx context.Context, tenantID, invoiceID string, item domain.InvoiceLineItem, emit func(tx *sql.Tx, item domain.InvoiceLineItem, inv domain.Invoice) error) (domain.InvoiceLineItem, domain.Invoice, error) {
	before := m.invoices[invoiceID]
	beforeItems := len(m.lineItems[invoiceID])
	out, inv, err := m.AddLineItemAtomic(ctx, tenantID, invoiceID, item)
	if err != nil {
		return domain.InvoiceLineItem{}, domain.Invoice{}, err
	}
	if emit != nil {
		if err := emit(nil, out, inv); err != nil {
			// shared fate: undo the line insert AND the totals rewrite
			m.lineItems[invoiceID] = m.lineItems[invoiceID][:beforeItems]
			m.invoices[invoiceID] = before
			return domain.InvoiceLineItem{}, domain.Invoice{}, fmt.Errorf("audit emission: %w", err)
		}
	}
	return out, inv, nil
}

func (m *memStore) ClaimAutoCharge(_ context.Context, _, id string) (bool, error) {
	inv, ok := m.invoices[id]
	if !ok {
		return false, errs.ErrNotFound
	}
	return inv.AutoChargePending && inv.PaymentStatus == domain.PaymentPending &&
		inv.Status == domain.InvoiceFinalized && inv.AmountDueCents > 0, nil
}

func (m *memStore) ClaimChargeForManualCollect(_ context.Context, _, id string) (bool, error) {
	inv, ok := m.invoices[id]
	if !ok {
		return false, errs.ErrNotFound
	}
	// Mirrors claimChargeLease's predicate (sans the lease window, which
	// memStore doesn't model): finalized, pending/failed, owing.
	return (inv.PaymentStatus == domain.PaymentPending || inv.PaymentStatus == domain.PaymentFailed) &&
		inv.Status == domain.InvoiceFinalized && inv.AmountDueCents > 0, nil
}

func (m *memStore) ReleaseAutoChargeClaim(_ context.Context, _, _ string) error { return nil }

func (m *memStore) SetAutoChargePending(_ context.Context, _, id string, pending bool) error {
	inv, ok := m.invoices[id]
	if !ok {
		return errs.ErrNotFound
	}
	inv.AutoChargePending = pending
	m.invoices[id] = inv
	return nil
}

func (m *memStore) ListAutoChargePending(_ context.Context, _ int) ([]domain.Invoice, error) {
	var result []domain.Invoice
	for _, inv := range m.invoices {
		if inv.AutoChargePending {
			result = append(result, inv)
		}
	}
	return result, nil
}

// memNumberer is a deterministic in-memory InvoiceNumberer for tests.
// Hands out VLX-000001, VLX-000002, ... so assertions on invoice numbers
// don't depend on clock or DB state.
type memNumberer struct {
	next int
}

func newMemNumberer() *memNumberer { return &memNumberer{} }

func (m *memNumberer) NextInvoiceNumber(_ context.Context, _ string) (string, error) {
	m.next++
	return fmt.Sprintf("VLX-%06d", m.next), nil
}

// NextInvoiceNumberTx mirrors NextInvoiceNumber for tx-aware callers.
// The fake ignores the tx since it's in-memory.
func (m *memNumberer) NextInvoiceNumberTx(_ context.Context, _ *sql.Tx, _ string) (string, error) {
	m.next++
	return fmt.Sprintf("VLX-%06d", m.next), nil
}

func TestCreate(t *testing.T) {
	svc := NewService(newMemStore(), nil, newMemNumberer())
	ctx := context.Background()

	t.Run("valid", func(t *testing.T) {
		inv, err := svc.Create(ctx, "t1", CreateInput{
			CustomerID:         "cus_1",
			SubscriptionID:     "sub_1",
			BillingPeriodStart: time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
			BillingPeriodEnd:   time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if inv.Status != domain.InvoiceDraft {
			t.Errorf("got status %q, want draft", inv.Status)
		}
		if inv.PaymentStatus != domain.PaymentPending {
			t.Errorf("got payment_status %q, want pending", inv.PaymentStatus)
		}
		// Canonical uppercase ISO-4217 (matches the tenant default "USD" that
		// analytics/dunning revenue queries filter on); empty-input default.
		if inv.Currency != "USD" {
			t.Errorf("got currency %q, want USD", inv.Currency)
		}
		if inv.NetPaymentTermDays != 30 {
			t.Errorf("got net_payment_term %d, want 30", inv.NetPaymentTermDays)
		}
		if inv.InvoiceNumber == "" {
			t.Error("invoice_number should be generated")
		}
		if inv.IssuedAt == nil {
			t.Error("issued_at should be set")
		}
		if inv.DueAt == nil {
			t.Error("due_at should be set")
		}
	})

	t.Run("missing customer_id", func(t *testing.T) {
		_, err := svc.Create(ctx, "t1", CreateInput{SubscriptionID: "s"})
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("one-off invoice — subscription_id optional", func(t *testing.T) {
		// Operator-issued one-off invoices (composer on the customer page)
		// have no parent subscription. Create must succeed, persist an empty
		// SubscriptionID, and default the billing window to the clock so the
		// NOT NULL period columns get sane values.
		inv, err := svc.Create(ctx, "t1", CreateInput{CustomerID: "c", Currency: "USD"})
		if err != nil {
			t.Fatalf("expected one-off create to succeed, got %v", err)
		}
		if inv.SubscriptionID != "" {
			t.Errorf("subscription_id: got %q, want empty", inv.SubscriptionID)
		}
		if inv.BillingPeriodStart.IsZero() || inv.BillingPeriodEnd.IsZero() {
			t.Errorf("billing period must default to clock now when omitted")
		}
	})
}

func TestFinalizeAndVoid(t *testing.T) {
	svc := NewService(newMemStore(), nil, newMemNumberer())
	ctx := context.Background()

	inv, _ := svc.Create(ctx, "t1", CreateInput{
		CustomerID: "c", SubscriptionID: "s",
		BillingPeriodStart: time.Now(), BillingPeriodEnd: time.Now().AddDate(0, 1, 0),
	})

	t.Run("finalize draft", func(t *testing.T) {
		finalized, err := svc.Finalize(ctx, "t1", inv.ID)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if finalized.Status != domain.InvoiceFinalized {
			t.Errorf("got status %q, want finalized", finalized.Status)
		}
		// Industry-standard hosted_invoice_url: finalize must mint a public
		// token so downstream email CTAs (T0-16) have a target on day one.
		if finalized.PublicToken == "" {
			t.Error("finalize should mint a public_token")
		}
		if !strings.HasPrefix(finalized.PublicToken, PublicTokenPrefix) {
			t.Errorf("public_token %q missing %q prefix", finalized.PublicToken, PublicTokenPrefix)
		}
		// Prefix + 64 hex chars of 32-byte entropy.
		if got, want := len(finalized.PublicToken), len(PublicTokenPrefix)+64; got != want {
			t.Errorf("public_token length = %d, want %d", got, want)
		}
	})

	t.Run("cannot finalize again", func(t *testing.T) {
		_, err := svc.Finalize(ctx, "t1", inv.ID)
		if err == nil {
			t.Fatal("expected error finalizing non-draft")
		}
	})

	t.Run("draft has no public_token", func(t *testing.T) {
		draft, _ := svc.Create(ctx, "t1", CreateInput{
			CustomerID: "c", SubscriptionID: "s2",
			BillingPeriodStart: time.Now(), BillingPeriodEnd: time.Now().AddDate(0, 1, 0),
		})
		if draft.PublicToken != "" {
			t.Errorf("draft invoice should not carry a public_token, got %q", draft.PublicToken)
		}
	})

	t.Run("rotate public token on finalized invoice", func(t *testing.T) {
		// inv is already finalized from the earlier subtest; rotate
		// should replace the token cleanly.
		current, _ := svc.Get(ctx, "t1", inv.ID)
		original := current.PublicToken
		if original == "" {
			t.Fatal("precondition: finalized invoice should have a token")
		}
		newToken, err := GeneratePublicToken()
		if err != nil {
			t.Fatalf("generate: %v", err)
		}
		if err := svc.SetPublicToken(ctx, "t1", inv.ID, newToken); err != nil {
			t.Fatalf("SetPublicToken: %v", err)
		}
		after, _ := svc.Get(ctx, "t1", inv.ID)
		if after.PublicToken == original {
			t.Error("SetPublicToken should have replaced the token")
		}
		if after.PublicToken != newToken {
			t.Errorf("after = %q, want %q", after.PublicToken, newToken)
		}
	})

	t.Run("rotate rejected on draft invoice", func(t *testing.T) {
		draft, _ := svc.Create(ctx, "t1", CreateInput{
			CustomerID: "c", SubscriptionID: "s3",
			BillingPeriodStart: time.Now(), BillingPeriodEnd: time.Now().AddDate(0, 1, 0),
		})
		err := svc.SetPublicToken(ctx, "t1", draft.ID, "vlx_pinv_abc")
		if !errors.Is(err, errs.ErrNotFound) {
			t.Errorf("expected ErrNotFound on draft rotate, got %v", err)
		}
	})

	t.Run("void finalized", func(t *testing.T) {
		voided, err := svc.Void(ctx, "t1", inv.ID)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if voided.Status != domain.InvoiceVoided {
			t.Errorf("got status %q, want voided", voided.Status)
		}
	})
}

// fakeTaxReverser records ReverseTax calls so the void-tax-reverse
// integration can be asserted at unit-test level without a real
// provider.
type fakeTaxReverser struct {
	calls   []tax.ReversalRequest
	failErr error
}

func (f *fakeTaxReverser) ReverseTax(_ context.Context, _ string, req tax.ReversalRequest) (*tax.ReversalResult, error) {
	f.calls = append(f.calls, req)
	if f.failErr != nil {
		return nil, f.failErr
	}
	return &tax.ReversalResult{TransactionID: "tax_rev_" + req.Reference}, nil
}

// TestVoid_ReversesUpstreamTaxTransaction locks in the contract that
// voiding a finalized-but-unpaid invoice with a committed tax_transaction
// fires a full-mode upstream reversal. Without this, the authority
// keeps reporting the tax as collected even though the invoice was
// annulled — Stripe Tax docs: "you must reverse the corresponding tax
// transaction to keep your tax records accurate."
func TestVoid_ReversesUpstreamTaxTransaction(t *testing.T) {
	store := newMemStore()
	svc := NewService(store, nil, newMemNumberer())
	rev := &fakeTaxReverser{}
	svc.SetTaxReverser(rev)
	ctx := context.Background()

	inv, _ := svc.Create(ctx, "t1", CreateInput{
		CustomerID: "c", SubscriptionID: "s",
		BillingPeriodStart: time.Now(), BillingPeriodEnd: time.Now().AddDate(0, 1, 0),
	})
	if _, err := svc.Finalize(ctx, "t1", inv.ID); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	// Simulate a committed Stripe tax_transaction on the invoice (the
	// production path stamps this from billing.Engine.CommitTax).
	stale := store.invoices[inv.ID]
	stale.TaxTransactionID = "tx_committed_at_finalize"
	store.invoices[inv.ID] = stale

	t.Run("reverses tax with full-mode + invoice-scoped Reference", func(t *testing.T) {
		voided, err := svc.Void(ctx, "t1", inv.ID)
		if err != nil {
			t.Fatalf("Void: %v", err)
		}
		if voided.Status != domain.InvoiceVoided {
			t.Errorf("status: got %q want voided", voided.Status)
		}
		if len(rev.calls) != 1 {
			t.Fatalf("ReverseTax calls: got %d want 1", len(rev.calls))
		}
		call := rev.calls[0]
		if call.OriginalTransactionID != "tx_committed_at_finalize" {
			t.Errorf("OriginalTransactionID: got %q want tx_committed_at_finalize", call.OriginalTransactionID)
		}
		if call.Mode != tax.ReversalModeFull {
			t.Errorf("Mode: got %q want full", call.Mode)
		}
		expectedRef := "inv_taxrev_" + inv.ID
		if call.Reference != expectedRef {
			t.Errorf("Reference: got %q want %q (invoice-stable reversal ref, idempotent per invoice)", call.Reference, expectedRef)
		}
	})

	t.Run("upstream failure still voids locally (best-effort)", func(t *testing.T) {
		inv2, _ := svc.Create(ctx, "t1", CreateInput{
			CustomerID: "c", SubscriptionID: "s2",
			BillingPeriodStart: time.Now(), BillingPeriodEnd: time.Now().AddDate(0, 1, 0),
		})
		if _, err := svc.Finalize(ctx, "t1", inv2.ID); err != nil {
			t.Fatalf("finalize: %v", err)
		}
		stale2 := store.invoices[inv2.ID]
		stale2.TaxTransactionID = "tx_committed_at_finalize_2"
		store.invoices[inv2.ID] = stale2
		rev.failErr = errors.New("stripe: tax authority unreachable")
		defer func() { rev.failErr = nil }()

		voided, err := svc.Void(ctx, "t1", inv2.ID)
		if err != nil {
			t.Fatalf("Void should not surface upstream tax errors: %v", err)
		}
		if voided.Status != domain.InvoiceVoided {
			t.Errorf("status: got %q want voided (best-effort tax reversal)", voided.Status)
		}
	})

	t.Run("no upstream call when invoice has no tax transaction", func(t *testing.T) {
		// Manual / none provider: TaxTransactionID empty → reversal skipped.
		inv3, _ := svc.Create(ctx, "t1", CreateInput{
			CustomerID: "c", SubscriptionID: "s3",
			BillingPeriodStart: time.Now(), BillingPeriodEnd: time.Now().AddDate(0, 1, 0),
		})
		if _, err := svc.Finalize(ctx, "t1", inv3.ID); err != nil {
			t.Fatalf("finalize: %v", err)
		}
		before := len(rev.calls)
		if _, err := svc.Void(ctx, "t1", inv3.ID); err != nil {
			t.Fatalf("Void: %v", err)
		}
		if len(rev.calls) != before {
			t.Errorf("ReverseTax should not be called when TaxTransactionID is empty; got %d new calls", len(rev.calls)-before)
		}
	})
}

func TestRecordPayment(t *testing.T) {
	svc := NewService(newMemStore(), nil, newMemNumberer())
	ctx := context.Background()

	inv, _ := svc.Create(ctx, "t1", CreateInput{
		CustomerID: "c", SubscriptionID: "s",
		BillingPeriodStart: time.Now(), BillingPeriodEnd: time.Now().AddDate(0, 1, 0),
	})

	t.Run("success", func(t *testing.T) {
		paid, err := svc.RecordPayment(ctx, "t1", inv.ID, "pi_stripe_123")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if paid.PaymentStatus != domain.PaymentSucceeded {
			t.Errorf("got payment_status %q, want succeeded", paid.PaymentStatus)
		}
		if paid.StripePaymentIntentID != "pi_stripe_123" {
			t.Errorf("got stripe_pi %q, want pi_stripe_123", paid.StripePaymentIntentID)
		}
		if paid.PaidAt == nil {
			t.Error("paid_at should be set")
		}
	})

	t.Run("failure", func(t *testing.T) {
		failed, err := svc.RecordPaymentFailure(ctx, "t1", inv.ID, "pi_stripe_456", "card_declined")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if failed.PaymentStatus != domain.PaymentFailed {
			t.Errorf("got payment_status %q, want failed", failed.PaymentStatus)
		}
		if failed.LastPaymentError != "card_declined" {
			t.Errorf("got error %q, want card_declined", failed.LastPaymentError)
		}
	})
}

// captureAudit + captureEvents let the tests assert that the service-
// layer audit + webhook emit fire on state transitions.
type captureAuditInvoice struct {
	entries []capturedAuditEntryInvoice
}

// LogInTx captures like Log — a nil tx is fine for content-level tests;
// shared-fate semantics are pinned by the real-Postgres integration tests.
func (c *captureAuditInvoice) LogInTx(_ context.Context, _ *sql.Tx, e audit.Entry) error {
	c.entries = append(c.entries, capturedAuditEntryInvoice{
		action: e.Action, resourceID: e.ResourceID, resourceType: e.ResourceType, metadata: e.Metadata,
	})
	return nil
}

type capturedAuditEntryInvoice struct {
	action       string
	resourceID   string
	resourceType string
	metadata     map[string]any
}

func (c *captureAuditInvoice) Log(_ context.Context, _, action, resourceType, resourceID, _ string, metadata map[string]any) error {
	c.entries = append(c.entries, capturedAuditEntryInvoice{
		action: action, resourceType: resourceType, resourceID: resourceID, metadata: metadata,
	})
	return nil
}

type captureEvents struct {
	events []capturedEvent
}

type capturedEvent struct {
	eventType string
	payload   map[string]any
}

func (c *captureEvents) Dispatch(_ context.Context, _, eventType string, payload map[string]any) error {
	c.events = append(c.events, capturedEvent{eventType: eventType, payload: payload})
	return nil
}

// TestMarkUncollectible_ReversesUpstreamTaxTransaction mirrors the
// void path: finalize commits a tax_transaction; marking the invoice
// uncollectible must reverse it so the authority's records stop
// reporting the tax as collected. Stripe Tax docs explicitly mention
// both void and uncollectible.
func TestVoid_PartialTaxReversalAfterPriorCreditNote(t *testing.T) {
	// Bug #5 regression: a finalized-unpaid invoice that already had a credit
	// note issued (which reversed its own tax slice via ModePartial) must, on
	// Void, reverse only the REMAINING un-credit-noted tax — not the whole
	// transaction again. An unconditional ModeFull would double-reverse the
	// already-reversed slice and under-remit the tenant's output tax.
	store := newMemStore()
	svc := NewService(store, nil, newMemNumberer())
	rev := &fakeTaxReverser{}
	svc.SetTaxReverser(rev)
	ctx := context.Background()

	inv, _ := svc.Create(ctx, "t1", CreateInput{
		CustomerID: "c", SubscriptionID: "s",
		BillingPeriodStart: time.Now(), BillingPeriodEnd: time.Now().AddDate(0, 1, 0),
	})
	if _, err := svc.Finalize(ctx, "t1", inv.ID); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	// $110 taxed invoice with a $50 adjustment credit note already issued
	// (amount_due 11000 -> 6000), committed upstream tax transaction.
	stale := store.invoices[inv.ID]
	stale.TaxTransactionID = "tx_committed"
	stale.TotalAmountCents = 11000
	stale.AmountDueCents = 6000
	stale.AmountPaidCents = 0
	store.invoices[inv.ID] = stale

	if _, err := svc.Void(ctx, "t1", inv.ID); err != nil {
		t.Fatalf("Void: %v", err)
	}
	if len(rev.calls) != 1 {
		t.Fatalf("ReverseTax calls: got %d want 1", len(rev.calls))
	}
	call := rev.calls[0]
	if call.Mode != tax.ReversalModePartial {
		t.Errorf("Mode: got %q want partial (prior CN already reversed part of the tax)", call.Mode)
	}
	if call.GrossAmountCents != 6000 {
		t.Errorf("GrossAmountCents: got %d want 6000 (remaining un-credit-noted gross, NOT the full 11000)", call.GrossAmountCents)
	}
}

type fakeCreditNoteTotaler struct{ credited int64 }

func (f *fakeCreditNoteTotaler) CreditedCents(_ context.Context, _, _ string) (int64, error) {
	return f.credited, nil
}

// TestVoid_CreditApplied_TaxNotUnderReversed is the audit's void-tax regression:
// applied customer credit reduces amount_due WITHOUT reversing tax, so the old
// amount_paid+amount_due proxy under-reversed tax on void. With the credit-note
// totaler wired (reporting NO credit note), void must reverse the FULL tax even
// though amount_due is below total because credit was applied.
func TestVoid_CreditApplied_TaxNotUnderReversed(t *testing.T) {
	store := newMemStore()
	svc := NewService(store, nil, newMemNumberer())
	rev := &fakeTaxReverser{}
	svc.SetTaxReverser(rev)
	svc.SetCreditNoteTotaler(&fakeCreditNoteTotaler{credited: 0}) // no credit note exists
	ctx := context.Background()

	inv, _ := svc.Create(ctx, "t1", CreateInput{
		CustomerID: "c", SubscriptionID: "s",
		BillingPeriodStart: time.Now(), BillingPeriodEnd: time.Now().AddDate(0, 1, 0),
	})
	if _, err := svc.Finalize(ctx, "t1", inv.ID); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	// $110 taxed invoice, $30 paid down by APPLIED CUSTOMER CREDIT (not a credit
	// note): amount_due 8000, amount_paid 0, no credit note. The credit didn't
	// reverse any tax, so the whole $110's tax must be reversed on void.
	stale := store.invoices[inv.ID]
	stale.TaxTransactionID = "tx_committed"
	stale.TotalAmountCents = 11000
	stale.AmountDueCents = 8000
	stale.AmountPaidCents = 0
	store.invoices[inv.ID] = stale

	if _, err := svc.Void(ctx, "t1", inv.ID); err != nil {
		t.Fatalf("Void: %v", err)
	}
	if len(rev.calls) != 1 {
		t.Fatalf("ReverseTax calls: got %d want 1", len(rev.calls))
	}
	// remaining = total - credited(0) = 11000 = total → FULL reversal. The old
	// proxy would have done ModePartial(8000), under-reversing $30 of tax.
	if rev.calls[0].Mode != tax.ReversalModeFull {
		t.Errorf("Mode: got %q want full (applied credit must NOT shrink the tax reversal)", rev.calls[0].Mode)
	}
}

func TestMarkUncollectible_ReversesUpstreamTaxTransaction(t *testing.T) {
	store := newMemStore()
	svc := NewService(store, nil, newMemNumberer())
	rev := &fakeTaxReverser{}
	svc.SetTaxReverser(rev)
	ctx := context.Background()

	inv, _ := svc.Create(ctx, "t1", CreateInput{
		CustomerID: "c", SubscriptionID: "s",
		BillingPeriodStart: time.Now(), BillingPeriodEnd: time.Now().AddDate(0, 1, 0),
	})
	if _, err := svc.Finalize(ctx, "t1", inv.ID); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	stale := store.invoices[inv.ID]
	stale.TaxTransactionID = "tx_committed_at_finalize"
	store.invoices[inv.ID] = stale

	if _, err := svc.MarkUncollectible(ctx, "t1", inv.ID); err != nil {
		t.Fatalf("MarkUncollectible: %v", err)
	}
	if len(rev.calls) != 1 {
		t.Fatalf("ReverseTax calls: got %d want 1", len(rev.calls))
	}
	call := rev.calls[0]
	if call.OriginalTransactionID != "tx_committed_at_finalize" {
		t.Errorf("OriginalTransactionID: got %q", call.OriginalTransactionID)
	}
	if call.Mode != tax.ReversalModeFull {
		t.Errorf("Mode: got %q want full", call.Mode)
	}
	expectedRef := "inv_taxrev_" + inv.ID
	if call.Reference != expectedRef {
		t.Errorf("Reference: got %q want %q (invoice-stable reversal ref — shared with Void so a later void dedups instead of double-reversing)", call.Reference, expectedRef)
	}
}

// TestUncollectibleThenVoid_ReversesTaxWithOneStableReference is the product-audit
// G1 regression: Void permits an `uncollectible` source (annulling a bad debt is
// a legitimate operator action), and pre-fix MarkUncollectible + Void reversed the
// tax under DIFFERENT references (inv_uncoll_<id> vs inv_void_<id>) that don't
// dedup at Stripe → the same tax transaction was reversed twice → the tenant
// UNDER-remitted output tax. The fix uses one invoice-stable reference so both
// transitions share it (same Reference + idempotency key velox_tax_rev_<ref>),
// collapsing to exactly one reversal.
func TestUncollectibleThenVoid_ReversesTaxWithOneStableReference(t *testing.T) {
	store := newMemStore()
	svc := NewService(store, nil, newMemNumberer())
	rev := &fakeTaxReverser{}
	svc.SetTaxReverser(rev)
	ctx := context.Background()

	inv, _ := svc.Create(ctx, "t1", CreateInput{
		CustomerID: "c", SubscriptionID: "s",
		BillingPeriodStart: time.Now(), BillingPeriodEnd: time.Now().AddDate(0, 1, 0),
	})
	if _, err := svc.Finalize(ctx, "t1", inv.ID); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	stale := store.invoices[inv.ID]
	stale.TaxTransactionID = "tx_committed"
	stale.TotalAmountCents = 11000
	stale.AmountDueCents = 11000
	stale.AmountPaidCents = 0
	store.invoices[inv.ID] = stale

	if _, err := svc.MarkUncollectible(ctx, "t1", inv.ID); err != nil {
		t.Fatalf("MarkUncollectible: %v", err)
	}
	if _, err := svc.Void(ctx, "t1", inv.ID); err != nil {
		t.Fatalf("Void of an uncollectible invoice should be allowed: %v", err)
	}

	if len(rev.calls) != 2 {
		t.Fatalf("expected 2 ReverseTax calls (uncollectible + void), got %d", len(rev.calls))
	}
	wantRef := "inv_taxrev_" + inv.ID
	if rev.calls[0].Reference != wantRef || rev.calls[1].Reference != wantRef {
		t.Fatalf("both reversals must share the invoice-stable reference %q so Stripe dedups to ONE reversal; got %q and %q",
			wantRef, rev.calls[0].Reference, rev.calls[1].Reference)
	}
	if rev.calls[0].OriginalTransactionID != rev.calls[1].OriginalTransactionID {
		t.Errorf("both reversals must target the same tax transaction; got %q and %q",
			rev.calls[0].OriginalTransactionID, rev.calls[1].OriginalTransactionID)
	}
}

// TestMarkUncollectible_WritesAuditAndDispatchesEvent locks in the
// Stripe-parity contract: MarkUncollectible must produce both an
// audit row (so Activity surfaces the transition) AND a webhook
// dispatch (invoice.marked_uncollectible — Stripe-matched name).
// Both fire from the service layer so any caller (HTTP handler,
// dunning final-action adapter, ResolveRun cross-flow) gets them.
func TestMarkUncollectible_WritesAuditAndDispatchesEvent(t *testing.T) {
	svc := NewService(newMemStore(), nil, newMemNumberer())
	audit := &captureAuditInvoice{}
	events := &captureEvents{}
	svc.SetAuditLogger(audit)
	svc.SetEventDispatcher(events)
	ctx := context.Background()

	inv, _ := svc.Create(ctx, "t1", CreateInput{
		CustomerID: "c", SubscriptionID: "s",
		BillingPeriodStart: time.Now(), BillingPeriodEnd: time.Now().AddDate(0, 1, 0),
	})
	if _, err := svc.Finalize(ctx, "t1", inv.ID); err != nil {
		t.Fatalf("finalize: %v", err)
	}

	out, err := svc.MarkUncollectible(ctx, "t1", inv.ID)
	if err != nil {
		t.Fatalf("MarkUncollectible: %v", err)
	}
	if out.Status != domain.InvoiceUncollectible {
		t.Errorf("status: got %q, want uncollectible", out.Status)
	}
	// Three rows — the fixture builds the invoice THROUGH the service, and
	// both of its steps are audited paths in their own right: Create emits the
	// manual-create row (ADR-090, replacing the middleware catch-all's guess),
	// Finalize the canonical finalize row. MarkUncollectible's row is the LAST.
	if len(audit.entries) != 3 {
		t.Fatalf("audit entries: got %d, want 3 (create + finalize + marked_uncollectible)", len(audit.entries))
	}
	if a := audit.entries[0]; a.action != string(domain.AuditActionCreate) {
		t.Errorf("first audit action: got %v, want create", a.action)
	}
	if a := audit.entries[1]; a.action != string(domain.AuditActionFinalize) {
		t.Errorf("second audit action: got %v, want finalize", a.action)
	}
	if a := audit.entries[2]; a.metadata["action"] != "marked_uncollectible" {
		t.Errorf("audit metadata.action: got %v, want marked_uncollectible", a.metadata["action"])
	}
	// The fixture finalizes through the service, which (correctly) now
	// emits invoice.finalized too — assert on the LAST event.
	if len(events.events) != 2 || events.events[1].eventType != domain.EventInvoiceMarkedUncollectible {
		t.Errorf("events: got %+v, want [invoice.finalized, invoice.marked_uncollectible]", events.events)
	}
}

// TestRecordOfflinePayment exercises the Stripe-parity offline-recovery
// transition: finalized-unpaid → paid AND uncollectible → paid. Both
// must (a) succeed, (b) write audit with the originating status in
// metadata, (c) dispatch invoice.payment_recorded. Voided + already-
// paid are rejected.
func TestRecordOfflinePayment(t *testing.T) {
	mkSvc := func() (*Service, *captureAuditInvoice, *captureEvents) {
		svc := NewService(newMemStore(), nil, newMemNumberer())
		audit := &captureAuditInvoice{}
		events := &captureEvents{}
		svc.SetAuditLogger(audit)
		svc.SetEventDispatcher(events)
		return svc, audit, events
	}
	ctx := context.Background()
	mkFinalized := func(svc *Service) domain.Invoice {
		inv, _ := svc.Create(ctx, "t1", CreateInput{
			CustomerID: "c", SubscriptionID: "s",
			BillingPeriodStart: time.Now(), BillingPeriodEnd: time.Now().AddDate(0, 1, 0),
		})
		final, _ := svc.Finalize(ctx, "t1", inv.ID)
		return final
	}

	t.Run("finalized → paid", func(t *testing.T) {
		svc, audit, events := mkSvc()
		inv := mkFinalized(svc)
		out, err := svc.RecordOfflinePayment(ctx, "t1", inv.ID, "Cheque #1234")
		if err != nil {
			t.Fatalf("RecordOfflinePayment: %v", err)
		}
		if out.PaymentStatus != domain.PaymentSucceeded {
			t.Errorf("payment_status: got %q, want succeeded", out.PaymentStatus)
		}
		if out.PaidAt == nil {
			t.Error("paid_at should be set")
		}
		// The fixture builds the invoice through the service, so entries[0] is
		// the manual-create row (ADR-090) and entries[1] the canonical finalize
		// row; entries[2] is the payment_recorded row under test.
		if len(audit.entries) != 3 || audit.entries[2].metadata["action"] != "payment_recorded" {
			t.Fatalf("audit: got %+v, want create + finalize + payment_recorded", audit.entries)
		}
		if audit.entries[2].metadata["recovered_from_status"] != string(domain.InvoiceFinalized) {
			t.Errorf("audit recovered_from_status: got %v, want finalized", audit.entries[2].metadata["recovered_from_status"])
		}
		if audit.entries[2].metadata["note"] != "Cheque #1234" {
			t.Errorf("audit note: got %v, want Cheque #1234", audit.entries[2].metadata["note"])
		}
		if len(events.events) != 2 || events.events[1].eventType != domain.EventInvoicePaymentRecorded {
			t.Errorf("events: got %+v, want [invoice.finalized, invoice.payment_recorded]", events.events)
		}
	})

	t.Run("uncollectible → paid (Stripe recovery)", func(t *testing.T) {
		svc, _, _ := mkSvc()
		inv := mkFinalized(svc)
		if _, err := svc.MarkUncollectible(ctx, "t1", inv.ID); err != nil {
			t.Fatalf("setup mark uncollectible: %v", err)
		}
		out, err := svc.RecordOfflinePayment(ctx, "t1", inv.ID, "")
		if err != nil {
			t.Fatalf("RecordOfflinePayment from uncollectible: %v", err)
		}
		if out.Status != domain.InvoicePaid {
			t.Errorf("status: got %q, want paid (recovery from uncollectible)", out.Status)
		}
	})

	t.Run("rejects already paid", func(t *testing.T) {
		svc, _, _ := mkSvc()
		inv := mkFinalized(svc)
		if _, err := svc.RecordOfflinePayment(ctx, "t1", inv.ID, ""); err != nil {
			t.Fatalf("first record: %v", err)
		}
		if _, err := svc.RecordOfflinePayment(ctx, "t1", inv.ID, ""); err == nil {
			t.Error("expected error on already-paid invoice")
		}
	})

	t.Run("rejects voided", func(t *testing.T) {
		svc, _, _ := mkSvc()
		inv := mkFinalized(svc)
		if _, err := svc.Void(ctx, "t1", inv.ID); err != nil {
			t.Fatalf("setup void: %v", err)
		}
		if _, err := svc.RecordOfflinePayment(ctx, "t1", inv.ID, ""); err == nil {
			t.Error("expected error on voided invoice")
		}
	})

	t.Run("rejects draft", func(t *testing.T) {
		svc, _, _ := mkSvc()
		inv, _ := svc.Create(ctx, "t1", CreateInput{
			CustomerID: "c", SubscriptionID: "s",
			BillingPeriodStart: time.Now(), BillingPeriodEnd: time.Now().AddDate(0, 1, 0),
		})
		if _, err := svc.RecordOfflinePayment(ctx, "t1", inv.ID, ""); err == nil {
			t.Error("expected error on draft invoice")
		}
	})

	// amount_cents must be the BOOKED amount, not the post-transition
	// amount_due. MarkPaid flips amount_paid=amount_due and amount_due=0
	// in one statement; the pre-fix payload read AmountDueCents from the
	// updated row, so every offline payment audited and webhooked as a
	// ZERO-value payment (2026-07-05 money-bug sprint).
	t.Run("audit + event carry the booked amount, not zero", func(t *testing.T) {
		store := newMemStore()
		store.invoices["inv_amt"] = domain.Invoice{
			ID: "inv_amt", TenantID: "t1", CustomerID: "c", InvoiceNumber: "VLX-42",
			Status: domain.InvoiceFinalized, AmountDueCents: 4200, Currency: "USD",
		}
		svc := NewService(store, nil, newMemNumberer())
		audit := &captureAuditInvoice{}
		events := &captureEvents{}
		svc.SetAuditLogger(audit)
		svc.SetEventDispatcher(events)

		if _, err := svc.RecordOfflinePayment(ctx, "t1", "inv_amt", "Wire 2026-07-05"); err != nil {
			t.Fatalf("RecordOfflinePayment: %v", err)
		}
		if len(audit.entries) != 1 || audit.entries[0].metadata["amount_cents"] != int64(4200) {
			t.Errorf("audit amount_cents: got %v, want 4200 (booked amount, not the zeroed amount_due)",
				audit.entries[0].metadata["amount_cents"])
		}
		if len(events.events) != 1 || events.events[0].payload["amount_cents"] != int64(4200) {
			t.Errorf("event amount_cents: got %v, want 4200 (integrators must see the real payment value)",
				events.events[0].payload["amount_cents"])
		}
	})
}

// TestMarkPaid_StoreGate_RejectsDraftAndTaxPending locks in the
// 2026-05-22 store-level invariant. The credit-fully-paid path in
// billOnePeriod (and threshold_scan) previously called MarkPaid on
// draft / tax-pending invoices, locking the customer at subtotal-only
// totals. The store-level gate is the canonical guard; the engine
// caller-level gates are defense-in-depth. This test exercises the
// store gate via the in-memory MarkPaid (which mirrors PostgresStore's
// gate).
func TestMarkPaid_StoreGate_RejectsDraftAndTaxPending(t *testing.T) {
	store := newMemStore()
	ctx := context.Background()

	now := time.Date(2027, 9, 19, 18, 30, 0, 0, time.UTC)

	t.Run("rejects draft (finalize first)", func(t *testing.T) {
		// Synthesize a draft invoice with tax_status=ok (so the failure
		// is purely the draft status gate, not the tax gate).
		store.invoices["inv_draft"] = domain.Invoice{
			ID: "inv_draft", TenantID: "t1",
			Status: domain.InvoiceDraft, TaxFacts: domain.TaxFacts{TaxStatus: domain.InvoiceTaxOK},
		}
		_, err := store.MarkPaid(ctx, "t1", "inv_draft", "", now)
		if err == nil {
			t.Error("expected MarkPaid on draft invoice to fail")
		}
	})

	t.Run("rejects tax_status=pending on finalized", func(t *testing.T) {
		// Even a finalized invoice — if tax somehow flipped back to
		// pending — must not transition to paid. Synthetic since the
		// natural path keeps these in sync, but the gate is a hard
		// invariant against future refactors.
		store.invoices["inv_finalized_tax_pending"] = domain.Invoice{
			ID: "inv_finalized_tax_pending", TenantID: "t1",
			Status: domain.InvoiceFinalized, TaxFacts: domain.TaxFacts{TaxStatus: domain.InvoiceTaxPending},
		}
		_, err := store.MarkPaid(ctx, "t1", "inv_finalized_tax_pending", "", now)
		if err == nil {
			t.Error("expected MarkPaid with tax_status=pending to fail")
		}
	})

	t.Run("rejects voided (terminal)", func(t *testing.T) {
		store.invoices["inv_voided"] = domain.Invoice{
			ID: "inv_voided", TenantID: "t1",
			Status: domain.InvoiceVoided, TaxFacts: domain.TaxFacts{TaxStatus: domain.InvoiceTaxOK},
		}
		_, err := store.MarkPaid(ctx, "t1", "inv_voided", "", now)
		if err == nil {
			t.Error("expected MarkPaid on voided invoice to fail")
		}
	})

	t.Run("allows finalized + tax_status=ok", func(t *testing.T) {
		store.invoices["inv_ok"] = domain.Invoice{
			ID: "inv_ok", TenantID: "t1",
			Status: domain.InvoiceFinalized, TaxFacts: domain.TaxFacts{TaxStatus: domain.InvoiceTaxOK},
			AmountDueCents: 7000,
		}
		out, err := store.MarkPaid(ctx, "t1", "inv_ok", "", now)
		if err != nil {
			t.Fatalf("MarkPaid: %v", err)
		}
		if out.Status != domain.InvoicePaid {
			t.Errorf("status: got %q, want paid", out.Status)
		}
	})

	t.Run("allows uncollectible (Stripe-parity recovery)", func(t *testing.T) {
		store.invoices["inv_unc"] = domain.Invoice{
			ID: "inv_unc", TenantID: "t1",
			Status: domain.InvoiceUncollectible, TaxFacts: domain.TaxFacts{TaxStatus: domain.InvoiceTaxOK},
		}
		_, err := store.MarkPaid(ctx, "t1", "inv_unc", "", now)
		if err != nil {
			t.Errorf("MarkPaid on uncollectible should succeed: %v", err)
		}
	})

	t.Run("idempotent on already-paid", func(t *testing.T) {
		store.invoices["inv_paid"] = domain.Invoice{
			ID: "inv_paid", TenantID: "t1",
			Status: domain.InvoicePaid, TaxFacts: domain.TaxFacts{TaxStatus: domain.InvoiceTaxOK},
		}
		_, err := store.MarkPaid(ctx, "t1", "inv_paid", "", now)
		if err != nil {
			t.Errorf("idempotent re-MarkPaid should succeed: %v", err)
		}
	})
}

// recordingTaxCommitter captures CommitTax calls for the reconciler test.
type recordingTaxCommitter struct {
	calls []string // invoice ids
	err   error
}

func (r *recordingTaxCommitter) CommitTax(_ context.Context, _, invoiceID, _ string) error {
	r.calls = append(r.calls, invoiceID)
	return r.err
}

// TestRetryPendingTaxCommit verifies the commit reconciler re-commits only the
// orphan invoices — finalized stripe_tax with a calculation id but no
// transaction id — and leaves already-committed / non-stripe invoices alone.
func TestRetryPendingTaxCommit(t *testing.T) {
	store := newMemStore()
	svc := NewService(store, nil, nil)
	committer := &recordingTaxCommitter{}
	svc.SetTaxCommitter(committer)
	ctx := context.Background()

	// The orphan: commit succeeded at Stripe, local tax_transaction_id lost.
	store.invoices["orphan"] = domain.Invoice{
		ID: "orphan", TenantID: "t1", Status: domain.InvoiceFinalized,
		TaxFacts: domain.TaxFacts{
			TaxStatus: domain.InvoiceTaxOK, TaxProvider: "stripe_tax",
			TaxCalculationID: "taxcalc_1",
		},
		TaxTransactionID: "",
	}
	// Already committed — excluded.
	store.invoices["committed"] = domain.Invoice{
		ID: "committed", TenantID: "t1", Status: domain.InvoiceFinalized,
		TaxFacts: domain.TaxFacts{
			TaxStatus: domain.InvoiceTaxOK, TaxProvider: "stripe_tax",
			TaxCalculationID: "taxcalc_2",
		},
		TaxTransactionID: "tx_2",
	}
	// Manual provider (no calc id, nothing to commit) — excluded.
	store.invoices["manual"] = domain.Invoice{
		ID: "manual", TenantID: "t1", Status: domain.InvoiceFinalized,
		TaxFacts: domain.TaxFacts{
			TaxStatus: domain.InvoiceTaxOK, TaxProvider: "manual",
		},
	}

	recovered, errs := svc.RetryPendingTaxCommit(ctx, 50)
	if len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}
	if recovered != 1 {
		t.Errorf("recovered = %d, want 1 (only the orphan)", recovered)
	}
	if len(committer.calls) != 1 || committer.calls[0] != "orphan" {
		t.Errorf("CommitTax calls = %v, want exactly [orphan]", committer.calls)
	}
}
