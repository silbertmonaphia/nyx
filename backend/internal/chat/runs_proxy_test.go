package chat

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace/noop"

	"nyx/internal/platform/api"
	"nyx/internal/platform/auth"
	"nyx/internal/platform/config"
)

const reattachRunID = "7e6a2c30-9b1f-4d3e-8a12-5c4b9f0e11a2"

const reattachFixture = "event: note\n" +
	`data: {"kind":"reattached","run_id":"` + reattachRunID + `"}` + "\n\n" +
	": keep-alive\n\n" +
	"event: delta\n" +
	`data: {"delta":"hi"}` + "\n\n"

type capturedRunsRequest struct {
	method        string
	requestURI    string
	path          string
	rawQuery      string
	authorization string
	userID        string
	serviceToken  string
	contentType   string
	accept        string
	body          string
}

type runsUpstream struct {
	mu    sync.Mutex
	calls int
	last  capturedRunsRequest
}

func (u *runsUpstream) snapshot() (capturedRunsRequest, int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.last, u.calls
}

func newRunsUpstreamFunc(t *testing.T, respond func(http.ResponseWriter, *http.Request)) (*httptest.Server, *runsUpstream) {
	t.Helper()
	up := &runsUpstream{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf, _ := io.ReadAll(r.Body)
		up.mu.Lock()
		up.calls++
		up.last = capturedRunsRequest{
			method:        r.Method,
			requestURI:    r.RequestURI,
			path:          r.URL.Path,
			rawQuery:      r.URL.RawQuery,
			authorization: r.Header.Get("Authorization"),
			userID:        r.Header.Get("X-Nyx-User-Id"),
			serviceToken:  r.Header.Get(ServiceTokenHeader),
			contentType:   r.Header.Get("Content-Type"),
			accept:        r.Header.Get("Accept"),
			body:          string(buf),
		}
		up.mu.Unlock()
		respond(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, up
}

func newRunsUpstream(t *testing.T, status int, body string, ct string) (*httptest.Server, *runsUpstream) {
	t.Helper()
	return newRunsUpstreamFunc(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", ct)
		w.WriteHeader(status)
		if body != "" {
			_, _ = io.WriteString(w, body)
		}
	})
}

func newRunsHarness(t *testing.T, proxy *AgentProxy, streamCap time.Duration, limiter *UserRateLimiter) (http.Handler, auth.TokenService) {
	t.Helper()
	cfg := &config.Config{
		LLMMaxHistoryMessages: 5,
		LLMMaxMessageChars:    50,
		LLMMaxTokens:          100,
	}
	if streamCap > 0 {
		cfg.LLMMaxStreamDuration = streamCap.String()
	}
	tracer := noop.NewTracerProvider().Tracer("test")
	svc := NewServiceWithProxy(&llmProviderUnused{}, cfg, tracer, proxy)
	return buildRouter(t, svc, limiter)
}

func doRunRequest(t *testing.T, server *httptest.Server, method, path, bearer string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, server.URL+path, nil)
	require.NoError(t, err)
	if bearer != "" {
		req.Header.Set("Authorization", bearer)
	}
	resp, err := server.Client().Do(req)
	require.NoError(t, err)
	return resp
}

func drainAndClose(t *testing.T, resp *http.Response) string {
	t.Helper()
	got, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	_ = resp.Body.Close()
	return string(got)
}

