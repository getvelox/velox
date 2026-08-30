package api

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// Read-egress auditing (ADR-090 §7). Bulk export is the act of COPYING the
// evidence — a tamper-evidence system that cannot say who did it has a hole in
// its chain of custody. These tests run against real Postgres because every
// guarantee here is a property of a committed row.

// testKeyLivemode is the plane the suite's API key operates on. createTestAPIKey
// inserts an api_keys row without a livemode column, and the column DEFAULTS to
// true (migration 0020) — so every request it authenticates is a LIVE request,
// and audit_log's trigger stamps its rows livemode=true. Reads that disagree see
// nothing (the explicit livemode predicate in buildListWhere), so the tests must
// query the same plane they wrote on.
const testKeyLivemode = true

// exportRow is one action=export audit row, read back from the log.
type exportRow struct {
	ID           string
	ResourceType string
	ResourceID   string
	ActorType    string
	ActorID      string
	Metadata     map[string]any
}

// readExportRows returns the audit rows for one exported resource type.
func readExportRows(t *testing.T, db *postgres.DB, tenantID, resourceType string) []exportRow {
	t.Helper()
	logger := audit.NewLogger(db)
	ctx := postgres.WithLivemode(context.Background(), testKeyLivemode)
	entries, _, err := logger.Query(ctx, tenantID, audit.QueryFilter{
		Action:       domain.AuditActionExport,
		ResourceType: resourceType,
		Limit:        100,
	})
	if err != nil {
		t.Fatalf("query audit log: %v", err)
	}
	out := make([]exportRow, 0, len(entries))
	for _, e := range entries {
		out = append(out, exportRow{
			ID: e.ID, ResourceType: e.ResourceType, ResourceID: e.ResourceID,
			ActorType: e.ActorType, ActorID: e.ActorID, Metadata: e.Metadata,
		})
	}
	return out
}

// TestExportsAudit_EachExportWritesExactlyOneRow drives all five CSV exports
// through the real router with a real API key and proves each one leaves exactly
// one action=export row, attributed to the caller, naming the file it handed over.
func TestExportsAudit_EachExportWritesExactlyOneRow(t *testing.T) {
	db := testutil.SetupTestDB(t)
	srv := NewServer(db, clock.NewFake(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)), nil)
	tenantID := testutil.CreateTestTenant(t, db, "Egress Audit Corp")
	apiKey := createTestAPIKey(t, db, tenantID)
	bearer := "Bearer " + apiKey

	ts := httptest.NewServer(srv)
	defer ts.Close()

	// One customer, so customers.csv has a data row to stream. The display name
	// is a spreadsheet formula on purpose — it rides into BOTH exports (the
	// customer CSV directly, and the audit CSV via the create row's
	// resource_label), which is exactly the injection path an operator hands an
	// auditor.
	resp := doPost(t, ts, "/v1/customers", bearer, map[string]any{
		"external_id":  "cus_egress_1",
		"display_name": `=HYPERLINK("http://evil.test","click")`,
		"email":        "egress@example.test",
	})
	assertStatus(t, resp, 201)
	_ = resp.Body.Close()

	cases := []struct {
		name         string
		path         string
		resourceType string
		wantScope    string // a metadata.filters value that must be present
	}{
		{"customers", "/v1/exports/customers.csv", "customer", "all"},
		{"invoices", "/v1/exports/invoices.csv", "invoice", "all"},
		{"subscriptions", "/v1/exports/subscriptions.csv", "subscription", "all"},
		{"usage events", "/v1/exports/usage-events.csv?from=2026-01-01&to=2026-03-01", "usage_event", ""},
		{"audit log", "/v1/exports/audit-log.csv", "audit_log", "all"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := doGet(t, ts, tc.path, bearer)
			defer func() { _ = resp.Body.Close() }()
			assertStatus(t, resp, 200)

			if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
				t.Errorf("Content-Type: got %q, want text/csv", ct)
			}
			body, _ := io.ReadAll(resp.Body)
			if len(body) == 0 {
				t.Fatal("export streamed no bytes")
			}

			rows := readExportRows(t, db, tenantID, tc.resourceType)
			if len(rows) != 1 {
				t.Fatalf("audit rows for export of %s: got %d, want exactly 1 (a no-op must not fabricate a row; an export must not be silent)", tc.resourceType, len(rows))
			}
			row := rows[0]

			// resource_id is empty BY DESIGN: a bulk export has no single subject.
			if row.ResourceID != "" {
				t.Errorf("resource_id: got %q, want empty — a bulk export has no single subject", row.ResourceID)
			}
			// Attributed to the caller, not to 'system'.
			if row.ActorType != "api_key" || row.ActorID == "" {
				t.Errorf("actor: got (%s, %s), want the calling api_key", row.ActorType, row.ActorID)
			}
			// The filename on the row is the filename the operator received — the
			// string that traces a file found later back to this export.
			gotFilename, _ := row.Metadata["filename"].(string)
			cd := resp.Header.Get("Content-Disposition")
			if gotFilename == "" || !strings.Contains(cd, gotFilename) {
				t.Errorf("metadata.filename %q is not the delivered filename (Content-Disposition: %q)", gotFilename, cd)
			}
			if format, _ := row.Metadata["format"].(string); format != "csv" {
				t.Errorf("metadata.format: got %q, want csv", format)
			}
			// No row count on the row: it is unknowable before the stream starts,
			// and a count we cannot honour is a lie in a permanent record.
			if _, present := row.Metadata["row_count"]; present {
				t.Error("metadata carries a row_count — an export cannot know its size before it streams; do not promise one")
			}
			filters, _ := row.Metadata["filters"].(map[string]any)
			if filters == nil {
				t.Fatalf("metadata.filters missing — the row must record WHAT was taken: %v", row.Metadata)
			}
			if tc.wantScope != "" {
				if got, _ := filters["date_range"].(string); got != tc.wantScope {
					t.Errorf("metadata.filters.date_range: got %q, want %q (an unfiltered export is a whole-table dump and must say so)", got, tc.wantScope)
				}
			} else {
				// usage-events requires an explicit range; it must be recorded.
				if filters["from"] == nil || filters["to"] == nil {
					t.Errorf("metadata.filters must carry the requested from/to: %v", filters)
				}
			}
		})
	}
}

