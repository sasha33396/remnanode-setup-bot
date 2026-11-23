package orchestrator

import (
	"context"
	"net/netip"
	"testing"

	"remnanode-setup-bot/internal/deployment"
	"remnanode-setup-bot/internal/remnawave"
	"remnanode-setup-bot/internal/repository"
)

func TestCertificateAuditEnumeratesHostOnceAndAggregatesNodes(t *testing.T) {
	fixture := newFixture(t)
	fixture.remnawave.hosts[0].Nodes = []string{testNodeUUID}
	fixture.cert.auditResult.Checked = 2
	fixture.cert.auditResult.Healthy = 1
	fixture.cert.auditResult.Repaired = 1
	app, err := NewTelegramApplication(fixture.service)
	if err != nil {
		t.Fatal(err)
	}
	result, err := app.AuditCertificates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Hosts != 1 || result.Checked != 2 || result.Healthy != 1 || result.Repaired != 1 || fixture.cert.auditCalls != 1 {
		t.Fatalf("audit result = %#v, calls = %d", result, fixture.cert.auditCalls)
	}
}

func TestCertificateAuditSkipsSelfSNIHostsManagedByNodeCertbot(t *testing.T) {
	fixture := newFixture(t)
	item := deployment.Deployment{
		ID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", PanelID: "default", Kind: deployment.KindAdvancedBalancer,
		AdvancedHostType: deployment.AdvancedHostTypeSelfSNI, AdvancedRealitySNI: "cover.example.net",
		SNIDomain: "entry.example.com", NodeName: "selfsni-node", TargetVPSIP: netip.MustParseAddr("8.8.8.8"),
		RemnawaveNodeUUID: stringPtr(testNodeUUID),
	}
	fixture.repo.items[item.ID] = item
	fixture.remnawave.hosts[0].Address = "entry.example.com"
	fixture.remnawave.hosts[0].Nodes = []string{testNodeUUID}
	fixture.remnawave.nodes = []remnawave.Node{{UUID: testNodeUUID, Name: item.NodeName, Address: item.TargetVPSIP.String(), IsConnected: true}}
	app, err := NewTelegramApplication(fixture.service)
	if err != nil {
		t.Fatal(err)
	}
	result, err := app.AuditCertificates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Hosts != 0 || fixture.cert.auditCalls != 0 {
		t.Fatalf("SelfSNI certificate was sent to central audit: result=%#v calls=%d SNIs=%v", result, fixture.cert.auditCalls, fixture.cert.auditSNIs)
	}
}

func TestCertificateAuditSkipsIPAddressHosts(t *testing.T) {
	fixture := newFixture(t)
	fixture.remnawave.hosts[0].Address = "203.0.113.10"
	fixture.remnawave.hosts[0].Nodes = []string{testNodeUUID}
	app, err := NewTelegramApplication(fixture.service)
	if err != nil {
		t.Fatal(err)
	}
	result, err := app.AuditCertificates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Hosts != 0 || fixture.cert.auditCalls != 0 {
		t.Fatalf("audit result = %#v, calls = %d", result, fixture.cert.auditCalls)
	}
}

func TestCertificateAuditIncludesManagedNodeSNIAbsentFromHosts(t *testing.T) {
	fixture := newFixture(t)
	prepared := fixture.prepare(t, "managed-node", "8.8.4.4")
	if _, err := fixture.repo.SetRemnawaveNodeUUID(context.Background(), prepared.Deployment.ID, testNodeUUID); err != nil {
		t.Fatal(err)
	}
	const nodeSNI = "service-nl.storagedirectguide.com"
	if _, err := fixture.repo.SetNodeHostBinding(context.Background(), prepared.Deployment.ID, repository.SetNodeHostBindingParams{HostUUID: testHostUUID, HostRemark: nodeSNI, SNIDomain: nodeSNI}); err != nil {
		t.Fatal(err)
	}
	fixture.remnawave.hosts[0].Nodes = nil
	fixture.remnawave.nodes = []remnawave.Node{{UUID: testNodeUUID, Name: "managed-node", Address: "8.8.4.4", IsConnected: true}}
	app, err := NewTelegramApplication(fixture.service)
	if err != nil {
		t.Fatal(err)
	}
	result, err := app.AuditCertificates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Hosts != 1 || fixture.cert.auditCalls != 1 || len(fixture.cert.auditSNIs) != 1 || fixture.cert.auditSNIs[0] != nodeSNI {
		t.Fatalf("audit result=%#v calls=%d SNIs=%v", result, fixture.cert.auditCalls, fixture.cert.auditSNIs)
	}
}