func TestRunsStream_Reattach_ForwardsBytesAndHeaders(t *testing.T) {
	srv, up := newRunsUpstream(t, http.StatusOK, reattachFixture, "text/event-stream; charset=utf-8")
	harness, tokens := newRunsHarness(t, mustAgentProxy(t, srv.URL), 0, nil)
	server := httptest.NewServer(harness)
	t.Cleanup(server.Close)

	bearer := "Bearer " + bearerToken(t, tokens)
	resp := doRunRequest(t, server, http.MethodGet, "/api/chat/runs/"+reattachRunID+"/stream", bearer)
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "text/event-stream; charset=utf-8", resp.Header.Get("Content-Type"))
	assert.Equal(t, "no-cache", resp.Header.Get("Cache-Control"))
	assert.Equal(t, "keep-alive", resp.Header.Get("Connection"))
	assert.Equal(t, "no", resp.Header.Get("X-Accel-Buffering"))

	got := drainAndClose(t, resp)
	assert.Equal(t, reattachFixture, got,
		"the re-attach stream must be forwarded byte-for-byte, including the : keep-alive comment frame")
	assert.Contains(t, got, ": keep-alive\n\n",
		"the comment frame must survive verbatim, not be parsed away or dropped")

	last, calls := up.snapshot()
	require.Equal(t, 1, calls)
	assert.Equal(t, http.MethodGet, last.method)
	assert.Equal(t, "/agent/runs/"+reattachRunID+"/stream", last.path)
	assert.Equal(t, "1", last.userID,
		"X-Nyx-User-Id carries the verified user id (bearerToken mints sub=1)")
	assert.Equal(t, testServiceToken, last.serviceToken,
		"X-Nyx-Service-Token carries AGENT_SERVICE_TOKEN")
	assert.Equal(t, bearer, last.authorization,
		"Authorization forwarded verbatim alongside the service credential")
	assert.Equal(t, "text/event-stream", last.accept)
	assert.Empty(t, last.contentType, "a body-less GET carries no content type")
	assert.Empty(t, last.body, "the re-attach GET carries no body")
}

func TestRunsStream_CapCutMidStream_SynthesizesTerminalTrio(t *testing.T) {
	upstreamCanceled := make(chan struct{})
	srv, _ := newRunsUpstreamFunc(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "event: note\ndata: {\"kind\":\"reattached\"}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		for {
			if r.Context().Err() != nil {
				close(upstreamCanceled)
				return
			}
			_, _ = io.WriteString(w, "event: delta\ndata: {\"delta\":\"x\"}\n\n")
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			select {
			case <-r.Context().Done():
				close(upstreamCanceled)
				return
			case <-time.After(40 * time.Millisecond):
			}
		}
	})

	harness, tokens := newRunsHarness(t, mustAgentProxy(t, srv.URL), 200*time.Millisecond, nil)
	server := httptest.NewServer(harness)
	t.Cleanup(server.Close)

	bearer := "Bearer " + bearerToken(t, tokens)
	resp := doRunRequest(t, server, http.MethodGet, "/api/chat/runs/"+reattachRunID+"/stream", bearer)
	defer func() { _ = resp.Body.Close() }()

	start := time.Now()
	got := drainAndClose(t, resp)
	assert.Less(t, time.Since(start), 5*time.Second,
		"the cap must cut the re-attach stream promptly")

	frames := readSSEFrames(t, strings.NewReader(got))
	require.NotEmpty(t, frames)
	require.GreaterOrEqual(t, len(frames), 4)

	last := frames[len(frames)-1]
	require.Equal(t, eventNameTerminator, last.event)
	var term struct {
		Reason string `json:"reason"`
	}
	require.NoError(t, json.Unmarshal([]byte(last.data), &term))
	assert.Equal(t, "stream_timeout", term.Reason)
	assert.Equal(t, eventNameDone, frames[len(frames)-2].event)

	var sawDelta, sawError bool
	errFrameIdx := -1
	for i, f := range frames[:len(frames)-2] {
		switch f.event {
		case eventNameDelta:
			sawDelta = true
		case eventNameError:
			sawError = true
			errFrameIdx = i
		}
	}
	require.True(t, sawDelta, "frames forwarded before the cut must reach the client; got %s", got)
	require.True(t, sawError, "the cut must surface as a typed error frame; got %s", got)
	assert.Contains(t, frames[errFrameIdx].data, `"code":"stream_timeout"`)
	assert.Contains(t, frames[errFrameIdx].data, `"error":"Agent stream exceeded the time limit"`)

	select {
	case <-upstreamCanceled:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream request was not canceled when the cap cut the stream")
	}
}

