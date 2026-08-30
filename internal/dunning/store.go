package dunning

import (
	"context"
	"database/sql"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
)

type Store interface {
	// Policy (ADR-036 campaigns model — see methods at bottom of
	// interface for the multi-policy-per-tenant resolution path).
	UpsertPolicy(ctx context.Context, tenantID string, policy domain.DunningPolicy) (domain.DunningPolicy, error)
	UpsertPolicyTx(ctx context.Context, tx *sql.Tx, tenantID string, policy domain.DunningPolicy) (domain.DunningPolicy, error)

	// Runs
	CreateRun(ctx context.Context, tenantID string, run domain.InvoiceDunningRun) (domain.InvoiceDunningRun, error)
	GetRun(ctx context.Context, tenantID, id string) (domain.InvoiceDunningRun, error)
	GetActiveRunByInvoice(ctx context.Context, tenantID, invoiceID string) (domain.InvoiceDunningRun, error)
	// GetRunByInvoice returns the (single) dunning run for an invoice
	// regardless of state. One-run-per-invoice is enforced at the DB
	// level by migration 0085's UNIQUE index. Used by StartDunning's
	// lifetime idempotency check.
	GetRunByInvoice(ctx context.Context, tenantID, invoiceID string) (domain.InvoiceDunningRun, error)
	ListRuns(ctx context.Context, filter RunListFilter) ([]domain.InvoiceDunningRun, int, error)
	// ResolveRun is the exactly-once resolve transition: it flips the run to its
	// resolved fields ONLY if it is not already resolved (CAS on
	// `state <> 'resolved'`) and reports whether THIS call won the transition.
	// The caller fires the non-idempotent side-effects (the resolved timeline row +
	// the dunning.resolved webhook) only on a win, so two resolvers racing the same
	// run (e.g. a card-settle resolve and processRun's own resolve) emit exactly one
	// dunning.resolved per recovery.
	ResolveRun(ctx context.Context, tenantID string, run domain.InvoiceDunningRun) (bool, error)
	// UpdateRunIfActive is UpdateRun guarded on `state <> 'resolved'`: it writes the
	// run's fields only if the row has NOT been concurrently resolved, and reports
	// whether it applied. processRun's retry-path writes (the pre-charge attempt
	// persist and the transient-skip rewind) use it so a concurrent card-settle
	// webhook resolve landing during the up-to-15s charge window is never clobbered
	// back to active — preserving the exactly-once dunning.resolved contract.
	UpdateRunIfActive(ctx context.Context, tenantID string, run domain.InvoiceDunningRun, expectedAttempts int) (bool, error)
	ListDueRuns(ctx context.Context, tenantID string, dueBefore time.Time, limit int) ([]domain.InvoiceDunningRun, error)

	// ListDueRunsForClock is the catchup-path counterpart to ListDueRuns.
	// ADR-029 Phase 5: clock-pinned dunning advances fire only on
	// operator Advance, against the clock's frozen_time, never on the
	// wall-clock cron tick.
	ListDueRunsForClock(ctx context.Context, tenantID, clockID string, frozenTime time.Time, limit int) ([]domain.InvoiceDunningRun, error)

	// Events
	CreateEvent(ctx context.Context, tenantID string, event domain.InvoiceDunningEvent) (domain.InvoiceDunningEvent, error)
	ListEvents(ctx context.Context, tenantID, runID string) ([]domain.InvoiceDunningEvent, error)

	// Stats returns aggregate counts per state + at-risk sum across
	// ALL runs for the tenant. Backs the dashboard's stat cards;
	// computing these client-side from a paginated /runs response
	// undercounts as soon as runs exceed the page size. Single
	// COUNT(*) GROUP BY state + SUM(amount_due_cents) LEFT JOIN
	// invoices, scoped by RLS tenant.
	GetStats(ctx context.Context, tenantID string) (Stats, error)

	// Multi-policy-per-tenant (ADR-036, campaigns model).
	//
	// GetPolicy returns the singleton tenant policy was REMOVED — every
	// caller must now resolve by id (GetPolicyByID for a known id) or
	// by tenant-default (GetDefaultPolicy for "no explicit assignment").
	// The Service's GetEffectivePolicyForCustomer wraps both.
	GetPolicyByID(ctx context.Context, tenantID, id string) (domain.DunningPolicy, error)
	GetDefaultPolicy(ctx context.Context, tenantID string) (domain.DunningPolicy, error)
	ListPolicies(ctx context.Context, tenantID string) ([]domain.DunningPolicy, error)
	DeletePolicy(ctx context.Context, tenantID, id string) error
	SetDefaultPolicy(ctx context.Context, tenantID, id string) error
	CountCustomersOnPolicy(ctx context.Context, tenantID, policyID string) (int, error)
}

// Stats is the dashboard-card payload — aggregate counts of dunning
// runs by state + total at-risk amount (sum of amount_due_cents on
// the invoices behind active+escalated runs). Computed server-side so
// the cards stay correct regardless of how many runs exist.
type Stats struct {
	ActiveCount    int   `json:"active_count"`
	EscalatedCount int   `json:"escalated_count"`
	ResolvedCount  int   `json:"resolved_count"`
	AtRiskCents    int64 `json:"at_risk_cents"`
	// Currency scopes AtRiskCents to the tenant's default currency — the
	// dashboard shows one coherent at-risk figure instead of a meaningless
	// sum across mixed-currency invoices.
	Currency string `json:"currency"`
}

type RunListFilter struct {
	TenantID   string
	InvoiceID  string
	CustomerID string
	State      string
	ActiveOnly bool
	Limit      int
	Offset     int
}
