package dunning

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
)

type memStore struct {
	// Multi-policy-per-tenant memStore. Tests can mutate policies map
	// directly or via the upsert/set-default methods. defaultID points
	// at the is_default=true row (exactly one per fixture).
	policies  map[string]domain.DunningPolicy
	defaultID string
	runs      map[string]domain.InvoiceDunningRun
	events    []domain.InvoiceDunningEvent
	hookErrs  []error // errors returned by UpdateRunIfActive `then` hooks (non-vetoing)
}

func newMemStore() *memStore {
	policy := domain.DunningPolicy{
		ID: "dpol_1", TenantID: "t1", Name: "Default",
		Enabled: true, IsDefault: true,
		MaxRetryAttempts: 3, GracePeriodDays: 3,
		RetrySchedule:           []string{"72h", "120h"},
		FinalSubscriptionAction: domain.SubActionNone,
		FinalInvoiceAction:      domain.InvActionNone,
	}
	return &memStore{
		policies:  map[string]domain.DunningPolicy{"dpol_1": policy},
		defaultID: "dpol_1",
		runs:      make(map[string]domain.InvoiceDunningRun),
	}
}

func (m *memStore) GetPolicyByID(_ context.Context, _ string, id string) (domain.DunningPolicy, error) {
	p, ok := m.policies[id]
	if !ok {
		return domain.DunningPolicy{}, errs.ErrNotFound
	}
	return p, nil
}

func (m *memStore) GetDefaultPolicy(_ context.Context, _ string) (domain.DunningPolicy, error) {
	if m.defaultID == "" {
		return domain.DunningPolicy{}, errs.ErrNotFound
	}
	return m.policies[m.defaultID], nil
}

