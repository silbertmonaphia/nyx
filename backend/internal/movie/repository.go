package movie

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/jmoiron/sqlx"
)

type Repository interface {
	GetAll(ctx context.Context, query string) ([]Movie, error)
	Create(ctx context.Context, m *Movie) error
	Update(ctx context.Context, id int, m *Movie) error
	Delete(ctx context.Context, id int) error
	Ping(ctx context.Context) error
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

func (r *sqlRepository) GetAll(ctx context.Context, queryParam string) ([]Movie, error) {
	var movies []Movie
	var err error

	if queryParam != "" {
		query := "SELECT id, title, description, rating, created_at, updated_at, deleted_at FROM movies WHERE (title ILIKE $1 OR description ILIKE $1) AND deleted_at IS NULL"
		err = sqlx.SelectContext(ctx, r.db, &movies, query, "%"+queryParam+"%")
	} else {
		query := "SELECT id, title, description, rating, created_at, updated_at, deleted_at FROM movies WHERE deleted_at IS NULL"
		err = sqlx.SelectContext(ctx, r.db, &movies, query)
	}

	if err != nil {
		return nil, err
	}
	return movies, nil
}

func (r *sqlRepository) Create(ctx context.Context, m *Movie) error {
	query := "INSERT INTO movies (title, description, rating) VALUES ($1, $2, $3) RETURNING id, created_at, updated_at"
	return sqlx.GetContext(ctx, r.db, m, query, m.Title, m.Description, m.Rating)
}

func (r *sqlRepository) Update(ctx context.Context, id int, m *Movie) error {
	query := "UPDATE movies SET title = $1, description = $2, rating = $3, updated_at = CURRENT_TIMESTAMP WHERE id = $4 AND deleted_at IS NULL RETURNING created_at, updated_at"
	err := sqlx.GetContext(ctx, r.db, m, query, m.Title, m.Description, m.Rating, id)
	if err == sql.ErrNoRows {
		return fmt.Errorf("movie not found")
	}
	return err
}

func (r *sqlRepository) Delete(ctx context.Context, id int) error {
	query := "UPDATE movies SET deleted_at = CURRENT_TIMESTAMP WHERE id = $1 AND deleted_at IS NULL"
	res, err := r.db.ExecContext(ctx, query, id)
	if err != nil {
		return err
	}

	rowsAffected, _ := res.RowsAffected()
	if rowsAffected == 0 {
		return fmt.Errorf("movie not found")
	}
	return nil
}

func (r *sqlRepository) Ping(ctx context.Context) error {
	if db, ok := r.db.(*sqlx.DB); ok {
		return db.PingContext(ctx)
	}
	return nil // If it's a Tx, we assume it's alive or will fail on next call
}
