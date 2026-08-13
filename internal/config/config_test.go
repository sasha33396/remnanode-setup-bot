package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadValidConfiguration(t *testing.T) {
	values := validValues()
	cfg, err := load(mapLookup(values))
	if err != nil {
		t.Fatalf("load() error = %v", err)
	}
	if got, want := len(cfg.TelegramAllowedUsers), 2; got != want {
		t.Fatalf("allowed users count = %d, want %d", got, want)
	}
	if got, want := cfg.HealthAddr, defaultHealthAddr; got != want {
		t.Errorf("health address = %q, want %q", got, want)
	}
	if cfg.XraySNIRepoURL != defaultXraySNIRepoURL || cfg.XraySNIRef != defaultXraySNIRef {
		t.Fatalf("xray-sni defaults = %q %q", cfg.XraySNIRepoURL, cfg.XraySNIRef)
	}
	if len(cfg.Panels) != 1 || cfg.Panels[0].ID != "default" || cfg.Panels[0].DNSMode != DNSModeEnabled {
		t.Fatalf("legacy panel = %#v", cfg.Panels)
	}
}

func TestLoadMultiplePanelsFromYAMLFile(t *testing.T) {
	values := validValues()
	for _, name := range []string{"REMNAWAVE_URL", "REMNAWAVE_TOKEN", "DNS_BALANCER_URL", "DNS_BALANCER_TOKEN", "CF_API_TOKEN"} {
		delete(values, name)
	}
	values["MAIN_REMNAWAVE_TOKEN"] = "main-remnawave-secret"
	values["MAIN_DNS_TOKEN"] = "main-dns-secret"
	values["MAIN_CF_TOKEN"] = "main-cloudflare-secret"
	values["SECOND_REMNAWAVE_TOKEN"] = "second-remnawave-secret"
	values["SECOND_CF_TOKEN"] = "second-cloudflare-secret"
	path := filepath.Join(t.TempDir(), "panels.yml")
	contents := `panels:
  - id: default
    name: Main
    remnawave:
      url: https://main-panel.example
      token_env: MAIN_REMNAWAVE_TOKEN
      api_ip: 192.0.2.20
    dns:
      mode: enabled
      url: https://main-dns.example
      token_env: MAIN_DNS_TOKEN
    certificate:
      cloudflare_token_env: MAIN_CF_TOKEN
  - id: second
    name: Second
    remnawave:
      url: https://second-panel.example
      token_env: SECOND_REMNAWAVE_TOKEN
      api_ip: 192.0.2.30
    dns:
      mode: disabled
    certificate:
      cloudflare_token_env: SECOND_CF_TOKEN
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	values["PANELS_CONFIG_FILE"] = path

	cfg, err := load(mapLookup(values))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Panels) != 2 || cfg.Panels[0].ID != "default" || cfg.Panels[1].DNSMode != DNSModeDisabled {
		t.Fatalf("panels = %#v", cfg.Panels)
	}
	if cfg.Panels[0].DNSBalancerToken != values["MAIN_DNS_TOKEN"] || cfg.Panels[1].CloudflareAPIToken != values["SECOND_CF_TOKEN"] {
		t.Fatal("YAML panel secret references were not resolved")
	}
	if got, want := cfg.Panels[0].RemnawaveAPIIP.String(), "192.0.2.20"; got != want {
		t.Fatalf("main panel Remnawave API IP = %q, want %q", got, want)
	}
	if got, want := cfg.Panels[1].RemnawaveAPIIP.String(), "192.0.2.30"; got != want {
		t.Fatalf("second panel Remnawave API IP = %q, want %q", got, want)
	}
}

func TestLoadRejectsTwoPanelConfigurationSources(t *testing.T) {
	values := validValues()
	values["PANELS_CONFIG_FILE"] = "panels.yml"
	values["PANELS_JSON"] = `[{"id":"default"}]`
	_, err := load(mapLookup(values))
	if err == nil || !strings.Contains(err.Error(), "cannot be used together") {
		t.Fatalf("load() error = %v", err)
	}
}

func TestLoadMultiplePanelsWithIndependentDNS(t *testing.T) {
	values := validValues()
	for _, name := range []string{"REMNAWAVE_URL", "REMNAWAVE_TOKEN", "DNS_BALANCER_URL", "DNS_BALANCER_TOKEN"} {
		delete(values, name)
	}
	values["EU_REMNAWAVE_TOKEN"] = "eu-remnawave-secret"
	values["EU_DNS_TOKEN"] = "eu-dns-secret"
	values["TEST_REMNAWAVE_TOKEN"] = "test-remnawave-secret"
	values["PANELS_JSON"] = `[
		{"id":"europe","name":"Europe","remnawave_url":"https://eu-panel.example","remnawave_token_env":"EU_REMNAWAVE_TOKEN","remnawave_api_ip":"192.0.2.40","dns":{"mode":"enabled","url":"https://eu-dns.example","token_env":"EU_DNS_TOKEN"}},
		{"id":"test","name":"Test","remnawave_url":"https://test-panel.example","remnawave_token_env":"TEST_REMNAWAVE_TOKEN","remnawave_api_ip":"192.0.2.50","dns":{"mode":"disabled"}}
	]`
	cfg, err := load(mapLookup(values))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Panels) != 2 || cfg.Panels[0].DNSMode != DNSModeEnabled || cfg.Panels[1].DNSMode != DNSModeDisabled {
		t.Fatalf("panels = %#v", cfg.Panels)
	}
	if cfg.Panels[0].RemnawaveToken != values["EU_REMNAWAVE_TOKEN"] || cfg.Panels[0].DNSBalancerToken != values["EU_DNS_TOKEN"] {
		t.Fatal("panel secret references were not resolved")
	}
	if got, want := cfg.Panels[0].RemnawaveAPIIP.String(), "192.0.2.40"; got != want {
		t.Fatalf("Europe Remnawave API IP = %q, want %q", got, want)
	}
	if got, want := cfg.Panels[1].RemnawaveAPIIP.String(), "192.0.2.50"; got != want {
		t.Fatalf("Test Remnawave API IP = %q, want %q", got, want)
	}
}

func TestLoadRejectsPanelWithoutRemnawaveAPIIP(t *testing.T) {
	values := validValues()
	values["EU_REMNAWAVE_TOKEN"] = "eu-remnawave-secret"
	values["PANELS_JSON"] = `[{"id":"europe","name":"Europe","remnawave_url":"https://eu-panel.example","remnawave_token_env":"EU_REMNAWAVE_TOKEN","dns":{"mode":"disabled"},"cloudflare_token_env":"CF_API_TOKEN"}]`

	_, err := load(mapLookup(values))
	if err == nil || !strings.Contains(err.Error(), "panel europe Remnawave API IP") {
		t.Fatalf("load() error = %v, want panel API IP validation error", err)
	}
}
func TestLoadXraySNIConfiguration(t *testing.T) {
	values := validValues()
	values["XRAY_SNI_REPO_URL"] = "https://git.example.com/xray-sni.git"
	values["XRAY_SNI_REF"] = "v2.0.0"
	cfg, err := load(mapLookup(values))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.XraySNIRepoURL != values["XRAY_SNI_REPO_URL"] || cfg.XraySNIRef != values["XRAY_SNI_REF"] {
		t.Fatalf("xray-sni config = %q %q", cfg.XraySNIRepoURL, cfg.XraySNIRef)
	}

	values["XRAY_SNI_REF"] = "main"
	if _, err := load(mapLookup(values)); err == nil || !strings.Contains(err.Error(), "XRAY_SNI_REF") {
		t.Fatalf("main ref error = %v", err)
	}
	values["XRAY_SNI_REF"] = "v2.0.0"
	values["XRAY_SNI_REPO_URL"] = "http://user:password@git.example.com/xray-sni.git"
	if _, err := load(mapLookup(values)); err == nil || !strings.Contains(err.Error(), "XRAY_SNI_REPO_URL") {
		t.Fatalf("unsafe repository URL error = %v", err)
	}
}

func TestLoadReportsMissingVariablesWithoutValues(t *testing.T) {
	values := validValues()
	delete(values, "REMNAWAVE_TOKEN")

	_, err := load(mapLookup(values))
	if err == nil || !strings.Contains(err.Error(), "REMNAWAVE_TOKEN is required") {
		t.Fatalf("load() error = %v, want missing variable error", err)
	}
	if strings.Contains(err.Error(), values["TELEGRAM_BOT_TOKEN"]) {
		t.Fatal("validation error leaked a secret")
	}
}

func TestLoadRejectsInvalidTypedValues(t *testing.T) {
	values := validValues()
	values["TELEGRAM_ALLOWED_USERS"] = "123,not-an-id"
	values["REMNA_API_IP"] = "invalid"
	values["DATABASE_URL"] = "https://example.com/database"

	_, err := load(mapLookup(values))
	if err == nil {
		t.Fatal("load() error = nil, want validation error")
	}
	for _, name := range []string{"TELEGRAM_ALLOWED_USERS", "REMNA_API_IP", "DATABASE_URL"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error %q does not mention %s", err, name)
		}
	}
}

func validValues() map[string]string {
	return map[string]string{
		"TELEGRAM_BOT_TOKEN":     "bot-secret",
		"TELEGRAM_ALLOWED_USERS": "123, 456,123",
		"REMNAWAVE_URL":          "https://remnawave.example.com",
		"REMNAWAVE_TOKEN":        "remnawave-secret",
		"DNS_BALANCER_URL":       "https://dns.example.com",
		"DNS_BALANCER_TOKEN":     "dns-secret",
		"CF_API_TOKEN":           "cloudflare-secret",
		"ACME_EMAIL":             "ops@example.com",
		"REMNA_API_IP":           "192.0.2.10",
		"METRICS_IP":             "192.0.2.11",
		"DATABASE_URL":           "postgres://user:pass@localhost:5432/deployer?sslmode=disable",
		"DEPLOY_SSH_PRIVATE_KEY": "/run/secrets/deploy_ssh_private_key",
	}
}

func mapLookup(values map[string]string) lookupFunc {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}
