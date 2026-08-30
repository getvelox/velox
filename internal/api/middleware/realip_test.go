package middleware

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestTrustedRealIP covers the pass-3 [security] audit finding: chi's RealIP
// honored X-Forwarded-For/X-Real-IP unconditionally, so any client could forge
// a forwarding header to rotate its per-IP rate-limit bucket or pin a victim's
// IP. TrustedRealIP only rewrites RemoteAddr when the TCP peer is a trusted
// proxy.
func TestTrustedRealIP(t *testing.T) {
	trusted := ParseTrustedProxies("10.0.0.0/8,127.0.0.1")

	run := func(remoteAddr, xff, xrealip string, nets []*net.IPNet) string {
		var seen string
		h := TrustedRealIP(nets)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			seen = r.RemoteAddr
		}))
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = remoteAddr
		if xff != "" {
			req.Header.Set("X-Forwarded-For", xff)
		}
		if xrealip != "" {
			req.Header.Set("X-Real-IP", xrealip)
		}
		h.ServeHTTP(httptest.NewRecorder(), req)
		return seen
	}

	t.Run("untrusted peer: forged XFF ignored", func(t *testing.T) {
		if got := run("203.0.113.9:5555", "1.2.3.4", "", trusted); got != "203.0.113.9:5555" {
			t.Errorf("got %q, want raw peer (forged XFF must not be trusted)", got)
		}
	})

	t.Run("trusted peer: XFF honored", func(t *testing.T) {
		if got := run("10.1.2.3:443", "198.51.100.7", "", trusted); got != "198.51.100.7" {
			t.Errorf("got %q, want 198.51.100.7 (real client via trusted proxy)", got)
		}
	})

	t.Run("trusted peer: picks first untrusted from the right", func(t *testing.T) {
		if got := run("127.0.0.1:8080", "198.51.100.7, 10.0.0.5, 10.0.0.6", "", trusted); got != "198.51.100.7" {
			t.Errorf("got %q, want 198.51.100.7 (skip trusted hops, take real client)", got)
		}
	})

	t.Run("trusted peer: X-Real-IP fallback", func(t *testing.T) {
		if got := run("10.0.0.9:80", "", "192.0.2.44", trusted); got != "192.0.2.44" {
			t.Errorf("got %q, want 192.0.2.44 (X-Real-IP from trusted proxy)", got)
		}
	})

	t.Run("no trusted proxies configured: always raw peer", func(t *testing.T) {
		if got := run("10.1.2.3:443", "198.51.100.7", "", nil); got != "10.1.2.3:443" {
			t.Errorf("got %q, want raw peer (no proxies trusted)", got)
		}
	})
}

func TestParseTrustedProxies(t *testing.T) {
	nets := ParseTrustedProxies("10.0.0.0/8, 127.0.0.1 , , bogus")
	if len(nets) != 2 {
		t.Fatalf("parsed %d nets, want 2 (CIDR + bare IP; blank/bogus skipped)", len(nets))
	}
}

func TestParseTrustedProxies_NoneMeansNoProxy(t *testing.T) {
	for _, spec := range []string{"none", " NONE ", "None"} {
		if got := ParseTrustedProxies(spec); got != nil {
			t.Fatalf("ParseTrustedProxies(%q) = %v, want nil", spec, got)
		}
	}
	if got := ParseTrustedProxies("10.0.0.0/8"); len(got) != 1 {
		t.Fatalf("a CIDR must still parse, got %v", got)
	}
}
