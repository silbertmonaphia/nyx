package rag

import (
	"context"
	"encoding/json"
	"math"
	"math/rand/v2"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"nyx/internal/feed"
	"nyx/internal/llm/openai"
	"nyx/internal/platform/config"
	ragdb "nyx/internal/rag/db"
	"nyx/test"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"
	pgxvec "github.com/pgvector/pgvector-go/pgx"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace/noop"
)

const (
	recallVectorDims      = 1024
	recallDBName          = "nyx_ragtest"
	recallCorpusFile      = "recall_corpus.json"
	recallVectorsFile     = "recall_vectors.json"
	recallK               = 10
	recallMinLabeledQuery = 15
	decoySeed             = 0x5eed
	decoyCosLo            = 0.40
	decoyCosHi            = 0.75
	decoyBatch            = 500
	annExercisedMinRow    = 1000
	recallKOffsets        = 5
	recallFloor5          = 0.60
	skipTrue              = "true"
)

type recallFeedSpec struct {
	Key         string `json:"key"`
	Category    string `json:"category"`
	Title       string `json:"title"`
	Description string `json:"description"`
}

type recallQuerySpec struct {
	Key      string   `json:"key"`
	Category string   `json:"category"`
	Text     string   `json:"text"`
	Relevant []string `json:"relevant"`
}

type recallCorpus struct {
	Version int               `json:"version"`
	Note    string            `json:"note"`
	TopK    int               `json:"top_k"`
	Feeds   []recallFeedSpec  `json:"feeds"`
	Queries []recallQuerySpec `json:"queries"`
}

type recallVectorSet struct {
	Version    int                  `json:"version"`
	Model      string               `json:"model"`
	Dimensions int                  `json:"dimensions"`
	Feeds      map[string][]float32 `json:"feeds"`
	Queries    map[string][]float32 `json:"queries"`
}

type recallQueryResult struct {
	Key            string  `json:"key"`
	Category       string  `json:"category"`
	Relevant       []int   `json:"relevant_feed_ids"`
	Retrieved      []int   `json:"retrieved_feed_ids"`
	ExactRetrieved []int   `json:"exact_retrieved_feed_ids"`
	Hits           int     `json:"hits_at_10"`
	HitsAt5        int     `json:"hits_at_5"`
	RecallAt1      float64 `json:"recall_at_1"`
	RecallAt3      float64 `json:"recall_at_3"`
	RecallAt5      float64 `json:"recall_at_5"`
	RecallAt10     float64 `json:"recall_at_10"`
	FirstRank      int     `json:"first_relevant_rank"`
	MinRelScore    float64 `json:"min_relevant_score"`
	HasRelScore    bool    `json:"has_relevant_score"`
	MaxIrrelScore  float64 `json:"max_irrelevant_score"`
	HasIrrelScore  bool    `json:"has_irrelevant_score"`
	TopScore       float64 `json:"top1_score"`
	RandomHits     int     `json:"random_control_hits"`
}

type recallProvenance struct {
	PGVersion       string `json:"pg_version"`
	PgvectorVersion string `json:"pgvector_version"`
	HNSEFSearch     string `json:"hnsw_ef_search"`
	EmbeddingModel  string `json:"embedding_model"`
}

type recallScenarioReport struct {
	Scenario              string              `json:"scenario"`
	Provenance            recallProvenance    `json:"provenance"`
	IndexedRows           int                 `json:"indexed_rows"`
	OwnerRows             int                 `json:"owner_rows"`
	CorpusFeeds           int                 `json:"corpus_feeds"`
	Decoys                int                 `json:"decoys"`
	DecoyMode             string              `json:"decoy_mode"`
	DecoyCosLo            float64             `json:"decoy_cosine_to_base_lo"`
	DecoyCosHi            float64             `json:"decoy_cosine_to_base_hi"`
	K                     int                 `json:"k"`
	ProductionK           int                 `json:"production_top_k"`
	LabeledQueries        int                 `json:"labeled_queries"`
	NegativeQueries       int                 `json:"negative_queries"`
	RecallAt1             float64             `json:"recall_at_1"`
	RecallAt3             float64             `json:"recall_at_3"`
	RecallAt5             float64             `json:"recall_at_5"`
	RecallAt10            float64             `json:"recall_at_10"`
	HitRateAt5            float64             `json:"hit_rate_at_5"`
	MRRAt5                float64             `json:"mrr_at_5"`
	ExactRecallAt5        float64             `json:"exact_recall_at_5"`
	RandomControlRecall   float64             `json:"random_control_recall_at_10"`
	RandomControlRecall5  float64             `json:"random_control_recall_at_5"`
	TrivialBaseRate5      float64             `json:"trivial_base_rate_at_5"`
	RandomControlHits     int                 `json:"random_control_hits"`
	DistinctTop1          int                 `json:"distinct_top1_across_labeled_queries"`
	NegativeMaxIrrelScore float64             `json:"negative_query_mean_max_irrelevant_score"`
	UnderfillQueries      int                 `json:"underfill_queries"`
	ANNExercised          bool                `json:"ann_exercised"`
	ANNDivergentQueries   int                 `json:"ann_divergent_queries"`
	ANNRecallDelta        float64             `json:"ann_recall_at_5_delta"`
	MeanRelScore          float64             `json:"mean_relevant_score"`
	MeanIrrelScore        float64             `json:"mean_irrelevant_score"`
	NegativeTop1Mean      float64             `json:"negative_query_mean_top1_score"`
	PerCategoryRecall     map[string]float64  `json:"per_category_recall_at_5"`
	Queries               []recallQueryResult `json:"queries"`
}

