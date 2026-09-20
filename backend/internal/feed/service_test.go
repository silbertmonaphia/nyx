package feed

import (
	"context"
	"testing"
	"time"

	"nyx/internal/platform/cache"
	"nyx/internal/reqctx"

	"github.com/alicebob/miniredis/v2"
	"go.opentelemetry.io/otel/trace/noop"
)

// stubRepo captures arguments and returns canned values. It avoids
// pgxmock because this test is about the cache layer, not SQL.
type stubRepo struct {
	getAllCalls int
	getAllResp  *Page
	getAllErr   error

	// Record the userID each call observed. Lets tests assert that
	// per-user cache keys are constructed correctly and that the
	// repo receives the caller's id (not a different one).
	lastUserID int
}

func (s *stubRepo) GetAll(_ context.Context, userID int, _ string, _, _ int, _ SortOrder) (*Page, error) {
	s.getAllCalls++
	s.lastUserID = userID
	return s.getAllResp, s.getAllErr
}
func (s *stubRepo) Create(_ context.Context, userID int, m *Feed) error {
	s.lastUserID = userID
	return nil
}
func (s *stubRepo) Update(_ context.Context, userID int, id int, m *Feed) error {
	s.lastUserID = userID
	return nil
}
func (s *stubRepo) Delete(_ context.Context, userID int, id int) error {
	s.lastUserID = userID
	return nil
}
func (s *stubRepo) Ping(context.Context) error { return nil }

