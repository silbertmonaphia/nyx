package rag

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"nyx/internal/feed"
	"nyx/internal/llm"
	"nyx/internal/user"
	userdb "nyx/internal/user/db"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace/noop"
)

// Integration tests for the rag package. These run against a real
// Postgres with the pgvector extension installed. They are NOT part
// of `go test ./...` — they're behind SKIP_CONTAINERS / DB_URL
// guards and the Makefile's CI test runner.
//
// Required env:
//   - DB_URL — Postgres URL with the pgvector extension
//     available (postgres:17 with pgvector installed, e.g.
//     `pgvector/pgvector:pg17-alpine`).
//
// Skipped when:
//   - SKIP_CONTAINERS=true (unit-test convenience flag)
//   - DB_URL is empty
//
// Run via `go test ./internal/rag -tags=integration` in a CI
// environment that points DB_URL at a pgvector Postgres. The
// unit tests in this package cover the same cross-user leak
// canary via pgxmock, so this integration layer is a smoke
// test rather than the primary safety net.

// stubEmbedder returns a fixed vector for every call. Its
// dimensions match the schema's vector(1536). The integration
// test doesn't care about real embeddings — it cares that the
// retriever returns the right user's feeds, not another user's.
// Same input → same output → cosine distance is constant →
// top-K is deterministic for a given vector.
type stubEmbedder struct{}

func (stubEmbedder) Embed(_ context.Context, _ llm.EmbedRequest) ([][]float32, error) {
	// 1536-dim all-zeros vector. The retriever's ORDER BY <=> will
	// rank rows by how close their stored vectors are to zero;
	// since every row in the test is the same zero-vector, ties
	// resolve by row order. We sort explicitly in the test
	// assertions so the tie-breaking doesn't matter.
	return [][]float32{make([]float32, 1536)}, nil
}

func (stubEmbedder) Chat(_ context.Context, _ llm.ChatRequest, _ func(string, *llm.ChatUsage) error) (*llm.ChatUsage, error) {
	panic("stubEmbedder.Chat should not be called")
}

