package provisioner

import (
	"context"
	"errors"
	"testing"
	"time"

	sshclient "remnanode-setup-bot/internal/ssh"
)

const validSystemOutput = `effective_uid=0
effective_user=root
os_id=ubuntu
os_version=24.04
architecture=x86_64
cpu_count=4
available_ram_kib=4194304
free_disk_kib=52428800`

const freePortsOutput = `port_22=true
port_443=false
port_2222=false
port_9100=false
port_9200=false
port_9443=false`

func TestPreflightReturnsTypedSuccessfulResult(t *testing.T) {
	runner := &fakeRunner{responses: map[string]fakeResponse{
		systemInfoCommand:     {stdout: validSystemOutput},
		componentStateCommand: {stdout: "docker=false\nremnanode_directory=false\nxray_sni_directory=false"},
		portStateCommand:      {stdout: freePortsOutput},
	}}
	preflight, err := NewPreflight(runner, Requirements{
		MinimumCPUCount:          2,
		MinimumAvailableRAMBytes: 1024 * 1024 * 1024,
		MinimumFreeDiskBytes:     10 * 1024 * 1024 * 1024,
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := preflight.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !result.Passed() || !result.SSHConnected {
		t.Fatalf("preflight did not pass: %#v", result)
	}
	if result.System.Distribution != "ubuntu" || result.System.Architecture != "amd64" || result.System.CPUCount != 4 {
		t.Fatalf("system info = %#v", result.System)
	}
	if !hasIssue(result.Warnings, "DOCKER_NOT_INSTALLED") {
		t.Fatalf("warnings = %#v, want Docker warning", result.Warnings)
	}
	if len(result.Ports) != 6 {
		t.Fatalf("port count = %d, want 6", len(result.Ports))
	}
}

func TestPreflightSeparatesWarningsAndFatalFailures(t *testing.T) {
	ports := `port_22=true
port_443=true
port_2222=false
port_9100=false
port_9200=false
port_9443=false`
	runner := &fakeRunner{responses: map[string]fakeResponse{
		systemInfoCommand:     {stdout: validSystemOutput},
		componentStateCommand: {stdout: "docker=true\nremnanode_directory=true\nxray_sni_directory=true"},
		containerListCommand:  {stdout: "remnanode\nxray-sni\nunrelated"},
		portStateCommand:      {stdout: ports},
	}}
	preflight, _ := NewPreflight(runner, Requirements{})
	result, err := preflight.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Passed() || !hasIssue(result.FatalFailures, "PORT_443_OCCUPIED") {
		t.Fatalf("fatal failures = %#v", result.FatalFailures)
	}
	if !hasIssue(result.Warnings, "REMNANODE_EXISTS") || !hasIssue(result.Warnings, "XRAY_SNI_EXISTS") {
		t.Fatalf("warnings = %#v", result.Warnings)
	}
}

func TestPreflightValidatesRootOSArchitectureAndResources(t *testing.T) {
	output := `effective_uid=1000
effective_user=ubuntu
os_id=alpine
os_version=3.20
architecture=riscv64
cpu_count=1
available_ram_kib=128
free_disk_kib=256`
	runner := &fakeRunner{responses: map[string]fakeResponse{
		systemInfoCommand:     {stdout: output},
		componentStateCommand: {stdout: "docker=false\nremnanode_directory=false\nxray_sni_directory=false"},
		portStateCommand:      {stdout: freePortsOutput},
	}}
	preflight, _ := NewPreflight(runner, Requirements{MinimumCPUCount: 2, MinimumAvailableRAMBytes: 1 << 30, MinimumFreeDiskBytes: 10 << 30})
	result, err := preflight.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, code := range []string{"NOT_ROOT", "UNSUPPORTED_OS", "UNSUPPORTED_ARCHITECTURE", "INSUFFICIENT_CPU", "INSUFFICIENT_RAM", "INSUFFICIENT_DISK"} {
		if !hasIssue(result.FatalFailures, code) {
			t.Errorf("fatal failures = %#v, missing %s", result.FatalFailures, code)
		}
	}
}

func TestPreflightParsingRejectsRawInvalidOutput(t *testing.T) {
	if _, err := parseSystemInfo("root password=secret"); !errors.Is(err, ErrInvalidPreflightOutput) {
		t.Fatalf("parseSystemInfo() error = %v", err)
	}
	if _, err := parsePortStates("port_22=maybe"); !errors.Is(err, ErrInvalidPreflightOutput) {
		t.Fatalf("parsePortStates() error = %v", err)
	}
}

type fakeResponse struct {
	stdout string
	stderr string
	exit   int
	err    error
}

type fakeRunner struct {
	responses map[string]fakeResponse
}

func (f *fakeRunner) Run(ctx context.Context, request sshclient.CommandRequest) (sshclient.CommandResult, error) {
	if err := ctx.Err(); err != nil {
		return sshclient.CommandResult{ExitStatus: -1}, err
	}
	if request.Timeout <= 0 {
		return sshclient.CommandResult{ExitStatus: -1}, errors.New("missing timeout")
	}
	response, found := f.responses[request.Command]
	if !found {
		return sshclient.CommandResult{ExitStatus: -1}, errors.New("unexpected command")
	}
	return sshclient.CommandResult{
		Stdout:     response.stdout,
		Stderr:     response.stderr,
		ExitStatus: response.exit,
		Duration:   time.Millisecond,
	}, response.err
}

func hasIssue(issues []Issue, code string) bool {
	for _, issue := range issues {
		if issue.Code == code {
			return true
		}
	}
	return false
}
