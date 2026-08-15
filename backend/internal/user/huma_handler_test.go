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
	"golang.org/x/crypto/bcrypt"
)

// TestMain lives in repository_integration_test.go — it both installs
// the huma error override (needed by handler tests below) and boots
// the integration-test Postgres container (skipped under
// SKIP_CONTAINERS=true). Go only allows one TestMain per package.

// setupTestRouter builds a chi + huma router carrying the same user
// operations as the real API. Prometheus, RequestID, Logging, CORS, and
// RateLimit are deliberately skipped — they have their own tests and
// only add noise (and a goroutine, in RateLimit's case) here. tokens
// is threaded through so the /api/logout route can attach the JWT
// middleware in tests the same way it does in production.
func setupTestRouter(h *Handler, tokens auth.TokenService) *chi.Mux {
	router := chi.NewMux()
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
	return setupTestRouter(NewHandler(NewService(repo, tokens, 15*time.Minute, 7*24*time.Hour)), tokens)
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

// TestRegisterHandler_Created is the happy path: 201 with a token and
// the persisted user. The password hash must never appear on the wire
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
	if res.Token == "" {
		t.Error("token is empty on a successful registration")
	}
	if res.User.ID != 42 || res.User.Username != "alice" {
		t.Errorf("user = %+v, want ID=42 username=alice", res.User)
	}
	if bytes.Contains(rr.Body.Bytes(), []byte("password")) {
		t.Errorf("response body leaks a password field: %s", rr.Body.String())
	}
}

