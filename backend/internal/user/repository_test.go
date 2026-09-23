package user

import (
	"context"
	"errors"
	"reflect"
	"regexp"
	"testing"
	"time"

	"nyx/internal/user/db"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/pashagolub/pgxmock/v3"
)

// stubQuerier is a hand-rolled mock of the local Querier interface.
// The user package has no testcontainers / pgxmock-pool setup yet (see
// FUTURE.md); a struct mock keeps these tests focused on the
// CreateUser / single-row projection logic without pulling in
// infrastructure.
//
// Each test sets queryRowResp (the value(s) QueryRow's Scan will
// assign to its dest slice, in declaration order) or queryRowErr
// (returned by Scan verbatim when non-nil). The InsertUser and
// single-row refresh-token methods all flow through QueryRow, so one
// pair of fields covers every "happy / error path" case the unit
// tests need. CreateRefreshToken's two-statement tx path is
// exercised by the pgxmock-based tests below.
type stubQuerier struct {
	queryRowCalls int

	revokeFamilyResp  int64
	revokeFamilyErr   error
	revokeFamilyCalls int
	revokeByIDErr     error

	execResp  pgconn.CommandTag
	execErr   error
	execCalls int

	queryRowResp []any
	queryRowErr  error
}

// stubRow is the pgx.Row stubQuerier.QueryRow hands back. It defers
// all decisions to the enclosing stubQuerier so tests can configure
// the row's behaviour without touching the DBTX surface.
type stubRow struct {
	resp []any
	err  error
}

func (r stubRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != len(r.resp) {
		return errors.New("stubRow: dest/resp length mismatch")
	}
	for i, v := range r.resp {
		assign(dest[i], v)
	}
	return nil
}

// assign copies src into dst via reflection so the stubRow doesn't
// need per-type cases. Both sides are pointers in practice (the
// generated code passes &i.ID etc.), so we route through reflect's
// pointer-to-value dance.
func assign(dst, src any) {
	dstVal := reflect.ValueOf(dst).Elem()
	srcVal := reflect.ValueOf(src)
	if srcVal.Kind() == reflect.Ptr {
		if srcVal.IsNil() {
			dstVal.Set(reflect.Zero(dstVal.Type()))
			return
		}
		srcVal = srcVal.Elem()
	}
	dstVal.Set(srcVal.Convert(dstVal.Type()))
}

func (s *stubQuerier) InsertUser(_ context.Context, _ db.InsertUserParams) (db.User, error) {
	// The InsertUser sqlc implementation reads via QueryRow, so the
	// fields are populated by the stubRow. We surface a zero User
	// here so the signature matches; callers should rely on
	// queryRowResp / queryRowErr to drive Scan.
	return db.User{}, nil
}
func (s *stubQuerier) GetUserByUsername(context.Context, string) (db.User, error) {
	return db.User{}, errors.New("GetUserByUsername: not implemented in stub")
}
func (s *stubQuerier) GetUserByID(context.Context, int32) (db.User, error) {
	return db.User{}, errors.New("GetUserByID: not implemented in stub")
}
func (s *stubQuerier) InsertRefreshToken(_ context.Context, _ db.InsertRefreshTokenParams) (int64, error) {
	// The two-step self-stamp tx is exercised by the pgxmock tests
	// below; the unit-level mock surfaces a sentinel error so any
	// accidental unit-test path that reaches this branch fails loudly.
	return 0, errors.New("InsertRefreshToken: covered by pgxmock-based tests; do not call via stubQuerier")
}
func (s *stubQuerier) StampRefreshTokenFamily(_ context.Context, _ int64) (db.StampRefreshTokenFamilyRow, error) {
	return db.StampRefreshTokenFamilyRow{}, errors.New("StampRefreshTokenFamily: covered by pgxmock-based tests; do not call via stubQuerier")
}
func (s *stubQuerier) GetRefreshTokenByHash(_ context.Context, _ []byte) (db.RefreshToken, error) {
	// The generated implementation reads via QueryRow; this method
	// is never reached because the repository routes through
	// r.q.GetRefreshTokenByHash, which hits stubQuerier.QueryRow.
	// Returning an error here is belt-and-braces — if a refactor
	// ever bypasses QueryRow the test fails loudly.
	return db.RefreshToken{}, errors.New("GetRefreshTokenByHash: not reached via stubQuerier stub")
}
func (s *stubQuerier) RotateRefreshToken(_ context.Context, _ db.RotateRefreshTokenParams) (db.RotateRefreshTokenRow, error) {
	// Same as above — the generated impl reads via QueryRow.
	return db.RotateRefreshTokenRow{}, errors.New("RotateRefreshToken: not reached via stubQuerier stub")
}
func (s *stubQuerier) RevokeRefreshTokenFamily(_ context.Context, _ int64) (int64, error) {
	s.revokeFamilyCalls++
	return s.revokeFamilyResp, s.revokeFamilyErr
}
func (s *stubQuerier) RevokeRefreshTokenByID(_ context.Context, _ int64) error {
	return s.revokeByIDErr
}
func (s *stubQuerier) CountActiveRefreshTokensByUser(_ context.Context, _ int32) (int64, error) {
	return 0, nil
}
func (s *stubQuerier) ListOldestActiveRefreshTokensByUser(_ context.Context, _ db.ListOldestActiveRefreshTokensByUserParams) ([]int64, error) {
	return nil, nil
}
func (s *stubQuerier) PurgeRefreshTokensOlderThan(_ context.Context, _ pgtype.Timestamptz) (int64, error) {
	return 0, nil
}

