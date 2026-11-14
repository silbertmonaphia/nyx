package llmsettings

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace/noop"

	"nyx/internal/llm/ssrf"
	"nyx/internal/middleware"
	platapi "nyx/internal/platform/api"
	"nyx/internal/platform/auth"
	"nyx/internal/platform/config"
)

const plantedKey = "sk-validate-planted-key-9876"

func newValidateFixture(t *testing.T) (*Resolver, *stubRepo) {
	t.Helper()
	stubPublicResolve(t)
	svc, repo, _ := newTestService(t)
	base := &config.Config{
		LLMProbeTimeout:       "500ms",
		LLMConfigProbeTimeout: "2s",
		EmbeddingDimensions:   1024,
		BYOKAllowPrivateURL:   true,
	}
	return NewResolver(svc, base, noop.NewTracerProvider()), repo
}

func statusServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestValidate_OKAcrossSlots(t *testing.T) {
	r, _ := newValidateFixture(t)
	var chatHits atomic.Int64
	chatSrv := newChatServer(t, "pong", &chatHits)
	embedSrv, _ := newEmbedServer(t, make([]float32, 1024))
	tavilySrv, tavilyGot := newTavilyStub(t, http.StatusOK, `{"results":[]}`)

	ctx := context.Background()
	in := ValidateInput{Values: UpdateInput{
		LLMBaseURL:       chatSrv.URL + "/v1",
		LLMAPIKey:        plantedKey,
		LLMModel:         "m",
		EmbeddingBaseURL: embedSrv.URL + "/v1",
		EmbeddingAPIKey:  plantedKey,
		EmbeddingModel:   "e",
		TavilyBaseURL:    tavilySrv.URL,
		TavilyAPIKey:     "tvly-ok",
	}}
	res, err := r.Validate(ctx, 5, in)
	require.NoError(t, err)
	require.NotNil(t, res.Slots.Chat)
	require.NotNil(t, res.Slots.Embedding)
	require.NotNil(t, res.Slots.Tavily)
	require.NotNil(t, res.Slots.Fallback)
	assert.False(t, res.Slots.Fallback.Probed)
	assert.Equal(t, ProbeOK, res.Slots.Chat.Status)
	assert.Equal(t, http.StatusOK, res.Slots.Chat.HTTPStatus)
	assert.Equal(t, ProbeOK, res.Slots.Embedding.Status)
	assert.Equal(t, http.StatusOK, res.Slots.Embedding.HTTPStatus)
	assert.Equal(t, "1024 dims", res.Slots.Embedding.Detail)
	assert.Equal(t, ProbeOK, res.Slots.Tavily.Status)
	assert.GreaterOrEqual(t, chatHits.Load(), int64(1))
	require.NotNil(t, res.Slots.Chat.Request)
	assert.Equal(t, "POST", res.Slots.Chat.Request.Method)
	assert.Equal(t, chatSrv.URL+"/v1/chat/completions", res.Slots.Chat.Request.URL)
	assert.Contains(t, string(res.Slots.Chat.Request.Body), `"model":"m"`)
	require.NotNil(t, tavilyGot.load())
	assert.Equal(t, 1, tavilyGot.load().maxResults)
	assert.Equal(t, probeQueryText, tavilyGot.load().query)
}

func TestValidate_AuthFailedIsClassifiedWithUpstreamStatus(t *testing.T) {
	r, _ := newValidateFixture(t)
	srv := statusServer(t, http.StatusUnauthorized, `{"error":{"message":"bad key","type":"invalid_request_error"}}`)
	ctx := context.Background()
	res, err := r.Validate(ctx, 5, ValidateInput{Values: UpdateInput{
		LLMBaseURL: srv.URL + "/v1", LLMAPIKey: plantedKey, LLMModel: "m",
	}})
	require.NoError(t, err)
	require.NotNil(t, res.Slots.Chat)
	assert.Equal(t, ProbeAuthFailed, res.Slots.Chat.Status)
	assert.Equal(t, http.StatusUnauthorized, res.Slots.Chat.HTTPStatus)
}

