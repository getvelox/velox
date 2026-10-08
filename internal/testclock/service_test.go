package testclock

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
)

// --- Tests ---

// fakeCustomerReader records calls into the testclock service's
// CustomerReader hook. Used to assert that ListAttachedCustomers
// routes through the customer domain (not the testclock store).
type fakeCustomerReader struct {
	calls   int
	gotID   string
	returns []domain.Customer
}

func (f *fakeCustomerReader) ListByTestClockID(_ context.Context, _, clockID string) ([]domain.Customer, error) {
	f.calls++
	f.gotID = clockID
	return f.returns, nil
}

// TestListAttachedCustomers_RoutesThroughCustomerReader pins the
// architectural contract: testclock.Service must NOT read customer
// rows out of its own store. The customer package owns the
// encrypt-at-rest wrapper around display_name / email; this surface
// reads through the customer service so values arrive decrypted.
func TestListAttachedCustomers_RoutesThroughCustomerReader(t *testing.T) {
	store := newMockStore()
	store.clocks["c1"] = domain.TestClock{ID: "c1", TenantID: "t1", Status: domain.TestClockStatusReady}

	reader := &fakeCustomerReader{returns: []domain.Customer{
		{ID: "cus_1", TenantID: "t1", DisplayName: "Smoke Corp"},
	}}
	svc := NewService(store)
	svc.SetCustomerReader(reader)

	got, err := svc.ListAttachedCustomers(context.Background(), "t1", "c1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reader.calls != 1 {
		t.Fatalf("expected reader to be called once; got %d", reader.calls)
	}
	if reader.gotID != "c1" {
		t.Errorf("reader received clockID=%q; want c1", reader.gotID)
	}
	if len(got) != 1 || got[0].DisplayName != "Smoke Corp" {
		t.Errorf("unexpected return: %#v", got)
	}
}

// TestListAttachedCustomers_FailsLoudWithoutReader ensures a
// misconfigured wiring (CustomerReader never set) produces a clear
// error instead of silently returning empty — which would have
// masked the encrypted-blob bug for a different reason.
func TestListAttachedCustomers_FailsLoudWithoutReader(t *testing.T) {
	store := newMockStore()
	store.clocks["c1"] = domain.TestClock{ID: "c1", TenantID: "t1", Status: domain.TestClockStatusReady}

	svc := NewService(store) // no SetCustomerReader
	_, err := svc.ListAttachedCustomers(context.Background(), "t1", "c1")
	if err == nil {
		t.Fatal("expected error when CustomerReader is unwired; got nil")
	}
}

func TestCreate_RequiresFrozenTime(t *testing.T) {
	s := NewService(newMockStore())
	_, err := s.Create(context.Background(), "t1", CreateInput{Name: "jan 1"})
	if err == nil {
		t.Fatal("expected error for zero frozen_time")
	}
	if !errors.Is(err, errs.ErrValidation) {
		t.Errorf("want validation error, got %T: %v", err, err)
	}
}

func TestCreate_RejectsLongName(t *testing.T) {
	s := NewService(newMockStore())
	longName := make([]byte, 201)
	for i := range longName {
		longName[i] = 'x'
	}
	_, err := s.Create(context.Background(), "t1", CreateInput{
		Name:       string(longName),
		FrozenTime: time.Now(),
	})
	if err == nil {
		t.Fatal("expected error for name > 200 chars")
	}
}

