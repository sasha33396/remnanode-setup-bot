ALTER TABLE deployments DROP CONSTRAINT deployments_status_check;
ALTER TABLE deployments ADD CONSTRAINT deployments_status_check CHECK (status IN (
    'CREATED', 'PREFLIGHT', 'PREPARING_CERTIFICATE', 'PROVISIONING',
    'CREATING_REMNAWAVE_NODE', 'WAITING_REMNAWAVE', 'ADDING_TO_DNS',
    'COMPLETED', 'FAILED', 'CANCELLED', 'DNS_FAILED', 'MANUAL_REVIEW'
));

ALTER TABLE deployments
DROP CONSTRAINT deployments_advanced_balancer_check,
DROP CONSTRAINT deployments_kind_check,
DROP COLUMN advanced_inbound_uuid,
DROP COLUMN advanced_config_profile_name,
DROP COLUMN advanced_config_profile_uuid,
DROP COLUMN advanced_internal_squad_uuid,
DROP COLUMN advanced_egress_ip,
DROP COLUMN advanced_balancer_tag,
DROP COLUMN deployment_kind;
