package chat

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"nyx/internal/platform/api"
	"nyx/internal/reqctx"
)

// This file owns the session REST surface of the chat proxy: the five
// authenticated routes under /api/chat/sessions that the SPA sidebar
// drives (list, message replay, create, resume, delete).
//
// These routes are NOT SSE. They are plain JSON request/response
// hops to the Python agent's /agent/sessions... endpoints, so they
// diverge from the streaming path in handler.go on three points:
//
//  1. Upstream status AND body are forwarded verbatim. The Go layer
//     does not translate, retry, or inspect the response — a
//     cross-user session id is the agent's 404, not something Go
//     remaps (owner-scoping lives in the Python store's WHERE
//     clause).
//  2. 5xx is NOT collapsed into an "unavailable" error the way
//     AgentProxy collapses it for SSE. The SSE contract mandates a
//     typed agent_unavailable frame INSTEAD of the upstream body;
//     the session contract requires the opposite (verbatim
//     forwarding). Only a transport failure — dial error, DNS
//     failure, timeout — is an error here, and the handler renders
//     it as 502.
//  3. The caller's Authorization header rides along as a passthrough
//     (same trust model as /agent/run): Go verified the JWT at the
//     SPA edge, Python does not verify tokens, and every hop carries
//     the shared AGENT_SERVICE_TOKEN in X-Nyx-Service-Token. Neither
//     credential value may be logged.

// sessionsBasePath is the upstream prefix every session call shares.
// The Python agent mounts the same names as the public routes, so
// the mapping is one-for-one with no renaming.
const sessionsBasePath = "/agent/sessions"

// defaultSessionsTimeout caps every session/approvals call. The
// list/replay/create/resume/delete endpoints are plain queries — 30s
// is generous. The cap is applied as the request-context deadline in
// Forward (NOT http.Client.Timeout): production reuses the
// AgentProxy's SSE client, which deliberately carries no Timeout
// after the 2026-10-14 60s-cut incident, and a client-level Timeout
// would have to be re-shared with the streaming hop. The nil-client
// branch in NewSessionsProxy uses the same constant as its client
// Timeout for callers that pass nil (tests, future wiring).
//
// A var — not a const — so tests can shrink the cap instead of
// waiting 30s of wall clock; production code never writes it.
var defaultSessionsTimeout = 30 * time.Second

// Static response literals. Every one of these is safe to ship: they
// describe the failure in user-facing terms and carry no error text,
// no SQL, and no upstream detail. The underlying error goes to the
// log via api.ClassifyAndLog.
const (
	// 503 — agent mode is off (AGENT_ENABLED=false / no
	// PYTHON_AGENT_URL). Sessions simply do not exist in the
	// single-turn world; that is a server-side capability gap, not a
	// client error.
	sessionsDisabledMessage = "Chat sessions are unavailable"
	sessionsDisabledDetail  = "The agent service is disabled"

	// 502 — agent mode is on but the upstream could not be reached
	// (connection refused / DNS failure / timeout).
	sessionsUnreachableMessage = "Agent sessions unavailable"
	sessionsUnreachableDetail  = "The agent service could not be reached"

	// 502 follow-up — the upstream answered but the body copy to the
	// client failed (typically the client hung up mid-response).
	// Logged only; the status is already on the wire by then.
	sessionsCopyFailedDetail = "Agent session response copy failed"
)

// SessionsProxy is the thin JSON proxy from the Go chat handler to
// the Python agent's session REST surface. See the file header for
// how it differs from AgentProxy; the two share the base URL and
// http.Client in production (the handler derives this proxy from the
// AgentProxy the Service was built with).
//
// Thread-safety: no mutable state; the *http.Client is safe for
// concurrent use, and the constructor starts no goroutines.
type SessionsProxy struct {
	baseURL      string // e.g. "http://agent:9000" — trailing "/" stripped
	serviceToken string // AGENT_SERVICE_TOKEN, sent as ServiceTokenHeader
	httpClient   *http.Client
}

// NewSessionsProxy builds a proxy that forwards to baseURL under
// /agent/sessions. baseURL and serviceToken are required — an empty
// value means the operator never configured the agent path, and the
// handler's IsAgentEnabled() gate is what decides between "disabled
// (503)" and "configured (proxy)"; a proxy built on "" could only
// ever fail with a misleading 502.
//
// httpClient is supplied by the caller (AgentProxy's client in
// production, a test client in tests) so a single transport — with
// its timeouts and any OTel instrumentation — is reused across both
// agent hops. A nil client gets a plain client with the default
// timeout.
func NewSessionsProxy(baseURL, serviceToken string, httpClient *http.Client) (*SessionsProxy, error) {
	if baseURL == "" {
		return nil, errors.New("chat: PYTHON_AGENT_URL is empty")
	}
	if serviceToken == "" {
		return nil, errors.New("chat: AGENT_SERVICE_TOKEN is empty")
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultSessionsTimeout}
	}
	return &SessionsProxy{
		baseURL:      strings.TrimRight(baseURL, "/"),
		serviceToken: serviceToken,
		httpClient:   httpClient,
	}, nil
}

