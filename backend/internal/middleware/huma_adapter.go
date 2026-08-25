package middleware

import (
	"errors"
	"net/http"
	"strings"

	"nyx/internal/platform/api"
	"nyx/internal/platform/auth"
	"nyx/internal/reqctx"

	"github.com/danielgtaylor/huma/v2"
)

// NewHumaAuth returns a huma.Middleware that performs the same JWT
// validation as NewAuth, but in huma's middleware shape
// (func(huma.Context, next func(huma.Context))). tokens carries the
// signing key captured at startup. accessCookieName is the
// configured cookie name (default "__Host-nyx-access"); cookieSecure
// mirrors CookieConfig.Secure so the helper that strips the
// __Host- prefix in dev (Secure=false) matches the read path used
// by handlers.
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
//
// Token source: __Host-nyx-access cookie first (httpOnly, browser
// auto-attaches), then Authorization: Bearer as a deprecation-
// window fallback for curl / Postman clients that haven't been
// updated to the cookie flow. Remove the fallback once all clients
// are cookie-native.
func NewHumaAuth(tokens auth.TokenService, accessCookieName string, cookieSecure bool) func(huma.Context, func(huma.Context)) {
	resolvedCookieName := accessCookieName
	if !cookieSecure {
		resolvedCookieName = strings.TrimPrefix(accessCookieName, "__Host-")
	}

	return func(ctx huma.Context, next func(huma.Context)) {
		raw, ok := readAccessToken(ctx, resolvedCookieName)
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

// readAccessToken pulls the JWT from the __Host-nyx-access cookie
// first; falls back to Authorization: Bearer for the deprecation
// window. Returns ("", false) when neither source is present. The
// Cookie is read via r.Cookie(name) on the *http.Request that
// StoreRequest stashed on the request context — huma's middleware
// path gives us a huma.Context, but the live request is recovered
// from reqctx.RequestFromContext so we can use the standard
// library's cookie parser.
func readAccessToken(ctx huma.Context, cookieName string) (string, bool) {
	r := reqctx.RequestFromContext(ctx.Context())
	if r != nil {
		if c, err := r.Cookie(cookieName); err == nil && c.Value != "" {
			return c.Value, true
		}
	}

	authHeader := ctx.Header("Authorization")
	if authHeader == "" {
		return "", false
	}
	parts := strings.Split(authHeader, " ")
	if len(parts) != 2 || parts[0] != "Bearer" {
		return "", false
	}
	return parts[1], true
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
