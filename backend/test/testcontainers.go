package test

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
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
	// Use runtime.Caller to find the location of this file and resolve migrations path.
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		return fmt.Errorf("failed to get caller information")
	}

	// This file is in backend/test/testcontainers.go
	// Migrations are in backend/migrations
	testDir := filepath.Dir(filename)
	migrationsPath := filepath.Join(testDir, "..", "migrations")

	// Use golang-migrate to run migrations
	migrationPath := "file://" + migrationsPath

	m, err := migrate.New(migrationPath, dbURL)
	if err != nil {
		return fmt.Errorf("could not create migration instance at %s: %w", migrationPath, err)
	}
	defer m.Close()

	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		return fmt.Errorf("migration failed: %w", err)
	}

	return nil
}