// sessionProxyRequest is one outbound session call. It is a struct
// rather than a parameter list because the handler builds it from
// four string-valued fields taken off the inbound request (method,
// path, query, content type); positional strings of the same type
// are exactly the shape that silently swaps.
type sessionProxyRequest struct {
	method      string
	path        string    // upstream path, e.g. "/agent/sessions/<id>/messages"
	rawQuery    string    // escaped query, forwarded verbatim (no leading "?")
	body        io.Reader // nil for body-less calls; the inbound r.Body otherwise
	contentType string    // inbound Content-Type, "" → application/json
	userID      int       // verified user id from reqctx
	bearer      string    // inbound Authorization header, forwarded verbatim
}

// Forward sends one session call upstream and returns the raw
// response. The caller owns resp.Body and MUST close it.
//
// Header policy is deny-by-default: only X-Nyx-Service-Token, the
// verified X-Nyx-User-Id, and Authorization (passthrough) are set,
// plus the content type. Cookies, Host, X-Forwarded-*, and
// hop-by-hop headers are NOT forwarded — Python has no business
// seeing the SPA's cookie jar, and a forwarded Host breaks routing.
//
// Returns a non-nil error ONLY when the call could not be made or
// completed (malformed base URL, transport failure, timeout, or a
// canceled request context). Any HTTP status the agent returns —
// including 4xx and 5xx — is a successful proxy call; the caller
// forwards it verbatim.
//
// The call is bounded by `defaultSessionsTimeout` on top of the
// caller's context: the shared client in production carries no
// http.Client.Timeout (it is the SSE client; see agent_proxy.go), so
// without this deadline a stalled upstream would hold these
// non-streaming requests — and their connection + limiter slots —
// indefinitely.
func (p *SessionsProxy) Forward(ctx context.Context, req sessionProxyRequest) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(ctx, defaultSessionsTimeout)
	defer cancel()

	target, err := url.Parse(p.baseURL + req.path)
	if err != nil {
		return nil, fmt.Errorf("chat: build agent session URL: %w", err)
	}
	// RawQuery is the already-escaped form taken off the inbound
	// request, so it is assigned rather than re-encoded — re-encoding
	// would double-escape the caller's cursor values (the list
	// endpoint's `before` cursor is base64url, which survives a
	// round trip only if we pass it through untouched).
	target.RawQuery = req.rawQuery

	httpReq, err := http.NewRequestWithContext(ctx, req.method, target.String(), req.body)
	if err != nil {
		return nil, fmt.Errorf("chat: build agent session request: %w", err)
	}
	httpReq.Header.Set("X-Nyx-User-Id", strconv.Itoa(req.userID))
	httpReq.Header.Set(ServiceTokenHeader, p.serviceToken)
	if req.bearer != "" {
		httpReq.Header.Set("Authorization", req.bearer)
	}
	contentType := req.contentType
	if contentType == "" {
		contentType = "application/json"
	}
	httpReq.Header.Set("Content-Type", contentType)

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		// Connection refused, DNS failure, timeout, TLS handshake
		// error, ctx cancel — all land here and are rendered as 502
		// by the handler. The wrapped error is logged, never shipped.
		return nil, fmt.Errorf("chat: agent session request failed: %w", err)
	}
	return resp, nil
}

// newSessionsProxyFromService derives the session proxy from the
// service's agent proxy: same base URL, transport, and service token.
// Returns nil when the service runs in legacy mode (AGENT_ENABLED=
// false), which is the signal the session routes use to answer 503.
//
// Deriving rather than wiring a second proxy in cmd/api keeps the two
// agent hops on one configuration surface: AGENT_ENABLED +
// PYTHON_AGENT_URL is the single switch, and a proxy can never be
// half-configured (SSE on, sessions off, or vice versa).
func newSessionsProxyFromService(s *Service) *SessionsProxy {
	if s == nil {
		return nil
	}
	agent := s.Proxy()
	if agent == nil {
		return nil
	}
	proxy, err := NewSessionsProxy(agent.baseURL, agent.serviceToken, agent.httpClient)
	if err != nil {
		// Unreachable: NewAgentProxy already rejects an empty base
		// URL, so the derived proxy can only fail if that invariant
		// is broken. Fail closed (nil → 503) rather than building a
		// proxy pointed at "".
		return nil
	}
	return proxy
}

// sessionPath builds an upstream path for a session-scoped route. The
// id is escaped as a SINGLE path segment (url.PathEscape) so a
// crafted id — one containing "/", "..", or a query separator —
// cannot introduce an extra segment, climb out of the
// /agent/sessions prefix, or smuggle a query string. Python
// re-validates the id anyway (uuid4); this is the Go-side guarantee
// that we never hand it a path it did not ask for.
func sessionPath(sessionID, suffix string) string {
	return sessionsBasePath + "/" + url.PathEscape(sessionID) + suffix
}

