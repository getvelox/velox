//go:build devsweep

package billing

// Throwaway pre-cutover evidence sweep (NOT committed): runs the
// ADR-101 shadow comparator over every active subscription's CURRENT
// period on a real database. Build-tagged so normal test runs never
// see it; run with:
//
//   SWEEP_DATABASE_URL=postgres://... go test -tags devsweep -run TestDevDBParitySweep ./internal/billing -v -count=1 -short=false

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/subscription"
	"github.com/sagarsuperuser/velox/internal/tenant"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestDevDBParitySweep(t *testing.T) {
	dsn := os.Getenv("SWEEP_DATABASE_URL")
	if dsn == "" {
		t.Skip("SWEEP_DATABASE_URL not set")
	}
	pool, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pool.Close()
	db := postgres.NewDB(pool, 30*time.Second)
	subStore := subscription.NewPostgresStore(db)

	e := &Engine{settings: tenant.NewSettingsStore(db)}
	e.intervalSnap = subStore
	e.intervalMode = IntervalReaderShadow

	ctx := context.Background()
	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT id, tenant_id, livemode, current_billing_period_start, current_billing_period_end
		FROM subscriptions
		WHERE status = 'active' AND current_billing_period_start IS NOT NULL AND current_billing_period_end IS NOT NULL`)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	type row struct {
		id, tenant string
		livemode   bool
		ps, pe     time.Time
	}
	var subs []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.tenant, &r.livemode, &r.ps, &r.pe); err != nil {
			t.Fatalf("scan: %v", err)
		}
		subs = append(subs, r)
	}
	_ = rows.Err()
	postgres.Rollback(tx)

	for _, r := range subs {
		sctx := postgres.WithLivemode(ctx, r.livemode)
		sub, err := subStore.Get(sctx, r.tenant, r.id)
		if err != nil {
			t.Fatalf("get %s: %v", r.id, err)
		}
		changes, err := subStore.ListItemChangesInPeriod(sctx, r.tenant, r.id, r.ps, r.pe)
		if err != nil {
			t.Fatalf("changes %s: %v", r.id, err)
		}
		byItem := map[string][]domain.SubscriptionItemChange{}
		for _, c := range changes {
			byItem[c.SubscriptionItemID] = append(byItem[c.SubscriptionItemID], c)
		}
		if _, err := e.windowSegments(sctx, sub, byItem, r.ps, r.pe); err != nil {
			t.Fatalf("windowSegments %s: %v", r.id, err)
		}
	}
	compared, allowlisted, unexplained := e.ShadowParityStats()
	t.Logf("SWEEP: subs=%d compared=%d allowlisted=%d unexplained=%d", len(subs), compared, allowlisted, unexplained)
	if unexplained != 0 {
		t.Fatalf("unexplained divergence on real data: %d (see WARN logs above)", unexplained)
	}
}
