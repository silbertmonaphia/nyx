// Package openai implements llm.Provider against the OpenAI Chat
// Completions streaming API via github.com/sashabaranov/go-openai.
// The same client works against any OpenAI-compatible server (vLLM,
// llama.cpp's Python server, etc.) because the wire contract is
// identical — base URL is the only difference. Operators who want
// the vLLM-specific path (per-dial DNS hardening, optional empty
// API key) should use internal/llm/vllm.Client instead by setting
// LLM_PROVIDER=vllm in the environment.
//
// Streaming protocol: ChatCompletionRequest.Stream=true and
// StreamOptions.IncludeUsage=true produce an SSE stream whose final
// chunk carries the usage summary (PromptTokens, CompletionTokens,
// TotalTokens) and whose last chunk ends with stream.Recv() == io.EOF.
// We map that into the chat handler's onDelta callback: every
// non-empty Delta.Content is forwarded as a delta, and the trailing
// usage chunk is returned once via *ChatUsage.
//
// Tool calls: the SDK already surfaces tool-call deltas on each
// chunk's choice[].delta.tool_calls. Chat (legacy callback) drops
// them — the legacy signature cannot represent them, and chat's
// RAG-only path doesn't need them. ChatDelta surfaces every frame
// through the typed Delta callback so the executor can reassemble
// parallel calls by ID.
//
// Error mapping: provider failures bubble up as llm sentinels so the
// api.MapError funnel can render the right HTTP status without any
// per-call switch. 429 → ErrRateLimited, 5xx → ErrProviderUnavailable,
// network/DNS → ErrProviderUnavailable, ctx cancel → ErrContextCanceled.
// We never compare on err.Error() — every branch goes through
// errors.As on a typed error.
package openai

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"

	openai "github.com/sashabaranov/go-openai"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"nyx/internal/llm"
	"nyx/internal/llm/ssrf"
	"nyx/internal/platform/config"
)

// Client implements llm.Provider (and llm.DeltaProvider) against an
// OpenAI-compatible Chat Completions server. Constructed once at
// process startup and reused across requests — the underlying
// *openai.Client is goroutine-safe.
type Client struct {
	http   *openai.Client
	model  string
	tracer trace.Tracer
}

// NewClient builds an OpenAI-compatible client from the resolved
// config. Returns an error when LLM is not enabled — callers in
// cmd/api/main.go guard on cfg.LLMEnabled before constructing.
func NewClient(cfg *config.Config, tracer trace.Tracer) (*Client, error) {
	if !cfg.LLMEnabled {
		return nil, fmt.Errorf("openai.NewClient: LLM_ENABLED=false")
	}
	if cfg.LLMBaseURL == "" || cfg.LLMAPIKey == "" || cfg.LLMModel == "" {
		return nil, fmt.Errorf("openai.NewClient: LLM_BASE_URL, LLM_API_KEY, and LLM_MODEL must be set when LLM_ENABLED=true")
	}

	oc := openai.DefaultConfig(cfg.LLMAPIKey)
	// WithBaseURL is the single seam that lets the same client hit
	// OpenAI's hosted endpoint today and a self-hosted vLLM endpoint
	// tomorrow. The SDK adds the /chat/completions suffix internally.
	oc.BaseURL = cfg.LLMBaseURL

	if !cfg.LLMAllowPrivateURL {
		parsed, err := url.Parse(cfg.LLMBaseURL)
		if err != nil {
			return nil, fmt.Errorf("openai.NewClient: invalid LLM_BASE_URL: %w", err)
		}
		httpClient, err := ssrf.NewHTTPClient(parsed, 0, false, "LLM_BASE_URL")
		if err != nil {
			if errors.Is(err, ssrf.ErrPrivateAddress) {
				return nil, fmt.Errorf("openai.NewClient: %w; set LLM_ALLOW_PRIVATE_URL=true to override", err)
			}
			return nil, fmt.Errorf("openai.NewClient: %w", err)
		}
		oc.HTTPClient = httpClient
	}

	return &Client{
		http:   openai.NewClientWithConfig(oc),
		model:  cfg.LLMModel,
		tracer: tracer,
	}, nil
}

