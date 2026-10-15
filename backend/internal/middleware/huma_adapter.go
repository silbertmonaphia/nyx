package middleware

import (
	"errors"
	"net/http"

	"nyx/internal/platform/api"
	"nyx/internal/platform/auth"
	"nyx/internal/reqctx"

	"github.com/danielgtaylor/huma/v2"
)

// NewHumaAuth returns a huma.Middleware that performs the same JWT
// validation as NewAuth, but in huma's middleware shape
// (func(huma.Context, next func(huma.Context))). tokens carries the
// signing key captured at startup.
//
// Token source: the Authorization request header (RFC 6750). Same
// rationale as NewAuth — Bearer lets native clients (iOS, Android,
// Unity / Unreal, console SDKs) speak the identical wire contract
// as the SPA, with the cross-origin CORS preflight providing the
// CSRF defence.
//
// Huma parses the request body BEFORE running per-operation
// Middlewares, which means a malicious 100 MB POST could trigger
// work before being rejected. To bound this, main.go installs a
// router-level http.MaxBytesReader cap of 1 MiB on the request body
// before any huma operation is registered. With that cap in place,
// huma's body parser will refuse oversize payloads before this
// middleware runs.
//
// We don't reuse NewAuth directly because huma.Context doesn't expose a
// plain http.ResponseWriter for short-circuit responses. Instead, we
// stamp the response via huma.Context's SetStatus / SetHeader /
// BodyWriter helpers, then return without calling next. The
// WWW-Authenticate challenge is set via SetHeader so the frontend's
// axios interceptor can distinguish a refresh-eligible 401 (token
// expired) from a hard-logout 401 (anything else).
func NewHumaAuth(tokens auth.TokenService) func(huma.Context, func(huma.Context)) {
	return func(ctx huma.Context, next func(huma.Context)) {
		r := reqctx.RequestFromContext(ctx.Context())
		raw, ok := bearerFromHeader(r)
		if !ok {
			ctx.SetHeader("WWW-Authenticate", wwwAuthInvalid)
			writeHumaError(ctx, http.StatusUnauthorized, "Authentication required", nil)
			return
		}

		claims, err := tokens.ValidateToken(raw)
		if err != nil {
			// Set the challenge before writeHumaError so it lands in
			// the headers — once SetStatus / BodyWriter runs, headers
			// are flushed and a later Set is a no-op.
			if errors.Is(err, auth.ErrExpiredToken) {
				ctx.SetHeader("WWW-Authenticate", wwwAuthExpired)
			} else {
				ctx.SetHeader("WWW-Authenticate", wwwAuthInvalid)
			}
			writeHumaError(ctx, http.StatusUnauthorized, "Invalid or expired token", api.ClassifyAndLog(ctx.Context(), err, "invalid token"))
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
// envelope via huma's response helpers. The request ID is read from the
// context so the correlation ID matches the log line.
func writeHumaError(ctx huma.Context, status int, message string, details interface{}) {
	body := api.NewErrorResponseFromContext(ctx.Context(), status, message, details)
	ctx.SetStatus(status)
	ctx.SetHeader("Content-Type", "application/json; charset=utf-8")
	_ = api.EncodeError(ctx.BodyWriter(), body)
}
