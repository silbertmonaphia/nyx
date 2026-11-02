package kb

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace/noop"
)

func testConfig(privateBase string) Config {
	return Config{
		Enabled:          true,
		PrivateBase:      privateBase,
		SharedSourceName: "shared",
		Chunk:            ChunkOptions{TargetTokens: 100, MaxTokens: 200, MinTokens: 5, OverlapTokens: 0},
		TopK:             5,
		MaxContextChars:  4000,
		IndexConcurrency: 1,
	}
}

func newTestService(store *fakeStore, embedder Embedder, cfg Config) *Service {
	indexer := newTestIndexer(store, embedder)
	return NewService(store, indexer, cfg, embedder, noop.NewTracerProvider().Tracer("test"))
}

func TestServiceRegisterSourceAcceptsRootInsideBase(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "alice")
	require.NoError(t, os.MkdirAll(root, 0o750))
	store := newFakeStore()
	service := newTestService(store, &countingEmbedder{}, testConfig(base))

	src, err := service.RegisterSource(context.Background(), 1, "my notes", root)
	require.NoError(t, err)
	assert.Equal(t, ScopeUser, src.Scope)
	assert.Equal(t, int64(1), src.OwnerID)
	assert.Equal(t, KindDir, src.Kind)
	assert.Equal(t, root, src.RootPath)
}

func TestServiceRegisterSourceRefusalsDoNotNamePaths(t *testing.T) {
	base := t.TempDir()
	inside := filepath.Join(base, "alice")
	require.NoError(t, os.MkdirAll(inside, 0o750))
	outside := t.TempDir()
	store := newFakeStore()
	service := newTestService(store, &countingEmbedder{}, testConfig(base))

	cases := []string{outside, filepath.Join(base, "missing"), base}
	for _, root := range cases {
		_, err := service.RegisterSource(context.Background(), 1, "mine", root)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrInvalidSourceRoot)
		assert.NotContains(t, err.Error(), base)
		assert.NotContains(t, err.Error(), outside)
		assert.Empty(t, store.sources)
	}
}

func TestServiceRegisterSourceRequiresPrivateBase(t *testing.T) {
	root := t.TempDir()
	store := newFakeStore()
	service := newTestService(store, &countingEmbedder{}, testConfig(""))

	_, err := service.RegisterSource(context.Background(), 1, "mine", root)
	assert.ErrorIs(t, err, ErrPrivateBaseNotSetup)
}

func TestServiceProvisionSharedIsIdempotent(t *testing.T) {
	root := t.TempDir()
	store := newFakeStore()
	cfg := testConfig("")
	cfg.Root = root
	service := newTestService(store, &countingEmbedder{}, cfg)

	first, err := service.ProvisionShared(context.Background())
	require.NoError(t, err)
	second, err := service.ProvisionShared(context.Background())
	require.NoError(t, err)
	assert.Equal(t, first.ID, second.ID)
	assert.Len(t, store.sources, 1)
	assert.Equal(t, ScopeShared, second.Scope)
	assert.Equal(t, int64(0), second.OwnerID)
	assert.Equal(t, KindDir, second.Kind)
}

func TestServiceProvisionInlineHasNoRootPath(t *testing.T) {
	store := newFakeStore()
	service := newTestService(store, &countingEmbedder{}, testConfig(""))

	first, err := service.ProvisionInline(context.Background(), 9)
	require.NoError(t, err)
	second, err := service.ProvisionInline(context.Background(), 9)
	require.NoError(t, err)
	assert.Equal(t, first.ID, second.ID)
	assert.Equal(t, KindInline, second.Kind)
	assert.Empty(t, second.RootPath)
}

