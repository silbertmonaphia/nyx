package feed

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"nyx/internal/feed/db"
	"nyx/internal/platform/api"
	"nyx/test"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	testDB *test.TestDB
	dbURL  string
)

func TestMain(m *testing.M) {
	// Install the huma error override before any huma-using test
	// (handler_test.go) builds its router. This keeps the on-wire
	// error envelope identical to the gin-era contract that the
	// Vite frontend and the e2e suite expect.
	api.OverrideHumaErrors()

	// Allow skipping container-based tests locally. The unit tests
	// (service_test.go, handler_test.go) still run in this mode; only
	// the Postgres-backed integration tests are skipped.
	skipContainers := os.Getenv("SKIP_CONTAINERS") == "true"

	// Set up the database container once for all tests in this package
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	if !skipContainers {
		// CI path: if DB_URL is set, use the externally-provided
		// Postgres (e.g. a service container) directly instead of
		// spinning up a testcontainer.
		if url := os.Getenv("DB_URL"); url != "" {
			dbURL = url
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

			// Run migrations
			if err := testDB.RunMigrationsWithContext(ctx); err != nil {
				fmt.Fprintf(os.Stderr, "Failed to run migrations: %v\n", err)
				if testDB.Container != nil {
					_ = testDB.Container.Terminate(context.Background())
				}
				os.Exit(1)
			}
		}
	} else {
		fmt.Println("Skipping container-based integration tests")
	}

	// Run all tests; integration tests in this file self-skip when
	// dbURL == "".
	code := m.Run()

	if !skipContainers && testDB != nil && testDB.Container != nil {
		_ = testDB.Container.Terminate(context.Background())
	}

	os.Exit(code)
}

func setupIntegrationTest(t *testing.T) (*pgxpool.Pool, Repository) {
	t.Helper()

	// Skip when the container-backed TestMain decided not to start one
	// (e.g. SKIP_CONTAINERS=true).
	if dbURL == "" {
		t.Skip("PostgreSQL container not available; set SKIP_CONTAINERS=false to run integration tests")
	}

	// Open a pgxpool for the test. Cap the pool small so a single
	// test process never starves the local Postgres.
	poolCfg, err := pgxpool.ParseConfig(dbURL)
	require.NoError(t, err)
	poolCfg.MaxConns = 5
	poolCfg.MinConns = 2
	poolCfg.MaxConnLifetime = 2 * time.Minute
	pool, err := pgxpool.NewWithConfig(context.Background(), poolCfg)
	require.NoError(t, err)

	// Clean up tables before each test to ensure isolation.
	_, err = pool.Exec(context.Background(), "DELETE FROM feeds")
	require.NoError(t, err)

	t.Cleanup(func() { pool.Close() })

	repo := NewRepository(pool)
	return pool, repo
}