// buildStreamRequest translates an llm.ChatRequest into the SDK's
// ChatCompletionRequest. Zero tools / nil tool choice are dropped so
// the wire body is byte-identical to the pre-change shape — no
// extra frames, no extra keys.
func buildStreamRequest(model string, req llm.ChatRequest) openai.ChatCompletionRequest {
	msgs := make([]openai.ChatCompletionMessage, len(req.Messages))
	for i, m := range req.Messages {
		entry := openai.ChatCompletionMessage{
			Role:       m.Role,
			Content:    m.Content,
			Name:       m.Name,
			ToolCallID: m.ToolCallID,
		}
		if len(m.ToolCalls) > 0 {
			tcs := make([]openai.ToolCall, len(m.ToolCalls))
			for j, tc := range m.ToolCalls {
				tcs[j] = openai.ToolCall{
					ID:   tc.ID,
					Type: openai.ToolTypeFunction,
					Function: openai.FunctionCall{
						Name:      tc.Name,
						Arguments: tc.Arguments,
					},
				}
			}
			entry.ToolCalls = tcs
		}
		msgs[i] = entry
	}

	streamReq := openai.ChatCompletionRequest{
		Model:    model,
		Messages: msgs,
		Stream:   true,
		// IncludeUsage asks OpenAI to emit a trailing chunk with the
		// usage summary; vLLM honours it. Without this flag, the SDK
		// never sees Usage on any chunk and *ChatUsage comes back nil.
		StreamOptions:       &openai.StreamOptions{IncludeUsage: true},
		MaxCompletionTokens: req.MaxTokens,
	}
	if req.Temperature != nil {
		streamReq.Temperature = *req.Temperature
	}
	if len(req.Tools) > 0 {
		tools := make([]openai.Tool, len(req.Tools))
		for i, t := range req.Tools {
			fn := openai.FunctionDefinition{
				Name:        t.Function.Name,
				Description: t.Function.Description,
				Parameters:  t.Function.Parameters,
			}
			tools[i] = openai.Tool{
				Type:     openai.ToolTypeFunction,
				Function: &fn,
			}
		}
		streamReq.Tools = tools
	}
	if req.ToolChoice != nil {
		streamReq.ToolChoice = req.ToolChoice
	}
	return streamReq
}

// Chat streams one completion. Each non-empty Delta.Content from the
// upstream is forwarded to onDelta in arrival order. The trailing
// usage chunk (when the upstream emits one) triggers a final
// onDelta call with an empty delta and the populated *ChatUsage —
// that's the signal to the handler to write the SSE terminal frame.
// A non-nil error from onDelta aborts the read and is returned to
// the caller unchanged — the chat handler uses this to surface
// client disconnects mid-stream.
//
// Tool-call deltas are dropped on this path — the legacy callback
// cannot represent them, and today's chat service never produces
// requests with Tools set. Use ChatDelta for tool-aware consumers.
//
// Errors from the upstream are mapped to llm sentinels: 429 →
// ErrRateLimited, 5xx → ErrProviderUnavailable, network/DNS →
// ErrProviderUnavailable, context cancellation → ErrContextCanceled.
// The mapping happens via errors.As on the SDK's typed errors
// (openai.APIError, openai.RequestError) — never on err.Error()
// strings, so a future SDK string change can't silently break
// routing.
func (c *Client) Chat(ctx context.Context, req llm.ChatRequest, onDelta func(delta string, finalUsage *llm.ChatUsage) error) (*llm.ChatUsage, error) {
	ctx, span := c.tracer.Start(ctx, "llm.openai.chat",
		trace.WithAttributes(
			attribute.String("llm.model", c.model),
			attribute.Int("llm.messages", len(req.Messages)),
			attribute.Int("llm.max_tokens", req.MaxTokens),
		),
	)
	defer span.End()

	streamReq := buildStreamRequest(c.model, req)

	stream, err := c.http.CreateChatCompletionStream(ctx, streamReq)
	if err != nil {
		span.RecordError(err)
		return nil, c.mapError(err)
	}
	// Close releases the underlying HTTP response body. Safe to call
	// after Recv has already returned io.EOF — the SDK no-ops in that
	// case.
	defer func() { _ = stream.Close() }()

	var usage *llm.ChatUsage
	for {
		// Honour cancellation between chunks. The SDK's Recv also
		// surfaces ctx cancellation, but the explicit check short-
		// circuits the next request byte read when the handler has
		// already noticed the client disconnect.
		if err := ctx.Err(); err != nil {
			span.RecordError(err)
			return usage, llm.ErrContextCanceled
		}

		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			span.RecordError(err)
			return usage, c.mapError(err)
		}

		// Usage arrives on a separate chunk with empty Choices when
		// IncludeUsage is set. We forward it to the caller as a
		// final callback (empty delta, populated usage) so handlers
		// can emit their terminal SSE frame. The chunk itself is
		// not a delta.
		if resp.Usage != nil {
			usage = &llm.ChatUsage{
				PromptTokens:     resp.Usage.PromptTokens,
				CompletionTokens: resp.Usage.CompletionTokens,
				TotalTokens:      resp.Usage.TotalTokens,
			}
			span.SetAttributes(
				attribute.Int("llm.prompt_tokens", usage.PromptTokens),
				attribute.Int("llm.completion_tokens", usage.CompletionTokens),
				attribute.Int("llm.total_tokens", usage.TotalTokens),
			)
			if err := onDelta("", usage); err != nil {
				// Caller-side error on the final callback — same
				// semantics as a mid-stream disconnect.
				span.RecordError(err)
				return usage, err
			}
			continue
		}

		for _, choice := range resp.Choices {
			// OpenAI sends a zero-length Delta.Content on role-change
			// frames (the very first chunk sets role="assistant" with
			// no content). Skip those — they would otherwise produce
			// empty SSE events. Tool-call deltas are dropped on this
			// path; ChatDelta is the typed-callback entry point.
			if choice.Delta.Content == "" {
				continue
			}
			if err := onDelta(choice.Delta.Content, nil); err != nil {
				// Caller-side error (typically: client disconnected,
				// http.Flusher returned an error). Surface the error
				// to the caller without mapping it to a sentinel —
				// it's not an LLM problem.
				span.RecordError(err)
				return usage, err
			}
		}
	}

	span.SetStatus(codes.Ok, "")
	return usage, nil
}

