package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	neturl "net/url"

	"github.com/sagarsuperuser/velox/internal/api"
	mw "github.com/sagarsuperuser/velox/internal/api/middleware"
	"github.com/sagarsuperuser/velox/internal/billing"
	"github.com/sagarsuperuser/velox/internal/config"
	"github.com/sagarsuperuser/velox/internal/email"
	"github.com/sagarsuperuser/velox/internal/platform/migrate"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/platform/telemetry"
	"github.com/sagarsuperuser/velox/internal/subscription"
	"github.com/sagarsuperuser/velox/internal/testclock"
	"github.com/sagarsuperuser/velox/internal/version"
	"github.com/sagarsuperuser/velox/internal/webhook"
)

func main() {
	// Pin the process to UTC (ADR-075). pgx decodes timestamptz via time.Unix,
	// which lands in time.Local — so on a non-UTC host the DB read path returns
	// timestamps in the host zone, and time.Time.MarshalJSON then emits a
	// host-dependent offset (e.g. "+05:30") on the API wire instead of canonical
	// "…Z". The app already MINTS instants in UTC (clock.Now().UTC()); this makes
	// the DB READ path agree, so every serialized timestamp is host-independent
	// UTC. Must be the first statement — before any DB connection or goroutine —
	// so the assignment can't race a concurrent time.Local read. Billing date-math
	// is unaffected: it anchors in explicit zones (ADR-058/074), never time.Local.
	time.Local = time.UTC

	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}

	subcmd := ""
	if len(os.Args) > 2 {
		subcmd = os.Args[2]
	}

	switch cmd {
	case "serve":
		serve()
	case "migrate":
		switch subcmd {
		case "status":
			runMigrateStatus()
		case "rollback":
			runMigrateRollback()
		case "":
			runMigrate()
		default:
			fmt.Fprintf(os.Stderr, "unknown migrate subcommand: %s\navailable: status, rollback\n", subcmd)
			os.Exit(1)
		}
	case "version":
		fmt.Println("velox " + version.Version)
	case "help", "-h", "--help":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", cmd)
		printUsage()
		os.Exit(1)
	}
}

