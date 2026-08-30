package creditnote

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
)

type memStore struct {
	notes     map[string]domain.CreditNote
	lineItems map[string][]domain.CreditNoteLineItem
	// lastLockLines records the lines passed INTO CreateUnderInvoiceLock —
	// the runtime pin that lines travel in the same call (one tx) as the
	// header, not in a post-commit loop (the orphan-CN shape).
	lastLockLines []domain.CreditNoteLineItem
	// setPendingCalls counts SetTaxReversalPending writes. The real store
	// bumps updated_at on every UPDATE, and the reversal scan's 24h window
	// keys on updated_at — so a write issued when the marker is ALREADY
	// false is not harmless: it refreshes the row's own eligibility and the
	// sweep never converges. This fake cannot model updated_at, so the
	// write count is the faithful stand-in.
	setPendingCalls int
	// setPendingErr, when set, makes SetTaxReversalPending fail — the
	// promote-write-fails path of the reversal sweep.
	setPendingErr error
}

func newMemStore() *memStore {
	return &memStore{
		notes:     make(map[string]domain.CreditNote),
		lineItems: make(map[string][]domain.CreditNoteLineItem),
	}
}

func (m *memStore) Create(ctx context.Context, tenantID string, cn domain.CreditNote) (domain.CreditNote, error) {
	cn.ID = fmt.Sprintf("vlx_cn_%d", len(m.notes)+1)
	cn.TenantID = tenantID
	// Mirrors the real Postgres store, which reads clock.Now(ctx) so a
	// clock-pinned write lands in simulated time (ADR-030 rule 3). A fake that
	// hardcodes time.Now() would silently pass a binding test that the real
	// store would fail.
	cn.CreatedAt = clock.Now(ctx)
	cn.UpdatedAt = cn.CreatedAt
	m.notes[cn.ID] = cn
	return cn, nil
}

func (m *memStore) CreateUnderInvoiceLock(ctx context.Context, tenantID, invoiceID string, lines []domain.CreditNoteLineItem, build func(existing []domain.CreditNote) (domain.CreditNote, error)) (domain.CreditNote, error) {
	return m.CreateUnderInvoiceLockAudited(ctx, tenantID, invoiceID, lines, build, nil)
}

// CreateUnderInvoiceLockAudited mirrors the real store: emit fires once, with
// the CREATED row, only after a successful insert; an emit error fails the
// create (the double can't roll back — real shared fate is pinned by the
// real-Postgres integration test).
func (m *memStore) CreateUnderInvoiceLockAudited(ctx context.Context, tenantID, invoiceID string, lines []domain.CreditNoteLineItem, build func(existing []domain.CreditNote) (domain.CreditNote, error), emit func(tx *sql.Tx, created domain.CreditNote) error) (domain.CreditNote, error) {
	created, err := m.createUnderInvoiceLock(ctx, tenantID, invoiceID, lines, build)
	if err != nil {
		return domain.CreditNote{}, err
	}
	if emit != nil {
		if err := emit(nil, created); err != nil {
			delete(m.notes, created.ID)
			delete(m.lineItems, created.ID)
			return domain.CreditNote{}, err
		}
	}
	return created, nil
}

// CreateUnderInvoiceLockTxAudited runs the same emission on the caller's tx —
// the double has no real tx, so it delegates.
func (m *memStore) CreateUnderInvoiceLockTxAudited(ctx context.Context, _ *sql.Tx, tenantID, invoiceID string, lines []domain.CreditNoteLineItem, build func(existing []domain.CreditNote) (domain.CreditNote, error), emit func(tx *sql.Tx, created domain.CreditNote) error) (domain.CreditNote, error) {
	return m.CreateUnderInvoiceLockAudited(ctx, tenantID, invoiceID, lines, build, emit)
}

func (m *memStore) createUnderInvoiceLock(ctx context.Context, tenantID, invoiceID string, lines []domain.CreditNoteLineItem, build func(existing []domain.CreditNote) (domain.CreditNote, error)) (domain.CreditNote, error) {
	m.lastLockLines = lines
	var existing []domain.CreditNote
	for _, cn := range m.notes {
		if cn.TenantID == tenantID && cn.InvoiceID == invoiceID {
			existing = append(existing, cn)
		}
	}
	cn, err := build(existing)
	if err != nil {
		return domain.CreditNote{}, err
	}
	created, err := m.Create(ctx, tenantID, cn)
	if err != nil {
		return domain.CreditNote{}, err
	}
	// Lines land with the header, mirroring the real store's single-tx write.
	for _, line := range lines {
		line.ID = fmt.Sprintf("vlx_cnli_%d", len(m.lineItems[created.ID])+1)
		line.TenantID = tenantID
		line.CreditNoteID = created.ID
		m.lineItems[created.ID] = append(m.lineItems[created.ID], line)
	}
	return created, nil
}

// CreateUnderInvoiceLockTx delegates to CreateUnderInvoiceLock — the in-memory
// double has no real transaction, so it can't model rollback. The atomicity
// (a clawback-create failure rolling back the caller's item delete) is
// exercised by the real-Postgres integration test, not this double.
func (m *memStore) CreateUnderInvoiceLockTx(ctx context.Context, _ *sql.Tx, tenantID, invoiceID string, lines []domain.CreditNoteLineItem, build func(existing []domain.CreditNote) (domain.CreditNote, error)) (domain.CreditNote, error) {
	return m.CreateUnderInvoiceLock(ctx, tenantID, invoiceID, lines, build)
}

// CreateUnderInvoiceLockDynamicTx mirrors the real store's dynamic-lines
// variant on the in-memory double (no real locks — the relief lock
// discipline is proven against real Postgres).
func (m *memStore) CreateUnderInvoiceLockDynamicTx(ctx context.Context, _ *sql.Tx, tenantID, invoiceID string, build func(existing []domain.CreditNote) (domain.CreditNote, []domain.CreditNoteLineItem, error)) (domain.CreditNote, error) {
	var lines []domain.CreditNoteLineItem
	created, err := m.CreateUnderInvoiceLock(ctx, tenantID, invoiceID, nil, func(existing []domain.CreditNote) (domain.CreditNote, error) {
		cn, ls, err := build(existing)
		lines = ls
		return cn, err
	})
	if err != nil {
		return domain.CreditNote{}, err
	}
	for _, line := range lines {
		line.ID = fmt.Sprintf("vlx_cnli_%d", len(m.lineItems[created.ID])+1)
		line.TenantID = tenantID
		line.CreditNoteID = created.ID
		m.lineItems[created.ID] = append(m.lineItems[created.ID], line)
	}
	return created, nil
}

// ListPendingClawbackDrafts returns the in-memory auto-issue clawback drafts
// (issue_pending, still draft). livemode/batch are ignored in the double.
func (m *memStore) ListPendingClawbackDrafts(_ context.Context, _ int, _ bool) ([]domain.CreditNote, error) {
	var out []domain.CreditNote
	for _, cn := range m.notes {
		if cn.IssuePending && cn.Status == domain.CreditNoteDraft {
			out = append(out, cn)
		}
	}
	return out, nil
}

// ListPendingClawbackDraftsForClock mirrors the wall-clock double for the
// catchup path; the double ignores clock scoping (the real test_clock_id filter
// is covered by the real-Postgres store test).
func (m *memStore) ListPendingClawbackDraftsForClock(_ context.Context, _, _ string, _ int) ([]domain.CreditNote, error) {
	return m.ListPendingClawbackDrafts(context.Background(), 0, false)
}

