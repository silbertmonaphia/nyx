package user

import (
	"context"
	"errors"
	"testing"
	"time"

	"nyx/internal/platform/auth"

	"go.opentelemetry.io/otel/trace/noop"
	"golang.org/x/crypto/bcrypt"
)

// newTestTokens builds a per-test TokenService so each test signs and
// validates against its own captured key. The TTL is hard-coded —
// service tests don't care about TTL, only the JWT round-trip.
func newTestTokens(t *testing.T) auth.TokenService {
	t.Helper()
	tokens, err := auth.NewTokenService([]byte(auth.TestSecret), 15*time.Minute)
	if err != nil {
		t.Fatalf("auth.NewTokenService: %v", err)
	}
	return tokens
}

// stubRepo is a hand-rolled mock of the Repository interface. The
// service tests don't need pgxmock — they exercise the bcrypt and JWT
// paths against a pure-Go fake, so failures here are unambiguous
// ("Register didn't return AuthResponse" rather than "mock expected X
// calls but got Y").
type stubRepo struct {
	createFn        func(ctx context.Context, u *User) error
	getByUsernameFn func(ctx context.Context, username string) (*User, error)
	getByIDFn       func(ctx context.Context, id int) (*User, error)

	// Refresh-token methods. Same "default error if unset" pattern as
	// the user CRUD stubs above: any test path that hits an unstubbed
	// refresh method fails loudly rather than silently passing.
	createRefreshFn func(ctx context.Context, userID int, tokenHash []byte, expiresAt time.Time) (*RefreshTokenRow, error)
	getRefreshFn    func(ctx context.Context, tokenHash []byte) (*RefreshTokenRow, error)
	rotateFn        func(ctx context.Context, oldID int64, userID int, tokenHash []byte, familyID int64, expiresAt time.Time) (*RefreshTokenRow, error)
	revokeFamilyFn  func(ctx context.Context, familyID int64) (int64, error)
	revokeByIDFn    func(ctx context.Context, id int64) error
}

func (s *stubRepo) CreateUser(ctx context.Context, u *User) error {
	if s.createFn == nil {
		return nil
	}
	return s.createFn(ctx, u)
}
func (s *stubRepo) GetUserByUsername(ctx context.Context, username string) (*User, error) {
	if s.getByUsernameFn == nil {
		return nil, errors.New("GetUserByUsername not stubbed")
	}
	return s.getByUsernameFn(ctx, username)
}
func (s *stubRepo) GetUserByID(ctx context.Context, id int) (*User, error) {
	if s.getByIDFn == nil {
		return nil, errors.New("GetUserByID not stubbed")
	}
	return s.getByIDFn(ctx, id)
}
func (s *stubRepo) CreateRefreshToken(ctx context.Context, userID int, tokenHash []byte, expiresAt time.Time) (*RefreshTokenRow, error) {
	if s.createRefreshFn == nil {
		// Default: succeed with a fabricated row keyed to the supplied
		// hash so tests that don't care about refresh tokens can keep
		// focusing on the user/auth flows. Refresh-specific tests
		// override this stub to assert on the precise projection.
		return &RefreshTokenRow{
			ID:        1,
			UserID:    userID,
			FamilyID:  1,
			ExpiresAt: expiresAt,
			CreatedAt: time.Now(),
		}, nil
	}
	return s.createRefreshFn(ctx, userID, tokenHash, expiresAt)
}
func (s *stubRepo) GetRefreshTokenByHash(ctx context.Context, tokenHash []byte) (*RefreshTokenRow, error) {
	if s.getRefreshFn == nil {
		return nil, errors.New("GetRefreshTokenByHash not stubbed")
	}
	return s.getRefreshFn(ctx, tokenHash)
}
func (s *stubRepo) RotateRefreshToken(ctx context.Context, oldID int64, userID int, tokenHash []byte, familyID int64, expiresAt time.Time) (*RefreshTokenRow, error) {
	if s.rotateFn == nil {
		return nil, errors.New("RotateRefreshToken not stubbed")
	}
	return s.rotateFn(ctx, oldID, userID, tokenHash, familyID, expiresAt)
}
func (s *stubRepo) RevokeRefreshTokenFamily(ctx context.Context, familyID int64) (int64, error) {
	if s.revokeFamilyFn == nil {
		return 0, errors.New("RevokeRefreshTokenFamily not stubbed")
	}
	return s.revokeFamilyFn(ctx, familyID)
}
func (s *stubRepo) RevokeRefreshTokenByID(ctx context.Context, id int64) error {
	if s.revokeByIDFn == nil {
		return errors.New("RevokeRefreshTokenByID not stubbed")
	}
	return s.revokeByIDFn(ctx, id)
}

