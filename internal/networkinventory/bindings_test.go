package networkinventory

import (
	"context"
	"encoding/json"
	"net/netip"
	sshclient "remnanode-setup-bot/internal/ssh"
	"strings"
	"testing"
)

func TestNetplanGeneratedPairsFromRealNetworkShapes(t *testing.T) {
	for _, tc := range []struct{ name, ip, id, addresses, gateway, match string }{
		{"datalix", "45.131.65.91", "eth0", "46.247.109.155/24, 45.131.65.91/32", "46.247.109.1", ""},
		{"cherry", "88.216.214.148", "ens3", "88.216.60.0/32, 88.216.130.228/32, 88.216.214.148/32", "192.168.0.1", "      match:\n        macaddress: '54:52:00:31:e0:12'\n      set-name: eth0\n"},
		{"hostof", "191.217.244.226", "eth0", "191.217.244.239/32, 191.217.244.226/32", "191.217.244.1", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := "network:\n  version: 2\n  renderer: networkd\n  ethernets:\n    " + tc.id + ":\n" + tc.match + "      addresses: [" + tc.addresses + "]\n      routes:\n        - to: default\n          via: " + tc.gateway + "\n          on-link: true\n          metric: 100\n"
			generated := "[Match]\nName=eth0\n[Network]\n"
			for _, address := range strings.Split(tc.addresses, ", ") {
				generated += "Address=" + address + "\n"
			}
			live := map[string]string{"eth0": "54:52:00:31:e0:12"}
			ip := netip.MustParseAddr(tc.ip)
			files := []ConfigFile{
				{Path: "/etc/netplan/50-cloud-init.yaml", Kind: "Netplan", MatchesPrimary: true, Bindings: netplanBindings(source, ip, live)},
				{Path: "/run/systemd/network/10-netplan-" + tc.id + ".network", Kind: "networkd", MatchesPrimary: true, Bindings: networkdBindings(generated, ip, live)},
			}
			if count := correlateFiles(files, "eth0"); count != 1 || files[1].GeneratedFrom != files[0].Path {
				t.Fatalf("count=%d files=%+v", count, files)
			}
			b := files[0].Bindings[0]
			if b.Routes[0].Via != tc.gateway || b.Routes[0].OnLink == nil || !*b.Routes[0].OnLink || *b.Routes[0].Metric != 100 || len(b.Addresses) != len(strings.Split(tc.addresses, ", ")) {
				t.Fatalf("settings lost: %+v", b)
			}
			files[1].GeneratedFrom = ""
			files[1].Bindings[0].Interface = "ens4"
			if correlateFiles(files, "eth0") == 1 {
				t.Fatal("wrong interface accepted")
			}
			files[1].Bindings[0].Interface = "eth0"
			files[1].Bindings[0].Addresses = []string{tc.ip + "/24"}
			if correlateFiles(files, "eth0") == 1 {
				t.Fatal("mismatched generated prefixes accepted")
			}
		})
	}
}

func TestConflictingSourcesAndGeneratedOrphansRequireReview(t *testing.T) {
	b := Binding{ID: "eth0", Interface: "eth0", Addresses: []string{"8.8.8.8/32"}}
	files := []ConfigFile{
		{Path: "/etc/netplan/a.yaml", Kind: "Netplan", MatchesPrimary: true, Bindings: []Binding{b}},
		{Path: "/etc/netplan/b.yaml", Kind: "Netplan", MatchesPrimary: true, Bindings: []Binding{b}},
		{Path: "/run/systemd/network/10-netplan-eth0.network", Kind: "networkd", MatchesPrimary: true, Bindings: []Binding{b}},
	}
	if correlateFiles(files, "eth0") == 1 || files[2].GeneratedFrom != "" {
		t.Fatal("conflicting sources folded")
	}
	if correlateFiles(files[2:], "eth0") == 1 {
		t.Fatal("orphan generated file accepted")
	}
}

func TestBindingMACMismatchAndSecretExclusion(t *testing.T) {
	source := "network:\n  ethernets:\n    ens3:\n      match:\n        macaddress: 'aa:bb:cc:dd:ee:ff'\n      set-name: eth0\n      addresses: [8.8.8.8/32]\n      auth:\n        password: super-secret\n"
	b := netplanBindings(source, netip.MustParseAddr("8.8.8.8"), map[string]string{"eth0": "00:00:00:00:00:01"})
	if len(b) != 1 || b[0].Interface != "" {
		t.Fatal("mismatched MAC resolved")
	}
	data, _ := json.Marshal(b)
	if strings.Contains(string(data), "super-secret") {
		t.Fatal("secret persisted")
	}
	generated := "[Match]\nName=eth0\nPermanentMACAddress=aa:bb:cc:dd:ee:ff\n[Network]\nAddress=8.8.8.8/32\n"
	bound := networkdBindings(generated, netip.MustParseAddr("8.8.8.8"), map[string]string{"eth0": "aa:bb:cc:dd:ee:ff"})
	if len(bound) != 1 || bound[0].Interface != "eth0" {
		t.Fatal("permanent MAC evidence was lost")
	}
}

type routeProbe struct{ probeRunner }

func (r *routeProbe) Run(ctx context.Context, req sshclient.CommandRequest) (sshclient.CommandResult, error) {
	if req.Command == "ip -j -4 route show table all" {
		return sshclient.CommandResult{Stdout: `[{"dst":"default","gateway":"192.168.0.1","dev":"ens3","flags":["onlink"],"metric":100,"protocol":"static"}]`}, nil
	}
	return r.probeRunner.Run(ctx, req)
}
func TestCollectorRetainsRouteFlagsAndFamilies(t *testing.T) {
	report, err := Collect(context.Background(), &routeProbe{}, netip.MustParseAddr("8.8.8.8"))
	if err != nil {
		t.Fatal(err)
	}
	route := report.Routes[0]
	if report.Version != 2 || report.NodeInterface != "ens3" || !route.OnLink || route.Family != "-4" || route.Metric == nil || *route.Metric != 100 || route.Protocol != "static" {
		t.Fatalf("report=%+v route=%+v", report, route)
	}
}
