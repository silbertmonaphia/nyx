package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace/noop"

	"nyx/internal/platform/api"
	"nyx/internal/platform/auth"
	"nyx/internal/platform/config"
)

// sessionIDFixture is a valid uuid4 — the shape the SPA receives from
// the agent and echoes back on the session routes.
const sessionIDFixture = "3f2504e0-4f89-41d3-9a0c-0305e82c3301"

// capturedSessionRequest is what the upstream stub records for one
// inbound proxied call. requestURI is the on-the-wire path+query
// (escaped); path is the server-side decoded path.
type capturedSessionRequest struct {
	method        string
	requestURI    string
	path          string
	rawQuery      string
	authorization string
	userID        string
	serviceToken  string
	contentType   string
	body          string
}

// sessionUpstream is the httptest stand-in for the Python agent's
// session REST surface. It records every inbound call and replies
// with a canned status + body, so tests can assert both directions
// of the forwarding contract (what the agent saw, what the client
// got back).
//
// The captured state is mutex-guarded: the stub handler runs on the
// test server's goroutines, the assertions run on the test's.
type sessionUpstream struct {
	mu     sync.Mutex
	last   capturedSessionRequest
	calls  int
	status int
	body   string
}

func newSessionUpstream(t *testing.T, status int, body string) (*httptest.Server, *sessionUpstream) {
	t.Helper()
	up := &sessionUpstream{status: status, body: body}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Read errors are not actionable here (the test client always
		// sends a complete body) and must not fail the test from a
		// server goroutine, so the error is discarded.
		buf, _ := io.ReadAll(r.Body)

		up.mu.Lock()
		up.calls++
		up.last = capturedSessionRequest{
			method:        r.Method,
			requestURI:    r.RequestURI,
			path:          r.URL.Path,
			rawQuery:      r.URL.RawQuery,
			authorization: r.Header.Get("Authorization"),
			userID:        r.Header.Get("X-Nyx-User-Id"),
			serviceToken:  r.Header.Get(ServiceTokenHeader),
			contentType:   r.Header.Get("Content-Type"),
			body:          string(buf),
		}
		up.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if body != "" {
			_, _ = io.WriteString(w, body)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, up
}

// snapshot returns the last captured request plus the total call
// count, under the stub's lock.
func (u *sessionUpstream) snapshot() (capturedSessionRequest, int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.last, u.calls
}

// mustAgentProxy builds an AgentProxy pointed at baseURL. The session
// proxy derives from the same config at NewHandler time, so this is
// the whole setup a session test needs.
func mustAgentProxy(t *testing.T, baseURL string) *AgentProxy {
	t.Helper()
	proxy, err := NewAgentProxy(baseURL, testServiceToken, nil)
	require.NoError(t, err)
	return proxy
}

// newSessionsHarness wires the chat route group with an agent-mode
// service (proxy required) and an optional limiter, returning the
// assembled handler + a TokenService for minting bearer tokens.
func newSessionsHarness(t *testing.T, proxy *AgentProxy, limiter *UserRateLimiter) (http.Handler, auth.TokenService) {
	t.Helper()
	cfg := &config.Config{
		LLMMaxHistoryMessages: 5,
		LLMMaxMessageChars:    50,
		LLMMaxTokens:          100,
	}
	tracer := noop.NewTracerProvider().Tracer("test")
	svc := NewServiceWithProxy(&llmProviderUnused{}, cfg, tracer, proxy)
	return buildRouter(t, svc, limiter)
}

// TestSessionsRoutes_ForwardToAgent covers all five routes end to
// end: the client call, the mapped upstream call (method / path /
// query / headers / body), and the verbatim response relay.
func TestSessionsRoutes_ForwardToAgent(t *testing.T) {
	const listBody = `{"sessions":[{"session_id":"` + sessionIDFixture + `"}],"next_cursor":"MTczMDAwMDAwMDAwMDAwMF9hYmM"}`

	cases := []struct {
		name           string
		method         string
		clientPath     string
		body           string
		wantPath       string
		wantQuery      string
		upstreamStatus int
		upstreamBody   string
	}{
		{
			name:           "list forwards pagination query verbatim",
			method:         http.MethodGet,
			clientPath:     "/api/chat/sessions?limit=10&before=MTczMDAwMDAwMDAwMDAwMF9hYmM",
			wantPath:       "/agent/sessions",
			wantQuery:      "limit=10&before=MTczMDAwMDAwMDAwMDAwMF9hYmM",
			upstreamStatus: http.StatusOK,
			upstreamBody:   listBody,
		},
		{
			name:           "messages replays one session",
			method:         http.MethodGet,
			clientPath:     "/api/chat/sessions/" + sessionIDFixture + "/messages",
			wantPath:       "/agent/sessions/" + sessionIDFixture + "/messages",
			upstreamStatus: http.StatusOK,
			upstreamBody:   `{"session_id":"` + sessionIDFixture + `","messages":[]}`,
		},
		{
			name:           "create posts the body",
			method:         http.MethodPost,
			clientPath:     "/api/chat/sessions",
			body:           `{"title":"ignored-by-agent"}`,
			wantPath:       "/agent/sessions",
			upstreamStatus: http.StatusCreated,
			upstreamBody:   `{"session_id":"` + sessionIDFixture + `","started_at":"2026-10-14T00:00:00Z"}`,
		},
		{
			name:           "resume refreshes the activity timestamp",
			method:         http.MethodPost,
			clientPath:     "/api/chat/sessions/" + sessionIDFixture + "/resume",
			wantPath:       "/agent/sessions/" + sessionIDFixture + "/resume",
			upstreamStatus: http.StatusOK,
			upstreamBody:   `{"session_id":"` + sessionIDFixture + `","resumed_at":"2026-10-14T00:00:00Z"}`,
		},
		{
			name:           "delete soft-deletes",
			method:         http.MethodDelete,
			clientPath:     "/api/chat/sessions/" + sessionIDFixture,
			wantPath:       "/agent/sessions/" + sessionIDFixture,
			upstreamStatus: http.StatusNoContent,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, up := newSessionUpstream(t, tc.upstreamStatus, tc.upstreamBody)
			harness, tokens := newSessionsHarness(t, mustAgentProxy(t, srv.URL), nil)
			server := httptest.NewServer(harness)
			t.Cleanup(server.Close)

			bearer := "Bearer " + bearerToken(t, tokens)
			var body io.Reader
			if tc.body != "" {
				body = bytes.NewReader([]byte(tc.body))
			}
			req, err := http.NewRequest(tc.method, server.URL+tc.clientPath, body)
			require.NoError(t, err)
			req.Header.Set("Authorization", bearer)
			req.Header.Set("Content-Type", "application/json")

			resp, err := server.Client().Do(req)
			require.NoError(t, err)
			defer func() { _ = resp.Body.Close() }()

			// Client side: the agent's status, content type, and body
			// arrive verbatim — no Go envelope, no translation.
			got, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			assert.Equal(t, tc.upstreamStatus, resp.StatusCode)
			assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))
			assert.Equal(t, tc.upstreamBody, string(got))

			// Agent side: the mapped call.
			last, calls := up.snapshot()
			require.Equal(t, 1, calls, "exactly one upstream call per client request")
			assert.Equal(t, tc.method, last.method)
			assert.Equal(t, tc.wantPath, last.path)
			assert.Equal(t, tc.wantQuery, last.rawQuery,
				"query string forwarded verbatim (cursor values must not be re-encoded)")
			assert.Equal(t, tc.body, last.body)
			assert.Equal(t, "application/json", last.contentType,
				"the caller's content type rides through")
			assert.Equal(t, "1", last.userID,
				"X-Nyx-User-Id carries the verified user id (bearerToken mints sub=1)")
			assert.Equal(t, testServiceToken, last.serviceToken,
				"X-Nyx-Service-Token carries AGENT_SERVICE_TOKEN")
			assert.Equal(t, bearer, last.authorization,
				"Authorization forwarded verbatim alongside the service credential")
		})
	}
}

