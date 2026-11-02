package kb

import (
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"strings"

	"github.com/rs/zerolog/log"
)

type Embedder interface {
	EmbedStrings(ctx context.Context, inputs []string) ([][]float32, error)
	Model() string
}

type EmbedderResolver func(ctx context.Context, userID int64) (Embedder, error)

type Indexer struct {
	store         Store
	normalizers   []Normalizer
	embedder      Embedder
	resolver      EmbedderResolver
	chunkOpts     ChunkOptions
	connectorOpts ConnectorOptions
	batchSize     int32
}

func NewIndexer(store Store, embedder Embedder, chunkOpts ChunkOptions, connectorOpts ConnectorOptions) *Indexer {
	return &Indexer{
		store:         store,
		normalizers:   []Normalizer{NewMarkdownNormalizer()},
		embedder:      embedder,
		chunkOpts:     chunkOpts,
		connectorOpts: connectorOpts,
		batchSize:     16,
	}
}

func (i *Indexer) SetEmbedderResolver(fn EmbedderResolver) {
	i.resolver = fn
}

func (i *Indexer) SetNormalizers(normalizers ...Normalizer) {
	i.normalizers = normalizers
}

//nolint:gocyclo // Each branch is one scan outcome (skip, fill, reparse, delete).
func (i *Indexer) ScanSource(ctx context.Context, src Source) (ScanResult, error) {
	if src.Kind != KindDir {
		return i.fillSourcePending(ctx, src)
	}
	candidates, skipped, err := WalkSource(src.RootPath, i.connectorOpts)
	result := ScanResult{Skipped: skipped}
	if err != nil {
		_ = i.store.UpdateSourceScan(ctx, src.ID, "source root is not accessible")
		return result, err
	}
	existing, err := i.allDocuments(ctx, src.ID)
	if err != nil {
		_ = i.store.UpdateSourceScan(ctx, src.ID, "source documents could not be loaded")
		return result, err
	}
	byPath := make(map[string]Document, len(existing))
	for _, doc := range existing {
		byPath[doc.RelPath] = doc
	}
	embedder := i.embedderFor(ctx, src.Scope, src.OwnerID)
	keep := make([]string, 0, len(candidates)+len(skipped))
	countLimited := false
	for _, skip := range skipped {
		keep = append(keep, skip.RelPath)
		if skip.Reason == "file count limit reached" {
			countLimited = true
		}
	}
	for _, candidate := range candidates {
		keep = append(keep, candidate.RelPath)
		if candidate.DocType == "" {
			result.Skipped = append(result.Skipped, ScanSkip{RelPath: candidate.RelPath, Reason: "unsupported document type"})
			continue
		}
		raw, readErr := os.ReadFile(candidate.Path)
		if readErr != nil {
			result.Skipped = append(result.Skipped, ScanSkip{RelPath: candidate.RelPath, Reason: "file is unreadable"})
			continue
		}
		hash := hashBytes(raw)
		if current, ok := byPath[candidate.RelPath]; ok && bytesEqual(current.ContentHash, hash) {
			if current.Status == StatusIndexed {
				continue
			}
			if current.Status == StatusPending {
				pendingChunks, countErr := i.store.CountChunksAtGeneration(ctx, current.ID, current.Generation+1)
				if countErr != nil {
					_ = i.store.UpdateSourceScan(ctx, src.ID, "pending documents could not be loaded")
					return result, countErr
				}
				if pendingChunks > 0 {
					filled, fillErr := i.FillDocument(ctx, current)
					result.Chunks += filled
					if fillErr != nil {
						log.Warn().Err(fillErr).Int64("document_id", current.ID).Msg("kb fill failed")
					}
					continue
				}
			}
		}
		stored, upsertErr := i.store.UpsertFileDocument(ctx, Document{
			SourceID:    src.ID,
			Scope:       src.Scope,
			OwnerID:     src.OwnerID,
			Origin:      OriginFile,
			RelPath:     candidate.RelPath,
			Title:       titleFromPath(candidate.RelPath),
			DocType:     candidate.DocType,
			ContentHash: hash,
			ByteSize:    int64(len(raw)),
			Mtime:       &candidate.ModTime,
		})
		if upsertErr != nil {
			_ = i.store.UpdateSourceScan(ctx, src.ID, "document could not be stored")
			return result, upsertErr
		}
		chunks, indexErr := i.indexParsed(ctx, stored, raw, embedder)
		result.Documents++
		result.Chunks += chunks
		if indexErr != nil {
			log.Warn().Err(indexErr).Int64("document_id", stored.ID).Msg("kb document index failed")
		}
	}
	if !countLimited && len(candidates) > 0 {
		if _, err := i.store.DeleteAbsentFileDocuments(ctx, src.ID, keep); err != nil {
			_ = i.store.UpdateSourceScan(ctx, src.ID, "removed documents could not be cleaned up")
			return result, err
		}
	}
	_ = i.store.UpdateSourceScan(ctx, src.ID, "")
	return result, nil
}

