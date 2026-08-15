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
//	{"api_key":"vlx_test_…","external_customer_id":"bench-customer","event_name":"bench_tokens"}
//
// Idempotent — safe to re-run. The fixtures are test-mode (livemode=false),
// so a key minted here authenticates into the same partition the fixtures
// live in; minting in the other mode yields a key that sees no bench data.
package main

import (
	"context"
	"encoding/json"
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

const (
	benchTenant             = "vlx_ten_bench"
	benchCustomer           = "vlx_cus_bench"
	benchMeter              = "vlx_mtr_bench"
	benchCustomerExternalID = "bench-customer"
	benchMeterKey           = "bench_tokens"
)

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
	ctx := context.Background()

	bootstrapFixtures(ctx, db)

	out := map[string]string{
		"api_key":              mintBenchAPIKey(ctx, db),
		"external_customer_id": benchCustomerExternalID,
		"event_name":           benchMeterKey,
	}
	if err := json.NewEncoder(os.Stdout).Encode(out); err != nil {
		log.Fatalf("write credentials: %v", err)
	}
}

// bootstrapFixtures ensures the benchmark tenant/customer/meter exist.
// Idempotent. Uses TxBypass because this is a CLI tool with full DB access;
// the runtime path sets tenant_id per-request via TxTenant as usual.
func bootstrapFixtures(ctx context.Context, db *postgres.DB) {
	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		log.Fatalf("begin bootstrap: %v", err)
	}
	defer postgres.Rollback(tx)

	if _, err = tx.ExecContext(ctx, `
		INSERT INTO tenants (id, name, status) VALUES ($1, 'velox-bench', 'active')
		ON CONFLICT (id) DO NOTHING
	`, benchTenant); err != nil {
		log.Fatalf("upsert tenant: %v", err)
	}

	if _, err = tx.ExecContext(ctx, `
		INSERT INTO customers (id, tenant_id, external_id, display_name, email, livemode)
		VALUES ($1, $2, $3, 'Bench Customer', 'bench@velox.local', false)
		ON CONFLICT (id) DO UPDATE SET livemode = false
	`, benchCustomer, benchTenant, benchCustomerExternalID); err != nil {
		log.Fatalf("upsert customer: %v", err)
	}

	if _, err = tx.ExecContext(ctx, `
		INSERT INTO meters (id, tenant_id, name, key, unit, aggregation, livemode)
		VALUES ($1, $2, 'Bench Tokens', $3, 'tokens', $4, false)
		ON CONFLICT (id) DO UPDATE SET livemode = false
	`, benchMeter, benchTenant, benchMeterKey, string(domain.AggSum)); err != nil {
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
func mintBenchAPIKey(ctx context.Context, db *postgres.DB) string {
	svc := auth.NewService(auth.NewPostgresStore(db))
	// Bench events are test-mode; the key must be minted in the same mode or
	// it authenticates into the live partition and sees no bench fixtures.
	res, err := svc.CreateKey(auth.WithLivemode(ctx, false), benchTenant, auth.CreateKeyInput{
		Name: "velox-bench", KeyType: auth.KeyTypeSecret,
	})
	if err != nil {
		log.Fatalf("mint bench api key: %v", err)
	}
	return res.RawKey
}
