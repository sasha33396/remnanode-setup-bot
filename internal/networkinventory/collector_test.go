package networkinventory

import (
	"context"
	"errors"
	"net/netip"
	sshclient "remnanode-setup-bot/internal/ssh"
	"strings"
	"testing"
)

func TestConfigAddressEvidence(t *testing.T) {
	ip := netip.MustParseAddr("8.8.8.8")
	for _, test := range []struct {
		kind, content string
		match         bool
	}{
		{"Netplan", "network:\n  ethernets:\n    eth0:\n      addresses: [8.8.8.8/24, 1.1.1.1/32, '2001:db8::1/64']", true},
		{"Netplan", "network:\n  ethernets:\n    eth0:\n      nameservers:\n        addresses: [8.8.8.8]", false},
		{"interfaces", "# address 8.8.8.8\naddress 1.1.1.1/32", false},
		{"interfaces", "iface ens3 inet static\n address 8.8.8.8/24", true},
		{"networkd", "[Address]\nAddress=8.8.8.8/24", true},
		{"networkd", "[Network]\nDNS=8.8.8.8", false},
	} {
		if got := configContainsAddress(test.kind, test.content, ip); got != test.match {
			t.Errorf("%s: match=%v want=%v", test.content, got, test.match)
		}
	}
}

type probeRunner struct {
	commands      []string
	failAddresses bool
}

func (r *probeRunner) Run(_ context.Context, request sshclient.CommandRequest) (sshclient.CommandResult, error) {
	r.commands = append(r.commands, request.Command)
	var output string
	switch request.Command {
	case "ip -j address show":
		if r.failAddresses {
			return sshclient.CommandResult{}, errors.New("missing ip")
		}
		output = `[{"ifname":"ens3","addr_info":[{"local":"8.8.8.8","prefixlen":24},{"local":"1.1.1.1","prefixlen":32},{"local":"2001:db8::1","prefixlen":64}]}]`
	case filesCommand:
		output = "/etc/netplan/provider.yaml\n/etc/NetworkManager/system-connections/private.nmconnection\n/etc/netplan/old.yaml.bak\n"
	case "cat -- '/etc/netplan/provider.yaml'":
		output = "network:\n  ethernets:\n    ens3:\n      addresses: [8.8.8.8/24, 1.1.1.1/32]"
	case "systemctl is-active systemd-networkd":
		output = "active\n"
	case "systemctl is-active NetworkManager", "systemctl is-active networking":
		return sshclient.CommandResult{}, errors.New("inactive")
	case "ip -j -4 route show table all":
		output = `[{"dst":"default","gateway":"8.8.8.1","dev":"ens3","secret":"must not persist"}]`
	case "ip -j -6 route show table all", "ip -j -4 rule show", "ip -j -6 rule show":
		output = "[]"
	default:
		return sshclient.CommandResult{}, errors.New("unexpected command")
	}
	return sshclient.CommandResult{Stdout: output}, nil
}

func TestCollectorPreservesSecondaryAddressesAndOnlyReads(t *testing.T) {
	runner := &probeRunner{}
	report, err := Collect(context.Background(), runner, netip.MustParseAddr("8.8.8.8"))
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Addresses) != 3 || !report.Addresses[0].Primary || report.Addresses[1].Primary || report.Addresses[1].Prefix != 32 {
		t.Fatalf("wrong addresses: %+v", report.Addresses)
	}
	if report.Status != "observed" || len(report.Files) != 2 || report.Routes[0].Gateway != "8.8.8.1" {
		t.Fatalf("wrong report: %+v", report)
	}
	for _, command := range runner.commands {
		if strings.Contains(command, "private.nmconnection") || strings.Contains(command, "apply") || strings.Contains(command, "restart") {
			t.Fatalf("unsafe command: %s", command)
		}
	}
}

func TestMissingPrimaryRequiresReview(t *testing.T) {
	report, err := Collect(context.Background(), &probeRunner{}, netip.MustParseAddr("9.9.9.9"))
	if err != nil || report.Status != "review" {
		t.Fatalf("report=%+v error=%v", report, err)
	}
	_, err = Collect(context.Background(), &probeRunner{failAddresses: true}, netip.MustParseAddr("8.8.8.8"))
	if err == nil {
		t.Fatal("missing addresses must fail")
	}
}
