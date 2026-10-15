CREATE TABLE IF NOT EXISTS refresh_tokens (
    id             BIGSERIAL PRIMARY KEY,
    user_id        INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash     BYTEA   NOT NULL,
    family_id      BIGINT  NOT NULL,
    replaced_by_id BIGINT,
    expires_at     TIMESTAMP WITH TIME ZONE NOT NULL,
    revoked_at     TIMESTAMP WITH TIME ZONE,
    created_at     TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_refresh_tokens_token_hash ON refresh_tokens (token_hash);
CREATE INDEX        IF NOT EXISTS idx_refresh_tokens_family_id  ON refresh_tokens (family_id);
CREATE INDEX        IF NOT EXISTS idx_refresh_tokens_user_active
    ON refresh_tokens (user_id) WHERE revoked_at IS NULL;
ALTER TABLE refresh_tokens
    ADD CONSTRAINT fk_refresh_tokens_replaced_by
    FOREIGN KEY (replaced_by_id) REFERENCES refresh_tokens(id) ON DELETE SET NULL;