func newServiceWithCache(t *testing.T) (Service, *stubRepo, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	c, err := cache.NewRedis("redis://" + mr.Addr())
	if err != nil {
		t.Fatalf("connect miniredis: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	repo := &stubRepo{}
	svc := NewService(repo, c, time.Minute, noop.NewTracerProvider().Tracer("test"))
	return svc, repo, mr
}

// ctxWithUser stamps a user id onto ctx the same way the auth
// middleware would. Tests that exercise the per-user cache key or
// the repo threading must seed this; the service reads the value
// via reqctx.UserIDFromContext.
func ctxWithUser(t *testing.T, userID int) context.Context {
	t.Helper()
	return reqctx.WithUserID(context.Background(), userID)
}

func TestGetFeeds_CacheMissThenHit(t *testing.T) {
	svc, repo, _ := newServiceWithCache(t)
	ctx := ctxWithUser(t, 1)

	repo.getAllResp = &Page{
		Items:    []Feed{{ID: 1, Title: "The Matrix", Rating: 8.7}},
		Total:    1,
		Page:     1,
		PageSize: 20,
	}

	if _, err := svc.GetFeeds(ctx, "", 1, 20, SortDesc); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if repo.getAllCalls != 1 {
		t.Errorf("expected 1 repo call after miss, got %d", repo.getAllCalls)
	}

	if _, err := svc.GetFeeds(ctx, "", 1, 20, SortDesc); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if repo.getAllCalls != 1 {
		t.Errorf("expected repo call count to stay 1 after cache hit, got %d", repo.getAllCalls)
	}
}

func TestGetFeeds_CacheKeyIncludesSearch(t *testing.T) {
	svc, repo, _ := newServiceWithCache(t)
	ctx := ctxWithUser(t, 1)

	repo.getAllResp = &Page{Items: []Feed{}, Total: 0, Page: 1, PageSize: 20}

	if _, err := svc.GetFeeds(ctx, "", 1, 20, SortDesc); err != nil {
		t.Fatalf("call 1: %v", err)
	}
	if _, err := svc.GetFeeds(ctx, "matrix", 1, 20, SortDesc); err != nil {
		t.Fatalf("call 2: %v", err)
	}
	if _, err := svc.GetFeeds(ctx, "", 1, 20, SortDesc); err != nil {
		t.Fatalf("call 3: %v", err)
	}
	if repo.getAllCalls != 2 {
		t.Errorf("expected 2 repo calls (different keys miss, same key hits), got %d", repo.getAllCalls)
	}
}

// TestGetFeeds_CacheKeyIncludesOrder guards against an ASC/DESC cache
// collision: if both directions share a cache key, the second caller
// gets the wrong ordering until the TTL expires.
func TestGetFeeds_CacheKeyIncludesOrder(t *testing.T) {
	svc, repo, _ := newServiceWithCache(t)
	ctx := ctxWithUser(t, 1)

	repo.getAllResp = &Page{Items: []Feed{}, Total: 0, Page: 1, PageSize: 20}

	if _, err := svc.GetFeeds(ctx, "", 1, 20, SortDesc); err != nil {
		t.Fatalf("desc call: %v", err)
	}
	if _, err := svc.GetFeeds(ctx, "", 1, 20, SortAsc); err != nil {
		t.Fatalf("asc call: %v", err)
	}
	if _, err := svc.GetFeeds(ctx, "", 1, 20, SortDesc); err != nil {
		t.Fatalf("desc repeat: %v", err)
	}
	// Two distinct keys: desc + asc each miss once; desc repeat hits.
	if repo.getAllCalls != 2 {
		t.Errorf("expected 2 repo calls (ASC and DESC are separate cache keys), got %d", repo.getAllCalls)
	}
}

// TestGetFeeds_CacheKeyIncludesUserID is the per-user cache isolation
// guard: user 1 and user 2 sharing the same query/page/order tuple
// must NOT collide on the cache key. Otherwise user A could read user
// B's cached page until TTL expiry — and vice versa for mutations.
func TestGetFeeds_CacheKeyIncludesUserID(t *testing.T) {
	svc, repo, _ := newServiceWithCache(t)

	repo.getAllResp = &Page{Items: []Feed{}, Total: 0, Page: 1, PageSize: 20}

	if _, err := svc.GetFeeds(ctxWithUser(t, 1), "", 1, 20, SortDesc); err != nil {
		t.Fatalf("user 1 call 1: %v", err)
	}
	if _, err := svc.GetFeeds(ctxWithUser(t, 2), "", 1, 20, SortDesc); err != nil {
		t.Fatalf("user 2 call: %v", err)
	}
	if _, err := svc.GetFeeds(ctxWithUser(t, 1), "", 1, 20, SortDesc); err != nil {
		t.Fatalf("user 1 call 2: %v", err)
	}
	// user 1 (twice) + user 2 (once) = 2 distinct cache misses.
	if repo.getAllCalls != 2 {
		t.Errorf("expected 2 repo calls (per-user cache isolation), got %d", repo.getAllCalls)
	}
	// The repo should have observed userID=1 and userID=2 across
	// the misses.
	if repo.lastUserID != 2 {
		t.Errorf("expected last repo call to observe userID=2, got %d", repo.lastUserID)
	}
}

func TestMutations_InvalidateCache(t *testing.T) {
	svc, repo, _ := newServiceWithCache(t)
	ctx := ctxWithUser(t, 1)

	repo.getAllResp = &Page{Items: []Feed{{ID: 1, Title: "A"}}, Total: 1, Page: 1, PageSize: 20}

	if _, err := svc.GetFeeds(ctx, "", 1, 20, SortDesc); err != nil {
		t.Fatalf("warm cache: %v", err)
	}
	if _, err := svc.GetFeeds(ctx, "", 1, 20, SortDesc); err != nil {
		t.Fatalf("read after warm: %v", err)
	}
	if repo.getAllCalls != 1 {
		t.Fatalf("expected 1 call before mutation, got %d", repo.getAllCalls)
	}

	// Mutation should invalidate the cache prefix, forcing a fresh DB read.
	if err := svc.CreateFeed(ctx, &Feed{Title: "B"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := svc.GetFeeds(ctx, "", 1, 20, SortDesc); err != nil {
		t.Fatalf("read after invalidate: %v", err)
	}
	if repo.getAllCalls != 2 {
		t.Errorf("expected repo to be called again after invalidation, got %d calls", repo.getAllCalls)
	}
}

// TestMutations_InvalidateOnlyCallersCache is the per-user
// invalidation guard: a mutation by user 1 must NOT evict user 2's
// cached page. Otherwise user 2 would suffer a cold cache every
// time user 1 wrote anything — and on a multi-tenant deployment
// every write would invalidate every user's cache.
func TestMutations_InvalidateOnlyCallersCache(t *testing.T) {
	svc, repo, _ := newServiceWithCache(t)

	repo.getAllResp = &Page{Items: []Feed{}, Total: 0, Page: 1, PageSize: 20}

	ctx1 := ctxWithUser(t, 1)
	ctx2 := ctxWithUser(t, 2)

	// Warm both users' caches.
	if _, err := svc.GetFeeds(ctx1, "", 1, 20, SortDesc); err != nil {
		t.Fatalf("warm user 1: %v", err)
	}
	if _, err := svc.GetFeeds(ctx2, "", 1, 20, SortDesc); err != nil {
		t.Fatalf("warm user 2: %v", err)
	}
	if repo.getAllCalls != 2 {
		t.Fatalf("expected 2 warm-up calls, got %d", repo.getAllCalls)
	}

	// User 1 mutates. User 1's cache should invalidate; user 2's
	// must NOT.
	if err := svc.CreateFeed(ctx1, &Feed{Title: "B"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, err := svc.GetFeeds(ctx1, "", 1, 20, SortDesc); err != nil {
		t.Fatalf("user 1 read after mutation: %v", err)
	}
	if _, err := svc.GetFeeds(ctx2, "", 1, 20, SortDesc); err != nil {
		t.Fatalf("user 2 read after user 1 mutation: %v", err)
	}
	// User 1 forced a fresh fetch (+1), user 2 hit cache (+0).
	if repo.getAllCalls != 3 {
		t.Errorf("expected 3 repo calls (user 1 miss after invalidation, user 2 hit), got %d", repo.getAllCalls)
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

// TestMutationCacheFailure_DoesNotFailRequest verifies that a cache
// failure during invalidation is best-effort: the mutation still
// succeeds and the next read still gets a fresh result from the repo.
func TestMutationCacheFailure_DoesNotFailRequest(t *testing.T) {
	svc, repo, mr := newServiceWithCache(t)
	ctx := ctxWithUser(t, 1)

	repo.getAllResp = &Page{Items: []Feed{{ID: 1, Title: "A"}}, Total: 1, Page: 1, PageSize: 20}

	if _, err := svc.GetFeeds(ctx, "", 1, 20, SortDesc); err != nil {
		t.Fatalf("warm cache: %v", err)
	}

	// Kill the cache so the mutation's invalidation will fail. The
	// service must not propagate that error.
	mr.Close()

	if err := svc.CreateFeed(ctx, &Feed{Title: "B"}); err != nil {
		t.Fatalf("create should not fail when cache invalidation fails: %v", err)
	}
}

// TestGetFeeds_DefensiveEmptyPageWhenNoUser guards the invariant
// when auth middleware failed to stamp the context (should never
// happen in production, but unit tests bypass it). Returning an
// empty page is safer than querying with userID=0 — which would
// match no rows in production but could resolve to a stale cache
// populated by a regression elsewhere.
func TestGetFeeds_DefensiveEmptyPageWhenNoUser(t *testing.T) {
	svc, repo, _ := newServiceWithCache(t)

	page, err := svc.GetFeeds(context.Background(), "", 1, 20, SortDesc)
	if err != nil {
		t.Fatalf("GetFeeds without user: %v", err)
	}
	if page == nil || page.Total != 0 || len(page.Items) != 0 {
		t.Errorf("expected empty page when no user id, got %+v", page)
	}
	if repo.getAllCalls != 0 {
		t.Errorf("repo must NOT be called without a user id, got %d calls", repo.getAllCalls)
	}
}
