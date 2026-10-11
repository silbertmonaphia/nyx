package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
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
//   - HTTP 4xx (including 401) → Run returns the response; the
//     handler forwards the body verbatim. The spec scenario
//     "Python returns 401 (JWT rejected)" requires no translation
//     — the operator's Python service is misconfigured, not the
//     client.
//   - HTTP 2xx → Run returns the response; the handler forwards
//     the SSE body verbatim.
//
// Token safety: the caller passes the full Authorization header
// value (e.g. "Bearer eyJhbGc…") and the proxy sets it on the
// outbound request unmodified. The proxy MUST NOT log the token —
// use the TokenFingerprint helper if a log line needs a
// distinguishing tag for debugging.
//
// Thread-safety: AgentProxy has no mutable state; the underlying
// *http.Client is safe for concurrent use. The constructor does
// not start goroutines.
type AgentProxy struct {
	baseURL    string // e.g. "http://agent:9000" — trailing "/" stripped
	runPath    string // typically "/agent/run"
	httpClient *http.Client
}

// NewAgentProxy builds a proxy that posts to baseURL + runPath.
// baseURL is required (an empty PYTHON_AGENT_URL at boot time
// means the operator has not configured the agent path; the
// handler's IsAgentEnabled() gates this so the constructor is
// only called when the operator explicitly opted in).
//
// httpClient is supplied by the caller so test code can plug in
// a controlled transport. Production wiring uses a wrapped
// transport with OpenTelemetry http instrumentation; tests pass
// http.DefaultTransport or an httptest.Server's URL via a custom
// RoundTripper.
//
// timeout caps the per-request socket deadline (default 60s —
// long enough for a multi-step run to complete but short enough
// that a wedged upstream doesn't leak a goroutine forever). The
// handler wraps the call in a request-scoped context that
// cancels on client disconnect; this timeout is the upper bound.
func NewAgentProxy(baseURL string, httpClient *http.Client, timeout time.Duration) (*AgentProxy, error) {
	if baseURL == "" {
		return nil, errors.New("chat: PYTHON_AGENT_URL is empty")
	}
	if httpClient == nil {
		if timeout <= 0 {
			timeout = 60 * time.Second
		}
		httpClient = &http.Client{Timeout: timeout}
	}
	return &AgentProxy{
		baseURL:    strings.TrimRight(baseURL, "/"),
		runPath:    "/agent/run",
		httpClient: httpClient,
	}, nil
}

// Run posts the chat request to the Python agent service and
// returns the HTTP response. The handler is responsible for
// reading resp.Body and forwarding bytes verbatim to the SSE
// response writer.
//
// bearerToken is the full Authorization header value (typically
// "Bearer <access_token>"). It is set on the outbound request
// unchanged. The proxy never logs the token value.
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
func (p *AgentProxy) Run(ctx context.Context, req ChatRequest, bearerToken string) (*http.Response, error) {
	body, err := json.Marshal(req)
	if err != nil {
		// ChatRequest is a small struct with json tags on every
		// field — a marshal failure here means a programming bug,
		// not a runtime input issue. Classify as unavailable so
		// the caller fails closed rather than masking the bug.
		return nil, ErrAgentUnavailable
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+p.runPath, bytes.NewReader(body))
	if err != nil {
		return nil, ErrAgentUnavailable
	}
	httpReq.Header.Set("Authorization", bearerToken)
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
