-- Preserve explicit API-key retention decisions across observer retirement.
ALTER TABLE subscription_observations
    ADD COLUMN IF NOT EXISTS retain_api_keys BOOLEAN NOT NULL DEFAULT FALSE;
