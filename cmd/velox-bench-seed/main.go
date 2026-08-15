// velox-bench-seed prepares a database for a load test and prints the API key
// to drive it with.
//
// It does NOT generate load. That job belongs to k6
// (scripts/bench-rig/ingest.js), for one reason worth stating plainly: a
// reader evaluating Velox should not have to trust our own load generator's
// timing code. Ours had two bugs — latency measured from send rather than from
// when the request was due, and every worker computing identical due times so
// they fired in lockstep and their self-inflicted queueing was counted as
// Velox's latency. k6 found the second one. A tool whose numbers you have to
// audit is worse than one you can point at.
//
// What remains here is the part k6 cannot do: create the tenant, customers and
// meters, and mint a key.
//
//	DATABASE_URL=... velox-bench-seed --customers 50 --meters 5
//	# prints: export VELOX_BENCH_KEY=vlx_secret_test_...
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"os"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/config"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

// Fixture identifiers. The external id and meter key are the handles the
// PUBLIC API uses, so k6 addresses customers and meters the way a customer's
// SDK does — by external_customer_id and event_name — rather than by the
// internal ids an in-process caller would pass straight through.
const (
	benchTenant             = "vlx_ten_bench"
	benchCustomer           = "vlx_cus_bench"
	benchMeter              = "vlx_mtr_bench"
	benchCustomerExternalID = "bench-customer"
	benchMeterKey           = "bench_tokens"
)

func main() {
	customers := flag.Int("customers", 50, "distinct customers to create")
	meters := flag.Int("meters", 5, "distinct meters to create")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	pool, err := config.OpenPostgres(cfg.DB)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer func() { _ = pool.Close() }()
	db := postgres.NewDB(pool, 30*time.Second)

	// Bench rows are synthetic — stamp them test-mode. TxTenant refuses to open
	// without an explicit livemode, so a bare context would fail every write.
	ctx := postgres.WithLivemode(context.Background(), false)

	seedFixtures(ctx, db, *customers, *meters)
	key := mintKey(ctx, db)

	fmt.Printf("tenant:     %s\n", benchTenant)
	fmt.Printf("customers:  %d (external ids %s-000 … %s-%03d)\n",
		*customers, benchCustomerExternalID, benchCustomerExternalID, *customers-1)
	fmt.Printf("meters:     %d (event names %s_000 … %s_%03d)\n",
		*meters, benchMeterKey, benchMeterKey, *meters-1)
	fmt.Printf("\nexport VELOX_BENCH_KEY=%s\n", key)
}

// seedFixtures is idempotent: deterministic ids mean repeated runs reuse the
// same rows rather than growing the customer table on every invocation.
func seedFixtures(ctx context.Context, db *postgres.DB, nCustomers, nMeters int) {
	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		log.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)

	if _, err = tx.ExecContext(ctx, `
		INSERT INTO tenants (id, name, status) VALUES ($1, 'velox-bench', 'active')
		ON CONFLICT (id) DO NOTHING`, benchTenant); err != nil {
		log.Fatalf("upsert tenant: %v", err)
	}
	for i := 0; i < nCustomers; i++ {
		if _, err = tx.ExecContext(ctx, `
			INSERT INTO customers (id, tenant_id, external_id, display_name, email, livemode)
			VALUES ($1, $2, $3, 'Bench Customer', 'bench@velox.local', false)
			ON CONFLICT (id) DO UPDATE SET livemode = false`,
			fmt.Sprintf("%s_%03d", benchCustomer, i), benchTenant,
			fmt.Sprintf("%s-%03d", benchCustomerExternalID, i)); err != nil {
			log.Fatalf("upsert customer %d: %v", i, err)
		}
	}
	for i := 0; i < nMeters; i++ {
		if _, err = tx.ExecContext(ctx, `
			INSERT INTO meters (id, tenant_id, name, key, unit, aggregation, livemode)
			VALUES ($1, $2, 'Bench Tokens', $3, 'tokens', $4, false)
			ON CONFLICT (id) DO UPDATE SET livemode = false`,
			fmt.Sprintf("%s_%03d", benchMeter, i), benchTenant,
			fmt.Sprintf("%s_%03d", benchMeterKey, i), string(domain.AggSum)); err != nil {
			log.Fatalf("upsert meter %d: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		log.Fatalf("commit: %v", err)
	}
}

// mintKey creates a test-mode secret key scoped to the bench tenant. Uses the
// real auth service rather than inserting a row by hand: the salt-and-hash
// format is auth's business and a hand-rolled copy here would silently stop
// matching the day it changes.
func mintKey(ctx context.Context, db *postgres.DB) string {
	svc := auth.NewService(auth.NewPostgresStore(db))
	res, err := svc.CreateKey(auth.WithLivemode(ctx, false), benchTenant, auth.CreateKeyInput{
		Name: "velox-bench", KeyType: auth.KeyTypeSecret,
	})
	if err != nil {
		log.Fatalf("mint api key: %v", err)
	}
	return res.RawKey
}
