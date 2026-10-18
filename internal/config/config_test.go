package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestLoad_Defaults(t *testing.T) {
	// Explicitly control env to avoid .env file interference via Makefile
	t.Setenv("DATABASE_URL", "postgres://test:test@localhost/test")
	t.Setenv("PORT", "")
	t.Setenv("APP_ENV", "")
	t.Setenv("RUN_MIGRATIONS_ON_BOOT", "")
	t.Setenv("DB_MAX_OPEN_CONNS", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Port != "8080" {
		t.Errorf("port: got %q, want 8080", cfg.Port)
	}
	if cfg.Env != "local" {
		t.Errorf("env: got %q, want local", cfg.Env)
	}
	if cfg.Migrate != false {
		t.Error("migrate should default to false")
	}
	if cfg.DB.MaxOpenConns != 20 {
		t.Errorf("max_open_conns: got %d, want 20", cfg.DB.MaxOpenConns)
	}
}

func TestLoad_MissingDatabaseURL(t *testing.T) {
	_ = os.Unsetenv("DATABASE_URL")
	_ = os.Unsetenv("DB_HOST")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error when DATABASE_URL is missing")
	}
}

func TestLoad_CustomPort(t *testing.T) {
	_ = os.Setenv("DATABASE_URL", "postgres://test:test@localhost/test")
	_ = os.Setenv("PORT", "3000")
	defer func() { _ = os.Unsetenv("DATABASE_URL") }()
	defer func() { _ = os.Unsetenv("PORT") }()

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Port != "3000" {
		t.Errorf("port: got %q, want 3000", cfg.Port)
	}
}

func TestValidate_ProductionWarnsMissingRedis(t *testing.T) {
	cfg := Config{
		Port: "8080",
		Env:  "production",
		DB:   DBConfig{MaxOpenConns: 20, MaxIdleConns: 5, QueryTimeout: 5 * time.Second},
	}
	warnings := cfg.Validate()
	var found bool
	for _, w := range warnings {
		if w == "REDIS_URL is not set — in production the general and hosted-invoice rate limiters fail CLOSED (every covered request 429); boot refuses" {
			found = true
		}
	}
	if !found {
		t.Error("expected warning about missing REDIS_URL in production")
	}
}

