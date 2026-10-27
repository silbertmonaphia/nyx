package rag

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace/noop"

	"nyx/internal/feed"
	"nyx/internal/llm"
)

// stubFeedRepo is a no-op feed.Repository — the lazy-backfill
// path needs Repository only for "find feeds without embeddings"
// which it actually bypasses (it issues ragdb.ListFeedsWithoutEmbeddings
// directly). Tests that don't exercise the backfill can pass nil
// for the repo; the service guards on it.
type stubFeedRepo struct{}

func (stubFeedRepo) GetAll(_ context.Context, _ int, _ string, _, _ int, _ feed.SortOrder) (*feed.Page, error) {
	return nil, nil
}
func (stubFeedRepo) GetFeedByID(_ context.Context, _ int, _ int) (*feed.Feed, error) {
	return nil, feed.ErrNotFound
}
func (stubFeedRepo) Create(_ context.Context, _ int, _ *feed.Feed) error    { return nil }
func (stubFeedRepo) Update(_ context.Context, _, _ int, _ *feed.Feed) error { return nil }
func (stubFeedRepo) Delete(_ context.Context, _, _ int) error               { return nil }
func (stubFeedRepo) Ping(_ context.Context) error                           { return nil }

// TestRenderContextBlock_BasicShape pins the prompt shape: each
// passage renders with "[Feed N] Title: ...\nDescription: ...",
// passages are joined with "\n\n---\n\n", empty descriptions
// render Title-only.
func TestRenderContextBlock_BasicShape(t *testing.T) {
	passages := []Passage{
		{FeedID: 1, Title: "A", Description: "desc-a"},
		{FeedID: 2, Title: "B", Description: ""},
		{FeedID: 3, Title: "C", Description: "desc-c"},
	}
	got := renderContextBlock(passages, 4000)
	assert.Contains(t, got, "[Feed 1] Title: A\nDescription: desc-a")
	assert.Contains(t, got, "[Feed 2] Title: B", "empty description renders Title-only")
	assert.NotContains(t, got, "[Feed 2] Title: B\nDescription", "empty description has no Description line")
	assert.Contains(t, got, "[Feed 3] Title: C\nDescription: desc-c")
	assert.Contains(t, got, "\n\n---\n\n", "passages separated by triple-dash")
}

// TestRenderContextBlock_TruncatesAtMaxChars pins that the
// context block truncates at the budget. A first passage larger
// than the budget is truncated with an ellipsis; subsequent
// oversize passages are omitted with a "[truncated]" marker.
func TestRenderContextBlock_TruncatesAtMaxChars(t *testing.T) {
	passages := []Passage{
		{FeedID: 1, Title: strings.Repeat("a", 100), Description: strings.Repeat("b", 100)},
		{FeedID: 2, Title: strings.Repeat("c", 100), Description: strings.Repeat("d", 100)},
	}
	got := renderContextBlock(passages, 150)
	assert.Contains(t, got, "…", "oversize passages truncate with ellipsis")
	assert.Contains(t, got, "[truncated — additional feeds omitted]",
		"oversize trailing passages surface as a truncation marker")
	assert.NotContains(t, got, "[Feed 2]", "second passage omitted entirely")
}

// TestRenderContextBlock_EmptyPassagesIsEmpty pins that an empty
// slice returns "". The chat service treats "" as "no context to
// inject".
func TestRenderContextBlock_EmptyPassagesIsEmpty(t *testing.T) {
	assert.Equal(t, "", renderContextBlock(nil, 4000))
	assert.Equal(t, "", renderContextBlock([]Passage{}, 4000))
	assert.Equal(t, "", renderContextBlock([]Passage{{Title: "x"}}, 0), "maxChars <= 0 returns empty")
}

// TestRenderContextBlock_IndexesAreOneBased pins the 1-based
// index in "[Feed N]" — the prompt reads more naturally than
// "[Feed 0]".
func TestRenderContextBlock_IndexesAreOneBased(t *testing.T) {
	got := renderContextBlock([]Passage{
		{Title: "First"},
		{Title: "Second"},
	}, 4000)
	assert.Contains(t, got, "[Feed 1] Title: First")
	assert.Contains(t, got, "[Feed 2] Title: Second")
	assert.NotContains(t, got, "[Feed 0]")
}

