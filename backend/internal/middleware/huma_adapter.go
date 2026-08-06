package middleware

import (
	"net/http"
	"strings"

	"nyx/internal/platform/api"
	"nyx/internal/platform/auth"
	"nyx/internal/reqctx"

	"github.com/danielgtaylor/huma/v2"
)

// HumaAuth returns a huma.Middleware that performs the same JWT
// validation as the stdlib-shaped Auth function, but in huma's
// middleware shape (func(huma.Context, next func(huma.Context))).
//
// Huma parses the request body BEFORE running per-operation
// Middlewares, which means a malicious 100 MB POST could trigger
// work before being rejected. To bound this, main.go installs a
// router-level http.MaxBytesReader cap of 1 MiB on the request body
// before any huma operation is registered. With that cap in place,
// huma's body parser will refuse oversize payloads before this
// middleware runs.
//
// We don't reuse Auth directly because huma.Context doesn't expose a
// plain http.ResponseWriter for short-circuit responses. Instead, we
// stamp the response via huma.Context's SetStatus / SetHeader /
// BodyWriter helpers, then return without calling next.
func HumaAuth() func(huma.Context, func(huma.Context)) {
	return func(ctx huma.Context, next func(huma.Context)) {
		authHeader := ctx.Header("Authorization")
		if authHeader == "" {
			writeHumaError(ctx, http.StatusUnauthorized, "Authorization header is required", nil)
			return
		}

		parts := strings.Split(authHeader, " ")
		if len(parts) != 2 || parts[0] != "Bearer" {
			writeHumaError(ctx, http.StatusUnauthorized, "Authorization header must be in the format 'Bearer <token>'", nil)
			return
		}

		claims, err := auth.ValidateToken(parts[1])
		if err != nil {
			writeHumaError(ctx, http.StatusUnauthorized, "Invalid or expired token", err.Error())
			return
		}

		// Stamp the resolved user on the request context so handlers
		// can read it via reqctx.UserIDFromContext / UsernameFromContext.
		rctx := reqctx.WithUserID(ctx.Context(), claims.UserID)
		rctx = reqctx.WithUsername(rctx, claims.Username)
		// huma exposes a WithContext helper that wraps the adapter-
		// specific request with the new context.Context; downstream
		// handlers see the updated context via ctx.Context().
		next(huma.WithContext(ctx, rctx))
	}
}

// writeHumaError writes the canonical {error, code, request_id, details}
// envelope via huma's response helpers. The request ID is read from
// the context so the correlation ID matches the log line.
func writeHumaError(ctx huma.Context, status int, message string, details interface{}) {
	body := api.NewErrorResponseFromContext(ctx.Context(), status, message, details)
	ctx.SetStatus(status)
	ctx.SetHeader("Content-Type", "application/json; charset=utf-8")
	_ = api.EncodeError(ctx.BodyWriter(), body)
}
