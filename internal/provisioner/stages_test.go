package provisioner

import (
	"context"
	"net/netip"
	"strings"
	"testing"

	sshclient "remnanode-setup-bot/internal/ssh"
)

type successfulXrayAdapter struct{}

func (successfulXrayAdapter) Inspect(context.Context) (Inspection, error) {
	return Inspection{Satisfied: true}, nil
}
func (successfulXrayAdapter) Install(context.Context) error  { return nil }
func (successfulXrayAdapter) Validate(context.Context) error { return nil }

type captureRunner struct{ requests []sshclient.CommandRequest }

func (r *captureRunner) Run(_ context.Context, request sshclient.CommandRequest) (sshclient.CommandResult, error) {
	r.requests = append(r.requests, request)
	return sshclient.CommandResult{}, nil
}

func TestRemnanodeComposeUsesRealityHostNetworkWithoutCertificates(t *testing.T) {
	secret := []byte("very-secret-value")
	compose, err := buildRemnanodeCompose(secret)
	if err != nil {
		t.Fatal(err)
	}
	text := string(compose)
	for _, required := range []string{"network_mode: host", `NODE_PORT: "2222"`, "SECRET_KEY:", "/var/log/remnanode:/var/log/remnanode"} {
		if !strings.Contains(text, required) {
			t.Fatalf("compose does not contain %q", required)
		}
	}
	for _, forbidden := range []string{"certificate", "fullchain", "privkey", "443:443"} {
		if strings.Contains(strings.ToLower(text), forbidden) {
			t.Fatalf("compose unexpectedly contains %q", forbidden)
		}
	}
}

func TestManagedSecretIsSentOnlyThroughStdin(t *testing.T) {
	runner := &captureRunner{}
	stage := &remoteStage{name: "test", runner: runner, files: []managedFile{{path: "/opt/remnanode/docker-compose.yml", mode: "0600", content: []byte("SECRET_KEY=top-secret")}}}
	if err := stage.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(runner.requests) != 1 {
		t.Fatalf("request count = %d", len(runner.requests))
	}
	if strings.Contains(runner.requests[0].Command, "top-secret") {
		t.Fatal("secret leaked into SSH command")
	}
	if !strings.Contains(string(runner.requests[0].Stdin), "top-secret") {
		t.Fatal("secret was not supplied via stdin")
	}
}

func TestDefaultStagesHaveStableOrder(t *testing.T) {
	config, err := NewConfig(netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.2"), []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	stages, err := NewDefaultStages(&captureRunner{}, config, successfulXrayAdapter{})
	if err != nil {
		t.Fatal(err)
	}
	for index, stage := range stages {
		if stage.Name() != StageNames[index] {
			t.Fatalf("stage %d = %q, want %q", index, stage.Name(), StageNames[index])
		}
	}
	for _, stage := range stages {
		remote, ok := stage.(*remoteStage)
		if !ok {
			continue
		}
		commands := remote.inspectCommand + remote.applyCommand + remote.validateCommand
		for _, forbidden := range []string{"eval ", "acme", "letsencrypt", "CF_API_TOKEN", "StrictHostKeyChecking=no"} {
			if strings.Contains(strings.ToLower(commands), strings.ToLower(forbidden)) {
				t.Fatalf("stage %s contains forbidden behavior %q", stage.Name(), forbidden)
			}
		}
	}
}

func TestRemnanodeValidationWaitsForAPIReadiness(t *testing.T) {
	config, err := NewConfig(netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.2"), []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	stages, err := NewDefaultStages(&captureRunner{}, config, successfulXrayAdapter{})
	if err != nil {
		t.Fatal(err)
	}
	stage, ok := stages[6].(*remoteStage)
	if !ok || stage.Name() != "remnanode" {
		t.Fatalf("stage 6 = %T %q, want remnanode remote stage", stages[6], stages[6].Name())
	}
	for _, required := range []string{`while [ "$attempt" -lt 30 ]`, "sleep 1", "2222"} {
		if !strings.Contains(stage.validateCommand, required) {
			t.Fatalf("remnanode validation does not contain %q", required)
		}
	}
}

func TestFirewallUsesConfiguredSourcesAndReplacesStaleManagedRules(t *testing.T) {
	inspect, apply, _ := firewallCommands("192.0.2.10", "198.51.100.20")
	for _, required := range []string{"192.0.2.10", "198.51.100.20", "2222", "9100", "9200"} {
		if !strings.Contains(inspect+apply, required) {
			t.Fatalf("firewall commands do not contain %q", required)
		}
	}
	if !strings.Contains(apply, `ufw --force delete "$number"`) {
		t.Fatal("firewall apply cannot remove a stale deployer-managed rule")
	}
}
