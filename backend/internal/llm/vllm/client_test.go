package vllm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace/noop"

	"nyx/internal/llm"
	"nyx/internal/platform/config"
)

// newTestClient wires a Client pointed at the given test server URL.
// Tests that need LLM_ALLOW_PRIVATE_URL=false (the SSRF-guard
// rejection cases) build their own Config so the helper can stay
// parameter-free.
func newTestClient(t *testing.T, serverURL string) *Client {
	t.Helper()
	cfg := &config.Config{
		LLMEnabled:         true,
		LLMProvider:        "vllm",
		LLMBaseURL:         serverURL,
		LLMAPIKey:          "sk-real-test-key-1234567890",
		LLMModel:           "test-model",
		LLMTimeout:         "30s",
		LLMAllowPrivateURL: true,
	}
	tracer := noop.NewTracerProvider().Tracer("test")
	c, err := NewClient(cfg, tracer)
	require.NoError(t, err)
	return c
}

// writeSSE writes one `data: <payload>\n\n` frame and flushes. Test
// servers use it to emit the canonical SSE chunk shape OpenAI/vLLM use.
func writeSSE(w http.ResponseWriter, payload string) {
	_, _ = fmt.Fprintf(w, "data: %s\n\n", payload)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// TestChat_StreamsChunksAndUsage exercises the happy path: three
// content chunks + a usage chunk should fire onDelta three times in
// order and return the usage. Asserts the outbound request shape
// (Stream=true, IncludeUsage=true, MaxTokens, Model, Messages) so
// drift between us and vLLM is caught here, not in prod.
func TestChat_StreamsChunksAndUsage(t *testing.T) {
	var captured atomic.Value // outboundRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req outboundRequest
		_ = json.Unmarshal(body, &req)
		captured.Store(&req)

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		// Role frame — Delta.Content is "", so the client should skip.
		writeSSE(w, `{"id":"x","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`)
		// Three content frames.
		for _, s := range []string{"Hello", " ", "world"} {
			writeSSE(w, fmt.Sprintf(`{"id":"x","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":%q},"finish_reason":null}]}`, s))
		}
		// Stop frame.
		writeSSE(w, `{"id":"x","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
		// Usage frame (Choices is empty; Usage is non-nil).
		writeSSE(w, `{"id":"x","object":"chat.completion.chunk","created":1,"model":"m","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`)
		// Trailing terminator.
		writeSSE(w, "[DONE]")
	}))
	defer server.Close()

	c := newTestClient(t, server.URL)

	var deltas []string
	usage, err := c.Chat(context.Background(), llm.ChatRequest{
		Messages:  []llm.Message{{Role: "user", Content: "hi"}},
		MaxTokens: 100,
	}, func(s string, u *llm.ChatUsage) error {
		if u != nil {
			return nil
		}
		deltas = append(deltas, s)
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"Hello", " ", "world"}, deltas)
	require.NotNil(t, usage)
	assert.Equal(t, 5, usage.PromptTokens)
	assert.Equal(t, 2, usage.CompletionTokens)
	assert.Equal(t, 7, usage.TotalTokens)

	// Verify the outbound request shape.
	reqPtr, _ := captured.Load().(*outboundRequest)
	require.NotNil(t, reqPtr)
	assert.True(t, reqPtr.Stream)
	require.NotNil(t, reqPtr.Options)
	assert.True(t, reqPtr.Options.IncludeUsage)
	assert.Equal(t, 100, reqPtr.MaxTokens)
	assert.Equal(t, "test-model", reqPtr.Model)
	require.Len(t, reqPtr.Messages, 1)
	assert.Equal(t, "user", reqPtr.Messages[0].Role)
	assert.Equal(t, "hi", reqPtr.Messages[0].Content)
}

// TestChat_AuthorizationHeaderSentWhenKeySet pins that we forward the
// bearer token the operator configured.
func TestChat_AuthorizationHeaderSentWhenKeySet(t *testing.T) {
	var authSeen atomic.Value // string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authSeen.Store(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		writeSSE(w, `{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
		writeSSE(w, "[DONE]")
	}))
	defer server.Close()

	c := newTestClient(t, server.URL)
	_, err := c.Chat(context.Background(), llm.ChatRequest{
		Messages: []llm.Message{{Role: "user", Content: "hi"}},
	}, func(string, *llm.ChatUsage) error { return nil })
	require.NoError(t, err)
	got, _ := authSeen.Load().(string)
	assert.Equal(t, "Bearer sk-real-test-key-1234567890", got)
}

