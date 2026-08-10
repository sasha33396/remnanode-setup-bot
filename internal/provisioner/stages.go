package provisioner

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	sshclient "remnanode-setup-bot/internal/ssh"
)

// NewDefaultStages builds the complete, stable stage list. The xray adapter is
// mandatory because certificate acquisition and layout are owned elsewhere.
func NewDefaultStages(runner sshclient.CommandRunner, config Config, xray XraySNIInstaller) ([]Stage, error) {
	if runner == nil || xray == nil {
		return nil, errors.New("SSH runner and xray-sni adapter are required")
	}
	config, err := config.normalized()
	if err != nil {
		return nil, err
	}
	if !regexp.MustCompile(`^[A-Za-z0-9_+/-]+$`).MatchString(config.Timezone) ||
		!regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`).MatchString(config.NodeExporterVersion) {
		return nil, errors.New("invalid provisioner version or timezone")
	}

	remote := func(name, inspect, apply, validate string, files ...managedFile) Stage {
		return &remoteStage{name: name, runner: runner, timeout: config.CommandTimeout, files: files, inspectCommand: inspect, applyCommand: apply, validateCommand: validate}
	}

	sysctlConfig := `net.core.somaxconn = 65535
net.core.netdev_max_backlog = 65535
net.core.rmem_max = 16777216
net.core.wmem_max = 16777216
net.ipv4.tcp_rmem = 4096 87380 16777216
net.ipv4.tcp_wmem = 4096 65536 16777216
net.ipv4.tcp_max_syn_backlog = 65535
net.ipv4.ip_local_port_range = 1024 65535
net.ipv4.tcp_fin_timeout = 15
net.ipv4.tcp_tw_reuse = 1
net.netfilter.nf_conntrack_max = 262144
net.core.default_qdisc = fq
net.ipv4.tcp_congestion_control = bbr
`
	limitsConfig := `* soft nofile 300000
* hard nofile 300000
root soft nofile 300000
root hard nofile 300000
`
	systemdLimits := "[Manager]\nDefaultLimitNOFILE=300000\n"
	fail2banConfig := `[sshd]
enabled = true
backend = systemd
`
	logrotateConfig := `/var/log/remnanode/*.log {
    size 50M
    rotate 5
    compress
    missingok
    notifempty
    copytruncate
}
`

	remnanodeCompose, err := buildRemnanodeCompose(config.remnanodeSecret)
	if err != nil {
		return nil, err
	}
	speedtestCompose := `services:
  speedtest-exporter:
    image: kutovoys/speedtest-exporter:latest
    container_name: speedtest-exporter
    restart: always
    environment:
      SERVER_IDS: "32983"
      UPDATE_INTERVAL: "60"
      METRICS_PROTECTED: "false"
    ports:
      - "9200:9090"
`
	nodeExporterService := `[Unit]
Description=Prometheus Node Exporter
After=network-online.target
Wants=network-online.target

[Service]
User=node_exporter
Group=node_exporter
Type=simple
ExecStart=/usr/local/bin/node_exporter
Restart=on-failure

[Install]
WantedBy=multi-user.target
`

	systemInspect := fmt.Sprintf(`ready=true
	for package in mc htop btop iftop curl wget ca-certificates git; do
    dpkg-query -W -f='${Status}' "$package" 2>/dev/null | grep -qx 'install ok installed' || ready=false
done
if [ "$(timedatectl show -p Timezone --value)" != %q ]; then ready=false; fi
if [ "$ready" = true ]; then printf ready; else printf pending; fi`, config.Timezone)
	systemApply := fmt.Sprintf(`set -eu
export DEBIAN_FRONTEND=noninteractive
apt-get update
apt-get upgrade -y
apt-get install -y mc htop btop iftop curl wget ca-certificates git
timedatectl set-timezone %s`, config.Timezone)

	dockerInspect := `if command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1 && systemctl is-active --quiet docker; then printf ready; else printf pending; fi`
	dockerApply := `set -eu
export DEBIAN_FRONTEND=noninteractive
. /etc/os-release
case "$ID" in ubuntu|debian) ;; *) exit 2 ;; esac
apt-get update
apt-get install -y ca-certificates curl gpg
install -m 0755 -d /etc/apt/keyrings
curl --fail --silent --show-error --location "https://download.docker.com/linux/$ID/gpg" --output /etc/apt/keyrings/docker.asc
chmod a+r /etc/apt/keyrings/docker.asc
arch=$(dpkg --print-architecture)
printf 'deb [arch=%s signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/%s %s stable\n' "$arch" "$ID" "$VERSION_CODENAME" > /etc/apt/sources.list.d/docker.list
apt-get update
apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
systemctl enable --now docker`

	firewallInspect, firewallApply, firewallValidate := firewallCommands(config.RemnawaveAPIIP.String(), config.MetricsIP.String())
	nodeInspect := fmt.Sprintf(`if command -v node_exporter >/dev/null 2>&1 && node_exporter --version 2>&1 | grep -Fq 'version %s' && systemctl is-active --quiet node_exporter; then printf ready; else printf pending; fi`, config.NodeExporterVersion)
	remnanodeValidate := `attempt=0
while [ "$attempt" -lt 30 ]; do
    if docker inspect -f '{{.State.Running}}' remnanode 2>/dev/null | grep -qx true && ss -lnt | awk '{print $4}' | grep -Eq '(^|:)2222$'; then
        printf ready
        exit 0
    fi
    attempt=$((attempt + 1))
    sleep 1
done
printf pending`
	nodeApply := fmt.Sprintf(`set -eu
arch=$(uname -m)
case "$arch" in x86_64) arch=amd64 ;; aarch64) arch=arm64 ;; *) exit 2 ;; esac
version=%s
filename="node_exporter-${version}.linux-${arch}.tar.gz"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
curl --fail --silent --show-error --location "https://github.com/prometheus/node_exporter/releases/download/v${version}/${filename}" --output "$work/$filename"
curl --fail --silent --show-error --location "https://github.com/prometheus/node_exporter/releases/download/v${version}/sha256sums.txt" --output "$work/sha256sums.txt"
(cd "$work" && grep "  ${filename}$" sha256sums.txt | sha256sum --check --status)
tar -xzf "$work/$filename" -C "$work"
install -m 0755 "$work/node_exporter-${version}.linux-${arch}/node_exporter" /usr/local/bin/node_exporter
id node_exporter >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin node_exporter
systemctl daemon-reload
systemctl enable --now node_exporter`, config.NodeExporterVersion)

	stages := []Stage{
		remote("system", systemInspect, systemApply, systemInspect),
		remote("docker", dockerInspect, dockerApply, dockerInspect),
		remote("sysctl", "", `set -eu
modprobe nf_conntrack
sysctl --system >/dev/null`, `if [ "$(sysctl -n net.ipv4.tcp_congestion_control)" = bbr ] && [ "$(sysctl -n net.netfilter.nf_conntrack_max)" = 262144 ]; then printf ready; else printf pending; fi`,
			managedFile{path: "/etc/modules-load.d/remnanode.conf", mode: "0644", content: []byte("nf_conntrack\n")},
			managedFile{path: "/etc/sysctl.d/99-remnanode.conf", mode: "0644", content: []byte(sysctlConfig)}),
		remote("limits", "", "systemctl daemon-reexec", `if systemctl show --property DefaultLimitNOFILE | grep -q '=300000$'; then printf ready; else printf pending; fi`,
			managedFile{path: "/etc/security/limits.d/99-remnanode.conf", mode: "0644", content: []byte(limitsConfig)},
			managedFile{path: "/etc/systemd/system.conf.d/99-remnanode.conf", mode: "0644", content: []byte(systemdLimits)}),
		remote("firewall", firewallInspect, firewallApply, firewallValidate),
		remote("fail2ban", `if command -v fail2ban-client >/dev/null 2>&1 && systemctl is-active --quiet fail2ban; then printf ready; else printf pending; fi`, `set -eu
export DEBIAN_FRONTEND=noninteractive
apt-get update
apt-get install -y fail2ban
systemctl enable --now fail2ban`, `if systemctl is-active --quiet fail2ban && fail2ban-client status sshd >/dev/null 2>&1; then printf ready; else printf pending; fi`,
			managedFile{path: "/etc/fail2ban/jail.d/remnanode-sshd.local", mode: "0644", content: []byte(fail2banConfig)}),
		remote("remnanode", `if docker inspect -f '{{.State.Running}}' remnanode 2>/dev/null | grep -qx true; then printf ready; else printf pending; fi`, `set -eu
install -d -m 0755 /var/log/remnanode
docker compose -f /opt/remnanode/docker-compose.yml up -d --remove-orphans`, remnanodeValidate,
			managedFile{path: "/opt/remnanode/docker-compose.yml", mode: "0600", content: remnanodeCompose}),
		remote("node_exporter", nodeInspect, nodeApply, `if systemctl is-active --quiet node_exporter && ss -lnt | awk '{print $4}' | grep -Eq '(^|:)9100$'; then printf ready; else printf pending; fi`,
			managedFile{path: "/etc/systemd/system/node_exporter.service", mode: "0644", content: []byte(nodeExporterService)}),
		remote("speedtest_exporter", `if docker inspect -f '{{.State.Running}}' speedtest-exporter 2>/dev/null | grep -qx true; then printf ready; else printf pending; fi`, `docker compose -f /opt/speedtest-exporter/docker-compose.yml up -d --remove-orphans`, `if docker inspect -f '{{.State.Running}}' speedtest-exporter 2>/dev/null | grep -qx true && ss -lnt | awk '{print $4}' | grep -Eq '(^|:)9200$'; then printf ready; else printf pending; fi`,
			managedFile{path: "/opt/speedtest-exporter/docker-compose.yml", mode: "0644", content: []byte(speedtestCompose)}),
		remote("logrotate", `if command -v logrotate >/dev/null 2>&1; then printf ready; else printf pending; fi`, `set -eu
export DEBIAN_FRONTEND=noninteractive
apt-get update
apt-get install -y logrotate`, `if logrotate --debug /etc/logrotate.d/remnanode >/dev/null 2>&1; then printf ready; else printf pending; fi`,
			managedFile{path: "/etc/logrotate.d/remnanode", mode: "0644", content: []byte(logrotateConfig)}),
		xraySNIStage{installer: xray},
		remote("healthcheck", healthcheckCommand, "", healthcheckCommand),
	}
	return stages, nil
}

func buildRemnanodeCompose(secret []byte) ([]byte, error) {
	encoded, err := json.Marshal(string(secret))
	if err != nil {
		return nil, errors.New("encode Remnanode configuration")
	}
	content := fmt.Sprintf(`services:
  remnanode:
    image: remnawave/node:latest
    container_name: remnanode
    hostname: remnanode
    network_mode: host
    restart: always
    cap_add:
      - NET_ADMIN
    ulimits:
      nofile:
        soft: 1048576
        hard: 1048576
    environment:
      NODE_PORT: "2222"
      SECRET_KEY: %s
    volumes:
      - /var/log/remnanode:/var/log/remnanode
`, encoded)
	return []byte(content), nil
}

func firewallCommands(panelIP, metricsIP string) (string, string, string) {
	rules := []struct{ marker, expected, command string }{
		{"deployer-ssh", "22/tcp", "ufw allow 22/tcp comment 'deployer-ssh'"},
		{"deployer-https", "443/tcp", "ufw allow 443/tcp comment 'deployer-https'"},
		{"deployer-remnanode", panelIP, fmt.Sprintf("ufw allow from %s to any port 2222 proto tcp comment 'deployer-remnanode'", panelIP)},
		{"deployer-node-metrics", metricsIP, fmt.Sprintf("ufw allow from %s to any port 9100 proto tcp comment 'deployer-node-metrics'", metricsIP)},
		{"deployer-speedtest-metrics", metricsIP, fmt.Sprintf("ufw allow from %s to any port 9200 proto tcp comment 'deployer-speedtest-metrics'", metricsIP)},
		{"deployer-smtp-block", "25/tcp", "ufw deny out 25/tcp comment 'deployer-smtp-block'"},
	}
	blocked := []string{"178.162.203.0/24", "45.159.79.0/24", "85.17.155.0/24", "185.221.222.0/24", "89.150.57.0/24", "46.165.199.0/24", "178.162.202.0/24", "85.17.70.0/24", "64.62.203.0/24"}
	for index, cidr := range blocked {
		markerIn := fmt.Sprintf("deployer-block-%d-in", index+1)
		markerOut := fmt.Sprintf("deployer-block-%d-out", index+1)
		rules = append(rules,
			struct{ marker, expected, command string }{markerIn, cidr, fmt.Sprintf("ufw deny from %s comment '%s'", cidr, markerIn)},
			struct{ marker, expected, command string }{markerOut, cidr, fmt.Sprintf("ufw deny out to %s comment '%s'", cidr, markerOut)},
		)
	}
	var checks, apply []string
	for _, rule := range rules {
		checks = append(checks, fmt.Sprintf("printf '%%s\\n' \"$status\" | grep -F %q | grep -Fq %q", rule.marker, rule.expected))
		apply = append(apply, fmt.Sprintf(`if ! printf '%%s\n' "$status" | grep -F %q | grep -Fq %q; then
    while number=$(printf '%%s\n' "$status" | awk -v marker=%q 'index($0, marker) {gsub(/[][]/, "", $1); print $1; exit}') && [ -n "$number" ]; do
        ufw --force delete "$number"
        status=$(ufw status numbered)
    done
    %s
    status=$(ufw status numbered)
fi`, rule.marker, rule.expected, rule.marker, rule.command))
	}
	check := "status=$(ufw status numbered 2>/dev/null || true); if " + strings.Join(checks, " && ") + ` && printf '%s\n' "$status" | grep -q '^Status: active'; then printf ready; else printf pending; fi`
	applyCommand := `set -eu
export DEBIAN_FRONTEND=noninteractive
if ! command -v ufw >/dev/null 2>&1; then apt-get update; apt-get install -y ufw; fi
if ! ufw show added | grep -Fq 'deployer-ssh'; then ufw allow 22/tcp comment 'deployer-ssh'; fi
ufw --force enable
status=$(ufw status numbered)
` + strings.Join(apply, "\n") + `
true`
	return check, applyCommand, check
}

const healthcheckCommand = `if systemctl is-active --quiet docker &&
docker inspect -f '{{.State.Running}}' remnanode 2>/dev/null | grep -qx true &&
systemctl is-active --quiet node_exporter &&
docker inspect -f '{{.State.Running}}' speedtest-exporter 2>/dev/null | grep -qx true &&
ss -lnt | awk '{print $4}' | grep -Eq '(^|:)2222$' &&
ss -lnt | awk '{print $4}' | grep -Eq '(^|:)9100$' &&
ss -lnt | awk '{print $4}' | grep -Eq '(^|:)9200$'; then printf ready; else printf pending; fi`
