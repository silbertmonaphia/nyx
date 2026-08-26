package user

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	platapi "nyx/internal/platform/api"
	"nyx/internal/platform/auth"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
	"go.opentelemetry.io/otel/trace/noop"
	"golang.org/x/crypto/bcrypt"

	"nyx/internal/middleware"
)

// TestMain lives in repository_integration_test.go — it both installs
// the huma error override (needed by handler tests below) and boots
// the integration-test Postgres container (skipped under
// SKIP_CONTAINERS=true). Go only allows one TestMain per package.

// testCookieConfig mirrors what cmd/api/main.go feeds production —
// the same struct the real handler threads through to the SetCookie
// emission paths. Values don't have to match production because
// tests don't assert on cookie attributes (those have their own
// tests in the auth package); what's needed is a non-zero
// CookieConfig so NewHandler and RegisterUserOpsTest type-check.
//
// Secure=false mirrors the test ergonomics of "localhost over http":
// the prefix-stripping logic in resolveCookieName then yields the
// bare "nyx-access" / "nyx-refresh" names below.
var testCookieConfig = auth.CookieConfig{
	Secure:        false,
	Domain:        "",
	AccessName:    "__Host-nyx-access",
	RefreshName:   "__Host-nyx-refresh",
	AccessMaxAge:  15 * time.Minute,
	RefreshMaxAge: 7 * 24 * time.Hour,
	SameSite:      1, // http.SameSiteLaxMode
}

// The resolved cookie names (after the Secure=false → prefix-strip
// dance in resolveCookieName). Test code that constructs inbound
// cookies or asserts on Set-Cookie values uses these constants so a
// future flip to Secure=true only needs to change one line.
const (
	testAccessCookieName  = "nyx-access"
	testRefreshCookieName = "nyx-refresh"
)

// setupTestRouter builds a chi + huma router carrying the same user
// operations as the real API. Prometheus, RequestID, Logging, CORS,
// RateLimit, and the body-cap middleware are deliberately skipped —
// they have their own tests and only add noise (and a goroutine, in
// RateLimit's case) here. StoreRequest IS wired in because the
// auth/refresh/logout flows read cookies off the live request via
// reqctx.RequestFromContext; without this middleware, tests would
// silently bypass the cookie path. tokens is threaded through so
// the /api/logout route can attach the JWT middleware in tests the
// same way it does in production.
func setupTestRouter(h *Handler, tokens auth.TokenService) *chi.Mux {
	router := chi.NewMux()
	router.Use(middleware.StoreRequest)
	hapi := humachi.New(router, huma.Config{
		OpenAPI: &huma.OpenAPI{
			OpenAPI: "3.1.0",
			Info:    &huma.Info{Title: "Nyx test", Version: "0.0.0"},
		},
		Formats:       huma.DefaultFormats,
		DefaultFormat: "application/json",
	})
	RegisterUserOpsTest(hapi, h, tokens, testCookieConfig)
	return router
}

// newTestRouterWithRepo is the common three-line arrangement of every
// test below: stub repo → real service → handler → router.
func newTestRouterWithRepo(repo Repository) *chi.Mux {
	tokens, err := auth.NewTokenService([]byte(auth.TestSecret), 15*time.Minute)
	if err != nil {
		panic(err) // test setup; never expected to fail
	}
	return setupTestRouter(NewHandler(NewService(repo, tokens, 15*time.Minute, 7*24*time.Hour, noop.NewTracerProvider().Tracer("test")), testCookieConfig), tokens)
}

// testHash bcrypt-hashes plain at MinCost. The default cost is ~60ms
// per call, which is real time added to every login test; MinCost keeps
// the suite fast while exercising the identical comparison path.
func testHash(t *testing.T, plain string) string {
	t.Helper()
	h, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt.GenerateFromPassword: %v", err)
	}
	return string(h)
}

// postJSON serves an arbitrary JSON body against the router. body is
// marshalled as-is so tests can send partial/invalid payloads that the
// typed request structs could not express.
func postJSON(t *testing.T, router *chi.Mux, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	return rr
}

// postWithRefreshCookie posts an empty JSON body with a refresh
// cookie attached. /api/refresh reads the refresh token from the
// __Host-nyx-refresh cookie (the test config uses Secure=false so
// the prefix is stripped in dev mode and the resolved name is
// "nyx-refresh").
func postWithRefreshCookie(t *testing.T, router *chi.Mux, refreshRaw string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/refresh", bytes.NewReader([]byte("{}")))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: testRefreshCookieName, Value: refreshRaw})
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	return rr
}

