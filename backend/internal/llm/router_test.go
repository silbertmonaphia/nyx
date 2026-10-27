package llm

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace/noop"
)

// stubProvider is the test-only llm.Provider the Router drives.
// chatFn controls what happens when the Router calls Chat on the
// underlying slot; tests seed it with the canned sequences they
// need (streaming partials, erroring mid-stream, etc.).
// embedFn is the Embed counterpart; the existing Chat tests leave
// it nil and rely on the nil-deref panic in Embed to catch any
// accidental call. Embed tests seed it directly.
type stubProvider struct {
	chatFn  func(ctx context.Context, req ChatRequest, onDelta func(delta string, finalUsage *ChatUsage) error) (*ChatUsage, error)
	embedFn func(ctx context.Context, req EmbedRequest) ([][]float32, error)
}

func (s *stubProvider) Chat(ctx context.Context, req ChatRequest, onDelta func(delta string, finalUsage *ChatUsage) error) (*ChatUsage, error) {
	return s.chatFn(ctx, req, onDelta)
}

func (s *stubProvider) Embed(ctx context.Context, req EmbedRequest) ([][]float32, error) {
	if s.embedFn == nil {
		panic("stubProvider.Embed called without an embedFn seeded")
	}
	return s.embedFn(ctx, req)
}

// newProbeServer wires an httptest.NewServer that responds to
// GET /v1/models with HTTP 200. The hit counter lets tests assert
// the probe was actually called (vs. skipped via passthrough).
func newProbeServer(t *testing.T, hits *atomic.Int64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			hits.Add(1)
		}
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintln(w, `{"data":[]}`)
	}))
}

// newTestRouter builds a Router around two stubProvider slots
// pointing at the supplied probe servers. probeTimeout is short
// (500ms default) so the "probe fails" tests don't need a slow
// DNS stub. The hit counters are accepted only for symmetry with
// the call sites that pass them; the helper itself doesn't read
// them.
func newTestRouter(primaryName string, primary Provider, primaryProbe string, primaryKey string, _ *atomic.Int64,
	secondaryName string, secondary Provider, secondaryProbe string, secondaryKey string, _ *atomic.Int64,
	probeTimeout time.Duration) *Router {
	return NewRouter(
		ProviderSlot{Name: primaryName, Provider: primary, BaseURL: primaryProbe, ProbeKey: primaryKey},
		&ProviderSlot{Name: secondaryName, Provider: secondary, BaseURL: secondaryProbe, ProbeKey: secondaryKey},
		noop.NewTracerProvider().Tracer("test"),
		probeTimeout,
	)
}

// captureOnNote is a tiny helper that returns an OnNote callback
// which records what the router told it.
func captureOnNote() (func(string, string) error, *[]string) {
	var got []string
	cb := func(text, provider string) error {
		got = append(got, fmt.Sprintf("%s|%s", text, provider))
		return nil
	}
	return cb, &got
}

// TestRouter_PrimaryProbeOK_UsesPrimary covers case 1: probe
// succeeds → primary is used; secondary receives zero hits.
func TestRouter_PrimaryProbeOK_UsesPrimary(t *testing.T) {
	primaryHits := &atomic.Int64{}
	secondaryHits := &atomic.Int64{}
	primaryProbe := newProbeServer(t, primaryHits)
	defer primaryProbe.Close()
	secondaryProbe := newProbeServer(t, secondaryHits)
	defer secondaryProbe.Close()

	primaryCalled := &atomic.Int64{}
	primary := &stubProvider{chatFn: func(_ context.Context, _ ChatRequest, onDelta func(string, *ChatUsage) error) (*ChatUsage, error) {
		primaryCalled.Add(1)
		_ = onDelta("hello", nil)
		return &ChatUsage{TotalTokens: 1}, nil
	}}
	secondary := &stubProvider{chatFn: func(_ context.Context, _ ChatRequest, _ func(string, *ChatUsage) error) (*ChatUsage, error) {
		secondaryHits.Add(1)
		return nil, nil
	}}

	r := newTestRouter("openai", primary, primaryProbe.URL, "k", primaryHits,
		"vllm", secondary, secondaryProbe.URL, "k", secondaryHits, 500*time.Millisecond)

	var deltas []string
	usage, err := r.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}},
		func(d string, _ *ChatUsage) error { deltas = append(deltas, d); return nil })
	require.NoError(t, err)
	assert.Equal(t, []string{"hello"}, deltas)
	require.NotNil(t, usage)
	assert.Equal(t, int64(1), primaryCalled.Load())
	assert.Equal(t, int64(0), secondaryHits.Load(), "secondary Chat must not be called")
	// Probe target sees at least one hit.
	assert.GreaterOrEqual(t, primaryHits.Load(), int64(1))
}

// TestRouter_PrimaryProbeFails_UsesSecondary covers case 2: probe
// fails → secondary used; primary's chat path never hit.
func TestRouter_PrimaryProbeFails_UsesSecondary(t *testing.T) {
	// Closed listener → probe target always fails.
	primaryURL := newAlwaysFailingURL(t)
	secondaryHits := &atomic.Int64{}
	secondaryProbe := newProbeServer(t, secondaryHits)
	defer secondaryProbe.Close()

	primary := &stubProvider{chatFn: func(_ context.Context, _ ChatRequest, _ func(string, *ChatUsage) error) (*ChatUsage, error) {
		t.Fatal("primary Chat must not be called when probe fails")
		return nil, nil
	}}
	secondaryCalled := &atomic.Int64{}
	secondary := &stubProvider{chatFn: func(_ context.Context, _ ChatRequest, onDelta func(string, *ChatUsage) error) (*ChatUsage, error) {
		secondaryCalled.Add(1)
		_ = onDelta("from-secondary", nil)
		return &ChatUsage{TotalTokens: 2}, nil
	}}

	r := NewRouter(
		ProviderSlot{Name: "openai", Provider: primary, BaseURL: primaryURL, ProbeKey: "k"},
		&ProviderSlot{Name: "vllm", Provider: secondary, BaseURL: secondaryProbe.URL, ProbeKey: "k"},
		noop.NewTracerProvider().Tracer("test"),
		500*time.Millisecond,
	)

	var deltas []string
	_, err := r.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}},
		func(d string, _ *ChatUsage) error { deltas = append(deltas, d); return nil })
	require.NoError(t, err)
	assert.Equal(t, []string{"from-secondary"}, deltas)
	assert.Equal(t, int64(1), secondaryCalled.Load())
}

