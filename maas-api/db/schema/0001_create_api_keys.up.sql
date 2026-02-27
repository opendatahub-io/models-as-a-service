-- Migration: 0001_create_api_keys
-- Description: Initial schema for API Key Management with group-based authorization
-- Includes: hash-only storage, status tracking, usage tracking, user groups

CREATE TABLE IF NOT EXISTS api_keys (
    id TEXT PRIMARY KEY,
    username TEXT NOT NULL,
    name TEXT NOT NULL,
    description TEXT,
    key_hash TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'active',
    user_groups TEXT[] NOT NULL, -- PostgreSQL array of user's groups at creation time (immutable)
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at TIMESTAMPTZ,
    last_used_at TIMESTAMPTZ,

    CONSTRAINT api_keys_status_check CHECK (status IN ('active', 'revoked', 'expired'))
);

-- Composite index for listing keys by user ordered by creation date
-- Supports: SELECT ... FROM api_keys WHERE username = $1 ORDER BY created_at DESC
CREATE INDEX IF NOT EXISTS idx_api_keys_username_created ON api_keys(username, created_at DESC);

-- Unique index on key_hash for fast validation lookups
-- CRITICAL for performance - Authorino calls validation on every request
CREATE UNIQUE INDEX IF NOT EXISTS idx_api_keys_key_hash ON api_keys(key_hash);

-- Index for finding stale keys (audit/cleanup queries)
CREATE INDEX IF NOT EXISTS idx_api_keys_last_used ON api_keys(last_used_at) WHERE last_used_at IS NOT NULL;

-- GIN index for efficient array queries (e.g., WHERE 'admin-users' = ANY(user_groups))
CREATE INDEX IF NOT EXISTS idx_api_keys_user_groups ON api_keys USING GIN (user_groups);
