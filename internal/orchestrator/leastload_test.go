package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"remnanode-setup-bot/internal/deployment"
	"remnanode-setup-bot/internal/leastload"
	"remnanode-setup-bot/internal/remnawave"
	sshclient "remnanode-setup-bot/internal/ssh"
	"strings"
	"testing"
	"time"
)

type ipJobStore struct {
	*memoryRepository
	data    []byte
	pending bool
}

func (s *ipJobStore) SaveLeastloadJob(_ context.Context, j leastload.Job) error {
	var err error
	s.data, err = json.Marshal(j)
	return err
}
func (s *ipJobStore) GetLeastloadJob(context.Context, string) (leastload.Job, error) {
	var j leastload.Job
	err := json.Unmarshal(s.data, &j)
	return j, err
}
func (s *ipJobStore) ListLeastloadJobs(context.Context) ([]leastload.Job, error) { return nil, nil }
func (s *ipJobStore) HasPendingLeastload(context.Context, string, string) (bool, error) {
	return s.pending, nil
}
func (s *ipJobStore) Lock(context.Context, string) (func(), error) { return func() {}, nil }

type ipRecord struct {
	fakeDNSRecords
	fail    bool
	updates int
}

func (d *ipRecord) ReplaceARecord(_ context.Context, _ string, _, _ netip.Addr, apply bool) error {
	if apply {
		d.updates++
		if d.fail {
			return errors.New("DNS down")
		}
	}
	return nil
}

type verifiedIPSession struct {
	writes  int
	content string
}

func (s *verifiedIPSession) Close() error { return nil }
func (s *verifiedIPSession) Run(_ context.Context, r sshclient.CommandRequest) (sshclient.CommandResult, error) {
	switch {
	case strings.HasPrefix(r.Command, "cat --"):
		return sshclient.CommandResult{Stdout: s.content}, nil
	case r.Command == "ip -j address show":
		return sshclient.CommandResult{Stdout: `[{"ifname":"eth0","addr_info":[{"local":"1.1.1.1","prefixlen":32}]}]`}, nil
	case strings.Contains(r.Command, "route show"), strings.Contains(r.Command, "rule show"):
		return sshclient.CommandResult{Stdout: `[]`}, nil
	default:
		s.writes++
		return sshclient.CommandResult{}, errors.New("unexpected command")
	}
}
func TestLeastloadRetryAfterDNSFailureDoesNotChangeNetwork(t *testing.T) {
	f := newFixture(t)
	store := &ipJobStore{memoryRepository: f.repo}
	f.service.repository = store
	f.remnawave.nodes = []remnawave.Node{{UUID: testNodeUUID, Name: "leastload", Address: "8.8.8.8"}}
	f.remnawave.hosts = []remnawave.Host{{UUID: testHostUUID, IsHidden: true, Address: "edge.example.com", Nodes: []string{testNodeUUID}}}
	f.repo.items["managed"] = deployment.Deployment{ID: "managed", PanelID: "default", RemnawaveNodeUUID: stringPtr(testNodeUUID), SSHHostKeyFingerprint: stringPtr("pinned"), TargetVPSIP: netip.MustParseAddr("8.8.8.8")}
	dns := &ipRecord{fail: true}
	f.service.config.DNSRecords = dns
	app, err := NewTelegramApplication(f.service)
	if err != nil {
		t.Fatal(err)
	}
	app.maintenance = &NodeMaintenance{}
	remote := &verifiedIPSession{content: "persisted config"}
	app.leastloadSSH = func(context.Context, string, netip.Addr) (leastload.Session, error) { return remote, nil }
	job := leastload.Job{ID: "job", PanelID: "default", Operator: 42, CreatedAt: time.Now(), Items: []leastload.Item{{ID: "item", Status: "network_done", Mapping: leastload.Mapping{NodeUUID: testNodeUUID, OldIP: "8.8.8.8", NewIP: "1.1.1.1"}, DeploymentID: "managed", HostID: testHostUUID, Domain: "edge.example.com", Plan: leastload.Plan{NewIP: "1.1.1.1", OldIP: "8.8.8.8", Interface: "eth0", Prefix: 32, Mode: "replace", File: "/etc/netplan/config.yaml", TargetHash: fmt.Sprintf("%x", sha256.Sum256([]byte(remote.content)))}}}}
	_ = store.SaveLeastloadJob(context.Background(), job)
	result, err := app.ApplyLeastload(context.Background(), job.ID, 42, nil)
	if err != nil || result.Items[0].Status != "network_done" || remote.writes != 0 {
		t.Fatalf("partial: %+v %v writes=%d", result, err, remote.writes)
	}
	dns.fail = false
	result, err = app.ApplyLeastload(context.Background(), job.ID, 42, nil)
	if err != nil || result.Items[0].Status != "done" || remote.writes != 0 {
		t.Fatalf("retry: %+v %v", result, err)
	}
	if f.repo.mustGet("managed").TargetVPSIP.String() != "1.1.1.1" {
		t.Fatal("managed address not persisted")
	}
	_, err = app.ApplyLeastload(context.Background(), job.ID, 99, nil)
	if err == nil {
		t.Fatal("another operator executed job")
	}
}
func TestLeastloadPreviewRejectsSwapsBeforeSSH(t *testing.T) {
	f := newFixture(t)
	store := &ipJobStore{memoryRepository: f.repo}
	f.service.repository = store
	app, _ := NewTelegramApplication(f.service)
	app.maintenance = &NodeMaintenance{}
	_, err := app.PrepareLeastload(context.Background(), "default", leastload.ModeNetwork, 42, []leastload.Mapping{{NodeUUID: "a", OldIP: "8.8.8.8", NewIP: "1.1.1.1"}, {NodeUUID: "b", OldIP: "1.1.1.1", NewIP: "8.8.8.8"}}, nil)
	if err == nil || len(store.data) != 0 {
		t.Fatal("address swap accepted")
	}
}

