package access

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	platapi "nyx/internal/platform/api"
)

func getPublicConfig(t *testing.T, policy RegistrationPolicy) *httptest.ResponseRecorder {
	t.Helper()
	platapi.OverrideHumaErrors()
	router := chi.NewMux()
	hapi := humachi.New(router, platapi.HumaConfig())
	RegisterConfigOps(hapi, NewConfigHandler(policy))
	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	return rr
}

func TestGetPublicConfig_RegistrationMode(t *testing.T) {
	cases := []struct {
		name   string
		policy RegistrationPolicy
		want   string
	}{
		{"open", RegistrationPolicy{Enabled: true}, RegistrationModeOpen},
		{"invite only", RegistrationPolicy{Enabled: true, Allowlist: []string{"allowed-user"}}, RegistrationModeInviteOnly},
		{"closed", RegistrationPolicy{Enabled: false, Allowlist: []string{"allowed-user"}}, RegistrationModeClosed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := getPublicConfig(t, tc.policy)
			require.Equal(t, http.StatusOK, rr.Code, "body=%s", rr.Body.String())

			var body struct {
				RegistrationMode string `json:"registration_mode"`
			}
			require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
			assert.Equal(t, tc.want, body.RegistrationMode)
		})
	}
}

func TestGetPublicConfig_DoesNotLeakAllowlist(t *testing.T) {
	rr := getPublicConfig(t, RegistrationPolicy{Enabled: true, Allowlist: []string{"allowed-user"}})
	require.Equal(t, http.StatusOK, rr.Code)
	assert.NotContains(t, rr.Body.String(), "allowed-user")
}