// stubQuerier satisfies db.DBTX structurally so it can be wrapped in
// *db.Queries for NewRepositoryFromQuerier. QueryRow returns a
// stubRow pre-loaded with queryRowResp / queryRowErr so tests can
// drive both the happy path and the error-translation branches
// without pgxmock. Exec is exercised by the :execrows-style methods
// (e.g. RevokeRefreshTokenFamily) and surfaces execResp / execErr.
// Query is not used by any current test path.
func (s *stubQuerier) Exec(context.Context, string, ...interface{}) (pgconn.CommandTag, error) {
	s.execCalls++
	return s.execResp, s.execErr
}
func (s *stubQuerier) Query(context.Context, string, ...interface{}) (pgx.Rows, error) {
	return nil, errors.New("Query: not implemented in stub")
}
func (s *stubQuerier) QueryRow(context.Context, string, ...interface{}) pgx.Row {
	s.queryRowCalls++
	return stubRow{resp: s.queryRowResp, err: s.queryRowErr}
}

func TestCreateUser_UsernameUniqueViolationMapsToErrUsernameTaken(t *testing.T) {
	stub := &stubQuerier{
		queryRowErr: &pgconn.PgError{
			Code:           "23505",
			ConstraintName: "users_username_key",
			Message:        "duplicate key value violates unique constraint",
		},
	}
	repo := NewRepositoryFromQuerier(db.New(stub))

	u := &User{Username: "alice", PasswordHash: "hash"}
	err := repo.CreateUser(context.Background(), u)

	if !errors.Is(err, ErrUsernameTaken) {
		t.Errorf("expected ErrUsernameTaken, got %v", err)
	}
	if stub.queryRowCalls != 1 {
		t.Errorf("expected 1 QueryRow call, got %d", stub.queryRowCalls)
	}
}

func TestCreateUser_PropagatesNonUniqueErrors(t *testing.T) {
	other := errors.New("connection refused")
	stub := &stubQuerier{queryRowErr: other}
	repo := NewRepositoryFromQuerier(db.New(stub))

	u := &User{Username: "alice"}
	err := repo.CreateUser(context.Background(), u)

	if errors.Is(err, ErrUsernameTaken) {
		t.Errorf("non-unique error must not map to ErrUsernameTaken")
	}
	if !errors.Is(err, other) {
		t.Errorf("expected original error to propagate, got %v", err)
	}
}

