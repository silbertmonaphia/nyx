package llmsettings

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"nyx/internal/middleware"
	"nyx/internal/platform/api"
	"nyx/internal/platform/auth"
	"nyx/internal/reqctx"
)

type Handler struct {
	service                *Service
	onEmbeddingInvalidated func(ctx context.Context, userID int)
}

func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

func (h *Handler) SetEmbeddingInvalidatedHook(fn func(ctx context.Context, userID int)) {
	h.onEmbeddingInvalidated = fn
}

type llmEndpointResponse struct {
	BaseURL      string `json:"base_url"`
	Model        string `json:"model"`
	APIKeyMasked string `json:"api_key_masked" doc:"Masked preview of the stored key: the last four characters only. The plaintext key is write-only and never returned."`
}

type tavilyResponse struct {
	BaseURL      string `json:"base_url"`
	APIKeyMasked string `json:"api_key_masked" doc:"Masked preview of the stored key: the last four characters only. The plaintext key is write-only and never returned."`
}

type configResponse struct {
	Configured bool                 `json:"configured"`
	LLM        *llmEndpointResponse `json:"llm"`
	Embedding  *llmEndpointResponse `json:"embedding"`
	Tavily     *tavilyResponse      `json:"tavily"`
	Fallback   *llmEndpointResponse `json:"fallback"`
}

type llmEndpointInput struct {
	BaseURL string `json:"base_url" required:"true" doc:"OpenAI-compatible base URL including the provider's version prefix (for example, a trailing /v1). Private and localhost addresses are allowed."`
	APIKey  string `json:"api_key" required:"false" writeOnly:"true" doc:"Write-only API key. Required on create; leave empty on update to keep the stored key."`
	Model   string `json:"model" required:"true" doc:"Model name as the configured endpoint expects it."`
}

type tavilyInput struct {
	BaseURL string `json:"base_url" required:"false" doc:"Search provider base URL. Omit the whole section to run without web search."`
	APIKey  string `json:"api_key" required:"false" writeOnly:"true" doc:"Write-only API key. Optional: an empty key uses the provider's keyless tier; on update, empty keeps the stored key."`
}

func derefEndpointBaseURL(in *llmEndpointInput) string {
	if in == nil {
		return ""
	}
	return in.BaseURL
}

func derefEndpointAPIKey(in *llmEndpointInput) string {
	if in == nil {
		return ""
	}
	return in.APIKey
}

func derefEndpointModel(in *llmEndpointInput) string {
	if in == nil {
		return ""
	}
	return in.Model
}

func llmResponseFor(s *Settings) *llmEndpointResponse {
	if s.LLMBaseURL == "" {
		return nil
	}
	return &llmEndpointResponse{
		BaseURL:      s.LLMBaseURL,
		Model:        s.LLMModel,
		APIKeyMasked: MaskKey(s.LLMAPIKey),
	}
}

func embeddingResponseFor(s *Settings) *llmEndpointResponse {
	if s.EmbeddingBaseURL == "" {
		return nil
	}
	return &llmEndpointResponse{
		BaseURL:      s.EmbeddingBaseURL,
		Model:        s.EmbeddingModel,
		APIKeyMasked: MaskKey(s.EmbeddingAPIKey),
	}
}

func derefTavilyBaseURL(in *tavilyInput) string {
	if in == nil {
		return ""
	}
	return in.BaseURL
}

func derefTavilyAPIKey(in *tavilyInput) string {
	if in == nil {
		return ""
	}
	return in.APIKey
}

func tavilyResponseFor(s *Settings) *tavilyResponse {
	if s.TavilyBaseURL == "" {
		return nil
	}
	return &tavilyResponse{
		BaseURL:      s.TavilyBaseURL,
		APIKeyMasked: MaskKey(s.TavilyAPIKey),
	}
}

type configUpdateInput struct {
	LLM       *llmEndpointInput `json:"llm" required:"false" doc:"Optional main-LLM group. Required for the AI Agent; without it feeds and other features keep working."`
	Embedding *llmEndpointInput `json:"embedding" required:"false" doc:"Optional embedding group. When omitted the agent runs without feed retrieval (RAG) and feed indexing is skipped."`
	Tavily    *tavilyInput      `json:"tavily" required:"false" doc:"Optional search provider group. When omitted the agent does not plan web searches."`
	Fallback  *llmEndpointInput `json:"fallback,omitempty" required:"false" doc:"Optional retry-model group. Supply all three fields or omit the section entirely."`
}

type getConfigInput struct{}

type getConfigOutput struct {
	Body configResponse
}

type putConfigInput struct {
	Body configUpdateInput
}

type putConfigOutput struct {
	Body configResponse
}

type deleteConfigInput struct{}

type deleteConfigOutput struct {
	Status int `status:"204"`
}

func RegisterLLMConfigOps(api huma.API, h *Handler, tokens auth.TokenService) {
	RegisterLLMConfigOpsTest(api, h, tokens, true)
}

