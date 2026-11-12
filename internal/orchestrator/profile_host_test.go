package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"slices"
	"testing"

	"remnanode-setup-bot/internal/deployment"
	"remnanode-setup-bot/internal/remnawave"
	"remnanode-setup-bot/internal/telegram"
)

func TestProfileHostOptionsAndIdempotentCreation(t *testing.T) {
	fixture := newFixture(t)
	fixture.remnawave.profiles = []remnawave.ConfigProfile{{
		UUID: testProfileUUID, Name: "DE Reality",
		Config:   json.RawMessage(`{"inbounds":[{"tag":"VLESS_REALITY","port":443,"streamSettings":{"realitySettings":{"serverNames":["DE-MODX.NODEXPHERE.NET.","de-modx.nodexphere.net","invalid name"]}}}]}`),
		Inbounds: []remnawave.ConfigProfileInbound{{UUID: testInboundUUID, Tag: "VLESS_REALITY"}},
	}}
	app, err := NewTelegramApplication(fixture.service)
	if err != nil {
		t.Fatal(err)
	}
	options, err := app.ListProfileHostOptions(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	if len(options.Inbounds) != 1 || options.Inbounds[0].Port != 443 || len(options.Inbounds[0].ServerNames) != 1 || options.Inbounds[0].ServerNames[0] != "de-modx.nodexphere.net" {
		t.Fatalf("profile Host options = %#v", options)
	}
	input := telegram.ProfileHostCreateInput{PanelID: "default", ProfileID: testProfileUUID, InboundID: testInboundUUID, Domain: "DE-MODX.NODEXPHERE.NET.", VPSIP: netip.MustParseAddr("8.8.8.8")}
	first, err := app.EnsureProfileHost(context.Background(), input)
	if err != nil || first.Address != "de-modx.nodexphere.net" {
		t.Fatalf("first EnsureProfileHost() = %#v, %v", first, err)
	}
	second, err := app.EnsureProfileHost(context.Background(), input)
	if err != nil || second.ID != first.ID {
		t.Fatalf("second EnsureProfileHost() = %#v, %v", second, err)
	}
	if len(fixture.remnawave.visibleHostInputs) != 1 {
		t.Fatalf("visible Host create calls = %d", len(fixture.remnawave.visibleHostInputs))
	}
	if fixture.dns.ensureCalls != 2 {
		t.Fatalf("DNS ensure calls = %d, want 2 idempotent calls", fixture.dns.ensureCalls)
	}
}

func TestProfileHostOptionsUseRemnawave281InboundFields(t *testing.T) {
	fixture := newFixture(t)
	port := 8443
	fixture.remnawave.profiles = []remnawave.ConfigProfile{{
		UUID: testProfileUUID, Name: "Snippet Profile", Config: json.RawMessage(`{"log":{"loglevel":"warning"}}`),
		Inbounds: []remnawave.ConfigProfileInbound{{
			UUID: testInboundUUID, Tag: "VLESS_SNIPPET", Port: &port,
			RawInbound: json.RawMessage(`{"tag":"VLESS_SNIPPET","port":8443,"streamSettings":{"realitySettings":{"serverNames":["snippet.example.com"]}}}`),
		}},
	}}
	app, err := NewTelegramApplication(fixture.service)
	if err != nil {
		t.Fatal(err)
	}
	options, err := app.ListProfileHostOptions(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	if len(options.Inbounds) != 1 || options.Inbounds[0].Port != 8443 || len(options.Inbounds[0].ServerNames) != 1 || options.Inbounds[0].ServerNames[0] != "snippet.example.com" {
		t.Fatalf("2.8.1 inbound options = %#v", options)
	}
}

func TestProfileHostDNSUsesOnlySelectedPanelInstance(t *testing.T) {
	events := &eventLog{}
	repo := newMemoryRepository()
	type panelFixture struct {
		service *DeploymentService
		dns     *fakeDNS
	}
	newPanel := func(t *testing.T, panelID string) panelFixture {
		t.Helper()
		port := 443
		api := &fakeRemnawave{
			events: events, secret: "generated-secret",
			profiles: []remnawave.ConfigProfile{{
				UUID: testProfileUUID, Name: "Reality",
				Inbounds: []remnawave.ConfigProfileInbound{{UUID: testInboundUUID, Tag: "VLESS", Port: &port}},
			}},
		}
		dns := &fakeDNS{events: events}
		cert := &fakeCertificateProvider{events: events, readiness: CertificateReady}
		service, err := NewDeploymentService(repo, api, dns, cert, &fakeVPS{events: events}, Config{PanelID: panelID, MaxConcurrentDeployments: 1})
		if err != nil {
			t.Fatal(err)
		}
		return panelFixture{service: service, dns: dns}
	}
	polus := newPanel(t, "polus")
	horda := newPanel(t, "horda")
	hit := newPanel(t, "hit")
	app, err := NewMultiPanelTelegramApplication([]PanelApplicationConfig{
		{ID: "polus", Name: "Полюс", DNSEnabled: true, Service: polus.service},
		{ID: "horda", Name: "Horda", DNSEnabled: true, Service: horda.service},
		{ID: "hit", Name: "Hit", DNSEnabled: true, Service: hit.service},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = app.EnsureProfileHost(context.Background(), telegram.ProfileHostCreateInput{
		PanelID: "horda", ProfileID: testProfileUUID, InboundID: testInboundUUID,
		Domain: "nl-horda.example.com", VPSIP: netip.MustParseAddr("8.8.8.8"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if polus.dns.ensureCalls != 0 || horda.dns.ensureCalls != 1 || hit.dns.ensureCalls != 0 {
		t.Fatalf("DNS calls polus=%d horda=%d hit=%d", polus.dns.ensureCalls, horda.dns.ensureCalls, hit.dns.ensureCalls)
	}
}

func TestProfileListErrorExposesOnlySafeHTTPStatus(t *testing.T) {
	safeErr, category, status := profileListError(&remnawave.HTTPError{StatusCode: 502})
	if safeErr.Code != "PROFILE_LIST_HTTP_ERROR" || safeErr.SafeMessage != "Remnawave API вернул HTTP 502 при чтении Config Profiles." || category != "http" || status != 502 {
		t.Fatalf("profileListError() = %#v, %q, %d", safeErr, category, status)
	}
}

func TestProfileHostCreationRejectsAddressConflict(t *testing.T) {
	fixture := newFixture(t)
	fixture.remnawave.profiles = []remnawave.ConfigProfile{{
		UUID: testProfileUUID, Name: "DE Reality", Config: json.RawMessage(`{"inbounds":[{"tag":"VLESS_REALITY","port":443}]}`),
		Inbounds: []remnawave.ConfigProfileInbound{{UUID: testInboundUUID, Tag: "VLESS_REALITY"}},
	}}
	otherProfile := "55555555-5555-4555-8555-555555555555"
	fixture.remnawave.hosts = append(fixture.remnawave.hosts, remnawave.Host{
		UUID: "66666666-6666-4666-8666-666666666666", Remark: "conflict", Address: "de-modx.nodexphere.net",
		Inbound: remnawave.HostInbound{ConfigProfileUUID: &otherProfile, ConfigProfileInboundUUID: stringPtr(testInboundUUID)},
	})
	app, err := NewTelegramApplication(fixture.service)
	if err != nil {
		t.Fatal(err)
	}
	_, err = app.EnsureProfileHost(context.Background(), telegram.ProfileHostCreateInput{PanelID: "default", ProfileID: testProfileUUID, InboundID: testInboundUUID, Domain: "de-modx.nodexphere.net", VPSIP: netip.MustParseAddr("8.8.8.8")})
	if !errors.Is(err, errProfileHostConflict) || len(fixture.remnawave.visibleHostInputs) != 0 {
		t.Fatalf("EnsureProfileHost() error = %v, creates = %d", err, len(fixture.remnawave.visibleHostInputs))
	}
}

func TestProfileHostDeploymentBindsCreatedNodeToHost(t *testing.T) {
	fixture := newFixture(t)
	profileUUID, inboundUUID := testProfileUUID, testInboundUUID
	fixture.remnawave.hosts = []remnawave.Host{{
		UUID: testHostUUID, Remark: "de-modx.nodexphere.net", Address: "de-modx.nodexphere.net",
		Tags:    []string{remnawave.ProfileHostTag},
		Inbound: remnawave.HostInbound{ConfigProfileUUID: &profileUUID, ConfigProfileInboundUUID: &inboundUUID},
	}}
	prepared := fixture.prepare(t, "profile-node", "8.8.4.4")
	if err := fixture.service.Deploy(context.Background(), startInput(prepared.Deployment), nil); err != nil {
		t.Fatal(err)
	}
	if len(fixture.remnawave.updatedHostNodes) != 1 || fixture.remnawave.updatedHostNodes[0].UUID != testHostUUID || len(fixture.remnawave.updatedHostNodes[0].Nodes) != 1 || fixture.remnawave.updatedHostNodes[0].Nodes[0] != testNodeUUID {
		t.Fatalf("Host node updates = %#v", fixture.remnawave.updatedHostNodes)
	}
	if fixture.dns.ensureCalls != 1 || fixture.dns.addCalls != 0 {
		t.Fatalf("profile Host DNS calls = ensure:%d add:%d", fixture.dns.ensureCalls, fixture.dns.addCalls)
	}
	events := fixture.events.snapshot()
	if !slices.Contains(events, "certificate.prepare-new") || slices.Contains(events, "certificate.prepare") {
		t.Fatalf("profile Host certificate path = %v", events)
	}
}

func TestProfileHostCertificateFailureOffersDirectRetry(t *testing.T) {
	fixture := newFixture(t)
	profileUUID, inboundUUID := testProfileUUID, testInboundUUID
	fixture.remnawave.hosts = []remnawave.Host{{
		UUID: testHostUUID, Remark: "de-modx.nodexphere.net", Address: "de-modx.nodexphere.net",
		Tags:    []string{remnawave.ProfileHostTag},
		Inbound: remnawave.HostInbound{ConfigProfileUUID: &profileUUID, ConfigProfileInboundUUID: &inboundUUID},
	}}
	prepared := fixture.prepare(t, "profile-node-retry", "8.8.4.5")
	if err := fixture.service.setState(context.Background(), prepared.Deployment.ID, deployment.StatusFailed, stepPrepareCertificate, "CERTIFICATE_DISTRIBUTION_FAILED", "DNS-balancer certificate zone is unavailable"); err != nil {
		t.Fatal(err)
	}
	app, err := NewTelegramApplication(fixture.service)
	if err != nil {
		t.Fatal(err)
	}
	details, err := app.GetDeploymentDetails(context.Background(), prepared.Deployment.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !details.CanRetryStep || details.CanRepairCert {
		t.Fatalf("profile Host recovery actions = retry:%t repair:%t", details.CanRetryStep, details.CanRepairCert)
	}
}

func TestEmptyVisibleHostUsesNewTargetCertificateWithoutTag(t *testing.T) {
	fixture := newFixture(t)
	legacySNI := fixture.remnawave.hosts[0].Address
	fixture.remnawave.hosts[0].Remark = fixture.remnawave.hosts[0].Address
	fixture.remnawave.hosts[0].SNI = &legacySNI
	fixture.remnawave.hosts[0].Tags = nil
	fixture.remnawave.hosts[0].Nodes = nil
	prepared := fixture.prepare(t, "empty-host-node", "8.8.4.6")
	if err := fixture.service.Deploy(context.Background(), startInput(prepared.Deployment), nil); err != nil {
		t.Fatal(err)
	}
	events := fixture.events.snapshot()
	if !slices.Contains(events, "certificate.prepare-new") || slices.Contains(events, "certificate.prepare") {
		t.Fatalf("empty Host certificate path = %v", events)
	}
	if len(fixture.remnawave.updatedHostNodes) != 1 || fixture.remnawave.updatedHostNodes[0].UUID != testHostUUID || len(fixture.remnawave.updatedHostNodes[0].Nodes) != 1 || fixture.remnawave.updatedHostNodes[0].Nodes[0] != testNodeUUID {
		t.Fatalf("legacy 2.7 Host node updates = %#v", fixture.remnawave.updatedHostNodes)
	}
	if fixture.dns.ensureCalls != 1 || fixture.dns.addCalls != 0 {
		t.Fatalf("legacy 2.7 Host DNS calls = ensure:%d add:%d", fixture.dns.ensureCalls, fixture.dns.addCalls)
	}
}

func TestLegacyProfileHostDNSRetryRestoresZoneAndBinding(t *testing.T) {
	fixture := newFixture(t)
	legacySNI := fixture.remnawave.hosts[0].Address
	fixture.remnawave.hosts[0].Remark = fixture.remnawave.hosts[0].Address
	fixture.remnawave.hosts[0].SNI = &legacySNI
	fixture.remnawave.hosts[0].Tags = nil
	fixture.dns.addErr = errors.New("zone was removed before Node became healthy")
	prepared := fixture.prepare(t, "legacy-profile-dns-retry", "8.8.4.8")
	if err := fixture.service.Deploy(context.Background(), startInput(prepared.Deployment), nil); !errors.Is(err, ErrDNSUpdateFailed) {
		t.Fatalf("Deploy() error = %v, want DNS failure", err)
	}

	// Reproduce a deployment completed by the older 2.7-incompatible build:
	// its Host had neither the retained tag nor the Node assignment.
	fixture.remnawave.hosts[0].Tags = nil
	fixture.remnawave.hosts[0].Nodes = nil
	fixture.remnawave.updatedHostNodes = nil
	fixture.dns.addErr = nil
	if err := fixture.service.RetryDNS(context.Background(), prepared.Deployment.ID, nil); err != nil {
		t.Fatal(err)
	}
	if fixture.dns.ensureCalls != 2 || fixture.dns.addCalls != 0 {
		t.Fatalf("legacy DNS retry calls = ensure:%d add:%d", fixture.dns.ensureCalls, fixture.dns.addCalls)
	}
	if len(fixture.remnawave.updatedHostNodes) != 1 || len(fixture.remnawave.updatedHostNodes[0].Nodes) != 1 || fixture.remnawave.updatedHostNodes[0].Nodes[0] != testNodeUUID {
		t.Fatalf("legacy Host repair = %#v", fixture.remnawave.updatedHostNodes)
	}
	if stored := fixture.repo.mustGet(prepared.Deployment.ID); stored.Status != deployment.StatusCompleted {
		t.Fatalf("deployment status after DNS retry = %s", stored.Status)
	}
}
