package chat

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"nyx/internal/llm"
	"nyx/internal/platform/config"
	"nyx/internal/rag"
	"nyx/internal/reqctx"
)

// ErrAgentNotEnabled is what the handler's streamProxy path returns
// when callers invoke it on a legacy-mode Service (the proxy is nil).
// Kept as a sentinel so any future internal caller that branches on
// IsAgentEnabled() and then reaches for the proxy gets a typed
// failure rather than a nil-pointer panic. Production handlers
// guard on IsAgentEnabled() before reaching this branch.
var ErrAgentNotEnabled = fmt.Errorf("chat: agent mode is not enabled")

// defaultSystemPrompt is the server-controlled persona prepended to
// every chat completion when LLM_SYSTEM_PROMPT is empty. Operators
// override via env var; clients cannot. Keep it short — every
// prepended token is billed on every request.
const defaultSystemPrompt = "You are a helpful assistant for a movie catalog. Answer concisely and ask for clarification when the user's question is ambiguous."

// Service validates chat requests, prepends the server-controlled
// system prompt, and forwards the conversation to the configured
// llm.Provider. The handler layer owns the SSE wire format; the
// service is provider-agnostic and could be reused by a future
// batch-completion endpoint without touching the streaming path.
//
// rag is the optional RAG facade. When nil, the rag:true flag is
// silently ignored — useful for deployments where LLM is enabled
// but embeddings aren't configured (single-model deployments that
// reuse the chat model for both, or operators who want chat-only).
// When non-nil, req.RAG=true triggers a top-K retrieval against
// the authenticated user's feeds and the retrieved passages are
// injected as a synthetic user-role message before the LLM call.
//
// proxy, when non-nil, switches the handler to the AGENT_ENABLED=true
// path: the handler streams the upstream Python service's SSE body
// verbatim via AgentProxy. nil proxy → legacy single-turn path
// (AGENT_ENABLED=false). IsAgentEnabled() returns proxy != nil so
// the handler dispatches on this flag and never reaches for the
// proxy without it being wired.
//
// RAG semantics under AGENT_ENABLED=true: the rag:true flag is
// IGNORED. The Python orchestrator's planner emits a step of kind
// "rag" when retrieval is appropriate; pre-LLM context injection
// happens INSIDE the agent run, not in the chat service. See
// openspec/changes/agent-langgraph/design.md §3 ("RAG flows through
// a new tiny Go endpoint").
type Service struct {
	provider        llm.Provider
	systemPrompt    string
	maxHistory      int
	maxMessageChars int
	maxTokens       int
	maxStreamDur    time.Duration
	rag             *rag.Service
	tracer          trace.Tracer

	proxy *AgentProxy
}

// NewService builds a Service from config. cfg supplies the per-
// request caps and the system prompt (or the hard-coded default
// when empty). tracer is the OTel tracer; on OTEL_ENABLED=false
// it's a noop and every span is free. ragSvc is the RAG facade —
// pass nil when embeddings aren't configured (the rag:true flag
// then degrades to a no-op rather than failing the request).
//
// proxy is intentionally left nil — the constructor builds the
// legacy single-turn path. Use NewServiceWithProxy to wire the
// AGENT_ENABLED=true path.
func NewService(p llm.Provider, cfg *config.Config, ragSvc *rag.Service, tracer trace.Tracer) *Service {
	prompt := cfg.LLMSystemPrompt
	if prompt == "" {
		prompt = defaultSystemPrompt
	}
	// The stream duration is parsed up-front (in config.Load →
	// validateDurations) so a typo in the env file fails at
	// startup, not on the first chat request. A zero duration
	// here would mean the env var was never set — fall back to
	// 10m, matching the setDefaults value, so a future config
	// drift doesn't silently disable the cap.
	maxStreamDur, err := time.ParseDuration(cfg.LLMMaxStreamDuration)
	if err != nil || maxStreamDur <= 0 {
		maxStreamDur = 10 * time.Minute
	}
	return &Service{
		provider:        p,
		systemPrompt:    prompt,
		maxHistory:      cfg.LLMMaxHistoryMessages,
		maxMessageChars: cfg.LLMMaxMessageChars,
		maxTokens:       cfg.LLMMaxTokens,
		maxStreamDur:    maxStreamDur,
		rag:             ragSvc,
		tracer:          tracer,
	}
}

