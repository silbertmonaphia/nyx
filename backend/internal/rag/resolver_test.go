package rag

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace/noop"

	"nyx/internal/feed"
)

type vecRecordingRetriever struct {
	mu       sync.Mutex
	calls    int
	userID   int
	queryVec []float32
}

func (r *vecRecordingRetriever) Retrieve(_ context.Context, userID int, queryVec []float32, _ int) ([]Passage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.userID = userID
	r.queryVec = queryVec
	return []Passage{{FeedID: 1, Title: "hit"}}, nil
}

func (r *vecRecordingRetriever) snapshot() (int, int, []float32) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls, r.userID, r.queryVec
}

func TestService_Retrieve_UsesPerUserEmbedder(t *testing.T) {
	providerA := &stubProvider{vecs: [][]float32{{1, 0, 0}}}
	providerB := &stubProvider{vecs: [][]float32{{0, 1, 0}}}
	embedderA := NewEmbedder(providerA, "embed-a", noop.NewTracerProvider().Tracer("test"))
	embedderB := NewEmbedder(providerB, "embed-b", noop.NewTracerProvider().Tracer("test"))

	svc := NewService(nil, nil, nil, stubFeedRepo{}, 5, 4000, 20, noop.NewTracerProvider().Tracer("test"))
	svc.SetEmbedderResolver(func(_ context.Context, userID int) (*Embedder, error) {
		switch userID {
		case 1:
			return embedderA, nil
		case 2:
			return embedderB, nil
		default:
			return nil, ErrNoEmbedder
		}
	})
	rec := &vecRecordingRetriever{}
	svc.retriever = rec

	_, block, err := svc.Retrieve(context.Background(), 1, "alpha")
	require.NoError(t, err)
	require.NotEmpty(t, block)
	assert.Equal(t, 1, providerA.embedCalls)
	assert.Zero(t, providerB.embedCalls)
	assert.Equal(t, "embed-a", providerA.lastReq.Model)
	calls, seenUser, vec := rec.snapshot()
	assert.Equal(t, 1, calls)
	assert.Equal(t, 1, seenUser)
	assert.Equal(t, []float32{1, 0, 0}, vec)

	_, _, err = svc.Retrieve(context.Background(), 2, "beta")
	require.NoError(t, err)
	assert.Equal(t, 1, providerA.embedCalls)
	assert.Equal(t, 1, providerB.embedCalls)
	assert.Equal(t, "embed-b", providerB.lastReq.Model)
	calls, seenUser, vec = rec.snapshot()
	assert.Equal(t, 2, calls)
	assert.Equal(t, 2, seenUser)
	assert.Equal(t, []float32{0, 1, 0}, vec)
}

func TestService_Retrieve_UnconfiguredReturnsErrNoEmbedder(t *testing.T) {
	cases := map[string]EmbedderResolver{
		"resolver reports not found": func(context.Context, int) (*Embedder, error) {
			return nil, ErrNoEmbedder
		},
		"resolver returns no embedder": func(context.Context, int) (*Embedder, error) {
			return nil, nil
		},
	}
	for name, resolver := range cases {
		t.Run(name, func(t *testing.T) {
			svc := NewService(nil, nil, nil, stubFeedRepo{}, 5, 4000, 20, noop.NewTracerProvider().Tracer("test"))
			svc.SetEmbedderResolver(resolver)
			rec := &vecRecordingRetriever{}
			svc.retriever = rec

			_, _, err := svc.Retrieve(context.Background(), 7, "anything")
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrNoEmbedder)
			calls, _, _ := rec.snapshot()
			assert.Zero(t, calls, "retrieval must not run without an embedder")
		})
	}
}

func TestService_Retrieve_WidthMismatchReturnsErrNoEmbedder(t *testing.T) {
	embedder := NewEmbedder(&stubProvider{vecs: [][]float32{{0.1, 0.2, 0.3}}}, "m", noop.NewTracerProvider().Tracer("test"))
	embedder.SetDimensions(1024)
	svc := NewService(embedder, nil, nil, stubFeedRepo{}, 5, 4000, 20, noop.NewTracerProvider().Tracer("test"))
	rec := &vecRecordingRetriever{}
	svc.retriever = rec

	_, _, err := svc.Retrieve(context.Background(), 1, "anything")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNoEmbedder)
	calls, _, _ := rec.snapshot()
	assert.Zero(t, calls)
}

func TestEmbedder_DimensionMismatchReturnsErrNoEmbedder(t *testing.T) {
	provider := &stubProvider{vecs: [][]float32{{0.1, 0.2, 0.3}}}
	embedder := NewEmbedder(provider, "m", noop.NewTracerProvider().Tracer("test"))
	embedder.SetDimensions(1024)

	f := &feed.Feed{ID: 1, Title: "x"}
	_, _, err := embedder.Embed(context.Background(), f)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNoEmbedder)

	_, err = embedder.EmbedStrings(context.Background(), []string{"q"})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNoEmbedder)

	_, _, err = embedder.EmbedBatch(context.Background(), []*feed.Feed{f})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNoEmbedder)

	embedder.SetDimensions(3)
	vec, _, err := embedder.Embed(context.Background(), f)
	require.NoError(t, err)
	assert.Len(t, vec, 3)
}