func TestCreate_Succeeds(t *testing.T) {
	store := newMockStore()
	s := NewService(store)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	clk, err := s.Create(context.Background(), "t1", CreateInput{
		Name:       "fy26",
		FrozenTime: now,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if clk.Name != "fy26" {
		t.Errorf("name: got %q, want fy26", clk.Name)
	}
	if !clk.FrozenTime.Equal(now) {
		t.Errorf("frozen_time: got %v, want %v", clk.FrozenTime, now)
	}
	if clk.Status != domain.TestClockStatusReady {
		t.Errorf("status: got %q, want ready", clk.Status)
	}
}

func TestAdvance_RejectsNonReadyClock(t *testing.T) {
	store := newMockStore()
	store.clocks["c1"] = domain.TestClock{
		ID:         "c1",
		TenantID:   "t1",
		Status:     domain.TestClockStatusAdvancing,
		FrozenTime: time.Now(),
	}
	s := NewService(store)

	_, err := s.Advance(context.Background(), "t1", "c1", AdvanceInput{
		FrozenTime: time.Now().Add(24 * time.Hour),
	})
	if err == nil {
		t.Fatal("expected error advancing non-ready clock")
	}
}

func TestAdvance_RejectsBackwardTime(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	store := newMockStore()
	store.clocks["c1"] = domain.TestClock{
		ID:         "c1",
		TenantID:   "t1",
		Status:     domain.TestClockStatusReady,
		FrozenTime: now,
	}
	s := NewService(store)

	_, err := s.Advance(context.Background(), "t1", "c1", AdvanceInput{
		FrozenTime: now.Add(-time.Hour),
	})
	if err == nil {
		t.Fatal("advancing backward must be rejected")
	}
}

// TestAdvance_RejectsAdvanceOverOneYear asserts the Stripe-parity
// per-call advance window cap (ADR-028 amendment). Operators chunk
// longer simulations into successive advances; a single call may
// not shift frozen_time by more than 1 year.
func TestAdvance_RejectsAdvanceOverOneYear(t *testing.T) {
	start := time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name     string
		newTime  time.Time
		wantErr  bool
		errField string
	}{
		{"exactly one year", start.AddDate(1, 0, 0), false, ""},
		{"one second under one year", start.AddDate(1, 0, 0).Add(-time.Second), false, ""},
		{"one second over one year", start.AddDate(1, 0, 0).Add(time.Second), true, "frozen_time"},
		{"five years forward", start.AddDate(5, 0, 0), true, "frozen_time"},
		{"twenty-five years forward", start.AddDate(25, 0, 0), true, "frozen_time"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newMockStore()
			store.clocks["c1"] = domain.TestClock{
				ID: "c1", TenantID: "t1", Status: domain.TestClockStatusReady, FrozenTime: start,
			}
			s := NewService(store)
			_, err := s.Advance(context.Background(), "t1", "c1", AdvanceInput{FrozenTime: tc.newTime})

			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected validation error, got nil")
				}
				field := errs.Field(err)
				if field != tc.errField {
					t.Errorf("error field: got %q, want %q", field, tc.errField)
				}
				return
			}
			if err != nil {
				t.Fatalf("expected success, got error: %v", err)
			}
		})
	}
}

func TestAdvance_RunsBillingUntilQuiet(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	newTime := start.Add(60 * 24 * time.Hour) // 60 days forward → ~2 monthly cycles

	store := newMockStore()
	store.clocks["c1"] = domain.TestClock{
		ID: "c1", TenantID: "t1", Status: domain.TestClockStatusReady, FrozenTime: start,
	}
	// One sub attached; the runner advances its next_billing_at by 30 days
	// each call, mimicking a monthly invoice being emitted.
	sub := domain.Subscription{
		ID:          "s1",
		TenantID:    "t1",
		TestClockID: "c1",
		Status:      domain.SubscriptionActive,
	}
	nextBilling := start.Add(1 * time.Hour)
	sub.NextBillingAt = &nextBilling
	store.subsOnClock["c1"] = []domain.Subscription{sub}

	runner := &stubRunner{
		store:    store,
		clockID:  "c1",
		subID:    "s1",
		cycleDur: 30 * 24 * time.Hour,
	}
	s := NewService(store)
	s.SetBillingRunner(runner)

	clk, err := s.Advance(context.Background(), "t1", "c1", AdvanceInput{FrozenTime: newTime})
	if err != nil {
		t.Fatalf("advance failed: %v", err)
	}
	if clk.Status != domain.TestClockStatusReady {
		t.Errorf("final status: got %q, want ready", clk.Status)
	}
	if !clk.FrozenTime.Equal(newTime) {
		t.Errorf("frozen_time: got %v, want %v", clk.FrozenTime, newTime)
	}
	// Post-ADR-028: RunCatchup invokes RunCycle exactly once. The
	// engine (here, stubRunner) catches the sub up across all due
	// periods internally — no outer loop in the test-clock service.
	if runner.calls != 1 {
		t.Errorf("billing runs: got %d, want 1 (per-sub looping moved into engine)", runner.calls)
	}
	// Sub itself should now be caught up past frozen_time.
	subs := store.subsOnClock["c1"]
	if len(subs) != 1 || subs[0].NextBillingAt == nil || !subs[0].NextBillingAt.After(newTime) {
		t.Errorf("sub should be caught up: got next_billing_at=%v, frozen=%v", subs[0].NextBillingAt, newTime)
	}
}

