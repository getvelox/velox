package payment

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// memChargeIntents is an in-memory ChargeIntentRecorder + ChargeIntentLedger
// that FAITHFULLY models the two guarantees the real table enforces in SQL:
// one unresolved intent per invoice (the partial unique index), and a stable
// identity per idempotency key. A fake that merely returns success would let a
// double-charge test pass against a ledger that never blocked anything.
type memChargeIntents struct {
	mu      sync.Mutex
	rows    map[string]*domain.ChargeIntent // id → row
	nextID  int
	openErr error // when set, Open fails — exercising the fail-closed path
}

func newMemChargeIntents() *memChargeIntents {
	return &memChargeIntents{rows: map[string]*domain.ChargeIntent{}}
}

func (m *memChargeIntents) Open(_ context.Context, in domain.ChargeIntent) (domain.ChargeIntent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.openErr != nil {
		return domain.ChargeIntent{}, m.openErr
	}
	// The one-unresolved-per-invoice index: an existing unresolved row wins.
	for _, r := range m.rows {
		if r.InvoiceID == in.InvoiceID && r.TenantID == in.TenantID && r.State != domain.ChargeIntentResolved {
			return *r, nil
		}
	}
	m.nextID++
	row := in
	row.ID = fmt.Sprintf("vlx_cin_mem%d", m.nextID)
	row.State = domain.ChargeIntentOpen
	if row.OccurredAt.IsZero() {
		row.OccurredAt = time.Now().UTC()
	}
	m.rows[row.ID] = &row
	return row, nil
}

func (m *memChargeIntents) Resolve(_ context.Context, _, id, piID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.rows[id]; ok && r.State != domain.ChargeIntentResolved {
		r.State = domain.ChargeIntentResolved
		r.StripePaymentIntentID = piID
	}
	return nil
}

func (m *memChargeIntents) Delete(_ context.Context, _, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.rows[id]; ok && r.State == domain.ChargeIntentOpen {
		delete(m.rows, id)
	}
	return nil
}

func (m *memChargeIntents) ListOpen(_ context.Context, olderThan time.Time, limit int) ([]domain.ChargeIntent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []domain.ChargeIntent
	for _, r := range m.rows {
		if r.State == domain.ChargeIntentOpen && r.UpdatedAt.Before(olderThan) {
			out = append(out, *r)
		}
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (m *memChargeIntents) BumpRecoveryAttempt(_ context.Context, _, id string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[id]
	if !ok || r.State != domain.ChargeIntentOpen {
		return false, nil
	}
	r.RecoveryAttempts++
	return true, nil
}

func (m *memChargeIntents) MarkNeedsReview(_ context.Context, _, id, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.rows[id]; ok && r.State == domain.ChargeIntentOpen {
		r.State = domain.ChargeIntentNeedsReview
		r.LastError = reason
	}
	return nil
}

func (m *memChargeIntents) HasUnresolved(_ context.Context, tenantID, invoiceID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.rows {
		if r.TenantID == tenantID && r.InvoiceID == invoiceID && r.State != domain.ChargeIntentResolved {
			return true, nil
		}
	}
	return false, nil
}

func (m *memChargeIntents) get(id string) domain.ChargeIntent {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.rows[id]; ok {
		return *r
	}
	return domain.ChargeIntent{}
}

func (m *memChargeIntents) all() []domain.ChargeIntent {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []domain.ChargeIntent
	for _, r := range m.rows {
		out = append(out, *r)
	}
	return out
}
