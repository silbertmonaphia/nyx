package llmsettings

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace/noop"

	"nyx/internal/llm"
	"nyx/internal/platform/config"
)

type embedRecorder struct {
	mu    sync.Mutex
	path  string
	model string
	auth  string
}

func (r *embedRecorder) record(req *http.Request, model string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.path = req.URL.Path
	r.model = model
	r.auth = req.Header.Get("Authorization")
}

func (r *embedRecorder) snapshot() (string, string, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.path, r.model, r.auth
}

func newEmbedServer(t *testing.T, vec []float32) (*httptest.Server, *embedRecorder) {
	t.Helper()
	rec := &embedRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(req.Body).Decode(&body)
		rec.record(req, body.Model)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data": []map[string]any{
				{"object": "embedding", "index": 0, "embedding": vec},
			},
			"model": body.Model,
		})
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

func newChatServer(t *testing.T, content string, hits *atomic.Int64) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		hits.Add(1)
		switch {
		case req.Method == http.MethodGet && req.URL.Path == "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"object":"list","data":[]}`)
		case req.Method == http.MethodPost && req.URL.Path == "/v1/chat/completions":
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			chunk := fmt.Sprintf(`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":%q},"finish_reason":null}]}`+"\n\n", content)
			_, _ = io.WriteString(w, chunk)
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newResolverFixture(t *testing.T) (*Resolver, *Service) {
	t.Helper()
	svc, _, _ := newTestService(t)
	base := &config.Config{LLMProbeTimeout: "500ms"}
	return NewResolver(svc, base, noop.NewTracerProvider()), svc
}

func inputFor(chatURL, embedURL string) UpdateInput {
	return UpdateInput{
		LLMBaseURL:       chatURL + "/v1",
		LLMAPIKey:        "sk-user-chat-key-1111",
		LLMModel:         "user-chat-model",
		EmbeddingBaseURL: embedURL + "/v1",
		EmbeddingAPIKey:  "sk-user-embed-key-2222",
		EmbeddingModel:   "user-embed-model",
		TavilyBaseURL:    "https://api.tavily.com/search",
		TavilyAPIKey:     "tvly-user-key-3333",
	}
}

func TestResolver_ResolvesChatAndEmbeddingClientsPerUser(t *testing.T) {
	r, svc := newResolverFixture(t)
	chatSrv, chatRec := newEmbedServer(t, []float32{1, 2, 3})
	embedSrv, embedRec := newEmbedServer(t, []float32{4, 5, 6})

	ctx := context.Background()
	require.NoError(t, svc.Save(ctx, 7, inputFor(chatSrv.URL, embedSrv.URL)))

	stack, err := r.Resolve(ctx, 7)
	require.NoError(t, err)
	require.NotNil(t, stack.Chat)
	require.NotNil(t, stack.Embed)
	assert.Equal(t, "user-embed-model", stack.EmbedModel)
	assert.Equal(t, "sk-user-chat-key-1111", stack.LLMAPIKey)
	assert.Equal(t, "sk-user-embed-key-2222", stack.EmbeddingAPIKey)
	assert.Equal(t, "tvly-user-key-3333", stack.TavilyAPIKey)
	assert.Equal(t, chatSrv.URL+"/v1", stack.LLMBaseURL)
	assert.Equal(t, embedSrv.URL+"/v1", stack.EmbeddingBaseURL)
	assert.Empty(t, stack.FallbackBaseURL, "no fallback configured")

	vecs, err := stack.Embed.Embed(ctx, llm.EmbedRequest{Model: stack.EmbedModel, Inputs: []string{"hello"}})
	require.NoError(t, err)
	require.Len(t, vecs, 1)
	assert.Equal(t, []float32{4, 5, 6}, vecs[0], "the embedding slot serves the embedding vector")

	path, model, auth := embedRec.snapshot()
	assert.Equal(t, "/v1/embeddings", path)
	assert.Equal(t, "user-embed-model", model)
	assert.Equal(t, "Bearer sk-user-embed-key-2222", auth)

	chatPath, _, _ := chatRec.snapshot()
	assert.Empty(t, chatPath, "the embedding slot must not send traffic to the chat endpoint")

	vecs, err = stack.Chat.Embed(ctx, llm.EmbedRequest{Model: stack.LLMModel, Inputs: []string{"hi"}})
	require.NoError(t, err)
	require.Len(t, vecs, 1)
	assert.Equal(t, []float32{1, 2, 3}, vecs[0])

	path, model, auth = chatRec.snapshot()
	assert.Equal(t, "/v1/embeddings", path)
	assert.Equal(t, "user-chat-model", model)
	assert.Equal(t, "Bearer sk-user-chat-key-1111", auth)

	embedPath, _, _ := embedRec.snapshot()
	assert.Equal(t, "/v1/embeddings", embedPath)

	second, err := r.Resolve(ctx, 7)
	require.NoError(t, err)
	assert.True(t, stack.Chat == second.Chat, "unchanged settings reuse the cached chat client")
	assert.True(t, stack.Embed == second.Embed, "unchanged settings reuse the cached embedding client")
}

func TestResolver_RebuildsClientsAfterUpdate(t *testing.T) {
	r, svc := newResolverFixture(t)
	chatSrv, _ := newEmbedServer(t, []float32{1})
	movedSrv, movedRec := newEmbedServer(t, []float32{2})
	embedSrv, _ := newEmbedServer(t, []float32{3})

	ctx := context.Background()
	require.NoError(t, svc.Save(ctx, 2, inputFor(chatSrv.URL, embedSrv.URL)))
	first, err := r.Resolve(ctx, 2)
	require.NoError(t, err)

	moved := inputFor(movedSrv.URL, embedSrv.URL)
	moved.LLMAPIKey = ""
	moved.EmbeddingAPIKey = ""
	moved.TavilyAPIKey = ""
	require.NoError(t, svc.Save(ctx, 2, moved))

	second, err := r.Resolve(ctx, 2)
	require.NoError(t, err)
	assert.True(t, first.Chat != second.Chat, "a moved chat endpoint must rebuild the chat client")
	assert.True(t, first.Embed == second.Embed, "an unchanged embedding slot keeps its cached client")

	_, err = second.Chat.Embed(ctx, llm.EmbedRequest{Model: second.LLMModel, Inputs: []string{"x"}})
	require.NoError(t, err)
	_, model, _ := movedRec.snapshot()
	assert.Equal(t, "user-chat-model", model)
}

func TestResolver_NotFoundPassesThrough(t *testing.T) {
	r, _ := newResolverFixture(t)

	_, err := r.Resolve(context.Background(), 404)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestResolver_CacheCapEvicts(t *testing.T) {
	r, svc := newResolverFixture(t)
	chatSrv, _ := newEmbedServer(t, []float32{1})
	embedSrv, _ := newEmbedServer(t, []float32{2})

	ctx := context.Background()
	for i := 1; i <= maxResolverCacheEntries+6; i++ {
		in := inputFor(chatSrv.URL, embedSrv.URL)
		in.LLMModel = fmt.Sprintf("chat-model-%d", i)
		in.EmbeddingModel = fmt.Sprintf("embed-model-%d", i)
		require.NoError(t, svc.Save(ctx, i, in))
		_, err := r.Resolve(ctx, i)
		require.NoError(t, err)
	}

	r.mu.Lock()
	size := len(r.cache)
	r.mu.Unlock()
	assert.LessOrEqual(t, size, maxResolverCacheEntries, "the client cache is capped")
	assert.Equal(t, maxResolverCacheEntries, size)

	_, err := r.Resolve(ctx, 1)
	require.NoError(t, err, "an evicted entry is rebuilt on the next resolve")

	r.mu.Lock()
	size = len(r.cache)
	r.mu.Unlock()
	assert.LessOrEqual(t, size, maxResolverCacheEntries)
}

func TestResolver_BuildsRouterWhenFallbackConfigured(t *testing.T) {
	r, svc := newResolverFixture(t)
	var primaryHits, fallbackHits atomic.Int64
	primarySrv := newChatServer(t, "primary-delta", &primaryHits)
	fallbackSrv := newChatServer(t, "fallback-delta", &fallbackHits)
	embedSrv, _ := newEmbedServer(t, []float32{1})

	in := inputFor(primarySrv.URL, embedSrv.URL)
	in.FallbackBaseURL = fallbackSrv.URL + "/v1"
	in.FallbackAPIKey = "sk-user-fallback-key-4444"
	in.FallbackModel = testFallbackModel
	require.NoError(t, svc.Save(context.Background(), 3, in))

	ctx := context.Background()
	stack, err := r.Resolve(ctx, 3)
	require.NoError(t, err)
	router, ok := stack.Chat.(*llm.Router)
	require.True(t, ok, "a configured fallback wraps the chat slot in a router")

	var deltas []string
	_, err = router.Chat(ctx, llm.ChatRequest{
		Messages: []llm.Message{{Role: "user", Content: "hello"}},
	}, func(delta string, _ *llm.ChatUsage) error {
		if delta != "" {
			deltas = append(deltas, delta)
		}
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"primary-delta"}, deltas)
	assert.Zero(t, fallbackHits.Load(), "a healthy version-prefixed primary is not failed over")
	assert.GreaterOrEqual(t, primaryHits.Load(), int64(2), "probe plus streaming call hit the primary")

	again, err := r.Resolve(ctx, 3)
	require.NoError(t, err)
	assert.True(t, stack.Chat == again.Chat, "the router itself is cached")
}