func serve() {
	// LOG_LEVEL controls the slog filter. Standard env name + values
	// (debug / info / warn / error) — production tunes verbosity via
	// the env var without a redeploy of a binary built for one level.
	// Default 'info' matches the prior hardcoded behavior. Unrecognized
	// values fall through to 'info' rather than failing boot — a typo
	// in an ops setting should never take the API offline.
	level := slog.LevelInfo
	switch strings.ToLower(strings.TrimSpace(os.Getenv("LOG_LEVEL"))) {
	case "debug":
		level = slog.LevelDebug
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	jsonHandler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})
	slog.SetDefault(slog.New(telemetry.NewContextHandler(jsonHandler)))

	cfg, err := config.Load()
	if err != nil {
		slog.Error("load config", "error", err)
		os.Exit(1)
	}

	// Initialize OpenTelemetry tracing (noop if OTEL_EXPORTER_OTLP_ENDPOINT not set)
	tracingShutdown, err := telemetry.Init(context.Background())
	if err != nil {
		slog.Error("init tracing", "error", err)
		os.Exit(1)
	}
	defer func() { _ = tracingShutdown(context.Background()) }()

	slog.Info("velox starting", "env", cfg.Env, "port", cfg.Port)

	pool, err := config.OpenPostgres(cfg.DB)
	if err != nil {
		slog.Error("open database", "error", err)
		os.Exit(1)
	}
	defer func() { _ = pool.Close() }()

	if cfg.Migrate {
		if cfg.Env == "production" {
			slog.Warn("RUN_MIGRATIONS_ON_BOOT=true in production — prefer a dedicated migration Job before rollout")
		}
		if err := migrate.Up(cfg.DB.URL); err != nil {
			slog.Error("run migrations", "error", err)
			os.Exit(1)
		}
	}

	// Refuse to start if schema is behind what this binary expects — catches
	// missed migration Jobs, races during rolling deploys, and dirty migrations.
	if err := migrate.CheckSchemaReady(pool); err != nil {
		slog.Error("schema check failed", "error", err)
		os.Exit(1)
	}

	appPool, closeAppPool := openAppPool(cfg, pool)
	defer closeAppPool()

	db := postgres.NewDB(appPool, cfg.DB.QueryTimeout)

	// Refuse to start behind a transaction-mode pooler: session advisory
	// locks (billing/dunning/outbox/webhook leader gates) silently strand
	// there — the first Release lands on the wrong server session, the
	// lock never frees, and every replica skips every tick forever. A
	// week of missing invoices with zero errors; a boot failure instead.
	if err := db.VerifyAdvisoryLockTopology(context.Background()); err != nil {
		slog.Error("connection topology check failed", "error", err)
		os.Exit(1)
	}

	// ADR-101 Phase 1: reconstruct billing_intervals for items that predate
	// the dual-write (idempotent — items with rows are skipped, so this is
	// a no-op on every boot after the first). Runs BEFORE serving so no
	// item mutation can race the reconstruction of its own history.
	if n, err := subscription.NewPostgresStore(db).BackfillBillingIntervals(context.Background()); err != nil {
		slog.Error("billing-intervals backfill failed", "error", err)
		os.Exit(1)
	} else if n > 0 {
		slog.Info("billing-intervals backfill complete", "intervals", n)
	}

	// Redis: production refuses to boot without a reachable one (the
	// general and hosted-invoice rate limiters fail CLOSED without Redis,
	// so a Redis-less replica would boot green and answer 429 to 1/N of
	// traffic — including hosted-invoice pay pages — while /health/ready
	// stays ok). config.validateFatal already rejects an UNSET REDIS_URL
	// in production; this catches invalid/unreachable. Other environments
	// warn and run fail-open.
	pingCtx, cancelPing := context.WithTimeout(context.Background(), 5*time.Second)
	rdb, err := mw.OpenRedis(pingCtx, cfg.RedisURL)
	cancelPing()
	if redisBootFatal(cfg.Env, err) {
		slog.Error("refusing to start: REDIS_URL is set but Redis is invalid or unreachable — in production the general and hosted-invoice rate limiters fail CLOSED without it (every covered request 429); fix REDIS_URL or the network path", "error", err)
		os.Exit(1)
	}
	if err != nil {
		slog.Warn("redis unavailable — rate limiting fails open in this environment", "env", cfg.Env, "error", err)
	} else if rdb != nil {
		slog.Info("redis connected for rate limiting")
	}

	server := api.NewServer(db, nil, rdb)

	billingInterval := 1 * time.Hour
	if cfg.Env == "local" {
		billingInterval = 5 * time.Minute
	}
	scheduler := billing.NewScheduler(server.BillingEngine, billingInterval, 50, server.DunningSvc, server.SettingsStore, nil, server.CreditSvc)
	// Every service below is constructed unconditionally by api.NewServer —
	// a nil can only mean a broken refactor. Wire unconditionally: the
	// scheduler's own tick-time nil-guards are a seam for TEST
	// configurations, and hiding a wiring bug behind them would skip a
	// money-path worker silently, forever. (Seven dead nil-guards here did
	// exactly that in theory until the 2026-08-11 module audit.)
	scheduler.SetTokenCleaner(server.TokenSvc)
	scheduler.SetIdempotencyCleaner(mw.NewIdempotencyCleaner(db))
	scheduler.SetTaxRetrier(server.InvoiceSvc)
	// Re-issue clawback credit-note drafts whose post-commit Issue() failed
	// (created atomically with a subscription downgrade/removal). ADR-056
	// follow-up — closes the post-commit fire-and-forget clawback gap.
	scheduler.SetClawbackRetrier(server.CreditNoteSvc)
	// Wall-clock trial expiry (Bug #8 — non-clock-pinned subs):
	// each tick, flip trialing subs to active at trial_end_at so
	// the dashboard doesn't lie about lifecycle state for up to
	// ~30 days past actual trial-end.
	scheduler.SetTrialExpirer(server.SubscriptionSvc)
	// Wall-clock pause-resume — clear pause_collection on subs
	// whose resumes_at has elapsed BEFORE the cycle scan reads the
	// due list. Stripe-parity (resume AT resumes_at, not next cycle
	// close). Without this, a paused sub whose next_billing_at is
	// in the future stays paused indefinitely.
	scheduler.SetPauseResumer(server.SubscriptionSvc)
	scheduler.SetPaymentReconciler(server.PaymentReconciler)
	// Leader gating: each replica tries the billing / dunning advisory locks
	// per tick; the winner runs the work, the losers skip. On crash the TCP
	// session drops and Postgres auto-releases — no zombie locks.
	scheduler.SetLocker(billing.NewPostgresLocker(db), postgres.LockKeyBillingScheduler, postgres.LockKeyDunningScheduler)

	// Wire scheduler health tracking so /health/ready can detect stalled schedulers
	api.SetSchedulerInterval(billingInterval)
	scheduler.SetOnRun(api.RecordSchedulerRun)

	// Test-clock async catchup (ADR-015). The Advance HTTP handler
	// flips the clock to status='advancing' and enqueues a job; this
	// worker drains the queue and runs the billing catchup
	// off-request. Buffer of 100 is generous — at expected operator
	// volumes there's never more than a handful of in-flight
	// advances.
	catchupQueue := testclock.NewCatchupQueue(100)
	server.TestClockSvc.SetCatchupQueue(catchupQueue)
	catchupWorker := testclock.NewCatchupWorker(catchupQueue, func(ctx context.Context, job testclock.CatchupJob) error {
		return server.TestClockSvc.RunCatchup(ctx, job)
	})

	srv := &http.Server{
		Addr:    fmt.Sprintf(":%s", cfg.Port),
		Handler: server,
		// ReadHeaderTimeout bounds header parsing (slowloris guard); ReadTimeout
		// gives the body a generous window so batch ingest (up to 1000 events)
		// over a slow link isn't truncated. WriteTimeout MUST exceed the 30s
		// request middleware.Timeout so the handler writes its clean 504 before
		// the server closes the socket (equal values race → connection reset).
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      35 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 13, // 8 KB
	}

	// Shutdown is a published three-stage budget — 90s worst case, typically
	// under two seconds — and every stage's bound abandons only work that is
	// designed to be resumed. Configure the orchestrator's grace period at
	// 120s (deploy/README.md 'Rolling deploys and shutdown'; compose sets
	// stop_grace_period). Docker's 10s default SIGKILLs mid-drain.
	const (
		shutdownHTTPDrain   = 30 * time.Second // stage 1: in-flight requests
		shutdownCatchupWait = 30 * time.Second // stage 2: an in-flight test-clock advance
		shutdownWorkerDrain = 30 * time.Second // stage 3: scheduler + dispatcher ticks
	)

	// Stage 0: the moment Shutdown is called, close every SSE subscriber so
	// the dashboard's live tail does not hold the HTTP drain open for its
	// full 30s bound (see EventBus.CloseAll).
	srv.RegisterOnShutdown(server.WebhookOutSvc.EventBus().CloseAll)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Track background workers so shutdown can wait for them to finish their
	// current tick before the process exits. Without this, a SIGTERM during a
	// billing run could leave partial work — half-generated invoices, ledger
	// entries without matching invoice updates, in-flight webhook deliveries.
	var workers sync.WaitGroup

	workers.Add(2)
	go func() {
		defer workers.Done()
		scheduler.Start(ctx)
	}()
	go func() {
		defer workers.Done()
		server.WebhookOutSvc.StartRetryWorker(ctx, 30*time.Second)
	}()

	// Test-clock catchup worker. Started AFTER the queue is wired
	// onto the service so any boot-recovered clock has somewhere to
	// land. Recovery is best-effort: stuck clocks from a prior
	// process get re-enqueued; the worker drains them like any other
	// job. If recovery itself errors, log loudly but don't block
	// boot — operators can still create new clocks; only legacy
	// stuck ones miss out, and they can be deleted manually.
	catchupWorker.Start()
	recoveryCtx, recoveryCancel := context.WithTimeout(context.Background(), 30*time.Second)
	if err := server.TestClockSvc.RecoverInFlight(recoveryCtx); err != nil {
		slog.Error("test-clock catchup recovery failed", "error", err)
	}
	recoveryCancel()

	// Outbox dispatchers (always-on per ADR-040). Webhook outbox drains
	// webhook_outbox → Service.Dispatch; email outbox drains email_outbox
	// → *email.Sender. The dispatchers gate each tick on a cluster-wide
	// advisory lock (postgres.LockKeyOutboxDispatcher / LockKeyEmailDispatcher)
	// so multi-replica deploys stay safe — only one replica drains at a
	// time. Operators who need to pause delivery can hold the lock from
	// an external psql session; restart isn't required.
	workers.Add(1)
	go func() {
		defer workers.Done()
		dispatcher := webhook.NewDispatcher(server.OutboxStore, server.WebhookOutSvc, webhook.DispatcherConfig{})
		dispatcher.SetLocker(server.OutboxStore)
		dispatcher.Start(ctx)
	}()

	workers.Add(1)
	go func() {
		defer workers.Done()
		dispatcher := email.NewDispatcher(server.EmailOutboxStore, server.EmailSender, email.DispatcherConfig{})
		dispatcher.SetSettledChecker(server.EmailSettledChecker)
		dispatcher.SetLocker(server.EmailOutboxStore)
		dispatcher.Start(ctx)
	}()

	go func() {
		slog.Info("listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	slog.Info("shutting down — announcing drain, then draining workers and in-flight requests",
		"drain_delay", cfg.ShutdownDrainDelay)

	// Readiness-first: /health/ready answers 503 'draining' from now on
	// while /health (liveness) stays 200, so a health-check-driven load
	// balancer stops routing to this replica BEFORE the listener closes —
	// otherwise every rolling deploy ends with connection resets on the
	// draining replica. Keep listening for the configured delay so the
	// balancer's probes observe the 503 (orchestrator-driven removal, e.g.
	// Kubernetes endpoints, is immediate — the delay just covers the
	// health-check path). 0 in local.
	api.SetDraining()
	if cfg.ShutdownDrainDelay > 0 {
		time.Sleep(cfg.ShutdownDrainDelay)
	}

	// Stage 1 (≤ 30s): stop accepting new HTTP requests, drain in-flight
	// ones. RegisterOnShutdown above closes the SSE subscribers first, so
	// an open live tail no longer pins this stage to its bound.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownHTTPDrain)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("http shutdown error", "error", err)
	}

	// Stage 2 (≤ 30s): the test-clock catch-up worker. Catch-up runs on
	// context.Background() (an in-flight advance must not be cut short
	// mid-write), so cancellation does not reach it; Stop waits up to
	// shutdownCatchupWait for the in-flight advance — typical advances
	// finish in seconds — then abandons it. An abandoned advance leaves the
	// clock 'advancing' and resumes from durable state on the next boot's
	// RecoverInFlight; nothing is lost, only delayed. (Was CatchupTimeout =
	// 10 min, which blew every orchestrator grace period.)
	if !catchupWorker.Stop(shutdownCatchupWait) {
		slog.Warn("test-clock catchup worker still busy — abandoning the in-flight advance; it resumes on the next boot")
	}

	// Stage 3 (≤ 30s): scheduler + dispatchers. Their ticks run on ctx
	// (cancelled above) and abort atomically at their next DB call; this
	// wait exists for the WithoutCancel completions — a Stripe charge
	// bounded by the 30s client timeout, an outbox row's detached mark —
	// so a mid-tick run lands its result instead of leaving a claimed row
	// for the lease to expire. Bounded so a hung worker cannot prevent exit.
	done := make(chan struct{})
	go func() {
		workers.Wait()
		close(done)
	}()
	select {
	case <-done:
		slog.Info("workers drained cleanly")
	case <-time.After(shutdownWorkerDrain):
		slog.Warn("workers did not drain within the budget — forcing exit; claimed rows resume via their leases")
	}
}

