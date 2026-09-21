-- SQL queries for the feed feature. Each block becomes a method on the
-- generated internal/feed/db.Querier interface. The first line of each
-- block is the @name annotation (which becomes the method name) and the
-- query type (`:one`, `:many`, `:exec`, `:execrows`).
--
-- `sqlc.narg('query')` returns NULL when the caller does not set the
-- `query` parameter, which short-circuits the LIKE clauses via the
-- `IS NULL OR ...` pattern. When the caller sets it, the caller is
-- responsible for wrapping the search term in `%` wildcards.
--
-- Every query filters on user_id — feeds are owner-scoped. The handler
-- always supplies a user_id (the JWT subject); we use plain
-- `= @user_id` rather than `sqlc.narg` so a NULL filter can't accidentally
-- leak rows from every user.

-- name: QueryFeedsPage :many
SELECT id, user_id, title, description, created_at, updated_at, deleted_at
FROM feeds
WHERE (
        sqlc.narg('query')::text IS NULL
        OR title       ILIKE sqlc.narg('query')
        OR description ILIKE sqlc.narg('query')
      )
  AND deleted_at IS NULL
  AND user_id   = @user_id
ORDER BY created_at DESC, id DESC
LIMIT  sqlc.arg('page_size')::int
OFFSET sqlc.arg('offset')::int;

-- name: QueryFeedsPageAsc :many
-- Ascending counterpart of QueryFeedsPage. Two separate queries keep
-- the ORDER BY literal (sqlc doesn't interpolate direction tokens),
-- and let the planner pick a different index if one ever lands for
-- ASC. The id tiebreaker flips to ASC so pagination stays consistent
-- within a sort direction.
SELECT id, user_id, title, description, created_at, updated_at, deleted_at
FROM feeds
WHERE (
        sqlc.narg('query')::text IS NULL
        OR title       ILIKE sqlc.narg('query')
        OR description ILIKE sqlc.narg('query')
      )
  AND deleted_at IS NULL
  AND user_id   = @user_id
ORDER BY created_at ASC, id ASC
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
  AND deleted_at IS NULL
  AND user_id   = @user_id;

-- name: InsertFeed :one
INSERT INTO feeds (user_id, title, description)
VALUES (@user_id, @title, @description)
RETURNING *;

-- name: UpdateFeed :one
UPDATE feeds
SET title       = @title,
    description = @description,
    updated_at  = CURRENT_TIMESTAMP
WHERE id = @id AND user_id = @user_id AND deleted_at IS NULL
RETURNING *;

-- name: SoftDeleteFeed :execrows
UPDATE feeds
SET deleted_at = CURRENT_TIMESTAMP
WHERE id = @id AND user_id = @user_id AND deleted_at IS NULL;