func TestValidate_MatrixPerStatus(t *testing.T) {
	cases := []struct {
		name   string
		status int
		want   string
	}{
		{"forbidden", http.StatusForbidden, ProbeAuthFailed},
		{"rate limited", http.StatusTooManyRequests, ProbeRateLimited},
		{"model not found", http.StatusNotFound, ProbeInvalidModel},
		{"upstream 500", http.StatusInternalServerError, ProbeUnreachable},
		{"bad request", http.StatusBadRequest, ProbeInvalidConfig},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := newValidateFixture(t)
			srv := statusServer(t, tc.status, `{"error":{"message":"x","type":"y"}}`)
			res, err := r.Validate(context.Background(), 5, ValidateInput{Values: UpdateInput{
				LLMBaseURL: srv.URL + "/v1", LLMAPIKey: plantedKey, LLMModel: "m",
			}})
			require.NoError(t, err)
			assert.Equal(t, tc.want, res.Slots.Chat.Status)
		})
	}
}

func TestValidate_WidthMismatch(t *testing.T) {
	r, _ := newValidateFixture(t)
	embedSrv, _ := newEmbedServer(t, []float32{0.1, 0.2, 0.3})
	res, err := r.Validate(context.Background(), 5, ValidateInput{Values: UpdateInput{
		EmbeddingBaseURL: embedSrv.URL + "/v1", EmbeddingAPIKey: plantedKey, EmbeddingModel: "e",
	}})
	require.NoError(t, err)
	require.NotNil(t, res.Slots.Embedding)
	assert.Equal(t, ProbeWidthMismatch, res.Slots.Embedding.Status)
	assert.Equal(t, http.StatusOK, res.Slots.Embedding.HTTPStatus)
	assert.Contains(t, res.Slots.Embedding.Detail, "expected 1024 dims, got 3")
}

