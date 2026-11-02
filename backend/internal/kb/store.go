package kb

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/pgvector/pgvector-go"

	kbdb "nyx/internal/kb/db"
)

type RetrievalParams struct {
	Embedding      []float32
	EmbeddingModel string
	UserID         int64
	SourceID       *int64
	PathPrefix     string
	Limit          int
}

type Store interface {
	GetSource(ctx context.Context, id int64) (Source, error)
	ListVisibleSources(ctx context.Context, ownerID int64) ([]Source, error)
	ListAllSources(ctx context.Context) ([]Source, error)
	ListSourcesByOwner(ctx context.Context, ownerID int64) ([]Source, error)
	UpsertSharedSource(ctx context.Context, name, rootPath string) (Source, error)
	UpsertUserDirSource(ctx context.Context, ownerID int64, name, rootPath string) (Source, error)
	EnsureInlineSource(ctx context.Context, ownerID int64) (Source, error)
	SetSourceEnabled(ctx context.Context, id, ownerID int64, enabled bool) (Source, error)
	DeleteUserSource(ctx context.Context, id, ownerID int64) (int64, error)
	UpdateSourceScan(ctx context.Context, id int64, lastError string) error
	SourceStats(ctx context.Context, id int64) (SourceStats, error)
	ListTopLevelDirs(ctx context.Context, id int64) ([]string, error)
	SourceEmbeddingModels(ctx context.Context, id int64) ([]string, error)

	GetDocument(ctx context.Context, id int64) (Document, error)
	GetVisibleDocument(ctx context.Context, id, ownerID int64) (Document, error)
	GetOwnedDocument(ctx context.Context, id, ownerID int64) (Document, error)
	GetDocumentBySourcePath(ctx context.Context, sourceID int64, relPath string) (Document, error)
	GetInlineDocumentByHash(ctx context.Context, ownerID int64, hash []byte) (Document, error)
	ListVisibleDocuments(ctx context.Context, ownerID int64, sourceID *int64, origin, status string, limit, offset int32) ([]Document, error)
	ListDocumentsBySource(ctx context.Context, sourceID int64, limit, offset int32) ([]Document, error)
	ListPendingDocuments(ctx context.Context, ownerID int64, limit int32) ([]Document, error)
	ListDocumentsWithUnembedded(ctx context.Context, ownerID int64, limit int32) ([]Document, error)
	UpsertFileDocument(ctx context.Context, d Document) (Document, error)
	UpsertInlineDocument(ctx context.Context, d Document) (Document, error)
	UpdateInlineDocumentContent(ctx context.Context, id int64, content string, hash []byte, size int64) (Document, error)
	MarkDocumentStatus(ctx context.Context, id int64, status, detail string) error
	MarkDocumentPending(ctx context.Context, id int64, generation int32, detail string) error
	SetDocumentTitle(ctx context.Context, id int64, title string) error
	AdvanceDocumentGeneration(ctx context.Context, id int64, generation int32) (Document, error)
	DeleteDocument(ctx context.Context, id int64) error
	DeleteAbsentFileDocuments(ctx context.Context, sourceID int64, keep []string) (int64, error)

	InsertChunk(ctx context.Context, c Chunk) (int64, error)
	ClaimUnembeddedChunks(ctx context.Context, docID int64, generation int32, limit int32) ([]Chunk, error)
	SetChunkEmbedding(ctx context.Context, chunkID int64, vec []float32, model string) error
	CountUnembeddedChunks(ctx context.Context, docID int64, generation int32) (int64, error)
	CountChunksAtGeneration(ctx context.Context, docID int64, generation int32) (int64, error)
	ListDocumentHeadingPaths(ctx context.Context, docID int64, generation int32) ([][]string, error)
	DeletePendingChunks(ctx context.Context, docID int64, generation int32) error
	DeleteSupersededChunks(ctx context.Context, docID int64, generation int32) error
	DeleteChunksByDocument(ctx context.Context, docID int64) error
	RetrievePassages(ctx context.Context, params RetrievalParams) ([]Passage, error)
}

