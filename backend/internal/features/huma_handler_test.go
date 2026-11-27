package features

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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

func setupFeaturesRouter(t *testing.T, agentEnabled, llmEnabled bool, aiAllowlist []string) (chi.Router, auth.TokenService) {
	t.Helper()
	platapi.OverrideHumaErrors()
	tokens, err := auth.NewTokenService([]byte(auth.TestSecret), 15*time.Minute)
	require.NoError(t, err)
	router := chi.NewMux()
	router.Use(middleware.StoreRequest)
	hapi := humachi.New(router, platapi.HumaConfig())
	RegisterFeatureOps(hapi, NewHandler(agentEnabled, llmEnabled, aiAllowlist), tokens)
	return router, tokens
}

func featuresGET(t *testing.T, router chi.Router, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/features", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	return rr
}

type featuresBody struct {
	AgentEnabled bool `json:"agent_enabled"`
	ChatAllowed  bool `json:"chat_allowed"`
}

func TestGetFeatures_ReportsAgentEnabled(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		t.Run(fmt.Sprintf("agent_enabled=%t", enabled), func(t *testing.T) {
			router, tokens := setupFeaturesRouter(t, enabled, true, nil)
			token, err := tokens.GenerateToken(42, "user-42")
			require.NoError(t, err)

			rr := featuresGET(t, router, token)
			require.Equal(t, http.StatusOK, rr.Code)

			var body featuresBody
			require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
			assert.Equal(t, enabled, body.AgentEnabled)
		})
	}
}

func TestGetFeatures_ChatAllowed(t *testing.T) {
	listed := "allowed-user"
	unlisted := "other-user"

	t.Run("empty allowlist allows every authenticated user", func(t *testing.T) {
		router, tokens := setupFeaturesRouter(t, false, true, nil)
		token, err := tokens.GenerateToken(42, unlisted)
		require.NoError(t, err)

		var body featuresBody
		require.NoError(t, json.Unmarshal(featuresGET(t, router, token).Body.Bytes(), &body))
		assert.True(t, body.ChatAllowed)
	})

	t.Run("listed user is allowed", func(t *testing.T) {
		router, tokens := setupFeaturesRouter(t, false, true, []string{listed})
		token, err := tokens.GenerateToken(42, listed)
		require.NoError(t, err)

		var body featuresBody
		require.NoError(t, json.Unmarshal(featuresGET(t, router, token).Body.Bytes(), &body))
		assert.True(t, body.ChatAllowed)
	})

	t.Run("unlisted user is denied", func(t *testing.T) {
		router, tokens := setupFeaturesRouter(t, false, true, []string{listed})
		token, err := tokens.GenerateToken(42, unlisted)
		require.NoError(t, err)

		var body featuresBody
		require.NoError(t, json.Unmarshal(featuresGET(t, router, token).Body.Bytes(), &body))
		assert.False(t, body.ChatAllowed)
	})

	t.Run("LLM disabled denies everyone", func(t *testing.T) {
		router, tokens := setupFeaturesRouter(t, false, false, []string{listed})
		token, err := tokens.GenerateToken(42, listed)
		require.NoError(t, err)

		var body featuresBody
		require.NoError(t, json.Unmarshal(featuresGET(t, router, token).Body.Bytes(), &body))
		assert.False(t, body.ChatAllowed)
	})
}

func TestGetFeatures_RequiresAuth(t *testing.T) {
	router, _ := setupFeaturesRouter(t, true, true, nil)
	rr := featuresGET(t, router, "")
	assert.Equal(t, http.StatusUnauthorized, rr.Code)
}
