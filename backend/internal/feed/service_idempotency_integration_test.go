package feed

import (
	"context"
	"fmt"
	"testing"
	"time"

	"nyx/internal/idempotency"
	"nyx/internal/platform/cache"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace/noop"
)

type idempotentService interface {
	Service
	CreateFeedIdempotent(ctx context.Context, m *Feed, key string) error
	UpdateFeedIdempotent(ctx context.Context, id int, m *Feed, key string) error
	DeleteFeedIdempotent(ctx context.Context, id int, key string) (DeleteReceipt, error)
}

type countingIndexer struct {
	indexCalls  int
	deleteCalls int
}

func (c *countingIndexer) Index(context.Context, int, *Feed) error {
	c.indexCalls++
	return nil
}

func (c *countingIndexer) Delete(context.Context, int) error {
	c.deleteCalls++
	return nil
}

func newIdempotentService(t *testing.T) (idempotentService, *countingIndexer, *pgxpool.Pool) {
	t.Helper()
	if dbURL == "" {
		t.Skip("SKIP_CONTAINERS=true")
	}
	pool, err := pgxpool.New(context.Background(), dbURL)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	indexer := &countingIndexer{}
	svc := NewService(
		NewRepository(pool),
		cache.NewNoop(),
		indexer,
		time.Minute,
		time.Second,
		noop.NewTracerProvider().Tracer("test"),
	)
	setter, ok := svc.(interface{ SetIdempotency(*idempotency.Store) })
	require.True(t, ok)
	setter.SetIdempotency(idempotency.NewStore(pool, time.Hour))

	idem, ok := svc.(idempotentService)
	require.True(t, ok)
	return idem, indexer, pool
}

func insertFeedTestUser(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var id int
	err := pool.QueryRow(
		context.Background(),
		`INSERT INTO users (username, password_hash) VALUES ($1, $2) RETURNING id`,
		fmt.Sprintf("feed-idem-%d", time.Now().UnixNano()),
		"hash",
	).Scan(&id)
	require.NoError(t, err)
	return id
}

func countIdempotencyRows(t *testing.T, pool *pgxpool.Pool, ownerID int, key string) int {
	t.Helper()
	var count int
	err := pool.QueryRow(
		context.Background(),
		`SELECT count(*) FROM tool_idempotency WHERE owner_id = $1 AND key = $2`,
		ownerID,
		key,
	).Scan(&count)
	require.NoError(t, err)
	return count
}

func countUserFeeds(t *testing.T, pool *pgxpool.Pool, ownerID int) int {
	t.Helper()
	var count int
	err := pool.QueryRow(
		context.Background(),
		`SELECT count(*) FROM feeds WHERE user_id = $1 AND deleted_at IS NULL`,
		ownerID,
	).Scan(&count)
	require.NoError(t, err)
	return count
}

func TestCreateFeedIdempotentReplaysStoredResult(t *testing.T) {
	svc, indexer, pool := newIdempotentService(t)
	userID := insertFeedTestUser(t, pool)
	ctx := ctxWithUser(t, userID)
	key := fmt.Sprintf("create-%d", time.Now().UnixNano())

	first := &Feed{Title: "original", Description: "d"}
	require.NoError(t, svc.CreateFeedIdempotent(ctx, first, key))
	require.NotZero(t, first.ID)
	assert.Equal(t, 1, indexer.indexCalls)
	assert.Equal(t, 1, countUserFeeds(t, pool, userID))

	second := &Feed{Title: "original", Description: "d"}
	require.NoError(t, svc.CreateFeedIdempotent(ctx, second, key))
	assert.Equal(t, first.ID, second.ID)
	assert.Equal(t, first.Title, second.Title)
	assert.Equal(t, 1, indexer.indexCalls, "replay must not call the embedding indexer")
	assert.Equal(t, 1, countUserFeeds(t, pool, userID))
	assert.Equal(t, 1, countIdempotencyRows(t, pool, userID, key))
}