// TestChat_AuthorizationHeaderOmittedWhenKeyEmpty pins that an empty
// LLM_API_KEY means NO Authorization header — sending "Bearer " would
// return 401 from a vLLM started with --api-key set.
func TestChat_AuthorizationHeaderOmittedWhenKeyEmpty(t *testing.T) {
	var authSeen atomic.Value // string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authSeen.Store(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		writeSSE(w, `{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
		writeSSE(w, "[DONE]")
	}))
	defer server.Close()

	cfg := &config.Config{
		LLMEnabled:         true,
		LLMProvider:        "vllm",
		LLMBaseURL:         server.URL,
		LLMAPIKey:          "", // empty on purpose
		LLMModel:           "test-model",
		LLMTimeout:         "30s",
		LLMAllowPrivateURL: true,
	}
	tracer := noop.NewTracerProvider().Tracer("test")
	c, err := NewClient(cfg, tracer)
	require.NoError(t, err)

	_, err = c.Chat(context.Background(), llm.ChatRequest{
		Messages: []llm.Message{{Role: "user", Content: "hi"}},
	}, func(string, *llm.ChatUsage) error { return nil })
	require.NoError(t, err)
	got, _ := authSeen.Load().(string)
	assert.Empty(t, got, "Authorization header must be omitted when LLM_API_KEY is empty")
}

// TestChat_RateLimit_ReturnsSentinel asserts a 429 surfaces as
// llm.ErrRateLimited. mapError routes 429 → ErrRateLimited without
// inspecting the body.
func TestChat_RateLimit_ReturnsSentinel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"rate limited","type":"rate_limit_error","code":"rate_limit"}}`)
	}))
	defer server.Close()

	c := newTestClient(t, server.URL)
	usage, err := c.Chat(context.Background(), llm.ChatRequest{
		Messages: []llm.Message{{Role: "user", Content: "hi"}},
	}, func(string, *llm.ChatUsage) error { return nil })

	require.Error(t, err)
	assert.True(t, errors.Is(err, llm.ErrRateLimited), "expected ErrRateLimited, got %v", err)
	assert.Nil(t, usage)
}

// TestChat_ServerError_ReturnsSentinel asserts a 500 surfaces as
// llm.ErrProviderUnavailable.
func TestChat_ServerError_ReturnsSentinel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"message":"server error","type":"server_error","code":"internal"}}`)
	}))
	defer server.Close()

	c := newTestClient(t, server.URL)
	usage, err := c.Chat(context.Background(), llm.ChatRequest{
		Messages: []llm.Message{{Role: "user", Content: "hi"}},
	}, func(string, *llm.ChatUsage) error { return nil })

	require.Error(t, err)
	assert.True(t, errors.Is(err, llm.ErrProviderUnavailable), "expected ErrProviderUnavailable, got %v", err)
	assert.Nil(t, usage)
}

