package orchestrator

import (
	"context"
	"testing"
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