func TestServiceListSourcesReportsVisibleCounts(t *testing.T) {
	store := newFakeStore()
	shared, err := store.UpsertSharedSource(context.Background(), "shared", t.TempDir())
	require.NoError(t, err)
	private, err := store.UpsertUserDirSource(context.Background(), 1, "mine", t.TempDir())
	require.NoError(t, err)
	_, err = store.UpsertUserDirSource(context.Background(), 2, "other", t.TempDir())
	require.NoError(t, err)

	doc, err := store.UpsertFileDocument(context.Background(), Document{
		SourceID: private.ID, Scope: ScopeUser, OwnerID: 1, Origin: OriginFile,
		RelPath: "dir/a.md", Title: "a", DocType: DocTypeMarkdown, ContentHash: hashBytes([]byte("a")),
	})
	require.NoError(t, err)
	_, err = store.InsertChunk(context.Background(), Chunk{
		DocumentID: doc.ID, Scope: ScopeUser, OwnerID: 1, SourceID: private.ID,
		Generation: 1, Ordinal: 0, HeadingPath: []string{"H"}, Content: "body", EmbedText: "e",
		ContentHash: hashBytes([]byte("body")), EmbeddingModel: "model-x",
	})
	require.NoError(t, err)
	sharedDoc, err := store.UpsertFileDocument(context.Background(), Document{
		SourceID: shared.ID, Scope: ScopeShared, Origin: OriginFile,
		RelPath: "public.md", Title: "p", DocType: DocTypeMarkdown, ContentHash: hashBytes([]byte("p")),
	})
	require.NoError(t, err)
	_, err = store.InsertChunk(context.Background(), Chunk{
		DocumentID: sharedDoc.ID, Scope: ScopeShared, SourceID: shared.ID,
		Generation: 1, Content: "shared", EmbedText: "s", ContentHash: hashBytes([]byte("s")),
	})
	require.NoError(t, err)

	service := newTestService(store, &countingEmbedder{}, testConfig(""))
	infos, err := service.ListSources(context.Background(), 1)
	require.NoError(t, err)
	require.Len(t, infos, 2)
	seen := map[string]SourceInfo{}
	for _, info := range infos {
		seen[info.Name] = info
	}
	assert.Equal(t, int64(1), seen["mine"].Stats.Documents)
	assert.Equal(t, int64(1), seen["mine"].Stats.Chunks)
	assert.Equal(t, []string{"dir"}, seen["mine"].TopDirs)
	assert.Equal(t, []string{"model-x"}, seen["mine"].IndexedModels)
	assert.Equal(t, int64(1), seen["shared"].Stats.Chunks)
}

func TestServiceDeleteSourceCascadesAndProtectsShared(t *testing.T) {
	store := newFakeStore()
	private, err := store.UpsertUserDirSource(context.Background(), 1, "mine", t.TempDir())
	require.NoError(t, err)
	shared, err := store.UpsertSharedSource(context.Background(), "shared", t.TempDir())
	require.NoError(t, err)
	doc, err := store.UpsertFileDocument(context.Background(), Document{
		SourceID: private.ID, Scope: ScopeUser, OwnerID: 1, Origin: OriginFile,
		RelPath: "a.md", Title: "a", DocType: DocTypeMarkdown, ContentHash: hashBytes([]byte("a")),
	})
	require.NoError(t, err)
	_, err = store.InsertChunk(context.Background(), Chunk{
		DocumentID: doc.ID, Scope: ScopeUser, OwnerID: 1, SourceID: private.ID,
		Generation: 1, Content: "body", EmbedText: "e", ContentHash: hashBytes([]byte("body")),
	})
	require.NoError(t, err)

	service := newTestService(store, &countingEmbedder{}, testConfig(""))
	require.NoError(t, service.DeleteSource(context.Background(), 1, private.ID))
	assert.Empty(t, store.documents)
	assert.Empty(t, store.chunks)

	assert.ErrorIs(t, service.DeleteSource(context.Background(), 1, shared.ID), ErrSharedSourceLocked)
	assert.NotEmpty(t, store.sources)
}