func TestValidate_ValidConfig(t *testing.T) {
	t.Setenv("VELOX_ENCRYPTION_KEY", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")

	cfg := Config{
		Port:       "8080",
		Env:        "production",
		RedisURL:   "redis://localhost:6379",
		TrustProxy: "none",
		DB:         DBConfig{MaxOpenConns: 20, MaxIdleConns: 5, QueryTimeout: 5 * time.Second},
	}
	warnings := cfg.Validate()
	if len(warnings) > 0 {
		t.Errorf("expected no warnings for valid config, got: %v", warnings)
	}
}

func TestValidate_EncryptionKeyWarnings(t *testing.T) {
	_ = os.Unsetenv("VELOX_ENCRYPTION_KEY")
	cfg := Config{
		Env: "production", Port: "8080",
		RedisURL: "redis://localhost:6379",
		DB:       DBConfig{MaxOpenConns: 20, MaxIdleConns: 5, QueryTimeout: 5 * time.Second},
	}
	warnings := cfg.Validate()
	found := false
	for _, w := range warnings {
		if w == "VELOX_ENCRYPTION_KEY is not set — customer PII will be stored in plaintext" {
			found = true
		}
	}
	if !found {
		t.Error("expected warning about missing encryption key in production")
	}

	t.Setenv("VELOX_ENCRYPTION_KEY", "tooshort")
	warnings = cfg.Validate()
	found = false
	for _, w := range warnings {
		if len(w) > 20 && w[:20] == "VELOX_ENCRYPTION_KEY" {
			found = true
		}
	}
	if !found {
		t.Error("expected warning about invalid encryption key length")
	}

	t.Setenv("VELOX_ENCRYPTION_KEY", "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz")
	warnings = cfg.Validate()
	found = false
	for _, w := range warnings {
		if w == "VELOX_ENCRYPTION_KEY is not valid hex" {
			found = true
		}
	}
	if !found {
		t.Error("expected warning about invalid hex")
	}

	_ = os.Unsetenv("VELOX_ENCRYPTION_KEY")
	localCfg := Config{
		Env: "local", Port: "8080",
		DB: DBConfig{MaxOpenConns: 20, MaxIdleConns: 5, QueryTimeout: 5 * time.Second},
	}
	warnings = localCfg.Validate()
	for _, w := range warnings {
		if w == "VELOX_ENCRYPTION_KEY is not set — customer PII will be stored in plaintext" {
			t.Error("should not warn about missing encryption key in local env")
		}
	}
}

func TestValidate_DBPoolSanity(t *testing.T) {
	cfg := Config{
		Port: "8080",
		Env:  "local",
		DB:   DBConfig{MaxOpenConns: 5, MaxIdleConns: 10, QueryTimeout: 5 * time.Second},
	}
	warnings := cfg.Validate()
	found := false
	for _, w := range warnings {
		if w == "DB_MAX_IDLE_CONNS (10) exceeds DB_MAX_OPEN_CONNS (5) — idle conns will be capped" {
			found = true
		}
	}
	if !found {
		t.Error("expected warning about idle > open conns")
	}
}

func TestLoad_ProductionRequiresEncryptionKey(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://test:test@localhost/test")
	t.Setenv("APP_ENV", "production")
	t.Setenv("VELOX_ENCRYPTION_KEY", "")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error when VELOX_ENCRYPTION_KEY is missing in production")
	}
	if !strings.Contains(err.Error(), "VELOX_ENCRYPTION_KEY") {
		t.Errorf("error should mention encryption key, got: %v", err)
	}
}

func TestLoad_InvalidEncryptionKeyFormat(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://test:test@localhost/test")
	t.Setenv("APP_ENV", "local")
	t.Setenv("VELOX_ENCRYPTION_KEY", "tooshort")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for invalid encryption key length")
	}

	t.Setenv("VELOX_ENCRYPTION_KEY", "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz")
	_, err = Load()
	if err == nil {
		t.Fatal("expected error for invalid hex in encryption key")
	}
}

func TestLoad_DiscreteDBVars(t *testing.T) {
	_ = os.Unsetenv("DATABASE_URL")
	_ = os.Setenv("DB_HOST", "localhost")
	_ = os.Setenv("DB_NAME", "velox")
	_ = os.Setenv("DB_USER", "velox")
	_ = os.Setenv("DB_PASSWORD", "secret")
	defer func() {
		_ = os.Unsetenv("DB_HOST")
		_ = os.Unsetenv("DB_NAME")
		_ = os.Unsetenv("DB_USER")
		_ = os.Unsetenv("DB_PASSWORD")
	}()

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.DB.URL == "" {
		t.Error("DB URL should be constructed from discrete vars")
	}
}

// TestLoad_ProductionRequiresRedisURL pins HA-1 (2026-08-30): production
// refuses to boot without REDIS_URL, because the general and hosted-invoice
// limiters fail CLOSED without Redis and a Redis-less replica would boot
// green and 429 its share of traffic. Mutation-verify: change the env
// check in validateFatal to "staging" → this fails.
func TestLoad_ProductionRequiresRedisURL(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://test:test@localhost/test")
	t.Setenv("APP_DATABASE_URL", "postgres://app:app@localhost/test")
	t.Setenv("APP_ENV", "production")
	t.Setenv("VELOX_ENCRYPTION_KEY", strings.Repeat("ab", 32))
	t.Setenv("REDIS_URL", "")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error when REDIS_URL is missing in production")
	}
	if !strings.Contains(err.Error(), "REDIS_URL is required in production") {
		t.Errorf("error should name REDIS_URL, got: %v", err)
	}
}

