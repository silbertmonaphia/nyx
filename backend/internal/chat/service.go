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
)

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
type Service struct {
	provider        llm.Provider
	systemPrompt    string
	maxHistory      int
	maxMessageChars int
	maxTokens       int
	maxStreamDur    time.Duration
	tracer          trace.Tracer
}

// NewService builds a Service from config. cfg supplies the per-
// request caps and the system prompt (or the hard-coded default
// when empty). tracer is the OTel tracer; on OTEL_ENABLED=false
// it's a noop and every span is free.
func NewService(p llm.Provider, cfg *config.Config, tracer trace.Tracer) *Service {
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
		tracer:          tracer,
	}
}

// Chat validates req, builds an llm.ChatRequest, and forwards it to
// the provider. onDelta is invoked once per non-empty content chunk
// in arrival order; the trailing usage chunk (when emitted) is
// forwarded with a populated finalUsage and an empty delta — the
// signal to handlers that the stream is ending.
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
func (s *Service) Chat(ctx context.Context, req ChatRequest, onDelta func(delta string, finalUsage *llm.ChatUsage) error) (*llm.ChatUsage, error) {
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

	usage, err := s.provider.Chat(ctx, llm.ChatRequest{
		Model:     "", // model is configured at the provider level (openai.NewClient reads cfg.LLMModel)
		Messages:  msgs,
		MaxTokens: s.maxTokens,
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
const llmRoleSystem = "system"
