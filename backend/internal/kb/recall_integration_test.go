package kb_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace/noop"

	"nyx/internal/kb"
	"nyx/internal/llm/openai"
	"nyx/internal/platform/config"
	"nyx/internal/rag"
	"nyx/test"
)

const (
	kbRecallVersion           = 1
	kbRecallCorpusFile        = "kb_recall_corpus.json"
	kbRecallVectorsFile       = "kb_recall_vectors.json"
	kbRecallDocsDir           = "kb_recall_docs"
	kbRecallK                 = 5
	kbRecallMinLabeledQueries = 15
	kbRecallDims              = 1024
	kbRecallFloor5            = 0.85
	kbRecallNegativeCeiling   = 0.60
	kbRecallRandomSeed        = 0x6b62
	kbRecallCategoryPath      = "path_scope"
	kbRecallCategoryHeading   = "heading_scope"
	kbRecallCategoryExact     = "exact_term"
	kbRecallCategorySemantic  = "semantic"
	kbRecallCategoryNegative  = "negative"
)

type kbRecallDocument struct {
	Path string `json:"path"`
}

type kbRecallQuery struct {
	Key      string   `json:"key"`
	Category string   `json:"category"`
	Text     string   `json:"text"`
	Path     string   `json:"path,omitempty"`
	Relevant []string `json:"relevant"`
	Exclude  []string `json:"exclude,omitempty"`
}

type kbRecallCorpus struct {
	Version   int                `json:"version"`
	Note      string             `json:"note"`
	TopK      int                `json:"top_k"`
	Documents []kbRecallDocument `json:"documents"`
	Queries   []kbRecallQuery    `json:"queries"`
}

type kbRecallChunkFixture struct {
	TextSHA256 string    `json:"text_sha256"`
	Vector     []float32 `json:"vector"`
}

type kbRecallVectorSet struct {
	Version    int                             `json:"version"`
	Model      string                          `json:"model"`
	Dimensions int                             `json:"dimensions"`
	Chunks     map[string]kbRecallChunkFixture `json:"chunks"`
	Queries    map[string][]float32            `json:"queries"`
}

type kbRecallChunk struct {
	Key         string
	RelPath     string
	HeadingPath []string
	EmbedText   string
	Content     string
}

type kbRecallQueryResult struct {
	Key       string
	Category  string
	Relevant  []string
	Retrieved []string
	Hits      int
	Recall    float64
	TopScore  float64
}

type kbRecallEmbedder struct {
	byHash map[string][]float32
	model  string
}

func (e *kbRecallEmbedder) EmbedStrings(_ context.Context, inputs []string) ([][]float32, error) {
	out := make([][]float32, len(inputs))
	for i, input := range inputs {
		vec, ok := e.byHash[kbRecallTextSHA(input)]
		if !ok {
			return nil, fmt.Errorf("no frozen embedding for chunk text %q", kbRecallShorten(input))
		}
		out[i] = vec
	}
	return out, nil
}

func (e *kbRecallEmbedder) Model() string {
	return e.model
}

func TestKbRecallCorpusFixtureValid(t *testing.T) {
	corpus := loadKbRecallCorpus(t)
	vectors := loadKbRecallVectors(t)
	validateKbRecallFixture(t, corpus, vectors)
}

