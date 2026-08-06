package middleware

import "net/http"

// CORS returns a middleware that sets a permissive cross-origin policy
// matching the previous gin-era configuration (Access-Control-Allow-Origin:
// *). It is intentionally minimal — the Vite frontend talks to the API
// from the same Docker network and we do not need credentials, custom
// headers, or origin validation. If you ever add cookie auth, replace
// this with go-chi/cors and tighten the policy.
//
// OPTIONS preflight requests are short-circuited with 200 + headers and
// never reach downstream middleware (so they never appear as "route not
// matched" in Prometheus). This must remain the case so CORS preflight
// keeps working as a router-level middleware rather than a per-route
// handler.
func CORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		next.ServeHTTP(w, r)
	})
}
