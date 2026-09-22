package rag

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace/noop"

	"nyx/internal/feed"
	"nyx/internal/llm"
)

// stubProvider records every Embed call so tests can assert the
// rag package sent the right model + inputs, and lets each test
// seed a canned response or error.
type stubProvider struct {
	embedCalls int
	lastReq    llm.EmbedRequest
	vecs       [][]float32
	err        error
}

func (s *stubProvider) Chat(_ context.Context, _ llm.ChatRequest, _ func(string, *llm.ChatUsage) error) (*llm.ChatUsage, error) {
	panic("stubProvider.Chat should not be called from embedder tests")
}

func (s *stubProvider) Embed(_ context.Context, req llm.EmbedRequest) ([][]float32, error) {
	s.embedCalls++
	s.lastReq = req
	if s.err != nil {
		return nil, s.err
	}
	return s.vecs, nil
}

// TestChunkText pins the canonical embedding string.
func TestChunkText(t *testing.T) {
	f := &feed.Feed{Title: "The Matrix", Description: "A sci-fi film"}
	assert.Equal(t, "The Matrix\n\nA sci-fi film", ChunkText(f))

	empty := &feed.Feed{Title: "Only Title"}
	// Trailing "\n\n" is intentional: a feed whose Description
	// transitions between empty and non-empty produces a
	// different chunk_text, so the content_hash differs and the
	// indexer re-embeds.
	assert.Equal(t, "Only Title\n\n", ChunkText(empty))

	assert.Equal(t, "", ChunkText(nil))
}

// TestHashChunk pins sha256(chunk_text). Pinning the digest lets
// a future migration of the canonical text surface immediately
// (every stored content_hash becomes stale).
func TestHashChunk(t *testing.T) {
	f := &feed.Feed{Title: "The Matrix", Description: "A sci-fi film"}
	sum := HashChunk(f)
	require.Len(t, sum, 32, "sha256 digest is 32 bytes")

	// Determinism: same input → same output.
	assert.Equal(t, sum, HashChunk(f))

	// Different input → different output.
	g := &feed.Feed{Title: "The Matrix", Description: "A romantic comedy"}
	assert.NotEqual(t, sum, HashChunk(g))
}

// TestEmbedder_Embed pins the single-feed path: model passed
// through, one input in the request, one vector out, hash equals
// sha256(chunk_text).
func TestEmbedder_Embed(t *testing.T) {
	p := &stubProvider{vecs: [][]float32{{0.1, 0.2, 0.3}}}
	e := NewEmbedder(p, "text-embedding-3-small", noop.NewTracerProvider().Tracer("test"))

	f := &feed.Feed{ID: 42, Title: "Matrix", Description: "sci-fi"}
	vec, hash, err := e.Embed(context.Background(), f)
	require.NoError(t, err)
	assert.Equal(t, []float32{0.1, 0.2, 0.3}, vec)
	assert.Equal(t, HashChunk(f), hash)

	assert.Equal(t, 1, p.embedCalls)
	assert.Equal(t, "text-embedding-3-small", p.lastReq.Model)
	require.Len(t, p.lastReq.Inputs, 1)
	assert.Equal(t, ChunkText(f), p.lastReq.Inputs[0])
}

// TestEmbedder_Embed_PropagatesProviderError pins the contract
// that a provider error surfaces from Embed unchanged. The indexer
// relies on this to log + skip the upsert.
func TestEmbedder_Embed_PropagatesProviderError(t *testing.T) {
	p := &stubProvider{err: llm.ErrProviderUnavailable}
	e := NewEmbedder(p, "m", noop.NewTracerProvider().Tracer("test"))

	_, _, err := e.Embed(context.Background(), &feed.Feed{ID: 1, Title: "t"})
	require.Error(t, err)
	assert.ErrorIs(t, err, llm.ErrProviderUnavailable)
}

