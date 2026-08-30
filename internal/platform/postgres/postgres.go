package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/rs/xid"
)

const (
	DefaultQueryTimeout     = 5 * time.Second
	DefaultMigrationTimeout = 60 * time.Second
)

// DB wraps *sql.DB with query timeout and RLS-aware transactions.
type DB struct {
	Pool         *sql.DB
	QueryTimeout time.Duration
}

func NewDB(pool *sql.DB, queryTimeout time.Duration) *DB {
	if queryTimeout <= 0 {
		queryTimeout = DefaultQueryTimeout
	}
	return &DB{Pool: pool, QueryTimeout: queryTimeout}
}

// TxMode controls how a transaction sets RLS session variables.
type TxMode int

const (
	TxTenant TxMode = iota // Sets app.tenant_id, enforces RLS
	TxBypass               // Sets app.bypass_rls = on, skips RLS
)

// Context key for livemode propagation. Owned by the postgres package because
// BeginTx is the single choke point that needs to read it — having the key
// live here avoids circular imports between auth and postgres.
type livemodeCtxKey struct{}

// Stored as *bool so BeginTx can distinguish "caller explicitly chose live"
// from "nobody set this, defaulting to live". The public Livemode() still
// returns a plain bool and keeps the default-to-live fallback — only BeginTx
// and WithRequiredLivemode need to care about the set/unset distinction.

// WithLivemode returns a derived context carrying the mode flag. BeginTx
// reads this to set app.livemode on the tx session, which the RLS policy
// uses to filter rows by mode alongside tenant.
func WithLivemode(ctx context.Context, live bool) context.Context {
	return context.WithValue(ctx, livemodeCtxKey{}, &live)
}

// Livemode reads the livemode flag from ctx. Absent a value, defaults to
// true — the RLS policy interprets unset as "live mode" so background
// workers and bootstrap tooling that don't propagate mode operate safely
// against production data by default. Callers that need to know whether
// the value was set explicitly should use WithRequiredLivemode at their
// entry point instead of inspecting the return value here.
func Livemode(ctx context.Context) bool {
	p, ok := ctx.Value(livemodeCtxKey{}).(*bool)
	if !ok || p == nil {
		return true
	}
	return *p
}

// livemodeSet reports whether ctx had WithLivemode called on it anywhere up
// the chain. Package-private because it's a diagnostic for BeginTx and
// WithRequiredLivemode — callers that want the mode should use Livemode().
func livemodeSet(ctx context.Context) bool {
	p, ok := ctx.Value(livemodeCtxKey{}).(*bool)
	return ok && p != nil
}

// WithRequiredLivemode asserts that ctx has an explicit livemode set, and
// returns ctx unchanged if so. Panics otherwise. Call at the top of any
// background worker or scheduler path that opens a TxTenant — it catches
// "I forgot to fan out per mode" at the fan-out site instead of 30 frames
// deeper, where the bug would surface as silent test-mode data loss.
func WithRequiredLivemode(ctx context.Context) context.Context {
	if !livemodeSet(ctx) {
		panic("velox: background worker entered a mode-aware path without explicit livemode — wrap ctx with postgres.WithLivemode(ctx, true/false) at the fan-out site")
	}
	return ctx
}

// livemodeStrict reports whether the process should escalate unset-livemode
// warnings to panics. Enabled in tests (via VELOX_LIVEMODE_STRICT=true) so
// any test that opens a TxTenant without setting a mode fails loudly
// instead of silently routing to live. Default off in production — the
// warning is the signal, crashing a live install over a forgotten
// propagation is not.
func livemodeStrict() bool {
	return strings.EqualFold(os.Getenv("VELOX_LIVEMODE_STRICT"), "true")
}

// unsetLivemodeSeen tracks call sites that have already logged the
// "TxTenant opened without ctx livemode" warning. The warning is valuable
// the first time per site but noisy if fired for every tick of a long-
// running scheduler — dedup on caller file:line so operators get one log
// line per forgotten propagation, not one per request.
var unsetLivemodeSeen sync.Map // map[string]struct{}

