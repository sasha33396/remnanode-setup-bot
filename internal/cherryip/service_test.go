package cherryip

import (
	"context"
	"net/netip"
	"strings"
	"testing"
	"time"

	sshclient "remnanode-setup-bot/internal/ssh"
)

func TestAddAddressToNetplanMatchesInterfaceAndSetName(t *testing.T) {
	for _, document := range []string{
		"network:\n  version: 2\n  ethernets:\n    ens3:\n      addresses: [203.0.113.10/24]\n",
		"network:\n  version: 2\n  ethernets:\n    uplink:\n      set-name: ens3\n      addresses:\n      - 203.0.113.10/24\n",
	} {
		updated, key, changed, err := addAddressToNetplan([]byte(document), "ens3", "198.51.100.20/32")
		if err != nil || key == "" || !changed || !strings.Contains(string(updated), "198.51.100.20/32") {
			t.Fatalf("addAddressToNetplan() key=%q changed=%v err=%v document=%s", key, changed, err, updated)
		}
		_, _, changed, err = addAddressToNetplan(updated, "ens3", "198.51.100.20/32")
		if err != nil || changed {
			t.Fatalf("second add changed=%v err=%v", changed, err)
		}
	}
}

func TestConfigureAddsLiveAddressAndPersistsNetplan(t *testing.T) {
	runner := &fakeRunner{netplan: "network:\n  version: 2\n  ethernets:\n    ens3:\n      addresses:\n      - 8.8.8.8/24\n"}
	result, err := configure(context.Background(), runner, netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("1.1.1.1"), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !result.LiveConfigured || !result.Persistent || result.Interface != "ens3" {
		t.Fatalf("result = %#v", result)
	}
	if !strings.Contains(runner.netplan, "1.1.1.1/32") {
		t.Fatalf("netplan was not updated: %s", runner.netplan)
	}
}

type fakeRunner struct {
	netplan string
	live    bool
}

func (f *fakeRunner) Run(_ context.Context, request sshclient.CommandRequest) (sshclient.CommandResult, error) {
	switch request.Command {
	case "ip -4 -o addr show":
		return sshclient.CommandResult{Stdout: "2: ens3    inet 8.8.8.8/24 brd 8.8.8.255 scope global ens3\n"}, nil
	case "ip addr add 1.1.1.1/32 dev ens3":
		f.live = true
		return sshclient.CommandResult{}, nil
	case "ip -4 -o addr show dev ens3":
		if f.live {
			return sshclient.CommandResult{Stdout: "2: ens3 inet 1.1.1.1/32 scope global ens3\n"}, nil
		}
	case "find /etc/netplan -maxdepth 1 -type f \\( -name '*.yaml' -o -name '*.yml' \\) -print":
		return sshclient.CommandResult{Stdout: "/etc/netplan/50-cloud-init.yaml\n"}, nil
	case "cat -- '/etc/netplan/50-cloud-init.yaml'":
		return sshclient.CommandResult{Stdout: f.netplan}, nil
	case "cp -- '/etc/netplan/50-cloud-init.yaml' '/etc/netplan/50-cloud-init.yaml.bak-cherrybot'", "netplan generate", "netplan apply", "test -f /etc/cloud/cloud.cfg.d/99-disable-network-config.cfg || printf '%s\\n' 'network: {config: disabled}' > /etc/cloud/cloud.cfg.d/99-disable-network-config.cfg":
		return sshclient.CommandResult{}, nil
	case "cat > '/etc/netplan/50-cloud-init.yaml'":
		f.netplan = string(request.Stdin)
		return sshclient.CommandResult{}, nil
	}
	return sshclient.CommandResult{ExitStatus: 1}, &sshclient.CommandError{ExitStatus: 1}
}
