// velox-bench is a synthetic ingest benchmark validating the 50k events/sec
// target from docs/design-multi-dim-meters.md.
//
// It bootstraps (idempotently) a benchmark tenant, customer, and meter,
// then spawns N worker goroutines that POST through usage.Service.Ingest
// — the real production path, transactions and all. Workers pick from a
// realistic dimension cardinality (10 models × 4 operations × 2 cache
// states = 80 combinations) so the JSONB column behaves like a live
// AI-platform tenant rather than a synthetic single-row hammer.
//
// Output: total events, throughput (events/sec), and latency p50/p95/p99.
//
// Usage:
//
//	DATABASE_URL="postgres://velox:velox@localhost:5432/velox?sslmode=disable" \
//	  go run ./cmd/velox-bench --workers 32 --duration 30s
//
// The benchmark is destructive: it writes a large number of rows to the
// usage_events table under a dedicated benchmark tenant. Drop and recreate
// the database between runs if you care about exact comparisons.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"math/rand/v2"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/config"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/usage"
)

func main() {
	workers := flag.Int("workers", 16, "concurrent ingest workers (default 16)")
	duration := flag.Duration("duration", 30*time.Second, "benchmark wall-clock duration")
	// batch=1 is the single-event path. Larger values exercise BatchIngest,
	// which commits ONE transaction per batch — the lever that matters,
	// because profiling puts ~two thirds of in-database time in COMMIT and
	// under a third in the INSERT itself. Statement count per event does not
	// change with batching; commits per event fall as 1/batch.
	batch := flag.Int("batch", 1, "events per ingest call (1 = single-event path)")
	flag.Parse()

	if *batch < 1 {
		log.Fatalf("--batch must be >= 1")
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	// Workers will saturate the pool — set max conns ≥ workers.
	if cfg.DB.MaxOpenConns < *workers {
		cfg.DB.MaxOpenConns = *workers
	}
	pool, err := config.OpenPostgres(cfg.DB)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer func() { _ = pool.Close() }()

	db := postgres.NewDB(pool, 30*time.Second)
	// Bench rows are synthetic — stamp them test-mode. TxTenant refuses to
	// open without an explicit livemode, so a bare context would fail every
	// ingest.
	ctx := postgres.WithLivemode(context.Background(), false)

	tenantID, customerID, meterID := bootstrapFixtures(ctx, db)
	store := usage.NewPostgresStore(db)
	svc := usage.NewService(store)

	fmt.Printf("velox-bench: workers=%d batch=%d duration=%s tenant=%s meter=%s\n",
		*workers, *batch, *duration, tenantID, meterID)

	var totalEvents int64
	var totalErrors int64
	// A benchmark that errors on every insert must say WHY, not just count —
	// the 2026-07-21 re-run failed 100% (missing livemode ctx) and the old
	// count-only summary gave nothing to debug with.
	var firstErr atomic.Value
	var wg sync.WaitGroup
	latencyChan := make(chan []time.Duration, *workers)

	deadline := time.Now().Add(*duration)
	start := time.Now()

	for i := 0; i < *workers; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(uint64(workerID), uint64(workerID)+1))
			samples := make([]time.Duration, 0, 4096)
			for time.Now().Before(deadline) {
				if *batch == 1 {
					props := pickDimensions(rng)
					qty := decimal.NewFromInt(int64(rng.IntN(1000) + 1))
					t0 := time.Now()
					_, err := svc.Ingest(ctx, tenantID, usage.IngestInput{
						CustomerID: customerID, MeterID: meterID,
						Quantity: qty, Dimensions: props,
					})
					lat := time.Since(t0)
					if err != nil {
						atomic.AddInt64(&totalErrors, 1)
						firstErr.CompareAndSwap(nil, err.Error())
						continue
					}
					atomic.AddInt64(&totalEvents, 1)
					samples = append(samples, lat)
					continue
				}

				inputs := make([]usage.IngestInput, *batch)
				for i := range inputs {
					inputs[i] = usage.IngestInput{
						CustomerID: customerID, MeterID: meterID,
						Quantity:   decimal.NewFromInt(int64(rng.IntN(1000) + 1)),
						Dimensions: pickDimensions(rng),
					}
				}
				t0 := time.Now()
				inserted, _, errs := svc.BatchIngest(ctx, tenantID, inputs)
				lat := time.Since(t0)
				if len(errs) > 0 {
					atomic.AddInt64(&totalErrors, int64(len(errs)))
					firstErr.CompareAndSwap(nil, errs[0].Error())
				}
				if inserted > 0 {
					atomic.AddInt64(&totalEvents, int64(inserted))
					// Latency is per CALL; per-event latency is this over
					// batch size. Recorded per call so p99 answers "how long
					// does a client wait", which is the operational question.
					samples = append(samples, lat)
				}
			}
			latencyChan <- samples
		}(i)
	}

	wg.Wait()
	close(latencyChan)
	elapsed := time.Since(start)

	all := make([]time.Duration, 0, totalEvents)
	for s := range latencyChan {
		all = append(all, s...)
	}
	slices.Sort(all)

	throughput := float64(totalEvents) / elapsed.Seconds()
	fmt.Printf("\n--- result ---\n")
	fmt.Printf("elapsed:    %s\n", elapsed.Round(time.Millisecond))
	fmt.Printf("events:     %d (errors: %d)\n", totalEvents, totalErrors)
	if e := firstErr.Load(); e != nil {
		fmt.Printf("first err:  %s\n", e)
	}
	fmt.Printf("throughput: %.0f events/sec\n", throughput)
	if len(all) > 0 {
		fmt.Printf("p50:        %s\n", all[len(all)*50/100].Round(time.Microsecond))
		fmt.Printf("p95:        %s\n", all[len(all)*95/100].Round(time.Microsecond))
		fmt.Printf("p99:        %s\n", all[len(all)*99/100].Round(time.Microsecond))
		fmt.Printf("max:        %s\n", all[len(all)-1].Round(time.Microsecond))
	}
	target := 50_000.0
	if throughput >= target {
		fmt.Printf("\n✓ target %.0f events/sec MET (%.1fx)\n", target, throughput/target)
	} else {
		fmt.Printf("\n✗ target %.0f events/sec MISSED (%.0f%%)\n", target, throughput/target*100)
		fmt.Printf("  see docs/design-multi-dim-meters.md 'Mitigations' for next steps\n")
	}
}