// authedLogoutRequest builds a /api/logout request with both the
// access cookie (so the JWT middleware accepts it) and the refresh
// cookie (so the handler can revoke the family). When accessToken is
// empty the caller is testing the auth-rejection path; pass the
// token to exercise the success path.
func authedLogoutRequest(t *testing.T, path, accessToken, refreshRaw string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader([]byte("{}")))
	req.Header.Set("Content-Type", "application/json")
	if accessToken != "" {
		req.AddCookie(&http.Cookie{Name: testAccessCookieName, Value: accessToken})
	}
	if refreshRaw != "" {
		req.AddCookie(&http.Cookie{Name: testRefreshCookieName, Value: refreshRaw})
	}
	return req
}

// decodeEnvelope asserts the response carries the legacy error envelope
// with the expected status code, and returns it for further assertions.
func decodeEnvelope(t *testing.T, rr *httptest.ResponseRecorder, wantStatus int) platapi.ErrorResponse {
	t.Helper()
	if rr.Code != wantStatus {
		t.Fatalf("status = %d, want %d; body=%s", rr.Code, wantStatus, rr.Body.String())
	}
	var env platapi.ErrorResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal envelope: %v; body=%s", err, rr.Body.String())
	}
	if env.Code != wantStatus {
		t.Errorf("envelope.code = %d, want %d", env.Code, wantStatus)
	}
	return env
}

// ---- Register ----

// TestRegisterHandler_Created is the happy path: 201 with the user
// profile in the body and the access + refresh cookies in the
// Set-Cookie headers. The body must not contain tokens — those ride
// the cookies exclusively. The password hash must never appear on
// the wire (User.PasswordHash is json:"-").
func TestRegisterHandler_Created(t *testing.T) {
	repo := &stubRepo{
		createFn: func(_ context.Context, u *User) error {
			u.ID = 42
			u.CreatedAt = time.Now()
			u.UpdatedAt = u.CreatedAt
			return nil
		},
	}
	rr := postJSON(t, newTestRouterWithRepo(repo), "/api/register", RegisterRequest{
		Username: "alice",
		Email:    "alice@example.com",
		Password: "hunter2",
	})

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rr.Code, rr.Body.String())
	}

	var res AuthResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &res); err != nil {
		t.Fatalf("unmarshal AuthResponse: %v", err)
	}
	if res.User.ID != 42 || res.User.Username != "alice" {
		t.Errorf("user = %+v, want ID=42 username=alice", res.User)
	}
	// Tokens must not be in the body — they ride Set-Cookie headers.
	if bytes.Contains(rr.Body.Bytes(), []byte("token")) || bytes.Contains(rr.Body.Bytes(), []byte("refresh_token")) {
		t.Errorf("response body leaks a token field: %s", rr.Body.String())
	}
	if bytes.Contains(rr.Body.Bytes(), []byte("password")) {
		t.Errorf("response body leaks a password field: %s", rr.Body.String())
	}

	// Set-Cookie must include both auth cookies.
	cookies := rr.Result().Cookies()
	var sawAccess, sawRefresh bool
	for _, c := range cookies {
		if c.Name == testAccessCookieName && c.Value != "" {
			sawAccess = true
		}
		if c.Name == testRefreshCookieName && c.Value != "" {
			sawRefresh = true
		}
	}
	if !sawAccess {
		t.Errorf("response missing %s Set-Cookie; got %v", testAccessCookieName, cookies)
	}
	if !sawRefresh {
		t.Errorf("response missing %s Set-Cookie; got %v", testRefreshCookieName, cookies)
	}
}

// TestRegisterHandler_UsernameTakenReturns409 pins the wiring of the
// ErrUsernameTaken branch. Per H5, the wire message collapses to
// "User already exists" regardless of which field collided — the
// service-layer collapse ensures an attacker probing registration
// can't tell whether the username or the email is the one already
// in use (see SECURITY.md H5).
func TestRegisterHandler_UsernameTakenReturns409(t *testing.T) {
	repo := &stubRepo{
		createFn: func(_ context.Context, _ *User) error { return ErrUsernameTaken },
	}
	rr := postJSON(t, newTestRouterWithRepo(repo), "/api/register", RegisterRequest{
		Username: "alice",
		Email:    "alice@example.com",
		Password: "hunter2",
	})

	env := decodeEnvelope(t, rr, http.StatusConflict)
	if env.Message != "User already exists" {
		t.Errorf("envelope.error = %q, want %q", env.Message, "User already exists")
	}
}

