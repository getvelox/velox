package api

import (
	"database/sql"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/sagarsuperuser/velox/internal/doctor"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestE2E_ClockSoak_MonthEndAnchorThroughYearBoundary is the stability plan's
// B2 soak (docs/dev/product-stability-plan.md): one subscription anchored on
// the WORST day of the month (the 31st), driven through 13 real cycle closes
// across a year boundary and two Februaries, through the REAL server — the
// test-clock advance endpoint, the catchup orchestrator, the billing engine —
// with the money-invariant doctor sweeping the whole database after every
// advance.
//
// The pure date math is already pinned by anniversary_month_end_test.go
// (ADR-055: Jan 31 → Feb 28 → Mar 31, anchor preserved). What that cannot
// prove is the PIPELINE: that the engine actually consults the anchored
// advance at cycle close, that every close produces exactly one invoice whose
// period ends where the math says, and that thirteen consecutive closes leave
// ZERO invariant violations behind. A drift here is a permanently shifted
// billing day for every month-end customer; a violation here is the class the
// doctor exists to catch at the moment it is created, not months later.
func TestE2E_ClockSoak_MonthEndAnchorThroughYearBoundary(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test — requires postgres")
	}
	db := testutil.SetupTestDB(t)
	t.Setenv("VELOX_BOOTSTRAP_TOKEN", "soak-bootstrap-token-1234")

	srv := NewServer(db, clock.NewFake(time.Date(2026, 1, 31, 10, 0, 0, 0, time.UTC)), nil)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// The doctor needs a role that sees every row (RLS-scoped roles make it
	// silently blind — see internal/doctor).
	adminURL := strings.TrimSpace(os.Getenv("TEST_ADMIN_DATABASE_URL"))
	if adminURL == "" {
		adminURL = "postgres://velox:velox@localhost:5432/velox_test?sslmode=disable"
	}
	admin, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatalf("open admin pool: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close() })

	// --- fixture: tenant, clock frozen Jan 31, pinned customer, $50/mo sub ---
	resp := doPost(t, ts, "/v1/bootstrap", "", map[string]any{
		"token": "soak-bootstrap-token-1234", "tenant_name": "Soak Co",
		"owner_email": "owner@soak.test", "owner_password": "a-real-12char-pw",
	})
	assertStatus(t, resp, 201)
	key, _ := readJSON(t, resp)["secret_key_test"].(string)
	if key == "" {
		t.Fatal("bootstrap returned no test key")
	}
	auth := "Bearer " + key

	start := time.Date(2026, 1, 31, 10, 0, 0, 0, time.UTC)
	resp = doPost(t, ts, "/v1/test-clocks", auth, map[string]any{
		"name": "soak clock", "frozen_time": start.Format(time.RFC3339),
	})
	assertStatus(t, resp, 201)
	clockID := readJSON(t, resp)["id"].(string)

	resp = doPost(t, ts, "/v1/customers", auth, map[string]any{
		"external_id": "soak_cust", "display_name": "Soak Cust",
		"email": "", "test_clock_id": clockID,
	})
	assertStatus(t, resp, 201)
	custID := readJSON(t, resp)["id"].(string)

	resp = doPost(t, ts, "/v1/plans", auth, map[string]any{
		"code": "soak-mo", "name": "Soak Monthly", "currency": "USD",
		"billing_interval": "monthly", "base_amount_cents": 5000, "status": "active",
	})
	assertStatus(t, resp, 201)
	planID := readJSON(t, resp)["id"].(string)

	resp = doPost(t, ts, "/v1/subscriptions", auth, map[string]any{
		"code": "soak-sub", "display_name": "Soak Sub", "customer_id": custID,
		"items": []map[string]any{{"plan_id": planID}}, "started_at": start.Format(time.RFC3339),
		"billing_time": "anniversary",
	})
	assertStatus(t, resp, 201)
	subID := readJSON(t, resp)["id"].(string)
	resp = doPost(t, ts, "/v1/subscriptions/"+subID+"/activate", auth, nil)
	assertStatus(t, resp, 200)

	// The expected period-end DATES for a day-31 anchor (ADR-055 clamp):
	// Feb 28 '26, Mar 31, Apr 30, May 31, Jun 30, Jul 31, Aug 31, Sep 30,
	// Oct 31, Nov 30, Dec 31, Jan 31 '27, Feb 28 '27, Mar 31 '27.
	// Both Februaries are non-leap; the anchor must clamp to 28 twice and
	// RESTORE to 31 after each — the ratchet this sequence exists to refute.
	ends := []time.Time{
		time.Date(2026, 2, 28, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 3, 31, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 4, 30, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 5, 31, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 11, 30, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC),
		time.Date(2027, 1, 31, 0, 0, 0, 0, time.UTC),
		time.Date(2027, 2, 28, 0, 0, 0, 0, time.UTC),
		time.Date(2027, 3, 31, 0, 0, 0, 0, time.UTC),
	}

	assertPeriodEndDay := func(step int, wantDate time.Time) {
		t.Helper()
		resp := doGet(t, ts, "/v1/subscriptions/"+subID, auth)
		assertStatus(t, resp, 200)
		peRaw, _ := readJSON(t, resp)["current_billing_period_end"].(string)
		pe, err := time.Parse(time.RFC3339, peRaw)
		if err != nil {
			t.Fatalf("step %d: parse period end %q: %v", step, peRaw, err)
		}
		gy, gm, gd := pe.UTC().Date()
		wy, wm, wd := wantDate.Date()
		if gy != wy || gm != wm || gd != wd {
			t.Fatalf("step %d: period end = %04d-%02d-%02d, want %04d-%02d-%02d — the month-end anchor drifted (the exact ratchet ADR-055 exists to prevent)",
				step, gy, gm, gd, wy, wm, wd)
		}
	}

	// Activation must open the first period ending Feb 28.
	assertPeriodEndDay(0, ends[0])

	countInvoices := func() int {
		var n int
		if err := admin.QueryRow(`SELECT count(*) FROM invoices WHERE customer_id = $1`, custID).Scan(&n); err != nil {
			t.Fatalf("count invoices: %v", err)
		}
		return n
	}

	for i := 0; i < len(ends)-1; i++ {
		target := ends[i].Add(26 * time.Hour) // past boundary i, well short of boundary i+1
		resp := doPost(t, ts, "/v1/test-clocks/"+clockID+"/advance", auth, map[string]any{
			"frozen_time": target.Format(time.RFC3339),
		})
		assertStatus(t, resp, 200)
		// Advance runs the catchup phases asynchronously; poll to ready.
		deadline := time.Now().Add(60 * time.Second)
		for {
			resp := doGet(t, ts, "/v1/test-clocks/"+clockID, auth)
			status, _ := readJSON(t, resp)["status"].(string)
			if status == "ready" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("step %d: clock advance did not settle (status %q)", i+1, status)
			}
			time.Sleep(200 * time.Millisecond)
		}

		// One close per advance: the new open period must end at ends[i+1]…
		assertPeriodEndDay(i+1, ends[i+1])
		// …exactly one more invoice must exist (the closed period's)…
		if got, want := countInvoices(), i+1; got != want {
			t.Fatalf("step %d: invoice count = %d, want %d — a close either skipped or double-billed", i+1, got, want)
		}
		// …and the WHOLE database must still satisfy every money invariant.
		res := doctor.Run(t.Context(), admin, doctor.Checks)
		for _, e := range res.Errors {
			t.Errorf("step %d: doctor check failed to run: %v", i+1, e)
		}
		for _, v := range res.Violations {
			t.Errorf("step %d: invariant violated: [%s/%s] %s %s", i+1, v.Check.Domain, v.Check.Name, v.RowID, v.Detail)
		}
		if t.Failed() {
			t.FailNow()
		}
	}

	t.Logf("soak passed: 13 closes, Jan 31 anchor held through two Februaries and a year boundary, doctor clean after every advance")
}
