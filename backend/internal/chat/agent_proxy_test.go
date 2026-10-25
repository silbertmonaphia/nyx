package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newUpstreamServer wires a canned httptest.Server that returns the
// supplied response body + status, and records every inbound request
// header + body so a test can assert on them. The server is the
// stand-in for the Python orchestrator — the proxy is supposed to
// forward bytes verbatim and forward the Authorization header
// untouched.
func newUpstreamServer(t *testing.T, status int, body string, ct string) (*httptest.Server, *capturedRequest) {
	t.Helper()
	captured := &capturedRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.method = r.Method
		captured.path = r.URL.Path
		captured.authorization = r.Header.Get("Authorization")
		captured.userID = r.Header.Get("X-Nyx-User-Id")
		captured.contentType = r.Header.Get("Content-Type")
		captured.accept = r.Header.Get("Accept")
		buf, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		captured.body = string(buf)

		w.Header().Set("Content-Type", ct)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, captured
}

type capturedRequest struct {
	method        string
	path          string
	authorization string
	userID        string
	contentType   string
	accept        string
	body          string
}

// cannedSSE is the canned Python SSE stream the happy-path tests use
// to verify byte-for-byte forwarding. Frame sequence matches what
// the spec scenario "Frontend sees same frames as before" calls out:
// plan, step_start, tool_call, tool_result, step_end, delta, done,
// terminator. The trailing terminator is the new close signal in
// agent mode; Python emits it; the proxy forwards it; the SPA
// observes the stream is done.
const cannedSSE = "event: plan\n" +
	"data: {\"steps\":[{\"id\":\"s1\",\"kind\":\"llm\"}]}\n\n" +
	"event: step_start\n" +
	"data: {\"step_id\":\"s1\",\"kind\":\"llm\"}\n\n" +
	"event: delta\n" +
	"data: {\"delta\":\"hello\"}\n\n" +
	"event: step_end\n" +
	"data: {\"step_id\":\"s1\",\"status\":\"ok\"}\n\n" +
	"event: done\n" +
	"data: {}\n\n" +
	"event: terminator\n" +
	"data: {\"reason\":\"\"}\n\n"

func TestAgentProxy_Run_ForwardsRequestBody(t *testing.T) {
	srv, captured := newUpstreamServer(t, http.StatusOK, cannedSSE, "text/event-stream; charset=utf-8")
	proxy, err := NewAgentProxy(srv.URL, nil)
	require.NoError(t, err)

	resp, err := proxy.Run(context.Background(), AgentRunPayload{ChatRequest: ChatRequest{
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	}}, 42, "Bearer eyJhbGc.dummy.token")
	require.NoError(t, err)
	require.NotNil(t, resp)
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.MethodPost, captured.method)
	assert.Equal(t, "/agent/run", captured.path)
	assert.Equal(t, "Bearer eyJhbGc.dummy.token", captured.authorization,
		"Authorization header forwarded verbatim — proxy MUST NOT re-mint or strip the token")
	assert.Equal(t, "42", captured.userID,
		"X-Nyx-User-Id header set from the verified user id")
	assert.Equal(t, "application/json", captured.contentType)
	assert.Equal(t, "text/event-stream", captured.accept)
	assert.Contains(t, captured.body, `"messages"`,
		"chat request body reaches the upstream")
	assert.Contains(t, captured.body, `"hi"`,
		"the user's last message is in the upstream body")
}

// TestAgentProxy_Run_ReturnsBodyBytesForHandlerToForward pins the
// proxy contract: the response body is the raw Python SSE stream,
// untransformed. The handler reads resp.Body and copies it
// byte-for-byte to the chat response writer — see
// handler.streamProxy. Here we assert the body equals the canned
// upstream bytes verbatim.
func TestAgentProxy_Run_ReturnsBodyBytesForHandlerToForward(t *testing.T) {
	srv, _ := newUpstreamServer(t, http.StatusOK, cannedSSE, "text/event-stream; charset=utf-8")
	proxy, err := NewAgentProxy(srv.URL, nil)
	require.NoError(t, err)

	resp, err := proxy.Run(context.Background(), AgentRunPayload{ChatRequest: ChatRequest{
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	}}, 42, "Bearer test")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "text/event-stream; charset=utf-8", resp.Header.Get("Content-Type"))

	got, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, cannedSSE, string(got),
		"body bytes must equal upstream verbatim — handler forwards them without transformation")
}