func TestRunsStream_CapAfterUpstreamTerminal_AppendsNothing(t *testing.T) {
	upstreamScript := "event: delta\n" +
		`data: {"delta":"hi"}` + "\n\n" +
		"event: done\n" +
		"data: {}\n\n" +
		"event: " + eventNameTerminator + "\n" +
		`data: {"reason":""}` + "\n\n"

	srv, _ := newRunsUpstreamFunc(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, upstreamScript)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	})

	harness, tokens := newRunsHarness(t, mustAgentProxy(t, srv.URL), 200*time.Millisecond, nil)
	server := httptest.NewServer(harness)
	t.Cleanup(server.Close)

	bearer := "Bearer " + bearerToken(t, tokens)
	resp := doRunRequest(t, server, http.MethodGet, "/api/chat/runs/"+reattachRunID+"/stream", bearer)
	defer func() { _ = resp.Body.Close() }()

	got := drainAndClose(t, resp)
	assert.Equal(t, upstreamScript, got,
		"nothing may be appended after the upstream's own terminal frames")
	assert.NotContains(t, got, "stream_timeout",
		"a run that closed its own stream is not a timeout")
}

func TestRunsStream_AgentDisabled_Returns503(t *testing.T) {
	svc := newTestServiceForHandler(&chatStubProvider{})
	router, tokens := buildRouter(t, svc, nil)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	resp := doRunRequest(t, server, http.MethodGet, "/api/chat/runs/"+reattachRunID+"/stream",
		"Bearer "+bearerToken(t, tokens))
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "application/json")
	assert.Empty(t, resp.Header.Get("X-Accel-Buffering"),
		"the SSE envelope must never open on a 503")

	var env api.ErrorResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&env))
	assert.Equal(t, runStreamDisabledMessage, env.Message)
	assert.Equal(t, runStreamDisabledDetail, env.Details,
		"details is the static safe literal explaining the agent is disabled")
	assert.NotEmpty(t, env.RequestID)
}

func TestRunsStream_AgentMisconfigured_Returns503(t *testing.T) {
	svc := newMisconfiguredServiceForHandler(&chatStubProvider{})
	router, tokens := buildRouter(t, svc, nil)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	resp := doRunRequest(t, server, http.MethodGet, "/api/chat/runs/"+reattachRunID+"/stream",
		"Bearer "+bearerToken(t, tokens))
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	var env api.ErrorResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&env))
	assert.Equal(t, runStreamDisabledMessage, env.Message)
	assert.Equal(t, agentMisconfiguredDetail, env.Details,
		"details names the misconfiguration instead of claiming the agent is disabled")
}

func TestRunsStream_AgentUnreachable_Returns502(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	harness, tokens := newRunsHarness(t, mustAgentProxy(t, deadURL), 0, nil)
	server := httptest.NewServer(harness)
	t.Cleanup(server.Close)

	resp := doRunRequest(t, server, http.MethodGet, "/api/chat/runs/"+reattachRunID+"/stream",
		"Bearer "+bearerToken(t, tokens))
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusBadGateway, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "application/json")
	assert.Empty(t, resp.Header.Get("X-Accel-Buffering"),
		"the stream never opened, so the failure is a JSON envelope, not an SSE frame")

	raw := drainAndClose(t, resp)
	var env api.ErrorResponse
	require.NoError(t, json.Unmarshal([]byte(raw), &env))
	assert.Equal(t, http.StatusBadGateway, env.Code)
	assert.Equal(t, runStreamUnreachableMessage, env.Message)
	assert.Equal(t, runStreamUnreachableDetail, env.Details,
		"details MUST be the static safe literal, never err.Error()")

	for _, leak := range []string{"connection refused", "dial tcp", deadURL, "/agent/runs"} {
		assert.NotContains(t, raw, leak,
			"the 502 body must not carry transport internals")
	}
}

