// Package middleware provides HTTP middleware for the Velox API server.
// Includes rate limiting, idempotency keys, Prometheus metrics, CORS, and
// cursor-based pagination helpers.
package middleware

import (
	"net/http"
	"strings"
)

// CORS returns middleware that handles Cross-Origin Resource Sharing.
// Allows browser-based frontends to call the Velox API.
func CORS(allowedOrigins []string) func(http.Handler) http.Handler {
	originSet := make(map[string]bool, len(allowedOrigins))
	allowAll := false
	for _, o := range allowedOrigins {
		if o == "*" {
			allowAll = true
		}
		originSet[strings.ToLower(o)] = true
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")

			if allowAll || originSet[strings.ToLower(origin)] {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				// Required when the browser sends cookies (session auth). With
				// credentials=true the spec forbids the "*" wildcard, so we
				// always echo the specific origin above.
				w.Header().Set("Access-Control-Allow-Credentials", "true")
				// Vary on Origin so intermediaries don't cache a response
				// generated for one origin and serve it to a different one.
				w.Header().Add("Vary", "Origin")
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-API-Key, Idempotency-Key")
				w.Header().Set("Access-Control-Expose-Headers", "Velox-Version, Velox-Request-Id, X-RateLimit-Limit, X-RateLimit-Remaining, X-RateLimit-Reset, Retry-After")
				w.Header().Set("Access-Control-Max-Age", "86400")
			}

			// Handle preflight
			if r.Method == "OPTIONS" {
				w.WriteHeader(http.StatusNoContent)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}