// TestAgentProxy_Run_ConnectionRefused_ReturnsErrAgentUnavailable
// pins the fail-closed contract: when the upstream is unreachable,
// Run returns ErrAgentUnavailable, NOT a connection error. The
// handler relies on errors.Is(err, ErrAgentUnavailable) to decide
// whether to emit the agent_unavailable SSE frame.
func TestAgentProxy_Run_ConnectionRefused_ReturnsErrAgentUnavailable(t *testing.T) {
	// httptest server with a TCP listener we close immediately,
	// so the proxy's dial fails fast with connection refused.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // listener gone — dial will fail

	proxy, err := NewAgentProxy(url, nil)
	require.NoError(t, err)

	resp, err := proxy.Run(context.Background(), AgentRunPayload{}, 42, "Bearer t")
	if resp != nil {
		_ = resp.Body.Close()
	}
	assert.Nil(t, resp)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrAgentUnavailable),
		"connection refused must surface as ErrAgentUnavailable; got %v", err)
}

// TestAgentProxy_Run_5xx_ReturnsErrAgentUnavailable pins the
// "Python returns 5xx" spec scenario: the proxy MUST NOT forward
// 5xx bodies (the handler emits agent_unavailable instead). Run
// returns ErrAgentUnavailable for any 5xx status.
func TestAgentProxy_Run_5xx_ReturnsErrAgentUnavailable(t *testing.T) {
	for _, status := range []int{500, 502, 503, 504} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			srv, _ := newUpstreamServer(t, status, "internal error", "text/plain")
			proxy, err := NewAgentProxy(srv.URL, nil)
			require.NoError(t, err)

			resp, err := proxy.Run(context.Background(), AgentRunPayload{}, 42, "Bearer t")
			if resp != nil {
				_ = resp.Body.Close()
			}
			assert.Nil(t, resp, "5xx must return nil response — handler emits typed error")
			require.Error(t, err)
			assert.True(t, errors.Is(err, ErrAgentUnavailable),
				"5xx (%d) must surface as ErrAgentUnavailable; got %v", status, err)
		})
	}
}

// TestAgentProxy_Run_4xx_ReturnsResponseForByteForByteForward pins
// the "Python returns 401 (JWT rejected)" spec scenario: a non-5xx
// response is returned to the handler so the body can be forwarded
// verbatim. The handler does NOT translate the status — per the
// spec, the operator's Python service is misconfigured, not the
// client.
func TestAgentProxy_Run_4xx_ReturnsResponseForByteForByteForward(t *testing.T) {
	cases := []int{400, 401, 403, 404}
	for _, status := range cases {
		t.Run(http.StatusText(status), func(t *testing.T) {
			body := `{"error":"unauthorized"}`
			srv, _ := newUpstreamServer(t, status, body, "application/json")
			proxy, err := NewAgentProxy(srv.URL, nil)
			require.NoError(t, err)

			resp, err := proxy.Run(context.Background(), AgentRunPayload{}, 42, "Bearer t")
			require.NoError(t, err, "%d must NOT be classified as unavailable", status)
			require.NotNil(t, resp)
			defer func() { _ = resp.Body.Close() }()

			assert.Equal(t, status, resp.StatusCode)
			got, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			assert.Equal(t, body, string(got),
				"%d body returned verbatim — handler forwards unchanged", status)
		})
	}
}

// TestAgentProxy_Run_2xx_ReturnsResponse pins the happy path: 2xx
// (today the orchestrator only emits 200) returns the response with
// the SSE body intact.
func TestAgentProxy_Run_2xx_ReturnsResponse(t *testing.T) {
	srv, _ := newUpstreamServer(t, http.StatusOK, cannedSSE, "text/event-stream")
	proxy, err := NewAgentProxy(srv.URL, nil)
	require.NoError(t, err)

	resp, err := proxy.Run(context.Background(), AgentRunPayload{}, 42, "Bearer t")
	require.NoError(t, err)
	require.NotNil(t, resp)
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	got, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, cannedSSE, string(got))
}