// TestRegisterHandler_EmailTakenReturns409 mirrors the username case
// to confirm the wire surface is identical — distinguishing the two
// would defeat the H5 collapse.
func TestRegisterHandler_EmailTakenReturns409(t *testing.T) {
	repo := &stubRepo{
		createFn: func(_ context.Context, _ *User) error { return ErrEmailTaken },
	}
	rr := postJSON(t, newTestRouterWithRepo(repo), "/api/register", RegisterRequest{
		Username: "alice",
		Email:    "alice@example.com",
		Password: "hunter2",
	})

	env := decodeEnvelope(t, rr, http.StatusConflict)
	if env.Message != "User already exists" {
		t.Errorf("envelope.error = %q, want %q", env.Message, "User already exists")
	}
}

// TestRegisterHandler_MissingFieldReturns400 exercises huma's request
// validation: the body omits "password" entirely, so the request is
// rejected before the service (and therefore bcrypt and the repo) runs.
func TestRegisterHandler_MissingFieldReturns400(t *testing.T) {
	repo := &stubRepo{
		createFn: func(_ context.Context, _ *User) error {
			t.Error("repo.CreateUser must not be reached when validation fails")
			return nil
		},
	}
	rr := postJSON(t, newTestRouterWithRepo(repo), "/api/register", map[string]any{
		"username": "alice",
		"email":    "alice@example.com",
	})

	env := decodeEnvelope(t, rr, http.StatusBadRequest)
	if env.Details == nil {
		t.Error("validation failure has no details; the per-field messages were dropped")
	}
}

// TestRegisterHandler_TooShortUsernameReturns400 pins the huma
// minLength:"3" tag on RegisterRequest.Username. The repo must not be
// invoked — a 2-character username that reaches the DB would be
// rejected by the column constraint instead, masking the edge case as
// a 500.
func TestRegisterHandler_TooShortUsernameReturns400(t *testing.T) {
	repo := &stubRepo{
		createFn: func(_ context.Context, _ *User) error {
			t.Error("repo.CreateUser must not be reached when username is too short")
			return nil
		},
	}
	rr := postJSON(t, newTestRouterWithRepo(repo), "/api/register", RegisterRequest{
		Username: "ab",
		Email:    "alice@example.com",
		Password: "hunter2",
	})

	decodeEnvelope(t, rr, http.StatusBadRequest)
}

// TestRegisterHandler_InvalidEmailReturns400 pins the huma format:"email"
// tag on RegisterRequest.Email. A "not-an-email" payload must be
// rejected before the service runs (otherwise the row insert would
// fail with a 23514 check_violation and surface as 500).
func TestRegisterHandler_InvalidEmailReturns400(t *testing.T) {
	repo := &stubRepo{
		createFn: func(_ context.Context, _ *User) error {
			t.Error("repo.CreateUser must not be reached when email is invalid")
			return nil
		},
	}
	rr := postJSON(t, newTestRouterWithRepo(repo), "/api/register", RegisterRequest{
		Username: "alice",
		Email:    "not-an-email",
		Password: "hunter2",
	})

	decodeEnvelope(t, rr, http.StatusBadRequest)
}

// TestRegisterHandler_TooShortPasswordReturns400 pins the huma
// minLength:"6" tag on RegisterRequest.Password. Bcrypt would happily
// hash a 1-character password — letting it through would silently
// produce a weak account.
func TestRegisterHandler_TooShortPasswordReturns400(t *testing.T) {
	repo := &stubRepo{
		createFn: func(_ context.Context, _ *User) error {
			t.Error("repo.CreateUser must not be reached when password is too short")
			return nil
		},
	}
	rr := postJSON(t, newTestRouterWithRepo(repo), "/api/register", RegisterRequest{
		Username: "alice",
		Email:    "alice@example.com",
		Password: "x",
	})

	decodeEnvelope(t, rr, http.StatusBadRequest)
}

