-- Rollback: Remove ephemeral column and index from api_keys table
DROP INDEX IF EXISTS idx_api_keys_ephemeral_expired;
ALTER TABLE api_keys DROP COLUMN IF EXISTS ephemeral;