// TestChat_ModelNotFound_ReturnsProviderUnavailable pins the 404
// default. A misconfigured VLLM_MODEL surfaces to the user as a 502;
// the README documents the operator fix.
func TestChat_ModelNotFound_ReturnsProviderUnavailable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"object":"error","message":"The model `+"`X`"+` does not exist.","type":"NotFoundError","code":"model_not_found","param":null,"status_code":404}`)
	}))
	defer server.Close()

	c := newTestClient(t, server.URL)
	usage, err := c.Chat(context.Background(), llm.ChatRequest{
		Messages: []llm.Message{{Role: "user", Content: "hi"}},
	}, func(string, *llm.ChatUsage) error { return nil })

	require.Error(t, err)
	assert.True(t, errors.Is(err, llm.ErrProviderUnavailable), "expected ErrProviderUnavailable, got %v", err)
	assert.Nil(t, usage)
}

// TestChat_NetworkError_ReturnsSentinel asserts an unreachable
// upstream routes through *url.Error and becomes
// ErrProviderUnavailable. Bind to localhost:0 and close so the
// connect attempt fails.
func TestChat_NetworkError_ReturnsSentinel(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())

	c := newTestClient(t, "http://"+addr)
	usage, chatErr := c.Chat(context.Background(), llm.ChatRequest{
		Messages: []llm.Message{{Role: "user", Content: "hi"}},
	}, func(string, *llm.ChatUsage) error { return nil })

	require.Error(t, chatErr)
	assert.True(t, errors.Is(chatErr, llm.ErrProviderUnavailable), "expected ErrProviderUnavailable, got %v", chatErr)
	assert.Nil(t, usage)
}

// TestChat_ContextCanceled_StopsStream asserts cancelling the context
// while the server is still streaming returns promptly with
// llm.ErrContextCanceled.
func TestChat_ContextCanceled_StopsStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		require.True(t, ok)

		for i := 0; i < 100; i++ {
			select {
			case <-r.Context().Done():
				return
			default:
			}
			writeSSE(w, `{"id":"x","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"x"},"finish_reason":null}]}`)
			flusher.Flush()
			time.Sleep(20 * time.Millisecond)
		}
	}))
	defer server.Close()

	c := newTestClient(t, server.URL)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(60 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	usage, err := c.Chat(ctx, llm.ChatRequest{
		Messages: []llm.Message{{Role: "user", Content: "hi"}},
	}, func(string, *llm.ChatUsage) error { return nil })
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.True(t, errors.Is(err, llm.ErrContextCanceled), "expected ErrContextCanceled, got %v", err)
	assert.Nil(t, usage)
	assert.Less(t, elapsed, 500*time.Millisecond, "Chat did not honor context cancellation: %v", elapsed)
}

// TestChat_MalformedChunkMidStream asserts the client doesn't panic
// when the upstream emits an unparseable data frame. The parser is
// expected to surface the error, which mapError funnels into
// ErrProviderUnavailable.
func TestChat_MalformedChunkMidStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		writeSSE(w, `{"id":"x","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":null}]}`)
		// Garbage frame — not valid JSON.
		writeSSE(w, strings.Repeat("not-json", 50))
	}))
	defer server.Close()

	c := newTestClient(t, server.URL)
	usage, err := c.Chat(context.Background(), llm.ChatRequest{
		Messages: []llm.Message{{Role: "user", Content: "hi"}},
	}, func(string, *llm.ChatUsage) error { return nil })

	require.Error(t, err)
	assert.True(t, errors.Is(err, llm.ErrProviderUnavailable), "expected ErrProviderUnavailable, got %v", err)
	assert.Nil(t, usage)
}

// TestChat_ToolCallsIgnored asserts vLLM-emitted tool_call deltas
// don't break the stream — our schema doesn't model them but Go's
// permissive JSON decoder drops the unknown fields.
func TestChat_ToolCallsIgnored(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// Tool-call delta (no content).
		writeSSE(w, `{"choices":[{"index":0,"delta":{"role":"assistant","content":null,"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"f","arguments":""}}]},"finish_reason":null}]}`)
		// Two content frames after.
		writeSSE(w, `{"choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":null}]}`)
		writeSSE(w, `{"choices":[{"index":0,"delta":{"content":"!"},"finish_reason":null}]}`)
		writeSSE(w, `{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
		writeSSE(w, "[DONE]")
	}))
	defer server.Close()

	c := newTestClient(t, server.URL)
	var deltas []string
	usage, err := c.Chat(context.Background(), llm.ChatRequest{
		Messages: []llm.Message{{Role: "user", Content: "hi"}},
	}, func(s string, u *llm.ChatUsage) error {
		if u != nil {
			return nil
		}
		deltas = append(deltas, s)
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"ok", "!"}, deltas)
	require.NotNil(t, usage)
}

// TestChat_BuffersLargeSSEFrames asserts a single SSE frame larger
// than bufio.Scanner's default 64 KiB token size is decoded in full.
// Without the explicit Buffer(...) call in consumeSSE, the scanner
// would silently truncate long completions.
func TestChat_BuffersLargeSSEFrames(t *testing.T) {
	large := strings.Repeat("a", 200*1024) // 200 KiB
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		writeSSE(w, fmt.Sprintf(`{"choices":[{"index":0,"delta":{"content":%q},"finish_reason":null}]}`, large))
		writeSSE(w, `{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
		writeSSE(w, "[DONE]")
	}))
	defer server.Close()

	c := newTestClient(t, server.URL)
	var got string
	usage, err := c.Chat(context.Background(), llm.ChatRequest{
		Messages: []llm.Message{{Role: "user", Content: "hi"}},
	}, func(s string, u *llm.ChatUsage) error {
		if u != nil {
			return nil
		}
		got += s
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, large, got)
	require.NotNil(t, usage)
}

