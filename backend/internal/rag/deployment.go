package rag

import (
	"fmt"

	"go.opentelemetry.io/otel/trace"

	"nyx/internal/llm"
	"nyx/internal/llm/openai"
	"nyx/internal/llm/vllm"
	"nyx/internal/platform/config"
)

func NewDeploymentEmbedder(cfg *config.Config, tracerProvider trace.TracerProvider) (*Embedder, error) {
	if cfg == nil || !cfg.LLMEnabled {
		return nil, ErrNoEmbedder
	}
	provider := cfg.LLMProvider
	baseURL := cfg.LLMBaseURL
	apiKey := cfg.LLMAPIKey
	model := cfg.LLMModel
	allowPrivate := cfg.LLMAllowPrivateURL
	embedModel := cfg.LLMEmbeddingModel
	if cfg.LLMEmbeddingBaseURL != "" {
		provider = cfg.LLMEmbeddingProvider
		if provider == "" {
			provider = cfg.LLMProvider
		}
		baseURL = cfg.LLMEmbeddingBaseURL
		apiKey = cfg.LLMEmbeddingAPIKey
		if apiKey == "" {
			apiKey = cfg.LLMAPIKey
		}
		allowPrivate = cfg.LLMEmbeddingAllowPrivateURL || cfg.LLMAllowPrivateURL
	} else {
		if baseURL == "" || model == "" {
			return nil, ErrNoEmbedder
		}
		if embedModel == "" {
			embedModel = model
		}
	}
	if embedModel == "" {
		return nil, ErrNoEmbedder
	}
	if provider == "" {
		provider = config.LLMProviderOpenAI
	}
	clientCfg := *cfg
	clientCfg.LLMEnabled = true
	clientCfg.LLMProvider = provider
	clientCfg.LLMBaseURL = baseURL
	clientCfg.LLMAPIKey = apiKey
	clientCfg.LLMModel = embedModel
	clientCfg.LLMAllowPrivateURL = allowPrivate

	var client llm.Provider
	var err error
	switch provider {
	case config.LLMProviderVLLM:
		client, err = vllm.NewClient(&clientCfg, tracerProvider.Tracer("nyx.rag.deploy.vllm"))
	case config.LLMProviderOpenAI:
		client, err = openai.NewClient(&clientCfg, tracerProvider.Tracer("nyx.rag.deploy.openai"))
	default:
		return nil, fmt.Errorf("unsupported embedding provider %q: %w", provider, ErrNoEmbedder)
	}
	if err != nil {
		return nil, err
	}
	embedder := NewEmbedder(client, embedModel, tracerProvider.Tracer("nyx.rag.deploy"))
	embedder.SetDimensions(cfg.EmbeddingDimensions)
	return embedder, nil
}