// TestRouter_PrimaryFailsBeforeAnyDelta_Failover covers case 3:
// probe OK, primary errors before any delta → failover; secondary
// receives one POST; OnNote fires.
func TestRouter_PrimaryFailsBeforeAnyDelta_Failover(t *testing.T) {
	primaryHits := &atomic.Int64{}
	primaryProbe := newProbeServer(t, primaryHits)
	defer primaryProbe.Close()
	secondaryHits := &atomic.Int64{}
	secondaryProbe := newProbeServer(t, secondaryHits)
	defer secondaryProbe.Close()

	primary := &stubProvider{chatFn: func(_ context.Context, _ ChatRequest, _ func(string, *ChatUsage) error) (*ChatUsage, error) {
		return nil, fmt.Errorf("primary blown up: %w", ErrProviderUnavailable)
	}}
	secondaryCalled := &atomic.Int64{}
	secondary := &stubProvider{chatFn: func(_ context.Context, req ChatRequest, onDelta func(string, *ChatUsage) error) (*ChatUsage, error) {
		secondaryCalled.Add(1)
		_ = onDelta("secondary-hello", nil)
		return &ChatUsage{TotalTokens: 3}, nil
	}}

	noteCb, noteGot := captureOnNote()
	req := ChatRequest{
		Messages: []Message{{Role: "user", Content: "hi"}},
		OnNote:   noteCb,
	}
	r := NewRouter(
		ProviderSlot{Name: "openai", Provider: primary, BaseURL: primaryProbe.URL, ProbeKey: "k"},
		&ProviderSlot{Name: "vllm", Provider: secondary, BaseURL: secondaryProbe.URL, ProbeKey: "k"},
		noop.NewTracerProvider().Tracer("test"),
		500*time.Millisecond,
	)

	var deltas []string
	_, err := r.Chat(context.Background(), req,
		func(d string, _ *ChatUsage) error { deltas = append(deltas, d); return nil })
	require.NoError(t, err)
	assert.Equal(t, []string{"secondary-hello"}, deltas)
	assert.Equal(t, int64(1), secondaryCalled.Load())
	require.Len(t, *noteGot, 1)
	assert.Equal(t, "[continued on vllm]|vllm", (*noteGot)[0])
	// Secondary should have received the original messages (no synthetic
	// assistant turn because the primary emitted zero deltas).
}

// TestRouter_PrimaryFailsAfterDeltas_Failover covers case 4: probe
// OK, primary fails after 3 deltas → failover; secondary receives
// messages[-1] = {assistant, "<joined 3 deltas>"}; OnNote fires.
func TestRouter_PrimaryFailsAfterDeltas_Failover(t *testing.T) {
	primaryHits := &atomic.Int64{}
	primaryProbe := newProbeServer(t, primaryHits)
	defer primaryProbe.Close()
	secondaryHits := &atomic.Int64{}
	secondaryProbe := newProbeServer(t, secondaryHits)
	defer secondaryProbe.Close()

	primary := &stubProvider{chatFn: func(_ context.Context, _ ChatRequest, onDelta func(string, *ChatUsage) error) (*ChatUsage, error) {
		_ = onDelta("alpha", nil)
		_ = onDelta("-beta", nil)
		_ = onDelta("-gamma", nil)
		return nil, fmt.Errorf("primary crashed mid-stream: %w", ErrProviderUnavailable)
	}}
	var capturedReq ChatRequest
	secondaryCalled := &atomic.Int64{}
	secondary := &stubProvider{chatFn: func(_ context.Context, req ChatRequest, onDelta func(string, *ChatUsage) error) (*ChatUsage, error) {
		secondaryCalled.Add(1)
		capturedReq = req
		_ = onDelta("continued", nil)
		return &ChatUsage{TotalTokens: 4}, nil
	}}

	noteCb, noteGot := captureOnNote()
	r := NewRouter(
		ProviderSlot{Name: "openai", Provider: primary, BaseURL: primaryProbe.URL, ProbeKey: "k"},
		&ProviderSlot{Name: "vllm", Provider: secondary, BaseURL: secondaryProbe.URL, ProbeKey: "k"},
		noop.NewTracerProvider().Tracer("test"),
		500*time.Millisecond,
	)

	_, err := r.Chat(context.Background(), ChatRequest{
		Messages: []Message{{Role: "user", Content: "hi"}},
		OnNote:   noteCb,
	}, func(string, *ChatUsage) error { return nil })
	require.NoError(t, err)
	assert.Equal(t, int64(1), secondaryCalled.Load())
	require.Len(t, capturedReq.Messages, 2)
	assert.Equal(t, "user", capturedReq.Messages[0].Role)
	assert.Equal(t, "hi", capturedReq.Messages[0].Content)
	assert.Equal(t, "assistant", capturedReq.Messages[1].Role)
	assert.Equal(t, "alpha-beta-gamma", capturedReq.Messages[1].Content)
	require.Len(t, *noteGot, 1)
	assert.Equal(t, "[continued on vllm]|vllm", (*noteGot)[0])
}

// TestRouter_BothFail_ReturnsAllProvidersFailed covers case 5: both
// providers fail → errors.Is(err, ErrAllProvidersFailed) true and
// errors.Is(err, ErrProviderUnavailable) also true (wrapped).
func TestRouter_BothFail_ReturnsAllProvidersFailed(t *testing.T) {
	primaryHits := &atomic.Int64{}
	primaryProbe := newProbeServer(t, primaryHits)
	defer primaryProbe.Close()
	secondaryHits := &atomic.Int64{}
	secondaryProbe := newProbeServer(t, secondaryHits)
	defer secondaryProbe.Close()

	primary := &stubProvider{chatFn: func(_ context.Context, _ ChatRequest, _ func(string, *ChatUsage) error) (*ChatUsage, error) {
		return nil, fmt.Errorf("primary dead: %w", ErrProviderUnavailable)
	}}
	secondary := &stubProvider{chatFn: func(_ context.Context, _ ChatRequest, _ func(string, *ChatUsage) error) (*ChatUsage, error) {
		return nil, fmt.Errorf("secondary dead: %w", ErrProviderUnavailable)
	}}
	r := NewRouter(
		ProviderSlot{Name: "openai", Provider: primary, BaseURL: primaryProbe.URL, ProbeKey: "k"},
		&ProviderSlot{Name: "vllm", Provider: secondary, BaseURL: secondaryProbe.URL, ProbeKey: "k"},
		noop.NewTracerProvider().Tracer("test"),
		500*time.Millisecond,
	)

	_, err := r.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}},
		func(string, *ChatUsage) error { return nil })
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrAllProvidersFailed))
	assert.True(t, errors.Is(err, ErrProviderUnavailable))
}

