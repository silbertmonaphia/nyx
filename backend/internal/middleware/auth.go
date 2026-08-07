package middleware

import (
	"net/http"
	"strings"

	"nyx/internal/platform/api"
	"nyx/internal/platform/auth"
	"nyx/internal/reqctx"
)

// NewAuth returns a chi middleware that validates JWTs from the
// Authorization header using the injected TokenService. It is the
// stdlib-shaped counterpart to NewHumaAuth and is intended for
// mounting on protected sub-routers (e.g., a future /api/admin/...
// group) — keep it exported for that future use.
//
// On success, the resolved user ID and username are stamped on the
// context via reqctx.WithUserID / reqctx.WithUsername so downstream
// handlers can read them via reqctx.UserIDFromContext /
// reqctx.UsernameFromContext. On failure, the canonical {error, code,
// request_id, details} envelope is written and the chain is
// short-circuited.
//
// huma parses the request body before invoking per-operation
// Middlewares, so a 1 MiB body cap is applied at the router level in
// main.go to mitigate pre-auth DoS via large payloads.
func NewAuth(tokens auth.TokenService) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authHeader := r.Header.Get("Authorization")
			if authHeader == "" {
				api.WriteError(w, r, http.StatusUnauthorized, "Authorization header is required", nil)
				return
			}

			parts := strings.Split(authHeader, " ")
			if len(parts) != 2 || parts[0] != "Bearer" {
				api.WriteError(w, r, http.StatusUnauthorized, "Authorization header must be in the format 'Bearer <token>'", nil)
				return
			}

			claims, err := tokens.ValidateToken(parts[1])
			if err != nil {
				api.WriteError(w, r, http.StatusUnauthorized, "Invalid or expired token", api.ClassifyAndLog(r.Context(), err, "invalid token"))
				return
			}

			ctx := reqctx.WithUserID(r.Context(), claims.UserID)
			ctx = reqctx.WithUsername(ctx, claims.Username)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