// TestAdvance_AsyncEnqueuesAndReturnsAdvancing covers the production
// path: with a CatchupQueue wired, Advance returns synchronously
// after MarkAdvancing + Enqueue and does NOT run catchup inline.
// The clock should remain in status='advancing' on return; the
// worker is expected to drive it to 'ready' separately. ADR-015.
func TestAdvance_AsyncEnqueuesAndReturnsAdvancing(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	newTime := start.Add(60 * 24 * time.Hour)

	store := newMockStore()
	store.clocks["c1"] = domain.TestClock{
		ID: "c1", TenantID: "t1", Status: domain.TestClockStatusReady, FrozenTime: start,
	}
	store.subsOnClock["c1"] = nil

	queue := &recordingQueue{}
	s := NewService(store)
	s.SetCatchupQueue(queue)
	// Deliberately NO SetBillingRunner — the async path must not
	// invoke billing during the request handler at all.

	clk, err := s.Advance(context.Background(), "t1", "c1", AdvanceInput{FrozenTime: newTime})
	if err != nil {
		t.Fatalf("advance failed: %v", err)
	}
	if clk.Status != domain.TestClockStatusAdvancing {
		t.Errorf("status on return: got %q, want advancing (worker drives to ready async)", clk.Status)
	}
	if !clk.FrozenTime.Equal(newTime) {
		t.Errorf("frozen_time: got %v, want %v", clk.FrozenTime, newTime)
	}
	if len(queue.jobs) != 1 {
		t.Fatalf("expected 1 job enqueued, got %d", len(queue.jobs))
	}
	if queue.jobs[0].ClockID != "c1" || queue.jobs[0].TenantID != "t1" {
		t.Errorf("job identity: %+v", queue.jobs[0])
	}
}

// TestRetryAdvance_ResumesFromInternalFailure covers ADR-018:
// a clock parked at internal_failure can be retried without
// destroying simulation state. After RetryAdvance the clock is
// back in 'advancing' (or 'ready' on the sync path with no
// billing wired), the prior failure reason is cleared, and the
// catchup-job pipeline has been kicked.
func TestRetryAdvance_ResumesFromInternalFailure(t *testing.T) {
	store := newMockStore()
	store.clocks["c1"] = domain.TestClock{
		ID:                "c1",
		TenantID:          "t1",
		Status:            domain.TestClockStatusInternalFailed,
		FrozenTime:        time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
		LastFailureReason: "stripe tax: provider 503",
	}
	queue := &recordingQueue{}
	s := NewService(store)
	s.SetCatchupQueue(queue)

	clk, err := s.RetryAdvance(context.Background(), "t1", "c1")
	if err != nil {
		t.Fatalf("retry advance: %v", err)
	}
	if clk.Status != domain.TestClockStatusAdvancing {
		t.Errorf("status on return: got %q, want advancing", clk.Status)
	}
	if clk.LastFailureReason != "" {
		t.Errorf("last_failure_reason: got %q, want empty (cleared on retry)", clk.LastFailureReason)
	}
	if len(queue.jobs) != 1 || queue.jobs[0].ClockID != "c1" {
		t.Errorf("expected 1 enqueued job for c1, got %v", queue.jobs)
	}
}