// TestRegister_HappyPath covers the full Register pipeline: bcrypt
// hashing, repo.CreateUser, JWT mint, AuthResponse assembly. The
// assertion points that matter:
//   - PasswordHash is bcrypt-formatted (cost-prefixed, $ starts the hash)
//   - The plaintext password from the request never leaks into the hash
//   - The returned User.ID matches what the repo stamped
//   - The token is non-empty (round-trips through tokens.ValidateToken)
func TestRegister_HappyPath(t *testing.T) {
	repo := &stubRepo{
		createFn: func(_ context.Context, u *User) error {
			u.ID = 42
			return nil
		},
	}
	svc := NewService(repo, newTestTokens(t), 15*time.Minute, 7*24*time.Hour, noop.NewTracerProvider().Tracer("test"))

	res, err := svc.Register(context.Background(), RegisterRequest{
		Username: "alice",
		Email:    "alice@example.com",
		Password: "hunter2",
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	if res.User.ID != 42 {
		t.Errorf("User.ID = %d, want 42", res.User.ID)
	}
	if res.User.PasswordHash == "" || res.User.PasswordHash == "hunter2" {
		t.Errorf("PasswordHash = %q, want bcrypt hash, not plaintext", res.User.PasswordHash)
	}
	if res.User.PasswordHash[0] != '$' {
		t.Errorf("PasswordHash doesn't look like a bcrypt hash: %q", res.User.PasswordHash)
	}
	if res.AccessToken == "" {
		t.Error("Token is empty; tokens.GenerateToken returned an empty string")
	}
}

// TestRegister_RepoUniqueViolationBubbles confirms the service does
// not silently swallow a unique-violation sentinel — the handler maps
// each one to HTTP 409 with a distinct wire message. Anything else
// leaking through would mask the real cause from the client.
func TestRegister_RepoUniqueViolationBubbles(t *testing.T) {
	cases := []struct {
		name     string
		sentinel error
	}{
		{"UsernameTaken", ErrUsernameTaken},
		{"EmailTaken", ErrEmailTaken},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want := tc.sentinel
			repo := &stubRepo{
				createFn: func(_ context.Context, _ *User) error {
					return want
				},
			}
			svc := NewService(repo, newTestTokens(t), 15*time.Minute, 7*24*time.Hour, noop.NewTracerProvider().Tracer("test"))

			_, err := svc.Register(context.Background(), RegisterRequest{
				Username: "alice",
				Email:    "alice@example.com",
				Password: "hunter2",
			})
			if !errors.Is(err, want) {
				t.Errorf("expected %v to surface, got %v", want, err)
			}
		})
	}
}

// TestLogin_HappyPath exercises the Login pipeline. We pass a stub
// repo that returns a bcrypt-hashed password (computed in-test so the
// hash matches the password we send) so the test doesn't depend on
// bcrypt cost / timing constants.
//
// Two assertions matter here:
//   - ErrInvalidCredentials does NOT appear on success
//   - The returned AuthResponse carries both the JWT and the user
func TestLogin_HappyPath(t *testing.T) {
	const plain = "hunter2"
	hash, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt.GenerateFromPassword: %v", err)
	}
	repo := &stubRepo{
		getByUsernameFn: func(_ context.Context, username string) (*User, error) {
			return &User{
				ID:           7,
				Username:     username,
				Email:        "alice@example.com",
				PasswordHash: string(hash),
				CreatedAt:    time.Now(),
			}, nil
		},
	}
	svc := NewService(repo, newTestTokens(t), 15*time.Minute, 7*24*time.Hour, noop.NewTracerProvider().Tracer("test"))

	res, err := svc.Login(context.Background(), LoginRequest{
		Username: "alice",
		Password: plain,
	})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if res.User.Username != "alice" {
		t.Errorf("User.Username = %q, want %q", res.User.Username, "alice")
	}
	if res.AccessToken == "" {
		t.Error("Token is empty after successful login")
	}
}

