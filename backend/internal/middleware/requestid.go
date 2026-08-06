package middleware

import (
	"net/http"

	"nyx/internal/reqctx"

	"github.com/google/uuid"
)

// RequestID returns a middleware that injects a unique request ID into
// the request context and response headers. If the inbound request
// already carries an X-Request-ID header, that value is reused so
// caller-side correlation chains survive across services; otherwise a
// fresh UUIDv4 is generated.
//
// The ID is stamped in two places:
//   - r.Context(), via reqctx.WithRequestID, so handlers, loggers, and
//     the api error envelope can read it without depending on chi.
//   - The X-Request-ID response header, so callers can echo it in their
//     own logs / incident reports.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(reqctx.RequestIDHeader)
		if id == "" {
			id = uuid.NewString()
		}
		ctx := reqctx.WithRequestID(r.Context(), id)
		w.Header().Set(reqctx.RequestIDHeader, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
