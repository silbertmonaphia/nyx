package user

import (
	"context"
	"database/sql"
	"errors"

	"github.com/jmoiron/sqlx"
)

var (
	ErrUserNotFound      = errors.New("user not found")
	ErrUserAlreadyExists = errors.New("user already exists")
)

type Repository interface {
	CreateUser(ctx context.Context, u *User) error
	GetUserByUsername(ctx context.Context, username string) (*User, error)
	GetUserByID(ctx context.Context, id int) (*User, error)
	WithTx(tx *sqlx.Tx) Repository
}

type sqlRepository struct {
	db sqlx.ExtContext
}

func NewRepository(db *sqlx.DB) Repository {
	return &sqlRepository{db: db}
}

func (r *sqlRepository) WithTx(tx *sqlx.Tx) Repository {
	return &sqlRepository{db: tx}
}

func (r *sqlRepository) CreateUser(ctx context.Context, u *User) error {
	query := `INSERT INTO users (username, email, password_hash) VALUES ($1, $2, $3) RETURNING id, created_at, updated_at`
	err := sqlx.GetContext(ctx, r.db, u, query, u.Username, u.Email, u.PasswordHash)
	if err != nil {
		return err
	}
	return nil
}

func (r *sqlRepository) GetUserByUsername(ctx context.Context, username string) (*User, error) {
	query := `SELECT id, username, email, password_hash, created_at, updated_at, deleted_at FROM users WHERE username = $1 AND deleted_at IS NULL`
	var u User
	err := sqlx.GetContext(ctx, r.db, &u, query, username)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrUserNotFound
		}
		return nil, err
	}
	return &u, nil
}

func (r *sqlRepository) GetUserByID(ctx context.Context, id int) (*User, error) {
	query := `SELECT id, username, email, password_hash, created_at, updated_at, deleted_at FROM users WHERE id = $1 AND deleted_at IS NULL`
	var u User
	err := sqlx.GetContext(ctx, r.db, &u, query, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrUserNotFound
		}
		return nil, err
	}
	return &u, nil
}
