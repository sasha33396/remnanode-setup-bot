CREATE TABLE abuse_log_checkpoints (
    panel_id TEXT NOT NULL,
    node_uuid UUID NOT NULL,
    inode BIGINT NOT NULL DEFAULT 0,
    byte_offset BIGINT NOT NULL DEFAULT 0,
    partial_line TEXT NOT NULL DEFAULT '',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (panel_id, node_uuid)
);

CREATE TABLE abuse_events (
    id BIGSERIAL PRIMARY KEY,
    fingerprint BYTEA NOT NULL UNIQUE,
    panel_id TEXT NOT NULL,
    node_uuid UUID NOT NULL,
    node_name TEXT NOT NULL,
    node_ip INET NOT NULL,
    occurred_at TIMESTAMPTZ NOT NULL,
    client_ip INET NOT NULL,
    client_port INTEGER NOT NULL,
    destination_ip INET,
    destination TEXT NOT NULL,
    destination_port INTEGER NOT NULL,
    network TEXT NOT NULL,
    inbound TEXT NOT NULL,
    outbound TEXT NOT NULL,
    user_id TEXT NOT NULL,
    accepted BOOLEAN NOT NULL,
    blocked BOOLEAN NOT NULL,
    asn BIGINT NOT NULL DEFAULT 0,
    provider TEXT NOT NULL DEFAULT 'UNKNOWN',
    network_category TEXT NOT NULL DEFAULT 'UNKNOWN',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX abuse_events_identity_time_idx ON abuse_events (panel_id, user_id, occurred_at DESC);
CREATE INDEX abuse_events_node_time_idx ON abuse_events (panel_id, node_uuid, occurred_at DESC);

CREATE TABLE abuse_user_aggregates (
    panel_id TEXT NOT NULL,
    user_id TEXT NOT NULL,
    smtp25_attempts_10m INTEGER NOT NULL DEFAULT 0,
    submission_attempts_1h INTEGER NOT NULL DEFAULT 0,
    unique_mail_destinations_1h INTEGER NOT NULL DEFAULT 0,
    unique_ips_1h INTEGER NOT NULL DEFAULT 0,
    unique_ips_24h INTEGER NOT NULL DEFAULT 0,
    unique_asns_24h INTEGER NOT NULL DEFAULT 0,
    risk TEXT NOT NULL,
    score INTEGER NOT NULL,
    reasons JSONB NOT NULL DEFAULT '[]'::jsonb,
    first_seen TIMESTAMPTZ,
    last_seen TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (panel_id, user_id)
);

CREATE TABLE abuse_ip_cache (
    ip INET PRIMARY KEY,
    asn BIGINT NOT NULL DEFAULT 0,
    provider TEXT NOT NULL DEFAULT 'UNKNOWN',
    network_category TEXT NOT NULL DEFAULT 'UNKNOWN',
    expires_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE abuse_incidents (
    id TEXT PRIMARY KEY,
    panel_id TEXT NOT NULL,
    user_id TEXT NOT NULL,
    anomaly_type TEXT NOT NULL,
    risk TEXT NOT NULL,
    score INTEGER NOT NULL,
    reasons JSONB NOT NULL DEFAULT '[]'::jsonb,
    first_seen TIMESTAMPTZ NOT NULL,
    last_seen TIMESTAMPTZ NOT NULL,
    last_alerted_at TIMESTAMPTZ,
    ignored_until TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (panel_id, user_id, anomaly_type)
);

CREATE INDEX abuse_incidents_panel_user_idx ON abuse_incidents (panel_id, user_id);
