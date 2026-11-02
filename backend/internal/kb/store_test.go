package kb

import (
	"context"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	sourceColumns = []string{
		"id", "scope", "owner_id", "name", "kind", "root_path", "enabled",
		"last_scan_at", "last_error", "created_at", "updated_at",
	}
	documentColumns = []string{
		"id", "source_id", "scope", "owner_id", "origin", "rel_path", "title",
		"doc_type", "content_hash", "byte_size", "mtime", "status",
		"error_detail", "generation", "content", "created_at", "updated_at",
	}
	claimColumns = []string{
		"id", "document_id", "scope", "owner_id", "source_id", "generation",
		"ordinal", "heading_path", "content", "embed_text", "content_hash",
		"embedding_model", "token_count", "locator", "created_at",
	}
	retrievalColumns = []string{
		"id", "document_id", "source_id", "source_name", "rel_path",
		"heading_path", "content", "locator", "score",
	}
)

func anyArgs(n int) []any {
	out := make([]any, n)
	for i := range out {
		out[i] = pgxmock.AnyArg()
	}
	return out
}

func sourceRow() []any {
	now := time.Now()
	return []any{
		int64(1), ScopeShared, nil, "shared", KindDir, "/srv/kb/shared",
		true, now, "", now, now,
	}
}

func documentRow() []any {
	now := time.Now()
	return []any{
		int64(7), int64(1), ScopeShared, nil, OriginFile, "B-Cook/diet.md",
		"diet", DocTypeMarkdown, []byte("hash"), int64(12), nil,
		StatusIndexed, "", int32(1), nil, now, now,
	}
}

