package user

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"nyx/internal/platform/auth"
	"nyx/internal/reqctx"
)

// refreshTokenFromContext reads the __Host-nyx-refresh cookie off
// the underlying http.Request that the StoreRequest middleware has
// stashed on the context. huma v2's generic handler signature is
// func(context.Context, *I) (*O, error) — the handler doesn't get
// huma.Context directly, so we recover the live *http.Request via
// reqctx.RequestFromContext and call r.Cookie(name).
//
// Returns ("", false) when the request is unavailable (unit tests
// without HTTP) or the cookie is missing / empty.
func refreshTokenFromContext(ctx context.Context, cfg auth.CookieConfig) (string, bool) {
	r := reqctx.RequestFromContext(ctx)
	if r == nil {
		return "", false
	}
	c, err := r.Cookie(resolveCookieName(cfg.RefreshName, cfg))
	if err != nil || c.Value == "" {
		return "", false
	}
	return c.Value, true
}

// resolveCookieName returns the cookie name that the browser will
// have stored. Mirrors auth.CookieConfig.hostPrefixed: the __Host-
// prefix survives only when Secure=true AND Domain==""; otherwise
// the prefix is stripped. Doing the same arithmetic here keeps set /
// get paths in lockstep — a mismatch would silently break every
// authenticated request.
func resolveCookieName(name string, cfg auth.CookieConfig) string {
	if cfg.Secure && cfg.Domain == "" {
		return name
	}
	return strings.TrimPrefix(name, "__Host-")
}

// sameSiteString renders http.SameSite as the value Set-Cookie
// expects. We use a switch rather than a method because http.SameSite
// (int) has no String() in the standard library.
func sameSiteString(s http.SameSite) string {
	switch s {
	case http.SameSiteStrictMode:
		return "Strict"
	case http.SameSiteNoneMode:
		return "None"
	default:
		return "Lax"
	}
}

// buildSetCookieHeader returns a string suitable for a Set-Cookie
// header value. Assembled manually (rather than calling
// http.Cookie.String) so the formatting is byte-identical across
// the set and clear paths and tests can pin the exact wire format.
//
// The cookie name comes from resolveCookieName, not cfg directly —
// the same rule as the read path. MaxAge < 0 emits Max-Age=0 (the
// standard browser-expire idiom) regardless of the magnitude; we
// never produce a negative Max-Age because some browsers treat it
// as a session cookie instead of clearing.
func buildSetCookieHeader(value, name string, maxAge time.Duration, cfg auth.CookieConfig) string {
	resolvedName := resolveCookieName(name, cfg)

	parts := []string{
		resolvedName + "=" + value,
		"Path=/",
		"HttpOnly",
	}
	// maxAge < 0 → expire immediately. The browser only honours
	// Max-Age=0 in conjunction with an empty value, so we emit both
	// when clearing.
	if maxAge < 0 {
		parts = append(parts, "Max-Age=0")
	} else {
		parts = append(parts, "Max-Age="+strconv.Itoa(int(maxAge.Seconds())))
	}
	if cfg.Secure {
		parts = append(parts, "Secure")
	}
	if cfg.SameSite != http.SameSiteDefaultMode {
		parts = append(parts, "SameSite="+sameSiteString(cfg.SameSite))
	}
	if cfg.Domain != "" {
		parts = append(parts, "Domain="+cfg.Domain)
	}
	return strings.Join(parts, "; ")
}

// issueSetCookieStrings builds the Set-Cookie header values for
// the access + refresh cookies and returns them so the handler can
// stamp them onto the huma output struct's []string field. One
// header value per cookie so the browser stores them as a pair.
func issueSetCookieStrings(res *AuthResult, cfg auth.CookieConfig) []string {
	return []string{
		buildSetCookieHeader(res.AccessToken, cfg.AccessName, cfg.AccessMaxAge, cfg),
		buildSetCookieHeader(res.RefreshToken, cfg.RefreshName, cfg.RefreshMaxAge, cfg),
	}
}

// clearSetCookieStrings builds the Set-Cookie header values that
// expire both auth cookies at the browser. MaxAge=-1 inside the
// builder maps to Max-Age=0 (see buildSetCookieHeader), the
// standard browser-expire idiom.
func clearSetCookieStrings(cfg auth.CookieConfig) []string {
	return []string{
		buildSetCookieHeader("", cfg.AccessName, -1, cfg),
		buildSetCookieHeader("", cfg.RefreshName, -1, cfg),
	}
}

// toResponse strips the tokens off AuthResult so only the
// JSON-serialisable parts reach the client. The handler never lets
// AccessToken / RefreshToken escape into the response body — they
// ride the Set-Cookie headers, exclusively.
func (r *AuthResult) toResponse() AuthResponse {
	return AuthResponse{
		ExpiresAt: r.ExpiresAt,
		User:      r.User,
	}
}
