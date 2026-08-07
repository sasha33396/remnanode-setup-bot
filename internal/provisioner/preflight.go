// Package provisioner contains deterministic VPS preparation primitives.
package provisioner

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	sshclient "remnanode-setup-bot/internal/ssh"
)

const systemInfoCommand = `set -eu
. /etc/os-release
printf 'effective_uid=%s\n' "$(id -u)"
printf 'effective_user=%s\n' "$(id -un)"
printf 'os_id=%s\n' "$ID"
printf 'os_version=%s\n' "$VERSION_ID"
printf 'architecture=%s\n' "$(uname -m)"
printf 'cpu_count=%s\n' "$(getconf _NPROCESSORS_ONLN)"
printf 'available_ram_kib=%s\n' "$(awk '/^MemAvailable:/ {print $2}' /proc/meminfo)"
printf 'free_disk_kib=%s\n' "$(df -Pk / | awk 'NR == 2 {print $4}')"`

const componentStateCommand = `set -eu
if command -v docker >/dev/null 2>&1; then printf 'docker=true\n'; else printf 'docker=false\n'; fi
if [ -d /opt/remnanode ]; then printf 'remnanode_directory=true\n'; else printf 'remnanode_directory=false\n'; fi
if [ -d /opt/xray-sni ]; then printf 'xray_sni_directory=true\n'; else printf 'xray_sni_directory=false\n'; fi`

const containerListCommand = `docker ps -a --format '{{.Names}}'`

const portStateCommand = `set -eu
for item in 22:0016 443:01BB 2222:08AE 9100:238C 9200:23F0 9443:24E3; do
    port="${item%%:*}"
    hex="${item#*:}"
    if awk -v suffix=":$hex" '$4 == "0A" && toupper($2) ~ suffix "$" { found=1 } END { exit !found }' /proc/net/tcp /proc/net/tcp6 2>/dev/null; then
        printf 'port_%s=true\n' "$port"
    else
        printf 'port_%s=false\n' "$port"
    fi
done`

var ErrInvalidPreflightOutput = errors.New("invalid VPS preflight output")

var relevantPorts = []int{22, 443, 2222, 9100, 9200, 9443}

// Requirements controls resource validation without hard-coding deployment
// sizing policy. Empty distro/architecture lists use Ubuntu/Debian and
// amd64/arm64 defaults.
type Requirements struct {
	MinimumCPUCount          int
	MinimumAvailableRAMBytes int64
	MinimumFreeDiskBytes     int64
	AllowedDistributions     []string
	AllowedArchitectures     []string
	CommandTimeout           time.Duration
}

// SystemInfo is typed data collected from the VPS.
type SystemInfo struct {
	EffectiveUID      int
	EffectiveUser     string
	Distribution      string
	Version           string
	Architecture      string
	CPUCount          int
	AvailableRAMBytes int64
	FreeDiskBytes     int64
}

// ComponentState reports pre-existing deployment components.
type ComponentState struct {
	DockerInstalled     bool
	RemnanodeDirectory  bool
	XraySNIDirectory    bool
	RemnanodeContainers []string
	XraySNIContainers   []string
}

// PortState reports whether one relevant TCP port has a listener.
type PortState struct {
	Port     int
	Occupied bool
}

// Issue is a safe operator-facing preflight finding.
type Issue struct {
	Code    string
	Message string
}

// PreflightResult contains no raw command output.
type PreflightResult struct {
	SSHConnected  bool
	System        SystemInfo
	Components    ComponentState
	Ports         []PortState
	Warnings      []Issue
	FatalFailures []Issue
}

// Passed reports whether provisioning may continue.
func (r PreflightResult) Passed() bool { return r.SSHConnected && len(r.FatalFailures) == 0 }

// Preflight gathers and validates VPS state through a bounded SSH runner.
type Preflight struct {
	runner       sshclient.CommandRunner
	requirements Requirements
}

func NewPreflight(runner sshclient.CommandRunner, requirements Requirements) (*Preflight, error) {
	if runner == nil || requirements.MinimumCPUCount < 0 || requirements.MinimumAvailableRAMBytes < 0 || requirements.MinimumFreeDiskBytes < 0 {
		return nil, errors.New("invalid preflight configuration")
	}
	if requirements.CommandTimeout <= 0 {
		requirements.CommandTimeout = 15 * time.Second
	}
	if len(requirements.AllowedDistributions) == 0 {
		requirements.AllowedDistributions = []string{"ubuntu", "debian"}
	}
	if len(requirements.AllowedArchitectures) == 0 {
		requirements.AllowedArchitectures = []string{"amd64", "arm64"}
	}
	return &Preflight{runner: runner, requirements: requirements}, nil
}