func RegisterLLMConfigOpsTest(api huma.API, h *Handler, tokens auth.TokenService, withAuth bool) {
	huma.Register(api, huma.Operation{
		OperationID: "get-llm-config",
		Method:      http.MethodGet,
		Path:        "/api/me/llm-config",
		Summary:     "Get LLM configuration",
		Description: "Returns the authenticated caller's per-user LLM configuration with API keys masked to their last four characters. A user with no saved configuration receives configured=false with null sections.",
		Tags:        []string{"llm-config"},
		Security:    []map[string][]string{{"BearerAuth": {}}},
		Middlewares: llmConfigMiddlewares(tokens, withAuth),
	}, h.GetConfig)

	huma.Register(api, huma.Operation{
		OperationID: "update-llm-config",
		Method:      http.MethodPut,
		Path:        "/api/me/llm-config",
		Summary:     "Save LLM configuration",
		Description: "Validates and upserts the authenticated caller's optional main LLM, optional embedding, optional Tavily, and optional retry-model configuration. API keys are write-only: a blank key on update keeps the stored key; on create a key is required unless the endpoint is loopback. Omitting Tavily disables web search for the agent. Responds with the masked configuration.",
		Tags:        []string{"llm-config"},
		Security:    []map[string][]string{{"BearerAuth": {}}},
		Middlewares: llmConfigMiddlewares(tokens, withAuth),
	}, h.UpdateConfig)

	huma.Register(api, huma.Operation{
		OperationID: "delete-llm-config",
		Method:      http.MethodDelete,
		Path:        "/api/me/llm-config",
		Summary:     "Delete LLM configuration",
		Description: "Removes the authenticated caller's LLM configuration. Idempotent: deleting an unconfigured user also returns 204.",
		Tags:        []string{"llm-config"},
		Security:    []map[string][]string{{"BearerAuth": {}}},
		Middlewares: llmConfigMiddlewares(tokens, withAuth),
	}, h.DeleteConfig)
}

func llmConfigMiddlewares(tokens auth.TokenService, withAuth bool) huma.Middlewares {
	if !withAuth {
		return nil
	}
	return huma.Middlewares{middleware.NewHumaAuth(tokens)}
}

//nolint:revive // unexported-return is huma's idiomatic op pattern
func (h *Handler) GetConfig(ctx context.Context, _ *getConfigInput) (*getConfigOutput, error) {
	userID := reqctx.UserIDFromContext(ctx)
	s, err := h.service.Get(ctx, userID)
	if errors.Is(err, ErrNotFound) {
		return &getConfigOutput{Body: configResponse{Configured: false}}, nil
	}
	if err != nil {
		return nil, api.MapError(ctx, err, "Failed to load LLM configuration")
	}
	return &getConfigOutput{Body: responseFrom(s)}, nil
}

//nolint:revive // unexported-return is huma's idiomatic op pattern
func (h *Handler) UpdateConfig(ctx context.Context, in *putConfigInput) (*putConfigOutput, error) {
	userID := reqctx.UserIDFromContext(ctx)
	embeddingChanged, err := h.service.SaveTrackingEmbedding(ctx, userID, updateInputFrom(in.Body))
	if err != nil {
		var fieldErr *InvalidFieldError
		if errors.As(err, &fieldErr) {
			return nil, api.NewErrorResponseFromContext(ctx, http.StatusBadRequest, "Invalid LLM configuration", fieldErr.Field)
		}
		return nil, api.MapError(ctx, err, "Failed to save LLM configuration")
	}
	if embeddingChanged && h.onEmbeddingInvalidated != nil {
		h.onEmbeddingInvalidated(ctx, userID)
	}
	s, err := h.service.Get(ctx, userID)
	if err != nil {
		return nil, api.MapError(ctx, err, "Failed to load LLM configuration")
	}
	return &putConfigOutput{Body: responseFrom(s)}, nil
}

//nolint:revive // unexported-return is huma's idiomatic op pattern
func (h *Handler) DeleteConfig(ctx context.Context, _ *deleteConfigInput) (*deleteConfigOutput, error) {
	userID := reqctx.UserIDFromContext(ctx)
	deleted, err := h.service.Delete(ctx, userID)
	if err != nil {
		return nil, api.MapError(ctx, err, "Failed to delete LLM configuration")
	}
	if deleted > 0 && h.onEmbeddingInvalidated != nil {
		h.onEmbeddingInvalidated(ctx, userID)
	}
	return &deleteConfigOutput{Status: http.StatusNoContent}, nil
}

func responseFrom(s *Settings) configResponse {
	out := configResponse{
		Configured: true,
		LLM:        llmResponseFor(s),
		Embedding:  embeddingResponseFor(s),
		Tavily:     tavilyResponseFor(s),
	}
	if s.FallbackBaseURL != "" {
		out.Fallback = &llmEndpointResponse{
			BaseURL:      s.FallbackBaseURL,
			Model:        s.FallbackModel,
			APIKeyMasked: MaskKey(s.FallbackAPIKey),
		}
	}
	return out
}

func updateInputFrom(in configUpdateInput) UpdateInput {
	out := UpdateInput{
		LLMBaseURL:       derefEndpointBaseURL(in.LLM),
		LLMAPIKey:        derefEndpointAPIKey(in.LLM),
		LLMModel:         derefEndpointModel(in.LLM),
		EmbeddingBaseURL: derefEndpointBaseURL(in.Embedding),
		EmbeddingAPIKey:  derefEndpointAPIKey(in.Embedding),
		EmbeddingModel:   derefEndpointModel(in.Embedding),
		TavilyBaseURL:    derefTavilyBaseURL(in.Tavily),
		TavilyAPIKey:     derefTavilyAPIKey(in.Tavily),
	}
	if in.Fallback != nil {
		out.FallbackBaseURL = in.Fallback.BaseURL
		out.FallbackAPIKey = in.Fallback.APIKey
		out.FallbackModel = in.Fallback.Model
	}
	return out
}
