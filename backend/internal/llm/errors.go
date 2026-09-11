package llm

import (
	"errors"
	"net/http"

	"nyx/internal/platform/api"
)

// Sentinel errors the chat domain (and any future consumer of the
// llm package) returns from its service layer. The api package
// registers each one with a wire-level HTTP status + message so
// handlers stay one-liners: `return nil, api.MapError(ctx, err, "safe detail")`.
//
// Status choices:
//   - ErrProviderUnavailable → 502 Bad Gateway. The upstream LLM is
//     unreachable or returned a 5xx; this is the operator's problem,
//     not the client's.
//   - ErrRateLimited → 429 Too Many Requests. The provider throttled
//     the request (e.g. OpenAI 429). The frontend surfaces a retry
//     toast.
//   - ErrInvalidInput → 400 Bad Request. The chat service rejected
//     the request before calling the provider — empty history,
//     client-supplied system role, oversized content, etc.
//   - ErrContextCanceled → 499 (nginx "Client Closed Request"). The
//     SPA's AbortController fired or the handler's deadline elapsed.
//     Surfaced as a stream-error frame, not a JSON envelope, because
//     the response is already in SSE mode by the time this is hit.
var (
	ErrProviderUnavailable = errors.New("llm provider unavailable")
	ErrRateLimited         = errors.New("llm rate limited")
	ErrInvalidInput        = errors.New("invalid chat input")
	ErrContextCanceled     = errors.New("llm context canceled")
)

// Register each sentinel with api.MapError so handlers can funnel
// any wrapped error through the canonical envelope. Init order across
// packages is not deterministic but each registration is independent,
// so a swap in map.go order is harmless.
func init() {
	api.RegisterSentinel(ErrProviderUnavailable, http.StatusBadGateway, "Upstream LLM unavailable")
	api.RegisterSentinel(ErrRateLimited, http.StatusTooManyRequests, "LLM rate limited")
	api.RegisterSentinel(ErrInvalidInput, http.StatusBadRequest, "Invalid chat input")
	api.RegisterSentinel(ErrContextCanceled, 499, "Client closed request")
}
