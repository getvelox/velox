package middleware

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

// maxIdempotentBodyBytes caps how much of the request body we hash into the
// fingerprint. Requests larger than this (e.g., bulk imports) skip the body
// portion of the fingerprint and fall back to method+path — those endpoints
// are rare and the risk of key reuse with different bodies is low.
const maxIdempotentBodyBytes = 1 << 20 // 1 MiB

// statusPending marks a reserved-but-not-yet-completed idempotency row. The
// request that claims the key writes this sentinel before running the handler;
// concurrent requests that find it poll until the owner overwrites it with the
// real status_code (always > 0). status_code is NOT NULL, so a sentinel value
// is how "in flight" is encoded without a separate column.
const statusPending = 0

// idempotencyPollInterval / idempotencyPollTimeout bound how long a losing
// concurrent request waits for the winning request to store its response
// before giving up with 409. The timeout is shorter than typical handler
// budgets — a request that's still pending after this either died mid-flight
// or is a genuinely slow operation the client should retry, not block on.
const (
	idempotencyPollInterval = 25 * time.Millisecond
	idempotencyPollTimeout  = 5 * time.Second

	// idempotencyOrphanAfter: a reservation still pending this long after it
	// was taken has no live owner — the longest handler this middleware wraps
	// is the 5-minute export group (router.go), everything else 30 s. The
	// owner died (OOM, SIGKILL, node loss) or panicked (Recoverer is mounted
	// outside this middleware) between reserving and finalizing, so the
	// reservation would otherwise sit at status 0 for its 24 h TTL while every
	// retry, on any replica, polls 5 s and is told "please retry" (sweep
	// 2026-08-30 S2). Whether the side effect committed is genuinely unknown
	// here (the handler's transaction may have committed before the kill), so
	// the reservation is NOT released for re-execution; the client is told
	// the truth and how to find out.
	idempotencyOrphanAfter = 5 * time.Minute
)