type SQLStore struct {
	q *kbdb.Queries
}

func NewSQLStore(db kbdb.DBTX) *SQLStore {
	return &SQLStore{q: kbdb.New(db)}
}

func (s *SQLStore) GetSource(ctx context.Context, id int64) (Source, error) {
	row, err := s.q.GetSource(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Source{}, ErrSourceNotFound
	}
	if err != nil {
		return Source{}, err
	}
	return sourceFromRow(row), nil
}

func (s *SQLStore) ListVisibleSources(ctx context.Context, ownerID int64) ([]Source, error) {
	rows, err := s.q.ListVisibleSources(ctx, pgInt8(ownerID))
	if err != nil {
		return nil, err
	}
	return sourcesFromRows(rows), nil
}

func (s *SQLStore) ListAllSources(ctx context.Context) ([]Source, error) {
	rows, err := s.q.ListAllSources(ctx)
	if err != nil {
		return nil, err
	}
	return sourcesFromRows(rows), nil
}

func (s *SQLStore) ListSourcesByOwner(ctx context.Context, ownerID int64) ([]Source, error) {
	rows, err := s.q.ListSourcesByOwner(ctx, pgInt8(ownerID))
	if err != nil {
		return nil, err
	}
	return sourcesFromRows(rows), nil
}

func (s *SQLStore) UpsertSharedSource(ctx context.Context, name, rootPath string) (Source, error) {
	row, err := s.q.UpsertSharedSource(ctx, kbdb.UpsertSharedSourceParams{Name: name, RootPath: pgText(rootPath)})
	if err != nil {
		return Source{}, err
	}
	return sourceFromRow(row), nil
}

func (s *SQLStore) UpsertUserDirSource(ctx context.Context, ownerID int64, name, rootPath string) (Source, error) {
	row, err := s.q.UpsertUserDirSource(ctx, kbdb.UpsertUserDirSourceParams{
		OwnerID:  pgInt8(ownerID),
		Name:     name,
		RootPath: pgText(rootPath),
	})
	if err != nil {
		return Source{}, err
	}
	return sourceFromRow(row), nil
}

func (s *SQLStore) EnsureInlineSource(ctx context.Context, ownerID int64) (Source, error) {
	row, err := s.q.EnsureInlineSource(ctx, pgInt8(ownerID))
	if err != nil {
		return Source{}, err
	}
	return sourceFromRow(row), nil
}

