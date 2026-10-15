package middleware

import (
	"net/http"

	"nyx/internal/reqctx"
)

// StoreRequest stashes the inbound *http.Request on the request
// context so handlers that receive a plain context.Context (e.g.
// huma v2's generic handler signature) can still read cookies and
// other request-only data via reqctx.RequestFromContext.
//
// huma's generic Register signature is func(context.Context, *I)
// (*O, error) — the handler doesn't receive huma.Context, so it has
// no direct way to call r.Cookie(name). This middleware bridges that
// gap by surfacing the live *http.Request through a typed context
// value.
//
// The pointer is shared with the live request and must be treated
// as read-only — handlers should not modify it. It is wired in
// main.go immediately after RequestID so every downstream handler
// (including huma's) sees the value on r.Context().
func StoreRequest(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := reqctx.WithRequest(r.Context(), r)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
