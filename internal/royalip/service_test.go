package royalip

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"
	"time"

	sshclient "remnanode-setup-bot/internal/ssh"
)

const royalNetplan = `network:
  version: 2
  ethernets:
    eth0:
      match:
        macaddress: "bc:24:11:62:85:7f"
      addresses:
      - "37.16.74.103/24"
      - "2a0b:64c0:1::285/48"
      nameservers:
        addresses:
        - 1.1.1.1
        - 8.8.8.8
        search:
        - royalehosting.net
      set-name: "eth0"
      routes:
      - to: "default"
        via: "37.16.74.1"
      - to: "default"
        via: "2a0b:64c0:1::1"
`

func TestReplaceIPv4AndGatewayPreservesIPv6(t *testing.T) {
	updated, key, bits, gateway, err := replaceIPv4AndGateway([]byte(royalNetplan), "eth0", netip.MustParseAddr("37.16.74.103"), netip.MustParseAddr("47.23.12.146"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(updated)
	if key != "eth0" || bits != 24 || gateway.String() != "47.23.12.1" {
		t.Fatalf("key=%q bits=%d gateway=%s", key, bits, gateway)
	}
	for _, expected := range []string{"47.23.12.146/24", "47.23.12.1", "2a0b:64c0:1::285/48", "2a0b:64c0:1::1", "royalehosting.net"} {
		if !strings.Contains(text, expected) {
			t.Fatalf("updated netplan is missing %q:\n%s", expected, text)
		}
	}
	if strings.Contains(text, "37.16.74.103") || strings.Contains(text, "37.16.74.1") {
		t.Fatalf("old IPv4 data remains in netplan:\n%s", text)
	}
}

func TestReplaceIPv4AndGatewayMatchesSetName(t *testing.T) {
	document := strings.Replace(royalNetplan, "    eth0:\n", "    uplink:\n", 1)
	updated, key, _, _, err := replaceIPv4AndGateway([]byte(document), "eth0", netip.MustParseAddr("37.16.74.103"), netip.MustParseAddr("47.23.12.146"))
	if err != nil || key != "uplink" || !strings.Contains(string(updated), "47.23.12.146/24") {
		t.Fatalf("key=%q err=%v document=%s", key, err, updated)
	}
}

func TestReplaceIPv4AndGatewayRequiresIPv4DefaultRoute(t *testing.T) {
	document := strings.Replace(royalNetplan, "      - to: \"default\"\n        via: \"37.16.74.1\"\n", "", 1)
	_, key, _, _, err := replaceIPv4AndGateway([]byte(document), "eth0", netip.MustParseAddr("37.16.74.103"), netip.MustParseAddr("47.23.12.146"))
	if err == nil || key != "eth0" {
		t.Fatalf("key=%q err=%v", key, err)
	}
}

func TestConfigureAppliesAndVerifiesNewAddress(t *testing.T) {
	oldSession := &fakeSession{netplan: royalNetplan}
	newSession := &fakeSession{verified: true}
	connectCalls := 0
	service := &Service{
		timeout: time.Second,
		connect: func(_ context.Context, identity string, address netip.Addr, password []byte) (remoteSession, error) {
			connectCalls++
			if identity != "royal-server:37.16.74.103" || string(password) != "secret" {
				t.Fatalf("identity=%q address=%s password=%q", identity, address, password)
			}
			if connectCalls == 1 && address.String() == "37.16.74.103" {
				return oldSession, nil
			}
			if connectCalls == 2 && address.String() == "47.23.12.146" {
				return newSession, nil
			}
			return nil, errors.New("unexpected connection")
		},
	}
	result, err := service.Configure(context.Background(), Input{ServerIP: netip.MustParseAddr("37.16.74.103"), NewIP: netip.MustParseAddr("47.23.12.146"), Password: []byte("secret")})
	if err != nil {
		t.Fatal(err)
	}
	if connectCalls != 2 || !oldSession.applied || result.Gateway.String() != "47.23.12.1" || result.PrefixBits != 24 {
		t.Fatalf("calls=%d applied=%v result=%#v", connectCalls, oldSession.applied, result)
	}
	marker := confirmationMarker(netip.MustParseAddr("47.23.12.146"))
	if !strings.Contains(oldSession.applyCommand, "sleep 45") || !strings.Contains(oldSession.applyCommand, ".bak-royalbot") || !strings.Contains(oldSession.applyCommand, marker) {
		t.Fatalf("apply command has no safe rollback: %q", oldSession.applyCommand)
	}
	if !strings.Contains(oldSession.netplan, "47.23.12.146/24") || !strings.Contains(oldSession.netplan, "47.23.12.1") {
		t.Fatalf("netplan was not updated:\n%s", oldSession.netplan)
	}
}

func TestNewCapsRoyalCommandTimeout(t *testing.T) {
	service, err := New(fakeConnector{}, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if service.timeout != maxCommandTimeout {
		t.Fatalf("timeout=%v want=%v", service.timeout, maxCommandTimeout)
	}
}

type fakeConnector struct{}

func (fakeConnector) ConnectInitial(context.Context, string, *sshclient.InitialCredentials) (*sshclient.Connection, error) {
	return nil, ErrSSH
}

type fakeSession struct {
	netplan      string
	verified     bool
	applied      bool
	applyCommand string
}

func (f *fakeSession) Close() error { return nil }

func (f *fakeSession) Run(_ context.Context, request sshclient.CommandRequest) (sshclient.CommandResult, error) {
	switch request.Command {
	case "ip -4 -o addr show":
		return sshclient.CommandResult{Stdout: "2: eth0 inet 37.16.74.103/24 brd 37.16.74.255 scope global eth0\n"}, nil
	case "find /etc/netplan -maxdepth 1 -type f \\( -name '*.yaml' -o -name '*.yml' \\) -print":
		return sshclient.CommandResult{Stdout: "/etc/netplan/50-cloud-init.yaml\n"}, nil
	case "cat -- '/etc/netplan/50-cloud-init.yaml'":
		return sshclient.CommandResult{Stdout: f.netplan}, nil
	case "cat > '/etc/netplan/50-cloud-init.yaml'":
		f.netplan = string(request.Stdin)
		return sshclient.CommandResult{}, nil
	case "cp -- '/etc/netplan/50-cloud-init.yaml' '/etc/netplan/50-cloud-init.yaml.bak-royalbot'", "netplan generate", "test -f /etc/cloud/cloud.cfg.d/99-disable-network-config.cfg || printf '%s\\n' 'network: {config: disabled}' > /etc/cloud/cloud.cfg.d/99-disable-network-config.cfg":
		return sshclient.CommandResult{}, nil
	case "ip -4 -o addr show dev eth0":
		if f.verified {
			return sshclient.CommandResult{Stdout: "2: eth0 inet 47.23.12.146/24 brd 47.23.12.255 scope global eth0\n"}, nil
		}
	case "touch -- " + confirmationMarker(netip.MustParseAddr("47.23.12.146")):
		return sshclient.CommandResult{}, nil
	default:
		if strings.Contains(request.Command, "nohup sh -c") && strings.Contains(request.Command, "/run/remnanode-royal-confirmed-") {
			f.applied = true
			f.applyCommand = request.Command
			return sshclient.CommandResult{}, nil
		}
		return sshclient.CommandResult{ExitStatus: 1}, &sshclient.CommandError{ExitStatus: 1}
	}
	return sshclient.CommandResult{ExitStatus: 1}, &sshclient.CommandError{ExitStatus: 1}
}