// TestRegisterHandler_InternalErrorReturns500 covers the non-sentinel
// branch: an unexpected repo failure must surface as 500, not as a 409
// or a panic. MapError renders a static "Internal server error" wire
// message; the per-operation safe detail is hidden behind details.
func TestRegisterHandler_InternalErrorReturns500(t *testing.T) {
	repo := &stubRepo{
		createFn: func(_ context.Context, _ *User) error { return context.DeadlineExceeded },
	}
	rr := postJSON(t, newTestRouterWithRepo(repo), "/api/register", RegisterRequest{
		Username: "alice",
		Email:    "alice@example.com",
		Password: "hunter2",
	})

	env := decodeEnvelope(t, rr, http.StatusInternalServerError)
	if env.Message != "Internal server error" {
		t.Errorf("envelope.error = %q, want %q", env.Message, "Internal server error")
	}
	if env.Details != "Failed to register user" {
		t.Errorf("envelope.details = %v, want %q", env.Details, "Failed to register user")
	}
	if bytes.Contains(rr.Body.Bytes(), []byte("context deadline exceeded")) {
		t.Errorf("response leaks internal error text: %s", rr.Body.String())
	}
}

// ---- Login ----

// TestLoginHandler_OK is the happy path: 200 with the user profile
// in the body and the access + refresh cookies in the Set-Cookie
// headers. The body must not contain tokens.
func TestLoginHandler_OK(t *testing.T) {
	const plain = "hunter2"
	hash := testHash(t, plain)
	repo := &stubRepo{
		getByUsernameFn: func(_ context.Context, username string) (*User, error) {
			return &User{
				ID:           7,
				Username:     username,
				Email:        "alice@example.com",
				PasswordHash: hash,
			}, nil
		},
	}
	rr := postJSON(t, newTestRouterWithRepo(repo), "/api/login", LoginRequest{
		Username: "alice",
		Password: plain,
	})

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}

	var res AuthResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &res); err != nil {
		t.Fatalf("unmarshal AuthResponse: %v", err)
	}
	if res.User.ID != 7 || res.User.Username != "alice" {
		t.Errorf("user = %+v, want ID=7 username=alice", res.User)
	}
	if bytes.Contains(rr.Body.Bytes(), []byte("token")) {
		t.Errorf("response body leaks a token field: %s", rr.Body.String())
	}

	cookies := rr.Result().Cookies()
	var sawAccess, sawRefresh bool
	for _, c := range cookies {
		if c.Name == testAccessCookieName && c.Value != "" {
			sawAccess = true
		}
		if c.Name == testRefreshCookieName && c.Value != "" {
			sawRefresh = true
		}
	}
	if !sawAccess {
		t.Errorf("response missing %s Set-Cookie; got %v", testAccessCookieName, cookies)
	}
	if !sawRefresh {
		t.Errorf("response missing %s Set-Cookie; got %v", testRefreshCookieName, cookies)
	}
}

// TestLoginHandler_UnknownUserReturns401 — an unknown username maps to
// ErrInvalidCredentials at the service boundary (so user existence is
// not leaked) and to 401 at the handler boundary.
func TestLoginHandler_UnknownUserReturns401(t *testing.T) {
	repo := &stubRepo{
		getByUsernameFn: func(_ context.Context, _ string) (*User, error) { return nil, ErrUserNotFound },
	}
	rr := postJSON(t, newTestRouterWithRepo(repo), "/api/login", LoginRequest{
		Username: "ghost",
		Password: "anything",
	})

	env := decodeEnvelope(t, rr, http.StatusUnauthorized)
	if env.Message != "Invalid credentials" {
		t.Errorf("envelope.error = %q, want %q", env.Message, "Invalid credentials")
	}
}

// TestLoginHandler_WrongPasswordReturns401 must be indistinguishable on
// the wire from the unknown-user case above — same status, same message.
func TestLoginHandler_WrongPasswordReturns401(t *testing.T) {
	hash := testHash(t, "hunter2")
	repo := &stubRepo{
		getByUsernameFn: func(_ context.Context, username string) (*User, error) {
			return &User{ID: 1, Username: username, PasswordHash: hash}, nil
		},
	}
	rr := postJSON(t, newTestRouterWithRepo(repo), "/api/login", LoginRequest{
		Username: "alice",
		Password: "wrong-password",
	})

	env := decodeEnvelope(t, rr, http.StatusUnauthorized)
	if env.Message != "Invalid credentials" {
		t.Errorf("envelope.error = %q, want %q", env.Message, "Invalid credentials")
	}
}

// TestLoginHandler_MissingFieldReturns400 — the body omits "password",
// so huma rejects it before the repo lookup happens.
func TestLoginHandler_MissingFieldReturns400(t *testing.T) {
	repo := &stubRepo{
		getByUsernameFn: func(_ context.Context, _ string) (*User, error) {
			t.Error("repo.GetUserByUsername must not be reached when validation fails")
			return nil, nil
		},
	}
	rr := postJSON(t, newTestRouterWithRepo(repo), "/api/login", map[string]any{
		"username": "alice",
	})

	env := decodeEnvelope(t, rr, http.StatusBadRequest)
	if env.Details == nil {
		t.Error("validation failure has no details; the per-field messages were dropped")
	}
}

