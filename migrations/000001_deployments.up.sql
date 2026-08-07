CREATE TABLE deployments (
    id UUID PRIMARY KEY,
    telegram_operator_user_id BIGINT NOT NULL CHECK (telegram_operator_user_id > 0),
    selected_remnawave_host_uuid UUID NOT NULL,
    selected_host_remark TEXT NOT NULL,
    sni_domain TEXT NOT NULL CHECK (btrim(sni_domain) <> ''),
    node_name TEXT NOT NULL CHECK (btrim(node_name) <> ''),
    target_vps_ip INET NOT NULL,
    remnawave_node_uuid UUID,
    status TEXT NOT NULL CHECK (status IN (
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
    )),
    current_step TEXT NOT NULL,
    safe_error_code TEXT,
    safe_error_message TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    CHECK (completed_at IS NULL OR started_at IS NOT NULL)
);

CREATE TABLE deployment_steps (
    deployment_id UUID NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
    step_name TEXT NOT NULL CHECK (btrim(step_name) <> ''),
    status TEXT NOT NULL CHECK (status IN (
        'PENDING',
        'RUNNING',
        'COMPLETED',
        'FAILED',
        'SKIPPED'
    )),
    safe_output_summary TEXT,
    error_message TEXT,
    started_at TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    PRIMARY KEY (deployment_id, step_name),
    CHECK (completed_at IS NULL OR started_at IS NOT NULL)
);

CREATE INDEX deployments_recent_idx ON deployments (created_at DESC);

CREATE INDEX deployments_unfinished_idx ON deployments (updated_at ASC)
WHERE status NOT IN ('COMPLETED', 'FAILED', 'CANCELLED', 'DNS_FAILED');
