package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace/noop"

	"nyx/internal/llm"
	"nyx/internal/platform/auth"
	"nyx/internal/platform/config"
)

// SSE event tag constants used by the integration tests below.
// Pinning the strings avoids both runtime string typos and goconst
// lint warnings — every `f.event == eventXxx` check pulls from
// this single declaration, so a future rename only touches one
// place.
const (
	eventNameError      = "error"
	eventNameDone       = "done"
	eventNameTerminator = "terminator"
	eventNameDelta      = "delta"
)

// cannedPythonSSEBytes is the canonical Python orchestrator stream
// the regression test asserts the handler forwards byte-for-byte.
// The bytes are intentionally pinned at the byte level — the spec
// scenario "Wire envelope is byte-for-byte identical" requires that
// the response bytes the client sees equal the upstream bytes
// without any rewriting. The frame sequence mirrors what the Python
// service emits today: plan, step_start, delta, step_end, done,
// terminator (the agent-mode close signal).
const cannedPythonSSEBytes = "event: plan\n" +
	`data: {"steps":[{"id":"s1","kind":"llm","prompt":"say hi"}]}` + "\n\n" +
	"event: step_start\n" +
	`data: {"step_id":"s1","kind":"llm"}` + "\n\n" +
	"event: delta\n" +
	`data: {"delta":"hello"}` + "\n\n" +
	"event: step_end\n" +
	`data: {"step_id":"s1","status":"ok"}` + "\n\n" +
	"event: done\n" +
	"data: {}\n\n" +
	"event: terminator\n" +
	`data: {"reason":""}` + "\n\n"

// cannedPythonSSEUpstream stands up an httptest server that returns
// the canned Python SSE bytes with the standard SSE response
// headers the spec requires the proxy to forward unchanged. The
// server also records every Authorization + X-Nyx-User-Id header
// it saw so tests can assert on the forwarding contract.
func cannedPythonSSEUpstream(t *testing.T, body string) (*httptest.Server, *string, *string) {
	t.Helper()
	var lastAuth, lastUserID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastAuth = r.Header.Get("Authorization")
		lastUserID = r.Header.Get("X-Nyx-User-Id")
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, &lastAuth, &lastUserID
}

// newProxyHarness wires the chat route on a fresh chi router with
// the supplied proxy and returns the assembled http.Handler + a
// TokenService so the test can mint a bearer token. Mirrors
// buildRouter in handler_test.go but wires the AGENT_ENABLED=true
// path.
func newProxyHarness(t *testing.T, proxy *AgentProxy) (http.Handler, auth.TokenService) {
	t.Helper()
	tokens, err := auth.NewTokenService([]byte(auth.TestSecret), 15*time.Minute)
	require.NoError(t, err)

	cfg := &config.Config{
		LLMMaxHistoryMessages: 5,
		LLMMaxMessageChars:    50,
		LLMMaxTokens:          100,
	}
	tracer := noop.NewTracerProvider().Tracer("test")
	svc := NewServiceWithProxy(&llmProviderUnused{}, cfg, tracer, proxy)
	h := NewHandler(svc)
	router, t2 := buildRouter(t, svc, nil)
	_ = h
	_ = tokens
	return router, t2
}

// TestAgentProxy_Integration_HandlerForwardsPythonBytesByteForByte
// is the hero regression test for spec requirement "Wire envelope
// is byte-for-byte identical": the chat handler's response body
// must equal the upstream Python service's response body, with no
// rewriting, wrapping, or extra fields.
func TestAgentProxy_Integration_HandlerForwardsPythonBytesByteForByte(t *testing.T) {
	srv, lastAuth, lastUserID := cannedPythonSSEUpstream(t, cannedPythonSSEBytes)

	proxy, err := NewAgentProxy(srv.URL, nil)
	require.NoError(t, err)

	harness, tokens := newProxyHarness(t, proxy)
	server := httptest.NewServer(harness)
	defer server.Close()

	req, err := http.NewRequest(http.MethodPost, server.URL+"/api/chat",
		bytes.NewReader(chatRequest(Message{Role: RoleUser, Content: "hi"})))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+bearerToken(t, tokens))
	req.Header.Set("Content-Type", "application/json")

	resp, err := server.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	// Status, headers (Content-Type + the SSE-envelope trio) are
	// pinned by the spec. The handler writes its own SSE headers
	// before the proxy responds, so the client sees these.
	assert.Equal(t, "text/event-stream; charset=utf-8", resp.Header.Get("Content-Type"))
	assert.Equal(t, "no-cache", resp.Header.Get("Cache-Control"))
	assert.Equal(t, "keep-alive", resp.Header.Get("Connection"))
	assert.Equal(t, "no", resp.Header.Get("X-Accel-Buffering"))

	// Read the entire response body and assert byte-for-byte
	// equality with the canned Python stream.
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, cannedPythonSSEBytes, string(body),
		"chat handler must forward Python bytes verbatim — no rewriting, no wrapping")

	// The Authorization header the upstream saw must equal what
	// the client sent (the proxy MUST NOT mint a new token, MUST
	// NOT strip the Bearer prefix, MUST NOT log the value).
	assert.Equal(t, "Bearer "+bearerToken(t, tokens), *lastAuth,
		"Authorization header forwarded verbatim from chat request to Python service")

	// X-Nyx-User-Id is set from the JWT-verified user id (the
	// Python service reads this header directly and does not decode
	// the JWT itself). The token mint helper uses sub=1 so the
	// upstream sees "1".
	assert.Equal(t, "1", *lastUserID,
		"X-Nyx-User-Id header must equal the verified user id")
}

