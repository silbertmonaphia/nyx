// Package reqctx holds the typed context keys and accessor helpers used
// by both the middleware package (which sets values) and the api
// package (which reads them, e.g. for huma error envelopes).
//
// This package exists solely to break an import cycle: middleware needs
// to set request IDs / user IDs, api needs to read them, and either
// direction of dependency would create a cycle. Putting the keys in a
// leaf package that both depend on keeps the dependency graph a DAG.
package reqctx

import (
	"context"
	"net"
	"net/http"
)

// ctxKey is a typed key for context.WithValue. Using a typed key avoids
// collisions with keys used by other packages and prevents accidental
// misuse from stringly-typed keys.
type ctxKey int

const (
	requestIDKey ctxKey = iota
	userIDKey
	usernameKey
	clientIPKey
)

// RequestIDHeader is the canonical HTTP header used for request IDs.
const RequestIDHeader = "X-Request-ID"

// RequestIDFromContext returns the request ID stored in ctx by the
// RequestID middleware. Returns "" when no ID is present.
func RequestIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(requestIDKey).(string); ok {
		return v
	}
	return ""
}

// WithRequestID stores the given ID on the context. Used by the
// RequestID middleware; tests may also use it to seed a known ID.
func WithRequestID(parent context.Context, id string) context.Context {
	return context.WithValue(parent, requestIDKey, id)
}

// UserIDFromContext returns the authenticated user's ID stored by the
// Auth middleware. Returns 0 when not authenticated.
func UserIDFromContext(ctx context.Context) int {
	if v, ok := ctx.Value(userIDKey).(int); ok {
		return v
	}
	return 0
}

// WithUserID stores the authenticated user's ID on the context.
func WithUserID(parent context.Context, id int) context.Context {
	return context.WithValue(parent, userIDKey, id)
}

// UsernameFromContext returns the authenticated username stored by the
// Auth middleware. Returns "" when not authenticated.
func UsernameFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(usernameKey).(string); ok {
		return v
	}
	return ""
}

// WithUsername stores the authenticated username on the context.
func WithUsername(parent context.Context, name string) context.Context {
	return context.WithValue(parent, usernameKey, name)
}

// ClientIPFromContext returns the resolved client IP for the current
// request. The RealIP middleware populates this on each request.
func ClientIPFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(clientIPKey).(string); ok {
		return v
	}
	return ""
}

// WithClientIP stores the given client IP on the context.
func WithClientIP(parent context.Context, ip string) context.Context {
	return context.WithValue(parent, clientIPKey, ip)
}

// ClientIPFromRequest returns the host portion of r.RemoteAddr. It is
// the fallback used by RealIP when no X-Forwarded-For / X-Real-IP
// headers are present.
func ClientIPFromRequest(r *http.Request) string {
	if r == nil {
		return ""
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		// RemoteAddr may already be a bare address (no port) in tests.
		return r.RemoteAddr
	}
	return host
}