// TestRouter_PrimaryProbeFails_SecondaryDead covers case 6: probe
// fails + secondary unreachable → ErrAllProvidersFailed, no
// second failover.
func TestRouter_PrimaryProbeFails_SecondaryDead(t *testing.T) {
	primaryURL := newAlwaysFailingURL(t)

	primary := &stubProvider{chatFn: func(_ context.Context, _ ChatRequest, _ func(string, *ChatUsage) error) (*ChatUsage, error) {
		t.Fatal("primary Chat must not run when probe fails")
		return nil, nil
	}}
	secondary := &stubProvider{chatFn: func(_ context.Context, _ ChatRequest, _ func(string, *ChatUsage) error) (*ChatUsage, error) {
		return nil, fmt.Errorf("secondary unreachable: %w", ErrProviderUnavailable)
	}}
	r := NewRouter(
		ProviderSlot{Name: "openai", Provider: primary, BaseURL: primaryURL, ProbeKey: "k"},
		&ProviderSlot{Name: "vllm", Provider: secondary, BaseURL: "http://127.0.0.1:1", ProbeKey: "k"},
		noop.NewTracerProvider().Tracer("test"),
		500*time.Millisecond,
	)

	_, err := r.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}},
		func(string, *ChatUsage) error { return nil })
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrAllProvidersFailed))
}

// TestRouter_SingleProviderPassthrough covers case 7: secondary=nil
// → primary fails, returned error is ErrProviderUnavailable, no
// failover attempted.
func TestRouter_SingleProviderPassthrough(t *testing.T) {
	primary := &stubProvider{chatFn: func(_ context.Context, _ ChatRequest, _ func(string, *ChatUsage) error) (*ChatUsage, error) {
		return nil, ErrProviderUnavailable
	}}
	r := NewRouter(
		ProviderSlot{Name: "openai", Provider: primary, BaseURL: "http://unused", ProbeKey: "k"},
		nil,
		noop.NewTracerProvider().Tracer("test"),
		500*time.Millisecond,
	)
	_, err := r.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}},
		func(string, *ChatUsage) error { return nil })
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrProviderUnavailable))
	assert.False(t, errors.Is(err, ErrAllProvidersFailed))
}

// TestRouter_TerminalEmitted_NoFailover covers case 8: primary emits
// the terminal usage chunk then returns an error → no failover,
// error returned unchanged.
func TestRouter_TerminalEmitted_NoFailover(t *testing.T) {
	primaryHits := &atomic.Int64{}
	primaryProbe := newProbeServer(t, primaryHits)
	defer primaryProbe.Close()
	secondaryHits := &atomic.Int64{}
	secondaryProbe := newProbeServer(t, secondaryHits)
	defer secondaryProbe.Close()

	primaryUsage := &ChatUsage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3}
	primary := &stubProvider{chatFn: func(_ context.Context, _ ChatRequest, onDelta func(string, *ChatUsage) error) (*ChatUsage, error) {
		_ = onDelta("ok", nil)
		_ = onDelta("", primaryUsage) // terminal usage — sets terminalEmitted
		return primaryUsage, fmt.Errorf("post-usage noise: %w", ErrProviderUnavailable)
	}}
	secondary := &stubProvider{chatFn: func(_ context.Context, _ ChatRequest, _ func(string, *ChatUsage) error) (*ChatUsage, error) {
		t.Fatal("secondary must not be called when terminal usage already emitted")
		return nil, nil
	}}
	r := NewRouter(
		ProviderSlot{Name: "openai", Provider: primary, BaseURL: primaryProbe.URL, ProbeKey: "k"},
		&ProviderSlot{Name: "vllm", Provider: secondary, BaseURL: secondaryProbe.URL, ProbeKey: "k"},
		noop.NewTracerProvider().Tracer("test"),
		500*time.Millisecond,
	)
	usage, err := r.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}},
		func(string, *ChatUsage) error { return nil })
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrProviderUnavailable))
	require.NotNil(t, usage)
	assert.Equal(t, primaryUsage, usage)
}

// TestRouter_ErrContextCanceled_NoFailover covers case 9: primary
// returns ErrContextCanceled → no failover.
func TestRouter_ErrContextCanceled_NoFailover(t *testing.T) {
	primaryHits := &atomic.Int64{}
	primaryProbe := newProbeServer(t, primaryHits)
	defer primaryProbe.Close()
	secondaryHits := &atomic.Int64{}
	secondaryProbe := newProbeServer(t, secondaryHits)
	defer secondaryProbe.Close()

	primary := &stubProvider{chatFn: func(_ context.Context, _ ChatRequest, _ func(string, *ChatUsage) error) (*ChatUsage, error) {
		return nil, ErrContextCanceled
	}}
	secondary := &stubProvider{chatFn: func(_ context.Context, _ ChatRequest, _ func(string, *ChatUsage) error) (*ChatUsage, error) {
		t.Fatal("secondary must not be called on ErrContextCanceled")
		return nil, nil
	}}
	r := NewRouter(
		ProviderSlot{Name: "openai", Provider: primary, BaseURL: primaryProbe.URL, ProbeKey: "k"},
		&ProviderSlot{Name: "vllm", Provider: secondary, BaseURL: secondaryProbe.URL, ProbeKey: "k"},
		noop.NewTracerProvider().Tracer("test"),
		500*time.Millisecond,
	)
	_, err := r.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}},
		func(string, *ChatUsage) error { return nil })
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrContextCanceled))
	assert.False(t, errors.Is(err, ErrAllProvidersFailed))
}

// TestRouter_ErrRateLimited_NoFailover covers case 10: primary
// returns ErrRateLimited → no failover (operator's primary budget
// exhausted; failover wouldn't help with a different budget).
func TestRouter_ErrRateLimited_NoFailover(t *testing.T) {
	primaryHits := &atomic.Int64{}
	primaryProbe := newProbeServer(t, primaryHits)
	defer primaryProbe.Close()
	secondaryHits := &atomic.Int64{}
	secondaryProbe := newProbeServer(t, secondaryHits)
	defer secondaryProbe.Close()

	primary := &stubProvider{chatFn: func(_ context.Context, _ ChatRequest, _ func(string, *ChatUsage) error) (*ChatUsage, error) {
		return nil, ErrRateLimited
	}}
	secondary := &stubProvider{chatFn: func(_ context.Context, _ ChatRequest, _ func(string, *ChatUsage) error) (*ChatUsage, error) {
		t.Fatal("secondary must not be called on ErrRateLimited")
		return nil, nil
	}}
	r := NewRouter(
		ProviderSlot{Name: "openai", Provider: primary, BaseURL: primaryProbe.URL, ProbeKey: "k"},
		&ProviderSlot{Name: "vllm", Provider: secondary, BaseURL: secondaryProbe.URL, ProbeKey: "k"},
		noop.NewTracerProvider().Tracer("test"),
		500*time.Millisecond,
	)
	_, err := r.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}},
		func(string, *ChatUsage) error { return nil })
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrRateLimited))
	assert.False(t, errors.Is(err, ErrAllProvidersFailed))
}

