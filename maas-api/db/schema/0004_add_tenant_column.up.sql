-- Schema for API Key Management: 0004_add_tenant_column.up.sql
-- Description: Add tenant column — binds each API key to a tenant identifier

-- Add tenant column (idempotent). DEFAULT '' backfills pre-existing rows with the
-- sentinel empty-string value so the migration is safe on populated tables.
ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS tenant TEXT NOT NULL DEFAULT '';

-- Composite index for the primary tenant-scoped search pattern:
-- SELECT ... FROM api_keys WHERE tenant = $1 AND username = $2 ORDER BY created_at DESC
-- Also covers tenant-only lookups via leftmost prefix.
CREATE INDEX IF NOT EXISTS idx_api_keys_tenant_username_created
    ON api_keys(tenant, username, created_at DESC);
