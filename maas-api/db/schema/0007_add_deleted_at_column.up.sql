-- Schema for retained API-key soft deletion.
-- Keys invalidated by tenant or subscription lifecycle cleanup remain available
-- for the configured retention period for audit purposes, but are never valid.

ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS deleted_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS idx_api_keys_deleted_at
    ON api_keys(deleted_at)
    WHERE deleted_at IS NOT NULL;

-- Lifecycle invalidation filters by tenant and, for subscription deletion,
-- subscription. Keep only live rows in this index because soft-deleted rows
-- are never targeted a second time.
CREATE INDEX IF NOT EXISTS idx_api_keys_tenant_subscription_active
    ON api_keys(tenant, subscription)
    WHERE deleted_at IS NULL;