func (i *Indexer) IndexInline(ctx context.Context, doc Document) (int, error) {
	if doc.Origin != OriginInline {
		return 0, ErrFileBackedDocument
	}
	embedder := i.embedderFor(ctx, doc.Scope, doc.OwnerID)
	return i.indexParsed(ctx, doc, []byte(doc.Content), embedder)
}

func (i *Indexer) FillDocument(ctx context.Context, doc Document) (int, error) {
	generation := doc.Generation + 1
	count, err := i.store.CountChunksAtGeneration(ctx, doc.ID, generation)
	if err != nil {
		return 0, err
	}
	if count == 0 {
		return 0, nil
	}
	embedder := i.embedderFor(ctx, doc.Scope, doc.OwnerID)
	return i.fillGeneration(ctx, doc, generation, embedder)
}

func (i *Indexer) indexParsed(ctx context.Context, doc Document, raw []byte, embedder Embedder) (int, error) {
	normalizer := i.normalizerFor(doc.DocType)
	if normalizer == nil {
		_ = i.store.MarkDocumentStatus(ctx, doc.ID, StatusFailed, "unsupported document type")
		return 0, nil
	}
	normalized, err := normalizer.Normalize(ctx, raw, Source{
		ID: doc.SourceID, Scope: doc.Scope, OwnerID: doc.OwnerID,
	})
	if err != nil {
		_ = i.store.MarkDocumentStatus(ctx, doc.ID, StatusFailed, "document could not be parsed")
		return 0, nil
	}
	if normalized.Title != "" {
		_ = i.store.SetDocumentTitle(ctx, doc.ID, normalized.Title)
	}
	specs := ChunkDocument(doc.RelPath, normalized, i.chunkOpts)
	generation := doc.Generation + 1
	if err := i.store.DeletePendingChunks(ctx, doc.ID, doc.Generation); err != nil {
		return 0, err
	}
	model := ""
	if embedder != nil {
		model = embedder.Model()
	}
	for _, spec := range specs {
		if _, err := i.store.InsertChunk(ctx, Chunk{
			DocumentID:     doc.ID,
			Scope:          doc.Scope,
			OwnerID:        doc.OwnerID,
			SourceID:       doc.SourceID,
			Generation:     generation,
			Ordinal:        spec.Ordinal,
			HeadingPath:    spec.HeadingPath,
			Content:        spec.Content,
			EmbedText:      spec.EmbedText,
			ContentHash:    spec.ContentHash,
			EmbeddingModel: model,
			TokenCount:     spec.TokenCount,
			Locator:        spec.Locator,
		}); err != nil {
			_ = i.store.MarkDocumentStatus(ctx, doc.ID, StatusFailed, "document could not be stored")
			return 0, err
		}
	}
	if len(specs) == 0 {
		if _, err := i.store.AdvanceDocumentGeneration(ctx, doc.ID, generation); err != nil {
			return 0, err
		}
		return 0, i.store.DeleteSupersededChunks(ctx, doc.ID, generation)
	}
	return i.fillGeneration(ctx, doc, generation, embedder)
}

