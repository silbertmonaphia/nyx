-- Add a GIN trigram index on movies.description so substring search
-- (ILIKE '%term%') is index-accelerated on the description column.
--
-- Mirrors idx_movies_title_trgm (added in 000006). pg_trgm's trigrams are
-- generated on characters, not bytes, so this works for CJK text too.
-- Caveat: search terms shorter than 3 characters generate no trigrams
-- and fall back to a sequential scan — that floor applies equally to
-- ASCII and Chinese.
--
-- The CREATE EXTENSION call is wrapped in a DO block to match the
-- pattern in 000006: skip cleanly on databases where the role lacks
-- CREATE EXTENSION privilege (some managed Postgres offerings).
DO $$
BEGIN
    CREATE EXTENSION IF NOT EXISTS pg_trgm;
EXCEPTION
    WHEN insufficient_privilege OR feature_not_supported THEN
        RAISE NOTICE 'pg_trgm extension skipped: insufficient privilege';
END $$;

CREATE INDEX IF NOT EXISTS idx_movies_description_trgm
    ON movies USING GIN (description gin_trgm_ops);