func TestRepositoryIntegration(t *testing.T) {
	if dbURL == "" {
		t.Skip("PostgreSQL container not available; set SKIP_CONTAINERS=false to run integration tests")
	}
	ctx := context.Background()

	t.Run("CreateFeed", func(t *testing.T) {
		_, repo := setupIntegrationTest(t)

		feed := &Feed{
			Title:       "The Matrix",
			Description: "A computer hacker learns about the true nature of reality",
			Rating:      8.7,
		}

		err := repo.Create(ctx, feed)
		require.NoError(t, err)
		assert.NotZero(t, feed.ID)
		assert.NotZero(t, feed.CreatedAt)
		assert.NotZero(t, feed.UpdatedAt)
	})

	t.Run("GetAllFeeds", func(t *testing.T) {
		_, repo := setupIntegrationTest(t)

		// Create test feeds
		feeds := []*Feed{
			{Title: "Feed 1", Description: "Description 1", Rating: 7.5},
			{Title: "Feed 2", Description: "Description 2", Rating: 8.0},
			{Title: "Feed 3", Description: "Description 3", Rating: 9.0},
		}

		for _, f := range feeds {
			err := repo.Create(ctx, f)
			require.NoError(t, err)
		}

		// Get all feeds
		allFeeds, err := repo.GetAll(ctx, "", 1, 100)
		require.NoError(t, err)
		assert.Len(t, allFeeds.Items, 3)
		assert.Equal(t, 3, allFeeds.Total)
	})

	t.Run("SearchFeeds", func(t *testing.T) {
		_, repo := setupIntegrationTest(t)

		// Create test feeds
		feeds := []*Feed{
			{Title: "The Matrix", Description: "Sci-fi action", Rating: 8.7},
			{Title: "Inception", Description: "Mind-bending thriller", Rating: 8.8},
			{Title: "Interstellar", Description: "Space exploration", Rating: 8.6},
		}

		for _, f := range feeds {
			err := repo.Create(ctx, f)
			require.NoError(t, err)
		}

		// Search by title
		results, err := repo.GetAll(ctx, "matrix", 1, 20)
		require.NoError(t, err)
		assert.Len(t, results.Items, 1)
		assert.Equal(t, "The Matrix", results.Items[0].Title)

		// Search by description
		results, err = repo.GetAll(ctx, "thriller", 1, 20)
		require.NoError(t, err)
		assert.Len(t, results.Items, 1)
		assert.Equal(t, "Inception", results.Items[0].Title)

		// Search with no matches
		results, err = repo.GetAll(ctx, "nonexistent", 1, 20)
		require.NoError(t, err)
		assert.Empty(t, results.Items)
	})

	t.Run("UpdateFeed", func(t *testing.T) {
		_, repo := setupIntegrationTest(t)

		// Create a feed
		feed := &Feed{
			Title:       "Original Title",
			Description: "Original description",
			Rating:      7.0,
		}
		err := repo.Create(ctx, feed)
		require.NoError(t, err)
		originalID := feed.ID

		// Update the feed
		updated := &Feed{
			Title:       "Updated Title",
			Description: "Updated description",
			Rating:      9.5,
		}

		err = repo.Update(ctx, originalID, updated)
		require.NoError(t, err)

		// Verify update
		allFeeds, err := repo.GetAll(ctx, "", 1, 100)
		require.NoError(t, err)
		assert.Len(t, allFeeds.Items, 1)
		assert.Equal(t, "Updated Title", allFeeds.Items[0].Title)
		assert.Equal(t, "Updated description", allFeeds.Items[0].Description)
		assert.Equal(t, 9.5, allFeeds.Items[0].Rating)
	})

	t.Run("UpdateFeedNotFound", func(t *testing.T) {
		_, repo := setupIntegrationTest(t)

		feed := &Feed{
			Title:  "Test",
			Rating: 7.0,
		}

		err := repo.Update(ctx, 999, feed)
		assert.Error(t, err)
		assert.ErrorIs(t, err, ErrNotFound)
	})

	t.Run("DeleteFeed", func(t *testing.T) {
		_, repo := setupIntegrationTest(t)

		// Create a feed
		feed := &Feed{
			Title:  "To Delete",
			Rating: 7.0,
		}
		err := repo.Create(ctx, feed)
		require.NoError(t, err)

		// Delete the feed
		err = repo.Delete(ctx, feed.ID)
		require.NoError(t, err)

		// Verify soft delete (feed should not appear in results)
		allFeeds, err := repo.GetAll(ctx, "", 1, 100)
		require.NoError(t, err)
		assert.Empty(t, allFeeds.Items)
	})

	t.Run("DeleteFeedNotFound", func(t *testing.T) {
		_, repo := setupIntegrationTest(t)

		err := repo.Delete(ctx, 999)
		assert.Error(t, err)
		assert.ErrorIs(t, err, ErrNotFound)
	})

	t.Run("Ping", func(t *testing.T) {
		_, repo := setupIntegrationTest(t)

		err := repo.Ping(ctx)
		require.NoError(t, err)
	})
}

// TestRepositoryWithTransactions exercises the new
// NewRepositoryFromQuerier constructor. A tx-bound *db.Queries is
// built via db.New(pool).WithTx(tx), so the same Repository
// interface works for non-tx and tx-scoped operations.
func TestRepositoryWithTransactions(t *testing.T) {
	if dbURL == "" {
		t.Skip("PostgreSQL container not available; set SKIP_CONTAINERS=false to run integration tests")
	}
	ctx := context.Background()

	t.Run("TransactionRollback", func(t *testing.T) {
		pool, repo := setupIntegrationTest(t)

		// Start transaction
		tx, err := pool.Begin(ctx)
		require.NoError(t, err)

		// Get repository with transaction
		txRepo := NewRepositoryFromQuerier(db.New(pool).WithTx(tx))

		// Create a feed in transaction
		feed := &Feed{
			Title:  "Transaction Test",
			Rating: 8.0,
		}
		err = txRepo.Create(ctx, feed)
		require.NoError(t, err)

		// Rollback
		require.NoError(t, tx.Rollback(ctx))

		// Verify feed was not created
		allFeeds, err := repo.GetAll(ctx, "", 1, 100)
		require.NoError(t, err)
		assert.Empty(t, allFeeds.Items)
	})

	t.Run("TransactionCommit", func(t *testing.T) {
		pool, repo := setupIntegrationTest(t)

		// Start transaction
		tx, err := pool.Begin(ctx)
		require.NoError(t, err)

		// Get repository with transaction
		txRepo := NewRepositoryFromQuerier(db.New(pool).WithTx(tx))

		// Create a feed in transaction
		feed := &Feed{
			Title:  "Transaction Commit Test",
			Rating: 8.5,
		}
		err = txRepo.Create(ctx, feed)
		require.NoError(t, err)

		// Commit
		require.NoError(t, tx.Commit(ctx))

		// Verify feed was created
		allFeeds, err := repo.GetAll(ctx, "", 1, 100)
		require.NoError(t, err)
		assert.Len(t, allFeeds.Items, 1)
		assert.Equal(t, "Transaction Commit Test", allFeeds.Items[0].Title)
	})
}