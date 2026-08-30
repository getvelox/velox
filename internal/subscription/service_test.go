package subscription

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
)

// memStore is an in-memory Store implementation used across the subscription
// package's unit tests. It does not enforce every invariant the postgres store
// does — it covers just the behaviour the service layer depends on (item CRUD,
// atomic transitions, and pending-item bookkeeping).
type memStore struct {
	// clearPauseOverride, when set, is what ClearPauseCollection returns —
	// simulates a sibling leader having cleared the pause between the list
	// and the clear (HA hazard 5).
	clearPauseOverride error
	subs               map[string]domain.Subscription
	items              map[string]domain.SubscriptionItem
	itemChanges        []domain.SubscriptionItemChange
}

func newMemStore() *memStore {
	return &memStore{
		subs:  make(map[string]domain.Subscription),
		items: make(map[string]domain.SubscriptionItem),
	}
}

func (m *memStore) Create(ctx context.Context, tenantID string, s domain.Subscription) (domain.Subscription, error) {
	for _, existing := range m.subs {
		if existing.TenantID == tenantID && existing.Code == s.Code {
			return domain.Subscription{}, fmt.Errorf("%w: subscription code %q", errs.ErrAlreadyExists, s.Code)
		}
	}
	s.ID = fmt.Sprintf("vlx_sub_%d", len(m.subs)+1)
	s.TenantID = tenantID
	now := clock.Now(ctx)
	s.CreatedAt = now
	s.UpdatedAt = now

	hydrated := make([]domain.SubscriptionItem, 0, len(s.Items))
	for i, it := range s.Items {
		it.ID = fmt.Sprintf("%s_item_%d", s.ID, i+1)
		it.SubscriptionID = s.ID
		it.TenantID = tenantID
		it.CreatedAt = now
		it.UpdatedAt = now
		m.items[it.ID] = it
		hydrated = append(hydrated, it)
		m.recordChange(it, "add", "", it.PlanID, 0, it.Quantity, now)
	}
	s.Items = hydrated
	m.subs[s.ID] = s
	return s, nil
}

func (m *memStore) CreateWithBill(ctx context.Context, tenantID string, s domain.Subscription, billFn func(tx *sql.Tx, created domain.Subscription) error) (domain.Subscription, error) {
	created, err := m.Create(ctx, tenantID, s)
	if err != nil {
		return domain.Subscription{}, err
	}
	if billFn != nil {
		if err := billFn(nil, created); err != nil {
			// Simulate the tx rollback: the real store does not persist the
			// subscription (or its items) when billFn fails.
			delete(m.subs, created.ID)
			for _, it := range created.Items {
				delete(m.items, it.ID)
			}
			return domain.Subscription{}, err
		}
	}
	return created, nil
}

// recordChange mirrors the DB trigger that fills
// subscription_item_changes (migration 0029). Each mutation through
// the mem store appends a change row so tests that exercise
// segment-aware billing see the same audit log shape as production.
func (m *memStore) recordChange(it domain.SubscriptionItem, changeType, fromPlanID, toPlanID string, fromQty, toQty int64, at time.Time) {
	m.itemChanges = append(m.itemChanges, domain.SubscriptionItemChange{
		ID:                 fmt.Sprintf("vlx_sic_%d", len(m.itemChanges)+1),
		TenantID:           it.TenantID,
		SubscriptionID:     it.SubscriptionID,
		SubscriptionItemID: it.ID,
		ChangeType:         changeType,
		FromPlanID:         fromPlanID,
		ToPlanID:           toPlanID,
		FromQuantity:       fromQty,
		ToQuantity:         toQty,
		ChangedAt:          at,
		CreatedAt:          at,
	})
}

func (m *memStore) Get(ctx context.Context, tenantID, id string) (domain.Subscription, error) {
	s, ok := m.subs[id]
	if !ok || s.TenantID != tenantID {
		return domain.Subscription{}, errs.ErrNotFound
	}
	s.Items = m.hydrateItems(s.ID)
	return s, nil
}

func (m *memStore) List(ctx context.Context, filter ListFilter) ([]domain.Subscription, int, error) {
	var result []domain.Subscription
	for _, s := range m.subs {
		if s.TenantID != filter.TenantID {
			continue
		}
		if filter.Status != "" && string(s.Status) != filter.Status {
			continue
		}
		s.Items = m.hydrateItems(s.ID)
		result = append(result, s)
	}
	return result, len(result), nil
}

func (m *memStore) Update(ctx context.Context, tenantID string, s domain.Subscription) (domain.Subscription, error) {
	cur, ok := m.subs[s.ID]
	if !ok || cur.TenantID != tenantID {
		return domain.Subscription{}, errs.ErrNotFound
	}
	s.Items = cur.Items
	s.UpdatedAt = clock.Now(ctx)
	m.subs[s.ID] = s
	return s, nil
}

func (m *memStore) GetDueBilling(ctx context.Context, _ time.Time, _ int) ([]domain.Subscription, error) {
	return nil, nil
}

// ClosePeriodTx mirrors the store's ADR-115 CAS exactly: the row must still
// match `expected` (status, period start, watermark) or nothing changes.
func (m *memStore) ClosePeriodTx(_ context.Context, _ *sql.Tx, tenantID, id string, expected PeriodSnapshot, periodStart, periodEnd, nextBillingAt time.Time, anchorDay int) error {
	s, ok := m.subs[id]
	if !ok || s.TenantID != tenantID {
		return errs.ErrNotFound
	}
	if !snapshotMatches(s, expected) {
		return ErrWatermarkMoved
	}
	s.CurrentBillingPeriodStart = &periodStart
	s.CurrentBillingPeriodEnd = &periodEnd
	s.NextBillingAt = &nextBillingAt
	s.BillingAnchorDay = anchorDay
	m.subs[id] = s
	return nil
}

func (m *memStore) ClosePeriod(ctx context.Context, tenantID, id string, expected PeriodSnapshot, periodStart, periodEnd, nextBillingAt time.Time, anchorDay int) error {
	return m.ClosePeriodTx(ctx, nil, tenantID, id, expected, periodStart, periodEnd, nextBillingAt, anchorDay)
}

func snapshotMatches(s domain.Subscription, expected PeriodSnapshot) bool {
	sameTime := func(a, b *time.Time) bool {
		if a == nil || b == nil {
			return a == nil && b == nil
		}
		return a.Equal(*b)
	}
	return s.Status == expected.Status && sameTime(s.CurrentBillingPeriodStart, expected.Start) && sameTime(s.NextBillingAt, expected.Next)
}

func (m *memStore) CancelAtomic(ctx context.Context, tenantID, id string) (domain.Subscription, error) {
	s, ok := m.subs[id]
	if !ok || s.TenantID != tenantID {
		return domain.Subscription{}, errs.ErrNotFound
	}
	if s.Status == domain.SubscriptionCanceled || s.Status == domain.SubscriptionArchived {
		return domain.Subscription{}, errs.InvalidState(fmt.Sprintf("cannot cancel %s subscription (already terminated)", s.Status))
	}
	now := clock.Now(ctx)
	s.Status = domain.SubscriptionCanceled
	s.CanceledAt = &now
	s.UpdatedAt = now
	m.subs[id] = s
	return s, nil
}

func (m *memStore) CancelAtomicWithBill(ctx context.Context, tenantID, id string, billFn func(tx *sql.Tx, canceled domain.Subscription) error) (domain.Subscription, error) {
	prev, existed := m.subs[id]
	canceled, err := m.CancelAtomic(ctx, tenantID, id)
	if err != nil {
		return domain.Subscription{}, err
	}
	if billFn != nil {
		if err := billFn(nil, canceled); err != nil {
			// Simulate the tx rollback: restore the pre-cancel row.
			if existed {
				m.subs[id] = prev
			}
			return domain.Subscription{}, err
		}
	}
	return canceled, nil
}

func (m *memStore) ScheduleCancellation(ctx context.Context, tenantID, id string, cancelAt *time.Time, cancelAtPeriodEnd bool, observedStatus domain.SubscriptionStatus) (domain.Subscription, error) {
	s, ok := m.subs[id]
	if !ok || s.TenantID != tenantID {
		return domain.Subscription{}, errs.ErrNotFound
	}
	if s.Status == domain.SubscriptionCanceled || s.Status == domain.SubscriptionArchived {
		return domain.Subscription{}, fmt.Errorf("cannot schedule cancellation on %s subscription", s.Status)
	}
	// Mirror the store's ADR-069 status CAS.
	if s.Status != observedStatus {
		return domain.Subscription{}, errs.InvalidState(fmt.Sprintf(
			"subscription changed from %s to %s while scheduling — re-read and retry (the cancel's meaning depends on status)",
			observedStatus, s.Status))
	}
	s.CancelAt = cancelAt
	s.CancelAtPeriodEnd = cancelAtPeriodEnd
	s.UpdatedAt = clock.Now(ctx)
	m.subs[id] = s
	s.Items = m.hydrateItems(id)
	return s, nil
}

func (m *memStore) ClearScheduledCancellation(ctx context.Context, tenantID, id string) (domain.Subscription, error) {
	s, ok := m.subs[id]
	if !ok || s.TenantID != tenantID {
		return domain.Subscription{}, errs.ErrNotFound
	}
	s.CancelAt = nil
	s.CancelAtPeriodEnd = false
	s.UpdatedAt = clock.Now(ctx)
	m.subs[id] = s
	s.Items = m.hydrateItems(id)
	return s, nil
}

func (m *memStore) FireScheduledCancellationTx(_ context.Context, _ *sql.Tx, tenantID, id string, expected PeriodSnapshot, at time.Time) (domain.Subscription, error) {
	s, ok := m.subs[id]
	if !ok || s.TenantID != tenantID {
		return domain.Subscription{}, errs.ErrNotFound
	}
	if s.Status != domain.SubscriptionActive || !snapshotMatches(s, expected) {
		return domain.Subscription{}, ErrWatermarkMoved
	}
	s.Status = domain.SubscriptionCanceled
	s.CanceledAt = &at
	s.CancelAt = nil
	s.CancelAtPeriodEnd = false
	s.UpdatedAt = at
	m.subs[id] = s
	s.Items = m.hydrateItems(id)
	return s, nil
}

func (m *memStore) FireScheduledCancellationWithBill(ctx context.Context, tenantID, id string, at time.Time, billFn func(tx *sql.Tx, canceled domain.Subscription) error) (domain.Subscription, error) {
	s, ok := m.subs[id]
	if !ok || s.TenantID != tenantID {
		return domain.Subscription{}, errs.ErrNotFound
	}
	// Mirror the real CAS: status AND cancel_at equality (ADR-097).
	if s.Status != domain.SubscriptionActive {
		return domain.Subscription{}, errs.InvalidState(fmt.Sprintf("scheduled cancel cannot fire on %s subscription", s.Status))
	}
	if s.CancelAt == nil || !s.CancelAt.Equal(at) {
		return domain.Subscription{}, errs.InvalidState("scheduled cancel no longer current (unscheduled or re-scheduled concurrently)")
	}
	s.Status = domain.SubscriptionCanceled
	s.CanceledAt = &at
	s.CancelAt = nil
	s.CancelAtPeriodEnd = false
	s.UpdatedAt = clock.Now(ctx)
	s.Items = m.hydrateItems(id)
	if billFn != nil {
		if err := billFn(nil, s); err != nil {
			// Real store rolls the flip back on billFn error.
			return domain.Subscription{}, err
		}
	}
	m.subs[id] = s
	return s, nil
}

func (m *memStore) SetPauseCollection(ctx context.Context, tenantID, id string, pc domain.PauseCollection) (domain.Subscription, error) {
	s, ok := m.subs[id]
	if !ok || s.TenantID != tenantID {
		return domain.Subscription{}, errs.ErrNotFound
	}
	if s.Status == domain.SubscriptionCanceled || s.Status == domain.SubscriptionArchived {
		return domain.Subscription{}, fmt.Errorf("cannot pause collection on %s subscription", s.Status)
	}
	pcCopy := pc
	if pc.ResumesAt != nil {
		t := *pc.ResumesAt
		pcCopy.ResumesAt = &t
	}
	s.PauseCollection = &pcCopy
	s.UpdatedAt = clock.Now(ctx)
	m.subs[id] = s
	s.Items = m.hydrateItems(id)
	return s, nil
}

func (m *memStore) ClearPauseCollection(ctx context.Context, tenantID, id string) (domain.Subscription, error) {
	if m.clearPauseOverride != nil {
		return domain.Subscription{}, m.clearPauseOverride
	}
	s, ok := m.subs[id]
	if !ok || s.TenantID != tenantID {
		return domain.Subscription{}, errs.ErrNotFound
	}
	if s.PauseCollection == nil {
		return domain.Subscription{}, ErrNotPaused // mirrors the real store's CAS
	}
	s.PauseCollection = nil
	s.UpdatedAt = clock.Now(ctx)
	m.subs[id] = s
	s.Items = m.hydrateItems(id)
	return s, nil
}

func (m *memStore) ActivateAfterTrial(ctx context.Context, tenantID, id string, at time.Time) (domain.Subscription, error) {
	s, ok := m.subs[id]
	if !ok || s.TenantID != tenantID {
		return domain.Subscription{}, errs.ErrNotFound
	}
	if s.Status != domain.SubscriptionTrialing {
		return domain.Subscription{}, errs.InvalidState(fmt.Sprintf("cannot end trial on %s subscription", s.Status))
	}
	s.Status = domain.SubscriptionActive
	if s.ActivatedAt == nil {
		t := at
		s.ActivatedAt = &t
	}
	s.UpdatedAt = clock.Now(ctx)
	m.subs[id] = s
	s.Items = m.hydrateItems(id)
	return s, nil
}

func (m *memStore) ActivateDraftWithBill(ctx context.Context, tenantID, id string, at, periodStart, periodEnd time.Time, anchorDay int, billFn func(tx *sql.Tx, activated domain.Subscription) error) (domain.Subscription, error) {
	prev, existed := m.subs[id]
	s, ok := m.subs[id]
	if !ok || s.TenantID != tenantID {
		return domain.Subscription{}, errs.ErrNotFound
	}
	if s.Status != domain.SubscriptionDraft {
		return domain.Subscription{}, errs.InvalidState("can only activate draft subscriptions")
	}
	s.Status = domain.SubscriptionActive
	s.ActivatedAt, s.StartedAt = &at, &at
	s.CurrentBillingPeriodStart, s.CurrentBillingPeriodEnd = &periodStart, &periodEnd
	s.NextBillingAt = &periodEnd
	s.BillingAnchorDay = anchorDay
	m.subs[id] = s
	if billFn != nil {
		if err := billFn(nil, s); err != nil {
			if existed {
				m.subs[id] = prev
			}
			return domain.Subscription{}, err
		}
	}
	return s, nil
}

func (m *memStore) ActivateAfterTrialWithBill(ctx context.Context, tenantID, id string, at time.Time, billFn func(tx *sql.Tx, activated domain.Subscription) error) (domain.Subscription, error) {
	prev, existed := m.subs[id]
	activated, err := m.ActivateAfterTrial(ctx, tenantID, id, at)
	if err != nil {
		return domain.Subscription{}, err
	}
	if billFn != nil {
		if err := billFn(nil, activated); err != nil {
			if existed {
				m.subs[id] = prev
			}
			return domain.Subscription{}, err
		}
	}
	return activated, nil
}

func (m *memStore) EndTrialEarly(ctx context.Context, tenantID, id string, at, periodStart, periodEnd, nextBilling time.Time, anchorDay int) (domain.Subscription, error) {
	s, ok := m.subs[id]
	if !ok || s.TenantID != tenantID {
		return domain.Subscription{}, errs.ErrNotFound
	}
	if s.Status != domain.SubscriptionTrialing {
		return domain.Subscription{}, errs.InvalidState(fmt.Sprintf("cannot end trial on %s subscription", s.Status))
	}
	s.Status = domain.SubscriptionActive
	if s.ActivatedAt == nil {
		t := at
		s.ActivatedAt = &t
	}
	te := at
	s.TrialEndAt = &te
	ps := periodStart
	pe := periodEnd
	nb := nextBilling
	s.CurrentBillingPeriodStart = &ps
	s.CurrentBillingPeriodEnd = &pe
	s.NextBillingAt = &nb
	s.BillingAnchorDay = anchorDay
	s.UpdatedAt = clock.Now(ctx)
	m.subs[id] = s
	s.Items = m.hydrateItems(id)
	return s, nil
}

func (m *memStore) EndTrialEarlyWithBill(ctx context.Context, tenantID, id string, at, periodStart, periodEnd, nextBilling time.Time, anchorDay int, billFn func(tx *sql.Tx, activated domain.Subscription) error) (domain.Subscription, error) {
	prev, existed := m.subs[id]
	activated, err := m.EndTrialEarly(ctx, tenantID, id, at, periodStart, periodEnd, nextBilling, anchorDay)
	if err != nil {
		return domain.Subscription{}, err
	}
	if billFn != nil {
		if err := billFn(nil, activated); err != nil {
			if existed {
				m.subs[id] = prev
			}
			return domain.Subscription{}, err
		}
	}
	return activated, nil
}

