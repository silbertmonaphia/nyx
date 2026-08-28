// Package database opens the application's Postgres connection pool and
// applies the SQL migrations on startup. The pool is a *pgxpool.Pool; the
// per-feature repositories (movie, user) construct a sqlc Querier on top
// of it.
package database

import (
	"context"
	"fmt"
	"math"
	"time"

	"nyx/internal/platform/config"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog/log"
)

// New opens a pgx connection pool against cfg.DBURL and pings it. The
// retry loop tolerates a Postgres container that is still booting
// during local development; the first successful ping returns.
//
// tracer is an optional pgx.QueryTracer that emits one OTel span per
// Query / QueryRow / Exec call. Pass nil when tracing is disabled —
// pgxpool takes its existing fast path with zero overhead.
//
// Pool tuning fields:
//   - DBMaxOpenConns    -> MaxConns
//   - DBMaxIdleConns    -> MinConns (pgxpool has no MaxIdleConns equivalent;
//     MinConns is the closest fit and keeps that many connections warm)
//   - DBConnMaxLifetime -> MaxConnLifetime
//   - DBConnMaxIdleTime -> MaxConnIdleTime
func New(ctx context.Context, cfg *config.Config, tracer pgx.QueryTracer) (*pgxpool.Pool, error) {
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
			poolCfg.MaxConns = poolConns(cfg.DBMaxOpenConns)
			poolCfg.MinConns = poolConns(cfg.DBMaxIdleConns)
			poolCfg.MaxConnLifetime = maxLifetime
			poolCfg.MaxConnIdleTime = maxIdleTime
			poolCfg.ConnConfig.Tracer = tracer

			pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
			if err != nil {
				lastErr = err
			} else if err := pool.Ping(ctx); err == nil {
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

// RunMigrations applies SQL migrations from migrationPath to the database
// at dbURL. The path is supplied by the caller (typically
// cfg.MigrationPath from main.go) — RunMigrations no longer reads
// MIGRATION_PATH from the environment. Returns errors instead of
// exiting the process, so the caller can decide startup-failure
// handling (fail fast at startup, retry, etc.).
//
// It is independent of the application's DB driver: golang-migrate
// opens its own short-lived connection using dbURL.
func RunMigrations(dbURL, migrationPath string) error {
	if migrationPath == "" {
		return fmt.Errorf("migrations: migration path is required")
	}

	m, err := migrate.New(migrationPath, dbURL)
	if err != nil {
		return fmt.Errorf("migrations: create instance: %w", err)
	}
	defer func() {
		serr, derr := m.Close()
		if serr != nil {
			log.Warn().Err(serr).Msg("migration source close error")
		}
		if derr != nil {
			log.Warn().Err(derr).Msg("migration db close error")
		}
	}()

	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		return fmt.Errorf("migrations: apply: %w", err)
	}

	log.Info().Str("path", migrationPath).Msg("Database migrations applied successfully")
	return nil
}

// poolConns clamps an operator-supplied pool-size int down to the
// int32 range pgxpool.Config expects. DB_MAX_OPEN_CONNS /
// DB_MAX_IDLE_CONNS are env-driven and could in principle be set above
// math.MaxInt32; pool sizing above a few hundred is never sensible, so
// the clamp simply caps the value at the type's max and lets pgxpool
// enforce its own min-conns-vs-max-conns constraint.
//
//nolint:gosec // G115: the explicit math.MaxInt32 clamp above proves the cast is safe.
func poolConns(v int) int32 {
	if v > math.MaxInt32 {
		return math.MaxInt32
	}
	return int32(v)
}
