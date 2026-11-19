-- Remove identity-aware API-key binding and observer state. The down migration
-- is intentionally blocked when active keys still reference an identity so a
-- rollback cannot silently return tombstoned or rotated keys to name-only
-- validation.
DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM api_keys
        WHERE subscription_id IS NOT NULL
          AND deleted_at IS NULL
          AND status = 'active'
    ) THEN
        RAISE EXCEPTION 'cannot remove subscription identity schema while active API keys remain bound';
    END IF;
END
$$;

DROP INDEX IF EXISTS idx_api_keys_tenant_subscription_id_active;

ALTER TABLE api_keys
    DROP COLUMN IF EXISTS subscription_id,
    DROP COLUMN IF EXISTS subscription_namespace;

DROP TABLE IF EXISTS subscription_observations;
DROP TABLE IF EXISTS subscription_observers;
DROP TABLE IF EXISTS subscription_identities;
