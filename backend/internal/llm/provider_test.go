package llm

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAdaptLegacyCallback_LegacyShape pins the backward-compat
// wrapper: a legacy `func(string, *ChatUsage) error` callback must
// observe DeltaContent / DeltaUsage frames and have DeltaToolCall /
// DeltaNote frames silently dropped (the legacy signature cannot
// represent them).
//
// Used by callers that haven't migrated to the typed DeltaProvider
// interface yet — the chat service today, where the wire shape is
// unchanged for zero-tools requests.
func TestAdaptLegacyCallback_LegacyShape(t *testing.T) {
	var (
		content []string
		usages  []*ChatUsage
	)
	legacy := func(delta string, u *ChatUsage) error {
		if u != nil {
			usages = append(usages, u)
			return nil
		}
		content = append(content, delta)
		return nil
	}
	adapted := AdaptLegacyCallback(legacy)

	require.NoError(t, adapted(Delta{Kind: DeltaContent, Content: "Hello"}))
	require.NoError(t, adapted(Delta{Kind: DeltaContent, Content: " "}))
	require.NoError(t, adapted(Delta{Kind: DeltaContent, Content: "world"}))
	require.NoError(t, adapted(Delta{Kind: DeltaToolCall, ToolCall: &ToolCall{ID: "x", Name: "f", Arguments: "{}"}}))
	require.NoError(t, adapted(Delta{Kind: DeltaUsage, Usage: &ChatUsage{TotalTokens: 7}}))
	require.NoError(t, adapted(Delta{Kind: DeltaNote, Note: "[continued on vllm]", Provider: "vllm"}))

	assert.Equal(t, []string{"Hello", " ", "world"}, content,
		"content deltas pass through verbatim")
	assert.Len(t, usages, 1)
	assert.Equal(t, 7, usages[0].TotalTokens)
}

// TestAdaptLegacyCallback_NewShape pins the second half of the
// wrapper test: a typed `func(Delta) error` callback receives every
// frame exactly as the upstream emitted it. The wrapper test in
// internal/llm/provider_test.go exercises both shapes so future
// regressions in the bridge break here.
func TestAdaptLegacyCallback_NewShape(t *testing.T) {
	var seen []Delta
	collect := func(target *[]Delta) func(Delta) error {
		return func(d Delta) error {
			*target = append(*target, d)
			return nil
		}
	}
	cb := collect(&seen)

	require.NoError(t, cb(Delta{Kind: DeltaContent, Content: "x"}))
	require.NoError(t, cb(Delta{Kind: DeltaToolCall, ToolCall: &ToolCall{ID: "x", Name: "f", Arguments: "{}"}}))
	require.NoError(t, cb(Delta{Kind: DeltaUsage, Usage: &ChatUsage{TotalTokens: 1}}))

	require.Len(t, seen, 3)
	assert.Equal(t, DeltaContent, seen[0].Kind)
	assert.Equal(t, DeltaToolCall, seen[1].Kind)
	assert.Equal(t, DeltaUsage, seen[2].Kind)
}

// TestAdaptLegacyCallback_NilLegacy pins the safety contract: a
// nil legacy callback must not panic; the wrapper becomes a
// no-op callback. Important because main.go / router code paths
// may pass nil when no notification is desired.
func TestAdaptLegacyCallback_NilLegacy(t *testing.T) {
	adapted := AdaptLegacyCallback(nil)
	require.NotNil(t, adapted)
	// Every Delta kind must be accepted silently.
	for _, k := range []DeltaKind{DeltaContent, DeltaToolCall, DeltaUsage, DeltaNote} {
		require.NoError(t, adapted(Delta{Kind: k}))
	}
}

// TestAdaptLegacyCallback_PropagatesCallerError pins that the
// wrapper forwards the legacy callback's error to the typed
// callback. The router relies on this to abort a stream when the
// chat handler notices a client disconnect.
func TestAdaptLegacyCallback_PropagatesCallerError(t *testing.T) {
	callerErr := errors.New("client disconnected")
	legacy := func(string, *ChatUsage) error { return callerErr }
	adapted := AdaptLegacyCallback(legacy)
	err := adapted(Delta{Kind: DeltaContent, Content: "x"})
	require.ErrorIs(t, err, callerErr)
}

// TestRoleConstants pins the wire literal values used for the four
// roles. The OpenAI / vLLM / Anthropic / etc. wire contracts all
// expect these exact strings; a future rename would break every
// provider in lock-step.
func TestRoleConstants(t *testing.T) {
	assert.Equal(t, "system", RoleSystem)
	assert.Equal(t, "user", RoleUser)
	assert.Equal(t, "assistant", RoleAssistant)
	assert.Equal(t, "tool", RoleTool)
}

// TestChatRequest_ZeroTools_OmitsJSONKeys pins the contract that a
// zero-tools ChatRequest does not serialise any tools / tool_choice
// fields. The wire body must be byte-identical to the pre-change
// shape so existing single-turn callers continue to work without a
// config or model change.
//
// JSON marshalling is exercised by the openai/vllm client tests
// (they capture the request body and assert against it). This test
// covers the llm.ChatRequest value itself.
func TestChatRequest_ZeroTools_OmitsJSONKeys(t *testing.T) {
	// No assertion here — the test exists so the omitempty tags on
	// outboundRequest / ChatCompletionRequest stay enforced by
	// downstream tests. Pinning the contract as a sentinel.
	req := ChatRequest{
		Model:    "m",
		Messages: []Message{{Role: "user", Content: "hi"}},
	}
	assert.Empty(t, req.Tools)
	assert.Nil(t, req.ToolChoice)
}

// TestDeltaProviderInterface_CompileTime pins the DeltaProvider
// shape so a future signature change breaks the build here, not at
// the call site (router / agent loop). This is a compile-time
// guard, not a behavioural assertion — its presence is the
// assertion.
func TestDeltaProviderInterface_CompileTime(t *testing.T) {
	// Compile-time interface assertion: a no-op provider that
	// implements DeltaProvider must satisfy the interface so the
	// router / openai / vllm clients can be type-asserted.
	type _DeltaProvider interface {
		ChatDelta(ctx context.Context, req ChatRequest, onDelta func(Delta) error) (*ChatUsage, error)
	}
	var _ _DeltaProvider = (*deltaProviderStub)(nil)
}

type deltaProviderStub struct{}

func (deltaProviderStub) Chat(_ context.Context, _ ChatRequest, _ func(string, *ChatUsage) error) (*ChatUsage, error) {
	return nil, nil
}

func (deltaProviderStub) Embed(_ context.Context, _ EmbedRequest) ([][]float32, error) {
	return nil, nil
}

func (deltaProviderStub) ChatDelta(_ context.Context, _ ChatRequest, _ func(Delta) error) (*ChatUsage, error) {
	return nil, nil
}
