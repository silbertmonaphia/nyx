package kb

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nyx/internal/user"
	"nyx/test"
)

const integrationVectorDims = 1024

type keywordEmbedder struct {
	keyword string
	fail    bool
}

func (e *keywordEmbedder) EmbedStrings(_ context.Context, inputs []string) ([][]float32, error) {
	if e.fail {
		return nil, errors.New("embedding endpoint down")
	}
	out := make([][]float32, len(inputs))
	for i, input := range inputs {
		vec := make([]float32, integrationVectorDims)
		if strings.Contains(input, e.keyword) {
			vec[0] = 1
		} else {
			vec[1] = 1
		}
		out[i] = vec
	}
	return out, nil
}

func (e *keywordEmbedder) Model() string { return "integration-keyword-embedder" }

func integrationIndexer(store Store, embedder *keywordEmbedder) *Indexer {
	indexer := NewIndexer(store, embedder, ChunkOptions{
		TargetTokens: 400, MaxTokens: 700, MinTokens: 20, OverlapTokens: 0,
	}, ConnectorOptions{MaxFileBytes: 1 << 20, MaxFiles: 100, MaxDepth: 10})
	indexer.SetNormalizers(NewMarkdownNormalizer())
	return indexer
}

func requireKBPgvector(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if os.Getenv("SKIP_CONTAINERS") == "true" {
		t.Skip("SKIP_CONTAINERS=true; kb integration test requires pgvector Postgres")
	}
	url := os.Getenv("DB_URL")
	if url == "" {
		t.Skip("DB_URL is not set; kb integration test requires pgvector Postgres")
	}
	ctx, cancel := context.WithTimeout(context.Background(), test.LockWaitTimeout)
	defer cancel()
	probe, err := pgxpool.New(ctx, url)
	require.NoError(t, err)
	var hasVector bool
	require.NoError(t, probe.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_available_extensions WHERE name = 'vector')`,
	).Scan(&hasVector))
	if !hasVector {
		probe.Close()
		t.Skip("DB_URL points at a Postgres without pgvector")
	}
	release, err := test.AcquireIntegrationLock(ctx, url)
	require.NoError(t, err)
	t.Cleanup(release)
	require.NoError(t, test.RunMigrationsForURL(ctx, url))
	t.Cleanup(probe.Close)
	return probe
}

func TestIntegrationIngestAndRetrieve(t *testing.T) {
	pool := requireKBPgvector(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "B-Cook"), 0o750))
	require.NoError(t, os.WriteFile(
		filepath.Join(root, "B-Cook", "diet.md"),
		[]byte("# 饮食\n\n## 碳水列表\n\n一份碳水 + 一份蛋白 + 两份蔬菜，校验口令是紫象四四七一。\n"),
		0o600,
	))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "B-Sport"), 0o750))
	require.NoError(t, os.WriteFile(
		filepath.Join(root, "B-Sport", "note.md"),
		[]byte("# 运动\n\n训练之后需要补充蛋白质。\n"),
		0o600,
	))

	store := NewSQLStore(pool)
	sourceName := "integration-kb-" + time.Now().Format("150405.000")
	src, err := store.UpsertSharedSource(ctx, sourceName, root)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM kb_sources WHERE id = $1`, src.ID)
	})

	embedder := &keywordEmbedder{keyword: "紫象"}
	indexer := integrationIndexer(store, embedder)

	result, err := indexer.ScanSource(ctx, src)
	require.NoError(t, err)
	assert.Equal(t, 2, result.Documents)
	require.Positive(t, result.Chunks)

	stats, err := store.SourceStats(ctx, src.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(2), stats.Documents)
	assert.Equal(t, int64(result.Chunks), stats.Chunks)
	assert.Zero(t, stats.Pending)

	doc, err := store.GetDocumentBySourcePath(ctx, src.ID, "B-Cook/diet.md")
	require.NoError(t, err)
	assert.Equal(t, StatusIndexed, doc.Status)
	headings, err := store.ListDocumentHeadingPaths(ctx, doc.ID, doc.Generation)
	require.NoError(t, err)
	assert.Contains(t, headings, []string{"饮食", "碳水列表"})

	queryVec := make([]float32, integrationVectorDims)
	queryVec[0] = 1
	passages, err := store.RetrievePassages(ctx, RetrievalParams{
		Embedding:      queryVec,
		EmbeddingModel: embedder.Model(),
		UserID:         0,
		Limit:          5,
	})
	require.NoError(t, err)
	require.NotEmpty(t, passages)
	assert.Equal(t, "B-Cook/diet.md", passages[0].RelPath)
	assert.Contains(t, passages[0].Content, "紫象四四七一")
	assert.Contains(t, passages[0].HeadingPath, "碳水列表")
	for _, passage := range passages[1:] {
		assert.LessOrEqual(t, passage.Score, passages[0].Score)
	}
}

