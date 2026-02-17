-- API Key Management Queries
-- These queries are processed by sqlc to generate type-safe Go code in ../../internal/db/
-- Generated files: db.go, models.go, queries.sql.go (excluded from git, see .gitignore)
-- Run `make sqlc-generate` after modifying this file

-- name: CreateAPIKey :exec
INSERT INTO api_keys (
    id, username, name, description, key_hash, key_prefix, status, created_at, expires_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9
);

-- name: GetAPIKeyByID :one
SELECT id, username, name, description, key_prefix, status, created_at, expires_at, last_used_at
FROM api_keys
WHERE id = $1;

-- name: GetAPIKeyByHash :one
-- Critical path: called on every authenticated request via Authorino
SELECT id, username, name, description, key_prefix, status, last_used_at
FROM api_keys
WHERE key_hash = $1;

-- name: ListAPIKeysByUser :many
SELECT id, key_prefix, name, description, status, created_at, expires_at, last_used_at
FROM api_keys
WHERE username = $1
ORDER BY created_at DESC;

-- name: UpdateLastUsed :exec
UPDATE api_keys
SET last_used_at = $1
WHERE id = $2;

-- name: RevokeAPIKey :execrows
UPDATE api_keys
SET status = 'revoked'
WHERE id = $1 AND status = 'active';

-- name: RevokeAllUserKeys :execrows
UPDATE api_keys
SET status = 'revoked'
WHERE username = $1 AND status = 'active';

-- name: CountActiveKeysByUser :one
SELECT COUNT(*) as count
FROM api_keys
WHERE username = $1 AND status = 'active';

-- name: GetStaleKeys :many
-- Find keys not used in the specified interval (for cleanup/audit)
SELECT id, username, name, key_prefix, created_at, last_used_at
FROM api_keys
WHERE status = 'active'
  AND (last_used_at IS NULL OR last_used_at < $1);
