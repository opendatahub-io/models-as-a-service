-- Rollback for 0005_add_tenant_key_hash_index

-- Drop the tenant-scoped index
DROP INDEX IF EXISTS idx_api_keys_key_hash_tenant;

-- Restore the original global unique index on key_hash
CREATE UNIQUE INDEX IF EXISTS idx_api_keys_key_hash
    ON api_keys(key_hash);
