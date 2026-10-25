package llmsettings

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func settingsRow(userID int32, llmKeyEnc string) *pgxmock.Rows {
	return pgxmock.NewRows([]string{
		"user_id", "llm_base_url", "llm_api_key_enc", "llm_model",
		"embedding_base_url", "embedding_api_key_enc", "embedding_model",
		"tavily_base_url", "tavily_api_key_enc",
		"fallback_base_url", "fallback_api_key_enc", "fallback_model",
		"created_at", "updated_at",
	}).AddRow(
		userID, "http://localhost:8000/v1", llmKeyEnc, "Qwen/Qwen3-4B-Instruct-2507-FP8",
		"http://localhost:8001/v1", "v1:embed-ciphertext", "Qwen/Qwen3-Embedding-0.6B",
		"https://api.tavily.com/search", "v1:tavily-ciphertext",
		"http://fallback:8000/v1", "v1:fallback-ciphertext", "Qwen/Qwen2.5-3B-Instruct",
		time.Now(), time.Now(),
	)
}

func newMockRepo(t *testing.T) (Repository, pgxmock.PgxPoolIface) {
	t.Helper()
	mock, err := pgxmock.NewPool()
	require.NoError(t, err)
	t.Cleanup(func() { mock.Close() })
	return NewRepository(mock), mock
}

