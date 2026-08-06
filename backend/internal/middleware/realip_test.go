package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"nyx/internal/reqctx"
)

// TestRealIP_FallsBackToRemoteAddr covers the case where no proxy
// header is set. RealIP should read r.RemoteAddr (via
// reqctx.ClientIPFromRequest) and stamp the host portion on
// reqctx.WithClientIP so downstream handlers see a clean IP.
func TestRealIP_FallsBackToRemoteAddr(t *testing.T) {
	var seenOnContext string
	downstream := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seenOnContext = reqctx.ClientIPFromContext(r.Context())
	})
	handler := RealIP(downstream)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.10:54321"
	handler.ServeHTTP(rr, req)

	if seenOnContext != "203.0.113.10" {
		t.Errorf("ClientIPFromContext = %q, want %q (host portion of RemoteAddr)", seenOnContext, "203.0.113.10")
	}
}

// TestRealIP_RespectsXForwardedFor verifies the chi RealIP layer
// honours X-Forwarded-For. This is the path behind a reverse proxy
// (nginx, ELB) — without it, every request would appear to come from
// the proxy itself.
func TestRealIP_RespectsXForwardedFor(t *testing.T) {
	var seenOnContext string
	downstream := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seenOnContext = reqctx.ClientIPFromContext(r.Context())
	})
	handler := RealIP(downstream)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.1:80"                         // proxy
	req.Header.Set("X-Forwarded-For", "198.51.100.5")      // real client
	handler.ServeHTTP(rr, req)

	if seenOnContext != "198.51.100.5" {
		t.Errorf("ClientIPFromContext = %q, want %q (from X-Forwarded-For)", seenOnContext, "198.51.100.5")
	}
}

// TestRealIP_RespectsXRealIP verifies the X-Real-IP header (used by
// nginx's default real_ip module configuration) takes precedence
// over RemoteAddr.
func TestRealIP_RespectsXRealIP(t *testing.T) {
	var seenOnContext string
	downstream := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seenOnContext = reqctx.ClientIPFromContext(r.Context())
	})
	handler := RealIP(downstream)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.1:80"
	req.Header.Set("X-Real-IP", "198.51.100.7")
	handler.ServeHTTP(rr, req)

	if seenOnContext != "198.51.100.7" {
		t.Errorf("ClientIPFromContext = %q, want %q (from X-Real-IP)", seenOnContext, "198.51.100.7")
	}
}