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
	"nyx/internal/user"
	"nyx/test"
)

const (
	kbRecallVersion            = 1
	kbRecallCorpusFile         = "kb_recall_corpus.json"
	kbRecallVectorsFile        = "kb_recall_vectors.json"
	kbRecallDocsDir            = "kb_recall_docs"
	kbRecallTopK               = 5
	kbRecallFetchK             = 10
	kbRecallMinLabelledQueries = 30
	kbRecallMinNegativeQueries = 8
	kbRecallDims               = 1024
	kbRecallFloor5             = 0.94
	kbRecallNegativeCeiling    = 0.40
	kbRecallRandomSeed         = 0x6b62
	kbRecallDecoySeed          = 0x6b63
	kbRecallDecoyCount         = 2000
	kbRecallDecoyBatch         = 500
	kbRecallDecoyCosLo         = 0.35
	kbRecallDecoyCosHi         = 0.75
	kbRecallAnnFloor           = 1000
	kbRecallDecoySourcePrefix  = "kb_recall_decoys"
	skipTrue                   = "true"
)

const (
	kbRecallCategoryPath       = "path_scope"
	kbRecallCategoryHeading    = "heading_scope"
	kbRecallCategoryExact      = "exact_term"
	kbRecallCategorySemantic   = "semantic"
	kbRecallCategoryParaphrase = "paraphrase"
	kbRecallCategoryDistractor = "same_topic_distractor"
	kbRecallCategoryMulti      = "multi_relevant"
	kbRecallCategoryBuried     = "buried"
	kbRecallCategoryNegative   = "negative"
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
	Recall1   float64
	Recall5   float64
	Recall10  float64
	Hit5      float64
	FirstRank int
	TopScore  float64
}

type kbRecallEvaluation struct {
	Result        kbRecallQueryResult
	RandomRecall5 float64
	NegativeTop   *float64
}

type kbRecallScenario struct {
	Name         string
	Decoys       int
	Mode         kbRecallDecoyMode
	SourceScoped bool
}

type kbRecallCategoryReport struct {
	Queries  int
	Relevant int
	Hits1    int
	Hits5    int
	Hits10   int
}

