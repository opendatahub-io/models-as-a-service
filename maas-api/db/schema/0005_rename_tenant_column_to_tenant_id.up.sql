-- Schema for API Key Management: 0005_rename_tenant_column_to_tenant_id.up.sql
-- Description: Rename tenant to tenant_id and backfill the default MaaS tenant

DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM information_schema.columns
        WHERE table_schema = current_schema()
          AND table_name = 'api_keys'
          AND column_name = 'tenant_id'
    ) THEN
        -- Already migrated, likely from a development build of this branch.
        IF EXISTS (
            SELECT 1
            FROM information_schema.columns
            WHERE table_schema = current_schema()
              AND table_name = 'api_keys'
              AND column_name = 'tenant'
        ) THEN
            UPDATE api_keys
            SET tenant_id = tenant
            WHERE (tenant_id IS NULL OR tenant_id = '') AND tenant <> '';
        END IF;
    ELSIF EXISTS (
        SELECT 1
        FROM information_schema.columns
        WHERE table_schema = current_schema()
          AND table_name = 'api_keys'
          AND column_name = 'tenant'
    ) THEN
        ALTER TABLE api_keys RENAME COLUMN tenant TO tenant_id;
    ELSE
        ALTER TABLE api_keys ADD COLUMN tenant_id TEXT NOT NULL DEFAULT 'models-as-a-service';
    END IF;
END $$;

UPDATE api_keys
SET tenant_id = 'models-as-a-service'
WHERE tenant_id IS NULL OR tenant_id = '';

ALTER TABLE api_keys ALTER COLUMN tenant_id SET DEFAULT 'models-as-a-service';
ALTER TABLE api_keys ALTER COLUMN tenant_id SET NOT NULL;

DROP INDEX IF EXISTS idx_api_keys_tenant;
CREATE INDEX IF NOT EXISTS idx_api_keys_tenant_id ON api_keys(tenant_id);
