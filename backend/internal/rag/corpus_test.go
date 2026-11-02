package rag

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace/noop"

	"nyx/internal/kb"
)

type stubFeedRetriever struct {
	passages []Passage
	err      error
	calls    int
	lastK    int
}

func (r *stubFeedRetriever) Retrieve(_ context.Context, _ int, _ []float32, k int) ([]Passage, error) {
	r.calls++
	r.lastK = k
	if r.err != nil {
		return nil, r.err
	}
	return r.passages, nil
}

type stubKnowledgeRetriever struct {
	passages   []kb.Passage
	err        error
	calls      int
	lastK      int
	lastModel  string
	lastSource *int64
	lastPath   string
	lastUser   int64
	catchUps   atomic.Int32
}

func (r *stubKnowledgeRetriever) CatchUpOnce(_ context.Context, _ int64) {
	r.catchUps.Add(1)
}

func (r *stubKnowledgeRetriever) Retrieve(_ context.Context, userID int64, _ []float32, model string, k int, sourceID *int64, pathPrefix string) ([]kb.Passage, error) {
	r.calls++
	r.lastK = k
	r.lastModel = model
	r.lastUser = userID
	r.lastSource = sourceID
	r.lastPath = pathPrefix
	if r.err != nil {
		return nil, r.err
	}
	return r.passages, nil
}

func newCorpusService(t *testing.T, feeds []Passage, knowledge []kb.Passage) (*Service, *stubFeedRetriever, *stubKnowledgeRetriever) {
	t.Helper()
	provider := &stubProvider{vecs: [][]float32{{0.1, 0.2, 0.3}}}
	embedder := NewEmbedder(provider, "stub-model", noop.NewTracerProvider().Tracer("test"))
	feedRetriever := &stubFeedRetriever{passages: feeds}
	knowledgeRetriever := &stubKnowledgeRetriever{passages: knowledge}
	service := NewService(embedder, feedRetriever, nil, stubFeedRepo{}, 5, 4000, 20, noop.NewTracerProvider().Tracer("test"))
	service.SetKnowledgeRetriever(knowledgeRetriever, 5, 4000)
	return service, feedRetriever, knowledgeRetriever
}

func TestRetrieveCorpusFeedsIsUnchanged(t *testing.T) {
	feeds := []Passage{
		{FeedID: 1, Title: "A", Description: "desc-a", Score: 0.9},
		{FeedID: 2, Title: "B", Description: "", Score: 0.5},
	}
	service, feedRetriever, knowledgeRetriever := newCorpusService(t, feeds, nil)

	oldPassages, oldBlock, err := service.Retrieve(context.Background(), 42, "query")
	require.NoError(t, err)
	result, err := service.RetrieveCorpus(context.Background(), 42, "query", RetrievalOptions{Corpus: CorpusFeeds})
	require.NoError(t, err)

	assert.Equal(t, oldBlock, result.ContextBlock)
	require.Len(t, result.Passages, len(oldPassages))
	for i, passage := range result.Passages {
		assert.Equal(t, CitationKindFeed, passage.Kind)
		assert.Equal(t, oldPassages[i].FeedID, passage.Feed.FeedID)
		assert.Equal(t, oldPassages[i].Title, passage.Feed.Title)
	}
	assert.Equal(t, 2, feedRetriever.calls)
	assert.Equal(t, 0, knowledgeRetriever.calls)
}

func TestRetrieveCorpusKBOnlyIgnoresFeeds(t *testing.T) {
	knowledge := []kb.Passage{
		{DocID: 7, SourceID: 3, SourceName: "shared", RelPath: "B-Cook/diet.md", HeadingPath: []string{"煮食记"}, Content: "body-a", Score: 0.8},
	}
	service, feedRetriever, knowledgeRetriever := newCorpusService(t, []Passage{{FeedID: 1, Title: "Feed"}}, knowledge)

	result, err := service.RetrieveCorpus(context.Background(), 42, "query", RetrievalOptions{Corpus: CorpusKB})
	require.NoError(t, err)
	require.Len(t, result.Passages, 1)
	assert.Equal(t, CitationKindDoc, result.Passages[0].Kind)
	assert.Equal(t, int64(7), result.Passages[0].Knowledge.DocID)
	assert.Contains(t, result.ContextBlock, "B-Cook/diet.md")
	assert.Contains(t, result.ContextBlock, "body-a")
	assert.NotContains(t, result.ContextBlock, "[Feed")
	assert.Equal(t, 0, feedRetriever.calls)
	assert.Equal(t, "stub-model", knowledgeRetriever.lastModel)
	assert.Equal(t, int64(42), knowledgeRetriever.lastUser)
}

