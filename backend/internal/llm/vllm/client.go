// Package vllm implements llm.Provider against a self-hosted vLLM
// OpenAI-compatible chat-completions server. Unlike internal/llm/openai,
// which targets OpenAI's hosted service via sashabaranov/go-openai, this
// package talks raw HTTP so vLLM-specific quirks (slightly different
// error JSON, optional tool-call deltas) don't have to be filtered
// through an SDK whose error types are designed for OpenAI proper.
//
// The wire contract is identical to OpenAI's: POST /chat/completions
// with {model, messages, stream:true, stream_options.include_usage:true,
// max_tokens, tools?, tool_choice?} returns text/event-stream frames of
// shape
//
//	data: {"choices":[{"delta":{"content":"...","tool_calls":[...]}}]}\n\n
//
// followed by a trailing usage frame
//
//	data: {"usage":{"prompt_tokens":N,"completion_tokens":N,"total_tokens":N}}\n\n
//
// and terminated by `data: [DONE]\n\n`. vLLM (>= 0.4) honours
// stream_options.include_usage; older versions return no usage chunk
// and the returned *ChatUsage is nil (the chat handler tolerates that —
// see internal/chat/handler.go usageToMap).
//
// Tool calls: Chat (legacy callback) drops tool-call deltas — the
// legacy `func(string, *ChatUsage) error` signature cannot represent
// them. ChatDelta surfaces every frame through the typed Delta
// callback so the executor can reassemble parallel calls by ID. The
// SSE parser is shared between both paths; only the callback
// behaviour differs.
//
// Error mapping mirrors openai/client.go's contract so the api.MapError
// funnel renders the right HTTP status without any per-call switch:
// 429 → ErrRateLimited, 5xx + non-429 4xx → ErrProviderUnavailable,
// network/DNS → ErrProviderUnavailable, ctx cancel → ErrContextCanceled.
// We compare on typed errors (errors.As on *url.Error / HTTP status),
// never on err.Error() — a future vLLM JSON change can't silently
// break routing.
//
// Safety: the http.Client built here performs a per-dial IP-class
// allowlist (refuses loopback / RFC1918 / link-local unless
// LLM_ALLOW_PRIVATE_URL=true) to close the DNS-rebinding window that
// a one-shot startup LookupIP check would leave open. Operators
// running vLLM inside the dev compose use the escape hatch.
package vllm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"nyx/internal/llm/ssrf"

	"nyx/internal/llm"
	"nyx/internal/platform/config"
)

// Client implements llm.Provider (and llm.DeltaProvider) against a
// self-hosted vLLM OpenAI-compatible server. Constructed once at
// process startup and reused across requests — the underlying
// *http.Client is goroutine-safe.
type Client struct {
	baseURL string
	model   string
	apiKey  string // empty = omit Authorization header
	http    *http.Client
	tracer  trace.Tracer
}

// NewClient builds a vLLM provider from the resolved config. Returns
// an error when LLM is not enabled — callers in cmd/api/main.go
// guard on cfg.LLMEnabled before constructing. The constructor
// repeats the config-load checks so a future caller that bypasses
// config.Load (tests, ad-hoc tooling) still fails closed.
func NewClient(cfg *config.Config, tracer trace.Tracer) (*Client, error) {
	if !cfg.LLMEnabled {
		return nil, fmt.Errorf("vllm.NewClient: LLM_ENABLED=false")
	}
	if cfg.LLMBaseURL == "" {
		return nil, fmt.Errorf("vllm.NewClient: LLM_BASE_URL must be set when LLM_ENABLED=true")
	}
	if cfg.LLMModel == "" {
		return nil, fmt.Errorf("vllm.NewClient: LLM_MODEL must be set when LLM_ENABLED=true")
	}
	// LLM_API_KEY is intentionally optional — vLLM started without
	// --api-key accepts any (including empty) Authorization header.
	// The chat handler will omit the header in that case so the wire
	// matches what the operator expects.

	timeout, err := time.ParseDuration(cfg.LLMTimeout)
	if err != nil {
		return nil, fmt.Errorf("vllm.NewClient: invalid LLM_TIMEOUT: %w", err)
	}

	parsed, err := url.Parse(cfg.LLMBaseURL)
	if err != nil {
		return nil, fmt.Errorf("vllm.NewClient: invalid LLM_BASE_URL: %w", err)
	}
	httpClient, err := newHTTPClient(parsed, timeout, cfg.LLMAllowPrivateURL)
	if err != nil {
		return nil, fmt.Errorf("vllm.NewClient: %w", err)
	}

	return &Client{
		baseURL: strings.TrimRight(cfg.LLMBaseURL, "/"),
		model:   cfg.LLMModel,
		apiKey:  cfg.LLMAPIKey,
		http:    httpClient,
		tracer:  tracer,
	}, nil
}