// TestRetryAdvance_RefusesNonFailedClock covers the gate:
// retry from ready or advancing must 409, not silently transition.
func TestRetryAdvance_RefusesNonFailedClock(t *testing.T) {
	for _, status := range []domain.TestClockStatus{
		domain.TestClockStatusReady, domain.TestClockStatusAdvancing,
	} {
		t.Run(string(status), func(t *testing.T) {
			store := newMockStore()
			store.clocks["c1"] = domain.TestClock{
				ID: "c1", TenantID: "t1", Status: status,
				FrozenTime: time.Now(),
			}
			s := NewService(store)
			s.SetCatchupQueue(&recordingQueue{})
			_, err := s.RetryAdvance(context.Background(), "t1", "c1")
			if err == nil {
				t.Fatalf("expected error retrying from %s", status)
			}
		})
	}
}

// TestAdvance_FailurePersistsReason covers the worker-side: when
// catchup errors, MarkFailed gets called with the underlying
// error string, which is captured on the row for the dashboard.
func TestAdvance_FailurePersistsReason(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	newTime := start.Add(60 * 24 * time.Hour)

	store := newMockStore()
	store.clocks["c1"] = domain.TestClock{
		ID: "c1", TenantID: "t1", Status: domain.TestClockStatusReady, FrozenTime: start,
	}
	sub := domain.Subscription{
		ID: "s1", TenantID: "t1", TestClockID: "c1", Status: domain.SubscriptionActive,
	}
	nextBilling := start.Add(1 * time.Hour)
	sub.NextBillingAt = &nextBilling
	store.subsOnClock["c1"] = []domain.Subscription{sub}

	runner := &stubRunner{
		store: store, clockID: "c1", subID: "s1",
		err: errors.New("stripe tax: 503 service unavailable"),
	}
	s := NewService(store)
	s.SetBillingRunner(runner)
	// Sync path: no queue, so Advance runs catchup inline.
	_, err := s.Advance(context.Background(), "t1", "c1", AdvanceInput{FrozenTime: newTime})
	if err == nil {
		t.Fatal("expected advance to fail")
	}
	got := store.clocks["c1"]
	if got.Status != domain.TestClockStatusInternalFailed {
		t.Errorf("status: got %q, want internal_failure", got.Status)
	}
	if got.LastFailureReason == "" {
		t.Error("last_failure_reason should be populated after a failed catchup")
	}
}

// TestRecoverInFlight_ReEnqueuesAdvancingClocks covers the boot
// recovery path: a clock left in 'advancing' from a prior process
// (server restart mid-catchup) is re-enqueued so the worker
// resumes it rather than leaving it stuck.
func TestRecoverInFlight_ReEnqueuesAdvancingClocks(t *testing.T) {
	store := newMockStore()
	store.clocks["c1"] = domain.TestClock{
		ID: "c1", TenantID: "t1", Status: domain.TestClockStatusAdvancing,
	}
	store.clocks["c2"] = domain.TestClock{
		ID: "c2", TenantID: "t2", Status: domain.TestClockStatusReady,
	}

	queue := &recordingQueue{}
	s := NewService(store)
	s.SetCatchupQueue(queue)

	if err := s.RecoverInFlight(context.Background()); err != nil {
		t.Fatalf("recover failed: %v", err)
	}
	if len(queue.jobs) != 1 {
		t.Fatalf("expected 1 job enqueued (only c1 was advancing), got %d", len(queue.jobs))
	}
	if queue.jobs[0].ClockID != "c1" {
		t.Errorf("re-enqueued wrong clock: %s", queue.jobs[0].ClockID)
	}
}

// recordingQueue is a CatchupQueue test double that captures jobs
// in-memory for assertions instead of dispatching them.
type recordingQueue struct {
	jobs []CatchupJob
}

func (q *recordingQueue) Enqueue(job CatchupJob) error {
	q.jobs = append(q.jobs, job)
	return nil
}

