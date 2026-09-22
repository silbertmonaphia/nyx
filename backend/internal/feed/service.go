package feed

import (
	"context"
	"fmt"
	"time"

	"nyx/internal/platform/cache"
	"nyx/internal/reqctx"

	"github.com/rs/zerolog/log"
	"go.opentelemetry.io/otel/trace"
)

type Service interface {
	GetFeeds(ctx context.Context, query string, page, pageSize int, order SortOrder) (*Page, error)
	CreateFeed(ctx context.Context, m *Feed) error
	UpdateFeed(ctx context.Context, id int, m *Feed) error
	DeleteFeed(ctx context.Context, id int) error
	CheckHealth(ctx context.Context) error
	CheckCacheHealth(ctx context.Context) error
	SetIndexer(idx EmbeddingIndexer)
}

// EmbeddingIndexer is the narrow contract the feed service needs
// from the rag package. rag.Indexer satisfies it structurally
// (Go's implicit interface satisfaction), so feed stays unaware
// of the rag package while the rag package stays unaware of the
// feed service's cache + invalidation logic. The dependency is
// one-way: rag → feed.Feed, never feed → rag.
//
// All methods are best-effort: the feed service calls them after
// a successful write, logs a warn on error, and never propagates
// the error as a write failure (a flaky embedding provider must
// not block the user's CRUD).
type EmbeddingIndexer interface {
	Index(ctx context.Context, userID int, m *Feed) error
	Delete(ctx context.Context, feedID int) error
}

// NoopEmbeddingIndexer is the no-op implementation for deployments
// without an LLM (which can't embed). Constructed by main.go
// when cfg.LLMEnabled is false so feed.NewService never sees a
// nil interface — a nil interface would NPE on every feed write
// instead of degrading gracefully.
type NoopEmbeddingIndexer struct{}

// Index is a no-op.
func (NoopEmbeddingIndexer) Index(context.Context, int, *Feed) error { return nil }

// Delete is a no-op.
func (NoopEmbeddingIndexer) Delete(context.Context, int) error { return nil }

type feedService struct {
	repo    Repository
	cache   cache.Cache
	indexer EmbeddingIndexer
	ttl     time.Duration
	tracer  trace.Tracer
}

// NewService wires the feed domain. The cache parameter may be a no-op
// (cache.NewNoop()) when caching is disabled — the service treats both
// implementations uniformly. indexer may be a NoopEmbeddingIndexer
// when embeddings are not enabled — the service treats both
// implementations uniformly. tracer emits one OTel span per public
// method; pass the noop tracer (otel.Tracer("…") with the global
// noop provider) when tracing is disabled — Start becomes free.
func NewService(repo Repository, c cache.Cache, indexer EmbeddingIndexer, ttl time.Duration, tracer trace.Tracer) Service {
	if indexer == nil {
		indexer = NoopEmbeddingIndexer{}
	}
	return &feedService{repo: repo, cache: c, indexer: indexer, ttl: ttl, tracer: tracer}
}

// SetIndexer swaps the embedding indexer at runtime. The wiring
// site uses this when the LLM/RAG facade is constructed after
// the feed service (cmd/api/main.go) — the feed service starts
// with a NoopEmbeddingIndexer so its constructor doesn't have to
// know about LLM at all; once LLM is enabled the wiring site
// swaps in the real rag.Indexer. Not goroutine-safe by itself —
// callers must call it once during startup before serving
// traffic.
func (s *feedService) SetIndexer(idx EmbeddingIndexer) {
	if idx == nil {
		idx = NoopEmbeddingIndexer{}
	}
	s.indexer = idx
}

// GetFeeds is a cache-aside read scoped to the authenticated user.
// The cache key includes userID so user A's list page can never
// resolve to user B's cached entry, and invalidation only ever
// touches the caller's slice of the namespace. On miss it falls
// through to the repository and best-effort writes the result back
// to the cache; cache errors never fail the request.
//
// Known race: a mutation landing between the DB read and the cache.Set
// here can leave a stale entry in the cache for up to CACHE_TTL. We rely
// on TTL expiry as the eventual safety net rather than synchronising
// the read and write paths.
func (s *feedService) GetFeeds(ctx context.Context, query string, page, pageSize int, order SortOrder) (*Page, error) {
	ctx, span := s.tracer.Start(ctx, "feed.GetFeeds", trace.WithSpanKind(trace.SpanKindInternal))
	defer span.End()

	userID := reqctx.UserIDFromContext(ctx)

	// Defensive guard: if the auth middleware didn't stamp the
	// context (should be impossible on the auth-required GET
	// /api/feeds path, but unit tests bypass it), return an empty
	// page instead of querying. Preserves the no-cross-user-leak
	// invariant — a userID of 0 would otherwise match no rows in
	// production but might still resolve to a stale cache
	// populated by a buggy test or a future regression.
	if userID == 0 {
		return &Page{Items: []Feed{}, Page: page, PageSize: pageSize, Total: 0}, nil
	}

	key := cacheKey(userID, query, page, pageSize, order)

	var cached Page
	if hit, err := s.cache.Get(ctx, key, &cached); err == nil && hit {
		return &cached, nil
	}

	result, err := s.repo.GetAll(ctx, userID, query, page, pageSize, order)
	if err != nil {
		return nil, err
	}

	if err := s.cache.Set(ctx, key, result, s.ttl); err != nil {
		log.Warn().Err(err).Str("key", key).Msg("feed cache write failed")
	}
	return result, nil
}

