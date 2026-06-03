-- Schema for API Key Management: 0004_add_tenant_column.up.sql
-- Description: Add tenant column — binds each API key to a tenant identifier

-- Add tenant column (idempotent). DEFAULT '' backfills pre-existing rows with the
-- sentinel empty-string value so the migration is safe on populated tables.
ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS tenant TEXT NOT NULL DEFAULT '';

-- Index for tenant-scoped queries (used when S4 adds AND tenant = $X filtering)
CREATE INDEX IF NOT EXISTS idx_api_keys_tenant ON api_keys(tenant);
