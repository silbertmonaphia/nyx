package middleware

import (
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"nyx/internal/access"
	"nyx/internal/platform/api"
	"nyx/internal/reqctx"
)

const (
	aiGateAuthRequiredMessage = "Authentication required"
	aiNotAvailableMessage     = "AI features are not available"
)

func NewAIAllowlist(allowed []string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if len(allowed) == 0 {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			username := reqctx.UsernameFromContext(r.Context())
			if username == "" {
				w.Header().Set("WWW-Authenticate", wwwAuthInvalid)
				api.WriteError(w, r, http.StatusUnauthorized, aiGateAuthRequiredMessage, nil)
				return
			}
			if !access.Allows(allowed, username) {
				api.WriteError(w, r, http.StatusForbidden, aiNotAvailableMessage, nil)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func NewHumaAIAllowlist(allowed []string) func(huma.Context, func(huma.Context)) {
	if len(allowed) == 0 {
		return func(ctx huma.Context, next func(huma.Context)) { next(ctx) }
	}
	return func(ctx huma.Context, next func(huma.Context)) {
		username := reqctx.UsernameFromContext(ctx.Context())
		if username == "" {
			ctx.SetHeader("WWW-Authenticate", wwwAuthInvalid)
			writeHumaError(ctx, http.StatusUnauthorized, aiGateAuthRequiredMessage, nil)
			return
		}
		if !access.Allows(allowed, username) {
			writeHumaError(ctx, http.StatusForbidden, aiNotAvailableMessage, nil)
			return
		}
		next(ctx)
	}
}
