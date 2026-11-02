CREATE TABLE kb_sources (
    id           BIGSERIAL   PRIMARY KEY,
    scope        TEXT        NOT NULL CHECK (scope IN ('shared', 'user')),
    owner_id     BIGINT      REFERENCES users(id) ON DELETE CASCADE,
    name         TEXT        NOT NULL CHECK (length(name) BETWEEN 1 AND 200),
    kind         TEXT        NOT NULL CHECK (kind IN ('dir', 'inline')),
    root_path    TEXT,
    enabled      BOOLEAN     NOT NULL DEFAULT TRUE,
    last_scan_at TIMESTAMPTZ,
    last_error   TEXT        NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT kb_sources_scope_owner CHECK (
        (scope = 'shared' AND owner_id IS NULL) OR
        (scope = 'user' AND owner_id IS NOT NULL)
    ),
    CONSTRAINT kb_sources_kind_root CHECK (
        (kind = 'dir' AND root_path IS NOT NULL) OR
        (kind = 'inline' AND root_path IS NULL)
    )
);

CREATE UNIQUE INDEX kb_sources_scope_owner_name_uidx
    ON kb_sources (scope, COALESCE(owner_id, 0), name);

CREATE INDEX kb_sources_owner_idx ON kb_sources (owner_id) WHERE owner_id IS NOT NULL;

CREATE TABLE kb_documents (
    id           BIGSERIAL   PRIMARY KEY,
    source_id    BIGINT      NOT NULL REFERENCES kb_sources(id) ON DELETE CASCADE,
    scope        TEXT        NOT NULL CHECK (scope IN ('shared', 'user')),
    owner_id     BIGINT      REFERENCES users(id) ON DELETE CASCADE,
    origin       TEXT        NOT NULL CHECK (origin IN ('file', 'inline')),
    rel_path     TEXT        NOT NULL CHECK (length(rel_path) BETWEEN 1 AND 1024),
    title        TEXT        NOT NULL DEFAULT '',
    doc_type     TEXT        NOT NULL DEFAULT 'markdown',
    content_hash BYTEA       NOT NULL,
    byte_size    BIGINT      NOT NULL DEFAULT 0,
    mtime        TIMESTAMPTZ,
    status       TEXT        NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'indexed', 'failed')),
    error_detail TEXT        NOT NULL DEFAULT '',
    generation   INTEGER     NOT NULL DEFAULT 0,
    content      TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT kb_documents_scope_owner CHECK (
        (scope = 'shared' AND owner_id IS NULL) OR
        (scope = 'user' AND owner_id IS NOT NULL)
    ),
    CONSTRAINT kb_documents_origin_content CHECK (
        (origin = 'file' AND content IS NULL) OR
        (origin = 'inline' AND content IS NOT NULL)
    )
);

CREATE UNIQUE INDEX kb_documents_source_rel_path_uidx
    ON kb_documents (source_id, rel_path);

CREATE INDEX kb_documents_source_status_idx ON kb_documents (source_id, status);
CREATE INDEX kb_documents_owner_hash_idx
    ON kb_documents (owner_id, content_hash) WHERE origin = 'inline';
CREATE INDEX kb_documents_pending_idx ON kb_documents (updated_at) WHERE status = 'pending';

CREATE TABLE kb_chunks (
    id              BIGSERIAL   PRIMARY KEY,
    document_id     BIGINT      NOT NULL REFERENCES kb_documents(id) ON DELETE CASCADE,
    scope           TEXT        NOT NULL CHECK (scope IN ('shared', 'user')),
    owner_id        BIGINT      REFERENCES users(id) ON DELETE CASCADE,
    source_id       BIGINT      NOT NULL REFERENCES kb_sources(id) ON DELETE CASCADE,
    generation      INTEGER     NOT NULL,
    ordinal         INTEGER     NOT NULL,
    heading_path    TEXT[]      NOT NULL DEFAULT '{}',
    content         TEXT        NOT NULL,
    embed_text      TEXT        NOT NULL,
    content_hash    BYTEA       NOT NULL,
    embedding       vector(1024),
    embedding_model TEXT        NOT NULL DEFAULT '',
    token_count     INTEGER     NOT NULL DEFAULT 0,
    locator         JSONB       NOT NULL DEFAULT '{}'::jsonb,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT kb_chunks_scope_owner CHECK (
        (scope = 'shared' AND owner_id IS NULL) OR
        (scope = 'user' AND owner_id IS NOT NULL)
    )
);

CREATE INDEX kb_chunks_document_id_idx ON kb_chunks (document_id);
CREATE INDEX kb_chunks_scope_owner_idx ON kb_chunks (scope, owner_id);
CREATE INDEX kb_chunks_source_id_idx ON kb_chunks (source_id);
CREATE INDEX kb_chunks_document_generation_idx ON kb_chunks (document_id, generation);
CREATE UNIQUE INDEX kb_chunks_document_generation_ordinal_uidx
    ON kb_chunks (document_id, generation, ordinal);
CREATE INDEX kb_chunks_embedding_model_idx ON kb_chunks (embedding_model);
CREATE INDEX kb_chunks_embedding_hnsw_idx
    ON kb_chunks USING hnsw (embedding vector_cosine_ops)
    WHERE embedding IS NOT NULL;
