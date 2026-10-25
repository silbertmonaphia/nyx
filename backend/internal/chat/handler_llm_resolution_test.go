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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace/noop"

	"nyx/internal/llm"
	"nyx/internal/llmsettings"
	"nyx/internal/platform/api"
	"nyx/internal/platform/auth"
	"nyx/internal/platform/config"
)

type stubLLMResolver struct {
	mu     sync.Mutex
	stack  *llmsettings.UserStack
	err    error
	calls  int
	userID int
}

func (r *stubLLMResolver) Resolve(_ context.Context, userID int) (*llmsettings.UserStack, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.userID = userID
	return r.stack, r.err
}

func (r *stubLLMResolver) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

type countingProvider struct {
	mu     sync.Mutex
	calls  int
	deltas []string
}

func (p *countingProvider) Chat(_ context.Context, _ llm.ChatRequest, onDelta func(string, *llm.ChatUsage) error) (*llm.ChatUsage, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	for _, d := range p.deltas {
		if err := onDelta(d, nil); err != nil {
			return nil, err
		}
	}
	return nil, nil
}

func (p *countingProvider) Embed(_ context.Context, _ llm.EmbedRequest) ([][]float32, error) {
	panic("countingProvider.Embed should not be called from chat handler tests")
}

func (p *countingProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func testUserStack() *llmsettings.UserStack {
	return &llmsettings.UserStack{
		Chat:             &countingProvider{deltas: []string{"user-provider"}},
		Embed:            &countingProvider{},
		EmbedModel:       "user-embed-model",
		LLMBaseURL:       "http://user-llm:8000/v1",
		LLMAPIKey:        "sk-user-real-key-1234",
		LLMModel:         "user-chat-model",
		EmbeddingBaseURL: "http://user-embed:8001/v1",
		EmbeddingAPIKey:  "sk-user-embed-key-5678",
		EmbeddingModel:   "user-embed-model",
		TavilyBaseURL:    "https://api.tavily.com/search",
		TavilyAPIKey:     "tvly-user-real-key-9012",
		FallbackBaseURL:  "http://fallback:8000/v1",
		FallbackAPIKey:   "sk-fallback-key-3456",
		FallbackModel:    "fallback-model",
	}
}

func postChat(t *testing.T, server *httptest.Server, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, server.URL+"/api/chat",
		bytes.NewReader(chatRequest(Message{Role: RoleUser, Content: "hi"})))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := server.Client().Do(req)
	require.NoError(t, err)
	return resp
}

func assertConflictEnvelope(t *testing.T, resp *http.Response) {
	t.Helper()
	assert.Equal(t, http.StatusConflict, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "application/json")
	var env api.ErrorResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&env))
	assert.Equal(t, http.StatusConflict, env.Code)
	assert.Equal(t, "llm_not_configured", env.Details)
}

func newProxyHarnessWithResolver(t *testing.T, proxy *AgentProxy, resolver LLMResolver) (http.Handler, auth.TokenService) {
	t.Helper()
	cfg := &config.Config{
		LLMMaxHistoryMessages: 5,
		LLMMaxMessageChars:    50,
		LLMMaxTokens:          100,
	}
	svc := NewServiceWithProxy(&countingProvider{}, cfg, noop.NewTracerProvider().Tracer("test"), proxy)
	svc.SetResolver(resolver)
	return buildRouter(t, svc, nil)
}

func TestHandler_UnconfiguredLegacyReturnsJSON409(t *testing.T) {
	defaultProvider := &countingProvider{deltas: []string{"must-not-run"}}
	svc := newTestServiceForHandler(defaultProvider)
	resolver := &stubLLMResolver{err: llmsettings.ErrNotFound}
	svc.SetResolver(resolver)

	router, tokens := buildRouter(t, svc, nil)
	server := httptest.NewServer(router)
	defer server.Close()

	resp := postChat(t, server, bearerToken(t, tokens))
	defer func() { _ = resp.Body.Close() }()
	assertConflictEnvelope(t, resp)
	assert.Zero(t, defaultProvider.callCount(), "no provider may be called for an unconfigured user")
	assert.Equal(t, 1, resolver.callCount())
	assert.Equal(t, 1, resolver.userID, "resolution uses the JWT-identified user")
}

func TestHandler_UnconfiguredAgentModeRefusedBeforeUpstream(t *testing.T) {
	var upstreamHits int
	var mu sync.Mutex
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		upstreamHits++
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(cannedSSE))
	}))
	defer upstream.Close()

	proxy, err := NewAgentProxy(upstream.URL, nil)
	require.NoError(t, err)
	router, tokens := newProxyHarnessWithResolver(t, proxy, &stubLLMResolver{err: llmsettings.ErrNotFound})
	server := httptest.NewServer(router)
	defer server.Close()

	resp := postChat(t, server, bearerToken(t, tokens))
	defer func() { _ = resp.Body.Close() }()
	assertConflictEnvelope(t, resp)

	mu.Lock()
	defer mu.Unlock()
	assert.Zero(t, upstreamHits, "the Python agent must not be contacted before the 409")
}

