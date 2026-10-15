// Package paritytest asserts that both llm.Provider implementations
// (openai and vllm) emit the same Delta sequence against canned
// SSE. It lives in its own subpackage so it can import both
// internal/llm/openai and internal/llm/vllm without creating an
// import cycle (openai/vllm already depend on internal/llm for the
// Provider interface).
//
// Runs in the default `go test ./...` — no build tag. The subpackage
// itself exists only to break the openai↔vllm import cycle; running it
// unconditionally is cheap (canned SSE only) and protects against the
// wire-shape drift the spec calls out as a primary correctness risk.
// it; the default `go test ./internal/llm/...` skips these checks
// and runs only the per-provider tests.
package paritytest

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace/noop"

	"nyx/internal/llm"
	"nyx/internal/llm/openai"
	"nyx/internal/llm/vllm"
	"nyx/internal/platform/config"
)

// TestParity_ToolCallDeltaShape asserts both providers emit the same
// Delta sequence against a parallel-pair tool-call turn. The shared
// SSE payload mirrors the OpenAI /chat/completions streaming
// contract — vLLM's wire shape is identical so the same frames
// work for both servers.
//
// The contract:
//
//   - role-only frame → dropped (no callback fire)
//   - per tool-call: name + args-partials + args-finals (multiple
//     frames per call when the upstream emits partial JSON)
//   - per content delta: one DeltaContent frame
//   - terminal usage frame → one DeltaUsage frame
//
// Both providers must surface every DeltaToolCall frame; none may
// be silently dropped (the spec scenario "vLLM canned SSE replay
// surfaces tool calls — no deltas silently dropped").
func TestParity_ToolCallDeltaShape(t *testing.T) {
	frames := []string{
		// Role frame — empty content, no tool calls. Skipped.
		`{"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
		// Parallel tool-call name emission.
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"list_feeds","arguments":""}},{"index":1,"id":"call_b","type":"function","function":{"name":"get_feed","arguments":""}}]},"finish_reason":null}]}`,
		// Partial args.
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"limit\":"}},{"index":1,"function":{"arguments":"{\"id\":"}}]},"finish_reason":null}]}`,
		// Final args.
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"10}"}},{"index":1,"function":{"arguments":"42}"}}]},"finish_reason":null}]}`,
		// Content frames.
		`{"choices":[{"index":0,"delta":{"content":"fetched "},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{"content":"everything"},"finish_reason":null}]}`,
		// Stop frame — empty delta, finish_reason set.
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		// Usage frame.
		`{"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":4,"total_tokens":9}}`,
		`[DONE]`,
	}

	openaiFrames := captureFrames(t, "openai", frames)
	vllmFrames := captureFrames(t, "vllm", frames)

	require.Equal(t, len(openaiFrames), len(vllmFrames),
		"both providers must emit the same number of Delta frames")

	for i := range openaiFrames {
		o := openaiFrames[i]
		v := vllmFrames[i]
		assert.Equal(t, o.Kind, v.Kind, "frame %d Kind differs", i)
		if o.ToolCall == nil {
			assert.Nil(t, v.ToolCall, "frame %d ToolCall", i)
			continue
		}
		require.NotNil(t, v.ToolCall, "frame %d ToolCall", i)
		assert.Equal(t, o.ToolCall.ID, v.ToolCall.ID, "frame %d ID", i)
		assert.Equal(t, o.ToolCall.Name, v.ToolCall.Name, "frame %d Name", i)
		assert.Equal(t, o.ToolCall.Index, v.ToolCall.Index, "frame %d Index", i)
		assert.Equal(t, o.ToolCall.Arguments, v.ToolCall.Arguments, "frame %d Arguments", i)
	}

	// Sanity: the trailing frame is a DeltaUsage.
	last := vllmFrames[len(vllmFrames)-1]
	assert.Equal(t, llm.DeltaUsage, last.Kind)
	require.NotNil(t, last.Usage)
	assert.Equal(t, 9, last.Usage.TotalTokens)
}

// TestParity_ContentOnlyTurn pins the contract that a turn with no
// tool calls (zero Tools) produces only DeltaContent / DeltaUsage
// frames on both providers. This is the single-turn path the chat
// service exercises today; parity here means the agent loop's
// downstream consumers (SSE wire / store / etc.) don't need a
// per-provider branch.
func TestParity_ContentOnlyTurn(t *testing.T) {
	frames := []string{
		`{"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{"content":" world"},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
		`[DONE]`,
	}

	openaiFrames := captureFrames(t, "openai", frames)
	vllmFrames := captureFrames(t, "vllm", frames)

	require.Equal(t, len(openaiFrames), len(vllmFrames))
	for i := range openaiFrames {
		o := openaiFrames[i]
		v := vllmFrames[i]
		assert.Equal(t, o.Kind, v.Kind, "frame %d", i)
		if o.Kind == llm.DeltaContent {
			assert.Equal(t, o.Content, v.Content, "frame %d Content", i)
		}
	}
}

// captureFrames spins up an httptest.Server that emits the given
// SSE frames and runs the named provider's ChatDelta against it.
// Returns the captured Delta slice in emission order. The
// provider's HTTP client is pointed at the test server address;
// the SSRF guard is bypassed for the vLLM client via
// LLMAllowPrivateURL=true.
func captureFrames(t *testing.T, providerName string, frames []string) []llm.Delta {
	t.Helper()

	var captured []llm.Delta
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, f := range frames {
			_, _ = io.WriteString(w, "data: ")
			_, _ = io.WriteString(w, f)
			_, _ = io.WriteString(w, "\n\n")
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
		}
	}))
	defer server.Close()

	tracer := noop.NewTracerProvider().Tracer("test")
	switch providerName {
	case "openai":
		c, err := openai.NewClient(&config.Config{
			LLMEnabled: true,
			LLMBaseURL: server.URL,
			LLMAPIKey:  "sk-test-not-a-real-key",
			LLMModel:   "test-model",
		}, tracer)
		require.NoError(t, err)
		_, _ = c.ChatDelta(context.Background(), parityRequest(), func(d llm.Delta) error {
			captured = append(captured, d)
			return nil
		})
	case "vllm":
		c, err := vllm.NewClient(&config.Config{
			LLMEnabled:         true,
			LLMProvider:        "vllm",
			LLMBaseURL:         server.URL,
			LLMAPIKey:          "sk-test-not-a-real-key",
			LLMModel:           "test-model",
			LLMTimeout:         "30s",
			LLMAllowPrivateURL: true,
		}, tracer)
		require.NoError(t, err)
		_, _ = c.ChatDelta(context.Background(), parityRequest(), func(d llm.Delta) error {
			captured = append(captured, d)
			return nil
		})
	default:
		t.Fatalf("unknown provider %q", providerName)
	}
	return captured
}

// parityRequest builds the standard ChatRequest the parity tests
// use: a user message + two tool definitions. Tools is non-empty so
// the wire body matches what the agent loop sends.
func parityRequest() llm.ChatRequest {
	return llm.ChatRequest{
		Model: "test-model",
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: "hi"},
		},
		Tools: []llm.Tool{
			{Type: "function", Function: llm.ToolFunction{Name: "list_feeds"}},
			{Type: "function", Function: llm.ToolFunction{Name: "get_feed"}},
		},
	}
}