// TestNewClient_GatedOnLLMEnabled asserts that constructing a client
// when LLM_ENABLED is false fails closed at startup.
func TestNewClient_GatedOnLLMEnabled(t *testing.T) {
	cfg := &config.Config{
		LLMEnabled: false,
		LLMBaseURL: "http://example.com",
		LLMAPIKey:  "sk-prod-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		LLMModel:   "test",
	}
	tracer := noop.NewTracerProvider().Tracer("test")
	_, err := NewClient(cfg, tracer)
	require.Error(t, err)
}

// TestNewClient_RequiresBaseURLAndModel asserts the constructor
// fails closed when LLM is on but base URL or model is missing.
// LLM_API_KEY is intentionally NOT in this list — empty is allowed
// for vLLM started without --api-key.
func TestNewClient_RequiresBaseURLAndModel(t *testing.T) {
	tracer := noop.NewTracerProvider().Tracer("test")
	cases := []struct {
		name string
		cfg  *config.Config
	}{
		{"no base URL", &config.Config{LLMEnabled: true, LLMProvider: "vllm", LLMAPIKey: "k", LLMModel: "m"}},
		{"no model", &config.Config{LLMEnabled: true, LLMProvider: "vllm", LLMBaseURL: "http://x", LLMAPIKey: "k"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewClient(tc.cfg, tracer)
			require.Error(t, err)
		})
	}
}

// TestNewClient_AllowsEmptyAPIKey confirms an empty LLM_API_KEY
// succeeds — vLLM started without --api-key accepts any bearer.
// Use a public URL so the SSRF guard doesn't fire first.
func TestNewClient_AllowsEmptyAPIKey(t *testing.T) {
	cfg := &config.Config{
		LLMEnabled:  true,
		LLMProvider: "vllm",
		LLMBaseURL:  "http://example.com",
		LLMAPIKey:   "",
		LLMModel:    "test",
		LLMTimeout:  "30s",
	}
	tracer := noop.NewTracerProvider().Tracer("test")
	_, err := NewClient(cfg, tracer)
	require.NoError(t, err)
}

// TestNewClient_RejectsPrivateHostByDefault asserts the SSRF guard:
// an LLM_BASE_URL pointing at 127.0.0.1 (loopback) is refused unless
// LLM_ALLOW_PRIVATE_URL=true.
func TestNewClient_RejectsPrivateHostByDefault(t *testing.T) {
	cfg := &config.Config{
		LLMEnabled:  true,
		LLMProvider: "vllm",
		LLMBaseURL:  "http://127.0.0.1:8000/v1",
		LLMAPIKey:   "",
		LLMModel:    "test",
		LLMTimeout:  "30s",
		// LLMAllowPrivateURL intentionally false.
	}
	tracer := noop.NewTracerProvider().Tracer("test")
	_, err := NewClient(cfg, tracer)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "LLM_ALLOW_PRIVATE_URL")
}

// TestNewClient_AllowsPrivateHostWithOverride confirms the escape
// hatch works.
func TestNewClient_AllowsPrivateHostWithOverride(t *testing.T) {
	cfg := &config.Config{
		LLMEnabled:         true,
		LLMProvider:        "vllm",
		LLMBaseURL:         "http://127.0.0.1:8000/v1",
		LLMAPIKey:          "",
		LLMModel:           "test",
		LLMTimeout:         "30s",
		LLMAllowPrivateURL: true,
	}
	tracer := noop.NewTracerProvider().Tracer("test")
	_, err := NewClient(cfg, tracer)
	require.NoError(t, err)
}

// TestMapError_NilReturnsNil pins the nil-error contract.
func TestMapError_NilReturnsNil(t *testing.T) {
	c := &Client{}
	assert.NoError(t, c.mapError(nil))
}

// TestMapError_UnknownTypeBubblesAsProviderUnavailable pins the
// fallback branch: an error that doesn't match any known typed
// error collapses to ErrProviderUnavailable.
func TestMapError_UnknownTypeBubblesAsProviderUnavailable(t *testing.T) {
	c := &Client{}
	err := errors.New("totally unknown failure mode")
	mapped := c.mapError(err)
	require.Error(t, mapped)
	assert.True(t, errors.Is(mapped, llm.ErrProviderUnavailable))
}

