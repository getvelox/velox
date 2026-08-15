// velox-bench is a synthetic ingest benchmark.
//
// It bootstraps (idempotently) a benchmark tenant, customer, and meter, then
// spawns N worker goroutines that call usage.Service.Ingest (or BatchIngest)
// directly — in-process, transactions and all. It does NOT go over HTTP, so
// the numbers exclude the router, auth middleware, JSON decoding, and the
// extra transactions the real request path carries. Treat the result as an
// upper bound on what an HTTP client would see, not as an end-to-end figure.
// Workers pick from a realistic dimension cardinality (10 models × 4
// operations × 2 cache states = 80 combinations) so the JSONB column behaves
// like a live AI-platform tenant rather than a synthetic single-row hammer.
//
// Two modes, and the difference decides whether the latency output means
// anything:
//
//   - Closed-loop (default). Workers spin as fast as they can. This finds
//     maximum throughput, and its latency percentiles describe a saturated
//     queue — useful for a ceiling, useless as an SLO.
//   - Open-loop (--rate N). Requests are offered on a fixed schedule
//     regardless of how fast the system responds, and latency is measured
//     from when each request was DUE, not from when it was sent. That
//     correction (for coordinated omission) is what stops an overloaded run
//     from reporting excellent latency while its backlog grows without bound.
//
// The gap between the two is not small and is not a rounding error: on the
// reference laptop, closed-loop reports ~3,000 events/sec while the rate that
// holds p99 under 50ms is ~1,800. Quote the second one.
//
// Usage:
//
//	DATABASE_URL="postgres://velox:velox@localhost:5432/velox?sslmode=disable" \
//	  go run ./cmd/velox-bench --workers 8 --duration 20s --rate 1800 --slo-p99 50ms
//
// Exits non-zero when --slo-p99 is set and the run misses it, so it can gate.
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
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/auth"
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
	// rate > 0 switches from closed-loop (spin as fast as possible) to
	// open-loop (hold a target arrival rate). The distinction decides whether
	// the latency numbers mean anything: a closed-loop run always ends up
	// saturated by construction, so its p99 measures how deep the queue got,
	// not how long the system takes to serve a request. An SLO number has to
	// come from a rate the system is NOT saturated at.
	rate := flag.Float64("rate", 0, "target events/sec (open-loop). 0 = closed-loop, spin as fast as possible")
	sloP99 := flag.Duration("slo-p99", 0, "pass/fail p99 latency budget for this run (0 = no verdict)")
	// --http switches from the in-process service call to the public HTTP
	// endpoint. Only this mode produces an end-to-end number: it adds the
	// router, auth middleware, JSON decode, and the customer/meter resolution
	// that the in-process path receives pre-resolved.
	httpBase := flag.String("http", "", "base URL of a running velox server (e.g. http://localhost:8080). Empty = in-process")
	apiKey := flag.String("api-key", os.Getenv("VELOX_API_KEY"), "Bearer key for --http mode (default $VELOX_API_KEY)")
	flag.Parse()

	if *batch < 1 {
		log.Fatalf("--batch must be >= 1")
	}
	if *rate < 0 {
		log.Fatalf("--rate must be >= 0")
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

	var snd sender
	if *httpBase != "" {
		// Mint a key for the BENCH tenant if the caller did not supply one.
		// The bootstrap tenant's key authenticates to a different tenant than
		// the bench fixtures live in, so borrowing it would 404 on every
		// request — the tool provisioning its own key keeps HTTP mode a
		// one-command setup instead of a manual dance.
		if *apiKey == "" {
			*apiKey = mintBenchAPIKey(ctx, db, tenantID)
			fmt.Printf("api key:    minted for %s (use --api-key to supply your own)\n", tenantID)
		}
		snd = &httpSender{
			client: newHTTPClient(*workers), baseURL: strings.TrimRight(*httpBase, "/"),
			apiKey: *apiKey, externalCustomerID: benchCustomerExternalID, eventName: benchMeterKey,
		}
	} else {
		snd = &inProcessSender{svc: svc, tenantID: tenantID, customerID: customerID, meterID: meterID}
	}

	fmt.Printf("velox-bench: workers=%d batch=%d duration=%s tenant=%s meter=%s\n",
		*workers, *batch, *duration, tenantID, meterID)
	fmt.Printf("transport:  %s\n", snd.describe())

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

	// Open-loop pacing. Each worker owns every Nth slot on a shared schedule
	// fixed at t=0, so the offered rate is independent of how fast the system
	// actually responds. callInterval is per CALL, so a batched run offers the
	// same EVENT rate with 1/batch as many calls.
	var callInterval time.Duration
	if *rate > 0 {
		callsPerSec := *rate / float64(*batch)
		callInterval = time.Duration(float64(time.Second) / callsPerSec * float64(*workers))
	}

	for i := 0; i < *workers; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(uint64(workerID), uint64(workerID)+1))
			samples := make([]time.Duration, 0, 4096)
			slot := 0
			for time.Now().Before(deadline) {
				// due is when this request SHOULD have been sent. When the
				// system falls behind, due is already in the past and the
				// wait shows up in the sample — that is the point.
				//
				// Timing from time.Now() after the sleep instead would hide
				// exactly the latency an overloaded system produces: every
				// request would look fast while the backlog grew without
				// bound. Measuring from the due time is the standard
				// correction for coordinated omission, and it is the
				// difference between a p99 a buyer can trust and one that
				// flatters us.
				var due time.Time
				if callInterval > 0 {
					due = start.Add(time.Duration(slot) * callInterval)
					slot++
					if d := time.Until(due); d > 0 {
						time.Sleep(d)
					}
				}
				evts := make([]event, *batch)
				for i := range evts {
					evts[i] = event{
						quantity:   decimal.NewFromInt(int64(rng.IntN(1000) + 1)),
						dimensions: pickDimensions(rng),
					}
				}

				t0 := time.Now()
				if callInterval > 0 {
					t0 = due
				}
				accepted, err := snd.send(ctx, evts)
				lat := time.Since(t0)
				if err != nil {
					atomic.AddInt64(&totalErrors, int64(len(evts)-accepted))
					firstErr.CompareAndSwap(nil, err.Error())
				}
				if accepted > 0 {
					atomic.AddInt64(&totalEvents, int64(accepted))
					// Latency is per CALL; per-event latency is this over batch
					// size. Recorded per call so p99 answers "how long does a
					// client wait", which is the operational question.
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
	if *rate > 0 {
		// Achieved vs offered is the first thing to read. If achieved is
		// materially below target the system did not hold the rate, and the
		// latency percentiles below describe a backlog rather than an SLO.
		fmt.Printf("mode:       open-loop, target %.0f events/sec\n", *rate)
		fmt.Printf("achieved:   %.0f events/sec (%.1f%% of target)\n", throughput, throughput / *rate * 100)
	} else {
		fmt.Printf("mode:       closed-loop (saturating; latency below is queueing, not service time)\n")
		fmt.Printf("throughput: %.0f events/sec\n", throughput)
	}
	if len(all) > 0 {
		fmt.Printf("p50:        %s\n", pct(all, 50))
		fmt.Printf("p95:        %s\n", pct(all, 95))
		fmt.Printf("p99:        %s\n", pct(all, 99))
		fmt.Printf("p99.9:      %s\n", pct(all, 99.9))
		fmt.Printf("max:        %s\n", all[len(all)-1].Round(time.Microsecond))
	}

	if *sloP99 > 0 && len(all) > 0 {
		p99 := percentile(all, 99)
		heldRate := *rate == 0 || throughput >= *rate*0.99
		switch {
		case p99 <= *sloP99 && heldRate:
			fmt.Printf("\nPASS: p99 %s within budget %s at %.0f events/sec\n",
				p99.Round(time.Microsecond), *sloP99, throughput)
		case p99 > *sloP99:
			fmt.Printf("\nFAIL: p99 %s exceeds budget %s\n", p99.Round(time.Microsecond), *sloP99)
			os.Exit(1)
		default:
			// Latency looked fine only because the offered rate was never
			// actually delivered. Reporting this as a pass would be the
			// single most misleading thing this tool could do.
			fmt.Printf("\nFAIL: p99 %s is within budget, but only %.0f of %.0f events/sec were "+
				"delivered — the system did not hold the offered rate, so this is not an SLO result\n",
				p99.Round(time.Microsecond), throughput, *rate)
			os.Exit(1)
		}
	}
}

// percentile returns the p-th percentile of a sorted slice.
func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	i := int(float64(len(sorted)) * p / 100)
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i]
}