// loadDSN loads just the database URL for migrate subcommands. Skips
// Stripe/Redis/encryption validation that's irrelevant for migrate work.
func loadDSN() string {
	cfg, err := config.LoadDBOnly()
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config: %v\n", err)
		os.Exit(1)
	}
	return cfg.URL
}

func runMigrate() {
	if err := migrate.Up(loadDSN()); err != nil {
		fmt.Fprintf(os.Stderr, "migration failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("migrations applied successfully")
}

func runMigrateStatus() {
	v, dirty, err := migrate.Status(loadDSN())
	if err != nil {
		fmt.Fprintf(os.Stderr, "status failed: %v\n", err)
		os.Exit(1)
	}
	if v == 0 {
		fmt.Println("no migrations applied")
		return
	}
	fmt.Printf("version: %d, dirty: %v\n", v, dirty)
}

func runMigrateRollback() {
	v, err := migrate.Rollback(loadDSN(), 1)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rollback failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("rolled back to version %d\n", v)
}

func printUsage() {
	fmt.Println(`velox — usage-based billing engine

Commands:
  serve                Start the API server (default)
  migrate              Apply pending migrations
  migrate status       Show current migration version
  migrate rollback     Rollback the latest migration
  version              Print version
  help                 Show this help

Environment:
  DATABASE_URL              PostgreSQL connection string — admin/migration role (or the
                            split DB_HOST/DB_NAME/DB_USER/DB_PASSWORD shape; full list in
                            deploy/compose/.env.example)
  APP_DATABASE_URL          Runtime connection for the least-privilege velox_app role (RLS
                            enforced). Used verbatim; REQUIRED in staging/production. Local
                            dev derives velox_app:velox_app from DATABASE_URL when unset.
  PORT                      HTTP port (default: 8080)
  APP_ENV                   Environment: local, staging, production (default: local)
  RUN_MIGRATIONS_ON_BOOT    Run migrations on server start (default: false)
  VELOX_BOOTSTRAP_TOKEN     Token for POST /v1/bootstrap endpoint (min 16 chars outside local)
  PAYMENT_UPDATE_URL        Base URL for payment update page (e.g. https://app.example.com/update-payment)
  PAYMENT_UPDATE_RETURN_URL Where Stripe Checkout returns after the public payment-update flow; must be a real SPA route, e.g. https://app.example.com/payment-method-added (handler appends ?status=success|cancel)
  VELOX_ENCRYPTION_KEY      64-char hex key for PII encryption at rest
  REDIS_URL                 Redis URL for distributed rate limiting`)
}

// deriveAppURL replaces the user:password in a DATABASE_URL with velox_app:velox_app.
// Returns "" if the URL can't be parsed. LOCAL DEV ONLY (ADR-073) —
// staging/production require an explicit APP_DATABASE_URL.
func deriveAppURL(adminURL string) string {
	u, err := neturl.Parse(adminURL)
	if err != nil {
		return ""
	}
	u.User = neturl.UserPassword("velox_app", "velox_app")
	return u.String()
}

// resolveAppURL applies ADR-073's fail-closed matrix for the runtime
// (RLS-enforced) pool. Returns the URL to open, or fallbackWarn != ""
// meaning "use the admin pool and log this" (local dev only), or an
// error that must abort boot. Pure so the matrix is unit-testable.
func resolveAppURL(env, adminURL, appURL string) (url, fallbackWarn string, err error) {
	local := env == "local"

	if appURL != "" {
		u, perr := neturl.Parse(appURL)
		if perr != nil || u.User == nil {
			return "", "", fmt.Errorf("APP_DATABASE_URL is not a parseable connection URL with credentials: %v", perr)
		}
		username := u.User.Username()
		password, _ := u.User.Password()
		if !local {
			// The publicly documented default and the no-credential
			// trust-auth shape both hand cross-tenant read/write to
			// anyone with TCP reach — the exact exposure HIGH #9
			// flagged. Refuse outside local dev.
			switch password {
			case "":
				return "", "", fmt.Errorf("APP_DATABASE_URL has no password (trust-auth shape) — forbidden in %s; set a real password for the app role (ALTER ROLE %s PASSWORD ...)", env, username)
			case "velox_app", username:
				return "", "", fmt.Errorf("APP_DATABASE_URL uses the default/guessable password for role %q — forbidden in %s; rotate it (openssl rand -hex 24, then ALTER ROLE ... PASSWORD) and update APP_DATABASE_URL", username, env)
			}
		}
		return appURL, "", nil
	}

	if !local {
		return "", "", fmt.Errorf("APP_DATABASE_URL is required in %s: the runtime pool must be a least-privilege role with its own password, not one derived from DATABASE_URL with the documented default — see docs/self-host.md", env)
	}

	derived := deriveAppURL(adminURL)
	if derived == "" || derived == adminURL {
		return "", "running with admin database connection — RLS NOT enforced. Create the velox_app role.", nil
	}
	return derived, "", nil
}

// checkRLSCapability reports whether the pool's role can bypass RLS.
// Superuser and BYPASSRLS defeat row-level security wholesale; table
// ownership does not (every RLS table is FORCE ROW LEVEL SECURITY).
func checkRLSCapability(pool *sql.DB) (role string, canBypass bool, err error) {
	err = pool.QueryRow(
		`SELECT current_user, (rolsuper OR rolbypassrls) FROM pg_roles WHERE rolname = current_user`,
	).Scan(&role, &canBypass)
	return role, canBypass, err
}

// openAppPool returns the connection used for request-time queries,
// where RLS must be enforced (ADR-073). APP_DATABASE_URL is honored
// verbatim when set; staging/production refuse to boot without it, with
// a default/empty password, or with a role that can bypass RLS — the
// capability check is what catches the path of least resistance
// (copying DATABASE_URL into APP_DATABASE_URL), which no string check
// can see. Local dev keeps the velox_app:velox_app derivation and
// warn-and-fallback. The returned cleanup is a noop when falling back.
func openAppPool(cfg config.Config, adminPool *sql.DB) (*sql.DB, func()) {
	noop := func() {}

	appURL, fallbackWarn, err := resolveAppURL(cfg.Env, cfg.DB.URL, cfg.DB.AppURL)
	if err != nil {
		slog.Error("refusing to start: "+err.Error(), "env", cfg.Env)
		os.Exit(1)
	}
	if fallbackWarn != "" {
		slog.Warn(fallbackWarn)
		return adminPool, noop
	}

	appCfg := cfg.DB
	appCfg.URL = appURL
	appPool, err := config.OpenPostgres(appCfg)
	if err != nil {
		// An EXPLICIT APP_DATABASE_URL that doesn't work is fatal in
		// every env — explicit config never silently degrades. Only the
		// local derived URL keeps warn-and-fallback (role not created).
		if cfg.DB.AppURL != "" || cfg.Env != "local" {
			slog.Error("refusing to start: could not open the app database connection, so RLS would not be enforced and tenants would not be isolated. Check APP_DATABASE_URL and that the role exists with LOGIN — see docs/self-host.md.", "error", err, "env", cfg.Env)
			os.Exit(1)
		}
		slog.Warn("could not open app database connection, falling back to admin", "error", err)
		return adminPool, noop
	}

	role, canBypass, err := checkRLSCapability(appPool)
	if err != nil {
		slog.Error("refusing to start: could not verify the app role's RLS posture", "error", err, "env", cfg.Env)
		os.Exit(1)
	}
	if canBypass {
		if cfg.Env != "local" {
			slog.Error("refusing to start: the APP_DATABASE_URL role can BYPASS row-level security (superuser or BYPASSRLS) — tenants would not be isolated. Point APP_DATABASE_URL at a least-privilege role like velox_app, not the admin role — see docs/self-host.md.", "role", role, "env", cfg.Env)
			os.Exit(1)
		}
		slog.Warn("app database role can bypass RLS — acceptable in local dev only", "role", role)
	}

	slog.Info("using app database connection (RLS enforced)", "role", role)
	return appPool, func() { _ = appPool.Close() }
}

// redisBootFatal is the one place the Redis boot verdict lives: an error
// opening/pinging Redis is fatal in production only.
func redisBootFatal(env string, err error) bool {
	return err != nil && env == "production"
}
