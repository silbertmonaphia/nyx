package chat

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace/noop"

	"nyx/internal/middleware"
	"nyx/internal/platform/api"
	"nyx/internal/platform/auth"
	"nyx/internal/platform/config"
)

// Fixtures for the decision payload the SPA posts. Both ids are
// uuid4-shaped because the agent owns their validation — the Go layer
// must not care either way (see the opaque-body case below).
const (
	approvalRunIDFixture       = "2a3b4c5d-6e7f-4a8b-9c0d-1e2f3a4b5c6d"
	approvalIDFixture          = "9f8e7d6c-5b4a-4392-8170-6f5e4d3c2b1a"
	approvalApproveBodyFixture = `{"run_id":"` + approvalRunIDFixture + `","approval_id":"` + approvalIDFixture + `","decision":"approve"}`
)

// newApprovalsHarness mirrors cmd/api/main.go's registration order and
// middleware posture: the chat route group (stream + session routes)
// lands first, then the approvals route on the same router with its
// own per-user limiter. Individual tests pass a limiter suited to what
// they pin (nil disables the bucket, as in the session tests).
func newApprovalsHarness(t *testing.T, proxy *AgentProxy, approvalsLimiter *UserRateLimiter) (http.Handler, auth.TokenService) {
	t.Helper()
	cfg := &config.Config{
		LLMMaxHistoryMessages: 5,
		LLMMaxMessageChars:    50,
		LLMMaxTokens:          100,
	}
	tracer := noop.NewTracerProvider().Tracer("test")
	svc := NewServiceWithProxy(&llmProviderUnused{}, cfg, tracer, proxy)

	tokens, err := auth.NewTokenService([]byte(auth.TestSecret), 15*time.Minute)
	require.NoError(t, err)

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.StoreRequest)
	h := NewHandler(svc)
	RegisterChatRoute(r, h, tokens, NewUserRateLimiter(5.0/60.0, 3))
	RegisterApprovalsRoute(r, h, tokens, approvalsLimiter)
	return r, tokens
}

// newLegacyApprovalsHarness builds the AGENT_ENABLED=false shape: a
// service without an agent proxy, so the handler's disabled gate is
// the only thing between the client and a decision.
func newLegacyApprovalsHarness(t *testing.T) (http.Handler, auth.TokenService) {
	t.Helper()
	svc := newTestServiceForHandler(&chatStubProvider{}) // legacy mode: no proxy
	tokens, err := auth.NewTokenService([]byte(auth.TestSecret), 15*time.Minute)
	require.NoError(t, err)

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.StoreRequest)
	h := NewHandler(svc)
	RegisterChatRoute(r, h, tokens, nil)
	RegisterApprovalsRoute(r, h, tokens, nil)
	return r, tokens
}

// postApproval drives one decision request against the assembled
// router and returns the status, response headers, and fully-read
// body. The body is closed before returning, so callers cannot leak
// it (and bodyclose stays quiet without nolint sprinkles).
func postApproval(t *testing.T, server *httptest.Server, bearer, body string) (int, http.Header, []byte) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = bytes.NewReader([]byte(body))
	}
	req, err := http.NewRequest(http.MethodPost, server.URL+"/api/chat/approvals", reader)
	require.NoError(t, err)
	if bearer != "" {
		req.Header.Set("Authorization", bearer)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := server.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, resp.Header, raw
}

// captureApprovalLogs swaps the global zerolog logger for one writing
// into the returned buffer; t.Cleanup restores the previous logger.
// Mirrors internal/middleware's captureLogger. Tests that use this
// must NOT call t.Parallel() — the buffer races the global logger.
func captureApprovalLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := log.Logger
	log.Logger = zerolog.New(buf).Level(zerolog.WarnLevel)
	t.Cleanup(func() { log.Logger = prev })
	return buf
}

