package middleware

import (
	"encoding/json"
	"net/http"
	"runtime/debug"

	"nyx/internal/reqctx"

	"github.com/rs/zerolog/log"
)

// Recoverer returns a middleware that recovers from panics in downstream
// handlers, logs the panic with the request ID via zerolog, and writes a
// 500 JSON envelope.
//
// This is a hand-rolled replacement for chi/middleware.Recoverer because
// chi's default implementation prints to stderr and does not include the
// request ID. The on-wire response is intentionally minimal — the full
// {error, code, request_id, details} envelope is produced by huma's
// ErrorHandler for non-panic failures, and by api.WriteError for
// middleware-emitted aborts. (This file deliberately does not import
// platform/api to avoid an import cycle: api -> middleware -> api.)
func Recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			requestID := reqctx.RequestIDFromContext(r.Context())
			log.Error().
				Interface("panic", rec).
				Str("request_id", requestID).
				Str("method", r.Method).
				Str("path", r.URL.Path).
				Bytes("stack", debug.Stack()).
				Msg("panic recovered")

			// Minimal panic envelope — the full envelope lives in
			// api.WriteError. We intentionally don't include the
			// request ID in the body here to keep this middleware
			// free of platform/api imports; the correlation lives
			// in the structured log line above.
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": "Internal server error",
				"code":  http.StatusInternalServerError,
			})
		}()
		next.ServeHTTP(w, r)
	})
}
