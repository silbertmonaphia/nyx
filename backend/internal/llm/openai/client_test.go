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
	"nyx/internal/llm/ssrf"
	"nyx/internal/platform/config"
)

// newTestClient wires a Client pointed at the given test server URL.
// The full *config.Config is built so NewClient exercises the same
// production constructor — the only thing that differs is the
// BaseURL. tracer is the no-op provider so spans are free.
func newTestClient(t *testing.T, serverURL string) *Client {
	t.Helper()
	cfg := &config.Config{
		LLMEnabled:         true,
		LLMBaseURL:         serverURL,
		LLMAPIKey:          "sk-real-test-key-1234567890",
		LLMModel:           "test-model",
		LLMAllowPrivateURL: true,
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
// (Stream=true, IncludeUsage=true, MaxTokens→MaxCompletionTokens, Model, Messages) so
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
	// rename breaks StreamOptions or MaxCompletionTokens.
	reqPtr, _ := captured.Load().(*openai.ChatCompletionRequest)
	require.NotNil(t, reqPtr)
	assert.True(t, reqPtr.Stream)
	require.NotNil(t, reqPtr.StreamOptions)
	assert.True(t, reqPtr.StreamOptions.IncludeUsage)
	assert.Equal(t, 100, reqPtr.MaxCompletionTokens)
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
			writeSSE(w, `{"id":"x","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"x"},"finish_reason":null}]}`)
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

// TestChat_ToolCallsIgnored pins the legacy Chat path's contract:
// tool-call deltas are decoded from the SSE stream but dropped
// before the legacy callback fires (the legacy
// `func(string, *ChatUsage) error` signature cannot represent
// tool calls). The typed callback is ChatDelta's job; Chat stays
// backward-compatible so chat/service.go compiles untouched.
//
// Mirrors vllm/client_test.go's TestChat_ToolCallsIgnored — both
// providers drop tool-call frames on the legacy path.
func TestChat_ToolCallsIgnored(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		writeSSE(w, `{"id":"x","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":null,"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"list_feeds","arguments":""}}]},"finish_reason":null}]}`)
		writeSSE(w, `{"id":"x","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"limit\""}}]},"finish_reason":null}]}`)
		writeSSE(w, `{"id":"x","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"done"},"finish_reason":null}]}`)
		writeSSE(w, `{"id":"x","object":"chat.completion.chunk","created":1,"model":"m","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`)
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
	assert.Equal(t, []string{"done"}, deltas)
	require.NotNil(t, usage)
	assert.Equal(t, 7, usage.TotalTokens)
}

// TestChatDelta_ToolCallStreaming_SurfacesDeltas is the canned-SSE
// replay test for the typed callback. Fabricates an OpenAI stream
// that emits a parallel-pair tool call (two calls in one turn) and
// asserts the assembled Delta sequence:
//
//   - role-only frame (no Delta, no callback fire)
//   - tool-call #0 name frame → DeltaToolCall{ID="call_a", Name="list_feeds"}
//   - tool-call #1 name frame → DeltaToolCall{ID="call_b", Name="get_feed"}
//   - tool-call #0 partial args frame → DeltaToolCall{Arguments:`{"limit":`}
//   - tool-call #1 partial args frame → DeltaToolCall{Arguments:`{"id":`}
//   - tool-call #0 final-args frame → DeltaToolCall{Arguments:`10}`}
//   - tool-call #1 final-args frame → DeltaToolCall{Arguments:`42}`}
//   - content frame → DeltaContent{"fetched "}
//   - content frame → DeltaContent{"everything"}
//   - stop frame (no Delta, no callback fire)
//   - usage frame → DeltaUsage{TotalTokens:9}
//
// Parallel-call ordering is preserved (every call_a frame precedes
// the matching call_b frame in emission order), and the executor
// can reassemble by ID across frames.
func TestChatDelta_ToolCallStreaming_SurfacesDeltas(t *testing.T) {
	var captured atomic.Value // *openai.ChatCompletionRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req openai.ChatCompletionRequest
		_ = json.Unmarshal(body, &req)
		captured.Store(&req)

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		// Role frame — Delta.Content empty, no ToolCalls. Skipped.
		writeSSE(w, `{"id":"x","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`)
		// Parallel tool-call name emission: call_a index=0, call_b index=1.
		writeSSE(w, `{"id":"x","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"list_feeds","arguments":""}},{"index":1,"id":"call_b","type":"function","function":{"name":"get_feed","arguments":""}}]},"finish_reason":null}]}`)
		// Tool-call args partials — one frame per call (single
		// choices[] delta, two tool_calls).
		writeSSE(w, `{"id":"x","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"limit\":"}},{"index":1,"function":{"arguments":"{\"id\":"}}]},"finish_reason":null}]}`)
		writeSSE(w, `{"id":"x","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"10}"}},{"index":1,"function":{"arguments":"42}"}}]},"finish_reason":null}]}`)
		// Mixed content frames after the tool calls.
		writeSSE(w, `{"id":"x","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"fetched "},"finish_reason":null}]}`)
		writeSSE(w, `{"id":"x","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"everything"},"finish_reason":null}]}`)
		// Stop frame — Delta empty, finish_reason set.
		writeSSE(w, `{"id":"x","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`)
		// Usage frame (Choices empty; Usage non-nil).
		writeSSE(w, `{"id":"x","object":"chat.completion.chunk","created":1,"model":"m","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":4,"total_tokens":9}}`)
		writeSSE(w, "[DONE]")
	}))
	defer server.Close()

	c := newTestClient(t, server.URL)

	var frames []llm.Delta
	var finalUsage *llm.ChatUsage
	usage, err := c.ChatDelta(context.Background(), llm.ChatRequest{
		Messages: []llm.Message{{Role: "user", Content: "hi"}},
		Tools: []llm.Tool{{
			Type:     "function",
			Function: llm.ToolFunction{Name: "list_feeds"},
		}, {
			Type:     "function",
			Function: llm.ToolFunction{Name: "get_feed"},
		}},
	}, func(d llm.Delta) error {
		if d.Kind == llm.DeltaUsage {
			finalUsage = d.Usage
		}
		frames = append(frames, d)
		return nil
	})
	require.NoError(t, err)
	require.NotNil(t, usage)
	assert.Equal(t, 9, usage.TotalTokens)
	assert.Equal(t, usage, finalUsage, "ChatDelta returns the trailing *ChatUsage from the usage frame")

	// First two frames: tool-call name emissions (one per call,
	// in emission order).
	require.Len(t, frames, 9) // 6 tool calls (name+args+name+args+name+args) + 2 content + 1 usage
	require.Equal(t, llm.DeltaToolCall, frames[0].Kind)
	require.NotNil(t, frames[0].ToolCall)
	assert.Equal(t, "call_a", frames[0].ToolCall.ID)
	assert.Equal(t, "list_feeds", frames[0].ToolCall.Name)
	assert.Equal(t, 0, frames[0].ToolCall.Index)
	assert.Equal(t, llm.DeltaToolCall, frames[1].Kind)
	require.NotNil(t, frames[1].ToolCall)
	assert.Equal(t, "call_b", frames[1].ToolCall.ID)
	assert.Equal(t, "get_feed", frames[1].ToolCall.Name)
	assert.Equal(t, 1, frames[1].ToolCall.Index)

	// Two more tool-call frames with the partial args.
	assert.Equal(t, llm.DeltaToolCall, frames[2].Kind)
	assert.Equal(t, `{"limit":`, frames[2].ToolCall.Arguments)
	assert.Equal(t, 0, frames[2].ToolCall.Index)
	assert.Equal(t, llm.DeltaToolCall, frames[3].Kind)
	assert.Equal(t, `{"id":`, frames[3].ToolCall.Arguments)
	assert.Equal(t, 1, frames[3].ToolCall.Index)

	// Final args frames — the model closed both JSON blobs.
	assert.Equal(t, llm.DeltaToolCall, frames[4].Kind)
	assert.Equal(t, `10}`, frames[4].ToolCall.Arguments)
	assert.Equal(t, llm.DeltaToolCall, frames[5].Kind)
	assert.Equal(t, `42}`, frames[5].ToolCall.Arguments)

	// Content frames: "fetched " + "everything".
	assert.Equal(t, llm.DeltaContent, frames[6].Kind)
	assert.Equal(t, "fetched ", frames[6].Content)
	assert.Equal(t, llm.DeltaContent, frames[7].Kind)
	assert.Equal(t, "everything", frames[7].Content)
	assert.Equal(t, llm.DeltaUsage, frames[8].Kind)

	// Wire-shape assertion: the request body carries the tools
	// we passed in (so a future SDK drift that renames Tools is
	// caught here).
	reqPtr, _ := captured.Load().(*openai.ChatCompletionRequest)
	require.NotNil(t, reqPtr)
	require.Len(t, reqPtr.Tools, 2)
	assert.Equal(t, openai.ToolTypeFunction, reqPtr.Tools[0].Type)
	require.NotNil(t, reqPtr.Tools[0].Function)
	assert.Equal(t, "list_feeds", reqPtr.Tools[0].Function.Name)
	require.NotNil(t, reqPtr.Tools[1].Function)
	assert.Equal(t, "get_feed", reqPtr.Tools[1].Function.Name)
}

