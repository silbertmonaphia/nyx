package rag

import (
	"context"
	"crypto/sha256"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"nyx/internal/feed"
	"nyx/internal/llm"
	ragdb "nyx/internal/rag/db"
)

// Pool is the subset of *pgxpool.Pool the rag package needs.
// Both pgxpool.Pool and pgxmock.PgxPoolIface satisfy it (the
// latter is the unit-test path; see retriever_test.go). The
// interface embeds ragdb.DBTX so any concrete pool becomes a
// valid argument to ragdb.New — no adapter needed.
type Pool interface {
	ragdb.DBTX
}

// ChunkText is the canonical string the rag package embeds for a
// feed. Title + "\n\n" + Description — the leading newline + pair
// of separators gives the embedding model a clean visual break
// between the two fields so "Matrix" + "a sci-fi film" doesn't
// tokenise the same as "Matrix a sci-fi film" with no separation.
// Description is empty when the feed has none; the resulting
// chunk_text is just Title in that case.
func ChunkText(f *feed.Feed) string {
	if f == nil {
		return ""
	}
	return f.Title + "\n\n" + f.Description
}

// HashChunk returns the sha256 of ChunkText(f). The Indexer
// compares this against the stored content_hash to skip the Embed
// API call on no-op updates.
func HashChunk(f *feed.Feed) []byte {
	if f == nil {
		return nil
	}
	sum := sha256.Sum256([]byte(ChunkText(f)))
	return sum[:]
}

// Embedder wraps llm.Provider.Embed with the rag-package
// conventions: chunk text + content hash on every call. The
// constructor accepts the resolved embedding model (NOT the chat
// model) — cmd/api/main.go is responsible for resolving
// cfg.LLMEmbeddingModel (with cfg.LLMModel as the fallback) at
// wiring time and passing the resolved string here.
type Embedder struct {
	provider llm.Provider
	model    string
	tracer   trace.Tracer
}

// NewEmbedder wires an Embedder against the supplied provider and
// resolved model. model MUST be non-empty — the rag package never
// falls back to a default, so an empty model is a programming
// error at the wiring site.
func NewEmbedder(p llm.Provider, model string, tracer trace.Tracer) *Embedder {
	return &Embedder{provider: p, model: model, tracer: tracer}
}

// Embed embeds one feed and returns the vector + chunk hash. The
// rag.Service.Retrieve hot path never calls this directly; it's
// used by the Indexer for single-feed writes (Create/Update) and
// by the lazy-backfill path's per-feed retry. EmbedBatch is the
// preferred entry point when more than one feed needs embedding.
func (e *Embedder) Embed(ctx context.Context, f *feed.Feed) ([]float32, []byte, error) {
	ctx, span := e.tracer.Start(ctx, "rag.embed",
		trace.WithAttributes(
			attribute.String("rag.model", e.model),
			attribute.Int("rag.feed_id", f.ID),
		),
	)
	defer span.End()

	vecs, err := e.provider.Embed(ctx, llm.EmbedRequest{
		Model:  e.model,
		Inputs: []string{ChunkText(f)},
	})
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "embed failed")
		return nil, nil, err
	}
	if len(vecs) == 0 {
		// Should never happen — provider returns len(req.Inputs)
		// vectors — but defensive: surface as a clear error rather
		// than panic on out-of-range indexing.
		err := llm.ErrProviderUnavailable
		span.RecordError(err)
		return nil, nil, err
	}
	span.SetStatus(codes.Ok, "")
	return vecs[0], HashChunk(f), nil
}

// EmbedBatch amortises one HTTP round-trip across N feeds. Returns
// len(feeds) vectors in the same order, plus the matching content
// hashes. Used by the lazy-backfill loop so a 20-feed backfill
// pays one API call instead of 20.
func (e *Embedder) EmbedBatch(ctx context.Context, feeds []*feed.Feed) ([][]float32, [][]byte, error) {
	ctx, span := e.tracer.Start(ctx, "rag.embed_batch",
		trace.WithAttributes(
			attribute.String("rag.model", e.model),
			attribute.Int("rag.batch_size", len(feeds)),
		),
	)
	defer span.End()

	if len(feeds) == 0 {
		return nil, nil, nil
	}

	inputs := make([]string, len(feeds))
	hashes := make([][]byte, len(feeds))
	for i, f := range feeds {
		inputs[i] = ChunkText(f)
		hashes[i] = HashChunk(f)
	}

	vecs, err := e.provider.Embed(ctx, llm.EmbedRequest{
		Model:  e.model,
		Inputs: inputs,
	})
	if err != nil {
		span.RecordError(err)
		return nil, nil, err
	}
	if len(vecs) != len(feeds) {
		// Provider contract: one vector per input, in input order.
		// A length mismatch is an upstream bug or wire error.
		err := llm.ErrProviderUnavailable
		span.RecordError(err)
		return nil, nil, err
	}
	span.SetStatus(codes.Ok, "")
	return vecs, hashes, nil
}

// Model returns the resolved embedding model name. Useful for
// diagnostic logging at startup.
func (e *Embedder) Model() string { return e.model }

// EmbedStrings batches a raw string-slice through the provider.
// Used by rag.Service.Retrieve to embed the user's query (which
// has no associated feed row, so the Embed / EmbedBatch methods
// don't fit). Returns len(inputs) vectors in input order; an
// empty input slice returns (nil, nil).
func (e *Embedder) EmbedStrings(ctx context.Context, inputs []string) ([][]float32, error) {
	ctx, span := e.tracer.Start(ctx, "rag.embed_strings",
		trace.WithAttributes(
			attribute.String("rag.model", e.model),
			attribute.Int("rag.inputs", len(inputs)),
		),
	)
	defer span.End()
	if len(inputs) == 0 {
		return nil, nil
	}
	vecs, err := e.provider.Embed(ctx, llm.EmbedRequest{
		Model:  e.model,
		Inputs: inputs,
	})
	if err != nil {
		span.RecordError(err)
		return nil, err
	}
	span.SetStatus(codes.Ok, "")
	return vecs, nil
}
