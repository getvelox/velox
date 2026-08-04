package middleware

import (
	"net/http/httptest"
	"testing"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

// rateLimitKey decides which bucket a request spends from. The precedence
// (key id → tenant+mode → IP) and the ":test"/":live" suffix are load-bearing:
// without the suffix a tenant's test-mode exploration shares quota with its
// live operations, so a burst of dashboard clicking in test mode can 429 a
// real payment retry. MANUAL_TEST FLOW A3 asserts the per-mode split, but
// nothing pinned it until this test.
func TestRateLimitKey_PerModeTenantBuckets(t *testing.T) {
	tenantKey := func(tenantID string, livemode bool) string {
		r := httptest.NewRequest("GET", "/v1/customers", nil)
		ctx := auth.WithTenantID(r.Context(), tenantID)
		ctx = postgres.WithLivemode(ctx, livemode)
		return rateLimitKey(r.WithContext(ctx))
	}

	testKey := tenantKey("ten_abc", false)
	liveKey := tenantKey("ten_abc", true)

	if want := "tenant:ten_abc:test"; testKey != want {
		t.Errorf("test-mode bucket = %q, want %q", testKey, want)
	}
	if want := "tenant:ten_abc:live"; liveKey != want {
		t.Errorf("live-mode bucket = %q, want %q", liveKey, want)
	}
	if testKey == liveKey {
		t.Fatalf("test and live share bucket %q — one mode can exhaust the other's quota", testKey)
	}
}

func TestRateLimitKey_APIKeyWinsOverTenant(t *testing.T) {
	// An API key has a fixed livemode baked into its prefix, so per-key
	// keying is already per-mode. It takes precedence so one runaway
	// integration can't starve the tenant's dashboard.
	r := httptest.NewRequest("GET", "/v1/customers", nil)
	ctx := auth.WithTenantID(r.Context(), "ten_abc")
	ctx = auth.WithKeyID(ctx, "vlx_key_123")
	ctx = postgres.WithLivemode(ctx, true)

	if got, want := rateLimitKey(r.WithContext(ctx)), "key:vlx_key_123"; got != want {
		t.Errorf("bucket = %q, want %q", got, want)
	}
}

func TestRateLimitKey_UnauthenticatedFallsBackToIP(t *testing.T) {
	// Wholly-unauthenticated paths (login, bootstrap) still get a bucket,
	// so a flood can't bypass the limiter by simply not authenticating.
	//
	// Both RemoteAddr shapes matter. Go's server sets host:port, but
	// TrustedRealIP runs ahead of every limiter group and rewrites
	// RemoteAddr to a BARE ip when the request came through a trusted
	// proxy — the production shape. That takes SplitHostPort's error
	// branch, and without the bare case a regression collapsing it (say
	// to a constant) would leave every proxied client sharing one global
	// bucket while this test still passed.
	for _, tc := range []struct{ name, remoteAddr, want string }{
		{"host:port from the Go server", "203.0.113.7:54321", "ip:203.0.113.7"},
		{"bare ip rewritten by TrustedRealIP", "203.0.113.7", "ip:203.0.113.7"},
		{"IPv6 host:port", "[2001:db8::1]:54321", "ip:2001:db8::1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/v1/auth/login", nil)
			r.RemoteAddr = tc.remoteAddr
			if got := rateLimitKey(r); got != tc.want {
				t.Errorf("bucket = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRateLimitKey_TenantBucketsAreDistinctAcrossTenants(t *testing.T) {
	key := func(tenantID string) string {
		r := httptest.NewRequest("GET", "/v1/invoices", nil)
		return rateLimitKey(r.WithContext(auth.WithTenantID(r.Context(), tenantID)))
	}
	if key("ten_a") == key("ten_b") {
		t.Fatal("two tenants share a bucket — one tenant's load throttles another")
	}
}
