package llmsettings

import (
	"context"
	"errors"
	"fmt"
	"net/url"

	"nyx/internal/platform/config"
	"nyx/internal/platform/crypto"
)

const maskTailChars = 4

type Service struct {
	repo   Repository
	cipher *crypto.Cipher
}

func NewService(repo Repository, cipher *crypto.Cipher) *Service {
	return &Service{repo: repo, cipher: cipher}
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
	existing, err := s.repo.Get(ctx, userID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	if err = ValidateUpdate(&in, existing); err != nil {
		return err
	}

	out := &EncryptedSettings{
		UserID:           userID,
		LLMBaseURL:       in.LLMBaseURL,
		LLMModel:         in.LLMModel,
		EmbeddingBaseURL: in.EmbeddingBaseURL,
		EmbeddingModel:   in.EmbeddingModel,
		TavilyBaseURL:    in.TavilyBaseURL,
	}

	if out.LLMAPIKeyEnc, err = s.resolveKey(in.LLMAPIKey, "llm_api_key", existing); err != nil {
		return err
	}
	if out.EmbeddingAPIKeyEnc, err = s.resolveKey(in.EmbeddingAPIKey, "embedding_api_key", existing); err != nil {
		return err
	}
	if out.TavilyAPIKeyEnc, err = s.resolveKey(in.TavilyAPIKey, "tavily_api_key", existing); err != nil {
		return err
	}

	if in.FallbackBaseURL == "" && in.FallbackAPIKey == "" && in.FallbackModel == "" {
		out.FallbackBaseURL, out.FallbackAPIKeyEnc, out.FallbackModel = "", "", ""
	} else {
		if out.FallbackAPIKeyEnc, err = s.resolveKey(in.FallbackAPIKey, "fallback_api_key", existing); err != nil {
			return err
		}
		out.FallbackBaseURL = in.FallbackBaseURL
		out.FallbackModel = in.FallbackModel
	}

	_, err = s.repo.Upsert(ctx, out)
	return err
}

func (s *Service) Delete(ctx context.Context, userID int) (int64, error) {
	return s.repo.Delete(ctx, userID)
}

func (s *Service) resolveKey(raw, field string, existing *EncryptedSettings) (string, error) {
	if raw != "" {
		return s.cipher.Encrypt(raw)
	}
	if existing != nil && existingEncFor(field, existing) != "" {
		return existingEncFor(field, existing), nil
	}
	return "", invalidField(field)
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
	llmKey, err := s.cipher.Decrypt(e.LLMAPIKeyEnc)
	if err != nil {
		return nil, fmt.Errorf("llm_api_key: %w", err)
	}
	embeddingKey, err := s.cipher.Decrypt(e.EmbeddingAPIKeyEnc)
	if err != nil {
		return nil, fmt.Errorf("embedding_api_key: %w", err)
	}
	tavilyKey, err := s.cipher.Decrypt(e.TavilyAPIKeyEnc)
	if err != nil {
		return nil, fmt.Errorf("tavily_api_key: %w", err)
	}
	fallbackKey := ""
	if e.FallbackAPIKeyEnc != "" {
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
	if err := validateRequiredConfig(in); err != nil {
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
	return validateFallbackGroup(in, existing)
}

func validateRequiredConfig(in *UpdateInput) error {
	for _, u := range []struct{ raw, field string }{
		{in.LLMBaseURL, "llm_base_url"},
		{in.EmbeddingBaseURL, "embedding_base_url"},
		{in.TavilyBaseURL, "tavily_base_url"},
	} {
		if err := validateUserURL(u.raw, u.field); err != nil {
			return err
		}
	}
	for _, r := range []struct{ raw, field string }{
		{in.LLMBaseURL, "llm_base_url"},
		{in.LLMModel, "llm_model"},
		{in.EmbeddingBaseURL, "embedding_base_url"},
		{in.EmbeddingModel, "embedding_model"},
		{in.TavilyBaseURL, "tavily_base_url"},
	} {
		if r.raw == "" {
			return invalidField(r.field)
		}
	}
	return nil
}

func validateCreateKeys(in *UpdateInput) error {
	for _, k := range []struct{ raw, field string }{
		{in.LLMAPIKey, "llm_api_key"},
		{in.EmbeddingAPIKey, "embedding_api_key"},
		{in.TavilyAPIKey, "tavily_api_key"},
	} {
		if k.raw == "" {
			return invalidField(k.field)
		}
	}
	return nil
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

func validateFallbackGroup(in *UpdateInput, existing *EncryptedSettings) error {
	if in.FallbackBaseURL == "" && in.FallbackAPIKey == "" && in.FallbackModel == "" {
		return nil
	}
	fallbackKey := in.FallbackAPIKey
	if fallbackKey == "" && existing != nil {
		fallbackKey = existing.FallbackAPIKeyEnc
	}
	present := 0
	for _, v := range []string{in.FallbackBaseURL, fallbackKey, in.FallbackModel} {
		if v != "" {
			present++
		}
	}
	switch present {
	case 0:
		return nil
	case 3:
		if err := validateUserURL(in.FallbackBaseURL, "fallback_base_url"); err != nil {
			return err
		}
		if in.FallbackAPIKey != "" {
			if err := config.CheckNotPlaceholderKey(in.FallbackAPIKey, "fallback_api_key"); err != nil {
				return invalidFieldWrap("fallback_api_key", err)
			}
		}
		return nil
	default:
		return invalidField("fallback")
	}
}

func validateUserURL(raw, field string) error {
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
	return nil
}