// Chat streams one completion from vLLM. The wire contract is the
// OpenAI /chat/completions streaming contract — see the package doc
// for the frame shapes. onDelta is fired for every non-empty content
// fragment and once on the trailing usage chunk with an empty delta
// and a populated *ChatUsage. Tool-call deltas are dropped on this
// path; ChatDelta is the typed-callback entry point for tool-aware
// consumers.
//
// Errors funnel into llm sentinels via mapError so the api.MapError
// handler renders the right HTTP status without any per-call switch.
func (c *Client) Chat(ctx context.Context, req llm.ChatRequest, onDelta func(delta string, finalUsage *llm.ChatUsage) error) (*llm.ChatUsage, error) {
	ctx, span := c.tracer.Start(ctx, "llm.vllm.chat",
		trace.WithAttributes(
			attribute.String("llm.provider", "vllm"),
			attribute.String("llm.model", c.model),
			attribute.Int("llm.messages", len(req.Messages)),
			attribute.Int("llm.max_tokens", req.MaxTokens),
		),
	)
	defer span.End()

	body, err := buildRequest(c.model, req)
	if err != nil {
		span.RecordError(err)
		return nil, llm.ErrInvalidInput
	}
	payload, err := json.Marshal(body)
	if err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("vllm: marshal request: %w", llm.ErrInvalidInput)
	}

	resp, err := c.doStream(ctx, payload)
	if err != nil {
		span.RecordError(err)
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	return c.consumeSSE(ctx, span, resp.Body, onDelta)
}

// ChatDelta is the typed-callback streaming entry point. It surfaces
// every frame vLLM emits — content deltas, tool-call deltas (partial
// fields preserved so parallel calls can be reassembled by ID), the
// trailing usage chunk, and provider notes — through a single
// discriminated Delta value.
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
	ctx, span := c.tracer.Start(ctx, "llm.vllm.chat_delta",
		trace.WithAttributes(
			attribute.String("llm.provider", "vllm"),
			attribute.String("llm.model", c.model),
			attribute.Int("llm.messages", len(req.Messages)),
			attribute.Int("llm.max_tokens", req.MaxTokens),
			attribute.Int("llm.tools", len(req.Tools)),
		),
	)
	defer span.End()

	body, err := buildRequest(c.model, req)
	if err != nil {
		span.RecordError(err)
		return nil, llm.ErrInvalidInput
	}
	payload, err := json.Marshal(body)
	if err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("vllm: marshal request: %w", llm.ErrInvalidInput)
	}

	resp, err := c.doStream(ctx, payload)
	if err != nil {
		span.RecordError(err)
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	return c.consumeSSEDelta(ctx, span, resp.Body, onDelta)
}