func TestAdvance_BillingFailureMarksClockFailed(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store := newMockStore()
	store.clocks["c1"] = domain.TestClock{
		ID: "c1", TenantID: "t1", Status: domain.TestClockStatusReady, FrozenTime: start,
	}
	sub := domain.Subscription{
		ID: "s1", TenantID: "t1", TestClockID: "c1", Status: domain.SubscriptionActive,
	}
	nextBilling := start.Add(1 * time.Hour)
	sub.NextBillingAt = &nextBilling
	store.subsOnClock["c1"] = []domain.Subscription{sub}

	runner := &stubRunner{
		store:   store,
		clockID: "c1",
		subID:   "s1",
		err:     errors.New("boom"),
	}
	s := NewService(store)
	s.SetBillingRunner(runner)

	_, err := s.Advance(context.Background(), "t1", "c1", AdvanceInput{
		FrozenTime: start.Add(30 * 24 * time.Hour),
	})
	if err == nil {
		t.Fatal("expected error on billing failure")
	}
	if store.clocks["c1"].Status != domain.TestClockStatusInternalFailed {
		t.Errorf("clock status after failure: got %q, want internal_failure",
			store.clocks["c1"].Status)
	}
}

// TestAdvance_InjectFailureEnv asserts the manual-test injection knob:
// when VELOX_TEST_CLOCK_INJECT_FAILURE is set, runCatchupLoop fails
// with the value as the reason, the clock flips to internal_failure,
// and the env is cleared so a subsequent retry sees a clean state.
// Powers the MANUAL_TEST FLOW TC2 catchup-failure UI bullet —
// reliable reproduction without disconnecting Stripe.
func TestAdvance_InjectFailureEnv(t *testing.T) {
	t.Setenv("VELOX_TEST_CLOCK_INJECT_FAILURE", "manual UI test")

	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store := newMockStore()
	store.clocks["c1"] = domain.TestClock{
		ID: "c1", TenantID: "t1", Status: domain.TestClockStatusReady, FrozenTime: start,
	}
	sub := domain.Subscription{
		ID: "s1", TenantID: "t1", TestClockID: "c1", Status: domain.SubscriptionActive,
	}
	nextBilling := start.Add(1 * time.Hour)
	sub.NextBillingAt = &nextBilling
	store.subsOnClock["c1"] = []domain.Subscription{sub}

	// stubRunner with no error — proves the env injection runs BEFORE
	// the billing path, so a successful runner can't hide a missed
	// injection.
	runner := &stubRunner{store: store, clockID: "c1", subID: "s1"}
	s := NewService(store)
	s.SetBillingRunner(runner)

	_, err := s.Advance(context.Background(), "t1", "c1", AdvanceInput{
		FrozenTime: start.Add(30 * 24 * time.Hour),
	})
	if err == nil {
		t.Fatal("expected error from injected failure")
	}
	if !strings.Contains(err.Error(), "injected: manual UI test") {
		t.Errorf("expected reason to carry the injected text, got %q", err.Error())
	}
	if store.clocks["c1"].Status != domain.TestClockStatusInternalFailed {
		t.Errorf("clock status: got %q, want internal_failure",
			store.clocks["c1"].Status)
	}
	// One-shot: env cleared so retry would see clean state.
	if v := os.Getenv("VELOX_TEST_CLOCK_INJECT_FAILURE"); v != "" {
		t.Errorf("env should be cleared after one-shot fire, got %q", v)
	}
}

// Note: pacing-knob tests (VELOX_TEST_CLOCK_CATCHUP_DELAY_MS) moved
// to internal/billing alongside catchupDelayFromEnvBilling per
// ADR-028. The pacing now lives inside the engine's per-period
// loop, not the test-clock service's outer loop (which no longer
// exists).

// --- Mocks ---

type mockStore struct {
	clocks      map[string]domain.TestClock
	subsOnClock map[string][]domain.Subscription
}

func newMockStore() *mockStore {
	return &mockStore{
		clocks:      make(map[string]domain.TestClock),
		subsOnClock: make(map[string][]domain.Subscription),
	}
}

func (m *mockStore) Create(_ context.Context, tenantID string, clk domain.TestClock) (domain.TestClock, error) {
	clk.ID = "c_" + clk.Name
	clk.TenantID = tenantID
	clk.Status = domain.TestClockStatusReady
	clk.CreatedAt = time.Now()
	clk.UpdatedAt = clk.CreatedAt
	m.clocks[clk.ID] = clk
	return clk, nil
}

func (m *mockStore) Get(_ context.Context, _, id string) (domain.TestClock, error) {
	c, ok := m.clocks[id]
	if !ok {
		return domain.TestClock{}, errs.ErrNotFound
	}
	return c, nil
}

