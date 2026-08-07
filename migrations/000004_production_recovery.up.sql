ALTER TABLE deployments
DROP CONSTRAINT deployments_status_check;

ALTER TABLE deployments
ADD CONSTRAINT deployments_status_check CHECK (status IN (
    'CREATED',
    'PREFLIGHT',
    'PREPARING_CERTIFICATE',
    'PROVISIONING',
    'CREATING_REMNAWAVE_NODE',
    'WAITING_REMNAWAVE',
    'ADDING_TO_DNS',
    'COMPLETED',
    'FAILED',
    'CANCELLED',
    'DNS_FAILED',
    'MANUAL_REVIEW'
));

DROP INDEX deployments_unfinished_idx;
CREATE INDEX deployments_unfinished_idx ON deployments (updated_at ASC)
WHERE status NOT IN ('COMPLETED', 'FAILED', 'CANCELLED', 'DNS_FAILED', 'MANUAL_REVIEW');
