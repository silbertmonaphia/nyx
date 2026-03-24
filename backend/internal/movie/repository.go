package movie

import (
	"context"
	"database/sql"
	"fmt"
)

type Repository interface {
	GetAll(ctx context.Context, query string) ([]Movie, error)
	Create(ctx context.Context, m *Movie) error
	Update(ctx context.Context, id int, m *Movie) error
	Delete(ctx context.Context, id int) error
	Ping(ctx context.Context) error
}

type sqlRepository struct {
	db *sql.DB
}

func NewRepository(db *sql.DB) Repository {
	return &sqlRepository{db: db}
}

func (r *sqlRepository) GetAll(ctx context.Context, queryParam string) ([]Movie, error) {
	var rows *sql.Rows
	var err error

	if queryParam != "" {
		query := "SELECT id, title, description, rating, created_at, updated_at, deleted_at FROM movies WHERE title ILIKE $1 AND deleted_at IS NULL"
		rows, err = r.db.QueryContext(ctx, query, "%"+queryParam+"%")
	} else {
		query := "SELECT id, title, description, rating, created_at, updated_at, deleted_at FROM movies WHERE deleted_at IS NULL"
		rows, err = r.db.QueryContext(ctx, query)
	}

	if err != nil {
		return nil, err
	}
	defer rows.Close()

	movies := []Movie{}
	for rows.Next() {
		var m Movie
		if err := rows.Scan(&m.ID, &m.Title, &m.Description, &m.Rating, &m.CreatedAt, &m.UpdatedAt, &m.DeletedAt); err != nil {
			return nil, err
		}
		movies = append(movies, m)
	}
	return movies, nil
}

func (r *sqlRepository) Create(ctx context.Context, m *Movie) error {
	query := "INSERT INTO movies (title, description, rating) VALUES ($1, $2, $3) RETURNING id, created_at, updated_at"
	return r.db.QueryRowContext(ctx, query, m.Title, m.Description, m.Rating).Scan(&m.ID, &m.CreatedAt, &m.UpdatedAt)
}

func (r *sqlRepository) Update(ctx context.Context, id int, m *Movie) error {
	query := "UPDATE movies SET title = $1, description = $2, rating = $3, updated_at = CURRENT_TIMESTAMP WHERE id = $4 AND deleted_at IS NULL RETURNING created_at, updated_at"
	err := r.db.QueryRowContext(ctx, query, m.Title, m.Description, m.Rating, id).Scan(&m.CreatedAt, &m.UpdatedAt)
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
	return r.db.PingContext(ctx)
}
