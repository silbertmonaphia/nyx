package middleware

import (
	"net/http"
	"time"

	"nyx/internal/reqctx"

	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/rs/zerolog/log"
)

// Logging returns a middleware that emits a structured zerolog line
// per request. The line carries the request ID, resolved client IP,
// response status (captured via chi's WrapResponseWriter), and total
// latency.
//
// Place this AFTER Prometheus and RequestID in the chain so:
//   - The X-Request-ID middleware has populated the request ID.
//   - RealIP has resolved the client IP.
//   - The response status written by the handler is available.
func Logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		path := r.URL.Path
		raw := r.URL.RawQuery

		ww := chimw.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)

		if raw != "" {
			path = path + "?" + raw
		}

		log.Info().
			Str("request_id", reqctx.RequestIDFromContext(r.Context())).
			Str("method", r.Method).
			Str("path", path).
			Int("status", ww.Status()).
			Int("bytes", ww.BytesWritten()).
			Dur("duration", time.Since(start)).
			Str("client_ip", reqctx.ClientIPFromContext(r.Context())).
			Msg("Request processed")
	})
}
