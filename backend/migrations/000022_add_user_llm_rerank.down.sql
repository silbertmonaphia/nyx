ALTER TABLE user_llm_settings
    DROP COLUMN IF EXISTS rerank_model,
    DROP COLUMN IF EXISTS rerank_api_key_enc,
    DROP COLUMN IF EXISTS rerank_base_url;
