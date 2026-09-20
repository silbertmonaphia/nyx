package feed

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"nyx/internal/feed/db"
	"nyx/internal/platform/api"
	"nyx/internal/platform/pgerr"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Page is one page of feeds plus the metadata the handler needs to
// render pagination controls.
type Page struct {
	Items    []Feed
	Total    int
	Page     int
	PageSize int
}

// toInt32 narrows an int to int32. Feed rows use SERIAL PKs and
// pagination is hard-capped at 100 by the handler, so offset/pageSize
// and feed IDs are well below math.MaxInt32. If a hostile client
// somehow passed values above that, pgx would either wrap to a
// negative int32 (no rows match) or fail with a value-out-of-range
// SQLSTATE.
//
//nolint:gosec // G115: SERIAL PKs + handler-side caps keep this safe.
func toInt32(v int) int32 { return int32(v) }

// toInt64 narrows an int to int64. The feeds.user_id column is
// BIGINT (matches users.id SERIAL — both stay below math.MaxInt64 for
// any realistic user count). If a hostile client somehow sent a
// value above that, pgx would fail with a value-out-of-range SQLSTATE.
//
//nolint:gosec // G115: user_id comes from the JWT subject, capped by SERIAL.
func toInt64(v int) int64 { return int64(v) }

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
// 500. Migrated from the old `err.Error() == "feed not found"`
// string compare in handler.go.
//
// Now also covers the cross-owner case: a PUT/DELETE on a feed that
// exists but belongs to a different user returns ErrNotFound too —
// the SQL WHERE filters by both id and user_id, so a non-owner
// caller sees the same 0-rows outcome as a missing id. This is the
// leak-free path the product spec mandates (single sentinel, no
// ErrForbidden).
var ErrNotFound = errors.New("feed not found")

// SortOrder is the listing direction for paginated feed queries.
// Desc is the default (newest first); Asc flips to oldest first.
// The wire contract is the lowercase string values below.
type SortOrder string

const (
	SortDesc SortOrder = "desc"
	SortAsc  SortOrder = "asc"
)

// Sort returns the SortOrder for an unknown input, falling back to
// the safe default (Desc). The handler uses this to coerce a free-form
// `?order=` query parameter without 400ing on a typo.
func ParseSortOrder(s string) SortOrder {
	switch SortOrder(s) {
	case SortAsc:
		return SortAsc
	default:
		return SortDesc
	}
}

// Register the feed-domain sentinel with api.MapError. The
// repository is the only place that owns ErrNotFound — the handler
// simply funnels every error through api.MapError.
func init() {
	api.RegisterSentinel(ErrNotFound, http.StatusNotFound, "Feed not found")
}

type Repository interface {
	GetAll(ctx context.Context, userID int, query string, page, pageSize int, order SortOrder) (*Page, error)
	Create(ctx context.Context, userID int, m *Feed) error
	Update(ctx context.Context, userID int, id int, m *Feed) error
	Delete(ctx context.Context, userID int, id int) error
	Ping(ctx context.Context) error
}

type sqlRepository struct {
	pool Pool        // nil for tx-bound or stub repositories
	q    *db.Queries // sqlc-generated; WithTx returns *db.Queries
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

// GetAll returns one paginated page of feeds owned by userID. SELECT
// and COUNT run in a single transaction so the page count and the
// items stay consistent even under concurrent writes. The repo
// caller is responsible for clamping page/pageSize; defaults are
// applied defensively here.
//
// GetAll requires a Pool — calling it on a repository built via
// NewRepositoryFromQuerier returns an error. Production code always
// uses NewRepository.
func (r *sqlRepository) GetAll(ctx context.Context, userID int, queryParam string, page, pageSize int, order SortOrder) (*Page, error) {
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
		return nil, errors.New("feed.GetAll requires a pool; use NewRepository, not NewRepositoryFromQuerier")
	}

	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	// Defer Rollback after a successful Commit is a no-op.
	defer func() { _ = tx.Rollback(ctx) }()

	qtx := r.q.WithTx(tx)

	// Two sqlc queries keep the ORDER BY literal (sqlc doesn't
	// interpolate direction tokens); pick the matching one. Any
	// unknown SortOrder falls through to DESC via ParseSortOrder at
	// the handler boundary, so this branch only sees ASC or DESC.
	userIDParam := toInt64(userID)
	var feeds []Feed
	switch order {
	case SortAsc:
		rows, qErr := qtx.QueryFeedsPageAsc(ctx, db.QueryFeedsPageAscParams{
			Query:    queryArg,
			UserID:   userIDParam,
			Offset:   toInt32(offset),
			PageSize: toInt32(pageSize),
		})
		if qErr != nil {
			return nil, qErr
		}
		feeds = toFeedsFromAscRows(rows)
	default:
		rows, qErr := qtx.QueryFeedsPage(ctx, db.QueryFeedsPageParams{
			Query:    queryArg,
			UserID:   userIDParam,
			Offset:   toInt32(offset),
			PageSize: toInt32(pageSize),
		})
		if qErr != nil {
			return nil, qErr
		}
		feeds = toFeeds(rows)
	}
	total, err := qtx.CountFeeds(ctx, db.CountFeedsParams{
		Query:  queryArg,
		UserID: userIDParam,
	})
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit tx: %w", err)
	}

	return &Page{
		Items:    feeds,
		Total:    int(total),
		Page:     page,
		PageSize: pageSize,
	}, nil
}

