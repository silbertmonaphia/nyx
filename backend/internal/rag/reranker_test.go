package rag

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace/noop"

	"nyx/internal/kb"
)

type stubReranker struct {
	mu        sync.Mutex
	scores    []float64
	err       error
	calls     int
	lastQuery string
	lastDocs  []string
	key       string
}

func (r *stubReranker) Rerank(_ context.Context, query string, docs []string) ([]float64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.lastQuery = query
	r.lastDocs = append([]string(nil), docs...)
	if r.err != nil {
		return nil, r.err
	}
	return r.scores, nil
}

func (r *stubReranker) Model() string { return "stub-reranker" }

func (r *stubReranker) Key() string {
	if r.key == "" {
		return "stub-reranker-key"
	}
	return r.key
}

func (r *stubReranker) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func newRerankerServer(t *testing.T, body string, status int) (*httptest.Server, *rerankRequest, *string) {
	t.Helper()
	var got rerankRequest
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		auth = req.Header.Get("Authorization")
		_ = json.NewDecoder(req.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, &got, &auth
}

func TestHTTPRerankerRequestShape(t *testing.T) {
	srv, got, auth := newRerankerServer(t, `{"results":[{"index":0,"relevance_score":0.5}]}`, http.StatusOK)
	reranker := NewHTTPReranker(srv.URL, "secret-key", "test-model", 2*time.Second, true, noop.NewTracerProvider().Tracer("test"))

	scores, err := reranker.Rerank(context.Background(), "the query", []string{"doc-a", "doc-b"})
	require.NoError(t, err)
	assert.Equal(t, 0.5, scores[0])
	assert.True(t, math.IsNaN(scores[1]))
	assert.Equal(t, "Bearer secret-key", *auth)
	assert.Equal(t, "test-model", got.Model)
	assert.Equal(t, "the query", got.Query)
	assert.Equal(t, []string{"doc-a", "doc-b"}, got.Documents)
	assert.Equal(t, 2, got.TopN)
}

func TestHTTPRerankerParsesVLLMFixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/rerank_response_vllm.json")
	require.NoError(t, err)

	var shape map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &shape))
	for _, key := range []string{"id", "model", "usage", "results"} {
		assert.Contains(t, shape, key, "fixture must match the vLLM RerankResponse schema")
	}
	var results []map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(shape["results"], &results))
	require.NotEmpty(t, results)
	for _, key := range []string{"index", "document", "relevance_score"} {
		assert.Contains(t, results[0], key, "fixture results must match the vLLM RerankResult schema")
	}

	srv, _, _ := newRerankerServer(t, string(raw), http.StatusOK)
	reranker := NewHTTPReranker(srv.URL, "", "Qwen/Qwen3-Reranker-0.6B", 2*time.Second, true, noop.NewTracerProvider().Tracer("test"))
	scores, err := reranker.Rerank(context.Background(), "q", []string{"doc zero", "doc one", "doc two"})
	require.NoError(t, err)
	assert.Equal(t, 0.93, scores[0])
	assert.Equal(t, 0.12, scores[1])
	assert.Equal(t, 0.41, scores[2])
}

func TestHTTPRerankerIgnoresBadIndices(t *testing.T) {
	body := `{"results":[{"index":5,"relevance_score":0.9},{"index":1,"relevance_score":0.8},{"index":1,"relevance_score":0.1},{"index":-1,"relevance_score":0.7}]}`
	srv, _, _ := newRerankerServer(t, body, http.StatusOK)
	reranker := NewHTTPReranker(srv.URL, "", "m", 2*time.Second, true, noop.NewTracerProvider().Tracer("test"))

	scores, err := reranker.Rerank(context.Background(), "q", []string{"a", "b", "c"})
	require.NoError(t, err)
	assert.True(t, math.IsNaN(scores[0]))
	assert.Equal(t, 0.8, scores[1], "the first entry for an index wins")
	assert.True(t, math.IsNaN(scores[2]))
}

func TestHTTPRerankerUpstreamErrorDoesNotLeakKey(t *testing.T) {
	srv, _, _ := newRerankerServer(t, `upstream failure`, http.StatusInternalServerError)
	reranker := NewHTTPReranker(srv.URL, "super-secret-key", "m", 2*time.Second, true, noop.NewTracerProvider().Tracer("test"))

	_, err := reranker.Rerank(context.Background(), "q", []string{"a"})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "super-secret-key")
	assert.Contains(t, err.Error(), "500")
}

func TestHTTPRerankerMalformedBody(t *testing.T) {
	srv, _, _ := newRerankerServer(t, `{"results":`, http.StatusOK)
	reranker := NewHTTPReranker(srv.URL, "", "m", 2*time.Second, true, noop.NewTracerProvider().Tracer("test"))

	_, err := reranker.Rerank(context.Background(), "q", []string{"a"})
	require.Error(t, err)
}

func TestHTTPRerankerTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		_, _ = io.WriteString(w, `{"results":[]}`)
	}))
	t.Cleanup(srv.Close)
	reranker := NewHTTPReranker(srv.URL, "", "m", 20*time.Millisecond, true, noop.NewTracerProvider().Tracer("test"))

	_, err := reranker.Rerank(context.Background(), "q", []string{"a"})
	require.Error(t, err)
}