// reportUnsetLivemode logs (or panics under strict mode) when a TxTenant
// opens without ctx livemode. Skip is the number of stack frames above
// this function to attribute the warning to — 2 lands on the caller of
// BeginTx, which is the interesting site.
func reportUnsetLivemode(skip int) {
	_, file, line, ok := runtime.Caller(skip)
	site := "unknown"
	if ok {
		site = fmt.Sprintf("%s:%d", file, line)
	}
	msg := "TxTenant opened without ctx livemode; defaulting to live. Wrap ctx with postgres.WithLivemode(ctx, true/false) at the entry point."
	if livemodeStrict() {
		panic(fmt.Sprintf("velox (VELOX_LIVEMODE_STRICT): %s caller=%s", msg, site))
	}
	if _, loaded := unsetLivemodeSeen.LoadOrStore(site, struct{}{}); loaded {
		return
	}
	slog.Warn("velox: livemode propagation missing", "caller", site, "detail", msg)
}

func (db *DB) BeginTx(ctx context.Context, mode TxMode, tenantID string) (*sql.Tx, error) {
	tx, err := db.Pool.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}

	switch mode {
	case TxTenant:
		// Every TxTenant MUST carry an explicit livemode. Silent default-
		// to-live is exactly the no-silent-fallbacks rule's failure mode —
		// a forgotten propagation in test-mode code would route the
		// operation against live data and corrupt accounting.
		//
		// Test-mode escalation: under VELOX_LIVEMODE_STRICT (set by
		// `make test*`) a missing propagation panics, so any new code
		// without a WithLivemode wrap fails the suite at PR time.
		//
		// Production: refuse to open the transaction. The handler bubbles
		// the error up as a 500 — loud and immediately visible to the
		// operator, never silently corrupting data. The Makefile-strict
		// test gate is what keeps this safe; the audit on 2026-05-07
		// fixed every known site, so a regression here means a NEW
		// missed propagation that bypassed tests.
		//
		// skip=3: runtime.Caller → reportUnsetLivemode → BeginTx → caller.
		if !livemodeSet(ctx) {
			reportUnsetLivemode(3)
			_ = tx.Rollback()
			return nil, fmt.Errorf("postgres: TxTenant opened without ctx livemode (set postgres.WithLivemode at the entry point)")
		}
		if _, err := tx.ExecContext(ctx, `SELECT set_config('app.bypass_rls', 'off', true)`); err != nil {
			_ = tx.Rollback()
			return nil, err
		}
		tid := strings.TrimSpace(tenantID)
		if tid == "" {
			tid = "default"
		}
		if _, err := tx.ExecContext(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tid); err != nil {
			_ = tx.Rollback()
			return nil, err
		}
		mode := "on"
		if !Livemode(ctx) {
			mode = "off"
		}
		if _, err := tx.ExecContext(ctx, `SELECT set_config('app.livemode', $1, true)`, mode); err != nil {
			_ = tx.Rollback()
			return nil, err
		}
	case TxBypass:
		if _, err := tx.ExecContext(ctx, `SELECT set_config('app.bypass_rls', 'on', true)`); err != nil {
			_ = tx.Rollback()
			return nil, err
		}
	}

	return tx, nil
}

func Rollback(tx *sql.Tx) {
	if tx != nil {
		_ = tx.Rollback()
	}
}

// WithTenantTx runs fn inside one tenant-scoped (TxTenant) transaction:
// commit when fn returns nil, rollback otherwise. It exists as the seam for
// cross-store coordinator transactions — a caller that must commit writes in
// two domain stores atomically (e.g. the billing engine's threshold
// fire + cycle re-anchor) passes the same *sql.Tx to each store's Tx
// variant. Unit tests fake the interface this satisfies with a runner that
// calls fn(nil); the store mocks ignore the tx handle.
func (db *DB) WithTenantTx(ctx context.Context, tenantID string, fn func(tx *sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, TxTenant, tenantID)
	if err != nil {
		return err
	}
	defer Rollback(tx)
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// NewID generates a prefixed, time-sortable ID (e.g., vlx_cus_cv2q6ktjml6ng3v2q0tg).
// Uses xid: globally unique, 20-char, URL-safe, naturally ordered by creation time.
func NewID(prefix string) string {
	return fmt.Sprintf("%s_%s", prefix, xid.New().String())
}

func IsUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	return false
}

