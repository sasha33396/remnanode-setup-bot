CREATE TABLE certificate_records (
    sni_domain TEXT PRIMARY KEY CHECK (sni_domain = lower(btrim(sni_domain)) AND position('.' IN sni_domain) > 0),
    certificate_fingerprint TEXT,
    serial_number TEXT,
    issued_at TIMESTAMPTZ,
    expires_at TIMESTAMPTZ,
    last_renewed_at TIMESTAMPTZ,
    status TEXT NOT NULL CHECK (status IN (
        'ISSUING',
        'ACTIVE',
        'RENEWAL_DUE',
        'DISTRIBUTION_FAILED',
        'INVALID'
    )),
    active_version TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (
        active_version IS NULL
        OR (
            certificate_fingerprint IS NOT NULL
            AND serial_number IS NOT NULL
            AND issued_at IS NOT NULL
            AND expires_at IS NOT NULL
        )
    )
);

CREATE TABLE certificate_versions (
    sni_domain TEXT NOT NULL REFERENCES certificate_records(sni_domain) ON DELETE CASCADE,
    version TEXT NOT NULL,
    certificate_fingerprint TEXT NOT NULL,
    serial_number TEXT NOT NULL,
    issued_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    status TEXT NOT NULL CHECK (status IN (
        'PENDING',
        'ACTIVE',
        'SUPERSEDED',
        'DISTRIBUTION_FAILED',
        'INVALID'
    )),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (sni_domain, version),
    CHECK (expires_at > issued_at)
);

ALTER TABLE certificate_records
ADD CONSTRAINT certificate_records_active_version_fk
FOREIGN KEY (sni_domain, active_version)
REFERENCES certificate_versions(sni_domain, version)
DEFERRABLE INITIALLY DEFERRED;

CREATE TABLE certificate_distributions (
    sni_domain TEXT NOT NULL,
    version TEXT NOT NULL,
    deployment_id UUID NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
    node_ip INET NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('SUCCEEDED', 'FAILED')),
    safe_error_message TEXT,
    attempted_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (sni_domain, version, deployment_id),
    FOREIGN KEY (sni_domain, version)
        REFERENCES certificate_versions(sni_domain, version) ON DELETE CASCADE
);

CREATE INDEX certificate_records_expiry_idx
ON certificate_records (expires_at ASC)
WHERE active_version IS NOT NULL;

CREATE INDEX deployments_sni_domain_idx
ON deployments (lower(sni_domain), target_vps_ip);
