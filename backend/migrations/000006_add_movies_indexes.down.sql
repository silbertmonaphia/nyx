DROP INDEX IF EXISTS idx_movies_title_trgm;
DROP INDEX IF EXISTS idx_movies_created_at;
DROP INDEX IF EXISTS idx_movies_active;

-- pg_trgm extension is intentionally left in place on rollback; dropping
-- extensions can affect other objects outside this migration's scope.