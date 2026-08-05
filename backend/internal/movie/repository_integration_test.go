package movie

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"nyx/internal/movie/db"
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
	_, err = pool.Exec(context.Background(), "DELETE FROM movies")
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

	t.Run("CreateMovie", func(t *testing.T) {
		_, repo := setupIntegrationTest(t)

		movie := &Movie{
			Title:       "The Matrix",
			Description: "A computer hacker learns about the true nature of reality",
			Rating:      8.7,
		}

		err := repo.Create(ctx, movie)
		require.NoError(t, err)
		assert.NotZero(t, movie.ID)
		assert.NotZero(t, movie.CreatedAt)
		assert.NotZero(t, movie.UpdatedAt)
	})

	t.Run("GetAllMovies", func(t *testing.T) {
		_, repo := setupIntegrationTest(t)

		// Create test movies
		movies := []*Movie{
			{Title: "Movie 1", Description: "Description 1", Rating: 7.5},
			{Title: "Movie 2", Description: "Description 2", Rating: 8.0},
			{Title: "Movie 3", Description: "Description 3", Rating: 9.0},
		}

		for _, m := range movies {
			err := repo.Create(ctx, m)
			require.NoError(t, err)
		}

		// Get all movies
		allMovies, err := repo.GetAll(ctx, "", 1, 100)
		require.NoError(t, err)
		assert.Len(t, allMovies.Items, 3)
		assert.Equal(t, 3, allMovies.Total)
	})

	t.Run("SearchMovies", func(t *testing.T) {
		_, repo := setupIntegrationTest(t)

		// Create test movies
		movies := []*Movie{
			{Title: "The Matrix", Description: "Sci-fi action", Rating: 8.7},
			{Title: "Inception", Description: "Mind-bending thriller", Rating: 8.8},
			{Title: "Interstellar", Description: "Space exploration", Rating: 8.6},
		}

		for _, m := range movies {
			err := repo.Create(ctx, m)
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

	t.Run("UpdateMovie", func(t *testing.T) {
		_, repo := setupIntegrationTest(t)

		// Create a movie
		movie := &Movie{
			Title:       "Original Title",
			Description: "Original description",
			Rating:      7.0,
		}
		err := repo.Create(ctx, movie)
		require.NoError(t, err)
		originalID := movie.ID

		// Update the movie
		updated := &Movie{
			Title:       "Updated Title",
			Description: "Updated description",
			Rating:      9.5,
		}

		err = repo.Update(ctx, originalID, updated)
		require.NoError(t, err)

		// Verify update
		allMovies, err := repo.GetAll(ctx, "", 1, 100)
		require.NoError(t, err)
		assert.Len(t, allMovies.Items, 1)
		assert.Equal(t, "Updated Title", allMovies.Items[0].Title)
		assert.Equal(t, "Updated description", allMovies.Items[0].Description)
		assert.Equal(t, 9.5, allMovies.Items[0].Rating)
	})

	t.Run("UpdateMovieNotFound", func(t *testing.T) {
		_, repo := setupIntegrationTest(t)

		movie := &Movie{
			Title:  "Test",
			Rating: 7.0,
		}

		err := repo.Update(ctx, 999, movie)
		assert.Error(t, err)
		assert.ErrorIs(t, err, ErrNotFound)
	})

	t.Run("DeleteMovie", func(t *testing.T) {
		_, repo := setupIntegrationTest(t)

		// Create a movie
		movie := &Movie{
			Title:  "To Delete",
			Rating: 7.0,
		}
		err := repo.Create(ctx, movie)
		require.NoError(t, err)

		// Delete the movie
		err = repo.Delete(ctx, movie.ID)
		require.NoError(t, err)

		// Verify soft delete (movie should not appear in results)
		allMovies, err := repo.GetAll(ctx, "", 1, 100)
		require.NoError(t, err)
		assert.Empty(t, allMovies.Items)
	})

	t.Run("DeleteMovieNotFound", func(t *testing.T) {
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

		// Create a movie in transaction
		movie := &Movie{
			Title:  "Transaction Test",
			Rating: 8.0,
		}
		err = txRepo.Create(ctx, movie)
		require.NoError(t, err)

		// Rollback
		require.NoError(t, tx.Rollback(ctx))

		// Verify movie was not created
		allMovies, err := repo.GetAll(ctx, "", 1, 100)
		require.NoError(t, err)
		assert.Empty(t, allMovies.Items)
	})

	t.Run("TransactionCommit", func(t *testing.T) {
		pool, repo := setupIntegrationTest(t)

		// Start transaction
		tx, err := pool.Begin(ctx)
		require.NoError(t, err)

		// Get repository with transaction
		txRepo := NewRepositoryFromQuerier(db.New(pool).WithTx(tx))

		// Create a movie in transaction
		movie := &Movie{
			Title:  "Transaction Commit Test",
			Rating: 8.5,
		}
		err = txRepo.Create(ctx, movie)
		require.NoError(t, err)

		// Commit
		require.NoError(t, tx.Commit(ctx))

		// Verify movie was created
		allMovies, err := repo.GetAll(ctx, "", 1, 100)
		require.NoError(t, err)
		assert.Len(t, allMovies.Items, 1)
		assert.Equal(t, "Transaction Commit Test", allMovies.Items[0].Title)
	})
}
