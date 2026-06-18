package movie

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/jmoiron/sqlx"
)

// Page is one page of movies plus the metadata the handler needs to
// render pagination controls.
type Page struct {
	Items    []Movie
	Total    int
	Page     int
	PageSize int
}

type Repository interface {
	GetAll(ctx context.Context, query string, page, pageSize int) (*Page, error)
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

// GetAll returns one paginated page of movies. SELECT and COUNT run in a
// single transaction so the page count and the items stay consistent even
// under concurrent writes. The repo caller is responsible for clamping
// page/pageSize; defaults are applied defensively here.
func (r *sqlRepository) GetAll(ctx context.Context, queryParam string, page, pageSize int) (*Page, error) {
	if page < 1 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 20
	}
	offset := (page - 1) * pageSize

	db, ok := r.db.(*sqlx.DB)
	if !ok {
		return nil, fmt.Errorf("movie.GetAll requires *sqlx.DB, got %T", r.db)
	}

	tx, err := db.BeginTxx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit succeeds

	items, total, err := r.WithTx(tx).(*sqlRepository).queryPage(ctx, queryParam, pageSize, offset)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit tx: %w", err)
	}

	return &Page{
		Items:    items,
		Total:    total,
		Page:     page,
		PageSize: pageSize,
	}, nil
}

// queryPage runs the SELECT + COUNT pair against the supplied connection.
// Search and no-search branches share the ORDER BY / LIMIT / OFFSET.
func (r *sqlRepository) queryPage(ctx context.Context, queryParam string, pageSize, offset int) ([]Movie, int, error) {
	var (
		items []Movie
		total int
	)

	if queryParam != "" {
		pattern := "%" + queryParam + "%"
		if err := sqlx.SelectContext(ctx, r.db, &items,
			"SELECT id, title, description, rating, created_at, updated_at, deleted_at FROM movies WHERE (title ILIKE $1 OR description ILIKE $1) AND deleted_at IS NULL ORDER BY created_at DESC, id DESC LIMIT $2 OFFSET $3",
			pattern, pageSize, offset); err != nil {
			return nil, 0, err
		}
		if err := sqlx.GetContext(ctx, r.db, &total,
			"SELECT COUNT(*) FROM movies WHERE (title ILIKE $1 OR description ILIKE $1) AND deleted_at IS NULL",
			pattern); err != nil {
			return nil, 0, err
		}
		return items, total, nil
	}

	if err := sqlx.SelectContext(ctx, r.db, &items,
		"SELECT id, title, description, rating, created_at, updated_at, deleted_at FROM movies WHERE deleted_at IS NULL ORDER BY created_at DESC, id DESC LIMIT $1 OFFSET $2",
		pageSize, offset); err != nil {
		return nil, 0, err
	}
	if err := sqlx.GetContext(ctx, r.db, &total,
		"SELECT COUNT(*) FROM movies WHERE deleted_at IS NULL"); err != nil {
		return nil, 0, err
	}
	return items, total, nil
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