// Package llm owns the provider-agnostic LLM surface. A Provider is a
// streaming chat-completion client; the chat domain (internal/chat)
// consumes one through the Chat method without coupling to any
// vendor's SDK.
//
// Two implementations live under this package:
//
//   - internal/llm/openai.Client targets OpenAI proper (or any
//     OpenAI-compatible server) via github.com/sashabaranov/go-openai.
//     Operators who point LLM_BASE_URL at a vLLM instance and don't
//     enable LLM_PROVIDER=vllm get the OpenAI client targeting vLLM
//     — the wire contract is identical, so it works.
//   - internal/llm/vllm.Client targets a self-hosted vLLM with raw
//     net/http + a hand-rolled SSE decoder. It adds per-dial IP-class
//     DNS hardening that closes the rebinding window the SDK path
//     leaves open, and accepts an empty LLM_API_KEY (vLLM started
//     without --api-key).
//
// Selection happens once at process startup in cmd/api/main.go via
// the LLM_PROVIDER env var (default "openai"); the chat domain
// never imports either implementation directly.
//
// The streaming shape is callback-based. Pre-change callers used
// `func(string, *ChatUsage) error`; that signature is preserved on
// the legacy Provider.Chat method. New consumers (the Python agent
// orchestrator via internal/chat.AgentProxy) consume the typed
// `func(Delta) error` callback
// exposed via DeltaProvider.ChatDelta. The legacy callback drops
// tool-call deltas — it cannot represent them — so callers that
// want tool calls MUST switch to ChatDelta. Use AdaptLegacyCallback
// to mechanically forward a legacy callback through the typed Delta
// stream when the upstream is known to carry zero tools (the RAG-only
// path today, where this is a no-op).
package llm

import "context"

// Role constants for the wire shape. "system" is server-controlled;
// client-supplied system messages are rejected by the chat service
// before they reach the provider. "tool" is the role that carries a
// prior assistant tool call's textual result back to the model — the
// identifier and originating tool name travel with the message so the
// provider can route the result correctly.
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// Tool is one tool definition the model is offered. Today only
// "function" tools are surfaced; the wire shape leaves room for
// provider-specific kinds (web_search, file_search, …) which the
// caller may pass via Type. Function.Name / Description / Parameters
// follow the OpenAI Chat Completions shape so the same value can be
// POSTed to either provider unchanged.
//
// Parameters is the JSON Schema the upstream validates arguments
// against. Pass any value json.Marshal can serialise (a struct, a
// map[string]any, or json.RawMessage from a generated schema); the
// provider does not interpret it.
type Tool struct {
	Type     string
	Function ToolFunction
}

// ToolFunction is the executable half of a Tool definition.
type ToolFunction struct {
	Name        string
	Description string
	// Parameters is the JSON Schema the model is expected to produce
	// arguments matching. json.Marshal will be used to serialise it;
	// pass a *json.RawMessage to embed a pre-rendered schema byte-
	// for-byte.
	Parameters any
}

// ToolCall is one tool-call invocation the model emitted. The OpenAI
// streaming wire shape emits one of these per chunk, with empty
// fields for the parts the model hasn't filled in yet — the executor
// reassembles them by ID. ID is the call's wire-stable identifier;
// Name is the tool being called; Arguments is the partial or
// complete JSON-encoded arguments blob. Index is the call's
// positional index within the same response chunk — useful when the
// model emits parallel calls and the executor wants to keep them
// ordered; zero on the final frame.
type ToolCall struct {
	ID        string
	Name      string
	Arguments string
	Index     int
}

// DeltaKind identifies which field of a Delta value is populated.
// Every Delta carries exactly one populated field per the open spec
// (Provider failover honours tool-call parity, etc.) so callers
// branch on Kind instead of inspecting every field.
//
// DeltaToolCall frames MAY carry partial fields (ID empty, Name
// empty, Arguments partial) — the executor MUST reassemble them by
// ID across frames. A final Delta with a populated Content and
// ToolCall == nil marks the end of a tool-call sequence.
type DeltaKind int

