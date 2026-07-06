-- Rollback for 0005_add_tenant_key_hash_index
--
-- WARNING: This rollback should only be run as part of rolling back migration 0004.
-- Rolling back 0005 alone (keeping the tenant column from 0004) is not a supported
-- configuration, as the tenant-scoped index is required for correct multi-tenant operation.
--
-- If duplicate key_hash values exist across tenants, this rollback will fail with:
-- "ERROR: could not create unique index... Key (key_hash)=(...) is duplicated"
-- This is expected and indicates manual intervention is required.

-- Drop the tenant-scoped index
DROP INDEX IF EXISTS idx_api_keys_key_hash_tenant;

-- Restore the original global unique index on key_hash
CREATE UNIQUE INDEX IF NOT EXISTS idx_api_keys_key_hash
    ON api_keys(key_hash);
