package database

import (
	"context"
	"fmt"
	"os"
	"time"

	"nyx/internal/platform/config"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq"
	"github.com/rs/zerolog/log"
)

func New(cfg *config.Config) (*sqlx.DB, error) {
	var db *sqlx.DB
	var err error

	dbURL := cfg.DBURL

	// Parse durations
	maxLifetime, err := time.ParseDuration(cfg.DBConnMaxLifetime)
	if err != nil {
		return nil, fmt.Errorf("invalid DB_CONN_MAX_LIFETIME: %w", err)
	}
	maxIdleTime, err := time.ParseDuration(cfg.DBConnMaxIdleTime)
	if err != nil {
		return nil, fmt.Errorf("invalid DB_CONN_MAX_IDLE_TIME: %w", err)
	}

	// Retry loop for database connection
	for i := 0; i < 10; i++ {
		db, err = sqlx.Open("postgres", dbURL)
		if err == nil {
			// Configure connection pool
			db.SetMaxOpenConns(cfg.DBMaxOpenConns)
			db.SetMaxIdleConns(cfg.DBMaxIdleConns)
			db.SetConnMaxLifetime(maxLifetime)
			db.SetConnMaxIdleTime(maxIdleTime)

			err = db.Ping()
			if err == nil {
				log.Info().Msg("Successfully connected to the database")
				return db, nil
			}
		}
		log.Warn().
			Int("attempt", i+1).
			Int("max_attempts", 10).
			Err(err).
			Msg("Could not connect to DB")
		time.Sleep(3 * time.Second)
	}

	return nil, err
}

func BeginTx(ctx context.Context, db *sqlx.DB) (*sqlx.Tx, error) {
	return db.BeginTxx(ctx, nil)
}

func RunMigrations(dbURL string) {
	// Look for migrations in the migrations folder relative to the project root
	// Since the binary might be in cmd/api/main.go, we might need to adjust the path
	// but usually, it's run from the backend directory in Docker.
	migrationPath := os.Getenv("MIGRATION_PATH")
	if migrationPath == "" {
		migrationPath = "file://migrations"
	}

	m, err := migrate.New(
		migrationPath,
		dbURL,
	)
	if err != nil {
		log.Fatal().Err(err).Msg("Could not create migration instance")
	}

	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		log.Fatal().Err(err).Msg("An error occurred while running migrations")
	}

	log.Info().Msg("Database migrations applied successfully")
}