func (s *SQLStore) SetSourceEnabled(ctx context.Context, id, ownerID int64, enabled bool) (Source, error) {
	row, err := s.q.SetSourceEnabled(ctx, kbdb.SetSourceEnabledParams{
		ID:      id,
		Enabled: enabled,
		OwnerID: pgInt8(ownerID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Source{}, ErrSourceNotFound
	}
	if err != nil {
		return Source{}, err
	}
	return sourceFromRow(row), nil
}

func (s *SQLStore) DeleteUserSource(ctx context.Context, id, ownerID int64) (int64, error) {
	return s.q.DeleteUserSource(ctx, kbdb.DeleteUserSourceParams{ID: id, OwnerID: pgInt8(ownerID)})
}

func (s *SQLStore) UpdateSourceScan(ctx context.Context, id int64, lastError string) error {
	return s.q.UpdateSourceScan(ctx, kbdb.UpdateSourceScanParams{ID: id, LastError: lastError})
}

func (s *SQLStore) SourceStats(ctx context.Context, id int64) (SourceStats, error) {
	documents, err := s.q.SourceDocumentCount(ctx, id)
	if err != nil {
		return SourceStats{}, err
	}
	chunks, err := s.q.SourceChunkCount(ctx, id)
	if err != nil {
		return SourceStats{}, err
	}
	pending, err := s.q.SourcePendingCount(ctx, id)
	if err != nil {
		return SourceStats{}, err
	}
	return SourceStats{Documents: documents, Chunks: chunks, Pending: pending}, nil
}

func (s *SQLStore) ListTopLevelDirs(ctx context.Context, id int64) ([]string, error) {
	return s.q.ListTopLevelDirectories(ctx, id)
}

func (s *SQLStore) SourceEmbeddingModels(ctx context.Context, id int64) ([]string, error) {
	return s.q.SourceEmbeddingModels(ctx, id)
}

func (s *SQLStore) GetDocument(ctx context.Context, id int64) (Document, error) {
	row, err := s.q.GetDocument(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Document{}, ErrDocumentNotFound
	}
	if err != nil {
		return Document{}, err
	}
	return documentFromRow(row), nil
}

func (s *SQLStore) GetVisibleDocument(ctx context.Context, id, ownerID int64) (Document, error) {
	row, err := s.q.GetVisibleDocument(ctx, kbdb.GetVisibleDocumentParams{ID: id, OwnerID: ownerID})
	if errors.Is(err, pgx.ErrNoRows) {
		return Document{}, ErrDocumentNotFound
	}
	if err != nil {
		return Document{}, err
	}
	return documentFromRow(row), nil
}

func (s *SQLStore) GetOwnedDocument(ctx context.Context, id, ownerID int64) (Document, error) {
	row, err := s.q.GetOwnedDocument(ctx, kbdb.GetOwnedDocumentParams{ID: id, OwnerID: pgInt8(ownerID)})
	if errors.Is(err, pgx.ErrNoRows) {
		return Document{}, ErrDocumentNotFound
	}
	if err != nil {
		return Document{}, err
	}
	return documentFromRow(row), nil
}

func (s *SQLStore) GetDocumentBySourcePath(ctx context.Context, sourceID int64, relPath string) (Document, error) {
	row, err := s.q.GetDocumentBySourcePath(ctx, kbdb.GetDocumentBySourcePathParams{SourceID: sourceID, RelPath: relPath})
	if errors.Is(err, pgx.ErrNoRows) {
		return Document{}, ErrDocumentNotFound
	}
	if err != nil {
		return Document{}, err
	}
	return documentFromRow(row), nil
}

func (s *SQLStore) GetInlineDocumentByHash(ctx context.Context, ownerID int64, hash []byte) (Document, error) {
	row, err := s.q.GetInlineDocumentByHash(ctx, kbdb.GetInlineDocumentByHashParams{
		OwnerID:     pgInt8(ownerID),
		ContentHash: hash,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Document{}, ErrDocumentNotFound
	}
	if err != nil {
		return Document{}, err
	}
	return documentFromRow(row), nil
}

func (s *SQLStore) ListVisibleDocuments(ctx context.Context, ownerID int64, sourceID *int64, origin, status string, limit, offset int32) ([]Document, error) {
	rows, err := s.q.ListVisibleDocuments(ctx, kbdb.ListVisibleDocumentsParams{
		OwnerID:     ownerID,
		SourceID:    pgInt8Ptr(sourceID),
		Origin:      pgText(origin),
		Status:      pgText(status),
		LimitCount:  limit,
		OffsetCount: offset,
	})
	if err != nil {
		return nil, err
	}
	return documentsFromRows(rows), nil
}

func (s *SQLStore) ListDocumentsBySource(ctx context.Context, sourceID int64, limit, offset int32) ([]Document, error) {
	rows, err := s.q.ListDocumentsBySource(ctx, kbdb.ListDocumentsBySourceParams{
		SourceID: sourceID,
		Limit:    limit,
		Offset:   offset,
	})
	if err != nil {
		return nil, err
	}
	return documentsFromRows(rows), nil
}

func (s *SQLStore) ListPendingDocuments(ctx context.Context, ownerID int64, limit int32) ([]Document, error) {
	rows, err := s.q.ListPendingDocuments(ctx, kbdb.ListPendingDocumentsParams{OwnerID: pgInt8(ownerID), Limit: limit})
	if err != nil {
		return nil, err
	}
	return documentsFromRows(rows), nil
}

func (s *SQLStore) ListDocumentsWithUnembedded(ctx context.Context, ownerID int64, limit int32) ([]Document, error) {
	rows, err := s.q.ListDocumentsWithUnembeddedChunks(ctx, kbdb.ListDocumentsWithUnembeddedChunksParams{
		OwnerID: pgInt8(ownerID),
		Limit:   limit,
	})
	if err != nil {
		return nil, err
	}
	return documentsFromRows(rows), nil
}

func (s *SQLStore) UpsertFileDocument(ctx context.Context, d Document) (Document, error) {
	row, err := s.q.UpsertFileDocument(ctx, kbdb.UpsertFileDocumentParams{
		SourceID:    d.SourceID,
		Scope:       d.Scope,
		OwnerID:     ownerPtr(d.OwnerID),
		RelPath:     d.RelPath,
		Title:       d.Title,
		DocType:     d.DocType,
		ContentHash: d.ContentHash,
		ByteSize:    d.ByteSize,
		Mtime:       pgTime(d.Mtime),
	})
	if err != nil {
		return Document{}, err
	}
	return documentFromRow(row), nil
}

func (s *SQLStore) UpsertInlineDocument(ctx context.Context, d Document) (Document, error) {
	row, err := s.q.UpsertInlineDocument(ctx, kbdb.UpsertInlineDocumentParams{
		SourceID:    d.SourceID,
		OwnerID:     pgInt8(d.OwnerID),
		RelPath:     d.RelPath,
		Title:       d.Title,
		ContentHash: d.ContentHash,
		ByteSize:    d.ByteSize,
		Content:     pgText(d.Content),
	})
	if err != nil {
		return Document{}, err
	}
	return documentFromRow(row), nil
}

func (s *SQLStore) UpdateInlineDocumentContent(ctx context.Context, id int64, content string, hash []byte, size int64) (Document, error) {
	row, err := s.q.UpdateInlineDocumentContent(ctx, kbdb.UpdateInlineDocumentContentParams{
		ID:          id,
		Content:     pgText(content),
		ContentHash: hash,
		ByteSize:    size,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Document{}, ErrDocumentNotFound
	}
	if err != nil {
		return Document{}, err
	}
	return documentFromRow(row), nil
}

func (s *SQLStore) MarkDocumentStatus(ctx context.Context, id int64, status, detail string) error {
	return s.q.MarkDocumentStatus(ctx, kbdb.MarkDocumentStatusParams{
		ID:          id,
		Status:      status,
		ErrorDetail: detail,
	})
}

func (s *SQLStore) MarkDocumentPending(ctx context.Context, id int64, generation int32, detail string) error {
	return s.q.MarkDocumentPendingIfCurrent(ctx, kbdb.MarkDocumentPendingIfCurrentParams{
		ID:          id,
		ErrorDetail: detail,
		Generation:  generation,
	})
}

func (s *SQLStore) SetDocumentTitle(ctx context.Context, id int64, title string) error {
	return s.q.SetDocumentTitle(ctx, kbdb.SetDocumentTitleParams{ID: id, Title: title})
}

func (s *SQLStore) AdvanceDocumentGeneration(ctx context.Context, id int64, generation int32) (Document, error) {
	row, err := s.q.AdvanceDocumentGeneration(ctx, kbdb.AdvanceDocumentGenerationParams{ID: id, Generation: generation})
	if errors.Is(err, pgx.ErrNoRows) {
		return Document{}, nil
	}
	if err != nil {
		return Document{}, err
	}
	return documentFromRow(row), nil
}

func (s *SQLStore) DeleteDocument(ctx context.Context, id int64) error {
	return s.q.DeleteDocumentByID(ctx, id)
}

func (s *SQLStore) DeleteAbsentFileDocuments(ctx context.Context, sourceID int64, keep []string) (int64, error) {
	return s.q.DeleteAbsentFileDocuments(ctx, kbdb.DeleteAbsentFileDocumentsParams{
		SourceID: sourceID,
		Column2:  keep,
	})
}

func (s *SQLStore) InsertChunk(ctx context.Context, c Chunk) (int64, error) {
	id, err := s.q.InsertChunk(ctx, kbdb.InsertChunkParams{
		DocumentID: c.DocumentID,
		Scope:      c.Scope,
		OwnerID:    ownerPtr(c.OwnerID),
		SourceID:   c.SourceID,
		Generation: c.Generation,
		//nolint:gosec // G115: ordinal is a chunk index within one document.
		Ordinal:        int32(c.Ordinal),
		HeadingPath:    c.HeadingPath,
		Content:        c.Content,
		EmbedText:      c.EmbedText,
		ContentHash:    c.ContentHash,
		EmbeddingModel: c.EmbeddingModel,
		//nolint:gosec // G115: token count is bounded by the chunk budget.
		TokenCount: int32(c.TokenCount),
		Locator:    locatorJSON(c.Locator),
	})
	if err != nil {
		return 0, err
	}
	return id, nil
}

func (s *SQLStore) ClaimUnembeddedChunks(ctx context.Context, docID int64, generation int32, limit int32) ([]Chunk, error) {
	rows, err := s.q.ClaimUnembeddedChunks(ctx, kbdb.ClaimUnembeddedChunksParams{
		DocumentID: docID,
		Generation: generation,
		Limit:      limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]Chunk, len(rows))
	for i, row := range rows {
		out[i] = chunkFromClaimRow(row)
	}
	return out, nil
}

func (s *SQLStore) SetChunkEmbedding(ctx context.Context, chunkID int64, vec []float32, model string) error {
	return s.q.SetChunkEmbedding(ctx, kbdb.SetChunkEmbeddingParams{
		ID:             chunkID,
		Embedding:      pgvector.NewVector(vec),
		EmbeddingModel: model,
	})
}

func (s *SQLStore) CountUnembeddedChunks(ctx context.Context, docID int64, generation int32) (int64, error) {
	return s.q.CountUnembeddedChunks(ctx, kbdb.CountUnembeddedChunksParams{DocumentID: docID, Generation: generation})
}

func (s *SQLStore) CountChunksAtGeneration(ctx context.Context, docID int64, generation int32) (int64, error) {
	return s.q.CountChunksAtGeneration(ctx, kbdb.CountChunksAtGenerationParams{DocumentID: docID, Generation: generation})
}

func (s *SQLStore) ListDocumentHeadingPaths(ctx context.Context, docID int64, generation int32) ([][]string, error) {
	return s.q.ListDocumentHeadingPaths(ctx, kbdb.ListDocumentHeadingPathsParams{DocumentID: docID, Generation: generation})
}

func (s *SQLStore) DeletePendingChunks(ctx context.Context, docID int64, generation int32) error {
	return s.q.DeletePendingChunks(ctx, kbdb.DeletePendingChunksParams{DocumentID: docID, Generation: generation})
}

func (s *SQLStore) DeleteSupersededChunks(ctx context.Context, docID int64, generation int32) error {
	return s.q.DeleteSupersededChunks(ctx, kbdb.DeleteSupersededChunksParams{DocumentID: docID, Generation: generation})
}

func (s *SQLStore) DeleteChunksByDocument(ctx context.Context, docID int64) error {
	return s.q.DeleteChunksByDocument(ctx, docID)
}

func (s *SQLStore) RetrievePassages(ctx context.Context, params RetrievalParams) ([]Passage, error) {
	rows, err := s.q.RetrieveKBPassages(ctx, kbdb.RetrieveKBPassagesParams{
		Embedding:      pgvector.NewVector(params.Embedding),
		EmbeddingModel: params.EmbeddingModel,
		OwnerID:        params.UserID,
		SourceID:       pgInt8Ptr(params.SourceID),
		PathPrefix:     pgText(params.PathPrefix),
		//nolint:gosec // G115: retrieval limit is capped at maxRetrievalTopK.
		LimitCount: int32(params.Limit),
	})
	if err != nil {
		return nil, err
	}
	out := make([]Passage, len(rows))
	for i, row := range rows {
		out[i] = Passage{
			ChunkID:     row.ID,
			DocID:       row.DocumentID,
			SourceID:    row.SourceID,
			SourceName:  row.SourceName,
			RelPath:     row.RelPath,
			HeadingPath: row.HeadingPath,
			Content:     row.Content,
			Locator:     locatorFromJSON(row.Locator),
			Score:       row.Score,
		}
	}
	return out, nil
}

func sourceFromRow(row kbdb.KbSource) Source {
	return Source{
		ID:         row.ID,
		Scope:      row.Scope,
		OwnerID:    ownerValue(row.OwnerID),
		Name:       row.Name,
		Kind:       row.Kind,
		RootPath:   textValue(row.RootPath),
		Enabled:    row.Enabled,
		LastScanAt: timeValue(row.LastScanAt),
		LastError:  row.LastError,
		CreatedAt:  row.CreatedAt.Time,
		UpdatedAt:  row.UpdatedAt.Time,
	}
}

func sourcesFromRows(rows []kbdb.KbSource) []Source {
	out := make([]Source, len(rows))
	for i, row := range rows {
		out[i] = sourceFromRow(row)
	}
	return out
}

func documentFromRow(row kbdb.KbDocument) Document {
	return Document{
		ID:          row.ID,
		SourceID:    row.SourceID,
		Scope:       row.Scope,
		OwnerID:     ownerValue(row.OwnerID),
		Origin:      row.Origin,
		RelPath:     row.RelPath,
		Title:       row.Title,
		DocType:     row.DocType,
		ContentHash: row.ContentHash,
		ByteSize:    row.ByteSize,
		Mtime:       timeValue(row.Mtime),
		Status:      row.Status,
		ErrorDetail: row.ErrorDetail,
		Generation:  row.Generation,
		Content:     textValue(row.Content),
		CreatedAt:   row.CreatedAt.Time,
		UpdatedAt:   row.UpdatedAt.Time,
	}
}

func documentsFromRows(rows []kbdb.KbDocument) []Document {
	out := make([]Document, len(rows))
	for i, row := range rows {
		out[i] = documentFromRow(row)
	}
	return out
}

func chunkFromClaimRow(row kbdb.ClaimUnembeddedChunksRow) Chunk {
	return Chunk{
		ID:             row.ID,
		DocumentID:     row.DocumentID,
		Scope:          row.Scope,
		OwnerID:        ownerValue(row.OwnerID),
		SourceID:       row.SourceID,
		Generation:     row.Generation,
		Ordinal:        int(row.Ordinal),
		HeadingPath:    row.HeadingPath,
		Content:        row.Content,
		EmbedText:      row.EmbedText,
		ContentHash:    row.ContentHash,
		EmbeddingModel: row.EmbeddingModel,
		TokenCount:     int(row.TokenCount),
		Locator:        locatorFromJSON(row.Locator),
	}
}

func pgInt8(v int64) pgtype.Int8 {
	return pgtype.Int8{Int64: v, Valid: true}
}

func pgInt8Ptr(v *int64) pgtype.Int8 {
	if v == nil {
		return pgtype.Int8{}
	}
	return pgtype.Int8{Int64: *v, Valid: true}
}

func ownerPtr(v int64) pgtype.Int8 {
	if v == 0 {
		return pgtype.Int8{}
	}
	return pgtype.Int8{Int64: v, Valid: true}
}

func ownerValue(v pgtype.Int8) int64 {
	if !v.Valid {
		return 0
	}
	return v.Int64
}

func pgText(v string) pgtype.Text {
	if v == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: v, Valid: true}
}

func textValue(v pgtype.Text) string {
	if !v.Valid {
		return ""
	}
	return v.String
}

func pgTime(v *time.Time) pgtype.Timestamptz {
	if v == nil {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: *v, Valid: true}
}

func timeValue(v pgtype.Timestamptz) *time.Time {
	if !v.Valid {
		return nil
	}
	t := v.Time
	return &t
}

func locatorJSON(l Locator) []byte {
	raw, err := json.Marshal(l)
	if err != nil {
		return []byte("{}")
	}
	return raw
}

func locatorFromJSON(raw []byte) Locator {
	var l Locator
	if len(raw) == 0 {
		return l
	}
	_ = json.Unmarshal(raw, &l)
	return l
}
