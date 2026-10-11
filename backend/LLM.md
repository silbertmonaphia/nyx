# LLM provider seam

The `internal/llm` package is the provider-agnostic surface the chat
domain (and the future agent loop) consumes. Two implementations live
inside it:

- `internal/llm/openai.Client` — targets OpenAI proper (or any
  OpenAI-compatible server, including vLLM) via
  `github.com/sashabaranov/go-openai`.
- `internal/llm/vllm.Client` — targets a self-hosted vLLM with raw
  `net/http` + a hand-rolled SSE decoder. Adds per-dial IP-class DNS
  hardening that closes the rebinding window the SDK path leaves open.

Selection happens once at process startup in `cmd/api/main.go` via
the `LLM_PROVIDER` env var (default `openai`); the chat domain never
imports either implementation directly.

## Streaming callbacks

The `Provider` interface exposes the legacy streaming callback:

```go
Chat(ctx context.Context, req ChatRequest,
     onDelta func(delta string, finalUsage *ChatUsage) error) (*ChatUsage, error)
```

This is the callback the chat service uses today. It accepts content
fragments and the trailing usage chunk; the signature was deliberately
preserved so `internal/chat/` compiles without changes.

A second interface, `DeltaProvider`, exposes the typed-callback
extension:

```go
ChatDelta(ctx context.Context, req ChatRequest,
          onDelta func(Delta) error) (*ChatUsage, error)
```

`Delta` is a discriminated union (`Kind` + the relevant field) that
carries content deltas, tool-call deltas (full or partial), terminal
usage, and provider notes. New consumers (the agent loop in
`internal/agent/`) consume `ChatDelta`; today's chat service does not.

`llm.AdaptLegacyCallback` mechanically forwards the legacy callback
through the typed `Delta` stream, dropping `DeltaToolCall` and
`DeltaNote` frames (the legacy signature cannot represent them). For
zero-tools requests this is a no-op pass-through — no frames are
dropped because none are produced.

## Backward compatibility

A zero-tools `ChatRequest` MUST produce a request struct identical to
the pre-change shape: no extra `tools` / `tool_choice` JSON keys, no
extra frames, no extra bytes on the wire. This is enforced at two
levels:

1. **Wire shape** — `openai.Client.buildStreamRequest` and
   `vllm.Client.buildRequest` only attach `Tools` / `ToolChoice` when
   non-empty, and the `omitempty` JSON tags on `Tools` /
   `ToolChoice` / `ToolCallID` / `Name` / `ToolCalls` strip the
   fields from the marshalled body. The OpenAI client's
   `ChatCompletionRequest.Tools` / `.ToolChoice` fields are only set
   when the caller passed values.
2. **Streaming behaviour** — `Chat` (legacy callback) decodes
   tool-call frames but does NOT forward them; chat's RAG-only path
   never sees a tool call. `ChatDelta` surfaces them for callers
   that want them.

For the chat service's single-turn path (zero tools, the only path it
exercises today) the wire body and the SSE stream are byte-for-byte
unchanged from the pre-change behaviour. `internal/chat/service_test.go`
verifies this without modification.

## Tool calls

`Tool` is a tool definition:

```go
type Tool struct {
    Type     string       // today always "function"
    Function ToolFunction
}

type ToolFunction struct {
    Name        string
    Description string
    Parameters  any         // JSON Schema for the arguments
}
```

`ToolCall` is one tool-call invocation:

```go
type ToolCall struct {
    ID        string   // wire-stable identifier
    Name      string   // tool being called
    Arguments string   // partial or complete JSON-encoded arguments
    Index     int      // position within a parallel-call sequence
}
```

`Message` carries tool calls on the assistant role and tool results on
the tool role:

```go
type Message struct {
    Role       string
    Content    string
    ToolCallID string      // tool-result messages only
    Name       string      // tool-result messages only (originating tool name)
    ToolCalls  []ToolCall  // assistant messages only
}
```

`ChatRequest.Tools []Tool` and `ChatRequest.ToolChoice any` thread the
definitions + the optional tool-choice directive to the upstream. Both
fields are zero-omitted in the wire body.

## Provider failover and tool-call parity

`llm.Router` wraps two slots so a wedged upstream surfaces as a
transparent failover instead of a 502. The router exposes the same
behaviour on both `Chat` and `ChatDelta`.

Tool-bearing requests (non-empty `Tools` OR non-nil `ToolChoice`)
MAY NOT be failed over to the secondary provider unless
`LLM_FALLBACK_TOOLS_ENABLED` is on. `SetFallbackToolsEnabled` flips
the gate; the default is `false`. The router returns
`llm.ErrToolCallsUnsupported` (mapped to HTTP 503) on tool-bearing
failover while the gate is off — the same error whether the caller
uses `Chat` or `ChatDelta`.

The gate exists because vLLM's tool-call wire shape diverges from
OpenAI's in some builds. Failing open here would silently drop
in-flight tool-call deltas the caller already received. The operator
MUST keep the gate off until both providers' tool-call integration
tests are green in CI for ≥1 week — see `FUTURE_AGENT.md` / 6.1.

## Spec coverage

- `OpenAI and vLLM clients both surface tool calls` — `openai.Client.ChatDelta`
  + `vllm.Client.ChatDelta`; canned-SSE replay tests in
  `openai/client_test.go::TestChatDelta_ToolCallStreaming_SurfacesDeltas`
  and `vllm/client_test.go::TestChatDelta_ToolCallStreaming_SurfacesDeltas`
  + the cross-provider parity test in
  `internal/llm/paritytest/parity_test.go` (gated behind `-tags parity`).
- `Provider failover honours tool-call parity` —
  `router_test.go::TestRouter_ChatDelta_ToolBearingRefusedWithoutGate`
  + `TestRouter_ChatDelta_ToolBearingAllowedWithGate`.
- `Provider request accepts tool definitions` — `provider.go::Tool`,
  `ChatRequest.Tools`, `ChatRequest.ToolChoice`.
- `Message carries tool-call and tool-result fields` —
  `Message.ToolCalls`, `Message.ToolCallID`, `Message.Name`,
  `RoleTool`.
- `Streaming frames distinguish content from tool calls` —
  `Delta` union with `DeltaContent` / `DeltaToolCall` / `DeltaUsage` /
  `DeltaNote` kinds.

## Errors

Sentinel errors live in `internal/llm/errors.go`; the package
registers each with `api.RegisterSentinel` so handlers can funnel
through `api.MapError` unchanged:

| Sentinel                       | HTTP status | Wire message                  |
| ------------------------------ | ----------- | ----------------------------- |
| `ErrProviderUnavailable`       | 502         | Upstream LLM unavailable      |
| `ErrRateLimited`               | 429         | LLM rate limited              |
| `ErrInvalidInput`              | 400         | Invalid chat input            |
| `ErrContextCanceled`           | 499         | Client closed request         |
| `ErrAllProvidersFailed`        | 502         | Upstream LLM unavailable      |
| `ErrToolCallsUnsupported`      | 503         | LLM tool calls unsupported    |

`ErrToolCallsUnsupported` is new in this change; it surfaces the
router's tool-bearing failover refusal so the caller knows retrying
won't help (the gate is an operational decision, not a transient
condition).