// NewServiceWithProxy constructs a Service for the AGENT_ENABLED=true
// path. The proxy is required; ragSvc is unused on this path
// (RAG is an orchestrator-emitted step kind — the chat service
// no longer pre-injects context, so the rag facade is irrelevant
// when this constructor returns).
//
// tracer is still non-nil so the chat handler can start an OTel
// span around the proxy call; on OTEL_ENABLED=false the tracer is
// a noop and the span is free.
//
// The legacy fields (provider, systemPrompt, etc.) are populated
// from cfg so a future fall-back path can reuse the same Service
// instance without reconstructing it.
func NewServiceWithProxy(p llm.Provider, cfg *config.Config, tracer trace.Tracer, proxy *AgentProxy) *Service {
	s := NewService(p, cfg, nil, tracer)
	s.proxy = proxy
	return s
}

// IsAgentEnabled reports whether this Service is configured to
// stream through the Python agent proxy. Always false for the
// legacy constructor (NewService); true only when constructed via
// NewServiceWithProxy. The handler dispatches on this flag.
func (s *Service) IsAgentEnabled() bool {
	return s.proxy != nil
}

// Proxy returns the underlying agent proxy, or nil in the legacy
// configuration. The handler reaches for this only after
// IsAgentEnabled() returns true; tests use it to inject a proxy
// pointed at an httptest.Server without spinning up the real
// Python service.
func (s *Service) Proxy() *AgentProxy {
	return s.proxy
}

// Run is preserved as a thin shim for the legacy agent test
// surface (and any future internal caller). It returns
// ErrAgentNotEnabled because the Go-side planner/executor was
// retired in the agent-langgraph swap — the chat handler now
// delegates to Service.Proxy().Run instead, which forwards to the
// Python LangGraph orchestrator byte-for-byte. Kept (instead of
// removed) so a future caller that imports Service.Run gets a
// typed failure rather than a missing-symbol compile error.
func (s *Service) Run(_ context.Context, _ ChatRequest) (any, error) {
	return nil, ErrAgentNotEnabled
}