// TestAgentProxy_Integration_FailClosedOnConnectionRefused pins the
// spec scenario "Connection refused": when the proxy cannot
// establish a TCP connection, the chat handler emits
// `error{code:"agent_unavailable"}` followed by done + terminator
// and returns HTTP 200 (the SSE envelope was opened).
func TestAgentProxy_Integration_FailClosedOnConnectionRefused(t *testing.T) {
	// httptest server we close immediately → dial refused.
	deadSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := deadSrv.URL
	deadSrv.Close()

	proxy, err := NewAgentProxy(deadURL, nil)
	require.NoError(t, err)

	harness, tokens := newProxyHarness(t, proxy)
	server := httptest.NewServer(harness)
	defer server.Close()

	req, _ := http.NewRequest(http.MethodPost, server.URL+"/api/chat",
		bytes.NewReader(chatRequest(Message{Role: RoleUser, Content: "hi"})))
	req.Header.Set("Authorization", "Bearer "+bearerToken(t, tokens))
	req.Header.Set("Content-Type", "application/json")

	resp, err := server.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	// SSE envelope was opened → status is 200.
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "text/event-stream; charset=utf-8", resp.Header.Get("Content-Type"))

	// Parse the SSE stream; the SPA would do the same.
	frames := readSSEFrames(t, resp.Body)

	// Find the typed error frame; it MUST carry code:
	// "agent_unavailable" per the spec's "Python service unavailable
	// fails closed" requirement.
	var errFrame *sseFrame
	for i := range frames {
		if frames[i].event == eventNameError {
			errFrame = &frames[i]
			break
		}
	}
	require.NotNil(t, errFrame, "expected an event:error frame on connect failure; got %+v", frames)

	var errPayload struct {
		Code      string `json:"code"`
		Error     string `json:"error"`
		RequestID string `json:"request_id"`
	}
	require.NoError(t, json.Unmarshal([]byte(errFrame.data), &errPayload))
	assert.Equal(t, "agent_unavailable", errPayload.Code,
		"the typed code MUST be agent_unavailable per spec")
	assert.Equal(t, "Agent service unavailable", errPayload.Error,
		"the safeDetail MUST be a static literal — never err.Error()")
	assert.NotEmpty(t, errPayload.RequestID,
		"the request_id MUST be propagated so operators can correlate logs")

	// done + terminator MUST follow per the spec scenario
	// "Connection refused".
	var sawDone, sawTerminator bool
	for _, f := range frames {
		if f.event == eventNameDone {
			sawDone = true
		}
		if f.event == eventNameTerminator {
			sawTerminator = true
		}
	}
	assert.True(t, sawDone, "done frame MUST follow the error frame")
	assert.True(t, sawTerminator, "terminator frame MUST follow the done frame")
}