func (r *sqlRepository) Create(ctx context.Context, userID int, m *Feed) error {
	// Defense-in-depth: the handler always sets UserID from the JWT
	// subject, but pin it here too so a malformed Feed can never
	// persist as another user's feed.
	m.UserID = userID
	row, err := r.q.InsertFeed(ctx, db.InsertFeedParams{
		UserID:      toInt64(userID),
		Title:       m.Title,
		Description: textFromString(m.Description),
		Rating:      float8FromValue(m.Rating),
	})
	if err != nil {
		// pgerr.Map is a no-op for errors it doesn't recognize; today
		// the feeds table has no unique/FK/CHECK constraints so
		// every SQL error passes through unchanged. Wiring it
		// preemptively means new constraints added in future
		// migrations get translated for free.
		return pgerr.Map(err)
	}
	*m = toFeed(row)
	return nil
}

func (r *sqlRepository) Update(ctx context.Context, userID int, id int, m *Feed) error {
	row, err := r.q.UpdateFeed(ctx, db.UpdateFeedParams{
		Title:       m.Title,
		Description: textFromString(m.Description),
		Rating:      float8FromValue(m.Rating),
		ID:          toInt32(id),
		UserID:      toInt64(userID),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Either the row doesn't exist, is soft-deleted, OR is
			// owned by another user. All three map to ErrNotFound —
			// see the type comment. No ErrForbidden sentinel by design.
			return ErrNotFound
		}
		return pgerr.Map(err)
	}
	*m = toFeed(row)
	return nil
}

func (r *sqlRepository) Delete(ctx context.Context, userID int, id int) error {
	rows, err := r.q.SoftDeleteFeed(ctx, db.SoftDeleteFeedParams{
		ID:     toInt32(id),
		UserID: toInt64(userID),
	})
	if err != nil {
		return pgerr.Map(err)
	}
	if rows == 0 {
		// Same shape as Update: missing, soft-deleted, OR owned by
		// another user. Single ErrNotFound sentinel for all three.
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

// toFeed projects a sqlc-generated db.Feed into the API-shaped
// Feed. The model differences are:
//   - int32 (db) -> int (api)
//   - int64 (db) -> int (api)
//   - pgtype.Text (nullable) -> string (empty when not set)
//   - pgtype.Float8 (nullable) -> float64 (zero when not set;
//     the API model uses a non-pointer rating, so NULL is lossy)
//   - pgtype.Timestamptz -> time.Time / *time.Time
func toFeed(d db.Feed) Feed {
	m := Feed{
		ID:        int(d.ID),
		UserID:    int(d.UserID),
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

// toFeedsRow projects a sqlc-generated *QueryFeedsPageRow /
// *QueryFeedsPageAscRow into the API-shaped Feed. The page queries
// return dedicated Row types (not the shared db.Feed) because sqlc
// can't widen RETURNING * automatically — but the columns are
// identical, so the projection is the same shape.
func toFeedsRow(d db.QueryFeedsPageRow) Feed {
	m := Feed{
		ID:        int(d.ID),
		UserID:    int(d.UserID),
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

func toFeeds(ds []db.QueryFeedsPageRow) []Feed {
	out := make([]Feed, len(ds))
	for i, d := range ds {
		out[i] = toFeedsRow(d)
	}
	return out
}

// toFeedsFromAscRows is the ascending-query counterpart to
// toFeeds. The two page queries return distinct sqlc row types with
// identical fields; rather than introduce an interface (sqlc
// generates them as plain structs), we duplicate the projection in
// a sibling helper. The two should stay in lock-step — a column
// added to the SELECT list will fail to compile in both row types
// and force a fix here.
func toFeedsFromAscRows(ds []db.QueryFeedsPageAscRow) []Feed {
	out := make([]Feed, len(ds))
	for i, d := range ds {
		m := Feed{
			ID:        int(d.ID),
			UserID:    int(d.UserID),
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
		out[i] = m
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
