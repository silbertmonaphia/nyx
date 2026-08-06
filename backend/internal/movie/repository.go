package movie

import (
	"context"
	"errors"
	"fmt"

	"nyx/internal/movie/db"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Page is one page of movies plus the metadata the handler needs to
// render pagination controls.
type Page struct {
	Items    []Movie
	Total    int
	Page     int
	PageSize int
}

// Pool is the subset of *pgxpool.Pool the repository needs. Both
// pgxpool.Pool and pgxmock.PgxPoolIface satisfy it (the latter is
// the unit-test path; see handler_test.go). The interface embeds
// db.DBTX so a Pool is automatically a valid argument to db.New —
// no extra adapter is needed at the call site.
type Pool interface {
	db.DBTX
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
	Ping(ctx context.Context) error
	Close()
}

// ErrNotFound is returned by Update and Delete when no row matched.
// The handler layer maps this to HTTP 404; everything else becomes
// 500. Migrated from the old `err.Error() == "movie not found"`
// string compare in handler.go.
var ErrNotFound = errors.New("movie not found")

type Repository interface {
	GetAll(ctx context.Context, query string, page, pageSize int) (*Page, error)
	Create(ctx context.Context, m *Movie) error
	Update(ctx context.Context, id int, m *Movie) error
	Delete(ctx context.Context, id int) error
	Ping(ctx context.Context) error
}

type sqlRepository struct {
	pool Pool              // nil for tx-bound or stub repositories
	q    *db.Queries       // sqlc-generated; WithTx returns *db.Queries
}

// NewRepository is the production constructor. It wires the sqlc
// Querier to the pool so GetAll can open a transaction.
func NewRepository(pool Pool) Repository {
	return &sqlRepository{pool: pool, q: db.New(pool)}
}

// NewRepositoryFromQuerier is a test-only constructor that accepts a
// pre-built *db.Queries (e.g. one bound to a transaction via
// db.New(pool).WithTx(tx)). GetAll is not usable in this mode; tests
// that exercise GetAll go through NewRepository with a mock pool.
func NewRepositoryFromQuerier(q *db.Queries) Repository {
	return &sqlRepository{q: q}
}

// GetAll returns one paginated page of movies. SELECT and COUNT run
// in a single transaction so the page count and the items stay
// consistent even under concurrent writes. The repo caller is
// responsible for clamping page/pageSize; defaults are applied
// defensively here.
//
// GetAll requires a Pool — calling it on a repository built via
// NewRepositoryFromQuerier returns an error. Production code always
// uses NewRepository.
func (r *sqlRepository) GetAll(ctx context.Context, queryParam string, page, pageSize int) (*Page, error) {
	if page < 1 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 20
	}
	offset := (page - 1) * pageSize

	// Build the optional search pattern. sqlc.narg('query') in the
	// SQL is NULL when this Valid is false, which short-circuits the
	// LIKE clauses via the `IS NULL OR ...` pattern. When the caller
	// sets it, the wildcards are the caller's responsibility (matches
	// the old sqlx-based behaviour).
	var queryArg pgtype.Text
	if queryParam != "" {
		queryArg = pgtype.Text{String: "%" + queryParam + "%", Valid: true}
	}

	if r.pool == nil {
		return nil, errors.New("movie.GetAll requires a pool; use NewRepository, not NewRepositoryFromQuerier")
	}

	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	// Defer Rollback after a successful Commit is a no-op.
	defer func() { _ = tx.Rollback(ctx) }()

	qtx := r.q.WithTx(tx)

	items, err := qtx.QueryMoviesPage(ctx, db.QueryMoviesPageParams{
		Query:    queryArg,
		Offset:   int32(offset),
		PageSize: int32(pageSize),
	})
	if err != nil {
		return nil, err
	}
	total, err := qtx.CountMovies(ctx, queryArg)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit tx: %w", err)
	}

	return &Page{
		Items:    toMovies(items),
		Total:    int(total),
		Page:     page,
		PageSize: pageSize,
	}, nil
}

func (r *sqlRepository) Create(ctx context.Context, m *Movie) error {
	row, err := r.q.InsertMovie(ctx, db.InsertMovieParams{
		Title:       m.Title,
		Description: textFromString(m.Description),
		Rating:      float8FromValue(m.Rating),
	})
	if err != nil {
		return err
	}
	*m = toMovie(row)
	return nil
}

func (r *sqlRepository) Update(ctx context.Context, id int, m *Movie) error {
	row, err := r.q.UpdateMovie(ctx, db.UpdateMovieParams{
		Title:       m.Title,
		Description: textFromString(m.Description),
		Rating:      float8FromValue(m.Rating),
		ID:          int32(id),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	*m = toMovie(row)
	return nil
}

func (r *sqlRepository) Delete(ctx context.Context, id int) error {
	rows, err := r.q.SoftDeleteMovie(ctx, int32(id))
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *sqlRepository) Ping(ctx context.Context) error {
	if r.pool == nil {
		return nil
	}
	return r.pool.Ping(ctx)
}

// toMovie projects a sqlc-generated db.Movie into the API-shaped
// Movie. The model differences are:
//   - int32 (db) -> int (api)
//   - pgtype.Text (nullable) -> string (empty when not set)
//   - pgtype.Float8 (nullable) -> float64 (zero when not set;
//     the API model uses a non-pointer rating, so NULL is lossy)
//   - pgtype.Timestamptz -> time.Time / *time.Time
func toMovie(d db.Movie) Movie {
	m := Movie{
		ID:        int(d.ID),
		Title:     d.Title,
		CreatedAt: d.CreatedAt.Time,
		UpdatedAt: d.UpdatedAt.Time,
	}
	if d.Description.Valid {
		m.Description = d.Description.String
	}
	if d.Rating.Valid {
		m.Rating = d.Rating.Float64
	}
	if d.DeletedAt.Valid {
		t := d.DeletedAt.Time
		m.DeletedAt = &t
	}
	return m
}

func toMovies(ds []db.Movie) []Movie {
	out := make([]Movie, len(ds))
	for i, d := range ds {
		out[i] = toMovie(d)
	}
	return out
}

// textFromString returns an invalid pgtype.Text for "" so the SQL
// column receives NULL rather than empty string. The DB column is
// nullable, so an empty string and NULL are distinguishable; the API
// model treats both as "no description" but storing NULL matches the
// old sqlx-based behaviour for empty-string inputs.
func textFromString(s string) pgtype.Text {
	if s == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: s, Valid: true}
}

// float8FromValue treats every rating as a SET value. The API model
// uses a non-pointer float64, so it cannot express "no rating" vs
// "rating is 0" — we always persist the value. If a future model
// uses *float64, switch to float8FromPointer and route the NULL
// case explicitly.
func float8FromValue(r float64) pgtype.Float8 {
	return pgtype.Float8{Float64: r, Valid: true}
}