// SessionsList proxies GET /api/chat/sessions. The `limit` / `before`
// query parameters ride along untouched.
func (h *Handler) sessionsList(w http.ResponseWriter, r *http.Request) {
	h.proxySession(w, r, http.MethodGet, sessionsBasePath)
}

// sessionsCreate proxies POST /api/chat/sessions (create an empty
// session for the caller).
func (h *Handler) sessionsCreate(w http.ResponseWriter, r *http.Request) {
	h.proxySession(w, r, http.MethodPost, sessionsBasePath)
}

// sessionsMessages proxies GET /api/chat/sessions/{id}/messages
// (replay one session).
func (h *Handler) sessionsMessages(w http.ResponseWriter, r *http.Request) {
	h.proxySession(w, r, http.MethodGet, sessionPath(chi.URLParam(r, "session_id"), "/messages"))
}

// sessionsResume proxies POST /api/chat/sessions/{id}/resume (refresh
// the session's activity timestamp).
func (h *Handler) sessionsResume(w http.ResponseWriter, r *http.Request) {
	h.proxySession(w, r, http.MethodPost, sessionPath(chi.URLParam(r, "session_id"), "/resume"))
}

// sessionsDelete proxies DELETE /api/chat/sessions/{id} (soft-delete).
func (h *Handler) sessionsDelete(w http.ResponseWriter, r *http.Request) {
	h.proxySession(w, r, http.MethodDelete, sessionPath(chi.URLParam(r, "session_id"), ""))
}

// proxySession is the single code path behind all five session
// routes. Lifecycle:
//
//  1. Auth fence — a zero user id means the route was mounted without
//     the auth middleware. 401 rather than proxying with
//     "X-Nyx-User-Id: 0", which Python would treat as a foreign
//     (and possibly valid-looking) caller.
//  2. Availability gate — no proxy configured (AGENT_ENABLED=false)
//     → 503. No upstream call is attempted.
//  3. Forward method / path / query / body / headers to the agent.
//  4. Transport failure → 502 with a static safe detail; the error
//     is logged with the request id via api.ClassifyAndLog.
//  5. Otherwise forward the upstream status, content type, and body
//     verbatim.
//
// The handler does NOT validate the session id or the query string
// itself: the agent owns session semantics (including 404 for a
// cross-user id), and re-validating here would create a second
// source of truth for what a valid cursor or id is.
func (h *Handler) proxySession(w http.ResponseWriter, r *http.Request, method, path string) {
	userID := reqctx.UserIDFromContext(r.Context())
	if userID == 0 {
		api.WriteError(w, r, http.StatusUnauthorized, "Authentication required", nil)
		return
	}
	if h.sessions == nil || h.service == nil || !h.service.IsAgentEnabled() {
		api.WriteError(w, r, http.StatusServiceUnavailable, sessionsDisabledMessage, sessionsDisabledDetail)
		return
	}

	// POSTs carry a body (create / resume); GET / DELETE do not. The
	// inbound body is streamed straight through — it is already
	// bounded by the router-level 1 MiB maxBodyBytes middleware, and
	// buffering it here would only add a copy.
	var body io.Reader
	if r.Method == http.MethodPost {
		body = r.Body
	}

	resp, err := h.sessions.Forward(r.Context(), sessionProxyRequest{
		method:      method,
		path:        path,
		rawQuery:    r.URL.RawQuery,
		body:        body,
		contentType: r.Header.Get("Content-Type"),
		userID:      userID,
		bearer:      r.Header.Get("Authorization"),
	})
	if err != nil {
		if r.Context().Err() != nil {
			// The client hung up (or the server is shutting down).
			// There is nobody to write the 502 to, and logging a
			// canceled context as a proxy failure would be noise.
			return
		}
		api.WriteError(w, r, http.StatusBadGateway, sessionsUnreachableMessage,
			api.ClassifyAndLog(r.Context(), err, sessionsUnreachableDetail))
		return
	}
	defer func() { _ = resp.Body.Close() }()

	writeSessionResponse(w, r, resp)
}

// writeSessionResponse forwards the upstream status, content type,
// and body to the client verbatim. Only Content-Type is copied from
// the upstream header set (deny-by-default: an upstream Set-Cookie,
// CORS header, or hop-by-hop header has no business reaching the
// SPA through this hop).
func writeSessionResponse(w http.ResponseWriter, r *http.Request, resp *http.Response) {
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, resp.Body); err != nil {
		// A copy failure here means the client went away mid-
		// response; the status is already on the wire and cannot be
		// changed. Log real failures, skip the routine disconnects
		// (same posture as streamProxy).
		if !errors.Is(err, context.Canceled) && r.Context().Err() == nil {
			api.ClassifyAndLog(r.Context(), err, sessionsCopyFailedDetail)
		}
	}
}
