package middleware_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	mw "github.com/sagarsuperuser/velox/internal/api/middleware"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	vtestutil "github.com/sagarsuperuser/velox/internal/testutil"
)

// TestQueueDepthGauges_EveryQueryRunsOnRealPostgres runs every scrape-time
// gauge's SQL against the real schema. A gauge whose query fails reports -1,
// which a dashboard can show for months before anyone reads it as "broken"
// instead of "busy". Wiring mirrors router.go: a BYPASS tx per scrape.
func TestQueueDepthGauges_EveryQueryRunsOnRealPostgres(t *testing.T) {
	db := vtestutil.SetupTestDB(t)
	var failed []string
	mw.RegisterQueueDepthGauges(func(query string) (float64, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
		if err != nil {
			return 0, err
		}
		defer postgres.Rollback(tx)
		var n float64
		if err := tx.QueryRowContext(ctx, query).Scan(&n); err != nil {
			failed = append(failed, err.Error()+"\n"+query)
			return 0, err
		}
		return n, nil
	})

	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	seen := map[string]bool{}
	for _, f := range families {
		for _, m := range f.GetMetric() {
			if g := m.GetGauge(); g != nil && g.GetValue() == -1 {
				t.Errorf("%s%v reports -1 (query failed)", f.GetName(), m.GetLabel())
			}
		}
		seen[f.GetName()] = true
	}
	for _, name := range []string{"velox_billing_due_subscriptions", "velox_billing_oldest_due_age_seconds", "velox_auto_charge_queued_invoices", "velox_parked_invoices", "velox_email_outbox_pending"} {
		if !seen[name] {
			t.Errorf("gauge %s not registered", name)
		}
	}
	if len(failed) > 0 {
		t.Errorf("failing gauge queries:\n%s", strings.Join(failed, "\n---\n"))
	}
}