// TestPassage_Render pins the per-passage shape with and without
// description.
func TestPassage_Render(t *testing.T) {
	withDesc := Passage{Title: "T", Description: "D"}
	assert.Equal(t, "[Feed 1] Title: T\nDescription: D", withDesc.Render(1))

	noDesc := Passage{Title: "T"}
	assert.Equal(t, "[Feed 2] Title: T", noDesc.Render(2))
}

// TestService_Retrieve_HappyPath pins the end-to-end chat-side
// path: query → embed → retrieve → render context block.
// The stub provider returns one query vector; the stub retriever
// returns canned passages.
func TestService_Retrieve_HappyPath(t *testing.T) {
	p := &stubProvider{vecs: [][]float32{{0.1, 0.2, 0.3}}}
	e := NewEmbedder(p, "m", noop.NewTracerProvider().Tracer("test"))

	// Bypass the actual SQL by using a stub retriever that
	// captures the call and returns canned passages. The real
	// Retriever is tested separately against pgxmock.
	var capturedK int
	r := &serviceTestRetriever{
		passages: []Passage{
			{FeedID: 1, Title: "The Matrix", Description: "sci-fi", Score: 0.93},
			{FeedID: 2, Title: "The Matrix Reloaded", Description: "sequel", Score: 0.71},
		},
		recordK: func(k int) { capturedK = k },
	}

	svc := NewService(e, nil, nil, stubFeedRepo{}, 5, 4000, 20, noop.NewTracerProvider().Tracer("test"))
	svc.retriever = &serviceTestRetrieverAdapter{r: r}

	passages, block, err := svc.Retrieve(context.Background(), 42, "what is the matrix about?")
	require.NoError(t, err)
	require.Len(t, passages, 2)
	assert.Equal(t, "The Matrix", passages[0].Title)
	assert.NotEmpty(t, block)
	assert.Contains(t, block, "[Feed 1]")
	assert.Contains(t, block, "sci-fi")
	assert.Equal(t, 5, capturedK, "retriever received the configured top-K")

	assert.Equal(t, 1, p.embedCalls)
	assert.Equal(t, []string{"what is the matrix about?"}, p.lastReq.Inputs)
}

// TestService_Retrieve_EmptyQueryIsNoop pins that an empty query
// short-circuits before the Embed API call. This avoids an
// unnecessary embedding cost when a buggy client sends empty
// content.
func TestService_Retrieve_EmptyQueryIsNoop(t *testing.T) {
	p := &stubProvider{}
	e := NewEmbedder(p, "m", noop.NewTracerProvider().Tracer("test"))

	svc := NewService(e, nil, nil, stubFeedRepo{}, 5, 4000, 20, noop.NewTracerProvider().Tracer("test"))
	passages, block, err := svc.Retrieve(context.Background(), 42, "")
	require.NoError(t, err)
	assert.Empty(t, passages)
	assert.Empty(t, block)
	assert.Equal(t, 0, p.embedCalls)
}

// TestService_Retrieve_EmbedErrorFailsOpen pins the contract
// that an Embedding API error surfaces as an error (the chat service
// catches and continues without context). The provider error
// must NOT be wrapped with anything the chat service would
// mistake for an LLM sentinel.
func TestService_Retrieve_EmbedErrorFailsOpen(t *testing.T) {
	p := &stubProvider{err: llm.ErrProviderUnavailable}
	e := NewEmbedder(p, "m", noop.NewTracerProvider().Tracer("test"))

	svc := NewService(e, nil, nil, stubFeedRepo{}, 5, 4000, 20, noop.NewTracerProvider().Tracer("test"))
	_, _, err := svc.Retrieve(context.Background(), 42, "anything")
	require.Error(t, err)
	assert.ErrorIs(t, err, llm.ErrProviderUnavailable)
}

// TestService_Retrieve_NilEmbedderReturnsErrNoEmbedder pins the
// programming-error case: a Service built without an embedder
// (LLM disabled) returns ErrNoEmbedder when Retrieve is called.
// The chat service guards on s.rag != nil so this is belt-and-
// braces.
func TestService_Retrieve_NilEmbedderReturnsErrNoEmbedder(t *testing.T) {
	svc := NewService(nil, nil, nil, stubFeedRepo{}, 5, 4000, 20, noop.NewTracerProvider().Tracer("test"))
	_, _, err := svc.Retrieve(context.Background(), 42, "anything")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNoEmbedder)
}

