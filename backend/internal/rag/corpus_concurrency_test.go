package rag

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace/noop"

	"nyx/internal/kb"
)

type barrierFeedRetriever struct {
	started  chan struct{}
	release  chan struct{}
	passages []Passage
	err      error
}

func (r *barrierFeedRetriever) Retrieve(ctx context.Context, _ int, _ []float32, _ int) ([]Passage, error) {
	close(r.started)
	select {
	case <-r.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return r.passages, r.err
}

type barrierKnowledgeRetriever struct {
	started  chan struct{}
	release  chan struct{}
	passages []kb.Passage
	err      error
}

func (r *barrierKnowledgeRetriever) Retrieve(ctx context.Context, _ int64, _ []float32, _ string, _ int, _ *int64, _ string) ([]kb.Passage, error) {
	close(r.started)
	select {
	case <-r.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return r.passages, r.err
}

func newBothCorpusService(t *testing.T, feed *barrierFeedRetriever, knowledge *barrierKnowledgeRetriever) *Service {
	t.Helper()
	provider := &stubProvider{vecs: [][]float32{{0.1, 0.2}}}
	embedder := NewEmbedder(provider, "stub-model", noop.NewTracerProvider().Tracer("test"))
	service := NewService(embedder, feed, nil, stubFeedRepo{}, 5, 4000, 20, noop.NewTracerProvider().Tracer("test"))
	service.SetKnowledgeRetriever(knowledge, 5, 4000)
	return service
}

func waitForRetrievalStart(t *testing.T, started <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatalf("%s retrieval did not start; the both-corpus pipeline is sequential", label)
	}
}

func TestRetrieveCorpusBothRunsCorporaConcurrently(t *testing.T) {
	feed := &barrierFeedRetriever{
		started:  make(chan struct{}),
		release:  make(chan struct{}),
		passages: []Passage{{FeedID: 1, Title: "F"}},
	}
	knowledge := &barrierKnowledgeRetriever{
		started:  make(chan struct{}),
		release:  make(chan struct{}),
		passages: []kb.Passage{{DocID: 2, SourceName: "s", RelPath: "a.md", Content: "K"}},
	}
	service := newBothCorpusService(t, feed, knowledge)

	done := make(chan struct{})
	var result RetrievalResult
	var err error
	go func() {
		result, err = service.RetrieveCorpus(context.Background(), 1, "query", RetrievalOptions{Corpus: CorpusBoth})
		close(done)
	}()

	waitForRetrievalStart(t, feed.started, "feed")
	waitForRetrievalStart(t, knowledge.started, "knowledge")
	close(feed.release)
	close(knowledge.release)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RetrieveCorpus did not finish after both retrievers were released")
	}
	require.NoError(t, err)
	require.Len(t, result.Passages, 2)
}

func TestRetrieveCorpusBothPropagatesFeedError(t *testing.T) {
	feed := &barrierFeedRetriever{
		started: make(chan struct{}),
		release: make(chan struct{}),
		err:     errors.New("feed down"),
	}
	knowledge := &barrierKnowledgeRetriever{
		started:  make(chan struct{}),
		release:  make(chan struct{}),
		passages: []kb.Passage{{DocID: 2, SourceName: "s", RelPath: "a.md", Content: "K"}},
	}
	service := newBothCorpusService(t, feed, knowledge)

	done := make(chan error, 1)
	go func() {
		_, err := service.RetrieveCorpus(context.Background(), 1, "query", RetrievalOptions{Corpus: CorpusBoth})
		done <- err
	}()

	waitForRetrievalStart(t, feed.started, "feed")
	waitForRetrievalStart(t, knowledge.started, "knowledge")
	close(feed.release)
	close(knowledge.release)

	select {
	case err := <-done:
		require.Error(t, err)
		require.Contains(t, err.Error(), "feed down")
	case <-time.After(5 * time.Second):
		t.Fatal("RetrieveCorpus did not finish after both retrievers were released")
	}
}