// TestSessionsRoutes_UnauthenticatedRejectedBeforeProxy pins the spec
// scenario "Unauthenticated request is rejected before the proxy":
// the group's auth middleware answers 401 with the standard envelope
// and the agent never sees the call.
func TestSessionsRoutes_UnauthenticatedRejectedBeforeProxy(t *testing.T) {
	srv, up := newSessionUpstream(t, http.StatusOK, `{"sessions":[]}`)
	harness, _ := newSessionsHarness(t, mustAgentProxy(t, srv.URL), nil)
	server := httptest.NewServer(harness)
	t.Cleanup(server.Close)

	req, err := http.NewRequest(http.MethodGet, server.URL+"/api/chat/sessions", nil)
	require.NoError(t, err)

	resp, err := server.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("WWW-Authenticate"), "Bearer")

	var env api.ErrorResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&env))
	assert.Equal(t, http.StatusUnauthorized, env.Code)

	_, calls := up.snapshot()
	assert.Equal(t, 0, calls, "the proxy MUST NOT be reached without a verified user id")
}

// TestSessionsRoutes_AgentDisabled_Returns503 pins the spec scenario
// "Agent disabled responds 503": with AGENT_ENABLED=false there is no
// proxy at all, every session route answers 503 with the standard
// envelope, and no upstream call is attempted.
func TestSessionsRoutes_AgentDisabled_Returns503(t *testing.T) {
	svc := newTestServiceForHandler(&chatStubProvider{}) // legacy mode: no proxy
	router, tokens := buildRouter(t, svc, nil)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	endpoints := []struct{ method, path string }{
		{http.MethodGet, "/api/chat/sessions"},
		{http.MethodGet, "/api/chat/sessions/" + sessionIDFixture + "/messages"},
		{http.MethodPost, "/api/chat/sessions"},
		{http.MethodPost, "/api/chat/sessions/" + sessionIDFixture + "/resume"},
		{http.MethodDelete, "/api/chat/sessions/" + sessionIDFixture},
	}
	for _, ep := range endpoints {
		t.Run(ep.method+" "+ep.path, func(t *testing.T) {
			req, err := http.NewRequest(ep.method, server.URL+ep.path, nil)
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer "+bearerToken(t, tokens))

			resp, err := server.Client().Do(req)
			require.NoError(t, err)
			defer func() { _ = resp.Body.Close() }()

			assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)

			var env api.ErrorResponse
			require.NoError(t, json.NewDecoder(resp.Body).Decode(&env))
			assert.Equal(t, http.StatusServiceUnavailable, env.Code)
			assert.Equal(t, sessionsDisabledMessage, env.Message)
			assert.Equal(t, sessionsDisabledDetail, env.Details,
				"details is the static safe literal explaining the agent is disabled")
			assert.NotEmpty(t, env.RequestID, "the envelope carries the request id for log correlation")
		})
	}
}