func (m *mockStore) List(_ context.Context, _ string) ([]domain.TestClock, error) {
	out := make([]domain.TestClock, 0, len(m.clocks))
	for _, c := range m.clocks {
		out = append(out, c)
	}
	return out, nil
}

func (m *mockStore) Delete(_ context.Context, _, id string) error {
	if _, ok := m.clocks[id]; !ok {
		return errs.ErrNotFound
	}
	delete(m.clocks, id)
	return nil
}

func (m *mockStore) MarkAdvancing(_ context.Context, _, id string, newFrozenTime time.Time) (domain.TestClock, error) {
	c, ok := m.clocks[id]
	if !ok {
		return domain.TestClock{}, errs.ErrNotFound
	}
	if c.Status != domain.TestClockStatusReady {
		return domain.TestClock{}, errs.InvalidState("not ready")
	}
	c.Status = domain.TestClockStatusAdvancing
	c.FrozenTime = newFrozenTime
	m.clocks[id] = c
	return c, nil
}

func (m *mockStore) CompleteAdvance(_ context.Context, _, id string) (domain.TestClock, error) {
	c, ok := m.clocks[id]
	if !ok {
		return domain.TestClock{}, errs.ErrNotFound
	}
	if c.Status != domain.TestClockStatusAdvancing {
		return domain.TestClock{}, errs.InvalidState("not advancing")
	}
	c.Status = domain.TestClockStatusReady
	c.LastFailureReason = ""
	m.clocks[id] = c
	return c, nil
}

func (m *mockStore) MarkFailed(_ context.Context, _, id, reason string) (domain.TestClock, error) {
	c, ok := m.clocks[id]
	if !ok {
		return domain.TestClock{}, errs.ErrNotFound
	}
	c.Status = domain.TestClockStatusInternalFailed
	c.LastFailureReason = reason
	m.clocks[id] = c
	return c, nil
}

func (m *mockStore) SaveAdvanceSummary(_ context.Context, _, id string, summaryJSON []byte) error {
	c, ok := m.clocks[id]
	if !ok {
		return errs.ErrNotFound
	}
	var s domain.AdvanceSummary
	if err := json.Unmarshal(summaryJSON, &s); err != nil {
		return err
	}
	c.LastAdvanceSummary = &s
	m.clocks[id] = c
	return nil
}

func (m *mockStore) RetryFromFailed(_ context.Context, _, id string) (domain.TestClock, error) {
	c, ok := m.clocks[id]
	if !ok {
		return domain.TestClock{}, errs.ErrNotFound
	}
	if c.Status != domain.TestClockStatusInternalFailed {
		return domain.TestClock{}, errs.InvalidState("not internal_failure")
	}
	c.Status = domain.TestClockStatusAdvancing
	c.LastFailureReason = ""
	m.clocks[id] = c
	return c, nil
}

func (m *mockStore) ListSubscriptionsOnClock(_ context.Context, _, clockID string) ([]domain.Subscription, error) {
	return m.subsOnClock[clockID], nil
}

func (m *mockStore) ListAllAdvancing(_ context.Context) ([]domain.TestClock, error) {
	var out []domain.TestClock
	for _, c := range m.clocks {
		if c.Status == domain.TestClockStatusAdvancing {
			out = append(out, c)
		}
	}
	return out, nil
}

// stubRunner mimics the post-ADR-028 billing engine: one RunCycle call
// catches the sub up across all due periods. Advances next_billing_at
// by cycleDur in a loop until it overshoots frozen_time. Returns the
// number of "invoices" generated. Track call count for tests that
// assert RunCycle is invoked exactly once per RunCatchup.
// countingRunner is a deterministic BillingRunner: RunCycleForClock returns a
// fixed invoice count (or a fixed error), and the other phases are no-ops. Used
// to assert the advance summary captures the phase counts exactly.
type countingRunner struct {
	invoices int
	err      error
}

