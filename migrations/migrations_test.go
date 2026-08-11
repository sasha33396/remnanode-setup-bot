package migrations

import (
	"strings"
	"testing"
)

func TestInitialMigrationContainsRequiredPersistence(t *testing.T) {
	contents, err := Files.ReadFile("000001_deployments.up.sql")
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	sql := strings.ToLower(string(contents))
	for _, required := range []string{
		"create table deployments",
		"create table deployment_steps",
		"telegram_operator_user_id",
		"selected_remnawave_host_uuid",
		"remnawave_node_uuid",
		"safe_error_code",
		"safe_output_summary",
		"dns_failed",
	} {
		if !strings.Contains(sql, required) {
			t.Errorf("migration does not contain %q", required)
		}
	}
	if strings.Contains(sql, "root_password") {
		t.Fatal("migration must not persist root_password")
	}
}

func TestSSHHostKeyMigrationPersistsFingerprintWithoutPassword(t *testing.T) {
	contents, err := Files.ReadFile("000002_deployment_ssh_host_key.up.sql")
	if err != nil {
		t.Fatalf("read SSH host key migration: %v", err)
	}
	sql := strings.ToLower(string(contents))
	if !strings.Contains(sql, "ssh_host_key_fingerprint") {
		t.Fatal("migration does not contain SSH host key fingerprint")
	}
	if strings.Contains(sql, "password") {
		t.Fatal("SSH host key migration must not contain a password column")
	}
}

func TestCertificateManagerMigrationContainsMetadataWithoutPrivateKeys(t *testing.T) {
	contents, err := Files.ReadFile("000003_certificate_manager.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	sql := strings.ToLower(string(contents))
	for _, required := range []string{"certificate_records", "certificate_versions", "certificate_distributions", "certificate_fingerprint", "expires_at", "last_renewed_at", "active_version"} {
		if !strings.Contains(sql, required) {
			t.Errorf("certificate migration does not contain %q", required)
		}
	}
	for _, forbidden := range []string{"private_key", "privkey.pem", "cf_api_token"} {
		if strings.Contains(sql, forbidden) {
			t.Fatalf("certificate migration contains forbidden secret field %q", forbidden)
		}
	}
}

func TestRecoveryMigrationAddsManualReview(t *testing.T) {
	contents, err := Files.ReadFile("000004_production_recovery.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ToLower(string(contents)), "manual_review") {
		t.Fatal("recovery migration does not contain MANUAL_REVIEW")
	}
}

func TestLegacyTargetMigrationRequiresExplicitAcknowledgement(t *testing.T) {
	contents, err := Files.ReadFile("000005_certificate_legacy_targets.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	sql := strings.ToLower(string(contents))
	for _, required := range []string{"certificate_target_reviews", "manual_review", "legacy_acknowledged", "acknowledged_by", "node_ip"} {
		if !strings.Contains(sql, required) {
			t.Errorf("legacy target migration does not contain %q", required)
		}
	}
}

func TestMultiPanelMigrationScopesDeploymentsAndCertificates(t *testing.T) {
	contents, err := Files.ReadFile("000006_multi_panel_scope.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	sql := strings.ToLower(string(contents))
	for _, required := range []string{"alter table deployments", "certificate_records", "certificate_versions", "certificate_distributions", "certificate_target_reviews", "panel_id", "primary key (panel_id, sni_domain)"} {
		if !strings.Contains(sql, required) {
			t.Errorf("multi-panel migration does not contain %q", required)
		}
	}
}
