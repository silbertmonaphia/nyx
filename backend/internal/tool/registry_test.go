package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"nyx/internal/feed"
	"nyx/internal/reqctx"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// captureLogger swaps the global zerolog.Logger for one that writes
// into the returned buffer. Mirrors the helper in
// internal/platform/api/response_test.go so the safe-error
// contract tests can assert that the registry's safe detail
// reaches the log line.
//
// Tests using this helper MUST NOT call t.Parallel(); zerolog's
// global is process-wide.
func captureLogger(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := log.Logger
	log.Logger = zerolog.New(buf).Level(zerolog.WarnLevel)
	t.Cleanup(func() { log.Logger = prev })
	return buf
}

// ctxWithUser stamps the supplied user id on the context, mirroring
// what middleware.NewAuth does in production. The registry refuses
// to dispatch when this id is missing — see TestDispatch_RequiresOwner.
func ctxWithUser(uid int) context.Context {
	return reqctx.WithUserID(context.Background(), uid)
}

// oneofTestArgs is a dedicated struct for the oneof validation case
// so the test can exercise a single constraint in isolation (the
// production ListFeedsArgs falls back to desc on empty/unknown order,
// which the test doesn't want to mirror).
type oneofTestArgs struct {
	Order string `validate:"oneof=asc desc"`
}

// minTestArgs mirrors the min constraint in isolation — production
// ListFeedsArgs intentionally leaves Page / PageSize unconstrained
// (zero is "unspecified" → tool body applies defaults), so this
// dedicated struct lets the validation test exercise min without
// fighting the production contract.
type minTestArgs struct {
	N int `validate:"min=1"`
}

// --- 2.1: package skeleton ---

func TestRegistry_NewIsEmpty(t *testing.T) {
	r := New()
	assert.Empty(t, r.Names())
}

func TestRegistry_RegisterRejectsBadTool(t *testing.T) {
	cases := []struct {
		name string
		tool Tool
	}{
		{"empty name", Tool{Name: "", Dispatch: func(_ context.Context, _ any) (any, error) { return nil, nil }}},
		{"nil dispatch", Tool{Name: "x", ArgType: struct{}{}}},
		{"nil argtype", Tool{Name: "x", Dispatch: func(_ context.Context, _ any) (any, error) { return nil, nil }}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Panics(t, func() { New().Register(c.tool) })
		})
	}
}

// --- 2.2: per-owner scoping ---

// TestDispatch_RequiresOwner is the per-owner fail-closed gate. A
// dispatch without an authenticated user id on the context MUST
// return ErrMissingOwner before the tool body runs — even when the
// body is well-behaved. The body is instrumented to panic so a
// regression that lets it execute fails fast.
func TestDispatch_RequiresOwner(t *testing.T) {
	for _, c := range []struct {
		name string
		ctx  context.Context
	}{
		{"background", context.Background()},
		{"zero user id", reqctx.WithUserID(context.Background(), 0)},
	} {
		t.Run(c.name, func(t *testing.T) {
			called := false
			r := New()
			r.Register(Tool{
				Name:    "x",
				ArgType: struct{}{},
				Dispatch: func(ctx context.Context, _ any) (any, error) {
					called = true
					t.Fatal("body must NOT run when the user id is missing")
					return nil, nil
				},
			})

			_, err := r.Dispatch(c.ctx, "x", struct{}{})
			require.Error(t, err)
			assert.True(t, errors.Is(err, ErrMissingOwner))
			assert.False(t, called, "body must not have run")
		})
	}
}

// --- 2.3: input validation ---

