package idempotency

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"nyx/internal/idempotency/db"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/rs/zerolog/log"
)

const DefaultRetention = 7 * 24 * time.Hour

var ErrKeyReused = errors.New("idempotency: key already used for a different tool")

type Pool interface {
	db.DBTX
	Begin(ctx context.Context) (pgx.Tx, error)
}

type Store struct {
	pool      Pool
	retention time.Duration
}

func NewStore(pool Pool, retention time.Duration) *Store {
	if retention <= 0 {
		retention = DefaultRetention
	}
	return &Store{pool: pool, retention: retention}
}

func (s *Store) Do(
	ctx context.Context,
	ownerID int64,
	key string,
	tool string,
	out any,
	fn func(pgx.Tx) (any, error),
) (bool, error) {
	if out == nil {
		return false, errors.New("idempotency: out must not be nil")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("idempotency: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := db.New(tx)
	inserted, err := q.InsertToolIdempotency(ctx, db.InsertToolIdempotencyParams{
		OwnerID: ownerID,
		Key:     key,
		Tool:    tool,
	})
	if err != nil {
		return false, fmt.Errorf("idempotency: insert: %w", err)
	}

	if inserted == 0 {
		row, readErr := q.GetToolIdempotencyResponse(ctx, db.GetToolIdempotencyResponseParams{
			OwnerID: ownerID,
			Key:     key,
		})
		if readErr != nil {
			return false, fmt.Errorf("idempotency: read response: %w", readErr)
		}
		if row.Tool != tool {
			return false, ErrKeyReused
		}
		if decodeErr := json.Unmarshal(row.Response, out); decodeErr != nil {
			return false, fmt.Errorf("idempotency: decode stored response: %w", decodeErr)
		}
		return true, nil
	}

	result, err := fn(tx)
	if err != nil {
		return false, err
	}
	payload, err := json.Marshal(result)
	if err != nil {
		return false, fmt.Errorf("idempotency: encode response: %w", err)
	}
	if err := q.UpdateToolIdempotencyResponse(ctx, db.UpdateToolIdempotencyResponseParams{
		OwnerID:  ownerID,
		Key:      key,
		Response: payload,
	}); err != nil {
		return false, fmt.Errorf("idempotency: update response: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("idempotency: commit: %w", err)
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return false, fmt.Errorf("idempotency: decode response: %w", err)
	}
	return false, nil
}

func (s *Store) DeleteExpired(ctx context.Context) (int64, error) {
	cutoff := pgtype.Timestamptz{Time: time.Now().Add(-s.retention), Valid: true}
	return db.New(s.pool).DeleteExpiredToolIdempotency(ctx, cutoff)
}

func (s *Store) RunRetention(interval time.Duration) func() {
	if interval <= 0 {
		interval = time.Hour
	}
	stop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				deleted, err := s.DeleteExpired(ctx)
				cancel()
				if err != nil {
					log.Warn().Err(err).Msg("tool idempotency retention failed")
					continue
				}
				if deleted > 0 {
					log.Info().Int64("deleted", deleted).Msg("tool idempotency retention swept")
				}
			}
		}
	}()
	return func() { close(stop) }
}
