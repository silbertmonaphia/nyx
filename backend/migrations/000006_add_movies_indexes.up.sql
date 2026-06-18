-- Indexes for the paginated movies endpoint.
-- Every GET /movies query filters on `deleted_at IS NULL`, so a partial
-- index on that predicate keeps the working set tight.
CREATE INDEX IF NOT EXISTS idx_movies_active
    ON movies (id)
    WHERE deleted_at IS NULL;

-- Stable pagination order: most-recent first, id as tiebreaker.
CREATE INDEX IF NOT EXISTS idx_movies_created_at
    ON movies (created_at DESC, id DESC);

-- Trigram index accelerates ILIKE '%term%' on title without requiring
-- a leading wildcard escape. Created inside a DO block so the migration
-- does not fail on databases where the role lacks CREATE EXTENSION
-- privilege (e.g. some managed Postgres offerings).
DO $$
BEGIN
    CREATE EXTENSION IF NOT EXISTS pg_trgm;
EXCEPTION
    WHEN insufficient_privilege OR feature_not_supported THEN
        RAISE NOTICE 'pg_trgm extension skipped: insufficient privilege';
END $$;

CREATE INDEX IF NOT EXISTS idx_movies_title_trgm
    ON movies USING GIN (title gin_trgm_ops);