// TestAgentProxy_Run_CtxCanceled_NetworkError pins the request-
// scoped context contract: a canceled ctx reaches the http client
// and the network error is classified as ErrAgentUnavailable.
// (The handler wraps Run in the request context; client disconnect
// fires the cancel.)
func TestAgentProxy_Run_CtxCanceled_NetworkError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Hang until the client gives up; we never write a response.
		<-r.Context().Done()
	}))
	defer srv.Close()

	proxy, err := NewAgentProxy(srv.URL, &http.Client{Timeout: 50 * time.Millisecond})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel — client dial will fail

	resp, err := proxy.Run(ctx, AgentRunPayload{}, 42, "Bearer t")
	if resp != nil {
		_ = resp.Body.Close()
	}
	assert.Nil(t, resp)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrAgentUnavailable),
		"ctx-canceled network error must surface as ErrAgentUnavailable; got %v", err)
}

// TestNewAgentProxy_RejectsEmptyBaseURL pins the constructor's
// fail-closed contract: an empty PYTHON_AGENT_URL at config.Load
// time means the operator did not configure the agent path; the
// constructor refuses rather than silently building a proxy that
// would always fail with ErrAgentUnavailable.
func TestNewAgentProxy_RejectsEmptyBaseURL(t *testing.T) {
	_, err := NewAgentProxy("", nil)
	require.Error(t, err)
}

// TestNewAgentProxy_DefaultClientCarriesNoStreamTimeout pins the
// anti-footgun contract from the 2026-10-14 60s-cut incident: the
// proxy's client must NOT carry an http.Client.Timeout (Client.Timeout
// bounds the entire exchange INCLUDING the streamed body, so it
// severs any live run that outlives it — mid-approval, with no
// terminal frames). The default client instead carries a
// ResponseHeaderTimeout so a wedged upstream fails fast without
// touching a live stream.
func TestNewAgentProxy_DefaultClientCarriesNoStreamTimeout(t *testing.T) {
	proxy, err := NewAgentProxy("http://agent.test", nil)
	require.NoError(t, err)
	require.NotNil(t, proxy.httpClient)

	assert.Zero(t, proxy.httpClient.Timeout,
		"http.Client.Timeout must stay unset on the SSE proxy client")

	tr, ok := proxy.httpClient.Transport.(*http.Transport)
	require.True(t, ok, "default client must sit on an *http.Transport (clone)")
	assert.Equal(t, 30*time.Second, tr.ResponseHeaderTimeout,
		"default client must fail fast on a wedged upstream's headers")
}

// TestNewAgentProxy_TrimsTrailingSlash pins the URL normalisation:
// trailing "/" on PYTHON_AGENT_URL is allowed (operators tend to
// include it from the compose env file) and must not produce a
// doubled "//" path on the outbound URL.
func TestNewAgentProxy_TrimsTrailingSlash(t *testing.T) {
	srv, captured := newUpstreamServer(t, http.StatusOK, cannedSSE, "text/event-stream")
	proxy, err := NewAgentProxy(srv.URL+"/", nil) // trailing slash
	require.NoError(t, err)

	resp, err := proxy.Run(context.Background(), AgentRunPayload{}, 42, "Bearer t")
	if resp != nil {
		_ = resp.Body.Close()
	}
	require.NoError(t, err)
	assert.Equal(t, "/agent/run", captured.path,
		"trailing slash on base URL must not produce //agent/run")
}

// TestTokenFingerprint pins the token-logging safety helper: only
// the first 6 characters + "…" may appear; the full token never
// appears; the helper is safe to use in OTel span attributes.
func TestTokenFingerprint(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"short", "short…"},
		{"eyJhbGc.dummy.token", "eyJhbG…"},
		{"Bearer eyJhbGc.dummy.token", "eyJhbG…"},
		{"  Bearer   eyJhbGc.dummy.token  ", "eyJhbG…"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			assert.Equal(t, c.want, TokenFingerprint(c.in))
		})
	}

	// The full token never appears in the fingerprint — pin the
	// invariant so a future "include last 4" tweak doesn't slip
	// the tail onto the wire.
	full := "eyJhbGciOiJIUzI1NiJ9.dummy.signature"
	fp := TokenFingerprint(full)
	assert.False(t, strings.Contains(fp, "signature"),
		"fingerprint MUST NOT contain the token's tail")
	assert.False(t, strings.Contains(fp, "."),
		"fingerprint MUST NOT contain the token's body — only first 6 chars")
}

