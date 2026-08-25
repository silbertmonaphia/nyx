package auth

import (
	"net/http"
	"strings"
	"time"
)

// CookieConfig is the single source of truth for auth-cookie
// attributes. Config.CookieSettings() produces one of these at
// startup; handlers pass it to SetAuthCookies / ClearAuthCookies so
// every cookie emission flows through one helper and is easy to
// audit. Field comments document the constraints the browser
// enforces; do not add fields that weaken them.
type CookieConfig struct {
	// Secure forces HTTPS. The browser silently drops the cookie if
	// Secure=true over an http connection — that is the intended
	// fail-closed behaviour. The dev escape hatch is
	// COOKIE_SECURE=false (only when serving from localhost over http).
	Secure bool

	// Domain is intentionally empty by default. Setting it would
	// invalidate the __Host- name prefix (browsers refuse __Host-
	// cookies with a Domain attribute); if cross-subdomain sharing is
	// ever required, switch the name prefix to __Secure- and document
	// the downgrade.
	Domain string

	// AccessName / RefreshName carry the raw values; the prefix is
	// applied by the browser. Production uses __Host-nyx-* (deny-by-
	// default); dev (localhost http) uses plain nyx-* names because
	// __Host- requires Secure which forces HTTPS.
	AccessName  string
	RefreshName string

	// AccessMaxAge / RefreshMaxAge are Max-Age in seconds (the
	// browser's TTL). Set as http.Cookie.MaxAge — the cookie expires
	// at the end of the session when MaxAge < 0. We always set
	// MaxAge so the lifetime is unambiguous.
	AccessMaxAge  time.Duration
	RefreshMaxAge time.Duration

	// SameSite is parsed once at startup. Lax permits same-site XHR
	// (the only traffic the SPA produces) and rejects cross-site
	// POSTs that would carry credentials. Strict is the most
	// restrictive. None disables the browser-enforced CSRF guard.
	SameSite http.SameSite
}

// hostPrefixed reports whether the cookie name should keep its
// __Host- prefix. The prefix only works when Secure=true AND
// Domain=="" — failing either constraint makes the browser refuse
// the cookie. The dev escape hatch (Secure=false) therefore strips
// the prefix automatically.
func (c CookieConfig) hostPrefixed(name string) string {
	if c.Secure && c.Domain == "" {
		return name
	}
	// Strip the __Host- prefix if it slipped through into the
	// configured name. Defensive — config defaults are correct, but
	// operators who customise the name shouldn't have to remember
	// the prefix rules.
	return strings.TrimPrefix(name, "__Host-")
}

// SetAuthCookies writes the access and refresh cookies via a single
// call to http.SetCookie per cookie. Both are HttpOnly so the SPA's
// JavaScript never sees them; SameSite follows CookieConfig; Max-Age
// matches the respective JWT TTL so the browser expires them
// alongside the server-side credential.
//
// Use this from every handler that mints credentials (Login,
// Register, Refresh). The previous gin-era code path returned the
// tokens in the JSON body, which left them exposed to any XSS; this
// helper is the single point where the wire contract changed.
func SetAuthCookies(w http.ResponseWriter, accessToken, refreshToken string, cfg CookieConfig) {
	http.SetCookie(w, buildAuthCookie(accessToken, cfg.AccessName, cfg.AccessMaxAge, cfg))
	http.SetCookie(w, buildAuthCookie(refreshToken, cfg.RefreshName, cfg.RefreshMaxAge, cfg))
}

// ClearAuthCookies expires both cookies at the browser by emitting
// them with MaxAge=0. Path is set explicitly to match the set path
// (browser requires Path to match for a Set-Cookie to clear an
// existing one). Used by Logout.
func ClearAuthCookies(w http.ResponseWriter, cfg CookieConfig) {
	for _, name := range []string{cfg.AccessName, cfg.RefreshName} {
		c := buildAuthCookie("", name, -1, cfg)
		c.MaxAge = -1
		http.SetCookie(w, c)
	}
}

// buildAuthCookie assembles an http.Cookie with the attributes that
// every auth cookie must share: HttpOnly, SameSite, Path=/, and the
// resolved (possibly __Host- stripped) name. MaxAge and Value are
// caller-controlled so Set vs Clear can reuse the same template.
func buildAuthCookie(value, name string, maxAge time.Duration, cfg CookieConfig) *http.Cookie {
	return &http.Cookie{
		Name:     cfg.hostPrefixed(name),
		Value:    value,
		Path:     "/",
		MaxAge:   int(maxAge.Seconds()),
		Secure:   cfg.Secure,
		HttpOnly: true,
		SameSite: cfg.SameSite,
		Domain:   cfg.Domain,
	}
}

// AccessTokenFromCookie pulls the access cookie value out of an
// http.Request. Returns ("", false) if the cookie is absent. Used by
// the auth middleware as the primary token source — Authorization:
// Bearer is the fallback for curl/Postman during the rollout window.
func AccessTokenFromCookie(r *http.Request, cfg CookieConfig) (string, bool) {
	c, err := r.Cookie(cfg.hostPrefixed(cfg.AccessName))
	if err != nil || c.Value == "" {
		return "", false
	}
	return c.Value, true
}

// RefreshTokenFromCookie is the mirror of AccessTokenFromCookie for
// the refresh cookie. Read by the Refresh / Logout handlers.
func RefreshTokenFromCookie(r *http.Request, cfg CookieConfig) (string, bool) {
	c, err := r.Cookie(cfg.hostPrefixed(cfg.RefreshName))
	if err != nil || c.Value == "" {
		return "", false
	}
	return c.Value, true
}