func (m *memStore) Get(_ context.Context, tenantID, id string) (domain.CreditNote, error) {
	cn, ok := m.notes[id]
	if !ok || cn.TenantID != tenantID {
		return domain.CreditNote{}, errs.ErrNotFound
	}
	return cn, nil
}

func (m *memStore) List(_ context.Context, filter ListFilter) ([]domain.CreditNote, error) {
	var result []domain.CreditNote
	for _, cn := range m.notes {
		if cn.TenantID != filter.TenantID {
			continue
		}
		if filter.InvoiceID != "" && cn.InvoiceID != filter.InvoiceID {
			continue
		}
		result = append(result, cn)
	}
	return result, nil
}

// NOTE: no UpdateStatus on the double — the unguarded status writer was deleted
// with the ADR-090 void emission; every status write is a CAS.

// TransitionStatusAudited fake mirrors the real store: emit fires only on
// a won CAS; nil tx is fine for content-level unit tests, and an emit error
// leaves the flip un-rolled-back only in the fake (real-store shared fate
// is pinned by integration tests) so it fails loudly instead.
func (m *memStore) TransitionStatusAudited(ctx context.Context, tenantID, id string, from, to domain.CreditNoteStatus, emit func(tx *sql.Tx) error) (bool, error) {
	won, err := m.TransitionStatus(ctx, tenantID, id, from, to)
	if err != nil || !won {
		return won, err
	}
	if emit != nil {
		if err := emit(nil); err != nil {
			// roll the fake flip back for shared-fate fidelity
			if cn, ok := m.notes[id]; ok {
				cn.Status = from
				m.notes[id] = cn
			}
			return false, err
		}
	}
	return won, nil
}

func (m *memStore) TransitionStatus(ctx context.Context, tenantID, id string, from, to domain.CreditNoteStatus) (bool, error) {
	cn, ok := m.notes[id]
	if !ok || cn.TenantID != tenantID {
		return false, errs.ErrNotFound
	}
	if cn.Status != from {
		return false, nil // lost the CAS — already transitioned
	}
	cn.Status = to
	now := clock.Now(ctx)
	if to == domain.CreditNoteIssued {
		cn.IssuedAt = &now
	}
	if to == domain.CreditNoteVoided {
		cn.VoidedAt = &now
	}
	m.notes[id] = cn
	return true, nil
}

func (m *memStore) UpdateRefundStatus(ctx context.Context, tenantID, id string, status domain.RefundStatus, stripeRefundID string) error {
	return m.UpdateRefundStatusAudited(ctx, tenantID, id, RefundStatusWrite{Status: status, StripeRefundID: stripeRefundID}, nil)
}

// UpdateRefundStatusAudited mirrors the REAL store's attempt-scoped write
// rules — identity CAS, same-value no-op, same-identity regression guard with
// the ProviderRead bypass — not just its happy path. The fake-fidelity rule:
// a service test passing against a laxer fake would assert nothing about the
// paths that matter (retry_guard_integration_test.go pins the real SQL).
func (m *memStore) UpdateRefundStatusAudited(_ context.Context, tenantID, id string, w RefundStatusWrite, emit func(tx *sql.Tx, updated domain.CreditNote, prior domain.RefundStatus, changed bool) error) error {
	cn, ok := m.notes[id]
	if !ok || cn.TenantID != tenantID {
		return errs.ErrNotFound
	}
	prior := cn.RefundStatus
	priorRefundID := cn.StripeRefundID
	skip := false
	if cn.StripeRefundID != w.ExpectedPriorRefundID {
		skip = true // stale writer: identity moved
	} else {
		newID := cn.StripeRefundID
		if w.StripeRefundID != "" {
			newID = w.StripeRefundID
		}
		sameIdentity := newID == cn.StripeRefundID
		if sameIdentity && (w.Status == prior || (!w.ProviderRead && refundStatusRegression(prior, w.Status))) {
			skip = true
		}
	}
	if !skip {
		cn.RefundStatus = w.Status
		if w.StripeRefundID != "" {
			cn.StripeRefundID = w.StripeRefundID
		}
		m.notes[id] = cn
	}
	changed := cn.RefundStatus != prior || cn.StripeRefundID != priorRefundID
	// Mirrors the real store: emit ALWAYS runs (with `changed`); the caller
	// decides whether the fact is audit-worthy.
	if emit != nil {
		if err := emit(nil, cn, prior, changed); err != nil {
			// roll the fake write back for shared-fate fidelity
			cn.RefundStatus = prior
			cn.StripeRefundID = priorRefundID
			m.notes[id] = cn
			return err
		}
	}
	return nil
}

// Audited fake mirrors the real store: emit fires only when the status
// actually flipped; a nil tx is fine for content-level unit tests.
func (m *memStore) ApplyRefundWebhookStatusAudited(ctx context.Context, tenantID, stripeRefundID string, status domain.RefundStatus, emit func(tx *sql.Tx, cn domain.CreditNote) error) error {
	var before domain.RefundStatus
	var match domain.CreditNote
	found := false
	for _, cn := range m.notes {
		if cn.TenantID == tenantID && cn.StripeRefundID == stripeRefundID {
			before, match, found = cn.RefundStatus, cn, true
			break
		}
	}
	if err := m.ApplyRefundWebhookStatus(ctx, tenantID, stripeRefundID, status); err != nil {
		return err
	}
	if emit != nil && found {
		for _, cn := range m.notes {
			if cn.TenantID == tenantID && cn.StripeRefundID == stripeRefundID && cn.RefundStatus != before {
				match.RefundStatus = cn.RefundStatus
				if err := emit(nil, match); err != nil {
					return err
				}
				break
			}
		}
	}
	return nil
}

func (m *memStore) ApplyRefundWebhookStatus(_ context.Context, tenantID, stripeRefundID string, status domain.RefundStatus) error {
	for id, cn := range m.notes {
		if cn.TenantID != tenantID || cn.StripeRefundID != stripeRefundID {
			continue
		}
		// failed-absorbing precedence (mirrors the ApplyRefundWebhookStatus SQL):
		// failed never changes; succeeded yields only to failed; pending yields to any terminal.
		if cn.RefundStatus == domain.RefundFailed {
			return nil
		}
		if cn.RefundStatus == domain.RefundSucceeded && status != domain.RefundFailed {
			return nil
		}
		cn.RefundStatus = status
		m.notes[id] = cn
		return nil
	}
	return errs.ErrNotFound
}

func (m *memStore) UpdateAllocation(_ context.Context, tenantID, id string, refundCents, creditCents, outOfBandCents int64) error {
	cn, ok := m.notes[id]
	if !ok || cn.TenantID != tenantID {
		return errs.ErrNotFound
	}
	cn.RefundAmountCents = refundCents
	cn.CreditAmountCents = creditCents
	cn.OutOfBandAmountCents = outOfBandCents
	m.notes[id] = cn
	return nil
}

func (m *memStore) SetTaxTransaction(_ context.Context, tenantID, id, taxTransactionID string) error {
	cn, ok := m.notes[id]
	if !ok || cn.TenantID != tenantID {
		return errs.ErrNotFound
	}
	cn.TaxTransactionID = taxTransactionID
	m.notes[id] = cn
	return nil
}

func (m *memStore) ListLineItems(_ context.Context, _, creditNoteID string) ([]domain.CreditNoteLineItem, error) {
	return m.lineItems[creditNoteID], nil
}

