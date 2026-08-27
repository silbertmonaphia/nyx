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

// setupTestRouter builds a chi + huma router carrying the same user
// operations as the real API. Prometheus, RequestID, Logging, CORS,
// RateLimit, and the body-cap middleware are deliberately skipped —
// they have their own tests and only add noise (and a goroutine, in
// RateLimit's case) here. StoreRequest IS wired in because the auth
// middleware reads the Authorization header off the live request via
// reqctx.RequestFromContext. tokens is threaded through so the
// /api/logout route can attach the JWT middleware the same way it
// does in production.
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
	RegisterUserOpsTest(hapi, h, tokens)
	return router
}

// newTestRouterWithRepo is the common three-line arrangement of every
// test below: stub repo → real service → handler → router.
func newTestRouterWithRepo(repo Repository) *chi.Mux {
	tokens, err := auth.NewTokenService([]byte(auth.TestSecret), 15*time.Minute)
	if err != nil {
		panic(err) // test setup; never expected to fail
	}
	return setupTestRouter(NewHandler(NewService(repo, tokens, 15*time.Minute, 7*24*time.Hour, noop.NewTracerProvider().Tracer("test"))), tokens)
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

// postRefreshBody POSTs /api/refresh with the supplied refresh_token
// in the request body (the Bearer contract — was previously the
// __Host-nyx-refresh cookie).
func postRefreshBody(t *testing.T, router *chi.Mux, refreshRaw string) *httptest.ResponseRecorder {
	t.Helper()
	return postJSON(t, router, "/api/refresh", RefreshRequest{RefreshToken: refreshRaw})
}

// authedPostRequest builds a POST request carrying the
// Authorization: Bearer header (so the JWT middleware accepts it)
// plus the supplied JSON body. When accessToken is empty the caller
// is testing the auth-rejection path; pass the token to exercise
// the success path.
func authedPostRequest(t *testing.T, path, accessToken string, body any) *http.Request {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	if accessToken != "" {
		req.Header.Set("Authorization", "Bearer "+accessToken)
	}
	return req
}

// authedGetRequest builds a GET request carrying the
// Authorization: Bearer header. Mirrors authedPostRequest but for
// read-only endpoints (/api/me).
func authedGetRequest(t *testing.T, path, accessToken string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if accessToken != "" {
		req.Header.Set("Authorization", "Bearer "+accessToken)
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

// TestRegisterHandler_Created is the happy path: 201 with the access
// + refresh tokens, token type, expiry, and user profile all in the
// JSON body. The password hash must never appear on the wire
// (User.PasswordHash is json:"-").
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
	if res.AccessToken == "" {
		t.Errorf("response missing access_token; body=%s", rr.Body.String())
	}
	if res.RefreshToken == "" {
		t.Errorf("response missing refresh_token; body=%s", rr.Body.String())
	}
	if res.TokenType != "Bearer" {
		t.Errorf("token_type = %q, want %q", res.TokenType, "Bearer")
	}
	if res.ExpiresAt.IsZero() {
		t.Errorf("expires_at zero on successful register; body=%s", rr.Body.String())
	}
	if bytes.Contains(rr.Body.Bytes(), []byte("password")) {
		t.Errorf("response body leaks a password field: %s", rr.Body.String())
	}

	// /api/register must NOT set any cookies — the Bearer contract
	// only uses the response body.
	if cookies := rr.Result().Cookies(); len(cookies) != 0 {
		t.Errorf("response must not set cookies; got %d", len(cookies))
	}
}

// TestRegisterHandler_BodyCarriesTokens pins the wire-shape contract:
// the access_token, refresh_token, token_type, expires_at and user
// fields all live in the body. Clients (SPA / iOS / Android / game
// SDK) read them and store in their platform-appropriate secure
// storage.
func TestRegisterHandler_BodyCarriesTokens(t *testing.T) {
	repo := &stubRepo{
		createFn: func(_ context.Context, u *User) error {
			u.ID = 99
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

	var res AuthResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &res); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if res.AccessToken == "" || res.RefreshToken == "" {
		t.Fatalf("missing tokens in body: %+v", res)
	}
	if res.TokenType != "Bearer" {
		t.Errorf("token_type = %q, want Bearer", res.TokenType)
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

// TestLoginHandler_OK is the happy path: 200 with access + refresh
// tokens, token type, expiry, and user profile all in the body.
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
	if res.AccessToken == "" || res.RefreshToken == "" {
		t.Errorf("login response missing tokens: %+v", res)
	}
	if res.TokenType != "Bearer" {
		t.Errorf("token_type = %q, want Bearer", res.TokenType)
	}
	if cookies := rr.Result().Cookies(); len(cookies) != 0 {
		t.Errorf("login must not set cookies; got %d", len(cookies))
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
// valid (non-expired, non-revoked) refresh token (in the request body)
// comes back as a fresh AuthResponse — new access_token + new
// refresh_token (rotation). The new refresh token MUST differ from
// the supplied one — that's what rotation means at the wire.
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

	rr := postRefreshBody(t, newTestRouterWithRepo(repo), suppliedRaw)

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
	if res.AccessToken == "" || res.RefreshToken == "" {
		t.Errorf("refresh response missing tokens: %+v", res)
	}
	if res.TokenType != "Bearer" {
		t.Errorf("token_type = %q, want Bearer", res.TokenType)
	}
	if res.ExpiresAt.IsZero() {
		t.Error("ExpiresAt zero on successful refresh")
	}
	if res.RefreshToken == suppliedRaw {
		t.Errorf("refresh_token unchanged after rotation; rotation didn't mint a new one")
	}
	if cookies := rr.Result().Cookies(); len(cookies) != 0 {
		t.Errorf("refresh must not set cookies; got %d", len(cookies))
	}
}

// TestRefreshHandler_ReadsTokenFromBody pins the Bearer contract: the
// handler reads the refresh token from the request body (not a
// cookie). A successful refresh returns fresh tokens in the body.
func TestRefreshHandler_ReadsTokenFromBody(t *testing.T) {
	now := time.Now()
	raw, _, err := newRefreshToken()
	if err != nil {
		t.Fatalf("newRefreshToken: %v", err)
	}
	repo := &stubRepo{
		getRefreshFn: func(_ context.Context, _ []byte) (*RefreshTokenRow, error) {
			return &RefreshTokenRow{ID: 1, UserID: 7, FamilyID: 1, ExpiresAt: now.Add(time.Hour)}, nil
		},
		rotateFn: func(_ context.Context, _ int64, userID int, _ []byte, familyID int64, expiresAt time.Time) (*RefreshTokenRow, error) {
			return &RefreshTokenRow{ID: 2, UserID: userID, FamilyID: familyID, ExpiresAt: expiresAt}, nil
		},
		getByIDFn: func(_ context.Context, id int) (*User, error) {
			return &User{ID: id, Username: "alice"}, nil
		},
	}

	rr := postRefreshBody(t, newTestRouterWithRepo(repo), raw)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
}

// TestRefreshHandler_EmptyBodyReturns400 — huma's required:"true" tag
// on RefreshRequest.RefreshToken rejects an empty body before the
// handler runs. The repo must not be touched.
func TestRefreshHandler_EmptyBodyReturns400(t *testing.T) {
	repo := &stubRepo{
		getRefreshFn: func(_ context.Context, _ []byte) (*RefreshTokenRow, error) {
			t.Error("repo.GetRefreshTokenByHash must not be reached when body is empty")
			return nil, nil
		},
	}
	rr := postJSON(t, newTestRouterWithRepo(repo), "/api/refresh", map[string]any{})
	decodeEnvelope(t, rr, http.StatusBadRequest)
}

// TestRefreshHandler_ReuseReturns401 pins the reuse path: a refresh
// token that was already rotated surfaces as 401 with a static
// "Refresh token revoked" message. The wire must NOT echo the
// underlying service error.
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
	rr := postRefreshBody(t, newTestRouterWithRepo(repo), raw)

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
	rr := postRefreshBody(t, newTestRouterWithRepo(repo), raw)

	env := decodeEnvelope(t, rr, http.StatusUnauthorized)
	if env.Message != "Refresh token expired" {
		t.Errorf("envelope.error = %q, want %q", env.Message, "Refresh token expired")
	}
}

// TestRefreshHandler_UnknownTokenReturns401 covers the
// ErrRefreshTokenNotFound wire path: a refresh token whose hash is not
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
	rr := postRefreshBody(t, newTestRouterWithRepo(repo), raw)

	env := decodeEnvelope(t, rr, http.StatusUnauthorized)
	if env.Message != "Invalid refresh token" {
		t.Errorf("envelope.error = %q, want %q", env.Message, "Invalid refresh token")
	}
}

// ---- Logout ----

// TestLogoutHandler_NoContent covers the auth-required logout happy
// path. The caller must present a valid access token in the
// Authorization: Bearer header; the refresh_token in the body
// identifies the row we revoke. Per M1, Logout revokes only the
// supplied row, not the surrounding family. Returns 204 with empty
// body and no Set-Cookie headers (Bearer transport carries no cookies).
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
	req := authedPostRequest(t, "/api/logout", accessToken, LogoutRequest{RefreshToken: raw})
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
	if cookies := rr.Result().Cookies(); len(cookies) != 0 {
		t.Errorf("logout must not set cookies; got %d", len(cookies))
	}
}

// TestLogoutHandler_RequiresAuth confirms /api/logout is gated by the
// JWT middleware: a request with no Authorization header returns 401
// without ever reaching the handler.
func TestLogoutHandler_RequiresAuth(t *testing.T) {
	repo := &stubRepo{
		getRefreshFn: func(_ context.Context, _ []byte) (*RefreshTokenRow, error) {
			t.Error("repo.GetRefreshTokenByHash must not be reached when auth fails")
			return nil, nil
		},
	}
	raw, _, _ := newRefreshToken()
	// Empty access token → middleware rejects.
	req := authedPostRequest(t, "/api/logout", "", LogoutRequest{RefreshToken: raw})
	rr := httptest.NewRecorder()
	newTestRouterWithRepo(repo).ServeHTTP(rr, req)

	env := decodeEnvelope(t, rr, http.StatusUnauthorized)
	if env.Message == "" {
		t.Errorf("expected an error message on auth failure")
	}
}

// TestLogoutHandler_ReadsTokenFromBody pins the Bearer contract on
// the logout path: the handler reads the refresh token from the
// body (not a cookie) and revokes exactly that row.
func TestLogoutHandler_ReadsTokenFromBody(t *testing.T) {
	tokens, err := auth.NewTokenService([]byte(auth.TestSecret), 15*time.Minute)
	if err != nil {
		t.Fatalf("auth.NewTokenService: %v", err)
	}
	accessToken, err := tokens.GenerateToken(7, "alice")
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}

	var revokedID int64
	now := time.Now()
	repo := &stubRepo{
		getRefreshFn: func(_ context.Context, _ []byte) (*RefreshTokenRow, error) {
			return &RefreshTokenRow{ID: 7, UserID: 7, FamilyID: 1, ExpiresAt: now.Add(time.Hour)}, nil
		},
		revokeByIDFn: func(_ context.Context, id int64) error {
			revokedID = id
			return nil
		},
	}

	raw, _, _ := newRefreshToken()
	req := authedPostRequest(t, "/api/logout", accessToken, LogoutRequest{RefreshToken: raw})
	rr := httptest.NewRecorder()
	newTestRouterWithRepo(repo).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rr.Code, rr.Body.String())
	}
	if revokedID != 7 {
		t.Errorf("expected row 7 to be revoked, got %d", revokedID)
	}
}

// ---- /api/me (SECURITY.md L7) ----

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
// middleware. A request with no Authorization header reaches no
// handler — the repo must not be touched.
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