func TestValidate_TimeoutIsDistinctFromUnreachable(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(400 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"error":{"message":"late"}}`)
	}))
	t.Cleanup(slow.Close)
	svc, _, _ := newTestService(t)
	base := &config.Config{LLMConfigProbeTimeout: "50ms", BYOKAllowPrivateURL: true, EmbeddingDimensions: 1024}
	r := NewResolver(svc, base, noop.NewTracerProvider())

	res, err := r.Validate(context.Background(), 5, ValidateInput{Values: UpdateInput{
		LLMBaseURL: slow.URL + "/v1", LLMAPIKey: plantedKey, LLMModel: "m",
	}})
	require.NoError(t, err)
	assert.Equal(t, ProbeTimeout, res.Slots.Chat.Status)
	assert.Zero(t, res.Slots.Chat.HTTPStatus)
}

func TestValidate_SlowResponseAfterHeaders(t *testing.T) {
	stall := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-req.Context().Done()
	}))
	t.Cleanup(stall.Close)
	svc, _, _ := newTestService(t)
	base := &config.Config{LLMConfigProbeTimeout: "150ms", BYOKAllowPrivateURL: true, EmbeddingDimensions: 1024}
	r := NewResolver(svc, base, noop.NewTracerProvider())

	res, err := r.Validate(context.Background(), 5, ValidateInput{Values: UpdateInput{
		LLMBaseURL: stall.URL + "/v1", LLMAPIKey: plantedKey, LLMModel: "m",
	}})
	require.NoError(t, err)
	require.NotNil(t, res.Slots.Chat)
	assert.Equal(t, ProbeSlowResponse, res.Slots.Chat.Status)
	assert.Equal(t, http.StatusOK, res.Slots.Chat.HTTPStatus)
	assert.Contains(t, res.Slots.Chat.Detail, "no completion within")
}

func TestValidate_BlankKeyResolvesStoredKey(t *testing.T) {
	r, _ := newValidateFixture(t)
	embedSrv, embedRec := newEmbedServer(t, make([]float32, 1024))
	ctx := context.Background()
	require.NoError(t, r.service.Save(ctx, 7, UpdateInput{
		EmbeddingBaseURL: embedSrv.URL + "/v1",
		EmbeddingAPIKey:  plantedKey,
		EmbeddingModel:   "e",
	}))

	res, err := r.Validate(ctx, 7, ValidateInput{Values: UpdateInput{
		EmbeddingBaseURL: embedSrv.URL + "/v1",
		EmbeddingModel:   "e",
	}})
	require.NoError(t, err)
	assert.Equal(t, ProbeOK, res.Slots.Embedding.Status)
	path, _, auth := embedRec.snapshot()
	assert.True(t, strings.HasSuffix(path, "/embeddings"), "upstream must have been hit, got path %q", path)
	assert.Equal(t, "Bearer "+plantedKey, auth)
	assert.NotContains(t, res.Slots.Embedding.Request.Headers["Authorization"], plantedKey)
	assert.Contains(t, res.Slots.Embedding.Request.Headers["Authorization"], "••••9876")
}

func TestValidate_ResponseCarriesNoPlaintextKey(t *testing.T) {
	r, _ := newValidateFixture(t)
	embedSrv, _ := newEmbedServer(t, make([]float32, 1024))
	ctx := context.Background()
	require.NoError(t, r.service.Save(ctx, 7, UpdateInput{
		EmbeddingBaseURL: embedSrv.URL + "/v1",
		EmbeddingAPIKey:  plantedKey,
		EmbeddingModel:   "e",
	}))
	res, err := r.Validate(ctx, 7, ValidateInput{Values: UpdateInput{
		EmbeddingBaseURL: embedSrv.URL + "/v1",
		EmbeddingModel:   "e",
	}})
	require.NoError(t, err)
	require.NotNil(t, res.Slots.Embedding.Request)
	wire, err := json.Marshal(res)
	require.NoError(t, err)
	assert.NotContains(t, string(wire), plantedKey)
}

func TestValidate_BlankKeyRemoteUnconfiguredIsInvalidConfig(t *testing.T) {
	r, _ := newValidateFixture(t)
	res, err := r.Validate(context.Background(), 5, ValidateInput{Values: UpdateInput{
		EmbeddingBaseURL: "http://203.0.113.20:8001/v1",
		EmbeddingModel:   "e",
	}})
	require.NoError(t, err)
	assert.Equal(t, ProbeInvalidConfig, res.Slots.Embedding.Status)
}

func TestValidate_MissingModelIsInvalidConfig(t *testing.T) {
	r, _ := newValidateFixture(t)
	var hits atomic.Int64
	srv := newChatServer(t, "x", &hits)
	res, err := r.Validate(context.Background(), 5, ValidateInput{Values: UpdateInput{
		LLMBaseURL: srv.URL + "/v1", LLMAPIKey: plantedKey,
	}})
	require.NoError(t, err)
	assert.Equal(t, ProbeInvalidConfig, res.Slots.Chat.Status)
	assert.Equal(t, int64(0), hits.Load())
}

func TestValidate_EmptySlotsAreSkipped(t *testing.T) {
	r, _ := newValidateFixture(t)
	srv, _ := newEmbedServer(t, make([]float32, 1024))
	res, err := r.Validate(context.Background(), 5, ValidateInput{Values: UpdateInput{
		EmbeddingBaseURL: srv.URL + "/v1", EmbeddingAPIKey: plantedKey, EmbeddingModel: "e",
	}})
	require.NoError(t, err)
	require.NotNil(t, res.Slots.Embedding)
	assert.True(t, res.Slots.Embedding.Probed)
	for name, slot := range map[string]*SlotProbeResult{"chat": res.Slots.Chat, "fallback": res.Slots.Fallback, "tavily": res.Slots.Tavily} {
		require.NotNil(t, slot, name)
		assert.False(t, slot.Probed, name)
		assert.Empty(t, slot.Status, name)
	}
}

func TestValidate_SlotsFilterProbesOnlyNamed(t *testing.T) {
	r, _ := newValidateFixture(t)
	var chatHits atomic.Int64
	chatSrv := newChatServer(t, "x", &chatHits)
	embedSrv, _ := newEmbedServer(t, make([]float32, 1024))
	res, err := r.Validate(context.Background(), 5, ValidateInput{
		Values: UpdateInput{
			LLMBaseURL:       chatSrv.URL + "/v1",
			LLMAPIKey:        plantedKey,
			LLMModel:         "m",
			EmbeddingBaseURL: embedSrv.URL + "/v1",
			EmbeddingAPIKey:  plantedKey,
			EmbeddingModel:   "e",
		},
		Slots: []string{"embedding"},
	})
	require.NoError(t, err)
	assert.Nil(t, res.Slots.Chat)
	require.NotNil(t, res.Slots.Embedding)
	assert.Equal(t, ProbeOK, res.Slots.Embedding.Status)
	assert.Equal(t, int64(0), chatHits.Load())
}

func TestValidate_TavilyWireContractMatchesAgentShape(t *testing.T) {
	r, _ := newValidateFixture(t)
	tavilySrv, got := newTavilyStub(t, http.StatusOK, `{"results":[]}`)
	res, err := r.Validate(context.Background(), 5, ValidateInput{Values: UpdateInput{
		TavilyBaseURL: tavilySrv.URL + "/search",
		TavilyAPIKey:  "tvly-live-key-1234",
	}})
	require.NoError(t, err)
	assert.Equal(t, ProbeOK, res.Slots.Tavily.Status)
	snap := got.load()
	require.NotNil(t, snap)
	assert.Equal(t, "Bearer tvly-live-key-1234", snap.auth)
	assert.Equal(t, "", snap.accessMode)
	assert.True(t, strings.HasSuffix(snap.url, "/search"), "agent posts to base_url verbatim, got %q", snap.url)
}

func TestValidate_TavilyKeylessSendsAccessModeHeader(t *testing.T) {
	r, _ := newValidateFixture(t)
	tavilySrv, got := newTavilyStub(t, http.StatusOK, `{"results":[]}`)
	res, err := r.Validate(context.Background(), 5, ValidateInput{Values: UpdateInput{
		TavilyBaseURL: tavilySrv.URL + "/search",
	}})
	require.NoError(t, err)
	assert.Equal(t, ProbeOK, res.Slots.Tavily.Status)
	assert.Equal(t, "keyless", got.load().accessMode)
	assert.Equal(t, "", got.load().auth)
	require.NotNil(t, res.Slots.Tavily.Request)
	assert.Equal(t, "keyless", res.Slots.Tavily.Request.Headers["X-Tavily-Access-Mode"])
}

func TestValidate_TavilyStatusMapping(t *testing.T) {
	cases := []struct {
		name   string
		status int
		want   string
	}{
		{"unauthorized", http.StatusUnauthorized, ProbeAuthFailed},
		{"rate limited", http.StatusTooManyRequests, ProbeRateLimited},
		{"bad request", http.StatusBadRequest, ProbeInvalidConfig},
		{"not found", http.StatusNotFound, ProbeInvalidConfig},
		{"server error", http.StatusInternalServerError, ProbeUnreachable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := newValidateFixture(t)
			srv, _ := newTavilyStub(t, tc.status, `{}`)
			res, err := r.Validate(context.Background(), 5, ValidateInput{Values: UpdateInput{
				TavilyBaseURL: srv.URL, TavilyAPIKey: "tvly-x",
			}})
			require.NoError(t, err)
			assert.Equal(t, tc.want, res.Slots.Tavily.Status)
		})
	}
}

func TestValidate_ValidateDoesNotMutateStoredRow(t *testing.T) {
	r, repo := newValidateFixture(t)
	embedSrv, _ := newEmbedServer(t, make([]float32, 1024))
	ctx := context.Background()
	require.NoError(t, r.service.Save(ctx, 7, UpdateInput{
		EmbeddingBaseURL: embedSrv.URL + "/v1",
		EmbeddingAPIKey:  plantedKey,
		EmbeddingModel:   "e",
	}))
	before := repo.rows[7]
	callsBefore := len(repo.calls)
	_, err := r.Validate(ctx, 7, ValidateInput{Values: UpdateInput{
		EmbeddingBaseURL: embedSrv.URL + "/v1",
		EmbeddingModel:   "e",
	}})
	require.NoError(t, err)
	assert.Same(t, before, repo.rows[7])
	for _, c := range repo.calls[callsBefore:] {
		assert.NotEqual(t, "upsert", c)
	}
}

func TestValidate_SlotIndependence(t *testing.T) {
	r, _ := newValidateFixture(t)
	badSrv := statusServer(t, http.StatusUnauthorized, `{"error":{"message":"x"}}`)
	embedSrv, _ := newEmbedServer(t, make([]float32, 1024))
	tavilySrv, _ := newTavilyStub(t, http.StatusOK, `{"results":[]}`)
	res, err := r.Validate(context.Background(), 5, ValidateInput{Values: UpdateInput{
		LLMBaseURL:       badSrv.URL + "/v1",
		LLMAPIKey:        plantedKey,
		LLMModel:         "m",
		EmbeddingBaseURL: embedSrv.URL + "/v1",
		EmbeddingAPIKey:  plantedKey,
		EmbeddingModel:   "e",
		TavilyBaseURL:    tavilySrv.URL,
		TavilyAPIKey:     "tvly-x",
	}})
	require.NoError(t, err)
	assert.Equal(t, ProbeAuthFailed, res.Slots.Chat.Status)
	assert.Equal(t, ProbeOK, res.Slots.Embedding.Status)
	assert.Equal(t, ProbeOK, res.Slots.Tavily.Status)
}

type tavilySnapshot struct {
	url        string
	auth       string
	accessMode string
	query      string
	maxResults int
}

type tavilyRecorder struct {
	mu   sync.Mutex
	snap *tavilySnapshot
}

func (t *tavilyRecorder) load() *tavilySnapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.snap
}

func newTavilyStub(t *testing.T, status int, body string) (*httptest.Server, *tavilyRecorder) {
	t.Helper()
	rec := &tavilyRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var payload struct {
			Query      string `json:"query"`
			MaxResults int    `json:"max_results"`
		}
		_ = json.NewDecoder(req.Body).Decode(&payload)
		rec.mu.Lock()
		rec.snap = &tavilySnapshot{
			url:        req.URL.String(),
			auth:       req.Header.Get("Authorization"),
			accessMode: req.Header.Get("X-Tavily-Access-Mode"),
			query:      payload.Query,
			maxResults: payload.MaxResults,
		}
		rec.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

func setupValidateRouter(t *testing.T, allow func(int) bool) (*chi.Mux, auth.TokenService) {
	t.Helper()
	platapi.OverrideHumaErrors()
	svc, _, _ := newTestService(t)
	base := &config.Config{LLMConfigProbeTimeout: "2s", EmbeddingDimensions: 1024, BYOKAllowPrivateURL: true}
	resolver := NewResolver(svc, base, noop.NewTracerProvider())
	tokens, err := auth.NewTokenService([]byte(auth.TestSecret), 15*time.Minute)
	require.NoError(t, err)
	handler := NewHandler(svc)
	handler.SetValidator(resolver.ValidateValues)
	handler.SetValidateLimiter(allow)
	router := chi.NewMux()
	router.Use(middleware.StoreRequest)
	hapi := humachi.New(router, platapi.HumaConfig())
	RegisterLLMConfigOpsTest(hapi, handler, tokens, true)
	return router, tokens
}

func validateRequest(t *testing.T, router *chi.Mux, token string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/me/llm-config/validate", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	return rr
}

func TestHandler_ValidateProbesSingleSlotOverTheWire(t *testing.T) {
	stubPublicResolve(t)
	var chatHits atomic.Int64
	chatSrv := newChatServer(t, "pong", &chatHits)
	router, tokens := setupValidateRouter(t, func(int) bool { return true })
	tok := mustToken(t, tokens, 5)

	rr := validateRequest(t, router, tok, map[string]any{
		"llm":   map[string]any{"base_url": chatSrv.URL + "/v1", "api_key": plantedKey, "model": "m"},
		"slots": []string{"chat"},
	})
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var out struct {
		Slots struct {
			Chat *struct {
				Probed  bool   `json:"probed"`
				Status  string `json:"status"`
				Request *struct {
					Method  string            `json:"method"`
					URL     string            `json:"url"`
					Headers map[string]string `json:"headers"`
				} `json:"request"`
			} `json:"chat"`
		} `json:"slots"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out))
	t.Log(rr.Body.String())
	require.NotNil(t, out.Slots.Chat)
	assert.True(t, out.Slots.Chat.Probed)
	assert.Equal(t, ProbeOK, out.Slots.Chat.Status)
	assert.Equal(t, "POST", out.Slots.Chat.Request.Method)
	assert.NotContains(t, out.Slots.Chat.Request.Headers["Authorization"], plantedKey)
}

