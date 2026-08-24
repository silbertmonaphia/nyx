package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestTracing_NoopProviderPassesThrough confirms that when the global
// tracer provider is noop (the default for OTEL_ENABLED=false) the
// middleware still runs without panic and the downstream handler is
// invoked normally. We don't install a real provider here — the test
// relies on the implicit global noop default.
func TestTracing_NoopProviderPassesThrough(t *testing.T) {
	called := false
	h := Tracing("test")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(http.MethodGet, "/anything", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if !called {
		t.Fatal("downstream handler not called")
	}
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusNoContent)
	}
}

// routePattern is the helper used by the tracing middleware's span
// name formatter. Smoke-test it via the same code path Prometheus
// exercises.
func TestTracing_RoutePatternFromChiCtx(t *testing.T) {
	// chi.RouteContext is set by chi.Mux; exercising it through a
	// fully-routed mux would pull in chi as a test dep. The route
	// pattern returned for a plain net/http request (no chi ctx)
	// is "", which the span name formatter falls back from to the
	// raw path — this is the behaviour we want.
	req := httptest.NewRequest(http.MethodGet, "/api/movies", nil)
	if got := routePattern(req); got != "" {
		t.Fatalf("routePattern(no chi ctx) = %q, want \"\"", got)
	}
}