// TestExportsAudit_CustomerCSVNeutralizesFormulaInjection proves the artifact an
// operator opens in Excel is inert, end to end through the real router: a
// customer display name that IS a formula comes back quote-prefixed.
func TestExportsAudit_CustomerCSVNeutralizesFormulaInjection(t *testing.T) {
	db := testutil.SetupTestDB(t)
	srv := NewServer(db, clock.NewFake(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)), nil)
	tenantID := testutil.CreateTestTenant(t, db, "Formula Corp")
	bearer := "Bearer " + createTestAPIKey(t, db, tenantID)

	ts := httptest.NewServer(srv)
	defer ts.Close()

	payload := `=HYPERLINK("http://evil.test","click")`
	resp := doPost(t, ts, "/v1/customers", bearer, map[string]any{
		"external_id":  "cus_formula",
		"display_name": payload,
		"email":        "formula@example.test",
	})
	assertStatus(t, resp, 201)
	_ = resp.Body.Close()

	resp = doGet(t, ts, "/v1/exports/customers.csv", bearer)
	defer func() { _ = resp.Body.Close() }()
	assertStatus(t, resp, 200)

	records, err := csv.NewReader(resp.Body).ReadAll()
	if err != nil {
		t.Fatalf("parse csv: %v", err)
	}
	var found bool
	for _, rec := range records[1:] {
		if rec[1] != "cus_formula" {
			continue
		}
		found = true
		if rec[2] != "'"+payload {
			t.Errorf("display_name cell = %q, want it quote-prefixed (%q). Excel/Sheets EXECUTE a cell starting with '=' — the CSV is the artifact an operator hands an auditor.", rec[2], "'"+payload)
		}
	}
	if !found {
		t.Fatal("exported customer not found in the CSV")
	}

	// And the same payload, arriving through the AUDIT log's resource_label.
	resp2 := doGet(t, ts, "/v1/exports/audit-log.csv", bearer)
	defer func() { _ = resp2.Body.Close() }()
	assertStatus(t, resp2, 200)
	auditRecords, err := csv.NewReader(resp2.Body).ReadAll()
	if err != nil {
		t.Fatalf("parse audit csv: %v", err)
	}
	labelCol := 8 // resource_label
	var sawLabel bool
	for _, rec := range auditRecords[1:] {
		if strings.Contains(rec[labelCol], payload) {
			sawLabel = true
			if !strings.HasPrefix(rec[labelCol], "'") {
				t.Errorf("audit-log resource_label cell = %q, want it quote-prefixed — customer display names ride into the compliance export", rec[labelCol])
			}
		}
	}
	if !sawLabel {
		t.Fatal("the customer-create row's resource_label never appeared in the audit CSV — the injection path this asserts is not being exercised")
	}
}