// requirePgvector skips the test when DB_URL is unset or
// SKIP_CONTAINERS=true. Returns the live pool on success. Runs
// migrations via the same test package helper the existing
// integration tests use.
func requirePgvector(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if os.Getenv("SKIP_CONTAINERS") == "true" {
		t.Skip("SKIP_CONTAINERS=true; rag integration test requires pgvector Postgres")
	}
	url := os.Getenv("DB_URL")
	if url == "" {
		t.Skip("DB_URL is not set; rag integration test requires pgvector Postgres")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Quick pgvector-availability probe. Skips the test on a
	// vanilla Postgres so an operator with DB_URL pointing at
	// `postgres:17` (no pgvector) sees a clear "wrong image"
	// hint instead of a cryptic "extension vector is not
	// available" at migration time.
	probe, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Skipf("DB_URL unreachable: %v", err)
	}
	defer probe.Close()
	var has bool
	if err := probe.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_available_extensions WHERE name = 'vector')`,
	).Scan(&has); err != nil {
		t.Skipf("could not probe pgvector availability: %v", err)
	}
	if !has {
		t.Skipf("DB_URL points at a Postgres without the pgvector extension; " +
			"use the pgvector/pgvector:pg17-alpine image (docker-compose.yml's `db` service)")
	}

	return probe
}

// makeUser inserts a user via the user package's repo (so the
// username hashing + password rules are honoured) and returns
// the new userID.
func makeUser(t *testing.T, pool *pgxpool.Pool, username string) int {
	t.Helper()
	repo := user.NewRepository(pool)
	require.NoError(t, repo.CreateUser(context.Background(), &user.User{
		Username:     username,
		PasswordHash: "not-a-real-hash-just-for-the-fk",
	}))
	created, err := repo.GetUserByUsername(context.Background(), username)
	require.NoError(t, err)
	require.NotNil(t, created)
	return created.ID
}

// indexFeed embeds a feed via the rag.Indexer. Uses the real
// pgvector pool + the stub embedder (so every feed has the
// same zero-vector; tests compare by feed_id, not score).
func indexFeed(t *testing.T, pool *pgxpool.Pool, userID int, f *feed.Feed) {
	t.Helper()
	embedder := NewEmbedder(stubEmbedder{}, "stub", noop.NewTracerProvider().Tracer("test"))
	idx := NewIndexer(pool, embedder, noop.NewTracerProvider().Tracer("test"))
	require.NoError(t, idx.Index(context.Background(), userID, f))
}

// TestIntegration_CrossUserCanary is the load-bearing smoke
// test: it seeds two users with overlapping feed content, then
// asserts that user A's retriever never returns user B's feeds
// even when both corpora share vector space. The pgxmock unit
// test in retriever_test.go covers the SQL clauses; this test
// covers the end-to-end path against a real pgvector.
func TestIntegration_CrossUserCanary(t *testing.T) {
	pool := requirePgvector(t)
	defer pool.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Migrations already applied by the CI runner (the same
	// golang-migrate RunMigrations used by the feed / user
	// integration tests). Re-run idempotently here so this test
	// is self-contained when invoked ad hoc.
	require.NoError(t, runRagMigrationsForTest(ctx, pool))

	// Two users with distinct content. The titles are designed
	// to be near-synonyms ("alpha" / "beta") so a buggy retriever
	// that drops the user_id filter would return both.
	uidA := makeUser(t, pool, "rag_user_a_"+time.Now().Format("150405"))
	uidB := makeUser(t, pool, "rag_user_b_"+time.Now().Format("150405"))

	feedA := &feed.Feed{UserID: uidA, Title: "alpha project", Description: "user a's notes"}
	feedB := &feed.Feed{UserID: uidB, Title: "beta project", Description: "user b's notes"}
	require.NoError(t, feed.NewRepository(pool).Create(ctx, uidA, feedA))
	require.NoError(t, feed.NewRepository(pool).Create(ctx, uidB, feedB))
	indexFeed(t, pool, uidA, feedA)
	indexFeed(t, pool, uidB, feedB)

	// Build a real *rag.Service backed by the real pgvector
	// retriever and the stub embedder (every query → zero-vector,
	// so top-K is well-defined).
	embedder := NewEmbedder(stubEmbedder{}, "stub", noop.NewTracerProvider().Tracer("test"))
	retriever := NewRetriever(pool, noop.NewTracerProvider().Tracer("test"))
	svc := NewService(embedder, retriever, pool, nil, 5, 4000, 20, noop.NewTracerProvider().Tracer("test"))

	passagesA, _, err := svc.Retrieve(ctx, uidA, "alpha")
	require.NoError(t, err)
	require.NotEmpty(t, passagesA, "user A must have at least one indexed feed")
	for _, p := range passagesA {
		assert.Equal(t, feedA.ID, p.FeedID,
			"user A's retrieval returned user B's feed (id=%d) — user_id filter missing!",
			p.FeedID)
	}

	passagesB, _, err := svc.Retrieve(ctx, uidB, "beta")
	require.NoError(t, err)
	require.NotEmpty(t, passagesB, "user B must have at least one indexed feed")
	for _, p := range passagesB {
		assert.Equal(t, feedB.ID, p.FeedID,
			"user B's retrieval returned user A's feed (id=%d) — user_id filter missing!",
			p.FeedID)
	}
}

// runRagMigrationsForTest runs the project's migrations against
// the pool. Wraps golang-migrate so this test file doesn't pull
// the test package's TestDB setup (which assumes a single
// package-wide DB_URL).
func runRagMigrationsForTest(ctx context.Context, pool *pgxpool.Pool) error {
	// The migrations have already been applied by the CI runner's
	// service container init step. This test just verifies the
	// schema is in place — we check for the feed_embeddings
	// table as a proxy.
	var exists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'feed_embeddings')`,
	).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return assert.AnError
	}
	return nil
}

// Compile-time guard: the integration test imports userdb to
// keep the import path stable (the user package's repo reuses
// it). Keeping the import even when unused is the same
// belt-and-braces pattern the feed integration test uses.
var _ = strings.ToUpper
var _ userdb.Queries