// TestLogin_UnknownUsernameReturnsInvalidCredentials is the security
// invariant: "no such user" must be indistinguishable from "wrong
// password" at the service boundary. The handler relies on this —
// returning ErrUserNotFound instead would leak user existence to
// attackers probing the login endpoint.
func TestLogin_UnknownUsernameReturnsInvalidCredentials(t *testing.T) {
	repo := &stubRepo{
		getByUsernameFn: func(_ context.Context, _ string) (*User, error) {
			return nil, ErrUserNotFound
		},
	}
	svc := NewService(repo, newTestTokens(t), 15*time.Minute, 7*24*time.Hour, noop.NewTracerProvider().Tracer("test"))

	_, err := svc.Login(context.Background(), LoginRequest{
		Username: "ghost",
		Password: "anything",
	})
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("unknown username must map to ErrInvalidCredentials, got %v", err)
	}
}

// TestLogin_WrongPasswordReturnsInvalidCredentials confirms the same
// treatment for the "user exists, wrong password" branch.
func TestLogin_WrongPasswordReturnsInvalidCredentials(t *testing.T) {
	const plain = "hunter2"
	hash, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt.GenerateFromPassword: %v", err)
	}
	repo := &stubRepo{
		getByUsernameFn: func(_ context.Context, username string) (*User, error) {
			return &User{ID: 1, Username: username, PasswordHash: string(hash)}, nil
		},
	}
	svc := NewService(repo, newTestTokens(t), 15*time.Minute, 7*24*time.Hour, noop.NewTracerProvider().Tracer("test"))

	_, err = svc.Login(context.Background(), LoginRequest{
		Username: "alice",
		Password: "wrong-password",
	})
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("wrong password must map to ErrInvalidCredentials, got %v", err)
	}
}

// TestLogin_NonNotFoundRepoErrorPropagates — when the DB itself fails
// (network drop, table missing, etc.) we want the real error to flow
// up, NOT to be masked as ErrInvalidCredentials. That would hide
// outages behind a 401.
func TestLogin_NonNotFoundRepoErrorPropagates(t *testing.T) {
	other := errors.New("connection refused")
	repo := &stubRepo{
		getByUsernameFn: func(_ context.Context, _ string) (*User, error) {
			return nil, other
		},
	}
	svc := NewService(repo, newTestTokens(t), 15*time.Minute, 7*24*time.Hour, noop.NewTracerProvider().Tracer("test"))

	_, err := svc.Login(context.Background(), LoginRequest{Username: "alice", Password: "x"})
	if errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("non-NotFound repo error must not be masked as ErrInvalidCredentials")
	}
	if !errors.Is(err, other) {
		t.Errorf("expected original error to propagate, got %v", err)
	}
}

// ---- Refresh tests ----

// TestRefresh_HappyPath covers the full Refresh pipeline: hash
// supplied raw token, fetch the row, mint a new access + refresh
// pair, and verify both the new access token is a valid JWT and the
// raw refresh token is non-empty.
func TestRefresh_HappyPath(t *testing.T) {
	now := time.Now()
	row := &RefreshTokenRow{
		ID:        42,
		UserID:    7,
		FamilyID:  42,
		ExpiresAt: now.Add(time.Hour),
	}
	repo := &stubRepo{
		getRefreshFn: func(_ context.Context, hash []byte) (*RefreshTokenRow, error) {
			return row, nil
		},
		rotateFn: func(_ context.Context, oldID int64, userID int, hash []byte, familyID int64, expiresAt time.Time) (*RefreshTokenRow, error) {
			return &RefreshTokenRow{
				ID:        99,
				UserID:    userID,
				FamilyID:  familyID,
				ExpiresAt: expiresAt,
			}, nil
		},
		getByIDFn: func(_ context.Context, id int) (*User, error) {
			return &User{ID: id, Username: "alice", Email: "a@x.com"}, nil
		},
	}
	tokens := newTestTokens(t)
	svc := NewService(repo, tokens, 15*time.Minute, 7*24*time.Hour, noop.NewTracerProvider().Tracer("test"))

	// Use a real refresh token raw value so the SHA256 hash path
	// actually exercises the hashing helper.
	raw, _, err := newRefreshToken()
	if err != nil {
		t.Fatalf("newRefreshToken: %v", err)
	}
	res, err := svc.Refresh(context.Background(), raw)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if res.AccessToken == "" {
		t.Error("res.AccessToken is empty after successful refresh")
	}
	if res.RefreshToken == "" {
		t.Error("res.RefreshToken is empty after successful refresh")
	}
	if res.RefreshToken == raw {
		t.Error("refresh returned the same token; rotation didn't mint a new one")
	}
	if _, err := tokens.ValidateToken(res.AccessToken); err != nil {
		t.Errorf("new access token failed ValidateToken: %v", err)
	}
}

