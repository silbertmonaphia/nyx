package llmsettings

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nyx/internal/middleware"
	platapi "nyx/internal/platform/api"
	"nyx/internal/platform/auth"
)

func setupConfigRouter(t *testing.T) (*chi.Mux, *Service, *stubRepo, auth.TokenService) {
	t.Helper()
	platapi.OverrideHumaErrors()
	svc, repo, _ := newTestService(t)
	tokens, err := auth.NewTokenService([]byte(auth.TestSecret), 15*time.Minute)
	require.NoError(t, err)
	router := chi.NewMux()
	router.Use(middleware.StoreRequest)
	hapi := humachi.New(router, platapi.HumaConfig())
	RegisterLLMConfigOpsTest(hapi, NewHandler(svc), tokens, true)
	return router, svc, repo, tokens
}

func mustToken(t *testing.T, tokens auth.TokenService, userID int) string {
	t.Helper()
	tok, err := tokens.GenerateToken(userID, fmt.Sprintf("user-%d", userID))
	require.NoError(t, err)
	return tok
}

func configRequest(t *testing.T, router *chi.Mux, method, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		reader = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, "/api/me/llm-config", reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	return rr
}

func validPutBody() map[string]any {
	return map[string]any{
		"llm": map[string]any{
			"base_url": "http://localhost:8000/v1",
			"api_key":  "sk-real-user-key-0001",
			"model":    "chat-model",
		},
		"embedding": map[string]any{
			"base_url": "http://localhost:8001/v1",
			"api_key":  "sk-real-embed-key-0002",
			"model":    "embed-model",
		},
		"tavily": map[string]any{
			"base_url": "https://api.tavily.com/search",
			"api_key":  "tvly-real-key-0003",
		},
	}
}

func decodeConfig(t *testing.T, rr *httptest.ResponseRecorder) configResponse {
	t.Helper()
	var got configResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &got))
	return got
}

func TestHandler_GetUnconfigured(t *testing.T) {
	router, _, _, tokens := setupConfigRouter(t)
	tok := mustToken(t, tokens, 1)

	rr := configRequest(t, router, http.MethodGet, tok, nil)
	require.Equal(t, http.StatusOK, rr.Code)

	got := decodeConfig(t, rr)
	assert.False(t, got.Configured)
	assert.Nil(t, got.LLM)
	assert.Nil(t, got.Embedding)
	assert.Nil(t, got.Tavily)
	assert.Nil(t, got.Fallback)

	body := rr.Body.String()
	assert.Contains(t, body, `"llm":null`)
	assert.Contains(t, body, `"embedding":null`)
	assert.Contains(t, body, `"tavily":null`)
	assert.Contains(t, body, `"fallback":null`)
}

func TestHandler_GetMasksKeys(t *testing.T) {
	router, svc, _, tokens := setupConfigRouter(t)
	require.NoError(t, svc.Save(context.Background(), 1, validCreateInput()))
	tok := mustToken(t, tokens, 1)

	rr := configRequest(t, router, http.MethodGet, tok, nil)
	require.Equal(t, http.StatusOK, rr.Code)

	got := decodeConfig(t, rr)
	require.True(t, got.Configured)
	require.NotNil(t, got.LLM)
	require.NotNil(t, got.Embedding)
	require.NotNil(t, got.Tavily)
	assert.Nil(t, got.Fallback)

	assert.Equal(t, "http://localhost:8000/v1", got.LLM.BaseURL)
	assert.Equal(t, "Qwen/Qwen3-4B-Instruct-2507-FP8", got.LLM.Model)
	assert.Equal(t, "…0001", got.LLM.APIKeyMasked)
	assert.Equal(t, "…0002", got.Embedding.APIKeyMasked)
	assert.Equal(t, "…0003", got.Tavily.APIKeyMasked)

	body := rr.Body.String()
	assert.NotContains(t, body, "sk-real-user-key-0001")
	assert.NotContains(t, body, "sk-real-embed-key-0002")
	assert.NotContains(t, body, "tvly-real-key-0003")
	assert.NotContains(t, body, `"api_key"`)
}

func TestHandler_PutCreateAndBlankKeyUpdateKeepsStored(t *testing.T) {
	router, _, repo, tokens := setupConfigRouter(t)
	tok := mustToken(t, tokens, 1)

	rr := configRequest(t, router, http.MethodPut, tok, validPutBody())
	require.Equal(t, http.StatusOK, rr.Code)
	created := decodeConfig(t, rr)
	require.True(t, created.Configured)
	require.NotNil(t, created.LLM)
	assert.Equal(t, "…0001", created.LLM.APIKeyMasked)
	original := repo.rows[1]
	require.NotNil(t, original)
	originalEnc := original.LLMAPIKeyEnc

	update := validPutBody()
	update["llm"].(map[string]any)["base_url"] = "http://moved:8000/v1"
	update["llm"].(map[string]any)["api_key"] = ""
	update["embedding"].(map[string]any)["api_key"] = ""
	update["tavily"].(map[string]any)["api_key"] = ""

	rr = configRequest(t, router, http.MethodPut, tok, update)
	require.Equal(t, http.StatusOK, rr.Code)
	updated := decodeConfig(t, rr)
	require.NotNil(t, updated.LLM)
	assert.Equal(t, "http://moved:8000/v1", updated.LLM.BaseURL)
	assert.Equal(t, "…0001", updated.LLM.APIKeyMasked)
	assert.Equal(t, originalEnc, repo.rows[1].LLMAPIKeyEnc)
	assert.Equal(t, "…0002", updated.Embedding.APIKeyMasked)
	assert.Equal(t, "…0003", updated.Tavily.APIKeyMasked)
}