// Idempotency returns middleware that caches responses for POST/PUT/PATCH
// requests that include an Idempotency-Key header. If a request with the same
// key has been processed before, the cached response is returned.
//
// Stripe-compatible enforcement:
//   - Same key + same (method, path, body) → replay cached response
//     (sets Idempotent-Replayed: true header).
//   - Same key + different (method, path, body) → 422 idempotency_error
//     (protects against client bugs that recycle a key across operations —
//     e.g., retrying POST /invoices with a changed amount under the old key).
func Idempotency(db *postgres.DB) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "POST" && r.Method != "PUT" && r.Method != "PATCH" {
				next.ServeHTTP(w, r)
				return
			}

			key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
			if key == "" {
				next.ServeHTTP(w, r)
				return
			}

			tenantID := auth.TenantID(r.Context())
			if tenantID == "" {
				next.ServeHTTP(w, r)
				return
			}

			// Capture livemode off the request ctx so the post-response
			// cache write (which runs against context.Background, not
			// r.Context, so it survives a client disconnect) carries the
			// same mode. Without this, the BeginTx for the cache insert
			// defaults to live mode and a test-mode response gets stamped
			// with livemode=true on its cached row — corrupting future
			// replays for the test partition.
			livemode := postgres.Livemode(r.Context())

			// Read the body so we can (a) hash it for the fingerprint and
			// (b) hand a fresh reader to the downstream handler.
			bodyBytes, err := io.ReadAll(io.LimitReader(r.Body, maxIdempotentBodyBytes+1))
			if err != nil {
				ErrorJSON(w, http.StatusBadRequest, "bad_request", "failed to read body")
				return
			}
			_ = r.Body.Close()
			if len(bodyBytes) > maxIdempotentBodyBytes {
				// Body exceeds our hash cap — hash only the method+path+prefix
				// rather than refuse, so large requests aren't broken. The
				// fingerprint just becomes weaker (bodies matching on prefix
				// would false-match), which is acceptable given the rarity.
				bodyBytes = bodyBytes[:maxIdempotentBodyBytes]
			}
			r.Body = io.NopCloser(bytes.NewReader(bodyBytes))

			fingerprint := fingerprintRequest(r.Method, r.URL.Path, bodyBytes)

			// RESERVE the key before running the handler. A single INSERT ...
			// ON CONFLICT DO NOTHING is the serialization point: exactly one
			// concurrent request with the same (tenant, livemode, key) inserts
			// the pending row, every other request conflicts. Without this
			// reserve-before-act step, two simultaneous retries both passed the
			// old check-then-act read (no row yet) and both ran the side effect
			// — double credit-grant / double-charge.
			claimed, err := reserveKey(r.Context(), db, tenantID, key,
				r.Method, r.URL.Path, fingerprint)
			if err != nil {
				// DB error on the reserve — fail open (don't block the write).
				// An idempotency-infra failure shouldn't prevent the actual
				// operation; this matches the prior read-path fail-open. But it
				// IS an idempotency lapse: a concurrent/retried request can now
				// re-run the side effect, so make it observable rather than
				// silent — operators need to see it during a DB blip.
				idempotencyCacheErrors.WithLabelValues("reserve").Inc()
				slog.ErrorContext(r.Context(), "idempotency reserve failed; failing open (side effect may re-execute on retry)",
					"tenant_id", tenantID, "path", r.URL.Path, "error", err)
				next.ServeHTTP(w, r)
				return
			}
			if !claimed {
				// Someone else owns this key. It either already completed (or is
				// in flight from a concurrent request). Resolve it the same way
				// a serial retry would: replay the stored response, 422 on a
				// fingerprint mismatch, or 409 if it never resolves.
				replayExistingKey(w, r.Context(), db, tenantID, key, fingerprint)
				return
			}

			recorder := &responseRecorder{
				ResponseWriter: w,
				statusCode:     http.StatusOK,
				body:           &bytes.Buffer{},
			}
			next.ServeHTTP(recorder, r)

			// Finalize the reservation. Run against context.Background (with the
			// captured livemode) so the write survives a client disconnect — a
			// dropped connection after the side effect committed must still
			// persist the response, or the retry re-runs it.
			bgCtx := postgres.WithLivemode(context.Background(), livemode)

			// Cache 2xx/3xx/4xx/5xx except 409 and 422. The core value of
			// idempotency is that a transient 500 retry must not re-run side
			// effects — the first call may have already committed writes the
			// client can't observe (e.g., Stripe charge succeeded but our
			// response timed out). Without caching the 500, a retry under the
			// same key would double-charge.
			//
			// 409 Conflict and 422 Unprocessable Entity are excluded because
			// they signal "this isn't the real first response": 409 from
			// concurrent contention on shared state (a retry after the
			// contention clears may legitimately succeed), 422 typically
			// from input validation (the client usually fixes the body, and
			// our fingerprint check will flag that as a key-reuse error).
			// Pinning either would trap the caller — so we DELETE the pending
			// reservation instead, freeing the key for a clean retry.
			if recorder.statusCode == http.StatusConflict ||
				recorder.statusCode == http.StatusUnprocessableEntity {
				releaseKey(bgCtx, db, tenantID, key)
				return
			}
			finalizeKey(bgCtx, db, tenantID, key,
				recorder.statusCode, recorder.body.Bytes())
		})
	}
}

// fingerprintRequest returns a stable hash of the request's identifying
// parameters. Method+path keeps a key bound to a single endpoint; body keeps
// the same endpoint bound to the same payload.
func fingerprintRequest(method, path string, body []byte) []byte {
	h := sha256.New()
	h.Write([]byte(method))
	h.Write([]byte{0})
	h.Write([]byte(path))
	h.Write([]byte{0})
	h.Write(body)
	return h.Sum(nil)
}

type cachedResponse struct {
	StatusCode  int
	Body        []byte
	Fingerprint []byte
	CreatedAt   time.Time // when the key was reserved — a pending row's age
}

