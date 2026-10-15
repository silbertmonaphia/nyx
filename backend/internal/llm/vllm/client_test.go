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

// TestChat_ToolCallsIgnored pins the legacy Chat path's contract:
// tool-call deltas are decoded from the SSE stream but dropped
// before the legacy callback fires (the legacy
// `func(string, *ChatUsage) error` signature cannot represent
// tool calls). The typed callback is ChatDelta's job; Chat stays
// backward-compatible so chat/service.go compiles untouched.
//
// Mirrors openai/client_test.go's TestChat_ToolCallsIgnored.
func TestChat_ToolCallsIgnored(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// Tool-call delta (no content) — dropped on legacy path.
		writeSSE(w, `{"choices":[{"index":0,"delta":{"role":"assistant","content":null,"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"f","arguments":""}}]},"finish_reason":null}]}`)
		// Tool-call args partial — dropped.
		writeSSE(w, `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"limit\""}}]},"finish_reason":null}]}`)
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

// TestChatDelta_ToolCallStreaming_SurfacesDeltas is the canned-SSE
// replay test for vLLM's OpenAI-compatible wire shape. Fabricates
// a parallel-pair tool call (two calls in one turn) and asserts
// the assembled Delta sequence is identical to the OpenAI client's
// (covered by internal/llm/parity_test.go and the openai client
// test). The sequence:
//
//   - role-only frame (no Delta, no callback fire)
//   - tool-call #0 name frame → DeltaToolCall{ID="call_a", Name="list_feeds"}
//   - tool-call #1 name frame → DeltaToolCall{ID="call_b", Name="get_feed"}
//   - tool-call #0 partial args → DeltaToolCall{Arguments:`{"limit":`}
//   - tool-call #1 partial args → DeltaToolCall{Arguments:`{"id":`}
//   - tool-call #0 final args → DeltaToolCall{Arguments:`10}`}
//   - tool-call #1 final args → DeltaToolCall{Arguments:`42}`}
//   - content frame → DeltaContent{"fetched "}
//   - content frame → DeltaContent{"everything"}
//   - stop frame (no Delta, no callback fire)
//   - usage frame → DeltaUsage{TotalTokens:9}
//
// No deltas are silently dropped — that's the spec scenario
// "vLLM canned SSE replay surfaces tool calls".
func TestChatDelta_ToolCallStreaming_SurfacesDeltas(t *testing.T) {
	var captured atomic.Value // outboundRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req outboundRequest
		_ = json.Unmarshal(body, &req)
		captured.Store(&req)

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		writeSSE(w, `{"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`)
		writeSSE(w, `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"list_feeds","arguments":""}},{"index":1,"id":"call_b","type":"function","function":{"name":"get_feed","arguments":""}}]},"finish_reason":null}]}`)
		writeSSE(w, `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"limit\":"}},{"index":1,"function":{"arguments":"{\"id\":"}}]},"finish_reason":null}]}`)
		writeSSE(w, `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"10}"}},{"index":1,"function":{"arguments":"42}"}}]},"finish_reason":null}]}`)
		writeSSE(w, `{"choices":[{"index":0,"delta":{"content":"fetched "},"finish_reason":null}]}`)
		writeSSE(w, `{"choices":[{"index":0,"delta":{"content":"everything"},"finish_reason":null}]}`)
		writeSSE(w, `{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`)
		writeSSE(w, `{"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":4,"total_tokens":9}}`)
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
	assert.Equal(t, usage, finalUsage)

	require.Len(t, frames, 9)
	// Tool-call name emissions.
	assert.Equal(t, "call_a", frames[0].ToolCall.ID)
	assert.Equal(t, "list_feeds", frames[0].ToolCall.Name)
	assert.Equal(t, 0, frames[0].ToolCall.Index)
	assert.Equal(t, "call_b", frames[1].ToolCall.ID)
	assert.Equal(t, "get_feed", frames[1].ToolCall.Name)
	assert.Equal(t, 1, frames[1].ToolCall.Index)
	// Partial args.
	assert.Equal(t, `{"limit":`, frames[2].ToolCall.Arguments)
	assert.Equal(t, 0, frames[2].ToolCall.Index)
	assert.Equal(t, `{"id":`, frames[3].ToolCall.Arguments)
	assert.Equal(t, 1, frames[3].ToolCall.Index)
	// Final args.
	assert.Equal(t, `10}`, frames[4].ToolCall.Arguments)
	assert.Equal(t, `42}`, frames[5].ToolCall.Arguments)
	// Content.
	assert.Equal(t, "fetched ", frames[6].Content)
	assert.Equal(t, "everything", frames[7].Content)
	assert.Equal(t, llm.DeltaUsage, frames[8].Kind)

	// Wire-shape assertion: tools + tool_choice serialise when set.
	reqPtr, _ := captured.Load().(*outboundRequest)
	require.NotNil(t, reqPtr)
	require.Len(t, reqPtr.Tools, 2)
	assert.Equal(t, "function", reqPtr.Tools[0].Type)
	assert.Equal(t, "list_feeds", reqPtr.Tools[0].Function.Name)
	assert.Equal(t, "function", reqPtr.Tools[1].Type)
	assert.Equal(t, "get_feed", reqPtr.Tools[1].Function.Name)
}

// TestChatDelta_VLLMShapeDivergence_IndexOmitted exercises the spec
// scenario "vLLM canned SSE replay surfaces tool calls — no deltas
// silently dropped, including vLLM wire-shape divergence cases".
//
// Some vLLM builds omit the `index` field on tool-call frames
// after the first chunk (they emit parallel calls across multiple
// choices[] entries instead of one choice with a tool_calls array).
// The client must still surface every DeltaToolCall frame, defaulting
// the missing Index to 0.
func TestChatDelta_VLLMShapeDivergence_IndexOmitted(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// First chunk: index present.
		writeSSE(w, `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"f","arguments":""}}]},"finish_reason":null}]}`)
		// Subsequent chunks: NO index (a divergent vLLM shape).
		writeSSE(w, `{"choices":[{"index":0,"delta":{"tool_calls":[{"function":{"arguments":"{\"a\":"}}]},"finish_reason":null}]}`)
		writeSSE(w, `{"choices":[{"index":0,"delta":{"tool_calls":[{"function":{"arguments":"1}"}}]},"finish_reason":null}]}`)
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

	// Three tool-call frames; the latter two default Index to 0.
	require.Len(t, frames, 4) // 3 tool calls + 1 usage
	require.Equal(t, llm.DeltaToolCall, frames[0].Kind)
	assert.Equal(t, "c1", frames[0].ToolCall.ID)
	assert.Equal(t, "f", frames[0].ToolCall.Name)
	assert.Equal(t, 0, frames[0].ToolCall.Index)

	assert.Equal(t, llm.DeltaToolCall, frames[1].Kind)
	assert.Equal(t, `{"a":`, frames[1].ToolCall.Arguments)
	assert.Equal(t, 0, frames[1].ToolCall.Index, "missing Index defaults to 0")
	assert.Empty(t, frames[1].ToolCall.ID, "partial frame has no ID")

	assert.Equal(t, llm.DeltaToolCall, frames[2].Kind)
	assert.Equal(t, `1}`, frames[2].ToolCall.Arguments)
	assert.Equal(t, 0, frames[2].ToolCall.Index)
	assert.Empty(t, frames[2].ToolCall.ID)

	assert.Equal(t, llm.DeltaUsage, frames[3].Kind)
}

// TestChatDelta_RoleToolMessage_Accepted pins that buildRequest
// accepts role:"tool" + ToolCallID + Name on a message (the wire
// shape for a tool result). This is what makes the tool-call loop
// re-feed the model with a tool result and continue.
func TestChatDelta_RoleToolMessage_Accepted(t *testing.T) {
	var captured atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req outboundRequest
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
		Messages: []llm.Message{
			{Role: "user", Content: "list feeds"},
			{Role: "assistant", Content: "", ToolCalls: []llm.ToolCall{{ID: "call_a", Name: "list_feeds", Arguments: "{}"}}},
			{Role: "tool", ToolCallID: "call_a", Name: "list_feeds", Content: "[]"},
		},
	}, func(llm.Delta) error { return nil })
	require.NoError(t, err)

	reqPtr, _ := captured.Load().(*outboundRequest)
	require.NotNil(t, reqPtr)
	require.Len(t, reqPtr.Messages, 3)
	tool := reqPtr.Messages[2]
	assert.Equal(t, "tool", tool.Role)
	assert.Equal(t, "call_a", tool.ToolCallID)
	assert.Equal(t, "list_feeds", tool.Name)
	assert.Equal(t, "[]", tool.Content)
	assert.Empty(t, tool.ToolCalls)
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
// poison the wire. "tool" is now a real role (RoleTool carries
// tool results back to the model), so the unknown-role case uses a
// bogus literal to assert the rejection still fires.
func TestBuildRequest_RejectsUnknownRoles(t *testing.T) {
	_, err := buildRequest("m", llm.ChatRequest{
		Messages: []llm.Message{{Role: "bogus", Content: "x"}},
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

// TestEmbed_ReturnsVectorsInInputOrder pins the happy path:
// httptest server returns 2 embeddings and the client surfaces them
// in input order. The auth header assertion also catches a future
// regression where Embed forgets the empty-key handling.
func TestEmbed_ReturnsVectorsInInputOrder(t *testing.T) {
	var (
		gotPath        atomic.Value
		gotAuth        atomic.Value
		gotContentType atomic.Value
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath.Store(r.URL.Path)
		gotAuth.Store(r.Header.Get("Authorization"))
		gotContentType.Store(r.Header.Get("Content-Type"))
		// Match the OpenAI EmbeddingResponse layout that vLLM emits.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintln(w, `{"data":[{"embedding":[1.0,2.0,3.0]},{"embedding":[4.0,5.0,6.0]}]}`)
	}))
	defer server.Close()

	c := newTestClient(t, server.URL)
	vecs, err := c.Embed(context.Background(), llm.EmbedRequest{
		Model:  "embed-model",
		Inputs: []string{"hello", "world"},
	})
	require.NoError(t, err)
	require.Len(t, vecs, 2)
	assert.Equal(t, []float32{1.0, 2.0, 3.0}, vecs[0])
	assert.Equal(t, []float32{4.0, 5.0, 6.0}, vecs[1])
	assert.Equal(t, "/v1/embeddings", gotPath.Load())
	assert.Equal(t, "application/json", gotContentType.Load())
	assert.Equal(t, "Bearer sk-real-test-key-1234567890", gotAuth.Load())
}

// TestEmbed_AuthorizationOmittedWhenKeyEmpty pins the empty-key path
// for vLLM deployments started without --api-key. Sending `Bearer `
// (empty) would 401, so the header is omitted entirely.
func TestEmbed_AuthorizationOmittedWhenKeyEmpty(t *testing.T) {
	var gotAuth atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth.Store(r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintln(w, `{"data":[{"embedding":[0.0]}]}`)
	}))
	defer server.Close()

	cfg := &config.Config{
		LLMEnabled:         true,
		LLMProvider:        "vllm",
		LLMBaseURL:         server.URL,
		LLMModel:           "test-model",
		LLMTimeout:         "30s",
		LLMAllowPrivateURL: true,
	}
	c, err := NewClient(cfg, noop.NewTracerProvider().Tracer("test"))
	require.NoError(t, err)
	_, err = c.Embed(context.Background(), llm.EmbedRequest{Model: "m", Inputs: []string{"x"}})
	require.NoError(t, err)
	assert.Equal(t, "", gotAuth.Load())
}

// TestEmbed_LengthMismatchReturnsProviderUnavailable pins the
// malformed-upstream guard: if the server returns M embeddings for
// N inputs (M != N), surface ErrProviderUnavailable rather than
// panic on out-of-range indexing or silently return garbled data.
func TestEmbed_LengthMismatchReturnsProviderUnavailable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintln(w, `{"data":[{"embedding":[1.0]}]}`) // 1 vector, caller will send 2 inputs
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

// TestEmbed_UpstreamErrorReturnsSentinel pins the 5xx / 429 →
// ErrProviderUnavailable / ErrRateLimited mapping so a future
// regression that drops the sentinel breaks the chat-side handler
// fallback path.
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

// TestEmbed_EmptyInputsIsNoop pins the contract that an empty
// Inputs slice is not an error and produces an empty result — the
// rag package uses this to short-circuit when no feeds need
// embedding on a feed-write that hashes-match.
func TestEmbed_EmptyInputsIsNoop(t *testing.T) {
	c := newTestClient(t, "http://127.0.0.1:1") // unreachable; proves no call is made
	vecs, err := c.Embed(context.Background(), llm.EmbedRequest{Model: "m", Inputs: nil})
	require.NoError(t, err)
	assert.Empty(t, vecs)
}
