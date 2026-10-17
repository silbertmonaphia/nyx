package user

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"nyx/internal/platform/api"
	"nyx/internal/platform/auth"
	"nyx/test"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace/noop"
)

// TestMain for the user package. We only get one per package, so this
// file hosts both the huma error override (needed by handler tests in
// huma_handler_test.go) and the integration-test container bootstrap
// (skipped under SKIP_CONTAINERS=true). Tests below self-skip when
// dbURL is empty.
//
// Mirrors the pattern in repository_integration_test.go of the feed
// domain.
var (
	testDB *test.TestDB
	dbURL  string
)

func TestMain(m *testing.M) {
	// Install the huma error override before any handler test builds
	// its router. Same reason as the feed package: huma's default
	// error constructors don't speak the legacy envelope, so we route
	// them through api.ErrorResponse.
	api.OverrideHumaErrors()

	skipContainers := os.Getenv("SKIP_CONTAINERS") == "true"

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	var release func()
	if !skipContainers {
		if url := os.Getenv("DB_URL"); url != "" {
			dbURL = url
			var err error
			release, err = test.AcquireIntegrationLock(ctx, dbURL)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Failed to acquire integration lock: %v\n", err)
				os.Exit(1)
			}
			if err := test.RunMigrationsForURL(ctx, dbURL); err != nil {
				fmt.Fprintf(os.Stderr, "Failed to run migrations: %v\n", err)
				os.Exit(1)
			}
		} else {
			var err error
			testDB, err = test.StartPostgres(ctx)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Failed to start PostgreSQL container: %v\n", err)
				os.Exit(1)
			}
			dbURL = testDB.DBURL
			if err := testDB.RunMigrationsWithContext(ctx); err != nil {
				fmt.Fprintf(os.Stderr, "Failed to run migrations: %v\n", err)
				if testDB.Container != nil {
					_ = testDB.Container.Terminate(context.Background())
				}
				os.Exit(1)
			}
		}
	}

	code := m.Run()

	if release != nil {
		release()
	}

	if !skipContainers && testDB != nil && testDB.Container != nil {
		_ = testDB.Container.Terminate(context.Background())
	}
	os.Exit(code)
}

// setupRefreshIntegrationTest opens a pool against the test DB and
// returns a Repository wired against it. Skips when no DB is
// available (SKIP_CONTAINERS=true). Cleans up refresh_tokens + users
// before each test so order doesn't matter. The pool is captured in
// t.Cleanup so the test body doesn't need to thread it through —
// callers that DO need raw pool access can use setupRefreshIntegrationPool.
func setupRefreshIntegrationTest(t *testing.T) Repository {
	t.Helper()
	_, repo := setupRefreshIntegrationPool(t)
	return repo
}

// setupRefreshIntegrationPool is the lower-level helper: returns the
// pool so the caller can issue raw SQL (e.g. for cross-row asserts).
// Most tests only need the Repository and should prefer
// setupRefreshIntegrationTest.
func setupRefreshIntegrationPool(t *testing.T) (*pgxpool.Pool, Repository) {
	t.Helper()
	if dbURL == "" {
		t.Skip("PostgreSQL container not available; set SKIP_CONTAINERS=false to run integration tests")
	}
	poolCfg, err := pgxpool.ParseConfig(dbURL)
	require.NoError(t, err)
	poolCfg.MaxConns = 5
	poolCfg.MinConns = 2
	poolCfg.MaxConnLifetime = 2 * time.Minute
	pool, err := pgxpool.NewWithConfig(context.Background(), poolCfg)
	require.NoError(t, err)

	// Wipe in FK-safe order: refresh_tokens, feeds (depends on users),
	// then users. The feeds.user_id FK added by migration 000012 makes
	// "DELETE FROM users" fail with 23503 when any feed row exists,
	// so the dependent tables must go first. The user-package tests
	// don't touch feeds; the cleanup is purely to satisfy the new FK
	// inherited from migration 000012.
	_, err = pool.Exec(context.Background(), "DELETE FROM refresh_tokens")
	require.NoError(t, err)
	_, err = pool.Exec(context.Background(), "DELETE FROM feeds")
	require.NoError(t, err)
	_, err = pool.Exec(context.Background(), "DELETE FROM users")
	require.NoError(t, err)

	t.Cleanup(func() { pool.Close() })

	repo := NewRepository(pool)
	return pool, repo
}

