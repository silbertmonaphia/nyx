package test

import (
	"context"

	"github.com/jackc/pgx/v5"
)

const sharedIntegrationLockKey int64 = 0x6e7978

func AcquireIntegrationLock(ctx context.Context, url string) (func(), error) {
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return nil, err
	}
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", sharedIntegrationLockKey); err != nil {
		_ = conn.Close(context.Background())
		return nil, err
	}
	return func() {
		_, _ = conn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", sharedIntegrationLockKey)
		_ = conn.Close(context.Background())
	}, nil
}
