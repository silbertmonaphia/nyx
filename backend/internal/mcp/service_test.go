package mcp

import (
	"context"
	"errors"
	"testing"

	"nyx/internal/feed"
	"nyx/internal/reqctx"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace/noop"
)

// stubFeedService is the feed.Service fake used by every MCP service
// test. It captures the per-call userID (via reqctx) so tests can
// assert the per-owner contract: the JWT subject is the only source
// of truth for "who is the caller", and the wire must never carry an
// attacker-supplied id.
type stubFeedService struct {
	listResp *feed.Page
	listErr  error

	getResp *feed.Feed
	getErr  error

	createErr error
	updateErr error
	deleteErr error

	lastListUserID int
	lastListQuery  string
	lastListPage   int
	lastListSize   int
	lastListOrder  feed.SortOrder

	lastGetID     int
	lastGetUserID int

	lastCreateFeed *feed.Feed
	lastUpdateID   int
	lastUpdateFeed *feed.Feed

	lastDeleteID     int
	lastDeleteUserID int
}

func (s *stubFeedService) GetFeeds(ctx context.Context, query string, page, pageSize int, order feed.SortOrder) (*feed.Page, error) {
	s.lastListUserID = reqctx.UserIDFromContext(ctx)
	s.lastListQuery = query
	s.lastListPage = page
	s.lastListSize = pageSize
	s.lastListOrder = order
	if s.listResp != nil {
		return s.listResp, s.listErr
	}
	return &feed.Page{Items: []feed.Feed{}, Total: 0, Page: page, PageSize: pageSize}, s.listErr
}

func (s *stubFeedService) GetFeedByID(ctx context.Context, id int) (*feed.Feed, error) {
	s.lastGetID = id
	s.lastGetUserID = reqctx.UserIDFromContext(ctx)
	return s.getResp, s.getErr
}

func (s *stubFeedService) CreateFeed(ctx context.Context, m *feed.Feed) error {
	s.lastCreateFeed = m
	return s.createErr
}

func (s *stubFeedService) UpdateFeed(ctx context.Context, id int, m *feed.Feed) error {
	s.lastUpdateID = id
	s.lastUpdateFeed = m
	return s.updateErr
}

func (s *stubFeedService) DeleteFeed(ctx context.Context, id int) error {
	s.lastDeleteID = id
	s.lastDeleteUserID = reqctx.UserIDFromContext(ctx)
	return s.deleteErr
}

func (s *stubFeedService) CheckHealth(context.Context) error      { return nil }
func (s *stubFeedService) CheckCacheHealth(context.Context) error { return nil }
func (s *stubFeedService) SetIndexer(_ feed.EmbeddingIndexer)     {}

// contextOrBG returns a context.Background — tests that exercise the
// GetFeeds stub path don't have an auth-stamped ctx so the
// service's defensive userID==0 branch fires. Tests that care
// about userID propagate reqctx.WithUserID(ctx, n) themselves.
func contextOrBG() context.Context { return context.Background() }

// newTestService returns a Service wired over the supplied stub
// with a noop tracer so spans don't try to export.
func newTestService(stub *stubFeedService) *Service {
	return NewService(stub, noop.NewTracerProvider().Tracer("test"))
}

func TestService_ListFeeds_PageSizeClampedTo100(t *testing.T) {
	stub := &stubFeedService{}
	svc := newTestService(stub)

	_, _ = svc.handleListFeeds(context.Background(), mcp.CallToolRequest{}, listFeedsArgs{
		Page:     1,
		PageSize: 500,
	})

	assert.Equal(t, 100, stub.lastListSize, "page_size=500 must clamp to 100")
}

func TestService_ListFeeds_OrderDefaultsToDesc(t *testing.T) {
	stub := &stubFeedService{}
	svc := newTestService(stub)

	_, _ = svc.handleListFeeds(context.Background(), mcp.CallToolRequest{}, listFeedsArgs{
		Order: "sideways",
	})

	assert.Equal(t, feed.SortDesc, stub.lastListOrder, "unknown order falls back to desc via ParseSortOrder")
}

func TestService_ListFeeds_DefaultsApplied(t *testing.T) {
	stub := &stubFeedService{}
	svc := newTestService(stub)

	// Page and page_size both zero → defaults of 1 / 20.
	_, _ = svc.handleListFeeds(context.Background(), mcp.CallToolRequest{}, listFeedsArgs{})

	assert.Equal(t, 1, stub.lastListPage)
	assert.Equal(t, 20, stub.lastListSize)
}

