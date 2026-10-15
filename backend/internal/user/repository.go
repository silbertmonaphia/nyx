package user

import (
	"context"
	"errors"
	"net/http"
	"time"

	"nyx/internal/platform/api"
	"nyx/internal/platform/pgerr"
	"nyx/internal/user/db"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Sentinel errors. ErrUserNotFound is the contract the service layer
// uses to map onto ErrInvalidCredentials at the Login boundary; any
// caller that wants to distinguish "no such user" from a real DB
// failure should errors.Is against this value.
//
// ErrUserAlreadyExists is the public, wire-level collapsed form of
// ErrUsernameTaken — registration no longer distinguishes the
// underlying unique constraint so an attacker can't enumerate
// which username is already in use (see SECURITY.md H5). The
// granular sentinel remains as the *internal* failure mode from
// pgerr; the service layer maps it to ErrUserAlreadyExists before
// it reaches the handler.
var (
	ErrUserNotFound          = errors.New("user not found")
	ErrUsernameTaken         = errors.New("username already taken")
	ErrUserAlreadyExists     = errors.New("user already exists")
	ErrRefreshTokenCollision = errors.New("refresh token hash collision")
	ErrRefreshTokenNotFound  = errors.New("refresh token not found")
)

// Register each user-domain sentinel with the api package so
// MapError can render the right HTTP status + wire message. Runs at
// process startup (init() order is undefined across packages but
// each registration is independent). ErrUsernameTaken still maps to
// 409 — it's the internal collision sentinel, surfaced only via the
// service layer's pre-check that collapses it to ErrUserAlreadyExists
// before it hits the wire.
func init() {
	api.RegisterSentinel(ErrUserNotFound, http.StatusNotFound, "User not found")
	api.RegisterSentinel(ErrUsernameTaken, http.StatusConflict, "User already exists")
	api.RegisterSentinel(ErrUserAlreadyExists, http.StatusConflict, "User already exists")
	// ErrRefreshTokenCollision is a 500: a sha256 collision is ~10^-38
	// per row. No clean 4xx story — log loud and let the client retry.
	api.RegisterSentinel(ErrRefreshTokenCollision, http.StatusInternalServerError, "Internal server error")
	api.RegisterSentinel(ErrInvalidCredentials, http.StatusUnauthorized, "Invalid credentials")
	api.RegisterSentinel(ErrInvalidRefreshToken, http.StatusUnauthorized, "Invalid refresh token")
	api.RegisterSentinel(ErrRefreshTokenReuse, http.StatusUnauthorized, "Refresh token revoked")
	api.RegisterSentinel(ErrRefreshTokenExpired, http.StatusUnauthorized, "Refresh token expired")

	// Register pgerr matchers for the user-domain unique constraints.
	// Switched on ConstraintName (not Code) so other domains that share
	// the same SQLSTATE 23505 don't get hijacked.
	pgerr.Register(pgerr.ConstraintUsersUsername, func() error { return ErrUsernameTaken })
	pgerr.Register(pgerr.ConstraintRefreshTokensTokenHash, func() error { return ErrRefreshTokenCollision })
}

// toInt32 narrows an int to int32. The DB columns are SERIAL
// (Postgres INTEGER), so legitimate user-supplied IDs, limits, and
// pagination parameters cannot exceed math.MaxInt32 in practice. If
// one did, pgx would either wrap to a negative int32 (and the query
// would return ErrNoRows, which the service layer maps to a 404) or
// fail with a value-out-of-range SQLSTATE. The conversion is therefore
// safe; the cast is concentrated here so a future schema bump to
// BIGINT can swap the helper in one place.
//
//nolint:gosec // G115: SERIAL columns bound the value well below int32 max.
func toInt32(v int) int32 { return int32(v) }

// Querier is the subset of db.Querier we actually use. Defining it
// here (rather than importing db.Querier directly) lets tests
// substitute a hand-rolled mock without pulling in the generated
// package's full surface.
type Querier interface {
	InsertUser(ctx context.Context, arg db.InsertUserParams) (db.User, error)
	GetUserByUsername(ctx context.Context, username string) (db.User, error)
	GetUserByID(ctx context.Context, id int32) (db.User, error)

	// Refresh-token queries — see backend/queries/users.sql for the
	// SQL bodies. Method names mirror the @name annotations verbatim
	// so a service-layer mock can stub each one by name. The two
	// self-stamp steps (InsertRefreshToken + StampRefreshTokenFamily)
	// and RotateRefreshToken each return their own generated row
	// type because of the explicit RETURNING column lists;
	// GetRefreshTokenByHash queries the table directly so it gets
	// db.RefreshToken.
	InsertRefreshToken(ctx context.Context, arg db.InsertRefreshTokenParams) (int64, error)
	StampRefreshTokenFamily(ctx context.Context, id int64) (db.StampRefreshTokenFamilyRow, error)
	GetRefreshTokenByHash(ctx context.Context, tokenHash []byte) (db.RefreshToken, error)
	RotateRefreshToken(ctx context.Context, arg db.RotateRefreshTokenParams) (db.RotateRefreshTokenRow, error)
	RevokeRefreshTokenFamily(ctx context.Context, familyID int64) (int64, error)
	RevokeRefreshTokenByID(ctx context.Context, id int64) error

	// Refresh-token maintenance (see SECURITY.md M2).
	CountActiveRefreshTokensByUser(ctx context.Context, userID int32) (int64, error)
	ListOldestActiveRefreshTokensByUser(ctx context.Context, arg db.ListOldestActiveRefreshTokensByUserParams) ([]int64, error)
	PurgeRefreshTokensOlderThan(ctx context.Context, cutoff pgtype.Timestamptz) (int64, error)
}

// RefreshTokenRow is the API-shaped projection of db.RefreshToken.
// The service layer consumes this struct (not the generated one) so
// pgtype.Timestamptz → time.Time conversions stay in the repository.
// revoked_at and replaced_by_id are nullable: we surface them as
// pointer / time.Time zero when unset rather than leaking pgtype.
type RefreshTokenRow struct {
	ID           int64
	UserID       int
	FamilyID     int64
	ReplacedByID *int64
	ExpiresAt    time.Time
	RevokedAt    *time.Time
	CreatedAt    time.Time
}

// RefreshCap is the maximum number of concurrently active refresh
// tokens a single user may hold. The service enforces this on every
// mint — Login, Register, Refresh — by revoking the oldest row when
// the new mint would push the count above the cap. Sized to cover a
// realistic device fleet (phone, laptop, tablet, plus a couple of
// spare browser sessions) without letting a stolen-cookie flood
// accumulate forever (see SECURITY.md M2).
const RefreshCap = 10

// RefreshTokenRetention is how long revoked/expired refresh rows
// are kept before the background cleanup goroutine deletes them.
// Long enough to investigate a recent incident; short enough that
// the table doesn't grow unbounded.
const RefreshTokenRetention = 7 * 24 * time.Hour

type Repository interface {
	CreateUser(ctx context.Context, u *User) error
	GetUserByUsername(ctx context.Context, username string) (*User, error)
	GetUserByID(ctx context.Context, id int) (*User, error)

	// Refresh-token operations. CreateRefreshToken mints a row whose
	// family_id == its own id (self-stamped by the repository via a
	// two-statement transaction — see queries/users.sql for the
	// rationale). The caller is responsible for hashing the opaque
	// token before passing it in; this layer never sees raw tokens.
	CreateRefreshToken(ctx context.Context, userID int, tokenHash []byte, expiresAt time.Time) (*RefreshTokenRow, error)
	GetRefreshTokenByHash(ctx context.Context, tokenHash []byte) (*RefreshTokenRow, error)
	// RotateRefreshToken inserts a new row in the same family as @oldID,
	// marks the old row revoked, and returns the new row. Concurrent
	// rotations are detected by the service layer via RevokedAt != nil
	// on the fetched old row.
	RotateRefreshToken(ctx context.Context, oldID int64, userID int, tokenHash []byte, familyID int64, expiresAt time.Time) (*RefreshTokenRow, error)
	RevokeRefreshTokenFamily(ctx context.Context, familyID int64) (int64, error)
	RevokeRefreshTokenByID(ctx context.Context, id int64) error

	// Refresh-token maintenance. CountActiveRefreshTokensByUser feeds
	// RefreshCap enforcement; ListOldestActiveRefreshTokensByUser
	// returns the rows to revoke when the cap is hit; PurgeRefreshTokensOlderThan
	// is the background goroutine's delete path.
	CountActiveRefreshTokensByUser(ctx context.Context, userID int) (int64, error)
	ListOldestActiveRefreshTokensByUser(ctx context.Context, userID int, limit int) ([]int64, error)
	PurgeRefreshTokensOlderThan(ctx context.Context, cutoff time.Time) (int64, error)
}

// Pool is the subset of *pgxpool.Pool the Repository needs. Both
// *pgxpool.Pool and pgxmock.PgxPoolIface satisfy it (the latter is
// the unit-test path; see repository_test.go). The interface embeds
// db.DBTX so a Pool is automatically a valid argument to db.New —
// no extra adapter is needed at the call site. Mirrors
// internal/feed/repository.go's Pool so the two domains have
// identical shapes.
type Pool interface {
	db.DBTX
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
	Ping(ctx context.Context) error
	Close()
}

type sqlRepository struct {
	pool Pool        // nil for tx-bound or stub repositories
	q    *db.Queries // sqlc-generated; WithTx returns *db.Queries
}

// NewRepository is the production constructor. It wires the
// sqlc Querier to the pool so CreateRefreshToken can open a
// transaction for the two-step self-stamp (see queries/users.sql
// for why the work is split across two statements).
func NewRepository(pool Pool) Repository {
	return &sqlRepository{pool: pool, q: db.New(pool)}
}

// NewRepositoryFromQuerier is a test-only constructor that accepts
// a pre-built *db.Queries (e.g. one wired to a stubQuerier for unit
// tests). CreateRefreshToken is not usable in this mode because it
// needs the pool to begin a transaction; tests that exercise the
// CreateRefreshToken path go through NewRepository with a mock pool.
func NewRepositoryFromQuerier(q *db.Queries) Repository {
	return &sqlRepository{q: q}
}

func (r *sqlRepository) CreateUser(ctx context.Context, u *User) error {
	row, err := r.q.InsertUser(ctx, db.InsertUserParams{
		Username:     u.Username,
		PasswordHash: u.PasswordHash,
	})
	if err != nil {
		// pgerr.Map switches on ConstraintName so users_username_key
		// → ErrUsernameTaken. Other errors (network drop, schema
		// mismatch) pass through unchanged.
		return pgerr.Map(err)
	}
	converted := toUser(row)
	*u = converted
	return nil
}

func (r *sqlRepository) GetUserByUsername(ctx context.Context, username string) (*User, error) {
	row, err := r.q.GetUserByUsername(ctx, username)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrUserNotFound
		}
		return nil, err
	}
	u := toUser(row)
	return &u, nil
}

