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
// The streaming shape is callback-based: the Provider calls onDelta
// for each content fragment as it arrives, and returns the trailing
// usage summary once. This lets the chat handler own the SSE wire
// format (server-sent events, [DONE] terminator, error frames) and
// lets tests stub the Provider without an HTTP server.
package llm

import "context"

// Message is one turn in a chat-completion conversation.
//
// Role is one of "system", "user", "assistant". The chat service
// rejects any client-supplied "system" message (the system prompt is
// server-controlled) and rejects unknown roles — Provider
// implementations may rely on Role being one of those three strings
// when building the wire-format request.
type Message struct {
	Role    string
	Content string
}

// ChatRequest is the per-call shape the chat service hands to a
// Provider. Model, MaxTokens, and Temperature are operator-controlled
// knobs read from config; the service does not accept per-request
// overrides (model choice is operational, not client-driven).
type ChatRequest struct {
	Model       string
	Messages    []Message
	MaxTokens   int
	Temperature *float32
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
}
