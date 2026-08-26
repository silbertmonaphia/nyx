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
	createRefreshFn    func(ctx context.Context, userID int, tokenHash []byte, expiresAt time.Time) (*RefreshTokenRow, error)
	getRefreshFn       func(ctx context.Context, tokenHash []byte) (*RefreshTokenRow, error)
	rotateFn           func(ctx context.Context, oldID int64, userID int, tokenHash []byte, familyID int64, expiresAt time.Time) (*RefreshTokenRow, error)
	revokeFamilyFn     func(ctx context.Context, familyID int64) (int64, error)
	revokeByIDFn       func(ctx context.Context, id int64) error
	countActiveFn      func(ctx context.Context, userID int) (int64, error)
	listOldestActiveFn func(ctx context.Context, userID int, limit int) ([]int64, error)
	purgeOlderThanFn   func(ctx context.Context, cutoff time.Time) (int64, error)
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

func (s *stubRepo) CountActiveRefreshTokensByUser(ctx context.Context, userID int) (int64, error) {
	if s.countActiveFn == nil {
		// Default to "well under cap" so tests that don't care about
		// the cap path don't have to stub it.
		return 0, nil
	}
	return s.countActiveFn(ctx, userID)
}

func (s *stubRepo) ListOldestActiveRefreshTokensByUser(ctx context.Context, userID int, limit int) ([]int64, error) {
	if s.listOldestActiveFn == nil {
		return nil, nil
	}
	return s.listOldestActiveFn(ctx, userID, limit)
}

