package leastload

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"testing"

	"remnanode-setup-bot/internal/networkinventory"
	sshclient "remnanode-setup-bot/internal/ssh"
)

const source = `network:
  version: 2
  ethernets:
    eth0:
      addresses: [8.8.8.8/32, 1.1.1.1/32, '2001:db8::1/64']
      routes:
        - to: default
          via: 192.168.0.1
          on-link: true
      nameservers:
        addresses: [9.9.9.9]
`

func TestRewritePreservesSecondaryIPv4IPv6AndGateway(t *testing.T) {
	updated, mode, bits, err := rewrite([]byte(source), "eth0", netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("8.8.4.4"))
	if err != nil || mode != "replace" || bits != 32 {
		t.Fatalf("%s %d %v", mode, bits, err)
	}
	for _, preserved := range []string{"1.1.1.1/32", "2001:db8::1/64", "192.168.0.1", "on-link: true", "9.9.9.9", "8.8.4.4/32"} {
		if !strings.Contains(string(updated), preserved) {
			t.Fatalf("lost %s", preserved)
		}
	}
	if strings.Contains(string(updated), "8.8.8.8") {
		t.Fatal("old address retained")
	}
}
func TestExistingNewAddressNeverRewritesFile(t *testing.T) {
	updated, mode, bits, err := rewrite([]byte(source), "eth0", netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("1.1.1.1"))
	if err != nil || mode != "existing" || bits != 32 || string(updated) != source {
		t.Fatalf("existing address changed: %s %v", mode, err)
	}
}
func TestRewriteRejectsDuplicateAndPolicyReferences(t *testing.T) {
	for _, text := range []string{
		strings.Replace(source, "1.1.1.1/32", "8.8.8.8/32", 1),
		source + "      routing-policy:\n        - from: 8.8.8.8/32\n          table: 100\n",
		strings.Replace(source, "addresses: [8.8.8.8", "addresses: &ips [8.8.8.8", 1),
	} {
		if _, _, _, err := rewrite([]byte(text), "eth0", netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("8.8.4.4")); err == nil {
			t.Fatal("unsafe configuration accepted")
		}
	}
}

type fakeSession struct {
	content, liveNew, status string
	commands                 []string
	inputs                   map[string]string
	failTimer                bool
}

