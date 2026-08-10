CREATE TABLE certificate_target_reviews (
    sni_domain TEXT NOT NULL,
    node_ip INET NOT NULL,
    state TEXT NOT NULL CHECK (state IN (
        'MANUAL_REVIEW',
        'LEGACY_ACKNOWLEDGED'
    )),
    reason TEXT NOT NULL CHECK (btrim(reason) <> ''),
    acknowledged_by BIGINT CHECK (acknowledged_by IS NULL OR acknowledged_by > 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    acknowledged_at TIMESTAMPTZ,
    PRIMARY KEY (sni_domain, node_ip),
    FOREIGN KEY (sni_domain) REFERENCES certificate_records(sni_domain) ON DELETE CASCADE,
    CHECK (
        (state = 'MANUAL_REVIEW' AND acknowledged_by IS NULL AND acknowledged_at IS NULL)
        OR
        (state = 'LEGACY_ACKNOWLEDGED' AND acknowledged_by IS NOT NULL AND acknowledged_at IS NOT NULL)
    )
);

CREATE INDEX certificate_target_reviews_state_idx
ON certificate_target_reviews (state, updated_at DESC);
