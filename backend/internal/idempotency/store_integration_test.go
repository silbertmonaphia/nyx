package idempotency

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"nyx/test"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	testDB *test.TestDB
	dbURL  string
)

func TestMain(m *testing.M) {
	skipContainers := os.Getenv("SKIP_CONTAINERS") == "true"
	ctx, cancel := context.WithTimeout(context.Background(), test.LockWaitTimeout)
	defer cancel()

	var release func()
	if !skipContainers {
		if url := os.Getenv("DB_URL"); url != "" {
			dbURL = url
			var err error
			release, err = test.AcquireIntegrationLock(ctx, dbURL)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Failed to acquire integration lock: %v\n", err)
				os.Exit(1)
			}
			if err := test.RunMigrationsForURL(ctx, dbURL); err != nil {
				fmt.Fprintf(os.Stderr, "Failed to run migrations: %v\n", err)
				os.Exit(1)
			}
		} else {
			var err error
			testDB, err = test.StartPostgres(ctx)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Failed to start PostgreSQL container: %v\n", err)
				os.Exit(1)
			}
			dbURL = testDB.DBURL
			if err := testDB.RunMigrationsWithContext(ctx); err != nil {
				fmt.Fprintf(os.Stderr, "Failed to run migrations: %v\n", err)
				if testDB.Container != nil {
					_ = testDB.Container.Terminate(context.Background())
				}
				os.Exit(1)
			}
		}
	}

	code := m.Run()
	if release != nil {
		release()
	}
	if testDB != nil && testDB.Container != nil {
		_ = testDB.Container.Terminate(context.Background())
	}
	os.Exit(code)
}

func newPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if dbURL == "" {
		t.Skip("SKIP_CONTAINERS=true")
	}
	pool, err := pgxpool.New(context.Background(), dbURL)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

func insertUser(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(
		context.Background(),
		`INSERT INTO users (username, password_hash) VALUES ($1, $2) RETURNING id`,
		fmt.Sprintf("idem-%d", time.Now().UnixNano()),
		"hash",
	).Scan(&id)
	require.NoError(t, err)
	return id
}

func uniqueKey(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("key-%d", time.Now().UnixNano())
}

type testResult struct {
	ID int `json:"id"`
}

func countRows(t *testing.T, pool *pgxpool.Pool, ownerID int64, key string) int {
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

func TestStoreDoConcurrentDuplicateExecutesOnce(t *testing.T) {
	pool := newPool(t)
	ownerID := insertUser(t, pool)
	store := NewStore(pool, time.Hour)
	key := uniqueKey(t)

	var runs atomic.Int32
	const callers = 5
	outs := make([]testResult, callers)
	errs := make([]error, callers)

	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = store.Do(context.Background(), ownerID, key, "create_feed", &outs[i], func(pgx.Tx) (any, error) {
				runs.Add(1)
				time.Sleep(100 * time.Millisecond)
				return testResult{ID: 42}, nil
			})
		}(i)
	}
	wg.Wait()

	require.NoError(t, errs[0])
	require.NoError(t, errs[1])
	assert.EqualValues(t, 1, runs.Load())
	assert.Equal(t, testResult{ID: 42}, outs[0])
	assert.Equal(t, outs[0], outs[1])
	assert.Equal(t, 1, countRows(t, pool, ownerID, key))
}

func TestStoreDoReplaysStoredResponse(t *testing.T) {
	pool := newPool(t)
	ownerID := insertUser(t, pool)
	store := NewStore(pool, time.Hour)
	key := uniqueKey(t)

	var first testResult
	replayed, err := store.Do(context.Background(), ownerID, key, "create_feed", &first, func(pgx.Tx) (any, error) {
		return testResult{ID: 7}, nil
	})
	require.NoError(t, err)
	assert.False(t, replayed)
	assert.Equal(t, 7, first.ID)

	var second testResult
	replayed, err = store.Do(context.Background(), ownerID, key, "create_feed", &second, func(pgx.Tx) (any, error) {
		t.Fatal("mutation ran on replay")
		return nil, nil
	})
	require.NoError(t, err)
	assert.True(t, replayed)
	assert.Equal(t, first, second)
}

func TestStoreDoRollsBackOnMutationError(t *testing.T) {
	pool := newPool(t)
	ownerID := insertUser(t, pool)
	store := NewStore(pool, time.Hour)
	key := uniqueKey(t)

	var out testResult
	_, err := store.Do(context.Background(), ownerID, key, "create_feed", &out, func(pgx.Tx) (any, error) {
		return nil, errors.New("mutation failed")
	})
	require.Error(t, err)
	assert.Equal(t, 0, countRows(t, pool, ownerID, key))

	var runs int
	replayed, err := store.Do(context.Background(), ownerID, key, "create_feed", &out, func(pgx.Tx) (any, error) {
		runs++
		return testResult{ID: 9}, nil
	})
	require.NoError(t, err)
	assert.False(t, replayed)
	assert.Equal(t, 1, runs)
	assert.Equal(t, 9, out.ID)
}

func TestStoreDoScopesKeysPerOwner(t *testing.T) {
	pool := newPool(t)
	ownerA := insertUser(t, pool)
	ownerB := insertUser(t, pool)
	store := NewStore(pool, time.Hour)
	key := uniqueKey(t)

	var a, b testResult
	var runs atomic.Int32
	_, err := store.Do(context.Background(), ownerA, key, "create_feed", &a, func(pgx.Tx) (any, error) {
		runs.Add(1)
		return testResult{ID: 1}, nil
	})
	require.NoError(t, err)
	_, err = store.Do(context.Background(), ownerB, key, "create_feed", &b, func(pgx.Tx) (any, error) {
		runs.Add(1)
		return testResult{ID: 2}, nil
	})
	require.NoError(t, err)

	assert.EqualValues(t, 2, runs.Load())
	assert.Equal(t, 1, a.ID)
	assert.Equal(t, 2, b.ID)
	assert.Equal(t, 1, countRows(t, pool, ownerA, key))
	assert.Equal(t, 1, countRows(t, pool, ownerB, key))
}

func TestStoreDeleteExpired(t *testing.T) {
	pool := newPool(t)
	ownerID := insertUser(t, pool)
	key := uniqueKey(t)
	ctx := context.Background()

	_, err := pool.Exec(
		ctx,
		`INSERT INTO tool_idempotency (owner_id, key, tool, response, created_at)
		 VALUES ($1, $2, 'create_feed', 'null'::jsonb, now() - interval '30 days')`,
		ownerID,
		key,
	)
	require.NoError(t, err)

	var feedID int
	require.NoError(t, pool.QueryRow(
		ctx,
		`INSERT INTO feeds (user_id, title, description) VALUES ($1, 'kept', '') RETURNING id`,
		ownerID,
	).Scan(&feedID))

	store := NewStore(pool, 7*24*time.Hour)
	deleted, err := store.DeleteExpired(ctx)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, deleted, int64(1))
	assert.Equal(t, 0, countRows(t, pool, ownerID, key))

	var feedCount int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM feeds WHERE id = $1`, feedID).Scan(&feedCount))
	assert.Equal(t, 1, feedCount)
}
