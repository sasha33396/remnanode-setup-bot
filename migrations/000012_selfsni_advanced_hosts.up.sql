ALTER TABLE deployments
ADD COLUMN advanced_host_type TEXT NOT NULL DEFAULT 'SELFSTEAL',
ADD COLUMN advanced_reality_sni TEXT;

ALTER TABLE deployments
ADD CONSTRAINT deployments_advanced_host_type_check
CHECK (advanced_host_type IN ('SELFSTEAL', 'SELFSNI')),
ADD CONSTRAINT deployments_selfsni_reality_check
CHECK (
    deployment_kind <> 'ADVANCED_BALANCER' OR
    advanced_host_type = 'SELFSTEAL' OR
    (advanced_reality_sni IS NOT NULL AND btrim(advanced_reality_sni) <> '')
);