// TestDispatch_RejectsInvalidArgs confirms the struct-tag validator
// runs BEFORE the tool body. The body is instrumented to panic so
// a regression that lets it execute after a validation failure
// fails fast. Each case ties a tag rule to the failing field name.
func TestDispatch_RejectsInvalidArgs(t *testing.T) {
	cases := []struct {
		name    string
		args    any
		wantErr string
	}{
		{
			name:    "required missing",
			args:    GetFeedArgs{}, // ID == 0
			wantErr: "ID",
		},
		{
			name:    "min violated",
			args:    minTestArgs{N: 0},
			wantErr: "N",
		},
		{
			name:    "max violated",
			args:    CreateFeedArgs{Title: string(make([]byte, 101))},
			wantErr: "Title",
		},
		{
			name:    "oneof violated",
			args:    oneofTestArgs{Order: "sideways"},
			wantErr: "Order",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := New()
			r.Register(Tool{
				Name:    "x",
				ArgType: c.args,
				Dispatch: func(_ context.Context, _ any) (any, error) {
					t.Fatal("body must NOT run when validation fails")
					return nil, nil
				},
			})

			_, err := r.Dispatch(ctxWithUser(1), "x", c.args)
			require.Error(t, err)
			assert.True(t, errors.Is(err, ErrInvalidArgs),
				"validation failure must wrap ErrInvalidArgs (got %v)", err)
			var fe *FieldError
			assert.True(t, errors.As(err, &fe),
				"validation failure must wrap a *FieldError so callers can render field-level details")
			require.NotNil(t, fe, "FieldError must not be nil when errors.As succeeds")
			assert.Equal(t, c.wantErr, fe.Field)
		})
	}
}

// --- 2.4: safe-error mapping ---

// TestDispatch_SafeErrorMapping covers the three leak paths:
//
//  1. sentinel pass-through — a tool returning feed.ErrNotFound must
//     surface through errors.Is so the MCP shim's existing mapping
//     code keeps working;
//  2. unknown error — the wrapped underlying error is logged with
//     a static safe detail (never the leaky text) and the dispatch
//     preserves the chain so the caller can decide what to render.
//     The safe detail lives in the log Msg;
//  3. panic — the registry recovers, logs the panic with a static
//     safe detail, and returns ErrPanic. The panic text is never
//     on the returned error.
func TestDispatch_SafeErrorMapping(t *testing.T) {
	t.Run("sentinel pass-through", func(t *testing.T) {
		buf := captureLogger(t)

		r := New()
		r.Register(Tool{
			Name:    "x",
			ArgType: struct{}{},
			Dispatch: func(_ context.Context, _ any) (any, error) {
				return nil, feed.ErrNotFound
			},
		})

		_, err := r.Dispatch(ctxWithUser(1), "x", struct{}{})
		require.Error(t, err)
		assert.True(t, errors.Is(err, feed.ErrNotFound),
			"sentinel must walk through errors.Is unchanged (got %v)", err)
		assert.Contains(t, buf.String(), "internal error suppressed from response details",
			"safe-detail log line must be emitted on the sentinel path; the registry records the call for audit even when the error is a known sentinel")
	})

	t.Run("unknown error logs safe detail", func(t *testing.T) {
		buf := captureLogger(t)

		r := New()
		original := errors.New("pgx: connection refused")
		r.Register(Tool{
			Name:    "x",
			ArgType: struct{}{},
			Dispatch: func(_ context.Context, _ any) (any, error) {
				return nil, original
			},
		})

		_, err := r.Dispatch(ctxWithUser(1), "x", struct{}{})
		require.Error(t, err)

		// The original error is logged at Warn so operators can
		// correlate. The static "tool error suppressed" detail is
		// the safe message — the underlying pgx text is recorded
		// but never returned to the caller as the response text.
		logged := buf.String()
		assert.Contains(t, logged, "internal error suppressed from response details",
			"static safe detail Msg must be present in the log line")
		assert.Contains(t, logged, "pgx: connection refused",
			"underlying error text is logged for operators")
		// The registry preserves the chain so the caller's mapping
		// can errors.Is against domain sentinels; an unknown error
		// is also preserved so a future caller can wrap or replace
		// it as needed.
		assert.True(t, errors.Is(err, original) || errors.Is(err, ErrInternal),
			"unknown error must be logged with safe detail; returned error either preserves the chain or is ErrInternal (got %v)", err)
	})

	t.Run("panic recovered + logged", func(t *testing.T) {
		buf := captureLogger(t)

		r := New()
		r.Register(Tool{
			Name:    "x",
			ArgType: struct{}{},
			Dispatch: func(_ context.Context, _ any) (any, error) {
				panic("internal pgx stack trace would go here")
			},
		})

		result, err := r.Dispatch(ctxWithUser(1), "x", struct{}{})
		require.Error(t, err)
		assert.Nil(t, result, "result must be nil on panic recovery")
		assert.True(t, errors.Is(err, ErrPanic),
			"panicking dispatch must return ErrPanic (got %v)", err)
		// The panic text is the leak. It MUST be in the log (for
		// operators) and MUST NOT appear in err.Error().
		logged := buf.String()
		assert.Contains(t, logged, "internal error suppressed from response details",
			"static safe detail Msg must be present in the panic log line")
		assert.Contains(t, logged, "internal pgx stack trace would go here",
			"panic value must be logged for operator correlation")
		assert.NotContains(t, err.Error(), "internal pgx stack trace",
			"panic text must NOT reach the returned error")
	})
}

