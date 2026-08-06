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
