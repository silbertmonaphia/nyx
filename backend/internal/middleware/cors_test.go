package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestCORS_AddsHeadersOnNormalRequest verifies the standard response
// headers land on every request (not just OPTIONS). Browsers apply
// CORS to every cross-origin call, not just preflights, so the
// Access-Control-Allow-Origin header MUST accompany the regular
// response too.
func TestCORS_AddsHeadersOnNormalRequest(t *testing.T) {
	downstreamCalled := false
	downstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		downstreamCalled = true
	})
	handler := NewCORS([]string{"*"})(downstream)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/movies", nil)
	handler.ServeHTTP(rr, req)

	if !downstreamCalled {
		t.Error("downstream handler was not called on a non-OPTIONS request")
	}
	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin = %q, want \"*\"", got)
	}
	if got := rr.Header().Get("Access-Control-Allow-Methods"); got == "" {
		t.Error("Access-Control-Allow-Methods is empty")
	}
	if got := rr.Header().Get("Access-Control-Allow-Headers"); got == "" {
		t.Error("Access-Control-Allow-Headers is empty")
	}
}

// TestCORS_OPTIONSShortCircuits is the preflight path: OPTIONS must
// respond with 200 + headers and MUST NOT invoke the downstream
// handler. If the downstream ran, it would see a "no route matched"
// 404 in production because OPTIONS isn't a registered route —
// causing a confusing 404 in browser devtools.
func TestCORS_OPTIONSShortCircuits(t *testing.T) {
	downstreamCalled := false
	downstream := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		downstreamCalled = true
	})
	handler := NewCORS([]string{"*"})(downstream)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodOptions, "/api/movies", nil)
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("OPTIONS status = %d, want 200", rr.Code)
	}
	if downstreamCalled {
		t.Error("OPTIONS preflight invoked the downstream handler; preflight must short-circuit")
	}
	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("preflight: Access-Control-Allow-Origin = %q, want \"*\"", got)
	}
}

// TestCORS_AllowlistMatch echoes the request's Origin header back
// when it's in the allowlist and sets Vary: Origin so caches don't
// conflate responses across different origins.
func TestCORS_AllowlistMatch(t *testing.T) {
	const origin = "https://app.example.com"
	downstreamCalled := false
	downstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		downstreamCalled = true
		w.WriteHeader(http.StatusTeapot)
	})
	handler := NewCORS([]string{origin})(downstream)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/movies", nil)
	req.Header.Set("Origin", origin)
	handler.ServeHTTP(rr, req)

	if !downstreamCalled {
		t.Error("downstream was not called on an allowlisted origin")
	}
	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != origin {
		t.Errorf("Access-Control-Allow-Origin = %q, want %q", got, origin)
	}
	if got := rr.Header().Get("Vary"); got != "Origin" {
		t.Errorf("Vary = %q, want \"Origin\"", got)
	}
}

// TestCORS_AllowlistNoMatch — an Origin that is not on the allowlist
// gets NO Access-Control-Allow-Origin header. The browser will then
// block the response, which is the correct behaviour (we did not
// authorise this caller). The downstream handler still runs (CORS is
// a transport concern; the API itself should keep serving).
func TestCORS_AllowlistNoMatch(t *testing.T) {
	downstreamCalled := false
	downstream := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		downstreamCalled = true
	})
	handler := NewCORS([]string{"https://app.example.com"})(downstream)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/movies", nil)
	req.Header.Set("Origin", "https://attacker.example")
	handler.ServeHTTP(rr, req)

	if !downstreamCalled {
		t.Error("downstream was not called for a non-allowlisted origin; CORS must not block the API itself")
	}
	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Access-Control-Allow-Origin = %q, want \"\" (no header)", got)
	}
	if got := rr.Header().Get("Vary"); got != "" {
		t.Errorf("Vary = %q, want \"\"", got)
	}
}

// TestCORS_PreflightAllowlistMatch — preflight against an
// allowlisted origin must 200 + echo the origin + set Vary: Origin.
func TestCORS_PreflightAllowlistMatch(t *testing.T) {
	const origin = "https://app.example.com"
	downstreamCalled := false
	downstream := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		downstreamCalled = true
	})
	handler := NewCORS([]string{origin})(downstream)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodOptions, "/api/movies", nil)
	req.Header.Set("Origin", origin)
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("OPTIONS status = %d, want 200", rr.Code)
	}
	if downstreamCalled {
		t.Error("OPTIONS preflight invoked the downstream handler; preflight must short-circuit")
	}
	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != origin {
		t.Errorf("preflight Access-Control-Allow-Origin = %q, want %q", got, origin)
	}
	if got := rr.Header().Get("Vary"); got != "Origin" {
		t.Errorf("preflight Vary = %q, want \"Origin\"", got)
	}
}

// TestCORS_PreflightAllowlistNoMatch — preflight against a
// non-allowlisted origin must still 200 (the OPTIONS handler
// short-circuits regardless) but emit NO Access-Control-Allow-Origin
// header, so the browser refuses the actual cross-origin call.
func TestCORS_PreflightAllowlistNoMatch(t *testing.T) {
	downstreamCalled := false
	downstream := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		downstreamCalled = true
	})
	handler := NewCORS([]string{"https://app.example.com"})(downstream)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodOptions, "/api/movies", nil)
	req.Header.Set("Origin", "https://attacker.example")
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("OPTIONS status = %d, want 200", rr.Code)
	}
	if downstreamCalled {
		t.Error("OPTIONS preflight invoked the downstream handler")
	}
	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("preflight Access-Control-Allow-Origin = %q, want \"\"", got)
	}
}

// TestSplitNonEmpty verifies the comma-separated allowlist parser
// used to bridge a single env var into NewCORS's []string argument.
// Empty entries (between two commas) and whitespace-only entries are
// dropped; nil input yields nil output (not an empty slice, so
// NewCORS treats it the same as "no allowlist configured").
func TestSplitNonEmpty(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"*", []string{"*"}},
		{"https://a.example, https://b.example", []string{"https://a.example", "https://b.example"}},
		{"https://a.example,,https://b.example", []string{"https://a.example", "https://b.example"}},
		{"  https://a.example  ", []string{"https://a.example"}},
	}
	for _, tc := range cases {
		got := SplitNonEmpty(tc.in)
		if len(got) != len(tc.want) {
			t.Errorf("SplitNonEmpty(%q) = %v, want %v", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("SplitNonEmpty(%q)[%d] = %q, want %q", tc.in, i, got[i], tc.want[i])
			}
		}
	}
}