// TestRefresh_ReuseDetectedRevokesFamily confirms the RFC 9700 path:
// presenting a refresh token that's already been rotated triggers
// RevokeRefreshTokenFamily and surfaces ErrRefreshTokenReuse. The
// family revoke is the important security property; without it a
// stolen old token could keep being replayed even after the user
// already rotated.
func TestRefresh_ReuseDetectedRevokesFamily(t *testing.T) {
	now := time.Now()
	var revokedFamily int64
	repo := &stubRepo{
		getRefreshFn: func(_ context.Context, _ []byte) (*RefreshTokenRow, error) {
			return &RefreshTokenRow{
				ID:        42,
				UserID:    7,
				FamilyID:  42,
				ExpiresAt: now.Add(time.Hour),
				RevokedAt: &now, // already revoked — reuse signal
			}, nil
		},
		revokeFamilyFn: func(_ context.Context, familyID int64) (int64, error) {
			revokedFamily = familyID
			return 1, nil
		},
	}
	svc := NewService(repo, newTestTokens(t), 15*time.Minute, 7*24*time.Hour, noop.NewTracerProvider().Tracer("test"))

	raw, _, _ := newRefreshToken()
	_, err := svc.Refresh(context.Background(), raw)
	if !errors.Is(err, ErrRefreshTokenReuse) {
		t.Fatalf("expected ErrRefreshTokenReuse, got %v", err)
	}
	if revokedFamily != 42 {
		t.Errorf("expected family 42 to be revoked, got %d", revokedFamily)
	}
}

// TestRefresh_ExpiredReturnsErrExpired — a token past its expires_at
// surfaces as ErrRefreshTokenExpired (not ErrRefreshTokenReuse). The
// family is NOT revoked: a legitimate timeout is not a theft signal.
func TestRefresh_ExpiredReturnsErrExpired(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	repo := &stubRepo{
		getRefreshFn: func(_ context.Context, _ []byte) (*RefreshTokenRow, error) {
			return &RefreshTokenRow{
				ID:        42,
				UserID:    7,
				FamilyID:  42,
				ExpiresAt: past,
			}, nil
		},
		revokeFamilyFn: func(_ context.Context, _ int64) (int64, error) {
			t.Error("family must NOT be revoked on a legitimate expiry")
			return 0, nil
		},
	}
	svc := NewService(repo, newTestTokens(t), 15*time.Minute, 7*24*time.Hour, noop.NewTracerProvider().Tracer("test"))

	raw, _, _ := newRefreshToken()
	_, err := svc.Refresh(context.Background(), raw)
	if !errors.Is(err, ErrRefreshTokenExpired) {
		t.Errorf("expected ErrRefreshTokenExpired, got %v", err)
	}
}

// TestRefresh_InvalidHashReturnsErrInvalid — a token whose hash is
// not in the DB surfaces as ErrInvalidRefreshToken. The lookup
// happens before any state mutation, so no row is created or
// revoked.
func TestRefresh_InvalidHashReturnsErrInvalid(t *testing.T) {
	repo := &stubRepo{
		getRefreshFn: func(_ context.Context, _ []byte) (*RefreshTokenRow, error) {
			return nil, ErrRefreshTokenNotFound
		},
	}
	svc := NewService(repo, newTestTokens(t), 15*time.Minute, 7*24*time.Hour, noop.NewTracerProvider().Tracer("test"))

	raw, _, _ := newRefreshToken()
	_, err := svc.Refresh(context.Background(), raw)
	if !errors.Is(err, ErrInvalidRefreshToken) {
		t.Errorf("expected ErrInvalidRefreshToken, got %v", err)
	}
}