// TestService_BackfillOnce_ConcurrentRunsExactlyOnce pins the
// canonical sync.Map + sync.Once race-free pattern. 100
// goroutines call BackfillOnce for the same user; the
// backfillInFlight map must end with exactly one entry per
// userID, and different userIDs get separate entries.
func TestService_BackfillOnce_ConcurrentRunsExactlyOnce(t *testing.T) {
	p := &stubProvider{vecs: [][]float32{{0.1}}}
	e := NewEmbedder(p, "m", noop.NewTracerProvider().Tracer("test"))
	mock, err := pgxmock.NewPool()
	require.NoError(t, err)
	defer mock.Close()

	// BackfillOnce iterates ListFeedsWithoutEmbeddings; an empty
	// result row set makes the loop exit on the first iteration
	// before any Embed call. The 100 concurrent goroutines hit
	// the same Once so only one SQL call lands for user 42; the
	// later calls for users 99 + 100 add two more. Total: 3.
	mock.ExpectQuery(`SELECT`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "user_id", "title", "description", "created_at", "updated_at", "deleted_at"}))
	mock.ExpectQuery(`SELECT`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "user_id", "title", "description", "created_at", "updated_at", "deleted_at"}))
	mock.ExpectQuery(`SELECT`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "user_id", "title", "description", "created_at", "updated_at", "deleted_at"}))

	svc := NewService(e, nil, mock, stubFeedRepo{}, 5, 4000, 20, noop.NewTracerProvider().Tracer("test"))

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			svc.BackfillOnce(context.Background(), 42)
		}()
	}
	wg.Wait()

	// sync.Map should hold exactly one entry per userID.
	cnt := 0
	svc.backfillInFlight.Range(func(_, _ any) bool {
		cnt++
		return true
	})
	assert.Equal(t, 1, cnt, "sync.Map should hold exactly one entry per userID")

	// A second wave of calls doesn't add more entries.
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			svc.BackfillOnce(context.Background(), 42)
		}()
	}
	wg.Wait()
	cnt = 0
	svc.backfillInFlight.Range(func(_, _ any) bool {
		cnt++
		return true
	})
	assert.Equal(t, 1, cnt, "second wave must not add entries")

	// Different userIDs produce different entries.
	svc.BackfillOnce(context.Background(), 99)
	svc.BackfillOnce(context.Background(), 100)
	cnt = 0
	svc.backfillInFlight.Range(func(_, _ any) bool {
		cnt++
		return true
	})
	assert.Equal(t, 3, cnt, "different userIDs get separate Once entries")
	require.NoError(t, mock.ExpectationsWereMet())
}

type countingFeedID struct {
	calls *int
	want  int32
}

func (c countingFeedID) Match(v interface{}) bool {
	switch id := v.(type) {
	case int32:
		if id == c.want {
			*c.calls++
			return true
		}
	case int64:
		if id == int64(c.want) {
			*c.calls++
			return true
		}
	}
	return false
}

func TestService_EmbedBackfillBatch_RejectedBatchIsolatesPerFeed(t *testing.T) {
	p := &stubProvider{
		errMulti:     llm.ErrInvalidInput,
		failContains: "TOO_LONG",
		vecFromInput: true,
	}
	e := NewEmbedder(p, "m", noop.NewTracerProvider().Tracer("test"))
	svc := NewService(e, nil, nil, stubFeedRepo{}, 5, 4000, 20, noop.NewTracerProvider().Tracer("test"))

	feeds := []*feed.Feed{
		{ID: 1, Title: "good", Description: "ok"},
		{ID: 2, Title: "bad", Description: "TOO_LONG"},
	}
	ctx := context.Background()
	_, span := noop.NewTracerProvider().Tracer("test").Start(ctx, "test")

	out, err := svc.embedBackfillBatch(ctx, e, feeds, span)
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, 1, out[0].feed.ID)
	assert.Equal(t, vecForInput(ChunkText(feeds[0])), out[0].vec)
	assert.Equal(t, HashChunk(feeds[0]), out[0].hash)
	assert.Equal(t, 3, p.embedCalls, "one batch call plus one call per feed")
}

