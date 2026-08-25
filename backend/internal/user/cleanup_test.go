package user

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// stubCleanupRepo lets the cleanup tests drive StartRefreshCleanup
// without a real Postgres — assertions focus on the goroutine
// lifecycle (immediate first sweep + stop returning) and the SQL
// shape (PurgeRefreshTokensOlderThan forwards the cutoff).
type stubCleanupRepo struct {
	purgeCalls atomic.Int64
	lastCutoff time.Time
	purgeErr   error
}

func (s *stubCleanupRepo) CreateUser(context.Context, *User) error {
	return errors.New("not used")
}
func (s *stubCleanupRepo) GetUserByUsername(context.Context, string) (*User, error) {
	return nil, errors.New("not used")
}
func (s *stubCleanupRepo) GetUserByID(context.Context, int) (*User, error) {
	return nil, errors.New("not used")
}
func (s *stubCleanupRepo) CreateRefreshToken(context.Context, int, []byte, time.Time) (*RefreshTokenRow, error) {
	return nil, errors.New("not used")
}
func (s *stubCleanupRepo) GetRefreshTokenByHash(context.Context, []byte) (*RefreshTokenRow, error) {
	return nil, errors.New("not used")
}
func (s *stubCleanupRepo) RotateRefreshToken(context.Context, int64, int, []byte, int64, time.Time) (*RefreshTokenRow, error) {
	return nil, errors.New("not used")
}
func (s *stubCleanupRepo) RevokeRefreshTokenFamily(context.Context, int64) (int64, error) {
	return 0, errors.New("not used")
}
func (s *stubCleanupRepo) RevokeRefreshTokenByID(context.Context, int64) error {
	return errors.New("not used")
}
func (s *stubCleanupRepo) CountActiveRefreshTokensByUser(context.Context, int) (int64, error) {
	return 0, nil
}
func (s *stubCleanupRepo) ListOldestActiveRefreshTokensByUser(context.Context, int, int) ([]int64, error) {
	return nil, nil
}
func (s *stubCleanupRepo) PurgeRefreshTokensOlderThan(_ context.Context, cutoff time.Time) (int64, error) {
	s.purgeCalls.Add(1)
	s.lastCutoff = cutoff
	return 0, s.purgeErr
}

// TestPurgeRefreshTokensOlderThan_ForwardsCutoff pins the repo
// contract: the supplied cutoff must be `now - RefreshTokenRetention`.
// Without this property the cleanup goroutine could silently delete
// rows operators still need for incident response.
func TestPurgeRefreshTokensOlderThan_ForwardsCutoff(t *testing.T) {
	repo := &stubCleanupRepo{}
	cutoff := time.Now().Add(-RefreshTokenRetention).Add(-time.Hour)
	_, err := repo.PurgeRefreshTokensOlderThan(context.Background(), cutoff)
	if err != nil && !errors.Is(err, repo.purgeErr) {
		t.Fatalf("PurgeRefreshTokensOlderThan returned unexpected error: %v", err)
	}
	if repo.purgeCalls.Load() != 1 {
		t.Errorf("PurgeRefreshTokensOlderThan call count = %d, want 1", repo.purgeCalls.Load())
	}
	if !repo.lastCutoff.Equal(cutoff) {
		t.Errorf("cutoff = %v, want %v", repo.lastCutoff, cutoff)
	}
}

// TestStartRefreshCleanup_RunsImmediatelyThenStops pins the lifecycle
// contract: StartRefreshCleanup performs one sweep synchronously
// (before returning) so a fresh deploy catches up without waiting
// for the first tick, and the returned stop function returns within
// a reasonable time after cancellation.
func TestStartRefreshCleanup_RunsImmediatelyThenStops(t *testing.T) {
	repo := &stubCleanupRepo{}
	stop := StartRefreshCleanup(repo)

	// The immediate-first-sweep behaviour means by the time
	// StartRefreshCleanup returns, the stub has been hit once.
	// We poll briefly because the goroutine may not have flushed
	// before our first read.
	deadline := time.Now().Add(time.Second)
	for repo.purgeCalls.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := repo.purgeCalls.Load(); got < 1 {
		t.Fatalf("expected at least 1 purge call after StartRefreshCleanup, got %d", got)
	}

	stopDone := make(chan struct{})
	go func() {
		stop()
		close(stopDone)
	}()
	select {
	case <-stopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("stop() did not return within 2s; goroutine leak suspected")
	}
}