const (
	// DeltaContent delivers a textual chunk of the assistant's reply.
	DeltaContent DeltaKind = iota
	// DeltaToolCall delivers one tool-call invocation (full or partial).
	DeltaToolCall
	// DeltaUsage delivers the trailing usage summary. Providers that
	// don't surface usage (older vLLM builds) omit this frame.
	DeltaUsage
	// DeltaNote carries a provider-routed note (e.g. "continued on
	// <secondary>"). Pre-change callers never saw this; ChatDelta
	// consumers can render or ignore as they please.
	DeltaNote
)

// Delta is the typed union the new ChatDelta callback carries. Exactly
// one of Content / ToolCall / Usage / Note is populated per frame,
// discriminated by Kind. Pre-change callers using Chat's
// `func(string, *ChatUsage) error` callback receive Content and Usage
// as that signature requires; tool-call frames are silently dropped
// by the legacy callback path. AdaptLegacyCallback bridges the two.
type Delta struct {
	Kind     DeltaKind
	Content  string
	ToolCall *ToolCall
	Usage    *ChatUsage
	Note     string
	// Provider is set on DeltaNote frames so the consumer can render
	// "continued on <name>". Empty for all other kinds.
	Provider string
}

// Message is one turn in a chat-completion conversation.
//
// Role is one of RoleSystem / RoleUser / RoleAssistant / RoleTool.
// Provider implementations may rely on Role being one of those four
// strings when building the wire-format request; unknown roles are
// rejected by the chat service before they reach the provider.
//
// ToolCallID + Name are populated on RoleTool messages and identify
// the prior tool call whose result Content carries.
//
// ToolCalls is populated on RoleAssistant messages and carries the
// tool invocations the assistant emitted in this turn. Empty for
// text-only turns.
type Message struct {
	Role       string
	Content    string
	ToolCallID string
	Name       string
	ToolCalls  []ToolCall
}

// ChatRequest is the per-call shape the chat service hands to a
// Provider. Model, MaxTokens, and Temperature are operator-controlled
// knobs read from config; the service does not accept per-request
// overrides (model choice is operational, not client-driven).
//
// Tools is the ordered list of tools the model may invoke. Empty
// when the caller doesn't want tool calls — the upstream wire
// request omits the `tools` field entirely in that case (no extra
// frames, no extra bytes).
//
// ToolChoice is the optional tool-choice directive. nil = provider
// default. String values ("auto" / "none" / "required") and object
// values ({type:"function", function:{name:"x"}}) are both accepted;
// providers pass through to the upstream unchanged.
//
// OnNote is an optional mid-stream notification hook honoured by
// provider routers (e.g. llm.Router) when they fail over from one
// upstream to another. Providers other than the Router ignore it —
// pass nil when no notification is wanted. Routers fire OnNote
// exactly once per request, immediately before the secondary
// provider's first delta; a non-nil return value aborts the stream.
type ChatRequest struct {
	Model       string
	Messages    []Message
	MaxTokens   int
	Temperature *float32
	Tools       []Tool
	ToolChoice  any
	OnNote      func(text, provider string) error
}

// ChatUsage is the trailing usage summary OpenAI emits on the final
// SSE chunk when the request is made with stream_options.include_usage.
// PromptTokens + CompletionTokens may be zero for providers that don't
// surface usage; TotalTokens is what callers should treat as the
// cost-attribution signal.
type ChatUsage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
}

