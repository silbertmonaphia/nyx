package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// AgentProxy is the thin SSE proxy from the Go chat handler to the
// Python LangGraph orchestrator. Activated when AGENT_ENABLED=true
// and PYTHON_AGENT_URL is set; see
// internal/platform/config/config.go.
//
// The proxy is a THIN HTTP client. It does NOT transform the SSE
// frames Python emits — see design §1 and the byte-for-byte
// regression test (internal/chat/agent_proxy_integration_test.go).
// The handler is responsible for copying resp.Body bytes verbatim
// to the chat response writer; the proxy only opens the upstream
// request and returns the response.
//
// Failure semantics (spec requirement "Python service unavailable
// fails closed"):
//
//   - Connection refused / DNS failure / dial timeout / TLS
//     handshake failure → Run returns (nil, ErrAgentUnavailable).
//   - HTTP 5xx → Run returns (nil, ErrAgentUnavailable). The
//     handler MUST NOT forward 5xx bodies — the spec mandates an
//     `error{code:"agent_unavailable"}` SSE frame instead so
//     operators see agent-side failures clearly.
//   - HTTP 4xx → Run returns the response; the handler forwards
//     the body verbatim. The operator's Python service is
//     misconfigured when 4xx fires (missing X-Nyx-User-Id or a
//     service-token mismatch), not the client; surface unchanged.
//   - HTTP 2xx → Run returns the response; the handler forwards
//     the SSE body verbatim.
//
// Trust model: the Python service is public-facing, so every hop is
// authenticated with the shared AGENT_SERVICE_TOKEN. Python does NOT
// verify the JWT — Go verified it at the SPA edge. The proxy sends
// three credentials:
//   - X-Nyx-Service-Token: the shared credential; Python rejects
//     requests without it, so X-Nyx-User-Id is only trusted behind
//     this hop. The id from reqctx keys the budget guard + scope.
//   - Authorization: the user's original bearer token, forwarded as
//     a passthrough to Go's /mcp + /internal/rag/retrieve (re-verified
//     there). Never log either credential; TokenFingerprint is the
//     user-token tag.
//
// Thread-safety: AgentProxy has no mutable state; the underlying
// *http.Client is safe for concurrent use. The constructor does
// not start goroutines.
type AgentProxy struct {
	baseURL      string // e.g. "http://agent:9000" — trailing "/" stripped
	runPath      string // typically "/agent/run"
	serviceToken string // AGENT_SERVICE_TOKEN, sent as ServiceTokenHeader
	httpClient   *http.Client
}

// defaultResponseHeaderTimeout bounds how long the transport waits
// for the agent's response HEADERS on the default client. The agent
// writes its SSE headers in well under a second (the first frame is
// the run id, emitted before any model call), so this only fires on
// a wedged upstream — it never touches the streamed body itself.
const defaultResponseHeaderTimeout = 30 * time.Second

const ServiceTokenHeader = "X-Nyx-Service-Token" //nolint:gosec // header name, not a credential

// NewAgentProxy builds a proxy that posts to baseURL + runPath.
// baseURL and serviceToken are required (empty values mean the
// operator has not configured the agent path; the handler's
// IsAgentEnabled() gates this so the constructor is only called
// when the operator explicitly opted in).
//
// httpClient is supplied by the caller so test code can plug in
// a controlled transport; nil builds a default one. Production
// wiring passes nil.
//
// The client MUST NOT carry an http.Client.Timeout. Client.Timeout
// covers the ENTIRE exchange — response headers AND reading the
// body — and the agent answers with a long-lived SSE stream, so a
// set Timeout truncates every run that outlives it mid-stream with
// no terminal frame (the 2026-10-14 incident: the 60s production
// client severed a run waiting on an approval at t+61.8s,
// observed on the wire as keep-alive frames followed by a dead
// connection). The bound on a stream is the CALLER'S request
// context — the handler derives it from LLM_MAX_STREAM_DURATION —
// which also keeps the cancel-on-client-disconnect semantics; the
// default client adds only a ResponseHeaderTimeout so a wedged
// upstream fails fast instead of holding the request open for the
// full stream cap.
func NewAgentProxy(baseURL, serviceToken string, httpClient *http.Client) (*AgentProxy, error) {
	if baseURL == "" {
		return nil, errors.New("chat: PYTHON_AGENT_URL is empty")
	}
	if serviceToken == "" {
		return nil, errors.New("chat: AGENT_SERVICE_TOKEN is empty")
	}
	if httpClient == nil {
		transport := http.DefaultTransport
		if t, ok := transport.(*http.Transport); ok {
			clone := t.Clone()
			clone.ResponseHeaderTimeout = defaultResponseHeaderTimeout
			transport = clone
		}
		httpClient = &http.Client{Transport: transport}
	}
	return &AgentProxy{
		baseURL:      strings.TrimRight(baseURL, "/"),
		runPath:      "/agent/run",
		serviceToken: serviceToken,
		httpClient:   httpClient,
	}, nil
}

