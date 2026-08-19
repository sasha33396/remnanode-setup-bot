DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM deployments WHERE panel_id <> 'default')
       OR EXISTS (SELECT 1 FROM certificate_records WHERE panel_id <> 'default') THEN
        RAISE EXCEPTION 'cannot remove multi-panel scope while non-default panel data exists';
    END IF;
END $$;

DROP INDEX certificate_target_reviews_state_idx;
DROP INDEX deployments_panel_sni_domain_idx;
DROP INDEX certificate_records_expiry_idx;

ALTER TABLE certificate_records DROP CONSTRAINT certificate_records_active_version_fk;
ALTER TABLE certificate_distributions DROP CONSTRAINT certificate_distributions_version_fk;
ALTER TABLE certificate_distributions DROP CONSTRAINT certificate_distributions_deployment_fk;
ALTER TABLE certificate_target_reviews DROP CONSTRAINT certificate_target_reviews_record_fk;
ALTER TABLE certificate_versions DROP CONSTRAINT certificate_versions_record_fk;

ALTER TABLE certificate_target_reviews DROP CONSTRAINT certificate_target_reviews_pkey;
ALTER TABLE certificate_distributions DROP CONSTRAINT certificate_distributions_pkey;
ALTER TABLE certificate_versions DROP CONSTRAINT certificate_versions_pkey;
ALTER TABLE certificate_records DROP CONSTRAINT certificate_records_pkey;

ALTER TABLE certificate_records ADD PRIMARY KEY (sni_domain);
ALTER TABLE certificate_versions ADD PRIMARY KEY (sni_domain, version);
ALTER TABLE certificate_distributions ADD PRIMARY KEY (sni_domain, version, deployment_id);
ALTER TABLE certificate_target_reviews ADD PRIMARY KEY (sni_domain, node_ip);

ALTER TABLE certificate_versions ADD CONSTRAINT certificate_versions_sni_domain_fkey
    FOREIGN KEY (sni_domain) REFERENCES certificate_records(sni_domain) ON DELETE CASCADE;
ALTER TABLE certificate_records ADD CONSTRAINT certificate_records_active_version_fk
    FOREIGN KEY (sni_domain, active_version)
    REFERENCES certificate_versions(sni_domain, version) DEFERRABLE INITIALLY DEFERRED;
ALTER TABLE certificate_distributions ADD CONSTRAINT certificate_distributions_sni_domain_version_fkey
    FOREIGN KEY (sni_domain, version)
    REFERENCES certificate_versions(sni_domain, version) ON DELETE CASCADE;
ALTER TABLE certificate_distributions ADD CONSTRAINT certificate_distributions_deployment_id_fkey
    FOREIGN KEY (deployment_id) REFERENCES deployments(id) ON DELETE CASCADE;
ALTER TABLE certificate_target_reviews ADD CONSTRAINT certificate_target_reviews_sni_domain_fkey
    FOREIGN KEY (sni_domain) REFERENCES certificate_records(sni_domain) ON DELETE CASCADE;

ALTER TABLE certificate_target_reviews DROP COLUMN panel_id;
ALTER TABLE certificate_distributions DROP COLUMN panel_id;
ALTER TABLE certificate_versions DROP COLUMN panel_id;
ALTER TABLE certificate_records DROP COLUMN panel_id;
DROP INDEX deployments_panel_node_idx;
DROP INDEX deployments_panel_recent_idx;
ALTER TABLE deployments DROP CONSTRAINT deployments_panel_id_id_key;
ALTER TABLE deployments DROP COLUMN panel_id;

CREATE INDEX certificate_records_expiry_idx
    ON certificate_records (expires_at ASC) WHERE active_version IS NOT NULL;
CREATE INDEX deployments_sni_domain_idx
    ON deployments (lower(sni_domain), target_vps_ip);
CREATE INDEX certificate_target_reviews_state_idx
    ON certificate_target_reviews (state, updated_at DESC);