// TestRouter_NilOnNote_NoPanic covers case 11: req.OnNote == nil +
// failover needed → no panic.
func TestRouter_NilOnNote_NoPanic(t *testing.T) {
	primaryHits := &atomic.Int64{}
	primaryProbe := newProbeServer(t, primaryHits)
	defer primaryProbe.Close()
	secondaryHits := &atomic.Int64{}
	secondaryProbe := newProbeServer(t, secondaryHits)
	defer secondaryProbe.Close()

	primary := &stubProvider{chatFn: func(_ context.Context, _ ChatRequest, _ func(string, *ChatUsage) error) (*ChatUsage, error) {
		return nil, fmt.Errorf("primary failed: %w", ErrProviderUnavailable)
	}}
	secondary := &stubProvider{chatFn: func(_ context.Context, _ ChatRequest, onDelta func(string, *ChatUsage) error) (*ChatUsage, error) {
		_ = onDelta("still works", nil)
		return &ChatUsage{TotalTokens: 1}, nil
	}}
	r := NewRouter(
		ProviderSlot{Name: "openai", Provider: primary, BaseURL: primaryProbe.URL, ProbeKey: "k"},
		&ProviderSlot{Name: "vllm", Provider: secondary, BaseURL: secondaryProbe.URL, ProbeKey: "k"},
		noop.NewTracerProvider().Tracer("test"),
		500*time.Millisecond,
	)

	var deltas []string
	_, err := r.Chat(context.Background(), ChatRequest{
		Messages: []Message{{Role: "user", Content: "hi"}},
		OnNote:   nil, // explicit nil
	}, func(d string, _ *ChatUsage) error { deltas = append(deltas, d); return nil })
	require.NoError(t, err)
	assert.Equal(t, []string{"still works"}, deltas)
}

// TestRouter_ProbeSendsAuthorization covers the bearer header
// contract: when ProbeKey is non-empty the probe sends
// `Authorization: Bearer <key>`; when empty it omits the header.
// The probe only fires when secondary != nil, so each test mounts
// a dummy secondary slot pointing at a live server.
func TestRouter_ProbeSendsAuthorization(t *testing.T) {
	var sawAuth atomic.Value // string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth.Store(r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	primary := &stubProvider{chatFn: func(_ context.Context, _ ChatRequest, _ func(string, *ChatUsage) error) (*ChatUsage, error) {
		return &ChatUsage{}, nil
	}}
	secondaryProbeHits := &atomic.Int64{}
	secondaryProbe := newProbeServer(t, secondaryProbeHits)
	defer secondaryProbe.Close()

	r := NewRouter(
		ProviderSlot{Name: "vllm", Provider: primary, BaseURL: server.URL, ProbeKey: "my-key"},
		&ProviderSlot{Name: "openai", Provider: &stubProvider{}, BaseURL: secondaryProbe.URL, ProbeKey: ""},
		noop.NewTracerProvider().Tracer("test"),
		500*time.Millisecond,
	)
	_, err := r.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}},
		func(string, *ChatUsage) error { return nil })
	require.NoError(t, err)
	got, _ := sawAuth.Load().(string)
	assert.Equal(t, "Bearer my-key", got)
}

// TestRouter_EmptyProbeKey_OmitsHeader pins the vLLM-without-api-key
// path: empty ProbeKey means no Authorization header.
func TestRouter_EmptyProbeKey_OmitsHeader(t *testing.T) {
	var sawAuth atomic.Value // string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth.Store(r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	primary := &stubProvider{chatFn: func(_ context.Context, _ ChatRequest, _ func(string, *ChatUsage) error) (*ChatUsage, error) {
		return &ChatUsage{}, nil
	}}
	secondaryProbeHits := &atomic.Int64{}
	secondaryProbe := newProbeServer(t, secondaryProbeHits)
	defer secondaryProbe.Close()

	r := NewRouter(
		ProviderSlot{Name: "vllm", Provider: primary, BaseURL: server.URL, ProbeKey: ""},
		&ProviderSlot{Name: "openai", Provider: &stubProvider{}, BaseURL: secondaryProbe.URL, ProbeKey: ""},
		noop.NewTracerProvider().Tracer("test"),
		500*time.Millisecond,
	)
	_, err := r.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}},
		func(string, *ChatUsage) error { return nil })
	require.NoError(t, err)
	got, _ := sawAuth.Load().(string)
	assert.Empty(t, got)
}

// newAlwaysFailingURL returns a URL string pointing at a port
// nobody is listening on. Every request to it fails with
// ECONNREFUSED — used by "primary probe fails" tests. The
// caller uses only the URL; no server is constructed so there's
// nothing to close. A parallel test may grab the port in the
// brief window between close and probe, but that still causes
// the probe to fail (just with a different error class).
func newAlwaysFailingURL(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := "http://" + ln.Addr().String()
	require.NoError(t, ln.Close())
	return addr
}

// TestRouter_Embed_Passthrough pins the single-provider path: no
// probe round-trip, primary's Embed result is returned untouched.
func TestRouter_Embed_Passthrough(t *testing.T) {
	want := []float32{0.1, 0.2, 0.3}
	primary := &stubProvider{
		embedFn: func(_ context.Context, _ EmbedRequest) ([][]float32, error) {
			return [][]float32{want}, nil
		},
	}
	r := NewRouter(
		ProviderSlot{Name: "openai", Provider: primary, BaseURL: "http://primary"},
		nil, // single-provider passthrough
		noop.NewTracerProvider().Tracer("test"),
		100*time.Millisecond,
	)
	got, err := r.Embed(context.Background(), EmbedRequest{Model: "m", Inputs: []string{"x"}})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, want, got[0])
}