// TestExportsAudit_AuditLogExportStreamsEveryRow pins the absence of a cap.
//
// The dashboard used to build this file by paging the API in the browser and
// stopping at 50,000 rows — a SILENT truncation of the compliance evidence
// itself. Seeding 50k rows here would be a slow way to assert nothing: the
// server-side constants that could reintroduce truncation are the read path's
// limits (default 50, clamp 100) and the export's flush interval (100). 250 rows
// clears all three, so a cap of ANY of those shapes fails this test.
//
// It also proves the emit-BEFORE-stream ordering directly: the export's own
// audit row is committed before the stream's snapshot opens, so the file
// CONTAINS the record of its own export.
func TestExportsAudit_AuditLogExportStreamsEveryRow(t *testing.T) {
	db := testutil.SetupTestDB(t)
	srv := NewServer(db, clock.NewFake(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)), nil)
	tenantID := testutil.CreateTestTenant(t, db, "Big Log Corp")
	bearer := "Bearer " + createTestAPIKey(t, db, tenantID)

	ts := httptest.NewServer(srv)
	defer ts.Close()

	// Seed 250 rows on the same (tenant, livemode) plane the API key reads.
	const seeded = 250
	ctx := postgres.WithLivemode(context.Background(), testKeyLivemode)
	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	logger := audit.NewLogger(db)
	for i := 0; i < seeded; i++ {
		if err := logger.LogInTx(ctx, tx, audit.Entry{
			Action:       domain.AuditActionUpdate,
			ResourceType: "seed_probe",
			ResourceID:   fmt.Sprintf("vlx_seed_%03d", i),
		}); err != nil {
			_ = tx.Rollback()
			t.Fatalf("seed row %d: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit seed: %v", err)
	}

	resp := doGet(t, ts, "/v1/exports/audit-log.csv", bearer)
	defer func() { _ = resp.Body.Close() }()
	assertStatus(t, resp, 200)

	records, err := csv.NewReader(resp.Body).ReadAll()
	if err != nil {
		t.Fatalf("parse csv: %v", err)
	}
	if len(records) == 0 {
		t.Fatal("empty CSV")
	}
	if records[0][0] == "EXPORT_INCOMPLETE" {
		t.Fatal("export aborted mid-stream")
	}
	data := records[1:] // drop the header

	// Every seeded row is present — no cap, no page-loop truncation.
	seedRows := 0
	var sawOwnExportRow bool
	for _, rec := range data {
		if rec[0] == "EXPORT_INCOMPLETE" {
			t.Fatal("EXPORT_INCOMPLETE marker in the file — the stream aborted")
		}
		if rec[6] == "seed_probe" {
			seedRows++
		}
		if rec[5] == domain.AuditActionExport && rec[6] == "audit_log" {
			sawOwnExportRow = true
		}
	}
	if seedRows != seeded {
		t.Errorf("seeded audit rows in the export: got %d, want %d — the server-side export must not truncate the compliance record", seedRows, seeded)
	}
	if !sawOwnExportRow {
		t.Error("the audit-log export does not contain its own export row. Either the row was written AFTER the stream (a connection killed mid-stream would then leave NO record of the egress), or it was not written at all.")
	}
}