func TestService_EmbedBackfillBatch_AllFeedsRejectedReturnsError(t *testing.T) {
	p := &stubProvider{
		errMulti:     llm.ErrInvalidInput,
		failContains: "TOO_LONG",
		vecFromInput: true,
	}
	e := NewEmbedder(p, "m", noop.NewTracerProvider().Tracer("test"))
	svc := NewService(e, nil, nil, stubFeedRepo{}, 5, 4000, 20, noop.NewTracerProvider().Tracer("test"))

	feeds := []*feed.Feed{
		{ID: 1, Title: "bad", Description: "TOO_LONG"},
		{ID: 2, Title: "worse", Description: "TOO_LONG_TOO"},
	}
	ctx := context.Background()
	_, span := noop.NewTracerProvider().Tracer("test").Start(ctx, "test")

	out, err := svc.embedBackfillBatch(ctx, e, feeds, span)
	require.Error(t, err)
	assert.ErrorIs(t, err, llm.ErrInvalidInput)
	assert.Empty(t, out)
}

func TestService_EmbedBackfillBatch_TransientErrorDuringIsolationSurfaces(t *testing.T) {
	p := &stubProvider{
		errMulti:      llm.ErrInvalidInput,
		failContains:  "TOO_LONG",
		transientText: "FLAKY",
		vecFromInput:  true,
	}
	e := NewEmbedder(p, "m", noop.NewTracerProvider().Tracer("test"))
	svc := NewService(e, nil, nil, stubFeedRepo{}, 5, 4000, 20, noop.NewTracerProvider().Tracer("test"))

	feeds := []*feed.Feed{
		{ID: 1, Title: "good", Description: "ok"},
		{ID: 2, Title: "poison", Description: "TOO_LONG"},
		{ID: 3, Title: "flaky", Description: "FLAKY"},
	}
	ctx := context.Background()
	_, span := noop.NewTracerProvider().Tracer("test").Start(ctx, "test")

	out, err := svc.embedBackfillBatch(ctx, e, feeds, span)
	require.Error(t, err)
	assert.ErrorIs(t, err, llm.ErrProviderUnavailable)
	require.Len(t, out, 1, "the successful feed is still returned so it can be persisted")
	assert.Equal(t, 1, out[0].feed.ID)
}

func TestService_EmbedBackfillBatch_TransientErrorIsNotIsolated(t *testing.T) {
	p := &stubProvider{errMulti: llm.ErrProviderUnavailable}
	e := NewEmbedder(p, "m", noop.NewTracerProvider().Tracer("test"))
	svc := NewService(e, nil, nil, stubFeedRepo{}, 5, 4000, 20, noop.NewTracerProvider().Tracer("test"))

	feeds := []*feed.Feed{{ID: 1, Title: "a"}, {ID: 2, Title: "b"}}
	ctx := context.Background()
	_, span := noop.NewTracerProvider().Tracer("test").Start(ctx, "test")

	out, err := svc.embedBackfillBatch(ctx, e, feeds, span)
	require.Error(t, err)
	assert.ErrorIs(t, err, llm.ErrProviderUnavailable)
	assert.Empty(t, out)
	assert.Equal(t, 1, p.embedCalls, "a transient failure must not fan out into per-feed calls")
}

func TestService_EmbedBackfillBatch_BatchSuccessReturnsEveryFeed(t *testing.T) {
	p := &stubProvider{vecs: [][]float32{{0.1}, {0.2}}}
	e := NewEmbedder(p, "m", noop.NewTracerProvider().Tracer("test"))
	svc := NewService(e, nil, nil, stubFeedRepo{}, 5, 4000, 20, noop.NewTracerProvider().Tracer("test"))

	feeds := []*feed.Feed{{ID: 1, Title: "a"}, {ID: 2, Title: "b"}}
	ctx := context.Background()
	_, span := noop.NewTracerProvider().Tracer("test").Start(ctx, "test")

	out, err := svc.embedBackfillBatch(ctx, e, feeds, span)
	require.NoError(t, err)
	require.Len(t, out, 2)
	assert.Equal(t, 1, out[0].feed.ID)
	assert.Equal(t, 2, out[1].feed.ID)
	assert.Equal(t, 1, p.embedCalls)
}