// BeginTx returns a nil *sql.Tx: the in-memory double has no real DB, so the
// coordinator-tx ATOMICITY is exercised by the real-Postgres test, not here.
// Issue() guards Commit/Rollback on tx != nil and the *Tx methods below ignore
// the (nil) tx, mutating the map directly — so the logic still runs end-to-end.
func (m *memStore) BeginTx(_ context.Context, _ string) (*sql.Tx, error) {
	return nil, nil
}

func (m *memStore) TransitionStatusTx(ctx context.Context, _ *sql.Tx, tenantID, id string, from, to domain.CreditNoteStatus) (bool, error) {
	return m.TransitionStatus(ctx, tenantID, id, from, to)
}

func (m *memStore) UpdateAllocationTx(ctx context.Context, _ *sql.Tx, tenantID, id string, refundCents, creditCents, outOfBandCents int64) error {
	return m.UpdateAllocation(ctx, tenantID, id, refundCents, creditCents, outOfBandCents)
}

func (m *memStore) SetTaxReversalPending(_ context.Context, tenantID, id string, pending bool) error {
	m.setPendingCalls++
	if m.setPendingErr != nil {
		return m.setPendingErr
	}
	cn, ok := m.notes[id]
	if !ok || cn.TenantID != tenantID {
		return errs.ErrNotFound
	}
	cn.TaxReversalPending = pending
	m.notes[id] = cn
	return nil
}

func (m *memStore) ListPendingCreditNoteTaxReversal(_ context.Context, _ int, _ bool) ([]domain.CreditNote, error) {
	var out []domain.CreditNote
	for _, cn := range m.notes {
		if cn.Status != domain.CreditNoteIssued {
			continue
		}
		// Marker fast-path OR the structural over-remit state (issued CN with no
		// reversal stamped) — mirrors the store's broadened eligibility. The store
		// ANDs a tax-bearing-source JOIN; this double omits it (no invoices here)
		// and the service re-checks the invoice per row, clearing stale non-tax
		// rows. The JOIN itself is covered by the real-Postgres store test.
		if cn.TaxReversalPending || (cn.TaxTransactionID == "" && cn.TotalCents > 0) {
			out = append(out, cn)
		}
	}
	return out, nil
}

// fakeCNNumbers is the test NumberGenerator — the service now REQUIRES one
// (no synthesized fallback), matching the invoice-numbering contract.
type fakeCNNumbers struct {
	n   int
	err error
}

func (f *fakeCNNumbers) NextCreditNoteNumber(_ context.Context, _ string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	f.n++
	return fmt.Sprintf("CN-TEST-%04d", f.n), nil
}

type memInvoiceReader struct {
	invoices  map[string]domain.Invoice
	lineItems map[string][]domain.InvoiceLineItem
}

func (m *memInvoiceReader) Get(_ context.Context, tenantID, id string) (domain.Invoice, error) {
	inv, ok := m.invoices[id]
	if !ok || inv.TenantID != tenantID {
		return domain.Invoice{}, errs.ErrNotFound
	}
	return inv, nil
}

func (m *memInvoiceReader) ListLineItems(_ context.Context, _, invoiceID string) ([]domain.InvoiceLineItem, error) {
	return m.lineItems[invoiceID], nil
}

func (m *memInvoiceReader) ApplyCreditNote(_ context.Context, tenantID, id string, amountCents int64) (domain.Invoice, error) {
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

// ApplyCreditNoteTx ignores the (nil) coordinator tx and delegates — the
// in-memory double can't model rollback; the real-Postgres test covers it.
func (m *memInvoiceReader) ApplyCreditNoteTx(ctx context.Context, _ *sql.Tx, tenantID, id string, amountCents int64) (domain.Invoice, error) {
	return m.ApplyCreditNote(ctx, tenantID, id, amountCents)
}

// TestCreate_IsSimulated locks the per-CN time-domain flag (migration 0117):
// it's true ONLY when the issuance runs in the invoice's bound time domain
// (engine clawback, input.IsSimulated) AND the invoice itself is simulated.
// Operator HTTP issuance (input.IsSimulated=false) is always wall-clock. The
// invoice activity timeline lanes a CN by this flag, so getting it wrong shows
// a simulated timestamp in the wall-clock "Real-time activity" lane.
func TestCreate_IsSimulated(t *testing.T) {
	ctx := context.Background()
	mk := func(id string, simulated bool) domain.Invoice {
		return domain.Invoice{
			ID: id, TenantID: "t1", CustomerID: "cus_1",
			Status: domain.InvoiceFinalized, Currency: "USD",
			TotalAmountCents: 10000, AmountDueCents: 10000, IsSimulated: simulated,
		}
	}
	newSvc := func(inv domain.Invoice) *Service {
		s := NewService(newMemStore(), &memInvoiceReader{invoices: map[string]domain.Invoice{inv.ID: inv}}, nil)
		s.SetNumberGenerator(&fakeCNNumbers{})
		return s
	}
	line := []CreditLineInput{{Description: "adj", Quantity: 1, UnitAmountCents: 1000}}

	// The rule is one line: the SOURCE INVOICE's pin decides. Corrected
	// 2026-07-29 — this used to also gate on a CreateInput.IsSimulated flag,
	// i.e. "operator HTTP issuance is always wall-clock", which contradicted
	// ADR-030 (audit log is its only always-wall-clock carve-out) and produced
	// credit notes dated before the invoices they credited. That input field is
	// now deleted, so the actor cannot influence the flag at all.
	cases := []struct {
		name      string
		invoice   domain.Invoice
		input     CreateInput
		wantSimul bool
	}{
		{"engine clawback on simulated invoice → simulated",
			mk("inv_sim", true), CreateInput{InvoiceID: "inv_sim", Reason: "subscription_downgrade", Lines: line}, true},
		{"operator issue on the SAME simulated invoice → simulated too",
			mk("inv_sim2", true), CreateInput{InvoiceID: "inv_sim2", Reason: "billing error", Lines: line}, true},
		{"engine clawback on a non-simulated invoice → wall-clock",
			mk("inv_real", false), CreateInput{InvoiceID: "inv_real", Reason: "subscription_downgrade", Lines: line}, false},
		{"operator issue on a non-simulated invoice → wall-clock",
			mk("inv_real2", false), CreateInput{InvoiceID: "inv_real2", Reason: "billing error", Lines: line}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cn, err := newSvc(tc.invoice).Create(ctx, "t1", tc.input)
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			if cn.IsSimulated != tc.wantSimul {
				t.Errorf("is_simulated: got %v, want %v", cn.IsSimulated, tc.wantSimul)
			}
		})
	}
}

