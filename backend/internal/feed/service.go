package feed

import (
	"context"
	"fmt"
	"time"

	"github.com/rs/zerolog/log"
	"go.opentelemetry.io/otel/trace"

	"nyx/internal/platform/cache"
)

type Service interface {
	GetFeeds(ctx context.Context, query string, page, pageSize int) (*Page, error)
	CreateFeed(ctx context.Context, m *Feed) error
	UpdateFeed(ctx context.Context, id int, m *Feed) error
	DeleteFeed(ctx context.Context, id int) error
	CheckHealth(ctx context.Context) error
	CheckCacheHealth(ctx context.Context) error
}

type feedService struct {
	repo   Repository
	cache  cache.Cache
	ttl    time.Duration
	tracer trace.Tracer
}

// NewService wires the feed domain. The cache parameter may be a no-op
// (cache.NewNoop()) when caching is disabled — the service treats both
// implementations uniformly. tracer emits one OTel span per public
// method; pass the noop tracer (otel.Tracer("…") with the global
// noop provider) when tracing is disabled — Start becomes free.
func NewService(repo Repository, c cache.Cache, ttl time.Duration, tracer trace.Tracer) Service {
	return &feedService{repo: repo, cache: c, ttl: ttl, tracer: tracer}
}

// GetFeeds is a cache-aside read. On miss it falls through to the
// repository and best-effort writes the result back to the cache; cache
// errors never fail the request.
//
// Known race: a mutation landing between the DB read and the cache.Set
// here can leave a stale entry in the cache for up to CACHE_TTL. We rely
// on TTL expiry as the eventual safety net rather than synchronising
// the read and write paths.
func (s *feedService) GetFeeds(ctx context.Context, query string, page, pageSize int) (*Page, error) {
	ctx, span := s.tracer.Start(ctx, "feed.GetFeeds", trace.WithSpanKind(trace.SpanKindInternal))
	defer span.End()

	key := cacheKey(query, page, pageSize)

	var cached Page
	if hit, err := s.cache.Get(ctx, key, &cached); err == nil && hit {
		return &cached, nil
	}

	result, err := s.repo.GetAll(ctx, query, page, pageSize)
	if err != nil {
		return nil, err
	}

	if err := s.cache.Set(ctx, key, result, s.ttl); err != nil {
		log.Warn().Err(err).Str("key", key).Msg("feed cache write failed")
	}
	return result, nil
}

// CreateFeed persists the feed then invalidates every cached page.
func (s *feedService) CreateFeed(ctx context.Context, m *Feed) error {
	ctx, span := s.tracer.Start(ctx, "feed.CreateFeed", trace.WithSpanKind(trace.SpanKindInternal))
	defer span.End()

	if err := s.repo.Create(ctx, m); err != nil {
		return err
	}
	s.invalidate(ctx)
	return nil
}

// UpdateFeed persists the changes then invalidates every cached page.
func (s *feedService) UpdateFeed(ctx context.Context, id int, m *Feed) error {
	ctx, span := s.tracer.Start(ctx, "feed.UpdateFeed", trace.WithSpanKind(trace.SpanKindInternal))
	defer span.End()

	if err := s.repo.Update(ctx, id, m); err != nil {
		return err
	}
	s.invalidate(ctx)
	return nil
}

// DeleteFeed soft-deletes the row then invalidates every cached page.
func (s *feedService) DeleteFeed(ctx context.Context, id int) error {
	ctx, span := s.tracer.Start(ctx, "feed.DeleteFeed", trace.WithSpanKind(trace.SpanKindInternal))
	defer span.End()

	if err := s.repo.Delete(ctx, id); err != nil {
		return err
	}
	s.invalidate(ctx)
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

// invalidate removes every feeds:* cache key. Best-effort: a cache
// failure here is logged but does not fail the write — the next read
// will refresh the entry once its TTL expires.
func (s *feedService) invalidate(ctx context.Context) {
	if err := s.cache.DeletePrefix(ctx, "feeds:"); err != nil {
		log.Warn().Err(err).Msg("feed cache invalidation failed")
	}
}

func cacheKey(query string, page, pageSize int) string {
	return fmt.Sprintf("feeds:q=%s:p=%d:s=%d", query, page, pageSize)
}