func (m *memStore) ListPolicies(_ context.Context, _ string) ([]domain.DunningPolicy, error) {
	out := make([]domain.DunningPolicy, 0, len(m.policies))
	// Default first, then insertion order (map iteration nondeterministic
	// for the non-default rows — tests that need ordering set IDs).
	if m.defaultID != "" {
		out = append(out, m.policies[m.defaultID])
	}
	for id, p := range m.policies {
		if id == m.defaultID {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

func (m *memStore) DeletePolicy(_ context.Context, _, id string) error {
	if _, ok := m.policies[id]; !ok {
		return errs.ErrNotFound
	}
	if id == m.defaultID {
		return errs.InvalidState("cannot delete default policy")
	}
	delete(m.policies, id)
	return nil
}

func (m *memStore) SetDefaultPolicy(_ context.Context, _, id string) error {
	p, ok := m.policies[id]
	if !ok {
		return errs.ErrNotFound
	}
	if cur, ok := m.policies[m.defaultID]; ok {
		cur.IsDefault = false
		m.policies[m.defaultID] = cur
	}
	p.IsDefault = true
	m.policies[id] = p
	m.defaultID = id
	return nil
}

func (m *memStore) CountCustomersOnPolicy(_ context.Context, _, _ string) (int, error) {
	// memStore has no customer table; tests that want a specific
	// count override this via a wrapper if needed.
	return 0, nil
}

func (m *memStore) UpsertPolicy(_ context.Context, tenantID string, p domain.DunningPolicy) (domain.DunningPolicy, error) {
	if p.ID == "" {
		p.ID = fmt.Sprintf("dpol_%d", len(m.policies)+1)
	}
	p.TenantID = tenantID
	if p.IsDefault {
		// Caller can't set default via Upsert (matches postgres behaviour).
		p.IsDefault = m.policies[p.ID].IsDefault
	}
	m.policies[p.ID] = p
	return p, nil
}

func (m *memStore) UpsertPolicyTx(ctx context.Context, _ *sql.Tx, tenantID string, p domain.DunningPolicy) (domain.DunningPolicy, error) {
	return m.UpsertPolicy(ctx, tenantID, p)
}

func (m *memStore) CreateRun(_ context.Context, tenantID string, run domain.InvoiceDunningRun) (domain.InvoiceDunningRun, error) {
	run.ID = fmt.Sprintf("drun_%d", len(m.runs)+1)
	run.TenantID = tenantID
	// Honor caller-provided CreatedAt to match PostgresStore.CreateRun
	// (postgres.go — it likewise falls back to now only when CreatedAt is
	// zero). Dunning Service stamps CreatedAt in the
	// effective-now domain so engine-triggered runs on a clock-pinned
	// invoice carry simulated time, not wall-clock.
	if run.CreatedAt.IsZero() {
		run.CreatedAt = time.Now().UTC()
	}
	run.UpdatedAt = run.CreatedAt
	m.runs[run.ID] = run
	return run, nil
}

func (m *memStore) GetRun(_ context.Context, _, id string) (domain.InvoiceDunningRun, error) {
	r, ok := m.runs[id]
	if !ok {
		return domain.InvoiceDunningRun{}, errs.ErrNotFound
	}
	return r, nil
}

func (m *memStore) GetActiveRunByInvoice(_ context.Context, _, invoiceID string) (domain.InvoiceDunningRun, error) {
	for _, r := range m.runs {
		// Mirror the store: exclude only resolved runs. Escalated runs are
		// still returnable so they can be resolved on out-of-band payment.
		if r.InvoiceID == invoiceID && r.State != domain.DunningResolved {
			return r, nil
		}
	}
	return domain.InvoiceDunningRun{}, errs.ErrNotFound
}

func (m *memStore) GetRunByInvoice(_ context.Context, _, invoiceID string) (domain.InvoiceDunningRun, error) {
	for _, r := range m.runs {
		if r.InvoiceID == invoiceID {
			return r, nil
		}
	}
	return domain.InvoiceDunningRun{}, errs.ErrNotFound
}

func (m *memStore) ListRuns(_ context.Context, filter RunListFilter) ([]domain.InvoiceDunningRun, int, error) {
	var result []domain.InvoiceDunningRun
	for _, r := range m.runs {
		if filter.State != "" && string(r.State) != filter.State {
			continue
		}
		result = append(result, r)
	}
	return result, len(result), nil
}

// ResolveRun mirrors the store's CAS: apply the resolved fields only if the row is
// not already resolved, and report whether this call won the transition.
func (m *memStore) ResolveRun(_ context.Context, _ string, run domain.InvoiceDunningRun) (bool, error) {
	if existing, ok := m.runs[run.ID]; ok && existing.State == domain.DunningResolved {
		return false, nil
	}
	m.runs[run.ID] = run
	return true, nil
}

// UpdateRunIfActive mirrors the guarded update: apply the run's fields only if the row
// is not already resolved, and report whether it applied.
// UpdateRunIfActive mirrors the REAL store's full predicate — state guard AND
// attempt-count CAS — rather than stubbing "true": a fake that always applies
// would keep every stale-processor test green forever (fake-fidelity rule).
func (m *memStore) UpdateRunIfActive(_ context.Context, _ string, run domain.InvoiceDunningRun, expectedAttempts int, then func(tx *sql.Tx) error) (bool, error) {
	existing, ok := m.runs[run.ID]
	if ok && existing.State == domain.DunningResolved {
		return false, nil
	}
	if ok && existing.AttemptCount != expectedAttempts {
		return false, nil
	}
	m.runs[run.ID] = run
	// Mirror the real store: the hook runs only when the write applied, and
	// its error never vetoes (recorded for tests).
	if then != nil {
		if err := then(nil); err != nil {
			m.hookErrs = append(m.hookErrs, err)
		}
	}
	return true, nil
}

// ListDueRunsForClock — mirrors ListDueRuns but compares against the
// caller-supplied frozenTime instead of the cron's wall-clock "now."
// Narrow-test version: ignores the clockID filter since memStore has
// no clock binding (callers pass single-clock fixtures). Postgres
// integration tests exercise the real SQL.
func (m *memStore) ListDueRunsForClock(_ context.Context, _, _ string, frozenTime time.Time, limit int) ([]domain.InvoiceDunningRun, error) {
	return m.ListDueRuns(context.Background(), "", frozenTime, limit)
}

func (m *memStore) ListDueRuns(_ context.Context, _ string, before time.Time, limit int) ([]domain.InvoiceDunningRun, error) {
	var result []domain.InvoiceDunningRun
	for _, r := range m.runs {
		if r.NextActionAt != nil && !r.NextActionAt.After(before) && !r.Paused &&
			r.State != domain.DunningResolved && r.State != domain.DunningEscalated {
			result = append(result, r)
		}
	}
	if len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

func (m *memStore) CreateEvent(_ context.Context, _ string, event domain.InvoiceDunningEvent) (domain.InvoiceDunningEvent, error) {
	event.ID = fmt.Sprintf("devt_%d", len(m.events)+1)
	m.events = append(m.events, event)
	return event, nil
}

func (m *memStore) ListEvents(_ context.Context, _, runID string) ([]domain.InvoiceDunningEvent, error) {
	var result []domain.InvoiceDunningEvent
	for _, e := range m.events {
		if e.RunID == runID {
			result = append(result, e)
		}
	}
	return result, nil
}

func (m *memStore) GetStats(_ context.Context, tenantID string) (Stats, error) {
	var s Stats
	for _, r := range m.runs {
		if r.TenantID != tenantID {
			continue
		}
		switch r.State {
		case "active":
			s.ActiveCount++
		case "escalated":
			s.EscalatedCount++
		case "resolved":
			s.ResolvedCount++
		}
		// At-risk: mem mock doesn't carry invoice amount_due,
		// service-layer tests don't assert the sum. Postgres impl
		// is verified via integration test.
	}
	return s, nil
}

type noopRetrier struct{}

func (n *noopRetrier) RetryPayment(_ context.Context, _, _, _ string) error { return nil }

type failingRetrier struct{}

func (f *failingRetrier) RetryPayment(_ context.Context, _, _, _ string) error {
	return fmt.Errorf("payment declined")
}

func TestStartDunning(t *testing.T) {
	store := newMemStore()
	svc := NewService(store, &noopRetrier{}, nil)
	ctx := context.Background()

	t.Run("creates run", func(t *testing.T) {
		run, err := svc.StartDunning(ctx, "t1", "inv_1", "cus_1", time.Now(), domain.DunningCausePaymentFailed)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if run.InvoiceID != "inv_1" {
			t.Errorf("invoice_id: got %q, want inv_1", run.InvoiceID)
		}
		if run.State != domain.DunningActive {
			t.Errorf("state: got %q, want scheduled", run.State)
		}
		if run.NextActionAt == nil {
			t.Error("next_action_at should be set from retry schedule")
		}
		if run.PolicyID != "dpol_1" {
			t.Errorf("policy_id: got %q, want dpol_1", run.PolicyID)
		}
	})

	t.Run("idempotent — returns existing", func(t *testing.T) {
		run2, err := svc.StartDunning(ctx, "t1", "inv_1", "cus_1", time.Now(), domain.DunningCausePaymentFailed)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// Should return the same run, not create a new one
		if len(store.runs) != 1 {
			t.Errorf("expected 1 run, got %d", len(store.runs))
		}
		if run2.InvoiceID != "inv_1" {
			t.Errorf("should return existing run")
		}
	})

	t.Run("records start event", func(t *testing.T) {
		found := false
		for _, e := range store.events {
			if e.EventType == domain.DunningEventStarted {
				found = true
			}
		}
		if !found {
			t.Error("should record dunning_started event")
		}
	})
}

func TestStartDunning_DisabledPolicy(t *testing.T) {
	store := newMemStore()
	p := store.policies[store.defaultID]
	p.Enabled = false
	store.policies[store.defaultID] = p
	svc := NewService(store, &noopRetrier{}, nil)

	_, err := svc.StartDunning(context.Background(), "t1", "inv_2", "cus_1", time.Now(), domain.DunningCausePaymentFailed)
	if err == nil {
		t.Fatal("expected error when dunning is disabled")
	}
}

// TestStartDunning_NoPolicyConfigured locks the resilience seam (Finding 2):
// a tenant with NO effective/default policy must yield a DELIBERATE-SKIP
// ErrInvalidState (the same class as disabled) — NOT a raw ErrNotFound — so the
// money-path enrollment sweep swallows it instead of poisoning the catchup.
// Mutation guard: before the fix this returned a fmt.Errorf wrapping ErrNotFound
// (errors.Is(err, ErrNotFound) == true), which the adapter propagated as a
// sweep error.
func TestStartDunning_NoPolicyConfigured(t *testing.T) {
	store := newMemStore()
	store.defaultID = "" // no default → GetDefaultPolicy returns ErrNotFound
	svc := NewService(store, &noopRetrier{}, nil)

	_, err := svc.StartDunning(context.Background(), "t1", "inv_3", "cus_1", time.Now(), domain.DunningCausePaymentFailed)
	if err == nil {
		t.Fatal("expected a deliberate-skip error when no policy is configured")
	}
	if !errors.Is(err, errs.ErrInvalidState) {
		t.Errorf("no-policy must map to ErrInvalidState (deliberate skip); got %v", err)
	}
	if errors.Is(err, errs.ErrNotFound) {
		t.Errorf("no-policy must NOT surface raw ErrNotFound (would poison the sweep); got %v", err)
	}
}

func TestProcessDueRuns(t *testing.T) {
	store := newMemStore()
	svc := NewService(store, &failingRetrier{}, nil) // Use failing retrier
	ctx := context.Background()

	// Start a run, then make it due
	run, _ := svc.StartDunning(ctx, "t1", "inv_1", "cus_1", time.Now(), domain.DunningCausePaymentFailed)
	past := time.Now().UTC().Add(-1 * time.Hour)
	run.NextActionAt = &past
	store.runs[run.ID] = run

	processed, errs := svc.ProcessDueRuns(ctx, "t1", 20)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if processed != 1 {
		t.Errorf("processed: got %d, want 1", processed)
	}

	// Verify run was updated — retry failed so state stays scheduled
	updated := store.runs[run.ID]
	if updated.AttemptCount != 1 {
		t.Errorf("attempt_count: got %d, want 1", updated.AttemptCount)
	}
	if updated.State != domain.DunningActive {
		t.Errorf("state: got %q, want scheduled", updated.State)
	}
}

// TestResolveByInvoice_ResolvesEscalatedRun guards the fix for escalated runs
// being unresolvable: when a customer pays out-of-band AFTER retries exhaust
// (state=escalated), ResolveByInvoice must still transition the run to resolved.
// Pre-fix GetActiveRunByInvoice excluded 'escalated', so the run got stuck.
func TestResolveByInvoice_ResolvesEscalatedRun(t *testing.T) {
	store := newMemStore()
	svc := NewService(store, &noopRetrier{}, nil)
	ctx := context.Background()

	run, _ := svc.StartDunning(ctx, "t1", "inv_1", "cus_1", time.Now(), domain.DunningCausePaymentFailed)
	run.State = domain.DunningEscalated // retries exhausted
	run.Resolution = domain.ResolutionRetriesExhausted
	store.runs[run.ID] = run

	if err := svc.ResolveByInvoice(ctx, "t1", "inv_1", domain.ResolutionPaymentRecovered); err != nil {
		t.Fatalf("ResolveByInvoice: %v", err)
	}

	updated := store.runs[run.ID]
	if updated.State != domain.DunningResolved {
		t.Errorf("state: got %q, want resolved — an escalated run must resolve on out-of-band payment", updated.State)
	}
	if updated.Resolution != domain.ResolutionPaymentRecovered {
		t.Errorf("resolution: got %q, want payment_recovered", updated.Resolution)
	}
}

func TestProcessDueRuns_MaxRetriesExhausted(t *testing.T) {
	store := newMemStore()
	svc := NewService(store, &noopRetrier{}, nil)
	ctx := context.Background()

	run, _ := svc.StartDunning(ctx, "t1", "inv_1", "cus_1", time.Now(), domain.DunningCausePaymentFailed)

	// Simulate max retries reached
	run.AttemptCount = 3 // equals MaxRetryAttempts
	past := time.Now().UTC().Add(-1 * time.Hour)
	run.NextActionAt = &past
	store.runs[run.ID] = run

	svc.ProcessDueRuns(ctx, "t1", 20)

	updated := store.runs[run.ID]
	if updated.State != domain.DunningEscalated {
		t.Errorf("state: got %q, want escalated (manual_review policy)", updated.State)
	}
	if updated.Resolution != domain.ResolutionRetriesExhausted {
		t.Errorf("resolution: got %q, want retries_exhausted", updated.Resolution)
	}
	if updated.ResolvedAt == nil {
		t.Error("resolved_at should be set")
	}
}

// TestProcessDueRunsForClock_LoopsUntilExhausted locks in the Gap B
// fix: a single Advance click must walk dunning state forward to
// completion when multiple retries fit inside the advance window.
// Pre-fix, the catchup orchestrator fired AT MOST one retry per
// click — operators had to click Advance N times to exhaust an
// N-retry policy. Stripe Test Clocks parity is one click → all
// time-driven actions in [old_frozen, new_frozen] fire.
//
// Scenario: cycle closes May 1 → dunning starts → policy gives
// retries at May 4, May 7, May 12 → final action at May 12. Operator
// advances to May 20 in one click.
func TestProcessDueRunsForClock_LoopsUntilExhausted(t *testing.T) {
	store := newMemStore()
	svc := NewService(store, &failingRetrier{}, nil)
	ctx := context.Background()

	cycleClose := time.Date(2024, 5, 1, 0, 0, 0, 0, time.UTC)
	frozen := time.Date(2024, 5, 20, 0, 0, 0, 0, time.UTC)

	// Start dunning at simulated cycle-close time. With grace=3d this
	// schedules retry #1 for May 4 (well inside the May 20 window).
	if _, err := svc.StartDunning(ctx, "t1", "inv_1", "cus_1", cycleClose, domain.DunningCausePaymentFailed); err != nil {
		t.Fatalf("start dunning: %v", err)
	}

	processed, errs := svc.ProcessDueRunsForClock(ctx, "t1", "clock_1", frozen, 20)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	// max_retry_attempts=3 → 3 retries fire before exhaustion.
	if processed != 3 {
		t.Errorf("processed: got %d, want 3 (all retries in one Advance)", processed)
	}

	// Run ends in terminal state. The fixture policy is (none, none)
	// in this fixture → state=escalated, resolution=retries_exhausted.
	var only domain.InvoiceDunningRun
	for _, r := range store.runs {
		only = r
	}
	if only.State != domain.DunningEscalated {
		t.Errorf("state: got %q, want escalated", only.State)
	}
	if only.AttemptCount != 3 {
		t.Errorf("attempt_count: got %d, want 3", only.AttemptCount)
	}
	if only.Resolution != domain.ResolutionRetriesExhausted {
		t.Errorf("resolution: got %q, want retries_exhausted", only.Resolution)
	}

	// Each event row must carry its own simulated instant — not all
	// pinned to advance-end frozen_time. Started at cycle_close;
	// retry #1 at cycle_close + grace (3d); retry #2 at retry#1 +
	// retry_schedule[0] (3d); retry #3 at retry#2 + retry_schedule[1]
	// (5d); escalated co-instant with retry #3 (the retry that
	// triggered the exhaustion).
	wantTimestamps := map[domain.DunningEventType]time.Time{
		domain.DunningEventStarted:   cycleClose,                                                  // May 1
		domain.DunningEventEscalated: cycleClose.Add(72*time.Hour + 72*time.Hour + 120*time.Hour), // May 12
	}
	wantRetryTimestamps := []time.Time{
		cycleClose.Add(72 * time.Hour),                              // retry #1: May 4
		cycleClose.Add(72*time.Hour + 72*time.Hour),                 // retry #2: May 7
		cycleClose.Add(72*time.Hour + 72*time.Hour + 120*time.Hour), // retry #3: May 12
	}
	retryIdx := 0
	for _, e := range store.events {
		if e.EventType == domain.DunningEventRetryAttempted {
			if retryIdx >= len(wantRetryTimestamps) {
				t.Errorf("unexpected extra retry event #%d at %v", retryIdx+1, e.CreatedAt)
				continue
			}
			if !e.CreatedAt.Equal(wantRetryTimestamps[retryIdx]) {
				t.Errorf("retry #%d CreatedAt: got %v, want %v",
					retryIdx+1, e.CreatedAt, wantRetryTimestamps[retryIdx])
			}
			retryIdx++
			continue
		}
		want, ok := wantTimestamps[e.EventType]
		if !ok {
			continue
		}
		if !e.CreatedAt.Equal(want) {
			t.Errorf("%s event CreatedAt: got %v, want %v",
				e.EventType, e.CreatedAt, want)
		}
	}
	if retryIdx != len(wantRetryTimestamps) {
		t.Errorf("retry events count: got %d, want %d", retryIdx, len(wantRetryTimestamps))
	}
}

// TestProcessDueRunsForClock_StopsWhenAllAdvancePastFrozen confirms
// the loop terminates naturally when all remaining runs have
// next_action_at past frozen_time — no spinning, no spurious work.
func TestProcessDueRunsForClock_StopsWhenAllAdvancePastFrozen(t *testing.T) {
	store := newMemStore()
	svc := NewService(store, &failingRetrier{}, nil)
	ctx := context.Background()

	cycleClose := time.Date(2024, 5, 1, 0, 0, 0, 0, time.UTC)
	// Advance only to May 5 — past May 4 retry #1 but before May 7 retry #2.
	frozen := time.Date(2024, 5, 5, 0, 0, 0, 0, time.UTC)

	if _, err := svc.StartDunning(ctx, "t1", "inv_1", "cus_1", cycleClose, domain.DunningCausePaymentFailed); err != nil {
		t.Fatalf("start dunning: %v", err)
	}

	processed, errs := svc.ProcessDueRunsForClock(ctx, "t1", "clock_1", frozen, 20)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	// Only retry #1 fires (May 4 ≤ May 5); retry #2 is May 7 > May 5.
	if processed != 1 {
		t.Errorf("processed: got %d, want 1 (retry #1 only)", processed)
	}
	var only domain.InvoiceDunningRun
	for _, r := range store.runs {
		only = r
	}
	if only.AttemptCount != 1 {
		t.Errorf("attempt_count: got %d, want 1", only.AttemptCount)
	}
	if only.State != domain.DunningActive {
		t.Errorf("state: got %q, want active (not yet escalated)", only.State)
	}
}

// recordingEmailNotifier captures the per-type email enqueues so
// catchup-walk tests can assert "warnings + escalation actually
// enqueued" instead of just "DB state is escalated."
type recordingEmailNotifier struct {
	warnings     int
	escalations  int
	paymentFails int
}

func (r *recordingEmailNotifier) SendPaymentFailed(context.Context, string, string, []string, string, string, string, string) error {
	r.paymentFails++
	return nil
}
func (r *recordingEmailNotifier) SendDunningWarning(context.Context, string, string, []string, string, string, int, int, string, string, string) error {
	r.warnings++
	return nil
}
func (r *recordingEmailNotifier) SendDunningWarningTx(ctx context.Context, _ *sql.Tx, tenantID, to string, cc []string, customerName, invoiceNumber string, attemptNumber, maxAttempts int, nextRetryDate, failureReason, publicToken string) error {
	return r.SendDunningWarning(ctx, tenantID, to, cc, customerName, invoiceNumber, attemptNumber, maxAttempts, nextRetryDate, failureReason, publicToken)
}

func (r *recordingEmailNotifier) SendDunningEscalation(context.Context, string, string, []string, string, string, domain.DunningEscalationOutcome, string) error {
	r.escalations++
	return nil
}

func (r *recordingEmailNotifier) SendDunningEscalationTx(ctx context.Context, _ *sql.Tx, tenantID, to string, cc []string, customerName, invoiceNumber string, outcome domain.DunningEscalationOutcome, publicToken string) error {
	return r.SendDunningEscalation(ctx, tenantID, to, cc, customerName, invoiceNumber, outcome, publicToken)
}

type stubCustomerEmail struct{}

func (stubCustomerEmail) GetCustomerEmail(context.Context, string, string) (string, string, []string, error) {
	return "test@velox.dev", "Test Co", nil, nil
}

// TestProcessDueRunsForClock_EnqueuesEscalationEmailOnExhaust locks in
// the ADR-035 follow-up: the dunning_escalation email MUST be enqueued
// when the final retry exhausts max attempts in a catchup pass, even
// though that's the LAST goroutine the dunning service would spawn.
//
// Pre-fix the warning + escalation enqueues ran in goroutines bound to
// the catchup ctx, which testclock/catchup.go cancels via `defer
// cancel()` the instant RunCatchup returns. The escalation, spawned
// during the final retry, lost the race and the email never landed in
// the outbox — even though the run's DB state correctly read
// state=escalated. Symptom: 5/6 of an exhausted run's emails appeared
// in Mailpit, the escalation missing.
//
// Fix: synchronous enqueue. The store-level email outbox is a fast
// INSERT and the SMTP dispatch already happens on its own long-lived
// ctx via the outbox dispatcher worker — no need for an in-service
// goroutine. This test fails if a future refactor reintroduces the
// goroutine.
func TestProcessDueRunsForClock_EnqueuesEscalationEmailOnExhaust(t *testing.T) {
	store := newMemStore()
	svc := NewService(store, &failingRetrier{}, nil)
	emails := &recordingEmailNotifier{}
	svc.SetEmailNotifier(emails)
	svc.SetCustomerEmailFetcher(stubCustomerEmail{})
	ctx := context.Background()

	cycleClose := time.Date(2024, 5, 1, 0, 0, 0, 0, time.UTC)
	frozen := time.Date(2024, 5, 20, 0, 0, 0, 0, time.UTC)
	if _, err := svc.StartDunning(ctx, "t1", "inv_1", "cus_1", cycleClose, domain.DunningCausePaymentFailed); err != nil {
		t.Fatalf("start: %v", err)
	}

	if _, errs := svc.ProcessDueRunsForClock(ctx, "t1", "clock_1", frozen, 20); len(errs) > 0 {
		t.Fatalf("processing errors: %v", errs)
	}

	// Policy fixture: max=3, retry_schedule=["72h","120h"]. Of 3 retries,
	// retries #1 and #2 fire warnings; retry #3 skips warning and triggers
	// exhaustRun → 1 escalation.
	if emails.warnings != 2 {
		t.Errorf("dunning_warning enqueues: got %d, want 2 (retries 1 + 2)", emails.warnings)
	}
	if emails.escalations != 1 {
		t.Errorf("dunning_escalation enqueues: got %d, want 1 (= the retry that exhausted)", emails.escalations)
	}
}

func TestResolveRun(t *testing.T) {
	store := newMemStore()
	svc := NewService(store, &noopRetrier{}, nil)
	ctx := context.Background()

	run, _ := svc.StartDunning(ctx, "t1", "inv_1", "cus_1", time.Now(), domain.DunningCausePaymentFailed)

	resolved, err := svc.ResolveRun(ctx, "t1", run.ID, domain.ResolutionPaymentRecovered)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resolved.State != domain.DunningResolved {
		t.Errorf("state: got %q, want resolved", resolved.State)
	}
	if resolved.Resolution != domain.ResolutionPaymentRecovered {
		t.Errorf("resolution: got %q, want payment_succeeded", resolved.Resolution)
	}
	if resolved.ResolvedAt == nil {
		t.Error("resolved_at should be set")
	}
}

// capturingUncollect records calls to MarkUncollectible so the
// cross-flow test below can assert the dunning resolve path actually
// reaches the invoice service.
type capturingUncollect struct {
	calls []string // invoice IDs
	err   error
}

func (c *capturingUncollect) MarkUncollectible(_ context.Context, _, invoiceID string) error {
	c.calls = append(c.calls, invoiceID)
	return c.err
}

// TestResolveRun_InvoiceNotCollectible_CascadesToInvoice locks in the
// cross-flow contract: picking "Write off invoice" in the resolve
// dialog must reach Invoice.MarkUncollectible so the underlying
// invoice transitions to status=uncollectible — not just update the
// dunning_runs row. Pre-fix the two flows diverged, leaving the
// invoice in finalized+failed state with every UI gate still treating
// it as live.
func TestResolveRun_InvoiceNotCollectible_CascadesToInvoice(t *testing.T) {
	store := newMemStore()
	svc := NewService(store, &noopRetrier{}, nil)
	uncollect := &capturingUncollect{}
	svc.SetInvoiceUncollectibleMarker(uncollect)
	ctx := context.Background()

	run, _ := svc.StartDunning(ctx, "t1", "inv_42", "cus_1", time.Now(), domain.DunningCausePaymentFailed)

	resolved, err := svc.ResolveRun(ctx, "t1", run.ID, domain.ResolutionInvoiceNotCollectible)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resolved.Resolution != domain.ResolutionInvoiceNotCollectible {
		t.Errorf("resolution: got %q, want invoice_not_collectible", resolved.Resolution)
	}
	if len(uncollect.calls) != 1 || uncollect.calls[0] != "inv_42" {
		t.Errorf("MarkUncollectible calls: got %v, want [inv_42]", uncollect.calls)
	}
}

// TestResolveRun_OtherResolutionsDoNotCascade ensures the cross-flow
// branch is gated exclusively to invoice_not_collectible. The other
// resolutions (payment_recovered, invoice_voided, retries_exhausted)
// must leave the invoice alone — they're audit signals, not state
// transitions on the invoice.
//
// invoice_voided is here deliberately post-0170: ResolveRun must NOT
// mark-uncollectible on it. Its void propagation lives in the handler, and a
// cross-flow branch that fired on both would write two terminal states.
func TestResolveRun_OtherResolutionsDoNotCascade(t *testing.T) {
	store := newMemStore()
	svc := NewService(store, &noopRetrier{}, nil)
	uncollect := &capturingUncollect{}
	svc.SetInvoiceUncollectibleMarker(uncollect)
	ctx := context.Background()

	for _, res := range []domain.DunningResolution{
		domain.ResolutionPaymentRecovered,
		domain.ResolutionInvoiceVoided,
		domain.ResolutionRetriesExhausted,
	} {
		run, _ := svc.StartDunning(ctx, "t1", "inv_"+string(res), "cus_1", time.Now(), domain.DunningCausePaymentFailed)
		if _, err := svc.ResolveRun(ctx, "t1", run.ID, res); err != nil {
			t.Fatalf("ResolveRun(%s): %v", res, err)
		}
	}
	if len(uncollect.calls) != 0 {
		t.Errorf("non-uncollectible resolutions leaked into MarkUncollectible: %v", uncollect.calls)
	}
}

func TestUpsertPolicy(t *testing.T) {
	store := newMemStore()
	svc := NewService(store, &noopRetrier{}, nil)
	ctx := context.Background()

	// Save-time validation (ADR-036) requires the retry_schedule to have
	// at least MaxRetryAttempts-1 entries. Defaults: max=3 → need 2.
	policy, err := svc.UpsertPolicy(ctx, "t1", domain.DunningPolicy{
		Name:          "Custom",
		Enabled:       true,
		RetrySchedule: []string{"72h", "120h"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if policy.MaxRetryAttempts != 3 {
		t.Errorf("default max_retry: got %d, want 3", policy.MaxRetryAttempts)
	}
	if policy.GracePeriodDays != 3 {
		t.Errorf("default grace_period: got %d, want 3", policy.GracePeriodDays)
	}
	// Default flipped from manual_review → pause in migration 0071 so
	// dunning-exhausted subs go into pause_collection.keep_as_draft
	// automatically instead of stacking finalized invoices each cycle.
	// ADR-112 split the axes and kept this default exactly.
	if policy.FinalSubscriptionAction != domain.SubActionPause {
		t.Errorf("default final_subscription_action: got %q, want pause", policy.FinalSubscriptionAction)
	}
	// The invoice half deliberately does NOT default to a write-off, even
	// though Recurly defaults its equivalent on: writing off asserts bad
	// debt, and a machine must not make that assertion unasked (ADR-112).
	if policy.FinalInvoiceAction != domain.InvActionNone {
		t.Errorf("default final_invoice_action: got %q, want none — defaulting to a write-off would assert bad debt on the tenant's behalf", policy.FinalInvoiceAction)
	}

	// Save-time validation rejects under-spec'd schedules — drops the
	// pre-ADR-036 "reuse last interval" silent fallback.
	_, err = svc.UpsertPolicy(ctx, "t1", domain.DunningPolicy{
		Name:             "Bad",
		Enabled:          true,
		MaxRetryAttempts: 5,
		RetrySchedule:    []string{"72h", "120h"}, // need 4, got 2
	})
	if err == nil {
		t.Error("expected error for max_retry_attempts > schedule length + 1")
	}
}

// TestUpsertPolicy_RejectsUnparseableSchedule is the regression for the
// FLOW TC5 "3d" livelock (2026-07-18): save-time validation checked only
// schedule LENGTH, so an entry time.ParseDuration can't read ("3d" — Go
// has no day unit) persisted fine and then wedged the run at retry-time:
// the parse error fired after the attempt persisted but before the
// reschedule landed, so next_action_at never advanced and every later
// tick re-attempted into the no-progress guard. Every entry must parse
// as a positive duration at save.
func TestUpsertPolicy_RejectsUnparseableSchedule(t *testing.T) {
	store := newMemStore()
	svc := NewService(store, &noopRetrier{}, nil)
	ctx := context.Background()

	cases := []struct {
		name     string
		schedule []string
		wantErr  bool
	}{
		{"day shorthand rejected", []string{"3d", "5d"}, true},
		{"garbage rejected", []string{"soon"}, true},
		{"negative rejected", []string{"-72h", "120h"}, true},
		{"zero rejected", []string{"0s", "120h"}, true},
		{"go durations accepted", []string{"72h", "120h"}, false},
		{"mixed units accepted", []string{"72h30m", "120h"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.UpsertPolicy(ctx, "t1", domain.DunningPolicy{
				Name: "P", Enabled: true, MaxRetryAttempts: 2, RetrySchedule: tc.schedule,
			})
			if tc.wantErr && err == nil {
				t.Errorf("schedule %v: expected save-time rejection, got nil", tc.schedule)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("schedule %v: unexpected error: %v", tc.schedule, err)
			}
		})
	}
}

// TestUpsertPolicyTx_ValidationParity locks the recipe-path (tx) upsert to the
// SAME save-time invariants as the API path. Pre-fix UpsertPolicyTx forwarded
// straight to the store — its comment claimed "the recipe template layer
// already validated," but recipe/parse.go validates only the terminal actions, so a
// mismatched recipe (max_retries exceeding intervals_hours) persisted fine and
// stalled its campaign at retry-time ("retry_schedule index out of bounds" in a
// background tick) instead of failing loudly at instantiate-time.
func TestUpsertPolicyTx_ValidationParity(t *testing.T) {
	store := newMemStore()
	svc := NewService(store, &noopRetrier{}, nil)
	ctx := context.Background()

	// The invariant the recipe layer does NOT check: under-length schedule.
	_, err := svc.UpsertPolicyTx(ctx, nil, "t1", domain.DunningPolicy{
		Name:             "Bad Recipe",
		Enabled:          true,
		MaxRetryAttempts: 4,
		RetrySchedule:    []string{"24h", "72h"}, // need 3, got 2
	})
	if err == nil {
		t.Error("UpsertPolicyTx must reject an under-length retry_schedule exactly like UpsertPolicy (a mismatched recipe must fail at instantiate-time, not stall a campaign at retry-time)")
	}

	// A valid recipe-shaped policy (the embedded recipes' 4/4 shape) still
	// persists, and the shared normalize applies the same defaults.
	p, err := svc.UpsertPolicyTx(ctx, nil, "t1", domain.DunningPolicy{
		Name:                    "Good Recipe",
		Enabled:                 true,
		RetrySchedule:           []string{"24h", "72h", "168h", "336h"},
		FinalSubscriptionAction: domain.SubActionPause,
		FinalInvoiceAction:      domain.InvActionNone,
		// MaxRetryAttempts / GracePeriodDays unset → shared defaults (3 / 3).
	})
	if err != nil {
		t.Fatalf("valid recipe policy rejected: %v", err)
	}
	if p.MaxRetryAttempts != 3 || p.GracePeriodDays != 3 {
		t.Errorf("tx path must apply the same defaults: max=%d grace=%d, want 3/3", p.MaxRetryAttempts, p.GracePeriodDays)
	}
}

// stubClockResolver returns a fixed time per invoice ID — lets tests
// pin the simulated "now" deterministically. Implements clock.Resolver
// (the customer / subscription methods are unused by dunning tests but
// required by the interface).
type stubClockResolver struct {
	byInvoice map[string]time.Time
	err       error
}

func (s *stubClockResolver) SimForInvoice(_ context.Context, _, invoiceID string) (clock.Sim, error) {
	if s.err != nil {
		return clock.Sim{}, s.err
	}
	if t, ok := s.byInvoice[invoiceID]; ok {
		return clock.Sim{At: t, TestClockID: "tc_stub"}, nil
	}
	return clock.Sim{}, fmt.Errorf("no stub for invoice %s", invoiceID)
}

func (s *stubClockResolver) SimForCustomer(_ context.Context, _, _ string) (clock.Sim, error) {
	return clock.Sim{}, fmt.Errorf("not used by dunning tests")
}

func (s *stubClockResolver) SimForSubscription(_ context.Context, _, _ string) (clock.Sim, error) {
	return clock.Sim{}, fmt.Errorf("not used by dunning tests")
}

// TestClockResolver_StampsFrozenDomain locks in the ADR-029 follow-up:
// when the clock resolver is wired, every per-invoice timestamp dunning
// writes (next_action_at, last_attempt_at, resolved_at, created_at)
// lands in the resolver's domain — not s.clock.Now(). Without this,
// clock-pinned runs whose stamps land in wall-clock get stranded
// against a catchup window that compares to frozen_time.
func TestClockResolver_StampsFrozenDomain(t *testing.T) {
	frozen := time.Date(2024, 2, 1, 12, 0, 0, 0, time.UTC)
	resolver := &stubClockResolver{byInvoice: map[string]time.Time{"inv_1": frozen}}

	t.Run("StartDunning stamps frozen", func(t *testing.T) {
		store := newMemStore()
		svc := NewService(store, &noopRetrier{}, nil)
		svc.SetResolver(resolver)

		run, err := svc.StartDunning(context.Background(), "t1", "inv_1", "cus_1", frozen, domain.DunningCausePaymentFailed)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !run.CreatedAt.Equal(frozen) {
			t.Errorf("created_at: got %v, want %v (frozen)", run.CreatedAt, frozen)
		}
		// next_action_at = frozen + grace (3 days default).
		if run.NextActionAt == nil {
			t.Fatal("next_action_at should be set")
		}
		wantNext := frozen.Add(72 * time.Hour)
		if !run.NextActionAt.Equal(wantNext) {
			t.Errorf("next_action_at: got %v, want %v (frozen + 3d grace)", *run.NextActionAt, wantNext)
		}
	})

	t.Run("processRun chains in simulated time off run.NextActionAt", func(t *testing.T) {
		// Post-Gap-A fix: processRun anchors LastAttemptAt + the next
		// retry's NextActionAt on run.NextActionAt (the scheduled
		// simulated moment), NOT on the resolver's "now". This keeps
		// the catchup loop walking forward in simulated time even when
		// the orchestrator's frozen_time is way past the retry's
		// scheduled instant. The resolver is now only relied on for
		// fields with no per-row simulated-time source (e.g. resolved_at).
		store := newMemStore()
		svc := NewService(store, &failingRetrier{}, nil)
		svc.SetResolver(resolver)
		ctx := context.Background()

		run, _ := svc.StartDunning(ctx, "t1", "inv_1", "cus_1", frozen, domain.DunningCausePaymentFailed)
		// Mark run as due so processRun picks it up.
		past := frozen.Add(-1 * time.Hour)
		run.NextActionAt = &past
		store.runs[run.ID] = run

		_, _ = svc.ProcessDueRuns(ctx, "t1", 20)

		updated := store.runs[run.ID]
		if updated.LastAttemptAt == nil {
			t.Fatal("last_attempt_at should be stamped")
		}
		if !updated.LastAttemptAt.Equal(past) {
			t.Errorf("last_attempt_at: got %v, want %v (= run.NextActionAt at fire time)", *updated.LastAttemptAt, past)
		}
		// Next retry chains off the previous scheduled instant.
		if updated.NextActionAt == nil {
			t.Fatal("next_action_at should be re-stamped")
		}
		wantNext := past.Add(72 * time.Hour)
		if !updated.NextActionAt.Equal(wantNext) {
			t.Errorf("next_action_at: got %v, want %v (past + 3d retry interval)", *updated.NextActionAt, wantNext)
		}
	})

	t.Run("ResolveRun stamps frozen", func(t *testing.T) {
		store := newMemStore()
		svc := NewService(store, &noopRetrier{}, nil)
		svc.SetResolver(resolver)
		ctx := context.Background()

		run, _ := svc.StartDunning(ctx, "t1", "inv_1", "cus_1", frozen, domain.DunningCausePaymentFailed)
		resolved, err := svc.ResolveRun(ctx, "t1", run.ID, domain.ResolutionPaymentRecovered)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if resolved.ResolvedAt == nil {
			t.Fatal("resolved_at should be set")
		}
		if !resolved.ResolvedAt.Equal(frozen) {
			t.Errorf("resolved_at: got %v, want %v (frozen)", *resolved.ResolvedAt, frozen)
		}
	})
}

// TestClockResolver_NotWired confirms the fallback shape — without a
// resolver, every site reads s.clock.Now() (wall-clock). The previous
// behaviour, kept so tests and narrow wirings that don't need clock
// awareness keep working.
func TestClockResolver_NotWired(t *testing.T) {
	store := newMemStore()
	svc := NewService(store, &noopRetrier{}, nil)
	// No SetClockResolver — must fall back to s.clock.Now().

	before := time.Now().UTC()
	run, err := svc.StartDunning(context.Background(), "t1", "inv_1", "cus_1", time.Now(), domain.DunningCausePaymentFailed)
	after := time.Now().UTC()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if run.CreatedAt.Before(before) || run.CreatedAt.After(after) {
		t.Errorf("created_at: got %v, want between %v and %v (wall-clock fallback)", run.CreatedAt, before, after)
	}
}

// TestClockResolver_ErrorFallback confirms a resolver error doesn't
// fail the operation — falls back to wall-clock with a warn. Failing
// the operator's retry / resolve is worse than a stamp in the wrong
// domain on a dangling-pointer row.
func TestClockResolver_ErrorFallback(t *testing.T) {
	store := newMemStore()
	svc := NewService(store, &noopRetrier{}, nil)
	svc.SetResolver(&stubClockResolver{err: fmt.Errorf("invoice gone")})

	before := time.Now().UTC()
	run, err := svc.StartDunning(context.Background(), "t1", "inv_1", "cus_1", time.Now(), domain.DunningCausePaymentFailed)
	after := time.Now().UTC()
	if err != nil {
		t.Fatalf("StartDunning should not fail on resolver error: %v", err)
	}
	if run.CreatedAt.Before(before) || run.CreatedAt.After(after) {
		t.Errorf("created_at on resolver error: got %v, want wall-clock fallback between %v and %v",
			run.CreatedAt, before, after)
	}
}

// failThenSucceedRetrier declines the first N attempts, then succeeds —
// exercises processRun's success branch mid-schedule.
type failThenSucceedRetrier struct{ failures, calls int }

func (f *failThenSucceedRetrier) RetryPayment(_ context.Context, _, _, _ string) error {
	f.calls++
	if f.calls <= f.failures {
		return fmt.Errorf("payment declined")
	}
	return nil
}

// TestProcessDueRunsForClock_RecoveryStampsContractedInstant is the
// contracted-instant regression for the retry-SUCCESS path (4th sighting
// of the class: trial flips, scheduled cancels, pause resumes, and now
// dunning resolves). Retry #1 (May 4) fails, retry #2 (May 7) succeeds,
// the operator advanced to May 20 in one click. Pre-fix: resolved_at and
// the resolved timeline row stamped May 20 (advance-end frozen_time), and
// NO retry_attempted row existed for the succeeding attempt — the May 7
// recovery instant appeared nowhere.
func TestProcessDueRunsForClock_RecoveryStampsContractedInstant(t *testing.T) {
	store := newMemStore()
	svc := NewService(store, &failThenSucceedRetrier{failures: 1}, nil)
	ctx := context.Background()

	cycleClose := time.Date(2024, 5, 1, 0, 0, 0, 0, time.UTC)
	frozen := time.Date(2024, 5, 20, 0, 0, 0, 0, time.UTC)
	retry2At := cycleClose.Add(72*time.Hour + 72*time.Hour) // grace 3d + schedule[0] 3d = May 7

	if _, err := svc.StartDunning(ctx, "t1", "inv_1", "cus_1", cycleClose, domain.DunningCausePaymentFailed); err != nil {
		t.Fatalf("start dunning: %v", err)
	}
	if _, errs := svc.ProcessDueRunsForClock(ctx, "t1", "clock_1", frozen, 20); len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}

	var run domain.InvoiceDunningRun
	for _, r := range store.runs {
		run = r
	}
	if run.State != domain.DunningResolved || run.Resolution != domain.ResolutionPaymentRecovered {
		t.Fatalf("run: state=%q resolution=%q, want resolved/payment_recovered", run.State, run.Resolution)
	}
	if run.ResolvedAt == nil || !run.ResolvedAt.Equal(retry2At) {
		t.Errorf("resolved_at: got %v, want the recovering retry's contracted instant %v (NOT advance-end %v)", run.ResolvedAt, retry2At, frozen)
	}

	var successRow, resolvedRow *domain.InvoiceDunningEvent
	for i := range store.events {
		e := store.events[i]
		if e.EventType == domain.DunningEventRetryAttempted && e.Reason == "succeeded" {
			successRow = &store.events[i]
		}
		if e.EventType == domain.DunningEventResolved {
			resolvedRow = &store.events[i]
		}
	}
	if successRow == nil {
		t.Fatal("the SUCCESSFUL retry must write its own retry_attempted row — 'resolved after N attempts' with N-1 visible retries otherwise")
	}
	if !successRow.CreatedAt.Equal(retry2At) {
		t.Errorf("success retry row: got %v, want %v", successRow.CreatedAt, retry2At)
	}
	if resolvedRow == nil {
		t.Fatal("resolved event missing")
	}
	if !resolvedRow.CreatedAt.Equal(retry2At) {
		t.Errorf("resolved row: got %v, want the contracted instant %v (NOT advance-end %v)", resolvedRow.CreatedAt, retry2At, frozen)
	}
}

// clockCapturingRetrier records the ctx time-binding at RetryPayment — the
// mechanism the paid_at anchor rides on (the charge stamps PI metadata
// velox_anchor_at from clock.SimOf(ctx)).
type clockCapturingRetrier struct {
	sims []clock.Sim
}

func (c *clockCapturingRetrier) RetryPayment(ctx context.Context, _, _, _ string) error {
	if sim, ok := clock.SimOf(ctx); ok {
		c.sims = append(c.sims, sim)
	} else {
		c.sims = append(c.sims, clock.Sim{})
	}
	return fmt.Errorf("payment declined")
}

// TestProcessDueRunsForClock_ChargeCtxCarriesAnchoredInstant: each retry's
// charge must fire under a ctx whose Sim instant is the retry's CONTRACTED
// next_action_at (clock retained) — that binding is what the payment layer
// reads into the PI's velox_anchor_at, which the settle prefers for
// paid_at. Pre-fix the ctx carried the catchup's advance-end frozen_time
// for every retry.
func TestProcessDueRunsForClock_ChargeCtxCarriesAnchoredInstant(t *testing.T) {
	store := newMemStore()
	retrier := &clockCapturingRetrier{}
	svc := NewService(store, retrier, nil)
	ctx := clock.WithSim(context.Background(), clock.Sim{At: time.Date(2024, 5, 20, 0, 0, 0, 0, time.UTC), TestClockID: "clock_1"})

	cycleClose := time.Date(2024, 5, 1, 0, 0, 0, 0, time.UTC)
	frozen := time.Date(2024, 5, 20, 0, 0, 0, 0, time.UTC)
	if _, err := svc.StartDunning(ctx, "t1", "inv_1", "cus_1", cycleClose, domain.DunningCausePaymentFailed); err != nil {
		t.Fatalf("start dunning: %v", err)
	}
	if _, errs := svc.ProcessDueRunsForClock(ctx, "t1", "clock_1", frozen, 20); len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}

	wantInstants := []time.Time{
		cycleClose.Add(72 * time.Hour),                              // retry #1: May 4
		cycleClose.Add(72*time.Hour + 72*time.Hour),                 // retry #2: May 7
		cycleClose.Add(72*time.Hour + 72*time.Hour + 120*time.Hour), // retry #3: May 12
	}
	if len(retrier.sims) != len(wantInstants) {
		t.Fatalf("retries: got %d, want %d", len(retrier.sims), len(wantInstants))
	}
	for i, want := range wantInstants {
		if !retrier.sims[i].At.Equal(want) {
			t.Errorf("retry #%d charge ctx instant: got %v, want the contracted %v (NOT advance-end %v)", i+1, retrier.sims[i].At, want, frozen)
		}
		if retrier.sims[i].TestClockID != "clock_1" {
			t.Errorf("retry #%d lost its clock: %q (WithEffectiveNow would do this — must be WithSim)", i+1, retrier.sims[i].TestClockID)
		}
	}
}

// piCarryingRetrier declines with a RetryError carrying the failed PI id —
// the adapter's post-fix shape.
type piCarryingRetrier struct{ pi string }

func (p *piCarryingRetrier) RetryPayment(_ context.Context, _, _, _ string) error {
	return &RetryError{PaymentIntentID: p.pi, Err: fmt.Errorf("payment declined")}
}

// TestProcessRun_FailedRetryStampsPIAttribution: the retry_attempted event
// carries the failed charge's PI id in metadata (the invoice timeline's
// exact-attribution key) and the Reason string is the INNER error —
// RetryError must be invisible in operator-facing text.
func TestProcessRun_FailedRetryStampsPIAttribution(t *testing.T) {
	store := newMemStore()
	svc := NewService(store, &piCarryingRetrier{pi: "pi_dun_1"}, nil)
	ctx := context.Background()

	cycleClose := time.Date(2024, 5, 1, 0, 0, 0, 0, time.UTC)
	if _, err := svc.StartDunning(ctx, "t1", "inv_1", "cus_1", cycleClose, domain.DunningCausePaymentFailed); err != nil {
		t.Fatalf("start dunning: %v", err)
	}
	if _, errs := svc.ProcessDueRunsForClock(ctx, "t1", "clock_1", cycleClose.Add(96*time.Hour), 20); len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}

	var retryEvt *domain.InvoiceDunningEvent
	for i := range store.events {
		if store.events[i].EventType == domain.DunningEventRetryAttempted {
			retryEvt = &store.events[i]
		}
	}
	if retryEvt == nil {
		t.Fatal("retry_attempted event missing")
	}
	if got, _ := retryEvt.Metadata["payment_intent_id"].(string); got != "pi_dun_1" {
		t.Errorf("event metadata payment_intent_id: got %q, want pi_dun_1", got)
	}
	if retryEvt.Reason != "payment declined" {
		t.Errorf("Reason must be the inner error, got %q (RetryError leaking into operator text)", retryEvt.Reason)
	}
}

// TestStartDunning_CauseStamped locks the write-time cause (the fix for
// the hardcoded-payment_failed lie): the caller's declared cause lands
// on BOTH the run and the dunning_started event, and an unknown cause
// is refused before any row is written.
func TestStartDunning_CauseStamped(t *testing.T) {
	store := newMemStore()
	svc := NewService(store, &noopRetrier{}, nil)
	ctx := context.Background()

	run, err := svc.StartDunning(ctx, "t1", "inv_cause", "cus_1", time.Now(), domain.DunningCauseNoPaymentMethod)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if run.Reason != string(domain.DunningCauseNoPaymentMethod) {
		t.Errorf("run reason = %q, want no_payment_method", run.Reason)
	}
	evs, _ := store.ListEvents(ctx, "t1", run.ID)
	found := false
	for _, e := range evs {
		if e.EventType == domain.DunningEventStarted {
			found = true
			if e.Reason != string(domain.DunningCauseNoPaymentMethod) {
				t.Errorf("started-event reason = %q, want no_payment_method", e.Reason)
			}
		}
	}
	if !found {
		t.Fatal("no dunning_started event written")
	}

	if _, err := svc.StartDunning(ctx, "t1", "inv_cause2", "cus_1", time.Now(), domain.DunningStartCause("wat")); err == nil {
		t.Fatal("unknown cause must be refused")
	}
}

// TestStartDunning_DisabledPolicy_SkipsSameClassAsNoPolicy pins the sibling of
// TestStartDunning_NoPolicyConfigured. Both end in "this invoice will never
// dun", so both must be the same DELIBERATE-SKIP class the enrollment sweeps
// swallow — never a raw error that poisons a catchup.
//
// Why this exists (FLOW TC5 walk, 2026-08-04): POST /v1/dunning/policies
// decodes straight into the domain struct, so omitting `enabled` creates a
// DISABLED policy — and the first policy a tenant creates is auto-promoted to
// default. An API caller therefore ends up with a policy that is
// simultaneously the tenant default and inert: no error at create, no dunning
// run, and — before the fix this test guards — no log line either, while the
// no-policy path next door logged a WARN. The dashboard never hits it (its
// form sends enabled:true) and the recipe installer sets Enabled explicitly,
// so this is the API/SDK-caller trap specifically.
func TestStartDunning_DisabledPolicy_SkipsSameClassAsNoPolicy(t *testing.T) {
	store := newMemStore()
	p := store.policies["dpol_1"]
	p.Enabled = false // the shape POST /v1/dunning/policies produces without `enabled`
	store.policies["dpol_1"] = p
	svc := NewService(store, &noopRetrier{}, nil)

	_, err := svc.StartDunning(context.Background(), "t1", "inv_disabled", "cus_1", time.Now(), domain.DunningCausePaymentFailed)
	if err == nil {
		t.Fatal("expected a deliberate-skip error when the effective policy is disabled")
	}
	if !errors.Is(err, errs.ErrInvalidState) {
		t.Errorf("disabled policy must map to ErrInvalidState (deliberate skip, swallowed by the sweeps); got %v", err)
	}
	if errors.Is(err, errs.ErrNotFound) {
		t.Errorf("disabled policy must NOT surface ErrNotFound — that is the no-policy signal; got %v", err)
	}
	if _, runErr := store.GetRunByInvoice(context.Background(), "t1", "inv_disabled"); runErr == nil {
		t.Error("a disabled policy must not create a dunning run")
	}
}