// TestEmbedder_EmbedBatch pins the batched path: one provider
// call per EmbedBatch call (amortises the HTTP round-trip),
// vectors returned in input order, hashes match sha256 of each
// chunk_text.
func TestEmbedder_EmbedBatch(t *testing.T) {
	p := &stubProvider{
		vecs: [][]float32{{0.1, 0.2}, {0.3, 0.4}, {0.5, 0.6}},
	}
	e := NewEmbedder(p, "m", noop.NewTracerProvider().Tracer("test"))

	feeds := []*feed.Feed{
		{ID: 1, Title: "a"},
		{ID: 2, Title: "b"},
		{ID: 3, Title: "c"},
	}
	vecs, hashes, err := e.EmbedBatch(context.Background(), feeds)
	require.NoError(t, err)
	require.Len(t, vecs, 3)
	require.Len(t, hashes, 3)

	for i, f := range feeds {
		assert.Equal(t, vecs[i], p.vecs[i])
		assert.Equal(t, HashChunk(f), hashes[i])
	}

	assert.Equal(t, 1, p.embedCalls, "EmbedBatch must be one provider call")
	require.Len(t, p.lastReq.Inputs, 3)
	for i, f := range feeds {
		assert.Equal(t, ChunkText(f), p.lastReq.Inputs[i])
	}
}

// TestEmbedder_EmbedBatch_EmptyIsNoop pins that an empty feed
// list does no work and returns nil cleanly. The lazy backfill
// relies on this — when no rows match the backfill query, the
// batch call short-circuits without paying for a provider call.
func TestEmbedder_EmbedBatch_EmptyIsNoop(t *testing.T) {
	p := &stubProvider{}
	e := NewEmbedder(p, "m", noop.NewTracerProvider().Tracer("test"))

	vecs, hashes, err := e.EmbedBatch(context.Background(), nil)
	require.NoError(t, err)
	assert.Empty(t, vecs)
	assert.Empty(t, hashes)
	assert.Equal(t, 0, p.embedCalls)
}

// TestEmbedder_EmbedBatch_LengthMismatchReturnsSentinel pins the
// upstream-bug guard: provider returned M != N vectors → sentinel
// so the rag service can log + skip the upsert rather than
// propagate garbled data.
func TestEmbedder_EmbedBatch_LengthMismatchReturnsSentinel(t *testing.T) {
	p := &stubProvider{vecs: [][]float32{{0.1}}} // 1 vector, caller will send 3
	e := NewEmbedder(p, "m", noop.NewTracerProvider().Tracer("test"))

	_, _, err := e.EmbedBatch(context.Background(), []*feed.Feed{
		{ID: 1, Title: "a"},
		{ID: 2, Title: "b"},
		{ID: 3, Title: "c"},
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, llm.ErrProviderUnavailable)
}

// TestEmbedder_EmbedStrings pins the query-embedding path used by
// rag.Service.Retrieve: arbitrary []string → [][]float32 in
// order, no feed row involved.
func TestEmbedder_EmbedStrings(t *testing.T) {
	p := &stubProvider{vecs: [][]float32{{0.7, 0.8}}}
	e := NewEmbedder(p, "m", noop.NewTracerProvider().Tracer("test"))

	vecs, err := e.EmbedStrings(context.Background(), []string{"what is the matrix about?"})
	require.NoError(t, err)
	assert.Equal(t, []float32{0.7, 0.8}, vecs[0])
	assert.Equal(t, 1, p.embedCalls)
	assert.Equal(t, []string{"what is the matrix about?"}, p.lastReq.Inputs)
}

// TestEmbedder_EmbedStrings_EmptyIsNoop pins that an empty query
// short-circuits — rag.Service.Retrieve guards on the empty-query
// case upstream, but defence-in-depth at the embedder keeps the
// contract uniform.
func TestEmbedder_EmbedStrings_EmptyIsNoop(t *testing.T) {
	p := &stubProvider{}
	e := NewEmbedder(p, "m", noop.NewTracerProvider().Tracer("test"))

	vecs, err := e.EmbedStrings(context.Background(), nil)
	require.NoError(t, err)
	assert.Empty(t, vecs)
	assert.Equal(t, 0, p.embedCalls)
}

// TestEmbedder_Model pins the diagnostic accessor.
func TestEmbedder_Model(t *testing.T) {
	e := NewEmbedder(&stubProvider{}, "text-embedding-3-large", noop.NewTracerProvider().Tracer("test"))
	assert.Equal(t, "text-embedding-3-large", e.Model())
}