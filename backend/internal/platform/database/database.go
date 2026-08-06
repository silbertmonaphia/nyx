// Package database opens the application's Postgres connection pool and
// applies the SQL migrations on startup. The pool is a *pgxpool.Pool; the
// per-feature repositories (movie, user) construct a sqlc Querier on top
// of it.
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
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog/log"
)

// New opens a pgx connection pool against cfg.DBURL and pings it. The
// retry loop tolerates a Postgres container that is still booting
// during local development; the first successful ping returns.
//
// Pool tuning maps the previous *sqlx.DB settings:
//   - DBMaxOpenConns    -> MaxConns
//   - DBMaxIdleConns    -> MinConns (pgxpool has no MaxIdleConns equivalent;
//     MinConns is the closest fit and keeps that many connections warm)
//   - DBConnMaxLifetime -> MaxConnLifetime
//   - DBConnMaxIdleTime -> MaxConnIdleTime
func New(cfg *config.Config) (*pgxpool.Pool, error) {
	maxLifetime, err := time.ParseDuration(cfg.DBConnMaxLifetime)
	if err != nil {
		return nil, fmt.Errorf("invalid DB_CONN_MAX_LIFETIME: %w", err)
	}
	maxIdleTime, err := time.ParseDuration(cfg.DBConnMaxIdleTime)
	if err != nil {
		return nil, fmt.Errorf("invalid DB_CONN_MAX_IDLE_TIME: %w", err)
	}

	var lastErr error
	for i := 0; i < 10; i++ {
		poolCfg, err := pgxpool.ParseConfig(cfg.DBURL)
		if err != nil {
			lastErr = err
		} else {
			poolCfg.MaxConns = int32(cfg.DBMaxOpenConns)
			poolCfg.MinConns = int32(cfg.DBMaxIdleConns)
			poolCfg.MaxConnLifetime = maxLifetime
			poolCfg.MaxConnIdleTime = maxIdleTime

			pool, err := pgxpool.NewWithConfig(context.Background(), poolCfg)
			if err != nil {
				lastErr = err
			} else if err := pool.Ping(context.Background()); err == nil {
				log.Info().Msg("Successfully connected to the database")
				return pool, nil
			} else {
				lastErr = err
				pool.Close()
			}
		}
		log.Warn().
			Int("attempt", i+1).
			Int("max_attempts", 10).
			Err(lastErr).
			Msg("Could not connect to DB")
		time.Sleep(3 * time.Second)
	}

	return nil, fmt.Errorf("connect to db after retries: %w", lastErr)
}

// RunMigrations applies the SQL migrations under migrations/. It is
// independent of the application's DB driver: golang-migrate opens its
// own short-lived connection using the same URL, so swapping the app
// from lib/pq to pgx does not affect this path.
func RunMigrations(dbURL string) {
	migrationPath := os.Getenv("MIGRATION_PATH")
	if migrationPath == "" {
		migrationPath = "file://migrations"
	}

	m, err := migrate.New(migrationPath, dbURL)
	if err != nil {
		log.Fatal().Err(err).Msg("Could not create migration instance")
	}

	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		log.Fatal().Err(err).Msg("An error occurred while running migrations")
	}

	log.Info().Msg("Database migrations applied successfully")
}