// TestSessionsProxy_ForwardBoundedBySessionsTimeout pins the
// non-streaming hops' time bound. Production reuses the AgentProxy's
// SSE client, which carries NO http.Client.Timeout (the 2026-10-14
// incident fix) — without Forward's own context deadline a stalled
// agent body would hold session/approvals requests, their connection,
// and their limiter slots indefinitely. The cap is shrunk so the test
// does not pay 30s of wall clock.
func TestSessionsProxy_ForwardBoundedBySessionsTimeout(t *testing.T) {
	orig := defaultSessionsTimeout
	defaultSessionsTimeout = 50 * time.Millisecond
	t.Cleanup(func() { defaultSessionsTimeout = orig })

	upstreamCanceled := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // accept, then never respond
		close(upstreamCanceled)
	}))
	t.Cleanup(srv.Close)

	// Mirror production wiring: the shared client has no Timeout.
	proxy, err := NewSessionsProxy(srv.URL, testServiceToken, &http.Client{})
	require.NoError(t, err)

	start := time.Now()
	resp, err := proxy.Forward(context.Background(), sessionProxyRequest{
		method: http.MethodGet,
		path:   sessionsBasePath,
		userID: 1,
	})
	if resp != nil {
		_ = resp.Body.Close()
	}
	elapsed := time.Since(start)

	require.Error(t, err, "a stalled upstream must not satisfy the call")
	assert.Less(t, elapsed, 5*time.Second,
		"Forward's context deadline must bound the call, not a client Timeout that no longer exists")
	assert.True(t, errors.Is(err, context.DeadlineExceeded),
		"the bound is a deadline (seen through the proxy's %w wrap); got %v", err)

	select {
	case <-upstreamCanceled:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream request was not canceled when the cap fired")
	}
}