// TestAgentProxy_Integration_FailClosedOnPython5xx pins the spec
// scenario "Python returns 5xx": a 500 from the Python service
// must produce the same typed error + done + terminator envelope
// as the connection-refused case.
func TestAgentProxy_Integration_FailClosedOnPython5xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	proxy, err := NewAgentProxy(srv.URL, nil)
	require.NoError(t, err)

	harness, tokens := newProxyHarness(t, proxy)
	server := httptest.NewServer(harness)
	defer server.Close()

	req, _ := http.NewRequest(http.MethodPost, server.URL+"/api/chat",
		bytes.NewReader(chatRequest(Message{Role: RoleUser, Content: "hi"})))
	req.Header.Set("Authorization", "Bearer "+bearerToken(t, tokens))
	req.Header.Set("Content-Type", "application/json")

	resp, err := server.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusOK, resp.StatusCode,
		"SSE envelope was opened; status is 200 even on 5xx from upstream")

	frames := readSSEFrames(t, resp.Body)
	var errFrame *sseFrame
	for i := range frames {
		if frames[i].event == eventNameError {
			errFrame = &frames[i]
			break
		}
	}
	require.NotNil(t, errFrame, "expected error frame on 5xx from Python; got %+v", frames)
	var errPayload struct {
		Code string `json:"code"`
	}
	require.NoError(t, json.Unmarshal([]byte(errFrame.data), &errPayload))
	assert.Equal(t, "agent_unavailable", errPayload.Code,
		"5xx from Python MUST surface as agent_unavailable — body MUST NOT be forwarded")
}

// TestAgentProxy_Integration_MultiFrameUpstreamStreamed asserts the
// handler streams a multi-frame upstream response. The Python
// service emits a longer stream; the handler must forward every
// frame intact without dropping or merging.
func TestAgentProxy_Integration_MultiFrameUpstreamStreamed(t *testing.T) {
	// Build a 5-frame SSE stream.
	var upstream strings.Builder
	for i := 0; i < 5; i++ {
		upstream.WriteString("event: delta\n")
		upstream.WriteString(`data: {"delta":"chunk` + string(rune('a'+i)) + `"}` + "\n\n")
	}
	upstream.WriteString("event: done\ndata: {}\n\n")
	upstream.WriteString("event: terminator\ndata: {\"reason\":\"\"}\n\n")
	body := upstream.String()

	srv, _, _ := cannedPythonSSEUpstream(t, body)

	proxy, err := NewAgentProxy(srv.URL, nil)
	require.NoError(t, err)
	harness, tokens := newProxyHarness(t, proxy)
	server := httptest.NewServer(harness)
	defer server.Close()

	req, _ := http.NewRequest(http.MethodPost, server.URL+"/api/chat",
		bytes.NewReader(chatRequest(Message{Role: RoleUser, Content: "hi"})))
	req.Header.Set("Authorization", "Bearer "+bearerToken(t, tokens))
	req.Header.Set("Content-Type", "application/json")

	resp, err := server.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	got, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, body, string(got),
		"5-frame upstream stream forwarded byte-for-byte")

	// Parse out the delta count to pin that all 5 deltas reached
	// the client (not merged or dropped).
	frames := readSSEFrames(t, bytes.NewReader(got))
	var deltaCount int
	for _, f := range frames {
		if f.event == eventNameDelta {
			deltaCount++
		}
	}
	assert.Equal(t, 5, deltaCount, "5 delta frames in the upstream must reach the client")
}