// TestMapError_DeadlineExceededRoutesToCtxCanceled pins the
// context-deadline branch — same routing as context.Canceled.
func TestMapError_DeadlineExceededRoutesToCtxCanceled(t *testing.T) {
	c := &Client{}
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Nanosecond)
	defer cancel()
	<-ctx.Done()
	mapped := c.mapError(ctx.Err())
	assert.True(t, errors.Is(mapped, llm.ErrContextCanceled))
}

// TestMapError_UpstreamStatusErrorRoutes pins the typed
// upstreamStatusError path: 429 → ErrRateLimited, anything else →
// ErrProviderUnavailable.
func TestMapError_UpstreamStatusErrorRoutes(t *testing.T) {
	c := &Client{}
	t.Run("429 → ErrRateLimited", func(t *testing.T) {
		mapped := c.mapError(&upstreamStatusError{status: http.StatusTooManyRequests})
		assert.True(t, errors.Is(mapped, llm.ErrRateLimited), "got %v", mapped)
	})
	t.Run("500 → ErrProviderUnavailable", func(t *testing.T) {
		mapped := c.mapError(&upstreamStatusError{status: http.StatusInternalServerError})
		assert.True(t, errors.Is(mapped, llm.ErrProviderUnavailable), "got %v", mapped)
	})
	t.Run("404 → ErrProviderUnavailable", func(t *testing.T) {
		mapped := c.mapError(&upstreamStatusError{status: http.StatusNotFound})
		assert.True(t, errors.Is(mapped, llm.ErrProviderUnavailable), "got %v", mapped)
	})
}

// TestMapError_URLErrorRoutesToProviderUnavailable pins the network
// branch: a *url.Error wrapping a dial failure routes to
// ErrProviderUnavailable.
func TestMapError_URLErrorRoutesToProviderUnavailable(t *testing.T) {
	c := &Client{}
	urlErr := &net.OpError{
		Op:  "dial",
		Net: "tcp",
		Err: errors.New("connection refused"),
	}
	wrapped := fmt.Errorf("vllm: post: %w", &urlErrorShim{opErr: urlErr})
	mapped := c.mapError(wrapped)
	assert.True(t, errors.Is(mapped, llm.ErrProviderUnavailable), "got %v", mapped)
}

// urlErrorShim exposes a net.OpError via errors.As to *url.Error so
// the test can drive mapError's *url.Error branch without a real HTTP
// transport.
type urlErrorShim struct{ opErr error }

func (u *urlErrorShim) Error() string { return u.opErr.Error() }
func (u *urlErrorShim) Unwrap() error { return u.opErr }

// TestBuildRequest_RejectsUnknownRoles confirms defense-in-depth: a
// caller that bypasses the chat service's validate() still can't
// poison the wire.
func TestBuildRequest_RejectsUnknownRoles(t *testing.T) {
	_, err := buildRequest("m", llm.ChatRequest{
		Messages: []llm.Message{{Role: "tool", Content: "x"}},
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, llm.ErrInvalidInput))
}

// TestCheckHost_RejectsLoopbackByDefault pins the SSRF guard at the
// DNS level.
func TestCheckHost_RejectsLoopbackByDefault(t *testing.T) {
	err := checkHost("127.0.0.1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "loopback")
}

// TestCheckHost_RejectsRFC1918ByDefault pins the RFC1918 branch.
func TestCheckHost_RejectsRFC1918ByDefault(t *testing.T) {
	err := checkHost("10.0.0.1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "private")
}

// TestCheckHost_RejectsLinkLocalByDefault pins the link-local branch.
func TestCheckHost_RejectsLinkLocalByDefault(t *testing.T) {
	err := checkHost("169.254.169.254") // AWS metadata service
	require.Error(t, err)
	assert.Contains(t, err.Error(), "link-local")
}

// TestCheckHost_RejectsIPv6LoopbackByDefault pins the IPv6 loopback
// branch (::1).
func TestCheckHost_RejectsIPv6LoopbackByDefault(t *testing.T) {
	err := checkHost("::1")
	require.Error(t, err)
}
