package rag

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/pgvector/pgvector-go"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	ragdb "nyx/internal/rag/db"
)

// Passage is one retrieved feed + its relevance score. The score
// is the cosine similarity in [0, 1] (1 - cosine distance), so
// higher is better. Empty Description is represented as "".
type Passage struct {
	FeedID      int
	Title       string
	Description string
	Score       float64
}

// Render formats the passage into the prompt-fragment shape the
// rag.Service injects. The index is 1-based so the prompt reads
// "[Feed 1] Title: ..." rather than "[Feed 0]". Empty Description
// renders just the Title line — the chat service's context block
// stays clean for the common "title-only" feed.
func (p Passage) Render(index int) string {
	if p.Description == "" {
		return fmt.Sprintf("[Feed %d] Title: %s", index, p.Title)
	}
	return fmt.Sprintf("[Feed %d] Title: %s\nDescription: %s", index, p.Title, p.Description)
}

// Retriever runs the top-K semantic lookup for the authenticated
// user. It does NOT embed the query (the rag.Service owns the
// embed-then-retrieve orchestration so the embedding can be reused
// if the chat path ever grows multi-hop retrieval).
type Retriever struct {
	q      *ragdb.Queries
	tracer trace.Tracer
}

// NewRetriever wires a Retriever against the supplied pool. The
// pool is the same *pgxpool.Pool the feed domain uses; pgvector's
// vector codec is already registered on every connection.
func NewRetriever(pool Pool, tracer trace.Tracer) *Retriever {
	return &Retriever{q: ragdb.New(pool), tracer: tracer}
}

// Retrieve runs the user-scoped top-K query. The queryVec MUST
// be in the same latent space as the indexed vectors — calling
// Retrieve with a vector from a different embedding model returns
// garbage rankings. The caller (rag.Service) is responsible for
// using the same embedder that was used at index time.
//
// Empty results are NOT an error — a user with no indexed feeds
// simply gets an empty slice and the chat service skips context
// injection. k MUST be positive; a zero or negative k collapses
// to no rows.
func (r *Retriever) Retrieve(ctx context.Context, userID int, queryVec []float32, k int) ([]Passage, error) {
	ctx, span := r.tracer.Start(ctx, "rag.retrieve",
		trace.WithAttributes(
			attribute.Int("rag.user_id", userID),
			attribute.Int("rag.k", k),
			attribute.Int("rag.query_dim", len(queryVec)),
		),
	)
	defer span.End()

	if k <= 0 || len(queryVec) == 0 {
		return nil, nil
	}

	rows, err := r.q.RetrieveFeedPassages(ctx, ragdb.RetrieveFeedPassagesParams{
		Embedding: pgvector.NewVector(queryVec),
		UserID:    int64(userID),
		Limit:     int32(k),
	})
	if err != nil {
		span.RecordError(err)
		return nil, err
	}

	passages := make([]Passage, len(rows))
	for i, row := range rows {
		passages[i] = Passage{
			FeedID:      int(row.FeedID),
			Title:       row.Title,
			Description: row.Description.String,
			Score:       row.Score,
		}
	}
	span.SetAttributes(attribute.Int("rag.hits", len(passages)))
	span.SetStatus(codes.Ok, "")
	return passages, nil
}

// ErrNoEmbedder is returned by rag.Service.Retrieve when the
// caller forgot to wire the embedder (a programming error at the
// wiring site, not a runtime condition).
var ErrNoEmbedder = errors.New("rag: embedder not configured")

// Compile-time guard: the indexer calls pgx.ErrNoRows via errors.Is,
// so the pgx package must stay imported even if every other call
// site in this file is removed. Cheap insurance against a future
// refactor deleting the check.
var _ = pgx.ErrNoRows