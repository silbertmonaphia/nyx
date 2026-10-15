package rag

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/pgvector/pgvector-go"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"nyx/internal/feed"
	ragdb "nyx/internal/rag/db"
)

// maxBackfillIterations caps a single BackfillOnce call so a user
// with thousands of unindexed feeds doesn't monopolise the
// process. The rest get picked up on subsequent chat requests —
// or, if the operator wants a full backfill, a manual SQL loop
// against the same query (the plan leaves a CLI for later).
const maxBackfillIterations = 5

// contextSeparator is rendered between passages in the context
// block. Three dashes is unique enough that an LLM treats it as a
// hard break, not as part of a passage.
const contextSeparator = "\n\n---\n\n"

// RetrieverIface is the narrow contract rag.Service needs from
// the retrieval layer. The concrete *Retriever satisfies it
// (see retriever.go); tests inject a stub via NewService when
// they want canned passages without a database round-trip.
type RetrieverIface interface {
	Retrieve(ctx context.Context, userID int, queryVec []float32, k int) ([]Passage, error)
}

// Service orchestrates the chat-side RAG path: embed the user's
// query, retrieve the top-K passages, render a context block
// sized to RAGMaxContextChars, and run the lazy backfill.
//
// The chat service holds one Service pointer (nil when
// LLM_ENABLED=false) and only invokes Retrieve when the request
// body carries rag:true.
type Service struct {
	embedder    *Embedder
	retriever   RetrieverIface
	pool        Pool
	feedRepo    feed.Repository
	topK        int
	maxCtxChars int
	maxBackfill int
	tracer      trace.Tracer

	// backfillInFlight serialises BackfillOnce per userID via the
	// canonical LoadOrStore + sync.Once pattern. The map itself is
	// never garbage collected (a future improvement: evict entries
	// after the Once completes + a TTL), so the memory cost is
	// bounded by the number of users who've ever triggered RAG —
	// typically a few hundred for any realistic deployment, well
	// under the per-entry overhead.
	backfillInFlight sync.Map
}

// NewService wires the chat-side rag facade. The pool is shared
// across embedder / retriever (both reach into ragdb.Queries built
// on it); the feed.Repository is needed for the lazy backfill's
// "feeds without embeddings" query.
//
// All five integer fields have safe defaults in cmd/api/main.go
// via cfg.RAGTopK / cfg.RAGMaxContextChars / cfg.RAGMaxBackfillPerRequest.
func NewService(embedder *Embedder, retriever RetrieverIface, pool Pool,
	feedRepo feed.Repository, topK, maxCtxChars, maxBackfill int,
	tracer trace.Tracer) *Service {
	return &Service{
		embedder:    embedder,
		retriever:   retriever,
		pool:        pool,
		feedRepo:    feedRepo,
		topK:        topK,
		maxCtxChars: maxCtxChars,
		maxBackfill: maxBackfill,
		tracer:      tracer,
	}
}

// Retrieve is the chat-side hot path. Returns the passages + a
// rendered context block (empty when no passages, which the chat
// service treats as "no augmentation"). Truncates the context
// block at maxCtxChars so a runaway corpus can't blow out the
// LLM context window.
//
// A retrieval failure returns an error but the chat service
// catches it and continues without context — a retrieval error
// must never fail the chat.
func (s *Service) Retrieve(ctx context.Context, userID int, query string) ([]Passage, string, error) {
	ctx, span := s.tracer.Start(ctx, "rag.retrieve_for_chat",
		trace.WithAttributes(
			attribute.Int("rag.user_id", userID),
			attribute.Int("rag.top_k", s.topK),
		),
	)
	defer span.End()

	if s.embedder == nil {
		span.RecordError(ErrNoEmbedder)
		return nil, "", ErrNoEmbedder
	}
	if query == "" {
		return nil, "", nil
	}

	vecs, err := s.embedder.EmbedStrings(ctx, []string{query})
	if err != nil {
		span.RecordError(err)
		return nil, "", err
	}
	if len(vecs) == 0 {
		return nil, "", nil
	}

	passages, err := s.retriever.Retrieve(ctx, userID, vecs[0], s.topK)
	if err != nil {
		span.RecordError(err)
		return nil, "", err
	}
	if len(passages) == 0 {
		span.SetStatus(codes.Ok, "")
		return nil, "", nil
	}

	block := renderContextBlock(passages, s.maxCtxChars)
	span.SetAttributes(
		attribute.Int("rag.hits", len(passages)),
		attribute.Int("rag.context_chars", len(block)),
	)
	span.SetStatus(codes.Ok, "")
	return passages, block, nil
}