func TestUpdateFeedIdempotentReplaysStoredResult(t *testing.T) {
	svc, indexer, pool := newIdempotentService(t)
	userID := insertFeedTestUser(t, pool)
	ctx := ctxWithUser(t, userID)

	created := &Feed{Title: "before"}
	require.NoError(t, svc.CreateFeed(ctx, created))
	indexCallsBefore := indexer.indexCalls

	key := fmt.Sprintf("update-%d", time.Now().UnixNano())
	first := &Feed{Title: "after"}
	require.NoError(t, svc.UpdateFeedIdempotent(ctx, created.ID, first, key))
	assert.Equal(t, created.ID, first.ID)
	assert.Equal(t, 1, indexer.indexCalls-indexCallsBefore)

	second := &Feed{Title: "after"}
	require.NoError(t, svc.UpdateFeedIdempotent(ctx, created.ID, second, key))
	assert.Equal(t, first.ID, second.ID)
	assert.Equal(t, first.Title, second.Title)
	assert.Equal(t, 1, indexer.indexCalls-indexCallsBefore, "replay must not call the embedding indexer")
}

func TestDeleteFeedIdempotentReplayReturnsOriginalSuccess(t *testing.T) {
	svc, indexer, pool := newIdempotentService(t)
	userID := insertFeedTestUser(t, pool)
	ctx := ctxWithUser(t, userID)

	created := &Feed{Title: "delete me"}
	require.NoError(t, svc.CreateFeed(ctx, created))
	deleteCallsBefore := indexer.deleteCalls

	key := fmt.Sprintf("delete-%d", time.Now().UnixNano())
	first, err := svc.DeleteFeedIdempotent(ctx, created.ID, key)
	require.NoError(t, err)
	assert.True(t, first.Deleted)
	assert.Equal(t, created.ID, first.ID)
	assert.Equal(t, 1, indexer.deleteCalls-deleteCallsBefore)

	second, err := svc.DeleteFeedIdempotent(ctx, created.ID, key)
	require.NoError(t, err, "replayed delete must not surface not-found")
	assert.Equal(t, first, second)
	assert.Equal(t, 1, indexer.deleteCalls-deleteCallsBefore, "replay must not call the embedding indexer")
}

func TestIdempotentWritesScopeKeysPerOwner(t *testing.T) {
	svc, _, pool := newIdempotentService(t)
	userA := insertFeedTestUser(t, pool)
	userB := insertFeedTestUser(t, pool)
	key := fmt.Sprintf("shared-%d", time.Now().UnixNano())

	a := &Feed{Title: "a"}
	require.NoError(t, svc.CreateFeedIdempotent(ctxWithUser(t, userA), a, key))
	b := &Feed{Title: "b"}
	require.NoError(t, svc.CreateFeedIdempotent(ctxWithUser(t, userB), b, key))

	assert.NotEqual(t, a.ID, b.ID)
	assert.Equal(t, 1, countUserFeeds(t, pool, userA))
	assert.Equal(t, 1, countUserFeeds(t, pool, userB))
	assert.Equal(t, 1, countIdempotencyRows(t, pool, userA, key))
	assert.Equal(t, 1, countIdempotencyRows(t, pool, userB, key))
}

func TestWritesWithoutKeyWriteNoIdempotencyRow(t *testing.T) {
	svc, _, pool := newIdempotentService(t)
	userID := insertFeedTestUser(t, pool)
	ctx := ctxWithUser(t, userID)

	created := &Feed{Title: "legacy"}
	require.NoError(t, svc.CreateFeed(ctx, created))
	require.NoError(t, svc.UpdateFeed(ctx, created.ID, &Feed{Title: "legacy updated"}))
	require.NoError(t, svc.DeleteFeed(ctx, created.ID))

	var count int
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT count(*) FROM tool_idempotency WHERE owner_id = $1`, userID).Scan(&count))
	assert.Zero(t, count)
}

func TestIdempotentDeleteFailureLeavesNoRecord(t *testing.T) {
	svc, _, pool := newIdempotentService(t)
	userID := insertFeedTestUser(t, pool)
	ctx := ctxWithUser(t, userID)
	key := fmt.Sprintf("missing-%d", time.Now().UnixNano())

	_, err := svc.DeleteFeedIdempotent(ctx, 999999, key)
	require.ErrorIs(t, err, ErrNotFound)
	assert.Zero(t, countIdempotencyRows(t, pool, userID, key))

	created := &Feed{Title: "retry"}
	require.NoError(t, svc.CreateFeed(ctx, created))
	receipt, err := svc.DeleteFeedIdempotent(ctx, created.ID, key)
	require.NoError(t, err)
	assert.True(t, receipt.Deleted)
}
