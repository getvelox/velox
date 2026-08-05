package migrate_test

import (
	"database/sql"
	"os"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/sagarsuperuser/velox/internal/platform/migrate"
)

// dunningTwoAxesVersion is migration 0171 — the ADR-112 split of
// dunning_policies.final_action into final_subscription_action +
// final_invoice_action.
const dunningTwoAxesVersion = 171

// TestMigration0171_DunningActionMapping exercises the DATA halves of the
// split, which TestMigrationRoundTrip does not: that test proves the
// migration applies and un-applies cleanly, not that it maps rows correctly.
//
// Both directions matter for different reasons.
//
// The UP runs on every real deploy against live policies. A mismap there
// silently changes what a policy does when a customer stops paying — a
// `cancel_subscription` policy that came back as (none, none) would leave
// subscriptions billing a delinquent customer forever, with no error
// anywhere. So the up must be behaviour-preserving on all four legacy
// values.
//
// The DOWN is lossy by construction — the two columns express combinations
// the single enum has no value for — so what is asserted is the PRECEDENCE:
// the subscription action wins. A rollback also rolls code back, and the
// rolled-back code fires one action. Losing the subscription half means a
// policy whose operator asked to STOP billing keeps billing; losing the
// invoice half only leaves a debt open on the books. Leave the debt; never
// keep charging.
func TestMigration0171_DunningActionMapping(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test — requires postgres")
	}

	adminURL := strings.TrimSpace(os.Getenv("TEST_ADMIN_DATABASE_URL"))
	if adminURL == "" {
		adminURL = "postgres://velox:velox@localhost:5432/velox_test?sslmode=disable"
	}

	scratchDB := "velox_migrate_0171"
	scratchURL := rewriteDBName(t, adminURL, scratchDB)
	createScratchDB(t, adminURL, scratchDB)
	t.Cleanup(func() { dropScratchDB(t, adminURL, scratchDB) })

	if err := migrate.Up(scratchURL); err != nil {
		t.Fatalf("initial Up: %v", err)
	}
	top, _, err := migrate.Status(scratchURL)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if top < dunningTwoAxesVersion {
		t.Fatalf("schema top is %d, below the migration under test (%d)", top, dunningTwoAxesVersion)
	}

	db, err := sql.Open("pgx", scratchURL)
	if err != nil {
		t.Fatalf("open scratch: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// One tenant per policy: dunning_policies is UNIQUE (tenant_id) at
	// 0001 and the campaigns model keeps one default per tenant, so
	// distinct tenants are the cheapest way to hold several policies.
	seedPolicy := func(key, sub, inv string) {
		t.Helper()
		tenantID := "tnt_0171_" + key
		if _, err := db.Exec(
			`INSERT INTO tenants (id, name) VALUES ($1, $2) ON CONFLICT (id) DO NOTHING`,
			tenantID, "0171 "+key); err != nil {
			t.Fatalf("seed tenant %s: %v", key, err)
		}
		if _, err := db.Exec(`
			INSERT INTO dunning_policies
				(id, tenant_id, name, final_subscription_action, final_invoice_action)
			VALUES ($1, $2, $3, $4, $5)`,
			"dpol_0171_"+key, tenantID, key, sub, inv); err != nil {
			t.Fatalf("seed policy %s: %v", key, err)
		}
	}

	// Every combination the two axes can express. cancel+write-off is the
	// pairing the single enum could not represent at all — Stripe's own
	// default outcome — so it is the discriminating row in both directions.
	seedPolicy("cancel_writeoff", "cancel", "mark_uncollectible")
	seedPolicy("pause_writeoff", "pause", "mark_uncollectible")
	seedPolicy("cancel_only", "cancel", "none")
	seedPolicy("pause_only", "pause", "none")
	seedPolicy("writeoff_only", "none", "mark_uncollectible")
	seedPolicy("nothing", "none", "none")

	// ---- DOWN: collapse, subscription action winning -------------------
	if _, err := migrate.Rollback(scratchURL, 1); err != nil {
		t.Fatalf("rollback one step: %v", err)
	}

	legacyOf := func(key string) string {
		t.Helper()
		var got string
		if err := db.QueryRow(
			`SELECT final_action FROM dunning_policies WHERE id = $1`, "dpol_0171_"+key,
		).Scan(&got); err != nil {
			t.Fatalf("read final_action for %s: %v", key, err)
		}
		return got
	}

	for _, tc := range []struct{ key, want, why string }{
		{"cancel_writeoff", "cancel_subscription",
			"the subscription half must win — rolling back to code that only cancels-OR-writes-off must not leave a sub billing a customer the operator gave up on"},
		{"pause_writeoff", "pause",
			"same precedence: keep the collection pause, lose the write-off"},
		{"cancel_only", "cancel_subscription", "unambiguous"},
		{"pause_only", "pause", "unambiguous"},
		{"writeoff_only", "mark_uncollectible", "unambiguous"},
		{"nothing", "manual_review", "(none, none) is the old manual_review"},
	} {
		if got := legacyOf(tc.key); got != tc.want {
			t.Errorf("down %s: got final_action=%q, want %q — %s", tc.key, got, tc.want, tc.why)
		}
	}

	// Rows written by the OLD binary, i.e. what a real rollback window
	// produces. These exercise the up's backfill on genuinely legacy data
	// rather than on rows the down just wrote.
	seedLegacy := func(key, action string) {
		t.Helper()
		tenantID := "tnt_0171_legacy_" + key
		if _, err := db.Exec(
			`INSERT INTO tenants (id, name) VALUES ($1, $2) ON CONFLICT (id) DO NOTHING`,
			tenantID, "0171 legacy "+key); err != nil {
			t.Fatalf("seed legacy tenant %s: %v", key, err)
		}
		if _, err := db.Exec(`
			INSERT INTO dunning_policies (id, tenant_id, name, final_action)
			VALUES ($1, $2, $3, $4)`,
			"dpol_0171_legacy_"+key, tenantID, key, action); err != nil {
			t.Fatalf("seed legacy policy %s: %v", key, err)
		}
	}
	seedLegacy("manual_review", "manual_review")
	seedLegacy("pause", "pause")
	seedLegacy("cancel", "cancel_subscription")
	seedLegacy("writeoff", "mark_uncollectible")

	// ---- UP: behaviour-preserving backfill ------------------------------
	if err := migrate.Up(scratchURL); err != nil {
		t.Fatalf("re-Up: %v", err)
	}

	axesOf := func(id string) (string, string) {
		t.Helper()
		var sub, inv string
		if err := db.QueryRow(
			`SELECT final_subscription_action, final_invoice_action FROM dunning_policies WHERE id = $1`, id,
		).Scan(&sub, &inv); err != nil {
			t.Fatalf("read axes for %s: %v", id, err)
		}
		return sub, inv
	}

	for _, tc := range []struct{ key, wantSub, wantInv string }{
		{"manual_review", "none", "none"},
		{"pause", "pause", "none"},
		{"cancel", "cancel", "none"},
		{"writeoff", "none", "mark_uncollectible"},
	} {
		sub, inv := axesOf("dpol_0171_legacy_" + tc.key)
		if sub != tc.wantSub || inv != tc.wantInv {
			t.Errorf("up %s: got (%q, %q), want (%q, %q) — the backfill must preserve every existing policy's behaviour exactly; no policy may start or stop writing off an invoice because of this migration",
				tc.key, sub, inv, tc.wantSub, tc.wantInv)
		}
	}

	// A NEW policy still defaults to (pause, none): 0071's default is kept,
	// and the invoice half deliberately does NOT default to a write-off —
	// that would have a machine assert bad debt on a fresh tenant's behalf.
	if _, err := db.Exec(
		`INSERT INTO tenants (id, name) VALUES ('tnt_0171_default', '0171 default') ON CONFLICT (id) DO NOTHING`); err != nil {
		t.Fatalf("seed default tenant: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO dunning_policies (id, tenant_id, name) VALUES ('dpol_0171_default', 'tnt_0171_default', 'defaults')`); err != nil {
		t.Fatalf("seed default policy: %v", err)
	}
	if sub, inv := axesOf("dpol_0171_default"); sub != "pause" || inv != "none" {
		t.Errorf("new-policy defaults: got (%q, %q), want (pause, none)", sub, inv)
	}
}
