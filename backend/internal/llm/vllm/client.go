// Package vllm implements llm.Provider against a self-hosted vLLM
// OpenAI-compatible chat-completions server. Unlike internal/llm/openai,
// which targets OpenAI's hosted service via sashabaranov/go-openai, this
// package talks raw HTTP so vLLM-specific quirks (slightly different
// error JSON, optional tool-call deltas) don't have to be filtered
// through an SDK whose error types are designed for OpenAI proper.
//
// The wire contract is identical to OpenAI's: POST /chat/completions
// with {model, messages, stream:true, stream_options.include_usage:true,
// max_tokens} returns text/event-stream frames of shape
//
//	data: {"choices":[{"delta":{"content":"..."}}]}\n\n
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

	"nyx/internal/llm"
	"nyx/internal/platform/config"
)

// Client implements llm.Provider against a self-hosted vLLM
// OpenAI-compatible server. Constructed once at process startup and
// reused across requests — the underlying *http.Client is
// goroutine-safe.
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
// and a populated *ChatUsage.
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

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		span.RecordError(err)
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
		span.RecordError(err)
		return nil, c.mapError(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Drain the body so the connection can be reused; surface the
		// status as a typed marker so mapError can route it without
		// string-matching err.Error().
		_, _ = io.Copy(io.Discard, resp.Body)
		statusErr := &upstreamStatusError{status: resp.StatusCode}
		span.RecordError(statusErr)
		return nil, c.mapError(statusErr)
	}

	return c.consumeSSE(ctx, span, resp.Body, onDelta)
}

// consumeSSE decodes one SSE stream into a sequence of onDelta calls.
// Returns the trailing *ChatUsage (nil if vLLM didn't emit one) and
// the first non-recoverable error.
func (c *Client) consumeSSE(ctx context.Context, span trace.Span, body io.Reader, onDelta func(delta string, finalUsage *llm.ChatUsage) error) (*llm.ChatUsage, error) {
	// bufio.Scanner's default 64 KiB token size truncates long content
	// deltas (a 70 KiB chunked completion would be silently split).
	// Raise the ceiling to 1 MiB which comfortably fits any realistic
	// single-token emission.
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)

	var usage *llm.ChatUsage
	for scanner.Scan() {
		// Cancellation check between lines — cheaper than waiting for
		// the next read on the body.
		if err := ctx.Err(); err != nil {
			span.RecordError(err)
			return usage, llm.ErrContextCanceled
		}

		line := scanner.Text()
		if line == "" {
			// Blank line = end of SSE frame. vLLM (and OpenAI)
			// don't emit event:/id:/retry: on /chat/completions
			// streams, so we don't track per-event state.
			continue
		}
		if strings.HasPrefix(line, ":") {
			// SSE comment / heartbeat — ignore.
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			// Field we don't model (event:, id:, retry:). Skip.
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))

		if payload == "[DONE]" {
			break
		}

		frame, err := decodeFrame(payload)
		if err != nil {
			// Malformed mid-stream JSON — collapse to
			// ErrProviderUnavailable, mirroring the OpenAI client.
			span.RecordError(err)
			return usage, c.mapError(fmt.Errorf("vllm: decode frame: %w", err))
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

	if err := scanner.Err(); err != nil {
		span.RecordError(err)
		return usage, c.mapError(err)
	}

	span.SetStatus(codes.Ok, "")
	return usage, nil
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

// sseFrame is the subset of the OpenAI /vLLM chunk schema we read.
// Tool-call deltas, finish_reason, logprobs, etc. are intentionally
// ignored — Go's permissive JSON decoder drops unknown fields
// without error, so adding them on the vLLM side never breaks us.
type sseFrame struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
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
// fields we use, minus the SDK dependency.
type outboundRequest struct {
	Model       string              `json:"model"`
	Messages    []outboundMessage   `json:"messages"`
	Stream      bool                `json:"stream"`
	Options     *outboundStreamOpts `json:"stream_options,omitempty"`
	MaxTokens   int                 `json:"max_tokens,omitempty"`
	Temperature *float32            `json:"temperature,omitempty"`
}

type outboundMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
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
		case "system", "user", "assistant":
		default:
			return outboundRequest{}, fmt.Errorf("invalid role %q: %w", m.Role, llm.ErrInvalidInput)
		}
		msgs[i] = outboundMessage{Role: m.Role, Content: m.Content}
	}
	return outboundRequest{
		Model:       model,
		Messages:    msgs,
		Stream:      true,
		Options:     &outboundStreamOpts{IncludeUsage: true},
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
	}, nil
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
	if !allowPrivate {
		if err := checkHost(parsed.Hostname()); err != nil {
			return nil, err
		}
	}

	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, &net.OpError{Op: "dial", Net: network, Source: nil, Addr: nil, Err: err}
			}
			if !allowPrivate {
				if err := checkHost(host); err != nil {
					return nil, &net.OpError{Op: "dial", Net: network, Source: nil, Addr: nil, Err: err}
				}
			}
			var d net.Dialer
			return d.DialContext(ctx, network, net.JoinHostPort(host, port))
		},
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	return &http.Client{
		Transport: transport,
		Timeout:   timeout,
	}, nil
}

// checkHost resolves the host and refuses loopback, RFC1918, link-
// local, multicast, and unspecified addresses. The check fires once
// at startup (cheap typo guard) AND once per dial inside DialContext
// (closes the DNS-rebinding window).
func checkHost(host string) error {
	if host == "" {
		return errors.New("LLM_BASE_URL host is empty")
	}
	// IP literal? Resolve directly without DNS.
	if ip := net.ParseIP(host); ip != nil {
		if refused, reason := ipClassRefused(ip); refused {
			return fmt.Errorf("LLM_BASE_URL points at a private/restricted address (%s); set LLM_ALLOW_PRIVATE_URL=true to override", reason)
		}
		return nil
	}
	addrs, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("LLM_BASE_URL DNS lookup failed for %q: %w", host, err)
	}
	if len(addrs) == 0 {
		return fmt.Errorf("LLM_BASE_URL DNS lookup returned no addresses for %q", host)
	}
	for _, ip := range addrs {
		if refused, reason := ipClassRefused(ip); refused {
			return fmt.Errorf("LLM_BASE_URL resolves to a private/restricted address (%s); set LLM_ALLOW_PRIVATE_URL=true to override", reason)
		}
	}
	return nil
}

// ipClassRefused returns (true, reason) when the IP belongs to a
// class we refuse by default. Uses net.IP's classification methods
// (Go 1.17+) so RFC1918 + ULA + link-local + IPv4-mapped IPv6 are
// all covered without rolling CIDR checks by hand.
func ipClassRefused(ip net.IP) (bool, string) {
	switch {
	case ip.IsUnspecified():
		return true, "unspecified (0.0.0.0 / ::)"
	case ip.IsLoopback():
		return true, "loopback (127.0.0.0/8 / ::1)"
	case ip.IsPrivate():
		// Covers RFC1918 (10/8, 172.16/12, 192.168/16) and ULA (fc00::/7).
		return true, "private (RFC1918 / ULA)"
	case ip.IsLinkLocalUnicast():
		return true, "link-local unicast (169.254/16 / fe80::/10)"
	case ip.IsLinkLocalMulticast():
		return true, "link-local multicast"
	case ip.IsInterfaceLocalMulticast():
		return true, "interface-local multicast (ff01::/16)"
	case ip.IsMulticast():
		// Site-local / org-local multicast — operators don't
		// legitimately point an LLM URL at a multicast group.
		return true, "multicast"
	}
	return false, ""
}
