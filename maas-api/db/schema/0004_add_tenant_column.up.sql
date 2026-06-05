-- Schema for API Key Management: 0004_add_tenant_column.up.sql
-- Description: Add tenant_id column — binds each API key to a tenant identifier

-- Add tenant_id column (idempotent). The ADR default tenant backfills pre-existing
-- single-tenant rows so existing API keys remain valid after upgrade.
ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS tenant_id TEXT NOT NULL DEFAULT 'models-as-a-service';

-- Index for tenant-scoped queries.
CREATE INDEX IF NOT EXISTS idx_api_keys_tenant_id ON api_keys(tenant_id);