// TestLoginHandler_InternalErrorReturns500 — a genuine DB failure must
// not be masked as a 401, which would hide outages behind "bad
// password" in client-side telemetry. MapError renders a static
// "Internal server error" wire message; the per-operation safe detail
// is hidden behind details.
func TestLoginHandler_InternalErrorReturns500(t *testing.T) {
	repo := &stubRepo{
		getByUsernameFn: func(_ context.Context, _ string) (*User, error) {
			return nil, context.DeadlineExceeded
		},
	}
	rr := postJSON(t, newTestRouterWithRepo(repo), "/api/login", LoginRequest{
		Username: "alice",
		Password: "hunter2",
	})

	env := decodeEnvelope(t, rr, http.StatusInternalServerError)
	if env.Message != "Internal server error" {
		t.Errorf("envelope.error = %q, want %q", env.Message, "Internal server error")
	}
	if env.Details != "Failed to login" {
		t.Errorf("envelope.details = %v, want %q", env.Details, "Failed to login")
	}
	if bytes.Contains(rr.Body.Bytes(), []byte("context deadline exceeded")) {
		t.Errorf("response leaks internal error text: %s", rr.Body.String())
	}
}

// ---- Refresh ----

// TestRefreshHandler_OK covers the happy path of /api/refresh: a
// valid (non-expired, non-revoked) refresh token (sent via the
// __Host-nyx-refresh cookie) comes back as a fresh AuthResponse in
// the body + re-issued Set-Cookie headers. The body must not
// contain tokens, and the Set-Cookie values MUST differ from the
// supplied refresh token — that's what rotation means at the wire.
func TestRefreshHandler_OK(t *testing.T) {
	now := time.Now()

	suppliedRaw, _, err := newRefreshToken()
	if err != nil {
		t.Fatalf("newRefreshToken: %v", err)
	}
	suppliedHash := sha256Sum(suppliedRaw)

	repo := &stubRepo{
		getRefreshFn: func(_ context.Context, hash []byte) (*RefreshTokenRow, error) {
			if !bytes.Equal(hash, suppliedHash) {
				t.Errorf("lookup hash mismatch: got %x, want %x", hash, suppliedHash)
			}
			return &RefreshTokenRow{
				ID:        42,
				UserID:    7,
				FamilyID:  42,
				ExpiresAt: now.Add(time.Hour),
			}, nil
		},
		rotateFn: func(_ context.Context, _ int64, userID int, _ []byte, familyID int64, expiresAt time.Time) (*RefreshTokenRow, error) {
			return &RefreshTokenRow{
				ID:        100,
				UserID:    userID,
				FamilyID:  familyID,
				ExpiresAt: expiresAt,
			}, nil
		},
		getByIDFn: func(_ context.Context, id int) (*User, error) {
			return &User{ID: id, Username: "alice", Email: "a@x.com"}, nil
		},
	}

	rr := postWithRefreshCookie(t, newTestRouterWithRepo(repo), suppliedRaw)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var res AuthResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &res); err != nil {
		t.Fatalf("unmarshal AuthResponse: %v; body=%s", err, rr.Body.String())
	}
	if res.User.ID != 7 || res.User.Username != "alice" {
		t.Errorf("user = %+v, want ID=7 username=alice", res.User)
	}
	if res.ExpiresAt.IsZero() {
		t.Error("ExpiresAt zero on successful refresh")
	}
	// Tokens must not be in the body.
	if bytes.Contains(rr.Body.Bytes(), []byte("token")) || bytes.Contains(rr.Body.Bytes(), []byte("refresh_token")) {
		t.Errorf("response body leaks a token field: %s", rr.Body.String())
	}

	// Set-Cookie must include both refreshed auth cookies, and the
	// refresh cookie value MUST differ from the supplied one (rotation
	// minted a new raw via service.newRefreshToken, not the repo
	// stub).
	cookies := rr.Result().Cookies()
	var sawAccess, sawRefresh string
	for _, c := range cookies {
		switch c.Name {
		case testAccessCookieName:
			sawAccess = c.Value
		case testRefreshCookieName:
			sawRefresh = c.Value
		}
	}
	if sawAccess == "" {
		t.Errorf("response missing nyx-access Set-Cookie")
	}
	if sawRefresh == "" {
		t.Errorf("response missing nyx-refresh Set-Cookie")
	}
	if sawRefresh == suppliedRaw {
		t.Errorf("nyx-refresh unchanged after rotation; rotation didn't mint a new one")
	}
}