// TestApprovals_DecisionForwardedVerbatim is the spec scenario
// "Authenticated decision is forwarded": the client's status + body
// come back unchanged, and the upstream call carries the mapped path,
// the verified user id, the bearer passthrough, and the decision body
// untouched. The opaque-body case pins the "handler MUST NOT interpret
// or shortcut a decision" clause directly: Go never decodes the body,
// so even a non-JSON payload is forwarded (and the agent owns
// rejecting it).
func TestApprovals_DecisionForwardedVerbatim(t *testing.T) {
	cases := []struct {
		name           string
		body           string
		upstreamStatus int
		upstreamBody   string
	}{
		{
			name:           "approve decision",
			body:           approvalApproveBodyFixture,
			upstreamStatus: http.StatusOK,
			upstreamBody:   `{"status":"recorded","decision":"approve"}`,
		},
		{
			name:           "deny decision is not special-cased",
			body:           `{"run_id":"` + approvalRunIDFixture + `","approval_id":"` + approvalIDFixture + `","decision":"deny"}`,
			upstreamStatus: http.StatusOK,
			upstreamBody:   `{"status":"recorded","decision":"deny"}`,
		},
		{
			name:           "opaque body is forwarded, never decoded",
			body:           `definitely-not-json`,
			upstreamStatus: http.StatusBadRequest,
			upstreamBody:   `{"detail":"invalid body"}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, up := newSessionUpstream(t, tc.upstreamStatus, tc.upstreamBody)
			harness, tokens := newApprovalsHarness(t, mustAgentProxy(t, srv.URL), nil)
			server := httptest.NewServer(harness)
			t.Cleanup(server.Close)

			bearer := "Bearer " + bearerToken(t, tokens)
			status, hdr, raw := postApproval(t, server, bearer, tc.body)

			// Client side: the agent's answer, byte-for-byte.
			assert.Equal(t, tc.upstreamStatus, status)
			assert.Equal(t, "application/json", hdr.Get("Content-Type"))
			assert.Equal(t, tc.upstreamBody, string(raw))

			// Agent side: the mapped call. The recorder is the shared
			// session-upstream stub (the same wrapper shape serves both
			// JSON hops).
			last, calls := up.snapshot()
			require.Equal(t, 1, calls, "exactly one upstream call per decision")
			assert.Equal(t, http.MethodPost, last.method)
			assert.Equal(t, approvalsPath, last.path,
				"the decision lands on /agent/approvals")
			assert.Equal(t, tc.body, last.body,
				"the body is forwarded verbatim — the Go layer never rewrites a decision")
			assert.Equal(t, "application/json", last.contentType)
			assert.Equal(t, "1", last.userID,
				"X-Nyx-User-Id carries the verified user id (bearerToken mints sub=1)")
			assert.Equal(t, testServiceToken, last.serviceToken,
				"X-Nyx-Service-Token carries AGENT_SERVICE_TOKEN")
			assert.Equal(t, bearer, last.authorization,
				"Authorization is a passthrough alongside the service credential")
		})
	}
}

// TestApprovals_UnauthenticatedRejectedBeforeForwarding pins the spec
// scenario "Missing or invalid JWT is rejected at the edge": the
// route's auth middleware answers the standard 401 envelope and the
// agent never sees the decision.
func TestApprovals_UnauthenticatedRejectedBeforeForwarding(t *testing.T) {
	cases := []struct {
		name   string
		bearer string
	}{
		{name: "no bearer", bearer: ""},
		{name: "garbage bearer", bearer: "Bearer not-a-jwt"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, up := newSessionUpstream(t, http.StatusOK, `{"status":"recorded"}`)
			harness, _ := newApprovalsHarness(t, mustAgentProxy(t, srv.URL), nil)
			server := httptest.NewServer(harness)
			t.Cleanup(server.Close)

			status, hdr, raw := postApproval(t, server, tc.bearer, approvalApproveBodyFixture)

			assert.Equal(t, http.StatusUnauthorized, status)
			assert.Contains(t, hdr.Get("WWW-Authenticate"), "Bearer")

			var env api.ErrorResponse
			require.NoError(t, json.Unmarshal(raw, &env))
			assert.Equal(t, http.StatusUnauthorized, env.Code)

			_, calls := up.snapshot()
			assert.Equal(t, 0, calls,
				"an unauthenticated decision MUST NOT reach the Python service")
		})
	}
}

// TestApprovals_UpstreamStatusAndBodyPassthrough pins the relay for the
// agent's own error shapes: a typed 404 (unknown / expired / foreign
// approval id) and a 500 both arrive unchanged, never wrapped in the
// Go envelope and never remapped to a success.
func TestApprovals_UpstreamStatusAndBodyPassthrough(t *testing.T) {
	cases := []struct {
		name           string
		upstreamStatus int
		upstreamBody   string
	}{
		{
			name:           "typed 404 for unknown or expired approval",
			upstreamStatus: http.StatusNotFound,
			upstreamBody:   `{"detail":"approval not found"}`,
		},
		{
			name:           "upstream 500 is relayed, not swallowed",
			upstreamStatus: http.StatusInternalServerError,
			upstreamBody:   `{"detail":"broker unavailable"}`,
		},
		{
			name:           "204 relayed with no body",
			upstreamStatus: http.StatusNoContent,
			upstreamBody:   "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := newSessionUpstream(t, tc.upstreamStatus, tc.upstreamBody)
			harness, tokens := newApprovalsHarness(t, mustAgentProxy(t, srv.URL), nil)
			server := httptest.NewServer(harness)
			t.Cleanup(server.Close)

			status, _, raw := postApproval(t, server, "Bearer "+bearerToken(t, tokens), approvalApproveBodyFixture)

			assert.Equal(t, tc.upstreamStatus, status,
				"the upstream status is relayed verbatim")
			assert.Equal(t, tc.upstreamBody, string(raw),
				"the upstream body is relayed byte-for-byte")
			assert.NotContains(t, string(raw), `"code"`,
				"the Go error envelope must not replace the agent's shape")
		})
	}
}

// TestApprovals_AgentDisabled_FailsClosed pins the spec scenario
// "Agent unreachable fails closed" for the disabled configuration
// (AGENT_ENABLED=false): the route answers 503 with the static
// envelope, and nothing that looks like a recorded decision is ever
// written.
func TestApprovals_AgentDisabled_FailsClosed(t *testing.T) {
	harness, tokens := newLegacyApprovalsHarness(t)
	server := httptest.NewServer(harness)
	t.Cleanup(server.Close)

	status, _, raw := postApproval(t, server, "Bearer "+bearerToken(t, tokens), approvalApproveBodyFixture)

	assert.Equal(t, http.StatusServiceUnavailable, status)

	var env api.ErrorResponse
	require.NoError(t, json.Unmarshal(raw, &env))
	assert.Equal(t, http.StatusServiceUnavailable, env.Code)
	assert.Equal(t, approvalsDisabledMessage, env.Message)
	assert.Equal(t, approvalsDisabledDetail, env.Details)
	assert.NotEmpty(t, env.RequestID)
	assert.NotContains(t, string(raw), `"status"`,
		"no implicit success: the response is the error envelope, not a decision ack")
}

// TestApprovals_AgentUnreachable_FailsClosed pins the spec scenario
// "Agent unreachable fails closed" for a dead upstream: 502 with a
// STATIC safe detail (the dial error never reaches the wire) and no
// fallback path.
func TestApprovals_AgentUnreachable_FailsClosed(t *testing.T) {
	// Stand up a server, then close it: the port is dead, so the
	// proxy's dial fails fast with connection refused.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	harness, tokens := newApprovalsHarness(t, mustAgentProxy(t, deadURL), nil)
	server := httptest.NewServer(harness)
	t.Cleanup(server.Close)

	status, hdr, raw := postApproval(t, server, "Bearer "+bearerToken(t, tokens), approvalApproveBodyFixture)

	assert.Equal(t, http.StatusBadGateway, status)
	assert.Contains(t, hdr.Get("Content-Type"), "application/json")

	var env api.ErrorResponse
	require.NoError(t, json.Unmarshal(raw, &env))
	assert.Equal(t, http.StatusBadGateway, env.Code)
	assert.Equal(t, approvalsUnreachableMessage, env.Message)
	assert.Equal(t, approvalsUnreachableDetail, env.Details,
		"details MUST be the static safe literal, never err.Error()")
	assert.NotEmpty(t, env.RequestID)

	// Safety pin: none of the transport-level text may leak.
	for _, leak := range []string{"connection refused", "dial tcp", deadURL, approvalsPath} {
		assert.NotContains(t, string(raw), leak,
			"the 502 body must not carry transport internals")
	}
}

// TestApprovals_RateLimitedPerUser pins the design's abuse guard
// (D7 / Risk "Approval endpoint abuse"): decisions are charged against
// a per-user bucket, an exhausted bucket is a 429 that never reaches
// the agent, and a different user is unaffected.
func TestApprovals_RateLimitedPerUser(t *testing.T) {
	srv, up := newSessionUpstream(t, http.StatusOK, `{"status":"recorded"}`)
	limiter := NewUserRateLimiter(0.001, 1) // burst 1 — the second call would 429
	harness, tokens := newApprovalsHarness(t, mustAgentProxy(t, srv.URL), limiter)
	server := httptest.NewServer(harness)
	t.Cleanup(server.Close)

	user1 := "Bearer " + bearerToken(t, tokens) // sub=1
	user2Token, err := tokens.GenerateToken(2, "other")
	require.NoError(t, err)
	user2 := "Bearer " + user2Token

	status, _, _ := postApproval(t, server, user1, approvalApproveBodyFixture)
	assert.Equal(t, http.StatusOK, status)

	status, hdr, _ := postApproval(t, server, user1, approvalApproveBodyFixture)
	assert.Equal(t, http.StatusTooManyRequests, status)
	assert.NotEmpty(t, hdr.Get("Retry-After"))

	_, calls := up.snapshot()
	assert.Equal(t, 1, calls, "a 429ed decision must never be forwarded upstream")

	status, _, _ = postApproval(t, server, user2, approvalApproveBodyFixture)
	assert.Equal(t, http.StatusOK, status,
		"the bucket is per-user: another user's decision is unaffected")
	_, calls = up.snapshot()
	assert.Equal(t, 2, calls)
}

// TestApprovals_TokenNeverLogged pins the spec scenario "Token value
// never logged": with the global logger captured, both the fail-closed
// path (which does log — via api.ClassifyAndLog) and the happy path
// are driven, and the bearer token string appears in no log line.
func TestApprovals_TokenNeverLogged(t *testing.T) {
	buf := captureApprovalLogs(t)

	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	deadHarness, deadTokens := newApprovalsHarness(t, mustAgentProxy(t, deadURL), nil)
	deadServer := httptest.NewServer(deadHarness)
	t.Cleanup(deadServer.Close)

	failingToken := bearerToken(t, deadTokens)
	status, _, _ := postApproval(t, deadServer, "Bearer "+failingToken, approvalApproveBodyFixture)
	require.Equal(t, http.StatusBadGateway, status)
	require.NotEmpty(t, buf.String(),
		"the fail-closed path must log the suppressed error for operators")

	srv, _ := newSessionUpstream(t, http.StatusOK, `{"status":"recorded"}`)
	harness, tokens := newApprovalsHarness(t, mustAgentProxy(t, srv.URL), nil)
	server := httptest.NewServer(harness)
	t.Cleanup(server.Close)

	happyToken := bearerToken(t, tokens)
	status, _, _ = postApproval(t, server, "Bearer "+happyToken, approvalApproveBodyFixture)
	require.Equal(t, http.StatusOK, status)

	logged := buf.String()
	for _, secret := range []string{failingToken, happyToken, "Bearer " + failingToken, "Bearer " + happyToken} {
		assert.NotContains(t, logged, secret,
			"the bearer token value must never appear in a log line")
	}
	assert.Contains(t, logged, approvalsPath,
		"sanity: the captured log is the approvals hop, so the absence assertion above is meaningful")
}

// TestApprovals_ChatRoutesStillRouted pins the coexistence of the two
// registrations on one router (main.go registers RegisterChatRoute
// then RegisterApprovalsRoute): the static /api/chat/approvals route
// is not swallowed by the /api/chat subrouter's /* mount, and the
// session routes keep working.
func TestApprovals_ChatRoutesStillRouted(t *testing.T) {
	srv, up := newSessionUpstream(t, http.StatusOK, `{"sessions":[]}`)
	harness, tokens := newApprovalsHarness(t, mustAgentProxy(t, srv.URL), nil)
	server := httptest.NewServer(harness)
	t.Cleanup(server.Close)

	bearer := "Bearer " + bearerToken(t, tokens)

	sessionReq, err := http.NewRequest(http.MethodGet, server.URL+"/api/chat/sessions", nil)
	require.NoError(t, err)
	sessionReq.Header.Set("Authorization", bearer)
	sessionResp, err := server.Client().Do(sessionReq)
	require.NoError(t, err)
	sessionRaw, err := io.ReadAll(sessionResp.Body)
	require.NoError(t, err)
	_ = sessionResp.Body.Close()
	assert.Equal(t, http.StatusOK, sessionResp.StatusCode)
	assert.Equal(t, `{"sessions":[]}`, string(sessionRaw))
	last, _ := up.snapshot()
	assert.Equal(t, sessionsBasePath, last.path, "the session route still resolves")

	status, _, _ := postApproval(t, server, bearer, approvalApproveBodyFixture)
	assert.Equal(t, http.StatusOK, status)
	last, calls := up.snapshot()
	require.Equal(t, 2, calls)
	assert.Equal(t, approvalsPath, last.path,
		"the approvals route resolves on the same router, past the /api/chat mount")
}

// TestRegisterApprovalsRoute_PanicsOnNilHandler mirrors
// RegisterChatRoute's programmer-error contract: a nil handler is a
// wiring bug and panics at mount time rather than nil-dereferencing
// on the first request.
func TestRegisterApprovalsRoute_PanicsOnNilHandler(t *testing.T) {
	tokens, err := auth.NewTokenService([]byte(auth.TestSecret), 15*time.Minute)
	require.NoError(t, err)
	assert.Panics(t, func() {
		RegisterApprovalsRoute(chi.NewRouter(), nil, tokens, nil)
	})
}

// TestApprovals_LegacyHandlerHasNoForwarder pins the disabled gate's
// mechanism: a legacy service (no agent proxy) leaves the derived
// forwarder nil, which is what the handler's 503 branch keys on.
func TestApprovals_LegacyHandlerHasNoForwarder(t *testing.T) {
	svc := newTestServiceForHandler(&chatStubProvider{})
	h := NewHandler(svc)
	assert.Nil(t, h.sessions,
		"a legacy service must leave the approvals forwarder nil — the fail-closed gate")
}

func TestApprovals_AgentMisconfigured_FailsClosed(t *testing.T) {
	svc := newMisconfiguredServiceForHandler(&llmProviderUnused{})
	tokens, err := auth.NewTokenService([]byte(auth.TestSecret), 15*time.Minute)
	require.NoError(t, err)

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.StoreRequest)
	h := NewHandler(svc)
	RegisterChatRoute(r, h, tokens, nil)
	RegisterApprovalsRoute(r, h, tokens, nil)
	server := httptest.NewServer(r)
	t.Cleanup(server.Close)

	status, _, raw := postApproval(t, server, "Bearer "+bearerToken(t, tokens), approvalApproveBodyFixture)

	assert.Equal(t, http.StatusServiceUnavailable, status)
	var env api.ErrorResponse
	require.NoError(t, json.Unmarshal(raw, &env))
	assert.Equal(t, approvalsDisabledMessage, env.Message)
	assert.Equal(t, agentMisconfiguredDetail, env.Details)
}
