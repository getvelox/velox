package middleware

import (
	"context"
	"net/http"

	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/rs/xid"
)

// requestIDPrefix marks an id as server-minted. Rows written before this
// middleware landed may carry a client-supplied request_id (append-only: they
// cannot be rewritten), so the prefix is also the marker that tells a forensic
// reader which era a row belongs to — a `req_` id is provably ours.
const requestIDPrefix = "req_"

// RequestID replaces chi's middleware.RequestID. It ALWAYS mints the id
// server-side and NEVER honours an inbound header.
//
// chi's version does this (chi/v5 middleware/request_id.go):
//
//	requestID := r.Header.Get("X-Request-Id")   // <- client-controlled
//	if requestID == "" { requestID = <generated> }
//
// which means any caller could choose the request_id that lands on their own
// audit_log rows — the column the audit UI presents as forensic correlation
// evidence, and the value support uses to join a customer's report back to
// server logs. An attacker could set it to a constant to make their actions
// unjoinable, collide it with an innocent tenant's traffic, or forge a value
// that "proves" an action came from somewhere it didn't. Correlation evidence
// an adversary can write is not evidence. CloudTrail's eventID, Stripe's
// request id, and GCP's insertId are all server-minted for exactly this reason.
//
// Why we drop the inbound value entirely instead of keeping it for log
// correlation under a second key:
//
//   - Nothing consumes it as INPUT. (The name still appears in the repo — in
//     tests that forge the header to prove it is ignored, and in the docs that
//     record this decision. An earlier version of this comment told you to grep
//     and promised zero hits, which stopped being true the moment the regression
//     test landed. Do not re-add a claim about grep output; state the property.)
//     Historically the reason was: grep across the repo, web-v2 and
//     docs: there are no hits. Velox's published contract is the Velox-Request-Id
//     RESPONSE header (respond.go), which the dashboard captures (web-v2
//     lib/api.ts) and the docs point support at. That contract is unchanged.
//   - Cross-service trace continuity belongs to W3C Trace Context, which
//     mw.Tracing() (otelhttp) propagates from inbound headers. Be precise about
//     what that buys today: tracing is a NO-OP unless OTEL_EXPORTER_OTLP_ENDPOINT
//     is set (internal/platform/telemetry), so on a default deployment there is
//     no cross-service correlation channel at all. That is a real, if small,
//     ACCEPTED LOSS: a caller that wants to correlate its own request with a
//     Velox audit row must now read the Velox-Request-Id response header rather
//     than dictate the id up front. We take that trade because an inbound
//     X-Request-Id would be a second, weaker, UNAUTHENTICATED channel — and it is
//     the one that lands in the compliance log. Closure trigger: a caller with a
//     concrete cross-service correlation need → wire OTLP, not a client-chosen id.
//   - Recording the client's string anywhere on the row — even under an
//     honestly-named metadata key — puts unverified client input into a
//     permanent append-only compliance record. The audit redesign's rule is that
//     nothing unverified enters that log; a key nobody reads is not worth the
//     exception.
//
// The id is stored under chi's own RequestIDKey, so chimw.GetReqID(ctx) — the
// accessor audit.Logger, telemetry.ContextHandler, respond.go and payment/stripe
// all already call — keeps working unchanged, and there is exactly one place
// where a request id is born.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := requestIDPrefix + xid.New().String()
		ctx := context.WithValue(r.Context(), chimw.RequestIDKey, id)
		// Stamp the published header here, not only in respond.JSON. Responses
		// that never reach a respond helper — chi's default "404 page not
		// found" for an unrouted path, net/http's "400 Bad Request" for a
		// malformed request line — used to carry no id in the header OR the
		// body, so the one case where a caller most needs the header fallback
		// (an unparseable envelope) was exactly the case that lacked it.
		// Set before next.ServeHTTP so it is in place whoever writes the
		// status; respond.JSON re-Sets the same value, which is a no-op.
		w.Header().Set("Velox-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