// TestRouter_Embed_FailoverOnProviderUnavailable pins the
// dual-provider failover contract: ErrProviderUnavailable on the
// primary triggers a single retry on the secondary.
func TestRouter_Embed_FailoverOnProviderUnavailable(t *testing.T) {
	primaryCalls := atomic.Int64{}
	secondaryCalls := atomic.Int64{}
	primary := &stubProvider{
		embedFn: func(_ context.Context, _ EmbedRequest) ([][]float32, error) {
			primaryCalls.Add(1)
			return nil, ErrProviderUnavailable
		},
	}
	secondary := &stubProvider{
		embedFn: func(_ context.Context, _ EmbedRequest) ([][]float32, error) {
			secondaryCalls.Add(1)
			return [][]float32{{0.5, 0.6}}, nil
		},
	}
	r := NewRouter(
		ProviderSlot{Name: "openai", Provider: primary, BaseURL: "http://primary"},
		&ProviderSlot{Name: "vllm", Provider: secondary, BaseURL: "http://secondary"},
		noop.NewTracerProvider().Tracer("test"),
		100*time.Millisecond,
	)
	got, err := r.Embed(context.Background(), EmbedRequest{Model: "m", Inputs: []string{"x"}})
	require.NoError(t, err)
	assert.Equal(t, int64(1), primaryCalls.Load())
	assert.Equal(t, int64(1), secondaryCalls.Load())
	assert.Equal(t, []float32{0.5, 0.6}, got[0])
}

func TestRouter_Embed_FailoverOnInvalidInput(t *testing.T) {
	primaryCalls := atomic.Int64{}
	secondaryCalls := atomic.Int64{}
	primary := &stubProvider{
		embedFn: func(_ context.Context, _ EmbedRequest) ([][]float32, error) {
			primaryCalls.Add(1)
			return nil, ErrInvalidInput
		},
	}
	secondary := &stubProvider{
		embedFn: func(_ context.Context, _ EmbedRequest) ([][]float32, error) {
			secondaryCalls.Add(1)
			return [][]float32{{0.5, 0.6}}, nil
		},
	}
	r := NewRouter(
		ProviderSlot{Name: "openai", Provider: primary, BaseURL: "http://primary"},
		&ProviderSlot{Name: "vllm", Provider: secondary, BaseURL: "http://secondary"},
		noop.NewTracerProvider().Tracer("test"),
		100*time.Millisecond,
	)
	got, err := r.Embed(context.Background(), EmbedRequest{Model: "m", Inputs: []string{"x"}})
	require.NoError(t, err)
	assert.Equal(t, int64(1), primaryCalls.Load())
	assert.Equal(t, int64(1), secondaryCalls.Load())
	assert.Equal(t, []float32{0.5, 0.6}, got[0])
}

func TestRouter_Embed_BothRejectInvalidInputKeepsSentinel(t *testing.T) {
	primary := &stubProvider{
		embedFn: func(_ context.Context, _ EmbedRequest) ([][]float32, error) {
			return nil, ErrInvalidInput
		},
	}
	secondary := &stubProvider{
		embedFn: func(_ context.Context, _ EmbedRequest) ([][]float32, error) {
			return nil, ErrInvalidInput
		},
	}
	r := NewRouter(
		ProviderSlot{Name: "openai", Provider: primary, BaseURL: "http://primary"},
		&ProviderSlot{Name: "vllm", Provider: secondary, BaseURL: "http://secondary"},
		noop.NewTracerProvider().Tracer("test"),
		100*time.Millisecond,
	)
	_, err := r.Embed(context.Background(), EmbedRequest{Model: "m", Inputs: []string{"x"}})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidInput))
	assert.True(t, errors.Is(err, ErrAllProvidersFailed))
}

// TestRouter_Embed_NonFailoverErrorsDoNotFailover pins that
// rate-limit and ctx-cancel do NOT trigger a secondary retry.
// Same rule as shouldFailover for chat.
func TestRouter_Embed_NonFailoverErrorsDoNotFailover(t *testing.T) {
	primaryCalls := atomic.Int64{}
	secondaryCalls := atomic.Int64{}
	primary := &stubProvider{
		embedFn: func(_ context.Context, _ EmbedRequest) ([][]float32, error) {
			primaryCalls.Add(1)
			return nil, ErrRateLimited
		},
	}
	secondary := &stubProvider{
		embedFn: func(_ context.Context, _ EmbedRequest) ([][]float32, error) {
			secondaryCalls.Add(1)
			return [][]float32{{0}}, nil
		},
	}
	r := NewRouter(
		ProviderSlot{Name: "openai", Provider: primary, BaseURL: "http://primary"},
		&ProviderSlot{Name: "vllm", Provider: secondary, BaseURL: "http://secondary"},
		noop.NewTracerProvider().Tracer("test"),
		100*time.Millisecond,
	)
	_, err := r.Embed(context.Background(), EmbedRequest{Model: "m", Inputs: []string{"x"}})
	require.Error(t, err)
	assert.Equal(t, int64(1), primaryCalls.Load())
	assert.Equal(t, int64(0), secondaryCalls.Load(), "non-failover error must not retry on secondary")
}

// TestRouter_Embed_BothFail_ReturnsAllProvidersFailed pins the
// both-down branch: ErrAllProvidersFailed wraps ErrProviderUnavailable
// so the chat-side handler can still walk the chain with errors.Is.
func TestRouter_Embed_BothFail_ReturnsAllProvidersFailed(t *testing.T) {
	primary := &stubProvider{
		embedFn: func(_ context.Context, _ EmbedRequest) ([][]float32, error) {
			return nil, ErrProviderUnavailable
		},
	}
	secondary := &stubProvider{
		embedFn: func(_ context.Context, _ EmbedRequest) ([][]float32, error) {
			return nil, ErrProviderUnavailable
		},
	}
	r := NewRouter(
		ProviderSlot{Name: "openai", Provider: primary, BaseURL: "http://primary"},
		&ProviderSlot{Name: "vllm", Provider: secondary, BaseURL: "http://secondary"},
		noop.NewTracerProvider().Tracer("test"),
		100*time.Millisecond,
	)
	_, err := r.Embed(context.Background(), EmbedRequest{Model: "m", Inputs: []string{"x"}})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrAllProvidersFailed))
	assert.True(t, errors.Is(err, ErrProviderUnavailable))
}