// Chat validates req, builds an llm.ChatRequest, and forwards it to
// the provider. onDelta is invoked once per non-empty content chunk
// in arrival order; the trailing usage chunk (when emitted) is
// forwarded with a populated finalUsage and an empty delta — the
// signal to handlers that the stream is ending.
//
// onNote is plumbed through to llm.Router when the configured
// provider is a router; other providers ignore it. Nil is a valid
// value — handlers that don't surface notes pass nil and the router
// just doesn't notify on failover. Routers fire onNote exactly
// once per request, immediately before the secondary provider's
// first delta.
//
// Validation runs before the provider call:
//   - messages must be non-empty
//   - every message role must be "user" or "assistant" (system from
//     a client is rejected — the system prompt is server-controlled)
//   - history length must be ≤ maxHistory
//   - per-message content must be ≤ maxMessageChars
//
// Any validation failure is returned as llm.ErrInvalidInput so the
// handler can render the 400 envelope via api.MapError before any
// SSE bytes are written.
//
// The system prompt is prepended in a freshly-allocated slice so the
// caller's Messages is never mutated — the same request body could
// be replayed on retry without the second call seeing the system
// message twice.
func (s *Service) Chat(ctx context.Context, req ChatRequest, onDelta func(delta string, finalUsage *llm.ChatUsage) error, onNote func(text, provider string) error) (*llm.ChatUsage, error) {
	ctx, span := s.tracer.Start(ctx, "chat.service",
		trace.WithAttributes(attribute.Int("chat.messages", len(req.Messages))),
	)
	defer span.End()

	if err := s.validate(&req); err != nil {
		span.RecordError(err)
		return nil, err
	}

	// Hard wall-clock deadline on the stream. Closes the "open &
	// idle" abuse vector — a chat stream held open longer than the
	// configured cap gets context-cancelled regardless of chunk
	// timing, and the handler's error path surfaces it as a
	// Client-closed-request frame (DeadlineExceeded routes to
	// ErrContextCanceled via mapError).
	if s.maxStreamDur > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.maxStreamDur)
		defer cancel()
	}

	// Copy-on-prepend: avoid mutating the caller's slice in case the
	// request is retried (e.g., a 401 → refresh → replay in a future
	// client). +1 because we're prepending the system message.
	msgs := make([]llm.Message, 0, len(req.Messages)+1)
	msgs = append(msgs, llm.Message{Role: llmRoleSystem, Content: s.systemPrompt})
	for _, m := range req.Messages {
		msgs = append(msgs, llm.Message{Role: m.Role, Content: m.Content})
	}

	// RAG injection: when req.RAG is set, retrieve the user's
	// top-K feeds and inject the context as a NEW user message
	// immediately after the last user turn. Best-effort — a retrieval
	// failure logs + continues without context; the user's chat
	// never fails because RAG is sick.
	//
	// Why a new message (rather than prepending to the last user
	// turn): keeps the wire-visible Messages consistent with what
	// the validator just saw (no splice-vs-validate confusion), and
	// matches the standard "document Q&A" prompt shape OpenAI's
	// cookbook recommends. The lazy backfill is fire-and-forget;
	// see rag.Service.BackfillOnce for the once-per-user guard.
	if req.RAG && s.rag != nil {
		userID := reqctx.UserIDFromContext(ctx)
		if userID != 0 {
			lastUser := lastUserContent(req.Messages)
			if lastUser != "" {
				_, ctxBlock, rerr := s.rag.Retrieve(ctx, userID, lastUser)
				if rerr != nil {
					span.RecordError(rerr)
					// Continue without context — never fail the chat.
				} else if ctxBlock != "" {
					msgs = injectContext(msgs, ctxBlock, lastUser)
				}
			}
			// Detached ctx so the backfill survives the chat request
			// returning. The lazy backfill is bounded; long backfills
			// are not a memory leak.
			bgCtx := detachContext(ctx)
			go s.rag.BackfillOnce(bgCtx, userID)
		}
	}

	usage, err := s.provider.Chat(ctx, llm.ChatRequest{
		Model:     "", // model is configured at the provider level (openai.NewClient reads cfg.LLMModel)
		Messages:  msgs,
		MaxTokens: s.maxTokens,
		OnNote:    onNote,
	}, onDelta)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return usage, err
	}

	if usage != nil {
		span.SetAttributes(
			attribute.Int("llm.prompt_tokens", usage.PromptTokens),
			attribute.Int("llm.completion_tokens", usage.CompletionTokens),
			attribute.Int("llm.total_tokens", usage.TotalTokens),
		)
	}
	span.SetStatus(codes.Ok, "")
	return usage, nil
}

// Validate exposes the per-request rules for callers that need to
// reject invalid input before committing to a side-effecting
// operation (the SSE handler uses this to write a 400 envelope
// before any SSE bytes hit the wire). Returns llm.ErrInvalidInput
// so MapError routes 400 correctly; the wrapping error message is
// safe to surface to the client.
func (s *Service) Validate(req *ChatRequest) error {
	return s.validate(req)
}