func TestRepository_Get_NotFound(t *testing.T) {
	repo, mock := newMockRepo(t)

	mock.ExpectQuery(`SELECT .+ FROM user_llm_settings WHERE user_id = \$1`).
		WithArgs(int32(42)).
		WillReturnError(pgx.ErrNoRows)

	_, err := repo.Get(context.Background(), 42)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotFound,
		"a missing row must surface as the internal ErrNotFound, never pgx.ErrNoRows")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestRepository_Get_RoundTripsColumns(t *testing.T) {
	repo, mock := newMockRepo(t)

	mock.ExpectQuery(`SELECT .+ FROM user_llm_settings WHERE user_id = \$1`).
		WithArgs(int32(7)).
		WillReturnRows(settingsRow(7, "v1:llm-ciphertext"))

	got, err := repo.Get(context.Background(), 7)
	require.NoError(t, err)
	assert.Equal(t, 7, got.UserID)
	assert.Equal(t, "http://localhost:8000/v1", got.LLMBaseURL)
	assert.Equal(t, "v1:llm-ciphertext", got.LLMAPIKeyEnc)
	assert.Equal(t, "Qwen/Qwen3-4B-Instruct-2507-FP8", got.LLMModel)
	assert.Equal(t, "http://localhost:8001/v1", got.EmbeddingBaseURL)
	assert.Equal(t, "v1:embed-ciphertext", got.EmbeddingAPIKeyEnc)
	assert.Equal(t, "Qwen/Qwen3-Embedding-0.6B", got.EmbeddingModel)
	assert.Equal(t, "https://api.tavily.com/search", got.TavilyBaseURL)
	assert.Equal(t, "v1:tavily-ciphertext", got.TavilyAPIKeyEnc)
	assert.Equal(t, "http://fallback:8000/v1", got.FallbackBaseURL)
	assert.Equal(t, "v1:fallback-ciphertext", got.FallbackAPIKeyEnc)
	assert.Equal(t, "Qwen/Qwen2.5-3B-Instruct", got.FallbackModel)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestRepository_Upsert_RoundTripScopedToCaller(t *testing.T) {
	repo, mock := newMockRepo(t)

	mock.ExpectQuery(`INSERT INTO user_llm_settings`).
		WithArgs(
			int32(9),
			"http://u9:8000/v1", "v1:u9-key", "u9-model",
			"http://u9-embed:8001/v1", "v1:u9-embed-key", "u9-embed-model",
			"https://tavily.example", "v1:u9-tavily-key",
			"", "", "",
		).
		WillReturnRows(settingsRow(9, "v1:u9-key"))

	in := &EncryptedSettings{
		UserID:             9,
		LLMBaseURL:         "http://u9:8000/v1",
		LLMAPIKeyEnc:       "v1:u9-key",
		LLMModel:           "u9-model",
		EmbeddingBaseURL:   "http://u9-embed:8001/v1",
		EmbeddingAPIKeyEnc: "v1:u9-embed-key",
		EmbeddingModel:     "u9-embed-model",
		TavilyBaseURL:      "https://tavily.example",
		TavilyAPIKeyEnc:    "v1:u9-tavily-key",
	}
	got, err := repo.Upsert(context.Background(), in)
	require.NoError(t, err)
	assert.Equal(t, 9, got.UserID)
	assert.Equal(t, "v1:u9-key", got.LLMAPIKeyEnc)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestRepository_Delete_ReportsRowsAffected(t *testing.T) {
	repo, mock := newMockRepo(t)

	mock.ExpectExec(`DELETE FROM user_llm_settings WHERE user_id = \$1`).
		WithArgs(int32(11)).
		WillReturnResult(pgxmock.NewResult("DELETE", 1))

	n, err := repo.Delete(context.Background(), 11)
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestRepository_Delete_IdempotentWhenNoRow(t *testing.T) {
	repo, mock := newMockRepo(t)

	mock.ExpectExec(`DELETE FROM user_llm_settings WHERE user_id = \$1`).
		WithArgs(int32(11)).
		WillReturnResult(pgxmock.NewResult("DELETE", 0))

	n, err := repo.Delete(context.Background(), 11)
	require.NoError(t, err)
	assert.Equal(t, int64(0), n, "deleting an unconfigured user affects zero rows")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestRepository_UserScoping_EveryQueryBindsCallerID(t *testing.T) {
	repo, mock := newMockRepo(t)

	mock.ExpectQuery(`SELECT .+ FROM user_llm_settings WHERE user_id = \$1`).
		WithArgs(int32(5)).
		WillReturnRows(settingsRow(5, "v1:k"))
	mock.ExpectQuery(`INSERT INTO user_llm_settings`).
		WithArgs(
			int32(5),
			"http://u5:8000/v1", "v1:k", "u5-model",
			"http://u5-embed:8001/v1", "v1:e", "u5-embed-model",
			"https://tavily.example", "v1:tv",
			"", "", "",
		).
		WillReturnRows(settingsRow(5, "v1:k"))
	mock.ExpectExec(`DELETE FROM user_llm_settings WHERE user_id = \$1`).
		WithArgs(int32(5)).
		WillReturnResult(pgxmock.NewResult("DELETE", 0))

	_, err := repo.Get(context.Background(), 6)
	require.Error(t, err, "a call bound to a different user id must not consume user 5's expectations")

	_, err = repo.Get(context.Background(), 5)
	require.NoError(t, err)
	_, err = repo.Upsert(context.Background(), &EncryptedSettings{
		UserID:             5,
		LLMBaseURL:         "http://u5:8000/v1",
		LLMAPIKeyEnc:       "v1:k",
		LLMModel:           "u5-model",
		EmbeddingBaseURL:   "http://u5-embed:8001/v1",
		EmbeddingAPIKeyEnc: "v1:e",
		EmbeddingModel:     "u5-embed-model",
		TavilyBaseURL:      "https://tavily.example",
		TavilyAPIKeyEnc:    "v1:tv",
	})
	require.NoError(t, err)
	_, err = repo.Delete(context.Background(), 5)
	require.NoError(t, err)

	require.NoError(t, mock.ExpectationsWereMet(),
		"every query must have been scoped to the caller's user id")
}

func TestRepository_Get_PropagatesUnexpectedErrors(t *testing.T) {
	repo, mock := newMockRepo(t)

	boom := errors.New("connection reset")
	mock.ExpectQuery(`SELECT .+ FROM user_llm_settings WHERE user_id = \$1`).
		WithArgs(int32(1)).
		WillReturnError(boom)

	_, err := repo.Get(context.Background(), 1)
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrNotFound, "a real DB failure must not masquerade as not-found")
	assert.ErrorIs(t, err, boom)
}
