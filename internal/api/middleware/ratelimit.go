package middleware

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/go-redis/redis_rate/v10"
	goredis "github.com/redis/go-redis/v9"

	"github.com/sagarsuperuser/velox/internal/api/respond"
	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

// RateLimiter implements distributed rate limiting using the GCRA (Generic Cell
// Rate Algorithm) backed by Redis. GCRA is a leaky-bucket variant that smooths
// traffic instead of allowing burst-then-block at window boundaries.
//
// Powered by go-redis/redis_rate — the official rate limiting companion for
// go-redis, used across the go-redis ecosystem.
type RateLimiter struct {
	limiter    *redis_rate.Limiter
	limit      redis_rate.Limit
	rdb        *goredis.Client
	failClosed bool
	// name namespaces this limiter's Redis keys. Distinct limiters (general
	// API, hosted-invoice, setup-link cooldown) share one Redis and key on the
	// same identifier (e.g. "ip:1.2.3.4"), but each has DIFFERENT GCRA
	// parameters. Without a per-limiter namespace they'd collide on one bucket
	// — a request to one surface would consume another's allowance and the
	// conflicting rate/period would corrupt the smoothing state.
	name string
}

// OpenRedis parses url, opens a client and verifies it with a Ping bounded
// by ctx. An empty url is not an error — it returns (nil, nil), the
// fail-open shape every limiter accepts. The caller decides the verdict:
// production REFUSES to boot on an error (cmd/velox/main.go), because
// the general and hosted-invoice limiters fail CLOSED without Redis and a
// replica would otherwise boot green and 429 its share of traffic; other
// environments warn and continue fail-open.
func OpenRedis(ctx context.Context, url string) (*goredis.Client, error) {
	if url == "" {
		return nil, nil
	}
	opt, err := goredis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("invalid REDIS_URL: %w", err)
	}
	c := goredis.NewClient(opt)
	if err := c.Ping(ctx).Err(); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("redis unreachable at %s: %w", opt.Addr, err)
	}
	return c, nil
}

// SetFailClosed controls what happens when Redis is unreachable or unconfigured.
// Default (false) — fail open: allow all requests. Appropriate for local/dev.
// true — fail closed: return 429 for every non-infra request. Use in production,
// where availability without rate limiting is a DDoS vector.
func (rl *RateLimiter) SetFailClosed(v bool) {
	rl.failClosed = v
}

// NewRateLimiter creates a Redis-backed GCRA rate limiter. name namespaces the
// limiter's Redis keys so distinct limiters with different parameters don't
// collide on a shared bucket (e.g. "general", "hosted_invoice", "setup_link").
// Example: NewRateLimiter(rdb, "general", 100, time.Minute) = 100 requests per
// minute with smooth distribution (no boundary bursts).
// If rdb is nil, the limiter fails open (all requests allowed).
func NewRateLimiter(rdb *goredis.Client, name string, rate int, window time.Duration) *RateLimiter {
	rl := &RateLimiter{rdb: rdb, name: name}

	// Convert rate + window to redis_rate.Limit
	switch {
	case window <= time.Second:
		rl.limit = redis_rate.PerSecond(rate)
	case window <= time.Minute:
		rl.limit = redis_rate.PerMinute(rate)
	default:
		rl.limit = redis_rate.Limit{
			Rate:   rate,
			Burst:  rate, // allow full rate as burst capacity
			Period: window,
		}
	}

	if rdb != nil {
		rl.limiter = redis_rate.NewLimiter(rdb)
	}

	return rl
}