func TestServiceDeleteSourceRefusesAnotherOwner(t *testing.T) {
	store := newFakeStore()
	private, err := store.UpsertUserDirSource(context.Background(), 1, "mine", t.TempDir())
	require.NoError(t, err)
	service := newTestService(store, &countingEmbedder{}, testConfig(""))

	err = service.DeleteSource(context.Background(), 2, private.ID)
	assert.ErrorIs(t, err, ErrSourceNotFound)
	assert.NotEmpty(t, store.sources)
}

func TestServiceIndexVisibleSelectsCallerSources(t *testing.T) {
	base := t.TempDir()
	sharedRoot := t.TempDir()
	writeFile(t, filepath.Join(sharedRoot, "shared.md"), "# Shared\n\nshared body\n")
	aliceRoot := filepath.Join(base, "alice")
	bobRoot := filepath.Join(base, "bob")
	writeFile(t, filepath.Join(aliceRoot, "alice.md"), "# Alice\n\nalice body\n")
	writeFile(t, filepath.Join(bobRoot, "bob.md"), "# Bob\n\nbob body\n")

	store := newFakeStore()
	shared, err := store.UpsertSharedSource(context.Background(), "shared", sharedRoot)
	require.NoError(t, err)
	alice, err := store.UpsertUserDirSource(context.Background(), 1, "alice", aliceRoot)
	require.NoError(t, err)
	bob, err := store.UpsertUserDirSource(context.Background(), 2, "bob", bobRoot)
	require.NoError(t, err)

	service := newTestService(store, &countingEmbedder{}, testConfig(base))
	report, err := service.IndexVisible(context.Background(), 1)
	require.NoError(t, err)
	require.Len(t, report.Sources, 2)
	ids := []int64{report.Sources[0].SourceID, report.Sources[1].SourceID}
	assert.ElementsMatch(t, []int64{shared.ID, alice.ID}, ids)

	for _, doc := range store.documents {
		assert.NotEqual(t, bob.ID, doc.SourceID)
	}
	assert.Len(t, store.documents, 2)
}

func TestServiceIndexVisibleRefusesEscapingSourceRoot(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "alice")
	writeFile(t, filepath.Join(root, "note.md"), "# Note\n\nbody\n")
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "secret.md"), "# Secret\n\nsecret body\n")

	store := newFakeStore()
	src, err := store.UpsertUserDirSource(context.Background(), 1, "alice", root)
	require.NoError(t, err)
	require.NoError(t, os.RemoveAll(root))
	require.NoError(t, os.Symlink(outside, root))

	service := newTestService(store, &countingEmbedder{}, testConfig(base))
	report, err := service.IndexVisible(context.Background(), 1)
	require.NoError(t, err)
	require.Len(t, report.Sources, 1)
	assert.ErrorIs(t, report.Sources[0].Err, ErrInvalidSourceRoot)
	assert.Empty(t, store.documents)
	assert.Equal(t, src.ID, report.Sources[0].SourceID)
}

func pendingInlineDoc(t *testing.T, store *fakeStore, ownerID int64, relPath string) Document {
	t.Helper()
	src, err := store.EnsureInlineSource(context.Background(), ownerID)
	require.NoError(t, err)
	body := "# Doc\n\n" + relPath + " body\n"
	doc, err := store.UpsertInlineDocument(context.Background(), Document{
		SourceID: src.ID, Scope: ScopeUser, OwnerID: ownerID, Origin: OriginInline,
		RelPath: relPath, Title: relPath, DocType: DocTypeMarkdown,
		ContentHash: hashBytes([]byte(body)), ByteSize: int64(len(body)), Content: body,
	})
	require.NoError(t, err)
	_, err = store.InsertChunk(context.Background(), Chunk{
		DocumentID: doc.ID, Scope: ScopeUser, OwnerID: ownerID, SourceID: src.ID,
		Generation: 1, Ordinal: 0, HeadingPath: []string{"Doc"}, Content: body,
		EmbedText: relPath + "\n\n" + body, ContentHash: hashBytes([]byte(body)),
		EmbeddingModel: "stub-model", TokenCount: 10,
	})
	require.NoError(t, err)
	return doc
}