// --- 2.5: registry lookup surface ---

// TestRegistry_NamesSortedAndUnique is the contract tools/list
// clients rely on: deterministic order across calls so a frontend
// diff doesn't flicker.
func TestRegistry_NamesSortedAndUnique(t *testing.T) {
	r := New()
	for _, n := range []string{"z", "a", "m", "b"} {
		r.Register(Tool{Name: n, ArgType: struct{}{}, Dispatch: func(_ context.Context, _ any) (any, error) { return nil, nil }})
	}
	assert.Equal(t, []string{"a", "b", "m", "z"}, r.Names())
}

// TestRegistry_ToolNotFound pins the lookup miss contract.
func TestRegistry_ToolNotFound(t *testing.T) {
	r := New()
	_, err := r.Dispatch(ctxWithUser(1), "ghost", struct{}{})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrToolNotFound))
}

// --- 2.7: concurrency under panic ---

// TestDispatch_OwnerScopingUnderPanic fires N concurrent goroutines,
// each with its own user id, against a tool that records the
// reqctx.UserIDFromContext it observed. One goroutine (user 99)
// drives a panic; every other goroutine's record is checked to
// confirm per-owner isolation held throughout the panic/recover
// cycle. The panicking dispatch returns ErrPanic; all others
// return their expected per-owner payload.
//
// The lock-step count via atomic.Int64 gives the test a way to
// detect a regression where the panic leaks past the deferred
// recover and tears down sibling goroutines — those would never
// reach their final assertion.
func TestDispatch_OwnerScopingUnderPanic(t *testing.T) {
	const N = 50
	const panicUser = 7

	type record struct {
		userID int
		ok     bool
	}
	var seen sync.Map

	r := New()
	r.Register(Tool{
		Name:    "x",
		ArgType: struct{}{},
		Dispatch: func(ctx context.Context, _ any) (any, error) {
			uid := reqctx.UserIDFromContext(ctx)
			if uid == panicUser {
				panic(fmt.Sprintf("simulated panic for user %d", uid))
			}
			seen.Store(uid, record{userID: uid, ok: true})
			return fmt.Sprintf("ok for user %d", uid), nil
		},
	})

	var wg sync.WaitGroup
	var panicCount atomic.Int64
	results := make([]error, N+1)
	ws := make([]any, N+1)

	for i := 0; i < N; i++ {
		uid := i + 1 // user id 0 would short-circuit on ErrMissingOwner
		wg.Add(1)
		go func(uid int) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					// The registry's recover() must catch every
					// panic — a leak here means the test harness
					// would unwind into t.Fatal, which is what
					// we want for a regression.
					t.Errorf("panic leaked out of Dispatch for user %d: %v", uid, r)
				}
			}()
			ctx := ctxWithUser(uid)
			w, e := r.Dispatch(ctx, "x", struct{}{})
			ws[uid] = w
			results[uid] = e
			if errors.Is(e, ErrPanic) {
				panicCount.Add(1)
			}
		}(uid)
	}
	wg.Wait()

	// Exactly one goroutine — user panicUser — must have observed
	// ErrPanic. Every other goroutine must have succeeded.
	assert.Equal(t, int64(1), panicCount.Load(),
		"exactly one panic (user %d) is expected; got %d",
		panicUser, panicCount.Load())
	for uid := 1; uid <= N; uid++ {
		if uid == panicUser {
			assert.True(t, errors.Is(results[uid], ErrPanic),
				"user %d must see ErrPanic (got %v)", uid, results[uid])
			assert.Nil(t, ws[uid])
			continue
		}
		require.NoError(t, results[uid], "user %d must succeed", uid)
		require.NotNil(t, ws[uid])
		assert.Equal(t, fmt.Sprintf("ok for user %d", uid), ws[uid])
		rec, ok := seen.Load(uid)
		require.True(t, ok, "user %d's body never recorded", uid)
		assert.Equal(t, uid, rec.(record).userID)
	}
}

