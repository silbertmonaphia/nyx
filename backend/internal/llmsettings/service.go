package llmsettings

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"

	"nyx/internal/llm/ssrf"
	"nyx/internal/platform/config"
	"nyx/internal/platform/crypto"
)

const maskTailChars = 4

type Service struct {
	repo             Repository
	cipher           *crypto.Cipher
	embeddingProbe   func(ctx context.Context, baseURL, apiKey, model string) error
	byokAllowPrivate bool
}

func NewService(repo Repository, cipher *crypto.Cipher) *Service {
	return &Service{repo: repo, cipher: cipher}
}

func (s *Service) SetEmbeddingProbe(fn func(ctx context.Context, baseURL, apiKey, model string) error) {
	s.embeddingProbe = fn
}

func (s *Service) SetBYOKAllowPrivate(allowed bool) {
	s.byokAllowPrivate = allowed
}

type UpdateInput struct {
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

func (s *Service) Get(ctx context.Context, userID int) (*Settings, error) {
	enc, err := s.repo.Get(ctx, userID)
	if err != nil {
		return nil, err
	}
	return s.decrypt(enc)
}

func (s *Service) Save(ctx context.Context, userID int, in UpdateInput) error {
	_, err := s.SaveTrackingEmbedding(ctx, userID, in)
	return err
}

func (s *Service) SaveTrackingEmbedding(ctx context.Context, userID int, in UpdateInput) (bool, error) {
	existing, err := s.repo.Get(ctx, userID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return false, err
	}
	embeddingChanged := embeddingIdentityChanged(existing, &in)
	if err = ValidateUpdateWithPolicy(&in, existing, s.byokAllowPrivate); err != nil {
		return false, err
	}
	if err = s.probeEmbedding(ctx, existing, &in); err != nil {
		return false, err
	}

	out := &EncryptedSettings{
		UserID:        userID,
		LLMBaseURL:    in.LLMBaseURL,
		LLMModel:      in.LLMModel,
		TavilyBaseURL: in.TavilyBaseURL,
	}

	if err = s.applyLLM(out, &in, existing); err != nil {
		return false, err
	}
	if err = s.applyEmbedding(out, &in, existing); err != nil {
		return false, err
	}
	if err = s.applyTavily(out, &in, existing); err != nil {
		return false, err
	}

	if in.FallbackBaseURL == "" && in.FallbackAPIKey == "" && in.FallbackModel == "" {
		out.FallbackBaseURL, out.FallbackAPIKeyEnc, out.FallbackModel = "", "", ""
	} else {
		if out.FallbackAPIKeyEnc, err = s.resolveKey(in.FallbackAPIKey, "fallback_api_key", in.FallbackBaseURL, existing); err != nil {
			return false, err
		}
		out.FallbackBaseURL = in.FallbackBaseURL
		out.FallbackModel = in.FallbackModel
	}

	if _, err = s.repo.Upsert(ctx, out); err != nil {
		return false, err
	}
	return embeddingChanged, nil
}

func embeddingIdentityChanged(existing *EncryptedSettings, in *UpdateInput) bool {
	if existing == nil {
		return false
	}
	return existing.EmbeddingModel != in.EmbeddingModel
}

func (s *Service) probeEmbedding(ctx context.Context, existing *EncryptedSettings, in *UpdateInput) error {
	if s.embeddingProbe == nil || in.EmbeddingBaseURL == "" {
		return nil
	}
	apiKey := in.EmbeddingAPIKey
	if apiKey == "" && existing != nil {
		if decrypted, err := s.cipher.Decrypt(existing.EmbeddingAPIKeyEnc); err == nil {
			apiKey = decrypted
		}
	}
	err := s.embeddingProbe(ctx, in.EmbeddingBaseURL, apiKey, in.EmbeddingModel)
	if errors.Is(err, ErrEmbeddingWidthMismatch) {
		return invalidField("embedding_model")
	}
	return nil
}

func (s *Service) Delete(ctx context.Context, userID int) (int64, error) {
	return s.repo.Delete(ctx, userID)
}

func (s *Service) resolveKey(raw, field, baseURL string, existing *EncryptedSettings) (string, error) {
	if raw != "" {
		return s.cipher.Encrypt(raw)
	}
	if existing != nil && existingEncFor(field, existing) != "" {
		return existingEncFor(field, existing), nil
	}
	if isLoopbackBaseURL(baseURL) {
		return "", nil
	}
	return "", invalidField(field)
}

func (s *Service) applyLLM(out *EncryptedSettings, in *UpdateInput, existing *EncryptedSettings) error {
	if in.LLMBaseURL == "" {
		out.LLMBaseURL, out.LLMModel, out.LLMAPIKeyEnc = "", "", ""
		return nil
	}
	out.LLMBaseURL = in.LLMBaseURL
	out.LLMModel = in.LLMModel
	if in.LLMAPIKey != "" {
		enc, err := s.cipher.Encrypt(in.LLMAPIKey)
		if err != nil {
			return err
		}
		out.LLMAPIKeyEnc = enc
	} else if existing != nil {
		out.LLMAPIKeyEnc = existing.LLMAPIKeyEnc
	}
	return nil
}

func (s *Service) applyEmbedding(out *EncryptedSettings, in *UpdateInput, existing *EncryptedSettings) error {
	if in.EmbeddingBaseURL == "" {
		out.EmbeddingBaseURL, out.EmbeddingModel, out.EmbeddingAPIKeyEnc = "", "", ""
		return nil
	}
	out.EmbeddingBaseURL = in.EmbeddingBaseURL
	out.EmbeddingModel = in.EmbeddingModel
	if in.EmbeddingAPIKey != "" {
		enc, err := s.cipher.Encrypt(in.EmbeddingAPIKey)
		if err != nil {
			return err
		}
		out.EmbeddingAPIKeyEnc = enc
	} else if existing != nil {
		out.EmbeddingAPIKeyEnc = existing.EmbeddingAPIKeyEnc
	}
	return nil
}

func (s *Service) applyTavily(out *EncryptedSettings, in *UpdateInput, existing *EncryptedSettings) error {
	if in.TavilyBaseURL == "" {
		out.TavilyBaseURL, out.TavilyAPIKeyEnc = "", ""
		return nil
	}
	out.TavilyBaseURL = in.TavilyBaseURL
	if in.TavilyAPIKey != "" {
		enc, err := s.cipher.Encrypt(in.TavilyAPIKey)
		if err != nil {
			return err
		}
		out.TavilyAPIKeyEnc = enc
	} else if existing != nil {
		out.TavilyAPIKeyEnc = existing.TavilyAPIKeyEnc
	}
	return nil
}

func existingEncFor(field string, e *EncryptedSettings) string {
	switch field {
	case "llm_api_key":
		return e.LLMAPIKeyEnc
	case "embedding_api_key":
		return e.EmbeddingAPIKeyEnc
	case "tavily_api_key":
		return e.TavilyAPIKeyEnc
	case "fallback_api_key":
		return e.FallbackAPIKeyEnc
	}
	return ""
}

func (s *Service) decrypt(e *EncryptedSettings) (*Settings, error) {
	llmKey := ""
	if e.LLMAPIKeyEnc != "" {
		var err error
		if llmKey, err = s.cipher.Decrypt(e.LLMAPIKeyEnc); err != nil {
			return nil, fmt.Errorf("llm_api_key: %w", err)
		}
	}
	embeddingKey := ""
	if e.EmbeddingAPIKeyEnc != "" {
		var err error
		if embeddingKey, err = s.cipher.Decrypt(e.EmbeddingAPIKeyEnc); err != nil {
			return nil, fmt.Errorf("embedding_api_key: %w", err)
		}
	}
	tavilyKey := ""
	if e.TavilyAPIKeyEnc != "" {
		var err error
		if tavilyKey, err = s.cipher.Decrypt(e.TavilyAPIKeyEnc); err != nil {
			return nil, fmt.Errorf("tavily_api_key: %w", err)
		}
	}
	fallbackKey := ""
	if e.FallbackAPIKeyEnc != "" {
		var err error
		if fallbackKey, err = s.cipher.Decrypt(e.FallbackAPIKeyEnc); err != nil {
			return nil, fmt.Errorf("fallback_api_key: %w", err)
		}
	}
	return &Settings{
		UserID:           e.UserID,
		LLMBaseURL:       e.LLMBaseURL,
		LLMAPIKey:        llmKey,
		LLMModel:         e.LLMModel,
		EmbeddingBaseURL: e.EmbeddingBaseURL,
		EmbeddingAPIKey:  embeddingKey,
		EmbeddingModel:   e.EmbeddingModel,
		TavilyBaseURL:    e.TavilyBaseURL,
		TavilyAPIKey:     tavilyKey,
		FallbackBaseURL:  e.FallbackBaseURL,
		FallbackAPIKey:   fallbackKey,
		FallbackModel:    e.FallbackModel,
	}, nil
}

func MaskKey(key string) string {
	if key == "" {
		return ""
	}
	if len(key) <= maskTailChars {
		return "…"
	}
	return "…" + key[len(key)-maskTailChars:]
}

func ValidateUpdate(in *UpdateInput, existing *EncryptedSettings) error {
	return ValidateUpdateWithPolicy(in, existing, false)
}

func ValidateUpdateWithPolicy(in *UpdateInput, existing *EncryptedSettings, allowPrivate bool) error {
	if err := validateRequiredConfig(in, existing, allowPrivate); err != nil {
		return err
	}
	if existing == nil {
		if err := validateCreateKeys(in); err != nil {
			return err
		}
	}
	if err := validateProvidedKeys(in); err != nil {
		return err
	}
	return validateFallbackGroup(in, existing, allowPrivate)
}

func validateRequiredConfig(in *UpdateInput, existing *EncryptedSettings, allowPrivate bool) error {
	if err := validateLLMGroup(in, allowPrivate); err != nil {
		return err
	}
	if err := validateEmbeddingGroup(in, existing, allowPrivate); err != nil {
		return err
	}
	if err := validateTavilyGroup(in, allowPrivate); err != nil {
		return err
	}
	if !anyGroupSet(in) && in.FallbackBaseURL == "" && in.FallbackModel == "" {
		return invalidField("llm_base_url")
	}
	return nil
}

func anyGroupSet(in *UpdateInput) bool {
	return in.LLMBaseURL != "" || in.EmbeddingBaseURL != "" || in.TavilyBaseURL != ""
}

func validateLLMGroup(in *UpdateInput, allowPrivate bool) error {
	if in.LLMBaseURL == "" {
		if in.LLMModel != "" || in.LLMAPIKey != "" {
			return invalidField("llm_base_url")
		}
		return nil
	}
	if err := validateUserURL(in.LLMBaseURL, "llm_base_url", allowPrivate); err != nil {
		return err
	}
	if in.LLMModel == "" {
		return invalidField("llm_model")
	}
	return nil
}

func validateEmbeddingGroup(in *UpdateInput, existing *EncryptedSettings, allowPrivate bool) error {
	if in.EmbeddingBaseURL == "" {
		if in.EmbeddingModel != "" || in.EmbeddingAPIKey != "" {
			return invalidField("embedding_base_url")
		}
		return nil
	}
	if err := validateUserURL(in.EmbeddingBaseURL, "embedding_base_url", allowPrivate); err != nil {
		return err
	}
	if in.EmbeddingModel == "" {
		return invalidField("embedding_model")
	}
	if existing != nil && existing.EmbeddingModel != "" && in.EmbeddingModel != existing.EmbeddingModel {
		return invalidField("embedding_model")
	}
	return nil
}

func validateTavilyGroup(in *UpdateInput, allowPrivate bool) error {
	if in.TavilyBaseURL == "" {
		return nil
	}
	return validateUserURL(in.TavilyBaseURL, "tavily_base_url", allowPrivate)
}

func validateCreateKeys(in *UpdateInput) error {
	if in.LLMBaseURL != "" && in.LLMAPIKey == "" && !isLoopbackBaseURL(in.LLMBaseURL) {
		return invalidField("llm_api_key")
	}
	if in.EmbeddingBaseURL != "" && in.EmbeddingAPIKey == "" && !isLoopbackBaseURL(in.EmbeddingBaseURL) {
		return invalidField("embedding_api_key")
	}
	if in.TavilyAPIKey != "" && in.TavilyBaseURL == "" {
		return invalidField("tavily_base_url")
	}
	return nil
}

func isLoopbackBaseURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func validateProvidedKeys(in *UpdateInput) error {
	for _, k := range []struct{ raw, field string }{
		{in.LLMAPIKey, "llm_api_key"},
		{in.EmbeddingAPIKey, "embedding_api_key"},
		{in.TavilyAPIKey, "tavily_api_key"},
	} {
		if k.raw == "" {
			continue
		}
		if err := config.CheckNotPlaceholderKey(k.raw, k.field); err != nil {
			return invalidFieldWrap(k.field, err)
		}
	}
	return nil
}

func validateFallbackGroup(in *UpdateInput, existing *EncryptedSettings, allowPrivate bool) error {
	if in.FallbackBaseURL == "" && in.FallbackAPIKey == "" && in.FallbackModel == "" {
		return nil
	}
	if in.FallbackBaseURL == "" || in.FallbackModel == "" {
		return invalidField("fallback")
	}
	fallbackKey := in.FallbackAPIKey
	if fallbackKey == "" && existing != nil {
		fallbackKey = existing.FallbackAPIKeyEnc
	}
	if fallbackKey == "" && !isLoopbackBaseURL(in.FallbackBaseURL) {
		return invalidField("fallback")
	}
	if err := validateUserURL(in.FallbackBaseURL, "fallback_base_url", allowPrivate); err != nil {
		return err
	}
	if in.FallbackAPIKey != "" {
		if err := config.CheckNotPlaceholderKey(in.FallbackAPIKey, "fallback_api_key"); err != nil {
			return invalidFieldWrap("fallback_api_key", err)
		}
	}
	return nil
}

func validateUserURL(raw, field string, allowPrivate bool) error {
	u, err := url.Parse(raw)
	if err != nil {
		return invalidFieldWrap(field, fmt.Errorf("not a valid URL: %w", err))
	}
	switch u.Scheme {
	case "http", "https":
	default:
		return invalidField(field)
	}
	if u.Hostname() == "" {
		return invalidField(field)
	}
	if u.User != nil {
		return invalidField(field)
	}
	if !allowPrivate {
		if err := ssrf.CheckHostBestEffort(u.Hostname(), field); err != nil {
			return invalidFieldWrap(field, err)
		}
	}
	return nil
}
