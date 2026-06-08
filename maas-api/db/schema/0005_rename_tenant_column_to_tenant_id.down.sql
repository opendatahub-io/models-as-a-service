-- Rollback for 0005_rename_tenant_column_to_tenant_id.up.sql
-- Restores the migration-0004 tenant column shape.

DROP INDEX IF EXISTS idx_api_keys_tenant_id;

DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM information_schema.columns
        WHERE table_schema = current_schema()
          AND table_name = 'api_keys'
          AND column_name = 'tenant'
    ) THEN
        IF EXISTS (
            SELECT 1
            FROM information_schema.columns
            WHERE table_schema = current_schema()
              AND table_name = 'api_keys'
              AND column_name = 'tenant_id'
        ) THEN
            UPDATE api_keys
            SET tenant = tenant_id
            WHERE tenant = '' OR tenant IS NULL;
        END IF;
    ELSIF EXISTS (
        SELECT 1
        FROM information_schema.columns
        WHERE table_schema = current_schema()
          AND table_name = 'api_keys'
          AND column_name = 'tenant_id'
    ) THEN
        ALTER TABLE api_keys RENAME COLUMN tenant_id TO tenant;
    ELSE
        ALTER TABLE api_keys ADD COLUMN tenant TEXT NOT NULL DEFAULT '';
    END IF;
END $$;

ALTER TABLE api_keys DROP COLUMN IF EXISTS tenant_id;

UPDATE api_keys
SET tenant = ''
WHERE tenant IS NULL;

ALTER TABLE api_keys ALTER COLUMN tenant SET DEFAULT '';
ALTER TABLE api_keys ALTER COLUMN tenant SET NOT NULL;

CREATE INDEX IF NOT EXISTS idx_api_keys_tenant ON api_keys(tenant);