// ChatDelta is the typed-callback streaming entry point. It surfaces
// every frame OpenAI emits — content deltas, tool-call deltas
// (partial fields preserved so parallel calls can be reassembled by
// ID), the trailing usage chunk, and provider notes — through a
// single discriminated Delta value.
//
// Frame ordering matches the upstream emission order so the executor
// can pair a DeltaToolCall with the following DeltaContent frame that
// resumes the turn, and so the final DeltaUsage is always the last
// frame before EOF.
//
// Errors from onDelta abort the stream and propagate to the caller
// unchanged (same contract as Chat). Upstream errors map to llm
// sentinels via mapError.
func (c *Client) ChatDelta(ctx context.Context, req llm.ChatRequest, onDelta func(llm.Delta) error) (*llm.ChatUsage, error) {
	ctx, span := c.tracer.Start(ctx, "llm.openai.chat_delta",
		trace.WithAttributes(
			attribute.String("llm.model", c.model),
			attribute.Int("llm.messages", len(req.Messages)),
			attribute.Int("llm.max_tokens", req.MaxTokens),
			attribute.Int("llm.tools", len(req.Tools)),
		),
	)
	defer span.End()

	streamReq := buildStreamRequest(c.model, req)

	stream, err := c.http.CreateChatCompletionStream(ctx, streamReq)
	if err != nil {
		span.RecordError(err)
		return nil, c.mapError(err)
	}
	defer func() { _ = stream.Close() }()

	var usage *llm.ChatUsage
	for {
		if err := ctx.Err(); err != nil {
			span.RecordError(err)
			return usage, llm.ErrContextCanceled
		}

		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			span.RecordError(err)
			return usage, c.mapError(err)
		}

		if resp.Usage != nil {
			usage = &llm.ChatUsage{
				PromptTokens:     resp.Usage.PromptTokens,
				CompletionTokens: resp.Usage.CompletionTokens,
				TotalTokens:      resp.Usage.TotalTokens,
			}
			span.SetAttributes(
				attribute.Int("llm.prompt_tokens", usage.PromptTokens),
				attribute.Int("llm.completion_tokens", usage.CompletionTokens),
				attribute.Int("llm.total_tokens", usage.TotalTokens),
			)
			if err := onDelta(llm.Delta{Kind: llm.DeltaUsage, Usage: usage}); err != nil {
				span.RecordError(err)
				return usage, err
			}
			continue
		}

		for _, choice := range resp.Choices {
			delta := choice.Delta

			// Tool-call deltas may arrive on the same frame as a
			// zero-length content slice. The SDK models them as a
			// slice so the executor can keep parallel calls ordered
			// by Index; we forward each one as its own DeltaToolCall
			// frame so the caller sees them in emission order.
			for _, tc := range delta.ToolCalls {
				idx := 0
				if tc.Index != nil {
					idx = *tc.Index
				}
				if err := onDelta(llm.Delta{
					Kind: llm.DeltaToolCall,
					ToolCall: &llm.ToolCall{
						ID:        tc.ID,
						Name:      tc.Function.Name,
						Arguments: tc.Function.Arguments,
						Index:     idx,
					},
				}); err != nil {
					span.RecordError(err)
					return usage, err
				}
			}

			// Role-change / empty-content frames are skipped — they
			// carry no information for a typed-callback consumer
			// either.
			if delta.Content == "" {
				continue
			}
			if err := onDelta(llm.Delta{Kind: llm.DeltaContent, Content: delta.Content}); err != nil {
				span.RecordError(err)
				return usage, err
			}
		}
	}

	span.SetStatus(codes.Ok, "")
	return usage, nil
}