func TestLoad_ProductionWithRedisURLLoads(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://test:test@localhost/test")
	t.Setenv("APP_DATABASE_URL", "postgres://app:app@localhost/test")
	t.Setenv("APP_ENV", "production")
	t.Setenv("VELOX_ENCRYPTION_KEY", strings.Repeat("ab", 32))
	t.Setenv("REDIS_URL", "redis://redis:6379")
	t.Setenv("TRUST_PROXY", "none")

	if _, err := Load(); err != nil {
		t.Fatalf("production with REDIS_URL set must load: %v", err)
	}
}

func TestLoad_StagingWithoutRedisLoadsFailOpen(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://test:test@localhost/test")
	t.Setenv("APP_ENV", "staging")
	t.Setenv("VELOX_ENCRYPTION_KEY", "")
	t.Setenv("REDIS_URL", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("staging without Redis must load (fail-open): %v", err)
	}
	for _, w := range cfg.Validate() {
		if strings.Contains(w, "REDIS_URL") {
			t.Errorf("staging must not warn about REDIS_URL, got %q", w)
		}
	}
}

// APP_ENV is normalised: "Production" must be treated as production
// everywhere, not slip past the fatal checks as an unrecognised value.
func TestLoad_AppEnvIsCaseNormalised(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://test:test@localhost/test")
	t.Setenv("APP_DATABASE_URL", "postgres://app:app@localhost/test")
	t.Setenv("APP_ENV", " Production ")
	t.Setenv("VELOX_ENCRYPTION_KEY", strings.Repeat("ab", 32))
	t.Setenv("REDIS_URL", "")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "REDIS_URL is required in production") {
		t.Fatalf("' Production ' must normalise to production and hit the fatal check, got: %v", err)
	}
}

// AppEnv is the one normalisation Load and `velox migrate rollback` share:
// the rollback check must call " Local " local, and an unset APP_ENV local.
func TestAppEnv(t *testing.T) {
	for raw, want := range map[string]string{"": "local", " Local ": "local", "staging": "staging", "PRODUCTION": "production"} {
		t.Setenv("APP_ENV", raw)
		if got := AppEnv(); got != want {
			t.Errorf("APP_ENV=%q: got %q, want %q", raw, got, want)
		}
	}
}

// SHUTDOWN_DRAIN_DELAY: unset defaults by environment, values parse as Go
// durations, junk is a load error rather than a silent default.
func TestLoad_ShutdownDrainDelay(t *testing.T) {
	cases := []struct {
		env, raw string
		want     time.Duration
		wantErr  bool
	}{
		{"local", "", 0, false},
		{"staging", "", 5 * time.Second, false},
		{"production", "", 5 * time.Second, false},
		{"production", "250ms", 250 * time.Millisecond, false},
		{"production", "0", 0, false},
		{"production", "junk", 0, true},
		{"production", "-1s", 0, true},
	}
	for _, c := range cases {
		t.Setenv("SHUTDOWN_DRAIN_DELAY", c.raw)
		got, err := loadShutdownDrainDelay(c.env)
		if (err != nil) != c.wantErr {
			t.Errorf("env=%s raw=%q: err=%v wantErr=%v", c.env, c.raw, err, c.wantErr)
			continue
		}
		if got != c.want {
			t.Errorf("env=%s raw=%q: got %v want %v", c.env, c.raw, got, c.want)
		}
	}
}

