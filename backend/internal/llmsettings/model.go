package llmsettings

import "errors"

var (
	ErrNotFound = errors.New("llm settings not found")

	ErrInvalidConfig = errors.New("invalid llm configuration")
)

type Settings struct {
	UserID           int
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

type EncryptedSettings struct {
	UserID             int
	LLMBaseURL         string
	LLMAPIKeyEnc       string
	LLMModel           string
	EmbeddingBaseURL   string
	EmbeddingAPIKeyEnc string
	EmbeddingModel     string
	TavilyBaseURL      string
	TavilyAPIKeyEnc    string
	FallbackBaseURL    string
	FallbackAPIKeyEnc  string
	FallbackModel      string
}

type InvalidFieldError struct {
	Field string
	Err   error
}

func (e *InvalidFieldError) Error() string {
	if e.Err == nil {
		return e.Field + ": " + ErrInvalidConfig.Error()
	}
	return e.Field + ": " + e.Err.Error()
}

func (e *InvalidFieldError) Unwrap() error {
	if e.Err == nil {
		return ErrInvalidConfig
	}
	return e.Err
}

func invalidField(field string) error {
	return &InvalidFieldError{Field: field, Err: ErrInvalidConfig}
}

func invalidFieldWrap(field string, cause error) error {
	return &InvalidFieldError{Field: field, Err: cause}
}
