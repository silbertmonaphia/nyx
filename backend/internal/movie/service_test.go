package movie

import (
	"context"
	"testing"
	"time"

	"nyx/internal/platform/cache"

	"github.com/alicebob/miniredis/v2"
	"github.com/jmoiron/sqlx"
)

// stubRepo captures arguments and returns canned values. It avoids
// sqlmock because this test is about the cache layer, not SQL.
type stubRepo struct {
	getAllCalls int
	getAllResp  *Page
	getAllErr   error
}

func (s *stubRepo) GetAll(ctx context.Context, query string, page, pageSize int) (*Page, error) {
	s.getAllCalls++
	return s.getAllResp, s.getAllErr
}
func (s *stubRepo) Create(context.Context, *Movie) error   { return nil }
func (s *stubRepo) Update(context.Context, int, *Movie) error { return nil }
func (s *stubRepo) Delete(context.Context, int) error       { return nil }
func (s *stubRepo) Ping(context.Context) error             { return nil }
func (s *stubRepo) WithTx(tx *sqlx.Tx) Repository          { return s }

func newServiceWithCache(t *testing.T) (Service, *stubRepo, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	c, err := cache.NewRedis("redis://" + mr.Addr())
	if err != nil {
		t.Fatalf("connect miniredis: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	repo := &stubRepo{}
	svc := NewService(repo, c, time.Minute)
	return svc, repo, mr
}

func TestGetMovies_CacheMissThenHit(t *testing.T) {
	svc, repo, _ := newServiceWithCache(t)
	ctx := context.Background()

	repo.getAllResp = &Page{
		Items:    []Movie{{ID: 1, Title: "The Matrix", Rating: 8.7}},
		Total:    1,
		Page:     1,
		PageSize: 20,
	}

	if _, err := svc.GetMovies(ctx, "", 1, 20); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if repo.getAllCalls != 1 {
		t.Errorf("expected 1 repo call after miss, got %d", repo.getAllCalls)
	}

	if _, err := svc.GetMovies(ctx, "", 1, 20); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if repo.getAllCalls != 1 {
		t.Errorf("expected repo call count to stay 1 after cache hit, got %d", repo.getAllCalls)
	}
}

func TestGetMovies_CacheKeyIncludesSearch(t *testing.T) {
	svc, repo, _ := newServiceWithCache(t)
	ctx := context.Background()

	repo.getAllResp = &Page{Items: []Movie{}, Total: 0, Page: 1, PageSize: 20}

	if _, err := svc.GetMovies(ctx, "", 1, 20); err != nil {
		t.Fatalf("call 1: %v", err)
	}
	if _, err := svc.GetMovies(ctx, "matrix", 1, 20); err != nil {
		t.Fatalf("call 2: %v", err)
	}
	if _, err := svc.GetMovies(ctx, "", 1, 20); err != nil {
		t.Fatalf("call 3: %v", err)
	}
	if repo.getAllCalls != 2 {
		t.Errorf("expected 2 repo calls (different keys miss, same key hits), got %d", repo.getAllCalls)
	}
}

func TestMutations_InvalidateCache(t *testing.T) {
	svc, repo, _ := newServiceWithCache(t)
	ctx := context.Background()

	repo.getAllResp = &Page{Items: []Movie{{ID: 1, Title: "A"}}, Total: 1, Page: 1, PageSize: 20}

	if _, err := svc.GetMovies(ctx, "", 1, 20); err != nil {
		t.Fatalf("warm cache: %v", err)
	}
	if _, err := svc.GetMovies(ctx, "", 1, 20); err != nil {
		t.Fatalf("read after warm: %v", err)
	}
	if repo.getAllCalls != 1 {
		t.Fatalf("expected 1 call before mutation, got %d", repo.getAllCalls)
	}

	// Mutation should invalidate the cache prefix, forcing a fresh DB read.
	if err := svc.CreateMovie(ctx, &Movie{Title: "B"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := svc.GetMovies(ctx, "", 1, 20); err != nil {
		t.Fatalf("read after invalidate: %v", err)
	}
	if repo.getAllCalls != 2 {
		t.Errorf("expected repo to be called again after invalidation, got %d calls", repo.getAllCalls)
	}
}

func TestCheckHealth_AndCheckCacheHealth(t *testing.T) {
	svc, _, _ := newServiceWithCache(t)
	ctx := context.Background()

	if err := svc.CheckHealth(ctx); err != nil {
		t.Errorf("CheckHealth: %v", err)
	}
	if err := svc.CheckCacheHealth(ctx); err != nil {
		t.Errorf("CheckCacheHealth: %v", err)
	}
}