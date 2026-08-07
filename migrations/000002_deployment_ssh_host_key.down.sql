ALTER TABLE deployments
DROP CONSTRAINT IF EXISTS deployments_ssh_host_key_fingerprint_format;

ALTER TABLE deployments
DROP COLUMN IF EXISTS ssh_host_key_fingerprint;
