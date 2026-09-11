package openai

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

	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace/noop"

	"nyx/internal/llm"
	"nyx/internal/platform/config"
)

// newTestClient wires a Client pointed at the given test server URL.
// The full *config.Config is built so NewClient exercises the same
// production constructor — the only thing that differs is the
// BaseURL. tracer is the no-op provider so spans are free.
func newTestClient(t *testing.T, serverURL string) *Client {
	t.Helper()
	cfg := &config.Config{
		LLMEnabled: true,
		LLMBaseURL: serverURL,
		LLMAPIKey:  "sk-real-test-key-1234567890",
		LLMModel:   "test-model",
	}
	tracer := noop.NewTracerProvider().Tracer("test")
	c, err := NewClient(cfg, tracer)
	require.NoError(t, err)
	return c
}

// writeSSE writes one data frame and flushes. Test servers use it to
// emit the canonical `data: <json>\n\n` chunk shape OpenAI uses.
// The double newline is the SSE frame terminator — the SDK's
// line-reader waits for it before decoding.
func writeSSE(w http.ResponseWriter, payload string) {
	_, _ = fmt.Fprintf(w, "data: %s\n\n", payload)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// TestChat_StreamsChunksAndUsage exercises the happy path:
// three content chunks + a usage chunk should fire onDelta three
// times in order and return the usage. Asserts the request shape
// (Stream=true, IncludeUsage=true, MaxTokens, Model, Messages) so
// drift between the SDK and our wrapper is caught here, not in prod.
func TestChat_StreamsChunksAndUsage(t *testing.T) {
	var captured atomic.Value // *openai.ChatCompletionRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req openai.ChatCompletionRequest
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

	// Verify the request shape — catches drift if a future SDK
	// rename breaks StreamOptions or MaxTokens.
	reqPtr, _ := captured.Load().(*openai.ChatCompletionRequest)
	require.NotNil(t, reqPtr)
	assert.True(t, reqPtr.Stream)
	require.NotNil(t, reqPtr.StreamOptions)
	assert.True(t, reqPtr.StreamOptions.IncludeUsage)
	assert.Equal(t, 100, reqPtr.MaxTokens)
	assert.Equal(t, "test-model", reqPtr.Model)
	require.Len(t, reqPtr.Messages, 1)
	assert.Equal(t, "user", reqPtr.Messages[0].Role)
	assert.Equal(t, "hi", reqPtr.Messages[0].Content)
}

// TestChat_RateLimit_ReturnsSentinel asserts a 429 from the upstream
// surfaces as llm.ErrRateLimited. The SDK decodes the JSON error
// body into *openai.APIError; mapError must route 429 → ErrRateLimited
// without inspecting the body.
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

// TestChat_ServerError_ReturnsSentinel asserts a 500 from the upstream
// surfaces as llm.ErrProviderUnavailable. The SDK wraps non-2xx
// responses into *openai.APIError; mapError must route 5xx →
// ErrProviderUnavailable.
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

// TestChat_NetworkError_ReturnsSentinel asserts that an unreachable
// upstream (closed listener) routes through *apierr.RequestError and
// becomes llm.ErrProviderUnavailable. We bind to localhost:0 and
// immediately close so the connect attempt fails.
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

// TestChat_ContextCanceled_StopsStream asserts that cancelling the
// context while the server is still streaming returns promptly with
// llm.ErrContextCanceled. The server emits a slow chunk stream so
// the client has time to cancel between chunks.
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
			writeSSE(w, fmt.Sprintf(`{"id":"x","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"x"},"finish_reason":null}]}`))
			flusher.Flush()
			time.Sleep(20 * time.Millisecond)
		}
	}))
	defer server.Close()

	c := newTestClient(t, server.URL)

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel after a short delay — well within the server's
	// 100-chunk × 20ms = 2s emission window.
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
	// Should return promptly after cancel, not wait for the server to
	// finish its 2-second emission loop. 500 ms is generous — the
	// real bound is "one chunk after cancel".
	assert.Less(t, elapsed, 500*time.Millisecond, "Chat did not honor context cancellation: %v", elapsed)
}

// TestChat_MalformedChunkMidStream asserts the client doesn't panic
// when the upstream emits an unparseable data frame. The SDK is
// expected to surface the error, which mapError should funnel into
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

	// Either the SDK returns the parse error (which routes to
	// ErrProviderUnavailable via the fallback) or the SDK raises a
	// connection-reset-style error — both acceptable. The hard
	// requirement is "no panic, returns promptly".
	require.Error(t, err)
	assert.True(t, errors.Is(err, llm.ErrProviderUnavailable), "expected ErrProviderUnavailable, got %v", err)
	assert.Nil(t, usage)
}

// TestNewClient_GatedOnLLMEnabled asserts that constructing a client
// when LLM_ENABLED is false fails closed at startup. main.go relies
// on this so a stale deployment with an env file referencing
// LLM_API_KEY but LLM_ENABLED=false never opens a network connection.
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

// TestNewClient_RequiresFieldsWhenEnabled asserts that all three
// required config fields (BaseURL, APIKey, Model) are individually
// checked when LLM_ENABLED=true. config.Load does the same check
// upfront, but the constructor repeats it so a future caller that
// bypasses Load (tests, ad-hoc tooling) still fails closed.
func TestNewClient_RequiresFieldsWhenEnabled(t *testing.T) {
	tracer := noop.NewTracerProvider().Tracer("test")
	cases := []struct {
		name string
		cfg  *config.Config
	}{
		{"no base URL", &config.Config{LLMEnabled: true, LLMAPIKey: "k", LLMModel: "m"}},
		{"no API key", &config.Config{LLMEnabled: true, LLMBaseURL: "http://x", LLMModel: "m"}},
		{"no model", &config.Config{LLMEnabled: true, LLMBaseURL: "http://x", LLMAPIKey: "k"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewClient(tc.cfg, tracer)
			require.Error(t, err)
		})
	}
}

// TestMapError_UnknownTypeBubblesAsProviderUnavailable pins the
// fallback branch: an error that doesn't match any known SDK type
// collapses to ErrProviderUnavailable. We can't tell from a bare
// error whether the upstream or the client is at fault, so
// ErrProviderUnavailable is the safe default that won't trigger a
// client retry loop.
func TestMapError_UnknownTypeBubblesAsProviderUnavailable(t *testing.T) {
	c := &Client{}
	// Bare errors.New — not a context error, not a *RequestError,
	// not an *APIError. Must collapse to ErrProviderUnavailable.
	err := errors.New("totally unknown failure mode")
	mapped := c.mapError(err)
	require.Error(t, mapped)
	assert.True(t, errors.Is(mapped, llm.ErrProviderUnavailable))
}

// TestMapError_NilReturnsNil pins the nil-error contract. Without
// it, a nil fall-through from Chat (e.g., after the final usage
// chunk) would surface as an opaque ErrProviderUnavailable to the
// caller.
func TestMapError_NilReturnsNil(t *testing.T) {
	c := &Client{}
	assert.NoError(t, c.mapError(nil))
}

// TestMapError_DeadlineExceededRoutesToCtxCanceled pins the
// context-deadline branch separately from Canceled — both should
// map to ErrContextCanceled so the handler's client-cancel
// detection treats them uniformly.
func TestMapError_DeadlineExceededRoutesToCtxCanceled(t *testing.T) {
	c := &Client{}
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Nanosecond)
	defer cancel()
	<-ctx.Done()
	mapped := c.mapError(ctx.Err())
	assert.True(t, errors.Is(mapped, llm.ErrContextCanceled))
}
