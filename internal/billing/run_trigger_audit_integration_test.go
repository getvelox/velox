package billing

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// dueSubForTenant is dueSubFixture bound to a REAL tenant row, so the engine's
// own finalize rows (which ride sub.TenantID) satisfy audit_log's tenant FK and
// the run's trigger row lands in the same tenant the request is scoped to.
func dueSubForTenant(tenantID, id string) domain.Subscription {
	sub := dueSubFixture(id)
	sub.TenantID = tenantID
	return sub
}

// erroringRunAudit fails the post-commit trigger Log but lets the in-tx finalize
// emission (LogInTx) succeed. That split is deliberate and matters here: the
// per-invoice FINALIZE row now rides the invoice-create tx (ADR-090 shared fate),
// so failing it would roll the invoice back and the run would generate nothing —
// but this test needs the run to genuinely produce its invoice and only the
// operator TRIGGER row (written by the handler AFTER the batch commits, on its
// own tx) to fail. That trigger is the residual own-tx surface the run lives on.
type erroringRunAudit struct{ calls int }

func (e *erroringRunAudit) Log(context.Context, string, string, string, string, string, map[string]any) error {
	e.calls++
	return errors.New("injected audit failure")
}

func (e *erroringRunAudit) LogInTx(context.Context, *sql.Tx, audit.Entry) error {
	return nil
}

// TestTriggerCycleAudit_OperatorTriggerRow pins the ADR-090 emission on
// POST /v1/billing/run. The run's per-invoice finalize rows already exist, but
// they are byte-identical whether the SCHEDULER or an OPERATOR drove the cycle —
// so the operator's trigger is, today, recorded only by the middleware
// catch-all that is about to be deleted.
//
// Invariants:
//
//  1. a successful run emits exactly ONE trigger row — action=run,
//     resource_type=billing (the same frozen wire shape the catch-all wrote for
//     this route, so old and new rows unify) — carrying the invoice count the
//     run actually produced;
//  2. the trigger row is NOT one-per-invoice: it survives independently of the
//     finalize rows the engine writes on the same run;
//  3. an audit-write failure is NEVER swallowed: the run already committed, so
//     the response keeps its invoice count, but the caller is told the trigger
//     could not be recorded (206 + an error entry) rather than being handed a
//     clean 200 over a hole in the compliance log.
//
// Mutation-verify: `_ =` the Log error and (3) fails; drop the emission and (1)
// fails.
func TestTriggerCycleAudit_OperatorTriggerRow(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 60*time.Second)
	defer cancel()
	tenantID := testutil.CreateTestTenant(t, db, "Billing Run Trigger Audit")
	logger := audit.NewLogger(db)

	runRows := func(t *testing.T) []domain.AuditEntry {
		t.Helper()
		rows, _, err := logger.Query(ctx, tenantID, audit.QueryFilter{ResourceType: "billing"})
		if err != nil {
			t.Fatalf("query audit: %v", err)
		}
		return rows
	}

	post := func(t *testing.T, h *Handler) *httptest.ResponseRecorder {
		t.Helper()
		reqCtx := auth.WithTenantID(ctx, tenantID)
		req := httptest.NewRequest(http.MethodPost, "/run", nil).WithContext(reqCtx)
		rec := httptest.NewRecorder()
		h.Routes().ServeHTTP(rec, req)
		return rec
	}

	t.Run("successful run emits one trigger row with its invoice count", func(t *testing.T) {
		subs := &mockSubs{
			subs:         map[string]domain.Subscription{"sub_run": dueSubForTenant(tenantID, "sub_run")},
			cycleUpdated: make(map[string]bool),
		}
		subs.db = db // the closer tx is real too, so the in-tx finalize emission has a handle
		engine := tenantRunEngineWith(subs, &mockInvoices{db: db})
		engine.SetAuditLogger(logger)
		h := NewHandler(engine, subs)

		rec := post(t, h)
		if rec.Code != http.StatusOK {
			t.Fatalf("run must be 200, got %d: %s", rec.Code, rec.Body.String())
		}
		var resp struct {
			InvoicesGenerated int      `json:"invoices_generated"`
			Errors            []string `json:"errors"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.InvoicesGenerated != 1 {
			t.Fatalf("invoices_generated = %d, want 1", resp.InvoicesGenerated)
		}

		rows := runRows(t)
		if len(rows) != 1 {
			t.Fatalf("want exactly one billing-run trigger row; got %d: %+v", len(rows), rows)
		}
		r := rows[0]
		if r.Action != domain.AuditActionRun || r.ResourceType != "billing" {
			t.Errorf("row vocabulary = %s/%s, want run/billing", r.Action, r.ResourceType)
		}
		if r.ResourceID != "" {
			t.Errorf("resource_id = %q, want empty — a run is tenant-wide, it has no single resource", r.ResourceID)
		}
		if r.Metadata["action"] != "cycle_run_triggered" {
			t.Errorf("metadata action = %v, want cycle_run_triggered", r.Metadata["action"])
		}
		// JSON round-trip through JSONB: numbers come back as float64.
		if got, ok := r.Metadata["invoices_created"].(float64); !ok || int(got) != 1 {
			t.Errorf("invoices_created = %v, want 1 — the row must record what the run actually produced", r.Metadata["invoices_created"])
		}

		// The trigger row is distinct from the engine's per-invoice finalize
		// evidence: both exist, and the trigger is the only one that can name
		// the operator.
		finalizeRows, _, err := logger.Query(ctx, tenantID, audit.QueryFilter{ResourceType: "invoice"})
		if err != nil {
			t.Fatalf("query invoice audit rows: %v", err)
		}
		if len(finalizeRows) == 0 {
			t.Error("expected the run's finalize row(s) alongside the trigger row")
		}
	})

	t.Run("audit failure surfaces instead of a clean 200", func(t *testing.T) {
		subs := &mockSubs{
			subs:         map[string]domain.Subscription{"sub_fail": dueSubForTenant(tenantID, "sub_fail")},
			cycleUpdated: make(map[string]bool),
		}
		subs.db = db // the closer tx is real too, so the in-tx finalize emission has a handle
		engine := tenantRunEngineWith(subs, &mockInvoices{db: db})
		failing := &erroringRunAudit{}
		engine.SetAuditLogger(failing)
		h := NewHandler(engine, subs)

		before := len(runRows(t))
		rec := post(t, h)
		if rec.Code != http.StatusPartialContent {
			t.Fatalf("an unrecorded trigger must not return a clean 200; got %d", rec.Code)
		}
		var resp struct {
			InvoicesGenerated int      `json:"invoices_generated"`
			Errors            []string `json:"errors"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.InvoicesGenerated != 1 {
			t.Errorf("invoices_generated = %d, want 1 — the run committed; the response must not lie about it", resp.InvoicesGenerated)
		}
		if len(resp.Errors) != 1 || !strings.Contains(resp.Errors[0], "audit") {
			t.Fatalf("the failed audit record must be surfaced to the caller, got %v", resp.Errors)
		}
		if after := len(runRows(t)); after != before {
			t.Errorf("billing trigger rows changed from %d to %d despite the failed write", before, after)
		}
	})
}