// reserveKey atomically claims the idempotency key by inserting a pending row
// (status_code = statusPending) with ON CONFLICT DO NOTHING. It reports whether
// THIS request won the claim: RowsAffected == 1 → claimed (proceed to handler),
// 0 → another request already owns the key (replay its outcome). This single
// statement is the serialization point that prevents two concurrent requests
// from both executing the side effect.
//
// livemode is omitted from the INSERT column list on purpose — the BEFORE
// INSERT trigger (set_livemode_from_session) stamps it from the tx session, so
// the row lands in the same mode partition the request runs under. The
// (tenant_id, livemode, key) primary key is the unique constraint the conflict
// resolves against.
func reserveKey(ctx context.Context, db *postgres.DB, tenantID, key, method, path string, fingerprint []byte) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return false, err
	}
	defer postgres.Rollback(tx)

	res, err := tx.ExecContext(ctx,
		`INSERT INTO idempotency_keys (key, tenant_id, http_method, http_path, request_fingerprint, status_code, response_body)
		VALUES ($1, $2, $3, $4, $5, $6, ''::bytea)
		ON CONFLICT (tenant_id, livemode, key) DO NOTHING`,
		key, tenantID, method, path, fingerprint, statusPending,
	)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n == 0 {
		// Conflict: another request holds the key. Nothing to commit.
		return false, nil
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// finalizeKey writes the captured response onto the pending reservation. The
// owning request always overwrites its own pending row, so an unconditional
// UPDATE on (tenant_id, key) is correct.
func finalizeKey(ctx context.Context, db *postgres.DB, tenantID, key string, statusCode int, body []byte) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	// A finalize failure leaves the row 'pending', so a retry under the same
	// key re-runs the handler (the response was never cached) — the exact
	// double-execution idempotency exists to prevent. Make every failure path
	// observable instead of swallowing it.
	fail := func(stage string, err error) {
		idempotencyCacheErrors.WithLabelValues("finalize").Inc()
		slog.ErrorContext(ctx, "idempotency finalize failed; response NOT cached, retry will re-run the handler",
			"stage", stage, "tenant_id", tenantID, "status_code", statusCode, "error", err)
	}

	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		fail("begin", err)
		return
	}
	defer postgres.Rollback(tx)

	if _, err := tx.ExecContext(ctx,
		`UPDATE idempotency_keys SET status_code = $3, response_body = $4
		WHERE tenant_id = $1 AND key = $2`,
		tenantID, key, statusCode, body,
	); err != nil {
		fail("update", err)
		return
	}
	if err := tx.Commit(); err != nil {
		fail("commit", err)
	}
}

