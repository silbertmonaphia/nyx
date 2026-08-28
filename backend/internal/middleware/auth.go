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

// bearerPrefix is the case-insensitive scheme the middleware accepts.
// RFC 6750 §2.1 specifies "Bearer" (capitalised) but most clients
// send it case-insensitively; we accept any case to be friendly to
// hand-rolled clients (curl, mobile SDKs, game engines) without
// weakening the contract.
const bearerPrefix = "bearer "

// NewAuth returns a chi middleware that validates JWTs using the
// injected TokenService. It is the stdlib-shaped counterpart to
// NewHumaAuth and is intended for mounting on protected sub-routers
// (e.g., a future /api/admin/... group) — keep it exported for that
// future use.
//
// Token source: the Authorization request header (RFC 6750). The
// previous httpOnly-cookie transport has been replaced with Bearer
// so native clients (iOS, Android, Unity/Unreal game binaries,
// console SDKs) can speak the same wire contract as the SPA. A
// single Authorization header on a cross-origin XHR triggers a CORS
// preflight, which is the natural CSRF defence — no SameSite cookie
// and no token flow needed.
//
// On success, the resolved user ID and username are stamped on the
// context via reqctx.WithUserID / reqctx.WithUsername so downstream
// handlers can read them via reqctx.UserIDFromContext /
// UsernameFromContext. On failure, the canonical {error, code,
// request_id, details} envelope is written and the chain is
// short-circuited. The WWW-Authenticate header distinguishes "expired"
// (refresh-eligible) from "invalid" (force logout) without leaking
// the underlying error in the body.
//
// huma parses the request body before invoking per-operation
// Middlewares, so a 1 MiB body cap is applied at the router level in
// main.go to mitigate pre-auth DoS via large payloads.
func NewAuth(tokens auth.TokenService) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, ok := bearerFromHeader(r)
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

// bearerFromHeader extracts a JWT from the Authorization request
// header per RFC 6750 §2.1. Accepts case-insensitive "Bearer "
// prefix; rejects every other scheme ("Token", "Basic", missing
// scheme) and rejects a present-but-empty token string. The shared
// helper is used by both NewAuth (stdlib) and NewHumaAuth (huma)
// so the read rules can't drift.
//
// Returned bool is false when the header is absent, malformed, or
// carries a non-Bearer scheme — callers treat that as "no
// credential supplied" and 401.
func bearerFromHeader(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if h == "" {
		return "", false
	}
	if len(h) <= len(bearerPrefix) || !strings.EqualFold(h[:len(bearerPrefix)], bearerPrefix) {
		return "", false
	}
	raw := strings.TrimSpace(h[len(bearerPrefix):])
	if raw == "" {
		return "", false
	}
	return raw, true
}
