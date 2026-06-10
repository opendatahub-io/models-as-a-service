-- Rollback for 0004_add_tenant_column.up.sql
-- Removes the tenant column from the api_keys table.
ALTER TABLE api_keys DROP COLUMN IF EXISTS tenant;