// Provider is the seam a feature (chat today; summaries, semantic
// search tomorrow) consumes.
//
// Chat streams a single completion. onDelta is invoked once per
// content fragment as the provider decodes it. Implementations MUST
// skip null content chunks (OpenAI emits them on role-change frames).
// For every content chunk, onDelta is called with a non-empty delta
// and a nil finalUsage. For the trailing usage chunk (when the
// upstream emits one), onDelta is called once with an empty delta
// and a non-nil finalUsage, signalling end-of-stream. If onDelta
// returns a non-nil error, Chat stops reading and returns that error
// unchanged — handlers can use this to surface a client disconnect
// (the SSE writer propagates the http.Flusher error and aborts).
//
// Tool calls: the legacy `func(string, *ChatUsage) error` signature
// cannot represent tool-call deltas. Implementations drop them on
// this path so the byte-for-byte behaviour for zero-tools requests is
// unchanged. Consumers that need tool calls MUST use ChatDelta.
//
// The returned *ChatUsage mirrors the last non-nil finalUsage
// passed to onDelta; it is non-nil when the provider surfaced usage.
// Providers that don't (older vLLM builds, custom backends) may
// return nil for both.
//
// Errors returned by Chat SHOULD be one of the package sentinels
// (ErrProviderUnavailable, ErrRateLimited, ErrInvalidInput,
// ErrContextCanceled) so the api.MapError funnel renders the right
// HTTP status. Implementations are free to wrap sentinels with
// fmt.Errorf("...: %w", ...) — MapError walks the chain with
// errors.Is.
type Provider interface {
	Chat(ctx context.Context, req ChatRequest, onDelta func(delta string, finalUsage *ChatUsage) error) (*ChatUsage, error)
	Embed(ctx context.Context, req EmbedRequest) ([][]float32, error)
}

// DeltaProvider is the typed-callback extension of Provider. It
// surfaces content deltas, tool-call deltas (full or partial),
// terminal usage, and provider notes through a single discriminated
// Delta value. Implementations populate exactly one of Delta's
// fields per frame, discriminated by Kind. Tool-call deltas MUST be
// delivered in emission order so the executor can reassemble
// parallel calls by ID; the same ID may appear across multiple
// frames as Name / Arguments fill in.
//
// The same SSE plumbing backs Provider.Chat and DeltaProvider.ChatDelta;
// the legacy callback drops DeltaToolCall / DeltaNote frames so the
// pre-change byte shape is preserved for zero-tools callers. New
// consumers (the agent loop) consume ChatDelta.
//
// ChatDelta's error contract mirrors Chat's: a non-nil return from
// onDelta aborts the stream and is propagated to the caller; sentinels
// wrap upstream errors via errors.Is.
type DeltaProvider interface {
	ChatDelta(ctx context.Context, req ChatRequest, onDelta func(Delta) error) (*ChatUsage, error)
}

// AdaptLegacyCallback returns a typed-callback wrapper that forwards
// the legacy `func(string, *ChatUsage) error` semantics through the
// new Delta stream:
//
//   - DeltaContent  → onDelta(content, nil)
//   - DeltaUsage    → onDelta("", usage)
//   - DeltaToolCall → dropped (the legacy signature cannot represent it)
//   - DeltaNote     → dropped
//
// Use this when a caller that owns the legacy callback shape needs
// to invoke a DeltaProvider (e.g. inside a transitional layer that
// hasn't been migrated yet). For zero-tools requests — the only path
// the chat service exercises today — the wrapper is a no-op
// pass-through: tool-call frames are never produced, so dropping
// them is sound.
func AdaptLegacyCallback(legacy func(delta string, finalUsage *ChatUsage) error) func(Delta) error {
	if legacy == nil {
		return func(Delta) error { return nil }
	}
	return func(d Delta) error {
		switch d.Kind {
		case DeltaContent:
			return legacy(d.Content, nil)
		case DeltaUsage:
			return legacy("", d.Usage)
		case DeltaToolCall, DeltaNote:
			// Legacy callbacks cannot represent these; drop silently.
			return nil
		default:
			return nil
		}
	}
}

// EmbedRequest is the per-call shape for embedding one or more
// strings. The rag package batches inputs into a single request to
// amortise the HTTP round-trip — every supported upstream (OpenAI,
// vLLM) accepts an array shape natively.
//
// Model is optional; the rag package resolves it from
// cfg.LLMEmbeddingModel at construction (falling back to
// cfg.LLMModel) and passes the resolved string on every call. An
// empty Model on the request is a programming error — clients that
// want "use whatever the provider is configured for" should pass
// the resolved string, not rely on a default.
type EmbedRequest struct {
	Model  string
	Inputs []string
}
