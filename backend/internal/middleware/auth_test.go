package middleware

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	platapi "nyx/internal/platform/api"
	"nyx/internal/platform/auth"
	"nyx/internal/reqctx"
)

// stubTokens is an auth.TokenService that always returns the supplied
// error from ValidateToken. The middleware must never echo that error
// text to the client — it goes to zerolog instead.
type stubTokens struct{ validateErr error }

func (s *stubTokens) GenerateToken(int, string) (string, error) {
	return "", errors.New("not used in these tests")
}
func (s *stubTokens) ValidateToken(string) (*auth.Claims, error) {
	return nil, s.validateErr
}

// newCtxWithReqID returns a context carrying the supplied request ID,
// mirroring what the upstream RequestID middleware would have stamped.
func newCtxWithReqID(req *http.Request, id string) *http.Request {
	return req.WithContext(reqctx.WithRequestID(req.Context(), id))
}

// captureLogger swaps the global zerolog logger for one that writes
// into the returned buffer; t.Cleanup restores the previous logger.
func captureLogger(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := log.Logger
	log.Logger = zerolog.New(buf).Level(zerolog.WarnLevel)
	t.Cleanup(func() { log.Logger = prev })
	return buf
}

// ---- Stdlib-shaped auth middleware ----

// TestAuth_InvalidTokenHidesInternalDetails pins the wire contract of
// commit 4 for the stdlib auth middleware: when ValidateToken returns
// a parser-flavoured error, the response body must carry only the
// static "invalid token" detail (and the canonical "Invalid or expired
// token" message) — never the underlying error text. The wrapped
// error must still appear in zerolog so operators can correlate.
//
// captureLogger swaps the global zerolog.Logger; tests that use it
// must NOT call t.Parallel(). Adding parallelism here would race the
// captured buffer against sibling tests' log writes.
//
//nolint:paralleltest
func TestAuth_InvalidTokenHidesInternalDetails(t *testing.T) {
	buf := captureLogger(t)

	rawErr := errors.New("crypto/hmac: invalid authentication code (token signature mismatch)")
	tokens := &stubTokens{validateErr: rawErr}

	downstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("downstream handler must not be reached on invalid token")
		w.WriteHeader(http.StatusOK)
	})

	handler := NewAuth(tokens)(downstream)

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Authorization", "Bearer abc.def.ghi")
	req = newCtxWithReqID(req, "req-stdlib")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", rr.Code, rr.Body.String())
	}

	var env platapi.ErrorResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal envelope: %v; body=%s", err, rr.Body.String())
	}
	if env.Message != "Invalid or expired token" {
		t.Errorf("envelope.error = %q, want %q", env.Message, "Invalid or expired token")
	}
	if env.Details != "invalid token" {
		t.Errorf("envelope.details = %v, want %q", env.Details, "invalid token")
	}
	if env.RequestID != "req-stdlib" {
		t.Errorf("envelope.request_id = %q, want %q", env.RequestID, "req-stdlib")
	}
	if strings.Contains(rr.Body.String(), "crypto/hmac") || strings.Contains(rr.Body.String(), "signature mismatch") {
		t.Errorf("response leaks JWT parser internals: %s", rr.Body.String())
	}

	logged := buf.String()
	if !strings.Contains(logged, "crypto/hmac") {
		t.Errorf("expected underlying error logged for operators, got: %s", logged)
	}
	if !strings.Contains(logged, "req-stdlib") {
		t.Errorf("expected request_id req-stdlib in log, got: %s", logged)
	}
}

// TestAuth_ExpiredTokenSetsExpiredChallenge pins the refresh-on-401
// wire contract: when ValidateToken returns ErrExpiredToken, the
// response carries `WWW-Authenticate: Bearer error="invalid_token",
// error_description="expired"`. That's the marker the frontend's
// axios interceptor keys on to trigger single-flight refresh instead
// of hard-logout. The body must still say "Invalid or expired token"
// — never the raw parser error.
func TestAuth_ExpiredTokenSetsExpiredChallenge(t *testing.T) {
	tokens := &stubTokens{validateErr: auth.ErrExpiredToken}

	downstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("downstream handler must not be reached on expired token")
		w.WriteHeader(http.StatusOK)
	})
	handler := NewAuth(tokens)(downstream)

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Authorization", "Bearer expired.jwt.token")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("WWW-Authenticate"); got != `Bearer error="invalid_token", error_description="expired"` {
		t.Errorf("WWW-Authenticate = %q, want %q", got, `Bearer error="invalid_token", error_description="expired"`)
	}

	var env platapi.ErrorResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal envelope: %v; body=%s", err, rr.Body.String())
	}
	if env.Message != "Invalid or expired token" {
		t.Errorf("envelope.error = %q, want %q", env.Message, "Invalid or expired token")
	}
}