func TestHandler_LegacyUsesResolvedPerUserProvider(t *testing.T) {
	defaultProvider := &countingProvider{deltas: []string{"deployment-provider"}}
	svc := newTestServiceForHandler(defaultProvider)
	stack := testUserStack()
	userProvider := stack.Chat.(*countingProvider)
	svc.SetResolver(&stubLLMResolver{stack: stack})

	router, tokens := buildRouter(t, svc, nil)
	server := httptest.NewServer(router)
	defer server.Close()

	resp := postChat(t, server, bearerToken(t, tokens))
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	frames := readSSEFrames(t, resp.Body)
	var deltas []string
	for _, f := range frames {
		if f.event == eventNameDelta {
			var d struct {
				Delta string `json:"delta"`
			}
			require.NoError(t, json.Unmarshal([]byte(f.data), &d))
			deltas = append(deltas, d.Delta)
		}
	}
	assert.Equal(t, []string{"user-provider"}, deltas)
	assert.Equal(t, 1, userProvider.callCount())
	assert.Zero(t, defaultProvider.callCount(), "the deployment provider must not serve a configured user")
}

func TestHandler_ResolveFailureReturns500WithSafeDetails(t *testing.T) {
	svc := newTestServiceForHandler(&countingProvider{})
	svc.SetResolver(&stubLLMResolver{err: errors.New("dial tcp 10.0.0.5:5432 secret-dial")})

	router, tokens := buildRouter(t, svc, nil)
	server := httptest.NewServer(router)
	defer server.Close()

	resp := postChat(t, server, bearerToken(t, tokens))
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "application/json")

	var env api.ErrorResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&env))
	assert.Equal(t, http.StatusInternalServerError, env.Code)
	assert.Equal(t, "Failed to resolve LLM configuration", env.Details)
	raw, err := json.Marshal(env)
	require.NoError(t, err)
	assert.False(t, strings.Contains(string(raw), "secret-dial"), "internal error text must not reach the wire")
}

func TestHandler_AgentForwardsResolvedPerUserConfig(t *testing.T) {
	upstream, captured := newUpstreamServer(t, http.StatusOK, cannedSSE, "text/event-stream")

	proxy, err := NewAgentProxy(upstream.URL, nil)
	require.NoError(t, err)
	stack := testUserStack()
	router, tokens := newProxyHarnessWithResolver(t, proxy, &stubLLMResolver{stack: stack})
	server := httptest.NewServer(router)
	defer server.Close()

	resp := postChat(t, server, bearerToken(t, tokens))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	var decoded map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(captured.body), &decoded))
	require.Contains(t, decoded, "llm_config")
	require.Contains(t, decoded, "tavily_config")
	assert.NotContains(t, decoded, "fallback")

	var llmCfg AgentLLMConfig
	require.NoError(t, json.Unmarshal(decoded["llm_config"], &llmCfg))
	assert.Equal(t, stack.LLMBaseURL, llmCfg.BaseURL)
	assert.Equal(t, stack.LLMAPIKey, llmCfg.APIKey)
	assert.Equal(t, stack.LLMModel, llmCfg.Model)

	var tavilyCfg AgentTavilyConfig
	require.NoError(t, json.Unmarshal(decoded["tavily_config"], &tavilyCfg))
	assert.Equal(t, stack.TavilyBaseURL, tavilyCfg.BaseURL)
	assert.Equal(t, stack.TavilyAPIKey, tavilyCfg.APIKey)
}

func TestHandler_AgentPayloadOmitsTavilyWhenUnconfigured(t *testing.T) {
	srv, captured := newUpstreamServer(t, http.StatusOK, cannedSSE, "text/event-stream")
	proxy, err := NewAgentProxy(srv.URL, nil)
	require.NoError(t, err)

	stack := testUserStack()
	stack.TavilyBaseURL = ""
	stack.TavilyAPIKey = ""

	router, tokens := newProxyHarnessWithResolver(t, proxy, &stubLLMResolver{stack: stack})
	server := httptest.NewServer(router)
	defer server.Close()

	resp := postChat(t, server, bearerToken(t, tokens))
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var payload map[string]any
	require.NoError(t, json.Unmarshal([]byte(captured.body), &payload))
	assert.Contains(t, payload, "llm_config")
	assert.NotContains(t, payload, "tavily_config")
}