// validate enforces the per-request rules. Returning llm.ErrInvalidInput
// keeps the failure-mode contract uniform with the provider's
// sentinels — MapError walks the chain and routes to 400.
func (s *Service) validate(req *ChatRequest) error {
	if len(req.Messages) == 0 {
		return fmt.Errorf("messages is required: %w", llm.ErrInvalidInput)
	}
	if len(req.Messages) > s.maxHistory {
		return fmt.Errorf("messages exceeds max history (%d): %w", s.maxHistory, llm.ErrInvalidInput)
	}
	for i, m := range req.Messages {
		switch m.Role {
		case RoleUser, RoleAssistant:
			// ok
		case RoleSystem:
			return fmt.Errorf("messages[%d].role must not be %q: %w", i, RoleSystem, llm.ErrInvalidInput)
		default:
			return fmt.Errorf("messages[%d].role %q is invalid (must be %q or %q): %w",
				i, m.Role, RoleUser, RoleAssistant, llm.ErrInvalidInput)
		}
		if len(m.Content) > s.maxMessageChars {
			return fmt.Errorf("messages[%d].content exceeds max length (%d): %w",
				i, s.maxMessageChars, llm.ErrInvalidInput)
		}
	}
	return nil
}

// llmRoleSystem is the role string the llm package expects on the
// system message we prepend. Keeping it local means the llm package
// stays unaware of chat.Role* — both packages agree on the wire
// literal "system" without an import cycle.
const (
	llmRoleSystem = "system"
	llmRoleUser   = "user"
)

// lastUserContent returns the content of the LAST user-role message
// in the slice, or "" if there are none. The retrieval query is
// always the most recent user turn — embedding the entire history
// would dilute the signal and bloat the request.
func lastUserContent(msgs []Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == RoleUser {
			return msgs[i].Content
		}
	}
	return ""
}

// injectContext inserts a user-role "use this context to answer"
// message immediately after the last user message in msgs. msgs
// already has the system prompt at index 0 (see Chat), so the
// search starts at index 1.
//
// The injected content restates the question at the end so the
// model sees both the context AND the explicit question in the
// same turn — closer to the OpenAI cookbook's document-Q&A
// pattern than appending context to the user's question
// (which would change the user's wire-visible content).
func injectContext(msgs []llm.Message, contextBlock, lastUser string) []llm.Message {
	injection := llm.Message{
		Role: llmRoleUser,
		Content: fmt.Sprintf(
			"Use the following context from my feeds to answer my question. If the context is irrelevant, ignore it and answer from your general knowledge.\n\nContext:\n%s\n\nMy question: %s",
			contextBlock,
			lastUser,
		),
	}
	lastIdx := -1
	for i := len(msgs) - 1; i >= 1; i-- {
		if msgs[i].Role == llmRoleUser {
			lastIdx = i
			break
		}
	}
	if lastIdx == -1 {
		// No user message after the system prompt — just append.
		return append(msgs, injection)
	}
	out := make([]llm.Message, 0, len(msgs)+1)
	out = append(out, msgs[:lastIdx+1]...)
	out = append(out, injection)
	out = append(out, msgs[lastIdx+1:]...)
	return out
}

// ragContextTimeout caps the detached context for the lazy
// backfill. Without this, a never-completing embed call would
// leak the goroutine forever. The ceiling is generous — the
// backfill is bounded to ~fewer-feeds than this should ever
// process per call.
const ragContextTimeout = 30 * time.Second

// detachContext returns a context that survives the caller's
// cancellation. The background context + a fresh timeout keeps
// the lazy backfill running long enough to make progress without
// leaking forever. A future per-batch progress emitter could
// shorten this; for now the goroutine ends on completion or ctx
// cancellation, whichever comes first.
//
// We intentionally do NOT inherit the caller's request ID, span
// context, or auth — the backfill runs on behalf of the user but
// is not part of the chat request's trace tree (it would confuse
// trace correlation when the chat span ends before the backfill).
func detachContext(_ context.Context) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), ragContextTimeout)
	// Detached goroutine: cancel at function return so a future
	// caller that forgets to defer doesn't leak the timer.
	// (Today's only caller is the inline `go s.rag.BackfillOnce(...)`
	// — cancel is effectively a no-op since the timer auto-fires
	// after ragContextTimeout regardless.)
	cancel()
	return ctx
}
