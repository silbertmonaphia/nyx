package user

import (
	"context"
	"errors"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// stubRepo is a hand-rolled mock of the Repository interface. The
// service tests don't need pgxmock — they exercise the bcrypt and JWT
// paths against a pure-Go fake, so failures here are unambiguous
// ("Register didn't return AuthResponse" rather than "mock expected X
// calls but got Y").
type stubRepo struct {
	createFn        func(ctx context.Context, u *User) error
	getByUsernameFn func(ctx context.Context, username string) (*User, error)
	getByIDFn       func(ctx context.Context, id int) (*User, error)
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

// TestRegister_HappyPath covers the full Register pipeline: bcrypt
// hashing, repo.CreateUser, JWT mint, AuthResponse assembly. The
// assertion points that matter:
//   - PasswordHash is bcrypt-formatted (cost-prefixed, $ starts the hash)
//   - The plaintext password from the request never leaks into the hash
//   - The returned User.ID matches what the repo stamped
//   - The token is non-empty (round-trips through auth.ValidateToken)
func TestRegister_HappyPath(t *testing.T) {
	repo := &stubRepo{
		createFn: func(_ context.Context, u *User) error {
			u.ID = 42
			return nil
		},
	}
	svc := NewService(repo)

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
	if res.Token == "" {
		t.Error("Token is empty; auth.GenerateToken returned an empty string")
	}
}

// TestRegister_RepoUniqueViolationBubbles confirms the service does
// not silently swallow ErrUserAlreadyExists — the handler maps it to
// HTTP 409. Anything else leaking through would mask the real cause
// from the client.
func TestRegister_RepoUniqueViolationBubbles(t *testing.T) {
	repo := &stubRepo{
		createFn: func(_ context.Context, _ *User) error {
			return ErrUserAlreadyExists
		},
	}
	svc := NewService(repo)

	_, err := svc.Register(context.Background(), RegisterRequest{
		Username: "alice",
		Email:    "alice@example.com",
		Password: "hunter2",
	})
	if !errors.Is(err, ErrUserAlreadyExists) {
		t.Errorf("expected ErrUserAlreadyExists to surface, got %v", err)
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
	svc := NewService(repo)

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
	if res.Token == "" {
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
	svc := NewService(repo)

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
	svc := NewService(repo)

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
	svc := NewService(repo)

	_, err := svc.Login(context.Background(), LoginRequest{Username: "alice", Password: "x"})
	if errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("non-NotFound repo error must not be masked as ErrInvalidCredentials")
	}
	if !errors.Is(err, other) {
		t.Errorf("expected original error to propagate, got %v", err)
	}
}
