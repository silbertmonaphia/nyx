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

	handler := NewAuth(tokens, "__Host-nyx-access", true)(downstream)

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
	handler := NewAuth(tokens, "__Host-nyx-access", true)(downstream)

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
	handler := NewAuth(tokens, "__Host-nyx-access", true)(downstream)

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
		Middlewares: huma.Middlewares{NewHumaAuth(tokens, "__Host-nyx-access", true)},
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
		Middlewares: huma.Middlewares{NewHumaAuth(tokens, "__Host-nyx-access", true)},
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
