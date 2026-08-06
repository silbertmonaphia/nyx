package user

import (
	"context"
	"errors"
	"testing"
	"time"

	"nyx/internal/user/db"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

// stubQuerier is a hand-rolled mock of the local Querier interface.
// The user package has no testcontainers / pgxmock-pool setup yet (see
// FUTURE.md); a struct mock keeps these tests focused on the
// CreateUser translation logic without pulling in infrastructure.
type stubQuerier struct {
	insertResp  db.User
	insertErr   error
	insertCalls int
}

func (s *stubQuerier) InsertUser(_ context.Context, _ db.InsertUserParams) (db.User, error) {
	s.insertCalls++
	return s.insertResp, s.insertErr
}
func (s *stubQuerier) GetUserByUsername(context.Context, string) (db.User, error) {
	return db.User{}, errors.New("GetUserByUsername: not implemented in stub")
}
func (s *stubQuerier) GetUserByID(context.Context, int32) (db.User, error) {
	return db.User{}, errors.New("GetUserByID: not implemented in stub")
}

func TestCreateUser_MapsUniqueViolationToSentinel(t *testing.T) {
	stub := &stubQuerier{
		insertErr: &pgconn.PgError{Code: "23505", Message: "duplicate key value violates unique constraint"},
	}
	repo := NewRepository(stub)

	u := &User{Username: "alice", Email: "alice@example.com", PasswordHash: "hash"}
	err := repo.CreateUser(context.Background(), u)

	if !errors.Is(err, ErrUserAlreadyExists) {
		t.Errorf("expected ErrUserAlreadyExists, got %v", err)
	}
	if stub.insertCalls != 1 {
		t.Errorf("expected 1 InsertUser call, got %d", stub.insertCalls)
	}
}

func TestCreateUser_PropagatesNonUniqueErrors(t *testing.T) {
	other := errors.New("connection refused")
	stub := &stubQuerier{insertErr: other}
	repo := NewRepository(stub)

	u := &User{Username: "alice"}
	err := repo.CreateUser(context.Background(), u)

	if errors.Is(err, ErrUserAlreadyExists) {
		t.Errorf("non-unique error must not map to ErrUserAlreadyExists")
	}
	if !errors.Is(err, other) {
		t.Errorf("expected original error to propagate, got %v", err)
	}
}

func TestCreateUser_HappyPath(t *testing.T) {
	now := time.Now()
	stub := &stubQuerier{
		insertResp: db.User{
			ID:        7,
			Username:  "alice",
			Email:     "alice@example.com",
			CreatedAt: pgtype.Timestamptz{Time: now, Valid: true},
			UpdatedAt: pgtype.Timestamptz{Time: now, Valid: true},
		},
	}
	repo := NewRepository(stub)

	u := &User{Username: "alice", Email: "alice@example.com", PasswordHash: "hash"}
	if err := repo.CreateUser(context.Background(), u); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if u.ID != 7 {
		t.Errorf("expected ID=7 after CreateUser, got %d", u.ID)
	}
	if u.Username != "alice" {
		t.Errorf("expected username to be preserved, got %q", u.Username)
	}
}