// TestRefreshHandler_ReuseReturns401 pins the reuse path: a refresh
// token that was already rotated surfaces as 401 with a static
// "Refresh token revoked" message. The wire must NOT echo the
// underlying service error. The refresh token arrives via cookie.
func TestRefreshHandler_ReuseReturns401(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	repo := &stubRepo{
		getRefreshFn: func(_ context.Context, _ []byte) (*RefreshTokenRow, error) {
			return &RefreshTokenRow{
				ID:        42,
				UserID:    7,
				FamilyID:  42,
				ExpiresAt: past,
				RevokedAt: &past,
			}, nil
		},
	}
	raw, _, _ := newRefreshToken()
	rr := postWithRefreshCookie(t, newTestRouterWithRepo(repo), raw)

	env := decodeEnvelope(t, rr, http.StatusUnauthorized)
	if env.Message != "Refresh token revoked" {
		t.Errorf("envelope.error = %q, want %q", env.Message, "Refresh token revoked")
	}
}

// TestRefreshHandler_ExpiredReturns401 covers the timeout path: a
// non-revoked refresh token whose expires_at is in the past. The
// service layer distinguishes this from reuse (no family revoke),
// but the handler still maps it to 401 with a distinct static
// message.
func TestRefreshHandler_ExpiredReturns401(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	repo := &stubRepo{
		getRefreshFn: func(_ context.Context, _ []byte) (*RefreshTokenRow, error) {
			return &RefreshTokenRow{
				ID:        42,
				UserID:    7,
				FamilyID:  42,
				ExpiresAt: past,
				// RevokedAt intentionally nil — purely expired.
			}, nil
		},
	}
	raw, _, _ := newRefreshToken()
	rr := postWithRefreshCookie(t, newTestRouterWithRepo(repo), raw)

	env := decodeEnvelope(t, rr, http.StatusUnauthorized)
	if env.Message != "Refresh token expired" {
		t.Errorf("envelope.error = %q, want %q", env.Message, "Refresh token expired")
	}
}

// TestRefreshHandler_MissingCookieReturns401 — refresh token cookie
// is absent. The handler maps this to 401 with the static
// "Invalid refresh token" message (no lookup happened because the
// cookie reader returned false).
func TestRefreshHandler_MissingCookieReturns401(t *testing.T) {
	repo := &stubRepo{
		getRefreshFn: func(_ context.Context, _ []byte) (*RefreshTokenRow, error) {
			t.Error("repo.GetRefreshTokenByHash must not be reached when refresh cookie is missing")
			return nil, nil
		},
	}
	rr := postJSON(t, newTestRouterWithRepo(repo), "/api/refresh", map[string]any{})

	env := decodeEnvelope(t, rr, http.StatusUnauthorized)
	if env.Message != "Invalid refresh token" {
		t.Errorf("envelope.error = %q, want %q", env.Message, "Invalid refresh token")
	}
}

// TestRefreshHandler_UnknownTokenReturns401 covers the
// ErrInvalidRefreshToken wire path: a refresh token whose hash is not
// in the DB. Distinct from TestRefresh_InvalidHashReturnsErrInvalid at
// the service layer — this pins the handler-level mapping to 401 with
// the static "Invalid refresh token" message (no err.Error() echo).
func TestRefreshHandler_UnknownTokenReturns401(t *testing.T) {
	repo := &stubRepo{
		getRefreshFn: func(_ context.Context, _ []byte) (*RefreshTokenRow, error) {
			return nil, ErrRefreshTokenNotFound
		},
	}
	raw, _, _ := newRefreshToken()
	rr := postWithRefreshCookie(t, newTestRouterWithRepo(repo), raw)

	env := decodeEnvelope(t, rr, http.StatusUnauthorized)
	if env.Message != "Invalid refresh token" {
		t.Errorf("envelope.error = %q, want %q", env.Message, "Invalid refresh token")
	}
}

// ---- Logout ----

