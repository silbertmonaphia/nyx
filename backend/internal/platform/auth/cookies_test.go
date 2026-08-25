package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCookieConfig_HostPrefixed(t *testing.T) {
	tests := []struct {
		name     string
		cfg      CookieConfig
		in, want string
	}{
		{
			name: "secure + empty domain keeps __Host- prefix",
			cfg:  CookieConfig{Secure: true, Domain: ""},
			in:   "__Host-nyx-access",
			want: "__Host-nyx-access",
		},
		{
			name: "insecure drops the prefix (dev escape hatch)",
			cfg:  CookieConfig{Secure: false, Domain: ""},
			in:   "__Host-nyx-access",
			want: "nyx-access",
		},
		{
			name: "domain set drops the prefix (would be rejected)",
			cfg:  CookieConfig{Secure: true, Domain: ".example.com"},
			in:   "__Host-nyx-access",
			want: "nyx-access",
		},
		{
			name: "non-prefixed names pass through",
			cfg:  CookieConfig{Secure: true, Domain: ""},
			in:   "nyx-refresh",
			want: "nyx-refresh",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.hostPrefixed(tt.in); got != tt.want {
				t.Errorf("hostPrefixed(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestSetAuthCookies(t *testing.T) {
	cfg := CookieConfig{
		Secure:        true,
		Domain:        "",
		AccessName:    "__Host-nyx-access",
		RefreshName:   "__Host-nyx-refresh",
		AccessMaxAge:  15 * time.Minute,
		RefreshMaxAge: 7 * 24 * time.Hour,
		SameSite:      http.SameSiteLaxMode,
	}

	rec := httptest.NewRecorder()
	SetAuthCookies(rec, "access-raw", "refresh-raw", cfg)

	cookies := rec.Result().Cookies()
	if got := len(cookies); got != 2 {
		t.Fatalf("expected 2 cookies, got %d", got)
	}

	// Build a map for order-independent assertions.
	byName := make(map[string]*http.Cookie, len(cookies))
	for _, c := range cookies {
		byName[c.Name] = c
	}

	access, ok := byName["__Host-nyx-access"]
	if !ok {
		t.Fatal("expected __Host-nyx-access cookie")
	}
	assertAccessCookie(t, access, "access-raw", 15*time.Minute)

	refresh, ok := byName["__Host-nyx-refresh"]
	if !ok {
		t.Fatal("expected __Host-nyx-refresh cookie")
	}
	assertRefreshCookie(t, refresh, "refresh-raw", 7*24*time.Hour)
}

// assertAccessCookie checks every invariant the access cookie must
// hold: HttpOnly + Secure + SameSite + Path=/ + Domain="" +
// non-empty value + Max-Age matching the configured TTL. Pulled out
// of TestSetAuthCookies to keep that test under the cyclomatic-
// complexity ceiling (gocyclo). Same shape for refresh.
func assertAccessCookie(t *testing.T, c *http.Cookie, wantValue string, wantTTL time.Duration) {
	t.Helper()
	if c.Value != wantValue {
		t.Errorf("access value = %q, want %q", c.Value, wantValue)
	}
	if !c.HttpOnly {
		t.Error("access cookie must be HttpOnly")
	}
	if !c.Secure {
		t.Error("access cookie must be Secure")
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Errorf("access SameSite = %v, want Lax", c.SameSite)
	}
	if c.Path != "/" {
		t.Errorf("access Path = %q, want /", c.Path)
	}
	if c.Domain != "" {
		t.Errorf("access Domain = %q, want empty (__Host- requires no Domain)", c.Domain)
	}
	wantMaxAge := int(wantTTL.Seconds())
	if c.MaxAge != wantMaxAge {
		t.Errorf("access MaxAge = %d, want %d", c.MaxAge, wantMaxAge)
	}
}

func assertRefreshCookie(t *testing.T, c *http.Cookie, wantValue string, wantTTL time.Duration) {
	t.Helper()
	if c.Value != wantValue {
		t.Errorf("refresh value = %q, want %q", c.Value, wantValue)
	}
	if !c.HttpOnly {
		t.Error("refresh cookie must be HttpOnly")
	}
	if !c.Secure {
		t.Error("refresh cookie must be Secure")
	}
	wantMaxAge := int(wantTTL.Seconds())
	if c.MaxAge != wantMaxAge {
		t.Errorf("refresh MaxAge = %d, want %d", c.MaxAge, wantMaxAge)
	}
}

func TestSetAuthCookies_DevInsecureStripsPrefix(t *testing.T) {
	cfg := CookieConfig{
		Secure:        false,
		Domain:        "",
		AccessName:    "__Host-nyx-access",
		RefreshName:   "__Host-nyx-refresh",
		AccessMaxAge:  15 * time.Minute,
		RefreshMaxAge: 7 * 24 * time.Hour,
		SameSite:      http.SameSiteLaxMode,
	}

	rec := httptest.NewRecorder()
	SetAuthCookies(rec, "a", "r", cfg)

	cookies := rec.Result().Cookies()
	for _, c := range cookies {
		if c.Secure {
			t.Errorf("cookie %q must NOT be Secure in dev mode", c.Name)
		}
		if c.Name != "nyx-access" && c.Name != "nyx-refresh" {
			t.Errorf("cookie name = %q, want __Host- prefix stripped", c.Name)
		}
	}
}

func TestClearAuthCookies(t *testing.T) {
	cfg := CookieConfig{
		Secure:       true,
		Domain:       "",
		AccessName:   "__Host-nyx-access",
		RefreshName:  "__Host-nyx-refresh",
		AccessMaxAge: 15 * time.Minute,
		SameSite:     http.SameSiteLaxMode,
	}

	rec := httptest.NewRecorder()
	ClearAuthCookies(rec, cfg)

	cookies := rec.Result().Cookies()
	if got := len(cookies); got != 2 {
		t.Fatalf("expected 2 cookies, got %d", got)
	}
	for _, c := range cookies {
		if c.MaxAge >= 0 {
			t.Errorf("clear cookie %q must have MaxAge < 0 (got %d)", c.Name, c.MaxAge)
		}
		if c.Value != "" {
			t.Errorf("clear cookie %q must have empty value (got %q)", c.Name, c.Value)
		}
		if !c.HttpOnly {
			t.Errorf("clear cookie %q must be HttpOnly", c.Name)
		}
	}
}

func TestAccessTokenFromCookie(t *testing.T) {
	cfg := CookieConfig{
		Secure:     true,
		AccessName: "__Host-nyx-access",
	}

	t.Run("present", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.AddCookie(&http.Cookie{Name: "__Host-nyx-access", Value: "the-jwt"})
		got, ok := AccessTokenFromCookie(r, cfg)
		if !ok {
			t.Fatal("expected ok=true")
		}
		if got != "the-jwt" {
			t.Errorf("got %q, want %q", got, "the-jwt")
		}
	})

	t.Run("absent", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		got, ok := AccessTokenFromCookie(r, cfg)
		if ok {
			t.Errorf("expected ok=false, got value %q", got)
		}
	})

	t.Run("empty value", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.AddCookie(&http.Cookie{Name: "__Host-nyx-access", Value: ""})
		got, ok := AccessTokenFromCookie(r, cfg)
		if ok {
			t.Errorf("expected ok=false for empty cookie, got value %q", got)
		}
	})
}

func TestRefreshTokenFromCookie(t *testing.T) {
	cfg := CookieConfig{
		Secure:      true,
		RefreshName: "__Host-nyx-refresh",
	}

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(&http.Cookie{Name: "__Host-nyx-refresh", Value: "the-refresh"})
	got, ok := RefreshTokenFromCookie(r, cfg)
	if !ok || got != "the-refresh" {
		t.Errorf("got (%q, %v), want (%q, true)", got, ok, "the-refresh")
	}
}
