-- RAG support: per-feed vector embeddings. One row per feed, holding
-- the embedding of Title + "\n\n" + Description. Created lazily on
-- write (the rag package's Indexer is wired into feed.Service.Create /
-- Update / Delete) and backfilled on first RAG query for users who
-- had feeds before this migration landed (rag.Service.BackfillOnce).
--
-- Schema notes:
--   * vector(1536) is the typed form — matches EMBEDDING_DIMENSIONS
--     default (text-embedding-3-small). Operators who switch to a
--     different-dim model must write a new migration that drops &
--     recreates the column (pgvector's vector type does not support
--     ALTER COLUMN TYPE in-place for dim changes). The startup-time
--     schema-drift check in cmd/api/main.go warns loudly when the
--     column's stored dim disagrees with EMBEDDING_DIMENSIONS.
--   * user_id is denormalised from feeds.user_id so the retriever's
--     SELECT can filter by ownership in one indexed range scan, no
--     join. The retriever still JOINs back to feeds and applies the
--     user_id filter on both sides as defence-in-depth against a
--     hypothetical bug that desyncs the two user_ids.
--   * content_hash is sha256(chunk_text). The Indexer SELECTs the
--     existing hash before calling the embedding API, so a no-op
--     UpdateFeed (e.g. a debounced auto-save re-sending the same
--     payload) costs one extra sha256 and zero embedding API calls.
--   * ON DELETE CASCADE is set for the future hard-delete path; the
--     today-only soft-delete leaves the embedding row behind, but
--     the retriever's `deleted_at IS NULL` filter never reads it.
CREATE EXTENSION IF NOT EXISTS vector;

CREATE TABLE feed_embeddings (
    feed_id      INTEGER     PRIMARY KEY REFERENCES feeds(id) ON DELETE CASCADE,
    user_id      BIGINT      NOT NULL,
    embedding    vector(1536) NOT NULL,
    chunk_text   TEXT        NOT NULL,
    content_hash BYTEA       NOT NULL,
    embedded_at  TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- HNSW over the embedding column. The retriever's WHERE user_id = X
-- is applied as a post-HNSW filter (pgvector supports WHERE-filtered
-- HNSW at query time); per-user corpora are small enough (<10k rows
-- in realistic single-user scenarios) that the "scan more candidates,
-- filter post-hoc" overhead is negligible. vector_cosine_ops matches
-- the <=> operator the retriever orders by.
CREATE INDEX feed_embeddings_embedding_hnsw_idx
    ON feed_embeddings
    USING hnsw (embedding vector_cosine_ops);

-- Btree on user_id alone — speeds up the lazy-backfill query
-- ("feeds without embeddings for this user") and the user-scoping
-- filter on the retriever.
CREATE INDEX feed_embeddings_user_id_idx ON feed_embeddings(user_id);