func TestServiceCatchUpIsBounded(t *testing.T) {
	store := newFakeStore()
	for i := 0; i < 25; i++ {
		pendingInlineDoc(t, store, 1, "doc-"+string(rune('a'+i))+".md")
	}
	service := newTestService(store, &countingEmbedder{}, testConfig(""))
	service.CatchUpOnce(context.Background(), 1)

	indexed := 0
	for _, doc := range store.documents {
		if doc.Status == StatusIndexed {
			indexed++
		}
	}
	assert.LessOrEqual(t, indexed, 2*catchUpBatch)
	assert.Greater(t, indexed, 0)
}

func TestServiceCatchUpFailureLeavesDocumentPending(t *testing.T) {
	store := newFakeStore()
	doc := pendingInlineDoc(t, store, 1, "doc.md")
	embedder := &countingEmbedder{fail: true}
	service := newTestService(store, embedder, testConfig(""))
	service.CatchUpOnce(context.Background(), 1)

	stored := store.documents[doc.ID]
	assert.Equal(t, StatusPending, stored.Status)
}

func TestServiceInlineCreateResetsConsumedCatchUp(t *testing.T) {
	store := newFakeStore()
	embedder := &countingEmbedder{}
	service := newTestService(store, embedder, testConfig(""))
	service.CatchUpOnce(context.Background(), 1)

	embedder.fail = true
	created, err := service.CreateInlineDocument(context.Background(), 1, "notes.md", "# Notes\n\nbody\n")
	require.NoError(t, err)
	require.Equal(t, StatusPending, created.Status)

	embedder.fail = false
	service.CatchUpOnce(context.Background(), 1)
	assert.Equal(t, StatusIndexed, store.documents[created.ID].Status)
}

func TestServiceInlineUpdateResetsConsumedCatchUp(t *testing.T) {
	store := newFakeStore()
	embedder := &countingEmbedder{}
	service := newTestService(store, embedder, testConfig(""))
	created, err := service.CreateInlineDocument(context.Background(), 1, "notes.md", "# Notes\n\nfirst body\n")
	require.NoError(t, err)
	require.Equal(t, StatusIndexed, created.Status)
	service.CatchUpOnce(context.Background(), 1)

	embedder.fail = true
	updated, err := service.UpdateInlineDocument(context.Background(), 1, created.ID, "# Notes\n\nsecond body\n")
	require.NoError(t, err)
	require.Equal(t, StatusPending, updated.Status)

	embedder.fail = false
	service.CatchUpOnce(context.Background(), 1)
	assert.Equal(t, StatusIndexed, store.documents[created.ID].Status)
}

func TestServiceInlineCreateEditDelete(t *testing.T) {
	store := newFakeStore()
	service := newTestService(store, &countingEmbedder{}, testConfig(""))

	created, err := service.CreateInlineDocument(context.Background(), 1, "notes.md", "# Notes\n\nfirst body\n")
	require.NoError(t, err)
	assert.Equal(t, OriginInline, created.Origin)
	assert.Equal(t, int64(1), created.OwnerID)
	assert.Equal(t, StatusIndexed, created.Status)
	require.NotEmpty(t, store.chunksForDoc(created.ID))
	chunkCount := len(store.chunksForDoc(created.ID))

	reuploaded, err := service.CreateInlineDocument(context.Background(), 1, "copy.md", "# Notes\n\nfirst body\n")
	require.NoError(t, err)
	assert.Equal(t, created.ID, reuploaded.ID)
	assert.Len(t, store.chunksForDoc(created.ID), chunkCount)

	edited, err := service.UpdateInlineDocument(context.Background(), 1, created.ID, "# Notes\n\nsecond body\n")
	require.NoError(t, err)
	assert.Equal(t, created.ID, edited.ID)
	assert.Equal(t, int32(2), edited.Generation)
	for _, chunk := range store.chunksForDoc(created.ID) {
		assert.Contains(t, chunk.Content, "second body")
		assert.NotContains(t, chunk.Content, "first body")
	}

	require.NoError(t, service.DeleteInlineDocument(context.Background(), 1, created.ID))
	_, ok := store.documents[created.ID]
	assert.False(t, ok)
	assert.Empty(t, store.chunksForDoc(created.ID))
}