// TestChatDelta_ToolChoiceForwarded pins that a non-nil
// ChatRequest.ToolChoice is forwarded to the upstream verbatim.
// Today callers pass "auto" / "none" / "required" (strings) or an
// object {type:"function", function:{name:"x"}}. The SDK's
// ChatCompletionRequest.ToolChoice is `any`, so we don't have to
// care which shape the caller picked.
func TestChatDelta_ToolChoiceForwarded(t *testing.T) {
	var captured atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req openai.ChatCompletionRequest
		_ = json.Unmarshal(body, &req)
		captured.Store(&req)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		writeSSE(w, `{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
		writeSSE(w, "[DONE]")
	}))
	defer server.Close()

	c := newTestClient(t, server.URL)
	_, err := c.ChatDelta(context.Background(), llm.ChatRequest{
		Messages:   []llm.Message{{Role: "user", Content: "hi"}},
		ToolChoice: "auto",
	}, func(llm.Delta) error { return nil })
	require.NoError(t, err)

	reqPtr, _ := captured.Load().(*openai.ChatCompletionRequest)
	require.NotNil(t, reqPtr)
	assert.Equal(t, "auto", reqPtr.ToolChoice)
}

// TestChatDelta_PureToolCallTurn_NoStrayContent covers the spec
// scenario "Pure tool-call turn has no stray content": a model turn
// that emits only tool calls (no content) must surface DeltaToolCall
// frames only — no DeltaContent frames, no empty string.
func TestChatDelta_PureToolCallTurn_NoStrayContent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		writeSSE(w, `{"choices":[{"index":0,"delta":{"role":"assistant","content":null,"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]},"finish_reason":null}]}`)
		writeSSE(w, `{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`)
		writeSSE(w, `{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
		writeSSE(w, "[DONE]")
	}))
	defer server.Close()

	c := newTestClient(t, server.URL)
	var frames []llm.Delta
	_, err := c.ChatDelta(context.Background(), llm.ChatRequest{
		Messages: []llm.Message{{Role: "user", Content: "hi"}},
	}, func(d llm.Delta) error {
		frames = append(frames, d)
		return nil
	})
	require.NoError(t, err)
	require.Len(t, frames, 2) // 1 tool call + 1 usage
	assert.Equal(t, llm.DeltaToolCall, frames[0].Kind)
	assert.Equal(t, "f", frames[0].ToolCall.Name)
	assert.Equal(t, "{}", frames[0].ToolCall.Arguments)
	assert.Equal(t, llm.DeltaUsage, frames[1].Kind)
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

// TestEmbed_ReturnsVectorsInInputOrder pins the happy path:
// httptest server returns 2 embeddings and the client surfaces them
// in input order. Index field is honoured (server might reorder).
func TestEmbed_ReturnsVectorsInInputOrder(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/embeddings", r.URL.Path)
		assert.Equal(t, http.MethodPost, r.Method)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// Echo each input with its index, in input order.
		_, _ = fmt.Fprintln(w, `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[1.0,2.0]},{"object":"embedding","index":1,"embedding":[3.0,4.0]}],"model":"test-embed","usage":{"prompt_tokens":4,"completion_tokens":0,"total_tokens":4}}`)
	}))
	defer server.Close()

	c := newTestClient(t, server.URL)
	vecs, err := c.Embed(context.Background(), llm.EmbedRequest{
		Model:  "test-embed",
		Inputs: []string{"hello", "world"},
	})
	require.NoError(t, err)
	require.Len(t, vecs, 2)
	assert.Equal(t, []float32{1.0, 2.0}, vecs[0])
	assert.Equal(t, []float32{3.0, 4.0}, vecs[1])
}

// TestEmbed_EmptyInputsIsNoop pins the contract: an empty Inputs
// slice is a no-op (no provider call). Mirrors vllm/client_test.go.
func TestEmbed_EmptyInputsIsNoop(t *testing.T) {
	c := newTestClient(t, "http://127.0.0.1:1") // unreachable; proves no call
	vecs, err := c.Embed(context.Background(), llm.EmbedRequest{Model: "m", Inputs: nil})
	require.NoError(t, err)
	assert.Empty(t, vecs)
}

// TestEmbed_OutOfRangeIndexReturnsSentinel pins the malformed-
// upstream guard: if the server returns data with an index that
// doesn't fit the input slice, surface ErrProviderUnavailable
// rather than panic.
func TestEmbed_OutOfRangeIndexReturnsSentinel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		// Caller will send 2 inputs but this returns index=99.
		_, _ = fmt.Fprintln(w, `{"data":[{"index":99,"embedding":[1.0]}]}`)
	}))
	defer server.Close()

	c := newTestClient(t, server.URL)
	_, err := c.Embed(context.Background(), llm.EmbedRequest{
		Model:  "m",
		Inputs: []string{"a", "b"},
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, llm.ErrProviderUnavailable))
}

// TestEmbed_UpstreamErrorReturnsSentinel pins the 5xx → sentinel
// mapping so a future regression that drops the mapping breaks
// the chat-side fallback path.
func TestEmbed_UpstreamErrorReturnsSentinel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	c := newTestClient(t, server.URL)
	_, err := c.Embed(context.Background(), llm.EmbedRequest{Model: "m", Inputs: []string{"x"}})
	require.Error(t, err)
	assert.True(t, errors.Is(err, llm.ErrProviderUnavailable))
}

func TestEmbed_Upstream400ReturnsInvalidInput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer server.Close()

	c := newTestClient(t, server.URL)
	_, err := c.Embed(context.Background(), llm.EmbedRequest{Model: "m", Inputs: []string{"x"}})
	require.Error(t, err)
	assert.True(t, errors.Is(err, llm.ErrInvalidInput))
	assert.False(t, errors.Is(err, llm.ErrProviderUnavailable))
}

func TestNewClient_RefusesPrivateBaseURLByDefault(t *testing.T) {
	cfg := &config.Config{
		LLMEnabled: true,
		LLMBaseURL: "http://127.0.0.1:8000/v1",
		LLMAPIKey:  "sk-real-test-key-1234567890",
		LLMModel:   "test-model",
	}
	tracer := noop.NewTracerProvider().Tracer("test")
	_, err := NewClient(cfg, tracer)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "LLM_ALLOW_PRIVATE_URL")
}

func TestNewClient_AllowsPrivateWithOperatorFlag(t *testing.T) {
	cfg := &config.Config{
		LLMEnabled:         true,
		LLMBaseURL:         "http://127.0.0.1:8000/v1",
		LLMAPIKey:          "sk-real-test-key-1234567890",
		LLMModel:           "test-model",
		LLMAllowPrivateURL: true,
	}
	tracer := noop.NewTracerProvider().Tracer("test")
	_, err := NewClient(cfg, tracer)
	require.NoError(t, err)
}

func TestNewClient_GuardedTransportStillStreams(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, `{"choices":[{"delta":{"content":"hello"}}]}`)
		writeSSE(w, `{"choices":[{"delta":{}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
		writeSSE(w, `[DONE]`)
	}))
	defer server.Close()

	orig := ssrf.ResolveHost
	ssrf.ResolveHost = func(host string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("93.184.216.34")}, nil
	}
	t.Cleanup(func() { ssrf.ResolveHost = orig })

	cfg := &config.Config{
		LLMEnabled: true,
		LLMBaseURL: server.URL,
		LLMAPIKey:  "sk-real-test-key-1234567890",
		LLMModel:   "test-model",
	}
	tracer := noop.NewTracerProvider().Tracer("test")
	c, err := NewClient(cfg, tracer)
	require.NoError(t, err)

	var got []string
	usage, err := c.Chat(context.Background(), llm.ChatRequest{
		Messages: []llm.Message{{Role: "user", Content: "hi"}},
	}, func(delta string, _ *llm.ChatUsage) error {
		got = append(got, delta)
		return nil
	})
	require.NoError(t, err)
	require.NotEmpty(t, got)
	assert.Equal(t, "hello", got[0])
	require.NotNil(t, usage)
	assert.Equal(t, 2, usage.TotalTokens)
}

func TestNewClient_PerDialRecheckRefusesReboundHost(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("request must not reach the server after rebinding")
	}))
	defer server.Close()

	orig := ssrf.ResolveHost
	ssrf.ResolveHost = func(host string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("93.184.216.34")}, nil
	}

	cfg := &config.Config{
		LLMEnabled: true,
		LLMBaseURL: server.URL,
		LLMAPIKey:  "sk-real-test-key-1234567890",
		LLMModel:   "test-model",
	}
	tracer := noop.NewTracerProvider().Tracer("test")
	c, err := NewClient(cfg, tracer)
	require.NoError(t, err)

	ssrf.ResolveHost = orig

	_, err = c.ChatDelta(context.Background(), llm.ChatRequest{
		Messages: []llm.Message{{Role: "user", Content: "hi"}},
	}, func(llm.Delta) error { return nil })
	require.Error(t, err)
	assert.ErrorIs(t, err, llm.ErrProviderUnavailable)
}
