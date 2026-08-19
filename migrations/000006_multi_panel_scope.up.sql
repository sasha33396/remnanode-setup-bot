ALTER TABLE deployments
    ADD COLUMN panel_id TEXT NOT NULL DEFAULT 'default'
    CHECK (panel_id ~ '^[a-z0-9][a-z0-9_-]{0,31}$');

CREATE INDEX deployments_panel_recent_idx
    ON deployments (panel_id, created_at DESC);
CREATE INDEX deployments_panel_node_idx
    ON deployments (panel_id, remnawave_node_uuid)
    WHERE remnawave_node_uuid IS NOT NULL;
ALTER TABLE deployments ADD CONSTRAINT deployments_panel_id_id_key UNIQUE (panel_id, id);

ALTER TABLE certificate_records DROP CONSTRAINT certificate_records_active_version_fk;
ALTER TABLE certificate_versions DROP CONSTRAINT certificate_versions_sni_domain_fkey;
ALTER TABLE certificate_distributions DROP CONSTRAINT certificate_distributions_sni_domain_version_fkey;
ALTER TABLE certificate_distributions DROP CONSTRAINT certificate_distributions_deployment_id_fkey;
ALTER TABLE certificate_target_reviews DROP CONSTRAINT certificate_target_reviews_sni_domain_fkey;

ALTER TABLE certificate_records ADD COLUMN panel_id TEXT NOT NULL DEFAULT 'default'
    CHECK (panel_id ~ '^[a-z0-9][a-z0-9_-]{0,31}$');
ALTER TABLE certificate_versions ADD COLUMN panel_id TEXT NOT NULL DEFAULT 'default'
    CHECK (panel_id ~ '^[a-z0-9][a-z0-9_-]{0,31}$');
ALTER TABLE certificate_distributions ADD COLUMN panel_id TEXT NOT NULL DEFAULT 'default'
    CHECK (panel_id ~ '^[a-z0-9][a-z0-9_-]{0,31}$');
ALTER TABLE certificate_target_reviews ADD COLUMN panel_id TEXT NOT NULL DEFAULT 'default'
    CHECK (panel_id ~ '^[a-z0-9][a-z0-9_-]{0,31}$');

ALTER TABLE certificate_records DROP CONSTRAINT certificate_records_pkey;
ALTER TABLE certificate_records ADD PRIMARY KEY (panel_id, sni_domain);
ALTER TABLE certificate_versions DROP CONSTRAINT certificate_versions_pkey;
ALTER TABLE certificate_versions ADD PRIMARY KEY (panel_id, sni_domain, version);
ALTER TABLE certificate_distributions DROP CONSTRAINT certificate_distributions_pkey;
ALTER TABLE certificate_distributions ADD PRIMARY KEY (panel_id, sni_domain, version, deployment_id);
ALTER TABLE certificate_target_reviews DROP CONSTRAINT certificate_target_reviews_pkey;
ALTER TABLE certificate_target_reviews ADD PRIMARY KEY (panel_id, sni_domain, node_ip);

ALTER TABLE certificate_versions ADD CONSTRAINT certificate_versions_record_fk
    FOREIGN KEY (panel_id, sni_domain)
    REFERENCES certificate_records(panel_id, sni_domain) ON DELETE CASCADE;
ALTER TABLE certificate_records ADD CONSTRAINT certificate_records_active_version_fk
    FOREIGN KEY (panel_id, sni_domain, active_version)
    REFERENCES certificate_versions(panel_id, sni_domain, version)
    DEFERRABLE INITIALLY DEFERRED;
ALTER TABLE certificate_distributions ADD CONSTRAINT certificate_distributions_version_fk
    FOREIGN KEY (panel_id, sni_domain, version)
    REFERENCES certificate_versions(panel_id, sni_domain, version) ON DELETE CASCADE;
ALTER TABLE certificate_distributions ADD CONSTRAINT certificate_distributions_deployment_fk
    FOREIGN KEY (panel_id, deployment_id)
    REFERENCES deployments(panel_id, id) ON DELETE CASCADE;
ALTER TABLE certificate_target_reviews ADD CONSTRAINT certificate_target_reviews_record_fk
    FOREIGN KEY (panel_id, sni_domain)
    REFERENCES certificate_records(panel_id, sni_domain) ON DELETE CASCADE;

DROP INDEX certificate_records_expiry_idx;
CREATE INDEX certificate_records_expiry_idx
    ON certificate_records (panel_id, expires_at ASC)
    WHERE active_version IS NOT NULL;
DROP INDEX deployments_sni_domain_idx;
CREATE INDEX deployments_panel_sni_domain_idx
    ON deployments (panel_id, lower(sni_domain), target_vps_ip);
DROP INDEX certificate_target_reviews_state_idx;
CREATE INDEX certificate_target_reviews_state_idx
    ON certificate_target_reviews (panel_id, state, updated_at DESC);
