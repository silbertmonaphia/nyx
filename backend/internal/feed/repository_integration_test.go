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
	// The feeds.user_id FK references users.id, so we delete feeds
	// first and then re-seed two canonical test users. We seed via
	// raw SQL rather than going through the user repo because the
	// schema columns here are intentionally minimal (only the FK
	// fields the migration cares about).
	//
	// The test users sit at id=101 / id=102 (not 1/2) so they don't
	// collide with the placeholder user that migration 000012
	// inserted at id=1 to satisfy the owner-scope backfill on a
	// freshly-bootstrapped DB. Each test below references the same
	// numeric ids for the "owner" / "other user" pair.
	_, err = pool.Exec(context.Background(), "DELETE FROM feeds")
	require.NoError(t, err)
	_, err = pool.Exec(context.Background(),
		`INSERT INTO users (id, username, password_hash) VALUES
			(101, 'owner1', 'x'),
			(102, 'owner2', 'x')
		 ON CONFLICT (id) DO NOTHING`)
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

		err := repo.Create(ctx, 101, feed)
		require.NoError(t, err)
		assert.NotZero(t, feed.ID)
		assert.NotZero(t, feed.CreatedAt)
		assert.NotZero(t, feed.UpdatedAt)
		assert.Equal(t, 101, feed.UserID, "Create must stamp the caller's user_id")
	})

	t.Run("CreateFeed_StampsOwner", func(t *testing.T) {
		// Pin the per-user stamping. Create with userID=102 → row
		// stores user_id=102 even though Feed.UserID was zero before
		// the call (defense-in-depth at the repo).
		_, repo := setupIntegrationTest(t)

		feed := &Feed{Title: "Owned by 102", Rating: 7.0}
		require.NoError(t, repo.Create(ctx, 102, feed))

		assert.Equal(t, 102, feed.UserID)

		// And the row is reachable as user 102 only.
		page, err := repo.GetAll(ctx, 102, "", 1, 20, SortDesc)
		require.NoError(t, err)
		require.Len(t, page.Items, 1)
		assert.Equal(t, 102, page.Items[0].UserID)
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
			err := repo.Create(ctx, 101, f)
			require.NoError(t, err)
		}

		// Get all feeds (owner=user 101)
		allFeeds, err := repo.GetAll(ctx, 101, "", 1, 100, SortDesc)
		require.NoError(t, err)
		assert.Len(t, allFeeds.Items, 3)
		assert.Equal(t, 3, allFeeds.Total)
	})

	t.Run("GetAll_OtherUserSeesNothing", func(t *testing.T) {
		// Owner-scoping guard. A feed owned by user 101 must NOT
		// surface in user 102's listing — neither as items nor in the
		// count. Without the WHERE filter the test would see 1 item.
		_, repo := setupIntegrationTest(t)

		ownerFeed := &Feed{Title: "Not yours", Rating: 5.0}
		require.NoError(t, repo.Create(ctx, 101, ownerFeed))

		u2Page, err := repo.GetAll(ctx, 102, "", 1, 100, SortDesc)
		require.NoError(t, err)
		assert.Empty(t, u2Page.Items)
		assert.Equal(t, 0, u2Page.Total)

		u1Page, err := repo.GetAll(ctx, 101, "", 1, 100, SortDesc)
		require.NoError(t, err)
		require.Len(t, u1Page.Items, 1)
		assert.Equal(t, "Not yours", u1Page.Items[0].Title)
	})

	t.Run("GetAllFeedsAscReturnsOldestFirst", func(t *testing.T) {
		// Sleep between inserts so created_at strictly increases.
		// Serial inserts within the same microsecond would tie and
		// fall back to the id tiebreaker — same outcome for ASC vs
		// DESC, but the explicit sleep makes the test self-documenting.
		_, repo := setupIntegrationTest(t)

		first := &Feed{Title: "Oldest", Description: "first", Rating: 5.0}
		require.NoError(t, repo.Create(ctx, 101, first))
		time.Sleep(10 * time.Millisecond)
		mid := &Feed{Title: "Middle", Description: "second", Rating: 6.0}
		require.NoError(t, repo.Create(ctx, 101, mid))
		time.Sleep(10 * time.Millisecond)
		last := &Feed{Title: "Newest", Description: "third", Rating: 7.0}
		require.NoError(t, repo.Create(ctx, 101, last))

		asc, err := repo.GetAll(ctx, 101, "", 1, 100, SortAsc)
		require.NoError(t, err)
		require.Len(t, asc.Items, 3)
		assert.Equal(t, "Oldest", asc.Items[0].Title)
		assert.Equal(t, "Middle", asc.Items[1].Title)
		assert.Equal(t, "Newest", asc.Items[2].Title)
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
			err := repo.Create(ctx, 101, f)
			require.NoError(t, err)
		}

		// Search by title
		results, err := repo.GetAll(ctx, 101, "matrix", 1, 20, SortDesc)
		require.NoError(t, err)
		assert.Len(t, results.Items, 1)
		assert.Equal(t, "The Matrix", results.Items[0].Title)

		// Search by description
		results, err = repo.GetAll(ctx, 101, "thriller", 1, 20, SortDesc)
		require.NoError(t, err)
		assert.Len(t, results.Items, 1)
		assert.Equal(t, "Inception", results.Items[0].Title)

		// Search with no matches
		results, err = repo.GetAll(ctx, 101, "nonexistent", 1, 20, SortDesc)
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
		err := repo.Create(ctx, 101, feed)
		require.NoError(t, err)
		originalID := feed.ID

		// Update the feed
		updated := &Feed{
			Title:       "Updated Title",
			Description: "Updated description",
			Rating:      9.5,
		}

		err = repo.Update(ctx, 101, originalID, updated)
		require.NoError(t, err)

		// Verify update
		allFeeds, err := repo.GetAll(ctx, 101, "", 1, 100, SortDesc)
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

		err := repo.Update(ctx, 101, 999, feed)
		assert.Error(t, err)
		assert.ErrorIs(t, err, ErrNotFound)
	})

	t.Run("Update_OtherUserReturnsNotFound", func(t *testing.T) {
		// Cross-owner PUT must return ErrNotFound — same path as
		// the missing-row case. No leak, no ErrForbidden.
		_, repo := setupIntegrationTest(t)

		ownerFeed := &Feed{Title: "Mine", Rating: 7.0}
		require.NoError(t, repo.Create(ctx, 101, ownerFeed))

		err := repo.Update(ctx, 102, ownerFeed.ID, &Feed{Title: "Hijack", Rating: 9.9})
		assert.Error(t, err)
		assert.ErrorIs(t, err, ErrNotFound)

		// The row was not modified.
		all, err := repo.GetAll(ctx, 101, "", 1, 100, SortDesc)
		require.NoError(t, err)
		require.Len(t, all.Items, 1)
		assert.Equal(t, "Mine", all.Items[0].Title)
		assert.Equal(t, 7.0, all.Items[0].Rating)
	})

	t.Run("DeleteFeed", func(t *testing.T) {
		_, repo := setupIntegrationTest(t)

		// Create a feed
		feed := &Feed{
			Title:  "To Delete",
			Rating: 7.0,
		}
		err := repo.Create(ctx, 101, feed)
		require.NoError(t, err)

		// Delete the feed
		err = repo.Delete(ctx, 101, feed.ID)
		require.NoError(t, err)

		// Verify soft delete (feed should not appear in results)
		allFeeds, err := repo.GetAll(ctx, 101, "", 1, 100, SortDesc)
		require.NoError(t, err)
		assert.Empty(t, allFeeds.Items)
	})

	t.Run("DeleteFeedNotFound", func(t *testing.T) {
		_, repo := setupIntegrationTest(t)

		err := repo.Delete(ctx, 101, 999)
		assert.Error(t, err)
		assert.ErrorIs(t, err, ErrNotFound)
	})

	t.Run("Delete_OtherUserReturnsNotFound", func(t *testing.T) {
		// Cross-owner DELETE returns ErrNotFound — RowsAffected=0
		// because the SQL WHERE filters on user_id. Same 404 as the
		// missing-id case.
		_, repo := setupIntegrationTest(t)

		ownerFeed := &Feed{Title: "Stays", Rating: 7.0}
		require.NoError(t, repo.Create(ctx, 101, ownerFeed))

		err := repo.Delete(ctx, 102, ownerFeed.ID)
		assert.Error(t, err)
		assert.ErrorIs(t, err, ErrNotFound)

		// The row was not deleted.
		all, err := repo.GetAll(ctx, 101, "", 1, 100, SortDesc)
		require.NoError(t, err)
		assert.Len(t, all.Items, 1)
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
		err = txRepo.Create(ctx, 101, feed)
		require.NoError(t, err)

		// Rollback
		require.NoError(t, tx.Rollback(ctx))

		// Verify feed was not created
		allFeeds, err := repo.GetAll(ctx, 101, "", 1, 100, SortDesc)
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
		err = txRepo.Create(ctx, 101, feed)
		require.NoError(t, err)

		// Commit
		require.NoError(t, tx.Commit(ctx))

		// Verify feed was created
		allFeeds, err := repo.GetAll(ctx, 101, "", 1, 100, SortDesc)
		require.NoError(t, err)
		assert.Len(t, allFeeds.Items, 1)
		assert.Equal(t, "Transaction Commit Test", allFeeds.Items[0].Title)
	})
}