// IsExclusionViolation reports whether err is a Postgres EXCLUDE-constraint
// violation (SQLSTATE 23P01) — the signature of two writers reconstructing
// the same range, e.g. two replicas running the boot-time billing_intervals
// backfill on one partition; the second committer sees this after the first
// commits, and the table is already correct.
func IsExclusionViolation(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23P01"
	}
	return false
}

// UniqueViolationConstraint returns the constraint name if err is a Postgres
// unique-violation (SQLSTATE 23505), otherwise "". Use this to disambiguate
// multiple unique constraints on the same table — e.g. subscriptions has
// both (tenant_id, code) and a partial-unique (tenant_id, customer_id, plan_id)
// for live statuses, and the callers need to surface distinct errors.
func UniqueViolationConstraint(err error) string {
	if err == nil {
		return ""
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return pgErr.ConstraintName
	}
	return ""
}

func IsForeignKeyViolation(err error) bool {
	if err == nil {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23503"
	}
	return false
}

// IsCheckViolation reports whether err is a Postgres check-constraint
// violation (SQLSTATE 23514). Used to translate DB-level invariant failures
// (e.g. the test_clocks livemode CHECK, or subscriptions_test_clock_requires_testmode)
// into user-facing 400s instead of leaking raw SQL error text.
func IsCheckViolation(err error) bool {
	if err == nil {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23514"
	}
	return false
}

func NullableString(v string) any {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	return v
}

// NullableStringPtr converts a *string into a sql-friendly value: nil
// (becomes SQL NULL) when the pointer is nil or points at an empty
// string, otherwise the dereferenced value. Saves callers from juggling
// sql.NullString for optional reference columns.
func NullableStringPtr(v *string) any {
	if v == nil {
		return nil
	}
	if strings.TrimSpace(*v) == "" {
		return nil
	}
	return *v
}

func NullableFloat64(v *float64) any {
	if v == nil {
		return nil
	}
	return *v
}

func NullableTime(v *time.Time) any {
	if v == nil {
		return nil
	}
	return *v
}

// StringArray is a []string that implements sql.Scanner and driver.Valuer
// for PostgreSQL TEXT[] columns.
type StringArray []string

// Value converts the StringArray to a PostgreSQL array literal.
func (a StringArray) Value() (interface{}, error) {
	if a == nil {
		return "{}", nil
	}
	if len(a) == 0 {
		return "{}", nil
	}
	elements := make([]string, len(a))
	for i, s := range a {
		// Escape double quotes and backslashes inside elements
		escaped := strings.ReplaceAll(s, `\`, `\\`)
		escaped = strings.ReplaceAll(escaped, `"`, `\"`)
		elements[i] = `"` + escaped + `"`
	}
	return "{" + strings.Join(elements, ",") + "}", nil
}

// Scan parses a PostgreSQL TEXT[] value into a StringArray.
func (a *StringArray) Scan(src interface{}) error {
	if src == nil {
		*a = StringArray{}
		return nil
	}
	var s string
	switch v := src.(type) {
	case string:
		s = v
	case []byte:
		s = string(v)
	default:
		return fmt.Errorf("StringArray.Scan: unsupported type %T", src)
	}
	// Trim outer braces
	s = strings.TrimSpace(s)
	if s == "{}" || s == "" {
		*a = StringArray{}
		return nil
	}
	s = s[1 : len(s)-1] // Remove { and }
	var result []string
	var current strings.Builder
	inQuote := false
	escaped := false
	for _, ch := range s {
		if escaped {
			current.WriteRune(ch)
			escaped = false
			continue
		}
		if ch == '\\' {
			escaped = true
			continue
		}
		if ch == '"' {
			inQuote = !inQuote
			continue
		}
		if ch == ',' && !inQuote {
			result = append(result, current.String())
			current.Reset()
			continue
		}
		current.WriteRune(ch)
	}
	if current.Len() > 0 {
		result = append(result, current.String())
	}
	*a = result
	return nil
}