// TestSessionsRoutes_AgentUnreachable_Returns502 pins the spec
// scenario "Agent unreachable fails with 502": a dead upstream yields
// the standard envelope with a STATIC detail — the dial error itself
// never reaches the wire.
func TestSessionsRoutes_AgentUnreachable_Returns502(t *testing.T) {
	// Stand up a server, then close it: the port is dead, so the
	// proxy's dial fails fast with connection refused.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	harness, tokens := newSessionsHarness(t, mustAgentProxy(t, deadURL), nil)
	server := httptest.NewServer(harness)
	t.Cleanup(server.Close)

	req, err := http.NewRequest(http.MethodGet, server.URL+"/api/chat/sessions", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+bearerToken(t, tokens))

	resp, err := server.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusBadGateway, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "application/json")

	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	var env api.ErrorResponse
	require.NoError(t, json.Unmarshal(raw, &env))
	assert.Equal(t, http.StatusBadGateway, env.Code)
	assert.Equal(t, sessionsUnreachableMessage, env.Message)
	assert.Equal(t, sessionsUnreachableDetail, env.Details,
		"details MUST be the static safe literal, never err.Error()")
	assert.NotEmpty(t, env.RequestID)

	// Safety pin: none of the transport-level text may leak.
	for _, leak := range []string{"connection refused", "dial tcp", deadURL, "/agent/sessions"} {
		assert.NotContains(t, string(raw), leak,
			"the 502 body must not carry transport internals")
	}
}

// TestSessionsRoutes_Upstream404ForwardedVerbatim pins the spec
// scenario "Cross-user session is a 404 from the agent, forwarded
// verbatim": the Go layer does not translate, retry, or inspect the
// agent's answer, and does not wrap it in the Go error envelope.
func TestSessionsRoutes_Upstream404ForwardedVerbatim(t *testing.T) {
	const agentBody = `{"detail":"session not found"}`
	srv, _ := newSessionUpstream(t, http.StatusNotFound, agentBody)
	harness, tokens := newSessionsHarness(t, mustAgentProxy(t, srv.URL), nil)
	server := httptest.NewServer(harness)
	t.Cleanup(server.Close)

	req, err := http.NewRequest(http.MethodGet,
		server.URL+"/api/chat/sessions/"+sessionIDFixture+"/messages", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+bearerToken(t, tokens))

	resp, err := server.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	assert.Equal(t, agentBody, string(raw),
		"the agent's 404 body is relayed byte-for-byte")
	assert.NotContains(t, string(raw), `"error"`,
		"the Go envelope must not replace the agent's error shape")
}

// TestSessionsRoutes_Upstream5xxForwardedVerbatim pins the deliberate
// divergence from AgentProxy: on the SSE path a 5xx is collapsed into
// agent_unavailable, but the session contract is verbatim forwarding,
// so the agent's own 500 reaches the client unchanged.
func TestSessionsRoutes_Upstream5xxForwardedVerbatim(t *testing.T) {
	const agentBody = `{"detail":"internal error"}`
	srv, _ := newSessionUpstream(t, http.StatusInternalServerError, agentBody)
	harness, tokens := newSessionsHarness(t, mustAgentProxy(t, srv.URL), nil)
	server := httptest.NewServer(harness)
	t.Cleanup(server.Close)

	req, err := http.NewRequest(http.MethodGet, server.URL+"/api/chat/sessions", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+bearerToken(t, tokens))

	resp, err := server.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	assert.Equal(t, agentBody, string(raw))
}

// TestSessionsRoutes_SessionIDStaysOnePathSegment pins the escaping
// contract: a crafted id can never add a path segment, climb out of
// /agent/sessions, or smuggle a query string — the upstream path
// keeps exactly its three fixed segments.
func TestSessionsRoutes_SessionIDStaysOnePathSegment(t *testing.T) {
	srv, up := newSessionUpstream(t, http.StatusNotFound, `{"detail":"not found"}`)
	harness, tokens := newSessionsHarness(t, mustAgentProxy(t, srv.URL), nil)
	server := httptest.NewServer(harness)
	t.Cleanup(server.Close)

	// "%2F" is an encoded "/" and ".." a traversal segment; both must
	// survive as opaque characters inside the single id segment.
	req, err := http.NewRequest(http.MethodGet,
		server.URL+"/api/chat/sessions/..%2F..%2Fadmin%3Flimit%3D999/messages", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+bearerToken(t, tokens))

	resp, err := server.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	last, calls := up.snapshot()
	require.Equal(t, 1, calls)
	assert.Equal(t, 4, strings.Count(last.requestURI, "/"),
		"the id must not introduce a separator: got %q", last.requestURI)
	assert.NotContains(t, last.path, "/../", "the id must not introduce a traversal segment")
	assert.NotContains(t, last.requestURI, "?limit=999",
		"an encoded query separator inside the id must not become a real query")
}

