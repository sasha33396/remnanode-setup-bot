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