// Middleware returns chi-compatible rate limiting middleware.
// Keys by tenant ID (from auth context) or IP address for unauthenticated requests.
func (rl *RateLimiter) Middleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Skip rate limiting for infrastructure endpoints
			if r.URL.Path == "/health" || r.URL.Path == "/health/ready" || r.URL.Path == "/metrics" {
				next.ServeHTTP(w, r)
				return
			}

			key := rateLimitKey(r)
			remaining, resetAt, allowed := rl.allow(r, key)

			// Set rate limit headers (Stripe convention)
			w.Header().Set("X-RateLimit-Limit", strconv.Itoa(rl.limit.Rate))
			w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(remaining))
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(resetAt.Unix(), 10))

			if !allowed {
				retryAfter := int(time.Until(resetAt).Seconds()) + 1
				if retryAfter < 1 {
					retryAfter = 1
				}
				w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
				respond.RateLimited(w, r)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// AllowKey is the exported variant of allow() for callers that need
// to enforce a custom-keyed bucket from within a handler (vs. as
// middleware). Used by paymentmethods.send-setup-email to enforce a
// per-customer cooldown — prevents double-click double-emails.
// Returns (remaining, resetAt, allowed); same fail-open / fail-closed
// semantics as the middleware path.
func (rl *RateLimiter) AllowKey(ctx context.Context, key string) (int, time.Time, bool) {
	now := time.Now().UTC()
	if rl.limiter == nil {
		if rl.failClosed {
			return 0, now.Add(rl.limit.Period), false
		}
		return rl.limit.Rate - 1, now.Add(rl.limit.Period), true
	}
	res, err := rl.limiter.Allow(ctx, rl.bucketKey(key), rl.limit)
	if err != nil {
		if rl.failClosed {
			slog.Error("rate_limiter: redis error on AllowKey, failing closed", "error", err, "key", key)
			return 0, now.Add(rl.limit.Period), false
		}
		slog.Warn("rate_limiter: redis error on AllowKey, failing open", "error", err, "key", key)
		return rl.limit.Rate - 1, now.Add(rl.limit.Period), true
	}
	return res.Remaining, now.Add(res.ResetAfter), res.Allowed > 0
}

// allow checks the rate limit for the given key.
// Returns (remaining, resetAt, allowed). On Redis absence or error, behavior
// depends on failClosed — see SetFailClosed.
func (rl *RateLimiter) allow(r *http.Request, key string) (int, time.Time, bool) {
	now := time.Now().UTC()

	if rl.limiter == nil {
		if rl.failClosed {
			slog.Error("rate_limiter: no redis configured, failing closed", "key", key)
			return 0, now.Add(rl.limit.Period), false
		}
		return rl.limit.Rate - 1, now.Add(rl.limit.Period), true
	}

	res, err := rl.limiter.Allow(r.Context(), rl.bucketKey(key), rl.limit)
	if err != nil {
		if rl.failClosed {
			slog.Error("rate_limiter: redis error, failing closed",
				"error", err,
				"key", key,
			)
			return 0, now.Add(rl.limit.Period), false
		}
		slog.Warn("rate_limiter: redis error, failing open",
			"error", err,
			"key", key,
		)
		return rl.limit.Rate - 1, now.Add(rl.limit.Period), true
	}

	resetAt := now.Add(res.ResetAfter)
	return res.Remaining, resetAt, res.Allowed > 0
}

// bucketKey is the Redis key for a logical bucket: the per-limiter namespace
// (name) keeps limiters with different GCRA parameters from colliding.
func (rl *RateLimiter) bucketKey(key string) string {
	return "rl:" + rl.name + ":" + key
}

// rateLimitKey chooses the most-specific bucket available for this request,
// Stripe-style:
//
//  1. API key id when present — per-key buckets so a runaway integration
//     can't starve the tenant's dashboard or a parallel worker. Each key
//     has a fixed livemode (vlx_secret_test_* vs vlx_secret_live_*) so
//     per-key keying is naturally per-mode.
//  2. Tenant id for session-authed UI traffic (no key attached). Suffixed
//     with the active mode so dashboard test-mode exploration and live-mode
//     operations do NOT share quota — Stripe's pattern. Without the suffix,
//     one operator's heavy test load could throttle another operator's
//     legitimate live action.
//  3. Remote IP for wholly-unauthenticated paths (bootstrap, login, public
//     health probes that slipped past the skip-list) so an unauthenticated
//     flood can't bypass the limiter entirely.
func rateLimitKey(r *http.Request) string {
	if keyID := auth.KeyID(r.Context()); keyID != "" {
		return "key:" + keyID
	}
	if tenantID := auth.TenantID(r.Context()); tenantID != "" {
		mode := "test"
		if postgres.Livemode(r.Context()) {
			mode = "live"
		}
		return "tenant:" + tenantID + ":" + mode
	}
	ip := r.RemoteAddr
	if host, _, err := net.SplitHostPort(ip); err == nil {
		ip = host
	}
	return "ip:" + ip
}

// SplitRateLimit routes each request to one of two limiters: `special`
// when match(r) is true, `base` otherwise. Backs the ingest-vs-CRUD
// split — the ingest surface (usage events + LiteLLM callbacks, one POST
// per LLM call) needs a bucket orders of magnitude larger than the
// operator-CRUD default, and squeezing it through the general bucket
// silently dropped revenue data (LiteLLM retries only on 5xx, so every
// 429 was a permanently lost event).
func SplitRateLimit(match func(*http.Request) bool, special, base *RateLimiter) func(http.Handler) http.Handler {
	specialMW := special.Middleware()
	baseMW := base.Middleware()
	return func(next http.Handler) http.Handler {
		sp := specialMW(next)
		bs := baseMW(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if match(r) {
				sp.ServeHTTP(w, r)
				return
			}
			bs.ServeHTTP(w, r)
		})
	}
}