func recallFixturePath(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join("testdata", name)
}

func loadRecallCorpus(t *testing.T) recallCorpus {
	t.Helper()
	raw, err := os.ReadFile(recallFixturePath(t, recallCorpusFile))
	require.NoErrorf(t, err, "corpus fixture %s is the labelled ground truth for the recall baseline", recallCorpusFile)
	var c recallCorpus
	require.NoError(t, json.Unmarshal(raw, &c))
	return c
}

func loadRecallVectors(t *testing.T) recallVectorSet {
	t.Helper()
	raw, err := os.ReadFile(recallFixturePath(t, recallVectorsFile))
	require.NoErrorf(t, err, "vector fixture %s is missing; regenerate with NYX_RAG_RECALL_REGEN=1 against a live embedder", recallVectorsFile)
	var v recallVectorSet
	require.NoError(t, json.Unmarshal(raw, &v))
	return v
}

func loadRecallFixtures(t *testing.T) (recallCorpus, recallVectorSet) {
	t.Helper()
	return loadRecallCorpus(t), loadRecallVectors(t)
}

func vectorNorm(v []float32) float64 {
	sum := 0.0
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	return math.Sqrt(sum)
}

func TestRecallCorpusFixtureValid(t *testing.T) {
	corpus := loadRecallCorpus(t)
	vectors := loadRecallVectors(t)

	require.Equal(t, corpus.Version, vectors.Version, "corpus and vector fixtures are from different generations")
	require.Equal(t, recallVectorDims, vectors.Dimensions, "vector fixture dim must match migrations 000015")
	require.NotEmpty(t, vectors.Model, "vector fixture must record the embedding model that produced it")
	require.Positive(t, corpus.TopK)
	require.LessOrEqual(t, corpus.TopK, recallK, "production top_k must be derivable from the single k=%d retrieval", recallK)

	keys := make(map[string]struct{}, len(corpus.Feeds))
	chunks := make(map[string]string, len(corpus.Feeds))
	for _, f := range corpus.Feeds {
		require.NotEmpty(t, f.Key, "every feed needs a stable key")
		require.NotEmpty(t, f.Title, "feed %s needs a title", f.Key)
		_, dup := keys[f.Key]
		require.False(t, dup, "duplicate feed key %s", f.Key)
		keys[f.Key] = struct{}{}

		chunk := ChunkText(&feed.Feed{Title: f.Title, Description: f.Description})
		require.NotEmpty(t, chunk, "feed %s chunks to empty", f.Key)
		prev, dupChunk := chunks[chunk]
		require.Falsef(t, dupChunk, "feeds %s and %s share chunk text; Indexer skips re-embedding on hash match", prev, f.Key)
		chunks[chunk] = f.Key

		vec, ok := vectors.Feeds[f.Key]
		require.Truef(t, ok, "no frozen vector for feed %s", f.Key)
		require.Len(t, vec, recallVectorDims, "feed %s vector has wrong dim", f.Key)
		require.Positivef(t, vectorNorm(vec), "feed %s has a zero vector; cosine distance against it is undefined", f.Key)
		for i, x := range vec {
			require.Falsef(t, math.IsNaN(float64(x)) || math.IsInf(float64(x), 0),
				"feed %s vector has a non-finite value at %d", f.Key, i)
		}
	}
	require.Len(t, vectors.Feeds, len(corpus.Feeds), "vector fixture has stale or missing feed entries")

	require.NotEmpty(t, corpus.Queries)
	negatives := 0
	labeled := 0
	queryKeys := make(map[string]struct{}, len(corpus.Queries))
	queryTexts := make(map[string]struct{}, len(corpus.Queries))
	for _, q := range corpus.Queries {
		require.NotEmpty(t, q.Key, "every query needs a stable key")
		require.NotEmpty(t, q.Text, "query %s needs text", q.Key)
		_, dup := queryKeys[q.Key]
		require.False(t, dup, "duplicate query key %s", q.Key)
		queryKeys[q.Key] = struct{}{}
		_, dupText := queryTexts[q.Text]
		require.False(t, dupText, "duplicate query text on %s", q.Key)
		queryTexts[q.Text] = struct{}{}

		require.LessOrEqualf(t, len(q.Relevant), recallK,
			"query %s labels %d relevant docs, more than k=%d", q.Key, len(q.Relevant), recallK)
		for _, ref := range q.Relevant {
			_, ok := keys[ref]
			require.Truef(t, ok, "query %s references unknown feed key %s", q.Key, ref)
		}
		if len(q.Relevant) == 0 {
			negatives++
		} else {
			labeled++
		}
		vec, ok := vectors.Queries[q.Key]
		require.Truef(t, ok, "no frozen vector for query %s", q.Key)
		require.Len(t, vec, recallVectorDims, "query %s vector has wrong dim", q.Key)
		require.Positivef(t, vectorNorm(vec), "query %s has a zero vector", q.Key)
	}
	require.Positive(t, negatives, "corpus needs out-of-domain negative queries to measure score_gap on a threshold-less retriever")
	require.GreaterOrEqualf(t, labeled, recallMinLabeledQuery,
		"corpus has only %d labelled queries; a fixture trimmed this far silently destabilises every reported macro average", labeled)
	require.Len(t, vectors.Queries, len(corpus.Queries), "vector fixture has stale or missing query entries")
}