// doStream issues the POST and returns the response, mapping any
// transport / status error through mapError. Shared by Chat and
// ChatDelta so the SSE plumbing is in one place.
func (c *Client) doStream(ctx context.Context, payload []byte) (*http.Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return nil, c.mapError(err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	// Empty apiKey intentionally omits the header so vLLM started
	// without --api-key accepts the call. Sending `Authorization:
	// Bearer ` (empty token) would return 401.
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, c.mapError(err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Drain the body so the connection can be reused; surface the
		// status as a typed marker so mapError can route it without
		// string-matching err.Error().
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		statusErr := &upstreamStatusError{status: resp.StatusCode}
		return nil, c.mapError(statusErr)
	}
	return resp, nil
}

// newSSEScanner builds a bufio.Scanner with the project's buffer
// ceiling (1 MiB) so a long single-token completion isn't silently
// truncated. Both consumeSSE (legacy) and consumeSSEDelta (typed)
// share this builder.
func newSSEScanner(body io.Reader) *bufio.Scanner {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	return scanner
}

// consumeSSE decodes one SSE stream into a sequence of onDelta calls.
// Returns the trailing *ChatUsage (nil if vLLM didn't emit one) and
// the first non-recoverable error.
//
// Tool-call deltas are decoded from sseFrame but dropped before the
// callback is fired — Chat's legacy signature cannot represent them.
// ChatDelta uses consumeSSEDelta for the full typed stream.
//
// Error semantics: a malformed JSON frame collapses to
// ErrProviderUnavailable (same as the pre-change behaviour). A
// cancelled context returns ErrContextCanceled so the handler's
// client-disconnect detection treats it uniformly.
func (c *Client) consumeSSE(ctx context.Context, span trace.Span, body io.Reader, onDelta func(delta string, finalUsage *llm.ChatUsage) error) (*llm.ChatUsage, error) {
	scanner := newSSEScanner(body)

	var usage *llm.ChatUsage
	for scanner.Scan() {
		// Cancellation check between lines — cheaper than waiting for
		// the next read on the body.
		if err := ctx.Err(); err != nil {
			span.RecordError(err)
			return usage, llm.ErrContextCanceled
		}

		frame, err := c.decodeLine(scanner.Text())
		if err != nil {
			span.RecordError(err)
			return usage, c.mapError(fmt.Errorf("vllm: decode frame: %w", err))
		}
		if frame == nil {
			continue
		}

		if frame.Usage != nil {
			usage = &llm.ChatUsage{
				PromptTokens:     frame.Usage.PromptTokens,
				CompletionTokens: frame.Usage.CompletionTokens,
				TotalTokens:      frame.Usage.TotalTokens,
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

		for _, choice := range frame.Choices {
			// Tool-call frames carry no content; on the legacy path
			// we drop them silently. ChatDelta surfaces them.
			if choice.Delta.ToolCalls != nil {
				continue
			}
			if choice.Delta.Content == "" {
				// Role-change frame (OpenAI/vLLM emit the first
				// chunk with role="assistant" and empty content).
				// Skip — forwarding an empty SSE event would render
				// as a blank UI bubble.
				continue
			}
			if err := onDelta(choice.Delta.Content, nil); err != nil {
				span.RecordError(err)
				return usage, err
			}
		}
	}

	if err := ctx.Err(); err != nil {
		span.RecordError(err)
		return usage, llm.ErrContextCanceled
	}
	if err := scanner.Err(); err != nil {
		span.RecordError(err)
		return usage, c.mapError(err)
	}

	span.SetStatus(codes.Ok, "")
	return usage, nil
}

// consumeSSEDelta is the typed-callback counterpart of consumeSSE. It
// surfaces content deltas, tool-call deltas (full or partial), the
// terminal usage frame, and provider notes through a single
// discriminated Delta value. Tool-call deltas keep their partial
// fields (ID empty, Name empty, Arguments partial) so the executor
// can reassemble parallel calls by ID across frames.
//
// Same error semantics as consumeSSE: malformed JSON →
// ErrProviderUnavailable, ctx cancellation → ErrContextCanceled.
func (c *Client) consumeSSEDelta(ctx context.Context, span trace.Span, body io.Reader, onDelta func(llm.Delta) error) (*llm.ChatUsage, error) {
	scanner := newSSEScanner(body)

	var usage *llm.ChatUsage
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			span.RecordError(err)
			return usage, llm.ErrContextCanceled
		}

		frame, err := c.decodeLine(scanner.Text())
		if err != nil {
			span.RecordError(err)
			return usage, c.mapError(fmt.Errorf("vllm: decode frame: %w", err))
		}
		if frame == nil {
			continue
		}

		if frame.Usage != nil {
			usage = &llm.ChatUsage{
				PromptTokens:     frame.Usage.PromptTokens,
				CompletionTokens: frame.Usage.CompletionTokens,
				TotalTokens:      frame.Usage.TotalTokens,
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

		for _, choice := range frame.Choices {
			for _, tc := range choice.Delta.ToolCalls {
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
			if choice.Delta.Content == "" {
				// Role-change frame or tool-call-only frame with
				// no content — skip; no information to surface.
				continue
			}
			if err := onDelta(llm.Delta{Kind: llm.DeltaContent, Content: choice.Delta.Content}); err != nil {
				span.RecordError(err)
				return usage, err
			}
		}
	}

	if err := ctx.Err(); err != nil {
		span.RecordError(err)
		return usage, llm.ErrContextCanceled
	}
	if err := scanner.Err(); err != nil {
		span.RecordError(err)
		return usage, c.mapError(err)
	}

	span.SetStatus(codes.Ok, "")
	return usage, nil
}

// decodeLine parses one SSE line into an *sseFrame. Returns a nil
// frame for blank lines, SSE comments, non-data fields, and the
// [DONE] terminator — all of which are ignored. Returns an error
// only for unparseable JSON payloads; the caller surfaces that as
// ErrProviderUnavailable.
func (c *Client) decodeLine(line string) (*sseFrame, error) {
	if line == "" {
		return nil, nil
	}
	if strings.HasPrefix(line, ":") {
		return nil, nil
	}
	if !strings.HasPrefix(line, "data:") {
		return nil, nil
	}
	payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if payload == "[DONE]" {
		return nil, nil
	}

	frame, err := decodeFrame(payload)
	if err != nil {
		return nil, err
	}
	return &frame, nil
}

// mapError turns a raw error into an llm sentinel. We can't reuse
// openai.Client's mapError because it inspects SDK-typed errors
// (*openai.APIError, *openai.RequestError) that vLLM never
// produces — vLLM's error JSON is similar but not identical.
//
// The error chain we walk:
//   - context.Canceled / DeadlineExceeded → ErrContextCanceled
//   - *url.Error wrapping a dial failure (DNS, connection refused,
//     private-IP refused) → ErrProviderUnavailable
//   - any error carrying the synthetic `vllm: upstream status N`
//     marker produced by Chat → 429 → ErrRateLimited,
//     anything else → ErrProviderUnavailable
//   - bare / unknown errors → ErrProviderUnavailable (safe default)
func (c *Client) mapError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return llm.ErrContextCanceled
	}

	// Our own upstream-status marker carries the HTTP status code.
	var statusErr *upstreamStatusError
	if errors.As(err, &statusErr) {
		switch statusErr.status {
		case http.StatusTooManyRequests:
			return fmt.Errorf("upstream 429: %w", llm.ErrRateLimited)
		default:
			return fmt.Errorf("upstream %d: %w", statusErr.status, llm.ErrProviderUnavailable)
		}
	}

	// Network / DNS / connection refused / refused-by-DialContext.
	// *url.Error is what Go's http transport wraps dial failures in.
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return fmt.Errorf("upstream unreachable: %w", llm.ErrProviderUnavailable)
	}
	// Belt-and-braces for callers that unwrap url.Error.
	var netErr net.Error
	if errors.As(err, &netErr) {
		return fmt.Errorf("upstream unreachable: %w", llm.ErrProviderUnavailable)
	}

	return fmt.Errorf("llm chat failed: %w", llm.ErrProviderUnavailable)
}

// upstreamStatusError is a typed marker so mapError can route HTTP
// status codes without string-matching err.Error(). Built once per
// non-2xx response in Chat.
type upstreamStatusError struct {
	status int
}

func (e *upstreamStatusError) Error() string {
	return fmt.Sprintf("vllm: upstream status %d", e.status)
}

// Embed batches len(req.Inputs) strings into a single POST
// {base}/v1/embeddings call and returns one []float32 per input,
// in input order. vLLM exposes an OpenAI-compatible /v1/embeddings
// endpoint, so the wire contract is identical to OpenAI's; the
// only reason this code lives in vllm/ rather than openai/ is the
// SSRF-hardened http.Client below.
//
// CRITICAL: this method REUSES c.http (built by newHTTPClient).
// That client runs the per-dial IP-class allowlist on every
// connection, so the /v1/embeddings path inherits the same
// protection as /chat/completions. Adding a new http.Client here
// would silently open an SSRF window — code review must catch
// that.
//
// The model is taken from req.Model, not from c.model — chat and
// embedding models can differ. cmd/api/main.go is responsible for
// resolving the embedding model (cfg.LLMEmbeddingModel, with
// cfg.LLMModel as the fallback) at wiring time and passing it on
// every Embed call.
func (c *Client) Embed(ctx context.Context, req llm.EmbedRequest) ([][]float32, error) {
	ctx, span := c.tracer.Start(ctx, "llm.vllm.embed",
		trace.WithAttributes(
			attribute.String("llm.provider", "vllm"),
			attribute.String("llm.model", req.Model),
			attribute.Int("llm.inputs", len(req.Inputs)),
		),
	)
	defer span.End()

	if len(req.Inputs) == 0 {
		return nil, nil
	}

	body := struct {
		Model string   `json:"model"`
		Input []string `json:"input"`
	}{
		Model: req.Model,
		Input: req.Inputs,
	}
	payload, err := json.Marshal(body)
	if err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("vllm: marshal embed request: %w", llm.ErrInvalidInput)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/embeddings", bytes.NewReader(payload))
	if err != nil {
		span.RecordError(err)
		return nil, c.mapError(err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	// Same empty-key handling as Chat — vLLM started without --api-key
	// accepts the call without an Authorization header. Sending
	// `Bearer ` (empty) would return 401.
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		span.RecordError(err)
		return nil, c.mapError(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, resp.Body)
		statusErr := &upstreamStatusError{status: resp.StatusCode}
		span.RecordError(statusErr)
		return nil, c.mapError(statusErr)
	}

	// vLLM follows the OpenAI EmbeddingResponse layout. We only need
	// data[].embedding; everything else (object, model, usage,
	// http_header) is ignored. Go's permissive decoder drops unknown
	// fields without error so a future vLLM response-field addition
	// doesn't break us.
	var embedResp struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&embedResp); err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("vllm: decode embed response: %w", llm.ErrProviderUnavailable)
	}

	if len(embedResp.Data) != len(req.Inputs) {
		// Malformed upstream: we asked for N, got M. Surface a sentinel
		// so the rag layer logs and continues without context — a
		// retrieval failure must never fail the chat.
		span.RecordError(fmt.Errorf("vllm embed: got %d embeddings, want %d", len(embedResp.Data), len(req.Inputs)))
		return nil, llm.ErrProviderUnavailable
	}

	// OpenAI preserves order via data[i].index; vLLM typically just
	// returns the embeddings in input order without an index field.
	// We accept the simpler vLLM shape (positional, indexed 0..N-1)
	// because we already asserted len(data) == len(req.Inputs).
	out := make([][]float32, len(req.Inputs))
	for i, d := range embedResp.Data {
		out[i] = d.Embedding
	}
	span.SetStatus(codes.Ok, "")
	return out, nil
}

