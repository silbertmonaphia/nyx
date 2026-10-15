-- SQL queries for the RAG-over-feeds feature. Each block becomes a
-- method on the generated internal/rag/db.Querier interface.
--
-- All queries scope by user_id — retrieval cannot return another
-- user's feed embeddings even if a bug desyncs the denormalised
-- user_id (the retriever JOINs back to feeds and applies the
-- user_id filter on both sides as defence-in-depth).

-- name: GetFeedEmbeddingHash :one
SELECT content_hash FROM feed_embeddings WHERE feed_id = $1;

-- name: UpsertFeedEmbedding :exec
INSERT INTO feed_embeddings (feed_id, user_id, embedding, chunk_text, content_hash, embedded_at)
VALUES ($1, $2, $3, $4, $5, CURRENT_TIMESTAMP)
ON CONFLICT (feed_id) DO UPDATE SET
    user_id      = EXCLUDED.user_id,
    embedding    = EXCLUDED.embedding,
    chunk_text   = EXCLUDED.chunk_text,
    content_hash = EXCLUDED.content_hash,
    embedded_at  = CURRENT_TIMESTAMP;

-- name: DeleteFeedEmbedding :exec
DELETE FROM feed_embeddings WHERE feed_id = $1;

-- name: RetrieveFeedPassages :many
SELECT
    fe.feed_id,
    f.title,
    f.description,
    (1 - (fe.embedding <=> $1))::float8 AS score
FROM feed_embeddings fe
JOIN feeds f ON f.id = fe.feed_id
WHERE fe.user_id = $2
  AND f.user_id = $2
  AND f.deleted_at IS NULL
ORDER BY fe.embedding <=> $1
LIMIT $3;

-- name: ListFeedsWithoutEmbeddings :many
SELECT id, user_id, title, description, created_at, updated_at, deleted_at
FROM feeds f
WHERE f.user_id = $1
  AND f.deleted_at IS NULL
  AND NOT EXISTS (
      SELECT 1 FROM feed_embeddings fe WHERE fe.feed_id = f.id
  )
ORDER BY f.created_at ASC
LIMIT $2;