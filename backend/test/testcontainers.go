package test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// TestDB holds the PostgreSQL container and connection details
type TestDB struct {
	Container *postgres.PostgresContainer
	DBURL     string
}

// StartPostgres starts a PostgreSQL container and returns the TestDB instance.
// It does not use t.Cleanup so the caller is responsible for termination.
func StartPostgres(ctx context.Context) (*TestDB, error) {
	container, err := postgres.Run(ctx,
		"postgres:17-alpine",
		postgres.WithDatabase("nyx_test"),
		postgres.WithUsername("postgres"),
		postgres.WithPassword("postgres"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(1).
				WithStartupTimeout(60 * time.Second),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to start PostgreSQL container: %w", err)
	}

	dbURL, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		_ = container.Terminate(ctx)
		return nil, fmt.Errorf("failed to get connection string: %w", err)
	}

	return &TestDB{
		Container: container,
		DBURL:     dbURL,
	}, nil
}

// SetupPostgres starts a PostgreSQL container for testing and registers cleanup.
func SetupPostgres(t *testing.T) *TestDB {
	t.Helper()

	ctx := context.Background()
	tdb, err := StartPostgres(ctx)
	if err != nil {
		t.Fatalf("SetupPostgres failed: %v", err)
	}

	// Register cleanup
	t.Cleanup(func() {
		_ = tdb.Container.Terminate(ctx)
	})

	return tdb
}

// RunMigrationsWithContext applies database migrations to the test database.
func (tdb *TestDB) RunMigrationsWithContext(ctx context.Context) error {
	return runMigrations(ctx, tdb.DBURL)
}

// RunMigrations applies database migrations and fails the test if it fails.
func (tdb *TestDB) RunMigrations(t *testing.T) {
	t.Helper()

	err := tdb.RunMigrationsWithContext(context.Background())
	if err != nil {
		t.Fatalf("Failed to run migrations: %v", err)
	}
}

func runMigrations(ctx context.Context, dbURL string) error {
	// Use golang-migrate to run migrations
	// We need to find the migrations path relative to the test
	migrationPath := "file://../migrations"

	m, err := migrate.New(migrationPath, dbURL)
	if err != nil {
		return fmt.Errorf("could not create migration instance: %w", err)
	}
	defer m.Close()

	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		return fmt.Errorf("migration failed: %w", err)
	}

	return nil
}