func (r *sqlRepository) GetUserByID(ctx context.Context, id int) (*User, error) {
	row, err := r.q.GetUserByID(ctx, toInt32(id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrUserNotFound
		}
		return nil, err
	}
	u := toUser(row)
	return &u, nil
}

// toUser projects a sqlc-generated db.User into the API-shaped User.
// The model differences are:
//   - int32 (db) -> int (api)
//   - pgtype.Timestamptz -> time.Time / *time.Time
//
// PasswordHash and the always-present scalars pass through unchanged.
// created_at/updated_at are NOT NULL in the schema, so we read .Time
// directly; only deleted_at needs a Valid check.
func toUser(d db.User) User {
	u := User{
		ID:           int(d.ID),
		Username:     d.Username,
		PasswordHash: d.PasswordHash,
		CreatedAt:    d.CreatedAt.Time,
		UpdatedAt:    d.UpdatedAt.Time,
	}
	if d.DeletedAt.Valid {
		t := d.DeletedAt.Time
		u.DeletedAt = &t
	}
	return u
}

// ---- Refresh-token implementations ----

// CreateRefreshToken mints a fresh refresh row with family_id stamped
// to the row's own id (a brand-new family). The work is split across
// two statements inside a transaction — see queries/users.sql for the
// full rationale; the short version is that a single-statement CTE
// pattern cannot see its own INSERT from its sibling UPDATE under
// PostgreSQL data-modifying CTE snapshot semantics, so the previous
// single-CTE implementation returned zero rows against real Postgres.
//
// pgerr.Map translates idx_refresh_tokens_token_hash collisions to
// ErrRefreshTokenCollision (500); a sha256 collision is ~10^-38 per
// row so this is defense-in-depth, not the happy path.
//
// Requires a Pool — calling CreateRefreshToken on a repository
// built via NewRepositoryFromQuerier returns an error because the
// transaction can't be opened. Production code always uses
// NewRepository.
func (r *sqlRepository) CreateRefreshToken(ctx context.Context, userID int, tokenHash []byte, expiresAt time.Time) (*RefreshTokenRow, error) {
	if r.pool == nil {
		return nil, errors.New("user.CreateRefreshToken requires a pool; use NewRepository, not NewRepositoryFromQuerier")
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, err
	}
	// Defer Rollback after a successful Commit is a no-op.
	defer func() { _ = tx.Rollback(ctx) }()

	qtx := r.q.WithTx(tx)
	id, err := qtx.InsertRefreshToken(ctx, db.InsertRefreshTokenParams{
		UserID:    toInt32(userID),
		TokenHash: tokenHash,
		ExpiresAt: pgtype.Timestamptz{Time: expiresAt, Valid: true},
	})
	if err != nil {
		return nil, pgerr.Map(err)
	}
	row, err := qtx.StampRefreshTokenFamily(ctx, id)
	if err != nil {
		return nil, pgerr.Map(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	out := toRefreshTokenRowFromStamp(row)
	return &out, nil
}

func (r *sqlRepository) GetRefreshTokenByHash(ctx context.Context, tokenHash []byte) (*RefreshTokenRow, error) {
	row, err := r.q.GetRefreshTokenByHash(ctx, tokenHash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrRefreshTokenNotFound
		}
		return nil, err
	}
	out := toRefreshTokenRow(row)
	return &out, nil
}

func (r *sqlRepository) RotateRefreshToken(ctx context.Context, oldID int64, userID int, tokenHash []byte, familyID int64, expiresAt time.Time) (*RefreshTokenRow, error) {
	row, err := r.q.RotateRefreshToken(ctx, db.RotateRefreshTokenParams{
		OldID:     oldID,
		UserID:    toInt32(userID),
		TokenHash: tokenHash,
		FamilyID:  familyID,
		ExpiresAt: pgtype.Timestamptz{Time: expiresAt, Valid: true},
	})
	if err != nil {
		return nil, err
	}
	out := toRefreshTokenRowFromRotation(row)
	return &out, nil
}

func (r *sqlRepository) RevokeRefreshTokenFamily(ctx context.Context, familyID int64) (int64, error) {
	return r.q.RevokeRefreshTokenFamily(ctx, familyID)
}

func (r *sqlRepository) RevokeRefreshTokenByID(ctx context.Context, id int64) error {
	return r.q.RevokeRefreshTokenByID(ctx, id)
}

func (r *sqlRepository) CountActiveRefreshTokensByUser(ctx context.Context, userID int) (int64, error) {
	return r.q.CountActiveRefreshTokensByUser(ctx, toInt32(userID))
}

func (r *sqlRepository) ListOldestActiveRefreshTokensByUser(ctx context.Context, userID int, limit int) ([]int64, error) {
	return r.q.ListOldestActiveRefreshTokensByUser(ctx, db.ListOldestActiveRefreshTokensByUserParams{
		UserID: toInt32(userID),
		Lim:    toInt32(limit),
	})
}

func (r *sqlRepository) PurgeRefreshTokensOlderThan(ctx context.Context, cutoff time.Time) (int64, error) {
	return r.q.PurgeRefreshTokensOlderThan(ctx, pgtype.Timestamptz{Time: cutoff, Valid: true})
}

// toRefreshTokenRow projects db.RefreshToken into the API-shaped
// RefreshTokenRow. Same conversion rules as toUser: int32 → int for
// UserID, pgtype.Timestamptz → time.Time / *time.Time for nullable
// columns. revoked_at and replaced_by_id are nullable in the schema.
func toRefreshTokenRow(d db.RefreshToken) RefreshTokenRow {
	out := RefreshTokenRow{
		ID:        d.ID,
		UserID:    int(d.UserID),
		FamilyID:  d.FamilyID,
		ExpiresAt: d.ExpiresAt.Time,
		CreatedAt: d.CreatedAt.Time,
	}
	if d.ReplacedByID.Valid {
		v := d.ReplacedByID.Int64
		out.ReplacedByID = &v
	}
	if d.RevokedAt.Valid {
		t := d.RevokedAt.Time
		out.RevokedAt = &t
	}
	return out
}

// toRefreshTokenRowFromStamp is the projection for the
// StampRefreshTokenFamilyRow that StampRefreshTokenFamily returns.
// Structurally identical to db.RefreshToken (same columns, same
// pgtype fields) — kept as a separate function so future schema
// divergence is a one-line fix.
func toRefreshTokenRowFromStamp(d db.StampRefreshTokenFamilyRow) RefreshTokenRow {
	out := RefreshTokenRow{
		ID:        d.ID,
		UserID:    int(d.UserID),
		FamilyID:  d.FamilyID,
		ExpiresAt: d.ExpiresAt.Time,
		CreatedAt: d.CreatedAt.Time,
	}
	if d.ReplacedByID.Valid {
		v := d.ReplacedByID.Int64
		out.ReplacedByID = &v
	}
	if d.RevokedAt.Valid {
		t := d.RevokedAt.Time
		out.RevokedAt = &t
	}
	return out
}

// toRefreshTokenRowFromRotation is the projection for the CTE-shaped
// RotateRefreshTokenRow that RotateRefreshToken returns. Structurally
// identical to db.RefreshToken and to CreateRefreshTokenRow — kept
// as a separate function so future schema divergence is a one-line
// fix.
func toRefreshTokenRowFromRotation(d db.RotateRefreshTokenRow) RefreshTokenRow {
	out := RefreshTokenRow{
		ID:        d.ID,
		UserID:    int(d.UserID),
		FamilyID:  d.FamilyID,
		ExpiresAt: d.ExpiresAt.Time,
		CreatedAt: d.CreatedAt.Time,
	}
	if d.ReplacedByID.Valid {
		v := d.ReplacedByID.Int64
		out.ReplacedByID = &v
	}
	if d.RevokedAt.Valid {
		t := d.RevokedAt.Time
		out.RevokedAt = &t
	}
	return out
}