func (i *Indexer) fillGeneration(ctx context.Context, doc Document, generation int32, embedder Embedder) (int, error) {
	if embedder == nil {
		_ = i.store.MarkDocumentPending(ctx, doc.ID, generation, "embedding unavailable")
		return 0, ErrEmbedderUnavailable
	}
	filled := 0
	model := embedder.Model()
	for {
		chunks, err := i.store.ClaimUnembeddedChunks(ctx, doc.ID, generation, i.batchSize)
		if err != nil {
			return filled, err
		}
		if len(chunks) == 0 {
			break
		}
		inputs := make([]string, len(chunks))
		for index, chunk := range chunks {
			inputs[index] = chunk.EmbedText
		}
		vectors, err := embedder.EmbedStrings(ctx, inputs)
		if err != nil {
			log.Warn().Err(err).Int64("document_id", doc.ID).Msg("kb embedding unavailable")
			_ = i.store.MarkDocumentPending(ctx, doc.ID, generation, "embedding unavailable")
			return filled, nil
		}
		if len(vectors) != len(chunks) {
			log.Warn().Int64("document_id", doc.ID).Msg("kb embedding count mismatch")
			_ = i.store.MarkDocumentPending(ctx, doc.ID, generation, "embedding unavailable")
			return filled, nil
		}
		for index, chunk := range chunks {
			if err := i.store.SetChunkEmbedding(ctx, chunk.ID, vectors[index], model); err != nil {
				return filled, err
			}
		}
		filled += len(chunks)
	}
	remaining, err := i.store.CountUnembeddedChunks(ctx, doc.ID, generation)
	if err != nil {
		return filled, err
	}
	if remaining > 0 {
		_ = i.store.MarkDocumentPending(ctx, doc.ID, generation, "embedding unavailable")
		return filled, nil
	}
	if _, err := i.store.AdvanceDocumentGeneration(ctx, doc.ID, generation); err != nil {
		return filled, err
	}
	if err := i.store.DeleteSupersededChunks(ctx, doc.ID, generation); err != nil {
		return filled, err
	}
	return filled, nil
}

func (i *Indexer) fillSourcePending(ctx context.Context, src Source) (ScanResult, error) {
	docs, err := i.store.ListPendingDocuments(ctx, src.OwnerID, 100)
	if err != nil {
		return ScanResult{}, err
	}
	result := ScanResult{}
	for _, doc := range docs {
		if doc.SourceID != src.ID {
			continue
		}
		filled, fillErr := i.FillDocument(ctx, doc)
		result.Chunks += filled
		if fillErr != nil {
			log.Warn().Err(fillErr).Int64("document_id", doc.ID).Msg("kb fill failed")
		}
	}
	_ = i.store.UpdateSourceScan(ctx, src.ID, "")
	return result, nil
}

func (i *Indexer) allDocuments(ctx context.Context, sourceID int64) ([]Document, error) {
	const pageSize int32 = 500
	var out []Document
	var offset int32
	for {
		page, err := i.store.ListDocumentsBySource(ctx, sourceID, pageSize, offset)
		if err != nil {
			return nil, err
		}
		out = append(out, page...)
		//nolint:gosec // G115: pageSize is the fixed 500-row page bound.
		if int32(len(page)) < pageSize {
			return out, nil
		}
		offset += pageSize
	}
}

func (i *Indexer) embedderFor(ctx context.Context, scope string, ownerID int64) Embedder {
	if scope == ScopeUser && i.resolver != nil {
		embedder, err := i.resolver(ctx, ownerID)
		if err != nil {
			log.Warn().Err(err).Int64("user_id", ownerID).Msg("kb per-user embedder unavailable")
			return nil
		}
		if embedder != nil {
			return embedder
		}
	}
	return i.embedder
}

func (i *Indexer) normalizerFor(docType string) Normalizer {
	for _, normalizer := range i.normalizers {
		if normalizer.Handles(docType) {
			return normalizer
		}
	}
	return nil
}

func hashBytes(raw []byte) []byte {
	sum := sha256.Sum256(raw)
	return sum[:]
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func titleFromPath(relPath string) string {
	base := filepath.Base(relPath)
	ext := filepath.Ext(base)
	if ext != "" {
		base = strings.TrimSuffix(base, ext)
	}
	return base
}