// pickDimensions returns a realistic AI-platform dimension set. Mirrors
// the cardinality assumed in the design doc benchmark plan: 10 models,
// 4 operations, 2 cache states (~80 unique combinations).
func pickDimensions(rng *rand.Rand) map[string]any {
	return map[string]any{
		"model":     models[rng.IntN(len(models))],
		"operation": operations[rng.IntN(len(operations))],
		"cached":    rng.IntN(2) == 0,
	}
}

var (
	models     = []string{"gpt-4", "gpt-4-turbo", "gpt-3.5-turbo", "claude-3-opus", "claude-3-sonnet", "claude-3-haiku", "gemini-pro", "llama-2-70b", "mistral-large", "command-r"}
	operations = []string{"input", "output", "embedding", "moderation"}
)

// bootstrapFixtures ensures the benchmark tenant/customer/meter exist.
// Idempotent: safe to run repeatedly. Uses TxBypass because we're in a
// CLI tool with full DB access; the runtime path will set tenant_id
// per-request via TxTenant as usual.
func bootstrapFixtures(ctx context.Context, db *postgres.DB) (string, string, string) {
	const (
		benchTenant   = "vlx_ten_bench"
		benchCustomer = "vlx_cus_bench"
		benchMeter    = "vlx_mtr_bench"
	)

	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		log.Fatalf("begin bootstrap: %v", err)
	}
	defer postgres.Rollback(tx)

	_, err = tx.ExecContext(ctx, `
		INSERT INTO tenants (id, name, status) VALUES ($1, 'velox-bench', 'active')
		ON CONFLICT (id) DO NOTHING
	`, benchTenant)
	if err != nil {
		log.Fatalf("upsert tenant: %v", err)
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO customers (id, tenant_id, external_id, display_name, email, livemode)
		VALUES ($1, $2, 'bench-customer', 'Bench Customer', 'bench@velox.local', false)
		ON CONFLICT (id) DO UPDATE SET livemode = false
	`, benchCustomer, benchTenant)
	if err != nil {
		log.Fatalf("upsert customer: %v", err)
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO meters (id, tenant_id, name, key, unit, aggregation, livemode)
		VALUES ($1, $2, 'Bench Tokens', 'bench_tokens', 'tokens', $3, false)
		ON CONFLICT (id) DO UPDATE SET livemode = false
	`, benchMeter, benchTenant, string(domain.AggSum))
	if err != nil {
		log.Fatalf("upsert meter: %v", err)
	}

	if err := tx.Commit(); err != nil {
		log.Fatalf("commit bootstrap: %v", err)
	}
	return benchTenant, benchCustomer, benchMeter
}
