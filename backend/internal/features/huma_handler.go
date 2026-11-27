package features

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"nyx/internal/access"
	"nyx/internal/middleware"
	"nyx/internal/platform/auth"
	"nyx/internal/reqctx"
)

type Handler struct {
	agentEnabled bool
	llmEnabled   bool
	aiAllowlist  []string
}

func NewHandler(agentEnabled, llmEnabled bool, aiAllowlist []string) *Handler {
	return &Handler{agentEnabled: agentEnabled, llmEnabled: llmEnabled, aiAllowlist: aiAllowlist}
}

type featuresResponse struct {
	AgentEnabled bool `json:"agent_enabled" doc:"True when the chat handler proxies to the Python agent (AGENT_ENABLED=true)."`
	ChatAllowed  bool `json:"chat_allowed" doc:"True when the caller may use AI surfaces: LLM_ENABLED=true and the caller is on AI_ALLOWLIST (an empty list allows every authenticated user)."`
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
		Description: "Reports feature switches the SPA renders conditionally. agent_enabled is true when the chat handler proxies to the Python agent; chat_allowed is true when the caller may use AI surfaces (LLM_ENABLED plus AI_ALLOWLIST).",
		Tags:        []string{"features"},
		Security:    []map[string][]string{{"BearerAuth": {}}},
		Middlewares: huma.Middlewares{middleware.NewHumaAuth(tokens)},
	}, h.GetFeatures)
}

//nolint:revive // unexported-return is huma's idiomatic op pattern
func (h *Handler) GetFeatures(ctx context.Context, _ *struct{}) (*getFeaturesOutput, error) {
	return &getFeaturesOutput{Body: featuresResponse{
		AgentEnabled: h.agentEnabled,
		ChatAllowed:  h.llmEnabled && access.Allows(h.aiAllowlist, reqctx.UsernameFromContext(ctx)),
	}}, nil
}
