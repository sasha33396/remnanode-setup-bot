ALTER TABLE deployments
    DROP CONSTRAINT IF EXISTS deployments_advanced_fingerprint_check;

ALTER TABLE deployments
    DROP COLUMN IF EXISTS advanced_fingerprint;
