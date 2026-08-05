package user

import (
	"context"
	"errors"

	"nyx/internal/user/db"

	"github.com/jackc/pgx/v5"
)

// Sentinel errors. ErrUserNotFound is the contract the service layer
// uses to map onto ErrInvalidCredentials at the Login boundary; any
// caller that wants to distinguish "no such user" from a real DB
// failure should errors.Is against this value.
var (
	ErrUserNotFound      = errors.New("user not found")
	ErrUserAlreadyExists = errors.New("user already exists")
)

// Querier is the subset of db.Querier we actually use. Defining it
// here (rather than importing db.Querier directly) lets tests
// substitute a hand-rolled mock without pulling in the generated
// package's full surface.
type Querier interface {
	InsertUser(ctx context.Context, arg db.InsertUserParams) (db.User, error)
	GetUserByUsername(ctx context.Context, username string) (db.User, error)
	GetUserByID(ctx context.Context, id int32) (db.User, error)
}

type Repository interface {
	CreateUser(ctx context.Context, u *User) error
	GetUserByUsername(ctx context.Context, username string) (*User, error)
	GetUserByID(ctx context.Context, id int) (*User, error)
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
		return err
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
