// Package middleware provides HTTP middleware for the Nyx API. Middleware
// follow the standard `func(http.Handler) http.Handler` shape and work
// with any router that supports net/http-compatible middleware (chi,
// huma, plain http.ServeMux).
//
// The context keys for request IDs, user IDs, and client IPs live in
// `nyx/internal/reqctx` so both this package and `nyx/internal/platform/api`
// can read them without an import cycle. This file re-exports the
// commonly used helpers for callers that already import middleware.
package middleware

import (
	"context"

	"nyx/internal/reqctx"
)

// RequestIDHeader is the canonical HTTP header used for request IDs.
const RequestIDHeader = reqctx.RequestIDHeader

// RequestIDFromContext returns the request ID stored in ctx.
func RequestIDFromContext(ctx context.Context) string { return reqctx.RequestIDFromContext(ctx) }

// UserIDFromContext returns the authenticated user's ID stored in ctx.
func UserIDFromContext(ctx context.Context) int { return reqctx.UserIDFromContext(ctx) }

// UsernameFromContext returns the authenticated username stored in ctx.
func UsernameFromContext(ctx context.Context) string { return reqctx.UsernameFromContext(ctx) }

// ClientIPFromContext returns the resolved client IP for the request.
func ClientIPFromContext(ctx context.Context) string { return reqctx.ClientIPFromContext(ctx) }