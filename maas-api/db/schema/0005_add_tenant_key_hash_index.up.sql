-- Schema for API Key Management: 0005_add_tenant_key_hash_index.up.sql
-- Description: Add composite index for tenant-scoped key_hash lookups

-- Drop the old unique index on key_hash alone since keys must be unique per-tenant, not globally
DROP INDEX IF EXISTS idx_api_keys_key_hash;

-- Create composite unique index for tenant-scoped validation lookups
-- This is CRITICAL for performance - Authorino validation is called on every inference request
-- Query pattern: SELECT ... FROM api_keys WHERE key_hash = $1 AND tenant = $2
-- The (key_hash, tenant) order allows Postgres to quickly find the hash and verify tenant
CREATE UNIQUE INDEX IF NOT EXISTS idx_api_keys_key_hash_tenant
    ON api_keys(key_hash, tenant);
