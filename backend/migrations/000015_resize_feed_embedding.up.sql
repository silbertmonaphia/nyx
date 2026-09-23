-- Resize feed_embeddings.embedding from vector(1536) to vector(1024)
-- to match the new local embed model (Qwen/Qwen3-Embedding-0.6B).
--
-- pgvector does not support ALTER COLUMN TYPE in-place for dim changes
-- (see 000014_add_feed_embeddings.up.sql:10-11), so this migration
-- drops + recreates the column and the HNSW index. The btree on
-- user_id (feed_embeddings_user_id_idx) survives the column drop.
--
-- OPERATOR IMPACT — this wipes every row in feed_embeddings:
--   * Existing feeds will be re-embedded lazily on the next RAG
--     chat (see internal/rag/service.go BackfillOnce). The cap is
--     5 iterations x RAG_MAX_BACKFILL_PER_REQUEST (=20) = 100 feeds
--     per process per user; operators with more feeds per user
--     should bump RAG_MAX_BACKFILL_PER_REQUEST or run a manual
--     UpdateFeed loop.
--   * Boot-time warning (cmd/api/main.go warnEmbeddingDimMismatch)
--     will fire on the intermediate boot if EMBEDDING_DIMENSIONS
--     hasn't been updated to 1024 yet. Update EMBEDDING_DIMENSIONS
--     in the same release as this migration.
--
-- After this migration + EMBEDDING_DIMENSIONS=1024 the table shape
-- matches the model's vector output (Qwen/Qwen3-Embedding-0.6B emits
-- 1024-dim vectors).
DROP INDEX IF EXISTS feed_embeddings_embedding_hnsw_idx;

ALTER TABLE feed_embeddings DROP COLUMN embedding;

ALTER TABLE feed_embeddings
    ADD COLUMN embedding vector(1024) NOT NULL;

-- Same HNSW + opclass as 000014; recreated on the new column.
CREATE INDEX feed_embeddings_embedding_hnsw_idx
    ON feed_embeddings
    USING hnsw (embedding vector_cosine_ops);