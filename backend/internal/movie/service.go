package movie

import (
	"context"
	"fmt"
	"time"

	"github.com/rs/zerolog/log"
	"go.opentelemetry.io/otel/trace"

	"nyx/internal/platform/cache"
)

type Service interface {
	GetMovies(ctx context.Context, query string, page, pageSize int) (*Page, error)
	CreateMovie(ctx context.Context, m *Movie) error
	UpdateMovie(ctx context.Context, id int, m *Movie) error
	DeleteMovie(ctx context.Context, id int) error
	CheckHealth(ctx context.Context) error
	CheckCacheHealth(ctx context.Context) error
}

type movieService struct {
	repo   Repository
	cache  cache.Cache
	ttl    time.Duration
	tracer trace.Tracer
}

// NewService wires the movie domain. The cache parameter may be a no-op
// (cache.NewNoop()) when caching is disabled — the service treats both
// implementations uniformly. tracer emits one OTel span per public
// method; pass the noop tracer (otel.Tracer("…") with the global
// noop provider) when tracing is disabled — Start becomes free.
func NewService(repo Repository, c cache.Cache, ttl time.Duration, tracer trace.Tracer) Service {
	return &movieService{repo: repo, cache: c, ttl: ttl, tracer: tracer}
}

// GetMovies is a cache-aside read. On miss it falls through to the
// repository and best-effort writes the result back to the cache; cache
// errors never fail the request.
//
// Known race: a mutation landing between the DB read and the cache.Set
// here can leave a stale entry in the cache for up to CACHE_TTL. We rely
// on TTL expiry as the eventual safety net rather than synchronising
// the read and write paths.
func (s *movieService) GetMovies(ctx context.Context, query string, page, pageSize int) (*Page, error) {
	ctx, span := s.tracer.Start(ctx, "movie.GetMovies", trace.WithSpanKind(trace.SpanKindInternal))
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
		log.Warn().Err(err).Str("key", key).Msg("movie cache write failed")
	}
	return result, nil
}

// CreateMovie persists the movie then invalidates every cached page.
func (s *movieService) CreateMovie(ctx context.Context, m *Movie) error {
	ctx, span := s.tracer.Start(ctx, "movie.CreateMovie", trace.WithSpanKind(trace.SpanKindInternal))
	defer span.End()

	if err := s.repo.Create(ctx, m); err != nil {
		return err
	}
	s.invalidate(ctx)
	return nil
}

// UpdateMovie persists the changes then invalidates every cached page.
func (s *movieService) UpdateMovie(ctx context.Context, id int, m *Movie) error {
	ctx, span := s.tracer.Start(ctx, "movie.UpdateMovie", trace.WithSpanKind(trace.SpanKindInternal))
	defer span.End()

	if err := s.repo.Update(ctx, id, m); err != nil {
		return err
	}
	s.invalidate(ctx)
	return nil
}

// DeleteMovie soft-deletes the row then invalidates every cached page.
func (s *movieService) DeleteMovie(ctx context.Context, id int) error {
	ctx, span := s.tracer.Start(ctx, "movie.DeleteMovie", trace.WithSpanKind(trace.SpanKindInternal))
	defer span.End()

	if err := s.repo.Delete(ctx, id); err != nil {
		return err
	}
	s.invalidate(ctx)
	return nil
}

func (s *movieService) CheckHealth(ctx context.Context) error {
	ctx, span := s.tracer.Start(ctx, "movie.CheckHealth", trace.WithSpanKind(trace.SpanKindInternal))
	defer span.End()
	return s.repo.Ping(ctx)
}

func (s *movieService) CheckCacheHealth(ctx context.Context) error {
	ctx, span := s.tracer.Start(ctx, "movie.CheckCacheHealth", trace.WithSpanKind(trace.SpanKindInternal))
	defer span.End()
	return s.cache.Ping(ctx)
}

// invalidate removes every movies:* cache key. Best-effort: a cache
// failure here is logged but does not fail the write — the next read
// will refresh the entry once its TTL expires.
func (s *movieService) invalidate(ctx context.Context) {
	if err := s.cache.DeletePrefix(ctx, "movies:"); err != nil {
		log.Warn().Err(err).Msg("movie cache invalidation failed")
	}
}

func cacheKey(query string, page, pageSize int) string {
	return fmt.Sprintf("movies:q=%s:p=%d:s=%d", query, page, pageSize)
}
