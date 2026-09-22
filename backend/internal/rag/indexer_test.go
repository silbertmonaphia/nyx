package rag

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace/noop"

	"nyx/internal/feed"
)

// TestIndexer_Index_HappyPath pins the create/update path: the
// pre-flight SELECT returns no row (pgx.ErrNoRows is not an
// error), the embedder is called, and UpsertFeedEmbedding is
// issued with the right params.
func TestIndexer_Index_HappyPath(t *testing.T) {
	mock, err := pgxmock.NewPool()
	require.NoError(t, err)
	defer mock.Close()

	mock.ExpectQuery(`SELECT content_hash FROM feed_embeddings WHERE feed_id`).
		WithArgs(int32(42)).
		WillReturnError(pgx.ErrNoRows)
	mock.ExpectExec(`INSERT INTO feed_embeddings`).
		WithArgs(int32(42), int64(7), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	embedder := NewEmbedder(&stubProvider{vecs: [][]float32{{0.1, 0.2, 0.3}}}, "m", noop.NewTracerProvider().Tracer("test"))
	idx := NewIndexer(mock, embedder, noop.NewTracerProvider().Tracer("test"))

	f := &feed.Feed{ID: 42, UserID: 7, Title: "The Matrix", Description: "sci-fi"}
	err = idx.Index(context.Background(), 7, f)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestIndexer_Index_SkipsNoOp pins the no-op-skip path: the
// pre-flight SELECT returns a row whose content_hash equals the
// freshly computed hash, so Embed is NOT called and Upsert is
// NOT issued. A no-op UpdateFeed (debounced auto-save with
// identical payload) costs one sha256 and zero API calls.
func TestIndexer_Index_SkipsNoOp(t *testing.T) {
	mock, err := pgxmock.NewPool()
	require.NoError(t, err)
	defer mock.Close()

	f := &feed.Feed{ID: 42, Title: "The Matrix", Description: "sci-fi"}
	storedHash := HashChunk(f) // what would already be in the DB
	mock.ExpectQuery(`SELECT content_hash FROM feed_embeddings WHERE feed_id`).
		WithArgs(int32(42)).
		WillReturnRows(pgxmock.NewRows([]string{"content_hash"}).AddRow(storedHash))
	// NO ExpectExec — the indexer must short-circuit before
	// issuing the upsert.

	embedder := NewEmbedder(&stubProvider{vecs: [][]float32{{0.1, 0.2, 0.3}}}, "m", noop.NewTracerProvider().Tracer("test"))
	idx := NewIndexer(mock, embedder, noop.NewTracerProvider().Tracer("test"))

	err = idx.Index(context.Background(), 7, f)
	require.NoError(t, err)
	// The embedder's EmbedFn is the only way to know whether the
	// API was called — the stub's embedCalls counter.
	assert.Equal(t, 0, embedder.provider.(*stubProvider).embedCalls,
		"no-op update must skip the embedding API call")
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestIndexer_Index_PropagatesEmbedError pins that an embedder
// error surfaces from Index. The caller (feed service) logs +
// continues; the test pins the contract.
func TestIndexer_Index_PropagatesEmbedError(t *testing.T) {
	mock, err := pgxmock.NewPool()
	require.NoError(t, err)
	defer mock.Close()

	mock.ExpectQuery(`SELECT content_hash FROM feed_embeddings WHERE feed_id`).
		WithArgs(int32(42)).
		WillReturnError(pgx.ErrNoRows)

	embedder := NewEmbedder(&stubProvider{err: errors.New("embedding provider down")}, "m", noop.NewTracerProvider().Tracer("test"))
	idx := NewIndexer(mock, embedder, noop.NewTracerProvider().Tracer("test"))

	err = idx.Index(context.Background(), 7, &feed.Feed{ID: 42, Title: "x"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "embedding provider down")
}

// TestIndexer_Index_PropagatesSelectError pins that a DB error
// during the pre-flight hash SELECT surfaces from Index.
func TestIndexer_Index_PropagatesSelectError(t *testing.T) {
	mock, err := pgxmock.NewPool()
	require.NoError(t, err)
	defer mock.Close()

	mock.ExpectQuery(`SELECT content_hash FROM feed_embeddings WHERE feed_id`).
		WithArgs(int32(42)).
		WillReturnError(assert.AnError)

	embedder := NewEmbedder(&stubProvider{}, "m", noop.NewTracerProvider().Tracer("test"))
	idx := NewIndexer(mock, embedder, noop.NewTracerProvider().Tracer("test"))

	err = idx.Index(context.Background(), 7, &feed.Feed{ID: 42, Title: "x"})
	require.Error(t, err)
}

// TestIndexer_Delete pins the soft-delete path: one DELETE is
// issued with the right feed_id.
func TestIndexer_Delete(t *testing.T) {
	mock, err := pgxmock.NewPool()
	require.NoError(t, err)
	defer mock.Close()

	mock.ExpectExec(`DELETE FROM feed_embeddings WHERE feed_id`).
		WithArgs(int32(42)).
		WillReturnResult(pgxmock.NewResult("DELETE", 1))

	embedder := NewEmbedder(&stubProvider{}, "m", noop.NewTracerProvider().Tracer("test"))
	idx := NewIndexer(mock, embedder, noop.NewTracerProvider().Tracer("test"))

	require.NoError(t, idx.Delete(context.Background(), 42))
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestIndexer_Delete_PropagatesError pins that a DELETE error
// surfaces from Delete so the caller (feed service) can log.
func TestIndexer_Delete_PropagatesError(t *testing.T) {
	mock, err := pgxmock.NewPool()
	require.NoError(t, err)
	defer mock.Close()

	mock.ExpectExec(`DELETE FROM feed_embeddings WHERE feed_id`).
		WithArgs(int32(42)).
		WillReturnError(assert.AnError)

	embedder := NewEmbedder(&stubProvider{}, "m", noop.NewTracerProvider().Tracer("test"))
	idx := NewIndexer(mock, embedder, noop.NewTracerProvider().Tracer("test"))

	err = idx.Delete(context.Background(), 42)
	require.Error(t, err)
}