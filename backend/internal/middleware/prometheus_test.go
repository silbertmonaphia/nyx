package middleware

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
)

// resetMetrics clears any label permutations accumulated by prior
// subtests. Counters and histograms registered via promauto can't be
// unregistered, but their per-label series can be cleared.
func resetMetrics() {
	httpRequests.Reset()
	httpDuration.Reset()
}

// TestPrometheus_RouteTemplateLabelUsesChiPattern is the cardinality
// guard. With a router that has `/api/movies/{id}`, hitting
// `/api/movies/1` and `/api/movies/2` must record the SAME route
// label (`/api/movies/{id}`), not the literal URL — otherwise every
// distinct movie ID creates a new time series and the Prometheus
// server eventually OOMs.
//
// Note: the middleware must be registered via router.Use so it lives
// inside chi's handler chain; only then does chi populate the
// route context before the middleware reads it.
func TestPrometheus_RouteTemplateLabelUsesChiPattern(t *testing.T) {
	resetMetrics()

	router := chi.NewMux()
	router.Use(Prometheus)
	router.Get("/api/movies/{id}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	for _, id := range []string{"1", "42", "9999"} {
		router.ServeHTTP(httptest.NewRecorder(),
			httptest.NewRequest(http.MethodGet, "/api/movies/"+id, nil))
	}

	gathered := gatherMetric(t, "http_requests_total")
	// Cardinality check: distinct series count for the metric.
	got := strings.Count(gathered, "http_requests_total{")
	if got != 1 {
		t.Errorf("route-template cardinality = %d, want 1; literal URL leaked into the label", got)
	}
	const want = `route="/api/movies/{id}"`
	if !strings.Contains(gathered, want) {
		t.Errorf("expected route label %q in metrics; got:\n%s", want, gathered)
	}
	for _, leak := range []string{`route="/api/movies/1"`, `route="/api/movies/42"`} {
		if strings.Contains(gathered, leak) {
			t.Errorf("literal URL leaked into route label — cardinality explosion:\n%s", gathered)
		}
	}
}

// TestPrometheus_UnmatchedRoutesBucketSeparately confirms that
// requests to paths with no matching chi route get a stable
// "unmatched" label rather than their literal path. Same cardinality
// concern as above: an attacker probing /admin, /.env, /wp-login.php
// would otherwise create one series per attempt.
func TestPrometheus_UnmatchedRoutesBucketSeparately(t *testing.T) {
	resetMetrics()

	router := chi.NewMux()
	router.Use(Prometheus)
	// One decoy route so chi's tree is non-empty and the
	// middleware chain actually runs for unmatched paths.
	router.Get("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	for _, path := range []string{"/admin", "/.env", "/wp-login.php"} {
		router.ServeHTTP(httptest.NewRecorder(),
			httptest.NewRequest(http.MethodGet, path, nil))
	}

	gathered := gatherMetric(t, "http_requests_total")
	if !strings.Contains(gathered, `route="unmatched"`) {
		t.Errorf("expected route=\"unmatched\" for 404 probes; got:\n%s", gathered)
	}
	for _, leak := range []string{"/admin", "/.env", "/wp-login.php"} {
		if strings.Contains(gathered, leak) {
			t.Errorf("unmatched probe path %q leaked into label", leak)
		}
	}
}

// TestPrometheus_RecordsStatusAndMethodLabels verifies the counter
// surfaces method + status too. A regression that drops those labels
// would silently lose observability.
func TestPrometheus_RecordsStatusAndMethodLabels(t *testing.T) {
	resetMetrics()

	router := chi.NewMux()
	router.Use(Prometheus)
	router.Get("/ok", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	router.Get("/bad", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	for _, path := range []string{"/ok", "/bad"} {
		router.ServeHTTP(httptest.NewRecorder(),
			httptest.NewRequest(http.MethodGet, path, nil))
	}

	gathered := gatherMetric(t, "http_requests_total")
	for _, want := range []string{`method="GET"`, `status="200"`, `status="500"`} {
		if !strings.Contains(gathered, want) {
			t.Errorf("expected %q in metrics; got:\n%s", want, gathered)
		}
	}
}

// TestRoutePattern_NoChiContext returns the empty string when called
// outside a chi router — used as the "no route matched" signal by
// the Prometheus middleware. The empty result is what triggers the
// fallback to "unmatched".
func TestRoutePattern_NoChiContext(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/anything", nil)
	if got := routePattern(req); got != "" {
		t.Errorf("routePattern on bare request = %q, want \"\"", got)
	}
}

// gatherMetric renders the named counter as a Prometheus text-format
// string for substring assertions.
func gatherMetric(t *testing.T, name string) string {
	t.Helper()
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	var b strings.Builder
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			labels := make([]string, 0, len(m.GetLabel()))
			for _, lp := range m.GetLabel() {
				labels = append(labels, lp.GetName()+`="`+lp.GetValue()+`"`)
			}
			b.WriteString(name)
			b.WriteByte('{')
			b.WriteString(strings.Join(labels, ","))
			b.WriteString("} ")
			b.WriteString(strconv.FormatFloat(m.GetCounter().GetValue(), 'f', -1, 64))
			b.WriteByte('\n')
		}
	}
	return b.String()
}
