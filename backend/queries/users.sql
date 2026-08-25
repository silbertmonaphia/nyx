-- SQL queries for the user feature. Each block becomes a method on the
-- generated internal/user/db.Querier interface. The first line of each
-- block is the @name annotation (which becomes the method name) and the
-- query type (`:one`, `:many`, `:exec`, `:execrows`).

-- name: InsertUser :one
INSERT INTO users (username, email, password_hash)
VALUES (@username, @email, @password_hash)
RETURNING *;

-- name: GetUserByUsername :one
SELECT id, username, email, password_hash, created_at, updated_at, deleted_at
FROM users
WHERE username = @username AND deleted_at IS NULL;

-- name: GetUserByID :one
SELECT id, username, email, password_hash, created_at, updated_at, deleted_at
FROM users
WHERE id = @id AND deleted_at IS NULL;

-- name: CreateRefreshToken :one
-- Atomic self-stamping: insert with family_id=0 placeholder, then
-- update family_id to the inserted row's id, then SELECT out the
-- updated row. The two-CTE shape (rather than UPDATE … RETURNING
-- directly off the inserted CTE) avoids a same-table update snapshot
-- issue that left the outer RETURNING with zero rows under real
-- Postgres — the unit tests passed because they stubbed the query.
WITH inserted AS (
    INSERT INTO refresh_tokens (user_id, token_hash, family_id, expires_at)
    VALUES (@user_id, @token_hash, 0, @expires_at)
    RETURNING id
),
updated AS (
    UPDATE refresh_tokens
    SET family_id = inserted.id
    FROM inserted
    WHERE refresh_tokens.id = inserted.id
    RETURNING refresh_tokens.*
)
SELECT * FROM updated;

-- name: GetRefreshTokenByHash :one
SELECT id, user_id, token_hash, family_id, replaced_by_id, expires_at, revoked_at, created_at
FROM refresh_tokens
WHERE token_hash = @token_hash;

-- name: RotateRefreshToken :one
-- Single CTE: insert new row tied to old via replaced_by_id, then mark
-- the old row revoked and link it back. Atomic — concurrent rotations
-- will see the second one with revoked_at NOT NULL and trigger reuse
-- detection at the service layer.
WITH new_row AS (
    INSERT INTO refresh_tokens (user_id, token_hash, family_id, replaced_by_id, expires_at)
    VALUES (
        @user_id,
        @token_hash,
        @family_id,
        @old_id,
        @expires_at
    )
    RETURNING *
)
UPDATE refresh_tokens
SET replaced_by_id = new_row.id,
    revoked_at     = COALESCE(refresh_tokens.revoked_at, now())
FROM new_row
WHERE refresh_tokens.id = @old_id
RETURNING new_row.*;

-- name: RevokeRefreshTokenFamily :execrows
UPDATE refresh_tokens
SET revoked_at = COALESCE(revoked_at, now())
WHERE family_id = $1 AND revoked_at IS NULL;

-- name: RevokeRefreshTokenByID :exec
UPDATE refresh_tokens
SET revoked_at = COALESCE(revoked_at, now())
WHERE id = $1 AND revoked_at IS NULL;

-- name: CountActiveRefreshTokensByUser :one
-- Counts non-revoked, non-expired rows for a user. Used by the
-- service layer on every mint to enforce the per-user family cap
-- (see SECURITY.md M2). Excludes already-revoked rows because
-- those are being pruned in the background goroutine — counting
-- them would inflate the result and trigger spurious revokes.
SELECT COUNT(*)::bigint
FROM refresh_tokens
WHERE user_id = @user_id
  AND revoked_at IS NULL
  AND expires_at > now();

-- name: ListOldestActiveRefreshTokensByUser :many
-- Returns up to `limit` rows for a user, oldest first. Used when
-- the active-row count exceeds the cap: the service revokes the
-- surplus oldest rows to make room for the new mint. The mint
-- itself races only on this user's existing rows, so the index on
-- (user_id) WHERE revoked_at IS NULL covers it efficiently.
SELECT id
FROM refresh_tokens
WHERE user_id = @user_id
  AND revoked_at IS NULL
  AND expires_at > now()
ORDER BY created_at ASC
LIMIT @lim;

-- name: PurgeRefreshTokensOlderThan :execrows
-- Bulk-delete rows older than the supplied cutoff that are also
-- revoked OR expired. The cutoff exists so we never delete a row
-- an active session could still reach: even a revoked row is
-- useful to operators for the first few days after revocation
-- when investigating an incident. Used by the background cleanup
-- goroutine (see SECURITY.md M2).
DELETE FROM refresh_tokens
WHERE created_at < @cutoff
  AND (revoked_at IS NOT NULL OR expires_at < now());