func TestLeastloadControlPlaneModeSkipsSSHAndUpdatesCanonicalAddress(t *testing.T) {
	f := newFixture(t)
	store := &ipJobStore{memoryRepository: f.repo}
	f.service.repository = store
	f.remnawave.nodes = []remnawave.Node{{UUID: testNodeUUID, Name: "leastload", Address: "8.8.8.8"}}
	f.remnawave.hosts = []remnawave.Host{{UUID: testHostUUID, IsHidden: true, Address: "edge.example.com", Nodes: []string{testNodeUUID}}}
	// This mode intentionally does not require a pinned SSH host key.
	f.repo.items["managed"] = deployment.Deployment{ID: "managed", PanelID: "default", RemnawaveNodeUUID: stringPtr(testNodeUUID), TargetVPSIP: netip.MustParseAddr("8.8.8.8")}
	dns := &ipRecord{}
	f.service.config.DNSRecords = dns
	app, err := NewTelegramApplication(f.service)
	if err != nil {
		t.Fatal(err)
	}
	app.maintenance = &NodeMaintenance{}

	job, err := app.PrepareLeastload(context.Background(), "default", leastload.ModeControlPlane, 42, []leastload.Mapping{{NodeUUID: testNodeUUID, OldIP: "8.8.8.8", NewIP: "1.1.1.1"}}, nil)
	if err != nil || len(job.Items) != 1 || job.Items[0].Status != "prepared" || job.Items[0].Plan.Mode != leastload.ModeControlPlane {
		t.Fatalf("PrepareLeastload() = %#v, %v", job, err)
	}
	job, err = app.ApplyLeastload(context.Background(), job.ID, 42, nil)
	if err != nil || job.Items[0].Status != "done" {
		t.Fatalf("ApplyLeastload() = %#v, %v", job, err)
	}
	if dns.updates != 1 || f.remnawave.nodes[0].Address != "1.1.1.1" || f.repo.mustGet("managed").TargetVPSIP.String() != "1.1.1.1" {
		t.Fatalf("control-plane result: dns=%d node=%s deployment=%s", dns.updates, f.remnawave.nodes[0].Address, f.repo.mustGet("managed").TargetVPSIP)
	}
}

func TestLeastloadEgressModeUpdatesOnlySendThroughWithoutSSH(t *testing.T) {
	f := newFixture(t)
	store := &ipJobStore{memoryRepository: f.repo}
	f.service.repository = store
	f.remnawave.nodes = []remnawave.Node{{UUID: testNodeUUID, Name: "leastload", Address: "8.8.8.8"}}
	config, _ := json.Marshal(map[string]any{"outbounds": []any{map[string]any{"tag": "DIRECT", "protocol": "freedom", "sendThrough": "9.9.9.9"}, map[string]any{"tag": "BLOCK", "protocol": "blackhole"}}})
	f.remnawave.profiles = []remnawave.ConfigProfile{{UUID: testProfileUUID, Name: "edge_example", Config: config}}
	f.repo.items["managed"] = deployment.Deployment{ID: "managed", PanelID: "default", Kind: deployment.KindAdvancedBalancer, RemnawaveNodeUUID: stringPtr(testNodeUUID), TargetVPSIP: netip.MustParseAddr("8.8.8.8"), AdvancedEgressIP: netip.MustParseAddr("9.9.9.9"), AdvancedConfigProfileUUID: testProfileUUID}
	app, err := NewTelegramApplication(f.service)
	if err != nil {
		t.Fatal(err)
	}
	app.maintenance = &NodeMaintenance{}

	job, err := app.PrepareLeastload(context.Background(), "default", leastload.ModeEgress, 42, []leastload.Mapping{{NodeUUID: testNodeUUID, OldIP: "9.9.9.9", NewIP: "1.1.1.1"}}, nil)
	if err != nil || len(job.Items) != 1 || job.Items[0].Status != "prepared" {
		t.Fatalf("PrepareLeastload() = %#v, %v", job, err)
	}
	job, err = app.ApplyLeastload(context.Background(), job.ID, 42, nil)
	if err != nil || job.Items[0].Status != "done" {
		t.Fatalf("ApplyLeastload() = %#v, %v", job, err)
	}
	stored := f.repo.mustGet("managed")
	if stored.AdvancedEgressIP.String() != "1.1.1.1" || stored.TargetVPSIP.String() != "8.8.8.8" || f.remnawave.nodes[0].Address != "8.8.8.8" {
		t.Fatalf("deployment=%+v node=%+v", stored, f.remnawave.nodes[0])
	}
	var updated map[string]any
	if json.Unmarshal(f.remnawave.profiles[0].Config, &updated) != nil || updated["outbounds"].([]any)[0].(map[string]any)["sendThrough"] != "1.1.1.1" {
		t.Fatalf("updated profile = %s", f.remnawave.profiles[0].Config)
	}
}
