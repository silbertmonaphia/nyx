ALTER TABLE user_llm_settings
    ADD COLUMN rerank_base_url    TEXT NOT NULL DEFAULT '',
    ADD COLUMN rerank_api_key_enc TEXT NOT NULL DEFAULT '',
    ADD COLUMN rerank_model       TEXT NOT NULL DEFAULT '';
