// velox-bench-seed prepares a database for an ingest benchmark and prints the
// credentials the load generator needs. It does not generate load.
//
// Load generation is k6's job (scripts/bench-rig/ingest.js). This binary exists
// because the two things k6 cannot do are exactly the two things that need
// Velox's own code: creating the bench tenant/customer/meter with the right
// livemode partition, and minting an API key whose salt-and-hash format is
// auth's business rather than a hand-rolled copy that silently stops matching.
//
// Usage:
//
//	DATABASE_URL="postgres://velox:velox@localhost:5432/velox?sslmode=disable" \
//	  go run ./cmd/velox-bench-seed
//
// Prints a JSON object to stdout:
//
//	{"api_key":"vlx_…","event_name":"bench_tokens",
//	 "customer_count":"200","customer_id_prefix":"vlx_cus_bench_",
//	 "external_customer_id_prefix":"bench-customer-",
//	 "external_customer_id":"bench-customer-000","customer_id":"vlx_cus_bench_000"}
//
// BENCH_CUSTOMERS (default 200) customers are created, not one. That number is
// the difference between a benchmark and a pathological case: with a single
// customer every event lands on one customer_id, so the resolve cache never
// misses, the btrees append to one hot edge, and — worst — the responsiveness
// probe's usage-summary aggregates EVERY row in the table for its one customer,
// which at 1.1M rows already costs ~180ms on a warm laptop and would report the
// product as DEGRADED on the AWS rig for a reason no real tenant would hit.
// Customers are numbered NNN so the load generator can pick one arithmetically.
//
// Idempotent — safe to re-run. The fixtures are test-mode (livemode=false),
// so a key minted here authenticates into the same partition the fixtures
// live in; minting in the other mode yields a key that sees no bench data.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"os"
	"strconv"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/config"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

const (
	benchTenant                 = "vlx_ten_bench"
	benchMeter                  = "vlx_mtr_bench"
	benchMeterKey               = "bench_tokens"
	benchCustomerIDPrefix       = "vlx_cus_bench_"
	benchCustomerExternalPrefix = "bench-customer-"
	defaultBenchCustomers       = 200
)

func benchLivemode() bool {
	switch os.Getenv("BENCH_LIVEMODE") {
	case "", "true", "1", "live":
		return true
	case "false", "0", "test":
		return false
	}
	log.Fatalf("BENCH_LIVEMODE must be true|false, got %q", os.Getenv("BENCH_LIVEMODE"))
	return true
}

func benchCustomerCount() int {
	if v := os.Getenv("BENCH_CUSTOMERS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
		log.Fatalf("BENCH_CUSTOMERS must be a positive integer, got %q", v)
	}
	return defaultBenchCustomers
}

func main() {
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

	// BENCH_LIVEMODE selects WHICH ingest path the benchmark measures, and the
	// default is the production one. Test mode is not merely a label: the
	// service does an extra per-event test-clock lookup in test mode that live
	// mode skips by design ("the high-volume production ingest path stays at
	// zero extra queries" — internal/usage/service.go simNow), so every number
	// measured in test mode includes a query production never makes. The bench
	// database is throwaway, so nothing about live mode here touches money.
	live := benchLivemode()
	// WithLivemode is read by the key-minting path (auth opens a TxTenant). It
	// does NOT reach the fixture INSERTs — see the note in bootstrapFixtures.
	ctx := postgres.WithLivemode(context.Background(), live)

	n := benchCustomerCount()
	bootstrapFixtures(ctx, db, n, live)

	out := map[string]string{
		"api_key":                     mintBenchAPIKey(ctx, db, live),
		"livemode":                    strconv.FormatBool(live),
		"event_name":                  benchMeterKey,
		"customer_count":              strconv.Itoa(n),
		"customer_id_prefix":          benchCustomerIDPrefix,
		"external_customer_id_prefix": benchCustomerExternalPrefix,
		// The first customer, for callers that only need one (bringup's smoke
		// test). The INTERNAL id is what /v1/usage-summary/{id} is keyed by.
		"external_customer_id": fmt.Sprintf("%s%03d", benchCustomerExternalPrefix, 0),
		"customer_id":          fmt.Sprintf("%s%03d", benchCustomerIDPrefix, 0),
	}
	if err := json.NewEncoder(os.Stdout).Encode(out); err != nil {
		log.Fatalf("write credentials: %v", err)
	}
}