func (s *fakeSession) Close() error { return nil }
func (s *fakeSession) Run(_ context.Context, req sshclient.CommandRequest) (sshclient.CommandResult, error) {
	s.commands = append(s.commands, req.Command)
	if s.inputs == nil {
		s.inputs = map[string]string{}
	}
	s.inputs[req.Command] = string(req.Stdin)
	output := ""
	switch {
	case req.Command == "ip -j address show":
		output = `[{"ifname":"eth0","addr_info":[{"local":"8.8.8.8","prefixlen":32},{"local":"2001:db8::1","prefixlen":64}`
		if s.liveNew != "absent" {
			output += `,{"local":"1.1.1.1","prefixlen":32}`
		}
		output += `]}]`
	case strings.Contains(req.Command, " route show table all"):
		output = `[{"dst":"default","gateway":"192.168.0.1","dev":"eth0","protocol":"static","flags":["onlink"]}]`
	case strings.Contains(req.Command, " rule show"):
		output = "[]"
	case strings.HasPrefix(req.Command, "systemctl is-active "):
		output = "active"
	case strings.HasPrefix(req.Command, "find /etc/netplan"):
		output = "/etc/netplan/provider.yaml\n"
	case req.Command == "cat -- '/etc/netplan/provider.yaml'":
		output = s.content
	case strings.Contains(req.Command, "/status'"):
		output = s.status
	case strings.HasPrefix(req.Command, "systemd-run") && strings.Contains(req.Command, "--on-active") && s.failTimer:
		return sshclient.CommandResult{}, errors.New("timer unavailable")
	}
	return sshclient.CommandResult{Stdout: output}, nil
}
func TestPrepareModesAndExplicitNetworkRules(t *testing.T) {
	for _, tc := range []struct{ name, newIP, live, mode string }{
		{"already active", "1.1.1.1", "present", "metadata"},
		{"configured inactive", "1.1.1.1", "absent", "activate"},
		{"replacement", "8.8.4.4", "present", "replace"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("LEASTLOAD_NETWORK_RULES", "8.8.8.0/24>8.8.4.0/24")
			s := &fakeSession{content: source, liveNew: tc.live}
			p, err := Prepare(context.Background(), s, netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr(tc.newIP))
			if err != nil || p.Mode != tc.mode {
				t.Fatalf("plan=%+v err=%v", p, err)
			}
			for _, command := range s.commands {
				if strings.Contains(command, "apply") || strings.Contains(command, "cat >") || strings.Contains(command, "mkdir") {
					t.Fatal("preview mutated server")
				}
			}
		})
	}
	t.Setenv("LEASTLOAD_NETWORK_RULES", "")
	if _, err := Prepare(context.Background(), &fakeSession{content: source}, netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("8.8.4.4")); err == nil {
		t.Fatal("unknown provider network accepted")
	}
}
func TestStartArmsRollbackBeforeWorkerAndRejectsStaleFile(t *testing.T) {
	p := Plan{OldIP: "8.8.8.8", NewIP: "8.8.4.4", Mode: "replace", File: "/etc/netplan/provider.yaml", Section: "eth0", FileHash: digest([]byte(source))}
	id := "12345678-1234-4234-8234-123456789abc"
	s := &fakeSession{content: source, failTimer: true}
	if err := Start(context.Background(), s, id, p); err == nil {
		t.Fatal("timer failure ignored")
	}
	for _, command := range s.commands {
		if strings.HasPrefix(command, "systemd-run") && !strings.Contains(command, "--on-active") {
			t.Fatal("worker started without rollback")
		}
	}
	stale := &fakeSession{content: source + "\n"}
	if err := Start(context.Background(), stale, id, p); err == nil || len(stale.commands) != 1 {
		t.Fatal("stale preview mutated files")
	}
	s = &fakeSession{content: source}
	if err := Start(context.Background(), s, id, p); err != nil {
		t.Fatal(err)
	}
	timerIndex, workerIndex := -1, -1
	for i, c := range s.commands {
		if strings.HasPrefix(c, "systemd-run") {
			if strings.Contains(c, "--on-active") {
				timerIndex = i
			} else {
				workerIndex = i
			}
		}
	}
	if timerIndex < 0 || workerIndex <= timerIndex {
		t.Fatal("rollback scheduling order")
	}
}
func TestVerifyRequiresPersistentConfigurationAndSecondaryAddresses(t *testing.T) {
	p := Plan{OldIP: "8.8.8.8", NewIP: "1.1.1.1", Mode: "metadata", File: "/etc/netplan/provider.yaml", TargetHash: digest([]byte(source)), Interface: "eth0", Prefix: 32, Before: networkinventory.Report{Addresses: []networkinventory.Address{{Interface: "eth0", IP: "8.8.8.8", Prefix: 32}}}}
	if err := Verify(context.Background(), &fakeSession{content: source}, p); err != nil {
		t.Fatal(err)
	}
	if err := Verify(context.Background(), &fakeSession{content: source + "\n"}, p); err == nil {
		t.Fatal("changed persistent config accepted")
	}
	p.Before.Addresses = append(p.Before.Addresses, networkinventory.Address{Interface: "eth0", IP: "9.9.9.9", Prefix: 32})
	if err := Verify(context.Background(), &fakeSession{content: source}, p); err == nil {
		t.Fatal("lost secondary accepted")
	}
}
func TestFinishDoesNotTreatRollbackAsSuccess(t *testing.T) {
	connect := func(context.Context, netip.Addr) (Session, error) { return &fakeSession{status: "rolled_back"}, nil }
	status, err := Finish(context.Background(), connect, "12345678-1234-4234-8234-123456789abc", Plan{OldIP: "8.8.8.8", NewIP: "1.1.1.1"})
	if err == nil || status != "rolled_back" {
		t.Fatal("rollback treated as commit")
	}
}
func TestPlanJSONHasNoRawNetplan(t *testing.T) {
	p, err := Prepare(context.Background(), &fakeSession{content: source}, netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("1.1.1.1"))
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(p)
	if strings.Contains(string(data), "network:\n") {
		t.Fatal("raw config persisted")
	}
	_ = fmt.Sprint(p)
}
