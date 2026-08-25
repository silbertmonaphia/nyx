package middleware

import (
	"errors"
	"net/http"
	"strings"

	"nyx/internal/platform/api"
	"nyx/internal/platform/auth"
	"nyx/internal/reqctx"
)

// wwwAuthExpired is the standards-based challenge the middleware
// emits when the access token's expiry has passed. The
// error_description=\"expired\" token is what the frontend's axios
// interceptor keys on to trigger a single-flight refresh instead of a
// hard logout.
const wwwAuthExpired = `Bearer error="invalid_token", error_description="expired"`

// wwwAuthInvalid is the bare challenge for every other failure mode
// (missing header, malformed Authorization, signature mismatch, type
// guard rejection, etc.) — the client treats this as a real auth
// failure.
const wwwAuthInvalid = `Bearer error="invalid_token"`

// NewAuth returns a chi middleware that validates JWTs using the
// injected TokenService. It is the stdlib-shaped counterpart to
// NewHumaAuth and is intended for mounting on protected sub-routers
// (e.g., a future /api/admin/... group) — keep it exported for that
// future use.
//
// accessCookieName is the configured access cookie name
// ("__Host-nyx-access" in prod). cookieSecure mirrors
// CookieConfig.Secure so the prefix-stripping logic in dev matches
// the read path. Token source is the httpOnly cookie only — the
// Authorization: Bearer fallback that shipped during the cookie
// rollout has been removed (see SECURITY.md L1); every legitimate
// client now goes through the browser's automatic cookie attachment.
//
// On success, the resolved user ID and username are stamped on the
// context via reqctx.WithUserID / reqctx.WithUsername so downstream
// handlers can read them via reqctx.UserIDFromContext /
// reqctx.UsernameFromContext. On failure, the canonical {error, code,
// request_id, details} envelope is written and the chain is
// short-circuited. The WWW-Authenticate header distinguishes "expired"
// (refresh-eligible) from "invalid" (force logout) without leaking
// the underlying error in the body.
//
// huma parses the request body before invoking per-operation
// Middlewares, so a 1 MiB body cap is applied at the router level in
// main.go to mitigate pre-auth DoS via large payloads.
func NewAuth(tokens auth.TokenService, accessCookieName string, cookieSecure bool) func(http.Handler) http.Handler {
	resolvedCookieName := accessCookieName
	if !cookieSecure {
		resolvedCookieName = strings.TrimPrefix(accessCookieName, "__Host-")
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, ok := readAccessTokenFromRequest(r, resolvedCookieName)
			if !ok {
				w.Header().Set("WWW-Authenticate", wwwAuthInvalid)
				api.WriteError(w, r, http.StatusUnauthorized, "Authentication required", nil)
				return
			}

			claims, err := tokens.ValidateToken(raw)
			if err != nil {
				// Set the challenge BEFORE WriteError so it's in the
				// response headers — WriteError calls WriteHeader and
				// any later Set is a no-op.
				if errors.Is(err, auth.ErrExpiredToken) {
					w.Header().Set("WWW-Authenticate", wwwAuthExpired)
				} else {
					w.Header().Set("WWW-Authenticate", wwwAuthInvalid)
				}
				api.WriteError(w, r, http.StatusUnauthorized, "Invalid or expired token", api.ClassifyAndLog(r.Context(), err, "invalid token"))
				return
			}

			ctx := reqctx.WithUserID(r.Context(), claims.UserID)
			ctx = reqctx.WithUsername(ctx, claims.Username)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// readAccessTokenFromRequest pulls the JWT from the httpOnly access
// cookie only. The Authorization: Bearer fallback was removed once
// the cookie rollout completed (see SECURITY.md L1); the legacy
// path is no longer reachable from production traffic, and leaving
// it in would re-open the door to credential-leak headers on shared
// infrastructure (proxy logs, CDN caches).
func readAccessTokenFromRequest(r *http.Request, cookieName string) (string, bool) {
	if c, err := r.Cookie(cookieName); err == nil && c.Value != "" {
		return c.Value, true
	}
	return "", false
}
