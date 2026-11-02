package kb

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeStore struct {
	sources       map[int64]Source
	documents     map[int64]Document
	chunks        map[int64]Chunk
	embedded      map[int64]bool
	nextSource    int64
	nextDoc       int64
	nextChunk     int64
	deletedDocs   []int64
	lastRetrieval *RetrievalParams
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		sources:    map[int64]Source{},
		documents:  map[int64]Document{},
		chunks:     map[int64]Chunk{},
		embedded:   map[int64]bool{},
		nextSource: 1,
		nextDoc:    1,
		nextChunk:  1,
	}
}

func (f *fakeStore) addSource(src Source) Source {
	if src.ID == 0 {
		src.ID = f.nextSource
		f.nextSource++
	}
	f.sources[src.ID] = src
	return src
}

func (f *fakeStore) GetSource(_ context.Context, id int64) (Source, error) {
	src, ok := f.sources[id]
	if !ok {
		return Source{}, ErrSourceNotFound
	}
	return src, nil
}

func (f *fakeStore) ListVisibleSources(_ context.Context, ownerID int64) ([]Source, error) {
	out := make([]Source, 0, len(f.sources))
	for _, src := range f.sources {
		if src.Enabled && (src.Scope == ScopeShared || src.OwnerID == ownerID) {
			out = append(out, src)
		}
	}
	return out, nil
}