// sseFrame is the subset of the OpenAI /vLLM chunk schema we read.
// Tool-call deltas are now surfaced; Chat's legacy callback drops
// them, ChatDelta surfaces them. Unknown fields (finish_reason,
// logprobs, refusal, …) are dropped silently by the permissive
// decoder so a future vLLM response-field addition never breaks us.
type sseFrame struct {
	Choices []struct {
		Delta struct {
			Content   string        `json:"content"`
			ToolCalls []sseToolCall `json:"tool_calls,omitempty"`
		} `json:"delta"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

// sseToolCall is the per-call subset of OpenAI's tool-call streaming
// shape. Index is a pointer because OpenAI only emits it on the first
// chunk of a parallel-call sequence; subsequent chunks omit it and
// we default to 0.
type sseToolCall struct {
	Index    *int   `json:"index,omitempty"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function"`
}

func decodeFrame(payload string) (sseFrame, error) {
	var f sseFrame
	if err := json.Unmarshal([]byte(payload), &f); err != nil {
		return f, err
	}
	return f, nil
}

// outboundRequest is the JSON body we POST to vLLM. Mirrors
// internal/llm/openai/client.go's openai.ChatCompletionRequest
// fields we use, minus the SDK dependency. Tools / ToolChoice are
// only serialised when non-empty so the wire body is byte-identical
// to the pre-change shape for zero-tools requests.
type outboundRequest struct {
	Model       string              `json:"model"`
	Messages    []outboundMessage   `json:"messages"`
	Stream      bool                `json:"stream"`
	Options     *outboundStreamOpts `json:"stream_options,omitempty"`
	MaxTokens   int                 `json:"max_tokens,omitempty"`
	Temperature *float32            `json:"temperature,omitempty"`
	Tools       []outboundTool      `json:"tools,omitempty"`
	ToolChoice  any                 `json:"tool_choice,omitempty"`
}

// outboundMessage mirrors openai.ChatCompletionMessage's JSON shape
// for the four roles we model: system / user / assistant / tool.
// Tool-bearing assistant messages carry ToolCalls; tool-result
// messages carry ToolCallID + Name + Content. Empty Content is
// emitted only for assistant messages with tool calls.
type outboundMessage struct {
	Role       string             `json:"role"`
	Content    string             `json:"content,omitempty"`
	Name       string             `json:"name,omitempty"`
	ToolCallID string             `json:"tool_call_id,omitempty"`
	ToolCalls  []outboundToolCall `json:"tool_calls,omitempty"`
}

type outboundTool struct {
	Type     string               `json:"type"`
	Function outboundToolFunction `json:"function"`
}

type outboundToolFunction struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters,omitempty"`
}

