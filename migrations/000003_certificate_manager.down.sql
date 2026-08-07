DROP INDEX IF EXISTS deployments_sni_domain_idx;
DROP TABLE IF EXISTS certificate_distributions;
ALTER TABLE certificate_records
DROP CONSTRAINT IF EXISTS certificate_records_active_version_fk;
DROP TABLE IF EXISTS certificate_versions;
DROP TABLE IF EXISTS certificate_records;