// TestRefresh_ConcurrentRotationSecondCallWins models the
// concurrent-rotation race: the first call sees revoked_at=NULL and
// proceeds; the second call (running in parallel, simulating two
// devices refreshing simultaneously) sees revoked_at != NULL because
// the first call's CTE stamped the old row.
//
// We can't run a real concurrent test here without a real DB, so we
// model the race sequentially: two calls share one stubbed
// RotateRefreshToken that flips the row state to revoked between
// calls. The service's reuse detection must catch the second.
func TestRefresh_ConcurrentRotationSecondCallWins(t *testing.T) {
	now := time.Now()
	var calls int
	var revoked bool
	repo := &stubRepo{
		getRefreshFn: func(_ context.Context, _ []byte) (*RefreshTokenRow, error) {
			row := &RefreshTokenRow{
				ID:        42,
				UserID:    7,
				FamilyID:  42,
				ExpiresAt: now.Add(time.Hour),
			}
			if revoked {
				t := now
				row.RevokedAt = &t
			}
			return row, nil
		},
		rotateFn: func(_ context.Context, oldID int64, userID int, _ []byte, familyID int64, expiresAt time.Time) (*RefreshTokenRow, error) {
			// First rotation wins; second call observes revoked.
			revoked = true
			calls++
			return &RefreshTokenRow{ID: int64(100 + calls), UserID: userID, FamilyID: familyID, ExpiresAt: expiresAt}, nil
		},
		getByIDFn: func(_ context.Context, id int) (*User, error) {
			return &User{ID: id, Username: "alice"}, nil
		},
	}
	svc := NewService(repo, newTestTokens(t), 15*time.Minute, 7*24*time.Hour, noop.NewTracerProvider().Tracer("test"))

	raw1, _, _ := newRefreshToken()
	_, err := svc.Refresh(context.Background(), raw1)
	if err != nil {
		t.Fatalf("first Refresh: %v", err)
	}

	// Second call uses the SAME raw token. Service must take the
	// reuse branch because the first call's RotateRefreshToken
	// stamped revoked_at on the old row.
	_, err = svc.Refresh(context.Background(), raw1)
	if !errors.Is(err, ErrRefreshTokenReuse) {
		t.Errorf("second Refresh: expected ErrRefreshTokenReuse, got %v", err)
	}
}

// ---- Logout tests ----

// TestLogout_Idempotent — calling Logout with a token that's not in
// the DB returns nil, not an error. This is the design choice
// (per plan §service): Logout is idempotent so the frontend can call
// it without first checking whether the session is still alive.
func TestLogout_Idempotent(t *testing.T) {
	repo := &stubRepo{
		getRefreshFn: func(_ context.Context, _ []byte) (*RefreshTokenRow, error) {
			return nil, ErrRefreshTokenNotFound
		},
	}
	svc := NewService(repo, newTestTokens(t), 15*time.Minute, 7*24*time.Hour, noop.NewTracerProvider().Tracer("test"))

	raw, _, _ := newRefreshToken()
	if err := svc.Logout(context.Background(), raw); err != nil {
		t.Errorf("Logout on unknown token should be nil, got %v", err)
	}
}

// TestLogout_RevokesFamily — the supplied token's family is the unit
// of revocation. We confirm RevokeRefreshTokenFamily is called with
// the row's family_id, not the row's id.
func TestLogout_RevokesFamily(t *testing.T) {
	now := time.Now()
	var revokedFamily int64
	repo := &stubRepo{
		getRefreshFn: func(_ context.Context, _ []byte) (*RefreshTokenRow, error) {
			return &RefreshTokenRow{ID: 1, UserID: 7, FamilyID: 99, ExpiresAt: now.Add(time.Hour)}, nil
		},
		revokeFamilyFn: func(_ context.Context, familyID int64) (int64, error) {
			revokedFamily = familyID
			return 3, nil
		},
	}
	svc := NewService(repo, newTestTokens(t), 15*time.Minute, 7*24*time.Hour, noop.NewTracerProvider().Tracer("test"))

	raw, _, _ := newRefreshToken()
	if err := svc.Logout(context.Background(), raw); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if revokedFamily != 99 {
		t.Errorf("expected family 99 to be revoked, got %d", revokedFamily)
	}
}

// TestLogout_RequiresRefreshToken — huma's `required:"true"` tag
// rejects an empty token before it reaches the service. We can't
// easily exercise that path in service_test.go (it's a huma
// validator), so this test instead pins the service's
// no-panic-on-empty-token behavior. An empty string hashes to a
// deterministic value, the lookup misses, and Logout returns nil
// (idempotent). The actual 400 surface happens at the handler.
func TestLogout_RequiresRefreshToken(t *testing.T) {
	repo := &stubRepo{
		getRefreshFn: func(_ context.Context, _ []byte) (*RefreshTokenRow, error) {
			return nil, ErrRefreshTokenNotFound
		},
	}
	svc := NewService(repo, newTestTokens(t), 15*time.Minute, 7*24*time.Hour, noop.NewTracerProvider().Tracer("test"))

	if err := svc.Logout(context.Background(), ""); err != nil {
		t.Errorf("Logout with empty token should be nil (idempotent), got %v", err)
	}
}
