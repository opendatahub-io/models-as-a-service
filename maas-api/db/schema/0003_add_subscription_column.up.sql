-- Schema for API Key Management: 0003_add_subscription_column.up.sql
-- Description: Add subscription column — binds each API key to a MaaSSubscription name at mint time

-- Add subscription column (idempotent). Value is MaaSSubscription metadata.name, resolved when the key is created.
ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS subscription TEXT NOT NULL;