func (m *memStore) ListExpiredTrials(_ context.Context, before time.Time, _ bool, limit int) ([]domain.Subscription, error) {
	// The mem store has no livemode field on its rows; the livemode
	// partition is enforced on the postgres path via the WHERE
	// livemode = $1 clause. Mem store tests don't typically cross
	// modes, so the filter is a no-op here.
	if limit <= 0 {
		limit = 100
	}
	var out []domain.Subscription
	for _, s := range m.subs {
		if s.TestClockID != "" {
			continue
		}
		if s.Status != domain.SubscriptionTrialing {
			continue
		}
		if s.TrialEndAt == nil || s.TrialEndAt.After(before) {
			continue
		}
		s.Items = m.hydrateItems(s.ID)
		out = append(out, s)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (m *memStore) ListExpiredTrialsForClock(_ context.Context, tenantID, clockID string, frozen time.Time, limit int) ([]domain.Subscription, error) {
	if limit <= 0 {
		limit = 100
	}
	var out []domain.Subscription
	for _, s := range m.subs {
		if s.TenantID != tenantID {
			continue
		}
		if s.TestClockID != clockID {
			continue
		}
		if s.Status != domain.SubscriptionTrialing {
			continue
		}
		if s.TrialEndAt == nil || s.TrialEndAt.After(frozen) {
			continue
		}
		s.Items = m.hydrateItems(s.ID)
		out = append(out, s)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (m *memStore) ListExpiredPauseCollections(_ context.Context, before time.Time, limit int) ([]domain.Subscription, error) {
	if limit <= 0 {
		limit = 100
	}
	var out []domain.Subscription
	for _, s := range m.subs {
		if s.TestClockID != "" {
			continue
		}
		if s.PauseCollection == nil || s.PauseCollection.ResumesAt == nil {
			continue
		}
		if s.PauseCollection.ResumesAt.After(before) {
			continue
		}
		s.Items = m.hydrateItems(s.ID)
		out = append(out, s)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (m *memStore) ListExpiredPauseCollectionsForClock(_ context.Context, tenantID, clockID string, frozen time.Time, limit int) ([]domain.Subscription, error) {
	if limit <= 0 {
		limit = 100
	}
	var out []domain.Subscription
	for _, s := range m.subs {
		if s.TenantID != tenantID || s.TestClockID != clockID {
			continue
		}
		if s.PauseCollection == nil || s.PauseCollection.ResumesAt == nil {
			continue
		}
		if s.PauseCollection.ResumesAt.After(frozen) {
			continue
		}
		s.Items = m.hydrateItems(s.ID)
		out = append(out, s)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (m *memStore) ExtendTrial(ctx context.Context, tenantID, id string, newTrialEnd, periodStart, periodEnd, nextBilling time.Time, anchorDay int) (domain.Subscription, error) {
	s, ok := m.subs[id]
	if !ok || s.TenantID != tenantID {
		return domain.Subscription{}, errs.ErrNotFound
	}
	if s.Status != domain.SubscriptionTrialing {
		return domain.Subscription{}, errs.InvalidState(fmt.Sprintf("cannot extend trial on %s subscription", s.Status))
	}
	t := newTrialEnd
	ps := periodStart
	pe := periodEnd
	nb := nextBilling
	s.TrialEndAt = &t
	s.CurrentBillingPeriodStart = &ps
	s.CurrentBillingPeriodEnd = &pe
	s.NextBillingAt = &nb
	s.BillingAnchorDay = anchorDay
	s.UpdatedAt = clock.Now(ctx)
	m.subs[id] = s
	s.Items = m.hydrateItems(id)
	return s, nil
}

func (m *memStore) ListItems(ctx context.Context, tenantID, subscriptionID string) ([]domain.SubscriptionItem, error) {
	s, ok := m.subs[subscriptionID]
	if !ok || s.TenantID != tenantID {
		return nil, errs.ErrNotFound
	}
	return m.hydrateItems(subscriptionID), nil
}

func (m *memStore) GetItem(ctx context.Context, tenantID, itemID string) (domain.SubscriptionItem, error) {
	it, ok := m.items[itemID]
	if !ok || it.TenantID != tenantID {
		return domain.SubscriptionItem{}, errs.ErrNotFound
	}
	return it, nil
}

func (m *memStore) AddItem(ctx context.Context, tenantID string, item domain.SubscriptionItem) (domain.SubscriptionItem, error) {
	for _, existing := range m.items {
		if existing.SubscriptionID == item.SubscriptionID && existing.PlanID == item.PlanID {
			return domain.SubscriptionItem{}, errs.ErrAlreadyExists
		}
	}
	item.ID = fmt.Sprintf("%s_item_%d", item.SubscriptionID, len(m.items)+1)
	item.TenantID = tenantID
	now := clock.Now(ctx)
	item.CreatedAt = now
	item.UpdatedAt = now
	m.items[item.ID] = item
	m.recordChange(item, "add", "", item.PlanID, 0, item.Quantity, now)
	return item, nil
}

// AddItemTx mirrors AddItem for tx-aware callers. Fake ignores the tx
// since it's in-memory.
func (m *memStore) AddItemTx(ctx context.Context, _ *sql.Tx, tenantID string, item domain.SubscriptionItem) (domain.SubscriptionItem, error) {
	return m.AddItem(ctx, tenantID, item)
}

func (m *memStore) UpdateItemQuantity(ctx context.Context, tenantID, itemID string, quantity int64) (domain.SubscriptionItem, error) {
	it, ok := m.items[itemID]
	if !ok || it.TenantID != tenantID {
		return domain.SubscriptionItem{}, errs.ErrNotFound
	}
	prevQty := it.Quantity
	it.Quantity = quantity
	it.UpdatedAt = clock.Now(ctx)
	m.items[itemID] = it
	m.recordChange(it, "quantity", it.PlanID, it.PlanID, prevQty, quantity, it.UpdatedAt)
	return it, nil
}

// Tx-variant stubs — forward to the non-Tx implementations. The fake
// is in-memory; tx parameter is ignored. Atomic behavior is tested via
// integration tests against a real DB.
func (m *memStore) UpdateItemQuantityTx(ctx context.Context, _ *sql.Tx, tenantID, itemID string, quantity int64) (domain.SubscriptionItem, error) {
	return m.UpdateItemQuantity(ctx, tenantID, itemID, quantity)
}

func (m *memStore) ApplyItemPlanImmediatelyTx(ctx context.Context, _ *sql.Tx, tenantID, itemID, newPlanID string, changedAt time.Time) (domain.SubscriptionItem, error) {
	return m.ApplyItemPlanImmediately(ctx, tenantID, itemID, newPlanID, changedAt)
}

func (m *memStore) RemoveItemTx(ctx context.Context, _ *sql.Tx, tenantID, itemID string) error {
	return m.RemoveItem(ctx, tenantID, itemID)
}

// FindMeterConflicts: the in-memory fake reports no conflicts — the
// double-billing guard's SQL (live-sub join + meter_ids unnest) is proven
// against real Postgres in meter_overlap_guard_integration_test.go.
func (m *memStore) FindMeterConflicts(context.Context, string, string, string, []string) ([]MeterConflict, error) {
	return nil, nil
}

func (m *memStore) ApplyItemPlanImmediately(ctx context.Context, tenantID, itemID, newPlanID string, changedAt time.Time) (domain.SubscriptionItem, error) {
	it, ok := m.items[itemID]
	if !ok || it.TenantID != tenantID {
		return domain.SubscriptionItem{}, errs.ErrNotFound
	}
	oldPlan := it.PlanID
	it.PlanID = newPlanID
	it.PlanChangedAt = &changedAt
	it.PendingPlanID = ""
	it.PendingPlanEffectiveAt = nil
	it.UpdatedAt = changedAt
	m.items[itemID] = it
	m.recordChange(it, "plan", oldPlan, newPlanID, it.Quantity, it.Quantity, changedAt)
	return it, nil
}

func (m *memStore) SetItemPendingPlan(ctx context.Context, tenantID, itemID, pendingPlanID string, effectiveAt time.Time) (domain.SubscriptionItem, error) {
	it, ok := m.items[itemID]
	if !ok || it.TenantID != tenantID {
		return domain.SubscriptionItem{}, errs.ErrNotFound
	}
	it.PendingPlanID = pendingPlanID
	it.PendingPlanEffectiveAt = &effectiveAt
	it.UpdatedAt = clock.Now(ctx)
	m.items[itemID] = it
	return it, nil
}

func (m *memStore) ClearItemPendingPlan(ctx context.Context, tenantID, itemID string) (domain.SubscriptionItem, error) {
	it, ok := m.items[itemID]
	if !ok || it.TenantID != tenantID {
		return domain.SubscriptionItem{}, errs.ErrNotFound
	}
	it.PendingPlanID = ""
	it.PendingPlanEffectiveAt = nil
	it.UpdatedAt = clock.Now(ctx)
	m.items[itemID] = it
	return it, nil
}

func (m *memStore) ApplyDuePendingItemPlansAtomic(ctx context.Context, tenantID, subscriptionID string, now time.Time) ([]domain.SubscriptionItem, error) {
	var applied []domain.SubscriptionItem
	for id, it := range m.items {
		if it.TenantID != tenantID || it.SubscriptionID != subscriptionID {
			continue
		}
		if it.PendingPlanID == "" || it.PendingPlanEffectiveAt == nil || it.PendingPlanEffectiveAt.After(now) {
			continue
		}
		oldPlan := it.PlanID
		it.PlanID = it.PendingPlanID
		it.PlanChangedAt = &now
		it.PendingPlanID = ""
		it.PendingPlanEffectiveAt = nil
		it.UpdatedAt = now
		m.items[id] = it
		applied = append(applied, it)
		m.recordChange(it, "plan", oldPlan, it.PlanID, it.Quantity, it.Quantity, now)
	}
	return applied, nil
}

func (m *memStore) RemoveItem(ctx context.Context, tenantID, itemID string) error {
	it, ok := m.items[itemID]
	if !ok || it.TenantID != tenantID {
		return errs.ErrNotFound
	}
	delete(m.items, itemID)
	m.recordChange(it, "remove", it.PlanID, "", it.Quantity, 0, clock.Now(ctx))
	return nil
}

// SetBillingThresholds is a minimal in-memory implementation matching the
// store contract: stores the BillingThresholds struct on the row and rejects
// terminal subs. The handler tests don't exercise this path; integration
// tests against real Postgres cover the full behaviour.
func (m *memStore) SetBillingThresholds(ctx context.Context, tenantID, id string, t domain.BillingThresholds) (domain.Subscription, error) {
	s, ok := m.subs[id]
	if !ok || s.TenantID != tenantID {
		return domain.Subscription{}, errs.ErrNotFound
	}
	if s.Status == domain.SubscriptionCanceled || s.Status == domain.SubscriptionArchived {
		return domain.Subscription{}, errs.InvalidState("cannot configure billing thresholds on terminated subscription")
	}
	bt := t
	s.BillingThresholds = &bt
	m.subs[id] = s
	s.Items = m.hydrateItems(id)
	return s, nil
}

func (m *memStore) ClearBillingThresholds(ctx context.Context, tenantID, id string) (domain.Subscription, error) {
	s, ok := m.subs[id]
	if !ok || s.TenantID != tenantID {
		return domain.Subscription{}, errs.ErrNotFound
	}
	s.BillingThresholds = nil
	m.subs[id] = s
	s.Items = m.hydrateItems(id)
	return s, nil
}

// ListWithThresholdsForClock — ADR-029 Phase 3 stub. Narrow service
// tests don't exercise per-clock threshold scans; the per-clock SQL
// is verified in postgres integration tests. No-op satisfies the
// interface contract.
func (m *memStore) ListWithThresholdsForClock(ctx context.Context, _, _, _ string, _ int) ([]domain.Subscription, error) {
	return nil, nil
}

func (m *memStore) ListItemChangesInPeriod(_ context.Context, tenantID, subscriptionID string, periodStart, periodEnd time.Time) ([]domain.SubscriptionItemChange, error) {
	var out []domain.SubscriptionItemChange
	for _, c := range m.itemChanges {
		if c.TenantID != tenantID || c.SubscriptionID != subscriptionID {
			continue
		}
		if !c.ChangedAt.After(periodStart) || c.ChangedAt.After(periodEnd) {
			continue
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].ChangedAt.Equal(out[j].ChangedAt) {
			return out[i].ChangedAt.Before(out[j].ChangedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (m *memStore) ListWithThresholds(ctx context.Context, _ bool, _ string, _ int) ([]domain.Subscription, error) {
	var out []domain.Subscription
	for _, s := range m.subs {
		if s.BillingThresholds == nil {
			continue
		}
		if s.Status != domain.SubscriptionActive && s.Status != domain.SubscriptionTrialing {
			continue
		}
		s.Items = m.hydrateItems(s.ID)
		out = append(out, s)
	}
	return out, nil
}

func (m *memStore) hydrateItems(subID string) []domain.SubscriptionItem {
	var out []domain.SubscriptionItem
	for _, it := range m.items {
		if it.SubscriptionID == subID {
			out = append(out, it)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestCreate(t *testing.T) {
	svc := NewService(newMemStore(), nil)
	ctx := context.Background()

	t.Run("draft by default", func(t *testing.T) {
		sub, err := svc.Create(ctx, "t1", CreateInput{
			Code: "sub-001", DisplayName: "Pro Monthly",
			CustomerID: "cus_1",
			Items:      []CreateItemInput{{PlanID: "pln_1"}},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if sub.Status != domain.SubscriptionDraft {
			t.Errorf("got status %q, want draft", sub.Status)
		}
		if sub.BillingTime != domain.BillingTimeCalendar {
			t.Errorf("got billing_time %q, want calendar", sub.BillingTime)
		}
		if len(sub.Items) != 1 || sub.Items[0].PlanID != "pln_1" || sub.Items[0].Quantity != 1 {
			t.Errorf("items: got %+v, want single pln_1 qty=1", sub.Items)
		}
	})

	t.Run("multiple items", func(t *testing.T) {
		sub, err := svc.Create(ctx, "t1", CreateInput{
			Code: "sub-multi", DisplayName: "Bundle",
			CustomerID: "cus_1",
			Items: []CreateItemInput{
				{PlanID: "pln_base", Quantity: 2},
				{PlanID: "pln_addon", Quantity: 10},
			},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(sub.Items) != 2 {
			t.Fatalf("items count: got %d, want 2", len(sub.Items))
		}
	})

	t.Run("start_now activates immediately", func(t *testing.T) {
		sub, err := svc.Create(ctx, "t1", CreateInput{
			Code: "sub-002", DisplayName: "Active",
			CustomerID: "cus_1",
			Items:      []CreateItemInput{{PlanID: "pln_1"}},
			StartNow:   true,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if sub.Status != domain.SubscriptionActive {
			t.Errorf("got status %q, want active", sub.Status)
		}
		if sub.StartedAt == nil {
			t.Error("started_at should be set")
		}
		// Immediate activation must stamp activated_at — MRR movement /
		// point-in-time / churn key on it as the MRR-start event. A NULL
		// here (the old bug) makes the sub count in headline MRR but
		// vanish from New/Net movement, so the dashboard never reconciles.
		if sub.ActivatedAt == nil {
			t.Error("start_now must stamp activated_at (else invisible to MRR movement)")
		} else if sub.StartedAt != nil && !sub.ActivatedAt.Equal(*sub.StartedAt) {
			t.Errorf("activated_at %v should equal started_at %v for start_now", *sub.ActivatedAt, *sub.StartedAt)
		}
	})

	t.Run("trial_days sets trial window and status=trialing", func(t *testing.T) {
		sub, err := svc.Create(ctx, "t1", CreateInput{
			Code: "sub-003", DisplayName: "Trial",
			CustomerID: "cus_1",
			Items:      []CreateItemInput{{PlanID: "pln_1"}},
			TrialDays:  14,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if sub.Status != domain.SubscriptionTrialing {
			t.Errorf("got status %q, want trialing", sub.Status)
		}
		if sub.TrialStartAt == nil || sub.TrialEndAt == nil {
			t.Fatal("trial dates should be set")
		}
		diff := sub.TrialEndAt.Sub(*sub.TrialStartAt)
		if diff.Hours() < 13*24 || diff.Hours() > 15*24 {
			t.Errorf("trial duration %v, expected ~14 days", diff)
		}
		if sub.StartedAt == nil {
			t.Error("started_at should be set on trialing sub")
		}
	})

	t.Run("validation errors", func(t *testing.T) {
		cases := []CreateInput{
			{DisplayName: "x", CustomerID: "c", Items: []CreateItemInput{{PlanID: "p"}}},                                   // missing code
			{Code: "x", CustomerID: "c", Items: []CreateItemInput{{PlanID: "p"}}},                                          // missing display_name
			{Code: "x", DisplayName: "x", Items: []CreateItemInput{{PlanID: "p"}}},                                         // missing customer_id
			{Code: "x", DisplayName: "x", CustomerID: "c"},                                                                 // missing items
			{Code: "x", DisplayName: "x", CustomerID: "c", Items: []CreateItemInput{{}}},                                   // item missing plan_id
			{Code: "x", DisplayName: "x", CustomerID: "c", Items: []CreateItemInput{{PlanID: "p"}, {PlanID: "p"}}},         // duplicate plan
			{Code: "x", DisplayName: "x", CustomerID: "c", Items: []CreateItemInput{{PlanID: "p"}}, BillingTime: "weekly"}, // invalid billing_time
		}
		for _, input := range cases {
			_, err := svc.Create(ctx, "t1", input)
			if err == nil {
				t.Errorf("expected error for input %+v", input)
			}
		}
	})

	t.Run("ADR-031 biller called for active sub", func(t *testing.T) {
		svcWithBiller := NewService(newMemStore(), nil)
		fb := &fakeBiller{}
		svcWithBiller.SetBiller(fb)
		_, err := svcWithBiller.Create(ctx, "t1", CreateInput{
			Code: "sub-bill-active", DisplayName: "Active",
			CustomerID: "cus_1",
			Items:      []CreateItemInput{{PlanID: "pln_1"}},
			StartNow:   true,
		})
		if err != nil {
			t.Fatalf("unexpected: %v", err)
		}
		// ADR-056: the day-1 invoice is now built in-tx via BillOnCreateTx.
		if fb.createTxCalls != 1 {
			t.Errorf("BillOnCreateTx called %d times, want 1", fb.createTxCalls)
		}
	})

	t.Run("ADR-031 biller NOT called for trialing sub", func(t *testing.T) {
		svcWithBiller := NewService(newMemStore(), nil)
		fb := &fakeBiller{}
		svcWithBiller.SetBiller(fb)
		_, err := svcWithBiller.Create(ctx, "t1", CreateInput{
			Code: "sub-bill-trial", DisplayName: "Trial",
			CustomerID: "cus_1",
			Items:      []CreateItemInput{{PlanID: "pln_1"}},
			TrialDays:  14,
		})
		if err != nil {
			t.Fatalf("unexpected: %v", err)
		}
		if fb.calls != 0 {
			t.Errorf("biller called %d times for trialing sub, want 0", fb.calls)
		}
	})

	t.Run("ADR-031 biller called on EndTrial (covers in_advance first paid period)", func(t *testing.T) {
		// Pre-fix: operator EndTrial flipped status to active but
		// never triggered BillOnCreate — in_advance items missed their
		// first paid period entirely (revenue leak specific to in_advance
		// + trial). Fix fires BillOnCreate from Service.EndTrial after
		// EndTrialEarly returns the activated sub.
		svcWithBiller := NewService(newMemStore(), nil)
		fb := &fakeBiller{}
		svcWithBiller.SetBiller(fb)
		sub, err := svcWithBiller.Create(ctx, "t1", CreateInput{
			Code: "sub-end-trial", DisplayName: "Ends trial early",
			CustomerID: "cus_1",
			Items:      []CreateItemInput{{PlanID: "pln_1"}},
			TrialDays:  14,
		})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if fb.calls != 0 {
			t.Fatalf("biller calls after create-trialing: got %d, want 0", fb.calls)
		}
		if _, err := svcWithBiller.EndTrial(ctx, "t1", sub.ID); err != nil {
			t.Fatalf("EndTrial: %v", err)
		}
		// ADR-056: the day-1 invoice is now built in-tx via BillOnCreateTx.
		if fb.createTxCalls != 1 {
			t.Errorf("BillOnCreateTx after EndTrial: got %d, want 1 (covers in_advance first paid period)", fb.createTxCalls)
		}
	})

	t.Run("ADR-056 EndTrial day-1 billing failure rolls the early-end back (atomic)", func(t *testing.T) {
		// Pre-ADR-056 a BillOnCreate failure was swallowed: the trial ended,
		// the sub went active with NO first-period invoice — a revenue leak (the
		// cycle scheduler skips the just-elapsed in_advance segment). Now the
		// day-1 invoice is built IN the early-end tx, so a hard failure rolls the
		// activation back and the sub stays trialing.
		store := newMemStore()
		svcWithBiller := NewService(store, nil)
		fb := &fakeBiller{createTxErr: fmt.Errorf("invoice insert failed")}
		svcWithBiller.SetBiller(fb)
		sub, err := svcWithBiller.Create(ctx, "t1", CreateInput{
			Code: "sub-end-trial-billfail", DisplayName: "Bill-fail path",
			CustomerID: "cus_1",
			Items:      []CreateItemInput{{PlanID: "pln_1"}},
			TrialDays:  14,
		})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if _, err := svcWithBiller.EndTrial(ctx, "t1", sub.ID); err == nil {
			t.Fatal("a day-1 billing failure must fail EndTrial (atomic), not leave an active sub with no first-period invoice")
		}
		after, gerr := store.Get(ctx, "t1", sub.ID)
		if gerr != nil {
			t.Fatalf("get after rollback: %v", gerr)
		}
		if after.Status != domain.SubscriptionTrialing {
			t.Errorf("early-end must roll back on a billing failure; status=%q, want trialing", after.Status)
		}
	})

	t.Run("PR-10: BillFinalOnImmediateCancel called on Cancel (covers partial-period usage gap)", func(t *testing.T) {
		// Pre-fix: mid-period immediate cancel generated NO final
		// invoice. Customer's partial-period usage went uncaptured
		// (revenue leak). Fix: Service.Cancel now also calls
		// BillFinalOnImmediateCancel, which emits a usage + prorated
		// in_arrears base invoice for [period_start, canceled_at].
		svcWithBiller := NewService(newMemStore(), nil)
		fb := &fakeBiller{}
		svcWithBiller.SetBiller(fb)
		sub, err := svcWithBiller.Create(ctx, "t1", CreateInput{
			Code: "sub-cancel-mid", DisplayName: "Mid-period cancel",
			CustomerID: "cus_1",
			Items:      []CreateItemInput{{PlanID: "pln_1"}},
			StartNow:   true,
		})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if _, _, err := svcWithBiller.Cancel(ctx, "t1", sub.ID); err != nil {
			t.Fatalf("cancel: %v", err)
		}
		if fb.finalCalls != 1 {
			t.Errorf("BillFinalOnImmediateCancel called %d times, want 1", fb.finalCalls)
		}
		// BillOnCancel (credit grant for in_advance) also still fires
		// — the two are independent operations (invoice vs credit).
		if fb.cancelCalls != 1 {
			t.Errorf("BillOnCancel called %d times, want 1 (independent of BillFinalOnImmediateCancel)", fb.cancelCalls)
		}
	})

	t.Run("ADR-056 a final-on-cancel billing failure rolls the cancel back (atomic)", func(t *testing.T) {
		// Pre-ADR-056 a final-on-cancel invoice failure was swallowed: the sub
		// was canceled with the partial period UNINVOICED — a revenue leak (no
		// final-on-cancel reconciler exists; the "G3 deferred reconciler" the old
		// comment referenced never did). Now the final invoice is built IN the
		// cancel tx, so a failure rolls the cancel back rather than orphaning an
		// uninvoiced canceled sub.
		store := newMemStore()
		svcWithBiller := NewService(store, nil)
		fb := &fakeBiller{finalCancelErr: fmt.Errorf("invoice insert failed")}
		svcWithBiller.SetBiller(fb)
		sub, err := svcWithBiller.Create(ctx, "t1", CreateInput{
			Code: "sub-cancel-final-err", DisplayName: "Final errpath",
			CustomerID: "cus_1",
			Items:      []CreateItemInput{{PlanID: "pln_1"}},
			StartNow:   true,
		})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if _, _, err := svcWithBiller.Cancel(ctx, "t1", sub.ID); err == nil {
			t.Fatal("a final-on-cancel billing failure must fail Cancel (atomic), not leave an uninvoiced canceled sub")
		}
		// The cancel must have rolled back — the sub is still active, not canceled.
		after, gerr := store.Get(ctx, "t1", sub.ID)
		if gerr != nil {
			t.Fatalf("get after rollback: %v", gerr)
		}
		if after.Status != domain.SubscriptionActive {
			t.Errorf("cancel must roll back on a final-invoice failure; status=%q, want active", after.Status)
		}
		// BillOnCancel (the proration credit) must NOT run when the cancel didn't commit.
		if fb.cancelCalls != 0 {
			t.Errorf("BillOnCancel must not run when the cancel rolled back; got %d", fb.cancelCalls)
		}
	})

	t.Run("ADR-031 BillOnCancel called on Cancel", func(t *testing.T) {
		svcWithBiller := NewService(newMemStore(), nil)
		fb := &fakeBiller{}
		svcWithBiller.SetBiller(fb)
		sub, err := svcWithBiller.Create(ctx, "t1", CreateInput{
			Code: "sub-cancel-active", DisplayName: "Active to cancel",
			CustomerID: "cus_1",
			Items:      []CreateItemInput{{PlanID: "pln_1"}},
			StartNow:   true,
		})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if _, _, err := svcWithBiller.Cancel(ctx, "t1", sub.ID); err != nil {
			t.Fatalf("cancel: %v", err)
		}
		if fb.cancelCalls != 1 {
			t.Errorf("BillOnCancel called %d times, want 1", fb.cancelCalls)
		}
	})

	t.Run("ADR-031 BillOnCancel error does NOT fail cancel", func(t *testing.T) {
		svcWithBiller := NewService(newMemStore(), nil)
		fb := &fakeBiller{cancelErr: fmt.Errorf("credit grant failed")}
		svcWithBiller.SetBiller(fb)
		sub, err := svcWithBiller.Create(ctx, "t1", CreateInput{
			Code: "sub-cancel-errpath", DisplayName: "Error on cancel",
			CustomerID: "cus_1",
			Items:      []CreateItemInput{{PlanID: "pln_1"}},
			StartNow:   true,
		})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		canceled, _, err := svcWithBiller.Cancel(ctx, "t1", sub.ID)
		if err != nil {
			t.Fatalf("biller error should not fail cancel: %v", err)
		}
		if canceled.Status != domain.SubscriptionCanceled {
			t.Errorf("status %q, want canceled", canceled.Status)
		}
		if fb.cancelCalls != 1 {
			t.Errorf("BillOnCancel called %d times, want 1", fb.cancelCalls)
		}
	})

	t.Run("ADR-056 a hard day-1 billing failure rolls back the create (atomic)", func(t *testing.T) {
		// Pre-ADR-056 a BillOnCreate failure was swallowed: the sub stayed
		// active with NO first-period invoice — a permanent revenue leak (the
		// cycle scheduler bills the UPCOMING period and skips the just-elapsed
		// in_advance segment, so it is NOT "deferred to next cycle close"). Now
		// the day-1 invoice is built IN the create tx, so a hard failure rolls
		// the whole create back rather than orphaning a sub.
		store := newMemStore()
		svcWithBiller := NewService(store, nil)
		fb := &fakeBiller{createTxErr: fmt.Errorf("invoice insert failed")}
		svcWithBiller.SetBiller(fb)
		_, err := svcWithBiller.Create(ctx, "t1", CreateInput{
			Code: "sub-bill-err", DisplayName: "Error path",
			CustomerID: "cus_1",
			Items:      []CreateItemInput{{PlanID: "pln_1"}},
			StartNow:   true,
		})
		if err == nil {
			t.Fatal("a day-1 billing failure must fail Create (atomic), not orphan a sub with no first-period invoice")
		}
		if len(store.subs) != 0 {
			t.Fatalf("subscription must be rolled back on a billing failure; got %d persisted", len(store.subs))
		}
		if fb.createTxCalls != 1 {
			t.Errorf("BillOnCreateTx called %d times, want 1", fb.createTxCalls)
		}
		if fb.finalizeCalls != 0 {
			t.Errorf("FinalizeOnCreateInvoice must NOT run when the create rolled back; got %d", fb.finalizeCalls)
		}
	})
}

// fakeBiller captures BillOnCreate / BillOnCancel invocations for
// ADR-031 tests.
type fakeBiller struct {
	// thresholdBilledThrough feeds ThresholdBilledThroughTx — the swap's
	// fire-vs-swap guard (ADR-115). nil = no threshold fire this period.
	thresholdBilledThrough    *time.Time
	thresholdBilledThroughErr error
	calls                     int
	finalCalls                int
	cancelCalls               int
	planSwapCalls             int
	planSwapAt                time.Time
	err                       error
	finalCancelErr            error
	finalCancelInv            domain.Invoice
	cancelErr                 error
	planSwapErr               error
	cancelCreditCents         int64
	planSwapCreditCents       int64
	createTxCalls             int
	createTxOK                bool
	createTxInv               domain.Invoice
	createTxErr               error
	finalizeCalls             int
	// cancel-credit draft path (ADR-057 ext). draftHandled defaults FALSE, so
	// existing cancel tests fall through to BillOnCancel (today's path) unchanged.
	draftTxCalls    int
	draftIDs        []string
	draftCredit     int64
	draftHandled    bool
	draftTxErr      error
	issueDraftCalls int
	issuedDraftIDs  []string
	// plan-swap draft path (Bug B transplant). swapDraftHandled defaults
	// FALSE, so existing swap tests fall through to BillOnPlanSwapImmediate
	// (the pre-fix path) unchanged.
	swapDraftTxCalls    int
	swapDraftIDs        []string
	swapDraftHandled    bool
	swapDraftTxErr      error
	issueSwapDraftCalls int
	issuedSwapDraftIDs  []string
}

func (f *fakeBiller) BillOnCreate(_ context.Context, _ domain.Subscription) (domain.Invoice, error) {
	f.calls++
	return domain.Invoice{}, f.err
}

func (f *fakeBiller) BillFinalOnImmediateCancel(_ context.Context, _ domain.Subscription) (domain.Invoice, error) {
	f.finalCalls++
	return domain.Invoice{}, f.finalCancelErr
}

func (f *fakeBiller) BillFinalOnImmediateCancelTx(_ context.Context, _ *sql.Tx, _ domain.Subscription) (domain.Invoice, error) {
	f.finalCalls++
	return f.finalCancelInv, f.finalCancelErr
}

func (f *fakeBiller) BillOnCancel(_ context.Context, _ domain.Subscription) (int64, error) {
	f.cancelCalls++
	return f.cancelCreditCents, f.cancelErr
}

func (f *fakeBiller) BillOnCancelDraftsTx(_ context.Context, _ *sql.Tx, _ domain.Subscription) ([]string, int64, bool, error) {
	f.draftTxCalls++
	return f.draftIDs, f.draftCredit, f.draftHandled, f.draftTxErr
}

func (f *fakeBiller) IssueCancelDrafts(_ context.Context, _ domain.Subscription, ids []string) {
	f.issueDraftCalls++
	f.issuedDraftIDs = append(f.issuedDraftIDs, ids...)
}

func (f *fakeBiller) BillOnPlanSwapImmediate(_ context.Context, _ domain.Subscription, at time.Time) (int64, error) {
	f.planSwapCalls++
	f.planSwapAt = at
	return f.planSwapCreditCents, f.planSwapErr
}

func (f *fakeBiller) BillOnPlanSwapDraftsTx(_ context.Context, _ *sql.Tx, _ domain.Subscription, _ time.Time) ([]string, int64, bool, error) {
	f.swapDraftTxCalls++
	return f.swapDraftIDs, 0, f.swapDraftHandled, f.swapDraftTxErr
}

func (f *fakeBiller) IssueSwapDrafts(_ context.Context, _ domain.Subscription, ids []string) {
	f.issueSwapDraftCalls++
	f.issuedSwapDraftIDs = append(f.issuedSwapDraftIDs, ids...)
}

func (f *fakeBiller) BillOnCreateTx(_ context.Context, _ *sql.Tx, _ domain.Subscription) (domain.Invoice, bool, error) {
	f.createTxCalls++
	return f.createTxInv, f.createTxOK, f.createTxErr
}

func (f *fakeBiller) ThresholdBilledThroughTx(_ context.Context, _ *sql.Tx, _ domain.Subscription) (*time.Time, error) {
	return f.thresholdBilledThrough, f.thresholdBilledThroughErr
}

func (f *fakeBiller) FinalizeOnCreateInvoice(_ context.Context, _ domain.Subscription, _ domain.Invoice) {
	f.finalizeCalls++
}

// fakeDispatcher captures outbound-webhook Dispatch calls so tests
// can assert on the event type + payload without standing up the
// webhook outbox.
type fakeDispatcher struct {
	events []struct {
		eventType string
		payload   map[string]any
	}
}

func (f *fakeDispatcher) Dispatch(_ context.Context, _ string, eventType string, payload map[string]any) error {
	f.events = append(f.events, struct {
		eventType string
		payload   map[string]any
	}{eventType, payload})
	return nil
}

// fakePlanReader returns plans by id — lets tests stub plan intervals
// for the yearly-vs-monthly period-anchoring branches without standing
// up the full pricing package.
type fakePlanReader struct {
	plans map[string]domain.Plan
}

func (f *fakePlanReader) GetPlan(_ context.Context, _, planID string) (domain.Plan, error) {
	if p, ok := f.plans[planID]; ok {
		return p, nil
	}
	return domain.Plan{}, fmt.Errorf("no stub for plan %s", planID)
}

// fakeSettings is a SettingsReader that returns a fixed timezone — used
// to assert the day-grade snap honours the tenant's configured TZ.
type fakeSettings struct{ tz string }

func (f fakeSettings) Get(_ context.Context, _ string) (domain.TenantSettings, error) {
	return domain.TenantSettings{Timezone: f.tz}, nil
}

// TestPeriod_DayGradeSnap pins the day-grade calendar billing
// behaviour: a subscription created at any wall-clock time on a given
// day produces a period whose start is 00:00 in tenant TZ, NOT the
// raw timestamp. Without this, a 14:00 signup gets billed for 30/31
// days (truncating int division) even though the UI shows the period
// as a whole month — Chargebee / Lago default. ADR follow-up.
func TestPeriod_DayGradeSnap(t *testing.T) {
	ctx := context.Background()

	// Asia/Kolkata: UTC+05:30, no DST. Picked because the screenshot
	// that surfaced this bug used IST and because no-DST keeps the
	// expected values trivially predictable.
	ist, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatalf("load Asia/Kolkata: %v", err)
	}
	// Fix the clock at May 1, 2026 14:00 IST. Pre-snap, periodStart
	// would be this exact instant; post-snap it should be May 1 00:00
	// IST.
	creation := time.Date(2026, 5, 1, 14, 0, 0, 0, ist)
	expectedStart := time.Date(2026, 5, 1, 0, 0, 0, 0, ist).UTC()
	expectedEnd := time.Date(2026, 6, 1, 0, 0, 0, 0, ist).UTC()

	svc := NewService(newMemStore(), clock.NewFake(creation))
	svc.SetSettingsReader(fakeSettings{tz: "Asia/Kolkata"})

	sub, err := svc.Create(ctx, "t1", CreateInput{
		Code: "sub-snap-calendar", DisplayName: "Snap Calendar",
		CustomerID: "cus_1",
		Items:      []CreateItemInput{{PlanID: "pln_1"}},
		StartNow:   true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sub.CurrentBillingPeriodStart == nil || !sub.CurrentBillingPeriodStart.Equal(expectedStart) {
		t.Errorf("calendar periodStart: got %v, want %v", sub.CurrentBillingPeriodStart, expectedStart)
	}
	if sub.CurrentBillingPeriodEnd == nil || !sub.CurrentBillingPeriodEnd.Equal(expectedEnd) {
		t.Errorf("calendar periodEnd: got %v, want %v", sub.CurrentBillingPeriodEnd, expectedEnd)
	}

	// Anniversary billing: same snap rule — 14:00 signup → 00:00
	// start in tenant TZ. For a May 1 IST start the next anniversary is
	// June 1 IST (May has 31 days), identical to the calendar boundary —
	// computed in the TENANT zone (ADR-058), NOT a UTC AddDate on the
	// UTC-located start (which lands a day early on May 31 IST).
	sub, err = svc.Create(ctx, "t1", CreateInput{
		Code: "sub-snap-anniversary", DisplayName: "Snap Anniversary",
		CustomerID:  "cus_1",
		Items:       []CreateItemInput{{PlanID: "pln_1"}},
		StartNow:    true,
		BillingTime: domain.BillingTimeAnniversary,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sub.CurrentBillingPeriodStart == nil || !sub.CurrentBillingPeriodStart.Equal(expectedStart) {
		t.Errorf("anniversary periodStart: got %v, want %v", sub.CurrentBillingPeriodStart, expectedStart)
	}
	annivExpectedEnd := expectedEnd // June 1 IST — same as the calendar boundary for a 1st-of-month start
	if sub.CurrentBillingPeriodEnd == nil || !sub.CurrentBillingPeriodEnd.Equal(annivExpectedEnd) {
		t.Errorf("anniversary periodEnd: got %v, want %v", sub.CurrentBillingPeriodEnd, annivExpectedEnd)
	}

	// SettingsReader nil → UTC fallback, behaviour stays correct.
	// Operators who haven't configured a TZ get the same shape, just
	// against the UTC clock instead.
	svcUTC := NewService(newMemStore(), clock.NewFake(creation))
	subUTC, err := svcUTC.Create(ctx, "t1", CreateInput{
		Code: "sub-snap-utc", DisplayName: "Snap UTC",
		CustomerID: "cus_1",
		Items:      []CreateItemInput{{PlanID: "pln_1"}},
		StartNow:   true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 14:00 IST = 08:30 UTC, so the UTC start-of-day is May 1 00:00
	// UTC = April 30 18:30 IST. The snap targets UTC midnight here.
	expectedUTCStart := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	if !subUTC.CurrentBillingPeriodStart.Equal(expectedUTCStart) {
		t.Errorf("UTC fallback periodStart: got %v, want %v", subUTC.CurrentBillingPeriodStart, expectedUTCStart)
	}
}

var errWiring = errors.New("injected bill failure")

func TestActivateAndCancel(t *testing.T) {
	svc := NewService(newMemStore(), nil)
	ctx := context.Background()

	sub, _ := svc.Create(ctx, "t1", CreateInput{
		Code: "sub-act", DisplayName: "Test", CustomerID: "c",
		Items: []CreateItemInput{{PlanID: "p"}},
	})

	t.Run("activate draft", func(t *testing.T) {
		activated, err := svc.Activate(ctx, "t1", sub.ID)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if activated.Status != domain.SubscriptionActive {
			t.Errorf("got status %q, want active", activated.Status)
		}
	})

	t.Run("cannot activate again", func(t *testing.T) {
		_, err := svc.Activate(ctx, "t1", sub.ID)
		if err == nil {
			t.Fatal("expected error activating non-draft")
		}
	})

	t.Run("activating a draft BILLS day-1 in the flip tx (ADR-056 sibling)", func(t *testing.T) {
		// Pre-fix: Activate flipped status without ever calling the biller —
		// the first in_advance base fee of a draft-then-activated sub was
		// never invoiced by any path (revenue leak).
		svcB := NewService(newMemStore(), nil)
		fb := &fakeBiller{createTxOK: true, createTxInv: domain.Invoice{ID: "inv_day1"}}
		svcB.SetBiller(fb)
		draft, err := svcB.Create(ctx, "t1", CreateInput{
			Code: "sub-act-bill", DisplayName: "ActBill", CustomerID: "c",
			Items: []CreateItemInput{{PlanID: "p"}},
		})
		if err != nil {
			t.Fatalf("create draft: %v", err)
		}
		if fb.createTxCalls != 0 {
			t.Fatalf("draft create must not bill, got %d", fb.createTxCalls)
		}
		activated, err := svcB.Activate(ctx, "t1", draft.ID)
		if err != nil {
			t.Fatalf("activate: %v", err)
		}
		if fb.createTxCalls != 1 {
			t.Errorf("BillOnCreateTx calls = %d, want 1 (day-1 rides the flip tx)", fb.createTxCalls)
		}
		if fb.finalizeCalls != 1 {
			t.Errorf("FinalizeOnCreateInvoice calls = %d, want 1 (post-commit external leg)", fb.finalizeCalls)
		}
		if activated.CurrentBillingPeriodStart == nil || activated.NextBillingAt == nil {
			t.Error("activation must stamp the first billing period before billing")
		}
	})

	t.Run("bill failure rolls the activation back — never an active sub with a lost invoice", func(t *testing.T) {
		svcB := NewService(newMemStore(), nil)
		fb := &fakeBiller{createTxErr: errWiring}
		svcB.SetBiller(fb)
		draft, err := svcB.Create(ctx, "t1", CreateInput{
			Code: "sub-act-fail", DisplayName: "ActFail", CustomerID: "c",
			Items: []CreateItemInput{{PlanID: "p"}},
		})
		if err != nil {
			t.Fatalf("create draft: %v", err)
		}
		if _, err := svcB.Activate(ctx, "t1", draft.ID); err == nil {
			t.Fatal("expected activation to fail when the day-1 bill fails")
		}
		got, err := svcB.Get(ctx, "t1", draft.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.Status != domain.SubscriptionDraft {
			t.Errorf("status = %s, want draft (atomic rollback)", got.Status)
		}
	})

	t.Run("cancel active", func(t *testing.T) {
		canceled, _, err := svc.Cancel(ctx, "t1", sub.ID)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if canceled.Status != domain.SubscriptionCanceled {
			t.Errorf("got status %q, want canceled", canceled.Status)
		}
	})

	t.Run("cannot cancel canceled", func(t *testing.T) {
		_, _, err := svc.Cancel(ctx, "t1", sub.ID)
		if err == nil {
			t.Fatal("expected error canceling already canceled")
		}
	})
}

// TestCancel_NonTerminalStatuses locks in the industry-aligned cancel
// state machine (Stripe / Lago / Recurly / Chargebee parity): cancel
// works from every non-terminal status. Pre-fix, Velox rejected cancel
// from draft and trialing — the trialing case is the dominant cancel
// path (customer abandons during trial), so the rejection silently
// trapped operators in the dashboard.
func TestCancel_NonTerminalStatuses(t *testing.T) {
	ctx := context.Background()

	t.Run("cancel from draft", func(t *testing.T) {
		svc := NewService(newMemStore(), nil)
		sub, _ := svc.Create(ctx, "t1", CreateInput{
			Code: "sub-draft", DisplayName: "Draft", CustomerID: "c",
			Items: []CreateItemInput{{PlanID: "p"}},
			// No StartNow, no TrialDays → status defaults to draft.
		})
		if sub.Status != domain.SubscriptionDraft {
			t.Fatalf("setup: expected draft, got %q", sub.Status)
		}
		canceled, _, err := svc.Cancel(ctx, "t1", sub.ID)
		if err != nil {
			t.Fatalf("cancel from draft: %v", err)
		}
		if canceled.Status != domain.SubscriptionCanceled {
			t.Errorf("status: got %q, want canceled", canceled.Status)
		}
		if canceled.CanceledAt == nil {
			t.Error("canceled_at must be set")
		}
	})

	t.Run("cancel from trialing", func(t *testing.T) {
		svc := NewService(newMemStore(), nil)
		sub, _ := svc.Create(ctx, "t1", CreateInput{
			Code: "sub-trial", DisplayName: "Trial", CustomerID: "c",
			Items:     []CreateItemInput{{PlanID: "p"}},
			TrialDays: 14,
		})
		if sub.Status != domain.SubscriptionTrialing {
			t.Fatalf("setup: expected trialing, got %q", sub.Status)
		}
		canceled, _, err := svc.Cancel(ctx, "t1", sub.ID)
		if err != nil {
			t.Fatalf("cancel from trialing: %v", err)
		}
		if canceled.Status != domain.SubscriptionCanceled {
			t.Errorf("status: got %q, want canceled", canceled.Status)
		}
		// Trial-end timestamp is historical evidence — preserved on
		// cancel so reporting can answer "did this customer cancel
		// during their trial or after?"
		if canceled.TrialEndAt == nil {
			t.Error("trial_end_at must be preserved across cancel for historical record")
		}
	})

	t.Run("rejects cancel from canceled (already terminated)", func(t *testing.T) {
		svc := NewService(newMemStore(), nil)
		sub, _ := svc.Create(ctx, "t1", CreateInput{
			Code: "sub-2x", DisplayName: "Twice", CustomerID: "c",
			Items: []CreateItemInput{{PlanID: "p"}}, StartNow: true,
		})
		_, _, _ = svc.Cancel(ctx, "t1", sub.ID)
		_, _, err := svc.Cancel(ctx, "t1", sub.ID)
		if err == nil {
			t.Fatal("expected InvalidState on double-cancel")
		}
	})
}

// TestUpdateItem_PlanChange exercises immediate and scheduled per-item plan
// changes. Mirrors the invariants the old TestChangePlan covered, scoped to
// a single item: immediate stamps plan_changed_at and overwrites plan_id;
// scheduled leaves plan_id alone and records pending fields keyed to the
// current billing period end.
func TestUpdateItem_PlanChange(t *testing.T) {
	svc := NewService(newMemStore(), nil)
	ctx := context.Background()

	sub, _ := svc.Create(ctx, "t1", CreateInput{
		Code: "sub-change", DisplayName: "Test", CustomerID: "c",
		Items:    []CreateItemInput{{PlanID: "plan_old"}},
		StartNow: true,
	})

	// Set billing period so scheduled-change effective_at lines up with it.
	store := svc.store.(*memStore)
	s := store.subs[sub.ID]
	start := clock.Now(ctx).AddDate(0, 0, -15)
	end := clock.Now(ctx).AddDate(0, 0, 15)
	s.CurrentBillingPeriodStart = &start
	s.CurrentBillingPeriodEnd = &end
	store.subs[sub.ID] = s

	itemID := sub.Items[0].ID

	t.Run("immediate plan swap", func(t *testing.T) {
		result, err := svc.UpdateItem(ctx, "t1", sub.ID, itemID, UpdateItemInput{
			NewPlanID: "plan_new",
			Immediate: true,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if result.Item.PlanID != "plan_new" {
			t.Errorf("plan_id: got %q, want plan_new", result.Item.PlanID)
		}
		if result.Item.PlanChangedAt == nil {
			t.Error("plan_changed_at should be set")
		}
	})

	t.Run("same plan rejected", func(t *testing.T) {
		_, err := svc.UpdateItem(ctx, "t1", sub.ID, itemID, UpdateItemInput{NewPlanID: "plan_new", Immediate: true})
		if err == nil {
			t.Fatal("expected error for same plan")
		}
	})

	t.Run("missing new_plan_id and quantity", func(t *testing.T) {
		_, err := svc.UpdateItem(ctx, "t1", sub.ID, itemID, UpdateItemInput{})
		if err == nil {
			t.Fatal("expected error for missing fields")
		}
	})

	t.Run("both quantity and new_plan_id rejected", func(t *testing.T) {
		q := int64(2)
		_, err := svc.UpdateItem(ctx, "t1", sub.ID, itemID, UpdateItemInput{Quantity: &q, NewPlanID: "other"})
		if err == nil {
			t.Fatal("expected error when both fields set")
		}
	})
}

// TestUpdateItem_Scheduled locks in that a non-immediate plan change records
// pending fields at the billing period boundary and leaves the active plan_id
// untouched. Cancel-pending and immediate-supersede-pending are exercised too.
func TestUpdateItem_Scheduled(t *testing.T) {
	svc := NewService(newMemStore(), nil)
	ctx := context.Background()

	sub, _ := svc.Create(ctx, "t1", CreateInput{
		Code: "sub-sched", DisplayName: "Test", CustomerID: "c",
		Items:    []CreateItemInput{{PlanID: "plan_old"}},
		StartNow: true,
	})

	store := svc.store.(*memStore)
	s := store.subs[sub.ID]
	start := clock.Now(ctx).AddDate(0, 0, -5)
	end := clock.Now(ctx).AddDate(0, 0, 25)
	s.CurrentBillingPeriodStart = &start
	s.CurrentBillingPeriodEnd = &end
	store.subs[sub.ID] = s

	itemID := sub.Items[0].ID

	t.Run("records pending, leaves plan_id untouched", func(t *testing.T) {
		result, err := svc.UpdateItem(ctx, "t1", sub.ID, itemID, UpdateItemInput{
			NewPlanID: "plan_new",
			Immediate: false,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if result.Item.PlanID != "plan_old" {
			t.Errorf("plan_id must not change on scheduled: got %q, want plan_old", result.Item.PlanID)
		}
		if result.Item.PendingPlanID != "plan_new" {
			t.Errorf("pending_plan_id: got %q, want plan_new", result.Item.PendingPlanID)
		}
		if result.Item.PendingPlanEffectiveAt == nil {
			t.Fatal("pending_plan_effective_at must be set")
		}
		if !result.Item.PendingPlanEffectiveAt.Equal(end) {
			t.Errorf("pending_plan_effective_at: got %v, want period end %v", *result.Item.PendingPlanEffectiveAt, end)
		}
		if !result.EffectiveAt.Equal(end) {
			t.Errorf("response effective_at: got %v, want %v", result.EffectiveAt, end)
		}
	})

	t.Run("cancel pending clears scheduled change", func(t *testing.T) {
		updated, err := svc.CancelPendingItemChange(ctx, "t1", sub.ID, itemID)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if updated.PendingPlanID != "" {
			t.Errorf("pending_plan_id should be cleared: got %q", updated.PendingPlanID)
		}
		if updated.PendingPlanEffectiveAt != nil {
			t.Errorf("pending_plan_effective_at should be nil: got %v", updated.PendingPlanEffectiveAt)
		}
		if updated.PlanID != "plan_old" {
			t.Errorf("plan_id should remain unchanged: got %q", updated.PlanID)
		}
	})

	t.Run("cancel with no pending is no-op", func(t *testing.T) {
		updated, err := svc.CancelPendingItemChange(ctx, "t1", sub.ID, itemID)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if updated.PendingPlanID != "" {
			t.Errorf("expected no pending: got %q", updated.PendingPlanID)
		}
	})

	t.Run("immediate supersedes pending", func(t *testing.T) {
		_, err := svc.UpdateItem(ctx, "t1", sub.ID, itemID, UpdateItemInput{NewPlanID: "plan_scheduled", Immediate: false})
		if err != nil {
			t.Fatalf("schedule: %v", err)
		}
		result, err := svc.UpdateItem(ctx, "t1", sub.ID, itemID, UpdateItemInput{NewPlanID: "plan_immediate", Immediate: true})
		if err != nil {
			t.Fatalf("immediate: %v", err)
		}
		if result.Item.PlanID != "plan_immediate" {
			t.Errorf("plan_id: got %q, want plan_immediate", result.Item.PlanID)
		}
		if result.Item.PendingPlanID != "" {
			t.Errorf("pending_plan_id should be cleared: got %q", result.Item.PendingPlanID)
		}
	})
}

// TestAddRemoveItem covers item add/remove rules: duplicate plans rejected,
// removing the last item on an active subscription is blocked (caller should
// Cancel instead), removing a non-last item succeeds.
func TestAddRemoveItem(t *testing.T) {
	svc := NewService(newMemStore(), nil)
	ctx := context.Background()

	sub, _ := svc.Create(ctx, "t1", CreateInput{
		Code: "sub-items", DisplayName: "Test", CustomerID: "c",
		Items:    []CreateItemInput{{PlanID: "plan_base"}},
		StartNow: true,
	})

	t.Run("add second item", func(t *testing.T) {
		added, err := svc.AddItem(ctx, "t1", sub.ID, AddItemInput{PlanID: "plan_addon", Quantity: 5})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if added.PlanID != "plan_addon" || added.Quantity != 5 {
			t.Errorf("added item: got %+v", added)
		}
	})

	t.Run("duplicate plan rejected", func(t *testing.T) {
		_, err := svc.AddItem(ctx, "t1", sub.ID, AddItemInput{PlanID: "plan_base"})
		if err == nil {
			t.Fatal("expected ErrAlreadyExists for duplicate plan on subscription")
		}
	})

	t.Run("remove non-last item", func(t *testing.T) {
		fresh, _ := svc.Get(ctx, "t1", sub.ID)
		var addonID string
		for _, it := range fresh.Items {
			if it.PlanID == "plan_addon" {
				addonID = it.ID
			}
		}
		if addonID == "" {
			t.Fatal("addon item not found")
		}
		if err := svc.RemoveItem(ctx, "t1", sub.ID, addonID); err != nil {
			t.Fatalf("remove item: %v", err)
		}
	})

	t.Run("cannot remove last item on active subscription", func(t *testing.T) {
		fresh, _ := svc.Get(ctx, "t1", sub.ID)
		if len(fresh.Items) != 1 {
			t.Fatalf("expected 1 item remaining, got %d", len(fresh.Items))
		}
		err := svc.RemoveItem(ctx, "t1", sub.ID, fresh.Items[0].ID)
		if err == nil {
			t.Fatal("expected error removing last item from active sub")
		}
	})
}

// TestScheduleCancel covers the v1 service-layer validation rules for the
// soft-cancel surface: the at_period_end / cancel_at mutual exclusion, the
// "must be in the future" guard, the "must land on a clean cycle boundary"
// guard, the round-trip schedule → clear path, and toggling between modes.
func TestScheduleCancel(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	periodStart := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)

	mkSvc := func() (*Service, domain.Subscription) {
		svc := NewService(newMemStore(), clock.NewFake(now))
		sub, _ := svc.Create(context.Background(), "t1", CreateInput{
			Code: "sub-sched", DisplayName: "Test", CustomerID: "c",
			Items:    []CreateItemInput{{PlanID: "p"}},
			StartNow: true,
		})
		_, _ = svc.Activate(context.Background(), "t1", sub.ID)
		// Stamp a billing period so cancel_at validation has something to compare against.
		stored, _ := svc.Get(context.Background(), "t1", sub.ID)
		stored.CurrentBillingPeriodStart = &periodStart
		stored.CurrentBillingPeriodEnd = &periodEnd
		_, _ = svc.store.(*memStore).Update(context.Background(), "t1", stored)
		return svc, stored
	}

	t.Run("at_period_end sets the flag", func(t *testing.T) {
		svc, sub := mkSvc()
		out, err := svc.ScheduleCancel(context.Background(), "t1", sub.ID, ScheduleCancelInput{AtPeriodEnd: true})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !out.CancelAtPeriodEnd {
			t.Error("cancel_at_period_end should be true")
		}
		if out.CancelAt != nil {
			t.Error("cancel_at should remain nil when scheduling at_period_end")
		}
	})

	t.Run("cancel_at on or after period end accepted", func(t *testing.T) {
		svc, sub := mkSvc()
		ts := periodEnd
		out, err := svc.ScheduleCancel(context.Background(), "t1", sub.ID, ScheduleCancelInput{CancelAt: &ts})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if out.CancelAt == nil || !out.CancelAt.Equal(periodEnd) {
			t.Errorf("cancel_at: got %v, want %v", out.CancelAt, periodEnd)
		}
		if out.CancelAtPeriodEnd {
			t.Error("at_period_end flag should remain false on timestamp schedule")
		}
	})

	t.Run("rejects empty input", func(t *testing.T) {
		svc, sub := mkSvc()
		_, err := svc.ScheduleCancel(context.Background(), "t1", sub.ID, ScheduleCancelInput{})
		if err == nil {
			t.Fatal("expected error when neither field is set")
		}
	})

	t.Run("rejects both fields together", func(t *testing.T) {
		svc, sub := mkSvc()
		ts := periodEnd
		_, err := svc.ScheduleCancel(context.Background(), "t1", sub.ID, ScheduleCancelInput{AtPeriodEnd: true, CancelAt: &ts})
		if err == nil {
			t.Fatal("expected error when both fields are set")
		}
	})

	t.Run("rejects past timestamp", func(t *testing.T) {
		svc, sub := mkSvc()
		past := now.Add(-time.Hour)
		_, err := svc.ScheduleCancel(context.Background(), "t1", sub.ID, ScheduleCancelInput{CancelAt: &past})
		if err == nil {
			t.Fatal("expected error for past cancel_at")
		}
	})

	t.Run("rejects mid-period timestamp", func(t *testing.T) {
		svc, sub := mkSvc()
		mid := periodEnd.Add(-24 * time.Hour)
		_, err := svc.ScheduleCancel(context.Background(), "t1", sub.ID, ScheduleCancelInput{CancelAt: &mid})
		if err == nil {
			t.Fatal("expected error for cancel_at before current_billing_period_end")
		}
	})

	t.Run("clear undoes any prior schedule", func(t *testing.T) {
		svc, sub := mkSvc()
		_, err := svc.ScheduleCancel(context.Background(), "t1", sub.ID, ScheduleCancelInput{AtPeriodEnd: true})
		if err != nil {
			t.Fatalf("schedule: %v", err)
		}
		out, err := svc.ClearScheduledCancel(context.Background(), "t1", sub.ID)
		if err != nil {
			t.Fatalf("clear: %v", err)
		}
		if out.CancelAtPeriodEnd {
			t.Error("cancel_at_period_end should be false after clear")
		}
		if out.CancelAt != nil {
			t.Error("cancel_at should be nil after clear")
		}
	})

	t.Run("toggle from at_period_end to cancel_at replaces schedule", func(t *testing.T) {
		svc, sub := mkSvc()
		if _, err := svc.ScheduleCancel(context.Background(), "t1", sub.ID, ScheduleCancelInput{AtPeriodEnd: true}); err != nil {
			t.Fatalf("first schedule: %v", err)
		}
		ts := periodEnd
		out, err := svc.ScheduleCancel(context.Background(), "t1", sub.ID, ScheduleCancelInput{CancelAt: &ts})
		if err != nil {
			t.Fatalf("second schedule: %v", err)
		}
		if out.CancelAtPeriodEnd {
			t.Error("at_period_end flag should be cleared by full replacement")
		}
		if out.CancelAt == nil {
			t.Error("cancel_at should be set after toggle")
		}
	})
}

// TestPauseCollection covers the v1 service-layer validation for the
// pause_collection surface: behavior whitelist (only keep_as_draft for now),
// resumes_at must be future or nil, and the round-trip pause → resume path.
func TestPauseCollection(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)

	mkSvc := func() (*Service, domain.Subscription) {
		svc := NewService(newMemStore(), clock.NewFake(now))
		sub, _ := svc.Create(context.Background(), "t1", CreateInput{
			Code: "sub-pause", DisplayName: "Test", CustomerID: "c",
			Items:    []CreateItemInput{{PlanID: "p"}},
			StartNow: true,
		})
		_, _ = svc.Activate(context.Background(), "t1", sub.ID)
		stored, _ := svc.Get(context.Background(), "t1", sub.ID)
		return svc, stored
	}

	t.Run("behavior keep_as_draft accepted, no resumes_at", func(t *testing.T) {
		svc, sub := mkSvc()
		out, err := svc.PauseCollection(context.Background(), "t1", sub.ID, PauseCollectionInput{
			Behavior: domain.PauseCollectionKeepAsDraft,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if out.PauseCollection == nil {
			t.Fatal("PauseCollection must be set")
		}
		if out.PauseCollection.Behavior != domain.PauseCollectionKeepAsDraft {
			t.Errorf("behavior: got %q, want %q", out.PauseCollection.Behavior, domain.PauseCollectionKeepAsDraft)
		}
		if out.PauseCollection.ResumesAt != nil {
			t.Error("ResumesAt must be nil when not provided")
		}
	})

	t.Run("future resumes_at accepted", func(t *testing.T) {
		svc, sub := mkSvc()
		future := now.Add(7 * 24 * time.Hour)
		out, err := svc.PauseCollection(context.Background(), "t1", sub.ID, PauseCollectionInput{
			Behavior:  domain.PauseCollectionKeepAsDraft,
			ResumesAt: &future,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if out.PauseCollection == nil || out.PauseCollection.ResumesAt == nil {
			t.Fatal("ResumesAt must be set")
		}
		if !out.PauseCollection.ResumesAt.Equal(future) {
			t.Errorf("ResumesAt: got %v, want %v", out.PauseCollection.ResumesAt, future)
		}
	})

	t.Run("rejects empty behavior", func(t *testing.T) {
		svc, sub := mkSvc()
		_, err := svc.PauseCollection(context.Background(), "t1", sub.ID, PauseCollectionInput{})
		if err == nil {
			t.Fatal("expected error for empty behavior")
		}
	})

	t.Run("rejects unsupported behavior", func(t *testing.T) {
		svc, sub := mkSvc()
		_, err := svc.PauseCollection(context.Background(), "t1", sub.ID, PauseCollectionInput{
			Behavior: domain.PauseCollectionBehavior("mark_uncollectible"),
		})
		if err == nil {
			t.Fatal("expected error for unsupported behavior")
		}
	})

	t.Run("rejects past resumes_at", func(t *testing.T) {
		svc, sub := mkSvc()
		past := now.Add(-time.Hour)
		_, err := svc.PauseCollection(context.Background(), "t1", sub.ID, PauseCollectionInput{
			Behavior:  domain.PauseCollectionKeepAsDraft,
			ResumesAt: &past,
		})
		if err == nil {
			t.Fatal("expected error for past resumes_at")
		}
	})

	t.Run("resume clears the pause", func(t *testing.T) {
		svc, sub := mkSvc()
		future := now.Add(time.Hour)
		if _, err := svc.PauseCollection(context.Background(), "t1", sub.ID, PauseCollectionInput{
			Behavior:  domain.PauseCollectionKeepAsDraft,
			ResumesAt: &future,
		}); err != nil {
			t.Fatalf("pause: %v", err)
		}
		out, err := svc.ResumeCollection(context.Background(), "t1", sub.ID)
		if err != nil {
			t.Fatalf("resume: %v", err)
		}
		if out.PauseCollection != nil {
			t.Errorf("PauseCollection should be nil after resume, got %+v", out.PauseCollection)
		}
	})

	t.Run("re-pause replaces resumes_at", func(t *testing.T) {
		svc, sub := mkSvc()
		first := now.Add(time.Hour)
		if _, err := svc.PauseCollection(context.Background(), "t1", sub.ID, PauseCollectionInput{
			Behavior:  domain.PauseCollectionKeepAsDraft,
			ResumesAt: &first,
		}); err != nil {
			t.Fatalf("first: %v", err)
		}
		second := now.Add(48 * time.Hour)
		out, err := svc.PauseCollection(context.Background(), "t1", sub.ID, PauseCollectionInput{
			Behavior:  domain.PauseCollectionKeepAsDraft,
			ResumesAt: &second,
		})
		if err != nil {
			t.Fatalf("second: %v", err)
		}
		if !out.PauseCollection.ResumesAt.Equal(second) {
			t.Errorf("ResumesAt: got %v, want %v (second call must replace first)",
				out.PauseCollection.ResumesAt, second)
		}
	})
}

func TestEndTrial(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)

	mkTrialingSvc := func() (*Service, domain.Subscription) {
		svc := NewService(newMemStore(), clock.NewFake(now))
		sub, err := svc.Create(context.Background(), "t1", CreateInput{
			Code: "sub-trial", DisplayName: "Trial", CustomerID: "c",
			Items:     []CreateItemInput{{PlanID: "p"}},
			TrialDays: 14,
		})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		return svc, sub
	}

	t.Run("flips trialing to active and stamps activated_at", func(t *testing.T) {
		svc, sub := mkTrialingSvc()
		if sub.Status != domain.SubscriptionTrialing {
			t.Fatalf("precondition: want trialing, got %q", sub.Status)
		}
		if sub.ActivatedAt != nil {
			t.Fatal("precondition: activated_at should be nil during trial")
		}

		out, err := svc.EndTrial(context.Background(), "t1", sub.ID)
		if err != nil {
			t.Fatalf("EndTrial: %v", err)
		}
		if out.Status != domain.SubscriptionActive {
			t.Errorf("status: got %q, want active", out.Status)
		}
		if out.ActivatedAt == nil {
			t.Error("activated_at must be set after EndTrial")
		}
	})

	t.Run("rejects when sub is already active (not trialing)", func(t *testing.T) {
		svc := NewService(newMemStore(), clock.NewFake(now))
		sub, _ := svc.Create(context.Background(), "t1", CreateInput{
			Code: "sub-active", DisplayName: "Active", CustomerID: "c",
			Items:    []CreateItemInput{{PlanID: "p"}},
			StartNow: true,
		})
		_, err := svc.EndTrial(context.Background(), "t1", sub.ID)
		if err == nil {
			t.Fatal("expected error when ending trial on non-trialing sub")
		}
	})

	t.Run("idempotent: second call on already-active returns error", func(t *testing.T) {
		svc, sub := mkTrialingSvc()
		if _, err := svc.EndTrial(context.Background(), "t1", sub.ID); err != nil {
			t.Fatalf("first EndTrial: %v", err)
		}
		if _, err := svc.EndTrial(context.Background(), "t1", sub.ID); err == nil {
			t.Error("second EndTrial on already-active should error")
		}
	})
}

func TestExtendTrial(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)

	mkTrialingSvc := func() (*Service, domain.Subscription) {
		svc := NewService(newMemStore(), clock.NewFake(now))
		sub, err := svc.Create(context.Background(), "t1", CreateInput{
			Code: "sub-trial", DisplayName: "Trial", CustomerID: "c",
			Items:     []CreateItemInput{{PlanID: "p"}},
			TrialDays: 14,
		})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		return svc, sub
	}

	t.Run("pushes trial_end_at later", func(t *testing.T) {
		svc, sub := mkTrialingSvc()
		original := *sub.TrialEndAt
		newEnd := original.AddDate(0, 0, 14) // +14 days

		out, err := svc.ExtendTrial(context.Background(), "t1", sub.ID, newEnd)
		if err != nil {
			t.Fatalf("ExtendTrial: %v", err)
		}
		if out.TrialEndAt == nil || !out.TrialEndAt.Equal(newEnd) {
			t.Errorf("trial_end_at: got %v, want %v", out.TrialEndAt, newEnd)
		}
		if out.Status != domain.SubscriptionTrialing {
			t.Errorf("status should remain trialing, got %q", out.Status)
		}
	})

	t.Run("rejects trial_end in the past", func(t *testing.T) {
		svc, sub := mkTrialingSvc()
		past := now.AddDate(0, 0, -1)
		if _, err := svc.ExtendTrial(context.Background(), "t1", sub.ID, past); err == nil {
			t.Error("expected error for past trial_end")
		}
	})

	t.Run("rejects trial_end at or before current trial_end_at", func(t *testing.T) {
		svc, sub := mkTrialingSvc()
		// Same as current — not strictly after.
		if _, err := svc.ExtendTrial(context.Background(), "t1", sub.ID, *sub.TrialEndAt); err == nil {
			t.Error("expected error for non-extending trial_end")
		}
		// Strictly before current trial_end_at but still in the future.
		earlier := sub.TrialEndAt.Add(-time.Hour)
		if _, err := svc.ExtendTrial(context.Background(), "t1", sub.ID, earlier); err == nil {
			t.Error("expected error when shrinking trial_end")
		}
	})

	t.Run("rejects when sub is not trialing", func(t *testing.T) {
		svc := NewService(newMemStore(), clock.NewFake(now))
		sub, _ := svc.Create(context.Background(), "t1", CreateInput{
			Code: "sub-active", DisplayName: "Active", CustomerID: "c",
			Items:    []CreateItemInput{{PlanID: "p"}},
			StartNow: true,
		})
		future := now.AddDate(0, 0, 30)
		if _, err := svc.ExtendTrial(context.Background(), "t1", sub.ID, future); err == nil {
			t.Error("expected error when extending trial on non-trialing sub")
		}
	})
}

// TestProcessExpiredTrialsForClock covers Phase 0.5 of the catchup
// orchestrator: trialing subs whose `trial_end_at` has elapsed in
// simulated time must flip to active at trial_end_at (not at the
// later cycle close). Locks Bug #8 — pre-fix, status='trialing'
// hung on for the gap between trial_end_at and next_billing_at (up
// to ~30 days for calendar billing).
func TestProcessExpiredTrialsForClock(t *testing.T) {
	createdAt := time.Date(2025, 11, 29, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()

	t.Run("flips trialing → active at trial_end_at when frozen has elapsed", func(t *testing.T) {
		svc := NewService(newMemStore(), clock.NewFake(createdAt))
		fb := &fakeBiller{}
		svc.SetBiller(fb)
		aud := &captureAudit{}
		svc.SetAuditLogger(aud)
		sub, err := svc.Create(ctx, "t1", CreateInput{
			Code: "sub-trial", DisplayName: "Trial", CustomerID: "c",
			Items:       []CreateItemInput{{PlanID: "p"}},
			TrialDays:   14,
			BillingTime: domain.BillingTimeCalendar,
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		// Pin the sub to a clock so the scan picks it up.
		mem := svc.store.(*memStore)
		seeded := mem.subs[sub.ID]
		seeded.TestClockID = "tclk_1"
		mem.subs[sub.ID] = seeded

		// Advance simulated clock to 5 days past trial_end (Dec 13).
		frozen := sub.TrialEndAt.Add(5 * 24 * time.Hour)
		// The DRIVER CONTRACT: every *ForClock phase is called only from
		// testclock.RunCatchup, which binds the clock + its frozen instant onto
		// ctx ONCE for the whole advance (one bind, not nine self-binds). Every
		// audit row a phase emits inherits the axis from that ctx. Reproduce it
		// here — calling the phase on a bare ctx tests a shape production never
		// runs, and would let a real unstamping regression pass.
		ctx := clock.WithSim(ctx, clock.Sim{At: frozen, TestClockID: "tclk_1"})

		processed, errs := svc.ProcessExpiredTrialsForClock(ctx, "t1", "tclk_1", frozen)
		if len(errs) != 0 {
			t.Fatalf("unexpected errors: %v", errs)
		}
		if processed != 1 {
			t.Errorf("processed: got %d, want 1", processed)
		}

		out := mem.subs[sub.ID]
		if out.Status != domain.SubscriptionActive {
			t.Errorf("status: got %q, want active", out.Status)
		}
		// ActivatedAt should be stamped at trial_end_at, not at frozen
		// (the "as if the sub activated exactly when trial ended" semantic).
		if out.ActivatedAt == nil || !out.ActivatedAt.Equal(*sub.TrialEndAt) {
			t.Errorf("activated_at: got %v, want %v (= trial_end_at)", out.ActivatedAt, sub.TrialEndAt)
		}

		// ADR-069 alignment: the day-1 invoice now rides the SAME tx as the
		// flip on the clock path too (BillOnCreateTx via WithBill) — the old
		// post-flip BillOnCreate's "deferred" WARN was a revenue leak.
		if fb.createTxCalls != 1 {
			t.Errorf("BillOnCreateTx calls: got %d, want 1 (in-tx day-1 coverage)", fb.createTxCalls)
		}

		// Audit row: the auto-flip must leave the same trial_ended trace the
		// operator EndTrial writes, marked triggered_by=schedule.
		if len(aud.entries) != 1 {
			t.Fatalf("audit entries: got %d, want 1 (trial_ended)", len(aud.entries))
		}
		ae := aud.entries[0]
		am := ae.metadata
		if am["action"] != "trial_ended" || am["triggered_by"] != "schedule" {
			t.Errorf("audit metadata: got %+v, want action=trial_ended triggered_by=schedule", am)
		}
		// The sim axis rides the ctx binding (the writer reads it there), so the
		// emission must run BOUND to the clock. It must NOT hand-stamp the axis
		// into metadata: the writer owns those keys and overwrites them.
		if !ae.bound {
			t.Fatal("trial_ended emitted on an UNBOUND ctx — the row lands with NULL sim columns and is invisible to ?test_clock_id=")
		}
		if ae.sim.TestClockID != "tclk_1" {
			t.Errorf("bound clock: got %q, want tclk_1", ae.sim.TestClockID)
		}
		for _, k := range []string{"sim_effective_at", "test_clock_id"} {
			if _, hand := am[k]; hand {
				t.Errorf("emitter hand-stamped %q — the audit writer owns that key", k)
			}
		}
		// The trial-end instant is a DIFFERENT fact from the row's sim instant
		// (a catchup performs this flip at the ADVANCE's instant, which can be
		// weeks of simulated time later), so it travels under its own key.
		if am["trial_end_at"] == nil {
			t.Errorf("audit metadata: missing trial_end_at, got %+v", am)
		}
	})

	t.Run("skips subs whose trial has not yet elapsed", func(t *testing.T) {
		svc := NewService(newMemStore(), clock.NewFake(createdAt))
		svc.SetBiller(&fakeBiller{})
		sub, _ := svc.Create(ctx, "t1", CreateInput{
			Code: "sub-still-trialing", DisplayName: "Still Trialing",
			CustomerID: "c",
			Items:      []CreateItemInput{{PlanID: "p"}},
			TrialDays:  14,
		})
		mem := svc.store.(*memStore)
		seeded := mem.subs[sub.ID]
		seeded.TestClockID = "tclk_1"
		mem.subs[sub.ID] = seeded

		// Frozen time is BEFORE trial_end → no expiry.
		frozen := sub.TrialEndAt.Add(-24 * time.Hour)
		processed, _ := svc.ProcessExpiredTrialsForClock(ctx, "t1", "tclk_1", frozen)
		if processed != 0 {
			t.Errorf("processed: got %d, want 0 (trial not yet elapsed)", processed)
		}
		if mem.subs[sub.ID].Status != domain.SubscriptionTrialing {
			t.Errorf("status: should remain trialing, got %q", mem.subs[sub.ID].Status)
		}
	})

	t.Run("skips subs pinned to a different clock", func(t *testing.T) {
		svc := NewService(newMemStore(), clock.NewFake(createdAt))
		svc.SetBiller(&fakeBiller{})
		sub, _ := svc.Create(ctx, "t1", CreateInput{
			Code: "sub-other-clock", DisplayName: "Other clock",
			CustomerID: "c",
			Items:      []CreateItemInput{{PlanID: "p"}},
			TrialDays:  14,
		})
		mem := svc.store.(*memStore)
		seeded := mem.subs[sub.ID]
		seeded.TestClockID = "tclk_other"
		mem.subs[sub.ID] = seeded

		frozen := sub.TrialEndAt.Add(24 * time.Hour)
		processed, _ := svc.ProcessExpiredTrialsForClock(ctx, "t1", "tclk_1", frozen)
		if processed != 0 {
			t.Errorf("processed: got %d, want 0 (different clock)", processed)
		}
	})

	t.Run("fires subscription.trial_ended webhook on activation", func(t *testing.T) {
		// Mirrors the engine auto-flip path's webhook dispatch so
		// consumers see one event per trial transition regardless of
		// which path activated the sub.
		svc := NewService(newMemStore(), clock.NewFake(createdAt))
		svc.SetBiller(&fakeBiller{})
		fd := &fakeDispatcher{}
		svc.SetEventDispatcher(fd)
		sub, _ := svc.Create(ctx, "t1", CreateInput{
			Code: "sub-webhook", DisplayName: "Webhook", CustomerID: "c",
			Items: []CreateItemInput{{PlanID: "p"}}, TrialDays: 14,
		})
		mem := svc.store.(*memStore)
		seeded := mem.subs[sub.ID]
		seeded.TestClockID = "tclk_1"
		mem.subs[sub.ID] = seeded
		frozen := sub.TrialEndAt.Add(24 * time.Hour)
		if _, errs := svc.ProcessExpiredTrialsForClock(ctx, "t1", "tclk_1", frozen); len(errs) != 0 {
			t.Fatalf("unexpected errors: %v", errs)
		}
		// subscription.trial_ended moved IN-TX to the store's
		// ActivateAfterTrial writer (DispatchTx subscription subset,
		// 2026-07-05) — the service must NOT also dispatch it. In-tx
		// enqueue + payload proven by lifecycle_outbox_integration_test.go.
		for _, ev := range fd.events {
			if ev.eventType == domain.EventSubscriptionTrialEnded {
				t.Fatalf("%s must not be dispatched post-commit — it is enqueued in the activation tx", domain.EventSubscriptionTrialEnded)
			}
		}
	})

	t.Run("operator-EndTrial race: row already active is silently skipped", func(t *testing.T) {
		// The scan's SELECT and the per-sub UPDATE aren't in the same
		// tx; an operator EndTrial between them moves the row out of
		// 'trialing' and ActivateAfterTrial returns InvalidState. The
		// phase treats this as a no-op (the desired state is already
		// reached) so a concurrent operator action doesn't trip the
		// catchup pass with a spurious error.
		svc := NewService(newMemStore(), clock.NewFake(createdAt))
		svc.SetBiller(&fakeBiller{})
		sub, _ := svc.Create(ctx, "t1", CreateInput{
			Code: "sub-race", DisplayName: "Race", CustomerID: "c",
			Items:     []CreateItemInput{{PlanID: "p"}},
			TrialDays: 14,
		})
		mem := svc.store.(*memStore)
		seeded := mem.subs[sub.ID]
		seeded.TestClockID = "tclk_1"
		// Pre-activate to simulate the race.
		seeded.Status = domain.SubscriptionActive
		mem.subs[sub.ID] = seeded

		frozen := sub.TrialEndAt.Add(24 * time.Hour)
		// ListExpiredTrialsForClock filters by status='trialing' so
		// the pre-activated row won't even be returned — verifies
		// the filter, not the InvalidState path. (The race-detection
		// in the production postgres path uses SKIP LOCKED + the
		// per-sub UPDATE WHERE status='trialing' clause.)
		processed, errs := svc.ProcessExpiredTrialsForClock(ctx, "t1", "tclk_1", frozen)
		if len(errs) != 0 {
			t.Errorf("unexpected errors: %v", errs)
		}
		if processed != 0 {
			t.Errorf("processed: got %d, want 0 (row already active)", processed)
		}
	})
}

// TestProcessExpiredTrials covers the wall-clock counterpart to the
// catchup orchestrator's Phase 0.5 — the scheduler-tick scan that
// flips non-clock-pinned trialing subs at trial_end_at. Mirrors
// TestProcessExpiredTrialsForClock but exercises the production
// (no test_clock_id) path.
func TestProcessExpiredTrials(t *testing.T) {
	createdAt := time.Date(2025, 11, 29, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()

	t.Run("wall-clock: flips trialing → active at trial_end_at past now", func(t *testing.T) {
		svc := NewService(newMemStore(), clock.NewFake(createdAt))
		fb := &fakeBiller{}
		svc.SetBiller(fb)
		sub, _ := svc.Create(ctx, "t1", CreateInput{
			Code: "sub-trial-wall", DisplayName: "Wall Trial", CustomerID: "c",
			Items:     []CreateItemInput{{PlanID: "p"}},
			TrialDays: 14,
		})

		// Advance the service clock to 5 days past trial_end. Since
		// NewService doesn't expose SetClock, build a fresh service
		// over the same mem store with an advanced fake clock.
		mem := svc.store.(*memStore)
		laterClock := clock.NewFake(sub.TrialEndAt.Add(5 * 24 * time.Hour))
		svc2 := NewService(mem, laterClock)
		svc2.SetBiller(fb)

		processed, errs := svc2.ProcessExpiredTrials(ctx, 100)
		if len(errs) != 0 {
			t.Fatalf("unexpected errors: %v", errs)
		}
		if processed != 1 {
			t.Errorf("processed: got %d, want 1", processed)
		}

		out := mem.subs[sub.ID]
		if out.Status != domain.SubscriptionActive {
			t.Errorf("status: got %q, want active", out.Status)
		}
		if out.ActivatedAt == nil || !out.ActivatedAt.Equal(*sub.TrialEndAt) {
			t.Errorf("activated_at: got %v, want %v (= trial_end_at)", out.ActivatedAt, sub.TrialEndAt)
		}
		// ADR-056: the day-1 invoice is now built in-tx via BillOnCreateTx.
		if fb.createTxCalls != 1 {
			t.Errorf("BillOnCreateTx: got %d calls, want 1", fb.createTxCalls)
		}
	})

	t.Run("wall-clock: excludes clock-pinned subs (ADR-028 disjoint flows)", func(t *testing.T) {
		// Clock-pinned subs must NOT be processed by the wall-clock
		// path — they flow through the catchup orchestrator's Phase
		// 0.5 instead. Running them in both paths would race the
		// orchestrator and could double-fire BillOnCreate (idempotent
		// on the UNIQUE constraint but still wrong semantically).
		svc := NewService(newMemStore(), clock.NewFake(createdAt))
		svc.SetBiller(&fakeBiller{})
		sub, _ := svc.Create(ctx, "t1", CreateInput{
			Code: "sub-clock-pinned", DisplayName: "Clock-pinned",
			CustomerID: "c",
			Items:      []CreateItemInput{{PlanID: "p"}},
			TrialDays:  14,
		})
		mem := svc.store.(*memStore)
		seeded := mem.subs[sub.ID]
		seeded.TestClockID = "tclk_1"
		mem.subs[sub.ID] = seeded

		laterClock := clock.NewFake(sub.TrialEndAt.Add(5 * 24 * time.Hour))
		svc2 := NewService(mem, laterClock)
		svc2.SetBiller(&fakeBiller{})

		processed, _ := svc2.ProcessExpiredTrials(ctx, 100)
		if processed != 0 {
			t.Errorf("processed: got %d, want 0 (clock-pinned subs flow through catchup)", processed)
		}
		// Status stays trialing — only the catchup orchestrator can
		// transition this sub.
		if mem.subs[sub.ID].Status != domain.SubscriptionTrialing {
			t.Errorf("status: got %q, want trialing (clock-pinned not touched by wall-clock scan)", mem.subs[sub.ID].Status)
		}
	})
}

// TestPeriodAnchoring locks in the post-Bug-1/2/11 contract for how the
// first billing period anchors across every entry point (Create with
// trial / Create with start_now / Activate from draft / EndTrial-early /
// ExtendTrial). Calendar billing must produce a stub period bounded by
// the next calendar month; anniversary must produce a full cycle from
// the anchor instant. Pre-fix Create+trial+calendar dropped the stub
// silently (revenue leak); pre-fix Activate hardcoded calendar and
// backdated to month-start; pre-fix EndTrial / ExtendTrial left
// period boundaries pointing at the original (now-wrong) anchor.
func TestPeriodAnchoring(t *testing.T) {
	// Pick a mid-month "now" so the calendar-billing branch produces a
	// non-trivial stub. UTC for all snaps — no tenant TZ surprises.
	now := time.Date(2025, 11, 29, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()

	t.Run("Create + trial + calendar → stub period from trial_end to next month start", func(t *testing.T) {
		svc := NewService(newMemStore(), clock.NewFake(now))
		sub, err := svc.Create(ctx, "t1", CreateInput{
			Code: "s", DisplayName: "n", CustomerID: "c",
			Items: []CreateItemInput{{PlanID: "p"}}, TrialDays: 14,
			BillingTime: domain.BillingTimeCalendar,
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		// trial_end = now + 14d = Dec 13, 2025
		wantTrialEnd := time.Date(2025, 12, 13, 12, 0, 0, 0, time.UTC)
		if !sub.TrialEndAt.Equal(wantTrialEnd) {
			t.Errorf("trial_end_at: got %v, want %v", sub.TrialEndAt, wantTrialEnd)
		}
		// First chargeable period: stub from Dec 13 → Jan 1 (calendar
		// anchors first full cycle at month boundary).
		wantPeriodStart := time.Date(2025, 12, 13, 0, 0, 0, 0, time.UTC)
		wantPeriodEnd := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		if !sub.CurrentBillingPeriodStart.Equal(wantPeriodStart) {
			t.Errorf("period_start: got %v, want %v (stub)", sub.CurrentBillingPeriodStart, wantPeriodStart)
		}
		if !sub.CurrentBillingPeriodEnd.Equal(wantPeriodEnd) {
			t.Errorf("period_end: got %v, want %v (next month boundary)", sub.CurrentBillingPeriodEnd, wantPeriodEnd)
		}
		if !sub.NextBillingAt.Equal(wantPeriodEnd) {
			t.Errorf("next_billing_at: got %v, want %v", sub.NextBillingAt, wantPeriodEnd)
		}
	})

	t.Run("Create + trial + anniversary → full cycle from trial_end", func(t *testing.T) {
		svc := NewService(newMemStore(), clock.NewFake(now))
		sub, _ := svc.Create(ctx, "t1", CreateInput{
			Code: "s", DisplayName: "n", CustomerID: "c",
			Items: []CreateItemInput{{PlanID: "p"}}, TrialDays: 14,
			BillingTime: domain.BillingTimeAnniversary,
		})
		// Anniversary: period_start = trial_end snapped to day, period_end = +1mo
		wantPeriodStart := time.Date(2025, 12, 13, 0, 0, 0, 0, time.UTC)
		wantPeriodEnd := time.Date(2026, 1, 13, 0, 0, 0, 0, time.UTC)
		if !sub.CurrentBillingPeriodStart.Equal(wantPeriodStart) {
			t.Errorf("period_start: got %v, want %v", sub.CurrentBillingPeriodStart, wantPeriodStart)
		}
		if !sub.CurrentBillingPeriodEnd.Equal(wantPeriodEnd) {
			t.Errorf("period_end: got %v, want %v", sub.CurrentBillingPeriodEnd, wantPeriodEnd)
		}
	})

	t.Run("Create + trial + calendar — trial_end exactly on calendar boundary → no stub", func(t *testing.T) {
		// Sub created Nov 1, 30-day trial → trial_end = Dec 1 (exact
		// month boundary). The stub computation collapses; promote to
		// full cycle Dec 1 → Jan 1.
		anchor := time.Date(2025, 11, 1, 0, 0, 0, 0, time.UTC)
		svc := NewService(newMemStore(), clock.NewFake(anchor))
		sub, _ := svc.Create(ctx, "t1", CreateInput{
			Code: "s", DisplayName: "n", CustomerID: "c",
			Items: []CreateItemInput{{PlanID: "p"}}, TrialDays: 30,
			BillingTime: domain.BillingTimeCalendar,
		})
		wantPeriodStart := time.Date(2025, 12, 1, 0, 0, 0, 0, time.UTC)
		wantPeriodEnd := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		if !sub.CurrentBillingPeriodStart.Equal(wantPeriodStart) {
			t.Errorf("period_start: got %v, want %v", sub.CurrentBillingPeriodStart, wantPeriodStart)
		}
		if !sub.CurrentBillingPeriodEnd.Equal(wantPeriodEnd) {
			t.Errorf("period_end: got %v, want %v (full cycle, stub collapsed)", sub.CurrentBillingPeriodEnd, wantPeriodEnd)
		}
	})

	t.Run("Activate draft + calendar → period starts at activation day, NOT month start", func(t *testing.T) {
		// Pre-fix Activate hardcoded beginningOfMonth(now) for period_start,
		// backdating to Nov 1 and billing the customer for the full Nov
		// cycle including days the sub was a draft.
		svc := NewService(newMemStore(), clock.NewFake(now))
		sub, _ := svc.Create(ctx, "t1", CreateInput{
			Code: "s", DisplayName: "n", CustomerID: "c",
			Items:       []CreateItemInput{{PlanID: "p"}}, // no StartNow, no TrialDays → draft
			BillingTime: domain.BillingTimeCalendar,
		})
		if sub.Status != domain.SubscriptionDraft {
			t.Fatalf("precondition: want draft, got %q", sub.Status)
		}
		activated, err := svc.Activate(ctx, "t1", sub.ID)
		if err != nil {
			t.Fatalf("Activate: %v", err)
		}
		// period_start = day-snapped activation instant (Nov 29 00:00),
		// NOT beginningOfMonth (Nov 1).
		wantPeriodStart := time.Date(2025, 11, 29, 0, 0, 0, 0, time.UTC)
		wantPeriodEnd := time.Date(2025, 12, 1, 0, 0, 0, 0, time.UTC)
		if !activated.CurrentBillingPeriodStart.Equal(wantPeriodStart) {
			t.Errorf("period_start: got %v, want %v (NOT backdated to Nov 1)", activated.CurrentBillingPeriodStart, wantPeriodStart)
		}
		if !activated.CurrentBillingPeriodEnd.Equal(wantPeriodEnd) {
			t.Errorf("period_end: got %v, want %v", activated.CurrentBillingPeriodEnd, wantPeriodEnd)
		}
	})

	t.Run("Activate draft + anniversary → period starts at activation day, ends +1 month", func(t *testing.T) {
		// Pre-fix Activate ignored sub.BillingTime entirely — an
		// anniversary draft activated mid-month still got calendar-
		// anchored periods. Lock the fix.
		svc := NewService(newMemStore(), clock.NewFake(now))
		sub, _ := svc.Create(ctx, "t1", CreateInput{
			Code: "s", DisplayName: "n", CustomerID: "c",
			Items:       []CreateItemInput{{PlanID: "p"}},
			BillingTime: domain.BillingTimeAnniversary,
		})
		activated, _ := svc.Activate(ctx, "t1", sub.ID)
		wantPeriodStart := time.Date(2025, 11, 29, 0, 0, 0, 0, time.UTC)
		wantPeriodEnd := time.Date(2025, 12, 29, 0, 0, 0, 0, time.UTC)
		if !activated.CurrentBillingPeriodStart.Equal(wantPeriodStart) {
			t.Errorf("period_start: got %v, want %v", activated.CurrentBillingPeriodStart, wantPeriodStart)
		}
		if !activated.CurrentBillingPeriodEnd.Equal(wantPeriodEnd) {
			t.Errorf("period_end: got %v, want %v (anniversary cycle, NOT calendar)", activated.CurrentBillingPeriodEnd, wantPeriodEnd)
		}
	})

	t.Run("EndTrial early + calendar → period resets to activation day, trial_end truncated", func(t *testing.T) {
		// Sub created Nov 29 + 14d trial = trial_end Dec 13. Operator
		// EndTrial at Dec 5 (8 days into trial, 8 days before scheduled
		// end). Pre-fix: trial_end stayed Dec 13, period stayed Dec 13 →
		// Jan 1, customer got 8 free days (Dec 5 → Dec 13). Post-fix:
		// trial_end truncates to Dec 5, period anchors at Dec 5 → Jan 1.
		earlyEnd := time.Date(2025, 12, 5, 9, 0, 0, 0, time.UTC)
		// Create with the original `now` clock (Nov 29), then build a
		// fresh Service over the same memStore with an earlyEnd-pinned
		// clock so EndTrial reads `now = earlyEnd`. NewService doesn't
		// expose a SetClock, so the two-service shape is the cleanest
		// way to advance the test clock between Create and EndTrial.
		mem := newMemStore()
		svc2 := NewService(mem, clock.NewFake(now))
		sub, _ := svc2.Create(ctx, "t1", CreateInput{
			Code: "s", DisplayName: "n", CustomerID: "c",
			Items: []CreateItemInput{{PlanID: "p"}}, TrialDays: 14,
			BillingTime: domain.BillingTimeCalendar,
		})
		// Now switch the service clock to earlyEnd before EndTrial.
		// NewService doesn't expose a SetClock — easiest is build a
		// fresh service over the same memStore.
		svc3 := NewService(mem, clock.NewFake(earlyEnd))
		out, err := svc3.EndTrial(ctx, "t1", sub.ID)
		if err != nil {
			t.Fatalf("EndTrial: %v", err)
		}
		if out.Status != domain.SubscriptionActive {
			t.Errorf("status: got %q, want active", out.Status)
		}
		// trial_end_at truncated to the early-end instant — historical
		// evidence "trial ended early on Dec 5", not the original Dec 13.
		if !out.TrialEndAt.Equal(earlyEnd) {
			t.Errorf("trial_end_at: got %v, want %v (truncated)", out.TrialEndAt, earlyEnd)
		}
		// Period anchors at the activation instant (Dec 5 day-snapped),
		// NOT the original trial-end anchor (Dec 13).
		wantPeriodStart := time.Date(2025, 12, 5, 0, 0, 0, 0, time.UTC)
		wantPeriodEnd := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		if !out.CurrentBillingPeriodStart.Equal(wantPeriodStart) {
			t.Errorf("period_start: got %v, want %v (reset to activation)", out.CurrentBillingPeriodStart, wantPeriodStart)
		}
		if !out.CurrentBillingPeriodEnd.Equal(wantPeriodEnd) {
			t.Errorf("period_end: got %v, want %v", out.CurrentBillingPeriodEnd, wantPeriodEnd)
		}
	})

	t.Run("ExtendTrial + calendar → period re-anchors on new trial_end", func(t *testing.T) {
		// Sub created Nov 29 + 14d trial = trial_end Dec 13, period
		// Dec 13 → Jan 1. Operator extends trial to Mar 15. Period must
		// re-anchor to Mar 15 → Apr 1 (stub) so the engine doesn't
		// silently drop the Mar 1 → Mar 15 partial-month at trial-end.
		svc := NewService(newMemStore(), clock.NewFake(now))
		sub, _ := svc.Create(ctx, "t1", CreateInput{
			Code: "s", DisplayName: "n", CustomerID: "c",
			Items: []CreateItemInput{{PlanID: "p"}}, TrialDays: 14,
			BillingTime: domain.BillingTimeCalendar,
		})
		newEnd := time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)
		out, err := svc.ExtendTrial(ctx, "t1", sub.ID, newEnd)
		if err != nil {
			t.Fatalf("ExtendTrial: %v", err)
		}
		if !out.TrialEndAt.Equal(newEnd) {
			t.Errorf("trial_end_at: got %v, want %v", out.TrialEndAt, newEnd)
		}
		// Period anchors on the new trial_end: Mar 15 → Apr 1 stub.
		wantPeriodStart := time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)
		wantPeriodEnd := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
		if !out.CurrentBillingPeriodStart.Equal(wantPeriodStart) {
			t.Errorf("period_start: got %v, want %v (re-anchored)", out.CurrentBillingPeriodStart, wantPeriodStart)
		}
		if !out.CurrentBillingPeriodEnd.Equal(wantPeriodEnd) {
			t.Errorf("period_end: got %v, want %v", out.CurrentBillingPeriodEnd, wantPeriodEnd)
		}
	})

	t.Run("Create + trial + yearly plan → first period is 1 YEAR, not 1 month (Bug #10)", func(t *testing.T) {
		// Pre-fix: yearly plan with trial got period = trial_end → trial_end+1mo
		// because the period helpers hardcoded monthly math. Customer paid
		// 1/12 of yearly fee for the first cycle (an off-cycle prorated
		// invoice), then full year invoices thereafter — 13 months of
		// service for 13/12 of yearly fee.
		svc := NewService(newMemStore(), clock.NewFake(now))
		svc.SetPlanReader(&fakePlanReader{plans: map[string]domain.Plan{
			"p": {ID: "p", BillingInterval: domain.BillingYearly},
		}})
		sub, err := svc.Create(ctx, "t1", CreateInput{
			Code: "s", DisplayName: "n", CustomerID: "c",
			Items: []CreateItemInput{{PlanID: "p"}}, TrialDays: 14,
			BillingTime: domain.BillingTimeAnniversary,
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		wantPeriodStart := time.Date(2025, 12, 13, 0, 0, 0, 0, time.UTC)
		wantPeriodEnd := time.Date(2026, 12, 13, 0, 0, 0, 0, time.UTC) // +1 YEAR
		if !sub.CurrentBillingPeriodStart.Equal(wantPeriodStart) {
			t.Errorf("period_start: got %v, want %v", sub.CurrentBillingPeriodStart, wantPeriodStart)
		}
		if !sub.CurrentBillingPeriodEnd.Equal(wantPeriodEnd) {
			t.Errorf("period_end: got %v, want %v (full year, NOT 1 month)", sub.CurrentBillingPeriodEnd, wantPeriodEnd)
		}
	})

	t.Run("Create rejects mixed monthly + yearly items (Stripe parity)", func(t *testing.T) {
		// Stripe / Lago / Chargebee all reject mixed intervals on the
		// same sub because the period anchor is per-sub and a monthly +
		// yearly mix has no coherent cycle. Velox should too.
		svc := NewService(newMemStore(), clock.NewFake(now))
		svc.SetPlanReader(&fakePlanReader{plans: map[string]domain.Plan{
			"p_monthly": {ID: "p_monthly", BillingInterval: domain.BillingMonthly},
			"p_yearly":  {ID: "p_yearly", BillingInterval: domain.BillingYearly},
		}})
		_, err := svc.Create(ctx, "t1", CreateInput{
			Code: "s", DisplayName: "n", CustomerID: "c",
			Items: []CreateItemInput{
				{PlanID: "p_monthly"},
				{PlanID: "p_yearly"},
			},
			StartNow: true,
		})
		if err == nil {
			t.Fatal("expected error for mixed monthly + yearly items")
		}
	})

	t.Run("Create allows multiple items with same interval", func(t *testing.T) {
		svc := NewService(newMemStore(), clock.NewFake(now))
		svc.SetPlanReader(&fakePlanReader{plans: map[string]domain.Plan{
			"p1": {ID: "p1", BillingInterval: domain.BillingMonthly},
			"p2": {ID: "p2", BillingInterval: domain.BillingMonthly},
		}})
		_, err := svc.Create(ctx, "t1", CreateInput{
			Code: "s", DisplayName: "n", CustomerID: "c",
			Items:    []CreateItemInput{{PlanID: "p1"}, {PlanID: "p2"}},
			StartNow: true,
		})
		if err != nil {
			t.Errorf("expected success for matching-interval items: %v", err)
		}
	})

	t.Run("UpdateItem rejects plan-change that would mix intervals on a multi-item sub", func(t *testing.T) {
		// On a multi-item sub, swapping ONE item's plan to a different
		// interval would create a mix. Reject. Single-item plan-change
		// (e.g. only item swaps monthly → yearly) is NOT covered by
		// this guard — that's a clean interval swap, not a mix, and
		// other platforms permit it.
		svc := NewService(newMemStore(), clock.NewFake(now))
		svc.SetPlanReader(&fakePlanReader{plans: map[string]domain.Plan{
			"p_monthly_a": {ID: "p_monthly_a", BillingInterval: domain.BillingMonthly},
			"p_monthly_b": {ID: "p_monthly_b", BillingInterval: domain.BillingMonthly},
			"p_yearly":    {ID: "p_yearly", BillingInterval: domain.BillingYearly},
		}})
		sub, err := svc.Create(ctx, "t1", CreateInput{
			Code: "s", DisplayName: "n", CustomerID: "c",
			Items:    []CreateItemInput{{PlanID: "p_monthly_a"}, {PlanID: "p_monthly_b"}},
			StartNow: true,
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		// Immediate plan-change to yearly while the OTHER item stays monthly → mix.
		_, err = svc.UpdateItem(ctx, "t1", sub.ID, sub.Items[0].ID, UpdateItemInput{
			NewPlanID: "p_yearly",
			Immediate: true,
		})
		if err == nil {
			t.Error("expected error on immediate plan-change that would mix intervals")
		}
		// Scheduled plan-change should also be rejected at request time
		// (Stripe parity — you can't queue a state that would be
		// invalid when it lands).
		_, err = svc.UpdateItem(ctx, "t1", sub.ID, sub.Items[0].ID, UpdateItemInput{
			NewPlanID: "p_yearly",
			Immediate: false,
		})
		if err == nil {
			t.Error("expected error on scheduled plan-change that would mix intervals")
		}
	})

	t.Run("AddItem rejects mismatched interval against existing sub", func(t *testing.T) {
		svc := NewService(newMemStore(), clock.NewFake(now))
		svc.SetPlanReader(&fakePlanReader{plans: map[string]domain.Plan{
			"p_monthly": {ID: "p_monthly", BillingInterval: domain.BillingMonthly},
			"p_yearly":  {ID: "p_yearly", BillingInterval: domain.BillingYearly},
		}})
		sub, err := svc.Create(ctx, "t1", CreateInput{
			Code: "s", DisplayName: "n", CustomerID: "c",
			Items:    []CreateItemInput{{PlanID: "p_monthly"}},
			StartNow: true,
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		_, err = svc.AddItem(ctx, "t1", sub.ID, AddItemInput{PlanID: "p_yearly"})
		if err == nil {
			t.Fatal("expected error when adding yearly item to monthly sub")
		}
	})

	t.Run("Create rejects mixed in_arrears + in_advance items", func(t *testing.T) {
		// Bill-timing mix on the same sub would emit inconsistent
		// invoice lines (arrears-close + advance-open under different
		// rules). Velox's hybrid invoice shape assumes a uniform
		// bill_timing across items, so reject at request time.
		svc := NewService(newMemStore(), clock.NewFake(now))
		svc.SetPlanReader(&fakePlanReader{plans: map[string]domain.Plan{
			"p_arrears": {ID: "p_arrears", BillingInterval: domain.BillingMonthly, BaseBillTiming: domain.BillInArrears},
			"p_advance": {ID: "p_advance", BillingInterval: domain.BillingMonthly, BaseBillTiming: domain.BillInAdvance},
		}})
		_, err := svc.Create(ctx, "t1", CreateInput{
			Code: "s", DisplayName: "n", CustomerID: "c",
			Items: []CreateItemInput{
				{PlanID: "p_arrears"},
				{PlanID: "p_advance"},
			},
			StartNow: true,
		})
		if err == nil {
			t.Fatal("expected error for mixed in_arrears + in_advance items")
		}
	})

	t.Run("UpdateItem rejects plan-swap that changes bill_timing on single-item sub (immediate=false)", func(t *testing.T) {
		// User's exact scenario: clock-pinned active sub on Plan A
		// (in_arrears $29/mo). Schedule a swap to Plan B
		// (in_advance $49/mo). The cross-bill-timing boundary swap
		// path is not exercised end-to-end, so reject at request
		// time and steer the operator to cancel + recreate.
		svc := NewService(newMemStore(), clock.NewFake(now))
		svc.SetPlanReader(&fakePlanReader{plans: map[string]domain.Plan{
			"p_arrears": {ID: "p_arrears", BillingInterval: domain.BillingMonthly, BaseBillTiming: domain.BillInArrears},
			"p_advance": {ID: "p_advance", BillingInterval: domain.BillingMonthly, BaseBillTiming: domain.BillInAdvance},
		}})
		sub, err := svc.Create(ctx, "t1", CreateInput{
			Code: "s", DisplayName: "n", CustomerID: "c",
			Items:    []CreateItemInput{{PlanID: "p_arrears"}},
			StartNow: true,
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		_, err = svc.UpdateItem(ctx, "t1", sub.ID, sub.Items[0].ID, UpdateItemInput{
			NewPlanID: "p_advance",
			Immediate: false,
		})
		if err == nil {
			t.Error("expected error on scheduled plan-swap that changes bill_timing")
		}
	})

	t.Run("UpdateItem rejects plan-swap that changes bill_timing on single-item sub (immediate=true)", func(t *testing.T) {
		// Cross-cadence (in_advance ↔ in_arrears) plan-swaps are
		// rejected — 2026-05-21 industry verification across Stripe /
		// Lago / Orb / Chargebee / Recurly / Metronome found no
		// documented support for swapping a customer between a prepaid
		// plan and a postpaid plan as an in-place operation. Lago — the
		// closest model to Velox — documents same-cadence transitions
		// only. Operator path: cancel + recreate.
		svc := NewService(newMemStore(), clock.NewFake(now))
		svc.SetPlanReader(&fakePlanReader{plans: map[string]domain.Plan{
			"p_arrears": {ID: "p_arrears", BillingInterval: domain.BillingMonthly, BaseBillTiming: domain.BillInArrears},
			"p_advance": {ID: "p_advance", BillingInterval: domain.BillingMonthly, BaseBillTiming: domain.BillInAdvance},
		}})
		sub, err := svc.Create(ctx, "t1", CreateInput{
			Code: "s", DisplayName: "n", CustomerID: "c",
			Items:    []CreateItemInput{{PlanID: "p_arrears"}},
			StartNow: true,
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		_, err = svc.UpdateItem(ctx, "t1", sub.ID, sub.Items[0].ID, UpdateItemInput{
			NewPlanID: "p_advance",
			Immediate: true,
		})
		if err == nil {
			t.Error("expected error on immediate plan-swap that changes bill_timing")
		}
	})

	t.Run("UpdateItem immediate cross-interval in_arrears truncates period to now", func(t *testing.T) {
		// Same-cadence cross-interval (monthly→yearly, both in_arrears)
		// truncates the current period to `now`. Scheduler picks up
		// next_billing_at=now, closes (oldPS, now) under OLD plan via
		// segment-aware billing, then opens a new period under NEW
		// plan's yearly interval. No synchronous bill.
		svc := NewService(newMemStore(), clock.NewFake(now))
		svc.SetPlanReader(&fakePlanReader{plans: map[string]domain.Plan{
			"p_monthly": {ID: "p_monthly", BillingInterval: domain.BillingMonthly, BaseBillTiming: domain.BillInArrears, BaseAmountCents: 2900},
			"p_yearly":  {ID: "p_yearly", BillingInterval: domain.BillingYearly, BaseBillTiming: domain.BillInArrears, BaseAmountCents: 28800},
		}})
		fb := &fakeBiller{}
		svc.SetBiller(fb)
		sub, err := svc.Create(ctx, "t1", CreateInput{
			Code: "s", DisplayName: "n", CustomerID: "c",
			Items:       []CreateItemInput{{PlanID: "p_monthly"}},
			BillingTime: domain.BillingTimeAnniversary,
			StartNow:    true,
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		// Reset the BillOnCreate counter — Create unconditionally calls
		// the biller even for in_arrears (the engine no-ops internally),
		// so fb.calls increments here. The assertion below is about the
		// swap path, not Create's call.
		fb.calls = 0
		oldPS := *sub.CurrentBillingPeriodStart
		res, err := svc.UpdateItem(ctx, "t1", sub.ID, sub.Items[0].ID, UpdateItemInput{
			NewPlanID: "p_yearly",
			Immediate: true,
		})
		if err != nil {
			t.Fatalf("immediate cross-interval in_arrears swap: %v", err)
		}
		if res.Item.PlanID != "p_yearly" {
			t.Errorf("item plan after swap: got %q want p_yearly", res.Item.PlanID)
		}
		// in_arrears path: refund orchestrator is called but is a no-op.
		// For in_arrears subs nothing was prebilled, so the engine
		// returns 0/nil. Service still invokes it (uniform shape).
		if fb.planSwapCalls != 1 {
			t.Errorf("expected 1 BillOnPlanSwapImmediate call, got %d", fb.planSwapCalls)
		}
		// in_arrears NEW plan: no synchronous BillOnCreate.
		if fb.calls != 0 {
			t.Errorf("expected 0 BillOnCreate calls (in_arrears NEW), got %d", fb.calls)
		}
		updated, err := svc.store.Get(ctx, "t1", sub.ID)
		if err != nil {
			t.Fatalf("get updated sub: %v", err)
		}
		if updated.CurrentBillingPeriodStart == nil || !updated.CurrentBillingPeriodStart.Equal(oldPS) {
			t.Errorf("period_start should be preserved: got %v want %v", updated.CurrentBillingPeriodStart, oldPS)
		}
		if updated.CurrentBillingPeriodEnd == nil || !updated.CurrentBillingPeriodEnd.Equal(now) {
			t.Errorf("period_end should be truncated to now: got %v want %v", updated.CurrentBillingPeriodEnd, now)
		}
		if updated.NextBillingAt == nil || !updated.NextBillingAt.Equal(now) {
			t.Errorf("next_billing_at should be now: got %v want %v", updated.NextBillingAt, now)
		}
	})

	t.Run("UpdateItem immediate cross-interval in_advance jumps period and bills synchronously", func(t *testing.T) {
		// Same-cadence cross-interval (yearly→monthly, both in_advance):
		// orchestrator refunds OLD unused, plan swaps, period jumps to
		// (now, NextBillingPeriodEnd for NEW monthly interval), and
		// BillOnCreate fires synchronously for the new in_advance bill.
		svc := NewService(newMemStore(), clock.NewFake(now))
		svc.SetPlanReader(&fakePlanReader{plans: map[string]domain.Plan{
			"p_yearly_adv":  {ID: "p_yearly_adv", BillingInterval: domain.BillingYearly, BaseBillTiming: domain.BillInAdvance, BaseAmountCents: 120000},
			"p_monthly_adv": {ID: "p_monthly_adv", BillingInterval: domain.BillingMonthly, BaseBillTiming: domain.BillInAdvance, BaseAmountCents: 12000},
		}})
		fb := &fakeBiller{}
		svc.SetBiller(fb)
		sub, err := svc.Create(ctx, "t1", CreateInput{
			Code: "s", DisplayName: "n", CustomerID: "c",
			Items:       []CreateItemInput{{PlanID: "p_yearly_adv"}},
			BillingTime: domain.BillingTimeAnniversary,
			StartNow:    true,
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		// Reset BillOnCreate counter — Create's day-1 invoice fires it once.
		fb.calls = 0
		res, err := svc.UpdateItem(ctx, "t1", sub.ID, sub.Items[0].ID, UpdateItemInput{
			NewPlanID: "p_monthly_adv",
			Immediate: true,
		})
		if err != nil {
			t.Fatalf("immediate cross-interval in_advance swap: %v", err)
		}
		if res.Item.PlanID != "p_monthly_adv" {
			t.Errorf("item plan after swap: got %q want p_monthly_adv", res.Item.PlanID)
		}
		if fb.planSwapCalls != 1 {
			t.Errorf("expected 1 BillOnPlanSwapImmediate call, got %d", fb.planSwapCalls)
		}
		if !fb.planSwapAt.Equal(now) {
			t.Errorf("BillOnPlanSwapImmediate at: got %v want %v", fb.planSwapAt, now)
		}
		if fb.calls != 1 {
			t.Errorf("expected 1 synchronous BillOnCreate call for new in_advance period, got %d", fb.calls)
		}
		updated, err := svc.store.Get(ctx, "t1", sub.ID)
		if err != nil {
			t.Fatalf("get updated sub: %v", err)
		}
		if updated.CurrentBillingPeriodStart == nil || !updated.CurrentBillingPeriodStart.Equal(now) {
			t.Errorf("period_start should jump to now: got %v want %v", updated.CurrentBillingPeriodStart, now)
		}
		// NextBillingPeriodEnd for monthly anniversary is now+1 month.
		expectedEnd := now.AddDate(0, 1, 0)
		if updated.CurrentBillingPeriodEnd == nil || !updated.CurrentBillingPeriodEnd.Equal(expectedEnd) {
			t.Errorf("period_end should be 1 month from now: got %v want %v", updated.CurrentBillingPeriodEnd, expectedEnd)
		}
	})

	t.Run("UpdateItem allows plan-swap when bill_timing matches", func(t *testing.T) {
		// Same-timing swap (in_arrears $29 → in_arrears $49) is the
		// vanilla supported path. Must not regress.
		svc := NewService(newMemStore(), clock.NewFake(now))
		svc.SetPlanReader(&fakePlanReader{plans: map[string]domain.Plan{
			"p_arrears_a": {ID: "p_arrears_a", BillingInterval: domain.BillingMonthly, BaseBillTiming: domain.BillInArrears},
			"p_arrears_b": {ID: "p_arrears_b", BillingInterval: domain.BillingMonthly, BaseBillTiming: domain.BillInArrears},
		}})
		sub, err := svc.Create(ctx, "t1", CreateInput{
			Code: "s", DisplayName: "n", CustomerID: "c",
			Items:    []CreateItemInput{{PlanID: "p_arrears_a"}},
			StartNow: true,
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		_, err = svc.UpdateItem(ctx, "t1", sub.ID, sub.Items[0].ID, UpdateItemInput{
			NewPlanID: "p_arrears_b",
			Immediate: false,
		})
		if err != nil {
			t.Errorf("same-timing swap should succeed, got: %v", err)
		}
	})

	t.Run("Create + start_now + yearly plan → first period is 1 year from now (no calendar stub)", func(t *testing.T) {
		// Yearly billing ignores billing_time — Stripe doesn't ship
		// calendar yearly either. Even with billing_time=calendar, a
		// yearly plan anchors anniversary-style.
		svc := NewService(newMemStore(), clock.NewFake(now))
		svc.SetPlanReader(&fakePlanReader{plans: map[string]domain.Plan{
			"p": {ID: "p", BillingInterval: domain.BillingYearly},
		}})
		sub, _ := svc.Create(ctx, "t1", CreateInput{
			Code: "s", DisplayName: "n", CustomerID: "c",
			Items: []CreateItemInput{{PlanID: "p"}}, StartNow: true,
			BillingTime: domain.BillingTimeCalendar, // intentionally calendar
		})
		wantPeriodStart := time.Date(2025, 11, 29, 0, 0, 0, 0, time.UTC)
		wantPeriodEnd := time.Date(2026, 11, 29, 0, 0, 0, 0, time.UTC) // +1 YEAR (billing_time ignored)
		if !sub.CurrentBillingPeriodStart.Equal(wantPeriodStart) {
			t.Errorf("period_start: got %v, want %v", sub.CurrentBillingPeriodStart, wantPeriodStart)
		}
		if !sub.CurrentBillingPeriodEnd.Equal(wantPeriodEnd) {
			t.Errorf("period_end: got %v, want %v (yearly forces anniversary, ignores calendar)", sub.CurrentBillingPeriodEnd, wantPeriodEnd)
		}
	})

	t.Run("ExtendTrial + anniversary → period re-anchors at new trial_end + 1mo", func(t *testing.T) {
		svc := NewService(newMemStore(), clock.NewFake(now))
		sub, _ := svc.Create(ctx, "t1", CreateInput{
			Code: "s", DisplayName: "n", CustomerID: "c",
			Items: []CreateItemInput{{PlanID: "p"}}, TrialDays: 14,
			BillingTime: domain.BillingTimeAnniversary,
		})
		newEnd := time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)
		out, _ := svc.ExtendTrial(ctx, "t1", sub.ID, newEnd)
		wantPeriodStart := time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)
		wantPeriodEnd := time.Date(2026, 4, 15, 0, 0, 0, 0, time.UTC)
		if !out.CurrentBillingPeriodStart.Equal(wantPeriodStart) {
			t.Errorf("period_start: got %v, want %v", out.CurrentBillingPeriodStart, wantPeriodStart)
		}
		if !out.CurrentBillingPeriodEnd.Equal(wantPeriodEnd) {
			t.Errorf("period_end: got %v, want %v (anniversary, NOT month-end snap)", out.CurrentBillingPeriodEnd, wantPeriodEnd)
		}
	})
}

// TestUpdateItemTx_CrossIntervalAtomic covers the atomic cross-interval swap
// (ADR-056): UpdateItemTx restructures the cycle on the caller's tx (plan write
// + watermark advance + new in_advance invoice via BillOnCreateTx), defers the
// OLD-period refund + the new invoice's finalize to post-commit, and fails loud
// (so the tx rolls back) when the in-tx bill fails — closing the silent
// revenue-drop. Control flow is verified here with the in-memory store + fake
// biller (which ignore the nil tx); true rollback against real tx semantics is
// exercised by TestUpdateItemTx_CrossIntervalSwap_RealTxRollsBackOnBillFailure
// (real-Postgres, -short=false).
func TestUpdateItemTx_CrossIntervalAtomic(t *testing.T) {
	now := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()

	newSvc := func(fb *fakeBiller) (*Service, domain.Subscription) {
		svc := NewService(newMemStore(), clock.NewFake(now))
		svc.SetPlanReader(&fakePlanReader{plans: map[string]domain.Plan{
			"p_yearly_adv":  {ID: "p_yearly_adv", BillingInterval: domain.BillingYearly, BaseBillTiming: domain.BillInAdvance, BaseAmountCents: 120000},
			"p_monthly_adv": {ID: "p_monthly_adv", BillingInterval: domain.BillingMonthly, BaseBillTiming: domain.BillInAdvance, BaseAmountCents: 12000},
			"p_monthly_arr": {ID: "p_monthly_arr", BillingInterval: domain.BillingMonthly, BaseBillTiming: domain.BillInArrears, BaseAmountCents: 2900},
		}})
		svc.SetBiller(fb)
		// The seed Create now bills the day-1 invoice IN-tx via BillOnCreateTx
		// (ADR-056). Temporarily neutralize any failure the subtest configured
		// for the SWAP so the seed create itself succeeds, then restore it.
		savedErr, savedOK := fb.createTxErr, fb.createTxOK
		fb.createTxErr, fb.createTxOK = nil, true
		sub, err := svc.Create(ctx, "t1", CreateInput{
			Code: "s", DisplayName: "n", CustomerID: "c",
			Items:       []CreateItemInput{{PlanID: "p_yearly_adv"}},
			BillingTime: domain.BillingTimeAnniversary,
			StartNow:    true,
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		fb.createTxErr, fb.createTxOK = savedErr, savedOK
		fb.calls, fb.createTxCalls, fb.finalizeCalls, fb.planSwapCalls = 0, 0, 0, 0
		return svc, sub
	}

	t.Run("in_advance swap bills new period in-tx, defers refund + finalize", func(t *testing.T) {
		fb := &fakeBiller{createTxOK: true, createTxInv: domain.Invoice{ID: "vlx_inv_swap"}}
		svc, sub := newSvc(fb)

		res, err := svc.UpdateItemTx(ctx, nil, "t1", sub.ID, sub.Items[0].ID, UpdateItemInput{
			NewPlanID: "p_monthly_adv", Immediate: true,
		})
		if err != nil {
			t.Fatalf("UpdateItemTx cross-interval in_advance: %v", err)
		}
		if !res.OrchestratedCrossAxis {
			t.Error("expected OrchestratedCrossAxis=true")
		}
		if res.Item.PlanID != "p_monthly_adv" {
			t.Errorf("item plan: got %q want p_monthly_adv", res.Item.PlanID)
		}
		if fb.createTxCalls != 1 {
			t.Errorf("BillOnCreateTx (new period in-tx) calls: got %d want 1", fb.createTxCalls)
		}
		if res.crossAxisNewInvoice == nil || res.crossAxisNewInvoice.ID != "vlx_inv_swap" {
			t.Errorf("crossAxisNewInvoice should carry the new invoice for post-commit finalize; got %+v", res.crossAxisNewInvoice)
		}
		// Refund + finalize are POST-commit — they must NOT run inside the tx.
		if fb.planSwapCalls != 0 {
			t.Errorf("refund must be deferred to post-commit; BillOnPlanSwapImmediate ran %d times in-tx", fb.planSwapCalls)
		}
		if fb.finalizeCalls != 0 {
			t.Errorf("finalize must be deferred to post-commit; FinalizeOnCreateInvoice ran %d times in-tx", fb.finalizeCalls)
		}
		updated, _ := svc.store.Get(ctx, "t1", sub.ID)
		if updated.CurrentBillingPeriodStart == nil || !updated.CurrentBillingPeriodStart.Equal(now) {
			t.Errorf("period_start should jump to now: got %v", updated.CurrentBillingPeriodStart)
		}
		if expEnd := now.AddDate(0, 1, 0); updated.CurrentBillingPeriodEnd == nil || !updated.CurrentBillingPeriodEnd.Equal(expEnd) {
			t.Errorf("period_end: got %v want %v", updated.CurrentBillingPeriodEnd, expEnd)
		}
	})

	t.Run("in-tx bill failure returns error (drives the handler tx rollback)", func(t *testing.T) {
		fb := &fakeBiller{createTxErr: fmt.Errorf("tax provider down")}
		svc, sub := newSvc(fb)

		if _, err := svc.UpdateItemTx(ctx, nil, "t1", sub.ID, sub.Items[0].ID, UpdateItemInput{
			NewPlanID: "p_monthly_adv", Immediate: true,
		}); err == nil {
			t.Fatal("expected error when the in-tx new-period bill fails (so the tx rolls back the whole swap)")
		}
		if fb.createTxCalls != 1 {
			t.Errorf("BillOnCreateTx calls: got %d want 1", fb.createTxCalls)
		}
		if fb.planSwapCalls != 0 || fb.finalizeCalls != 0 {
			t.Errorf("post-commit steps must not run on in-tx failure; planSwap=%d finalize=%d", fb.planSwapCalls, fb.finalizeCalls)
		}
	})

	t.Run("cross-cadence swap rejected", func(t *testing.T) {
		fb := &fakeBiller{}
		svc, sub := newSvc(fb) // current item is in_advance
		if _, err := svc.UpdateItemTx(ctx, nil, "t1", sub.ID, sub.Items[0].ID, UpdateItemInput{
			NewPlanID: "p_monthly_arr", Immediate: true, // in_arrears — cross-cadence
		}); err == nil {
			t.Fatal("expected cross-cadence (in_advance↔in_arrears) swap to be rejected")
		}
	})

	t.Run("multi-item swap that would mix intervals is rejected before any restructure", func(t *testing.T) {
		// Regression for the review's M1: the atomic path must keep the
		// mixed-interval guard the non-atomic path enforces. A uniform 2-item
		// monthly sub, swap one item monthly→yearly: the OTHER item stays
		// monthly, so the post-swap set mixes intervals and must be rejected —
		// otherwise the cross-interval re-anchor would bill the unchanged item a
		// full base over a yearly period (overcharge).
		fb := &fakeBiller{}
		svc := NewService(newMemStore(), clock.NewFake(now))
		svc.SetPlanReader(&fakePlanReader{plans: map[string]domain.Plan{
			"p_m1": {ID: "p_m1", BillingInterval: domain.BillingMonthly, BaseBillTiming: domain.BillInAdvance, BaseAmountCents: 1000},
			"p_m2": {ID: "p_m2", BillingInterval: domain.BillingMonthly, BaseBillTiming: domain.BillInAdvance, BaseAmountCents: 2000},
			"p_y":  {ID: "p_y", BillingInterval: domain.BillingYearly, BaseBillTiming: domain.BillInAdvance, BaseAmountCents: 100000},
		}})
		svc.SetBiller(fb)
		sub, err := svc.Create(ctx, "t1", CreateInput{
			Code: "s", DisplayName: "n", CustomerID: "c",
			Items:       []CreateItemInput{{PlanID: "p_m1"}, {PlanID: "p_m2"}},
			BillingTime: domain.BillingTimeAnniversary,
			StartNow:    true,
		})
		if err != nil {
			t.Fatalf("Create multi-item: %v", err)
		}
		if len(sub.Items) != 2 {
			t.Fatalf("expected 2 items, got %d", len(sub.Items))
		}
		fb.createTxCalls = 0 // ignore the seed create's in-tx day-1 bill; assert only the swap
		if _, err := svc.UpdateItemTx(ctx, nil, "t1", sub.ID, sub.Items[0].ID, UpdateItemInput{
			NewPlanID: "p_y", Immediate: true,
		}); err == nil {
			t.Fatal("expected mixed-interval swap to be rejected")
		}
		if fb.createTxCalls != 0 {
			t.Errorf("rejected swap must not bill a new period; BillOnCreateTx called %d", fb.createTxCalls)
		}
		if it, _ := svc.store.GetItem(ctx, "t1", sub.Items[0].ID); it.PlanID != "p_m1" {
			t.Errorf("item must be unchanged after rejection; got %q", it.PlanID)
		}
	})

	t.Run("FinalizeCrossIntervalSwap runs refund + finalize post-commit", func(t *testing.T) {
		fb := &fakeBiller{}
		svc, sub := newSvc(fb)
		subBefore, _ := svc.store.Get(ctx, "t1", sub.ID)
		svc.FinalizeCrossIntervalSwap(ctx, "t1", subBefore, ItemChangeResult{
			EffectiveAt:         now,
			crossAxisNewInvoice: &domain.Invoice{ID: "vlx_inv_swap"},
		})
		if fb.planSwapCalls != 1 {
			t.Errorf("refund: BillOnPlanSwapImmediate calls got %d want 1", fb.planSwapCalls)
		}
		if !fb.planSwapAt.Equal(now) {
			t.Errorf("refund at: got %v want %v", fb.planSwapAt, now)
		}
		if fb.finalizeCalls != 1 {
			t.Errorf("FinalizeOnCreateInvoice calls got %d want 1", fb.finalizeCalls)
		}
	})
}

// TestSetBillingThresholds covers the validation paths the service applies on
// PATCH. The Postgres store handles the actual write; integration tests
// exercise that path. These unit tests are the merge gate for the hot config
// rules — drop a check here and the API would happily accept e.g. duplicate
// item ids, which would later surface as a mid-tx integrity error.
func TestSetBillingThresholds(t *testing.T) {
	ctx := context.Background()

	newSubFixture := func(t *testing.T) (*Service, domain.Subscription) {
		t.Helper()
		svc := NewService(newMemStore(), nil)
		sub, err := svc.Create(ctx, "t1", CreateInput{
			Code: "sub-thresh", DisplayName: "Test", CustomerID: "c",
			Items: []CreateItemInput{
				{PlanID: "plan_base"},
				{PlanID: "plan_addon"},
			},
			StartNow: true,
		})
		if err != nil {
			t.Fatalf("create sub: %v", err)
		}
		return svc, sub
	}

	t.Run("omitted reset_billing_cycle defaults to FALSE (keep-anchor, Stripe parity)", func(t *testing.T) {
		// 2026-07-10 design review: the old default was true behind a
		// comment claiming Stripe parity — verified wrong (Stripe's
		// reset_billing_cycle_anchor defaults to false). Silent-default
		// operators also stay clear of the ADR-066 §5 {in_advance ×
		// reset=true} interaction.
		store := newMemStore()
		subID, _ := seedSubWithItem(t, store, "t1", "cus_1", "plan_old")
		svc := NewService(store, nil)
		sub, err := svc.SetBillingThresholds(context.Background(), "t1", subID, BillingThresholdsInput{
			AmountGTE: 100000,
		})
		if err != nil {
			t.Fatalf("SetBillingThresholds: %v", err)
		}
		if sub.BillingThresholds == nil || sub.BillingThresholds.ResetBillingCycle {
			t.Errorf("omitted reset_billing_cycle must default to false, got %+v", sub.BillingThresholds)
		}
	})

	t.Run("rejects empty body", func(t *testing.T) {
		svc, sub := newSubFixture(t)
		_, err := svc.SetBillingThresholds(ctx, "t1", sub.ID, BillingThresholdsInput{})
		if err == nil {
			t.Fatal("expected error: empty body must be rejected, not silently no-op")
		}
	})

	t.Run("rejects negative amount_gte", func(t *testing.T) {
		svc, sub := newSubFixture(t)
		_, err := svc.SetBillingThresholds(ctx, "t1", sub.ID, BillingThresholdsInput{
			AmountGTE: -1,
		})
		if err == nil {
			t.Fatal("expected error for negative amount_gte")
		}
	})

	t.Run("rejects unknown subscription", func(t *testing.T) {
		svc, _ := newSubFixture(t)
		_, err := svc.SetBillingThresholds(ctx, "t1", "vlx_sub_does_not_exist", BillingThresholdsInput{
			AmountGTE: 500000,
		})
		if err == nil {
			t.Fatal("expected ErrNotFound for unknown subscription")
		}
	})

	t.Run("rejects on canceled sub", func(t *testing.T) {
		svc, sub := newSubFixture(t)
		// Cancel the subscription so we can verify the terminal-state guard.
		if _, _, err := svc.Cancel(ctx, "t1", sub.ID); err != nil {
			t.Fatalf("setup cancel: %v", err)
		}
		_, err := svc.SetBillingThresholds(ctx, "t1", sub.ID, BillingThresholdsInput{
			AmountGTE: 500000,
		})
		if err == nil {
			t.Fatal("expected error setting threshold on canceled sub")
		}
	})

	t.Run("rejects item_thresholds with empty subscription_item_id", func(t *testing.T) {
		svc, sub := newSubFixture(t)
		_, err := svc.SetBillingThresholds(ctx, "t1", sub.ID, BillingThresholdsInput{
			ItemThresholds: []ItemThresholdInput{
				{SubscriptionItemID: "", UsageGTE: "1000"},
			},
		})
		if err == nil {
			t.Fatal("expected error for empty subscription_item_id")
		}
	})

	t.Run("rejects duplicate subscription_item_id", func(t *testing.T) {
		svc, sub := newSubFixture(t)
		fresh, _ := svc.Get(ctx, "t1", sub.ID)
		itemID := fresh.Items[0].ID
		_, err := svc.SetBillingThresholds(ctx, "t1", sub.ID, BillingThresholdsInput{
			ItemThresholds: []ItemThresholdInput{
				{SubscriptionItemID: itemID, UsageGTE: "1000"},
				{SubscriptionItemID: itemID, UsageGTE: "2000"},
			},
		})
		if err == nil {
			t.Fatal("expected error for duplicate subscription_item_id")
		}
	})

	t.Run("rejects subscription_item_id not on this subscription", func(t *testing.T) {
		svc, sub := newSubFixture(t)
		_, err := svc.SetBillingThresholds(ctx, "t1", sub.ID, BillingThresholdsInput{
			ItemThresholds: []ItemThresholdInput{
				{SubscriptionItemID: "vlx_subitem_foreign", UsageGTE: "1000"},
			},
		})
		if err == nil {
			t.Fatal("expected error for foreign subscription_item_id")
		}
	})

	t.Run("rejects non-numeric usage_gte", func(t *testing.T) {
		svc, sub := newSubFixture(t)
		fresh, _ := svc.Get(ctx, "t1", sub.ID)
		_, err := svc.SetBillingThresholds(ctx, "t1", sub.ID, BillingThresholdsInput{
			ItemThresholds: []ItemThresholdInput{
				{SubscriptionItemID: fresh.Items[0].ID, UsageGTE: "not-a-number"},
			},
		})
		if err == nil {
			t.Fatal("expected error for non-numeric usage_gte")
		}
	})

	t.Run("rejects negative usage_gte", func(t *testing.T) {
		svc, sub := newSubFixture(t)
		fresh, _ := svc.Get(ctx, "t1", sub.ID)
		_, err := svc.SetBillingThresholds(ctx, "t1", sub.ID, BillingThresholdsInput{
			ItemThresholds: []ItemThresholdInput{
				{SubscriptionItemID: fresh.Items[0].ID, UsageGTE: "-1"},
			},
		})
		if err == nil {
			t.Fatal("expected error for negative usage_gte")
		}
	})

	t.Run("accepts amount-only configuration", func(t *testing.T) {
		svc, sub := newSubFixture(t)
		updated, err := svc.SetBillingThresholds(ctx, "t1", sub.ID, BillingThresholdsInput{
			AmountGTE: 500000,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if updated.BillingThresholds == nil {
			t.Fatal("expected BillingThresholds set on updated sub")
		}
		if updated.BillingThresholds.AmountGTE != 500000 {
			t.Errorf("amount_gte: got %d, want 500000", updated.BillingThresholds.AmountGTE)
		}
		if updated.BillingThresholds.ResetBillingCycle {
			t.Error("reset_billing_cycle must default to FALSE when omitted (keep-anchor, Stripe parity — 2026-07-10)")
		}
	})

	t.Run("accepts item-only configuration", func(t *testing.T) {
		svc, sub := newSubFixture(t)
		fresh, _ := svc.Get(ctx, "t1", sub.ID)
		updated, err := svc.SetBillingThresholds(ctx, "t1", sub.ID, BillingThresholdsInput{
			ItemThresholds: []ItemThresholdInput{
				{SubscriptionItemID: fresh.Items[0].ID, UsageGTE: "1000.5"},
			},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if updated.BillingThresholds == nil {
			t.Fatal("expected BillingThresholds set on updated sub")
		}
		if len(updated.BillingThresholds.ItemThresholds) != 1 {
			t.Fatalf("expected 1 item threshold, got %d", len(updated.BillingThresholds.ItemThresholds))
		}
	})

	t.Run("accepts explicit reset_billing_cycle=false", func(t *testing.T) {
		svc, sub := newSubFixture(t)
		falseV := false
		updated, err := svc.SetBillingThresholds(ctx, "t1", sub.ID, BillingThresholdsInput{
			AmountGTE:         500000,
			ResetBillingCycle: &falseV,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if updated.BillingThresholds.ResetBillingCycle {
			t.Error("reset_billing_cycle: explicit false should be respected")
		}
	})
}

// TestClearBillingThresholds is idempotent and unconditional — the service
// delegates straight to the store. We just verify the round-trip wires
// through correctly and removing a never-set threshold is not an error.
func TestClearBillingThresholds(t *testing.T) {
	ctx := context.Background()
	svc := NewService(newMemStore(), nil)

	sub, err := svc.Create(ctx, "t1", CreateInput{
		Code: "sub-clear", DisplayName: "Test", CustomerID: "c",
		Items:    []CreateItemInput{{PlanID: "plan_base"}},
		StartNow: true,
	})
	if err != nil {
		t.Fatalf("create sub: %v", err)
	}

	t.Run("idempotent on never-set", func(t *testing.T) {
		if _, err := svc.ClearBillingThresholds(ctx, "t1", sub.ID); err != nil {
			t.Errorf("clearing never-set threshold should be a no-op, got %v", err)
		}
	})

	t.Run("clears after set", func(t *testing.T) {
		if _, err := svc.SetBillingThresholds(ctx, "t1", sub.ID, BillingThresholdsInput{
			AmountGTE: 500000,
		}); err != nil {
			t.Fatalf("set: %v", err)
		}
		updated, err := svc.ClearBillingThresholds(ctx, "t1", sub.ID)
		if err != nil {
			t.Fatalf("clear: %v", err)
		}
		if updated.BillingThresholds != nil {
			t.Errorf("BillingThresholds should be nil after clear, got %+v", updated.BillingThresholds)
		}
	})
}

// stubClockResolver returns a fixed time per entity id — lets tests
// pin the simulated "now" deterministically. Implements
// clock.Resolver so it can drop into Service.SetResolver directly.
type stubClockResolver struct {
	byCustomer map[string]time.Time
	bySub      map[string]time.Time
	byInvoice  map[string]time.Time
	// clockID is the pin the stub resolves to; defaults to "tc_stub". A
	// resolution always yields BOTH halves (instant + clock) — that pairing is
	// the invariant clock.Sim exists to hold.
	clockID string
}

func (s *stubClockResolver) clk() string {
	if s.clockID != "" {
		return s.clockID
	}
	return "tc_stub"
}

func (s *stubClockResolver) SimForCustomer(_ context.Context, _, customerID string) (clock.Sim, error) {
	if t, ok := s.byCustomer[customerID]; ok {
		return clock.Sim{At: t, TestClockID: s.clk()}, nil
	}
	return clock.Sim{}, fmt.Errorf("no stub for customer %s", customerID)
}

func (s *stubClockResolver) SimForSubscription(_ context.Context, _, subID string) (clock.Sim, error) {
	if t, ok := s.bySub[subID]; ok {
		return clock.Sim{At: t, TestClockID: s.clk()}, nil
	}
	return clock.Sim{}, fmt.Errorf("no stub for subscription %s", subID)
}

func (s *stubClockResolver) SimForInvoice(_ context.Context, _, invoiceID string) (clock.Sim, error) {
	if t, ok := s.byInvoice[invoiceID]; ok {
		return clock.Sim{At: t, TestClockID: s.clk()}, nil
	}
	return clock.Sim{}, fmt.Errorf("no stub for invoice %s", invoiceID)
}

// TestClockResolver_StampsFrozenDomain locks in the ADR-029 follow-up
// at the subscription-service layer: when the resolver is wired,
// per-sub timestamp writes (next_billing_at / period_start /
// period_end / activated_at / started_at) land in the simulated
// time domain, not wall-clock. Without this, a clock-pinned customer
// whose owning clock has drifted gets next_billing_at stranded
// outside the catchup window.
func TestClockResolver_StampsFrozenDomain(t *testing.T) {
	frozen := time.Date(2024, 4, 15, 12, 0, 0, 0, time.UTC)
	resolver := &stubClockResolver{
		byCustomer: map[string]time.Time{"cus_pinned": frozen},
	}

	t.Run("Create stamps frozen for clock-pinned customer", func(t *testing.T) {
		store := newMemStore()
		svc := NewService(store, nil)
		svc.SetResolver(resolver)

		sub, err := svc.Create(context.Background(), "t1", CreateInput{
			Code:        "sub-pinned",
			DisplayName: "Pinned",
			CustomerID:  "cus_pinned",
			Items:       []CreateItemInput{{PlanID: "pln_1"}},
			StartNow:    true,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// StartedAt + period bounds must come from the resolver, not wall-clock.
		if sub.StartedAt == nil {
			t.Fatal("started_at should be set")
		}
		// StartedAt is stamped raw from `now`, before any tenant-tz snap.
		if !sub.StartedAt.Equal(frozen) {
			t.Errorf("started_at: got %v, want %v (frozen)", *sub.StartedAt, frozen)
		}
		// Calendar + StartNow: ps = beginningOfDayIn(now, UTC); pe =
		// beginningOfMonthIn(now+1mo, UTC). Plus 1 month from 2024-04-15
		// = 2024-05-15 → first of May 2024.
		wantPeriodStart := time.Date(2024, 4, 15, 0, 0, 0, 0, time.UTC)
		wantPeriodEnd := time.Date(2024, 5, 1, 0, 0, 0, 0, time.UTC)
		if sub.CurrentBillingPeriodStart == nil || !sub.CurrentBillingPeriodStart.Equal(wantPeriodStart) {
			t.Errorf("period_start: got %v, want %v (frozen-derived)", sub.CurrentBillingPeriodStart, wantPeriodStart)
		}
		if sub.CurrentBillingPeriodEnd == nil || !sub.CurrentBillingPeriodEnd.Equal(wantPeriodEnd) {
			t.Errorf("period_end: got %v, want %v (frozen-derived)", sub.CurrentBillingPeriodEnd, wantPeriodEnd)
		}
		if sub.NextBillingAt == nil || !sub.NextBillingAt.Equal(wantPeriodEnd) {
			t.Errorf("next_billing_at: got %v, want %v (frozen-derived)", sub.NextBillingAt, wantPeriodEnd)
		}
	})
}

// TestClockResolver_NotWired confirms the wall-clock fallback shape —
// without a resolver, Create still works and stamps wall-clock.
// The previous behaviour, kept for narrow unit tests.
func TestClockResolver_NotWired(t *testing.T) {
	store := newMemStore()
	svc := NewService(store, nil) // no SetClockResolver

	before := time.Now().UTC()
	sub, err := svc.Create(context.Background(), "t1", CreateInput{
		Code:        "sub-fallback",
		DisplayName: "Fallback",
		CustomerID:  "cus_fallback",
		Items:       []CreateItemInput{{PlanID: "pln_1"}},
		StartNow:    true,
	})
	after := time.Now().UTC()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sub.StartedAt == nil {
		t.Fatal("started_at should be set")
	}
	if sub.StartedAt.Before(before.Add(-1*time.Second)) || sub.StartedAt.After(after.Add(1*time.Second)) {
		t.Errorf("started_at: got %v, want between %v and %v (wall-clock fallback)",
			*sub.StartedAt, before, after)
	}
}

// TestSubMutators_StampSimTimeOnClockPinnedSub enumerates every public
// Service mutator that touches a subscription row, and asserts each
// stamps simulated time (not wall-clock) on a clock-pinned sub. The
// pattern under test is bindForSub — every entry point must bind
// effective-now from the sub pin before delegating to the store, or
// the store's clock.Now(ctx) falls back to wall-clock and stamps the
// wrong domain (feedback_ctx_attr_audit class).
//
// Failure here means a new mutator was added without the binding —
// fix is one line: ctx = s.bindForSub(ctx, tenantID, id) before the
// store call. Add the new method to the table below to lock it in.
func TestSubMutators_StampSimTimeOnClockPinnedSub(t *testing.T) {
	frozen := time.Date(2024, 4, 15, 12, 0, 0, 0, time.UTC)

	// Helper: build a fresh service + clock-pinned sub for each case.
	// The resolver looks up by sub id; subID is the same string the
	// mem store mints (vlx_sub_1) so the resolver always resolves.
	newPinned := func(t *testing.T, status domain.SubscriptionStatus) (*Service, domain.Subscription) {
		t.Helper()
		store := newMemStore()
		svc := NewService(store, nil)
		svc.SetResolver(&stubClockResolver{
			bySub: map[string]time.Time{"vlx_sub_1": frozen},
		})
		// Seed a sub directly via the store so we sidestep Create's
		// own (correctly-bound) path — the goal is to exercise the
		// post-create mutators in isolation.
		sub, err := store.Create(context.Background(), "t1", domain.Subscription{
			Code:        "sub-pinned",
			DisplayName: "Pinned",
			CustomerID:  "cus_pinned",
			Status:      status,
			TestClockID: "tclk_1",
			Items: []domain.SubscriptionItem{
				{PlanID: "pln_1", Quantity: 1},
			},
		})
		if err != nil {
			t.Fatalf("seed sub: %v", err)
		}
		return svc, sub
	}

	// Every contract case names the mutator, the precondition status,
	// and runs the action. The assertion is uniform: UpdatedAt (or
	// CanceledAt) on the returned domain object must equal frozen.
	t.Run("Cancel", func(t *testing.T) {
		svc, sub := newPinned(t, domain.SubscriptionActive)
		out, _, err := svc.Cancel(context.Background(), "t1", sub.ID)
		if err != nil {
			t.Fatalf("Cancel: %v", err)
		}
		if out.CanceledAt == nil || !out.CanceledAt.Equal(frozen) {
			t.Errorf("canceled_at: got %v, want %v (frozen)", out.CanceledAt, frozen)
		}
		if !out.UpdatedAt.Equal(frozen) {
			t.Errorf("updated_at: got %v, want %v (frozen)", out.UpdatedAt, frozen)
		}
	})

	t.Run("AddItem", func(t *testing.T) {
		svc, sub := newPinned(t, domain.SubscriptionActive)
		item, err := svc.AddItem(context.Background(), "t1", sub.ID, AddItemInput{PlanID: "pln_2", Quantity: 1})
		if err != nil {
			t.Fatalf("AddItem: %v", err)
		}
		if !item.CreatedAt.Equal(frozen) {
			t.Errorf("created_at: got %v, want %v (frozen)", item.CreatedAt, frozen)
		}
	})

	t.Run("RemoveItem", func(t *testing.T) {
		// Two items on the sub so RemoveItem isn't rejected as last-item.
		svc, sub := newPinned(t, domain.SubscriptionActive)
		extra, err := svc.AddItem(context.Background(), "t1", sub.ID, AddItemInput{PlanID: "pln_2", Quantity: 1})
		if err != nil {
			t.Fatalf("seed extra item: %v", err)
		}
		if err := svc.RemoveItem(context.Background(), "t1", sub.ID, extra.ID); err != nil {
			t.Fatalf("RemoveItem: %v", err)
		}
		// RemoveItem returns no row — assert by reading the parent sub
		// indirectly. Since the postgres-store's hidden behavior here
		// is "DELETE" with no timestamp write, this case just locks in
		// that bindForSub was called (no panic + no wall-clock stamp on
		// any incidental write).
	})

	t.Run("CancelPendingItemChange", func(t *testing.T) {
		svc, sub := newPinned(t, domain.SubscriptionActive)
		// Schedule a plan change so there's something to cancel.
		_, err := svc.store.SetItemPendingPlan(context.Background(), "t1", sub.Items[0].ID, "pln_2", frozen.Add(72*time.Hour))
		if err != nil {
			t.Fatalf("seed pending plan: %v", err)
		}
		item, err := svc.CancelPendingItemChange(context.Background(), "t1", sub.ID, sub.Items[0].ID)
		if err != nil {
			t.Fatalf("CancelPendingItemChange: %v", err)
		}
		if !item.UpdatedAt.Equal(frozen) {
			t.Errorf("updated_at: got %v, want %v (frozen)", item.UpdatedAt, frozen)
		}
	})
}

// captureAudit lets the test inspect what Service writes when pause /
// resume auto-clears via the new scan phases.
type captureAudit struct {
	entries []capturedAuditEntry
}

// LogInTx captures like Log (nil tx fine for content-level unit tests).
func (c *captureAudit) LogInTx(_ context.Context, _ *sql.Tx, e audit.Entry) error {
	c.entries = append(c.entries, capturedAuditEntry{
		action: e.Action, resourceID: e.ResourceID, resourceType: e.ResourceType, metadata: e.Metadata,
	})
	return nil
}

type capturedAuditEntry struct {
	action       string
	resourceID   string
	metadata     map[string]any
	resourceType string
	sim          clock.Sim
	bound        bool
}

// Log captures the ctx CLOCK BINDING alongside the content. The sim axis is
// resolved by the audit writer from ctx (audit.simColumns) and nowhere else, so
// "is this row on the sim axis?" is a question about the ctx the emission ran
// under — not about metadata keys the emitter set.
func (c *captureAudit) Log(ctx context.Context, _, action, resourceType, resourceID, _ string, metadata map[string]any) error {
	sim, bound := clock.SimOf(ctx)
	c.entries = append(c.entries, capturedAuditEntry{
		action: action, resourceType: resourceType, resourceID: resourceID, metadata: metadata,
		sim: sim, bound: bound,
	})
	return nil
}

// TestProcessExpiredPauseCollections covers the new wall-clock scan
// that replaces the engine's in-cycle auto-resume gate. The gate
// only fired when a cycle was due; the scan runs every scheduler
// tick and is the Stripe-parity "resume AT resumes_at" mechanism.
func TestProcessExpiredPauseCollections(t *testing.T) {
	t.Run("resumes wall-clock subs whose resumes_at has passed", func(t *testing.T) {
		now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
		mem := newMemStore()
		svc := NewService(mem, clock.NewFake(now))
		audit := &captureAudit{}
		svc.SetAuditLogger(audit)

		// One sub paused with resumes_at in the past — should resume.
		// One sub paused with resumes_at in the future — should NOT.
		// One sub paused with no resumes_at — should NOT (indefinite).
		// Past-resumes_at is set by poking the store directly: the
		// Service.PauseCollection contract rejects past timestamps
		// (operator should never queue an already-elapsed resume), so
		// we simulate state that arose from the clock advancing past
		// a previously-future resumes_at.
		past := now.Add(-24 * time.Hour)
		future := now.Add(24 * time.Hour)
		ctx := context.Background()
		mkSub := func(code string, pc *domain.PauseCollection) string {
			s, _ := svc.Create(ctx, "t1", CreateInput{Code: code, DisplayName: code, CustomerID: "c", Items: []CreateItemInput{{PlanID: "p"}}, StartNow: true})
			_, _ = svc.Activate(ctx, "t1", s.ID)
			if pc != nil {
				row := mem.subs[s.ID]
				row.PauseCollection = pc
				mem.subs[s.ID] = row
			}
			return s.ID
		}
		dueID := mkSub("due", &domain.PauseCollection{Behavior: domain.PauseCollectionKeepAsDraft, ResumesAt: &past})
		futureID := mkSub("future", &domain.PauseCollection{Behavior: domain.PauseCollectionKeepAsDraft, ResumesAt: &future})
		indefiniteID := mkSub("indef", &domain.PauseCollection{Behavior: domain.PauseCollectionKeepAsDraft})

		audit.entries = nil // discard setup pauses
		processed, errs := svc.ProcessExpiredPauseCollections(ctx, 50)
		if len(errs) > 0 {
			t.Fatalf("unexpected errors: %v", errs)
		}
		if processed != 1 {
			t.Errorf("processed: got %d, want 1 (only the past-resumes_at sub)", processed)
		}

		got, _ := svc.Get(ctx, "t1", dueID)
		if got.PauseCollection != nil {
			t.Errorf("due sub should be resumed, got pause %+v", got.PauseCollection)
		}
		stillPaused, _ := svc.Get(ctx, "t1", futureID)
		if stillPaused.PauseCollection == nil {
			t.Error("future-resumes_at sub should still be paused")
		}
		indef, _ := svc.Get(ctx, "t1", indefiniteID)
		if indef.PauseCollection == nil {
			t.Error("indefinite-pause sub should still be paused")
		}

		// Audit row written with triggered_by=schedule.
		if len(audit.entries) != 1 {
			t.Fatalf("audit entries: got %d, want 1", len(audit.entries))
		}
		e := audit.entries[0]
		if e.metadata["action"] != "collection_resumed" {
			t.Errorf("audit action: got %v, want collection_resumed", e.metadata["action"])
		}
		if e.metadata["triggered_by"] != "schedule" {
			t.Errorf("audit triggered_by: got %v, want schedule", e.metadata["triggered_by"])
		}
		// Regression (FLOW TC9 live find): resumed_at is the CONTRACTED
		// resumes_at instant, not the tick/advance time that happened to
		// sweep it — same contractual-instant split as trial flips and
		// ADR-097 cancels.
		if got, ok := e.metadata["resumed_at"].(time.Time); !ok || !got.Equal(past) {
			t.Errorf("resumed_at: got %v, want the contracted resumes_at %v (not scan-time now)", e.metadata["resumed_at"], past)
		}
	})

	t.Run("clock path stamps the contracted resumes_at too", func(t *testing.T) {
		now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
		mem := newMemStore()
		svc := NewService(mem, clock.NewFake(now))
		audit := &captureAudit{}
		svc.SetAuditLogger(audit)

		ctx := context.Background()
		s, _ := svc.Create(ctx, "t1", CreateInput{Code: "clkpause", DisplayName: "clkpause", CustomerID: "c", Items: []CreateItemInput{{PlanID: "p"}}, StartNow: true})
		_, _ = svc.Activate(ctx, "t1", s.ID)
		contracted := time.Date(2028, 9, 3, 18, 30, 0, 0, time.UTC) // Sep 4 IST
		row := mem.subs[s.ID]
		row.TestClockID = "clk_1"
		row.PauseCollection = &domain.PauseCollection{Behavior: domain.PauseCollectionKeepAsDraft, ResumesAt: &contracted}
		mem.subs[s.ID] = row

		audit.entries = nil
		// Advance swept far past the contracted instant (Oct 2).
		frozen := time.Date(2028, 10, 2, 0, 0, 0, 0, time.UTC)
		processed, errs := svc.ProcessExpiredPauseCollectionsForClock(ctx, "t1", "clk_1", frozen)
		if len(errs) > 0 {
			t.Fatalf("unexpected errors: %v", errs)
		}
		if processed != 1 {
			t.Fatalf("processed: got %d, want 1", processed)
		}
		if len(audit.entries) != 1 {
			t.Fatalf("audit entries: got %d, want 1", len(audit.entries))
		}
		if got, ok := audit.entries[0].metadata["resumed_at"].(time.Time); !ok || !got.Equal(contracted) {
			t.Errorf("resumed_at: got %v, want the contracted %v, NOT advance-end %v", audit.entries[0].metadata["resumed_at"], contracted, frozen)
		}
		// The instant override must NOT strip the sim axis: the first fix
		// used WithEffectiveNow, which deliberately clears the clock id,
		// and the resume's audit row lost sim_effective_at/test_clock_id
		// (and its test-clock chip in the UI). Overriding the instant on a
		// KNOWN clock is WithSim's job — assert the pair survives.
		e := audit.entries[0]
		if !e.bound {
			t.Fatal("resume audit emission lost its sim binding — the row would render as wall-clock with no test-clock chip")
		}
		if !e.sim.At.Equal(contracted) {
			t.Errorf("sim instant: got %v, want the contracted %v", e.sim.At, contracted)
		}
		if e.sim.TestClockID != "clk_1" {
			t.Errorf("sim clock id: got %q, want clk_1", e.sim.TestClockID)
		}
	})
}

// CancelAtTrialEnd mirrors the store's ADR-069 CAS: trialing + observed
// trial_end + a due schedule, else ErrTrialCancelConflict. No invoice —
// callers own emissions.
func (m *memStore) CancelAtTrialEnd(ctx context.Context, tenantID, id string, observedTrialEnd time.Time) (domain.Subscription, error) {
	s, ok := m.subs[id]
	if !ok || s.TenantID != tenantID {
		return domain.Subscription{}, errs.ErrNotFound
	}
	scheduleDue := s.CancelAtPeriodEnd || (s.CancelAt != nil && s.TrialEndAt != nil && !s.CancelAt.After(*s.TrialEndAt))
	if s.Status != domain.SubscriptionTrialing || s.TrialEndAt == nil || !s.TrialEndAt.Equal(observedTrialEnd) || !scheduleDue {
		return domain.Subscription{}, ErrTrialCancelConflict
	}
	s.Status = domain.SubscriptionCanceled
	canceledAt := *s.TrialEndAt
	s.CanceledAt = &canceledAt
	s.CancelAt = nil
	s.CancelAtPeriodEnd = false
	s.UpdatedAt = clock.Now(ctx)
	m.subs[id] = s
	s.Items = m.hydrateItems(id)
	return s, nil
}

// CustomerSubPlanCurrencies (ADR-100 pin) — the fake has no plan table, so
// it reports no cross-sub currencies; the pin's cross-sub clause is covered
// by the real-Postgres integration tests.
func (m *memStore) CustomerSubPlanCurrencies(_ context.Context, _, _, _ string) ([]SubPlanCurrency, error) {
	return nil, nil
}

// TestProcessExpiredPauseCollections_AlreadyClearedAnnouncesNothing pins the
// hazard-5 root fix at the service seam: when the store reports the pause was
// already cleared (a sibling leader won the CAS), the cron must fire NO
// `subscription.collection_resumed` and write NO audit row. Mutation-verify:
// remove the ErrNotPaused branch in ProcessExpiredPauseCollections → the
// batch reports an error and, with the branch turned into a fall-through, the
// event fires.
func TestProcessExpiredPauseCollections_AlreadyClearedAnnouncesNothing(t *testing.T) {
	store := newMemStore()
	resumes := time.Now().Add(-time.Hour)
	store.subs["sub_paused"] = domain.Subscription{ID: "sub_paused", TenantID: "t1", CustomerID: "cus_1", Status: domain.SubscriptionActive,
		PauseCollection: &domain.PauseCollection{Behavior: "keep_as_draft", ResumesAt: &resumes}}
	store.clearPauseOverride = ErrNotPaused // the sibling got there first
	events := &fakeDispatcher{}
	svc := NewService(store, nil)
	svc.SetEventDispatcher(events)

	processed, errsOut := svc.ProcessExpiredPauseCollections(context.Background(), 10)
	if len(errsOut) != 0 {
		t.Fatalf("an already-cleared pause is not an error: %v", errsOut)
	}
	if processed != 0 {
		t.Fatalf("processed=%d, want 0 — nothing was resumed by this leader", processed)
	}
	if len(events.events) != 0 {
		t.Fatalf("dispatched %d events, want 0 — a second collection_resumed is the hazard-5 duplicate", len(events.events))
	}
}