func TestHandler_ValidateRateLimited(t *testing.T) {
	stubPublicResolve(t)
	var upstreamHits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data":   []map[string]any{{"object": "embedding", "index": 0, "embedding": make([]float32, 1024)}},
			"model":  "e",
		})
	}))
	t.Cleanup(srv.Close)
	calls := 0
	router, tokens := setupValidateRouter(t, func(int) bool { calls++; return calls == 1 })
	tok := mustToken(t, tokens, 5)

	body := map[string]any{
		"embedding": map[string]any{"base_url": srv.URL + "/v1", "api_key": plantedKey, "model": "e"},
	}
	first := validateRequest(t, router, tok, body)
	assert.Equal(t, http.StatusOK, first.Code, first.Body.String())
	hitsAfterFirst := upstreamHits.Load()
	assert.GreaterOrEqual(t, hitsAfterFirst, int64(1))
	second := validateRequest(t, router, tok, body)
	assert.Equal(t, http.StatusTooManyRequests, second.Code, second.Body.String())
	assert.Equal(t, hitsAfterFirst, upstreamHits.Load(), "a limited call must not dial upstream")
}

func TestHandler_ValidateUnauthenticated(t *testing.T) {
	router, _ := setupValidateRouter(t, func(int) bool { return true })
	rr := validateRequest(t, router, "", map[string]any{})
	assert.Equal(t, http.StatusUnauthorized, rr.Code)
}