// TestAuth_InvalidTokenSetsBareChallenge covers the non-expired
// failure mode: a parser-style error (signature mismatch, malformed
// token, etc.) sets the bare `Bearer error="invalid_token"`
// challenge. The frontend treats this as a hard logout because the
// token can't be revived by /api/refresh.
func TestAuth_InvalidTokenSetsBareChallenge(t *testing.T) {
	rawErr := errors.New("crypto/hmac: invalid authentication code")
	tokens := &stubTokens{validateErr: rawErr}

	downstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("downstream handler must not be reached on invalid token")
		w.WriteHeader(http.StatusOK)
	})
	handler := NewAuth(tokens)(downstream)

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Authorization", "Bearer garbage")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("WWW-Authenticate"); got != `Bearer error="invalid_token"` {
		t.Errorf("WWW-Authenticate = %q, want %q", got, `Bearer error="invalid_token"`)
	}

	var env platapi.ErrorResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal envelope: %v; body=%s", err, rr.Body.String())
	}
	if env.Message != "Invalid or expired token" {
		t.Errorf("envelope.error = %q, want %q", env.Message, "Invalid or expired token")
	}
}

// TestAuth_BearerHeaderAccepted pins the Bearer-on-the-wire contract:
// when a request carries a syntactically valid Authorization header
// and ValidateToken succeeds, the downstream handler runs and the
// response is the handler's, not a 401. This replaces the previous
// "Bearer ignored" test — cookies have been retired in favour of
// Bearer so the SPA, native mobile, and game clients can share the
// same wire contract.
func TestAuth_BearerHeaderAccepted(t *testing.T) {
	// happyTokens returns a valid Claims with no error.
	tokens := &happyTokens{}

	downstreamCalled := false
	downstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downstreamCalled = true
		// Verify the claims were stamped on the context — downstream
		// handlers depend on this for user identity.
		if uid := reqctx.UserIDFromContext(r.Context()); uid != 42 {
			t.Errorf("reqctx.UserIDFromContext = %d, want 42", uid)
		}
		if name := reqctx.UsernameFromContext(r.Context()); name != "alice" {
			t.Errorf("reqctx.UsernameFromContext = %q, want alice", name)
		}
		w.WriteHeader(http.StatusNoContent)
	})

	handler := NewAuth(tokens)(downstream)

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Authorization", "Bearer valid.jwt.token")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if !downstreamCalled {
		t.Fatal("downstream handler must run when Bearer is valid")
	}
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("WWW-Authenticate"); got != "" {
		t.Errorf("WWW-Authenticate = %q, want empty on success", got)
	}
}

// TestAuth_MissingAuthorizationReturns401 pins the "no credential"
// failure path. A request without an Authorization header must be
// rejected as unauthenticated with the bare Bearer challenge. This
// is what cross-origin clients see if they forget to stamp the
// header, and what the SPA sees on cold start before /api/refresh
// has produced a token.
func TestAuth_MissingAuthorizationReturns401(t *testing.T) {
	tokens := &happyTokens{}

	downstreamCalled := false
	downstream := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		downstreamCalled = true
	})

	handler := NewAuth(tokens)(downstream)
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if downstreamCalled {
		t.Error("downstream handler must not run without Authorization header")
	}
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("WWW-Authenticate"); got != `Bearer error="invalid_token"` {
		t.Errorf("WWW-Authenticate = %q, want %q", got, `Bearer error="invalid_token"`)
	}

	var env platapi.ErrorResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal envelope: %v; body=%s", err, rr.Body.String())
	}
	if env.Message != "Authentication required" {
		t.Errorf("envelope.error = %q, want %q", env.Message, "Authentication required")
	}
}

// TestAuth_MalformedBearerReturns401 pins the "wrong scheme / empty
// token" path. A header that carries something other than "Bearer"
// (e.g. "Token xyz", "Basic xyz") or a "Bearer " with no token must
// be rejected as unauthenticated — only RFC 6750's Bearer scheme is
// accepted. Hand-rolled clients and old curl invocations are the
// usual sources of these mistakes; the bare challenge tells the
// caller the contract is Bearer-only.
func TestAuth_MalformedBearerReturns401(t *testing.T) {
	cases := []struct {
		name  string
		value string
	}{
		{"wrong scheme", "Token xyz"},
		{"basic scheme", "Basic dXNlcjpwYXNz"},
		{"empty bearer", "Bearer "},
		{"bare bearer no space", "Bearer"},
	}
	tokens := &happyTokens{}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			downstream := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Error("downstream must not run on malformed Authorization")
			})
			handler := NewAuth(tokens)(downstream)

			req := httptest.NewRequest(http.MethodGet, "/x", nil)
			req.Header.Set("Authorization", tc.value)
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)

			if rr.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401; body=%s", rr.Code, rr.Body.String())
			}
			if got := rr.Header().Get("WWW-Authenticate"); got != `Bearer error="invalid_token"` {
				t.Errorf("WWW-Authenticate = %q, want bare invalid_token challenge", got)
			}
		})
	}
}

// happyTokens is a TokenService that always succeeds. Used by tests
// that don't care about JWT parsing internals — they only assert the
// middleware's routing of valid vs invalid credentials.
type happyTokens struct{}