func TestServiceInlineEditRejectsFileBackedDocument(t *testing.T) {
	store := newFakeStore()
	src, err := store.UpsertUserDirSource(context.Background(), 1, "mine", t.TempDir())
	require.NoError(t, err)
	doc, err := store.UpsertFileDocument(context.Background(), Document{
		SourceID: src.ID, Scope: ScopeUser, OwnerID: 1, Origin: OriginFile,
		RelPath: "disk.md", Title: "disk", DocType: DocTypeMarkdown,
		ContentHash: hashBytes([]byte("# Disk\n\nfile body\n")),
	})
	require.NoError(t, err)
	_, err = store.InsertChunk(context.Background(), Chunk{
		DocumentID: doc.ID, Scope: ScopeUser, OwnerID: 1, SourceID: src.ID,
		Generation: 1, Content: "file body", EmbedText: "e", ContentHash: hashBytes([]byte("file body")),
	})
	require.NoError(t, err)
	service := newTestService(store, &countingEmbedder{}, testConfig(""))

	_, err = service.UpdateInlineDocument(context.Background(), 1, doc.ID, "new body")
	assert.ErrorIs(t, err, ErrFileBackedDocument)
	err = service.DeleteInlineDocument(context.Background(), 1, doc.ID)
	assert.ErrorIs(t, err, ErrFileBackedDocument)

	stored := store.documents[doc.ID]
	assert.Equal(t, OriginFile, stored.Origin)
	assert.Len(t, store.chunksForDoc(doc.ID), 1)
}

func TestServiceInlineDocumentIsolatedPerOwner(t *testing.T) {
	store := newFakeStore()
	service := newTestService(store, &countingEmbedder{}, testConfig(""))
	created, err := service.CreateInlineDocument(context.Background(), 1, "notes.md", "# Notes\n\nbody\n")
	require.NoError(t, err)

	_, err = service.GetDocument(context.Background(), 2, created.ID)
	assert.ErrorIs(t, err, ErrDocumentNotFound)
	_, err = service.UpdateInlineDocument(context.Background(), 2, created.ID, "stolen")
	assert.ErrorIs(t, err, ErrDocumentNotFound)
	err = service.DeleteInlineDocument(context.Background(), 2, created.ID)
	assert.ErrorIs(t, err, ErrDocumentNotFound)
	assert.NotEmpty(t, store.documents)
}

func TestServiceOutlineCountsAndNoPaths(t *testing.T) {
	store := newFakeStore()
	sharedRoot := t.TempDir()
	shared, err := store.UpsertSharedSource(context.Background(), "shared", sharedRoot)
	require.NoError(t, err)
	for _, path := range []string{"B-Cook/a.md", "B-Sport/b.md", "top.md"} {
		doc, upsertErr := store.UpsertFileDocument(context.Background(), Document{
			SourceID: shared.ID, Scope: ScopeShared, Origin: OriginFile,
			RelPath: path, Title: path, DocType: DocTypeMarkdown,
			ContentHash: hashBytes([]byte(path)),
		})
		require.NoError(t, upsertErr)
		if path != "top.md" {
			_, chunkErr := store.InsertChunk(context.Background(), Chunk{
				DocumentID: doc.ID, Scope: ScopeShared, SourceID: shared.ID,
				Generation: 1, Content: "body", EmbedText: "e", ContentHash: hashBytes([]byte("body")),
			})
			require.NoError(t, chunkErr)
		}
	}

	service := newTestService(store, &countingEmbedder{}, testConfig(""))
	outline, err := service.Outline(context.Background(), 1)
	require.NoError(t, err)
	require.Len(t, outline.Sources, 1)
	assert.Equal(t, int64(3), outline.Sources[0].Documents)
	assert.Equal(t, int64(2), outline.Sources[0].Chunks)
	assert.Equal(t, []string{"B-Cook", "B-Sport"}, outline.Sources[0].Directories)

	raw, err := json.Marshal(outline)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), sharedRoot)
}