func TestKbRecallBaseline(t *testing.T) {
	pool := requireKbRecallPgvector(t)

	corpus := loadKbRecallCorpus(t)
	vectors := loadKbRecallVectors(t)
	validateKbRecallFixture(t, corpus, vectors)
	chunks := kbRecallAllChunks(t, corpus)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	store := kb.NewSQLStore(pool)
	sourceName := "kb_recall_" + time.Now().Format("20060102_150405.000000000")
	src, err := store.UpsertSharedSource(ctx, sourceName, kbRecallDocsRoot(t))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM kb_sources WHERE id = $1`, src.ID)
	})

	embedder := &kbRecallEmbedder{byHash: kbRecallVectorHashes(chunks, vectors), model: vectors.Model}
	indexer := kb.NewIndexer(store, embedder, kbRecallChunkOptions(), kb.ConnectorOptions{
		MaxFileBytes: 1 << 20,
		MaxFiles:     1000,
		MaxDepth:     10,
	})
	result, err := indexer.ScanSource(ctx, src)
	require.NoError(t, err)
	require.Equal(t, len(corpus.Documents), result.Documents)
	require.Equal(t, len(vectors.Chunks), result.Chunks)
	require.Empty(t, result.Skipped)

	stats, err := store.SourceStats(ctx, src.ID)
	require.NoError(t, err)
	require.Equal(t, int64(len(vectors.Chunks)), stats.Chunks)
	require.Zero(t, stats.Pending)

	randomVec := kbRecallRandomVector()
	recalls := make([]float64, 0, len(corpus.Queries))
	randomRecalls := make([]float64, 0, len(corpus.Queries))
	negativeTopScores := make([]float64, 0)
	distinctTop1 := map[string]struct{}{}
	categoryHits := map[string]int{}
	categoryTotal := map[string]int{}

	for _, query := range corpus.Queries {
		evaluation := kbRecallEvaluateQuery(t, ctx, store, src.ID, vectors, query, randomVec)
		if evaluation.NegativeTop != nil {
			negativeTopScores = append(negativeTopScores, *evaluation.NegativeTop)
			continue
		}
		if len(evaluation.Result.Retrieved) > 0 {
			distinctTop1[evaluation.Result.Retrieved[0]] = struct{}{}
		}
		recalls = append(recalls, evaluation.Result.Recall)
		randomRecalls = append(randomRecalls, evaluation.RandomRecall)
		categoryHits[query.Category] += evaluation.Result.Hits
		categoryTotal[query.Category] += len(evaluation.Result.Relevant)
		kbRecallLogQuery(t, evaluation.Result)
	}

	meanRecall := kbRecallMean(recalls)
	randomRecall := kbRecallMean(randomRecalls)
	negativeTop := kbRecallMean(negativeTopScores)
	t.Logf("kb recall@%d=%.3f random_control=%.3f negatives=%d negative_top1_mean=%.3f chunks=%d",
		kbRecallK, meanRecall, randomRecall, len(negativeTopScores), negativeTop, len(chunks))
	for _, category := range kbRecallCategories(categoryHits) {
		t.Logf("kb recall category %-14s %d/%d", category, categoryHits[category], categoryTotal[category])
	}

	require.GreaterOrEqualf(t, meanRecall, kbRecallFloor5,
		"recall@%d fell to %.3f against the pinned floor %.2f", kbRecallK, meanRecall, kbRecallFloor5)
	require.Greaterf(t, meanRecall, randomRecall,
		"a fixed random query vector recalls %.3f against a real-query %.3f", randomRecall, meanRecall)
	require.Lessf(t, negativeTop, kbRecallNegativeCeiling,
		"negative queries reached a top-1 score of %.3f against the pinned ceiling %.2f", negativeTop, kbRecallNegativeCeiling)
	require.Greaterf(t, len(distinctTop1), len(recalls)/2,
		"only %d distinct top-1 chunks across %d labelled queries", len(distinctTop1), len(recalls))
}

type kbRecallEvaluation struct {
	Result       kbRecallQueryResult
	RandomRecall float64
	NegativeTop  *float64
}

func kbRecallEvaluateQuery(t *testing.T, ctx context.Context, store *kb.SQLStore, sourceID int64, vectors kbRecallVectorSet, query kbRecallQuery, randomVec []float32) kbRecallEvaluation {
	t.Helper()
	passages, err := store.RetrievePassages(ctx, kb.RetrievalParams{
		Embedding:      vectors.Queries[query.Key],
		EmbeddingModel: vectors.Model,
		UserID:         0,
		SourceID:       &sourceID,
		PathPrefix:     query.Path,
		Limit:          kbRecallK,
	})
	require.NoErrorf(t, err, "query %s", query.Key)
	require.LessOrEqual(t, len(passages), kbRecallK)
	if query.Path != "" {
		for _, passage := range passages {
			require.Truef(t, strings.HasPrefix(passage.RelPath, query.Path),
				"query %s with path %q returned %s", query.Key, query.Path, passage.RelPath)
		}
	}

	retrieved := make([]string, 0, len(passages))
	for _, passage := range passages {
		retrieved = append(retrieved, kbRecallChunkKey(passage.RelPath, passage.HeadingPath))
	}
	result := kbRecallQueryResult{
		Key:       query.Key,
		Category:  query.Category,
		Relevant:  query.Relevant,
		Retrieved: retrieved,
	}
	if len(passages) > 0 {
		result.TopScore = passages[0].Score
	}
	if len(query.Relevant) == 0 {
		require.NotEmpty(t, passages)
		top := passages[0].Score
		return kbRecallEvaluation{Result: result, NegativeTop: &top}
	}

	relevant := make(map[string]struct{}, len(query.Relevant))
	for _, key := range query.Relevant {
		relevant[key] = struct{}{}
	}
	for _, key := range retrieved {
		if _, ok := relevant[key]; ok {
			result.Hits++
		}
	}
	for _, excluded := range query.Exclude {
		for _, key := range retrieved {
			require.NotEqualf(t, excluded, key, "query %s retrieved excluded chunk %s", query.Key, excluded)
		}
	}
	result.Recall = float64(result.Hits) / float64(len(query.Relevant))
	return kbRecallEvaluation{
		Result:       result,
		RandomRecall: kbRecallRandomRecall(t, ctx, store, sourceID, vectors.Model, query, relevant, randomVec),
	}
}

func kbRecallRandomRecall(t *testing.T, ctx context.Context, store *kb.SQLStore, sourceID int64, model string, query kbRecallQuery, relevant map[string]struct{}, randomVec []float32) float64 {
	t.Helper()
	passages, err := store.RetrievePassages(ctx, kb.RetrievalParams{
		Embedding:      randomVec,
		EmbeddingModel: model,
		UserID:         0,
		SourceID:       &sourceID,
		PathPrefix:     query.Path,
		Limit:          kbRecallK,
	})
	require.NoErrorf(t, err, "random control for %s", query.Key)
	hits := 0
	for _, passage := range passages {
		if _, ok := relevant[kbRecallChunkKey(passage.RelPath, passage.HeadingPath)]; ok {
			hits++
		}
	}
	return float64(hits) / float64(len(query.Relevant))
}

func TestRegenerateKbRecallVectors(t *testing.T) {
	if os.Getenv("NYX_KB_RECALL_REGEN") != "1" {
		t.Skip("set NYX_KB_RECALL_REGEN=1 with a live embedder to regenerate testdata/" + kbRecallVectorsFile)
	}
	baseURL := os.Getenv("NYX_KB_RECALL_EMBED_URL")
	require.NotEmpty(t, baseURL, "NYX_KB_RECALL_REGEN=1 requires NYX_KB_RECALL_EMBED_URL")
	model := os.Getenv("NYX_KB_RECALL_EMBED_MODEL")
	require.NotEmpty(t, model, "NYX_KB_RECALL_REGEN=1 requires NYX_KB_RECALL_EMBED_MODEL")
	apiKey := os.Getenv("NYX_KB_RECALL_EMBED_KEY")
	if apiKey == "" {
		apiKey = "EMPTY"
	}

	corpus := loadKbRecallCorpus(t)
	chunks := kbRecallAllChunks(t, corpus)
	validateKbRecallQueries(t, corpus)

	chunkTexts := make([]string, 0, len(chunks))
	for _, chunk := range chunks {
		chunkTexts = append(chunkTexts, chunk.EmbedText)
	}
	queryTexts := make([]string, 0, len(corpus.Queries))
	for _, query := range corpus.Queries {
		queryTexts = append(queryTexts, query.Text)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	tracer := noop.NewTracerProvider().Tracer("kb-recall-regen")
	client, err := openai.NewClient(&config.Config{
		LLMEnabled:         true,
		LLMBaseURL:         baseURL,
		LLMAPIKey:          apiKey,
		LLMModel:           model,
		LLMAllowPrivateURL: true,
	}, tracer)
	require.NoError(t, err)
	embedder := rag.NewEmbedder(client, model, tracer)
	embedder.SetDimensions(kbRecallDims)

	chunkVectors, err := kbRecallEmbedAll(ctx, embedder, chunkTexts, 8)
	require.NoError(t, err)
	require.Len(t, chunkVectors, len(chunks))
	queryVectors, err := kbRecallEmbedAll(ctx, embedder, queryTexts, 8)
	require.NoError(t, err)
	require.Len(t, queryVectors, len(corpus.Queries))

	out := kbRecallVectorSet{
		Version:    corpus.Version,
		Model:      model,
		Dimensions: kbRecallDims,
		Chunks:     make(map[string]kbRecallChunkFixture, len(chunks)),
		Queries:    make(map[string][]float32, len(corpus.Queries)),
	}
	for i, chunk := range chunks {
		require.Lenf(t, chunkVectors[i], kbRecallDims, "chunk %s came back with the wrong dim", chunk.Key)
		out.Chunks[chunk.Key] = kbRecallChunkFixture{
			TextSHA256: kbRecallTextSHA(chunk.EmbedText),
			Vector:     chunkVectors[i],
		}
	}
	for i, query := range corpus.Queries {
		require.Lenf(t, queryVectors[i], kbRecallDims, "query %s came back with the wrong dim", query.Key)
		out.Queries[query.Key] = queryVectors[i]
	}

	raw, err := json.MarshalIndent(out, "", " ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(kbRecallFixturePath(kbRecallVectorsFile), raw, 0o600))
	t.Logf("wrote testdata/%s: %d chunk vectors, %d query vectors, model=%s",
		kbRecallVectorsFile, len(out.Chunks), len(out.Queries), model)
}

func validateKbRecallFixture(t *testing.T, corpus kbRecallCorpus, vectors kbRecallVectorSet) {
	t.Helper()
	require.Equal(t, kbRecallVersion, corpus.Version)
	require.Equal(t, corpus.Version, vectors.Version)
	require.Equal(t, kbRecallDims, vectors.Dimensions)
	require.NotEmpty(t, vectors.Model)
	require.Equal(t, kbRecallK, corpus.TopK)

	chunks := kbRecallAllChunks(t, corpus)
	byKey := make(map[string]kbRecallChunk, len(chunks))
	for _, chunk := range chunks {
		byKey[chunk.Key] = chunk
	}
	require.Len(t, vectors.Chunks, len(chunks))
	for key, chunk := range byKey {
		fixture, ok := vectors.Chunks[key]
		require.Truef(t, ok, "no frozen vector for chunk %s", key)
		require.Equalf(t, kbRecallTextSHA(chunk.EmbedText), fixture.TextSHA256,
			"chunk %s no longer matches the frozen chunk text; regenerate the vector fixture", key)
		require.Lenf(t, fixture.Vector, kbRecallDims, "chunk %s has the wrong dim", key)
		require.Positivef(t, kbRecallVectorNorm(fixture.Vector), "chunk %s has a zero vector", key)
		require.Truef(t, kbRecallFinite(fixture.Vector), "chunk %s has a non-finite value", key)
	}

	validateKbRecallQueries(t, corpus)
	require.Len(t, vectors.Queries, len(corpus.Queries))
	for _, query := range corpus.Queries {
		vec, ok := vectors.Queries[query.Key]
		require.Truef(t, ok, "no frozen vector for query %s", query.Key)
		require.Lenf(t, vec, kbRecallDims, "query %s has the wrong dim", query.Key)
		require.Positivef(t, kbRecallVectorNorm(vec), "query %s has a zero vector", query.Key)
		require.Truef(t, kbRecallFinite(vec), "query %s has a non-finite value", query.Key)
		for _, key := range query.Relevant {
			_, known := byKey[key]
			require.Truef(t, known, "query %s references unknown chunk %s", query.Key, key)
		}
		for _, key := range query.Exclude {
			_, known := byKey[key]
			require.Truef(t, known, "query %s excludes unknown chunk %s", query.Key, key)
		}
	}
}

func validateKbRecallQueries(t *testing.T, corpus kbRecallCorpus) {
	t.Helper()
	require.NotEmpty(t, corpus.Note)
	labeled := 0
	negatives := 0
	categories := map[string]int{}
	keys := map[string]struct{}{}
	texts := map[string]struct{}{}
	for _, query := range corpus.Queries {
		require.NotEmpty(t, query.Key)
		require.NotEmpty(t, query.Text)
		_, duplicateKey := keys[query.Key]
		require.Falsef(t, duplicateKey, "duplicate query key %s", query.Key)
		keys[query.Key] = struct{}{}
		_, duplicateText := texts[query.Text]
		require.Falsef(t, duplicateText, "duplicate query text on %s", query.Key)
		texts[query.Text] = struct{}{}
		switch query.Category {
		case kbRecallCategoryPath, kbRecallCategoryHeading, kbRecallCategoryExact,
			kbRecallCategorySemantic, kbRecallCategoryNegative:
		default:
			require.Failf(t, "unknown category", "query %s has category %s", query.Key, query.Category)
		}
		categories[query.Category]++
		if query.Category == kbRecallCategoryNegative {
			require.Emptyf(t, query.Relevant, "negative query %s labels relevant chunks", query.Key)
			negatives++
		} else {
			require.NotEmptyf(t, query.Relevant, "query %s has no labelled chunks", query.Key)
			require.LessOrEqual(t, len(query.Relevant), kbRecallK)
			labeled++
		}
		if query.Path != "" {
			require.Truef(t, strings.HasSuffix(query.Path, "/"), "query %s path %q must be a directory prefix", query.Key, query.Path)
		}
		for _, key := range query.Relevant {
			for _, excluded := range query.Exclude {
				require.NotEqualf(t, key, excluded, "query %s both labels and excludes %s", query.Key, key)
			}
		}
	}
	require.GreaterOrEqual(t, labeled, kbRecallMinLabeledQueries)
	require.Positive(t, negatives)
	require.Positive(t, categories[kbRecallCategoryPath])
	require.Positive(t, categories[kbRecallCategoryHeading])
	require.Positive(t, categories[kbRecallCategoryExact])
}

func loadKbRecallCorpus(t *testing.T) kbRecallCorpus {
	t.Helper()
	raw, err := os.ReadFile(kbRecallFixturePath(kbRecallCorpusFile))
	require.NoErrorf(t, err, "corpus fixture %s is the labelled ground truth for the recall baseline", kbRecallCorpusFile)
	var corpus kbRecallCorpus
	require.NoError(t, json.Unmarshal(raw, &corpus))
	return corpus
}

func loadKbRecallVectors(t *testing.T) kbRecallVectorSet {
	t.Helper()
	raw, err := os.ReadFile(kbRecallFixturePath(kbRecallVectorsFile))
	require.NoErrorf(t, err, "vector fixture %s is missing; regenerate with NYX_KB_RECALL_REGEN=1 against a live embedder", kbRecallVectorsFile)
	var vectors kbRecallVectorSet
	require.NoError(t, json.Unmarshal(raw, &vectors))
	return vectors
}

func kbRecallFixturePath(name string) string {
	return filepath.Join("testdata", name)
}

func requireKbRecallPgvector(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if os.Getenv("SKIP_CONTAINERS") == "true" {
		t.Skip("SKIP_CONTAINERS=true; kb recall baseline requires pgvector Postgres")
	}
	url := os.Getenv("DB_URL")
	if url == "" {
		t.Skip("DB_URL is not set; kb recall baseline requires pgvector Postgres")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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

func kbRecallDocsRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("testdata", kbRecallDocsDir))
	require.NoError(t, err)
	return root
}

func kbRecallChunkOptions() kb.ChunkOptions {
	return kb.ChunkOptions{TargetTokens: 400, MaxTokens: 700, MinTokens: 50, OverlapTokens: 80}
}

func kbRecallChunkKey(relPath string, heading []string) string {
	headingKey := "(root)"
	if len(heading) > 0 {
		headingKey = strings.Join(heading, " / ")
	}
	return relPath + "::" + headingKey
}

func kbRecallAllChunks(t *testing.T, corpus kbRecallCorpus) []kbRecallChunk {
	t.Helper()
	root := kbRecallDocsRoot(t)
	seenDocs := map[string]struct{}{}
	chunks := make([]kbRecallChunk, 0, len(corpus.Documents)*3)
	for _, document := range corpus.Documents {
		require.NotEmpty(t, document.Path)
		require.Falsef(t, filepath.IsAbs(document.Path), "document %s must be relative", document.Path)
		require.NotContainsf(t, document.Path, "..", "document %s escapes the fixture root", document.Path)
		_, duplicate := seenDocs[document.Path]
		require.Falsef(t, duplicate, "duplicate document %s", document.Path)
		seenDocs[document.Path] = struct{}{}
		//nolint:gosec // G304: document paths are fixture-relative and validated against the testdata root.
		raw, err := os.ReadFile(filepath.Join(root, document.Path))
		require.NoErrorf(t, err, "document %s", document.Path)
		doc, err := kb.NewMarkdownNormalizer().Normalize(context.Background(), raw, kb.Source{})
		require.NoErrorf(t, err, "document %s", document.Path)
		specs := kb.ChunkDocument(document.Path, doc, kbRecallChunkOptions())
		require.NotEmptyf(t, specs, "document %s produced no chunks", document.Path)
		seenKeys := map[string]struct{}{}
		for _, spec := range specs {
			key := kbRecallChunkKey(document.Path, spec.HeadingPath)
			_, duplicateKey := seenKeys[key]
			require.Falsef(t, duplicateKey,
				"document %s heading %q produced more than one chunk; shorten that section so one label maps to one chunk",
				document.Path, strings.Join(spec.HeadingPath, " / "))
			seenKeys[key] = struct{}{}
			chunks = append(chunks, kbRecallChunk{
				Key:         key,
				RelPath:     document.Path,
				HeadingPath: spec.HeadingPath,
				EmbedText:   spec.EmbedText,
				Content:     spec.Content,
			})
		}
	}
	return chunks
}

func kbRecallVectorHashes(chunks []kbRecallChunk, vectors kbRecallVectorSet) map[string][]float32 {
	byHash := make(map[string][]float32, len(chunks))
	for _, chunk := range chunks {
		byHash[vectors.Chunks[chunk.Key].TextSHA256] = vectors.Chunks[chunk.Key].Vector
	}
	return byHash
}

func kbRecallEmbedAll(ctx context.Context, embedder *rag.Embedder, inputs []string, batch int) ([][]float32, error) {
	out := make([][]float32, 0, len(inputs))
	for start := 0; start < len(inputs); start += batch {
		end := min(start+batch, len(inputs))
		vectors, err := embedder.EmbedStrings(ctx, inputs[start:end])
		if err != nil {
			return nil, err
		}
		if len(vectors) != end-start {
			return nil, fmt.Errorf("embedder returned %d vectors for %d inputs", len(vectors), end-start)
		}
		out = append(out, vectors...)
	}
	return out, nil
}

func kbRecallTextSHA(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

func kbRecallShorten(text string) string {
	if len(text) <= 120 {
		return text
	}
	return text[:120]
}

func kbRecallVectorNorm(vec []float32) float64 {
	sum := 0.0
	for _, value := range vec {
		sum += float64(value) * float64(value)
	}
	return math.Sqrt(sum)
}

func kbRecallFinite(vec []float32) bool {
	for _, value := range vec {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return false
		}
	}
	return true
}

func kbRecallMean(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sum := 0.0
	for _, value := range values {
		sum += value
	}
	return sum / float64(len(values))
}

func kbRecallRandomVector() []float32 {
	rng := rand.New(rand.NewPCG(kbRecallRandomSeed, kbRecallRandomSeed)) //nolint:gosec // G404: deterministic PRNG keeps the control reproducible.
	vec := make([]float32, kbRecallDims)
	for i := range vec {
		vec[i] = float32(rng.NormFloat64())
	}
	return vec
}

func kbRecallCategories(hits map[string]int) []string {
	categories := make([]string, 0, len(hits))
	for category := range hits {
		categories = append(categories, category)
	}
	sort.Strings(categories)
	return categories
}

func kbRecallLogQuery(t *testing.T, result kbRecallQueryResult) {
	t.Helper()
	t.Logf("  %-22s %-14s hits=%d/%d recall=%.2f top1=%.4f",
		result.Key, result.Category, result.Hits, len(result.Relevant), result.Recall, result.TopScore)
}