type kbRecallScenarioReport struct {
	Name          string
	Decoys        int
	Mode          kbRecallDecoyMode
	Rows          int
	CosLo         float64
	CosHi         float64
	Labelled      int
	Negatives     int
	Recall1       float64
	Recall5       float64
	Recall10      float64
	Hit5          float64
	MeanFirstRank float64
	RandomRecall5 float64
	NegativeTop   float64
	DistinctTop1  int
	Categories    map[string]*kbRecallCategoryReport
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

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	store := kb.NewSQLStore(pool)
	sourceName := "kb_recall_" + time.Now().Format("20060102_150405.000000000")
	owner := kbRecallUser(t, pool)
	src, err := store.UpsertDirSource(ctx, owner, sourceName, kbRecallDocsRoot(t))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM kb_sources WHERE id = $1`, src.ID)
	})

	embedder := &kbRecallEmbedder{byHash: kbRecallVectorHashes(chunks, vectors), model: vectors.Model}
	indexer := kb.NewIndexer(store, kbRecallChunkOptions(), kb.ConnectorOptions{
		MaxFileBytes: 1 << 20,
		MaxFiles:     1000,
		MaxDepth:     10,
	})
	indexer.SetEmbedderResolver(func(context.Context, int64) (kb.Embedder, error) {
		return embedder, nil
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
	for _, scenario := range kbRecallScenarios() {
		t.Run(scenario.Name, func(t *testing.T) {
			kbRecallRunScenario(t, ctx, pool, store, owner, src.ID, corpus, chunks, vectors, scenario, randomVec)
		})
	}
}

func kbRecallScenarios() []kbRecallScenario {
	decoys := kbRecallDecoyCountFromEnv()
	return []kbRecallScenario{
		{Name: "corpus_only", Decoys: 0, Mode: kbRecallDecoyNoise, SourceScoped: true},
		{Name: "scaled_noise", Decoys: decoys, Mode: kbRecallDecoyNoise, SourceScoped: false},
		{Name: "scaled_near", Decoys: decoys, Mode: kbRecallDecoyNear, SourceScoped: false},
	}
}

func kbRecallRunScenario(t *testing.T, ctx context.Context, pool *pgxpool.Pool, store *kb.SQLStore, owner, fixtureSourceID int64, corpus kbRecallCorpus, chunks []kbRecallChunk, vectors kbRecallVectorSet, scenario kbRecallScenario, randomVec []float32) {
	t.Helper()
	cosLo, cosHi := 0.0, 0.0
	if scenario.Decoys > 0 {
		cosLo, cosHi = kbRecallSeedDecoys(t, ctx, pool, store, owner, corpus, chunks, vectors, scenario.Decoys, scenario.Mode)
	}

	var sourceID *int64
	if scenario.SourceScoped {
		sourceID = &fixtureSourceID
	}
	rows := kbRecallEligibleRows(t, ctx, pool, owner, vectors.Model, sourceID)
	if !scenario.SourceScoped {
		require.GreaterOrEqualf(t, rows, kbRecallAnnFloor,
			"scenario %s has %d eligible chunk rows below the ANN floor %d", scenario.Name, rows, kbRecallAnnFloor)
	}

	recalls1 := make([]float64, 0, len(corpus.Queries))
	recalls5 := make([]float64, 0, len(corpus.Queries))
	recalls10 := make([]float64, 0, len(corpus.Queries))
	hits5 := make([]float64, 0, len(corpus.Queries))
	firstRanks := make([]float64, 0, len(corpus.Queries))
	randomRecalls := make([]float64, 0, len(corpus.Queries))
	negativeTops := make([]float64, 0)
	distinctTop1 := map[string]struct{}{}
	categories := map[string]*kbRecallCategoryReport{}

	for _, query := range corpus.Queries {
		evaluation := kbRecallEvaluateQuery(t, ctx, store, owner, sourceID, vectors, query, randomVec)
		result := evaluation.Result
		kbRecallLogQuery(t, result, evaluation.RandomRecall5)
		if evaluation.NegativeTop != nil {
			negativeTops = append(negativeTops, *evaluation.NegativeTop)
			continue
		}
		recalls1 = append(recalls1, result.Recall1)
		recalls5 = append(recalls5, result.Recall5)
		recalls10 = append(recalls10, result.Recall10)
		hits5 = append(hits5, result.Hit5)
		firstRanks = append(firstRanks, float64(result.FirstRank))
		randomRecalls = append(randomRecalls, evaluation.RandomRecall5)
		if len(result.Retrieved) > 0 {
			distinctTop1[result.Retrieved[0]] = struct{}{}
		}
		category := categories[result.Category]
		if category == nil {
			category = &kbRecallCategoryReport{}
			categories[result.Category] = category
		}
		category.Queries++
		category.Relevant += len(result.Relevant)
		category.Hits1 += kbRecallHitsAt(result.Retrieved, result.Relevant, 1)
		category.Hits5 += kbRecallHitsAt(result.Retrieved, result.Relevant, 5)
		category.Hits10 += kbRecallHitsAt(result.Retrieved, result.Relevant, 10)
	}

	report := kbRecallScenarioReport{
		Name:          scenario.Name,
		Decoys:        scenario.Decoys,
		Mode:          scenario.Mode,
		Rows:          rows,
		CosLo:         cosLo,
		CosHi:         cosHi,
		Labelled:      len(recalls5),
		Negatives:     len(negativeTops),
		Recall1:       kbRecallMean(recalls1),
		Recall5:       kbRecallMean(recalls5),
		Recall10:      kbRecallMean(recalls10),
		Hit5:          kbRecallMean(hits5),
		MeanFirstRank: kbRecallMean(firstRanks),
		RandomRecall5: kbRecallMean(randomRecalls),
		NegativeTop:   kbRecallMean(negativeTops),
		DistinctTop1:  len(distinctTop1),
		Categories:    categories,
	}
	kbRecallLogScenario(t, report)

	require.GreaterOrEqualf(t, report.Recall5, kbRecallFloor5,
		"scenario %s recall@5 fell to %.3f against the pinned floor %.2f", scenario.Name, report.Recall5, kbRecallFloor5)
	require.Greaterf(t, report.Recall5, report.RandomRecall5,
		"scenario %s random control recalls %.3f against a real-query %.3f", scenario.Name, report.RandomRecall5, report.Recall5)
	if report.Negatives > 0 {
		require.Lessf(t, report.NegativeTop, kbRecallNegativeCeiling,
			"scenario %s negative top-1 mean %.3f against the pinned ceiling %.2f", scenario.Name, report.NegativeTop, kbRecallNegativeCeiling)
	}
	require.Greaterf(t, report.DistinctTop1, report.Labelled/2,
		"scenario %s has %d distinct top-1 chunks across %d labelled queries", scenario.Name, report.DistinctTop1, report.Labelled)
}

func kbRecallEvaluateQuery(t *testing.T, ctx context.Context, store *kb.SQLStore, owner int64, sourceID *int64, vectors kbRecallVectorSet, query kbRecallQuery, randomVec []float32) kbRecallEvaluation {
	t.Helper()
	params := kb.RetrievalParams{
		Embedding:      vectors.Queries[query.Key],
		EmbeddingModel: vectors.Model,
		UserID:         owner,
		SourceID:       sourceID,
		PathPrefix:     query.Path,
		Limit:          kbRecallFetchK,
	}
	passages, err := store.RetrievePassages(ctx, params)
	require.NoErrorf(t, err, "query %s", query.Key)
	require.LessOrEqualf(t, len(passages), kbRecallFetchK, "query %s", query.Key)
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
	if query.Category == kbRecallCategoryNegative {
		require.NotEmptyf(t, passages, "negative query %s returned no passages", query.Key)
		top := passages[0].Score
		return kbRecallEvaluation{Result: result, NegativeTop: &top}
	}

	goldRank := kbRecallFirstRank(retrieved, query.Relevant)
	for _, excluded := range query.Exclude {
		excludedRank := kbRecallFirstRank(retrieved, []string{excluded})
		if excludedRank > 0 {
			require.Truef(t, goldRank > 0 && goldRank < excludedRank,
				"query %s ranks excluded chunk %s at %d before the gold at %d", query.Key, excluded, excludedRank, goldRank)
		}
	}
	result.Recall1, result.Recall5, result.Recall10, result.Hit5, result.FirstRank = kbRecallScoreMetrics(retrieved, query.Relevant)

	randomParams := params
	randomParams.Embedding = randomVec
	randomPassages, err := store.RetrievePassages(ctx, randomParams)
	require.NoErrorf(t, err, "random control for %s", query.Key)
	randomRetrieved := make([]string, 0, len(randomPassages))
	for _, passage := range randomPassages {
		randomRetrieved = append(randomRetrieved, kbRecallChunkKey(passage.RelPath, passage.HeadingPath))
	}
	randomRecall5 := 0.0
	if len(query.Relevant) > 0 {
		randomRecall5 = float64(kbRecallHitsAt(randomRetrieved, query.Relevant, 5)) / float64(len(query.Relevant))
	}
	return kbRecallEvaluation{Result: result, RandomRecall5: randomRecall5}
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
	kbRecallRequireNoVacuousQueries(t, corpus, chunks)

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
	require.Equal(t, kbRecallTopK, corpus.TopK)

	chunks := kbRecallAllChunks(t, corpus)
	kbRecallRequireNoVacuousQueries(t, corpus, chunks)
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

func kbRecallRequireNoVacuousQueries(t *testing.T, corpus kbRecallCorpus, chunks []kbRecallChunk) {
	t.Helper()
	for _, vacuous := range kbRecallVacuousQueries(corpus, chunks, kbRecallTopK) {
		require.Failf(t, "vacuous recall query",
			"query %s filters to %d eligible chunks (<= top_k %d) under %q; add corpus or move the query",
			vacuous.Key, vacuous.Eligible, vacuous.TopK, vacuous.Path)
	}
}

func validateKbRecallQueries(t *testing.T, corpus kbRecallCorpus) {
	t.Helper()
	require.NotEmpty(t, corpus.Note)
	labelled := 0
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
			kbRecallCategorySemantic, kbRecallCategoryParaphrase, kbRecallCategoryDistractor,
			kbRecallCategoryMulti, kbRecallCategoryBuried, kbRecallCategoryNegative:
		default:
			require.Failf(t, "unknown category", "query %s has category %s", query.Key, query.Category)
		}
		categories[query.Category]++
		if query.Category == kbRecallCategoryNegative {
			require.Emptyf(t, query.Relevant, "negative query %s labels relevant chunks", query.Key)
			negatives++
		} else {
			require.NotEmptyf(t, query.Relevant, "query %s has no labelled chunks", query.Key)
			require.LessOrEqualf(t, len(query.Relevant), kbRecallTopK, "query %s labels more than top_k chunks", query.Key)
			labelled++
		}
		if query.Path != "" {
			require.Truef(t, strings.HasSuffix(query.Path, "/"), "query %s path %q must be a directory prefix", query.Key, query.Path)
		}
		require.Emptyf(t, kbRecallDuplicateKeys(query.Relevant), "query %s labels duplicate chunks", query.Key)
		require.Emptyf(t, kbRecallDuplicateKeys(query.Exclude), "query %s excludes duplicate chunks", query.Key)
		for _, key := range query.Relevant {
			for _, excluded := range query.Exclude {
				require.NotEqualf(t, key, excluded, "query %s both labels and excludes %s", query.Key, key)
			}
		}
	}
	require.GreaterOrEqual(t, labelled, kbRecallMinLabelledQueries)
	require.GreaterOrEqual(t, negatives, kbRecallMinNegativeQueries)
	require.Positive(t, categories[kbRecallCategoryPath])
	require.Positive(t, categories[kbRecallCategoryHeading])
	require.Positive(t, categories[kbRecallCategoryExact])
	require.Positive(t, categories[kbRecallCategorySemantic])
	require.Positive(t, categories[kbRecallCategoryParaphrase])
	require.Positive(t, categories[kbRecallCategoryDistractor])
	require.Positive(t, categories[kbRecallCategoryMulti])
	require.Positive(t, categories[kbRecallCategoryBuried])
}

func requireKbRecallPgvector(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ci := kbRecallCIEnabled()
	switch kbRecallPrereqAction(ci, os.Getenv("SKIP_CONTAINERS") == skipTrue, os.Getenv("DB_URL")) {
	case kbRecallPrereqFail:
		t.Fatalf("CI requires a pgvector Postgres for the kb recall baseline; set DB_URL, or set SKIP_CONTAINERS=true to opt this gate out")
	case kbRecallPrereqSkip:
		t.Skip("kb recall baseline requires pgvector Postgres; set DB_URL, or set SKIP_CONTAINERS=true to skip")
	}
	url := os.Getenv("DB_URL")
	ctx, cancel := context.WithTimeout(context.Background(), test.LockWaitTimeout)
	defer cancel()
	probe, err := pgxpool.New(ctx, url)
	require.NoError(t, err)
	t.Cleanup(probe.Close)
	var hasVector bool
	require.NoError(t, probe.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_available_extensions WHERE name = 'vector')`,
	).Scan(&hasVector))
	if !hasVector {
		if ci {
			t.Fatalf("DB_URL points at a Postgres without pgvector; CI requires it")
		}
		t.Skip("DB_URL points at a Postgres without pgvector")
	}
	release, err := test.AcquireIntegrationLock(ctx, url)
	require.NoError(t, err)
	t.Cleanup(release)
	require.NoError(t, test.RunMigrationsForURL(ctx, url))
	return probe
}

