package rag

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace/noop"

	"nyx/internal/kb"
	"nyx/internal/llm"
)

const rerankEvalTopK = 10

type rerankEvalDocument struct {
	Path string `json:"path"`
}

type rerankEvalQuery struct {
	Key      string   `json:"key"`
	Category string   `json:"category"`
	Text     string   `json:"text"`
	Path     string   `json:"path,omitempty"`
	Relevant []string `json:"relevant"`
}

type rerankEvalCorpus struct {
	Documents []rerankEvalDocument `json:"documents"`
	Queries   []rerankEvalQuery    `json:"queries"`
}

type rerankEvalChunkVector struct {
	TextSHA256 string    `json:"text_sha256"`
	Vector     []float32 `json:"vector"`
}

type rerankEvalVectors struct {
	Model   string                           `json:"model"`
	Chunks  map[string]rerankEvalChunkVector `json:"chunks"`
	Queries map[string][]float32             `json:"queries"`
}

type rerankEvalChunk struct {
	Key         string
	RelPath     string
	HeadingPath []string
	Content     string
	Vector      []float32
}

type rerankEvalProvider struct {
	byText map[string][]float32
}

func (p *rerankEvalProvider) Chat(context.Context, llm.ChatRequest, func(string, *llm.ChatUsage) error) (*llm.ChatUsage, error) {
	panic("rerank recall baseline never chats")
}

func (p *rerankEvalProvider) Embed(_ context.Context, req llm.EmbedRequest) ([][]float32, error) {
	out := make([][]float32, len(req.Inputs))
	for i, input := range req.Inputs {
		out[i] = p.byText[input]
	}
	return out, nil
}

type rerankEvalKBRetriever struct {
	chunks []rerankEvalChunk
}

func (r *rerankEvalKBRetriever) Retrieve(_ context.Context, _ int64, queryVec []float32, _ string, k int, _ *int64, pathPrefix string) ([]kb.Passage, error) {
	type scored struct {
		chunk rerankEvalChunk
		score float64
	}
	ranked := make([]scored, 0, len(r.chunks))
	for _, chunk := range r.chunks {
		if pathPrefix != "" && !strings.HasPrefix(chunk.RelPath, pathPrefix) {
			continue
		}
		ranked = append(ranked, scored{chunk: chunk, score: rerankEvalCosine(queryVec, chunk.Vector)})
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		return ranked[i].chunk.Key < ranked[j].chunk.Key
	})
	if len(ranked) > k {
		ranked = ranked[:k]
	}
	out := make([]kb.Passage, len(ranked))
	for i, item := range ranked {
		out[i] = kb.Passage{
			RelPath:     item.chunk.RelPath,
			HeadingPath: item.chunk.HeadingPath,
			Content:     item.chunk.Content,
			Score:       item.score,
		}
	}
	return out, nil
}