type outboundToolCall struct {
	Index    int                `json:"index,omitempty"`
	ID       string             `json:"id,omitempty"`
	Type     string             `json:"type,omitempty"`
	Function outboundToolCallFn `json:"function"`
}

type outboundToolCallFn struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type outboundStreamOpts struct {
	IncludeUsage bool `json:"include_usage"`
}

func buildRequest(model string, req llm.ChatRequest) (outboundRequest, error) {
	msgs := make([]outboundMessage, len(req.Messages))
	for i, m := range req.Messages {
		// Reject unknown roles here as a defense-in-depth check —
		// the chat service already validates, but a future caller
		// that bypasses it shouldn't get to corrupt the wire.
		switch m.Role {
		case "system", "user", "assistant", "tool":
		default:
			return outboundRequest{}, fmt.Errorf("invalid role %q: %w", m.Role, llm.ErrInvalidInput)
		}
		entry := outboundMessage{
			Role:       m.Role,
			Content:    m.Content,
			Name:       m.Name,
			ToolCallID: m.ToolCallID,
		}
		if len(m.ToolCalls) > 0 {
			entry.ToolCalls = make([]outboundToolCall, len(m.ToolCalls))
			for j, tc := range m.ToolCalls {
				entry.ToolCalls[j] = outboundToolCall{
					Index: tc.Index,
					ID:    tc.ID,
					Type:  "function",
					Function: outboundToolCallFn{
						Name:      tc.Name,
						Arguments: tc.Arguments,
					},
				}
			}
		}
		msgs[i] = entry
	}
	out := outboundRequest{
		Model:       model,
		Messages:    msgs,
		Stream:      true,
		Options:     &outboundStreamOpts{IncludeUsage: true},
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
	}
	if len(req.Tools) > 0 {
		out.Tools = make([]outboundTool, len(req.Tools))
		for i, t := range req.Tools {
			out.Tools[i] = outboundTool{
				Type: "function",
				Function: outboundToolFunction{
					Name:        t.Function.Name,
					Description: t.Function.Description,
					Parameters:  t.Function.Parameters,
				},
			}
		}
	}
	if req.ToolChoice != nil {
		out.ToolChoice = req.ToolChoice
	}
	return out, nil
}

// newHTTPClient builds an *http.Client whose Transport.DialContext
// re-runs the IP-class allowlist on every connection. A one-shot
// LookupIP at startup would let a DNS rebinding attacker swap a
// public hostname's answer for a private IP between startup and
// the first dial.
//
// allowPrivate=true disables the check entirely (used for dev
// compose where vLLM lives on a container-network address).
func newHTTPClient(parsed *url.URL, timeout time.Duration, allowPrivate bool) (*http.Client, error) {
	client, err := ssrf.NewHTTPClient(parsed, timeout, allowPrivate, "LLM_BASE_URL")
	if err != nil {
		if errors.Is(err, ssrf.ErrPrivateAddress) {
			return nil, fmt.Errorf("%w; set LLM_ALLOW_PRIVATE_URL=true to override", err)
		}
		return nil, err
	}
	return client, nil
}