// pct renders a percentile, or refuses to when there are too few samples to
// resolve it. A p99.9 computed from 324 samples is just the maximum wearing a
// percentile's name — batching makes this easy to hit, since it divides the
// call count by the batch size. Reporting it anyway is how a benchmark ends up
// quoting its own worst sample as a tail statistic.
func pct(sorted []time.Duration, p float64) string {
	if need := minSamplesFor(p); len(sorted) < need {
		return fmt.Sprintf("n/a (%d samples, need %d)", len(sorted), need)
	}
	return percentile(sorted, p).Round(time.Microsecond).String()
}

// minSamplesFor is how many samples a percentile needs before it is distinct
// from the maximum: 1/(1-p) puts one sample beyond it, and we ask for ten so
// the estimate rests on more than a single observation.
func minSamplesFor(p float64) int {
	if p >= 100 {
		return 0
	}
	return int(10.0 / (1.0 - p/100.0))
}

// Fixture identifiers. The external id and meter key are the handles the
// PUBLIC API uses — HTTP mode names the customer and meter the way a
// customer's SDK does, by external_customer_id and event_name, rather than by
// the internal ids the in-process path passes straight through.
const (
	benchTenant             = "vlx_ten_bench"
	benchCustomer           = "vlx_cus_bench"
	benchMeter              = "vlx_mtr_bench"
	benchCustomerExternalID = "bench-customer"
	benchMeterKey           = "bench_tokens"
)