// Run posts the chat request to the Python agent service and
// returns the HTTP response. The handler is responsible for
// reading resp.Body and forwarding bytes verbatim to the SSE
// response writer.
//
// userID is the verified user id from reqctx.UserIDFromContext,
// set on the outbound request as `X-Nyx-User-Id`. Python reads
// it directly (it does not decode the JWT) and uses it for the
// per-user budget counter and per-owner scoping on tool calls.
//
// bearerToken is the full Authorization header value (typically
// "Bearer <access_token>"). It is forwarded as a passthrough so
// Python can present it to Go's /mcp and /internal/rag/retrieve
// endpoints, which re-verify the JWT themselves. The proxy never
// logs the token value.
//
// Returns ErrAgentUnavailable when the upstream is unreachable
// (network error) or returns HTTP 5xx. On ErrAgentUnavailable
// the returned *http.Response is always nil.
//
// On HTTP 2xx/3xx/4xx the returned response's body is the raw
// Python service output — the handler MUST forward it byte-for-byte
// per the spec's "Wire envelope is byte-for-byte identical"
// requirement. The handler MAY inspect resp.StatusCode to decide
// whether to forward; the proxy itself does not transform.
func (p *AgentProxy) Run(ctx context.Context, payload AgentRunPayload, userID int, bearerToken string) (*http.Response, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, ErrAgentUnavailable
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+p.runPath, bytes.NewReader(body))
	if err != nil {
		return nil, ErrAgentUnavailable
	}
	httpReq.Header.Set("X-Nyx-User-Id", strconv.Itoa(userID))
	httpReq.Header.Set(ServiceTokenHeader, p.serviceToken)
	if bearerToken != "" {
		httpReq.Header.Set("Authorization", bearerToken)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		// Connection refused, DNS failure, timeout, TLS handshake
		// error, etc. — all collapse to ErrAgentUnavailable per
		// the spec's fail-closed contract.
		return nil, ErrAgentUnavailable
	}
	if resp.StatusCode >= 500 {
		// 5xx: close the body so the connection can be reused
		// (we're inside the http.Client's transport pool), then
		// return ErrAgentUnavailable. The handler will emit the
		// typed error frame; the body is not forwarded.
		_ = resp.Body.Close()
		return nil, ErrAgentUnavailable
	}
	return resp, nil
}

// TokenFingerprint returns a short, non-reversible tag identifying
// a bearer token for debug logs. The format is "<first 6 chars>…"
// for non-empty tokens, "" for empty. NEVER use this to compare
// token equality — only as a per-request visual fingerprint so an
// operator can correlate two log lines as "the same bearer" without
// seeing the token value.
//
// The token value MUST NOT appear in any log line other than this
// derived fingerprint. The proxy does not log the token at all;
// this helper exists for callers (e.g. an OTel span attribute) that
// want a distinguishing tag.
func TokenFingerprint(token string) string {
	if token == "" {
		return ""
	}
	// Strip the "Bearer " prefix if present so the fingerprint is
	// the same regardless of which form the caller passed. The
	// header value can legitimately be either "Bearer <token>" or
	// just "<token>" depending on how the caller extracted it.
	t := strings.TrimSpace(token)
	t = strings.TrimPrefix(t, "Bearer ")
	t = strings.TrimSpace(t)
	if len(t) <= 6 {
		return t + "…"
	}
	return t[:6] + "…"
}
