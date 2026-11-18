package llmsettings

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/trace"

	"nyx/internal/llm"
	"nyx/internal/llm/openai"
	"nyx/internal/llm/ssrf"
	"nyx/internal/platform/config"
	"nyx/internal/rag"
)

const (
	maxResolverCacheEntries = 64
	//nolint:gosec // G101: placeholder sentinel, not a credential.
	resolverKeylessAPIKey = "nyx-user-keyless-endpoint"
	defaultProbeTimeout   = 2 * time.Second
)

type UserStack struct {
	Chat       llm.Provider
	Embed      llm.Provider
	EmbedModel string
	Rerank     rag.Reranker

	LLMBaseURL       string
	LLMAPIKey        string
	LLMModel         string
	EmbeddingBaseURL string
	EmbeddingAPIKey  string
	EmbeddingModel   string
	TavilyBaseURL    string
	TavilyAPIKey     string
	FallbackBaseURL  string
	FallbackAPIKey   string
	FallbackModel    string
}

type Resolver struct {
	service *Service
	base    *config.Config
	tp      trace.TracerProvider

	mu          sync.Mutex
	cache       map[string]llm.Provider
	rerankCache map[string]rag.Reranker
}

func NewResolver(service *Service, baseCfg *config.Config, tp trace.TracerProvider) *Resolver {
	if baseCfg == nil {
		baseCfg = &config.Config{}
	}
	return &Resolver{
		service:     service,
		base:        baseCfg,
		tp:          tp,
		cache:       map[string]llm.Provider{},
		rerankCache: map[string]rag.Reranker{},
	}
}

func (r *Resolver) Resolve(ctx context.Context, userID int) (*UserStack, error) {
	s, err := r.service.Get(ctx, userID)
	if err != nil {
		return nil, err
	}

	chat, err := r.chatSlot(s)
	if err != nil {
		return nil, err
	}
	embed, err := r.embedSlot(s)
	if err != nil {
		return nil, err
	}
	rerank, err := r.rerankSlot(s)
	if err != nil {
		return nil, err
	}

	return &UserStack{
		Chat:             chat,
		Embed:            embed,
		EmbedModel:       s.EmbeddingModel,
		Rerank:           rerank,
		LLMBaseURL:       s.LLMBaseURL,
		LLMAPIKey:        s.LLMAPIKey,
		LLMModel:         s.LLMModel,
		EmbeddingBaseURL: s.EmbeddingBaseURL,
		EmbeddingAPIKey:  s.EmbeddingAPIKey,
		EmbeddingModel:   s.EmbeddingModel,
		TavilyBaseURL:    s.TavilyBaseURL,
		TavilyAPIKey:     s.TavilyAPIKey,
		FallbackBaseURL:  s.FallbackBaseURL,
		FallbackAPIKey:   s.FallbackAPIKey,
		FallbackModel:    s.FallbackModel,
	}, nil
}

func (r *Resolver) chatSlot(s *Settings) (llm.Provider, error) {
	if s.LLMBaseURL == "" {
		return nil, nil
	}
	fallbackModel := s.FallbackModel
	if fallbackModel == "" {
		fallbackModel = s.LLMModel
	}
	key := slotKey("chat", s.LLMBaseURL, s.LLMAPIKey, s.LLMModel,
		s.FallbackBaseURL, s.FallbackAPIKey, fallbackModel)
	return r.cached(key, func() (llm.Provider, error) {
		primary, err := r.buildClient(s.LLMBaseURL, s.LLMAPIKey, s.LLMModel)
		if err != nil {
			return nil, err
		}
		if s.FallbackBaseURL == "" {
			return primary, nil
		}
		fallback, err := r.buildClient(s.FallbackBaseURL, s.FallbackAPIKey, fallbackModel)
		if err != nil {
			return nil, err
		}
		return llm.NewRouter(
			llm.ProviderSlot{
				Name:     config.LLMProviderOpenAI,
				Provider: primary,
				BaseURL:  s.LLMBaseURL,
				ProbeKey: s.LLMAPIKey,
			},
			&llm.ProviderSlot{
				Name:     config.LLMProviderOpenAI,
				Provider: fallback,
				BaseURL:  s.FallbackBaseURL,
				ProbeKey: s.FallbackAPIKey,
			},
			r.tp.Tracer("nyx.llm.router"),
			r.probeTimeout(),
			llm.WithProbeHTTP(&http.Client{
				Transport: ssrf.NewTransport(r.base.BYOKAllowPrivateURL, "llm_base_url or fallback_base_url"),
				Timeout:   r.probeTimeout(),
			}),
		), nil
	})
}

