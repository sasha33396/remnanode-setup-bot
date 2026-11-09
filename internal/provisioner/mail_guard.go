package provisioner

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

var DefaultMailGuardPorts = []uint16{25, 26, 110, 143, 465, 587, 993, 995, 2525, 4190, 8025}

type MailGuardConfig struct {
	Enabled bool
	Ports   []uint16
}

func DefaultMailGuardConfig() MailGuardConfig {
	return MailGuardConfig{Enabled: true, Ports: append([]uint16(nil), DefaultMailGuardPorts...)}
}

func (c MailGuardConfig) normalized() (MailGuardConfig, error) {
	if !c.Enabled {
		return MailGuardConfig{}, nil
	}
	if len(c.Ports) == 0 {
		c.Ports = append([]uint16(nil), DefaultMailGuardPorts...)
	}
	seen := make(map[uint16]struct{}, len(c.Ports))
	ports := make([]uint16, 0, len(c.Ports))
	for _, port := range c.Ports {
		if port == 0 {
			return MailGuardConfig{}, errors.New("mail guard ports must be between 1 and 65535")
		}
		if _, ok := seen[port]; ok {
			continue
		}
		seen[port] = struct{}{}
		ports = append(ports, port)
	}
	sort.Slice(ports, func(i, j int) bool { return ports[i] < ports[j] })
	c.Ports = ports
	return c, nil
}

func (c MailGuardConfig) nftSet() string {
	values := make([]string, 0, len(c.Ports))
	for _, port := range c.Ports {
		values = append(values, strconv.FormatUint(uint64(port), 10))
	}
	if len(values) == 1 {
		return values[0]
	}
	return "{ " + strings.Join(values, ", ") + " }"
}

func mailGuardCommands(config MailGuardConfig) (check, apply string, err error) {
	config, err = config.normalized()
	if err != nil {
		return "", "", err
	}
	if !config.Enabled {
		check = `! systemctl is-active --quiet smtp-guard.service 2>/dev/null &&
! nft list table inet smtp_guard >/dev/null 2>&1`
		apply = `
if systemctl list-unit-files smtp-guard.service >/dev/null 2>&1; then
    systemctl disable --now smtp-guard.service >/dev/null 2>&1 || true
fi
if nft list table inet smtp_guard >/dev/null 2>&1; then
    nft delete table inet smtp_guard
fi
rm -f /etc/nftables.d/smtp-guard.nft /usr/local/sbin/apply-smtp-guard /etc/systemd/system/smtp-guard.service
systemctl daemon-reload`
		return check, apply, nil
	}
	portSet := config.nftSet()
	ruleChecks := make([]string, 0, 6)
	for _, chain := range []string{"input", "output", "forward"} {
		for _, protocol := range []string{"tcp", "udp"} {
			ruleChecks = append(ruleChecks, fmt.Sprintf("nft list chain inet smtp_guard %s 2>/dev/null | grep -Fq %q", chain, protocol+" dport "+portSet+" counter packets"))
		}
	}
	check = `systemctl is-enabled --quiet smtp-guard.service 2>/dev/null &&
systemctl is-active --quiet smtp-guard.service 2>/dev/null &&
nft list chain inet smtp_guard input 2>/dev/null | grep -Eq 'hook input priority (-20|filter[[:space:]]*-[[:space:]]*20)' &&
nft list chain inet smtp_guard output 2>/dev/null | grep -Eq 'hook output priority (-20|filter[[:space:]]*-[[:space:]]*20)' &&
nft list chain inet smtp_guard forward 2>/dev/null | grep -Eq 'hook forward priority (-20|filter[[:space:]]*-[[:space:]]*20)' &&
` + strings.Join(ruleChecks, " &&\n")
	apply = fmt.Sprintf(`
# MAIL GUARD: independent IPv4/IPv6 TCP/UDP protection for host and forwarded traffic.
install -d -m 0755 /etc/nftables.d /usr/local/sbin
cat > /etc/nftables.d/smtp-guard.nft <<'NFT'
table inet smtp_guard {
    chain input {
        type filter hook input priority -20; policy accept;
        tcp dport %s counter drop
        udp dport %s counter drop
    }
    chain output {
        type filter hook output priority -20; policy accept;
        tcp dport %s counter drop
        udp dport %s counter drop
    }
    chain forward {
        type filter hook forward priority -20; policy accept;
        tcp dport %s counter drop
        udp dport %s counter drop
    }
}
NFT
cat > /usr/local/sbin/apply-smtp-guard <<'SH'
#!/bin/sh
set -eu
CONFIG=/etc/nftables.d/smtp-guard.nft
TMP=$(mktemp)
trap 'rm -f "$TMP"' EXIT
if nft list table inet smtp_guard >/dev/null 2>&1; then
    echo "delete table inet smtp_guard" > "$TMP"
fi
cat "$CONFIG" >> "$TMP"
nft -c -f "$TMP"
nft -f "$TMP"
SH
chmod 0750 /usr/local/sbin/apply-smtp-guard
cat > /etc/systemd/system/smtp-guard.service <<'UNIT'
[Unit]
Description=Mail ports firewall guard
After=local-fs.target ufw.service nftables.service
Before=docker.service

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/local/sbin/apply-smtp-guard
ExecReload=/usr/local/sbin/apply-smtp-guard

[Install]
WantedBy=multi-user.target
UNIT
systemctl daemon-reload
systemctl enable smtp-guard.service
systemctl restart smtp-guard.service
`, portSet, portSet, portSet, portSet, portSet, portSet)
	return check, apply, nil
}