func TestCreate_CreditNote(t *testing.T) {
	store := newMemStore()
	invoices := &memInvoiceReader{
		invoices: map[string]domain.Invoice{
			"inv_1": {
				ID: "inv_1", TenantID: "t1", CustomerID: "cus_1",
				Status: domain.InvoiceFinalized, Currency: "USD",
				TotalAmountCents: 19900, AmountDueCents: 19900,
			},
		},
	}
	svc := NewService(store, invoices, nil)
	svc.SetNumberGenerator(&fakeCNNumbers{})
	ctx := context.Background()

	t.Run("valid credit note", func(t *testing.T) {
		cn, err := svc.Create(ctx, "t1", CreateInput{
			InvoiceID: "inv_1",
			Reason:    "Customer overcharged for API calls",
			Lines: []CreditLineInput{
				{Description: "API call adjustment", Quantity: 500, UnitAmountCents: 5},
			},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cn.Status != domain.CreditNoteDraft {
			t.Errorf("status: got %q, want draft", cn.Status)
		}
		if cn.TotalCents != 2500 {
			t.Errorf("total: got %d, want 2500", cn.TotalCents)
		}
		if cn.CreditAmountCents != 0 {
			t.Errorf("credit_amount: got %d, want 0 (unpaid invoice reduces amount_due directly)", cn.CreditAmountCents)
		}
		if cn.CustomerID != "cus_1" {
			t.Errorf("customer_id: got %q", cn.CustomerID)
		}
	})

	t.Run("refund for paid invoice (PM-paid → Stripe refund)", func(t *testing.T) {
		// Stripe-paid invoice: amount_paid_cents = total_amount_cents,
		// credits_applied_cents = 0. Refund routes 100% to PM.
		invoices.invoices["inv_paid"] = domain.Invoice{
			ID: "inv_paid", TenantID: "t1", CustomerID: "cus_1",
			Status: domain.InvoicePaid, Currency: "USD",
			TotalAmountCents: 10000,
			AmountPaidCents:  10000,
			// A card-settled invoice always carries a real PaymentIntent; without one
			// the refund rail is correctly unavailable (domain.HasCardPayment).
			StripePaymentIntentID: "pi_card_test",
		}
		cn, err := svc.Create(ctx, "t1", CreateInput{
			InvoiceID:  "inv_paid",
			Reason:     "Service not delivered",
			RefundType: "refund",
			Lines:      []CreditLineInput{{Description: "Full refund", Quantity: 1, UnitAmountCents: 5000}},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cn.RefundAmountCents != 5000 {
			t.Errorf("refund_amount: got %d, want 5000 (full PM refund)", cn.RefundAmountCents)
		}
		if cn.CreditAmountCents != 0 {
			t.Errorf("credit_amount: got %d, want 0 (no credit was applied to invoice)", cn.CreditAmountCents)
		}
	})

	t.Run("refund REJECTED on credit-only-paid invoice (operator must use Credit type)", func(t *testing.T) {
		// Invoice paid entirely by customer credits: amount_paid=0.
		// Operator picked "Refund -- return to payment method" — but
		// there's no payment method to refund against. Reject with a
		// helpful error rather than silently routing to credits;
		// honor the explicit UI choice.
		invoices.invoices["inv_credit_only"] = domain.Invoice{
			ID: "inv_credit_only", TenantID: "t1", CustomerID: "cus_1",
			Status: domain.InvoicePaid, Currency: "USD",
			TotalAmountCents:    10000,
			AmountPaidCents:     0,
			CreditsAppliedCents: 10000,
		}
		_, err := svc.Create(ctx, "t1", CreateInput{
			InvoiceID:  "inv_credit_only",
			Reason:     "Refund attempt",
			RefundType: "refund",
			Lines:      []CreditLineInput{{Description: "Refund", Quantity: 1, UnitAmountCents: 10000}},
		})
		if err == nil {
			t.Fatal("expected refund-type CN on credit-only-paid invoice to reject")
		}
		// Error message should steer operator toward the right choice.
		msg := err.Error()
		if !strings.Contains(msg, "credit") {
			t.Errorf("error message should mention credit alternative: %q", msg)
		}
	})

	t.Run("credit-type CN restores credit balance on credit-only-paid invoice", func(t *testing.T) {
		// Same setup as above, but operator picks RefundType='credit'.
		// Should succeed — credit_amount = subtotal, restoring the
		// credit balance via the existing credit-grant path at Issue().
		invoices.invoices["inv_credit_only_2"] = domain.Invoice{
			ID: "inv_credit_only_2", TenantID: "t1", CustomerID: "cus_1",
			Status: domain.InvoicePaid, Currency: "USD",
			TotalAmountCents:    10000,
			AmountPaidCents:     0,
			CreditsAppliedCents: 10000,
		}
		cn, err := svc.Create(ctx, "t1", CreateInput{
			InvoiceID:  "inv_credit_only_2",
			Reason:     "Restore credits",
			RefundType: "credit",
			Lines:      []CreditLineInput{{Description: "Restore", Quantity: 1, UnitAmountCents: 10000}},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cn.RefundAmountCents != 0 {
			t.Errorf("refund_amount: got %d, want 0", cn.RefundAmountCents)
		}
		if cn.CreditAmountCents != 10000 {
			t.Errorf("credit_amount: got %d, want 10000", cn.CreditAmountCents)
		}
	})

	t.Run("refund REJECTED when amount exceeds PM-refundable on mixed-paid invoice", func(t *testing.T) {
		// $200 invoice, paid $50 by card + $150 by credits.
		// Operator picks Refund type with amount $80 — but only $50
		// was charged to card. Reject with a helpful error explaining
		// the options: lower to $50, or use Credit type for the full
		// amount, or split into two CNs.
		invoices.invoices["inv_mixed"] = domain.Invoice{
			ID: "inv_mixed", TenantID: "t1", CustomerID: "cus_1",
			Status: domain.InvoicePaid, Currency: "USD",
			TotalAmountCents: 20000,
			AmountPaidCents:  5000,
			// A card-settled invoice always carries a real PaymentIntent; without one
			// the refund rail is correctly unavailable (domain.HasCardPayment).
			StripePaymentIntentID: "pi_card_test",
			CreditsAppliedCents:   15000,
		}
		_, err := svc.Create(ctx, "t1", CreateInput{
			InvoiceID:  "inv_mixed",
			Reason:     "Partial refund attempt",
			RefundType: "refund",
			Lines:      []CreditLineInput{{Description: "Refund", Quantity: 1, UnitAmountCents: 8000}},
		})
		if err == nil {
			t.Fatal("expected refund-type CN above PM-refundable to reject")
		}
		msg := err.Error()
		if !strings.Contains(msg, "50.00") {
			t.Errorf("error message should mention the $50.00 PM-refundable max: %q", msg)
		}
	})

	t.Run("refund AT EXACTLY PM-refundable on mixed-paid invoice succeeds", func(t *testing.T) {
		// Same shape, but operator lowers the amount to $50 (the PM-
		// refundable max). Should succeed as a pure Stripe refund.
		invoices.invoices["inv_mixed_at_max"] = domain.Invoice{
			ID: "inv_mixed_at_max", TenantID: "t1", CustomerID: "cus_1",
			Status: domain.InvoicePaid, Currency: "USD",
			TotalAmountCents: 20000,
			AmountPaidCents:  5000,
			// A card-settled invoice always carries a real PaymentIntent; without one
			// the refund rail is correctly unavailable (domain.HasCardPayment).
			StripePaymentIntentID: "pi_card_test",
			CreditsAppliedCents:   15000,
		}
		cn, err := svc.Create(ctx, "t1", CreateInput{
			InvoiceID:  "inv_mixed_at_max",
			Reason:     "Refund at PM max",
			RefundType: "refund",
			Lines:      []CreditLineInput{{Description: "Refund", Quantity: 1, UnitAmountCents: 5000}},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cn.RefundAmountCents != 5000 {
			t.Errorf("refund_amount: got %d, want 5000", cn.RefundAmountCents)
		}
		if cn.CreditAmountCents != 0 {
			t.Errorf("credit_amount: got %d, want 0", cn.CreditAmountCents)
		}
	})

	t.Run("refund within PM-paid portion stays 100% PM", func(t *testing.T) {
		// Same mixed invoice as above. CN for $40 ≤ $50 PM portion
		// → all PM, no credit involvement.
		invoices.invoices["inv_mixed2"] = domain.Invoice{
			ID: "inv_mixed2", TenantID: "t1", CustomerID: "cus_1",
			Status: domain.InvoicePaid, Currency: "USD",
			TotalAmountCents: 20000,
			AmountPaidCents:  5000,
			// A card-settled invoice always carries a real PaymentIntent; without one
			// the refund rail is correctly unavailable (domain.HasCardPayment).
			StripePaymentIntentID: "pi_card_test",
			CreditsAppliedCents:   15000,
		}
		cn, err := svc.Create(ctx, "t1", CreateInput{
			InvoiceID:  "inv_mixed2",
			Reason:     "Refund within PM-paid portion",
			RefundType: "refund",
			Lines:      []CreditLineInput{{Description: "Refund", Quantity: 1, UnitAmountCents: 4000}},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cn.RefundAmountCents != 4000 {
			t.Errorf("refund_amount: got %d, want 4000 (within PM portion)", cn.RefundAmountCents)
		}
		if cn.CreditAmountCents != 0 {
			t.Errorf("credit_amount: got %d, want 0", cn.CreditAmountCents)
		}
	})

	t.Run("rejects refund that exceeds PM+credit refundable", func(t *testing.T) {
		invoices.invoices["inv_overcap"] = domain.Invoice{
			ID: "inv_overcap", TenantID: "t1", CustomerID: "cus_1",
			Status: domain.InvoicePaid, Currency: "USD",
			TotalAmountCents: 10000,
			AmountPaidCents:  3000,
			// A card-settled invoice always carries a real PaymentIntent; without one
			// the refund rail is correctly unavailable (domain.HasCardPayment).
			StripePaymentIntentID: "pi_card_test",
			CreditsAppliedCents:   7000,
		}
		_, err := svc.Create(ctx, "t1", CreateInput{
			InvoiceID:  "inv_overcap",
			Reason:     "Too big",
			RefundType: "refund",
			// 9999 fits in total but exists alongside other rules
			// Total refundable = 3000 + 7000 = 10000. 11000 > 10000.
			Lines: []CreditLineInput{{Description: "x", Quantity: 1, UnitAmountCents: 11000}},
		})
		if err == nil {
			t.Error("expected error when refund exceeds total refundable")
		}
	})

	t.Run("missing invoice", func(t *testing.T) {
		_, err := svc.Create(ctx, "t1", CreateInput{
			InvoiceID: "nonexistent", Reason: "test",
			Lines: []CreditLineInput{{Description: "x", Quantity: 1, UnitAmountCents: 100}},
		})
		if err == nil {
			t.Fatal("expected error for missing invoice")
		}
	})

	t.Run("draft invoice rejected", func(t *testing.T) {
		invoices.invoices["inv_draft"] = domain.Invoice{
			ID: "inv_draft", TenantID: "t1", Status: domain.InvoiceDraft,
		}
		_, err := svc.Create(ctx, "t1", CreateInput{
			InvoiceID: "inv_draft", Reason: "test",
			Lines: []CreditLineInput{{Description: "x", Quantity: 1, UnitAmountCents: 100}},
		})
		if err == nil {
			t.Fatal("expected error for draft invoice")
		}
	})

	t.Run("missing reason", func(t *testing.T) {
		_, err := svc.Create(ctx, "t1", CreateInput{
			InvoiceID: "inv_1",
			Lines:     []CreditLineInput{{Description: "x", Quantity: 1, UnitAmountCents: 100}},
		})
		if err == nil {
			t.Fatal("expected error for missing reason")
		}
	})

	t.Run("no lines", func(t *testing.T) {
		_, err := svc.Create(ctx, "t1", CreateInput{InvoiceID: "inv_1", Reason: "test"})
		if err == nil {
			t.Fatal("expected error for no lines")
		}
	})
}

// TestCreate_ExplicitAllocation locks in the Stripe + Lago three-channel
// shape: callers pass refund_amount_cents + credit_amount_cents +
// out_of_band_amount_cents whose sum equals the CN total. The legacy
// RefundType field is honored as a single-channel shortcut when all
// three explicit amounts are zero.
func TestCreate_ExplicitAllocation(t *testing.T) {
	t.Parallel()

	mkSvc := func(inv domain.Invoice) (*Service, *memStore) {
		store := newMemStore()
		invoices := &memInvoiceReader{invoices: map[string]domain.Invoice{inv.ID: inv}}
		svc := NewService(store, invoices, nil)
		svc.SetNumberGenerator(&fakeCNNumbers{})
		return svc, store
	}
	ctx := context.Background()

	t.Run("refund+credit split — Stripe rule example ($80 CN on $82.60 invoice)", func(t *testing.T) {
		// Customer paid $62.60 by card, $20 via credit balance. CN
		// for $80: operator routes $62.60 to PM (the full card
		// portion) and $17.40 to credit balance.
		svc, _ := mkSvc(domain.Invoice{
			ID: "inv_split", TenantID: "t1", CustomerID: "cus_1",
			Status: domain.InvoicePaid, Currency: "USD",
			TotalAmountCents: 8260, AmountPaidCents: 6260, CreditsAppliedCents: 2000,
			// A card-settled invoice always carries a real PaymentIntent; without one
			// the refund rail is correctly unavailable (domain.HasCardPayment).
			StripePaymentIntentID: "pi_card_test",
		})
		cn, err := svc.Create(ctx, "t1", CreateInput{
			InvoiceID:         "inv_split",
			Reason:            "Partial refund + credit restore",
			RefundAmountCents: 6260,
			CreditAmountCents: 1740,
			Lines:             []CreditLineInput{{Description: "Refund", Quantity: 1, UnitAmountCents: 8000}},
		})
		if err != nil {
			t.Fatalf("Create split CN: %v", err)
		}
		if cn.RefundAmountCents != 6260 {
			t.Errorf("refund_amount_cents: got %d want 6260", cn.RefundAmountCents)
		}
		if cn.CreditAmountCents != 1740 {
			t.Errorf("credit_amount_cents: got %d want 1740", cn.CreditAmountCents)
		}
		if cn.OutOfBandAmountCents != 0 {
			t.Errorf("out_of_band_amount_cents: got %d want 0", cn.OutOfBandAmountCents)
		}
	})

	t.Run("three-channel split — refund + credit + out-of-band", func(t *testing.T) {
		// $100 CN: $40 to PM, $30 to credit, $30 marked as
		// handled outside Stripe (e.g. operator already wrote a
		// check). All three fields used together.
		svc, _ := mkSvc(domain.Invoice{
			ID: "inv_3ch", TenantID: "t1", CustomerID: "cus_1",
			Status: domain.InvoicePaid, Currency: "USD",
			TotalAmountCents: 10000, AmountPaidCents: 10000,
			// A card-settled invoice always carries a real PaymentIntent; without one
			// the refund rail is correctly unavailable (domain.HasCardPayment).
			StripePaymentIntentID: "pi_card_test",
		})
		cn, err := svc.Create(ctx, "t1", CreateInput{
			InvoiceID:            "inv_3ch",
			Reason:               "Mixed channels",
			RefundAmountCents:    4000,
			CreditAmountCents:    3000,
			OutOfBandAmountCents: 3000,
			Lines:                []CreditLineInput{{Description: "Mixed", Quantity: 1, UnitAmountCents: 10000}},
		})
		if err != nil {
			t.Fatalf("Create 3-channel CN: %v", err)
		}
		if cn.RefundAmountCents != 4000 || cn.CreditAmountCents != 3000 || cn.OutOfBandAmountCents != 3000 {
			t.Errorf("allocation: got (%d,%d,%d), want (4000,3000,3000)",
				cn.RefundAmountCents, cn.CreditAmountCents, cn.OutOfBandAmountCents)
		}
	})

	t.Run("default when paid invoice + all three zero + no RefundType → credit only", func(t *testing.T) {
		svc, _ := mkSvc(domain.Invoice{
			ID: "inv_default", TenantID: "t1", CustomerID: "cus_1",
			Status: domain.InvoicePaid, Currency: "USD",
			TotalAmountCents: 5000, AmountPaidCents: 5000,
			// A card-settled invoice always carries a real PaymentIntent; without one
			// the refund rail is correctly unavailable (domain.HasCardPayment).
			StripePaymentIntentID: "pi_card_test",
		})
		cn, err := svc.Create(ctx, "t1", CreateInput{
			InvoiceID: "inv_default",
			Reason:    "Operator picked nothing",
			Lines:     []CreditLineInput{{Description: "x", Quantity: 1, UnitAmountCents: 5000}},
		})
		if err != nil {
			t.Fatalf("Create with default routing: %v", err)
		}
		if cn.CreditAmountCents != 5000 {
			t.Errorf("default routing should be credit-only; got refund=%d credit=%d oob=%d",
				cn.RefundAmountCents, cn.CreditAmountCents, cn.OutOfBandAmountCents)
		}
	})

	t.Run("sum mismatch rejected", func(t *testing.T) {
		svc, _ := mkSvc(domain.Invoice{
			ID: "inv_mismatch", TenantID: "t1", CustomerID: "cus_1",
			Status: domain.InvoicePaid, Currency: "USD",
			TotalAmountCents: 10000, AmountPaidCents: 10000,
			// A card-settled invoice always carries a real PaymentIntent; without one
			// the refund rail is correctly unavailable (domain.HasCardPayment).
			StripePaymentIntentID: "pi_card_test",
		})
		_, err := svc.Create(ctx, "t1", CreateInput{
			InvoiceID:         "inv_mismatch",
			Reason:            "Bad sum",
			RefundAmountCents: 3000,
			CreditAmountCents: 3000,
			// 6000 != 10000 line total
			Lines: []CreditLineInput{{Description: "x", Quantity: 1, UnitAmountCents: 10000}},
		})
		if err == nil {
			t.Fatal("expected sum-mismatch rejection")
		}
		if !strings.Contains(err.Error(), "must equal") {
			t.Errorf("error should mention sum invariant: %q", err.Error())
		}
	})

	t.Run("refund > pmRefundable rejected — explicit fields", func(t *testing.T) {
		// $200 invoice, $50 PM + $150 credit. Operator asks for
		// $80 to PM — only $50 available. Error must steer to
		// credit_amount_cents.
		svc, _ := mkSvc(domain.Invoice{
			ID: "inv_overrefund", TenantID: "t1", CustomerID: "cus_1",
			Status: domain.InvoicePaid, Currency: "USD",
			TotalAmountCents: 20000, AmountPaidCents: 5000, CreditsAppliedCents: 15000,
			// A card-settled invoice always carries a real PaymentIntent; without one
			// the refund rail is correctly unavailable (domain.HasCardPayment).
			StripePaymentIntentID: "pi_card_test",
		})
		_, err := svc.Create(ctx, "t1", CreateInput{
			InvoiceID:         "inv_overrefund",
			Reason:            "Too much to PM",
			RefundAmountCents: 8000,
			Lines:             []CreditLineInput{{Description: "x", Quantity: 1, UnitAmountCents: 8000}},
		})
		if err == nil {
			t.Fatal("expected refund > pmRefundable to reject")
		}
		if !strings.Contains(err.Error(), "50.00") {
			t.Errorf("error should mention $50.00 PM-refundable max: %q", err.Error())
		}
	})

	t.Run("negative amount rejected", func(t *testing.T) {
		svc, _ := mkSvc(domain.Invoice{
			ID: "inv_neg", TenantID: "t1", CustomerID: "cus_1",
			Status: domain.InvoicePaid, Currency: "USD",
			TotalAmountCents: 10000, AmountPaidCents: 10000,
			// A card-settled invoice always carries a real PaymentIntent; without one
			// the refund rail is correctly unavailable (domain.HasCardPayment).
			StripePaymentIntentID: "pi_card_test",
		})
		_, err := svc.Create(ctx, "t1", CreateInput{
			InvoiceID:         "inv_neg",
			Reason:            "Bad",
			RefundAmountCents: -100,
			CreditAmountCents: 10100,
			Lines:             []CreditLineInput{{Description: "x", Quantity: 1, UnitAmountCents: 10000}},
		})
		if err == nil {
			t.Fatal("expected negative-amount rejection")
		}
	})

	t.Run("out-of-band only on credit-paid invoice", func(t *testing.T) {
		// Invoice paid 100% by credits. Operator manually
		// processed the refund outside Stripe (cash/wire) and
		// records it as out-of-band — no PM refund, no credit
		// restore. Sum check still applies; pmRefundable is 0
		// so refund_amount_cents must be 0.
		svc, _ := mkSvc(domain.Invoice{
			ID: "inv_oob", TenantID: "t1", CustomerID: "cus_1",
			Status: domain.InvoicePaid, Currency: "USD",
			TotalAmountCents: 5000, AmountPaidCents: 0, CreditsAppliedCents: 5000,
		})
		cn, err := svc.Create(ctx, "t1", CreateInput{
			InvoiceID:            "inv_oob",
			Reason:               "Wire refund processed",
			OutOfBandAmountCents: 5000,
			Lines:                []CreditLineInput{{Description: "Wired $50", Quantity: 1, UnitAmountCents: 5000}},
		})
		if err != nil {
			t.Fatalf("Create OOB-only CN: %v", err)
		}
		if cn.OutOfBandAmountCents != 5000 {
			t.Errorf("out_of_band_amount_cents: got %d want 5000", cn.OutOfBandAmountCents)
		}
		if cn.RefundAmountCents != 0 || cn.CreditAmountCents != 0 {
			t.Errorf("expected refund=credit=0; got refund=%d credit=%d",
				cn.RefundAmountCents, cn.CreditAmountCents)
		}
	})

	t.Run("legacy RefundType=refund honored when explicit fields zero", func(t *testing.T) {
		svc, _ := mkSvc(domain.Invoice{
			ID: "inv_legacy_refund", TenantID: "t1", CustomerID: "cus_1",
			Status: domain.InvoicePaid, Currency: "USD",
			TotalAmountCents: 10000, AmountPaidCents: 10000,
			// A card-settled invoice always carries a real PaymentIntent; without one
			// the refund rail is correctly unavailable (domain.HasCardPayment).
			StripePaymentIntentID: "pi_card_test",
		})
		cn, err := svc.Create(ctx, "t1", CreateInput{
			InvoiceID:  "inv_legacy_refund",
			Reason:     "Legacy SDK",
			RefundType: "refund",
			Lines:      []CreditLineInput{{Description: "x", Quantity: 1, UnitAmountCents: 10000}},
		})
		if err != nil {
			t.Fatalf("Create with legacy refund-type: %v", err)
		}
		if cn.RefundAmountCents != 10000 {
			t.Errorf("legacy refund-type translation: got refund=%d want 10000", cn.RefundAmountCents)
		}
	})
}

// TestCreate_SmartBucketTaxResidual locks in the Stripe-Tax-style
// "last CN absorbs the rounding residual" behavior (2026-05-25). Pre-fix:
// proportional tax via integer division left a 1-cent gap when an invoice
// was split into multiple CNs whose totals exhausted it. Post-fix: the
// CN that brings cumulative coverage to 100% gets `inv.tax - prior_tax`
// so the sum of per-CN tax exactly equals the invoice tax.
func TestCreate_SmartBucketTaxResidual(t *testing.T) {
	t.Parallel()

	// $82.60 invoice = $70 net + $12.60 IGST (18%).
	inv := domain.Invoice{
		ID: "inv_split", TenantID: "t1", CustomerID: "cus_1",
		Status: domain.InvoicePaid, Currency: "INR",
		TotalAmountCents: 8260, TaxFacts: domain.TaxFacts{TaxAmountCents: 1260},
		AmountPaidCents: 8260,
		// A card-settled invoice always carries a real PaymentIntent; without one
		// the refund rail is correctly unavailable (domain.HasCardPayment).
		StripePaymentIntentID: "pi_card_test",
	}
	store := newMemStore()
	invoices := &memInvoiceReader{invoices: map[string]domain.Invoice{inv.ID: inv}}
	svc := NewService(store, invoices, nil)
	svc.SetNumberGenerator(&fakeCNNumbers{})
	ctx := context.Background()

	// CN-A: $2.60 — proportional tax floor(1260*260/8260) = floor(39.66) = 39c.
	cnA, err := svc.Create(ctx, "t1", CreateInput{
		InvoiceID:         inv.ID,
		Reason:            "billing error 1",
		RefundAmountCents: 260,
		Lines:             []CreditLineInput{{Description: "x", Quantity: 1, UnitAmountCents: 260}},
	})
	if err != nil {
		t.Fatalf("CN A: %v", err)
	}
	if cnA.TaxAmountCents != 39 {
		t.Errorf("CN A tax: got %d want 39 (floor of proportional)", cnA.TaxAmountCents)
	}

	// CN-B: $80 — brings cumulative CN total to $82.60 (== invoice total).
	// Smart-bucket: absorb residual. tax = 1260 - 39 = 1221 (not the
	// pure proportional floor(1260*8000/8260) = 1220).
	cnB, err := svc.Create(ctx, "t1", CreateInput{
		InvoiceID:         inv.ID,
		Reason:            "billing error 2",
		CreditAmountCents: 8000,
		Lines:             []CreditLineInput{{Description: "y", Quantity: 1, UnitAmountCents: 8000}},
	})
	if err != nil {
		t.Fatalf("CN B: %v", err)
	}
	if cnB.TaxAmountCents != 1221 {
		t.Errorf("CN B tax: got %d want 1221 (residual-absorbing — not the floor() proportional 1220)",
			cnB.TaxAmountCents)
	}

	// Cumulative tax reversed must equal the original invoice tax exactly.
	if total := cnA.TaxAmountCents + cnB.TaxAmountCents; total != inv.TaxAmountCents {
		t.Errorf("cumulative tax reversed: got %d want %d (invoice tax) — residual not absorbed",
			total, inv.TaxAmountCents)
	}
}

// TestCreate_PartialCNUsesPureProportional pins that non-exhausting CNs
// stay on the pure proportional formula (so two partial CNs that don't
// add up to the invoice total still produce a small residual — the
// residual only gets absorbed when a CN actually completes the invoice).
func TestCreate_PartialCNUsesPureProportional(t *testing.T) {
	t.Parallel()

	inv := domain.Invoice{
		ID: "inv_partial", TenantID: "t1", CustomerID: "cus_1",
		Status: domain.InvoicePaid, Currency: "INR",
		TotalAmountCents: 8260, TaxFacts: domain.TaxFacts{TaxAmountCents: 1260},
		AmountPaidCents: 8260,
		// A card-settled invoice always carries a real PaymentIntent; without one
		// the refund rail is correctly unavailable (domain.HasCardPayment).
		StripePaymentIntentID: "pi_card_test",
	}
	store := newMemStore()
	invoices := &memInvoiceReader{invoices: map[string]domain.Invoice{inv.ID: inv}}
	svc := NewService(store, invoices, nil)
	svc.SetNumberGenerator(&fakeCNNumbers{})
	ctx := context.Background()

	// Single $40 CN — doesn't exhaust the $82.60 invoice. Should use
	// pure proportional: floor(1260*4000/8260) = floor(610.16) = 610c.
	cn, err := svc.Create(ctx, "t1", CreateInput{
		InvoiceID:         inv.ID,
		Reason:            "partial",
		CreditAmountCents: 4000,
		Lines:             []CreditLineInput{{Description: "x", Quantity: 1, UnitAmountCents: 4000}},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if cn.TaxAmountCents != 610 {
		t.Errorf("partial CN tax: got %d want 610 (pure proportional, no residual absorption)",
			cn.TaxAmountCents)
	}
}

func TestIssueAndVoid_CreditNote(t *testing.T) {
	store := newMemStore()
	invoices := &memInvoiceReader{
		invoices: map[string]domain.Invoice{
			"inv_1": {ID: "inv_1", TenantID: "t1", Status: domain.InvoiceFinalized, Currency: "USD", TotalAmountCents: 10000, AmountDueCents: 10000},
		},
	}
	svc := NewService(store, invoices, nil)
	svc.SetNumberGenerator(&fakeCNNumbers{})
	ctx := context.Background()

	cn, _ := svc.Create(ctx, "t1", CreateInput{
		InvoiceID: "inv_1", Reason: "Adjustment",
		Lines: []CreditLineInput{{Description: "adj", Quantity: 1, UnitAmountCents: 1000}},
	})

	t.Run("issue", func(t *testing.T) {
		issued, err := svc.Issue(ctx, "t1", cn.ID)
		if err != nil {
			t.Fatalf("issue: %v", err)
		}
		if issued.Status != domain.CreditNoteIssued {
			t.Errorf("status: got %q, want issued", issued.Status)
		}
		if issued.IssuedAt == nil {
			t.Error("issued_at should be set")
		}
		if got := invoices.invoices["inv_1"].AmountDueCents; got != 9000 {
			t.Errorf("amount_due after issue: got %d, want 9000 (10000 - 1000 CN)", got)
		}
	})

	t.Run("cannot issue again", func(t *testing.T) {
		_, err := svc.Issue(ctx, "t1", cn.ID)
		if err == nil {
			t.Fatal("expected error issuing non-draft")
		}
	})

	t.Run("cannot void issued", func(t *testing.T) {
		_, err := svc.Void(ctx, "t1", cn.ID)
		if err == nil {
			t.Fatal("expected error voiding issued credit note")
		}
	})

	t.Run("void draft", func(t *testing.T) {
		// Create a new CN and void it while still draft
		draft, _ := svc.Create(ctx, "t1", CreateInput{
			InvoiceID: "inv_1", Reason: "Draft to void",
			Lines: []CreditLineInput{{Description: "test", Quantity: 1, UnitAmountCents: 500}},
		})
		voided, err := svc.Void(ctx, "t1", draft.ID)
		if err != nil {
			t.Fatalf("void draft: %v", err)
		}
		if voided.Status != domain.CreditNoteVoided {
			t.Errorf("status: got %q, want voided", voided.Status)
		}
	})
}

// TestCreate_NumberAllocatorErrorAborts locks the no-silent-fallbacks fix:
// a failed credit-note-number allocation must ABORT Create — the previous
// behavior swallowed the error and synthesized a timestamp-based number
// (CN-YYYYMM-<unixnano%1e6>), which is non-monotonic and collision-prone.
// A duplicate document number corrupts accounting; a failed Create retries.
func TestCreate_NumberAllocatorErrorAborts(t *testing.T) {
	store := newMemStore()
	invoices := &memInvoiceReader{
		invoices: map[string]domain.Invoice{
			"inv_1": {
				ID: "inv_1", TenantID: "t1", CustomerID: "cus_1",
				Status: domain.InvoiceFinalized, Currency: "USD",
				TotalAmountCents: 10000, AmountDueCents: 10000,
			},
		},
	}
	in := CreateInput{
		InvoiceID: "inv_1", Reason: "adjustment",
		Lines: []CreditLineInput{{Description: "adj", Quantity: 1, UnitAmountCents: 5000}},
	}

	t.Run("allocator error → abort, nothing persisted", func(t *testing.T) {
		svc := NewService(store, invoices, nil)
		svc.SetNumberGenerator(&fakeCNNumbers{err: fmt.Errorf("sequence unavailable")})
		_, err := svc.Create(context.Background(), "t1", in)
		if err == nil {
			t.Fatal("expected Create to fail when the number allocator errors (no synthesized fallback)")
		}
		if !strings.Contains(err.Error(), "allocate credit note number") {
			t.Errorf("error %q: want the wrapped allocator error", err)
		}
		if len(store.notes) != 0 {
			t.Errorf("store has %d notes, want 0 — a failed allocation must not persist a credit note", len(store.notes))
		}
	})

	t.Run("generator unwired → loud config error", func(t *testing.T) {
		svc := NewService(store, invoices, nil)
		_, err := svc.Create(context.Background(), "t1", in)
		if err == nil || !strings.Contains(err.Error(), "number generator not wired") {
			t.Fatalf("error = %v, want the not-wired config error", err)
		}
	})
}

// TestCreate_LinesCommitWithHeader pins the atomicity refactor's WIRE SHAPE
// at runtime: the line items must arrive INSIDE the CreateUnderInvoiceLock
// call (one transaction with the header) — asserted via the store spy — not
// in a per-line loop after the header committed (the shape that left an
// orphan CN with a non-zero total and no lines). The single-transaction
// guarantee itself is structural (insertLineItemTx is only reachable inside
// the lock's tx) and enforced at compile time by the Store interface.
func TestCreate_LinesCommitWithHeader(t *testing.T) {
	store := newMemStore()
	invoices := &memInvoiceReader{
		invoices: map[string]domain.Invoice{
			"inv_1": {
				ID: "inv_1", TenantID: "t1", CustomerID: "cus_1",
				Status: domain.InvoiceFinalized, Currency: "USD",
				TotalAmountCents: 10000, AmountDueCents: 10000,
			},
		},
	}
	svc := NewService(store, invoices, nil)
	svc.SetNumberGenerator(&fakeCNNumbers{})

	cn, err := svc.Create(context.Background(), "t1", CreateInput{
		InvoiceID: "inv_1", Reason: "partial credit",
		Lines: []CreditLineInput{
			{Description: "line a", Quantity: 2, UnitAmountCents: 1000},
			{Description: "line b", Quantity: 1, UnitAmountCents: 3000},
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// The runtime pin: the two lines were handed to the lock call itself.
	// Pre-fix this slice was empty — lines were inserted in a loop AFTER the
	// header's transaction committed.
	if len(store.lastLockLines) != 2 {
		t.Fatalf("lines passed into CreateUnderInvoiceLock = %d, want 2 (lines must commit with the header)", len(store.lastLockLines))
	}
	lines, err := store.ListLineItems(context.Background(), "t1", cn.ID)
	if err != nil {
		t.Fatalf("ListLineItems: %v", err)
	}
	if len(lines) != 2 {
		t.Fatalf("lines = %d, want 2 (committed with the header)", len(lines))
	}
	var sum int64
	for _, l := range lines {
		if l.CreditNoteID != cn.ID {
			t.Errorf("line %s: credit_note_id %q, want %q", l.ID, l.CreditNoteID, cn.ID)
		}
		sum += l.AmountCents
	}
	if sum != cn.TotalCents || cn.TotalCents != 5000 {
		t.Errorf("Σ line amounts %d vs CN total %d, want both 5000 (gross lines sum to Credit Total)", sum, cn.TotalCents)
	}
}

// simResolver pins every invoice to a fixed simulated instant, standing in for
// the billing engine's clock.Resolver.
type simResolver struct{ at time.Time }

func (r *simResolver) SimForCustomer(context.Context, string, string) (clock.Sim, error) {
	return clock.Sim{}, errs.ErrNotFound
}
func (r *simResolver) SimForSubscription(context.Context, string, string) (clock.Sim, error) {
	return clock.Sim{}, errs.ErrNotFound
}
func (r *simResolver) SimForInvoice(_ context.Context, _, _ string) (clock.Sim, error) {
	return clock.Sim{At: r.at, TestClockID: "vlx_tclk_test"}, nil
}

// TestOperatorCreditNote_StampsSimulatedTime pins ADR-030 on the operator
// credit-note path: "bind effective-now at every operator entry point on a
// clock-pinned entity", with credit notes and their CreditGrant rows named as
// simulated-when-pinned.
//
// Pre-fix, creditnote.Service held no resolver at all, so only the ENGINE's
// clawback landed on the simulated axis (its callers bind) while an operator
// credit note on the same invoice was stamped wall-clock — producing a credit
// note dated weeks BEFORE the invoice it credits, and a ledger grant the
// clock-scoped catchup scans compare against frozen_time.
func TestOperatorCreditNote_StampsSimulatedTime(t *testing.T) {
	simAt := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	store := newMemStore()
	invoices := &memInvoiceReader{invoices: map[string]domain.Invoice{
		"inv_sim": {
			ID: "inv_sim", TenantID: "t1", CustomerID: "cus_1",
			Status: domain.InvoiceFinalized, Currency: "USD",
			TotalAmountCents: 10000, AmountDueCents: 10000,
		},
	}}
	svc := NewService(store, invoices, nil)
	svc.SetNumberGenerator(&fakeCNNumbers{})
	svc.SetResolver(&simResolver{at: simAt})

	cn, err := svc.Create(context.Background(), "t1", CreateInput{
		InvoiceID: "inv_sim",
		Reason:    "Goodwill credit",
		Lines:     []CreditLineInput{{Description: "adjustment", Quantity: 1, UnitAmountCents: 500}},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !cn.CreatedAt.Equal(simAt) {
		t.Errorf("created_at = %s, want the invoice's simulated instant %s — an operator credit note on a clock-pinned invoice must not be stamped wall-clock", cn.CreatedAt, simAt)
	}
}

// TestOperatorCreditNote_UnpinnedStaysWallClock is the negative: with no
// simulated pin the bind is a no-op and the row keeps wall-clock time.
func TestOperatorCreditNote_UnpinnedStaysWallClock(t *testing.T) {
	store := newMemStore()
	invoices := &memInvoiceReader{invoices: map[string]domain.Invoice{
		"inv_real": {
			ID: "inv_real", TenantID: "t1", CustomerID: "cus_1",
			Status: domain.InvoiceFinalized, Currency: "USD",
			TotalAmountCents: 10000, AmountDueCents: 10000,
		},
	}}
	svc := NewService(store, invoices, nil)
	svc.SetNumberGenerator(&fakeCNNumbers{})
	// No resolver wired at all — the unpinned production shape.

	before := time.Now().UTC().Add(-time.Minute)
	cn, err := svc.Create(context.Background(), "t1", CreateInput{
		InvoiceID: "inv_real",
		Reason:    "Goodwill credit",
		Lines:     []CreditLineInput{{Description: "adjustment", Quantity: 1, UnitAmountCents: 500}},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if cn.CreatedAt.Before(before) {
		t.Errorf("created_at = %s, want wall-clock (>= %s) on a non-simulated invoice", cn.CreatedAt, before)
	}
}
