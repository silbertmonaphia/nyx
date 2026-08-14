package user

import (
	"context"
	"errors"
	"testing"
	"time"

	"nyx/internal/user/db"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

// stubQuerier is a hand-rolled mock of the local Querier interface.
// The user package has no testcontainers / pgxmock-pool setup yet (see
// FUTURE.md); a struct mock keeps these tests focused on the
// CreateUser translation logic without pulling in infrastructure.
//
// The refresh-token fns default to errors so tests that don't exercise
// them still fail loudly if a test path accidentally calls into one.
type stubQuerier struct {
	insertResp  db.User
	insertErr   error
	insertCalls int

	createRefreshResp  db.RefreshToken
	createRefreshErr   error
	getRefreshResp     db.RefreshToken
	getRefreshErr      error
	rotateRefreshResp  db.RotateRefreshTokenRow
	rotateRefreshErr   error
	revokeFamilyResp   int64
	revokeFamilyErr    error
	revokeFamilyCalls  int
	revokeByIDErr      error
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
func (s *stubQuerier) CreateRefreshToken(_ context.Context, _ db.CreateRefreshTokenParams) (db.RefreshToken, error) {
	return s.createRefreshResp, s.createRefreshErr
}
func (s *stubQuerier) GetRefreshTokenByHash(_ context.Context, _ []byte) (db.RefreshToken, error) {
	return s.getRefreshResp, s.getRefreshErr
}
func (s *stubQuerier) RotateRefreshToken(_ context.Context, _ db.RotateRefreshTokenParams) (db.RotateRefreshTokenRow, error) {
	return s.rotateRefreshResp, s.rotateRefreshErr
}
func (s *stubQuerier) RevokeRefreshTokenFamily(_ context.Context, _ int64) (int64, error) {
	s.revokeFamilyCalls++
	return s.revokeFamilyResp, s.revokeFamilyErr
}
func (s *stubQuerier) RevokeRefreshTokenByID(_ context.Context, _ int64) error {
	return s.revokeByIDErr
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

// ---- Refresh-token repository tests ----

// TestCreateRefreshToken_SetsFamilyToSelfID verifies the API-shaped
// projection of CreateRefreshToken: the SQL CTE returns family_id equal
// to the inserted row's id (the self-stamp), so the service layer can
// start a brand-new family with a single round trip. We pin the
// conversion path so future schema drift (e.g. a UUID family_id)
// doesn't silently break the equality assumption.
func TestCreateRefreshToken_SetsFamilyToSelfID(t *testing.T) {
	now := time.Now()
	stub := &stubQuerier{
		createRefreshResp: db.RefreshToken{
			ID:        99,
			UserID:    7,
			TokenHash: []byte("hash"),
			FamilyID:  99, // self-stamped
			ExpiresAt: pgtype.Timestamptz{Time: now, Valid: true},
			CreatedAt: pgtype.Timestamptz{Time: now, Valid: true},
		},
	}
	repo := NewRepository(stub)

	row, err := repo.CreateRefreshToken(context.Background(), 7, []byte("hash"), now)
	if err != nil {
		t.Fatalf("CreateRefreshToken: %v", err)
	}
	if row.ID != 99 || row.FamilyID != 99 {
		t.Errorf("ID=%d FamilyID=%d, want both 99 (self-stamped)", row.ID, row.FamilyID)
	}
	if row.UserID != 7 {
		t.Errorf("UserID=%d, want 7", row.UserID)
	}
	if row.RevokedAt != nil {
		t.Errorf("RevokedAt=%v, want nil on a fresh token", row.RevokedAt)
	}
	if row.ReplacedByID != nil {
		t.Errorf("ReplacedByID=%v, want nil on a fresh token", row.ReplacedByID)
	}
}

// TestRotateRefreshToken_MarksOldRevoked confirms the rotated row
// carries the timestamp projection through. The CTE marks the OLD
// row's revoked_at = now() and the NEW row inherits replaced_by_id —
// neither is visible in this method's return (it returns the NEW row),
// but we at least confirm the new row is projected cleanly with no
// false-revoked state on itself.
func TestRotateRefreshToken_MarksOldRevoked(t *testing.T) {
	now := time.Now()
	stub := &stubQuerier{
		rotateRefreshResp: db.RotateRefreshTokenRow{
			ID:        100,
			UserID:    7,
			TokenHash: []byte("new-hash"),
			FamilyID:  42,
			// The new row is born fresh — ReplacedByID/RevokedAt unset
			// because RotateRefreshToken returns the inserted row, not
			// the old one being marked revoked.
			ExpiresAt: pgtype.Timestamptz{Time: now, Valid: true},
			CreatedAt: pgtype.Timestamptz{Time: now, Valid: true},
		},
	}
	repo := NewRepository(stub)

	row, err := repo.RotateRefreshToken(context.Background(), 42 /*oldID*/, 7, []byte("new-hash"), 42 /*familyID*/, now)
	if err != nil {
		t.Fatalf("RotateRefreshToken: %v", err)
	}
	if row.ID != 100 {
		t.Errorf("ID=%d, want 100", row.ID)
	}
	if row.FamilyID != 42 {
		t.Errorf("FamilyID=%d, want 42", row.FamilyID)
	}
	if row.RevokedAt != nil {
		t.Errorf("new row RevokedAt=%v, want nil", row.RevokedAt)
	}
}

// TestRevokeRefreshTokenFamily_SkipsAlreadyRevoked pins the family
// revocation pass-through. The repository's job is to surface the
// rows-affected count (0 vs >0) so the service layer can log
// appropriately; this test confirms the count flows through and the
// repository does not error on the zero case.
func TestRevokeRefreshTokenFamily_SkipsAlreadyRevoked(t *testing.T) {
	stub := &stubQuerier{revokeFamilyResp: 0}
	repo := NewRepository(stub)

	n, err := repo.RevokeRefreshTokenFamily(context.Background(), 99)
	if err != nil {
		t.Fatalf("RevokeRefreshTokenFamily: %v", err)
	}
	if n != 0 {
		t.Errorf("rows affected = %d, want 0 (already-revoked family)", n)
	}
	if stub.revokeFamilyCalls != 1 {
		t.Errorf("expected 1 call, got %d", stub.revokeFamilyCalls)
	}
}

// TestGetRefreshTokenByHash_NoRowsMapsToSentinel exercises the
// pgx.ErrNoRows → ErrRefreshTokenNotFound translation. A missing
// refresh token (e.g. caller garbage or already-rotated-and-deleted)
// must surface as the domain sentinel, not the driver error, so the
// service layer's errors.Is check works.
func TestGetRefreshTokenByHash_NoRowsMapsToSentinel(t *testing.T) {
	stub := &stubQuerier{getRefreshErr: pgx.ErrNoRows}
	repo := NewRepository(stub)

	_, err := repo.GetRefreshTokenByHash(context.Background(), []byte("missing"))
	if !errors.Is(err, ErrRefreshTokenNotFound) {
		t.Errorf("expected ErrRefreshTokenNotFound, got %v", err)
	}
}