func TestRunsStream_Non5xxStatuses_ForwardedVerbatim(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{
			name:   "409 run_not_live sentinel",
			status: http.StatusConflict,
			body:   `{"detail":{"code":"run_not_live","message":"run is not live"}}`,
		},
		{
			name:   "501 reattach_unavailable",
			status: http.StatusNotImplemented,
			body:   `{"detail":{"code":"reattach_unavailable","message":"persistence is disabled"}}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := newRunsUpstream(t, tc.status, tc.body, "application/json")
			harness, tokens := newRunsHarness(t, mustAgentProxy(t, srv.URL), 0, nil)
			server := httptest.NewServer(harness)
			t.Cleanup(server.Close)

			resp := doRunRequest(t, server, http.MethodGet, "/api/chat/runs/"+reattachRunID+"/stream",
				"Bearer "+bearerToken(t, tokens))
			defer func() { _ = resp.Body.Close() }()

			assert.Equal(t, tc.status, resp.StatusCode,
				"the agent's typed status reaches the client unchanged")
			assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))
			assert.Empty(t, resp.Header.Get("X-Accel-Buffering"),
				"the SSE envelope must never open for a forwarded error status")

			got := drainAndClose(t, resp)
			assert.Equal(t, tc.body, got,
				"the agent's envelope is relayed byte-for-byte")
			assert.NotContains(t, got, `"error":`,
				"the Go envelope must not replace the agent's error shape")
		})
	}
}

func TestRunRoutes_Unauthenticated_RejectedBeforeOutbound(t *testing.T) {
	srv, up := newRunsUpstream(t, http.StatusOK, reattachFixture, "text/event-stream; charset=utf-8")
	harness, _ := newRunsHarness(t, mustAgentProxy(t, srv.URL), 0, nil)
	server := httptest.NewServer(harness)
	t.Cleanup(server.Close)

	endpoints := []struct{ method, path string }{
		{http.MethodGet, "/api/chat/runs/" + reattachRunID + "/stream"},
		{http.MethodPost, "/api/chat/runs/" + reattachRunID + "/cancel"},
	}
	for _, ep := range endpoints {
		t.Run(ep.method+" "+ep.path, func(t *testing.T) {
			resp := doRunRequest(t, server, ep.method, ep.path, "")
			defer func() { _ = resp.Body.Close() }()

			assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
			assert.Contains(t, resp.Header.Get("WWW-Authenticate"), "Bearer")

			var env api.ErrorResponse
			require.NoError(t, json.NewDecoder(resp.Body).Decode(&env))
			assert.Equal(t, http.StatusUnauthorized, env.Code)

			_, calls := up.snapshot()
			assert.Equal(t, 0, calls,
				"the proxy MUST NOT be reached without a verified user id")
		})
	}
}

func TestRunsStream_ChargedAgainstStreamLimiter_CancelIsNot(t *testing.T) {
	srv, up := newRunsUpstreamFunc(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/stream") {
			w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, reattachFixture)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"detail":{"cancelled":true}}`)
	})

	limiter := NewUserRateLimiter(0.001, 1)
	harness, tokens := newRunsHarness(t, mustAgentProxy(t, srv.URL), 0, limiter)
	server := httptest.NewServer(harness)
	t.Cleanup(server.Close)

	bearer := "Bearer " + bearerToken(t, tokens)

	resp1 := doRunRequest(t, server, http.MethodGet, "/api/chat/runs/"+reattachRunID+"/stream", bearer)
	assert.Equal(t, http.StatusOK, resp1.StatusCode)
	_ = drainAndClose(t, resp1)

	resp2 := doRunRequest(t, server, http.MethodGet, "/api/chat/runs/"+reattachRunID+"/stream", bearer)
	assert.Equal(t, http.StatusTooManyRequests, resp2.StatusCode,
		"the re-attach shares the POST /api/chat stream bucket")
	_ = drainAndClose(t, resp2)

	resp3 := doRunRequest(t, server, http.MethodPost, "/api/chat/runs/"+reattachRunID+"/cancel", bearer)
	assert.Equal(t, http.StatusOK, resp3.StatusCode,
		"cancel is a cheap sessions-class call and must not be 429ed by the empty stream bucket")
	_ = drainAndClose(t, resp3)

	_, calls := up.snapshot()
	assert.Equal(t, 2, calls,
		"the 429 never reached the upstream: one stream open + one cancel")
}

