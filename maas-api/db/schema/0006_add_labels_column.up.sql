-- Add labels column as JSONB for structured key-value storage
-- NULL for existing keys (backward compatibility)
ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS labels JSONB DEFAULT NULL;

-- GIN index for efficient JSONB containment queries (@> operator)
-- Supports fast lookups like: WHERE labels @> '{"cmdb_id": "AST123456"}'::jsonb
CREATE INDEX IF NOT EXISTS idx_api_keys_labels_gin ON api_keys USING GIN (labels);

-- Add check constraint for labels structure (enforce object type)
ALTER TABLE api_keys ADD CONSTRAINT api_keys_labels_is_object 
    CHECK (labels IS NULL OR jsonb_typeof(labels) = 'object');