// TestLogoutHandler_NoContent covers the auth-required logout happy
// path. The caller must present a valid access token (cookie or
// Authorization header — the test uses the cookie path so the
// migration is exercised end-to-end); the __Host-nyx-refresh cookie
// carries the row we revoke. Per M1, Logout revokes only the
// supplied row, not the surrounding family. Returns 204 with empty
// body AND Set-Cookie headers that clear both auth cookies.
func TestLogoutHandler_NoContent(t *testing.T) {
	tokens, err := auth.NewTokenService([]byte(auth.TestSecret), 15*time.Minute)
	if err != nil {
		t.Fatalf("auth.NewTokenService: %v", err)
	}
	accessToken, err := tokens.GenerateToken(7, "alice")
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}

	now := time.Now()
	var revokedID int64
	repo := &stubRepo{
		getRefreshFn: func(_ context.Context, _ []byte) (*RefreshTokenRow, error) {
			return &RefreshTokenRow{ID: 1, UserID: 7, FamilyID: 99, ExpiresAt: now.Add(time.Hour)}, nil
		},
		revokeByIDFn: func(_ context.Context, id int64) error {
			revokedID = id
			return nil
		},
		revokeFamilyFn: func(_ context.Context, _ int64) (int64, error) {
			t.Error("RevokeRefreshTokenFamily must not be reached on per-session logout")
			return 0, nil
		},
	}

	raw, _, _ := newRefreshToken()
	req := authedLogoutRequest(t, "/api/logout", accessToken, raw)
	rr := httptest.NewRecorder()
	newTestRouterWithRepo(repo).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rr.Code, rr.Body.String())
	}
	if rr.Body.Len() != 0 {
		t.Errorf("204 body should be empty, got %q", rr.Body.String())
	}
	if revokedID != 1 {
		t.Errorf("expected row 1 to be revoked, got %d", revokedID)
	}

	// Both auth cookies must be cleared (MaxAge <= 0 + empty value).
	cookies := rr.Result().Cookies()
	for _, name := range []string{testAccessCookieName, testRefreshCookieName} {
		var found bool
		for _, c := range cookies {
			if c.Name == name {
				found = true
				if c.Value != "" {
					t.Errorf("clear cookie %q must have empty value, got %q", name, c.Value)
				}
				if c.MaxAge >= 0 {
					t.Errorf("clear cookie %q must have MaxAge < 0, got %d", name, c.MaxAge)
				}
			}
		}
		if !found {
			t.Errorf("response missing %q Set-Cookie clear; got %v", name, cookies)
		}
	}
}

// TestLogoutHandler_RequiresAuth confirms /api/logout is gated by the
// JWT middleware: a request with no access cookie and no
// Authorization header returns 401 without ever reaching the
// handler.
func TestLogoutHandler_RequiresAuth(t *testing.T) {
	repo := &stubRepo{
		getRefreshFn: func(_ context.Context, _ []byte) (*RefreshTokenRow, error) {
			t.Error("repo.GetRefreshTokenByHash must not be reached when auth fails")
			return nil, nil
		},
	}
	raw, _, _ := newRefreshToken()
	// Empty access token → middleware rejects.
	req := authedLogoutRequest(t, "/api/logout", "", raw)
	rr := httptest.NewRecorder()
	newTestRouterWithRepo(repo).ServeHTTP(rr, req)

	env := decodeEnvelope(t, rr, http.StatusUnauthorized)
	if env.Message == "" {
		t.Errorf("expected an error message on auth failure")
	}
}

// TestLogoutHandler_NoRefreshCookieClearsAnyway verifies the
// defensive branch: if the caller has a valid access cookie but no
// refresh cookie (e.g. the refresh cookie was stripped by an
// attacker, or the browser cleared it on its own), /api/logout
// still returns 204 + clears both auth cookies at the browser.
// Local clear is idempotent; family revocation is a no-op because
// the service's Logout returns nil on empty input.
func TestLogoutHandler_NoRefreshCookieClearsAnyway(t *testing.T) {
	tokens, err := auth.NewTokenService([]byte(auth.TestSecret), 15*time.Minute)
	if err != nil {
		t.Fatalf("auth.NewTokenService: %v", err)
	}
	accessToken, err := tokens.GenerateToken(7, "alice")
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	repo := &stubRepo{
		getRefreshFn: func(_ context.Context, _ []byte) (*RefreshTokenRow, error) {
			t.Error("repo.GetRefreshTokenByHash must not be reached when refresh cookie is absent")
			return nil, nil
		},
		revokeFamilyFn: func(_ context.Context, _ int64) (int64, error) {
			t.Error("RevokeRefreshTokenFamily must not be reached when refresh cookie is absent")
			return 0, nil
		},
	}

	req := authedLogoutRequest(t, "/api/logout", accessToken, "")
	rr := httptest.NewRecorder()
	newTestRouterWithRepo(repo).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rr.Code, rr.Body.String())
	}
	cookies := rr.Result().Cookies()
	if len(cookies) == 0 {
		t.Error("expected Set-Cookie clears even when refresh cookie is absent")
	}
}