// mapError turns an SDK error into an llm sentinel. The errors.As
// walks the chain, so wrapped errors (fmt.Errorf("...: %w", ...))
// route correctly. Unknown error types collapse to
// ErrProviderUnavailable — we can't tell from a bare error whether
// the upstream is broken or we are, and ErrProviderUnavailable is
// the safe default that won't trigger a client retry loop.
func (c *Client) mapError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return llm.ErrContextCanceled
	}

	// Network / DNS / connection refused. *openai.RequestError is the
	// SDK's typed wrapper around *http.Request errors.
	var reqErr *openai.RequestError
	if errors.As(err, &reqErr) {
		return fmt.Errorf("upstream unreachable: %w", llm.ErrProviderUnavailable)
	}

	// HTTP status from the upstream. *openai.APIError carries the
	// status code alongside the body; 429 is the only status we map
	// distinctly (ErrRateLimited) — every other status is treated as
	// provider-side since the client cannot recover from an
	// upstream-side schema rejection.
	var apiErr *openai.APIError
	if errors.As(err, &apiErr) {
		switch {
		case apiErr.HTTPStatusCode == 429:
			return fmt.Errorf("upstream 429: %w", llm.ErrRateLimited)
		default:
			return fmt.Errorf("upstream %d: %w", apiErr.HTTPStatusCode, llm.ErrProviderUnavailable)
		}
	}

	return fmt.Errorf("llm chat failed: %w", llm.ErrProviderUnavailable)
}

// Embed batches len(req.Inputs) strings into a single
// /v1/embeddings call and returns one []float32 per input, in the
// same order. The OpenAI SDK and any OpenAI-compatible server
// (including vLLM, since the SDK ignores auth headers when the
// upstream accepts anonymous access) honour the array shape
// natively, so one HTTP round-trip serves N inputs.
//
// The model is taken from req.Model, not from c.model — the chat
// model and the embedding model can differ (e.g. chat on gpt-4o,
// embeddings on text-embedding-3-small). cmd/api/main.go is
// responsible for resolving the embedding model from
// cfg.LLMEmbeddingModel (with cfg.LLMModel as the fallback) at
// wiring time and passing it on every Embed call.
//
// Error mapping reuses c.mapError — the SDK's typed errors
// (openai.APIError, openai.RequestError) are identical for the
// /v1/embeddings endpoint.
func (c *Client) Embed(ctx context.Context, req llm.EmbedRequest) ([][]float32, error) {
	ctx, span := c.tracer.Start(ctx, "llm.openai.embed",
		trace.WithAttributes(
			attribute.String("llm.model", req.Model),
			attribute.Int("llm.inputs", len(req.Inputs)),
		),
	)
	defer span.End()

	if len(req.Inputs) == 0 {
		return nil, nil
	}

	resp, err := c.http.CreateEmbeddings(ctx, openai.EmbeddingRequest{
		Model: openai.EmbeddingModel(req.Model),
		Input: req.Inputs,
	})
	if err != nil {
		span.RecordError(err)
		return nil, c.mapError(err)
	}

	out := make([][]float32, len(req.Inputs))
	for _, d := range resp.Data {
		// d.Index tells us which input the embedding corresponds to
		// (preserves order even when the server batches). The bounds
		// check guards against a malformed upstream that returns
		// fewer rows than requested — we surface a sentinel error
		// rather than a confusing nil-deref panic downstream.
		if d.Index < 0 || d.Index >= len(out) {
			return nil, fmt.Errorf("openai embed: index %d out of range [0,%d): %w", d.Index, len(out), llm.ErrProviderUnavailable)
		}
		out[d.Index] = d.Embedding
	}
	span.SetStatus(codes.Ok, "")
	return out, nil
}
