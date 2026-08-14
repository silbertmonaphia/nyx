package user

import (
	"context"
	"errors"
	"time"

	"nyx/internal/user/db"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

// Sentinel errors. ErrUserNotFound is the contract the service layer
// uses to map onto ErrInvalidCredentials at the Login boundary; any
// caller that wants to distinguish "no such user" from a real DB
// failure should errors.Is against this value.
var (
	ErrUserNotFound          = errors.New("user not found")
	ErrUserAlreadyExists     = errors.New("user already exists")
	ErrRefreshTokenNotFound  = errors.New("refresh token not found")
)

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
	// so a service-layer mock can stub each one by name. The
	// CTE-shaped queries (CreateRefreshToken, RotateRefreshToken) get
	// their own generated row types; GetRefreshTokenByHash queries the
	// table directly so it gets db.RefreshToken.
	CreateRefreshToken(ctx context.Context, arg db.CreateRefreshTokenParams) (db.CreateRefreshTokenRow, error)
	GetRefreshTokenByHash(ctx context.Context, tokenHash []byte) (db.RefreshToken, error)
	RotateRefreshToken(ctx context.Context, arg db.RotateRefreshTokenParams) (db.RotateRefreshTokenRow, error)
	RevokeRefreshTokenFamily(ctx context.Context, familyID int64) (int64, error)
	RevokeRefreshTokenByID(ctx context.Context, id int64) error
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

type Repository interface {
	CreateUser(ctx context.Context, u *User) error
	GetUserByUsername(ctx context.Context, username string) (*User, error)
	GetUserByID(ctx context.Context, id int) (*User, error)

	// Refresh-token operations. CreateRefreshToken mints a row whose
	// family_id == its own id (self-stamped by the CTE). The caller is
	// responsible for hashing the opaque token before passing it in;
	// this layer never sees raw tokens.
	CreateRefreshToken(ctx context.Context, userID int, tokenHash []byte, expiresAt time.Time) (*RefreshTokenRow, error)
	GetRefreshTokenByHash(ctx context.Context, tokenHash []byte) (*RefreshTokenRow, error)
	// RotateRefreshToken inserts a new row in the same family as @oldID,
	// marks the old row revoked, and returns the new row. Concurrent
	// rotations are detected by the service layer via RevokedAt != nil
	// on the fetched old row.
	RotateRefreshToken(ctx context.Context, oldID int64, userID int, tokenHash []byte, familyID int64, expiresAt time.Time) (*RefreshTokenRow, error)
	RevokeRefreshTokenFamily(ctx context.Context, familyID int64) (int64, error)
	RevokeRefreshTokenByID(ctx context.Context, id int64) error
}

type sqlRepository struct {
	q Querier
}

func NewRepository(q Querier) Repository {
	return &sqlRepository{q: q}
}

func (r *sqlRepository) CreateUser(ctx context.Context, u *User) error {
	row, err := r.q.InsertUser(ctx, db.InsertUserParams{
		Username:     u.Username,
		Email:        u.Email,
		PasswordHash: u.PasswordHash,
	})
	if err != nil {
		// Postgres unique-constraint violation (SQLSTATE 23505) — both
		// username and email have UNIQUE indexes. Translate the
		// driver-specific error into the domain sentinel so the service
		// and handler layers stay decoupled from pgx.
		if isUniqueViolation(err) {
			return ErrUserAlreadyExists
		}
		return err
	}
	converted := toUser(row)
	*u = converted
	return nil
}

// isUniqueViolation reports whether err is a Postgres unique-constraint
// violation (SQLSTATE 23505). The repository layer is the right place to
// translate driver-specific errors into domain sentinels; service and
// handler layers stay decoupled from pgx.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	return false
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
	row, err := r.q.GetUserByID(ctx, int32(id))
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
// PasswordHash and the always-present scalars pass through unchanged.
// created_at/updated_at are NOT NULL in the schema, so we read .Time
// directly; only deleted_at needs a Valid check.
func toUser(d db.User) User {
	u := User{
		ID:           int(d.ID),
		Username:     d.Username,
		Email:        d.Email,
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

func (r *sqlRepository) CreateRefreshToken(ctx context.Context, userID int, tokenHash []byte, expiresAt time.Time) (*RefreshTokenRow, error) {
	row, err := r.q.CreateRefreshToken(ctx, db.CreateRefreshTokenParams{
		UserID:    int32(userID),
		TokenHash: tokenHash,
		ExpiresAt: pgtype.Timestamptz{Time: expiresAt, Valid: true},
	})
	if err != nil {
		return nil, err
	}
	out := toRefreshTokenRowFromCreate(row)
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
		UserID:    int32(userID),
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

// toRefreshTokenRowFromCreate is the projection for the CTE-shaped
// CreateRefreshTokenRow that CreateRefreshToken returns. Structurally
// identical to db.RefreshToken (same columns, same pgtype fields) —
// kept as a separate function so future schema divergence is a
// one-line fix.
func toRefreshTokenRowFromCreate(d db.CreateRefreshTokenRow) RefreshTokenRow {
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