func TestOrderCandidates(t *testing.T) {
	order := orderCandidates([]float64{math.NaN(), 0.2, 0.9, 0.2}, 4)
	assert.Equal(t, []int{2, 1, 3, 0}, order)

	order = orderCandidates([]float64{0.5}, 3)
	assert.Equal(t, []int{0, 1, 2}, order)

	assert.Nil(t, orderCandidates(nil, 0))
}

func TestServiceReranksFeedCandidates(t *testing.T) {
	feeds := []Passage{
		{FeedID: 1, Title: "A", Description: "a", Score: 0.9},
		{FeedID: 2, Title: "B", Description: "b", Score: 0.8},
		{FeedID: 3, Title: "C", Description: "c", Score: 0.7},
	}
	service, retriever, _ := newCorpusService(t, feeds, nil)
	reranker := &stubReranker{scores: []float64{0.1, 0.2, 0.99}}
	service.SetReranker(func(context.Context, int) (Reranker, error) { return reranker, nil }, 20, 4000)

	result, err := service.RetrieveCorpus(context.Background(), 42, "query", RetrievalOptions{Corpus: CorpusFeeds, TopK: 2})
	require.NoError(t, err)
	require.Len(t, result.Passages, 2)
	assert.Equal(t, 3, result.Passages[0].Feed.FeedID)
	assert.Equal(t, 2, result.Passages[1].Feed.FeedID)
	assert.Equal(t, 20, retriever.lastK, "the candidate pool is over-fetched")
	assert.Equal(t, []string{
		"A\n\na",
		"B\n\nb",
		"C\n\nc",
	}, reranker.lastDocs)
}

func TestServiceReranksKBWithEmbeddedPrefix(t *testing.T) {
	knowledge := []kb.Passage{
		{DocID: 1, SourceName: "s", RelPath: "B-Cook/diet.md", HeadingPath: []string{"煮食记"}, Content: "body-a", Score: 0.9},
		{DocID: 2, SourceName: "s", RelPath: "B-Sport/run.md", HeadingPath: []string{"跑步"}, Content: "body-b", Score: 0.8},
	}
	service, _, _ := newCorpusService(t, nil, knowledge)
	reranker := &stubReranker{scores: []float64{0.1, 0.9}}
	service.SetReranker(func(context.Context, int) (Reranker, error) { return reranker, nil }, 20, 4000)

	result, err := service.RetrieveCorpus(context.Background(), 42, "query", RetrievalOptions{Corpus: CorpusKB})
	require.NoError(t, err)
	require.Len(t, result.Passages, 2)
	assert.Equal(t, int64(2), result.Passages[0].Knowledge.DocID)
	require.Len(t, reranker.lastDocs, 2)
	assert.Equal(t, "B-Cook/diet.md › 煮食记\n\nbody-a", reranker.lastDocs[0])
}

func TestServiceReranksEachCorpusBeforeFusion(t *testing.T) {
	feeds := []Passage{
		{FeedID: 1, Title: "F1", Score: 0.9},
		{FeedID: 2, Title: "F2", Score: 0.8},
	}
	knowledge := []kb.Passage{
		{DocID: 10, SourceName: "s", RelPath: "a.md", Content: "K1", Score: 0.7},
	}
	service, _, _ := newCorpusService(t, feeds, knowledge)
	reranker := &stubReranker{scores: []float64{0.1, 0.9, 0.95}}
	service.SetReranker(func(context.Context, int) (Reranker, error) { return reranker, nil }, 20, 4000)

	result, err := service.RetrieveCorpus(context.Background(), 42, "query", RetrievalOptions{Corpus: CorpusBoth})
	require.NoError(t, err)
	require.Len(t, result.Passages, 3)
	assert.Equal(t, 2, result.Passages[0].Feed.FeedID)
	assert.Equal(t, int64(10), result.Passages[1].Knowledge.DocID)
	assert.Equal(t, 1, result.Passages[2].Feed.FeedID)
	assert.Equal(t, 2, reranker.callCount(), "one rerank call per corpus")
}

func TestServiceRerankFailureFallsBackToDenseOrder(t *testing.T) {
	feeds := []Passage{
		{FeedID: 1, Title: "A", Score: 0.9},
		{FeedID: 2, Title: "B", Score: 0.8},
		{FeedID: 3, Title: "C", Score: 0.7},
	}
	service, _, _ := newCorpusService(t, feeds, nil)
	reranker := &stubReranker{err: assert.AnError}
	service.SetReranker(func(context.Context, int) (Reranker, error) { return reranker, nil }, 20, 4000)

	result, err := service.RetrieveCorpus(context.Background(), 42, "query", RetrievalOptions{Corpus: CorpusFeeds, TopK: 2})
	require.NoError(t, err)
	require.Len(t, result.Passages, 2)
	assert.Equal(t, 1, result.Passages[0].Feed.FeedID)
	assert.Equal(t, 2, result.Passages[1].Feed.FeedID)
}

