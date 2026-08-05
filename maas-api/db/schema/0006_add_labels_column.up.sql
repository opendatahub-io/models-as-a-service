-- Add labels column as JSONB for structured key-value storage
-- NULL for existing keys (backward compatibility)
ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS labels JSONB DEFAULT NULL;

-- Add check constraint for labels structure (enforce object type).
-- PostgreSQL doesn't support ADD CONSTRAINT IF NOT EXISTS for CHECK constraints, so use a DO $$ ... $$ block that checks
-- pg_constraint first to avoid an error if the constraint already exists from a prior run of this migration:
  DO $$
  BEGIN
      IF NOT EXISTS (
          SELECT 1 FROM pg_constraint WHERE conname = 'api_keys_labels_is_object'
      ) THEN
          ALTER TABLE api_keys ADD CONSTRAINT api_keys_labels_is_object
              CHECK (labels IS NULL OR jsonb_typeof(labels) = 'object');
      END IF;
  END
  $$;

-- Note: The GIN index for efficient JSONB containment queries (@> operator) cannot be run inside a transaction block.
-- It is applied in the runConcurrentMigrations function in the db_driver.go file.
