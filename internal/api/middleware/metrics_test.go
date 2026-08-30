package middleware

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// newMetricsRouter mirrors router.go's mount: Metrics() on the root mux, one
// parameterised route, one mounted subtree.
func newMetricsRouter() http.Handler {
	r := chi.NewRouter()
	r.Use(Metrics())
	r.Get("/v1/customers/{id}", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	sub := chi.NewRouter()
	sub.Get("/ping", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	r.Mount("/v1/sub", sub)
	return r
}

func hit(h http.Handler, method, path string) {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
}

// TestMetrics_LabelSpaceIsBounded pins the defect class this middleware
// existed to prevent and did not: client-chosen label values. 500 distinct
// scanner probes must produce exactly ONE new series (path="unmatched"),
// a parameterised route must be labelled by its PATTERN (one series for any
// number of ids), and an unknown verb must collapse to "other".
func TestMetrics_LabelSpaceIsBounded(t *testing.T) {
	h := newMetricsRouter()
	before := testutil.CollectAndCount(httpRequestsTotal)

	for i := 0; i < 500; i++ {
		hit(h, http.MethodGet, fmt.Sprintf("/wp-admin/probe-%d.php", i))
	}
	for i := 0; i < 50; i++ {
		hit(h, http.MethodGet, fmt.Sprintf("/v1/customers/cus_%d", i))
		hit(h, http.MethodGet, fmt.Sprintf("/v1/customers/vlx_cus_%020d", i))
	}
	hit(h, "BREW", "/v1/customers/x")
	hit(h, http.MethodGet, "/v1/sub/ping")
	hit(h, http.MethodGet, "/v1/sub/nope-1")
	hit(h, http.MethodGet, "/v1/sub/nope-2")

	added := testutil.CollectAndCount(httpRequestsTotal) - before
	// unmatched/GET/404 · /v1/customers/{id}/GET/200 · other/…/200 ·
	// /v1/sub/*/GET/200 (ping) · /v1/sub/*/GET/404 (nope) = 5 series.
	if added != 5 {
		t.Fatalf("expected 5 new series for 605 requests, got %d — the label space is not bounded", added)
	}
	if v := testutil.ToFloat64(httpRequestsTotal.WithLabelValues(http.MethodGet, unmatchedRoute, "404")); v != 500 {
		t.Fatalf("500 scanner probes should land on ONE unmatched series, got %v", v)
	}
	if v := testutil.ToFloat64(httpRequestsTotal.WithLabelValues(http.MethodGet, "/v1/customers/{id}", "200")); v != 100 {
		t.Fatalf("parameterised route should be labelled by pattern, got %v on the pattern series", v)
	}
	// chi answers an unknown method on a known path with 405; whether it also
	// records the pattern on that miss is chi's business — what this pins is
	// that the verb collapsed to "other" and the path label is one of the two
	// bounded values, never the raw path.
	onPattern := testutil.ToFloat64(httpRequestsTotal.WithLabelValues("other", "/v1/customers/{id}", "405"))
	onUnmatched := testutil.ToFloat64(httpRequestsTotal.WithLabelValues("other", unmatchedRoute, "405"))
	if onPattern+onUnmatched != 1 {
		t.Fatalf("unknown verb should collapse to method=other on a bounded path label, got pattern=%v unmatched=%v", onPattern, onUnmatched)
	}
	t.Logf("chi 405 on unknown verb landed on path label: pattern=%v unmatched=%v", onPattern, onUnmatched)
}

func TestMethodLabel(t *testing.T) {
	for _, m := range []string{"GET", "POST", "DELETE", "OPTIONS"} {
		if got := methodLabel(m); got != m {
			t.Errorf("methodLabel(%s) = %s", m, got)
		}
	}
	for _, m := range []string{"BREW", "get", "", "PROPFIND"} {
		if got := methodLabel(m); got != "other" {
			t.Errorf("methodLabel(%q) = %s, want other", m, got)
		}
	}
}
