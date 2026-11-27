package access

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

type ConfigHandler struct {
	mode string
}

func NewConfigHandler(policy RegistrationPolicy) *ConfigHandler {
	return &ConfigHandler{mode: policy.Mode()}
}

type publicConfigResponse struct {
	RegistrationMode string `json:"registration_mode" enum:"open,invite_only,closed" doc:"Registration availability for this deployment: open (anyone), invite_only (only usernames on the operator allowlist), closed (no new accounts)."`
}

type getConfigOutput struct {
	Body publicConfigResponse
}

func RegisterConfigOps(api huma.API, h *ConfigHandler) {
	huma.Register(api, huma.Operation{
		OperationID: "get-public-config",
		Method:      http.MethodGet,
		Path:        "/api/config",
		Summary:     "Get public site configuration",
		Description: "Anonymous probe the SPA uses to render registration affordances. Reports only the registration mode, never usernames or allowlist contents.",
		Tags:        []string{"config"},
	}, h.GetConfig)
}

//nolint:revive // unexported-return is huma's idiomatic op pattern
func (h *ConfigHandler) GetConfig(_ context.Context, _ *struct{}) (*getConfigOutput, error) {
	return &getConfigOutput{Body: publicConfigResponse{RegistrationMode: h.mode}}, nil
}
