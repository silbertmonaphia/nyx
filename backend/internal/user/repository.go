package user

import (
	"context"
	"database/sql"
	"errors"
)

var (
	ErrUserNotFound      = errors.New("user not found")
	ErrUserAlreadyExists = errors.New("user already exists")
)

type Repository interface {
	CreateUser(ctx context.Context, u *User) error
	GetUserByUsername(ctx context.Context, username string) (*User, error)
	GetUserByID(ctx context.Context, id int) (*User, error)
}

type sqlRepository struct {
	db *sql.DB
}

func NewRepository(db *sql.DB) Repository {
	return &sqlRepository{db: db}
}

func (r *sqlRepository) CreateUser(ctx context.Context, u *User) error {
	query := `INSERT INTO users (username, email, password_hash) VALUES ($1, $2, $3) RETURNING id, created_at, updated_at`
	err := r.db.QueryRowContext(ctx, query, u.Username, u.Email, u.PasswordHash).Scan(&u.ID, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return err
	}
	return nil
}

func (r *sqlRepository) GetUserByUsername(ctx context.Context, username string) (*User, error) {
	query := `SELECT id, username, email, password_hash, created_at, updated_at, deleted_at FROM users WHERE username = $1 AND deleted_at IS NULL`
	var u User
	err := r.db.QueryRowContext(ctx, query, username).Scan(&u.ID, &u.Username, &u.Email, &u.PasswordHash, &u.CreatedAt, &u.UpdatedAt, &u.DeletedAt)
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
	err := r.db.QueryRowContext(ctx, query, id).Scan(&u.ID, &u.Username, &u.Email, &u.PasswordHash, &u.CreatedAt, &u.UpdatedAt, &u.DeletedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrUserNotFound
		}
		return nil, err
	}
	return &u, nil
}