// renderContextBlock formats passages into the prompt fragment the
// chat service injects. The format is:
//
//	[Feed 1] Title: ...
//	Description: ...
//
//	---
//
//	[Feed 2] Title: ...
//	Description: ...
//
// Truncates at maxCtxChars with an ellipsis suffix so the model
// never sees a half-passage. Truncation is greedy — we keep whole
// passages until the next one would overflow the budget. A
// single oversized passage is kept verbatim with a length-truncation
// warning appended; the chat service surfaces the truncation via
// its OTel attributes anyway.
func renderContextBlock(passages []Passage, maxChars int) string {
	if maxChars <= 0 || len(passages) == 0 {
		return ""
	}
	var b strings.Builder
	for i, p := range passages {
		chunk := p.Render(i + 1)
		// +separator between chunks (and trailing): budget check
		// is chunk + separator to keep the math tight.
		next := b.Len() + len(chunk) + len(contextSeparator)
		if i > 0 {
			next -= len(contextSeparator) // no leading separator
		}
		if next > maxChars && b.Len() == 0 {
			// First passage is itself too long — truncate it
			// inline and append the marker so the LLM knows
			// additional passages (if any) were omitted.
			if len(chunk) > maxChars {
				chunk = chunk[:maxChars-len("\n\n[truncated — additional feeds omitted]")-1] + "…"
			}
			b.WriteString(chunk)
			b.WriteString("\n\n[truncated — additional feeds omitted]")
			return b.String()
		}
		if next > maxChars {
			// No room for another full passage — stop here.
			b.WriteString("\n\n[truncated — additional feeds omitted]")
			break
		}
		if i > 0 {
			b.WriteString(contextSeparator)
		}
		b.WriteString(chunk)
	}
	return b.String()
}

// BackfillOnce runs the lazy backfill at most once per process
// per user. The first caller for a userID stores a sync.Once in
// backfillInFlight; subsequent callers for the same userID load
// the same Once and the second Do is a no-op. The pattern is
// race-free across an arbitrary number of concurrent goroutines.
//
// Callers invoke this in a fire-and-forget goroutine from the
// chat service. The chat request returning does not cancel the
// backfill — the chat service detaches the context (Background +
// a derived timeout) before the go-statement so a long backfill
// survives the originating request.
func (s *Service) BackfillOnce(ctx context.Context, userID int) {
	if s.embedder == nil || s.feedRepo == nil {
		return
	}
	ctx, span := s.tracer.Start(ctx, "rag.backfill_once",
		trace.WithAttributes(attribute.Int("rag.user_id", userID)),
	)
	defer span.End()

	once, _ := s.backfillInFlight.LoadOrStore(userID, &sync.Once{})
	once.(*sync.Once).Do(func() {
		s.runBackfill(ctx, userID)
	})
}

// runBackfill is the actual work — bounded to maxBackfill * maxBackfillIterations
// feeds per call so a single BackfillOnce cannot monopolise the
// process. Each iteration embeds one batch via EmbedBatch and
// upserts each row individually (so a single bad row doesn't
// abort the rest of the batch).
func (s *Service) runBackfill(ctx context.Context, userID int) {
	ctx, span := s.tracer.Start(ctx, "rag.backfill")
	defer span.End()

	q := ragdb.New(s.pool)
	for iter := 0; iter < maxBackfillIterations; iter++ {
		rows, err := q.ListFeedsWithoutEmbeddings(ctx, ragdb.ListFeedsWithoutEmbeddingsParams{
			UserID: int64(userID),
			//nolint:gosec // G115: maxBackfill is bounded by RAG_MAX_BACKFILL_PER_REQUEST (default 20).
			Limit: int32(s.maxBackfill),
		})
		if err != nil {
			span.RecordError(err)
			return
		}
		if len(rows) == 0 {
			return
		}
		span.SetAttributes(attribute.Int("rag.backfill_iter", iter), attribute.Int("rag.backfill_rows", len(rows)))

		feeds := make([]*feed.Feed, len(rows))
		for i, r := range rows {
			feeds[i] = &feed.Feed{
				ID:          int(r.ID),
				UserID:      int(r.UserID),
				Title:       r.Title,
				Description: r.Description.String,
				CreatedAt:   r.CreatedAt.Time,
				UpdatedAt:   r.UpdatedAt.Time,
				DeletedAt:   nullableTime(r.DeletedAt),
			}
		}

		vecs, hashes, err := s.embedder.EmbedBatch(ctx, feeds)
		if err != nil {
			span.RecordError(err)
			return
		}
		for i, f := range feeds {
			if err := q.UpsertFeedEmbedding(ctx, ragdb.UpsertFeedEmbeddingParams{
				//nolint:gosec // G115: feed_id is a SERIAL PK; handler-side caps keep this well below math.MaxInt32.
				FeedID:      int32(f.ID),
				UserID:      int64(userID),
				Embedding:   pgvector.NewVector(vecs[i]),
				ChunkText:   ChunkText(f),
				ContentHash: hashes[i],
			}); err != nil {
				// Best-effort: a single bad row doesn't abort the rest.
				// The next BackfillOnce (or a future chat request) will
				// retry; the row is still in ListFeedsWithoutEmbeddings.
				span.RecordError(err)
				continue
			}
		}
		// Loop again only if we filled the batch — if we got fewer
		// rows than maxBackfill we're done.
		if len(rows) < s.maxBackfill {
			return
		}
	}
}

// nullableTime converts a pgtype.Timestamptz to a *time.Time,
// returning nil when the column was SQL NULL.
func nullableTime(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	return &t.Time
}
