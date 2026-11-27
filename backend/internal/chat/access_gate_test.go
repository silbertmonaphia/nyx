package chat

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nyx/internal/middleware"
	"nyx/internal/platform/auth"
)

func buildGatedRouter(t *testing.T, allowed []string) (http.Handler, auth.TokenService) {
	t.Helper()
	tokens, err := auth.NewTokenService([]byte(auth.TestSecret), 15*time.Minute)
	require.NoError(t, err)

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.StoreRequest)

	h := NewHandler(newTestServiceForHandler(&chatStubProvider{}))
	RegisterChatRoute(r, h, tokens, nil, allowed)
	RegisterApprovalsRoute(r, h, tokens, nil, allowed)
	return r, tokens
}

func TestChatRoutes_AIAllowlistGate(t *testing.T) {
	router, tokens := buildGatedRouter(t, []string{"allowed-user"})

	allowedToken, err := tokens.GenerateToken(1, "allowed-user")
	require.NoError(t, err)
	deniedToken, err := tokens.GenerateToken(2, "other-user")
	require.NoError(t, err)

	routes := []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/api/chat/"},
		{http.MethodGet, "/api/chat/runs/run-1/stream"},
		{http.MethodGet, "/api/chat/sessions"},
		{http.MethodPost, "/api/chat/sessions"},
		{http.MethodGet, "/api/chat/sessions/session-1/messages"},
		{http.MethodPost, "/api/chat/sessions/session-1/resume"},
		{http.MethodDelete, "/api/chat/sessions/session-1"},
		{http.MethodPost, "/api/chat/runs/run-1/cancel"},
		{http.MethodPost, "/api/chat/approvals"},
	}

	for _, route := range routes {
		t.Run(route.method+" "+route.path+" denies unlisted user", func(t *testing.T) {
			req := httptest.NewRequest(route.method, route.path, nil)
			req.Header.Set("Authorization", "Bearer "+deniedToken)
			rr := httptest.NewRecorder()
			router.ServeHTTP(rr, req)
			assert.Equal(t, http.StatusForbidden, rr.Code, "body=%s", rr.Body.String())
			assert.Contains(t, rr.Body.String(), "AI features are not available")
		})
	}

	t.Run("unauthenticated request is 401", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/chat/sessions", nil)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusUnauthorized, rr.Code)
	})

	t.Run("listed user passes the gate", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/chat/sessions", nil)
		req.Header.Set("Authorization", "Bearer "+allowedToken)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		assert.NotEqual(t, http.StatusForbidden, rr.Code, "body=%s", rr.Body.String())
	})
}

func TestChatRoutes_AIAllowlistEmptyAllowsEveryone(t *testing.T) {
	router, tokens := buildGatedRouter(t, nil)
	token, err := tokens.GenerateToken(3, "any-user")
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/api/chat/sessions", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	assert.NotEqual(t, http.StatusForbidden, rr.Code, "body=%s", rr.Body.String())
}
