package rag

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/pashagolub/pgxmock/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace/noop"
)

// TestRetriever_Retrieve_HappyPath pins the happy path: pgxmock
// returns two passages; the retriever surfaces them in the
// score-sorted order the SQL promises.
func TestRetriever_Retrieve_HappyPath(t *testing.T) {
	mock, err := pgxmock.NewPool()
	require.NoError(t, err)
	defer mock.Close()

	descA := pgtype.Text{String: "a sci-fi film", Valid: true}
	descB := pgtype.Text{String: "romantic comedy", Valid: true}
	rows := pgxmock.NewRows([]string{"feed_id", "title", "description", "score"}).
		AddRow(int32(1), "The Matrix", descA, float64(0.93)).
		AddRow(int32(2), "Pretty Woman", descB, float64(0.71))

	// pgxmock regex-matches the query string. Including the two
	// user_id filters + deleted_at IS NULL + <=> in the expected
	// pattern is the cross-user-leak canary: a future refactor
	// that drops any of these clauses makes the regex NOT match
	// and pgxmock fails the test.
	mock.ExpectQuery(`(?is)SELECT.*fe\.user_id\s*=\s*\$2.*f\.user_id\s*=\s*\$2.*deleted_at\s+IS\s+NULL.*<=>`).
		WithArgs(pgxmock.AnyArg(), int64(42), int32(5)).
		WillReturnRows(rows)

	r := NewRetriever(mock, noop.NewTracerProvider().Tracer("test"))
	passages, err := r.Retrieve(context.Background(), 42, []float32{0.1, 0.2, 0.3}, 5)
	require.NoError(t, err)
	require.Len(t, passages, 2)

	assert.Equal(t, 1, passages[0].FeedID)
	assert.Equal(t, "The Matrix", passages[0].Title)
	assert.Equal(t, "a sci-fi film", passages[0].Description)
	assert.InDelta(t, 0.93, passages[0].Score, 1e-9)

	assert.Equal(t, 2, passages[1].FeedID)
	assert.Equal(t, "Pretty Woman", passages[1].Title)
	assert.Equal(t, "romantic comedy", passages[1].Description)

	// assert all expectations were met (no extra SQL ran).
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestRetriever_Retrieve_CrossUserCanary is the same regex
// pinned at the assertion level so a future refactor that
// matches the broader happy-path pattern but accidentally drops
// a user_id filter still trips this test.
func TestRetriever_Retrieve_CrossUserCanary(t *testing.T) {
	mock, err := pgxmock.NewPool()
	require.NoError(t, err)
	defer mock.Close()

	rows := pgxmock.NewRows([]string{"feed_id", "title", "description", "score"})
	mock.ExpectQuery(`(?is)SELECT.*FROM feed_embeddings fe.*JOIN feeds f.*fe\.user_id\s*=\s*\$2.*f\.user_id\s*=\s*\$2.*f\.deleted_at\s+IS\s+NULL.*ORDER BY fe\.embedding\s*<=>`).
		WithArgs(pgxmock.AnyArg(), int64(42), int32(5)).
		WillReturnRows(rows)

	r := NewRetriever(mock, noop.NewTracerProvider().Tracer("test"))
	_, err = r.Retrieve(context.Background(), 42, []float32{0.1}, 5)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestRetriever_Retrieve_NoResultsIsNotError pins the contract
// that an empty result set is normal — the chat service skips
// context injection when passages is empty.
func TestRetriever_Retrieve_NoResultsIsNotError(t *testing.T) {
	mock, err := pgxmock.NewPool()
	require.NoError(t, err)
	defer mock.Close()

	rows := pgxmock.NewRows([]string{"feed_id", "title", "description", "score"})
	mock.ExpectQuery(`SELECT`).
		WithArgs(pgxmock.AnyArg(), int64(42), int32(5)).
		WillReturnRows(rows)

	r := NewRetriever(mock, noop.NewTracerProvider().Tracer("test"))
	passages, err := r.Retrieve(context.Background(), 42, []float32{0.1}, 5)
	require.NoError(t, err)
	assert.Empty(t, passages)
}

// TestRetriever_Retrieve_ZeroKIsNoop pins that k <= 0
// short-circuits without issuing SQL. pgxmock has no expectations
// registered; an accidental call would fail the test.
func TestRetriever_Retrieve_ZeroKIsNoop(t *testing.T) {
	mock, err := pgxmock.NewPool()
	require.NoError(t, err)
	defer mock.Close()

	r := NewRetriever(mock, noop.NewTracerProvider().Tracer("test"))
	passages, err := r.Retrieve(context.Background(), 42, []float32{0.1}, 0)
	require.NoError(t, err)
	assert.Empty(t, passages)

	passages, err = r.Retrieve(context.Background(), 42, []float32{0.1}, -1)
	require.NoError(t, err)
	assert.Empty(t, passages)
}

// TestRetriever_Retrieve_EmptyQueryIsNoop pins that an empty
// query vector short-circuits. rag.Service.Retrieve's empty-string
// guard makes this defensive.
func TestRetriever_Retrieve_EmptyQueryIsNoop(t *testing.T) {
	mock, err := pgxmock.NewPool()
	require.NoError(t, err)
	defer mock.Close()

	r := NewRetriever(mock, noop.NewTracerProvider().Tracer("test"))
	passages, err := r.Retrieve(context.Background(), 42, nil, 5)
	require.NoError(t, err)
	assert.Empty(t, passages)
}

// TestRetriever_Retrieve_PropagatesSQLError pins that a DB error
// surfaces from Retrieve unchanged so the chat service can log
// and skip context injection rather than fail the chat.
func TestRetriever_Retrieve_PropagatesSQLError(t *testing.T) {
	mock, err := pgxmock.NewPool()
	require.NoError(t, err)
	defer mock.Close()

	mock.ExpectQuery(`SELECT`).
		WithArgs(pgxmock.AnyArg(), int64(42), int32(5)).
		WillReturnError(assert.AnError)

	r := NewRetriever(mock, noop.NewTracerProvider().Tracer("test"))
	_, err = r.Retrieve(context.Background(), 42, []float32{0.1}, 5)
	require.Error(t, err)
}