// TestRegisterHandler_DuplicateUsernameReturns409 pins the wiring of the
// ErrUserAlreadyExists branch. The service test covers the sentinel
// bubbling up; this covers the handler translating it to HTTP 409.
func TestRegisterHandler_DuplicateUsernameReturns409(t *testing.T) {
	repo := &stubRepo{
		createFn: func(_ context.Context, _ *User) error { return ErrUserAlreadyExists },
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
// or a panic.
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
	if env.Message != "Failed to register user" {
		t.Errorf("envelope.error = %q, want %q", env.Message, "Failed to register user")
	}
	if env.Details != "Failed to register user" {
		t.Errorf("envelope.details = %v, want %q", env.Details, "Failed to register user")
	}
	if bytes.Contains(rr.Body.Bytes(), []byte("context deadline exceeded")) {
		t.Errorf("response leaks internal error text: %s", rr.Body.String())
	}
}

// ---- Login ----

// TestLoginHandler_OK is the happy path: 200 with a JWT for a user
// whose stored bcrypt hash matches the submitted password.
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
	if res.Token == "" {
		t.Error("token is empty on a successful login")
	}
	if res.User.ID != 7 || res.User.Username != "alice" {
		t.Errorf("user = %+v, want ID=7 username=alice", res.User)
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
// password" in client-side telemetry.
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
	if env.Message != "Failed to login" {
		t.Errorf("envelope.error = %q, want %q", env.Message, "Failed to login")
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
// valid (non-expired, non-revoked) refresh token comes back as a
// fresh AuthResponse with a new access token and a new refresh token.
// The new refresh_token MUST differ from the supplied one — that's
// what rotation means at the wire.
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

	rr := postJSON(t, newTestRouterWithRepo(repo), "/api/refresh", RefreshRequest{
		RefreshToken: suppliedRaw,
	})

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var res AuthResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &res); err != nil {
		t.Fatalf("unmarshal AuthResponse: %v; body=%s", err, rr.Body.String())
	}
	if res.Token == "" {
		t.Error("Token empty on successful refresh")
	}
	if res.RefreshToken == "" {
		t.Error("RefreshToken empty on successful refresh")
	}
	if res.RefreshToken == suppliedRaw {
		t.Error("RefreshToken unchanged after rotation; rotation didn't mint a new one")
	}
	if res.ExpiresAt.IsZero() {
		t.Error("ExpiresAt zero on successful refresh")
	}
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
	rr := postJSON(t, newTestRouterWithRepo(repo), "/api/refresh", RefreshRequest{RefreshToken: raw})

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
	rr := postJSON(t, newTestRouterWithRepo(repo), "/api/refresh", RefreshRequest{RefreshToken: raw})

	env := decodeEnvelope(t, rr, http.StatusUnauthorized)
	if env.Message != "Refresh token expired" {
		t.Errorf("envelope.error = %q, want %q", env.Message, "Refresh token expired")
	}
}

// TestRefreshHandler_MissingFieldReturns400 — body omits refresh_token
// entirely. huma's required:"true" tag rejects it before the service
// runs, so no RefreshTokenNotFound lookup occurs.
func TestRefreshHandler_MissingFieldReturns400(t *testing.T) {
	repo := &stubRepo{
		getRefreshFn: func(_ context.Context, _ []byte) (*RefreshTokenRow, error) {
			t.Error("repo.GetRefreshTokenByHash must not be reached when validation fails")
			return nil, nil
		},
	}
	rr := postJSON(t, newTestRouterWithRepo(repo), "/api/refresh", map[string]any{})

	decodeEnvelope(t, rr, http.StatusBadRequest)
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
	rr := postJSON(t, newTestRouterWithRepo(repo), "/api/refresh", RefreshRequest{RefreshToken: raw})

	env := decodeEnvelope(t, rr, http.StatusUnauthorized)
	if env.Message != "Invalid refresh token" {
		t.Errorf("envelope.error = %q, want %q", env.Message, "Invalid refresh token")
	}
}

// ---- Logout ----

// TestLogoutHandler_NoContent covers the auth-required logout happy
// path. The caller must present a valid access token (validated by
// the per-operation middleware); the body's refresh_token is what
// gets revoked. Returns 204 with empty body.
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
	var revokedFamily int64
	repo := &stubRepo{
		getRefreshFn: func(_ context.Context, _ []byte) (*RefreshTokenRow, error) {
			return &RefreshTokenRow{ID: 1, UserID: 7, FamilyID: 99, ExpiresAt: now.Add(time.Hour)}, nil
		},
		revokeFamilyFn: func(_ context.Context, familyID int64) (int64, error) {
			revokedFamily = familyID
			return 1, nil
		},
	}

	raw, _, _ := newRefreshToken()
	body, _ := json.Marshal(LogoutRequest{RefreshToken: raw})
	req := httptest.NewRequest(http.MethodPost, "/api/logout", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	rr := httptest.NewRecorder()
	newTestRouterWithRepo(repo).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rr.Code, rr.Body.String())
	}
	if rr.Body.Len() != 0 {
		t.Errorf("204 body should be empty, got %q", rr.Body.String())
	}
	if revokedFamily != 99 {
		t.Errorf("expected family 99 to be revoked, got %d", revokedFamily)
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
	rr := postJSON(t, newTestRouterWithRepo(repo), "/api/logout", LogoutRequest{RefreshToken: raw})

	env := decodeEnvelope(t, rr, http.StatusUnauthorized)
	if env.Message != "Authorization header is required" {
		t.Errorf("envelope.error = %q, want %q", env.Message, "Authorization header is required")
	}
}

// TestLogoutHandler_MissingFieldReturns400 — body omits refresh_token.
// huma's required:"true" tag rejects before the auth-gated handler
// runs (validation order: huma body parse, then middleware, then
// handler).
func TestLogoutHandler_MissingFieldReturns400(t *testing.T) {
	repo := &stubRepo{
		getRefreshFn: func(_ context.Context, _ []byte) (*RefreshTokenRow, error) {
			t.Error("repo.GetRefreshTokenByHash must not be reached when validation fails")
			return nil, nil
		},
		revokeFamilyFn: func(_ context.Context, _ int64) (int64, error) {
			t.Error("RevokeRefreshTokenFamily must not be reached when validation fails")
			return 0, nil
		},
	}
	tokens, err := auth.NewTokenService([]byte(auth.TestSecret), 15*time.Minute)
	if err != nil {
		t.Fatalf("auth.NewTokenService: %v", err)
	}
	accessToken, err := tokens.GenerateToken(7, "alice")
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}

	body, _ := json.Marshal(map[string]any{})
	req := httptest.NewRequest(http.MethodPost, "/api/logout", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	rr := httptest.NewRecorder()
	newTestRouterWithRepo(repo).ServeHTTP(rr, req)

	decodeEnvelope(t, rr, http.StatusBadRequest)
}
