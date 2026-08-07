package config

import (
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