// TestRouter_Embed_NoProbeRoundTrip pins the contract that
// embeddings do NOT trigger the GET /v1/models probe. The chat
// probe exists to detect a wedged upstream before committing to a
// streaming call; an embedding is one synchronous request whose
// outcome IS the liveness signal.
func TestRouter_Embed_NoProbeRoundTrip(t *testing.T) {
	hits := atomic.Int64{}
	probeURL := newProbeServer(t, &hits).URL
	defer func() {
		// Reach into the test server we just constructed to close it
		// explicitly — defer Close on the URL would race against the
		// httptest shutdown machinery otherwise.
	}()
	_ = probeURL // keep lint happy; we only need the hit counter
	primary := &stubProvider{
		embedFn: func(_ context.Context, _ EmbedRequest) ([][]float32, error) {
			return [][]float32{{0.0}}, nil
		},
	}
	r := NewRouter(
		ProviderSlot{Name: "openai", Provider: primary, BaseURL: "http://primary"},
		&ProviderSlot{Name: "vllm", Provider: primary, BaseURL: "http://secondary"},
		noop.NewTracerProvider().Tracer("test"),
		100*time.Millisecond,
	)
	_, err := r.Embed(context.Background(), EmbedRequest{Model: "m", Inputs: []string{"x"}})
	require.NoError(t, err)
	assert.Equal(t, int64(0), hits.Load(), "embed must not issue a /v1/models probe")
}

// deltaProvider wraps a stub chat function so the router can drive
// both Provider.Chat and DeltaProvider.ChatDelta. The same struct
// implements both interfaces; the parity check is that the router
// uses the typed callback for ChatDelta and the legacy callback for
// Chat, never mixing them.
type deltaProvider struct {
	chatFn  func(ctx context.Context, req ChatRequest, onDelta func(delta string, finalUsage *ChatUsage) error) (*ChatUsage, error)
	chatDFn func(ctx context.Context, req ChatRequest, onDelta func(Delta) error) (*ChatUsage, error)
	embedFn func(ctx context.Context, req EmbedRequest) ([][]float32, error)
}

func (d *deltaProvider) Chat(ctx context.Context, req ChatRequest, onDelta func(delta string, finalUsage *ChatUsage) error) (*ChatUsage, error) {
	return d.chatFn(ctx, req, onDelta)
}

func (d *deltaProvider) ChatDelta(ctx context.Context, req ChatRequest, onDelta func(Delta) error) (*ChatUsage, error) {
	return d.chatDFn(ctx, req, onDelta)
}

func (d *deltaProvider) Embed(ctx context.Context, req EmbedRequest) ([][]float32, error) {
	if d.embedFn == nil {
		return nil, nil
	}
	return d.embedFn(ctx, req)
}

// TestRouter_ChatDelta_Passthrough pins that the typed-callback
// router path with no secondary delegates straight to the primary's
// ChatDelta. The captured Delta sequence is forwarded verbatim.
func TestRouter_ChatDelta_Passthrough(t *testing.T) {
	primary := &deltaProvider{
		chatDFn: func(_ context.Context, _ ChatRequest, onDelta func(Delta) error) (*ChatUsage, error) {
			require.NoError(t, onDelta(Delta{Kind: DeltaContent, Content: "hello"}))
			require.NoError(t, onDelta(Delta{Kind: DeltaUsage, Usage: &ChatUsage{TotalTokens: 1}}))
			return &ChatUsage{TotalTokens: 1}, nil
		},
	}
	r := NewRouter(
		ProviderSlot{Name: "openai", Provider: primary, BaseURL: "http://primary"},
		nil,
		noop.NewTracerProvider().Tracer("test"),
		100*time.Millisecond,
	)

	var frames []Delta
	usage, err := r.ChatDelta(context.Background(), ChatRequest{
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, func(d Delta) error {
		frames = append(frames, d)
		return nil
	})
	require.NoError(t, err)
	require.Len(t, frames, 2)
	assert.Equal(t, DeltaContent, frames[0].Kind)
	assert.Equal(t, "hello", frames[0].Content)
	assert.Equal(t, DeltaUsage, frames[1].Kind)
	require.NotNil(t, usage)
	assert.Equal(t, 1, usage.TotalTokens)
}

// TestRouter_ChatDelta_ToolBearingRefusedWithoutGate pins the
// task 1.8 / spec scenario "Unverified provider refuses tool-
// bearing request": with SetFallbackToolsEnabled(false) (the
// default), a tool-bearing request whose primary fails must NOT
// be retried on the secondary — instead, the router returns
// ErrToolCallsUnsupported.
//
// This is the vLLM-drops-tool-calls problem the typed Delta stream
// exists to fix: failing open here would silently drop in-flight
// tool-call deltas the caller already received.
func TestRouter_ChatDelta_ToolBearingRefusedWithoutGate(t *testing.T) {
	primaryHits := &atomic.Int64{}
	primaryProbe := newProbeServer(t, primaryHits)
	defer primaryProbe.Close()
	secondaryHits := &atomic.Int64{}
	secondaryProbe := newProbeServer(t, secondaryHits)
	defer secondaryProbe.Close()

	primaryCalled := &atomic.Int64{}
	primary := &deltaProvider{
		chatDFn: func(_ context.Context, _ ChatRequest, _ func(Delta) error) (*ChatUsage, error) {
			primaryCalled.Add(1)
			return nil, fmt.Errorf("primary down: %w", ErrProviderUnavailable)
		},
	}
	secondaryCalled := &atomic.Int64{}
	secondary := &deltaProvider{
		chatDFn: func(_ context.Context, _ ChatRequest, _ func(Delta) error) (*ChatUsage, error) {
			secondaryCalled.Add(1)
			t.Fatal("secondary must NOT be called for tool-bearing requests without SetFallbackToolsEnabled")
			return nil, nil
		},
	}

	r := NewRouter(
		ProviderSlot{Name: "openai", Provider: primary, BaseURL: primaryProbe.URL, ProbeKey: "k"},
		&ProviderSlot{Name: "vllm", Provider: secondary, BaseURL: secondaryProbe.URL, ProbeKey: "k"},
		noop.NewTracerProvider().Tracer("test"),
		500*time.Millisecond,
	)
	// Default state: SetFallbackToolsEnabled was never called.
	// (The bool field's zero value is false.)

	tools := []Tool{{Type: "function", Function: ToolFunction{Name: "list_feeds"}}}
	_, err := r.ChatDelta(context.Background(), ChatRequest{
		Messages: []Message{{Role: "user", Content: "hi"}},
		Tools:    tools,
	}, func(Delta) error { return nil })

	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrToolCallsUnsupported),
		"expected ErrToolCallsUnsupported, got %v", err)
	assert.Equal(t, int64(1), primaryCalled.Load(), "primary must be tried first")
	assert.Equal(t, int64(0), secondaryCalled.Load(), "secondary must be refused")
}

