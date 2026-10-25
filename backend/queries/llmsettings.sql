-- name: GetUserLLMSettings :one
SELECT * FROM user_llm_settings WHERE user_id = @user_id;

-- name: UpsertUserLLMSettings :one
INSERT INTO user_llm_settings (
    user_id,
    llm_base_url,
    llm_api_key_enc,
    llm_model,
    embedding_base_url,
    embedding_api_key_enc,
    embedding_model,
    tavily_base_url,
    tavily_api_key_enc,
    fallback_base_url,
    fallback_api_key_enc,
    fallback_model
) VALUES (
    @user_id,
    @llm_base_url,
    @llm_api_key_enc,
    @llm_model,
    @embedding_base_url,
    @embedding_api_key_enc,
    @embedding_model,
    @tavily_base_url,
    @tavily_api_key_enc,
    @fallback_base_url,
    @fallback_api_key_enc,
    @fallback_model
)
ON CONFLICT (user_id) DO UPDATE SET
    llm_base_url          = EXCLUDED.llm_base_url,
    llm_api_key_enc       = EXCLUDED.llm_api_key_enc,
    llm_model             = EXCLUDED.llm_model,
    embedding_base_url    = EXCLUDED.embedding_base_url,
    embedding_api_key_enc = EXCLUDED.embedding_api_key_enc,
    embedding_model       = EXCLUDED.embedding_model,
    tavily_base_url       = EXCLUDED.tavily_base_url,
    tavily_api_key_enc    = EXCLUDED.tavily_api_key_enc,
    fallback_base_url     = EXCLUDED.fallback_base_url,
    fallback_api_key_enc  = EXCLUDED.fallback_api_key_enc,
    fallback_model        = EXCLUDED.fallback_model,
    updated_at            = now()
RETURNING *;

-- name: DeleteUserLLMSettings :execrows
DELETE FROM user_llm_settings WHERE user_id = @user_id;
