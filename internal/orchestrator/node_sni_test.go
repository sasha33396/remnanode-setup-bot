package orchestrator

import (
	"context"
	"strings"
	"testing"
	"time"

	sshclient "remnanode-setup-bot/internal/ssh"
)

type installDetectorRunner struct {
	stdout  string
	command string
}

func (r *installDetectorRunner) Run(_ context.Context, request sshclient.CommandRequest) (sshclient.CommandResult, error) {
	r.command = request.Command
	return sshclient.CommandResult{Stdout: r.stdout}, nil
}

func TestDetectXraySNIInstallTypeSupportsLegacyDirectory(t *testing.T) {
	runner := &installDetectorRunner{stdout: "legacy"}
	installType, err := detectXraySNIInstallType(context.Background(), runner, time.Second)
	if err != nil || installType != "legacy" {
		t.Fatalf("detectXraySNIInstallType() = %q, %v", installType, err)
	}
	for _, path := range []string{"/opt/xray-sni/docker-compose.yml", "/root/xray-sni/docker-compose.yml"} {
		if !strings.Contains(runner.command, path) {
			t.Fatalf("detection command does not check %s", path)
		}
	}
}