func TestHandler_PutRejectsInvalidFieldsWithFieldDetails(t *testing.T) {
	cases := []struct {
		name  string
		body  map[string]any
		field string
	}{
		{
			name: "invalid base url",
			body: func() map[string]any {
				b := validPutBody()
				b["embedding"].(map[string]any)["base_url"] = "gopher://embed:8001"
				return b
			}(),
			field: "embedding_base_url",
		},
		{
			name: "placeholder key",
			body: func() map[string]any {
				b := validPutBody()
				b["llm"].(map[string]any)["api_key"] = "changeme"
				return b
			}(),
			field: "llm_api_key",
		},
		{
			name: "missing required key on create",
			body: func() map[string]any {
				b := validPutBody()
				llm := b["llm"].(map[string]any)
				llm["base_url"] = testRemoteLLMBaseURL
				delete(llm, "api_key")
				return b
			}(),
			field: "llm_api_key",
		},
		{
			name: "partial fallback group",
			body: func() map[string]any {
				b := validPutBody()
				b["fallback"] = map[string]any{"base_url": "http://fallback:8000/v1"}
				return b
			}(),
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			router, _, repo, tokens := setupConfigRouter(t)
			tok := mustToken(t, tokens, 1)

			rr := configRequest(t, router, http.MethodPut, tok, c.body)
			require.Equal(t, http.StatusBadRequest, rr.Code)

			var env platapi.ErrorResponse
			require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &env))
			assert.Equal(t, http.StatusBadRequest, env.Code)
			if c.field != "" {
				assert.Equal(t, c.field, env.Details)
			} else {
				assert.NotNil(t, env.Details)
			}
			assert.Nil(t, repo.rows[1], "a rejected PUT must not write a row")
		})
	}
}

func TestHandler_PutFallbackRoundTrips(t *testing.T) {
	router, _, _, tokens := setupConfigRouter(t)
	tok := mustToken(t, tokens, 1)

	body := validPutBody()
	body["fallback"] = map[string]any{
		"base_url": "http://fallback:8000/v1",
		"api_key":  "sk-fallback-key-0009",
		"model":    "fallback-model",
	}

	rr := configRequest(t, router, http.MethodPut, tok, body)
	require.Equal(t, http.StatusOK, rr.Code)
	got := decodeConfig(t, rr)
	require.NotNil(t, got.Fallback)
	assert.Equal(t, "http://fallback:8000/v1", got.Fallback.BaseURL)
	assert.Equal(t, "fallback-model", got.Fallback.Model)
	assert.Equal(t, "…0009", got.Fallback.APIKeyMasked)
}

func TestHandler_PutLoopbackWithoutKeys(t *testing.T) {
	router, _, _, tokens := setupConfigRouter(t)
	tok := mustToken(t, tokens, 1)

	body := validPutBody()
	delete(body["llm"].(map[string]any), "api_key")
	delete(body["embedding"].(map[string]any), "api_key")
	body["tavily"] = map[string]any{
		"base_url": "http://127.0.0.1:8899/search",
	}

	rr := configRequest(t, router, http.MethodPut, tok, body)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	got := decodeConfig(t, rr)
	require.NotNil(t, got.LLM)
	assert.Equal(t, "", got.LLM.APIKeyMasked)
	require.NotNil(t, got.Tavily)
	assert.Equal(t, "", got.Tavily.APIKeyMasked)

	rr = configRequest(t, router, http.MethodGet, tok, nil)
	require.Equal(t, http.StatusOK, rr.Code)
	assert.True(t, decodeConfig(t, rr).Configured)
}

func TestHandler_PutWithoutEmbeddingIsAccepted(t *testing.T) {
	router, _, _, tokens := setupConfigRouter(t)
	tok := mustToken(t, tokens, 1)

	body := validPutBody()
	delete(body, "embedding")

	rr := configRequest(t, router, http.MethodPut, tok, body)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	got := decodeConfig(t, rr)
	assert.True(t, got.Configured)
	assert.Nil(t, got.Embedding)

	rr = configRequest(t, router, http.MethodGet, tok, nil)
	require.Equal(t, http.StatusOK, rr.Code)
	assert.Nil(t, decodeConfig(t, rr).Embedding)
}