func TestValidate_KeyNeverAppearsInLogs(t *testing.T) {
	orig := log.Logger
	var buf bytes.Buffer
	log.Logger = zerolog.New(&buf)
	t.Cleanup(func() { log.Logger = orig })

	r, _ := newValidateFixture(t)
	embedSrv, embedRec := newEmbedServer(t, make([]float32, 1024))
	ctx := context.Background()
	require.NoError(t, r.service.Save(ctx, 7, UpdateInput{
		EmbeddingBaseURL: embedSrv.URL + "/v1",
		EmbeddingAPIKey:  plantedKey,
		EmbeddingModel:   "e",
	}))
	res, err := r.Validate(ctx, 7, ValidateInput{Values: UpdateInput{
		EmbeddingBaseURL: embedSrv.URL + "/v1",
		EmbeddingModel:   "e",
	}})
	require.NoError(t, err)
	require.NotNil(t, res.Slots.Embedding)

	path, _, auth := embedRec.snapshot()
	assert.True(t, strings.HasSuffix(path, "/embeddings"), "probe must demonstrably have run, got path %q", path)
	assert.Equal(t, "Bearer "+plantedKey, auth, "upstream must have received the real key for the absence claim to be meaningful")

	assert.NotContains(t, buf.String(), plantedKey)
	assert.Contains(t, buf.String(), `"slot":"embedding"`)
}

