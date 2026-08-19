package provisioner

import (
	"context"
	"strings"
	"testing"
	"time"

	sshclient "remnanode-setup-bot/internal/ssh"
)

type legacySwitchRunner struct {
	request sshclient.CommandRequest
	err     error
}

func (r *legacySwitchRunner) Run(_ context.Context, request sshclient.CommandRequest) (sshclient.CommandResult, error) {
	r.request = request
	return sshclient.CommandResult{}, r.err
}

func TestSwitchLegacyXraySNIUsesAtomicEnvironmentUpdateAndRollback(t *testing.T) {
	runner := &legacySwitchRunner{}
	if err := SwitchLegacyXraySNI(context.Background(), runner, "old.example.com", "new.example.com", time.Minute); err != nil {
		t.Fatalf("SwitchLegacyXraySNI() error = %v", err)
	}
	for _, required := range []string{
		"cd /root/xray-sni",
		"cp -p .env \"$backup\"",
		"awk -v domain=\"$new_domain\"",
		"docker compose up -d --remove-orphans",
		"mv -f \"$backup\" .env",
		"test \"$new_code\" = 200",
	} {
		if !strings.Contains(runner.request.Command, required) {
			t.Errorf("switch command is missing %q", required)
		}
	}
	if strings.Contains(runner.request.Command, "CF_API_TOKEN=") {
		t.Fatal("switch command must not read or embed the Cloudflare token")
	}
}

func TestSwitchLegacyXraySNIRejectsInvalidDomain(t *testing.T) {
	err := SwitchLegacyXraySNI(context.Background(), &legacySwitchRunner{}, "old.example.com", "bad domain", time.Minute)
	if err != ErrInvalidXraySNIConfiguration {
		t.Fatalf("SwitchLegacyXraySNI() error = %v", err)
	}
}
