package features

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"nyx/internal/middleware"
	"nyx/internal/platform/auth"
)

type Handler struct {
	agentEnabled bool
}

func NewHandler(agentEnabled bool) *Handler {
	return &Handler{agentEnabled: agentEnabled}
}

type featuresResponse struct {
	AgentEnabled bool `json:"agent_enabled" doc:"True when the chat handler proxies to the Python agent (AGENT_ENABLED=true)."`
}

type getFeaturesOutput struct {
	Body featuresResponse
}

func RegisterFeatureOps(api huma.API, h *Handler, tokens auth.TokenService) {
	huma.Register(api, huma.Operation{
		OperationID: "get-features",
		Method:      http.MethodGet,
		Path:        "/api/features",
		Summary:     "Get feature availability",
		Description: "Reports operator-level feature switches the SPA renders conditionally. agent_enabled is true when the chat handler proxies to the Python agent.",
		Tags:        []string{"features"},
		Security:    []map[string][]string{{"BearerAuth": {}}},
		Middlewares: huma.Middlewares{middleware.NewHumaAuth(tokens)},
	}, h.GetFeatures)
}

//nolint:revive // unexported-return is huma's idiomatic op pattern
func (h *Handler) GetFeatures(_ context.Context, _ *struct{}) (*getFeaturesOutput, error) {
	return &getFeaturesOutput{Body: featuresResponse{AgentEnabled: h.agentEnabled}}, nil
}
