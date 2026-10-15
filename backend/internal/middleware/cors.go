package middleware

import (
	"net/http"
	"strings"
)

// NewCORS returns a middleware that sets a configurable cross-origin
// policy. Behaviour depends on whether "*" is in the allowlist:
//
//   - "*" in allowedOrigins → permissive, Access-Control-Allow-Origin: *.
//     No Vary header, no per-request echo, no credentials. Matches the
//     gin-era behaviour the frontend was built against. Use ONLY for
//     fully public credential-less APIs; combining "*" with cookie or
//     Bearer auth is unsafe and the CORS spec forbids
//     Access-Control-Allow-Credentials: true under "*" anyway.
//
//   - otherwise (the default) → the slice is the explicit allowlist of
//     origins. With an empty list, every cross-origin request from a
//     browser gets NO Access-Control-Allow-Origin header — the browser
//     blocks the response, which is the deny-by-default the project's
//     safety convention calls for. When the request's Origin IS in the
//     list, the header is echoed back (Access-Control-Allow-Origin: <origin>)
//     and Vary: Origin is set so caches don't conflate responses
//     across different origins.
//
// In both modes Access-Control-Allow-Methods / Allow-Headers carry the
// same values as before (the gin-era defaults; credentialed / custom
// header support is out of scope).
//
// OPTIONS preflight short-circuits with 200 + headers so the request
// never reaches downstream middleware. This must remain the case so
// CORS preflight keeps working as a router-level middleware rather
// than a per-route handler (otherwise OPTIONS would surface as a
// confusing 404 in browser devtools).
func NewCORS(allowedOrigins []string) func(http.Handler) http.Handler {
	wildcard := false
	for _, o := range allowedOrigins {
		if o == "*" {
			wildcard = true
			break
		}
	}
	allow := make(map[string]struct{}, len(allowedOrigins))
	for _, o := range allowedOrigins {
		if o == "" || o == "*" {
			continue
		}
		allow[o] = struct{}{}
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case wildcard:
				w.Header().Set("Access-Control-Allow-Origin", "*")
			case r.Header.Get("Origin") != "":
				if _, ok := allow[r.Header.Get("Origin")]; ok {
					w.Header().Set("Access-Control-Allow-Origin", r.Header.Get("Origin"))
					w.Header().Add("Vary", "Origin")
				}
			}
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusOK)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// SplitNonEmpty splits a comma-separated allowlist (e.g. an env var
// value) into trimmed, non-empty entries. Empty inputs and items
// composed solely of whitespace are dropped. The result is suitable
// for passing straight to NewCORS.
func SplitNonEmpty(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
