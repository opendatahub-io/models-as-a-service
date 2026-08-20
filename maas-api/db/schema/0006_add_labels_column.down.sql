-- Drop constraint first
ALTER TABLE api_keys DROP CONSTRAINT IF EXISTS api_keys_labels_is_object;

-- Drop index
DROP INDEX IF EXISTS idx_api_keys_labels_gin;

-- Drop column
ALTER TABLE api_keys DROP COLUMN IF EXISTS labels;