func kbRecallUser(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	repo := user.NewRepository(pool)
	username := "kb_recall_" + time.Now().Format("20060102_150405.000000000")
	require.NoError(t, repo.CreateUser(context.Background(), &user.User{Username: username, PasswordHash: "x"}))
	created, err := repo.GetUserByUsername(context.Background(), username)
	require.NoError(t, err)
	return int64(created.ID)
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

func kbRecallCategories(categories map[string]*kbRecallCategoryReport) []string {
	out := make([]string, 0, len(categories))
	for category := range categories {
		out = append(out, category)
	}
	sort.Strings(out)
	return out
}

func kbRecallRatio(hits, total int) float64 {
	if total == 0 {
		return 0
	}
	return float64(hits) / float64(total)
}

func kbRecallLogQuery(t *testing.T, result kbRecallQueryResult, randomRecall5 float64) {
	t.Helper()
	t.Logf("  %-24s %-22s recall@1=%.2f recall@5=%.2f recall@10=%.2f hit@5=%.0f first_rank=%d top1=%.4f random@5=%.2f",
		result.Key, result.Category, result.Recall1, result.Recall5, result.Recall10, result.Hit5,
		result.FirstRank, result.TopScore, randomRecall5)
}

func kbRecallLogScenario(t *testing.T, report kbRecallScenarioReport) {
	t.Helper()
	t.Logf("scenario=%s decoys=%d mode=%s cos_to_base=[%.3f,%.3f] eligible_rows=%d labelled=%d negatives=%d",
		report.Name, report.Decoys, report.Mode, report.CosLo, report.CosHi, report.Rows, report.Labelled, report.Negatives)
	t.Logf("  recall@1=%.3f recall@5=%.3f recall@10=%.3f hit@5=%.3f mean_first_rank=%.3f random_control_recall@5=%.3f negative_top1=%.3f distinct_top1=%d/%d",
		report.Recall1, report.Recall5, report.Recall10, report.Hit5, report.MeanFirstRank,
		report.RandomRecall5, report.NegativeTop, report.DistinctTop1, report.Labelled)
	for _, category := range kbRecallCategories(report.Categories) {
		stats := report.Categories[category]
		t.Logf("  category %-22s queries=%d relevant=%d recall@1=%.3f recall@5=%.3f recall@10=%.3f",
			category, stats.Queries, stats.Relevant,
			kbRecallRatio(stats.Hits1, stats.Relevant),
			kbRecallRatio(stats.Hits5, stats.Relevant),
			kbRecallRatio(stats.Hits10, stats.Relevant))
	}
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