func TestAgentProxy_Run_ForwardsPerUserConfig(t *testing.T) {
	srv, captured := newUpstreamServer(t, http.StatusOK, cannedSSE, "text/event-stream")
	proxy, err := NewAgentProxy(srv.URL, nil)
	require.NoError(t, err)

	model := "client-model"
	runID := "run-1"
	payload := AgentRunPayload{
		ChatRequest: ChatRequest{
			Messages: []Message{{Role: RoleUser, Content: "hello"}},
			Model:    &model,
			RAG:      true,
			RunID:    &runID,
		},
		LLMConfig: &AgentLLMConfig{
			BaseURL: "http://localhost:8000/v1",
			APIKey:  "e2e-placeholder-key-not-secret",
			Model:   "Qwen/Qwen3-4B-Instruct-2507-FP8",
		},
		TavilyConfig: &AgentTavilyConfig{
			BaseURL: "https://api.tavily.com/search",
			APIKey:  "e2e-placeholder-key-not-secret",
		},
	}

	resp, err := proxy.Run(context.Background(), payload, 42, "Bearer t")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	var decoded map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(captured.body), &decoded))
	require.Contains(t, decoded, "llm_config")
	require.Contains(t, decoded, "tavily_config")
	assert.NotContains(t, decoded, "fallback", "the fallback slot must never be forwarded")

	var llmCfg AgentLLMConfig
	require.NoError(t, json.Unmarshal(decoded["llm_config"], &llmCfg))
	assert.Equal(t, payload.LLMConfig.BaseURL, llmCfg.BaseURL)
	assert.Equal(t, payload.LLMConfig.APIKey, llmCfg.APIKey)
	assert.Equal(t, payload.LLMConfig.Model, llmCfg.Model)

	var tavilyCfg AgentTavilyConfig
	require.NoError(t, json.Unmarshal(decoded["tavily_config"], &tavilyCfg))
	assert.Equal(t, payload.TavilyConfig.BaseURL, tavilyCfg.BaseURL)
	assert.Equal(t, payload.TavilyConfig.APIKey, tavilyCfg.APIKey)

	last := -1
	for _, key := range []string{`"messages"`, `"model"`, `"rag"`, `"run_id"`, `"llm_config"`, `"tavily_config"`} {
		idx := strings.Index(captured.body, key)
		require.Greater(t, idx, last, "wire order: %s must follow the previous field", key)
		last = idx
	}
}

func TestAgentProxy_Run_MatchesAgentPayloadFixture(t *testing.T) {
	srv, captured := newUpstreamServer(t, http.StatusOK, cannedSSE, "text/event-stream")
	proxy, err := NewAgentProxy(srv.URL, nil)
	require.NoError(t, err)

	payload := AgentRunPayload{
		ChatRequest: ChatRequest{Messages: []Message{{Role: RoleUser, Content: "hello"}}},
		LLMConfig: &AgentLLMConfig{
			BaseURL: "http://localhost:8000/v1",
			APIKey:  "e2e-placeholder-key-not-secret",
			Model:   "Qwen/Qwen3-4B-Instruct-2507-FP8",
		},
		TavilyConfig: &AgentTavilyConfig{
			BaseURL: "https://api.tavily.com/search",
			APIKey:  "e2e-placeholder-key-not-secret",
		},
	}

	resp, err := proxy.Run(context.Background(), payload, 42, "Bearer t")
	require.NoError(t, err)
	_ = resp.Body.Close()

	var pretty bytes.Buffer
	require.NoError(t, json.Indent(&pretty, []byte(captured.body), "", "  "))
	pretty.WriteByte('\n')

	fixture := filepath.Join("..", "..", "..", "frontend", "tests", "e2e", "fixtures", "agent-payload-llm-config.json")
	if os.Getenv("UPDATE_FIXTURES") == "1" {
		require.NoError(t, os.WriteFile(fixture, pretty.Bytes(), 0o600))
	}

	//nolint:gosec // G304: test-controlled fixture path.
	raw, err := os.ReadFile(fixture)
	require.NoError(t, err, "fixture missing; regenerate with UPDATE_FIXTURES=1 go test ./internal/chat -run TestAgentProxy_Run_MatchesAgentPayloadFixture")

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(raw, &decoded))
	var capturedDecoded map[string]any
	require.NoError(t, json.Unmarshal([]byte(captured.body), &capturedDecoded))
	require.Equal(t, capturedDecoded, decoded, "fixture drift; regenerate with UPDATE_FIXTURES=1")

	assert.Contains(t, decoded, "llm_config")
	assert.Contains(t, decoded, "tavily_config")
	assert.NotContains(t, decoded, "fallback")
	assert.Contains(t, string(raw), "e2e-placeholder-key-not-secret")
}
