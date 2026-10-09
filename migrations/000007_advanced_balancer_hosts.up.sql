ALTER TABLE deployments
ADD COLUMN deployment_kind TEXT NOT NULL DEFAULT 'STANDARD',
ADD COLUMN advanced_balancer_tag TEXT,
ADD COLUMN advanced_egress_ip INET,
ADD COLUMN advanced_internal_squad_uuid UUID,
ADD COLUMN advanced_config_profile_uuid UUID,
ADD COLUMN advanced_config_profile_name TEXT,
ADD COLUMN advanced_inbound_uuid UUID;

ALTER TABLE deployments
ADD CONSTRAINT deployments_kind_check CHECK (deployment_kind IN ('STANDARD', 'ADVANCED_BALANCER')),
ADD CONSTRAINT deployments_advanced_balancer_check CHECK (
    deployment_kind = 'STANDARD' OR (
        advanced_balancer_tag IS NOT NULL AND
        btrim(advanced_balancer_tag) <> '' AND
        advanced_internal_squad_uuid IS NOT NULL
    )
);

ALTER TABLE deployments DROP CONSTRAINT deployments_status_check;
ALTER TABLE deployments ADD CONSTRAINT deployments_status_check CHECK (status IN (
    'CREATED', 'PREFLIGHT', 'PREPARING_CERTIFICATE', 'PROVISIONING',
    'CREATING_CONFIG_PROFILE', 'CREATING_REMNAWAVE_NODE', 'WAITING_REMNAWAVE',
    'ADDING_TO_DNS', 'CREATING_HOST', 'ADDING_TO_SQUAD',
    'COMPLETED', 'FAILED', 'CANCELLED', 'DNS_FAILED', 'MANUAL_REVIEW'
));
