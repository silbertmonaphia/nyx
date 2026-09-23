-- Reverse 000015: drop the resized vector(1024) column + HNSW index.
-- Mirrors the down behaviour of 000014 — leaves feed_embeddings empty.
-- The btree on user_id (feed_embeddings_user_id_idx) survives.
DROP INDEX IF EXISTS feed_embeddings_embedding_hnsw_idx;

ALTER TABLE feed_embeddings DROP COLUMN embedding;

ALTER TABLE feed_embeddings
    ADD COLUMN embedding vector(1536) NOT NULL;

CREATE INDEX feed_embeddings_embedding_hnsw_idx
    ON feed_embeddings
    USING hnsw (embedding vector_cosine_ops);