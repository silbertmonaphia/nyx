package llmsettings

import (
	"context"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"

	"nyx/internal/llmsettings/db"
	"nyx/internal/platform/api"
)

func init() {
	api.RegisterSentinel(ErrInvalidConfig, http.StatusBadRequest, "Invalid LLM configuration")
}

type Querier interface {
	GetUserLLMSettings(ctx context.Context, userID int32) (db.UserLlmSetting, error)
	UpsertUserLLMSettings(ctx context.Context, arg db.UpsertUserLLMSettingsParams) (db.UserLlmSetting, error)
	DeleteUserLLMSettings(ctx context.Context, userID int32) (int64, error)
}

type Pool interface {
	db.DBTX
}

type Repository interface {
	Get(ctx context.Context, userID int) (*EncryptedSettings, error)
	Upsert(ctx context.Context, s *EncryptedSettings) (*EncryptedSettings, error)
	Delete(ctx context.Context, userID int) (int64, error)
}

type sqlRepository struct {
	q Querier
}

func NewRepository(pool Pool) Repository {
	return &sqlRepository{q: db.New(pool)}
}

func (r *sqlRepository) Get(ctx context.Context, userID int) (*EncryptedSettings, error) {
	row, err := r.q.GetUserLLMSettings(ctx, toInt32(userID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	out := toEncrypted(row)
	return &out, nil
}

func (r *sqlRepository) Upsert(ctx context.Context, s *EncryptedSettings) (*EncryptedSettings, error) {
	row, err := r.q.UpsertUserLLMSettings(ctx, db.UpsertUserLLMSettingsParams{
		UserID:             toInt32(s.UserID),
		LLMBaseURL:         s.LLMBaseURL,
		LLMAPIKeyEnc:       s.LLMAPIKeyEnc,
		LLMModel:           s.LLMModel,
		EmbeddingBaseURL:   s.EmbeddingBaseURL,
		EmbeddingAPIKeyEnc: s.EmbeddingAPIKeyEnc,
		EmbeddingModel:     s.EmbeddingModel,
		TavilyBaseURL:      s.TavilyBaseURL,
		TavilyAPIKeyEnc:    s.TavilyAPIKeyEnc,
		FallbackBaseURL:    s.FallbackBaseURL,
		FallbackAPIKeyEnc:  s.FallbackAPIKeyEnc,
		FallbackModel:      s.FallbackModel,
	})
	if err != nil {
		return nil, err
	}
	out := toEncrypted(row)
	return &out, nil
}

func (r *sqlRepository) Delete(ctx context.Context, userID int) (int64, error) {
	return r.q.DeleteUserLLMSettings(ctx, toInt32(userID))
}

func toEncrypted(d db.UserLlmSetting) EncryptedSettings {
	return EncryptedSettings{
		UserID:             int(d.UserID),
		LLMBaseURL:         d.LLMBaseURL,
		LLMAPIKeyEnc:       d.LLMAPIKeyEnc,
		LLMModel:           d.LLMModel,
		EmbeddingBaseURL:   d.EmbeddingBaseURL,
		EmbeddingAPIKeyEnc: d.EmbeddingAPIKeyEnc,
		EmbeddingModel:     d.EmbeddingModel,
		TavilyBaseURL:      d.TavilyBaseURL,
		TavilyAPIKeyEnc:    d.TavilyAPIKeyEnc,
		FallbackBaseURL:    d.FallbackBaseURL,
		FallbackAPIKeyEnc:  d.FallbackAPIKeyEnc,
		FallbackModel:      d.FallbackModel,
	}
}

//nolint:gosec // G115: users.id is SERIAL (INTEGER); JWT-verified ids are well below int32 max.
func toInt32(v int) int32 { return int32(v) }