// mintBenchAPIKey creates a test-mode secret key scoped to the bench tenant
// and returns the raw value. Uses the real auth service rather than inserting
// a row by hand: the salt-and-hash format is auth's business, and a
// hand-rolled copy here would silently stop matching the day that changes.
func mintBenchAPIKey(ctx context.Context, db *postgres.DB, tenantID string) string {
	svc := auth.NewService(auth.NewPostgresStore(db))
	// Bench events are test-mode; the key must be minted in the same mode or
	// it authenticates into the live partition and sees no bench fixtures.
	res, err := svc.CreateKey(auth.WithLivemode(ctx, false), tenantID, auth.CreateKeyInput{
		Name: "velox-bench", KeyType: auth.KeyTypeSecret,
	})
	if err != nil {
		log.Fatalf("mint bench api key: %v", err)
	}
	return res.RawKey
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
		VALUES ($1, $2, $3, 'Bench Customer', 'bench@velox.local', false)
		ON CONFLICT (id) DO UPDATE SET livemode = false
	`, benchCustomer, benchTenant, benchCustomerExternalID)
	if err != nil {
		log.Fatalf("upsert customer: %v", err)
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO meters (id, tenant_id, name, key, unit, aggregation, livemode)
		VALUES ($1, $2, 'Bench Tokens', $3, 'tokens', $4, false)
		ON CONFLICT (id) DO UPDATE SET livemode = false
	`, benchMeter, benchTenant, benchMeterKey, string(domain.AggSum))
	if err != nil {
		log.Fatalf("upsert meter: %v", err)
	}

	if err := tx.Commit(); err != nil {
		log.Fatalf("commit bootstrap: %v", err)
	}
	return benchTenant, benchCustomer, benchMeter
}