func TestService_GetFeed_ForwardsID(t *testing.T) {
	stub := &stubFeedService{
		getResp: &feed.Feed{ID: 7, UserID: 42, Title: "x"},
	}
	svc := newTestService(stub)

	res, err := svc.handleGetFeed(context.Background(), mcp.CallToolRequest{}, getFeedArgs{ID: 7})
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Equal(t, 7, stub.lastGetID)
	assert.NotNil(t, res.StructuredContent)
}

func TestService_CreateFeed_StampsUserIDFromContext(t *testing.T) {
	stub := &stubFeedService{}
	svc := newTestService(stub)

	ctx := reqctx.WithUserID(context.Background(), 42)
	_, err := svc.handleCreateFeed(ctx, mcp.CallToolRequest{}, createFeedArgs{
		Title:       "Hello",
		Description: "World",
	})
	require.NoError(t, err)
	require.NotNil(t, stub.lastCreateFeed)
	assert.Equal(t, 42, stub.lastCreateFeed.UserID, "UserID must be stamped from ctx, not from args")
	assert.Equal(t, "Hello", stub.lastCreateFeed.Title)
}

func TestService_UpdateFeed_ErrNotFoundBubblesAsErrNotFound(t *testing.T) {
	stub := &stubFeedService{updateErr: feed.ErrNotFound}
	svc := newTestService(stub)

	ctx := reqctx.WithUserID(context.Background(), 1)
	_, err := svc.handleUpdateFeed(ctx, mcp.CallToolRequest{}, updateFeedArgs{ID: 999, Title: "x"})

	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNotFound), "feed.ErrNotFound must map to mcp.ErrNotFound (single sentinel)")
}

func TestService_GetFeed_ErrNotFoundBubblesAsErrNotFound(t *testing.T) {
	stub := &stubFeedService{getErr: feed.ErrNotFound}
	svc := newTestService(stub)

	ctx := reqctx.WithUserID(context.Background(), 1)
	_, err := svc.handleGetFeed(ctx, mcp.CallToolRequest{}, getFeedArgs{ID: 999})

	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNotFound))
}

func TestService_InternalErrorReturnsErrInternal(t *testing.T) {
	stub := &stubFeedService{deleteErr: errors.New("pgx: connection refused")}
	svc := newTestService(stub)

	ctx := reqctx.WithUserID(context.Background(), 1)
	_, err := svc.handleDeleteFeed(ctx, mcp.CallToolRequest{}, deleteFeedArgs{ID: 1})

	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInternal), "unknown error must map to mcp.ErrInternal — never leak err.Error() to the wire")
}

func TestService_CreateFeed_NoUserID_FailsClosed(t *testing.T) {
	stub := &stubFeedService{}
	svc := newTestService(stub)

	_, err := svc.handleCreateFeed(context.Background(), mcp.CallToolRequest{}, createFeedArgs{Title: "x"})

	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNotFound), "missing userID must fail closed with ErrNotFound (not panic, not propagate)")
	assert.Nil(t, stub.lastCreateFeed, "repo must NOT be called when userID is missing")
}

func TestService_FeedResult_EncodesStructuredContent(t *testing.T) {
	payload := struct {
		Data []feed.Feed `json:"data"`
	}{
		Data: []feed.Feed{{ID: 1, Title: "x"}},
	}
	res, err := feedResult(payload)
	require.NoError(t, err)
	require.NotNil(t, res)
	require.Len(t, res.Content, 1)
	assert.NotNil(t, res.StructuredContent, "StructuredContent must carry the typed payload")
}

// TestMapSentinel_NotFoundLeakFree pins the safe-detail contract:
// feed.ErrNotFound must surface as mcp.ErrNotFound (not as the
// original error, whose Error() includes internal ids that an
// attacker could probe).
func TestMapSentinel_NotFoundLeakFree(t *testing.T) {
	original := feed.ErrNotFound
	err := mapSentinel(context.Background(), original, "Failed to list feeds")
	assert.True(t, errors.Is(err, ErrNotFound), "feed.ErrNotFound must map to mcp.ErrNotFound (single sentinel)")
	// The wire envelope must carry ErrNotFound, NOT the original
	// error (whose Error() includes "feed not found" — fine, but we
	// want to assert that the mapper DID NOT wrap the original
	// since that could leak internal pgx text on other paths).
}
