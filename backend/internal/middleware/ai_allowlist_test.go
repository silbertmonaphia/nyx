package middleware

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nyx/internal/platform/api"
	"nyx/internal/platform/auth"
)

func newAIAllowlistTokens(t *testing.T) auth.TokenService {
	t.Helper()
	tokens, err := auth.NewTokenService([]byte(auth.TestSecret), 15*time.Minute)
	require.NoError(t, err)
	return tokens
}

func aiGET(t *testing.T, router http.Handler, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/ai", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	return rr
}

func TestNewAIAllowlist(t *testing.T) {
	allowed := []string{"alice"}

	t.Run("empty allowlist passes every request through", func(t *testing.T) {
		r := chi.NewRouter()
		r.Use(NewAIAllowlist(nil))
		r.Get("/ai", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

		assert.Equal(t, http.StatusOK, aiGET(t, r, "").Code)
	})

	t.Run("listed user is allowed", func(t *testing.T) {
		tokens := newAIAllowlistTokens(t)
		r := chi.NewRouter()
		r.Use(StoreRequest)
		r.Use(NewAuth(tokens))
		r.Use(NewAIAllowlist(allowed))
		r.Get("/ai", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

		token, err := tokens.GenerateToken(1, "alice")
		require.NoError(t, err)
		assert.Equal(t, http.StatusOK, aiGET(t, r, token).Code)
	})

	t.Run("unlisted user is forbidden", func(t *testing.T) {
		tokens := newAIAllowlistTokens(t)
		r := chi.NewRouter()
		r.Use(StoreRequest)
		r.Use(NewAuth(tokens))
		r.Use(NewAIAllowlist(allowed))
		r.Get("/ai", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

		token, err := tokens.GenerateToken(2, "bob")
		require.NoError(t, err)
		rr := aiGET(t, r, token)
		assert.Equal(t, http.StatusForbidden, rr.Code)

		var env api.ErrorResponse
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &env))
		assert.Equal(t, aiNotAvailableMessage, env.Message)
		assert.NotContains(t, rr.Body.String(), "bob")
	})

	t.Run("missing username fails closed", func(t *testing.T) {
		r := chi.NewRouter()
		r.Use(NewAIAllowlist(allowed))
		r.Get("/ai", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

		rr := aiGET(t, r, "")
		assert.Equal(t, http.StatusUnauthorized, rr.Code)
		assert.Equal(t, wwwAuthInvalid, rr.Header().Get("WWW-Authenticate"))
	})

	t.Run("auth runs before the gate", func(t *testing.T) {
		tokens := newAIAllowlistTokens(t)
		r := chi.NewRouter()
		r.Use(StoreRequest)
		r.Use(NewAuth(tokens))
		r.Use(NewAIAllowlist(allowed))
		r.Get("/ai", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

		assert.Equal(t, http.StatusUnauthorized, aiGET(t, r, "").Code)
	})
}

type aiProbeBody struct {
	OK bool `json:"ok"`
}

type aiProbeOutput struct {
	Body aiProbeBody
}

func newGatedHumaRouter(t *testing.T, allowed []string, withAuth bool) (chi.Router, auth.TokenService) {
	t.Helper()
	api.OverrideHumaErrors()
	tokens := newAIAllowlistTokens(t)
	router := chi.NewMux()
	router.Use(StoreRequest)
	hapi := humachi.New(router, api.HumaConfig())

	middlewares := huma.Middlewares{NewHumaAIAllowlist(allowed)}
	if withAuth {
		middlewares = huma.Middlewares{NewHumaAuth(tokens), NewHumaAIAllowlist(allowed)}
	}
	huma.Register(hapi, huma.Operation{
		OperationID: "ai-probe",
		Method:      http.MethodGet,
		Path:        "/ai",
		Middlewares: middlewares,
	}, func(_ context.Context, _ *struct{}) (*aiProbeOutput, error) {
		return &aiProbeOutput{Body: aiProbeBody{OK: true}}, nil
	})
	return router, tokens
}

func TestNewHumaAIAllowlist(t *testing.T) {
	t.Run("empty allowlist passes through", func(t *testing.T) {
		router, _ := newGatedHumaRouter(t, nil, false)
		assert.Equal(t, http.StatusOK, aiGET(t, router, "").Code)
	})

	t.Run("listed user is allowed", func(t *testing.T) {
		router, tokens := newGatedHumaRouter(t, []string{"alice"}, true)
		token, err := tokens.GenerateToken(1, "alice")
		require.NoError(t, err)
		assert.Equal(t, http.StatusOK, aiGET(t, router, token).Code)
	})

	t.Run("unlisted user is forbidden", func(t *testing.T) {
		router, tokens := newGatedHumaRouter(t, []string{"alice"}, true)
		token, err := tokens.GenerateToken(2, "bob")
		require.NoError(t, err)
		rr := aiGET(t, router, token)
		assert.Equal(t, http.StatusForbidden, rr.Code)

		var env api.ErrorResponse
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &env))
		assert.Equal(t, aiNotAvailableMessage, env.Message)
	})

	t.Run("missing username fails closed", func(t *testing.T) {
		router, _ := newGatedHumaRouter(t, []string{"alice"}, false)
		rr := aiGET(t, router, "")
		assert.Equal(t, http.StatusUnauthorized, rr.Code)
		assert.Equal(t, wwwAuthInvalid, rr.Header().Get("WWW-Authenticate"))
	})

	t.Run("auth runs before the gate", func(t *testing.T) {
		router, _ := newGatedHumaRouter(t, []string{"alice"}, true)
		assert.Equal(t, http.StatusUnauthorized, aiGET(t, router, "").Code)
	})
}