// TestExports_FlusherPassesThroughToTheSocket pins that the exports still STREAM.
// The retired audit catch-all buffered every response to sniff a label out of it,
// and its buffer implemented no http.Flusher — so the CSV silently accumulated in
// memory on the one route block given five minutes precisely BECAUSE it streams.
// httptest.ResponseRecorder records Flush(); if any middleware in the real chain
// ever wraps the writer without preserving Flusher, this goes false.
func TestExports_FlusherPassesThroughToTheSocket(t *testing.T) {
	db := testutil.SetupTestDB(t)
	srv := NewServer(db, clock.NewFake(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)), nil)
	tenantID := testutil.CreateTestTenant(t, db, "Flusher Corp")
	bearer := "Bearer " + createTestAPIKey(t, db, tenantID)

	ts := httptest.NewServer(srv)
	defer ts.Close()
	resp := doPost(t, ts, "/v1/customers", bearer, map[string]any{
		"external_id":  "cus_flush",
		"display_name": "Flush Co",
		"email":        "flush@example.test",
	})
	assertStatus(t, resp, 201)
	_ = resp.Body.Close()

	req := httptest.NewRequest(http.MethodGet, "/v1/exports/customers.csv", nil)
	req.Header.Set("Authorization", bearer)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if !rec.Flushed {
		t.Error("the export never reached http.Flusher through the real middleware chain — it is buffering the whole file in memory instead of streaming it")
	}
}

// failingAuditor is an audit writer that cannot write. It stands in for the real
// failure modes: Postgres down, the audit INSERT rejected, the tx unable to begin.
type failingAuditor struct{ calls int }

func (f *failingAuditor) Log(ctx context.Context, tenantID, action, resourceType, resourceID, resourceLabel string, metadata map[string]any) error {
	f.calls++
	return errors.New("audit log: insert: connection refused")
}

// panicStreamer fails the test if the export ever reaches the data read. It must
// not: the audit row is the gate.
type panicStreamer struct{ t *testing.T }

func (p *panicStreamer) Stream(ctx context.Context, tenantID string, filter audit.QueryFilter, fn func(domain.AuditEntry) error) error {
	p.t.Error("the export READ the audit log after its audit row failed to write — fail-closed means no data leaves")
	return nil
}

// TestExportsAudit_FailClosed_NoBytesStreamWhenTheRowCannotBeWritten is the
// ordering guarantee, made load-bearing.
//
// If the audit row cannot be written, the export does not happen: 5xx, no CSV
// headers, not one byte of tenant data. This is why the emission is BEFORE the
// stream and not a defer — a row written at completion is defeated by killing the
// connection mid-stream, and pages of customer PII would egress with nothing
// recorded. Emit-then-stream can only over-record (a row for a file that failed);
// stream-then-emit under-records, and in an append-only log that is unrecoverable.
func TestExportsAudit_FailClosed_NoBytesStreamWhenTheRowCannotBeWritten(t *testing.T) {
	// Stores are nil on purpose: reaching them at all would be the bug. The
	// handler must refuse before it touches tenant data.
	auditor := &failingAuditor{}
	h := newExportsHandler(nil, nil, nil, nil, auditor, &panicStreamer{t: t})

	// csvHeader is the first cell of THIS export's header row. The absence check
	// must be per-export: a shared grep for a handful of column names was
	// vacuous for the two CSVs that happen not to contain any of them, so those
	// cases asserted nothing about the bytes.
	exports := map[string]struct {
		handler   http.HandlerFunc
		path      string
		csvHeader string
	}{
		"customers":     {h.exportCustomers, "/v1/exports/customers.csv", "external_id"},
		"invoices":      {h.exportInvoices, "/v1/exports/invoices.csv", "invoice_number"},
		"subscriptions": {h.exportSubscriptions, "/v1/exports/subscriptions.csv", "billing_time"},
		"usage events":  {h.exportUsageEvents, "/v1/exports/usage-events.csv?from=2026-01-01&to=2026-02-01", "dimensions_json"},
		"audit log":     {h.exportAuditLog, "/v1/exports/audit-log.csv", "resource_label"},
	}

	for name, tc := range exports {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			ctx := auth.WithTenantID(req.Context(), "vlx_tnt_failclosed")
			ctx = auth.WithKeyID(ctx, "vlx_key_failclosed")
			ctx = postgres.WithLivemode(ctx, testKeyLivemode)
			rec := httptest.NewRecorder()

			tc.handler(rec, req.WithContext(ctx))

			if rec.Code != http.StatusInternalServerError {
				t.Errorf("status: got %d, want 500 — an export whose audit row cannot be written must NOT run", rec.Code)
			}
			if cd := rec.Header().Get("Content-Disposition"); cd != "" {
				t.Errorf("Content-Disposition was set (%q) — the browser was told a file is coming", cd)
			}
			if ct := rec.Header().Get("Content-Type"); strings.HasPrefix(ct, "text/csv") {
				t.Errorf("Content-Type is %q — a CSV response began despite the failed audit row", ct)
			}
			body := rec.Body.String()
			if strings.Contains(body, tc.csvHeader) {
				t.Errorf("CSV content streamed after the audit row failed (found the %q header):\n%s", tc.csvHeader, body)
			}
			// Belt: whatever the body is, it must not look like a CSV at all —
			// the only thing a fail-closed export may emit is the error envelope.
			if !strings.Contains(body, `"error"`) {
				t.Errorf("expected a JSON error envelope and nothing else; got:\n%s", body)
			}
		})
	}

	if auditor.calls != len(exports) {
		t.Errorf("audit emission attempts: got %d, want %d — every export must try to write its row", auditor.calls, len(exports))
	}
}