func TestRetrieveCorpusBothFusesByRank(t *testing.T) {
	feeds := []Passage{
		{FeedID: 1, Title: "Low score feed", Score: 0.02},
		{FeedID: 2, Title: "Second feed", Score: 0.01},
	}
	knowledge := []kb.Passage{
		{DocID: 9, SourceName: "shared", RelPath: "a.md", Content: "high score doc", Score: 0.99},
	}
	service, _, _ := newCorpusService(t, feeds, knowledge)

	result, err := service.RetrieveCorpus(context.Background(), 42, "query", RetrievalOptions{Corpus: CorpusBoth})
	require.NoError(t, err)
	require.Len(t, result.Passages, 3)

	assert.Equal(t, CitationKindFeed, result.Passages[0].Kind)
	assert.Equal(t, 1, result.Passages[0].Feed.FeedID)
	assert.Equal(t, CitationKindDoc, result.Passages[1].Kind)
	assert.Equal(t, int64(9), result.Passages[1].Knowledge.DocID)
	assert.Equal(t, CitationKindFeed, result.Passages[2].Kind)
	assert.Equal(t, 2, result.Passages[2].Feed.FeedID)
}

func TestRetrieveCorpusBothWithoutKnowledgeFallsBackToFeeds(t *testing.T) {
	feeds := []Passage{{FeedID: 4, Title: "only feed"}}
	provider := &stubProvider{vecs: [][]float32{{0.1}}}
	embedder := NewEmbedder(provider, "m", noop.NewTracerProvider().Tracer("test"))
	retriever := &stubFeedRetriever{passages: feeds}
	service := NewService(embedder, retriever, nil, stubFeedRepo{}, 5, 4000, 20, noop.NewTracerProvider().Tracer("test"))

	result, err := service.RetrieveCorpus(context.Background(), 1, "query", RetrievalOptions{Corpus: CorpusBoth})
	require.NoError(t, err)
	require.Len(t, result.Passages, 1)
	assert.Equal(t, CitationKindFeed, result.Passages[0].Kind)
}

func TestRetrieveCorpusPassesFiltersAndTopK(t *testing.T) {
	service, _, knowledgeRetriever := newCorpusService(t, nil, []kb.Passage{{DocID: 1}})
	sourceID := int64(12)
	_, err := service.RetrieveCorpus(context.Background(), 42, "query", RetrievalOptions{
		Corpus:     CorpusKB,
		SourceID:   &sourceID,
		PathPrefix: "B-Sport/",
		TopK:       3,
	})
	require.NoError(t, err)
	assert.Equal(t, 3, knowledgeRetriever.lastK)
	require.NotNil(t, knowledgeRetriever.lastSource)
	assert.Equal(t, sourceID, *knowledgeRetriever.lastSource)
	assert.Equal(t, "B-Sport/", knowledgeRetriever.lastPath)
}

func TestRetrieveCorpusRejectsUnknownCorpus(t *testing.T) {
	service, feedRetriever, knowledgeRetriever := newCorpusService(t, nil, nil)
	_, err := service.RetrieveCorpus(context.Background(), 1, "query", RetrievalOptions{Corpus: Corpus("nope")})
	assert.ErrorIs(t, err, ErrInvalidCorpus)
	assert.Equal(t, 0, feedRetriever.calls)
	assert.Equal(t, 0, knowledgeRetriever.calls)
}

func TestRetrieveCorpusEmptyQueryIsEmpty(t *testing.T) {
	service, feedRetriever, knowledgeRetriever := newCorpusService(t, []Passage{{FeedID: 1}}, nil)
	result, err := service.RetrieveCorpus(context.Background(), 1, "  ", RetrievalOptions{Corpus: CorpusBoth})
	require.NoError(t, err)
	assert.Empty(t, result.Passages)
	assert.Empty(t, result.ContextBlock)
	assert.Equal(t, 0, feedRetriever.calls)
	assert.Equal(t, 0, knowledgeRetriever.calls)
}

func TestRetrieveCorpusPropagatesKnowledgeError(t *testing.T) {
	service, _, knowledgeRetriever := newCorpusService(t, nil, nil)
	knowledgeRetriever.err = errors.New("kb down")
	_, err := service.RetrieveCorpus(context.Background(), 1, "query", RetrievalOptions{Corpus: CorpusKB})
	require.Error(t, err)
}

func TestParseCorpus(t *testing.T) {
	for _, raw := range []string{"", "feeds", "FEEDS", " kb ", "both"} {
		_, err := ParseCorpus(raw)
		assert.NoError(t, err, raw)
	}
	_, err := ParseCorpus("documents")
	assert.ErrorIs(t, err, ErrInvalidCorpus)
}

func TestKnowledgeBlockExcludesEmbeddedPrefix(t *testing.T) {
	block := knowledgeBlock(1, kb.Passage{
		SourceName:  "shared",
		RelPath:     "B-Cook/diet.md",
		HeadingPath: []string{"煮食记", "碳水列表"},
		Content:     "一份碳水 + 一份蛋白",
	})
	assert.Contains(t, block, "Source: shared")
	assert.Contains(t, block, "Path: B-Cook/diet.md")
	assert.Contains(t, block, "Section: 煮食记 / 碳水列表")
	assert.True(t, strings.HasSuffix(block, "一份碳水 + 一份蛋白"))
}