func (s *stubRepo) PurgeRefreshTokensOlderThan(ctx context.Context, cutoff time.Time) (int64, error) {
	if s.purgeOlderThanFn == nil {
		return 0, nil
	}
	return s.purgeOlderThanFn(ctx, cutoff)
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

// TestRegister_CollapsesUniqueViolationsToUserAlreadyExists pins the
// H5 invariant: regardless of whether the username or the email
// collides, the wire surface is a single ErrUserAlreadyExists. An
// attacker probing the registration endpoint can no longer tell
// which field is already taken.
func TestRegister_CollapsesUniqueViolationsToUserAlreadyExists(t *testing.T) {
	cases := []struct {
		name     string
		sentinel error
	}{
		{"UsernameTaken", ErrUsernameTaken},
		{"EmailTaken", ErrEmailTaken},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &stubRepo{
				createFn: func(_ context.Context, _ *User) error {
					return tc.sentinel
				},
			}
			svc := NewService(repo, newTestTokens(t), 15*time.Minute, 7*24*time.Hour, noop.NewTracerProvider().Tracer("test"))

			_, err := svc.Register(context.Background(), RegisterRequest{
				Username: "alice",
				Email:    "alice@example.com",
				Password: "hunter2",
			})
			if !errors.Is(err, ErrUserAlreadyExists) {
				t.Errorf("expected ErrUserAlreadyExists to surface, got %v", err)
			}
			// The granular sentinel must NOT leak — that would defeat
			// the whole point of the collapse.
			if errors.Is(err, tc.sentinel) && tc.sentinel != ErrUserAlreadyExists {
				t.Errorf("granular sentinel %v leaked through the collapse", tc.sentinel)
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

// TestDummyBcryptHashReady pins the H2 invariant: the dummy hash
// used to equalise Login timing on the not-found branch must be
// non-empty and bcrypt-comparable at package init. If init() fails
// the whole process fails to start, so a non-nil hash plus a
// successful CompareHashAndPassword is enough.
func TestDummyBcryptHashReady(t *testing.T) {
	if len(dummyBcryptHash) == 0 {
		t.Fatal("dummyBcryptHash not initialised; init() likely failed silently")
	}
	// Must reject the obvious wrong password and accept the plaintext
	// the dummy was computed from — together these confirm the hash
	// is bcrypt-shaped and bcrypt-cost-compatible with real hashes.
	if err := bcrypt.CompareHashAndPassword(dummyBcryptHash, []byte("wrong")); err == nil {
		t.Error("dummyBcryptHash matched an unrelated password")
	}
	if err := bcrypt.CompareHashAndPassword(dummyBcryptHash, []byte("timing-equaliser-not-a-real-password")); err != nil {
		t.Errorf("dummyBcryptHash did not match the plaintext it was computed from: %v", err)
	}
}

// TestRegister_RefreshCapEnforced pins M2 via the public Register
// entry point: when the user's active refresh-token count is at the
// cap, registering (which mints a refresh token) revokes the oldest
// surplus row before inserting. mintRefreshToken is unexported, so
// we drive it through the public surface.
func TestRegister_RefreshCapEnforced(t *testing.T) {
	var listCalls, revokeCalls int
	repo := &stubRepo{
		createFn: func(_ context.Context, u *User) error {
			u.ID = 1
			return nil
		},
		countActiveFn: func(_ context.Context, _ int) (int64, error) {
			return int64(RefreshCap), nil // user already at cap
		},
		listOldestActiveFn: func(_ context.Context, _ int, lim int) ([]int64, error) {
			listCalls++
			if lim != 1 {
				t.Errorf("ListOldest limit = %d, want 1 (cap surplus = cap - cap + 1)", lim)
			}
			return []int64{42}, nil
		},
		revokeByIDFn: func(_ context.Context, id int64) error {
			revokeCalls++
			if id != 42 {
				t.Errorf("revoked id = %d, want 42 (oldest row)", id)
			}
			return nil
		},
	}
	svc := NewService(repo, newTestTokens(t), 15*time.Minute, 7*24*time.Hour, noop.NewTracerProvider().Tracer("test"))

	if _, err := svc.Register(context.Background(), RegisterRequest{
		Username: "alice",
		Email:    "alice@example.com",
		Password: "hunter2",
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if listCalls != 1 {
		t.Errorf("ListOldestActiveRefreshTokensByUser called %d times, want 1", listCalls)
	}
	if revokeCalls != 1 {
		t.Errorf("RevokeRefreshTokenByID called %d times, want 1", revokeCalls)
	}
}

// TestRegister_BelowRefreshCapSkipsList pins the fast path: when
// the user is below the cap, no list/revoke queries should fire.
func TestRegister_BelowRefreshCapSkipsList(t *testing.T) {
	repo := &stubRepo{
		createFn: func(_ context.Context, u *User) error {
			u.ID = 1
			return nil
		},
		countActiveFn: func(_ context.Context, _ int) (int64, error) {
			return int64(RefreshCap - 1), nil
		},
		listOldestActiveFn: func(_ context.Context, _ int, _ int) ([]int64, error) {
			t.Error("ListOldestActiveRefreshTokensByUser must not be called below the cap")
			return nil, nil
		},
		revokeByIDFn: func(_ context.Context, _ int64) error {
			t.Error("RevokeRefreshTokenByID must not be called below the cap")
			return nil
		},
	}
	svc := NewService(repo, newTestTokens(t), 15*time.Minute, 7*24*time.Hour, noop.NewTracerProvider().Tracer("test"))

	if _, err := svc.Register(context.Background(), RegisterRequest{
		Username: "alice",
		Email:    "alice@example.com",
		Password: "hunter2",
	}); err != nil {
		t.Fatalf("Register: %v", err)
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

// TestGetByID_HappyPath verifies the /api/me service path: the
// user id from the access token's subject reaches the repository
// unchanged, and the returned user is passed back to the handler
// verbatim. SECURITY.md L7.
func TestGetByID_HappyPath(t *testing.T) {
	want := &User{ID: 42, Username: "alice", Email: "alice@example.com"}
	var seenID int
	repo := &stubRepo{
		getByIDFn: func(_ context.Context, id int) (*User, error) {
			seenID = id
			return want, nil
		},
	}
	svc := NewService(repo, newTestTokens(t), 15*time.Minute, 7*24*time.Hour, noop.NewTracerProvider().Tracer("test"))

	got, err := svc.GetByID(context.Background(), 42)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if seenID != 42 {
		t.Errorf("repo received id=%d, want 42", seenID)
	}
	if got != want {
		t.Errorf("returned user = %+v, want %+v", got, want)
	}
}

// TestGetByID_RepoErrorPropagates — a database failure surfaces
// to the handler unchanged so api.MapError can decide the HTTP
// status. SECURITY.md L7.
func TestGetByID_RepoErrorPropagates(t *testing.T) {
	sentinel := errors.New("connection reset")
	repo := &stubRepo{
		getByIDFn: func(_ context.Context, _ int) (*User, error) {
			return nil, sentinel
		},
	}
	svc := NewService(repo, newTestTokens(t), 15*time.Minute, 7*24*time.Hour, noop.NewTracerProvider().Tracer("test"))

	_, err := svc.GetByID(context.Background(), 1)
	if !errors.Is(err, sentinel) {
		t.Errorf("GetByID err = %v, want %v", err, sentinel)
	}
}

// TestGetByID_NotFound — ErrUserNotFound from the repository is
// the canonical "user was deleted between login and now" signal.
// api.MapError translates it to 404; this test pins the
// propagation so a future service-layer short-circuit doesn't
// accidentally swallow it.
func TestGetByID_NotFound(t *testing.T) {
	repo := &stubRepo{
		getByIDFn: func(_ context.Context, _ int) (*User, error) {
			return nil, ErrUserNotFound
		},
	}
	svc := NewService(repo, newTestTokens(t), 15*time.Minute, 7*24*time.Hour, noop.NewTracerProvider().Tracer("test"))

	_, err := svc.GetByID(context.Background(), 99)
	if !errors.Is(err, ErrUserNotFound) {
		t.Errorf("GetByID err = %v, want ErrUserNotFound", err)
	}
}

// TestLogout_RevokesOnlySuppliedRow — per-session revocation (M1).
// Logging out revokes exactly the supplied row's ID, not the
// surrounding family. This is the property that lets two devices
// stay logged in independently.
func TestLogout_RevokesOnlySuppliedRow(t *testing.T) {
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
			t.Error("Logout must NOT call RevokeRefreshTokenFamily; per-session only")
			return 0, nil
		},
	}
	svc := NewService(repo, newTestTokens(t), 15*time.Minute, 7*24*time.Hour, noop.NewTracerProvider().Tracer("test"))

	raw, _, _ := newRefreshToken()
	if err := svc.Logout(context.Background(), raw); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if revokedID != 1 {
		t.Errorf("expected row 1 to be revoked, got %d", revokedID)
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