// TestCreateUser_HappyPath pins the toUser projection: a successful
// InsertUser fills the caller's *User with the freshly assigned id
// and timestamps so the caller (the service layer) can mint tokens
// without re-reading the row.
func TestCreateUser_HappyPath(t *testing.T) {
	now := time.Now()
	resp := db.User{
		ID:           7,
		Username:     "alice",
		PasswordHash: "hash",
		CreatedAt:    pgtype.Timestamptz{Time: now, Valid: true},
		UpdatedAt:    pgtype.Timestamptz{Time: now, Valid: true},
		DeletedAt:    pgtype.Timestamptz{},
	}
	stub := &stubQuerier{
		queryRowResp: []any{
			resp.ID, resp.Username, resp.PasswordHash,
			resp.CreatedAt, resp.UpdatedAt, resp.DeletedAt,
		},
	}
	repo := NewRepositoryFromQuerier(db.New(stub))

	u := &User{Username: "alice", PasswordHash: "hash"}
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

// insertRefreshSQL / stampRefreshSQL mirror the bodies of the two
// queries in backend/queries/users.sql. pgxmock matches on regex by
// default, so we keep these terse (just the literal statement up to
// the keyword that uniquely identifies it).
var (
	insertRefreshSQL = regexp.QuoteMeta("INSERT INTO refresh_tokens (user_id, token_hash, family_id, expires_at)")
	stampRefreshSQL  = regexp.QuoteMeta("UPDATE refresh_tokens")
)

// TestCreateRefreshToken_TokenHashUniqueViolationMapsToErrRefreshTokenCollision
// covers the new constraint-name-aware path: the
// idx_refresh_tokens_token_hash unique-index collision (defense-in-depth
// against a sha256 hash collision; ~10^-38 per row) maps to the
// ErrRefreshTokenCollision sentinel, not a raw pgconn.PgError bubbling
// up to the 500 branch. The collision surfaces on the first
// statement of the two-step self-stamp (InsertRefreshToken).
func TestCreateRefreshToken_TokenHashUniqueViolationMapsToErrRefreshTokenCollision(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	mock.ExpectBegin()
	mock.ExpectQuery(insertRefreshSQL).
		WithArgs(int32(1), []byte("hash"), pgxmock.AnyArg()).
		WillReturnError(&pgconn.PgError{
			Code:           "23505",
			ConstraintName: "idx_refresh_tokens_token_hash",
			Message:        "duplicate key value violates unique constraint",
		})
	mock.ExpectRollback()

	repo := NewRepository(mock)

	expires := time.Now().Add(time.Hour)
	_, err = repo.CreateRefreshToken(context.Background(), 1, []byte("hash"), expires)

	if !errors.Is(err, ErrRefreshTokenCollision) {
		t.Errorf("expected ErrRefreshTokenCollision, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet pgxmock expectations: %v", err)
	}
}

// TestCreateRefreshToken_SetsFamilyToSelfID verifies the API-shaped
// projection of the two-step self-stamp: InsertRefreshToken returns
// the freshly assigned id, StampRefreshTokenFamily writes family_id
// = id and returns the final row, and the Repository projects it
// into a RefreshTokenRow with ID == FamilyID. We pin the
// projection path so future schema drift (e.g. a UUID family_id)
// doesn't silently break the equality assumption.
func TestCreateRefreshToken_SetsFamilyToSelfID(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	now := time.Now()
	mock.ExpectBegin()
	mock.ExpectQuery(insertRefreshSQL).
		WithArgs(int32(7), []byte("hash"), pgtype.Timestamptz{Time: now, Valid: true}).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(int64(99)))
	mock.ExpectQuery(stampRefreshSQL).
		WithArgs(int64(99)).
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "user_id", "family_id", "replaced_by_id", "expires_at", "revoked_at", "created_at",
		}).AddRow(
			int64(99), int32(7), int64(99), nil,
			pgtype.Timestamptz{Time: now, Valid: true}, nil,
			pgtype.Timestamptz{Time: now, Valid: true},
		))
	mock.ExpectCommit()

	repo := NewRepository(mock)

	row, err := repo.CreateRefreshToken(context.Background(), 7, []byte("hash"), now)
	if err != nil {
		t.Fatalf("CreateRefreshToken: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet pgxmock expectations: %v", err)
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

// TestCreateRefreshToken_RequiresPool covers the NewRepositoryFromQuerier
// guard: CreateRefreshToken panics / errors when called on a
// repository built without a pool. This pins the failure mode so a
// future caller that bypasses NewRepository sees a clear error
// instead of a nil-pointer panic.
func TestCreateRefreshToken_RequiresPool(t *testing.T) {
	stub := &stubQuerier{}
	repo := NewRepositoryFromQuerier(db.New(stub))

	_, err := repo.CreateRefreshToken(context.Background(), 1, []byte("hash"), time.Now())
	if err == nil {
		t.Errorf("expected error when CreateRefreshToken is called without a pool")
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
	resp := db.RotateRefreshTokenRow{
		ID:        100,
		UserID:    7,
		TokenHash: []byte("new-hash"),
		FamilyID:  42,
		// The new row is born fresh — ReplacedByID/RevokedAt unset
		// because RotateRefreshToken returns the inserted row, not
		// the old one being marked revoked.
		ExpiresAt: pgtype.Timestamptz{Time: now, Valid: true},
		CreatedAt: pgtype.Timestamptz{Time: now, Valid: true},
	}
	stub := &stubQuerier{
		queryRowResp: []any{
			resp.ID, resp.UserID, resp.TokenHash, resp.FamilyID,
			resp.ReplacedByID, resp.ExpiresAt, resp.RevokedAt, resp.CreatedAt,
		},
	}
	repo := NewRepositoryFromQuerier(db.New(stub))

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
	stub := &stubQuerier{execResp: pgconn.NewCommandTag("UPDATE 0")}
	repo := NewRepositoryFromQuerier(db.New(stub))

	n, err := repo.RevokeRefreshTokenFamily(context.Background(), 99)
	if err != nil {
		t.Fatalf("RevokeRefreshTokenFamily: %v", err)
	}
	if n != 0 {
		t.Errorf("rows affected = %d, want 0 (already-revoked family)", n)
	}
	if stub.execCalls != 1 {
		t.Errorf("expected 1 Exec call, got %d", stub.execCalls)
	}
}

// TestGetRefreshTokenByHash_NoRowsMapsToSentinel exercises the
// pgx.ErrNoRows → ErrRefreshTokenNotFound translation. A missing
// refresh token (e.g. caller garbage or already-rotated-and-deleted)
// must surface as the domain sentinel, not the driver error, so the
// service layer's errors.Is check works.
func TestGetRefreshTokenByHash_NoRowsMapsToSentinel(t *testing.T) {
	stub := &stubQuerier{queryRowErr: pgx.ErrNoRows}
	repo := NewRepositoryFromQuerier(db.New(stub))

	_, err := repo.GetRefreshTokenByHash(context.Background(), []byte("missing"))
	if !errors.Is(err, ErrRefreshTokenNotFound) {
		t.Errorf("expected ErrRefreshTokenNotFound, got %v", err)
	}
}