// --- 2.5: feed tools smoke ---

// TestFeedTools_RegisteredWiresSurface pins the names of the five
// migrated feed tools so the agent (and the MCP shim) can rely on
// the contract. A regression that drops a tool or renames it
// without coordination fails here first.
func TestFeedTools_RegisteredWiresSurface(t *testing.T) {
	r := New()
	RegisterFeedTools(r, &stubFeedService{})

	got := map[string]bool{}
	for _, n := range r.Names() {
		got[n] = true
	}
	for _, want := range []string{
		ToolNameListFeeds, ToolNameGetFeed,
		ToolNameCreateFeed, ToolNameUpdateFeed, ToolNameDeleteFeed,
	} {
		assert.True(t, got[want], "tool %q must be registered", want)
	}
}

// TestFeedTools_PageSizeClampedTo100 verifies the clamp logic moved
// from the MCP shim into the registry tool body — same wire shape,
// same effective behaviour.
func TestFeedTools_PageSizeClampedTo100(t *testing.T) {
	feeds := &spyFeed{}
	r := New()
	RegisterFeedTools(r, feeds)

	_, err := r.Dispatch(ctxWithUser(1), ToolNameListFeeds, ListFeedsArgs{Page: 1, PageSize: 500, Order: "asc"})
	require.NoError(t, err)
	assert.Equal(t, 100, feeds.lastListSize)
}

// TestFeedTools_CreateRequiresOwner confirms the per-owner gate
// runs before any work — a dispatch without user id must return
// ErrMissingOwner and the feed service must NOT see the call.
func TestFeedTools_CreateRequiresOwner(t *testing.T) {
	feeds := &spyFeed{}
	r := New()
	RegisterFeedTools(r, feeds)

	_, err := r.Dispatch(context.Background(), ToolNameCreateFeed,
		CreateFeedArgs{Title: "x"})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrMissingOwner))
	assert.Equal(t, 0, feeds.createCalls, "feed.CreateFeed must not have run")
}

// --- fakes for the feed integration ---

// stubFeedService is the smallest possible feed.Service
// implementation — the integration tests above only care that the
// tool body delegates to the service without caring about the
// service's return value.
type stubFeedService struct{}

func (stubFeedService) GetFeeds(context.Context, string, int, int, feed.SortOrder) (*feed.Page, error) {
	return &feed.Page{}, nil
}
func (stubFeedService) GetFeedByID(context.Context, int) (*feed.Feed, error) {
	return &feed.Feed{}, nil
}
func (stubFeedService) CreateFeed(context.Context, *feed.Feed) error      { return nil }
func (stubFeedService) UpdateFeed(context.Context, int, *feed.Feed) error { return nil }
func (stubFeedService) DeleteFeed(context.Context, int) error             { return nil }
func (stubFeedService) CheckHealth(context.Context) error                 { return nil }
func (stubFeedService) CheckCacheHealth(context.Context) error            { return nil }
func (stubFeedService) CacheMode() string                                 { return "noop" }
func (stubFeedService) SetIndexer(feed.EmbeddingIndexer)                  {}

