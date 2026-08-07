DROP INDEX deployments_unfinished_idx;
CREATE INDEX deployments_unfinished_idx ON deployments (updated_at ASC)
WHERE status NOT IN ('COMPLETED', 'FAILED', 'CANCELLED', 'DNS_FAILED');

UPDATE deployments
SET status = 'FAILED',
    safe_error_code = COALESCE(safe_error_code, 'MANUAL_REVIEW_DOWNGRADE'),
    safe_error_message = COALESCE(safe_error_message, 'Manual review was downgraded to failed'),
    completed_at = COALESCE(completed_at, now()),
    updated_at = now()
WHERE status = 'MANUAL_REVIEW';

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
    'DNS_FAILED'
));
