package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"testing"

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
}