// TestAuditLogCSV_NeutralizesHistoricalPoisonedRequestID pins the one column
// whose danger OUTLIVES its fix.
//
// request_id is server-minted today (ADR-090 §6 replaced chi's middleware,
// which copied an inbound X-Request-Id header verbatim onto the row). But
// audit_log is APPEND-ONLY: every row written before that change still carries
// whatever string its caller chose — and a caller could choose a formula. Those
// rows cannot be cleaned; they can only be rendered safely. So the export must
// neutralize the column even though nothing can poison it any more.
func TestAuditLogCSV_NeutralizesHistoricalPoisonedRequestID(t *testing.T) {
	db := testutil.SetupTestDB(t)
	srv := NewServer(db, clock.NewFake(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)), nil)
	tenantID := testutil.CreateTestTenant(t, db, "Legacy Poison Co")
	bearer := "Bearer " + createTestAPIKey(t, db, tenantID)

	ts := httptest.NewServer(srv)
	defer ts.Close()

	// A row as it would have been written BEFORE request_id became server-minted:
	// the caller sent this header, and chi copied it onto the row verbatim.
	const payload = `=HYPERLINK("http://evil.test","Q3 invoice")`
	ctx := postgres.WithLivemode(context.Background(), true)
	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO audit_log (id, tenant_id, actor_type, actor_id, action,
			resource_type, resource_id, resource_label, metadata, request_id, created_at)
		VALUES ($1, $2, 'api_key', 'k1', 'update', 'customer', 'cus_1', '', '{}', $3, now())
	`, postgres.NewID("vlx_aud"), tenantID, payload); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	req, _ := http.NewRequest("GET", ts.URL+"/v1/exports/audit-log.csv", nil)
	req.Header.Set("Authorization", bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	csv := string(body)

	// The row must actually be in the export, or the test proves nothing. (The
	// CSV writer doubles the inner quotes, so match on the distinctive head of
	// the payload rather than the raw string.)
	if !strings.Contains(csv, "HYPERLINK") {
		t.Fatalf("the poisoned request_id never reached the CSV — the test proves nothing:\n%s", csv)
	}
	// Defanged: csvSafe prefixes a single quote, so the spreadsheet treats the
	// cell as text instead of evaluating it. A LIVE formula would appear as
	// `"=HYPERLINK` (quote then equals) with no leading apostrophe.
	if !strings.Contains(csv, `'=HYPERLINK`) {
		t.Errorf("request_id cell is a LIVE FORMULA in the auditor's evidence pack — csvSafe was not applied to it:\n%s", csv)
	}
}