// INVOICE_GRACE_PERIOD (ADR-120): unset is 1h in every environment, a value is
// a Go duration from 0 to 72h, and anything else stops the boot rather than
// falling back to a default the operator did not choose.
func TestLoad_InvoiceGracePeriod(t *testing.T) {
	cases := []struct {
		raw     string
		want    time.Duration
		wantErr bool
	}{
		{"", time.Hour, false},
		{"0", 0, false},
		{"0s", 0, false},
		{"30m", 30 * time.Minute, false},
		{"72h", 72 * time.Hour, false},
		{"72h0m1s", 0, true},
		{"73h", 0, true},
		{"-1s", 0, true},
		{"1", 0, true}, // a bare number has no unit: not a duration
		{"junk", 0, true},
	}
	for _, c := range cases {
		t.Setenv("INVOICE_GRACE_PERIOD", c.raw)
		got, err := loadInvoiceGracePeriod()
		if (err != nil) != c.wantErr {
			t.Errorf("raw=%q: err=%v wantErr=%v", c.raw, err, c.wantErr)
			continue
		}
		if got != c.want {
			t.Errorf("raw=%q: got %v want %v", c.raw, got, c.want)
		}
	}

	// Through Load: the value lands on Config, and an out-of-range value
	// fails the load.
	t.Setenv("DATABASE_URL", "postgres://test:test@localhost/test")
	t.Setenv("APP_ENV", "local")
	t.Setenv("INVOICE_GRACE_PERIOD", "")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load with the default: %v", err)
	}
	if cfg.InvoiceGracePeriod != time.Hour {
		t.Errorf("Config.InvoiceGracePeriod = %v, want the 1h default", cfg.InvoiceGracePeriod)
	}
	t.Setenv("INVOICE_GRACE_PERIOD", "100h")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "INVOICE_GRACE_PERIOD") {
		t.Errorf("Load with 100h: err=%v, want an error naming INVOICE_GRACE_PERIOD", err)
	}
}

// TestLoad_ProductionRequiresTrustProxy: sweep-2026-08-30 S3. Behind a
// balancer with TRUST_PROXY unset, every per-IP bucket is one bucket for the
// whole install; production refuses to boot. "none" is the explicit "no
// proxy in front" declaration and passes.
func TestLoad_ProductionRequiresTrustProxy(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://test:test@localhost/test")
	t.Setenv("APP_ENV", "production")
	t.Setenv("VELOX_ENCRYPTION_KEY", strings.Repeat("ab", 32))
	t.Setenv("REDIS_URL", "redis://localhost:6379")

	t.Setenv("TRUST_PROXY", "")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "TRUST_PROXY") {
		t.Fatalf("production with TRUST_PROXY unset must refuse to boot naming TRUST_PROXY, got: %v", err)
	}
	for _, ok := range []string{"none", "10.0.0.0/8,127.0.0.1"} {
		t.Setenv("TRUST_PROXY", ok)
		if _, err := Load(); err != nil {
			t.Fatalf("TRUST_PROXY=%q must boot, got: %v", ok, err)
		}
	}
}

// A near-miss of "production" must refuse to boot. Every production safety
// check tests for the exact value "production", so "prod" used to start with
// all of them switched off. Other names (an install's own "dev" or "test")
// keep booting with the Validate warning.
func TestLoad_RejectsNearMissOfProduction(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://test:test@localhost/test")
	t.Setenv("VELOX_ENCRYPTION_KEY", "")
	t.Setenv("REDIS_URL", "")
	t.Setenv("TRUST_PROXY", "")

	for _, env := range []string{"prod", " Prod ", "production-eu", "prodution", "productoin"} {
		t.Setenv("APP_ENV", env)
		_, err := Load()
		if err == nil || !strings.Contains(err.Error(), "APP_ENV") {
			t.Errorf("APP_ENV=%q must refuse to boot naming APP_ENV, got: %v", env, err)
		}
	}
	for _, env := range []string{"", "local", "staging", " Staging ", "development", "dev", "test", "foo"} {
		t.Setenv("APP_ENV", env)
		if _, err := Load(); err != nil {
			t.Errorf("APP_ENV=%q must boot, got: %v", env, err)
		}
	}
}

