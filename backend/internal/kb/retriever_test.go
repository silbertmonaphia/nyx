package kb

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/pashagolub/pgxmock/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRetrievePassagesPinsVisibilityGenerationAndIdentity(t *testing.T) {
	mock, err := pgxmock.NewPool()
	require.NoError(t, err)
	defer mock.Close()

	rows := pgxmock.NewRows([]string{
		"id", "document_id", "source_id", "source_name", "rel_path",
		"heading_path", "content", "locator", "score",
	}).AddRow(
		int64(11), int64(7), int64(3), "shared", "B-Cook/2019-11-16-Cook-Diet.md",
		[]string{"煮食记", "碳水列表"}, "一份碳水 + 一份蛋白", []byte(`{"line":12}`), float64(0.87),
	)

	mock.ExpectQuery(`(?is)FROM kb_chunks c.*JOIN kb_documents d.*JOIN kb_sources s.*c\.embedding IS NOT NULL.*c\.generation = d\.generation.*c\.embedding_model = \$2::text.*d\.generation > 0.*s\.enabled.*c\.scope = 'shared'.*c\.owner_id = \$3::bigint.*d\.scope = 'shared'.*d\.owner_id = \$3::bigint.*d\.scope = c\.scope.*d\.owner_id IS NOT DISTINCT FROM c\.owner_id.*d\.source_id = c\.source_id.*s\.scope = c\.scope.*s\.owner_id IS NOT DISTINCT FROM c\.owner_id`).
		WithArgs(pgxmock.AnyArg(), "model-x", int64(42), pgtype.Int8{}, pgtype.Text{}, int32(5)).
		WillReturnRows(rows)

	store := NewSQLStore(mock)
	passages, err := store.RetrievePassages(context.Background(), RetrievalParams{
		Embedding:      []float32{0.1, 0.2},
		EmbeddingModel: "model-x",
		UserID:         42,
		Limit:          5,
	})
	require.NoError(t, err)
	require.Len(t, passages, 1)
	assert.Equal(t, int64(11), passages[0].ChunkID)
	assert.Equal(t, int64(7), passages[0].DocID)
	assert.Equal(t, "shared", passages[0].SourceName)
	assert.Equal(t, "B-Cook/2019-11-16-Cook-Diet.md", passages[0].RelPath)
	assert.Equal(t, []string{"煮食记", "碳水列表"}, passages[0].HeadingPath)
	assert.Equal(t, 12, passages[0].Locator.Line)
	assert.InDelta(t, 0.87, passages[0].Score, 1e-9)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestRetrievePassagesPassesOptionalFilters(t *testing.T) {
	mock, err := pgxmock.NewPool()
	require.NoError(t, err)
	defer mock.Close()

	rows := pgxmock.NewRows([]string{
		"id", "document_id", "source_id", "source_name", "rel_path",
		"heading_path", "content", "locator", "score",
	})
	mock.ExpectQuery(`(?is)starts_with\(d\.rel_path, \$5::text\).*LIMIT \$6::int`).
		WithArgs(pgxmock.AnyArg(), "model-y", int64(9), pgtype.Int8{Int64: 3, Valid: true}, pgtype.Text{String: "B-Sport/", Valid: true}, int32(2)).
		WillReturnRows(rows)

	store := NewSQLStore(mock)
	sourceID := int64(3)
	passages, err := store.RetrievePassages(context.Background(), RetrievalParams{
		Embedding:      []float32{0.1},
		EmbeddingModel: "model-y",
		UserID:         9,
		SourceID:       &sourceID,
		PathPrefix:     "B-Sport/",
		Limit:          2,
	})
	require.NoError(t, err)
	assert.Empty(t, passages)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestRetrievePassagesPropagatesSQLError(t *testing.T) {
	mock, err := pgxmock.NewPool()
	require.NoError(t, err)
	defer mock.Close()

	mock.ExpectQuery(`SELECT`).WillReturnError(assert.AnError)
	store := NewSQLStore(mock)
	_, err = store.RetrievePassages(context.Background(), RetrievalParams{
		Embedding:      []float32{0.1},
		EmbeddingModel: "model",
		UserID:         1,
		Limit:          5,
	})
	require.Error(t, err)
}
