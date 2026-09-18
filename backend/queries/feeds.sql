-- SQL queries for the feed feature. Each block becomes a method on the
-- generated internal/feed/db.Querier interface. The first line of each
-- block is the @name annotation (which becomes the method name) and the
-- query type (`:one`, `:many`, `:exec`, `:execrows`).
--
-- `sqlc.narg('query')` returns NULL when the caller does not set the
-- `query` parameter, which short-circuits the LIKE clauses via the
-- `IS NULL OR ...` pattern. When the caller sets it, the caller is
-- responsible for wrapping the search term in `%` wildcards.

-- name: QueryFeedsPage :many
SELECT id, title, description, rating, created_at, updated_at, deleted_at
FROM feeds
WHERE (
        sqlc.narg('query')::text IS NULL
        OR title       ILIKE sqlc.narg('query')
        OR description ILIKE sqlc.narg('query')
      )
  AND deleted_at IS NULL
ORDER BY created_at DESC, id DESC
LIMIT  sqlc.arg('page_size')::int
OFFSET sqlc.arg('offset')::int;

-- name: CountFeeds :one
SELECT COUNT(*)
FROM feeds
WHERE (
        sqlc.narg('query')::text IS NULL
        OR title       ILIKE sqlc.narg('query')
        OR description ILIKE sqlc.narg('query')
      )
  AND deleted_at IS NULL;

-- name: InsertFeed :one
INSERT INTO feeds (title, description, rating)
VALUES (@title, @description, @rating)
RETURNING *;

-- name: UpdateFeed :one
UPDATE feeds
SET title       = @title,
    description = @description,
    rating      = @rating,
    updated_at  = CURRENT_TIMESTAMP
WHERE id = @id AND deleted_at IS NULL
RETURNING *;

-- name: SoftDeleteFeed :execrows
UPDATE feeds
SET deleted_at = CURRENT_TIMESTAMP
WHERE id = @id AND deleted_at IS NULL;