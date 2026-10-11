package tool

import (
	"bytes"
	"context"
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
