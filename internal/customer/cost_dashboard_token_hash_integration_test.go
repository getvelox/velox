package customer_test

import (
	"context"
	"strings"
	"testing"

	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestCostDashboardToken_HashLookupAndNoPlaintextAtRest closes the pre-prod
// HARD GATE recorded by the 2026-07-19 lies audit: customers.cost_dashboard_token
// held the credential for the UNAUTHENTICATED
// GET /v1/public/cost-dashboard/{token} surface in plaintext. Any read of the
// customers table — snapshot, replica, backup, one SELECT through an injection
// — therefore yielded directly-replayable dashboard URLs for every customer at
// once. Migration 0172 stores only a SHA-256 blind index.
//
// Positive and negative halves both matter here, and the negative is the point:
// a lookup that still resolved would prove the read path works, while plaintext
// sat in the column unnoticed. So this asserts the token resolves AND that the
// raw token appears nowhere in the row.
func TestCostDashboardToken_HashLookupAndNoPlaintextAtRest(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test — requires postgres")
	}
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	store := customer.NewPostgresStore(db)

	tenantID := testutil.CreateTestTenant(t, db, "Cost Dash Token Hash")
	cust, err := store.Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_cdt", DisplayName: "CDT",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}

	token, err := customer.NewCostDashboardToken()
	if err != nil {
		t.Fatalf("mint token: %v", err)
	}
	if err := store.SetCostDashboardToken(ctx, tenantID, cust.ID, token); err != nil {
		t.Fatalf("set token: %v", err)
	}

	// Positive: the raw token from the rotate response still resolves.
	got, err := store.GetByCostDashboardToken(ctx, token)
	if err != nil {
		t.Fatalf("resolve by raw token: %v", err)
	}
	if got.ID != cust.ID {
		t.Fatalf("resolved customer = %s, want %s", got.ID, cust.ID)
	}

	// Negative: nothing in the row is the token. Scanning the whole row as
	// text catches a plaintext copy landing in ANY column, not just the one
	// this migration renamed — a narrower assertion would pass while some
	// other writer stashed it elsewhere.
	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin bypass: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	var rowText, storedHash string
	if err := tx.QueryRow(`
		SELECT customers::text, COALESCE(cost_dashboard_token_hash,'')
		  FROM customers WHERE id = $1
	`, cust.ID).Scan(&rowText, &storedHash); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if strings.Contains(rowText, token) {
		t.Errorf("raw cost-dashboard token found at rest in the customers row — a DB read yields a replayable dashboard URL")
	}
	if want := customer.HashCostDashboardToken(token); storedHash != want {
		t.Errorf("cost_dashboard_token_hash = %q, want %q", storedHash, want)
	}

	// A stale token must stop resolving the instant it is rotated — the
	// rotate button's whole purpose is "kill the previous URL now".
	second, err := customer.NewCostDashboardToken()
	if err != nil {
		t.Fatalf("mint second token: %v", err)
	}
	if err := store.SetCostDashboardToken(ctx, tenantID, cust.ID, second); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if _, err := store.GetByCostDashboardToken(ctx, token); err == nil {
		t.Error("the pre-rotation token still resolves — rotation did not invalidate the old URL")
	}
	if _, err := store.GetByCostDashboardToken(ctx, second); err != nil {
		t.Errorf("the rotated-in token does not resolve: %v", err)
	}
}

// TestHashCostDashboardToken_MatchesSQLBackfill pins the Go hash against the
// SQL expression migration 0172 uses to backfill existing rows. If these ever
// disagree, every dashboard URL minted before the migration silently stops
// resolving — a failure with no error anywhere, just 404s. The invoice twin
// (migration 0107) carries the same pairing for the same reason.
func TestHashCostDashboardToken_MatchesSQLBackfill(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test — requires postgres")
	}
	db := testutil.SetupTestDB(t)

	token, err := customer.NewCostDashboardToken()
	if err != nil {
		t.Fatalf("mint token: %v", err)
	}
	tx, err := db.BeginTx(context.Background(), postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin bypass: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	var sqlHash string
	if err := tx.QueryRow(`SELECT encode(sha256($1::bytea), 'hex')`, token).Scan(&sqlHash); err != nil {
		t.Fatalf("sql hash: %v", err)
	}
	if goHash := customer.HashCostDashboardToken(token); goHash != sqlHash {
		t.Fatalf("Go hash %q != migration 0172 SQL hash %q — the backfill would orphan every pre-existing dashboard URL", goHash, sqlHash)
	}
}