func TestService_BackfillOnce_RejectedFeedDoesNotStrandBatch(t *testing.T) {
	p := &stubProvider{
		errMulti:     llm.ErrInvalidInput,
		failContains: "TOO_LONG",
		vecFromInput: true,
	}
	e := NewEmbedder(p, "m", noop.NewTracerProvider().Tracer("test"))

	mock, err := pgxmock.NewPool()
	require.NoError(t, err)
	defer mock.Close()

	mock.ExpectQuery(`SELECT`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "user_id", "title", "description", "created_at", "updated_at", "deleted_at"}).
			AddRow(int32(1), int64(42), "good", "ok", time.Now(), time.Now(), nil).
			AddRow(int32(2), int64(42), "bad", "TOO_LONG", time.Now(), time.Now(), nil))

	poisonWrites := 0
	mock.ExpectExec(`INSERT INTO feed_embeddings`).
		WithArgs(countingFeedID{calls: &poisonWrites, want: 2}, pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1)).
		Maybe()
	mock.ExpectExec(`INSERT INTO feed_embeddings`).
		WithArgs(int32(1), int64(42), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	svc := NewService(e, nil, mock, stubFeedRepo{}, 5, 4000, 20, noop.NewTracerProvider().Tracer("test"))
	svc.BackfillOnce(context.Background(), 42)

	require.NoError(t, mock.ExpectationsWereMet())
	assert.Equal(t, 0, poisonWrites, "the rejected feed must not be indexed")
}

func TestService_BackfillOnce_PoisonFeedMakesProgressAcrossBatches(t *testing.T) {
	p := &stubProvider{
		errMulti:     llm.ErrInvalidInput,
		failContains: "TOO_LONG",
		vecFromInput: true,
	}
	e := NewEmbedder(p, "m", noop.NewTracerProvider().Tracer("test"))

	mock, err := pgxmock.NewPool()
	require.NoError(t, err)
	defer mock.Close()

	rows := func(ids ...int32) *pgxmock.Rows {
		r := pgxmock.NewRows([]string{"id", "user_id", "title", "description", "created_at", "updated_at", "deleted_at"})
		for _, id := range ids {
			desc := "ok"
			if id == 1 {
				desc = "TOO_LONG"
			}
			r.AddRow(id, int64(42), fmt.Sprintf("feed-%d", id), desc, time.Now(), time.Now(), nil)
		}
		return r
	}

	mock.ExpectQuery(`SELECT`).WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).WillReturnRows(rows(1, 2))
	mock.ExpectExec(`INSERT INTO feed_embeddings`).
		WithArgs(int32(2), int64(42), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectQuery(`SELECT`).WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).WillReturnRows(rows(1, 3))
	mock.ExpectExec(`INSERT INTO feed_embeddings`).
		WithArgs(int32(3), int64(42), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectQuery(`SELECT`).WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).WillReturnRows(rows(1, 4))
	mock.ExpectExec(`INSERT INTO feed_embeddings`).
		WithArgs(int32(4), int64(42), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectQuery(`SELECT`).WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).WillReturnRows(rows(1, 5))
	mock.ExpectExec(`INSERT INTO feed_embeddings`).
		WithArgs(int32(5), int64(42), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectQuery(`SELECT`).WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).WillReturnRows(rows(1))

	svc := NewService(e, nil, mock, stubFeedRepo{}, 5, 4000, 2, noop.NewTracerProvider().Tracer("test"))
	svc.BackfillOnce(context.Background(), 42)

	require.NoError(t, mock.ExpectationsWereMet())
}

// serviceTestRetriever is a tiny adapter for tests that want to
// stub the Retriever without using pgxmock. It implements the
// small surface rag.Service needs from Retriever (the Retrieve
// method) so the test can inject canned passages.
type serviceTestRetriever struct {
	passages []Passage
	recordK  func(k int)
}

// serviceTestRetrieverAdapter adapts the test stub to the
// internal Retriever shape rag.Service holds. We can't make
// rag.Service depend on a test-only interface, so the adapter
// implements the same single method via a wrapping type.
type serviceTestRetrieverAdapter struct {
	r *serviceTestRetriever
}

func (a *serviceTestRetrieverAdapter) Retrieve(_ context.Context, _ int, _ []float32, k int) ([]Passage, error) {
	if a.r.recordK != nil {
		a.r.recordK(k)
	}
	return a.r.passages, nil
}
