package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"net/http/httptest"

	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestE2E_AuditRequestIDIsServerMinted proves the ADR-090 §6 property on a REAL
// audit row, through the REAL router stack — not just at the middleware.
//
// The unit test (middleware.TestRequestID_IgnoresClientSuppliedHeader) pins that
// mw.RequestID ignores the inbound header. This one closes the rest of the
// chain: router mounts it → handler runs → the audit writer stamps
// audit_log.request_id from that ctx. A regression anywhere along it (someone
// re-mounts chi's middleware.RequestID, or an emitter starts reading a header)
// shows up here as an attacker-chosen string sitting in the append-only log.
func TestE2E_AuditRequestIDIsServerMinted(t *testing.T) {
	db := testutil.SetupTestDB(t)
	clk := clock.NewFake(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))
	srv := NewServer(db, clk, nil)

	tenantID := testutil.CreateTestTenant(t, db, "ReqID Corp")
	apiKey := createTestAPIKey(t, db, tenantID)

	ts := httptest.NewServer(srv)
	defer ts.Close()

	const forged = "forged-correlation-id"

	// A real, audited mutation (customer create → audit.Entry{create, customer}),
	// carrying the header chi's stock RequestID would have trusted verbatim.
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/customers",
		strings.NewReader(`{"external_id":"cus_reqid","display_name":"ReqID Co"}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", forged)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create customer: status %d, want 201", resp.StatusCode)
	}

	// The response header is Velox's published correlation contract — it must
	// hand back the SERVER's id, not echo the client's.
	headerID := resp.Header.Get("Velox-Request-Id")
	if headerID == forged {
		t.Errorf("Velox-Request-Id echoed the client's forged value: %q", headerID)
	}
	if !strings.HasPrefix(headerID, "req_") {
		t.Errorf("Velox-Request-Id %q is not server-minted (want req_ prefix)", headerID)
	}

	// Now the row itself — the thing an auditor would rely on.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)

	var rowRequestID string
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(request_id, '')
		FROM audit_log
		WHERE tenant_id = $1 AND action = 'create' AND resource_type = 'customer'
		ORDER BY created_at DESC LIMIT 1`, tenantID).Scan(&rowRequestID); err != nil {
		t.Fatalf("read audit row: %v", err)
	}

	if rowRequestID == forged {
		t.Fatalf("audit_log.request_id is the CLIENT's forged value %q — the append-only log's correlation evidence is attacker-controlled", forged)
	}
	if strings.Contains(rowRequestID, forged) {
		t.Fatalf("client-supplied value leaked into audit_log.request_id: %q", rowRequestID)
	}
	if !strings.HasPrefix(rowRequestID, "req_") {
		t.Fatalf("audit_log.request_id %q is not server-minted (want req_ prefix)", rowRequestID)
	}
	// The row and the response must name the SAME request, or support cannot
	// join a customer's report to the log at all.
	if rowRequestID != headerID {
		t.Errorf("audit row request_id %q != Velox-Request-Id header %q — correlation is broken", rowRequestID, headerID)
	}
}