func (h *happyTokens) GenerateToken(int, string) (string, error) {
	return "", errors.New("not used in these tests")
}
func (h *happyTokens) ValidateToken(string) (*auth.Claims, error) {
	return &auth.Claims{UserID: 42, Username: "alice"}, nil
}

// ---- Huma-shaped auth middleware ----

// TestHumaAuth_InvalidTokenHidesInternalDetails covers the huma adapter
// path used as per-operation middleware on protected movie routes.
// Same wire contract: static "invalid token" in details, raw error
// suppressed to zerolog.
//
// captureLogger swaps the global zerolog.Logger; tests that use it
// must NOT call t.Parallel(). Adding parallelism here would race the
// captured buffer against sibling tests' log writes.
//
//nolint:paralleltest
func TestHumaAuth_InvalidTokenHidesInternalDetails(t *testing.T) {
	buf := captureLogger(t)

	rawErr := errors.New("jwt: token is unverifiable: signature is invalid")
	tokens := &stubTokens{validateErr: rawErr}

	// Wire the middleware onto a minimal huma API so we exercise the
	// huma.Context code path that handlers actually use.
	router := chi.NewMux()
	router.Use(StoreRequest) // huma middleware reads r via reqctx.RequestFromContext
	hapi := humachi.New(router, huma.Config{
		OpenAPI: &huma.OpenAPI{
			OpenAPI: "3.1.0",
			Info:    &huma.Info{Title: "auth test", Version: "0.0.0"},
		},
		Formats:       huma.DefaultFormats,
		DefaultFormat: "application/json",
	})

	hit := false
	huma.Register(hapi, huma.Operation{
		OperationID: "guarded",
		Method:      http.MethodGet,
		Path:        "/guarded",
		Middlewares: huma.Middlewares{NewHumaAuth(tokens)},
	}, func(_ context.Context, _ *struct{}) (*struct{}, error) {
		hit = true
		return &struct{}{}, nil
	})

	req := httptest.NewRequest(http.MethodGet, "/guarded", nil)
	req.Header.Set("Authorization", "Bearer abc.def.ghi")
	req = newCtxWithReqID(req, "req-huma")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if hit {
		t.Error("downstream handler must not run when token is invalid")
	}
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", rr.Code, rr.Body.String())
	}

	var env platapi.ErrorResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal envelope: %v; body=%s", err, rr.Body.String())
	}
	if env.Details != "invalid token" {
		t.Errorf("envelope.details = %v, want %q", env.Details, "invalid token")
	}
	if strings.Contains(rr.Body.String(), "unverifiable") || strings.Contains(rr.Body.String(), "signature is invalid") {
		t.Errorf("response leaks JWT parser internals: %s", rr.Body.String())
	}

	logged := buf.String()
	if !strings.Contains(logged, "unverifiable") {
		t.Errorf("expected underlying error logged for operators, got: %s", logged)
	}
	if !strings.Contains(logged, "req-huma") {
		t.Errorf("expected request_id req-huma in log, got: %s", logged)
	}
}

// TestHumaAuth_ExpiredTokenSetsExpiredChallenge is the huma-shaped
// twin of TestAuth_ExpiredTokenSetsExpiredChallenge. The
// per-operation middleware path is what handlers actually use on
// protected routes; the challenge must surface the same
// error_description="expired" marker for the frontend to key on.
func TestHumaAuth_ExpiredTokenSetsExpiredChallenge(t *testing.T) {
	tokens := &stubTokens{validateErr: auth.ErrExpiredToken}

	router := chi.NewMux()
	router.Use(StoreRequest) // huma middleware reads r via reqctx.RequestFromContext
	hapi := humachi.New(router, huma.Config{
		OpenAPI: &huma.OpenAPI{
			OpenAPI: "3.1.0",
			Info:    &huma.Info{Title: "auth test", Version: "0.0.0"},
		},
		Formats:       huma.DefaultFormats,
		DefaultFormat: "application/json",
	})

	hit := false
	huma.Register(hapi, huma.Operation{
		OperationID: "guarded",
		Method:      http.MethodGet,
		Path:        "/guarded",
		Middlewares: huma.Middlewares{NewHumaAuth(tokens)},
	}, func(_ context.Context, _ *struct{}) (*struct{}, error) {
		hit = true
		return &struct{}{}, nil
	})

	req := httptest.NewRequest(http.MethodGet, "/guarded", nil)
	req.Header.Set("Authorization", "Bearer expired.jwt.token")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if hit {
		t.Error("downstream handler must not run when token is expired")
	}
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("WWW-Authenticate"); got != `Bearer error="invalid_token", error_description="expired"` {
		t.Errorf("WWW-Authenticate = %q, want %q", got, `Bearer error="invalid_token", error_description="expired"`)
	}

	var env platapi.ErrorResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal envelope: %v; body=%s", err, rr.Body.String())
	}
	if env.Message != "Invalid or expired token" {
		t.Errorf("envelope.error = %q, want %q", env.Message, "Invalid or expired token")
	}
}