// Run performs inspection only; it does not change the VPS.
func (p *Preflight) Run(ctx context.Context) (PreflightResult, error) {
	var result PreflightResult
	systemCommand, err := p.runner.Run(ctx, sshclient.CommandRequest{Command: systemInfoCommand, Timeout: p.requirements.CommandTimeout})
	if err != nil {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		result.FatalFailures = append(result.FatalFailures, Issue{Code: "SSH_CONNECTIVITY_FAILED", Message: "SSH preflight command could not be completed"})
		return result, nil
	}
	result.SSHConnected = true
	result.System, err = parseSystemInfo(systemCommand.Stdout)
	if err != nil {
		result.FatalFailures = append(result.FatalFailures, Issue{Code: "SYSTEM_INFO_INVALID", Message: "VPS system information could not be parsed"})
		return result, nil
	}

	componentCommand, componentErr := p.runner.Run(ctx, sshclient.CommandRequest{Command: componentStateCommand, Timeout: p.requirements.CommandTimeout})
	if componentErr != nil {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		result.Warnings = append(result.Warnings, Issue{Code: "COMPONENT_INSPECTION_FAILED", Message: "Existing component state could not be inspected"})
	} else {
		result.Components, err = parseComponentState(componentCommand.Stdout)
		if err != nil {
			result.Warnings = append(result.Warnings, Issue{Code: "COMPONENT_INFO_INVALID", Message: "Existing component state could not be parsed"})
		}
	}

	if result.Components.DockerInstalled {
		containerCommand, containerErr := p.runner.Run(ctx, sshclient.CommandRequest{Command: containerListCommand, Timeout: p.requirements.CommandTimeout})
		if containerErr != nil {
			if ctx.Err() != nil {
				return result, ctx.Err()
			}
			result.Warnings = append(result.Warnings, Issue{Code: "CONTAINER_INSPECTION_FAILED", Message: "Existing Docker containers could not be inspected"})
		} else {
			result.Components.RemnanodeContainers, result.Components.XraySNIContainers = parseContainers(containerCommand.Stdout)
		}
	}

	portCommand, portErr := p.runner.Run(ctx, sshclient.CommandRequest{Command: portStateCommand, Timeout: p.requirements.CommandTimeout})
	if portErr != nil {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		result.FatalFailures = append(result.FatalFailures, Issue{Code: "PORT_INSPECTION_FAILED", Message: "Relevant TCP ports could not be inspected"})
	} else {
		result.Ports, err = parsePortStates(portCommand.Stdout)
		if err != nil {
			result.FatalFailures = append(result.FatalFailures, Issue{Code: "PORT_INFO_INVALID", Message: "Relevant TCP port state could not be parsed"})
		}
	}

	p.validate(&result)
	return result, nil
}

func (p *Preflight) validate(result *PreflightResult) {
	if result.System.EffectiveUID != 0 {
		result.FatalFailures = append(result.FatalFailures, Issue{Code: "NOT_ROOT", Message: "Effective SSH user is not root"})
	}
	if !containsFold(p.requirements.AllowedDistributions, result.System.Distribution) {
		result.FatalFailures = append(result.FatalFailures, Issue{Code: "UNSUPPORTED_OS", Message: "VPS operating system distribution is unsupported"})
	}
	if strings.TrimSpace(result.System.Version) == "" {
		result.FatalFailures = append(result.FatalFailures, Issue{Code: "OS_VERSION_MISSING", Message: "VPS operating system version is missing"})
	}
	if !containsFold(p.requirements.AllowedArchitectures, normalizeArchitecture(result.System.Architecture)) {
		result.FatalFailures = append(result.FatalFailures, Issue{Code: "UNSUPPORTED_ARCHITECTURE", Message: "VPS architecture is unsupported"})
	}
	if result.System.CPUCount < max(1, p.requirements.MinimumCPUCount) {
		result.FatalFailures = append(result.FatalFailures, Issue{Code: "INSUFFICIENT_CPU", Message: "VPS CPU count is below the required minimum"})
	}
	if result.System.AvailableRAMBytes <= 0 || result.System.AvailableRAMBytes < p.requirements.MinimumAvailableRAMBytes {
		result.FatalFailures = append(result.FatalFailures, Issue{Code: "INSUFFICIENT_RAM", Message: "VPS available RAM is below the required minimum"})
	}
	if result.System.FreeDiskBytes <= 0 || result.System.FreeDiskBytes < p.requirements.MinimumFreeDiskBytes {
		result.FatalFailures = append(result.FatalFailures, Issue{Code: "INSUFFICIENT_DISK", Message: "VPS free disk space is below the required minimum"})
	}
	if !result.Components.DockerInstalled {
		result.Warnings = append(result.Warnings, Issue{Code: "DOCKER_NOT_INSTALLED", Message: "Docker is not installed and will be required by provisioning"})
	}
	if result.Components.RemnanodeDirectory || len(result.Components.RemnanodeContainers) > 0 {
		result.Warnings = append(result.Warnings, Issue{Code: "REMNANODE_EXISTS", Message: "Existing Remnanode files or containers were detected"})
	}
	if result.Components.XraySNIDirectory || len(result.Components.XraySNIContainers) > 0 {
		result.Warnings = append(result.Warnings, Issue{Code: "XRAY_SNI_EXISTS", Message: "Existing xray-sni files or containers were detected"})
	}
	for _, port := range result.Ports {
		if port.Port == 22 && !port.Occupied {
			result.Warnings = append(result.Warnings, Issue{Code: "SSH_PORT_NOT_DETECTED", Message: "Port 22 was not detected as listening despite the SSH connection"})
		}
		if port.Port != 22 && port.Occupied {
			result.FatalFailures = append(result.FatalFailures, Issue{Code: fmt.Sprintf("PORT_%d_OCCUPIED", port.Port), Message: fmt.Sprintf("Required TCP port %d is already occupied", port.Port)})
		}
	}
}