func TestServiceRetrievePassesScopeAndFilters(t *testing.T) {
	store := newFakeStore()
	service := newTestService(store, &countingEmbedder{}, testConfig(""))
	sourceID := int64(12)
	_, err := service.Retrieve(context.Background(), 5, []float32{0.1, 0.2}, "model-y", 7, &sourceID, "B-Sport/")
	require.NoError(t, err)
	require.NotNil(t, store.lastRetrieval)
	assert.Equal(t, int64(5), store.lastRetrieval.UserID)
	assert.Equal(t, "model-y", store.lastRetrieval.EmbeddingModel)
	assert.Equal(t, 7, store.lastRetrieval.Limit)
	require.NotNil(t, store.lastRetrieval.SourceID)
	assert.Equal(t, sourceID, *store.lastRetrieval.SourceID)
	assert.Equal(t, "B-Sport/", store.lastRetrieval.PathPrefix)
}

func TestServiceSettingsReportsBaseAndIdentity(t *testing.T) {
	base := t.TempDir()
	service := newTestService(newFakeStore(), &countingEmbedder{model: "model-z"}, testConfig(base))
	settings := service.Settings(context.Background(), 3)
	assert.True(t, settings.Enabled)
	assert.Equal(t, base, settings.PrivateBase)
	assert.Equal(t, "model-z", settings.EmbeddingModel)
}

func TestSanitizeDocumentName(t *testing.T) {
	cases := map[string]string{
		"notes.md":           "notes.md",
		"dir/notes.md":       "notes.md",
		"..\\evil.md":        "evil.md",
		"plain":              "plain.md",
		"with.dots.markdown": "with.dots.markdown",
	}
	for input, want := range cases {
		got, err := sanitizeDocumentName(input)
		require.NoError(t, err, input)
		assert.Equal(t, want, got, input)
	}
	for _, bad := range []string{"", " ", ".", "..", "dir/"} {
		_, err := sanitizeDocumentName(bad)
		assert.ErrorIs(t, err, ErrInvalidDocName, bad)
	}
}

type countingIngester struct {
	scans int
}

func (c *countingIngester) ScanSource(_ context.Context, _ Source) (ScanResult, error) {
	c.scans++
	return ScanResult{}, nil
}

func (c *countingIngester) IndexInline(_ context.Context, _ Document) (int, error) {
	return 0, nil
}

func (c *countingIngester) FillDocument(_ context.Context, _ Document) (int, error) {
	return 0, nil
}

func TestServiceIndexVisibleAndIndexSourceSharePipeline(t *testing.T) {
	store := newFakeStore()
	src, err := store.UpsertSharedSource(context.Background(), "shared", t.TempDir())
	require.NoError(t, err)
	ingester := &countingIngester{}
	service := NewService(store, ingester, testConfig(""), nil, noop.NewTracerProvider().Tracer("test"))

	_, err = service.IndexVisible(context.Background(), 1)
	require.NoError(t, err)
	assert.Equal(t, 1, ingester.scans)

	_, err = service.IndexSource(context.Background(), src)
	require.NoError(t, err)
	assert.Equal(t, 2, ingester.scans)
}