// spyFeed records the last args it saw so the smoke tests can
// assert what the registry forwarded.
type spyFeed struct {
	stubFeedService
	lastListSize  int
	createCalls   int
	lastCreateUID int
}

func (s *spyFeed) GetFeeds(_ context.Context, _ string, _, size int, _ feed.SortOrder) (*feed.Page, error) {
	s.lastListSize = size
	return &feed.Page{}, nil
}

func (s *spyFeed) CreateFeed(_ context.Context, m *feed.Feed) error {
	s.createCalls++
	s.lastCreateUID = m.UserID
	return nil
}

// --- dispatch bodies: defaults, results, error pass-through ---

// recordingFeed is the full-surface fake for the dispatch-body tests:
// it records every call's arguments and returns programmable errors,
// so one fake drives all five tools' success and failure paths (the
// closures in feeds.go are otherwise only reached through the two
// smoke tests above).
type recordingFeed struct {
	stubFeedService

	listErr, getErr, createErr, updateErr, deleteErr error

	lastQuery  string
	lastPage   int
	lastListSz int
	lastOrder  feed.SortOrder

	lastGetID    int
	lastCreate   *feed.Feed
	lastUpdateID int
	lastUpdate   *feed.Feed
	lastDeleteID int
}

func (f *recordingFeed) GetFeeds(_ context.Context, q string, page, size int, order feed.SortOrder) (*feed.Page, error) {
	f.lastQuery, f.lastPage, f.lastListSz, f.lastOrder = q, page, size, order
	if f.listErr != nil {
		return nil, f.listErr
	}
	return &feed.Page{Items: []feed.Feed{{ID: 9, Title: "t"}}, Page: page, PageSize: size, Total: 1}, nil
}

func (f *recordingFeed) GetFeedByID(_ context.Context, id int) (*feed.Feed, error) {
	f.lastGetID = id
	if f.getErr != nil {
		return nil, f.getErr
	}
	return &feed.Feed{ID: id, Title: "fetched"}, nil
}

func (f *recordingFeed) CreateFeed(_ context.Context, m *feed.Feed) error {
	f.lastCreate = m
	if f.createErr != nil {
		return f.createErr
	}
	m.ID = 42 // server-generated id, as the real repository does
	return nil
}

func (f *recordingFeed) UpdateFeed(_ context.Context, id int, m *feed.Feed) error {
	f.lastUpdateID, f.lastUpdate = id, m
	if f.updateErr != nil {
		return f.updateErr
	}
	m.ID = id
	return nil
}

func (f *recordingFeed) DeleteFeed(_ context.Context, id int) error {
	f.lastDeleteID = id
	return f.deleteErr
}

// TestFeedTools_ListDefaults pins the normalisation contract the doc
// comment on ListFeedsArgs promises: zero AND negative page /
// page_size fall back to 1 / 20 inside the tool body (they are
// deliberately NOT `min` validation). The >100 clamp is pinned by
// TestFeedTools_PageSizeClampedTo100.
func TestFeedTools_ListDefaults(t *testing.T) {
	for _, c := range []struct {
		name     string
		args     ListFeedsArgs
		wantPage int
		wantSize int
	}{
		{"zero", ListFeedsArgs{}, 1, 20},
		{"negative", ListFeedsArgs{Page: -3, PageSize: -7}, 1, 20},
	} {
		t.Run(c.name, func(t *testing.T) {
			feeds := &recordingFeed{}
			r := New()
			RegisterFeedTools(r, feeds)

			got, err := r.Dispatch(ctxWithUser(1), ToolNameListFeeds, c.args)
			require.NoError(t, err)
			assert.Equal(t, c.wantPage, feeds.lastPage)
			assert.Equal(t, c.wantSize, feeds.lastListSz)
			assert.Equal(t, feed.SortDesc, feeds.lastOrder,
				"empty / unknown order must normalise to desc at the tool body")

			page, ok := got.(feed.FeedsPage)
			require.True(t, ok, "list result must be the pagination envelope, got %T", got)
			assert.Equal(t, c.wantPage, page.Page)
			assert.Equal(t, c.wantSize, page.PageSize)
		})
	}
}