// ---- /api/me (SECURITY.md L7) ----

// authedGetRequest builds a GET request carrying the supplied access
// cookie. When accessToken is empty the caller is testing the
// auth-rejection path; pass the token to exercise the success path.
// Mirrors authedLogoutRequest but uses GET + no body, since /api/me
// is a read-only profile lookup with no Set-Cookie contract.
func authedGetRequest(t *testing.T, path, accessToken string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if accessToken != "" {
		req.AddCookie(&http.Cookie{Name: testAccessCookieName, Value: accessToken})
	}
	return req
}

// TestMeHandler_ReturnsProfile exercises the auth-required happy
// path. The access token's subject id reaches the service, the
// repository returns the user, and the JSON envelope carries the
// profile unchanged. No cookies are set or cleared on this route.
func TestMeHandler_ReturnsProfile(t *testing.T) {
	tokens, err := auth.NewTokenService([]byte(auth.TestSecret), 15*time.Minute)
	if err != nil {
		t.Fatalf("auth.NewTokenService: %v", err)
	}
	accessToken, err := tokens.GenerateToken(7, "alice")
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}

	var seenID int
	repo := &stubRepo{
		getByIDFn: func(_ context.Context, id int) (*User, error) {
			seenID = id
			return &User{
				ID:        7,
				Username:  "alice",
				Email:     "alice@example.com",
				CreatedAt: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
				UpdatedAt: time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC),
			}, nil
		},
	}

	req := authedGetRequest(t, "/api/me", accessToken)
	rr := httptest.NewRecorder()
	newTestRouterWithRepo(repo).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if seenID != 7 {
		t.Errorf("repo received id=%d, want 7", seenID)
	}

	var body User
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, rr.Body.String())
	}
	if body.ID != 7 || body.Username != "alice" || body.Email != "alice@example.com" {
		t.Errorf("body = %+v, want id=7 username=alice", body)
	}
	// The wire shape must NOT carry the password hash, even on
	// the dedicated profile endpoint — the json:"-" tag does the
	// work, but pin the invariant so a future refactor doesn't
	// drop the tag.
	if body.PasswordHash != "" {
		t.Errorf("PasswordHash must be omitted from /api/me wire body")
	}

	// No Set-Cookie headers — /api/me is a read-only profile lookup.
	if cookies := rr.Result().Cookies(); len(cookies) != 0 {
		t.Errorf("/api/me must not set cookies; got %d", len(cookies))
	}
}

// TestMeHandler_RequiresAuth confirms /api/me is gated by the JWT
// middleware. A request with no access cookie reaches no handler —
// the repo must not be touched.
func TestMeHandler_RequiresAuth(t *testing.T) {
	repo := &stubRepo{
		getByIDFn: func(_ context.Context, _ int) (*User, error) {
			t.Error("repo.GetUserByID must not be reached when auth fails")
			return nil, nil
		},
	}

	req := authedGetRequest(t, "/api/me", "")
	rr := httptest.NewRecorder()
	newTestRouterWithRepo(repo).ServeHTTP(rr, req)

	decodeEnvelope(t, rr, http.StatusUnauthorized)
}

// TestMeHandler_NotFoundReturns404 — if the user row was deleted
// between login and now (rare but possible), /api/me must surface
// a clean 404 via api.MapError rather than crashing or returning
// a partially-populated body.
func TestMeHandler_NotFoundReturns404(t *testing.T) {
	tokens, err := auth.NewTokenService([]byte(auth.TestSecret), 15*time.Minute)
	if err != nil {
		t.Fatalf("auth.NewTokenService: %v", err)
	}
	accessToken, err := tokens.GenerateToken(42, "ghost")
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}

	repo := &stubRepo{
		getByIDFn: func(_ context.Context, _ int) (*User, error) {
			return nil, ErrUserNotFound
		},
	}

	req := authedGetRequest(t, "/api/me", accessToken)
	rr := httptest.NewRecorder()
	newTestRouterWithRepo(repo).ServeHTTP(rr, req)

	decodeEnvelope(t, rr, http.StatusNotFound)
}