func (r *countingRunner) RunCycleForClock(_ context.Context, _, _ string, _ int) (int, []error) {
	if r.err != nil {
		return 0, []error{r.err}
	}
	return r.invoices, nil
}
func (r *countingRunner) ScanThresholdsForClock(_ context.Context, _, _ string, _ int) (int, []error) {
	return 0, nil
}
func (r *countingRunner) RetryPendingChargesForClock(_ context.Context, _, _ string, _ int) (int, []error) {
	return 0, nil
}

// TestRunCatchup_RecordsAdvanceSummary verifies the post-advance summary
// captures the (exact) per-phase counts and the simulated span, on both the
// success transition and the partial-failure transition.
func TestRunCatchup_RecordsAdvanceSummary(t *testing.T) {
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)

	t.Run("success persists exact counts and span", func(t *testing.T) {
		store := newMockStore()
		store.clocks["c1"] = domain.TestClock{
			ID: "c1", TenantID: "t1", Status: domain.TestClockStatusAdvancing, FrozenTime: to,
		}
		s := NewService(store)
		s.SetBillingRunner(&countingRunner{invoices: 3})

		if err := s.RunCatchup(context.Background(),
			CatchupJob{TenantID: "t1", ClockID: "c1", PrevFrozenTime: from}); err != nil {
			t.Fatalf("RunCatchup: %v", err)
		}
		got := store.clocks["c1"]
		if got.Status != domain.TestClockStatusReady {
			t.Fatalf("status: got %q, want ready", got.Status)
		}
		sum := got.LastAdvanceSummary
		if sum == nil {
			t.Fatal("LastAdvanceSummary was not persisted")
		}
		if !sum.AdvancedFrom.Equal(from) || !sum.AdvancedTo.Equal(to) {
			t.Errorf("span: got %s → %s, want %s → %s", sum.AdvancedFrom, sum.AdvancedTo, from, to)
		}
		if sum.InvoicesGenerated != 3 {
			t.Errorf("InvoicesGenerated: got %d, want 3", sum.InvoicesGenerated)
		}
		if sum.HadErrors {
			t.Error("HadErrors should be false on a clean advance")
		}
	})

	t.Run("partial failure still persists the summary with HadErrors", func(t *testing.T) {
		store := newMockStore()
		store.clocks["c1"] = domain.TestClock{
			ID: "c1", TenantID: "t1", Status: domain.TestClockStatusAdvancing, FrozenTime: to,
		}
		s := NewService(store)
		s.SetBillingRunner(&countingRunner{err: errors.New("phase boom")})

		if err := s.RunCatchup(context.Background(),
			CatchupJob{TenantID: "t1", ClockID: "c1", PrevFrozenTime: from}); err == nil {
			t.Fatal("expected RunCatchup to surface the phase error")
		}
		got := store.clocks["c1"]
		if got.Status != domain.TestClockStatusInternalFailed {
			t.Fatalf("status: got %q, want internal_failure", got.Status)
		}
		if got.LastAdvanceSummary == nil {
			t.Fatal("summary must persist even on a partial-failure advance")
		}
		if !got.LastAdvanceSummary.HadErrors {
			t.Error("HadErrors should be true when a phase errored")
		}
	})
}

type stubRunner struct {
	store    *mockStore
	clockID  string
	subID    string
	cycleDur time.Duration
	calls    int
	err      error
}

// RunCycleForClock implements the new disjoint-flow contract
// RetryPendingChargesForClock — ADR-029 Phase 1 stub. The narrow tests
// in this file don't exercise auto-charge retry (no payment-method
// fixture wired); a no-op satisfies the interface contract while
// existing assertions on RunCycleForClock keep proving period-loop
// semantics. Tests that exercise the orchestration sequence end-to-end
// live in the integration-test suite where a real engine is wired.
func (r *stubRunner) RetryPendingChargesForClock(_ context.Context, _, _ string, _ int) (int, []error) {
	return 0, nil
}

// ScanThresholdsForClock — ADR-029 Phase 3 stub. Threshold-scan
// behavior is exercised by the engine's own threshold_scan tests; the
// catchup-orchestrator tests just need a no-op to satisfy the
// interface, mirroring the Phase 1 charge stub above.
func (r *stubRunner) ScanThresholdsForClock(_ context.Context, _, _ string, _ int) (int, []error) {
	return 0, nil
}