func (r *Resolver) embedSlot(s *Settings) (llm.Provider, error) {
	if s.EmbeddingBaseURL == "" {
		return nil, nil
	}
	key := slotKey("embed", s.EmbeddingBaseURL, s.EmbeddingAPIKey, s.EmbeddingModel)
	return r.cached(key, func() (llm.Provider, error) {
		return r.buildClient(s.EmbeddingBaseURL, s.EmbeddingAPIKey, s.EmbeddingModel)
	})
}

const defaultRerankResolverTimeout = 2 * time.Second

func (r *Resolver) rerankSlot(s *Settings) (rag.Reranker, error) {
	if s.RerankBaseURL == "" || s.RerankModel == "" {
		return nil, nil
	}
	key := slotKey("rerank", s.RerankBaseURL, s.RerankAPIKey, s.RerankModel)
	r.mu.Lock()
	defer r.mu.Unlock()
	if cached, ok := r.rerankCache[key]; ok {
		return cached, nil
	}
	client := rag.NewHTTPReranker(
		s.RerankBaseURL,
		s.RerankAPIKey,
		s.RerankModel,
		r.rerankTimeout(),
		r.base.BYOKAllowPrivateURL,
		r.tp.Tracer("nyx.rag.rerank"),
	)
	if len(r.rerankCache) >= maxResolverCacheEntries {
		for existing := range r.rerankCache {
			delete(r.rerankCache, existing)
			break
		}
	}
	r.rerankCache[key] = client
	return client, nil
}

func (r *Resolver) rerankTimeout() time.Duration {
	d, err := time.ParseDuration(r.base.RerankTimeout)
	if err != nil || d <= 0 {
		return defaultRerankResolverTimeout
	}
	return d
}

func (r *Resolver) clientConfig(baseURL, apiKey, model string) *config.Config {
	if apiKey == "" {
		apiKey = resolverKeylessAPIKey
	}
	cfg := *r.base
	cfg.LLMEnabled = true
	cfg.LLMBaseURL = baseURL
	cfg.LLMAPIKey = apiKey
	cfg.LLMModel = model
	cfg.LLMAllowPrivateURL = cfg.BYOKAllowPrivateURL
	return &cfg
}

func (r *Resolver) buildClient(baseURL, apiKey, model string) (llm.Provider, error) {
	return openai.NewClient(r.clientConfig(baseURL, apiKey, model), r.tp.Tracer("nyx.llm.user"))
}

func (r *Resolver) cached(key string, build func() (llm.Provider, error)) (llm.Provider, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p, ok := r.cache[key]; ok {
		return p, nil
	}
	p, err := build()
	if err != nil {
		return nil, err
	}
	if len(r.cache) >= maxResolverCacheEntries {
		for k := range r.cache {
			delete(r.cache, k)
			break
		}
	}
	r.cache[key] = p
	return p, nil
}

func (r *Resolver) probeTimeout() time.Duration {
	d, err := time.ParseDuration(r.base.LLMProbeTimeout)
	if err != nil || d <= 0 {
		return defaultProbeTimeout
	}
	return d
}

func slotKey(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return fmt.Sprintf("%x", sum)
}
