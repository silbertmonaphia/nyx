package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"nyx/internal/reqctx"
)

// TestRequestID_GeneratesWhenAbsent is the happy path: no incoming
// header → middleware generates a fresh ID, stamps it on context,
// echoes it in the response.
func TestRequestID_GeneratesWhenAbsent(t *testing.T) {
	var seenOnContext string
	downstream := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seenOnContext = reqctx.RequestIDFromContext(r.Context())
	})
	handler := RequestID(downstream)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	handler.ServeHTTP(rr, req)

	if seenOnContext == "" {
		t.Error("request ID missing from downstream context")
	}
	if rr.Header().Get(reqctx.RequestIDHeader) != seenOnContext {
		t.Errorf("response header %q = %q, want %q (same as context)",
			reqctx.RequestIDHeader, rr.Header().Get(reqctx.RequestIDHeader), seenOnContext)
	}
}

// TestRequestID_ReusesIncomingHeader ensures an inbound X-Request-ID
// (typical from a load balancer or upstream proxy) is preserved
// rather than overwritten. Correlation across services only works
// when the upstream's ID flows through unchanged.
func TestRequestID_ReusesIncomingHeader(t *testing.T) {
	const inbound = "upstream-req-id-42"
	var seenOnContext string
	downstream := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seenOnContext = reqctx.RequestIDFromContext(r.Context())
	})
	handler := RequestID(downstream)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(reqctx.RequestIDHeader, inbound)
	handler.ServeHTTP(rr, req)

	if seenOnContext != inbound {
		t.Errorf("downstream saw %q, want %q (incoming header reused)", seenOnContext, inbound)
	}
	if rr.Header().Get(reqctx.RequestIDHeader) != inbound {
		t.Errorf("response header = %q, want %q", rr.Header().Get(reqctx.RequestIDHeader), inbound)
	}
}

// TestRequestID_GeneratedIDsAreUnique confirms two back-to-back
// requests without an inbound header get distinct IDs. A regression
// here (e.g. time-based ID) could collapse concurrent requests into
// a single correlation bucket.
func TestRequestID_GeneratedIDsAreUnique(t *testing.T) {
	downstream := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {})
	handler := RequestID(downstream)

	seen := make(map[string]bool)
	for i := 0; i < 10; i++ {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		handler.ServeHTTP(rr, req)
		id := rr.Header().Get(reqctx.RequestIDHeader)
		if id == "" {
			t.Fatalf("iter %d: no ID generated", i)
		}
		if seen[id] {
			t.Fatalf("iter %d: duplicate ID %q", i, id)
		}
		seen[id] = true
	}
}