func recallBaseURL(t *testing.T) string {
	t.Helper()
	if os.Getenv("SKIP_CONTAINERS") == skipTrue {
		t.Skip("SKIP_CONTAINERS=true; rag recall baseline requires pgvector Postgres")
	}
	url := os.Getenv("DB_URL")
	if url == "" {
		t.Skip("DB_URL is not set; rag recall baseline requires pgvector Postgres")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	cfg, err := pgxpool.ParseConfig(url)
	require.NoError(t, err)

	probe, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoErrorf(t, err, "DB_URL unreachable: %v", err)
	defer probe.Close()

	var has bool
	require.NoError(t, probe.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_available_extensions WHERE name = 'vector')`).Scan(&has))
	require.True(t, has, "DB_URL points at a Postgres without the pgvector extension; use the pgvector/pgvector:pg17 image (docker-compose.yml's `db` service)")
	return url
}

func requireRecallDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if os.Getenv("NYX_RAG_RECALL_CONTAINER") == "1" {
		return startRecallContainer(t)
	}
	baseURL := recallBaseURL(t)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	release, err := test.AcquireIntegrationLock(ctx, baseURL)
	require.NoError(t, err)
	t.Cleanup(release)

	baseCfg, err := pgxpool.ParseConfig(baseURL)
	require.NoError(t, err)

	adminCfg := baseCfg.Copy()
	adminCfg.ConnConfig.Database = "postgres"
	admin, err := pgxpool.NewWithConfig(ctx, adminCfg)
	require.NoError(t, err)
	var exists bool
	require.NoError(t, admin.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, recallDBName).Scan(&exists))
	if !exists {
		_, err = admin.Exec(ctx, `CREATE DATABASE `+recallDBName)
		require.NoError(t, err)
	}
	admin.Close()

	targetCfg := baseCfg.Copy()
	targetCfg.ConnConfig.Database = recallDBName
	targetCfg.MaxConns = 4
	targetCfg.MinConns = 1
	targetCfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		return pgxvec.RegisterTypes(ctx, conn)
	}

	require.NoError(t, test.RunMigrationsForURL(ctx, recallMigrateURL(baseURL)))

	pool, err := pgxpool.NewWithConfig(ctx, targetCfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

func startRecallContainer(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	if os.Getenv("SKIP_CONTAINERS") == skipTrue {
		t.Skip("SKIP_CONTAINERS=true; rag recall baseline requires pgvector Postgres")
	}

	db, err := test.StartPostgres(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Container.Terminate(context.Background()) })
	require.NoError(t, db.RunMigrationsWithContext(ctx))

	cfg, err := pgxpool.ParseConfig(db.DBURL)
	require.NoError(t, err)
	cfg.MaxConns = 4
	cfg.MinConns = 1
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		return pgxvec.RegisterTypes(ctx, conn)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

func recallMigrateURL(baseURL string) string {
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme == "" {
		return baseURL + "?sslmode=disable"
	}
	u.Path = "/" + recallDBName
	if u.Query().Get("sslmode") == "" {
		q := u.Query()
		q.Set("sslmode", "disable")
		u.RawQuery = q.Encode()
	}
	return u.String()
}

func purgeRecallRows(ctx context.Context, pool *pgxpool.Pool) {
	_, _ = pool.Exec(ctx,
		`DELETE FROM feeds WHERE user_id IN (SELECT id FROM users WHERE username LIKE 'rag_recall_%')`)
	_, _ = pool.Exec(ctx, `DELETE FROM users WHERE username LIKE 'rag_recall_%'`)
}

func seedRecallFeeds(t *testing.T, pool *pgxpool.Pool, userID int, corpus recallCorpus, vectors recallVectorSet) map[string]int {
	t.Helper()
	ctx := context.Background()
	q := ragdb.New(pool)
	repo := feed.NewRepository(pool)

	ids := make(map[string]int, len(corpus.Feeds))
	for _, spec := range corpus.Feeds {
		f := &feed.Feed{Title: spec.Title, Description: spec.Description}
		require.NoError(t, repo.Create(ctx, userID, f))
		require.NoError(t, q.UpsertFeedEmbedding(ctx, ragdb.UpsertFeedEmbeddingParams{
			FeedID:      int32(f.ID), //nolint:gosec // G115: SERIAL ids are bounded well below int32 max in test corpora.
			UserID:      int64(userID),
			Embedding:   pgvector.NewVector(vectors.Feeds[spec.Key]),
			ChunkText:   ChunkText(f),
			ContentHash: HashChunk(f),
		}))
		ids[spec.Key] = f.ID
	}
	return ids
}

func normalizeVector(v []float32) []float32 {
	norm := vectorNorm(v)
	if norm == 0 {
		return v
	}
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = float32(float64(x) / norm)
	}
	return out
}

func vectorLiteral(v []float32) string {
	var b strings.Builder
	b.Grow(len(v) * 10)
	b.WriteByte('[')
	for i, x := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(x), 'g', 6, 32))
	}
	b.WriteByte(']')
	return b.String()
}

type decoyMode string

const (
	decoyModeNoise decoyMode = "noise"
	decoyModeNear  decoyMode = "near"
)

func decoySigma(dim int, targetCos float64) float64 {
	c := targetCos
	if c <= 0 || c >= 1 {
		c = 0.5
	}
	return math.Sqrt((1/(c*c) - 1) / float64(dim))
}

func cosine(a, b []float32) float64 {
	var dot float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
	}
	return dot
}

func buildDecoyVectors(mode decoyMode, seedKeys []string, source map[string][]float32, n int) ([][]float32, float64, float64) {
	rng := rand.New(rand.NewPCG(decoySeed, decoySeed)) //nolint:gosec // G404: deterministic PRNG is the point; decoys must be reproducible.
	unit := make(map[string][]float32, len(seedKeys))
	for _, k := range seedKeys {
		unit[k] = normalizeVector(source[k])
	}
	out := make([][]float32, n)
	cosLo, cosHi := math.Inf(1), math.Inf(-1)

	for i := range out {
		key := seedKeys[rng.IntN(len(seedKeys))]
		base := unit[key]
		v := make([]float32, len(base))
		switch mode {
		case decoyModeNear:
			target := decoyCosLo + rng.Float64()*(decoyCosHi-decoyCosLo)
			sigma := decoySigma(len(base), target)
			for j := range v {
				v[j] = float32(float64(base[j]) + rng.NormFloat64()*sigma)
			}
			v = normalizeVector(v)
			c := cosine(v, base)
			cosLo = math.Min(cosLo, c)
			cosHi = math.Max(cosHi, c)
		default:
			for j := range v {
				v[j] = float32(rng.NormFloat64())
			}
			v = normalizeVector(v)
			c := cosine(v, base)
			cosLo = math.Min(cosLo, c)
			cosHi = math.Max(cosHi, c)
		}
		out[i] = v
	}
	return out, cosLo, cosHi
}

func seedRecallDecoys(t *testing.T, pool *pgxpool.Pool, userID, otherUserID int, corpus recallCorpus, vectors recallVectorSet, n int, mode decoyMode) (float64, float64) {
	t.Helper()
	if n <= 0 {
		return 0, 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	keys := make([]string, 0, len(corpus.Feeds))
	for _, f := range corpus.Feeds {
		keys = append(keys, f.Key)
	}
	sort.Strings(keys)
	decoys, cosLo, cosHi := buildDecoyVectors(mode, keys, vectors.Feeds, n)

	ids := make([]int32, 0, n)
	owners := make([]int64, 0, n)
	embs := make([]string, 0, n)

	for offset := 0; offset < n; offset += decoyBatch {
		end := min(offset+decoyBatch, n)
		rows, err := pool.Query(ctx,
			`INSERT INTO feeds (user_id, title, description)
			 SELECT CASE WHEN g % 2 = 0 THEN $1::bigint ELSE $2::bigint END,
			        'decoy ' || $5 || ' ' || g,
			        'synthetic filler entry ' || $5 || ' ' || g
			 FROM generate_series($3::int, $4::int) g
			 RETURNING id, user_id`,
			userID, otherUserID, offset+1, end, string(mode))
		require.NoError(t, err)
		for rows.Next() {
			var id int32
			var owner int64
			require.NoError(t, rows.Scan(&id, &owner))
			ids = append(ids, id)
			owners = append(owners, owner)
			embs = append(embs, vectorLiteral(decoys[len(ids)-1]))
		}
		rows.Close()
		require.NoError(t, rows.Err())
	}
	require.Len(t, ids, n, "decoy feed insert returned a different row count than requested")

	_, err := pool.Exec(ctx,
		`INSERT INTO feed_embeddings (feed_id, user_id, embedding, chunk_text, content_hash, embedded_at)
		 SELECT t.id, t.owner, t.emb::vector, 'synthetic filler entry', '\x00'::bytea, now()
		 FROM unnest($1::int[], $2::bigint[], $3::text[]) AS t(id, owner, emb)`,
		ids, owners, embs)
	require.NoError(t, err)
	return cosLo, cosHi
}

func passageFeedIDs(passages []Passage) []int {
	out := make([]int, 0, len(passages))
	for _, p := range passages {
		out = append(out, p.FeedID)
	}
	return out
}

func hitsWithin(top []int, relevant map[int]struct{}, limit int) int {
	n := 0
	for i, id := range top {
		if i >= limit {
			break
		}
		if _, ok := relevant[id]; ok {
			n++
		}
	}
	return n
}

func evaluateQuery(q recallQuerySpec, relevantIDs []int, passages []Passage, exactIDs []int) recallQueryResult {
	relevant := make(map[int]struct{}, len(relevantIDs))
	for _, id := range relevantIDs {
		relevant[id] = struct{}{}
	}
	top := passageFeedIDs(passages)
	res := recallQueryResult{
		Key:            q.Key,
		Category:       q.Category,
		Relevant:       relevantIDs,
		Retrieved:      top,
		ExactRetrieved: exactIDs,
		Hits:           hitsWithin(top, relevant, recallK),
		HitsAt5:        hitsWithin(top, relevant, recallKOffsets),
		MinRelScore:    math.Inf(1),
		MaxIrrelScore:  math.Inf(-1),
	}
	if len(relevantIDs) > 0 {
		den := float64(len(relevantIDs))
		res.RecallAt1 = float64(hitsWithin(top, relevant, 1)) / den
		res.RecallAt3 = float64(hitsWithin(top, relevant, 3)) / den
		res.RecallAt5 = float64(hitsWithin(top, relevant, recallKOffsets)) / den
		res.RecallAt10 = float64(res.Hits) / den
	}
	for rank, p := range passages {
		if _, ok := relevant[p.FeedID]; ok {
			if res.FirstRank == 0 {
				res.FirstRank = rank + 1
			}
			if !res.HasRelScore || p.Score < res.MinRelScore {
				res.MinRelScore = p.Score
				res.HasRelScore = true
			}
		} else if !res.HasIrrelScore || p.Score > res.MaxIrrelScore {
			res.MaxIrrelScore = p.Score
			res.HasIrrelScore = true
		}
	}
	if !res.HasRelScore {
		res.MinRelScore = 0
	}
	if !res.HasIrrelScore {
		res.MaxIrrelScore = 0
	}
	if len(passages) > 0 {
		res.TopScore = passages[0].Score
	}
	return res
}

func meanOf(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	sum := 0.0
	for _, v := range vals {
		sum += v
	}
	return sum / float64(len(vals))
}

func decoyCountFromEnv() int {
	raw := os.Getenv("NYX_RAG_RECALL_DECOYS")
	if raw == "" {
		return 2000
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 2000
	}
	return n
}

func readProvenance(ctx context.Context, pool *pgxpool.Pool, model string) recallProvenance {
	p := recallProvenance{EmbeddingModel: model}
	_ = pool.QueryRow(ctx,
		`SELECT current_setting('server_version'),
		        COALESCE((SELECT extversion FROM pg_extension WHERE extname = 'vector'), 'absent'),
		        COALESCE(current_setting('hnsw.ef_search', true), 'unset')`).Scan(
		&p.PGVersion, &p.PgvectorVersion, &p.HNSEFSearch)
	return p
}

func runRecallScenario(t *testing.T, pool *pgxpool.Pool, corpus recallCorpus, vectors recallVectorSet, name string, decoys int, mode decoyMode) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	purgeRecallRows(ctx, pool)

	suffix := time.Now().Format("150405.000000000")
	owner := makeUser(t, pool, "rag_recall_"+name+"_"+suffix)
	other := makeUser(t, pool, "rag_recall_other_"+name+"_"+suffix)
	t.Cleanup(func() { purgeRecallRows(context.Background(), pool) })

	feedIDByKey := seedRecallFeeds(t, pool, owner, corpus, vectors)
	cosLo, cosHi := seedRecallDecoys(t, pool, owner, other, corpus, vectors, decoys, mode)

	var indexedRows, ownerRows int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM feed_embeddings`).Scan(&indexedRows))
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM feed_embeddings WHERE user_id = $1`, owner).Scan(&ownerRows))

	tracer := noop.NewTracerProvider().Tracer("rag-recall")
	retriever := NewRetriever(pool, tracer)

	exactTx, err := pool.Begin(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = exactTx.Rollback(context.Background()) })
	_, err = exactTx.Exec(ctx, `SET LOCAL enable_indexscan = off`)
	require.NoError(t, err)
	_, err = exactTx.Exec(ctx, `SET LOCAL enable_indexonlyscan = off`)
	require.NoError(t, err)
	_, err = exactTx.Exec(ctx, `SET LOCAL enable_bitmapscan = off`)
	require.NoError(t, err)
	var idxScanOff string
	require.NoError(t, exactTx.QueryRow(ctx,
		`SELECT current_setting('enable_indexscan')`).Scan(&idxScanOff))
	require.Equal(t, "off", idxScanOff)
	exactRetriever := NewRetriever(exactTx, tracer)

	ownerFeedIDs := make(map[int]struct{}, ownerRows)
	ownerRowsRes, err := pool.Query(ctx, `SELECT id FROM feeds WHERE user_id = $1`, owner)
	require.NoError(t, err)
	for ownerRowsRes.Next() {
		var id int
		require.NoError(t, ownerRowsRes.Scan(&id))
		ownerFeedIDs[id] = struct{}{}
	}
	ownerRowsRes.Close()
	require.NoError(t, ownerRowsRes.Err())
	require.Len(t, ownerFeedIDs, ownerRows,
		"owner owns %d feeds but the isolation set holds %d; the seeded corpus and the owner row count disagree",
		ownerRows, len(ownerFeedIDs))

	randomVec := normalizeVector(func() []float32 {
		rng := rand.New(rand.NewPCG(decoySeed+1, decoySeed+1)) //nolint:gosec // G404: deterministic PRNG keeps the random-control reproducible.
		v := make([]float32, recallVectorDims)
		for i := range v {
			v[i] = float32(rng.NormFloat64())
		}
		return v
	}())

	report := recallScenarioReport{
		Scenario:          name,
		Provenance:        readProvenance(ctx, pool, vectors.Model),
		IndexedRows:       indexedRows,
		OwnerRows:         ownerRows,
		CorpusFeeds:       len(corpus.Feeds),
		Decoys:            decoys,
		DecoyMode:         string(mode),
		DecoyCosLo:        cosLo,
		DecoyCosHi:        cosHi,
		K:                 recallK,
		ProductionK:       corpus.TopK,
		PerCategoryRecall: map[string]float64{},
	}

	collector := newRecallCollector(&report, corpus, vectors, feedIDByKey, ownerFeedIDs, retriever, exactRetriever, owner, randomVec)
	for _, q := range corpus.Queries {
		collector.collect(ctx, t, q)
	}

	require.Positive(t, report.LabeledQueries, "corpus has no labelled queries")

	collector.finalize(ownerRows)

	report.ANNExercised = indexedRows >= annExercisedMinRow
	if report.ANNExercised {
		report.ANNRecallDelta = report.RecallAt5 - report.ExactRecallAt5
	}

	emitRecallReport(t, report)

	require.Greaterf(t, report.RecallAt5, report.TrivialBaseRate5,
		"recall@%d=%.3f does not beat the %.3f you get from returning %d of the owner's %d docs at random",
		recallKOffsets, report.RecallAt5, report.TrivialBaseRate5, recallKOffsets, ownerRows)
	require.Greaterf(t, report.RecallAt5, report.RandomControlRecall5,
		"a fixed random query vector already recalls %.3f against a real-query %.3f; the metric is not responding to the query vector",
		report.RandomControlRecall5, report.RecallAt5)

	if name != "corpus_only" {
		return
	}
	require.Equalf(t, 0, report.UnderfillQueries,
		"retrieval underfilled on %d queries at %d rows; the corpus is far too small for an ANN filter to cause that",
		report.UnderfillQueries, indexedRows)
	require.Greaterf(t, report.DistinctTop1, report.LabeledQueries/2,
		"only %d distinct top-1 feeds across %d labelled queries; an unbound, ignored or degenerate query vector returns the same rows every time",
		report.DistinctTop1, report.LabeledQueries)
	require.Greaterf(t, report.RecallAt5, recallFloor5,
		"recall@%d fell to %.3f against the pinned baseline of %.2f", recallKOffsets, report.RecallAt5, recallFloor5)
}

type recallCollector struct {
	report         *recallScenarioReport
	vectors        recallVectorSet
	feedIDByKey    map[string]int
	ownerFeedIDs   map[int]struct{}
	retriever      *Retriever
	exactRetriever *Retriever
	owner          int
	randomVec      []float32

	recalls1, recalls3, recalls5, recalls10 []float64
	exact5, hitsAny, rr                     []float64
	relScores, irrelScores                  []float64
	negTop1, negMaxScore                    []float64
	random5, random10                       []float64
	catHits, catTotal                       map[string]int
	distinctTop1                            map[int]struct{}
}

func newRecallCollector(report *recallScenarioReport, corpus recallCorpus, vectors recallVectorSet, feedIDByKey map[string]int, ownerFeedIDs map[int]struct{}, retriever, exactRetriever *Retriever, owner int, randomVec []float32) *recallCollector {
	n := len(corpus.Queries)
	return &recallCollector{
		report:         report,
		vectors:        vectors,
		feedIDByKey:    feedIDByKey,
		ownerFeedIDs:   ownerFeedIDs,
		retriever:      retriever,
		exactRetriever: exactRetriever,
		owner:          owner,
		randomVec:      randomVec,
		recalls1:       make([]float64, 0, n),
		recalls3:       make([]float64, 0, n),
		recalls5:       make([]float64, 0, n),
		recalls10:      make([]float64, 0, n),
		exact5:         make([]float64, 0, n),
		hitsAny:        make([]float64, 0, n),
		rr:             make([]float64, 0, n),
		relScores:      make([]float64, 0, n),
		irrelScores:    make([]float64, 0, n),
		negTop1:        make([]float64, 0, n),
		negMaxScore:    make([]float64, 0, n),
		random5:        make([]float64, 0, n),
		random10:       make([]float64, 0, n),
		catHits:        map[string]int{},
		catTotal:       map[string]int{},
		distinctTop1:   map[int]struct{}{},
	}
}

func (c *recallCollector) collect(ctx context.Context, t *testing.T, q recallQuerySpec) {
	vec, ok := c.vectors.Queries[q.Key]
	require.True(t, ok)

	passages, err := c.retriever.Retrieve(ctx, c.owner, vec, recallK)
	require.NoErrorf(t, err, "query %s", q.Key)
	for _, p := range passages {
		require.Containsf(t, c.ownerFeedIDs, p.FeedID,
			"query %s returned feed %d, which the requesting user does not own; the user_id filter leaked", q.Key, p.FeedID)
	}

	exactPassages, err := c.exactRetriever.Retrieve(ctx, c.owner, vec, recallK)
	require.NoErrorf(t, err, "exact-scan query %s", q.Key)

	if len(passages) < recallK {
		c.report.UnderfillQueries++
	}

	if len(q.Relevant) == 0 {
		c.collectNegative(q, passages, exactPassages)
		return
	}
	c.collectLabeled(ctx, t, q, passages, exactPassages)
}

func (c *recallCollector) collectNegative(q recallQuerySpec, passages, exactPassages []Passage) {
	c.report.NegativeQueries++
	c.negTop1 = append(c.negTop1, passages[0].Score)
	res := evaluateQuery(q, nil, passages, passageFeedIDs(exactPassages))
	c.negMaxScore = append(c.negMaxScore, res.MaxIrrelScore)
	c.report.Queries = append(c.report.Queries, res)
}

func (c *recallCollector) collectLabeled(ctx context.Context, t *testing.T, q recallQuerySpec, passages, exactPassages []Passage) {
	relevantIDs := make([]int, 0, len(q.Relevant))
	for _, key := range q.Relevant {
		relevantIDs = append(relevantIDs, c.feedIDByKey[key])
	}
	res := evaluateQuery(q, relevantIDs, passages, passageFeedIDs(exactPassages))

	randomPassages, err := c.retriever.Retrieve(ctx, c.owner, c.randomVec, recallK)
	require.NoError(t, err)
	randomRes := evaluateQuery(q, relevantIDs, randomPassages, nil)
	res.RandomHits = randomRes.Hits
	c.report.RandomControlHits += res.RandomHits
	c.random5 = append(c.random5, randomRes.RecallAt5)
	c.random10 = append(c.random10, randomRes.RecallAt10)
	if len(res.Retrieved) > 0 {
		c.distinctTop1[res.Retrieved[0]] = struct{}{}
	}

	exactRelevant := make(map[int]struct{}, len(relevantIDs))
	for _, id := range relevantIDs {
		exactRelevant[id] = struct{}{}
	}
	c.exact5 = append(c.exact5, float64(hitsWithin(passageFeedIDs(exactPassages), exactRelevant, recallKOffsets))/float64(len(relevantIDs)))

	c.report.Queries = append(c.report.Queries, res)
	c.report.LabeledQueries++
	c.recalls1 = append(c.recalls1, res.RecallAt1)
	c.recalls3 = append(c.recalls3, res.RecallAt3)
	c.recalls5 = append(c.recalls5, res.RecallAt5)
	c.recalls10 = append(c.recalls10, res.RecallAt10)
	c.catHits[q.Category] += hitsWithin(res.Retrieved, exactRelevant, recallKOffsets)
	c.catTotal[q.Category] += len(res.Relevant)
	if res.HitsAt5 > 0 {
		c.hitsAny = append(c.hitsAny, 1)
	}
	if res.FirstRank > 0 && res.FirstRank <= recallKOffsets {
		c.rr = append(c.rr, 1/float64(res.FirstRank))
	}
	if res.HasRelScore {
		c.relScores = append(c.relScores, res.MinRelScore)
	}
	if res.HasIrrelScore {
		c.irrelScores = append(c.irrelScores, res.MaxIrrelScore)
	}
	if len(res.Retrieved) != len(res.ExactRetrieved) ||
		!sameIDSet(res.Retrieved, res.ExactRetrieved) {
		c.report.ANNDivergentQueries++
	}
}

func (c *recallCollector) finalize(ownerRows int) {
	r := c.report
	r.RecallAt1 = meanOf(c.recalls1)
	r.RecallAt3 = meanOf(c.recalls3)
	r.RecallAt5 = meanOf(c.recalls5)
	r.RecallAt10 = meanOf(c.recalls10)
	r.ExactRecallAt5 = meanOf(c.exact5)
	r.HitRateAt5 = float64(len(c.hitsAny)) / float64(r.LabeledQueries)
	r.MRRAt5 = meanOf(c.rr)
	r.RandomControlRecall = meanOf(c.random10)
	r.RandomControlRecall5 = meanOf(c.random5)
	r.TrivialBaseRate5 = float64(recallKOffsets) / float64(ownerRows)
	r.MeanRelScore = meanOf(c.relScores)
	r.MeanIrrelScore = meanOf(c.irrelScores)
	r.NegativeTop1Mean = meanOf(c.negTop1)
	r.NegativeMaxIrrelScore = meanOf(c.negMaxScore)
	r.DistinctTop1 = len(c.distinctTop1)
	for cat, hits := range c.catHits {
		r.PerCategoryRecall[cat] = float64(hits) / float64(c.catTotal[cat])
	}
}

func sameIDSet(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[int]struct{}, len(a))
	for _, id := range a {
		seen[id] = struct{}{}
	}
	for _, id := range b {
		if _, ok := seen[id]; !ok {
			return false
		}
	}
	return true
}

func sortedKeys(m map[string]float64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func emitRecallReport(t *testing.T, r recallScenarioReport) {
	t.Logf("")
	t.Logf("scenario=%s rows=%d (owner %d: corpus %d + %d decoys mode=%s cos_to_base=[%.3f,%.3f]) k=%d production_top_k=%d labelled=%d negatives=%d",
		r.Scenario, r.IndexedRows, r.OwnerRows, r.CorpusFeeds, r.Decoys, r.DecoyMode, r.DecoyCosLo, r.DecoyCosHi,
		r.K, r.ProductionK, r.LabeledQueries, r.NegativeQueries)
	t.Logf("  env: pg=%s pgvector=%s hnsw.ef_search=%s model=%s",
		r.Provenance.PGVersion, r.Provenance.PgvectorVersion, r.Provenance.HNSEFSearch, r.Provenance.EmbeddingModel)
	t.Logf("  recall@1=%.3f recall@3=%.3f recall@5=%.3f recall@10=%.3f hit@5=%.3f mrr@5=%.3f exact_recall@5=%.3f",
		r.RecallAt1, r.RecallAt3, r.RecallAt5, r.RecallAt10, r.HitRateAt5, r.MRRAt5, r.ExactRecallAt5)
	t.Logf("  random_control recall@5=%.4f recall@10=%.4f  trivial_base_rate@5=%.4f  distinct_top1=%d/%d  underfill=%d",
		r.RandomControlRecall5, r.RandomControlRecall, r.TrivialBaseRate5, r.DistinctTop1, r.LabeledQueries, r.UnderfillQueries)
	t.Logf("  mean_relevant_score=%.4f  mean_irrelevant_score=%.4f  negative_query_mean_top1=%.4f  negative_query_mean_max_irrelevant=%.4f",
		r.MeanRelScore, r.MeanIrrelScore, r.NegativeTop1Mean, r.NegativeMaxIrrelScore)
	if r.ANNExercised {
		t.Logf("  ANN exercised at %d rows: divergent=%d/%d recall@5 delta=%+.4f",
			r.IndexedRows, r.ANNDivergentQueries, r.LabeledQueries, r.ANNRecallDelta)
	} else {
		t.Logf("  ANN NOT exercised (%d rows < %d): divergence would be structurally 0 and is withheld",
			r.IndexedRows, annExercisedMinRow)
	}
	for _, cat := range sortedKeys(r.PerCategoryRecall) {
		t.Logf("  category %-22s recall@%d=%.3f", cat, r.ProductionK, r.PerCategoryRecall[cat])
	}
	for _, q := range r.Queries {
		t.Logf("    %-30s %-22s hits@5=%d/%d hits@10=%d first_rank=%d top1=%.4f min_rel=%.4f max_irrel=%.4f",
			q.Key, q.Category, q.HitsAt5, len(q.Relevant), q.Hits, q.FirstRank, q.TopScore, q.MinRelScore, q.MaxIrrelScore)
	}
	if path := os.Getenv("NYX_RAG_RECALL_REPORT"); path != "" {
		raw, err := json.MarshalIndent(r, "", "  ")
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(path, raw, 0o600))
		t.Logf("  wrote %s", path)
	}
}

func TestRetrieveRecallBaseline(t *testing.T) {
	pool := requireRecallDB(t)
	corpus, vectors := loadRecallFixtures(t)

	scenarios := []struct {
		name   string
		decoys int
		mode   decoyMode
	}{
		{name: "corpus_only", decoys: 0, mode: decoyModeNoise},
		{name: "scaled_noise", decoys: decoyCountFromEnv(), mode: decoyModeNoise},
		{name: "scaled_near", decoys: decoyCountFromEnv(), mode: decoyModeNear},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			runRecallScenario(t, pool, corpus, vectors, sc.name, sc.decoys, sc.mode)
		})
	}
}

func TestRegenerateRecallVectors(t *testing.T) {
	if os.Getenv("NYX_RAG_RECALL_REGEN") != "1" {
		t.Skip("set NYX_RAG_RECALL_REGEN=1 with a live embedder to regenerate testdata/" + recallVectorsFile)
	}
	baseURL := os.Getenv("NYX_RAG_RECALL_EMBED_URL")
	require.NotEmpty(t, baseURL, "NYX_RAG_RECALL_REGEN=1 requires NYX_RAG_RECALL_EMBED_URL")
	model := os.Getenv("NYX_RAG_RECALL_EMBED_MODEL")
	require.NotEmpty(t, model, "NYX_RAG_RECALL_REGEN=1 requires NYX_RAG_RECALL_EMBED_MODEL")

	apiKey := os.Getenv("NYX_RAG_RECALL_EMBED_KEY")
	if apiKey == "" {
		apiKey = "EMPTY"
	}

	corpus := loadRecallCorpus(t)
	tracer := noop.NewTracerProvider().Tracer("rag-recall-regen")

	client, err := openai.NewClient(&config.Config{
		LLMEnabled: true,
		LLMBaseURL: baseURL,
		LLMAPIKey:  apiKey,
		LLMModel:   model,
	}, tracer)
	require.NoError(t, err)

	embedder := NewEmbedder(client, model, tracer)

	feeds := make([]*feed.Feed, 0, len(corpus.Feeds))
	queryTexts := make([]string, 0, len(corpus.Queries))
	for _, spec := range corpus.Feeds {
		feeds = append(feeds, &feed.Feed{Title: spec.Title, Description: spec.Description})
	}
	for _, q := range corpus.Queries {
		queryTexts = append(queryTexts, q.Text)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	feedVecs, _, err := embedder.EmbedBatch(ctx, feeds)
	require.NoError(t, err)
	require.Len(t, feedVecs, len(feeds))

	queryVecs, err := embedder.EmbedStrings(ctx, queryTexts)
	require.NoError(t, err)
	require.Len(t, queryVecs, len(queryTexts))

	out := recallVectorSet{
		Version:    corpus.Version,
		Model:      model,
		Dimensions: recallVectorDims,
		Feeds:      make(map[string][]float32, len(corpus.Feeds)),
		Queries:    make(map[string][]float32, len(corpus.Queries)),
	}
	for i, spec := range corpus.Feeds {
		require.Lenf(t, feedVecs[i], recallVectorDims, "feed %s came back with the wrong dim", spec.Key)
		out.Feeds[spec.Key] = normalizeVector(feedVecs[i])
	}
	for i, q := range corpus.Queries {
		require.Lenf(t, queryVecs[i], recallVectorDims, "query %s came back with the wrong dim", q.Key)
		out.Queries[q.Key] = normalizeVector(queryVecs[i])
	}

	raw, err := json.MarshalIndent(out, "", " ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(recallFixturePath(t, recallVectorsFile), raw, 0o600))
	t.Logf("wrote testdata/%s: %d feed vectors, %d query vectors, model=%s",
		recallVectorsFile, len(out.Feeds), len(out.Queries), model)
}