func TestServiceRerankCooldownAndRecovery(t *testing.T) {
	feeds := []Passage{
		{FeedID: 1, Title: "A", Score: 0.9},
		{FeedID: 2, Title: "B", Score: 0.8},
	}
	service, _, _ := newCorpusService(t, feeds, nil)
	reranker := &stubReranker{err: assert.AnError}
	service.SetReranker(func(context.Context, int) (Reranker, error) { return reranker, nil }, 20, 4000)

	for i := 0; i < rerankFailureThreshold; i++ {
		_, err := service.RetrieveCorpus(context.Background(), 42, "query", RetrievalOptions{Corpus: CorpusFeeds})
		require.NoError(t, err)
	}
	require.Equal(t, rerankFailureThreshold, reranker.callCount())

	_, err := service.RetrieveCorpus(context.Background(), 42, "query", RetrievalOptions{Corpus: CorpusFeeds})
	require.NoError(t, err)
	assert.Equal(t, rerankFailureThreshold, reranker.callCount(), "a cooldown skips the reranker call")

	reranker.mu.Lock()
	reranker.err = nil
	reranker.scores = []float64{0.1, 0.9}
	reranker.mu.Unlock()

	service.breakMu.Lock()
	breaker := service.breakers[reranker.Key()]
	breaker.openUntil = time.Now().Add(-time.Second)
	service.breakMu.Unlock()

	result, err := service.RetrieveCorpus(context.Background(), 42, "query", RetrievalOptions{Corpus: CorpusFeeds})
	require.NoError(t, err)
	assert.Equal(t, rerankFailureThreshold+1, reranker.callCount(), "the post-cooldown probe runs")
	assert.Equal(t, 2, result.Passages[0].Feed.FeedID)
}

func TestTruncateUTF8Boundary(t *testing.T) {
	text := "a界b"
	assert.Equal(t, "", truncateUTF8(text, 0))
	assert.Equal(t, "a", truncateUTF8(text, 2))
	assert.Equal(t, "a界", truncateUTF8(text, 4))
	assert.Equal(t, "a界b", truncateUTF8(text, 10))
}

func TestServiceRerankNoScoresFallsBackAndCoolsDown(t *testing.T) {
	feeds := []Passage{
		{FeedID: 1, Title: "A", Score: 0.9},
		{FeedID: 2, Title: "B", Score: 0.8},
	}
	service, _, _ := newCorpusService(t, feeds, nil)
	reranker := &stubReranker{scores: []float64{math.NaN(), math.NaN()}}
	service.SetReranker(func(context.Context, int) (Reranker, error) { return reranker, nil }, 20, 4000)

	for i := 0; i < rerankFailureThreshold; i++ {
		result, err := service.RetrieveCorpus(context.Background(), 42, "query", RetrievalOptions{Corpus: CorpusFeeds})
		require.NoError(t, err)
		require.Len(t, result.Passages, 2)
		assert.Equal(t, 1, result.Passages[0].Feed.FeedID)
	}
	require.Equal(t, rerankFailureThreshold, reranker.callCount())

	_, err := service.RetrieveCorpus(context.Background(), 42, "query", RetrievalOptions{Corpus: CorpusFeeds})
	require.NoError(t, err)
	assert.Equal(t, rerankFailureThreshold, reranker.callCount(), "zero-score responses open the cooldown")
}

func TestServiceUserStackResolverResolvesOnce(t *testing.T) {
	feeds := []Passage{
		{FeedID: 1, Title: "A", Score: 0.9},
		{FeedID: 2, Title: "B", Score: 0.8},
	}
	service, _, _ := newCorpusService(t, feeds, nil)
	reranker := &stubReranker{scores: []float64{0.1, 0.9}}
	calls := 0
	service.SetUserStackResolver(func(context.Context, int) (*Embedder, Reranker, error) {
		calls++
		return service.embedder, reranker, nil
	})
	service.SetReranker(nil, 20, 4000)

	result, err := service.RetrieveCorpus(context.Background(), 42, "query", RetrievalOptions{Corpus: CorpusFeeds})
	require.NoError(t, err)
	require.Len(t, result.Passages, 2)
	assert.Equal(t, 1, calls, "one settings resolution serves the embedder and the reranker")
	assert.Equal(t, 2, result.Passages[0].Feed.FeedID)
	assert.Equal(t, 1, reranker.callCount())
}

func TestServiceWithoutRerankerIsUnchanged(t *testing.T) {
	feeds := []Passage{{FeedID: 1, Title: "A", Score: 0.9}, {FeedID: 2, Title: "B", Score: 0.8}}
	service, retriever, _ := newCorpusService(t, feeds, nil)

	result, err := service.RetrieveCorpus(context.Background(), 42, "query", RetrievalOptions{Corpus: CorpusFeeds})
	require.NoError(t, err)
	require.Len(t, result.Passages, 2)
	assert.Equal(t, 1, result.Passages[0].Feed.FeedID)
	assert.Equal(t, 5, retriever.lastK, "no reranker means no over-fetch")
}