// A TRUST_PROXY value the router cannot parse used to pass the "is it set"
// gate and then behave exactly as unset. Every entry must parse, in every
// environment.
func TestLoad_TrustProxyMustParse(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://test:test@localhost/test")
	t.Setenv("APP_DATABASE_URL", "postgres://app:app@localhost/test")
	t.Setenv("VELOX_ENCRYPTION_KEY", strings.Repeat("ab", 32))
	t.Setenv("REDIS_URL", "redis://localhost:6379")

	for _, env := range []string{"production", "local"} {
		t.Setenv("APP_ENV", env)
		for _, bad := range []string{"10.0.0.0/33", "10.0.0.0/8,bogus", "true", "lb.internal", "10.0.0.0-10.0.0.9", ",", "none,10.0.0.0/8"} {
			t.Setenv("TRUST_PROXY", bad)
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), "TRUST_PROXY") {
				t.Errorf("APP_ENV=%s TRUST_PROXY=%q must refuse to boot naming TRUST_PROXY, got: %v", env, bad, err)
			}
		}
		for _, ok := range []string{"none", " NONE ", "10.0.0.0/8", " 10.0.0.0/8 , 127.0.0.1 ", "fd00::/8,::1", "10.0.0.0/8,"} {
			t.Setenv("TRUST_PROXY", ok)
			if _, err := Load(); err != nil {
				t.Errorf("APP_ENV=%s TRUST_PROXY=%q must boot, got: %v", env, ok, err)
			}
		}
	}
}

// ParseTrustProxy is the one parser behind both the boot check and the
// router's trusted-proxy list, so the two cannot disagree about a value.
// The valid networks come back alongside the error naming the bad entries.
func TestParseTrustProxy(t *testing.T) {
	nets, err := ParseTrustProxy("10.0.0.0/8, 127.0.0.1 , , bogus, ::1, 10.0.0.0/33")
	if len(nets) != 3 {
		t.Errorf("parsed %d networks, want 3 (CIDR + two bare IPs)", len(nets))
	}
	if err == nil || !strings.Contains(err.Error(), `"bogus", "10.0.0.0/33"`) {
		t.Errorf("error must name both bad entries, got: %v", err)
	}
	for _, spec := range []string{"", "  ", "none", " None "} {
		if nets, err := ParseTrustProxy(spec); nets != nil || err != nil {
			t.Errorf("ParseTrustProxy(%q) = %v, %v; want nil, nil", spec, nets, err)
		}
	}
	if _, err := ParseTrustProxy(" , "); err == nil {
		t.Error("a value with no address in it must be an error")
	}
}

// A DB_* pool setting that is not an integer used to fall back to the default
// without a word. It is a configuration error, for the server and for the
// migrate command alike.
func TestLoad_RejectsUnparseableInt(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://test:test@localhost/test")
	t.Setenv("APP_ENV", "local")
	t.Setenv("VELOX_ENCRYPTION_KEY", "")
	t.Setenv("TRUST_PROXY", "")

	for _, key := range []string{
		"DB_MAX_OPEN_CONNS", "DB_MAX_IDLE_CONNS", "DB_CONN_MAX_LIFETIME_MIN",
		"DB_CONN_MAX_IDLE_TIME_SEC", "DB_PING_TIMEOUT_SEC", "DB_QUERY_TIMEOUT_MS",
	} {
		t.Run(key, func(t *testing.T) {
			t.Setenv(key, "5s")
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), key) {
				t.Errorf("Load with %s=5s must fail naming it, got: %v", key, err)
			}
			if _, err := LoadDBOnly(); err == nil || !strings.Contains(err.Error(), key) {
				t.Errorf("LoadDBOnly with %s=5s must fail naming it, got: %v", key, err)
			}
		})
	}

	t.Setenv("DB_MAX_OPEN_CONNS", " 40 ")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("a parseable value must load: %v", err)
	}
	if cfg.DB.MaxOpenConns != 40 {
		t.Errorf("max_open_conns: got %d, want 40", cfg.DB.MaxOpenConns)
	}
}