func integrationQuery() []float32 {
	vec := make([]float32, integrationVectorDims)
	vec[0] = 1
	return vec
}

func TestIntegrationPendingGenerationKeepsPreviousContent(t *testing.T) {
	pool := requireKBPgvector(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	root := t.TempDir()
	path := filepath.Join(root, "note.md")
	require.NoError(t, os.WriteFile(path, []byte("# 笔记\n\n版本一，校验口令是紫象四四七一。\n"), 0o600))
	store := NewSQLStore(pool)
	src, err := store.UpsertSharedSource(ctx, "integration-gen-"+time.Now().Format("150405.000"), root)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM kb_sources WHERE id = $1`, src.ID)
	})
	embedder := &keywordEmbedder{keyword: "紫象"}
	indexer := integrationIndexer(store, embedder)

	_, err = indexer.ScanSource(ctx, src)
	require.NoError(t, err)
	passages := integrationRetrieve(t, store, embedder, &src.ID, 10)
	require.NotEmpty(t, passages)
	assert.Contains(t, passages[0].Content, "版本一")

	require.NoError(t, os.WriteFile(path, []byte("# 笔记\n\n版本二，校验口令是紫象四四七一。\n"), 0o600))
	embedder.fail = true
	_, err = indexer.ScanSource(ctx, src)
	require.NoError(t, err)
	doc, err := store.GetDocumentBySourcePath(ctx, src.ID, "note.md")
	require.NoError(t, err)
	assert.Equal(t, StatusPending, doc.Status)
	assert.Equal(t, int32(1), doc.Generation)

	passages = integrationRetrieve(t, store, embedder, &src.ID, 10)
	require.NotEmpty(t, passages, "the previous generation must keep serving while the replacement is pending")
	assert.Contains(t, passages[0].Content, "版本一")
	for _, passage := range passages {
		assert.NotContains(t, passage.Content, "版本二")
	}

	embedder.fail = false
	_, err = indexer.ScanSource(ctx, src)
	require.NoError(t, err)
	doc, err = store.GetDocumentBySourcePath(ctx, src.ID, "note.md")
	require.NoError(t, err)
	assert.Equal(t, StatusIndexed, doc.Status)
	assert.Equal(t, int32(2), doc.Generation)
	passages = integrationRetrieve(t, store, embedder, &src.ID, 10)
	require.NotEmpty(t, passages)
	assert.Contains(t, passages[0].Content, "版本二")
	for _, passage := range passages {
		assert.NotContains(t, passage.Content, "版本一")
	}
}

func integrationRetrieve(t *testing.T, store Store, embedder *keywordEmbedder, sourceID *int64, k int) []Passage {
	t.Helper()
	passages, err := store.RetrievePassages(context.Background(), RetrievalParams{
		Embedding:      integrationQuery(),
		EmbeddingModel: embedder.Model(),
		UserID:         0,
		SourceID:       sourceID,
		Limit:          k,
	})
	require.NoError(t, err)
	return passages
}

func TestIntegrationVisibilityIsolationAndDesyncReassertion(t *testing.T) {
	pool := requireKBPgvector(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	repo := user.NewRepository(pool)
	suffix := time.Now().Format("150405.000")
	makeUser := func(name string) int {
		require.NoError(t, repo.CreateUser(ctx, &user.User{Username: name + suffix, PasswordHash: "x"}))
		created, err := repo.GetUserByUsername(ctx, name+suffix)
		require.NoError(t, err)
		return created.ID
	}
	userA := makeUser("kb_a_")
	userB := makeUser("kb_b_")

	rootA := t.TempDir()
	rootB := t.TempDir()
	sharedRoot := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(rootA, "a.md"), []byte("# A\n\n紫象 A 的私有内容。\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(rootB, "b.md"), []byte("# B\n\n紫象 B 的私有内容。\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(sharedRoot, "s.md"), []byte("# S\n\n紫象 共享内容。\n"), 0o600))

	store := NewSQLStore(pool)
	srcA, err := store.UpsertUserDirSource(ctx, int64(userA), "a", rootA)
	require.NoError(t, err)
	srcB, err := store.UpsertUserDirSource(ctx, int64(userB), "b", rootB)
	require.NoError(t, err)
	srcShared, err := store.UpsertSharedSource(ctx, "integration-shared-"+suffix, sharedRoot)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM kb_sources WHERE id = ANY($1::bigint[])`,
			[]int64{srcA.ID, srcB.ID, srcShared.ID})
	})

	embedder := &keywordEmbedder{keyword: "紫象"}
	indexer := integrationIndexer(store, embedder)
	for _, src := range []Source{srcA, srcB, srcShared} {
		_, scanErr := indexer.ScanSource(ctx, src)
		require.NoError(t, scanErr)
	}

	passagesA, err := store.RetrievePassages(ctx, RetrievalParams{
		Embedding: integrationQuery(), EmbeddingModel: embedder.Model(), UserID: int64(userA), Limit: 20,
	})
	require.NoError(t, err)
	require.NotEmpty(t, passagesA)
	seenA := map[string]bool{}
	for _, passage := range passagesA {
		seenA[passage.RelPath] = true
		assert.NotEqual(t, "b.md", passage.RelPath)
	}
	assert.True(t, seenA["a.md"])
	assert.True(t, seenA["s.md"])

	passagesB, err := store.RetrievePassages(ctx, RetrievalParams{
		Embedding: integrationQuery(), EmbeddingModel: embedder.Model(), UserID: int64(userB), Limit: 20,
	})
	require.NoError(t, err)
	for _, passage := range passagesB {
		assert.NotEqual(t, "a.md", passage.RelPath)
	}

	docA, err := store.GetDocumentBySourcePath(ctx, srcA.ID, "a.md")
	require.NoError(t, err)
	desyncID, err := store.InsertChunk(ctx, Chunk{
		DocumentID: docA.ID, Scope: ScopeUser, OwnerID: int64(userB), SourceID: srcA.ID,
		Generation: docA.Generation, Ordinal: 99, HeadingPath: []string{"Desync"},
		Content: "DESYNC BODY", EmbedText: "紫象 DESYNC BODY",
		ContentHash: hashBytes([]byte("DESYNC BODY")), EmbeddingModel: embedder.Model(),
	})
	require.NoError(t, err)
	require.NoError(t, store.SetChunkEmbedding(ctx, desyncID, integrationQuery(), embedder.Model()))

	for _, userID := range []int64{int64(userA), int64(userB)} {
		rows, err := store.RetrievePassages(ctx, RetrievalParams{
			Embedding: integrationQuery(), EmbeddingModel: embedder.Model(), UserID: userID, Limit: 50,
		})
		require.NoError(t, err)
		for _, passage := range rows {
			assert.NotEqual(t, "DESYNC BODY", passage.Content)
		}
	}
}
