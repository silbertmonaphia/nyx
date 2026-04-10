package movie

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"nyx/test"

	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	testDB *test.TestDB
	dbURL  string
)

func TestMain(m *testing.M) {
	// Set up the database container once for all tests in this package
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

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

	// Run tests
	code := m.Run()

	// Cleanup
	if testDB.Container != nil {
		_ = testDB.Container.Terminate(context.Background())
	}

	os.Exit(code)
}

func setupIntegrationTest(t *testing.T) (*sqlx.DB, Repository) {
	t.Helper()

	// Create database connection using the shared container
	db, err := sqlx.Open("postgres", dbURL)
	require.NoError(t, err)

	// Clean up tables before each test to ensure isolation
	_, err = db.Exec("DELETE FROM movies")
	require.NoError(t, err)

	// Configure connection pool for tests
	db.SetMaxOpenConns(5)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(2 * time.Minute)

	t.Cleanup(func() {
		db.Close()
	})

	repo := NewRepository(db)
	return db, repo
}

func TestRepositoryIntegration(t *testing.T) {
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
		allMovies, err := repo.GetAll(ctx, "")
		require.NoError(t, err)
		assert.Len(t, allMovies, 3)
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
		results, err := repo.GetAll(ctx, "matrix")
		require.NoError(t, err)
		assert.Len(t, results, 1)
		assert.Equal(t, "The Matrix", results[0].Title)

		// Search by description
		results, err = repo.GetAll(ctx, "thriller")
		require.NoError(t, err)
		assert.Len(t, results, 1)
		assert.Equal(t, "Inception", results[0].Title)

		// Search with no matches
		results, err = repo.GetAll(ctx, "nonexistent")
		require.NoError(t, err)
		assert.Empty(t, results)
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
		allMovies, err := repo.GetAll(ctx, "")
		require.NoError(t, err)
		assert.Len(t, allMovies, 1)
		assert.Equal(t, "Updated Title", allMovies[0].Title)
		assert.Equal(t, "Updated description", allMovies[0].Description)
		assert.Equal(t, 9.5, allMovies[0].Rating)
	})

	t.Run("UpdateMovieNotFound", func(t *testing.T) {
		_, repo := setupIntegrationTest(t)

		movie := &Movie{
			Title:  "Test",
			Rating: 7.0,
		}

		err := repo.Update(ctx, 999, movie)
		assert.Error(t, err)
		assert.Equal(t, "movie not found", err.Error())
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
		allMovies, err := repo.GetAll(ctx, "")
		require.NoError(t, err)
		assert.Empty(t, allMovies)
	})

	t.Run("DeleteMovieNotFound", func(t *testing.T) {
		_, repo := setupIntegrationTest(t)

		err := repo.Delete(ctx, 999)
		assert.Error(t, err)
		assert.Equal(t, "movie not found", err.Error())
	})

	t.Run("Ping", func(t *testing.T) {
		db, repo := setupIntegrationTest(t)

		err := repo.Ping(ctx)
		require.NoError(t, err)

		// Verify it's the actual database connection
		assert.NotNil(t, db)
	})
}

func TestRepositoryWithTransactions(t *testing.T) {
	ctx := context.Background()

	t.Run("TransactionRollback", func(t *testing.T) {
		db, repo := setupIntegrationTest(t)

		// Start transaction
		tx, err := db.BeginTxx(ctx, nil)
		require.NoError(t, err)

		// Get repository with transaction
		txRepo := repo.WithTx(tx)

		// Create a movie in transaction
		movie := &Movie{
			Title:  "Transaction Test",
			Rating: 8.0,
		}
		err = txRepo.Create(ctx, movie)
		require.NoError(t, err)

		// Rollback
		tx.Rollback()

		// Verify movie was not created
		allMovies, err := repo.GetAll(ctx, "")
		require.NoError(t, err)
		assert.Empty(t, allMovies)
	})

	t.Run("TransactionCommit", func(t *testing.T) {
		db, repo := setupIntegrationTest(t)

		// Start transaction
		tx, err := db.BeginTxx(ctx, nil)
		require.NoError(t, err)

		// Get repository with transaction
		txRepo := repo.WithTx(tx)

		// Create a movie in transaction
		movie := &Movie{
			Title:  "Transaction Commit Test",
			Rating: 8.5,
		}
		err = txRepo.Create(ctx, movie)
		require.NoError(t, err)

		// Commit
		tx.Commit()

		// Verify movie was created
		allMovies, err := repo.GetAll(ctx, "")
		require.NoError(t, err)
		assert.Len(t, allMovies, 1)
		assert.Equal(t, "Transaction Commit Test", allMovies[0].Title)
	})
}