// TestFeedTools_ListForwardsQuery confirms the search string reaches
// the service untouched (no trimming / no normalisation beyond page
// bounds).
func TestFeedTools_ListForwardsQuery(t *testing.T) {
	feeds := &recordingFeed{}
	r := New()
	RegisterFeedTools(r, feeds)

	_, err := r.Dispatch(ctxWithUser(1), ToolNameListFeeds, ListFeedsArgs{Query: "rust news", Order: "asc"})
	require.NoError(t, err)
	assert.Equal(t, "rust news", feeds.lastQuery)
	assert.Equal(t, feed.SortAsc, feeds.lastOrder)
}

// TestFeedTools_ListServiceError — the list error branch: the
// service's error reaches the caller through errors.Is unchanged.
// (Dispatch logs it via ClassifyAndLog but preserves the chain — the
// "log + return original" contract documented on Registry.Dispatch.)
func TestFeedTools_ListServiceError(t *testing.T) {
	captureLogger(t)
	boom := errors.New("pgx: select boom")
	feeds := &recordingFeed{listErr: boom}
	r := New()
	RegisterFeedTools(r, feeds)

	got, err := r.Dispatch(ctxWithUser(1), ToolNameListFeeds, ListFeedsArgs{})
	require.Error(t, err)
	assert.Nil(t, got)
	assert.True(t, errors.Is(err, boom), "list error chain must be preserved (got %v)", err)
}

// TestFeedTools_GetFeed pins the read path: id forwarded, result
// returned as the service's *feed.Feed, and feed.ErrNotFound passes
// through errors.Is so the caller's wire mapping keeps working.
func TestFeedTools_GetFeed(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		feeds := &recordingFeed{}
		r := New()
		RegisterFeedTools(r, feeds)

		got, err := r.Dispatch(ctxWithUser(1), ToolNameGetFeed, GetFeedArgs{ID: 11})
		require.NoError(t, err)
		assert.Equal(t, 11, feeds.lastGetID)
		feedRow, ok := got.(*feed.Feed)
		require.True(t, ok, "get result must be *feed.Feed, got %T", got)
		assert.Equal(t, 11, feedRow.ID)
	})

	t.Run("sentinel passes through", func(t *testing.T) {
		captureLogger(t)
		feeds := &recordingFeed{getErr: feed.ErrNotFound}
		r := New()
		RegisterFeedTools(r, feeds)

		_, err := r.Dispatch(ctxWithUser(1), ToolNameGetFeed, GetFeedArgs{ID: 404})
		require.Error(t, err)
		assert.True(t, errors.Is(err, feed.ErrNotFound),
			"domain sentinel must pass through errors.Is (got %v)", err)
	})
}

// TestFeedTools_CreateFeed pins the owner stamp: the body reads the
// user id from the context (NOT from args) and the returned value is
// the service-mutated record (with its server-generated id).
func TestFeedTools_CreateFeed(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		feeds := &recordingFeed{}
		r := New()
		RegisterFeedTools(r, feeds)

		got, err := r.Dispatch(ctxWithUser(7), ToolNameCreateFeed,
			CreateFeedArgs{Title: "t", Description: "d"})
		require.NoError(t, err)
		require.NotNil(t, feeds.lastCreate)
		assert.Equal(t, 7, feeds.lastCreate.UserID, "owner must come from the context")
		assert.Equal(t, "t", feeds.lastCreate.Title)

		created, ok := got.(feed.Feed)
		require.True(t, ok, "create result must be feed.Feed, got %T", got)
		assert.Equal(t, 42, created.ID, "result must carry the service-generated id")
		assert.Equal(t, 7, created.UserID)
	})

	t.Run("service error", func(t *testing.T) {
		captureLogger(t)
		boom := errors.New("pgx: insert boom")
		feeds := &recordingFeed{createErr: boom}
		r := New()
		RegisterFeedTools(r, feeds)

		got, err := r.Dispatch(ctxWithUser(7), ToolNameCreateFeed, CreateFeedArgs{Title: "t"})
		require.Error(t, err)
		assert.Nil(t, got)
		// Stricter than the older SafeErrorMapping test, deliberately:
		// Registry.Dispatch's documented contract is "log via
		// ClassifyAndLog, return the original chain", so pinning
		// errors.Is here freezes the contract at the tool-body boundary.
		assert.True(t, errors.Is(err, boom), "underlying error chain must be preserved")
	})
}