// TestSessionsRoutes_NotChargedAgainstStreamLimiter pins the limiter
// scoping decision: the per-user chat bucket is charged for
// POST /api/chat (the expensive stream) only. Opening a session view
// costs list + messages + resume + the first stream — charging all of
// those against a burst-3 bucket would 429 the user's first message.
func TestSessionsRoutes_NotChargedAgainstStreamLimiter(t *testing.T) {
	srv, _ := newSessionUpstream(t, http.StatusOK, `{"sessions":[],"next_cursor":null}`)
	limiter := NewUserRateLimiter(0.001, 1) // burst 1 — the second call would 429
	harness, tokens := newSessionsHarness(t, mustAgentProxy(t, srv.URL), limiter)
	server := httptest.NewServer(harness)
	t.Cleanup(server.Close)

	bearer := "Bearer " + bearerToken(t, tokens)
	for i := 0; i < 4; i++ {
		req, err := http.NewRequest(http.MethodGet, server.URL+"/api/chat/sessions", nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", bearer)

		resp, err := server.Client().Do(req)
		require.NoError(t, err)
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		assert.Equal(t, http.StatusOK, resp.StatusCode,
			"session reads must not consume the stream bucket (call %d)", i+1)
	}
}

// TestNewSessionsProxy_RejectsEmptyBaseURL pins the constructor's
// fail-closed contract: an empty PYTHON_AGENT_URL must never produce
// a proxy (the handler answers 503 instead of a misleading 502).
func TestNewSessionsProxy_RejectsEmptyBaseURL(t *testing.T) {
	_, err := NewSessionsProxy("", testServiceToken, nil)
	require.Error(t, err)
}

func TestNewSessionsProxy_RejectsEmptyServiceToken(t *testing.T) {
	_, err := NewSessionsProxy("http://agent.test", "", nil)
	require.Error(t, err)
}

// TestNewSessionsProxy_TrimsTrailingSlash pins URL normalisation:
// operators tend to include the trailing "/" from the compose env
// file, and it must not produce a doubled "//agent/sessions".
func TestNewSessionsProxy_TrimsTrailingSlash(t *testing.T) {
	srv, up := newSessionUpstream(t, http.StatusOK, `{"sessions":[]}`)
	harness, tokens := newSessionsHarness(t, mustAgentProxy(t, srv.URL+"/"), nil)
	server := httptest.NewServer(harness)
	t.Cleanup(server.Close)

	req, err := http.NewRequest(http.MethodGet, server.URL+"/api/chat/sessions", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+bearerToken(t, tokens))

	resp, err := server.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	last, _ := up.snapshot()
	assert.Equal(t, "/agent/sessions", last.path,
		"trailing slash on the base URL must not produce //agent/sessions")
}

// TestSessionsRoutes_DerivedFromAgentProxy pins the wiring contract:
// the session proxy shares the AgentProxy's base URL AND its
// transport, so a custom client (the production 60s client, or a
// test's recorder) is reused rather than silently replaced.
func TestSessionsRoutes_DerivedFromAgentProxy(t *testing.T) {
	var seen atomic.Int32
	srv, _ := newSessionUpstream(t, http.StatusOK, `{"sessions":[]}`)
	proxy, err := NewAgentProxy(srv.URL, testServiceToken, &http.Client{Transport: roundTripRecorder{seen: &seen}})
	require.NoError(t, err)

	harness, tokens := newSessionsHarness(t, proxy, nil)
	server := httptest.NewServer(harness)
	t.Cleanup(server.Close)

	req, err := http.NewRequest(http.MethodGet, server.URL+"/api/chat/sessions", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+bearerToken(t, tokens))

	resp, err := server.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, int32(1), seen.Load(),
		"the session proxy must dial through the AgentProxy's transport")
}

// roundTripRecorder counts every request that passes through the
// supplied http.Client, proving which client the session proxy used.
type roundTripRecorder struct {
	seen *atomic.Int32
}

func (r roundTripRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	r.seen.Add(1)
	return http.DefaultTransport.RoundTrip(req)
}
