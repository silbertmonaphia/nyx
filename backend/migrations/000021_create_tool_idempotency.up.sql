CREATE TABLE tool_idempotency (
    owner_id   BIGINT      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    key        TEXT        NOT NULL CHECK (char_length(key) BETWEEN 1 AND 128),
    tool       TEXT        NOT NULL,
    response   JSONB       NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (owner_id, key)
);

CREATE INDEX tool_idempotency_created_at_idx ON tool_idempotency (created_at);
