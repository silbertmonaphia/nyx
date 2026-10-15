package rag

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/pgvector/pgvector-go"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"nyx/internal/feed"
	ragdb "nyx/internal/rag/db"
)

// Indexer writes embeddings into feed_embeddings. It is wired
// into the feed service's Create / Update / Delete paths as an
// EmbeddingIndexer (declared in the feed package); the interface
// inversion keeps feed from importing rag.
//
// Every method is best-effort at the caller (feed service logs a
// warn on error and continues). Indexer itself surfaces errors so
// the operator sees them in logs; the contract is "never fail
// the user's write because embedding failed".
type Indexer struct {
	q        *ragdb.Queries
	embedder *Embedder
	tracer   trace.Tracer
}

// NewIndexer builds an Indexer that issues SQL through the supplied
// pool. The pool is the same *pgxpool.Pool the feed domain uses;
// pgvector's vector codecs are already registered on every
// connection by the database package's AfterConnect hook.
func NewIndexer(pool Pool, e *Embedder, tracer trace.Tracer) *Indexer {
	return &Indexer{q: ragdb.New(pool), embedder: e, tracer: tracer}
}

// Index upserts a feed's embedding. Skips the Embed API call
// entirely when the new content_hash matches the stored hash — a
// no-op Update (debounced auto-save, identical re-save from the
// UI, etc.) costs one sha256 and zero API calls.
//
// Returns the embedding error so the caller can log it. The
// caller MUST NOT propagate the error as a write failure.
func (i *Indexer) Index(ctx context.Context, userID int, f *feed.Feed) error {
	ctx, span := i.tracer.Start(ctx, "rag.index",
		trace.WithAttributes(
			attribute.Int("rag.feed_id", f.ID),
			attribute.Int("rag.user_id", userID),
		),
	)
	defer span.End()

	newHash := HashChunk(f)

	// Pre-flight SELECT: an unchanged-hash row short-circuits before
	// we pay for the embedding API call. pgx's QueryRow().Scan
	// returns pgx.ErrNoRows when no row exists, which is the
	// common case on first Create — that's NOT an error.
	//
	//nolint:gosec // G115: feed_id is a SERIAL PK; handler-side caps keep this well below math.MaxInt32.
	existingHash, err := i.q.GetFeedEmbeddingHash(ctx, int32(f.ID))
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		span.RecordError(err)
		return err
	}
	if err == nil && bytesEqual(existingHash, newHash) {
		span.SetAttributes(attribute.Bool("rag.skipped_noop", true))
		span.SetStatus(codes.Ok, "")
		return nil
	}

	vec, hash, err := i.embedder.Embed(ctx, f)
	if err != nil {
		span.RecordError(err)
		return err
	}

	if err := i.q.UpsertFeedEmbedding(ctx, ragdb.UpsertFeedEmbeddingParams{
		//nolint:gosec // G115: feed_id is a SERIAL PK; handler-side caps keep this well below math.MaxInt32.
		FeedID:      int32(f.ID),
		UserID:      int64(userID),
		Embedding:   pgvector.NewVector(vec),
		ChunkText:   ChunkText(f),
		ContentHash: hash,
	}); err != nil {
		span.RecordError(err)
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

// Delete removes the embedding row for a feed. Called from
// feed.Service.DeleteFeed so a soft-deleted feed never surfaces
// in retrieval results. The retriever's JOIN also filters
// deleted_at IS NULL, so a leftover embedding row is harmless
// even if this fails — but the cost of one DELETE per feed
// delete is negligible and keeps the table tidy.
func (i *Indexer) Delete(ctx context.Context, feedID int) error {
	ctx, span := i.tracer.Start(ctx, "rag.delete",
		trace.WithAttributes(attribute.Int("rag.feed_id", feedID)),
	)
	defer span.End()

	//nolint:gosec // G115: feed_id is a SERIAL PK; handler-side caps keep this well below math.MaxInt32.
	if err := i.q.DeleteFeedEmbedding(ctx, int32(feedID)); err != nil {
		span.RecordError(err)
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

// bytesEqual is a tiny helper that avoids importing bytes just
// for one Equal call. Constant-time semantics are unnecessary —
// content_hash is not a secret.
func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
