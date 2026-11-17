-- name: InsertToolIdempotency :execrows
INSERT INTO tool_idempotency (owner_id, key, tool, response)
VALUES ($1, $2, $3, 'null'::jsonb)
ON CONFLICT (owner_id, key) DO NOTHING;

-- name: GetToolIdempotencyResponse :one
SELECT response, tool
FROM tool_idempotency
WHERE owner_id = $1 AND key = $2;

-- name: UpdateToolIdempotencyResponse :exec
UPDATE tool_idempotency
SET response = $3
WHERE owner_id = $1 AND key = $2;

-- name: DeleteExpiredToolIdempotency :execrows
DELETE FROM tool_idempotency
WHERE created_at < $1;
