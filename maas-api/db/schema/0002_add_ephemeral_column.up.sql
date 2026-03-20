-- Schema for API Key Management: 0002_add_ephemeral_column.up.sql
-- Description: Add ephemeral column for short-lived programmatic keys

-- Add ephemeral column to api_keys table (idempotent)
ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS ephemeral BOOLEAN NOT NULL DEFAULT FALSE;

-- Index for cleanup job: find expired ephemeral keys efficiently
-- Partial index only includes ephemeral keys to minimize index size
-- Note: ephemeral column excluded from index key since WHERE clause already filters it
CREATE INDEX IF NOT EXISTS idx_api_keys_ephemeral_expired 
ON api_keys(status, expires_at) 
WHERE ephemeral = TRUE;