func TestRunsCancel_ForwardsHeadersAndRelaysVerbatim(t *testing.T) {
	cases := []struct {
		name           string
		upstreamStatus int
		upstreamBody   string
	}{
		{
			name:           "200 cancelled",
			upstreamStatus: http.StatusOK,
			upstreamBody:   `{"detail":{"cancelled":true}}`,
		},
		{
			name:           "409 run_not_live sentinel forwarded verbatim",
			upstreamStatus: http.StatusConflict,
			upstreamBody:   `{"detail":{"code":"run_not_live","message":"run is not live"}}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, up := newRunsUpstream(t, tc.upstreamStatus, tc.upstreamBody, "application/json")
			harness, tokens := newRunsHarness(t, mustAgentProxy(t, srv.URL), 0, nil)
			server := httptest.NewServer(harness)
			t.Cleanup(server.Close)

			bearer := "Bearer " + bearerToken(t, tokens)
			resp := doRunRequest(t, server, http.MethodPost, "/api/chat/runs/"+reattachRunID+"/cancel", bearer)
			defer func() { _ = resp.Body.Close() }()

			assert.Equal(t, tc.upstreamStatus, resp.StatusCode)
			assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))
			got := drainAndClose(t, resp)
			assert.Equal(t, tc.upstreamBody, got,
				"the agent's answer is relayed byte-for-byte")

			last, calls := up.snapshot()
			require.Equal(t, 1, calls)
			assert.Equal(t, http.MethodPost, last.method)
			assert.Equal(t, "/agent/runs/"+reattachRunID+"/cancel", last.path)
			assert.Equal(t, "1", last.userID)
			assert.Equal(t, testServiceToken, last.serviceToken)
			assert.Equal(t, bearer, last.authorization,
				"Authorization forwarded verbatim alongside the service credential")
			assert.Equal(t, "application/json", last.contentType)
			assert.Empty(t, last.body, "cancel needs no request body")
		})
	}
}

func TestRunsCancel_AgentStates(t *testing.T) {
	t.Run("disabled returns 503", func(t *testing.T) {
		svc := newTestServiceForHandler(&chatStubProvider{})
		router, tokens := buildRouter(t, svc, nil)
		server := httptest.NewServer(router)
		t.Cleanup(server.Close)

		resp := doRunRequest(t, server, http.MethodPost, "/api/chat/runs/"+reattachRunID+"/cancel",
			"Bearer "+bearerToken(t, tokens))
		defer func() { _ = resp.Body.Close() }()

		assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
		var env api.ErrorResponse
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&env))
		assert.Equal(t, runCancelDisabledMessage, env.Message)
		assert.Equal(t, runCancelDisabledDetail, env.Details)
	})

	t.Run("misconfigured names the misconfiguration", func(t *testing.T) {
		svc := newMisconfiguredServiceForHandler(&chatStubProvider{})
		router, tokens := buildRouter(t, svc, nil)
		server := httptest.NewServer(router)
		t.Cleanup(server.Close)

		resp := doRunRequest(t, server, http.MethodPost, "/api/chat/runs/"+reattachRunID+"/cancel",
			"Bearer "+bearerToken(t, tokens))
		defer func() { _ = resp.Body.Close() }()

		assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
		var env api.ErrorResponse
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&env))
		assert.Equal(t, runCancelDisabledMessage, env.Message)
		assert.Equal(t, agentMisconfiguredDetail, env.Details)
	})

	t.Run("unreachable returns 502", func(t *testing.T) {
		dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		deadURL := dead.URL
		dead.Close()

		harness, tokens := newRunsHarness(t, mustAgentProxy(t, deadURL), 0, nil)
		server := httptest.NewServer(harness)
		t.Cleanup(server.Close)

		resp := doRunRequest(t, server, http.MethodPost, "/api/chat/runs/"+reattachRunID+"/cancel",
			"Bearer "+bearerToken(t, tokens))
		defer func() { _ = resp.Body.Close() }()

		raw := drainAndClose(t, resp)
		assert.Equal(t, http.StatusBadGateway, resp.StatusCode)
		var env api.ErrorResponse
		require.NoError(t, json.Unmarshal([]byte(raw), &env))
		assert.Equal(t, http.StatusBadGateway, env.Code)
		assert.Equal(t, runCancelUnreachableMessage, env.Message)
		assert.Equal(t, runCancelUnreachableDetail, env.Details)
		for _, leak := range []string{"connection refused", "dial tcp", deadURL} {
			assert.NotContains(t, raw, leak)
		}
	})

	t.Run("upstream 5xx becomes 502", func(t *testing.T) {
		const agentBody = `{"boom":"agent-internal"}`
		srv, _ := newRunsUpstream(t, http.StatusInternalServerError, agentBody, "application/json")
		harness, tokens := newRunsHarness(t, mustAgentProxy(t, srv.URL), 0, nil)
		server := httptest.NewServer(harness)
		t.Cleanup(server.Close)

		resp := doRunRequest(t, server, http.MethodPost, "/api/chat/runs/"+reattachRunID+"/cancel",
			"Bearer "+bearerToken(t, tokens))
		defer func() { _ = resp.Body.Close() }()

		raw := drainAndClose(t, resp)
		assert.Equal(t, http.StatusBadGateway, resp.StatusCode)
		assert.NotContains(t, raw, "agent-internal",
			"5xx bodies are collapsed into the static 502 envelope, not forwarded")
		var env api.ErrorResponse
		require.NoError(t, json.Unmarshal([]byte(raw), &env))
		assert.Equal(t, runCancelUnreachableMessage, env.Message)
		assert.Equal(t, runCancelUnreachableDetail, env.Details)
	})
}

func TestRunRoutes_RunIDSpecialChars_StaySingleEscapedSegment(t *testing.T) {
	srv, up := newRunsUpstream(t, http.StatusOK, `{"detail":{"ok":true}}`, "application/json")
	harness, tokens := newRunsHarness(t, mustAgentProxy(t, srv.URL), 0, nil)
	server := httptest.NewServer(harness)
	t.Cleanup(server.Close)

	bearer := "Bearer " + bearerToken(t, tokens)

	resp := doRunRequest(t, server, http.MethodPost, "/api/chat/runs/run%20with%20space/cancel", bearer)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	_ = drainAndClose(t, resp)

	last, _ := up.snapshot()
	assert.Equal(t, "/agent/runs/run with space/cancel", last.path,
		"chi decodes the path param and the proxy re-escapes it; the agent sees one decoded single segment")
	assert.Contains(t, last.requestURI, "%20",
		"the outbound segment is URL-escaped on the wire")

	resp2 := doRunRequest(t, server, http.MethodGet, "/api/chat/runs/run%20with%20space/stream", bearer)
	require.Equal(t, http.StatusOK, resp2.StatusCode)
	_ = drainAndClose(t, resp2)

	last, _ = up.snapshot()
	assert.Equal(t, "/agent/runs/run with space/stream", last.path)
	assert.Contains(t, last.requestURI, "%20")

	resp3 := doRunRequest(t, server, http.MethodGet, "/api/chat/runs/..%2F..%2Fadmin%3Fx%3D1/stream", bearer)
	require.Equal(t, http.StatusOK, resp3.StatusCode)
	_ = drainAndClose(t, resp3)

	last, _ = up.snapshot()
	assert.Equal(t, 4, strings.Count(last.path, "/"),
		"the crafted id must not introduce a path separator: got %q", last.path)
	assert.NotContains(t, last.path, "/../",
		"the crafted id must not introduce a traversal segment")
	assert.NotContains(t, last.requestURI, "?",
		"an encoded query separator inside the id must not become a real query")
}
