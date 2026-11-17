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

func setupFeaturesRouter(t *testing.T, agentEnabled bool) (chi.Router, auth.TokenService) {
	t.Helper()
	platapi.OverrideHumaErrors()
	tokens, err := auth.NewTokenService([]byte(auth.TestSecret), 15*time.Minute)
	require.NoError(t, err)
	router := chi.NewMux()
	router.Use(middleware.StoreRequest)
	hapi := humachi.New(router, platapi.HumaConfig())
	RegisterFeatureOps(hapi, NewHandler(agentEnabled), tokens)
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

func TestGetFeatures_ReportsAgentEnabled(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		t.Run(fmt.Sprintf("agent_enabled=%t", enabled), func(t *testing.T) {
			router, tokens := setupFeaturesRouter(t, enabled)
			token, err := tokens.GenerateToken(42, "user-42")
			require.NoError(t, err)

			rr := featuresGET(t, router, token)
			require.Equal(t, http.StatusOK, rr.Code)

			var body struct {
				AgentEnabled bool `json:"agent_enabled"`
			}
			require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
			assert.Equal(t, enabled, body.AgentEnabled)
		})
	}
}

func TestGetFeatures_RequiresAuth(t *testing.T) {
	router, _ := setupFeaturesRouter(t, true)
	rr := featuresGET(t, router, "")
	assert.Equal(t, http.StatusUnauthorized, rr.Code)
}