// TestAgentProxy_Integration_StreamCapCutEmitsTypedTerminalFrames
// regression-pins the 2026-10-14 60s-cut incident: a live upstream
// stream must be bounded by LLM_MAX_STREAM_DURATION (the handler's
// request-context deadline), and when that cap cuts a still-live
// stream the client must see typed terminal frames — error
// {code:"stream_timeout"} + done + terminator — instead of a
// silently dead connection.
//
// Red-first posture: before the fix the stream was bounded by the
// proxy client's 60s http.Client.Timeout (which cannot emit
// frames), so the body read below stayed open far past the 5s
// assertion. A wedged upstream (headers sent, then silence) is the
// minimal trigger.
func TestAgentProxy_Integration_StreamCapCutEmitsTypedTerminalFrames(t *testing.T) {
	upstreamCanceled := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		// One frame reaches the client, then the stream goes
		// silent-but-live until the handler's cap cuts it.
		_, _ = io.WriteString(w, "event: delta\ndata: {\"delta\":\"hel\"}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
		close(upstreamCanceled)
	}))
	defer srv.Close()

	proxy, err := NewAgentProxy(srv.URL, nil)
	require.NoError(t, err)

	// 200ms stream cap: the handler must cut the silent stream
	// promptly and close the wire contract with typed frames.
	cfg := &config.Config{
		LLMMaxHistoryMessages: 5,
		LLMMaxMessageChars:    50,
		LLMMaxTokens:          100,
		LLMMaxStreamDuration:  "200ms",
	}
	tracer := noop.NewTracerProvider().Tracer("test")
	svc := NewServiceWithProxy(&llmProviderUnused{}, cfg, tracer, proxy)
	harness, tokens := buildRouter(t, svc, nil)
	server := httptest.NewServer(harness)
	defer server.Close()

	req, err := http.NewRequest(http.MethodPost, server.URL+"/api/chat",
		bytes.NewReader(chatRequest(Message{Role: RoleUser, Content: "hi"})))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+bearerToken(t, tokens))
	req.Header.Set("Content-Type", "application/json")

	start := time.Now()
	resp, err := server.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	elapsed := time.Since(start)

	assert.Less(t, elapsed, 5*time.Second,
		"the cap must cut the stream promptly, not at a client-side default timeout")
	got := string(body)
	assert.Contains(t, got, `event: delta`,
		"frames forwarded before the cut must reach the client verbatim")
	assert.Contains(t, got, `"code":"stream_timeout"`,
		"the cut must be surfaced as a typed error frame")
	assert.Contains(t, got, "event: done")
	assert.Contains(t, got, "event: terminator")

	frames := readSSEFrames(t, strings.NewReader(got))
	var sawError, sawDone, sawTerminator bool
	for _, f := range frames {
		switch f.event {
		case eventNameError:
			sawError = true
			assert.Contains(t, f.data, `"error":"Agent stream exceeded the time limit"`,
				"the error detail is the static, safe-to-ship message")
		case eventNameDone:
			sawDone = true
		case eventNameTerminator:
			sawTerminator = true
			assert.Contains(t, f.data, `"reason":"stream_timeout"`)
		}
	}
	assert.True(t, sawError && sawDone && sawTerminator,
		"error + done + terminator must all be present; got: %s", got)

	select {
	case <-upstreamCanceled:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream request was not canceled when the cap cut the stream")
	}
}

// TestAgentProxy_Integration_StreamCapAfterUpstreamTerminalEmitsNothing
// pins the deadline branch's guard against double-terminating the
// wire: the Python agent keeps the response body open AFTER its own
// terminal frames while it persists the run's rows, so a cap that
// fires inside that window must append nothing — the upstream bytes
// are the whole response, and the SPA stopped reading at `done`
// anyway.
func TestAgentProxy_Integration_StreamCapAfterUpstreamTerminalEmitsNothing(t *testing.T) {
	upstream := "event: delta\n" +
		`data: {"delta":"hi"}` + "\n\n" +
		"event: done\n" +
		"data: {}\n\n" +
		"event: " + eventNameTerminator + "\n" +
		`data: {"reason":""}` + "\n\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, upstream)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Hold the body open (the agent's post-terminator finalize
		// window) until the handler's cap cuts it.
		<-r.Context().Done()
	}))
	defer srv.Close()

	proxy, err := NewAgentProxy(srv.URL, nil)
	require.NoError(t, err)

	cfg := &config.Config{
		LLMMaxHistoryMessages: 5,
		LLMMaxMessageChars:    50,
		LLMMaxTokens:          100,
		LLMMaxStreamDuration:  "200ms",
	}
	tracer := noop.NewTracerProvider().Tracer("test")
	svc := NewServiceWithProxy(&llmProviderUnused{}, cfg, tracer, proxy)
	harness, tokens := buildRouter(t, svc, nil)
	server := httptest.NewServer(harness)
	defer server.Close()

	req, err := http.NewRequest(http.MethodPost, server.URL+"/api/chat",
		bytes.NewReader(chatRequest(Message{Role: RoleUser, Content: "hi"})))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+bearerToken(t, tokens))
	req.Header.Set("Content-Type", "application/json")

	resp, err := server.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, upstream, string(body),
		"nothing may be appended after the upstream's own terminal frames")
	assert.NotContains(t, string(body), "stream_timeout",
		"a run that closed its own stream is not a timeout")
}

// llmProviderUnused is a stand-in llm.Provider used because
// NewServiceWithProxy's signature requires one. The proxy path
// never invokes it (the chat handler dispatches to the proxy, not
// the provider). Panicking on accidental calls would surface a
// regression where the agent path accidentally reached for the
// legacy provider.
type llmProviderUnused struct{}

func (n *llmProviderUnused) Chat(_ context.Context, _ llm.ChatRequest, _ func(string, *llm.ChatUsage) error) (*llm.ChatUsage, error) {
	panic("llmProviderUnused.Chat must not be called from agent-proxy path")
}
func (n *llmProviderUnused) Embed(_ context.Context, _ llm.EmbedRequest) ([][]float32, error) {
	panic("llmProviderUnused.Embed must not be called")
}