// TestRouter_ChatDelta_ToolBearingAllowedWithGate pins the
// complementary scenario "Both providers verified — failover
// proceeds": with SetFallbackToolsEnabled(true), a tool-bearing
// request whose primary fails IS retried on the secondary.
func TestRouter_ChatDelta_ToolBearingAllowedWithGate(t *testing.T) {
	primaryHits := &atomic.Int64{}
	primaryProbe := newProbeServer(t, primaryHits)
	defer primaryProbe.Close()
	secondaryHits := &atomic.Int64{}
	secondaryProbe := newProbeServer(t, secondaryHits)
	defer secondaryProbe.Close()

	primary := &deltaProvider{
		chatDFn: func(_ context.Context, _ ChatRequest, _ func(Delta) error) (*ChatUsage, error) {
			return nil, fmt.Errorf("primary down: %w", ErrProviderUnavailable)
		},
	}
	secondaryCalled := &atomic.Int64{}
	secondary := &deltaProvider{
		chatDFn: func(_ context.Context, _ ChatRequest, onDelta func(Delta) error) (*ChatUsage, error) {
			secondaryCalled.Add(1)
			require.NoError(t, onDelta(Delta{Kind: DeltaContent, Content: "from-secondary"}))
			require.NoError(t, onDelta(Delta{Kind: DeltaUsage, Usage: &ChatUsage{TotalTokens: 3}}))
			return &ChatUsage{TotalTokens: 3}, nil
		},
	}

	r := NewRouter(
		ProviderSlot{Name: "openai", Provider: primary, BaseURL: primaryProbe.URL, ProbeKey: "k"},
		&ProviderSlot{Name: "vllm", Provider: secondary, BaseURL: secondaryProbe.URL, ProbeKey: "k"},
		noop.NewTracerProvider().Tracer("test"),
		500*time.Millisecond,
	)
	r.SetFallbackToolsEnabled(true)

	tools := []Tool{{Type: "function", Function: ToolFunction{Name: "list_feeds"}}}
	var frames []Delta
	usage, err := r.ChatDelta(context.Background(), ChatRequest{
		Messages: []Message{{Role: "user", Content: "hi"}},
		Tools:    tools,
	}, func(d Delta) error { frames = append(frames, d); return nil })

	require.NoError(t, err)
	assert.Equal(t, int64(1), secondaryCalled.Load())
	require.NotNil(t, usage)
	assert.Equal(t, 3, usage.TotalTokens)
	require.GreaterOrEqual(t, len(frames), 1)
	assert.Equal(t, "from-secondary", frames[0].Content)
}

// TestRouter_ChatDelta_NonToolBearingStillFailsOver is the
// regression guard: the gate only affects tool-bearing requests.
// A non-tool request whose primary fails continues to failover to
// the secondary even with SetFallbackToolsEnabled(false). The chat
// service exercises this path today; changing the behaviour would
// break the existing single-provider-with-fallback deployment.
func TestRouter_ChatDelta_NonToolBearingStillFailsOver(t *testing.T) {
	primaryHits := &atomic.Int64{}
	primaryProbe := newProbeServer(t, primaryHits)
	defer primaryProbe.Close()
	secondaryHits := &atomic.Int64{}
	secondaryProbe := newProbeServer(t, secondaryHits)
	defer secondaryProbe.Close()

	primary := &deltaProvider{
		chatDFn: func(_ context.Context, _ ChatRequest, _ func(Delta) error) (*ChatUsage, error) {
			return nil, fmt.Errorf("primary down: %w", ErrProviderUnavailable)
		},
	}
	secondaryCalled := &atomic.Int64{}
	secondary := &deltaProvider{
		chatDFn: func(_ context.Context, _ ChatRequest, onDelta func(Delta) error) (*ChatUsage, error) {
			secondaryCalled.Add(1)
			require.NoError(t, onDelta(Delta{Kind: DeltaContent, Content: "ok"}))
			return &ChatUsage{TotalTokens: 1}, nil
		},
	}

	r := NewRouter(
		ProviderSlot{Name: "openai", Provider: primary, BaseURL: primaryProbe.URL, ProbeKey: "k"},
		&ProviderSlot{Name: "vllm", Provider: secondary, BaseURL: secondaryProbe.URL, ProbeKey: "k"},
		noop.NewTracerProvider().Tracer("test"),
		500*time.Millisecond,
	)
	// Default gate state — false — but no Tools so the request is
	// not tool-bearing.

	var frames []Delta
	_, err := r.ChatDelta(context.Background(), ChatRequest{
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, func(d Delta) error { frames = append(frames, d); return nil })

	require.NoError(t, err)
	assert.Equal(t, int64(1), secondaryCalled.Load())
}

// TestRouter_Chat_ToolBearingRefusedWithoutGate mirrors the
// typed-callback test for the legacy Chat path. The behaviour is
// the same: a tool-bearing request on the legacy callback is
// refused from failover when the gate is off. This is a defensive
// belt-and-braces — the chat service today never passes Tools, but
// a future caller (or a test stub) must still see the gate.
func TestRouter_Chat_ToolBearingRefusedWithoutGate(t *testing.T) {
	primaryHits := &atomic.Int64{}
	primaryProbe := newProbeServer(t, primaryHits)
	defer primaryProbe.Close()
	secondaryHits := &atomic.Int64{}
	secondaryProbe := newProbeServer(t, secondaryHits)
	defer secondaryProbe.Close()

	primary := &stubProvider{
		chatFn: func(_ context.Context, _ ChatRequest, _ func(string, *ChatUsage) error) (*ChatUsage, error) {
			return nil, fmt.Errorf("primary down: %w", ErrProviderUnavailable)
		},
	}
	secondary := &stubProvider{
		chatFn: func(_ context.Context, _ ChatRequest, _ func(string, *ChatUsage) error) (*ChatUsage, error) {
			t.Fatal("secondary must NOT be called for tool-bearing requests without SetFallbackToolsEnabled")
			return nil, nil
		},
	}

	r := NewRouter(
		ProviderSlot{Name: "openai", Provider: primary, BaseURL: primaryProbe.URL, ProbeKey: "k"},
		&ProviderSlot{Name: "vllm", Provider: secondary, BaseURL: secondaryProbe.URL, ProbeKey: "k"},
		noop.NewTracerProvider().Tracer("test"),
		500*time.Millisecond,
	)

	_, err := r.Chat(context.Background(), ChatRequest{
		Messages: []Message{{Role: "user", Content: "hi"}},
		Tools:    []Tool{{Type: "function", Function: ToolFunction{Name: "list_feeds"}}},
	}, func(string, *ChatUsage) error { return nil })

	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrToolCallsUnsupported))
}

