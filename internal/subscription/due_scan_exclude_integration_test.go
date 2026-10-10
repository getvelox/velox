package subscription_test

import (
	"context"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/leader"
	"github.com/sagarsuperuser/velox/internal/platform/leader/leadertest"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/subscription"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestDueScans_ExcludeAppliesBeforeLimit locks the exclusion the billing drain
// relies on (P27). Each scan is asked for ONE row with the oldest due sub
// excluded, and must return the second-oldest. A filter applied after the
// LIMIT (or no filter) returns the excluded sub or nothing, and the drain's
// next page would be the same failing sub again.
//
// Mutation seam: drop `AND NOT (s.id = ANY(...))` from any of the three
// queries and its subtest fails.
func TestDueScans_ExcludeAppliesBeforeLimit(t *testing.T) {
	db := testutil.SetupTestDB(t)
	store := subscription.NewPostgresStore(db)
	ctx := leadertest.Token(t, testutil.AdminPool(t), postgres.WithLivemode(context.Background(), false), leader.RoleBilling)

	const tenantID = "vlx_ten_dueexcl"
	const clockID = "vlx_tclk_dueexcl"
	wallNow := time.Now().UTC().Truncate(time.Microsecond)
	frozen := wallNow.Add(-10 * 24 * time.Hour)

	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := tx.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	mustExec(`SELECT set_config('app.livemode', 'off', true)`)
	mustExec(`INSERT INTO tenants (id, name, status) VALUES ($1, 'DueExclude', 'active')`, tenantID)
	mustExec(`INSERT INTO test_clocks (id, tenant_id, name, frozen_time, status, livemode)
		VALUES ($1, $2, 'dueexcl', $3, 'ready', false)`, clockID, tenantID, frozen)
	mustExec(`INSERT INTO customers (id, tenant_id, external_id, display_name, email, created_at, updated_at)
		VALUES ('cus_de', $1, 'de', 'DueExclude', '', $2, $2)`, tenantID, wallNow)
	seedSub := func(id string, nextBillingAt time.Time, clock any) {
		mustExec(`INSERT INTO subscriptions (
			id, tenant_id, code, display_name, customer_id, status, billing_time,
			current_billing_period_start, current_billing_period_end, next_billing_at,
			livemode, test_clock_id, created_at, updated_at
		) VALUES ($1, $2, $1, $1, 'cus_de', 'active', 'calendar', $3, $4, $4, false, $5, $3, $3)`,
			id, tenantID, nextBillingAt.Add(-30*24*time.Hour), nextBillingAt, clock)
	}
	// Wall subs, oldest first. Ages far past any other test's seed, so these
	// two are the oldest due wall subs in the test-mode partition.
	seedSub("sub_de_oldest", wallNow.Add(-3000*24*time.Hour), nil)
	seedSub("sub_de_second", wallNow.Add(-2999*24*time.Hour), nil)
	seedSub("sub_de_clk_oldest", frozen.Add(-2*time.Hour), clockID)
	seedSub("sub_de_clk_second", frozen.Add(-1*time.Hour), clockID)
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	cases := []struct {
		name, excluded, want string
		scan                 func(exclude []string) ([]string, error)
	}{
		{"GetDueBilling", "sub_de_oldest", "sub_de_second", func(ex []string) ([]string, error) {
			subs, err := store.GetDueBilling(ctx, wallNow, ex, 1)
			return ids(subs), err
		}},
		{"GetDueBillingForTenant", "sub_de_oldest", "sub_de_second", func(ex []string) ([]string, error) {
			subs, err := store.GetDueBillingForTenant(ctx, tenantID, wallNow, ex, 1)
			return ids(subs), err
		}},
		{"GetDueBillingForClock", "sub_de_clk_oldest", "sub_de_clk_second", func(ex []string) ([]string, error) {
			subs, err := store.GetDueBillingForClock(ctx, tenantID, clockID, ex, 1)
			return ids(subs), err
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Control: without the exclusion the oldest sub is the page.
			got, err := c.scan(nil)
			if err != nil {
				t.Fatalf("scan: %v", err)
			}
			if len(got) != 1 || got[0] != c.excluded {
				t.Fatalf("control: got %v, want [%s] (the oldest due sub)", got, c.excluded)
			}
			got, err = c.scan([]string{c.excluded})
			if err != nil {
				t.Fatalf("scan: %v", err)
			}
			if len(got) != 1 || got[0] != c.want {
				t.Fatalf("got %v, want [%s]: the excluded sub must be skipped before the LIMIT", got, c.want)
			}
		})
	}
}

func ids(subs []domain.Subscription) []string {
	out := make([]string, len(subs))
	for i, s := range subs {
		out[i] = s.ID
	}
	return out
}