func TestHandler_PutWithoutTavilyIsAccepted(t *testing.T) {
	router, _, _, tokens := setupConfigRouter(t)
	tok := mustToken(t, tokens, 1)

	body := validPutBody()
	delete(body, "tavily")

	rr := configRequest(t, router, http.MethodPut, tok, body)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	got := decodeConfig(t, rr)
	assert.True(t, got.Configured)
	assert.Nil(t, got.Tavily)

	rr = configRequest(t, router, http.MethodGet, tok, nil)
	require.Equal(t, http.StatusOK, rr.Code)
	assert.Nil(t, decodeConfig(t, rr).Tavily)
}

func TestHandler_PutClearsStoredFallback(t *testing.T) {
	router, _, _, tokens := setupConfigRouter(t)
	tok := mustToken(t, tokens, 1)

	body := validPutBody()
	body["fallback"] = map[string]any{
		"base_url": "http://fallback:8000/v1",
		"api_key":  "sk-fallback-key-0009",
		"model":    "fallback-model",
	}
	rr := configRequest(t, router, http.MethodPut, tok, body)
	require.Equal(t, http.StatusOK, rr.Code)

	clearBody := validPutBody()
	clearBody["fallback"] = nil
	rr = configRequest(t, router, http.MethodPut, tok, clearBody)
	require.Equal(t, http.StatusOK, rr.Code)
	assert.Nil(t, decodeConfig(t, rr).Fallback)

	rr = configRequest(t, router, http.MethodGet, tok, nil)
	require.Equal(t, http.StatusOK, rr.Code)
	assert.Nil(t, decodeConfig(t, rr).Fallback)
}

func TestHandler_DeleteIsIdempotent(t *testing.T) {
	router, svc, _, tokens := setupConfigRouter(t)
	require.NoError(t, svc.Save(context.Background(), 1, validCreateInput()))
	tok := mustToken(t, tokens, 1)

	rr := configRequest(t, router, http.MethodDelete, tok, nil)
	assert.Equal(t, http.StatusNoContent, rr.Code)
	assert.Empty(t, rr.Body.String())

	rr = configRequest(t, router, http.MethodDelete, tok, nil)
	assert.Equal(t, http.StatusNoContent, rr.Code)
	assert.Empty(t, rr.Body.String())

	rr = configRequest(t, router, http.MethodGet, tok, nil)
	require.Equal(t, http.StatusOK, rr.Code)
	assert.False(t, decodeConfig(t, rr).Configured)
}

func TestHandler_UnauthenticatedRefused(t *testing.T) {
	router, _, _, _ := setupConfigRouter(t)

	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			var body any
			if method == http.MethodPut {
				body = validPutBody()
			}
			rr := configRequest(t, router, method, "", body)
			assert.Equal(t, http.StatusUnauthorized, rr.Code)
			assert.NotEmpty(t, rr.Header().Get("WWW-Authenticate"))
		})
	}
}

func TestHandler_CrossUserIsolation(t *testing.T) {
	router, svc, repo, tokens := setupConfigRouter(t)
	require.NoError(t, svc.Save(context.Background(), 1, validCreateInput()))
	tokenA := mustToken(t, tokens, 1)
	tokenB := mustToken(t, tokens, 2)

	rr := configRequest(t, router, http.MethodGet, tokenB, nil)
	require.Equal(t, http.StatusOK, rr.Code)
	assert.False(t, decodeConfig(t, rr).Configured)

	bodyB := validPutBody()
	bodyB["llm"].(map[string]any)["model"] = "b-model"
	rr = configRequest(t, router, http.MethodPut, tokenB, bodyB)
	require.Equal(t, http.StatusOK, rr.Code)
	require.NotNil(t, repo.rows[2])
	assert.Equal(t, "b-model", repo.rows[2].LLMModel)
	assert.Equal(t, "Qwen/Qwen3-4B-Instruct-2507-FP8", repo.rows[1].LLMModel, "user A's row is untouched")

	rr = configRequest(t, router, http.MethodDelete, tokenB, nil)
	require.Equal(t, http.StatusNoContent, rr.Code)
	assert.Nil(t, repo.rows[2])
	require.NotNil(t, repo.rows[1], "user B's delete cannot remove user A's row")

	rr = configRequest(t, router, http.MethodGet, tokenA, nil)
	require.Equal(t, http.StatusOK, rr.Code)
	got := decodeConfig(t, rr)
	require.True(t, got.Configured)
	assert.Equal(t, "Qwen/Qwen3-4B-Instruct-2507-FP8", got.LLM.Model)
}

func TestHandler_KeysAreWriteOnlyInSpec(t *testing.T) {
	platapi.OverrideHumaErrors()
	tokens, err := auth.NewTokenService([]byte(auth.TestSecret), 15*time.Minute)
	require.NoError(t, err)
	router := chi.NewMux()
	router.Use(middleware.StoreRequest)
	hapi := humachi.New(router, platapi.HumaConfig())
	RegisterLLMConfigOpsTest(hapi, NewHandler(nil), tokens, false)

	spec, err := json.Marshal(hapi.OpenAPI())
	require.NoError(t, err)
	assert.True(t, strings.Contains(string(spec), `"writeOnly":true`), "API keys must be marked write-only in the spec")
}