// TestFeedTools_UpdateFeed — same owner-stamp + result contract as
// create, plus the id forwarding shape (id is a separate argument).
func TestFeedTools_UpdateFeed(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		feeds := &recordingFeed{}
		r := New()
		RegisterFeedTools(r, feeds)

		got, err := r.Dispatch(ctxWithUser(7), ToolNameUpdateFeed,
			UpdateFeedArgs{ID: 5, Title: "new"})
		require.NoError(t, err)
		assert.Equal(t, 5, feeds.lastUpdateID)
		require.NotNil(t, feeds.lastUpdate)
		assert.Equal(t, 7, feeds.lastUpdate.UserID)
		assert.Equal(t, "new", feeds.lastUpdate.Title)

		updated, ok := got.(feed.Feed)
		require.True(t, ok, "update result must be feed.Feed, got %T", got)
		assert.Equal(t, 5, updated.ID)
	})

	t.Run("sentinel passes through", func(t *testing.T) {
		captureLogger(t)
		feeds := &recordingFeed{updateErr: feed.ErrNotFound}
		r := New()
		RegisterFeedTools(r, feeds)

		_, err := r.Dispatch(ctxWithUser(7), ToolNameUpdateFeed, UpdateFeedArgs{ID: 404, Title: "x"})
		require.Error(t, err)
		assert.True(t, errors.Is(err, feed.ErrNotFound), "got %v", err)
	})
}

// TestFeedTools_DeleteFeed pins the wire shape of the delete result
// ({deleted, id}) via JSON — the envelope is an anonymous struct, so
// marshalling is the contract-faithful assertion.
func TestFeedTools_DeleteFeed(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		feeds := &recordingFeed{}
		r := New()
		RegisterFeedTools(r, feeds)

		got, err := r.Dispatch(ctxWithUser(1), ToolNameDeleteFeed, DeleteFeedArgs{ID: 5})
		require.NoError(t, err)
		assert.Equal(t, 5, feeds.lastDeleteID)

		raw, err := json.Marshal(got)
		require.NoError(t, err)
		assert.JSONEq(t, `{"deleted": true, "id": 5}`, string(raw))
	})

	t.Run("sentinel passes through", func(t *testing.T) {
		captureLogger(t)
		feeds := &recordingFeed{deleteErr: feed.ErrNotFound}
		r := New()
		RegisterFeedTools(r, feeds)

		_, err := r.Dispatch(ctxWithUser(1), ToolNameDeleteFeed, DeleteFeedArgs{ID: 404})
		require.Error(t, err)
		assert.True(t, errors.Is(err, feed.ErrNotFound), "got %v", err)
	})
}

// TestIsKnownSentinel pins the exported classifier for
// registry-internal sentinels (the MCP shim currently keeps its own
// switch instead of calling this, so the test pins the exported
// contract directly). All five sentinels match — including when
// wrapped; domain sentinels and unrelated errors do not.
func TestIsKnownSentinel(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"missing owner", ErrMissingOwner, true},
		{"invalid args", ErrInvalidArgs, true},
		{"tool not found", ErrToolNotFound, true},
		{"panic", ErrPanic, true},
		{"internal", ErrInternal, true},
		{"wrapped sentinel", fmt.Errorf("outer: %w", ErrInvalidArgs), true},
		{"domain sentinel", feed.ErrNotFound, false},
		{"unrelated error", errors.New("boom"), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, IsKnownSentinel(c.err))
		})
	}
}