func TestValidate_LoopbackRefusedWhenPrivateDenied(t *testing.T) {
	svc, _, _ := newTestService(t)
	base := &config.Config{LLMConfigProbeTimeout: "2s", EmbeddingDimensions: 1024, BYOKAllowPrivateURL: false}
	r := NewResolver(svc, base, noop.NewTracerProvider())

	res, err := r.Validate(context.Background(), 5, ValidateInput{Values: UpdateInput{
		EmbeddingBaseURL: "http://127.0.0.1:8000/v1",
		EmbeddingAPIKey:  plantedKey,
		EmbeddingModel:   "e",
	}})
	require.NoError(t, err)
	require.NotNil(t, res.Slots.Embedding)
	assert.Equal(t, ProbeUnreachable, res.Slots.Embedding.Status)
	assert.Nil(t, res.Slots.Embedding.Request, "an SSRF-refused probe never left the server")
}

func TestValidate_DNSFailureIsUnreachable(t *testing.T) {
	svc, _, _ := newTestService(t)
	orig := ssrf.ResolveHost
	ssrf.ResolveHost = func(string) ([]net.IP, error) { return nil, errors.New("no such host") }
	t.Cleanup(func() { ssrf.ResolveHost = orig })
	base := &config.Config{LLMConfigProbeTimeout: "2s", EmbeddingDimensions: 1024, BYOKAllowPrivateURL: false}
	r := NewResolver(svc, base, noop.NewTracerProvider())

	res, err := r.Validate(context.Background(), 5, ValidateInput{Values: UpdateInput{
		EmbeddingBaseURL: "http://unresolvable.example/v1",
		EmbeddingAPIKey:  plantedKey,
		EmbeddingModel:   "e",
	}})
	require.NoError(t, err)
	require.NotNil(t, res.Slots.Embedding)
	assert.Equal(t, ProbeUnreachable, res.Slots.Embedding.Status)
}

