-- Rollback for 0004_add_tenant_column.up.sql
-- Removes the tenant column from the api_keys table.
DROP INDEX IF EXISTS idx_api_keys_tenant;
ALTER TABLE api_keys DROP COLUMN IF EXISTS tenant;