func (f *fakeStore) ListAllSources(_ context.Context) ([]Source, error) {
	out := make([]Source, 0, len(f.sources))
	for _, src := range f.sources {
		out = append(out, src)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (f *fakeStore) ListSourcesByOwner(_ context.Context, ownerID int64) ([]Source, error) {
	out := make([]Source, 0, len(f.sources))
	for _, src := range f.sources {
		if src.OwnerID == ownerID {
			out = append(out, src)
		}
	}
	return out, nil
}

func (f *fakeStore) UpsertSharedSource(_ context.Context, name, rootPath string) (Source, error) {
	for _, src := range f.sources {
		if src.Scope == ScopeShared && src.Name == name {
			src.RootPath = rootPath
			src.Kind = KindDir
			src.Enabled = true
			f.sources[src.ID] = src
			return src, nil
		}
	}
	return f.addSource(Source{Scope: ScopeShared, Name: name, Kind: KindDir, RootPath: rootPath, Enabled: true}), nil
}

func (f *fakeStore) UpsertUserDirSource(_ context.Context, ownerID int64, name, rootPath string) (Source, error) {
	for _, src := range f.sources {
		if src.Scope == ScopeUser && src.OwnerID == ownerID && src.Name == name {
			src.RootPath = rootPath
			src.Kind = KindDir
			src.Enabled = true
			f.sources[src.ID] = src
			return src, nil
		}
	}
	return f.addSource(Source{Scope: ScopeUser, OwnerID: ownerID, Name: name, Kind: KindDir, RootPath: rootPath, Enabled: true}), nil
}

func (f *fakeStore) EnsureInlineSource(_ context.Context, ownerID int64) (Source, error) {
	for _, src := range f.sources {
		if src.Scope == ScopeUser && src.OwnerID == ownerID && src.Kind == KindInline {
			return src, nil
		}
	}
	return f.addSource(Source{Scope: ScopeUser, OwnerID: ownerID, Name: "inline", Kind: KindInline, Enabled: true}), nil
}

func (f *fakeStore) SetSourceEnabled(_ context.Context, id, ownerID int64, enabled bool) (Source, error) {
	src, ok := f.sources[id]
	if !ok || src.Scope != ScopeUser || src.OwnerID != ownerID {
		return Source{}, ErrSourceNotFound
	}
	src.Enabled = enabled
	f.sources[id] = src
	return src, nil
}

func (f *fakeStore) DeleteUserSource(_ context.Context, id, ownerID int64) (int64, error) {
	src, ok := f.sources[id]
	if !ok || src.Scope != ScopeUser || src.OwnerID != ownerID {
		return 0, nil
	}
	delete(f.sources, id)
	for docID, doc := range f.documents {
		if doc.SourceID == id {
			delete(f.documents, docID)
			for chunkID, chunk := range f.chunks {
				if chunk.DocumentID == docID {
					delete(f.chunks, chunkID)
				}
			}
		}
	}
	return 1, nil
}

func (f *fakeStore) UpdateSourceScan(_ context.Context, _ int64, _ string) error { return nil }

func (f *fakeStore) SourceStats(_ context.Context, id int64) (SourceStats, error) {
	var stats SourceStats
	for _, doc := range f.documents {
		if doc.SourceID != id {
			continue
		}
		stats.Documents++
		if doc.Status != StatusIndexed {
			stats.Pending++
		}
	}
	for _, chunk := range f.chunks {
		if chunk.SourceID == id {
			stats.Chunks++
		}
	}
	return stats, nil
}

func (f *fakeStore) ListTopLevelDirs(_ context.Context, sourceID int64) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, doc := range f.documents {
		if doc.SourceID != sourceID {
			continue
		}
		index := strings.Index(doc.RelPath, "/")
		if index <= 0 {
			continue
		}
		dir := doc.RelPath[:index]
		if !seen[dir] {
			seen[dir] = true
			out = append(out, dir)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (f *fakeStore) SourceEmbeddingModels(_ context.Context, sourceID int64) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, chunk := range f.chunks {
		if chunk.SourceID != sourceID || chunk.EmbeddingModel == "" {
			continue
		}
		if !seen[chunk.EmbeddingModel] {
			seen[chunk.EmbeddingModel] = true
			out = append(out, chunk.EmbeddingModel)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (f *fakeStore) GetDocument(_ context.Context, id int64) (Document, error) {
	doc, ok := f.documents[id]
	if !ok {
		return Document{}, ErrDocumentNotFound
	}
	return doc, nil
}

func (f *fakeStore) GetVisibleDocument(_ context.Context, id, ownerID int64) (Document, error) {
	doc, ok := f.documents[id]
	if !ok || (doc.Scope == ScopeUser && doc.OwnerID != ownerID) {
		return Document{}, ErrDocumentNotFound
	}
	return doc, nil
}

func (f *fakeStore) GetOwnedDocument(_ context.Context, id, ownerID int64) (Document, error) {
	doc, ok := f.documents[id]
	if !ok || doc.Scope != ScopeUser || doc.OwnerID != ownerID {
		return Document{}, ErrDocumentNotFound
	}
	return doc, nil
}

func (f *fakeStore) GetDocumentBySourcePath(_ context.Context, sourceID int64, relPath string) (Document, error) {
	for _, doc := range f.documents {
		if doc.SourceID == sourceID && doc.RelPath == relPath {
			return doc, nil
		}
	}
	return Document{}, ErrDocumentNotFound
}

func (f *fakeStore) GetInlineDocumentByHash(_ context.Context, ownerID int64, hash []byte) (Document, error) {
	for _, doc := range f.documents {
		if doc.Origin == OriginInline && doc.OwnerID == ownerID && bytesEqual(doc.ContentHash, hash) {
			return doc, nil
		}
	}
	return Document{}, ErrDocumentNotFound
}

func (f *fakeStore) ListVisibleDocuments(_ context.Context, ownerID int64, sourceID *int64, origin, status string, limit, offset int32) ([]Document, error) {
	out := make([]Document, 0, len(f.documents))
	for _, doc := range f.documents {
		if doc.Scope == ScopeUser && doc.OwnerID != ownerID {
			continue
		}
		if sourceID != nil && doc.SourceID != *sourceID {
			continue
		}
		if origin != "" && doc.Origin != origin {
			continue
		}
		if status != "" && doc.Status != status {
			continue
		}
		out = append(out, doc)
	}
	sortDocuments(out)
	start := int(offset)
	if start > len(out) {
		return nil, nil
	}
	end := start + int(limit)
	if end > len(out) {
		end = len(out)
	}
	return out[start:end], nil
}

func (f *fakeStore) ListDocumentsBySource(_ context.Context, sourceID int64, limit, offset int32) ([]Document, error) {
	out := make([]Document, 0, len(f.documents))
	for _, doc := range f.documents {
		if doc.SourceID == sourceID {
			out = append(out, doc)
		}
	}
	sortDocuments(out)
	start := int(offset)
	if start > len(out) {
		return nil, nil
	}
	end := start + int(limit)
	if end > len(out) {
		end = len(out)
	}
	return out[start:end], nil
}

func (f *fakeStore) ListPendingDocuments(_ context.Context, ownerID int64, limit int32) ([]Document, error) {
	out := make([]Document, 0, len(f.documents))
	for _, doc := range f.documents {
		if doc.Status != StatusPending {
			continue
		}
		if doc.Scope == ScopeUser && doc.OwnerID != ownerID {
			continue
		}
		out = append(out, doc)
	}
	sortDocuments(out)
	if int64(len(out)) > int64(limit) {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakeStore) ListDocumentsWithUnembedded(_ context.Context, ownerID int64, limit int32) ([]Document, error) {
	seen := map[int64]bool{}
	var out []Document
	for _, chunk := range f.chunks {
		if f.embedded[chunk.ID] {
			continue
		}
		doc := f.documents[chunk.DocumentID]
		if doc.Scope == ScopeUser && doc.OwnerID != ownerID {
			continue
		}
		if !seen[doc.ID] {
			seen[doc.ID] = true
			out = append(out, doc)
		}
	}
	sortDocuments(out)
	if int64(len(out)) > int64(limit) {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakeStore) UpsertFileDocument(_ context.Context, d Document) (Document, error) {
	for id, doc := range f.documents {
		if doc.SourceID == d.SourceID && doc.RelPath == d.RelPath {
			doc.ContentHash = d.ContentHash
			doc.ByteSize = d.ByteSize
			doc.Mtime = d.Mtime
			doc.Title = d.Title
			doc.DocType = d.DocType
			doc.Status = StatusPending
			doc.ErrorDetail = ""
			f.documents[id] = doc
			return doc, nil
		}
	}
	d.ID = f.nextDoc
	f.nextDoc++
	d.Status = StatusPending
	f.documents[d.ID] = d
	return d, nil
}

func (f *fakeStore) UpsertInlineDocument(_ context.Context, d Document) (Document, error) {
	for id, doc := range f.documents {
		if doc.SourceID == d.SourceID && doc.RelPath == d.RelPath {
			doc.ContentHash = d.ContentHash
			doc.ByteSize = d.ByteSize
			doc.Content = d.Content
			doc.Title = d.Title
			doc.Status = StatusPending
			doc.ErrorDetail = ""
			f.documents[id] = doc
			return doc, nil
		}
	}
	d.ID = f.nextDoc
	f.nextDoc++
	d.Status = StatusPending
	f.documents[d.ID] = d
	return d, nil
}

func (f *fakeStore) UpdateInlineDocumentContent(_ context.Context, id int64, content string, hash []byte, size int64) (Document, error) {
	doc, ok := f.documents[id]
	if !ok {
		return Document{}, ErrDocumentNotFound
	}
	doc.Content = content
	doc.ContentHash = hash
	doc.ByteSize = size
	doc.Status = StatusPending
	f.documents[id] = doc
	return doc, nil
}

func (f *fakeStore) MarkDocumentStatus(_ context.Context, id int64, status, detail string) error {
	doc, ok := f.documents[id]
	if !ok {
		return ErrDocumentNotFound
	}
	doc.Status = status
	doc.ErrorDetail = detail
	f.documents[id] = doc
	return nil
}

func (f *fakeStore) MarkDocumentPending(_ context.Context, id int64, generation int32, detail string) error {
	doc, ok := f.documents[id]
	if !ok {
		return ErrDocumentNotFound
	}
	if doc.Generation < generation {
		doc.Status = StatusPending
		doc.ErrorDetail = detail
		f.documents[id] = doc
	}
	return nil
}

func (f *fakeStore) SetDocumentTitle(_ context.Context, id int64, title string) error {
	doc, ok := f.documents[id]
	if !ok {
		return ErrDocumentNotFound
	}
	doc.Title = title
	f.documents[id] = doc
	return nil
}

func (f *fakeStore) AdvanceDocumentGeneration(_ context.Context, id int64, generation int32) (Document, error) {
	doc, ok := f.documents[id]
	if !ok {
		return Document{}, ErrDocumentNotFound
	}
	if doc.Generation >= generation {
		return doc, nil
	}
	doc.Generation = generation
	doc.Status = StatusIndexed
	doc.ErrorDetail = ""
	f.documents[id] = doc
	return doc, nil
}

func (f *fakeStore) DeleteDocument(_ context.Context, id int64) error {
	delete(f.documents, id)
	f.deletedDocs = append(f.deletedDocs, id)
	for chunkID, chunk := range f.chunks {
		if chunk.DocumentID == id {
			delete(f.chunks, chunkID)
		}
	}
	return nil
}

func (f *fakeStore) DeleteAbsentFileDocuments(_ context.Context, sourceID int64, keep []string) (int64, error) {
	keepSet := map[string]bool{}
	for _, path := range keep {
		keepSet[path] = true
	}
	var count int64
	for id, doc := range f.documents {
		if doc.SourceID == sourceID && doc.Origin == OriginFile && !keepSet[doc.RelPath] {
			delete(f.documents, id)
			for chunkID, chunk := range f.chunks {
				if chunk.DocumentID == id {
					delete(f.chunks, chunkID)
				}
			}
			count++
		}
	}
	return count, nil
}

func (f *fakeStore) InsertChunk(_ context.Context, c Chunk) (int64, error) {
	c.ID = f.nextChunk
	f.nextChunk++
	f.chunks[c.ID] = c
	f.embedded[c.ID] = false
	return c.ID, nil
}

func (f *fakeStore) ClaimUnembeddedChunks(_ context.Context, docID int64, generation int32, limit int32) ([]Chunk, error) {
	var out []Chunk
	for _, chunk := range f.chunks {
		if chunk.DocumentID == docID && chunk.Generation == generation && !f.embedded[chunk.ID] {
			out = append(out, chunk)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ordinal < out[j].Ordinal })
	if int64(len(out)) > int64(limit) {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakeStore) SetChunkEmbedding(_ context.Context, chunkID int64, _ []float32, model string) error {
	chunk, ok := f.chunks[chunkID]
	if !ok {
		return ErrDocumentNotFound
	}
	chunk.EmbeddingModel = model
	f.chunks[chunkID] = chunk
	f.embedded[chunkID] = true
	return nil
}

func (f *fakeStore) CountUnembeddedChunks(_ context.Context, docID int64, generation int32) (int64, error) {
	var count int64
	for _, chunk := range f.chunks {
		if chunk.DocumentID == docID && chunk.Generation == generation && !f.embedded[chunk.ID] {
			count++
		}
	}
	return count, nil
}

func (f *fakeStore) CountChunksAtGeneration(_ context.Context, docID int64, generation int32) (int64, error) {
	var count int64
	for _, chunk := range f.chunks {
		if chunk.DocumentID == docID && chunk.Generation == generation {
			count++
		}
	}
	return count, nil
}

func (f *fakeStore) ListDocumentHeadingPaths(_ context.Context, docID int64, generation int32) ([][]string, error) {
	seen := map[string]bool{}
	var out [][]string
	for _, chunk := range f.chunksForDoc(docID) {
		if chunk.Generation != generation {
			continue
		}
		key := strings.Join(chunk.HeadingPath, "\x00")
		if !seen[key] {
			seen[key] = true
			out = append(out, chunk.HeadingPath)
		}
	}
	sort.Slice(out, func(i, j int) bool { return strings.Join(out[i], "\x00") < strings.Join(out[j], "\x00") })
	return out, nil
}

func (f *fakeStore) DeletePendingChunks(_ context.Context, docID int64, generation int32) error {
	for id, chunk := range f.chunks {
		if chunk.DocumentID == docID && chunk.Generation > generation {
			delete(f.chunks, id)
		}
	}
	return nil
}

func (f *fakeStore) DeleteSupersededChunks(_ context.Context, docID int64, generation int32) error {
	for id, chunk := range f.chunks {
		if chunk.DocumentID == docID && chunk.Generation < generation {
			delete(f.chunks, id)
		}
	}
	return nil
}

func (f *fakeStore) DeleteChunksByDocument(_ context.Context, docID int64) error {
	for id, chunk := range f.chunks {
		if chunk.DocumentID == docID {
			delete(f.chunks, id)
		}
	}
	return nil
}

func (f *fakeStore) RetrievePassages(_ context.Context, params RetrievalParams) ([]Passage, error) {
	f.lastRetrieval = &params
	return nil, nil
}

func (f *fakeStore) chunksForDoc(docID int64) []Chunk {
	var out []Chunk
	for _, chunk := range f.chunks {
		if chunk.DocumentID == docID {
			out = append(out, chunk)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Generation != out[j].Generation {
			return out[i].Generation < out[j].Generation
		}
		return out[i].Ordinal < out[j].Ordinal
	})
	return out
}

func sortDocuments(docs []Document) {
	sort.Slice(docs, func(i, j int) bool {
		if docs[i].RelPath != docs[j].RelPath {
			return docs[i].RelPath < docs[j].RelPath
		}
		return docs[i].ID < docs[j].ID
	})
}

type countingEmbedder struct {
	calls  int
	inputs int
	model  string
	fail   bool
}

func (e *countingEmbedder) EmbedStrings(_ context.Context, inputs []string) ([][]float32, error) {
	e.calls++
	if e.fail {
		return nil, errors.New("embedding endpoint down")
	}
	e.inputs += len(inputs)
	out := make([][]float32, len(inputs))
	for i, input := range inputs {
		vec := make([]float32, 1024)
		vec[0] = float32(len(input))
		out[i] = vec
	}
	return out, nil
}

func (e *countingEmbedder) Model() string {
	if e.model == "" {
		return "stub-model"
	}
	return e.model
}

type countingNormalizer struct {
	inner Normalizer
	calls int
}

func (n *countingNormalizer) Handles(docType string) bool { return n.inner.Handles(docType) }

func (n *countingNormalizer) Normalize(ctx context.Context, raw []byte, src Source) (*NormalizedDocument, error) {
	n.calls++
	return n.inner.Normalize(ctx, raw, src)
}

func newTestIndexer(store Store, embedder Embedder) *Indexer {
	indexer := NewIndexer(store, embedder, ChunkOptions{TargetTokens: 100, MaxTokens: 200, MinTokens: 5, OverlapTokens: 0}, ConnectorOptions{
		MaxFileBytes: 1 << 20,
		MaxFiles:     100,
		MaxDepth:     10,
	})
	indexer.SetNormalizers(&countingNormalizer{inner: NewMarkdownNormalizer()})
	return indexer
}

func sharedDirSource(t *testing.T, store *fakeStore, root string) Source {
	t.Helper()
	src, err := store.UpsertSharedSource(context.Background(), "shared", root)
	require.NoError(t, err)
	return src
}

func TestIndexerUnchangedFileCostsNoEmbeddingCall(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.md"), []byte("# A\n\nhello world\n"), 0o600))
	store := newFakeStore()
	src := sharedDirSource(t, store, root)
	embedder := &countingEmbedder{}
	indexer := newTestIndexer(store, embedder)

	first, err := indexer.ScanSource(context.Background(), src)
	require.NoError(t, err)
	assert.Equal(t, 1, first.Documents)
	assert.Greater(t, first.Chunks, 0)
	assert.Greater(t, embedder.calls, 0)

	before := len(store.chunks)
	embedder.calls = 0
	second, err := indexer.ScanSource(context.Background(), src)
	require.NoError(t, err)
	assert.Equal(t, 0, second.Documents)
	assert.Equal(t, 0, second.Chunks)
	assert.Equal(t, 0, embedder.calls)
	assert.Len(t, store.chunks, before)
}

func TestIndexerChangedFileReplacesChunks(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "a.md")
	require.NoError(t, os.WriteFile(path, []byte("# A\n\nalpha content\n"), 0o600))
	store := newFakeStore()
	src := sharedDirSource(t, store, root)
	embedder := &countingEmbedder{}
	indexer := newTestIndexer(store, embedder)

	_, err := indexer.ScanSource(context.Background(), src)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, []byte("# A\n\nbeta replacement content\n"), 0o600))
	result, err := indexer.ScanSource(context.Background(), src)
	require.NoError(t, err)
	assert.Equal(t, 1, result.Documents)

	contents := make([]string, 0, len(store.chunks))
	for _, chunk := range store.chunks {
		contents = append(contents, chunk.Content)
	}
	joined := ""
	for _, content := range contents {
		joined += content
	}
	assert.Contains(t, joined, "beta replacement content")
	assert.NotContains(t, joined, "alpha content")
}

func TestIndexerDeletedFileStopsBeingIndexed(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "a.md")
	require.NoError(t, os.WriteFile(path, []byte("# A\n\nbody\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "keep.md"), []byte("# Keep\n\nkeep\n"), 0o600))
	store := newFakeStore()
	src := sharedDirSource(t, store, root)
	indexer := newTestIndexer(store, &countingEmbedder{})

	_, err := indexer.ScanSource(context.Background(), src)
	require.NoError(t, err)
	_, err = indexer.ScanSource(context.Background(), src)
	require.NoError(t, err)
	require.Len(t, store.documents, 2)

	require.NoError(t, os.Remove(path))
	_, err = indexer.ScanSource(context.Background(), src)
	require.NoError(t, err)
	require.Len(t, store.documents, 1)
	for _, doc := range store.documents {
		assert.Equal(t, "keep.md", doc.RelPath)
	}
}

func TestIndexerSkipsUnsupportedFileAndContinues(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.md"), []byte("# A\n\nbody\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "b.png"), []byte{0x89, 0x50, 0x4e, 0x47}, 0o600))
	store := newFakeStore()
	src := sharedDirSource(t, store, root)
	indexer := newTestIndexer(store, &countingEmbedder{})

	result, err := indexer.ScanSource(context.Background(), src)
	require.NoError(t, err)
	assert.Equal(t, 1, result.Documents)
	require.NotEmpty(t, result.Skipped)
	assert.Equal(t, "b.png", result.Skipped[0].RelPath)
	assert.Len(t, store.documents, 1)
}

func TestIndexerRecordsEmbeddingIdentityOnEveryChunk(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.md"), []byte("# A\n\none\n\n## B\n\ntwo\n"), 0o600))
	store := newFakeStore()
	src := sharedDirSource(t, store, root)
	indexer := newTestIndexer(store, &countingEmbedder{model: "model-x"})

	_, err := indexer.ScanSource(context.Background(), src)
	require.NoError(t, err)
	require.NotEmpty(t, store.chunks)
	for _, chunk := range store.chunks {
		assert.Equal(t, "model-x", chunk.EmbeddingModel)
	}
}

func TestIndexerFailedEmbedKeepsPreviousGenerationRetrievable(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "a.md")
	require.NoError(t, os.WriteFile(path, []byte("# A\n\nversion one\n"), 0o600))
	store := newFakeStore()
	src := sharedDirSource(t, store, root)
	embedder := &countingEmbedder{}
	indexer := newTestIndexer(store, embedder)
	normalizer := indexer.normalizers[0].(*countingNormalizer)

	_, err := indexer.ScanSource(context.Background(), src)
	require.NoError(t, err)
	doc := store.documents[1]
	require.Equal(t, StatusIndexed, doc.Status)
	require.Equal(t, int32(1), doc.Generation)
	oldContents := chunkContents(store.chunksForDoc(1))

	require.NoError(t, os.WriteFile(path, []byte("# A\n\nversion two replacement\n"), 0o600))
	embedder.fail = true
	_, err = indexer.ScanSource(context.Background(), src)
	require.NoError(t, err)
	doc = store.documents[1]
	assert.Equal(t, StatusPending, doc.Status)
	assert.Equal(t, int32(1), doc.Generation)

	var generationOne []string
	for _, chunk := range store.chunksForDoc(1) {
		if chunk.Generation == 1 {
			generationOne = append(generationOne, chunk.Content)
		}
	}
	assert.ElementsMatch(t, oldContents, generationOne)

	parseCalls := normalizer.calls
	embedder.fail = false
	_, err = indexer.ScanSource(context.Background(), src)
	require.NoError(t, err)
	assert.Equal(t, parseCalls, normalizer.calls)
	doc = store.documents[1]
	assert.Equal(t, StatusIndexed, doc.Status)
	assert.Equal(t, int32(2), doc.Generation)
	for _, chunk := range store.chunksForDoc(1) {
		assert.Equal(t, int32(2), chunk.Generation)
	}
}

func TestIndexerInlinePathKeepsDocumentIdentity(t *testing.T) {
	store := newFakeStore()
	src, err := store.EnsureInlineSource(context.Background(), 7)
	require.NoError(t, err)
	body := "# Notes\n\nfirst body\n"
	doc, err := store.UpsertInlineDocument(context.Background(), Document{
		SourceID:    src.ID,
		Scope:       ScopeUser,
		OwnerID:     7,
		Origin:      OriginInline,
		RelPath:     "notes.md",
		Title:       "Notes",
		DocType:     DocTypeMarkdown,
		ContentHash: hashBytes([]byte(body)),
		ByteSize:    int64(len(body)),
		Content:     body,
	})
	require.NoError(t, err)
	indexer := newTestIndexer(store, &countingEmbedder{})

	_, err = indexer.IndexInline(context.Background(), doc)
	require.NoError(t, err)
	require.NotEmpty(t, store.chunks)
	for _, chunk := range store.chunksForDoc(doc.ID) {
		assert.Equal(t, doc.ID, chunk.DocumentID)
		assert.Equal(t, src.ID, chunk.SourceID)
		assert.Equal(t, ScopeUser, chunk.Scope)
		assert.Equal(t, int64(7), chunk.OwnerID)
		assert.Equal(t, int32(1), chunk.Generation)
	}

	newBody := "# Notes\n\nsecond body with more words\n"
	updated, err := store.UpdateInlineDocumentContent(context.Background(), doc.ID, newBody, hashBytes([]byte(newBody)), int64(len(newBody)))
	require.NoError(t, err)
	_, err = indexer.IndexInline(context.Background(), updated)
	require.NoError(t, err)
	after := store.documents[doc.ID]
	assert.Equal(t, doc.ID, after.ID)
	assert.Equal(t, int32(2), after.Generation)
	for _, chunk := range store.chunksForDoc(doc.ID) {
		assert.Equal(t, int32(2), chunk.Generation)
		assert.Contains(t, chunk.Content, "second body")
	}
}

func chunkContents(chunks []Chunk) []string {
	out := make([]string, len(chunks))
	for i, chunk := range chunks {
		out[i] = chunk.Content
	}
	return out
}

func TestIndexerUnreadableRootPreservesDocuments(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read a 0000 directory; the unreadable-root path needs an unprivileged user")
	}
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.md"), []byte("# A\n\na\n"), 0o600))
	store := newFakeStore()
	src := sharedDirSource(t, store, root)
	indexer := newTestIndexer(store, &countingEmbedder{})

	first, err := indexer.ScanSource(context.Background(), src)
	require.NoError(t, err)
	require.Equal(t, 1, first.Documents)

	require.NoError(t, os.Chmod(root, 0))
	t.Cleanup(func() {
		//nolint:gosec // G302: the directory must be readable again for TempDir cleanup.
		_ = os.Chmod(root, 0o700)
	})

	_, err = indexer.ScanSource(context.Background(), src)
	require.Error(t, err)
	assert.Len(t, store.documents, 1, "an unreadable root must not delete the source's documents")
}

func TestIndexerEmptyRootKeepsDocuments(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "a.md")
	require.NoError(t, os.WriteFile(file, []byte("# A\n\na\n"), 0o600))
	store := newFakeStore()
	src := sharedDirSource(t, store, root)
	indexer := newTestIndexer(store, &countingEmbedder{})

	first, err := indexer.ScanSource(context.Background(), src)
	require.NoError(t, err)
	require.Equal(t, 1, first.Documents)
	require.Len(t, store.documents, 1)

	require.NoError(t, os.Remove(file))
	_, err = indexer.ScanSource(context.Background(), src)
	require.NoError(t, err)
	assert.Len(t, store.documents, 1, "an empty root may be a transient mount failure")

	require.NoError(t, os.WriteFile(filepath.Join(root, "b.md"), []byte("# B\n\nb\n"), 0o600))
	_, err = indexer.ScanSource(context.Background(), src)
	require.NoError(t, err)
	assert.Len(t, store.documents, 1, "a scan with a candidate must delete the absent document")
	for _, doc := range store.documents {
		assert.Equal(t, "b.md", doc.RelPath)
	}
}

func TestIndexerCountLimitedScanDoesNotDeleteBeyondLimit(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.md"), []byte("# A\n\na\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "b.md"), []byte("# B\n\nb\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "c.md"), []byte("# C\n\nc\n"), 0o600))
	store := newFakeStore()
	src := sharedDirSource(t, store, root)
	require.NoError(t, os.WriteFile(filepath.Join(root, "c.md"), []byte("# C\n\nold c\n"), 0o600))
	existing, err := store.UpsertFileDocument(context.Background(), Document{
		SourceID: src.ID, Scope: ScopeShared, Origin: OriginFile,
		RelPath: "c.md", Title: "c", DocType: DocTypeMarkdown,
		ContentHash: hashBytes([]byte("# C\n\nold c\n")),
	})
	require.NoError(t, err)

	indexer := newTestIndexer(store, &countingEmbedder{})
	indexer.connectorOpts.MaxFiles = 1
	_, err = indexer.ScanSource(context.Background(), src)
	require.NoError(t, err)

	_, stillThere := store.documents[existing.ID]
	assert.True(t, stillThere, "a count-limited scan must not delete documents beyond the walk")
}
