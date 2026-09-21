-- Reverse 000014_add_feed_embeddings. Drops the indexes before the
-- table so no dangling index references remain. Intentionally does
-- NOT drop the vector extension — once an extension is installed it
-- should stay for the DB lifetime, and a future migration may add
-- other vector-typed columns.
DROP INDEX IF EXISTS feed_embeddings_user_id_idx;
DROP INDEX IF EXISTS feed_embeddings_embedding_hnsw_idx;
DROP TABLE IF EXISTS feed_embeddings;