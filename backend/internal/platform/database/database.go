package database

import (
	"database/sql"
	"os"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	_ "github.com/lib/pq"
	"github.com/rs/zerolog/log"
)

func New(dbURL string) (*sql.DB, error) {
	var db *sql.DB
	var err error

	// Retry loop for database connection
	for i := 0; i < 10; i++ {
		db, err = sql.Open("postgres", dbURL)
		if err == nil {
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
