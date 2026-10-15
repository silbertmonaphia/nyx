package user

import (
	"context"
	"time"

	"github.com/rs/zerolog/log"
)

// CleanupTicker is how often the background goroutine sweeps for
// refresh rows past the retention horizon. Long enough that a single
// run isn't noisy; short enough that a deploy's first sweep doesn't
// have to wait a day.
const CleanupTicker = 1 * time.Hour

// StartRefreshCleanup launches a background goroutine that
// periodically deletes revoked / expired refresh rows older than
// RefreshTokenRetention. Returns a stop function the caller
// (main.go) defers to stop the goroutine on SIGTERM so the process
// doesn't leak the goroutine across graceful shutdown.
//
// Errors are logged at Warn and swallowed: a failed sweep simply
// retries on the next tick. The cleanup DELETE filters on
// `created_at < cutoff` plus `(revoked_at IS NOT NULL OR
// expires_at < now())`; with a small retained-row set (per-user
// cap + 7d retention) Postgres can seq-scan the partial-index
// region cheaply. See SECURITY.md M2.
func StartRefreshCleanup(repo Repository) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		defer close(done)
		ticker := time.NewTicker(CleanupTicker)
		defer ticker.Stop()

		// Run once immediately so a freshly-deployed instance
		// catches up without waiting a full interval.
		sweep(ctx, repo)

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				sweep(ctx, repo)
			}
		}
	}()

	return func() {
		cancel()
		<-done
	}
}

func sweep(ctx context.Context, repo Repository) {
	cutoff := time.Now().Add(-RefreshTokenRetention)
	n, err := repo.PurgeRefreshTokensOlderThan(ctx, cutoff)
	if err != nil {
		log.Warn().Err(err).Msg("refresh-token cleanup sweep failed; will retry")
		return
	}
	if n > 0 {
		log.Info().Int64("rows_deleted", n).Msg("refresh-token cleanup swept stale rows")
	}
}