// (ADR-028). The stub mimics the post-refactor engine: one call
// catches the sub up across all due periods of the targeted clock.
func (r *stubRunner) RunCycleForClock(_ context.Context, _, clockID string, _ int) (int, []error) {
	r.calls++
	if r.err != nil {
		return 0, []error{r.err}
	}
	clk, ok := r.store.clocks[clockID]
	if !ok {
		return 0, nil
	}
	subs := r.store.subsOnClock[clockID]
	invoices := 0
	for i := range subs {
		if subs[i].ID != r.subID || subs[i].NextBillingAt == nil {
			continue
		}
		// Loop until caught up (post-ADR-028 engine semantics).
		// Cap iterations at 10000 to mirror maxPeriodsPerSubPerCall.
		for n := 0; n < 10000 && !subs[i].NextBillingAt.After(clk.FrozenTime); n++ {
			next := subs[i].NextBillingAt.Add(r.cycleDur)
			subs[i].NextBillingAt = &next
			invoices++
		}
	}
	r.store.subsOnClock[clockID] = subs
	return invoices, nil
}

// simCapturingRunner records the ctx catchup hands each billing phase, so the
// test can assert what is bound on it.
type simCapturingRunner struct {
	sims []clock.Sim
	ok   []bool
}

func (r *simCapturingRunner) capture(ctx context.Context) {
	sim, ok := clock.SimOf(ctx)
	r.sims = append(r.sims, sim)
	r.ok = append(r.ok, ok)
}

func (r *simCapturingRunner) RunCycleForClock(ctx context.Context, _, _ string, _ int) (int, []error) {
	r.capture(ctx)
	return 0, nil
}

func (r *simCapturingRunner) ScanThresholdsForClock(ctx context.Context, _, _ string, _ int) (int, []error) {
	r.capture(ctx)
	return 0, nil
}

func (r *simCapturingRunner) RetryPendingChargesForClock(ctx context.Context, _, _ string, _ int) (int, []error) {
	r.capture(ctx)
	return 0, nil
}

// TestRunCatchup_BindsClockOntoCtx is the linchpin of the audit sim axis
// (ADR-090 §5): catchup binds the CLOCK, not just its frozen instant, onto the
// ctx every phase inherits. Every audit row a clock advance produces —
// finalize, cancel, credit expiry, clawback issue, dunning — is emitted under
// this ctx, and the Logger stamps sim_effective_at / test_clock_id from it.
//
// If this binding regresses to a bare WithEffectiveNow, no emitter breaks and
// no test fails except this one: the rows keep landing, silently missing the
// clock, and the ?test_clock_id= filter starts lying by omission — the exact
// class the sim axis exists to kill. That is why it is asserted on the driver,
// not on any one emitter.
func TestRunCatchup_BindsClockOntoCtx(t *testing.T) {
	frozen := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
	store := newMockStore()
	store.clocks["c1"] = domain.TestClock{
		ID: "c1", TenantID: "t1", Status: domain.TestClockStatusAdvancing, FrozenTime: frozen,
	}
	s := NewService(store)
	runner := &simCapturingRunner{}
	s.SetBillingRunner(runner)

	if err := s.RunCatchup(context.Background(), CatchupJob{
		TenantID: "t1", ClockID: "c1", PrevFrozenTime: frozen.AddDate(0, -3, 0),
	}); err != nil {
		t.Fatalf("RunCatchup: %v", err)
	}

	if len(runner.sims) == 0 {
		t.Fatal("no billing phase ran — the binding assertion would be vacuous")
	}
	for i, sim := range runner.sims {
		if !runner.ok[i] {
			t.Fatalf("phase %d ran on a ctx with NO clock binding — audit rows from it would carry no sim axis", i)
		}
		if sim.TestClockID != "c1" {
			t.Errorf("phase %d: test_clock_id = %q, want c1", i, sim.TestClockID)
		}
		if !sim.At.Equal(frozen) {
			t.Errorf("phase %d: sim instant = %v, want frozen_time %v", i, sim.At, frozen)
		}
	}
}
