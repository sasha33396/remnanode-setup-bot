ALTER TABLE deployments
ADD COLUMN ssh_host_key_fingerprint TEXT;

ALTER TABLE deployments
ADD CONSTRAINT deployments_ssh_host_key_fingerprint_format
CHECK (
    ssh_host_key_fingerprint IS NULL
    OR ssh_host_key_fingerprint LIKE 'SHA256:%'
);