func TestSQLStoreSourceAndDocumentMethods(t *testing.T) {
	mock, err := pgxmock.NewPool()
	require.NoError(t, err)
	defer mock.Close()
	store := NewSQLStore(mock)
	ctx := context.Background()

	mock.ExpectQuery(`FROM kb_sources WHERE id`).WithArgs(anyArgs(1)...).
		WillReturnRows(pgxmock.NewRows(sourceColumns).AddRow(sourceRow()...))
	src, err := store.GetSource(ctx, 1)
	require.NoError(t, err)
	assert.Equal(t, "shared", src.Name)
	assert.Equal(t, KindDir, src.Kind)

	mock.ExpectQuery(`FROM kb_sources.*WHERE enabled`).WithArgs(anyArgs(1)...).
		WillReturnRows(pgxmock.NewRows(sourceColumns).AddRow(sourceRow()...))
	visible, err := store.ListVisibleSources(ctx, 1)
	require.NoError(t, err)
	assert.Len(t, visible, 1)

	mock.ExpectQuery(`SELECT .* FROM kb_sources ORDER BY scope`).WithArgs(anyArgs(0)...).
		WillReturnRows(pgxmock.NewRows(sourceColumns).AddRow(sourceRow()...))
	all, err := store.ListAllSources(ctx)
	require.NoError(t, err)
	assert.Len(t, all, 1)

	mock.ExpectQuery(`WHERE scope = 'user' AND owner_id`).WithArgs(anyArgs(1)...).
		WillReturnRows(pgxmock.NewRows(sourceColumns).AddRow(sourceRow()...))
	owned, err := store.ListSourcesByOwner(ctx, 1)
	require.NoError(t, err)
	assert.Len(t, owned, 1)

	mock.ExpectQuery(`INSERT INTO kb_sources`).WithArgs(anyArgs(2)...).
		WillReturnRows(pgxmock.NewRows(sourceColumns).AddRow(sourceRow()...))
	_, err = store.UpsertSharedSource(ctx, "shared", "/srv/kb/shared")
	require.NoError(t, err)

	mock.ExpectQuery(`INSERT INTO kb_sources`).WithArgs(anyArgs(3)...).
		WillReturnRows(pgxmock.NewRows(sourceColumns).AddRow(sourceRow()...))
	_, err = store.UpsertUserDirSource(ctx, 1, "mine", "/srv/kb/users/1")
	require.NoError(t, err)

	mock.ExpectQuery(`INSERT INTO kb_sources`).WithArgs(anyArgs(1)...).
		WillReturnRows(pgxmock.NewRows(sourceColumns).AddRow(sourceRow()...))
	_, err = store.EnsureInlineSource(ctx, 1)
	require.NoError(t, err)

	mock.ExpectQuery(`UPDATE kb_sources`).WithArgs(anyArgs(3)...).
		WillReturnRows(pgxmock.NewRows(sourceColumns).AddRow(sourceRow()...))
	_, err = store.SetSourceEnabled(ctx, 1, 1, false)
	require.NoError(t, err)

	mock.ExpectExec(`DELETE FROM kb_sources`).WithArgs(anyArgs(2)...).WillReturnResult(pgxmock.NewResult("DELETE", 1))
	removed, err := store.DeleteUserSource(ctx, 1, 1)
	require.NoError(t, err)
	assert.Equal(t, int64(1), removed)

	mock.ExpectExec(`UPDATE kb_sources`).WithArgs(anyArgs(2)...).WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	require.NoError(t, store.UpdateSourceScan(ctx, 1, ""))

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM kb_documents WHERE source_id`).WithArgs(anyArgs(1)...).
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(int64(3)))
	mock.ExpectQuery(`SELECT COUNT\(\*\).*FROM kb_chunks c`).WithArgs(anyArgs(1)...).
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(int64(9)))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM kb_documents WHERE source_id.*status`).WithArgs(anyArgs(1)...).
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(int64(1)))
	stats, err := store.SourceStats(ctx, 1)
	require.NoError(t, err)
	assert.Equal(t, SourceStats{Documents: 3, Chunks: 9, Pending: 1}, stats)

	mock.ExpectQuery(`SELECT DISTINCT split_part`).WithArgs(anyArgs(1)...).
		WillReturnRows(pgxmock.NewRows([]string{"directory"}).AddRow("B-Cook"))
	dirs, err := store.ListTopLevelDirs(ctx, 1)
	require.NoError(t, err)
	assert.Equal(t, []string{"B-Cook"}, dirs)

	mock.ExpectQuery(`SELECT DISTINCT embedding_model`).WithArgs(anyArgs(1)...).
		WillReturnRows(pgxmock.NewRows([]string{"embedding_model"}).AddRow("model-x"))
	models, err := store.SourceEmbeddingModels(ctx, 1)
	require.NoError(t, err)
	assert.Equal(t, []string{"model-x"}, models)

	mock.ExpectQuery(`FROM kb_documents WHERE id = \$1`).WithArgs(anyArgs(1)...).
		WillReturnRows(pgxmock.NewRows(documentColumns).AddRow(documentRow()...))
	doc, err := store.GetDocument(ctx, 7)
	require.NoError(t, err)
	assert.Equal(t, "B-Cook/diet.md", doc.RelPath)

	mock.ExpectQuery(`FROM kb_documents.*scope = 'shared' OR`).WithArgs(anyArgs(2)...).
		WillReturnRows(pgxmock.NewRows(documentColumns).AddRow(documentRow()...))
	_, err = store.GetVisibleDocument(ctx, 7, 1)
	require.NoError(t, err)

	mock.ExpectQuery(`scope = 'user' AND owner_id`).WithArgs(anyArgs(2)...).
		WillReturnRows(pgxmock.NewRows(documentColumns).AddRow(documentRow()...))
	_, err = store.GetOwnedDocument(ctx, 7, 1)
	require.NoError(t, err)

	mock.ExpectQuery(`FROM kb_documents WHERE source_id = \$1 AND rel_path`).WithArgs(anyArgs(2)...).
		WillReturnRows(pgxmock.NewRows(documentColumns).AddRow(documentRow()...))
	_, err = store.GetDocumentBySourcePath(ctx, 1, "B-Cook/diet.md")
	require.NoError(t, err)

	mock.ExpectQuery(`origin = 'inline' AND owner_id`).WithArgs(anyArgs(2)...).
		WillReturnRows(pgxmock.NewRows(documentColumns).AddRow(documentRow()...))
	_, err = store.GetInlineDocumentByHash(ctx, 1, []byte("hash"))
	require.NoError(t, err)

	mock.ExpectQuery(`FROM kb_documents d.*WHERE \(d.scope`).WithArgs(anyArgs(6)...).
		WillReturnRows(pgxmock.NewRows(documentColumns).AddRow(documentRow()...))
	docs, err := store.ListVisibleDocuments(ctx, 1, nil, "", "", 10, 0)
	require.NoError(t, err)
	assert.Len(t, docs, 1)

	mock.ExpectQuery(`FROM kb_documents.*ORDER BY rel_path`).WithArgs(anyArgs(3)...).
		WillReturnRows(pgxmock.NewRows(documentColumns).AddRow(documentRow()...))
	docs, err = store.ListDocumentsBySource(ctx, 1, 10, 0)
	require.NoError(t, err)
	assert.Len(t, docs, 1)

	mock.ExpectQuery(`FROM kb_documents d.*status = 'pending'`).WithArgs(anyArgs(2)...).
		WillReturnRows(pgxmock.NewRows(documentColumns).AddRow(documentRow()...))
	docs, err = store.ListPendingDocuments(ctx, 1, 10)
	require.NoError(t, err)
	assert.Len(t, docs, 1)

	mock.ExpectQuery(`SELECT DISTINCT d\.id`).WithArgs(anyArgs(2)...).
		WillReturnRows(pgxmock.NewRows(documentColumns).AddRow(documentRow()...))
	docs, err = store.ListDocumentsWithUnembedded(ctx, 1, 10)
	require.NoError(t, err)
	assert.Len(t, docs, 1)

	mock.ExpectQuery(`INSERT INTO kb_documents`).WithArgs(anyArgs(9)...).
		WillReturnRows(pgxmock.NewRows(documentColumns).AddRow(documentRow()...))
	_, err = store.UpsertFileDocument(ctx, Document{SourceID: 1, Scope: ScopeShared, RelPath: "a.md"})
	require.NoError(t, err)

	mock.ExpectQuery(`INSERT INTO kb_documents`).WithArgs(anyArgs(7)...).
		WillReturnRows(pgxmock.NewRows(documentColumns).AddRow(documentRow()...))
	_, err = store.UpsertInlineDocument(ctx, Document{SourceID: 1, OwnerID: 1, RelPath: "a.md"})
	require.NoError(t, err)

	mock.ExpectQuery(`UPDATE kb_documents`).WithArgs(anyArgs(4)...).
		WillReturnRows(pgxmock.NewRows(documentColumns).AddRow(documentRow()...))
	_, err = store.UpdateInlineDocumentContent(ctx, 7, "body", []byte("hash"), 4)
	require.NoError(t, err)

	mock.ExpectExec(`UPDATE kb_documents`).WithArgs(anyArgs(3)...).WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	require.NoError(t, store.MarkDocumentStatus(ctx, 7, StatusFailed, "reason"))

	mock.ExpectExec(`UPDATE kb_documents`).WithArgs(anyArgs(3)...).WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	require.NoError(t, store.MarkDocumentPending(ctx, 7, 2, "embedding unavailable"))

	mock.ExpectExec(`UPDATE kb_documents`).WithArgs(anyArgs(2)...).WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	require.NoError(t, store.SetDocumentTitle(ctx, 7, "title"))

	mock.ExpectQuery(`UPDATE kb_documents`).WithArgs(anyArgs(2)...).
		WillReturnRows(pgxmock.NewRows(documentColumns).AddRow(documentRow()...))
	_, err = store.AdvanceDocumentGeneration(ctx, 7, 2)
	require.NoError(t, err)

	mock.ExpectExec(`DELETE FROM kb_documents`).WithArgs(anyArgs(1)...).WillReturnResult(pgxmock.NewResult("DELETE", 1))
	require.NoError(t, store.DeleteDocument(ctx, 7))

	mock.ExpectExec(`DELETE FROM kb_documents`).WithArgs(anyArgs(2)...).WillReturnResult(pgxmock.NewResult("DELETE", 2))
	removedDocs, err := store.DeleteAbsentFileDocuments(ctx, 1, []string{"a.md"})
	require.NoError(t, err)
	assert.Equal(t, int64(2), removedDocs)

	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSQLStoreChunkAndRetrievalMethods(t *testing.T) {
	mock, err := pgxmock.NewPool()
	require.NoError(t, err)
	defer mock.Close()
	store := NewSQLStore(mock)
	ctx := context.Background()
	now := time.Now()

	mock.ExpectQuery(`INSERT INTO kb_chunks`).WithArgs(anyArgs(13)...).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(int64(11)))
	id, err := store.InsertChunk(ctx, Chunk{DocumentID: 7, Scope: ScopeShared, SourceID: 1, Generation: 1})
	require.NoError(t, err)
	assert.Equal(t, int64(11), id)

	mock.ExpectQuery(`FROM kb_chunks.*embedding IS NULL.*FOR UPDATE SKIP LOCKED`).WithArgs(anyArgs(3)...).
		WillReturnRows(pgxmock.NewRows(claimColumns).AddRow(
			int64(12), int64(7), ScopeShared, nil, int64(1), int32(1), int32(0),
			[]string{"A"}, "body", "embed text", []byte("hash"), "model-x",
			int32(3), []byte(`{"line":1}`), now,
		))
	chunks, err := store.ClaimUnembeddedChunks(ctx, 7, 1, 16)
	require.NoError(t, err)
	require.Len(t, chunks, 1)
	assert.Equal(t, "embed text", chunks[0].EmbedText)
	assert.Equal(t, 1, chunks[0].Locator.Line)

	mock.ExpectExec(`UPDATE kb_chunks SET embedding`).WithArgs(anyArgs(3)...).WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	require.NoError(t, store.SetChunkEmbedding(ctx, 12, []float32{0.1}, "model-x"))

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM kb_chunks.*embedding IS NULL`).WithArgs(anyArgs(2)...).
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(int64(0)))
	remaining, err := store.CountUnembeddedChunks(ctx, 7, 1)
	require.NoError(t, err)
	assert.Zero(t, remaining)

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM kb_chunks`).WithArgs(anyArgs(2)...).
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(int64(2)))
	count, err := store.CountChunksAtGeneration(ctx, 7, 1)
	require.NoError(t, err)
	assert.Equal(t, int64(2), count)

	mock.ExpectQuery(`SELECT DISTINCT heading_path`).WithArgs(anyArgs(2)...).
		WillReturnRows(pgxmock.NewRows([]string{"heading_path"}).AddRow([]string{"A", "B"}))
	headings, err := store.ListDocumentHeadingPaths(ctx, 7, 1)
	require.NoError(t, err)
	assert.Equal(t, [][]string{{"A", "B"}}, headings)

	mock.ExpectExec(`DELETE FROM kb_chunks`).WithArgs(anyArgs(2)...).WillReturnResult(pgxmock.NewResult("DELETE", 1))
	require.NoError(t, store.DeletePendingChunks(ctx, 7, 1))
	mock.ExpectExec(`DELETE FROM kb_chunks`).WithArgs(anyArgs(2)...).WillReturnResult(pgxmock.NewResult("DELETE", 1))
	require.NoError(t, store.DeleteSupersededChunks(ctx, 7, 1))
	mock.ExpectExec(`DELETE FROM kb_chunks`).WithArgs(anyArgs(1)...).WillReturnResult(pgxmock.NewResult("DELETE", 1))
	require.NoError(t, store.DeleteChunksByDocument(ctx, 7))

	mock.ExpectQuery(`FROM kb_chunks c.*JOIN kb_documents d.*JOIN kb_sources s`).WithArgs(anyArgs(6)...).
		WillReturnRows(pgxmock.NewRows(retrievalColumns).AddRow(
			int64(9), int64(7), int64(1), "shared", "B-Cook/diet.md",
			[]string{"A"}, "body", []byte(`{"line":2}`), float64(0.8),
		))
	passages, err := store.RetrievePassages(ctx, RetrievalParams{
		Embedding: []float32{0.1}, EmbeddingModel: "model-x", UserID: 1, Limit: 5,
	})
	require.NoError(t, err)
	require.Len(t, passages, 1)
	assert.Equal(t, "B-Cook/diet.md", passages[0].RelPath)
	assert.InDelta(t, 0.8, passages[0].Score, 1e-9)

	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSQLStoreMapsMissingRowsToSentinels(t *testing.T) {
	mock, err := pgxmock.NewPool()
	require.NoError(t, err)
	defer mock.Close()
	store := NewSQLStore(mock)
	ctx := context.Background()

	emptySource := func() *pgxmock.Rows { return pgxmock.NewRows(sourceColumns) }
	emptyDocument := func() *pgxmock.Rows { return pgxmock.NewRows(documentColumns) }

	mock.ExpectQuery(`FROM kb_sources WHERE id`).WithArgs(anyArgs(1)...).WillReturnRows(emptySource())
	_, err = store.GetSource(ctx, 1)
	assert.ErrorIs(t, err, ErrSourceNotFound)

	mock.ExpectQuery(`UPDATE kb_sources`).WithArgs(anyArgs(3)...).WillReturnRows(emptySource())
	_, err = store.SetSourceEnabled(ctx, 1, 1, false)
	assert.ErrorIs(t, err, ErrSourceNotFound)

	mock.ExpectQuery(`FROM kb_documents WHERE id = \$1`).WithArgs(anyArgs(1)...).WillReturnRows(emptyDocument())
	_, err = store.GetDocument(ctx, 1)
	assert.ErrorIs(t, err, ErrDocumentNotFound)

	mock.ExpectQuery(`scope = 'shared' OR`).WithArgs(anyArgs(2)...).WillReturnRows(emptyDocument())
	_, err = store.GetVisibleDocument(ctx, 1, 1)
	assert.ErrorIs(t, err, ErrDocumentNotFound)

	mock.ExpectQuery(`scope = 'user' AND owner_id`).WithArgs(anyArgs(2)...).WillReturnRows(emptyDocument())
	_, err = store.GetOwnedDocument(ctx, 1, 1)
	assert.ErrorIs(t, err, ErrDocumentNotFound)

	mock.ExpectQuery(`source_id = \$1 AND rel_path`).WithArgs(anyArgs(2)...).WillReturnRows(emptyDocument())
	_, err = store.GetDocumentBySourcePath(ctx, 1, "missing.md")
	assert.ErrorIs(t, err, ErrDocumentNotFound)

	mock.ExpectQuery(`origin = 'inline' AND owner_id`).WithArgs(anyArgs(2)...).WillReturnRows(emptyDocument())
	_, err = store.GetInlineDocumentByHash(ctx, 1, []byte("hash"))
	assert.ErrorIs(t, err, ErrDocumentNotFound)

	mock.ExpectQuery(`UPDATE kb_documents`).WithArgs(anyArgs(4)...).WillReturnRows(emptyDocument())
	_, err = store.UpdateInlineDocumentContent(ctx, 1, "body", []byte("hash"), 4)
	assert.ErrorIs(t, err, ErrDocumentNotFound)

	require.NoError(t, mock.ExpectationsWereMet())
}