func TestKbRecallRerankBaseline(t *testing.T) {
	baseURL := strings.TrimSpace(os.Getenv("NYX_KB_RECALL_RERANK_URL"))
	if baseURL == "" {
		t.Skip("set NYX_KB_RECALL_RERANK_URL to an open POST /rerank endpoint to measure rerank quality")
	}
	model := strings.TrimSpace(os.Getenv("NYX_KB_RECALL_RERANK_MODEL"))
	require.NotEmpty(t, model, "NYX_KB_RECALL_RERANK_URL requires NYX_KB_RECALL_RERANK_MODEL")

	corpus, chunks, vectors := loadRerankEvalFixture(t)
	require.Equal(t, len(chunks), len(vectors.Chunks))

	tracer := noop.NewTracerProvider().Tracer("test")
	reranker := NewHTTPReranker(baseURL, strings.TrimSpace(os.Getenv("NYX_KB_RECALL_RERANK_KEY")), model, 30*time.Second, true, tracer)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	_, err := reranker.Rerank(ctx, "rerank connectivity probe", []string{"probe document"})
	require.NoError(t, err, "the reranker endpoint must answer before the baseline runs")

	byText := map[string][]float32{}
	queries := make([]rerankEvalQuery, 0, len(corpus.Queries))
	for _, query := range corpus.Queries {
		if query.Category == "negative" {
			continue
		}
		vec, ok := vectors.Queries[query.Key]
		require.Truef(t, ok, "query %s has no frozen vector", query.Key)
		byText[query.Text] = vec
		queries = append(queries, query)
	}

	embedder := NewEmbedder(&rerankEvalProvider{byText: byText}, vectors.Model, tracer)
	retriever := &rerankEvalKBRetriever{chunks: chunks}
	denseService := NewService(embedder, nil, nil, stubFeedRepo{}, rerankEvalTopK, 4000, 20, tracer)
	denseService.SetKnowledgeRetriever(retriever, rerankEvalTopK, 4000)
	rerankService := NewService(embedder, nil, nil, stubFeedRepo{}, rerankEvalTopK, 4000, 20, tracer)
	rerankService.SetKnowledgeRetriever(retriever, rerankEvalTopK, 4000)
	rerankService.SetReranker(func(context.Context, int) (Reranker, error) { return reranker, nil }, 20, 4000)

	denseCategories, denseOverall := measureRerankEval(t, ctx, denseService, queries)
	rerankCategories, rerankOverall := measureRerankEval(t, ctx, rerankService, queries)

	t.Logf("rerank baseline: model=%s chunks=%d labelled=%d candidates=20 result_k=%d", model, len(chunks), rerankOverall.queries, rerankEvalTopK)
	t.Logf("dense  (no rerank, k=%d): recall@1=%.3f recall@5=%.3f recall@10=%.3f hit@5=%.3f first=%.3f",
		rerankEvalTopK, denseOverall.r1, denseOverall.r5, denseOverall.r10, denseOverall.hit5, denseOverall.first)
	t.Logf("rerank (%d->%d): recall@1=%.3f recall@5=%.3f recall@10=%.3f hit@5=%.3f first=%.3f",
		20, rerankEvalTopK, rerankOverall.r1, rerankOverall.r5, rerankOverall.r10, rerankOverall.hit5, rerankOverall.first)
	t.Logf("%-24s %5s %8s %8s %8s %8s %10s", "category", "n", "dense@1", "rerank@1", "dense@5", "rerank@5", "rerank_first")
	names := make([]string, 0, len(rerankCategories))
	for name := range rerankCategories {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range append(names, "ALL") {
		entry := rerankCategories[name]
		base := denseCategories[name]
		if name == "ALL" {
			entry, base = rerankOverall, denseOverall
		}
		t.Logf("%-24s %5d %8.3f %8.3f %8.3f %8.3f %10.3f",
			name, entry.queries,
			base.r1, entry.r1, base.r5, entry.r5, entry.first,
		)
	}
}

type rerankEvalAggregate struct {
	queries int
	r1      float64
	r5      float64
	r10     float64
	hit5    float64
	first   float64
}

func measureRerankEval(t *testing.T, ctx context.Context, service *Service, queries []rerankEvalQuery) (map[string]*rerankEvalAggregate, *rerankEvalAggregate) {
	t.Helper()
	categories := map[string]*rerankEvalAggregate{}
	overall := &rerankEvalAggregate{}
	for _, query := range queries {
		result, err := service.RetrieveCorpus(ctx, 1, query.Text, RetrievalOptions{
			Corpus:     CorpusKB,
			PathPrefix: query.Path,
			TopK:       rerankEvalTopK,
		})
		require.NoErrorf(t, err, "query %s", query.Key)
		retrieved := make([]string, 0, len(result.Passages))
		for _, passage := range result.Passages {
			retrieved = append(retrieved, rerankEvalChunkKey(passage.Knowledge.RelPath, passage.Knowledge.HeadingPath))
		}
		r1, r5, r10, hit5, first := rerankEvalScore(retrieved, query.Relevant)
		entry := categories[query.Category]
		if entry == nil {
			entry = &rerankEvalAggregate{}
			categories[query.Category] = entry
		}
		for _, target := range []*rerankEvalAggregate{entry, overall} {
			target.queries++
			target.r1 += r1
			target.r5 += r5
			target.r10 += r10
			target.hit5 += hit5
			target.first += float64(first)
		}
	}
	finish := func(target *rerankEvalAggregate) {
		denom := float64(target.queries)
		target.r1 /= denom
		target.r5 /= denom
		target.r10 /= denom
		target.hit5 /= denom
		target.first /= denom
	}
	finish(overall)
	for _, entry := range categories {
		finish(entry)
	}
	return categories, overall
}

func loadRerankEvalFixture(t *testing.T) (rerankEvalCorpus, []rerankEvalChunk, rerankEvalVectors) {
	t.Helper()
	base := filepath.Join("..", "kb", "testdata")
	var corpus rerankEvalCorpus
	//nolint:gosec // G304: fixture path is testdata-relative.
	raw, err := os.ReadFile(filepath.Join(base, "kb_recall_corpus.json"))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &corpus))

	var vectors rerankEvalVectors
	//nolint:gosec // G304: fixture path is testdata-relative.
	raw, err = os.ReadFile(filepath.Join(base, "kb_recall_vectors.json"))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &vectors))

	opts := kb.ChunkOptions{TargetTokens: 400, MaxTokens: 700, MinTokens: 50, OverlapTokens: 80}
	chunks := make([]rerankEvalChunk, 0, len(vectors.Chunks))
	for _, document := range corpus.Documents {
		path := filepath.Join(base, "kb_recall_docs", document.Path)
		//nolint:gosec // G304: fixture-relative document paths.
		raw, err := os.ReadFile(path)
		require.NoErrorf(t, err, "document %s", document.Path)
		doc, err := kb.NewMarkdownNormalizer().Normalize(context.Background(), raw, kb.Source{})
		require.NoErrorf(t, err, "document %s", document.Path)
		for _, spec := range kb.ChunkDocument(document.Path, doc, opts) {
			key := rerankEvalChunkKey(document.Path, spec.HeadingPath)
			frozen, ok := vectors.Chunks[key]
			require.Truef(t, ok, "chunk %s has no frozen vector", key)
			chunks = append(chunks, rerankEvalChunk{
				Key:         key,
				RelPath:     document.Path,
				HeadingPath: spec.HeadingPath,
				Content:     spec.Content,
				Vector:      frozen.Vector,
			})
		}
	}
	return corpus, chunks, vectors
}

func rerankEvalChunkKey(relPath string, heading []string) string {
	if len(heading) == 0 {
		return relPath + "::(root)"
	}
	return relPath + "::" + strings.Join(heading, " / ")
}

func rerankEvalScore(retrieved, relevant []string) (float64, float64, float64, float64, int) {
	hitsAt := func(k int) int {
		hits := 0
		for _, rel := range relevant {
			for i := 0; i < k && i < len(retrieved); i++ {
				if retrieved[i] == rel {
					hits++
					break
				}
			}
		}
		return hits
	}
	first := 0
	for i, key := range retrieved {
		found := false
		for _, rel := range relevant {
			if key == rel {
				found = true
				break
			}
		}
		if found {
			first = i + 1
			break
		}
	}
	denom := float64(len(relevant))
	hit5 := 0.0
	if hitsAt(5) > 0 {
		hit5 = 1
	}
	return float64(hitsAt(1)) / denom, float64(hitsAt(5)) / denom, float64(hitsAt(10)) / denom, hit5, first
}

func rerankEvalCosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return math.Inf(-1)
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return math.Inf(-1)
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