// releaseKey deletes the pending reservation so a key whose handler returned a
// non-cacheable status (409/422) can be retried cleanly. Without this, the
// pending row would linger until expiry and every retry would 409.
func releaseKey(ctx context.Context, db *postgres.DB, tenantID, key string) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return
	}
	defer postgres.Rollback(tx)

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM idempotency_keys WHERE tenant_id = $1 AND key = $2 AND status_code = $3`,
		tenantID, key, statusPending,
	); err != nil {
		return
	}
	_ = tx.Commit()
}

// readKey loads the current state of a reserved/completed idempotency row.
// pending reports whether the owning request hasn't finalized yet
// (status_code == statusPending). found is false when no live row exists.
func readKey(ctx context.Context, db *postgres.DB, tenantID, key string) (c cachedResponse, found, pending bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return cachedResponse{}, false, false, err
	}
	defer postgres.Rollback(tx)

	err = tx.QueryRowContext(ctx,
		`SELECT status_code, response_body, request_fingerprint, created_at FROM idempotency_keys
		WHERE tenant_id = $1 AND key = $2 AND expires_at > now()`,
		tenantID, key,
	).Scan(&c.StatusCode, &c.Body, &c.Fingerprint, &c.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return cachedResponse{}, false, false, nil
	}
	if err != nil {
		return cachedResponse{}, false, false, err
	}
	if err := tx.Commit(); err != nil {
		return cachedResponse{}, false, false, err
	}
	return c, true, c.StatusCode == statusPending, nil
}

// replayExistingKey resolves a request that lost the reserve race. It enforces
// the fingerprint contract (422 on parameter mismatch), replays the stored
// response once the owner finalizes, and polls briefly while the owner is still
// in flight. If the owner never finalizes within the poll window — it died, or
// the operation is too slow to block on — it returns 409 conflict_idempotency,
// signaling the client to retry.
func replayExistingKey(w http.ResponseWriter, ctx context.Context, db *postgres.DB, tenantID, key string, fingerprint []byte) {
	deadline := time.Now().Add(idempotencyPollTimeout)
	for {
		c, found, pending, err := readKey(ctx, db, tenantID, key)
		if err != nil {
			// DB error resolving the conflict — fail open is unsafe here
			// (the side effect may already be running), so surface a 409 the
			// client retries rather than re-executing the operation.
			ErrorJSON(w, http.StatusConflict, "conflict_idempotency",
				"A request with this idempotency key is being processed. Please retry.")
			return
		}
		if found && !pending {
			// Fingerprint mismatch → key reused with different parameters.
			// Stripe returns 422 idempotency_error here.
			if len(c.Fingerprint) > 0 &&
				subtle.ConstantTimeCompare(c.Fingerprint, fingerprint) != 1 {
				ErrorJSON(w, http.StatusUnprocessableEntity, "idempotency_error",
					"Keys for idempotent requests can only be used with the same parameters they were first used with.")
				return
			}
			// A replay runs NO handler and executes NO mutation — it hands back
			// the response the FIRST request produced, and that request emitted
			// the audit row. Declare the request accounted-for so the
			// root-mounted audit-coverage detector doesn't read this 2xx as a
			// mutation that lost its row: the detector wraps this middleware
			// (the catch-all it replaces was mounted INSIDE it and never saw
			// replays), and without this every idempotent retry of a successful
			// mutation would be reported as an uncovered mutation.
			//
			// Emitting a second row here would be worse than the false alarm: it
			// would record a mutation that never happened, in an append-only log.
			audit.MarkSkip(ctx)

			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Idempotent-Replayed", "true")
			w.WriteHeader(c.StatusCode)
			_, _ = w.Write(c.Body)
			return
		}
		// Not found means the owner released the key (409/422 handler) — the
		// key is free again, but this request already lost the race; treat it
		// as a conflict so the client retries into a clean slot rather than
		// re-running concurrently.
		if !found {
			ErrorJSON(w, http.StatusConflict, "conflict_idempotency",
				"A concurrent request with this idempotency key did not complete. Please retry.")
			return
		}
		// Still pending — but with no owner alive? A reservation older than
		// any route budget was orphaned by a dead or panicked owner. Say so
		// at once: the outcome is unknown and polling cannot resolve it.
		if age := time.Since(c.CreatedAt); age > idempotencyOrphanAfter {
			ErrorJSON(w, http.StatusConflict, "conflict_idempotency_unresolved",
				fmt.Sprintf("A request with this idempotency key started %s ago and never recorded an outcome — the replica handling it may have died mid-request. Whether its side effect committed is unknown: fetch the resource to check, then retry with a NEW idempotency key only if it does not exist.", age.Round(time.Minute)))
			return
		}
		// Still pending. Wait for the owner to finalize, bounded by the poll
		// timeout and the request context.
		if time.Now().After(deadline) {
			ErrorJSON(w, http.StatusConflict, "conflict_idempotency",
				"A request with this idempotency key is still being processed. Please retry.")
			return
		}
		select {
		case <-ctx.Done():
			ErrorJSON(w, http.StatusConflict, "conflict_idempotency",
				"A request with this idempotency key is being processed. Please retry.")
			return
		case <-time.After(idempotencyPollInterval):
		}
	}
}

// responseRecorder captures the response for caching while writing to the client.
type responseRecorder struct {
	http.ResponseWriter
	statusCode int
	body       *bytes.Buffer
}

func (r *responseRecorder) WriteHeader(code int) {
	r.statusCode = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	r.body.Write(b)
	return r.ResponseWriter.Write(b)
}

// CleanExpired removes expired idempotency keys across all tenants and
// returns the number of rows deleted. Runs cross-tenant by design (a
// background scheduler, not a per-request path), so it uses TxBypass to
// sidestep the tenant_isolation policy.
func CleanExpired(ctx context.Context, db *postgres.DB) (int, error) {
	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return 0, err
	}
	defer postgres.Rollback(tx)

	res, err := tx.ExecContext(ctx, `DELETE FROM idempotency_keys WHERE expires_at < now()`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return int(n), nil
}

// IdempotencyCleaner adapts CleanExpired to the scheduler's cleaner
// interface (Cleanup(ctx) (int, error)), matching the shape used by
// payment.TokenService. Keeps scheduler wiring identical across cleanup
// tasks, and avoids the scheduler package importing anything from middleware.
type IdempotencyCleaner struct {
	db *postgres.DB
}

func NewIdempotencyCleaner(db *postgres.DB) *IdempotencyCleaner {
	return &IdempotencyCleaner{db: db}
}

func (c *IdempotencyCleaner) Cleanup(ctx context.Context) (int, error) {
	return CleanExpired(ctx, c.db)
}

// ErrorJSON is a helper that returns a Stripe-style error response for
// idempotency key misuse.
func ErrorJSON(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{
			"type":    errType,
			"message": message,
		},
	})
}
