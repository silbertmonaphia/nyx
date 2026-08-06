package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestCORS_AddsHeadersOnNormalRequest verifies the standard response
// headers land on every request (not just OPTIONS). Browsers apply
// CORS to every cross-origin call, not just preflights, so the
// Access-Control-Allow-Origin header MUST accompany the regular
// response too.
func TestCORS_AddsHeadersOnNormalRequest(t *testing.T) {
	downstreamCalled := false
	downstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		downstreamCalled = true
	})
	handler := CORS(downstream)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/movies", nil)
	handler.ServeHTTP(rr, req)

	if !downstreamCalled {
		t.Error("downstream handler was not called on a non-OPTIONS request")
	}
	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin = %q, want \"*\"", got)
	}
	if got := rr.Header().Get("Access-Control-Allow-Methods"); got == "" {
		t.Error("Access-Control-Allow-Methods is empty")
	}
	if got := rr.Header().Get("Access-Control-Allow-Headers"); got == "" {
		t.Error("Access-Control-Allow-Headers is empty")
	}
}

// TestCORS_OPTIONSShortCircuits is the preflight path: OPTIONS must
// respond with 200 + headers and MUST NOT invoke the downstream
// handler. If the downstream ran, it would see a "no route matched"
// 404 in production because OPTIONS isn't a registered route —
// causing a confusing 404 in browser devtools.
func TestCORS_OPTIONSShortCircuits(t *testing.T) {
	downstreamCalled := false
	downstream := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		downstreamCalled = true
	})
	handler := CORS(downstream)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodOptions, "/api/movies", nil)
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("OPTIONS status = %d, want 200", rr.Code)
	}
	if downstreamCalled {
		t.Error("OPTIONS preflight invoked the downstream handler; preflight must short-circuit")
	}
	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("preflight: Access-Control-Allow-Origin = %q, want \"*\"", got)
	}
}