func TestValidate_PopulatedFallbackSlotProbes(t *testing.T) {
	r, _ := newValidateFixture(t)
	var hits atomic.Int64
	fallbackSrv := newChatServer(t, "pong", &hits)
	res, err := r.Validate(context.Background(), 5, ValidateInput{Values: UpdateInput{
		FallbackBaseURL: fallbackSrv.URL + "/v1",
		FallbackAPIKey:  plantedKey,
		FallbackModel:   "fm",
	}, Slots: []string{"fallback"}})
	require.NoError(t, err)
	require.NotNil(t, res.Slots.Fallback)
	assert.True(t, res.Slots.Fallback.Probed)
	assert.Equal(t, ProbeOK, res.Slots.Fallback.Status)
	assert.Equal(t, http.StatusOK, res.Slots.Fallback.HTTPStatus)
	assert.GreaterOrEqual(t, hits.Load(), int64(1))
}

func TestValidate_UserinfoBaseURLIsInvalidConfig(t *testing.T) {
	r, _ := newValidateFixture(t)
	res, err := r.Validate(context.Background(), 5, ValidateInput{Values: UpdateInput{
		EmbeddingBaseURL: "https://key:secret@embed.example.test/v1",
		EmbeddingAPIKey:  plantedKey,
		EmbeddingModel:   "e",
	}})
	require.NoError(t, err)
	require.NotNil(t, res.Slots.Embedding)
	assert.Equal(t, ProbeInvalidConfig, res.Slots.Embedding.Status)
	assert.Nil(t, res.Slots.Embedding.Request, "a rejected URL must never be dialed or echoed")
	wire, merr := json.Marshal(res)
	require.NoError(t, merr)
	assert.NotContains(t, string(wire), "key:secret")
}

func TestValidate_TavilyUserinfoRejected(t *testing.T) {
	r, _ := newValidateFixture(t)
	res, err := r.Validate(context.Background(), 5, ValidateInput{Values: UpdateInput{
		TavilyBaseURL: "https://k:s@api.tavily.test/search",
		TavilyAPIKey:  "tvly-x",
	}})
	require.NoError(t, err)
	require.NotNil(t, res.Slots.Tavily)
	assert.Equal(t, ProbeInvalidConfig, res.Slots.Tavily.Status)
	assert.Nil(t, res.Slots.Tavily.Request)
}