// CreateFeed persists the feed then invalidates every cached page
// for the calling user. Embedding is best-effort: a failed Index
// logs a warn and never fails the write — the next read will
// re-embed when the user toggles RAG and the lazy backfill fires.
func (s *feedService) CreateFeed(ctx context.Context, m *Feed) error {
	ctx, span := s.tracer.Start(ctx, "feed.CreateFeed", trace.WithSpanKind(trace.SpanKindInternal))
	defer span.End()

	userID := reqctx.UserIDFromContext(ctx)

	if err := s.repo.Create(ctx, userID, m); err != nil {
		return err
	}
	if err := s.indexer.Index(ctx, userID, m); err != nil {
		log.Warn().Err(err).Int("feed_id", m.ID).Msg("feed embedding index failed")
	}
	s.invalidate(ctx, userID)
	return nil
}

// UpdateFeed persists the changes then invalidates every cached page
// for the calling user. Same best-effort Index contract as
// CreateFeed — the Indexer compares the new content_hash against
// the stored hash and skips the Embed API call on a no-op update.
func (s *feedService) UpdateFeed(ctx context.Context, id int, m *Feed) error {
	ctx, span := s.tracer.Start(ctx, "feed.UpdateFeed", trace.WithSpanKind(trace.SpanKindInternal))
	defer span.End()

	userID := reqctx.UserIDFromContext(ctx)

	if err := s.repo.Update(ctx, userID, id, m); err != nil {
		return err
	}
	if err := s.indexer.Index(ctx, userID, m); err != nil {
		log.Warn().Err(err).Int("feed_id", id).Msg("feed embedding index failed")
	}
	s.invalidate(ctx, userID)
	return nil
}

// DeleteFeed soft-deletes the row then invalidates every cached page
// for the calling user. Embedding deletion is best-effort — a
// dead embedding row is harmless because the retriever's JOIN
// filters deleted_at IS NULL anyway.
func (s *feedService) DeleteFeed(ctx context.Context, id int) error {
	ctx, span := s.tracer.Start(ctx, "feed.DeleteFeed", trace.WithSpanKind(trace.SpanKindInternal))
	defer span.End()

	userID := reqctx.UserIDFromContext(ctx)

	if err := s.repo.Delete(ctx, userID, id); err != nil {
		return err
	}
	if err := s.indexer.Delete(ctx, id); err != nil {
		log.Warn().Err(err).Int("feed_id", id).Msg("feed embedding delete failed")
	}
	s.invalidate(ctx, userID)
	return nil
}

func (s *feedService) CheckHealth(ctx context.Context) error {
	ctx, span := s.tracer.Start(ctx, "feed.CheckHealth", trace.WithSpanKind(trace.SpanKindInternal))
	defer span.End()
	return s.repo.Ping(ctx)
}

func (s *feedService) CheckCacheHealth(ctx context.Context) error {
	ctx, span := s.tracer.Start(ctx, "feed.CheckCacheHealth", trace.WithSpanKind(trace.SpanKindInternal))
	defer span.End()
	return s.cache.Ping(ctx)
}

// invalidate removes every cached page belonging to userID. The
// prefix is user-scoped so user A's mutation can never touch user
// B's cache and vice versa. Best-effort: a cache failure here is
// logged but does not fail the write — the next read will refresh
// the entry once its TTL expires.
func (s *feedService) invalidate(ctx context.Context, userID int) {
	prefix := fmt.Sprintf("feeds:u=%d:", userID)
	if err := s.cache.DeletePrefix(ctx, prefix); err != nil {
		log.Warn().Err(err).Str("prefix", prefix).Msg("feed cache invalidation failed")
	}
}

// cacheKey builds the per-user cache key. Including userID in the key
// is what enforces ownership at the cache layer: a read for user 1
// can never resolve to a cached page belonging to user 2 even if the
// rest of the query tuple happens to match.
func cacheKey(userID int, query string, page, pageSize int, order SortOrder) string {
	return fmt.Sprintf("feeds:u=%d:q=%s:p=%d:s=%d:o=%s", userID, query, page, pageSize, order)
}