// bootstrapFixtures ensures the benchmark tenant/customer/meter exist.
// Idempotent. Uses TxBypass because this is a CLI tool with full DB access;
// the runtime path sets tenant_id per-request via TxTenant as usual.
func bootstrapFixtures(ctx context.Context, db *postgres.DB, customers int, live bool) {
	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		log.Fatalf("begin bootstrap: %v", err)
	}
	defer postgres.Rollback(tx)

	// app.livemode MUST be set on this transaction explicitly.
	//
	// `customers` and `meters` carry the same BEFORE INSERT set_livemode
	// trigger as usage_events: it overwrites whatever the INSERT supplied,
	// reading the app.livemode GUC, which defaults to LIVE when unset. And
	// BeginTx sets that GUC only for TxTenant — the TxBypass branch sets
	// app.bypass_rls and nothing else. So postgres.WithLivemode on the context
	// is silently INERT here, and the fixtures land live-mode next to a
	// test-mode key, after which every ingest answers 422
	// `customer "bench-customer" not found`.
	//
	// Two things conspire to hide it. The trigger is INSERT-only, so the
	// ON CONFLICT DO UPDATE path below writes livemode=false correctly — which
	// means a SECOND run against the same database repairs it and looks fine.
	// A fresh database is the only one that shows the bug, and a fresh database
	// is exactly what a benchmark rig provisions.
	mode := "off"
	if live {
		mode = "on"
	}
	if _, err = tx.ExecContext(ctx, `SELECT set_config('app.livemode', $1, true)`, mode); err != nil {
		log.Fatalf("set livemode on bootstrap tx: %v", err)
	}

	if _, err = tx.ExecContext(ctx, `
		INSERT INTO tenants (id, name, status) VALUES ($1, 'velox-bench', 'active')
		ON CONFLICT (id) DO NOTHING
	`, benchTenant); err != nil {
		log.Fatalf("upsert tenant: %v", err)
	}

	// One statement for all customers, not a loop of round trips.
	if _, err = tx.ExecContext(ctx, `
		INSERT INTO customers (id, tenant_id, external_id, display_name, email, livemode)
		SELECT $1 || lpad(g::text, 3, '0'),
		       $2,
		       $3 || lpad(g::text, 3, '0'),
		       'Bench Customer ' || g,
		       'bench-' || g || '@velox.local',
		       $5
		FROM generate_series(0, $4 - 1) g
		ON CONFLICT (id) DO UPDATE SET livemode = $5
	`, benchCustomerIDPrefix, benchTenant, benchCustomerExternalPrefix, customers, live); err != nil {
		log.Fatalf("upsert customers: %v", err)
	}

	if _, err = tx.ExecContext(ctx, `
		INSERT INTO meters (id, tenant_id, name, key, unit, aggregation, livemode)
		VALUES ($1, $2, 'Bench Tokens', $3, 'tokens', $4, $5)
		ON CONFLICT (id) DO UPDATE SET livemode = $5
	`, benchMeter, benchTenant, benchMeterKey, string(domain.AggSum), live); err != nil {
		log.Fatalf("upsert meter: %v", err)
	}

	if err := tx.Commit(); err != nil {
		log.Fatalf("commit bootstrap: %v", err)
	}
}

// mintBenchAPIKey creates a test-mode secret key scoped to the bench tenant
// and returns the raw value. Uses the real auth service rather than inserting
// a row by hand: the salt-and-hash format is auth's business, and a
// hand-rolled copy here would silently stop matching the day that changes.
func mintBenchAPIKey(ctx context.Context, db *postgres.DB, live bool) string {
	svc := auth.NewService(auth.NewPostgresStore(db))
	// The key MUST be minted in the same mode as the fixtures, or it
	// authenticates into the other partition and sees no bench data.
	res, err := svc.CreateKey(auth.WithLivemode(ctx, live), benchTenant, auth.CreateKeyInput{
		Name: "velox-bench", KeyType: auth.KeyTypeSecret,
	})
	if err != nil {
		log.Fatalf("mint bench api key: %v", err)
	}
	return res.RawKey
}