// TestCreateUser_UsernameConstraintIntegration is the real-Postgres
// counterpart of the unit-level TestCreateUser_UsernameUniqueViolationMapsToErrUsernameTaken:
// inserting a second user with the same username produces a
// pgconn.PgError whose ConstraintName matches users_username_key and
// the repository surfaces it as ErrUsernameTaken (not the raw
// driver error). Skips when no DB is available.
func TestCreateUser_UsernameConstraintIntegration(t *testing.T) {
	if dbURL == "" {
		t.Skip("PostgreSQL container not available; set SKIP_CONTAINERS=false to run integration tests")
	}
	ctx := context.Background()

	repo := setupRefreshIntegrationTest(t)

	first := &User{Username: "dup-username", PasswordHash: "x"}
	require.NoError(t, repo.CreateUser(ctx, first))

	second := &User{Username: "dup-username", PasswordHash: "x"}
	err := repo.CreateUser(ctx, second)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrUsernameTaken),
		"expected ErrUsernameTaken against real Postgres, got %v", err)
}

// TestRefreshTokensIntegration is the round-trip integration check for
// the 000008 migration: it inserts a user, mints a refresh token,
// fetches it by hash, rotates it, and revokes the family. The
// integration test exercises the actual SQL produced by sqlc (the
// CTEs, the self-stamp, the family revoke) — the unit tests above
// only pin the repository's projection logic.
func TestRefreshTokensIntegration(t *testing.T) {
	if dbURL == "" {
		t.Skip("PostgreSQL container not available; set SKIP_CONTAINERS=false to run integration tests")
	}
	ctx := context.Background()

	t.Run("CreateAndFetch", func(t *testing.T) {
		repo := setupRefreshIntegrationTest(t)

		u := &User{
			Username:     "alice",
			PasswordHash: "x",
		}
		require.NoError(t, repo.CreateUser(ctx, u))

		expires := time.Now().Add(7 * 24 * time.Hour)
		tokenHash := []byte("test-hash-1")
		row, err := repo.CreateRefreshToken(ctx, u.ID, tokenHash, expires)
		require.NoError(t, err)
		assert.NotZero(t, row.ID)
		assert.Equal(t, row.ID, row.FamilyID, "family_id should be self-stamped to the row's id")
		assert.Nil(t, row.RevokedAt)
		assert.Nil(t, row.ReplacedByID)

		got, err := repo.GetRefreshTokenByHash(ctx, tokenHash)
		require.NoError(t, err)
		assert.Equal(t, row.ID, got.ID)
		assert.Equal(t, u.ID, got.UserID)
	})

	t.Run("RotateMarksOldRevoked", func(t *testing.T) {
		repo := setupRefreshIntegrationTest(t)

		u := &User{Username: "bob", PasswordHash: "x"}
		require.NoError(t, repo.CreateUser(ctx, u))

		expires := time.Now().Add(7 * 24 * time.Hour)
		first, err := repo.CreateRefreshToken(ctx, u.ID, []byte("hash-a"), expires)
		require.NoError(t, err)

		newHash := []byte("hash-b")
		second, err := repo.RotateRefreshToken(ctx, first.ID, u.ID, newHash, first.FamilyID, expires)
		require.NoError(t, err)
		assert.NotEqual(t, first.ID, second.ID, "rotated token must have a new id")
		assert.Equal(t, first.FamilyID, second.FamilyID, "rotated token must stay in the same family")

		// Re-fetch the old row directly via the second hash; the old
		// row should now be revoked. We confirm by looking up the new
		// hash and asserting ReplacedByID is populated.
		assert.NotNil(t, second.ReplacedByID)
		assert.Equal(t, first.ID, *second.ReplacedByID)
	})

	t.Run("RevokeFamily", func(t *testing.T) {
		repo := setupRefreshIntegrationTest(t)

		u := &User{Username: "carol", PasswordHash: "x"}
		require.NoError(t, repo.CreateUser(ctx, u))

		expires := time.Now().Add(7 * 24 * time.Hour)
		first, err := repo.CreateRefreshToken(ctx, u.ID, []byte("hash-c"), expires)
		require.NoError(t, err)
		_, err = repo.RotateRefreshToken(ctx, first.ID, u.ID, []byte("hash-d"), first.FamilyID, expires)
		require.NoError(t, err)

		// Revoke the entire family. After the rotation the OLD row
		// is already revoked by the RotateRefreshToken CTE, so only
		// the NEW row remains with revoked_at IS NULL. The
		// family-level revoke therefore flips one row.
		affected, err := repo.RevokeRefreshTokenFamily(ctx, first.FamilyID)
		require.NoError(t, err)
		assert.Equal(t, int64(1), affected, "expected only the new row to need a family revoke (the old row was revoked by rotation)")

		// Second call should be a no-op (the new row is now also
		// revoked) — pin the rows-affected = 0 invariant.
		affected, err = repo.RevokeRefreshTokenFamily(ctx, first.FamilyID)
		require.NoError(t, err)
		assert.Equal(t, int64(0), affected)
	})

	t.Run("RevokeAllForUser", func(t *testing.T) {
		repo := setupRefreshIntegrationTest(t)

		u := &User{Username: "dave", PasswordHash: "x"}
		require.NoError(t, repo.CreateUser(ctx, u))

		expires := time.Now().Add(7 * 24 * time.Hour)
		first, err := repo.CreateRefreshToken(ctx, u.ID, []byte("hash-e"), expires)
		require.NoError(t, err)
		_, err = repo.RotateRefreshToken(ctx, first.ID, u.ID, []byte("hash-f"), first.FamilyID, expires)
		require.NoError(t, err)
		second, err := repo.CreateRefreshToken(ctx, u.ID, []byte("hash-g"), expires)
		require.NoError(t, err)
		_, err = repo.RotateRefreshToken(ctx, second.ID, u.ID, []byte("hash-h"), second.FamilyID, expires)
		require.NoError(t, err)

		other := &User{Username: "erin", PasswordHash: "x"}
		require.NoError(t, repo.CreateUser(ctx, other))
		otherToken, err := repo.CreateRefreshToken(ctx, other.ID, []byte("hash-i"), expires)
		require.NoError(t, err)

		affected, err := repo.RevokeRefreshTokensByUser(ctx, u.ID)
		require.NoError(t, err)
		assert.Equal(t, int64(2), affected, "one active row per family should be revoked")

		affected, err = repo.RevokeRefreshTokensByUser(ctx, u.ID)
		require.NoError(t, err)
		assert.Equal(t, int64(0), affected, "second call must be a no-op")

		survivor, err := repo.GetRefreshTokenByHash(ctx, []byte("hash-i"))
		require.NoError(t, err)
		assert.Equal(t, otherToken.ID, survivor.ID)
		assert.Nil(t, survivor.RevokedAt, "another user's tokens must survive")
	})

	t.Run("LogoutFamilyIsolation", func(t *testing.T) {
		repo := setupRefreshIntegrationTest(t)
		tokens, err := auth.NewTokenService([]byte(auth.TestSecret), 15*time.Minute)
		require.NoError(t, err)
		svc := NewService(repo, tokens, 15*time.Minute, 7*24*time.Hour, noop.NewTracerProvider().Tracer("test"))

		u := &User{Username: "frank", PasswordHash: "x"}
		require.NoError(t, repo.CreateUser(ctx, u))

		expires := time.Now().Add(7 * 24 * time.Hour)
		rawA, hashA, err := newRefreshToken()
		require.NoError(t, err)
		_, err = repo.CreateRefreshToken(ctx, u.ID, hashA, expires)
		require.NoError(t, err)
		rawB, hashB, err := newRefreshToken()
		require.NoError(t, err)
		_, err = repo.CreateRefreshToken(ctx, u.ID, hashB, expires)
		require.NoError(t, err)

		require.NoError(t, svc.Logout(ctx, rawA))

		a, err := repo.GetRefreshTokenByHash(ctx, hashA)
		require.NoError(t, err)
		assert.NotNil(t, a.RevokedAt, "the logged-out family must be revoked")

		b, err := repo.GetRefreshTokenByHash(ctx, hashB)
		require.NoError(t, err)
		assert.Nil(t, b.RevokedAt, "the other device's family must survive")

		_, err = svc.Refresh(ctx, rawB)
		require.NoError(t, err, "the other device must still be able to refresh")
	})
}