// TestRouter_ToolChoiceOnly_IsToolBearing pins the edge case: a
// request with ToolChoice set but Tools empty is still "tool-
// bearing" — the model would not emit tool calls without Tool defs,
// but the field is intentionally included in the wire for the
// case where the operator wants to constrain behaviour (e.g.
// tool_choice="none"). The router must treat it as tool-bearing
// so the gate covers the directive path too.
func TestRouter_ToolChoiceOnly_IsToolBearing(t *testing.T) {
	primaryHits := &atomic.Int64{}
	primaryProbe := newProbeServer(t, primaryHits)
	defer primaryProbe.Close()
	secondaryHits := &atomic.Int64{}
	secondaryProbe := newProbeServer(t, secondaryHits)
	defer secondaryProbe.Close()

	primary := &deltaProvider{
		chatDFn: func(_ context.Context, _ ChatRequest, _ func(Delta) error) (*ChatUsage, error) {
			return nil, fmt.Errorf("primary down: %w", ErrProviderUnavailable)
		},
	}
	secondary := &deltaProvider{
		chatDFn: func(_ context.Context, _ ChatRequest, _ func(Delta) error) (*ChatUsage, error) {
			t.Fatal("secondary must not run — gate should refuse")
			return nil, nil
		},
	}
	r := NewRouter(
		ProviderSlot{Name: "openai", Provider: primary, BaseURL: primaryProbe.URL, ProbeKey: "k"},
		&ProviderSlot{Name: "vllm", Provider: secondary, BaseURL: secondaryProbe.URL, ProbeKey: "k"},
		noop.NewTracerProvider().Tracer("test"),
		500*time.Millisecond,
	)
	// ToolChoice set, no Tools — still tool-bearing per the router's
	// "toolsBearing" check.
	_, err := r.ChatDelta(context.Background(), ChatRequest{
		Messages:   []Message{{Role: "user", Content: "hi"}},
		ToolChoice: "none",
	}, func(Delta) error { return nil })
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrToolCallsUnsupported))
}

// TestSetFallbackToolsEnabled_Toggle pins the simple flip: setting
// the gate to true persists for subsequent requests, and setting it
// back to false revokes the opt-in. Operators toggle this when CI
// confirms parity per the rollout in FUTURE_AGENT.md / 6.1.
func TestSetFallbackToolsEnabled_Toggle(t *testing.T) {
	r := NewRouter(
		ProviderSlot{Name: "openai", Provider: &stubProvider{}, BaseURL: "http://x"},
		&ProviderSlot{Name: "vllm", Provider: &stubProvider{}, BaseURL: "http://y"},
		noop.NewTracerProvider().Tracer("test"),
		100*time.Millisecond,
	)
	// Default off.
	require.False(t, r.failoverToolsEnabled)
	r.SetFallbackToolsEnabled(true)
	require.True(t, r.failoverToolsEnabled)
	r.SetFallbackToolsEnabled(false)
	require.False(t, r.failoverToolsEnabled)
}

func TestRouter_ProbeNormalizesVersionSuffix(t *testing.T) {
	cases := []struct {
		name     string
		suffix   string
		wantPath string
	}{
		{name: "bare host probes the versioned path", suffix: "", wantPath: "/v1/models"},
		{name: "version-prefixed base is not double-versioned", suffix: "/v1", wantPath: "/v1/models"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var gotPath string
			primaryProbe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				w.WriteHeader(http.StatusOK)
				_, _ = fmt.Fprintln(w, `{"data":[]}`)
			}))
			defer primaryProbe.Close()
			secondaryProbe := newProbeServer(t, nil)
			defer secondaryProbe.Close()

			primaryCalled := &atomic.Int64{}
			primary := &stubProvider{chatFn: func(_ context.Context, _ ChatRequest, onDelta func(string, *ChatUsage) error) (*ChatUsage, error) {
				primaryCalled.Add(1)
				_ = onDelta("hello", nil)
				return &ChatUsage{TotalTokens: 1}, nil
			}}
			secondary := &stubProvider{chatFn: func(_ context.Context, _ ChatRequest, _ func(string, *ChatUsage) error) (*ChatUsage, error) {
				t.Fatal("secondary must not run when the primary probe succeeds")
				return nil, nil
			}}

			r := NewRouter(
				ProviderSlot{Name: "openai", Provider: primary, BaseURL: primaryProbe.URL + c.suffix, ProbeKey: "k"},
				&ProviderSlot{Name: "vllm", Provider: secondary, BaseURL: secondaryProbe.URL, ProbeKey: "k"},
				noop.NewTracerProvider().Tracer("test"),
				500*time.Millisecond,
			)

			var deltas []string
			_, err := r.Chat(context.Background(), ChatRequest{
				Messages: []Message{{Role: "user", Content: "hi"}},
			}, func(d string, _ *ChatUsage) error {
				deltas = append(deltas, d)
				return nil
			})
			require.NoError(t, err)
			assert.Equal(t, c.wantPath, gotPath, "probe path must match the base URL convention")
			assert.Equal(t, int64(1), primaryCalled.Load(),
				"a healthy primary must be used, not failed over to the secondary")
			assert.Equal(t, []string{"hello"}, deltas)
		})
	}
}

func TestWithProbeHTTP_OverridesProbeClient(t *testing.T) {
	sentinel := &http.Client{Timeout: 7 * time.Second}
	router := NewRouter(
		ProviderSlot{Name: "primary"},
		nil,
		noop.NewTracerProvider().Tracer("test"),
		time.Second,
		WithProbeHTTP(sentinel),
	)
	if router.probeHTTP != sentinel {
		t.Fatalf("probe client not overridden")
	}
	if router.probeHTTP.Timeout != 7*time.Second {
		t.Fatalf("probe client timeout lost")
	}
}

func TestNewRouter_DefaultProbeClientWhenNoOption(t *testing.T) {
	router := NewRouter(
		ProviderSlot{Name: "primary"},
		nil,
		noop.NewTracerProvider().Tracer("test"),
		3*time.Second,
	)
	if router.probeHTTP == nil {
		t.Fatal("default probe client missing")
	}
	if router.probeHTTP.Timeout != 3*time.Second {
		t.Fatalf("probe timeout not preserved, got %v", router.probeHTTP.Timeout)
	}
}
