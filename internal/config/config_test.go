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
		Port:     "8080",
		Env:      "production",
		RedisURL: "redis://localhost:6379",
		DB:       DBConfig{MaxOpenConns: 20, MaxIdleConns: 5, QueryTimeout: 5 * time.Second},
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