func TestService_BackfillOnce_UnconfiguredDoesNotPoison(t *testing.T) {
	mock, err := pgxmock.NewPool()
	require.NoError(t, err)
	defer mock.Close()

	provider := &stubProvider{vecs: [][]float32{{0.1}}}
	embedder := NewEmbedder(provider, "m", noop.NewTracerProvider().Tracer("test"))
	var configured atomic.Bool
	svc := NewService(embedder, nil, mock, stubFeedRepo{}, 5, 4000, 20, noop.NewTracerProvider().Tracer("test"))
	svc.SetEmbedderResolver(func(context.Context, int) (*Embedder, error) {
		if configured.Load() {
			return embedder, nil
		}
		return nil, ErrNoEmbedder
	})

	svc.BackfillOnce(context.Background(), 42)
	countEntries := func() int {
		cnt := 0
		svc.backfillInFlight.Range(func(_, _ any) bool {
			cnt++
			return true
		})
		return cnt
	}
	assert.Zero(t, countEntries(), "an unconfigured user must not consume the once")

	mock.ExpectQuery(`SELECT`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "user_id", "title", "description", "created_at", "updated_at", "deleted_at"}))
	configured.Store(true)
	svc.BackfillOnce(context.Background(), 42)
	assert.Equal(t, 1, countEntries(), "a later configured call runs the backfill")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestIndexer_Index_UnconfiguredReturnsErrNoEmbedder(t *testing.T) {
	cases := map[string]EmbedderResolver{
		"resolver reports not found": func(context.Context, int) (*Embedder, error) {
			return nil, ErrNoEmbedder
		},
		"resolver returns no embedder": func(context.Context, int) (*Embedder, error) {
			return nil, nil
		},
	}
	for name, resolver := range cases {
		t.Run(name, func(t *testing.T) {
			mock, err := pgxmock.NewPool()
			require.NoError(t, err)
			defer mock.Close()

			provider := &stubProvider{vecs: [][]float32{{0.1}}}
			embedder := NewEmbedder(provider, "m", noop.NewTracerProvider().Tracer("test"))
			idx := NewIndexer(mock, embedder, noop.NewTracerProvider().Tracer("test"))
			idx.SetEmbedderResolver(resolver)

			err = idx.Index(context.Background(), 7, &feed.Feed{ID: 42, Title: "x"})
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrNoEmbedder)
			assert.Zero(t, provider.embedCalls)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestIndexer_Index_UsesPerUserEmbedder(t *testing.T) {
	mock, err := pgxmock.NewPool()
	require.NoError(t, err)
	defer mock.Close()

	mock.ExpectQuery(`SELECT content_hash FROM feed_embeddings WHERE feed_id`).
		WithArgs(int32(42)).
		WillReturnError(pgx.ErrNoRows)
	mock.ExpectExec(`INSERT INTO feed_embeddings`).
		WithArgs(int32(42), int64(7), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	provider := &stubProvider{vecs: [][]float32{{0.1, 0.2}}}
	userEmbedder := NewEmbedder(provider, "user-embed", noop.NewTracerProvider().Tracer("test"))
	deploymentProvider := &stubProvider{vecs: [][]float32{{9, 9}}}
	deploymentEmbedder := NewEmbedder(deploymentProvider, "deployment-embed", noop.NewTracerProvider().Tracer("test"))
	idx := NewIndexer(mock, deploymentEmbedder, noop.NewTracerProvider().Tracer("test"))
	idx.SetEmbedderResolver(func(_ context.Context, userID int) (*Embedder, error) {
		if userID != 7 {
			t.Fatalf("resolver saw user %d, want 7", userID)
		}
		return userEmbedder, nil
	})

	err = idx.Index(context.Background(), 7, &feed.Feed{ID: 42, Title: "x"})
	require.NoError(t, err)
	assert.Equal(t, "user-embed", provider.lastReq.Model)
	assert.Zero(t, deploymentProvider.embedCalls)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestIndexer_InvalidateUserEmbeddings(t *testing.T) {
	mock, err := pgxmock.NewPool()
	require.NoError(t, err)
	defer mock.Close()

	mock.ExpectExec("DELETE FROM feed_embeddings").
		WithArgs(int64(9)).
		WillReturnResult(pgxmock.NewResult("DELETE", 3))

	idx := NewIndexer(mock, nil, noop.NewTracerProvider().Tracer("test"))
	rows, err := idx.InvalidateUserEmbeddings(context.Background(), 9)
	require.NoError(t, err)
	assert.Equal(t, int64(3), rows)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestService_ResetBackfillAllowsAnotherRun(t *testing.T) {
	svc := &Service{}
	svc.backfillInFlight.Store(7, &sync.Once{})
	svc.ResetBackfill(7)
	_, ok := svc.backfillInFlight.Load(7)
	assert.False(t, ok)
}
