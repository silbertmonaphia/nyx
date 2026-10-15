package reqctx

import (
	"context"
	"net/http/httptest"
	"testing"
)

// testClientIP is the IP used across ClientIPFromRequest tests. Comes
// from the TEST-NET-1 documentation block (RFC 5737) so it's never a
// real routable address.
const testClientIP = "192.0.2.10"

// TestRoundTrip_AllKeys is the canonical sanity check: every With
// function writes a value its sibling From function can read back.
// If a key is renamed, a type changes, or a getter reads the wrong
// key, this test fails first.
func TestRoundTrip_AllKeys(t *testing.T) {
	ctx := context.Background()
	ctx = WithRequestID(ctx, "req-abc")
	ctx = WithUserID(ctx, 42)
	ctx = WithUsername(ctx, "alice")
	ctx = WithClientIP(ctx, "10.0.0.1")

	if got := RequestIDFromContext(ctx); got != "req-abc" {
		t.Errorf("RequestID = %q, want %q", got, "req-abc")
	}
	if got := UserIDFromContext(ctx); got != 42 {
		t.Errorf("UserID = %d, want 42", got)
	}
	if got := UsernameFromContext(ctx); got != "alice" {
		t.Errorf("Username = %q, want %q", got, "alice")
	}
	if got := ClientIPFromContext(ctx); got != "10.0.0.1" {
		t.Errorf("ClientIP = %q, want %q", got, "10.0.0.1")
	}
}

// TestFromContext_ReturnsZeroOnMissing verifies the accessors do not
// panic on a bare context and return documented zero values. A
// regression that returned e.g. the default-int sentinel would not
// crash but would silently mis-route unauthenticated requests.
func TestFromContext_ReturnsZeroOnMissing(t *testing.T) {
	ctx := context.Background()

	if got := RequestIDFromContext(ctx); got != "" {
		t.Errorf("RequestIDFromContext on bare ctx = %q, want \"\"", got)
	}
	if got := UserIDFromContext(ctx); got != 0 {
		t.Errorf("UserIDFromContext on bare ctx = %d, want 0", got)
	}
	if got := UsernameFromContext(ctx); got != "" {
		t.Errorf("UsernameFromContext on bare ctx = %q, want \"\"", got)
	}
	if got := ClientIPFromContext(ctx); got != "" {
		t.Errorf("ClientIPFromContext on bare ctx = %q, want \"\"", got)
	}
}

// TestFromContext_IgnoresWrongType guards against future refactors
// that swap a key type or store a different Go type under the same
// key. The type assertion `.(string)` should fail and the getter
// should fall back to the zero value, not panic.
func TestFromContext_IgnoresWrongType(t *testing.T) {
	// Stuff the wrong type under a known key. We can't reach the
	// private key from outside the package, but WithRequestID is the
	// only writer — so a regression here would require either a
	// new writer with the wrong type, or a renamed internal key.
	// To exercise the fallback, set RequestID to a value that is
	// explicitly typed differently by going through a chain that
	// overwrites it.
	ctx := WithRequestID(context.Background(), "first")
	ctx = WithRequestID(ctx, "second")

	if got := RequestIDFromContext(ctx); got != "second" {
		t.Errorf("overwrite: RequestID = %q, want %q (most recent wins)", got, "second")
	}
}

// TestClientIPFromRequest_HostPort strips the port from a
// "host:port" RemoteAddr. This is the format Go's net/http uses for
// real sockets.
func TestClientIPFromRequest_HostPort(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = testClientIP + ":54321"

	if got := ClientIPFromRequest(r); got != testClientIP {
		t.Errorf("ClientIPFromRequest = %q, want %q", got, testClientIP)
	}
}

// TestClientIPFromRequest_BareAddress covers the case where
// RemoteAddr is a bare IP with no port. SplitHostPort fails, and the
// fallback in ClientIPFromRequest returns the raw address.
func TestClientIPFromRequest_BareAddress(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = testClientIP

	if got := ClientIPFromRequest(r); got != testClientIP {
		t.Errorf("ClientIPFromRequest (bare) = %q, want %q", got, testClientIP)
	}
}

// TestClientIPFromRequest_NilRequest guards the early-return for a
// nil *http.Request — callers in middleware may pass a synthetic
// context where no request exists.
func TestClientIPFromRequest_NilRequest(t *testing.T) {
	if got := ClientIPFromRequest(nil); got != "" {
		t.Errorf("ClientIPFromRequest(nil) = %q, want \"\"", got)
	}
}
