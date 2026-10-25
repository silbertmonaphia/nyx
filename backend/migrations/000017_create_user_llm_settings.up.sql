CREATE TABLE user_llm_settings (
    user_id               INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    llm_base_url          TEXT NOT NULL,
    llm_api_key_enc       TEXT NOT NULL,
    llm_model             TEXT NOT NULL,
    embedding_base_url    TEXT NOT NULL,
    embedding_api_key_enc TEXT NOT NULL,
    embedding_model       TEXT NOT NULL,
    tavily_base_url       TEXT NOT NULL,
    tavily_api_key_enc    TEXT NOT NULL,
    fallback_base_url     TEXT NOT NULL DEFAULT '',
    fallback_api_key_enc  TEXT NOT NULL DEFAULT '',
    fallback_model        TEXT NOT NULL DEFAULT '',
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);
