ALTER TABLE deployments
DROP CONSTRAINT IF EXISTS deployments_selfsni_reality_check,
DROP CONSTRAINT IF EXISTS deployments_advanced_host_type_check,
DROP COLUMN IF EXISTS advanced_reality_sni,
DROP COLUMN IF EXISTS advanced_host_type;
