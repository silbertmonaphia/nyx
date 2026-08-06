package middleware

import (
	"net/http"

	"nyx/internal/reqctx"

	chimw "github.com/go-chi/chi/v5/middleware"
)

// RealIP returns a middleware that resolves the client IP from proxy
// headers (X-Forwarded-For, X-Real-IP) when present and falls back to
// r.RemoteAddr otherwise. It composes chi's stock RealIP middleware
// with a small wrapper that mirrors the resolved IP into our own
// typed context key (reqctx.ClientIPFromContext), so logging, the
// rate limiter, and error envelopes can stay decoupled from chi.
//
// chi's RealIP rewrites r.RemoteAddr in place when it sees a proxy
// header. The wrapper reads the post-resolution address via
// reqctx.ClientIPFromRequest so the value is correct in both cases.
func RealIP(next http.Handler) http.Handler {
	return chimw.RealIP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := reqctx.WithClientIP(r.Context(), reqctx.ClientIPFromRequest(r))
		next.ServeHTTP(w, r.WithContext(ctx))
	}))
}