func parseSystemInfo(output string) (SystemInfo, error) {
	values, err := parseKeyValues(output)
	if err != nil {
		return SystemInfo{}, err
	}
	required := []string{"effective_uid", "effective_user", "os_id", "os_version", "architecture", "cpu_count", "available_ram_kib", "free_disk_kib"}
	for _, key := range required {
		if strings.TrimSpace(values[key]) == "" {
			return SystemInfo{}, ErrInvalidPreflightOutput
		}
	}
	uid, err := strconv.Atoi(values["effective_uid"])
	if err != nil {
		return SystemInfo{}, ErrInvalidPreflightOutput
	}
	cpus, err := strconv.Atoi(values["cpu_count"])
	if err != nil {
		return SystemInfo{}, ErrInvalidPreflightOutput
	}
	ramKiB, err := strconv.ParseInt(values["available_ram_kib"], 10, 64)
	if err != nil {
		return SystemInfo{}, ErrInvalidPreflightOutput
	}
	diskKiB, err := strconv.ParseInt(values["free_disk_kib"], 10, 64)
	if err != nil {
		return SystemInfo{}, ErrInvalidPreflightOutput
	}
	return SystemInfo{
		EffectiveUID:      uid,
		EffectiveUser:     values["effective_user"],
		Distribution:      strings.ToLower(values["os_id"]),
		Version:           values["os_version"],
		Architecture:      normalizeArchitecture(values["architecture"]),
		CPUCount:          cpus,
		AvailableRAMBytes: ramKiB * 1024,
		FreeDiskBytes:     diskKiB * 1024,
	}, nil
}

func parseComponentState(output string) (ComponentState, error) {
	values, err := parseKeyValues(output)
	if err != nil {
		return ComponentState{}, err
	}
	docker, err := strconv.ParseBool(values["docker"])
	if err != nil {
		return ComponentState{}, ErrInvalidPreflightOutput
	}
	remnanode, err := strconv.ParseBool(values["remnanode_directory"])
	if err != nil {
		return ComponentState{}, ErrInvalidPreflightOutput
	}
	xraySNI, err := strconv.ParseBool(values["xray_sni_directory"])
	if err != nil {
		return ComponentState{}, ErrInvalidPreflightOutput
	}
	return ComponentState{DockerInstalled: docker, RemnanodeDirectory: remnanode, XraySNIDirectory: xraySNI}, nil
}

func parseContainers(output string) ([]string, []string) {
	var remnanode []string
	var xraySNI []string
	for _, line := range strings.Split(output, "\n") {
		name := strings.TrimSpace(line)
		lower := strings.ToLower(name)
		if name == "" {
			continue
		}
		if strings.Contains(lower, "remnanode") {
			remnanode = append(remnanode, name)
		}
		if strings.Contains(lower, "xray-sni") || strings.Contains(lower, "xray_sni") {
			xraySNI = append(xraySNI, name)
		}
	}
	return remnanode, xraySNI
}

func parsePortStates(output string) ([]PortState, error) {
	values, err := parseKeyValues(output)
	if err != nil {
		return nil, err
	}
	ports := make([]PortState, 0, len(relevantPorts))
	for _, port := range relevantPorts {
		occupied, err := strconv.ParseBool(values[fmt.Sprintf("port_%d", port)])
		if err != nil {
			return nil, ErrInvalidPreflightOutput
		}
		ports = append(ports, PortState{Port: port, Occupied: occupied})
	}
	return ports, nil
}

func parseKeyValues(output string) (map[string]string, error) {
	values := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		key, value, found := strings.Cut(strings.TrimSpace(line), "=")
		if !found || key == "" {
			return nil, ErrInvalidPreflightOutput
		}
		if _, duplicate := values[key]; duplicate {
			return nil, ErrInvalidPreflightOutput
		}
		values[key] = strings.TrimSpace(value)
	}
	return values, nil
}

func normalizeArchitecture(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "x86_64", "amd64":
		return "amd64"
	case "aarch64", "arm64":
		return "arm64"
	default:
		return strings.ToLower(strings.TrimSpace(value))
	}
}

func containsFold(values []string, target string) bool {
	for _, value := range values {
		if strings.EqualFold(value, target) {
			return true
		}
	}
	return false
}
