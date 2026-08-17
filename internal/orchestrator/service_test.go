package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"remnanode-setup-bot/internal/certificates"
	"remnanode-setup-bot/internal/certmanager"
	"remnanode-setup-bot/internal/deployment"
	"remnanode-setup-bot/internal/dnsbalancer"
	"remnanode-setup-bot/internal/provisioner"
	"remnanode-setup-bot/internal/remnawave"
	"remnanode-setup-bot/internal/repository"
)

const (
	testHostUUID    = "11111111-1111-4111-8111-111111111111"
	testProfileUUID = "22222222-2222-4222-8222-222222222222"
	testInboundUUID = "33333333-3333-4333-8333-333333333333"
	testNodeUUID    = "44444444-4444-4444-8444-444444444444"
)

func TestDeploymentServiceFullSuccessOrdering(t *testing.T) {
	fixture := newFixture(t)
	prepared := fixture.prepare(t, "node-success", "8.8.8.8")
	var progress []Progress
	if err := fixture.service.Deploy(context.Background(), startInput(prepared.Deployment), func(update Progress) { progress = append(progress, update) }); err != nil {
		t.Fatalf("Deploy() error = %v", err)
	}

	stored := fixture.repo.mustGet(prepared.Deployment.ID)
	if stored.Status != deployment.StatusCompleted || stored.RemnawaveNodeUUID == nil || *stored.RemnawaveNodeUUID != testNodeUUID {
		t.Fatalf("completed deployment = %#v", stored)
	}
	wantStatuses := []deployment.Status{
		deployment.StatusPreflight,
		deployment.StatusPreparingCertificate,
		deployment.StatusProvisioning,
		deployment.StatusCreatingRemnawave,
		deployment.StatusWaitingRemnawave,
		deployment.StatusAddingToDNS,
		deployment.StatusCompleted,
	}
	if got := fixture.repo.statusHistory(prepared.Deployment.ID); !equalStatuses(got, wantStatuses) {
		t.Fatalf("status history = %v, want %v", got, wantStatuses)
	}
	events := fixture.events.snapshot()
	assertBefore(t, events, "certificate.prepare", "vps.provision")
	assertBefore(t, events, "vps.provision", "remnawave.create")
	assertBefore(t, events, "remnawave.connected", "dns.add")
	if fixture.remnawave.createCalls != 1 || fixture.dns.addCalls != 1 || fixture.vps.provisionCalls != 1 {
		t.Fatalf("create=%d dns=%d provision=%d", fixture.remnawave.createCalls, fixture.dns.addCalls, fixture.vps.provisionCalls)
	}
	if string(fixture.vps.secret) != "generated-secret" {
		t.Fatalf("provision secret = %q", fixture.vps.secret)
	}
	if len(progress) == 0 || progress[len(progress)-1].Completed != workflowStepCount {
		t.Fatalf("progress = %#v", progress)
	}
}

func TestDeploymentCompletesWithExplicitSkippedDNSForDisabledPanel(t *testing.T) {
	fixture := newFixture(t)
	fixture.service.config.PanelID = "test"
	fixture.service.config.DNSDisabled = true
	prepared := fixture.prepare(t, "node-without-dns", "8.8.4.4")
	if prepared.Deployment.PanelID != "test" {
		t.Fatalf("panel ID = %q", prepared.Deployment.PanelID)
	}
	if len(prepared.Warnings) != 1 || prepared.Warnings[0].Code != "W-DNS-DISABLED" {
		t.Fatalf("warnings = %#v", prepared.Warnings)
	}
	var progress []Progress
	if err := fixture.service.Deploy(context.Background(), startInput(prepared.Deployment), func(update Progress) { progress = append(progress, update) }); err != nil {
		t.Fatal(err)
	}
	if fixture.dns.addCalls != 0 {
		t.Fatalf("DNS add calls = %d", fixture.dns.addCalls)
	}
	step := fixture.repo.steps[prepared.Deployment.ID][stepAddDNS]
	if step.Status != deployment.StepStatusSkipped {
		t.Fatalf("DNS step = %#v", step)
	}
	last := progress[len(progress)-1]
	if last.Status != deployment.StepStatusSkipped || last.Code != "W-DNS-DISABLED" {
		t.Fatalf("last progress = %#v", last)
	}
}

func TestReplaceNodeIPUpdatesPanelThenDNSAndPersistence(t *testing.T) {
	fixture := newFixture(t)
	item := deployment.Deployment{ID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", TelegramOperatorUserID: 42, SNIDomain: "edge.example.com", NodeName: "node", TargetVPSIP: netip.MustParseAddr("8.8.8.8"), RemnawaveNodeUUID: stringPtr(testNodeUUID), Status: deployment.StatusCompleted}
	fixture.repo.items[item.ID] = item
	fixture.remnawave.nodes = []remnawave.Node{{UUID: testNodeUUID, Name: "node", Address: "8.8.8.8", IsConnected: true}}
	fixture.dns.zones = []dnsbalancer.ZoneMatch{{FQDN: "edge.example.com", Zone: dnsbalancer.Zone{IPs: []string{"8.8.8.8"}}}}
	target, err := fixture.service.FindNodeForIPChange(context.Background(), "node")
	if err != nil || !target.Managed || len(target.DNSZones) != 1 {
		t.Fatalf("FindNodeForIPChange() = %#v, %v", target, err)
	}
	message, err := fixture.service.ReplaceNodeIP(context.Background(), NodeIPChangeInput{NodeUUID: testNodeUUID, ExpectedIP: netip.MustParseAddr("8.8.8.8"), NewIP: netip.MustParseAddr("1.1.1.1")})
	if err != nil || message == "" {
		t.Fatalf("ReplaceNodeIP() = %q, %v", message, err)
	}
	if got := fixture.repo.mustGet(item.ID).TargetVPSIP.String(); got != "1.1.1.1" {
		t.Fatalf("persisted IP = %s", got)
	}
}

func TestReplaceNodeIPSupportsLegacyNodeAndAllMatchingZones(t *testing.T) {
	fixture := newFixture(t)
	fixture.remnawave.nodes = []remnawave.Node{{UUID: testNodeUUID, Name: "legacy-node", Address: "8.8.8.8"}}
	fixture.dns.zones = []dnsbalancer.ZoneMatch{
		{FQDN: "one.example.com", Zone: dnsbalancer.Zone{IPs: []string{"8.8.8.8", "9.9.9.9"}}},
		{FQDN: "two.example.net", Zone: dnsbalancer.Zone{IPs: []string{"8.8.8.8"}}},
	}
	target, err := fixture.service.FindNodeForIPChange(context.Background(), "8.8.8.8")
	if err != nil || target.Managed || len(target.DNSZones) != 2 {
		t.Fatalf("legacy target = %#v, %v", target, err)
	}
	_, err = fixture.service.ReplaceNodeIP(context.Background(), NodeIPChangeInput{NodeUUID: testNodeUUID, ExpectedIP: target.Address, NewIP: netip.MustParseAddr("1.1.1.1")})
	if err != nil {
		t.Fatalf("ReplaceNodeIP() error = %v", err)
	}
	if fixture.remnawave.nodes[0].Address != "1.1.1.1" {
		t.Fatalf("panel address = %s", fixture.remnawave.nodes[0].Address)
	}
	for _, zone := range fixture.dns.zones {
		if zone.Zone.IPs[0] != "1.1.1.1" {
			t.Fatalf("zone %s = %v", zone.FQDN, zone.Zone.IPs)
		}
	}
}

func TestDeploymentServiceProvisioningFailureStopsBeforeNodeCreation(t *testing.T) {
	fixture := newFixture(t)
	fixture.vps.provisionErr = errors.New("protected provisioner details")
	prepared := fixture.prepare(t, "node-provision-fail", "8.8.4.4")
	err := fixture.service.Deploy(context.Background(), startInput(prepared.Deployment), nil)
	if !errors.Is(err, ErrProvisioningFailed) {
		t.Fatalf("Deploy() error = %v, want ErrProvisioningFailed", err)
	}
	stored := fixture.repo.mustGet(prepared.Deployment.ID)
	if stored.Status != deployment.StatusFailed || fixture.remnawave.createCalls != 0 || fixture.dns.addCalls != 0 {
		t.Fatalf("status=%s create=%d dns=%d", stored.Status, fixture.remnawave.createCalls, fixture.dns.addCalls)
	}
	if stored.SafeErrorMessage == nil || *stored.SafeErrorMessage != "VPS provisioning failed" {
		t.Fatalf("safe error = %#v", stored.SafeErrorMessage)
	}
}

func TestDeploymentServicePreservesOnlyTrustedProvisioningPhase(t *testing.T) {
	fixture := newFixture(t)
	fixture.vps.provisionErr = provisioningPhase("xray-sni adapter could not be initialized")
	prepared := fixture.prepare(t, "phase-failure", "1.1.1.2")
	err := fixture.service.Deploy(context.Background(), startInput(prepared.Deployment), nil)
	if !errors.Is(err, ErrProvisioningFailed) {
		t.Fatalf("Deploy() error = %v, want ErrProvisioningFailed", err)
	}
	stored := fixture.repo.mustGet(prepared.Deployment.ID)
	if stored.SafeErrorMessage == nil || *stored.SafeErrorMessage != "xray-sni adapter could not be initialized" {
		t.Fatalf("safe provisioning message = %#v", stored.SafeErrorMessage)
	}
}

func TestDeploymentServiceClassifiesCertificateIssuanceFailure(t *testing.T) {
	fixture := newFixture(t)
	fixture.cert.err = certmanager.ErrIssuanceFailed
	prepared := fixture.prepare(t, "node-cert-fail", "8.8.4.4")
	err := fixture.service.Deploy(context.Background(), startInput(prepared.Deployment), nil)
	if !errors.Is(err, ErrCertificateUnavailable) {
		t.Fatalf("Deploy() error = %v, want ErrCertificateUnavailable", err)
	}
	stored := fixture.repo.mustGet(prepared.Deployment.ID)
	if stored.SafeErrorCode == nil || *stored.SafeErrorCode != "CERTIFICATE_ISSUANCE_FAILED" {
		t.Fatalf("safe error code = %#v", stored.SafeErrorCode)
	}
	if stored.SafeErrorMessage == nil || *stored.SafeErrorMessage != "Certificate is not ready" {
		t.Fatalf("safe error message = %#v", stored.SafeErrorMessage)
	}
	if fixture.vps.provisionCalls != 0 || fixture.remnawave.createCalls != 0 || fixture.dns.addCalls != 0 {
		t.Fatal("deployment continued after certificate issuance failure")
	}
}

func TestMultiPanelCertificateBootstrapResolvesPanelFromHostSNI(t *testing.T) {
	events := &eventLog{}
	repo := newMemoryRepository()
	mainCert := &fakeCertificateProvider{events: events, readiness: CertificateReady}
	hordaCert := &fakeCertificateProvider{
		events: events, readiness: CertificateReady,
		bootstrapResult: certmanager.BootstrapResult{SNI: "direct-nl.bachidze.com", Version: "v1"},
	}
	newPanelService := func(panelID string, host remnawave.Host, cert *fakeCertificateProvider) *DeploymentService {
		api := &fakeRemnawave{events: events, hosts: []remnawave.Host{host}, secret: "generated-secret"}
		service, err := NewDeploymentService(repo, api, &fakeDNS{events: events}, cert, &fakeVPS{events: events}, Config{PanelID: panelID, MaxConcurrentDeployments: 1})
		if err != nil {
			t.Fatal(err)
		}
		return service
	}
	profileUUID, inboundUUID := testProfileUUID, testInboundUUID
	host := func(address string) remnawave.Host {
		return remnawave.Host{
			UUID: testHostUUID, Remark: address, Address: address,
			Inbound: remnawave.HostInbound{ConfigProfileUUID: &profileUUID, ConfigProfileInboundUUID: &inboundUUID},
		}
	}
	app, err := NewMultiPanelTelegramApplication([]PanelApplicationConfig{
		{ID: "main", Name: "Main", Service: newPanelService("main", host("main.example.com"), mainCert)},
		{ID: "horda", Name: "Horda", Service: newPanelService("horda", host("direct-nl.bachidze.com"), hordaCert)},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := app.BootstrapCertificate(context.Background(), "DIRECT-NL.BACHIDZE.COM.", 42)
	if err != nil {
		t.Fatalf("BootstrapCertificate() error = %v, result = %q", err, result)
	}
	if mainCert.bootstrapCalls != 0 || hordaCert.bootstrapCalls != 1 {
		t.Fatalf("bootstrap calls: main=%d horda=%d", mainCert.bootstrapCalls, hordaCert.bootstrapCalls)
	}
	if !strings.Contains(result, "direct-nl.bachidze.com") {
		t.Fatalf("bootstrap result = %q", result)
	}
}

func TestDeploymentServiceRemnawaveCreationFailure(t *testing.T) {
	fixture := newFixture(t)
	fixture.remnawave.createErr = errors.New("upstream response with protected detail")
	prepared := fixture.prepare(t, "node-create-fail", "1.1.1.1")
	err := fixture.service.Deploy(context.Background(), startInput(prepared.Deployment), nil)
	if !errors.Is(err, ErrRemnawaveCreationFailed) {
		t.Fatalf("Deploy() error = %v, want ErrRemnawaveCreationFailed", err)
	}
	stored := fixture.repo.mustGet(prepared.Deployment.ID)
	if stored.Status != deployment.StatusFailed || fixture.vps.provisionCalls != 1 || fixture.dns.addCalls != 0 {
		t.Fatalf("status=%s provision=%d dns=%d", stored.Status, fixture.vps.provisionCalls, fixture.dns.addCalls)
	}
}

func TestDeploymentServiceNodeNeverConnects(t *testing.T) {
	fixture := newFixture(t)
	fixture.service.config.NodeConnectTimeout = 15 * time.Millisecond
	fixture.service.config.InitialPollBackoff = time.Millisecond
	fixture.service.config.MaxPollBackoff = 2 * time.Millisecond
	fixture.remnawave.alwaysConnecting = true
	prepared := fixture.prepare(t, "node-timeout", "9.9.9.9")
	err := fixture.service.Deploy(context.Background(), startInput(prepared.Deployment), nil)
	if !errors.Is(err, ErrNodeConnectionTimeout) {
		t.Fatalf("Deploy() error = %v, want ErrNodeConnectionTimeout", err)
	}
	stored := fixture.repo.mustGet(prepared.Deployment.ID)
	if stored.Status != deployment.StatusFailed || fixture.dns.addCalls != 0 {
		t.Fatalf("status=%s dns=%d", stored.Status, fixture.dns.addCalls)
	}
}

func TestDeploymentServiceExposesSafeTerminalNodeStatus(t *testing.T) {
	fixture := newFixture(t)
	message := "authentication rejected\nsecond line"
	fixture.remnawave.pollStates = []remnawave.Node{{UUID: testNodeUUID, IsConnected: false, IsConnecting: false, LastStatusMessage: &message}}
	prepared := fixture.prepare(t, "node-rejected", "1.0.0.1")
	err := fixture.service.Deploy(context.Background(), startInput(prepared.Deployment), nil)
	if !errors.Is(err, ErrNodeConnectionFailed) {
		t.Fatalf("Deploy() error = %v", err)
	}
	stored := fixture.repo.mustGet(prepared.Deployment.ID)
	if stored.SafeErrorMessage == nil || *stored.SafeErrorMessage != "authentication rejected second line" {
		t.Fatalf("safe lastStatusMessage = %#v", stored.SafeErrorMessage)
	}
	if fixture.dns.addCalls != 0 {
		t.Fatalf("DNS modified before connection")
	}
}

func TestDeploymentServiceDNSFailureAndRetry(t *testing.T) {
	fixture := newFixture(t)
	fixture.dns.addErr = errors.New("DNS service unavailable")
	prepared := fixture.prepare(t, "node-dns-retry", "208.67.222.222")
	err := fixture.service.Deploy(context.Background(), startInput(prepared.Deployment), nil)
	if !errors.Is(err, ErrDNSUpdateFailed) {
		t.Fatalf("Deploy() error = %v, want ErrDNSUpdateFailed", err)
	}
	stored := fixture.repo.mustGet(prepared.Deployment.ID)
	if stored.Status != deployment.StatusDNSFailed || stored.RemnawaveNodeUUID == nil {
		t.Fatalf("DNS failure deployment = %#v", stored)
	}
	if fixture.remnawave.createCalls != 1 || fixture.vps.provisionCalls != 1 {
		t.Fatalf("create=%d provision=%d", fixture.remnawave.createCalls, fixture.vps.provisionCalls)
	}

	fixture.dns.addErr = nil
	if err := fixture.service.RetryDNS(context.Background(), prepared.Deployment.ID, nil); err != nil {
		t.Fatalf("RetryDNS() error = %v", err)
	}
	stored = fixture.repo.mustGet(prepared.Deployment.ID)
	if stored.Status != deployment.StatusCompleted || fixture.dns.addCalls != 2 {
		t.Fatalf("status=%s DNS calls=%d", stored.Status, fixture.dns.addCalls)
	}
	if fixture.remnawave.createCalls != 1 || fixture.vps.provisionCalls != 1 {
		t.Fatalf("retry recreated resources: create=%d provision=%d", fixture.remnawave.createCalls, fixture.vps.provisionCalls)
	}
}

func TestDeploymentServiceDuplicateInvocation(t *testing.T) {
	fixture := newFixture(t)
	prepared := fixture.prepare(t, "node-duplicate-run", "4.2.2.2")
	fixture.vps.provisionStarted = make(chan struct{})
	fixture.vps.provisionRelease = make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- fixture.service.Deploy(context.Background(), startInput(prepared.Deployment), nil) }()
	<-fixture.vps.provisionStarted
	if err := fixture.service.Deploy(context.Background(), startInput(prepared.Deployment), nil); !errors.Is(err, ErrDeploymentAlreadyRunning) {
		t.Fatalf("duplicate Deploy() error = %v", err)
	}
	close(fixture.vps.provisionRelease)
	if err := <-done; err != nil {
		t.Fatalf("first Deploy() error = %v", err)
	}
	createCalls := fixture.remnawave.createCalls
	provisionCalls := fixture.vps.provisionCalls
	if err := fixture.service.Deploy(context.Background(), startInput(prepared.Deployment), nil); err != nil {
		t.Fatalf("completed duplicate Deploy() error = %v", err)
	}
	if fixture.remnawave.createCalls != createCalls || fixture.vps.provisionCalls != provisionCalls {
		t.Fatalf("completed duplicate repeated side effects")
	}
}

func TestDeploymentServiceBoundsConcurrentDeployments(t *testing.T) {
	fixture := newFixture(t)
	limited := &limitedVPS{started: make(chan struct{}, 2), release: make(chan struct{}, 2)}
	service, err := NewDeploymentService(fixture.repo, fixture.remnawave, fixture.dns, fixture.cert, limited, Config{MaxConcurrentDeployments: 1, NodeConnectTimeout: time.Second, InitialPollBackoff: time.Millisecond, MaxPollBackoff: 2 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	fixture.service = service
	first := fixture.prepare(t, "node-limit-one", "8.26.56.26")
	second := fixture.prepare(t, "node-limit-two", "8.20.247.20")

	results := make(chan error, 2)
	go func() { results <- service.Deploy(context.Background(), startInput(first.Deployment), nil) }()
	<-limited.started
	go func() { results <- service.Deploy(context.Background(), startInput(second.Deployment), nil) }()
	select {
	case <-limited.started:
		t.Fatal("second deployment passed concurrency limit before first released")
	case <-time.After(15 * time.Millisecond):
	}
	limited.release <- struct{}{}
	<-limited.started
	limited.release <- struct{}{}
	for range 2 {
		if err := <-results; err != nil {
			t.Fatalf("Deploy() error = %v", err)
		}
	}
	if limited.maximum() != 1 {
		t.Fatalf("maximum concurrent provisions = %d", limited.maximum())
	}
}

func TestDeploymentServiceHonorsContextCancellation(t *testing.T) {
	fixture := newFixture(t)
	prepared := fixture.prepare(t, "node-cancelled-context", "64.6.64.6")
	fixture.vps.provisionStarted = make(chan struct{})
	fixture.vps.provisionRelease = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- fixture.service.Deploy(ctx, startInput(prepared.Deployment), nil) }()
	<-fixture.vps.provisionStarted
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Deploy() error = %v, want context.Canceled", err)
	}
	stored := fixture.repo.mustGet(prepared.Deployment.ID)
	if stored.Status != deployment.StatusProvisioning {
		t.Fatalf("cancelled status = %s, want resumable PROVISIONING", stored.Status)
	}
	if fixture.remnawave.createCalls != 0 || fixture.dns.addCalls != 0 {
		t.Fatalf("side effects continued after cancellation: create=%d dns=%d", fixture.remnawave.createCalls, fixture.dns.addCalls)
	}
}

type fixture struct {
	service   *DeploymentService
	repo      *memoryRepository
	remnawave *fakeRemnawave
	vps       *fakeVPS
	cert      *fakeCertificateProvider
	dns       *fakeDNS
	events    *eventLog
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	events := &eventLog{}
	profileUUID, inboundUUID := testProfileUUID, testInboundUUID
	remnawaveAPI := &fakeRemnawave{
		events: events,
		hosts: []remnawave.Host{{
			UUID: testHostUUID, Remark: "Test Host", Address: "edge.example.com",
			Inbound: remnawave.HostInbound{ConfigProfileUUID: &profileUUID, ConfigProfileInboundUUID: &inboundUUID},
		}},
		secret: "generated-secret",
		pollStates: []remnawave.Node{
			{UUID: testNodeUUID, IsConnecting: true},
			{UUID: testNodeUUID, IsConnected: true},
		},
	}
	repo := newMemoryRepository()
	vps := &fakeVPS{events: events, preflightResult: provisioner.PreflightResult{SSHConnected: true}}
	cert := &fakeCertificateProvider{events: events, readiness: CertificateReady, material: certificates.Material{FullchainPEM: []byte("certificate"), PrivateKeyPEM: []byte("private-key")}}
	dns := &fakeDNS{events: events}
	service, err := NewDeploymentService(repo, remnawaveAPI, dns, cert, vps, Config{MaxConcurrentDeployments: 2, NodeConnectTimeout: time.Second, InitialPollBackoff: time.Millisecond, MaxPollBackoff: 2 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{service: service, repo: repo, remnawave: remnawaveAPI, vps: vps, cert: cert, dns: dns, events: events}
}

func (f *fixture) prepare(t *testing.T, name, ip string) PreparedDeployment {
	t.Helper()
	prepared, err := f.service.Prepare(context.Background(), PrepareInput{OperatorUserID: 42, HostID: testHostUUID, NodeName: name, VPSIP: netip.MustParseAddr(ip), Password: []byte("temporary-password")})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	return prepared
}

func startInput(item deployment.Deployment) StartInput {
	return StartInput{DeploymentID: item.ID, OperatorUserID: item.TelegramOperatorUserID, HostID: item.SelectedRemnawaveHostUUID, NodeName: item.NodeName, VPSIP: item.TargetVPSIP}
}

type eventLog struct {
	mu     sync.Mutex
	events []string
}

func (l *eventLog) add(event string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, event)
}

func (l *eventLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.events...)
}

func assertBefore(t *testing.T, values []string, first, second string) {
	t.Helper()
	firstIndex, secondIndex := -1, -1
	for index, value := range values {
		if value == first && firstIndex == -1 {
			firstIndex = index
		}
		if value == second && secondIndex == -1 {
			secondIndex = index
		}
	}
	if firstIndex == -1 || secondIndex == -1 || firstIndex >= secondIndex {
		t.Fatalf("event order %q before %q not satisfied: %v", first, second, values)
	}
}

func equalStatuses(left, right []deployment.Status) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

type memoryRepository struct {
	mu       sync.Mutex
	nextID   int
	items    map[string]deployment.Deployment
	steps    map[string]map[string]deployment.Step
	statuses map[string][]deployment.Status
}

func newMemoryRepository() *memoryRepository {
	return &memoryRepository{items: make(map[string]deployment.Deployment), steps: make(map[string]map[string]deployment.Step), statuses: make(map[string][]deployment.Status)}
}

func (r *memoryRepository) CreateDeployment(_ context.Context, params repository.CreateDeploymentParams) (deployment.Deployment, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextID++
	id := fmt.Sprintf("00000000-0000-4000-8000-%012d", r.nextID)
	now := time.Now()
	panelID := params.PanelID
	if panelID == "" {
		panelID = "default"
	}
	item := deployment.Deployment{ID: id, PanelID: panelID, TelegramOperatorUserID: params.TelegramOperatorUserID, SelectedRemnawaveHostUUID: params.SelectedRemnawaveHostUUID, SelectedHostRemark: params.SelectedHostRemark, SNIDomain: params.SNIDomain, NodeName: params.NodeName, TargetVPSIP: params.TargetVPSIP, Status: deployment.StatusCreated, CurrentStep: "created", CreatedAt: now, UpdatedAt: now}
	r.items[id] = item
	return item, nil
}

func (r *memoryRepository) GetDeployment(_ context.Context, id string) (deployment.Deployment, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	item, found := r.items[id]
	if !found {
		return deployment.Deployment{}, repository.ErrNotFound
	}
	return item, nil
}

func (r *memoryRepository) UpdateDeploymentState(_ context.Context, id string, params repository.UpdateDeploymentStateParams) (deployment.Deployment, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	item, found := r.items[id]
	if !found {
		return deployment.Deployment{}, repository.ErrNotFound
	}
	item.Status, item.CurrentStep = params.Status, params.CurrentStep
	item.SafeErrorCode, item.SafeErrorMessage = cloneString(params.SafeErrorCode), cloneString(params.SafeErrorMessage)
	item.UpdatedAt = time.Now()
	r.items[id] = item
	r.statuses[id] = append(r.statuses[id], params.Status)
	return item, nil
}

func (r *memoryRepository) SetRemnawaveNodeUUID(_ context.Context, id, uuid string) (deployment.Deployment, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	item, found := r.items[id]
	if !found {
		return deployment.Deployment{}, repository.ErrNotFound
	}
	item.RemnawaveNodeUUID = stringPtr(uuid)
	r.items[id] = item
	return item, nil
}

func (r *memoryRepository) SetTargetVPSIP(_ context.Context, id string, address netip.Addr) (deployment.Deployment, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	item, found := r.items[id]
	if !found {
		return deployment.Deployment{}, repository.ErrNotFound
	}
	item.TargetVPSIP = address.Unmap()
	r.items[id] = item
	return item, nil
}

func (r *memoryRepository) RecordDeploymentStep(_ context.Context, params repository.RecordStepParams) (deployment.Step, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, found := r.items[params.DeploymentID]; !found {
		return deployment.Step{}, repository.ErrNotFound
	}
	if r.steps[params.DeploymentID] == nil {
		r.steps[params.DeploymentID] = make(map[string]deployment.Step)
	}
	step := deployment.Step{DeploymentID: params.DeploymentID, Name: params.Name, Status: params.Status, SafeSummary: cloneString(params.SafeSummary), ErrorMessage: cloneString(params.ErrorMessage)}
	r.steps[params.DeploymentID][params.Name] = step
	return step, nil
}

func (r *memoryRepository) ListDeploymentSteps(_ context.Context, deploymentID string) ([]deployment.Step, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var result []deployment.Step
	for _, step := range r.steps[deploymentID] {
		result = append(result, step)
	}
	return result, nil
}

func (r *memoryRepository) ListRecentDeployments(context.Context, int) ([]deployment.Deployment, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]deployment.Deployment, 0, len(r.items))
	for _, item := range r.items {
		result = append(result, item)
	}
	return result, nil
}

func (r *memoryRepository) FindUnfinishedDeployments(context.Context, int) ([]deployment.Deployment, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var result []deployment.Deployment
	for _, item := range r.items {
		if !item.Status.Terminal() {
			result = append(result, item)
		}
	}
	return result, nil
}

func (r *memoryRepository) FindDeploymentByPanelNodeUUID(_ context.Context, panelID, nodeUUID string) (deployment.Deployment, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, item := range r.items {
		if (item.PanelID == panelID || (item.PanelID == "" && panelID == "default")) && item.RemnawaveNodeUUID != nil && *item.RemnawaveNodeUUID == nodeUUID {
			return item, nil
		}
	}
	return deployment.Deployment{}, repository.ErrNotFound
}

func (r *memoryRepository) mustGet(id string) deployment.Deployment {
	item, err := r.GetDeployment(context.Background(), id)
	if err != nil {
		panic(err)
	}
	return item
}

func (r *memoryRepository) statusHistory(id string) []deployment.Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]deployment.Status(nil), r.statuses[id]...)
}

func cloneString(value *string) *string {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

type fakeRemnawave struct {
	mu sync.Mutex

	events           *eventLog
	hosts            []remnawave.Host
	nodes            []remnawave.Node
	secret           string
	createErr        error
	pollStates       []remnawave.Node
	pollIndex        int
	alwaysConnecting bool
	createCalls      int
}

func (f *fakeRemnawave) GetHosts(context.Context) ([]remnawave.Host, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]remnawave.Host(nil), f.hosts...), nil
}

func (f *fakeRemnawave) GenerateSecretKey(context.Context) (string, error) {
	f.events.add("remnawave.keygen")
	return f.secret, nil
}

func (f *fakeRemnawave) GetNodes(context.Context) ([]remnawave.Node, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]remnawave.Node(nil), f.nodes...), nil
}

func (f *fakeRemnawave) GetNode(context.Context, string) (remnawave.Node, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.alwaysConnecting {
		return remnawave.Node{UUID: testNodeUUID, IsConnecting: true}, nil
	}
	if len(f.pollStates) > 0 {
		index := f.pollIndex
		if index >= len(f.pollStates) {
			index = len(f.pollStates) - 1
		}
		node := f.pollStates[index]
		f.pollIndex++
		if node.IsConnected {
			f.events.add("remnawave.connected")
		}
		return node, nil
	}
	return remnawave.Node{}, errors.New("Node not found")
}

func (f *fakeRemnawave) CreateNode(_ context.Context, input remnawave.CreateNodeInput) (remnawave.Node, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createCalls++
	f.events.add("remnawave.create")
	if f.createErr != nil {
		return remnawave.Node{}, f.createErr
	}
	node := remnawave.Node{UUID: testNodeUUID, Name: input.Name, Address: input.Address.String(), IsConnecting: true}
	f.nodes = append(f.nodes, node)
	return node, nil
}

func (f *fakeRemnawave) UpdateNodeAddress(_ context.Context, input remnawave.UpdateNodeAddressInput) (remnawave.Node, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for index := range f.nodes {
		if f.nodes[index].UUID == input.UUID {
			f.events.add("remnawave.update-address")
			f.nodes[index].Address = input.Address.String()
			return f.nodes[index], nil
		}
	}
	return remnawave.Node{}, errors.New("Node not found")
}

type fakeVPS struct {
	mu sync.Mutex

	events           *eventLog
	preflightResult  provisioner.PreflightResult
	preflightErr     error
	provisionErr     error
	provisionCalls   int
	secret           []byte
	provisionStarted chan struct{}
	provisionRelease chan struct{}
}

type limitedVPS struct {
	mu      sync.Mutex
	active  int
	maxSeen int
	started chan struct{}
	release chan struct{}
}

func (v *limitedVPS) Preflight(context.Context, PreflightVPSInput) (provisioner.PreflightResult, error) {
	return provisioner.PreflightResult{SSHConnected: true}, nil
}

func (v *limitedVPS) Provision(ctx context.Context, _ ProvisionVPSInput, _ func(provisioner.Report)) error {
	v.mu.Lock()
	v.active++
	if v.active > v.maxSeen {
		v.maxSeen = v.active
	}
	v.mu.Unlock()
	v.started <- struct{}{}
	select {
	case <-v.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	v.mu.Lock()
	v.active--
	v.mu.Unlock()
	return nil
}

func (v *limitedVPS) maximum() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.maxSeen
}

func (f *fakeVPS) Preflight(context.Context, PreflightVPSInput) (provisioner.PreflightResult, error) {
	f.events.add("vps.preflight")
	return f.preflightResult, f.preflightErr
}

func (f *fakeVPS) Provision(ctx context.Context, input ProvisionVPSInput, progress func(provisioner.Report)) error {
	f.mu.Lock()
	f.provisionCalls++
	f.secret = append([]byte(nil), input.RemnanodeSecretKey...)
	started, release := f.provisionStarted, f.provisionRelease
	f.mu.Unlock()
	f.events.add("vps.provision")
	if started != nil {
		close(started)
	}
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if progress != nil {
		progress(provisioner.Report{Name: "system", Status: deployment.StepStatusCompleted, Summary: "configured"})
	}
	return f.provisionErr
}

type fakeCertificateProvider struct {
	events          *eventLog
	readiness       CertificateReadiness
	material        certificates.Material
	err             error
	bootstrapResult certmanager.BootstrapResult
	bootstrapErr    error
	bootstrapCalls  int
}

func (f *fakeCertificateProvider) Readiness(context.Context, string) (CertificateReadiness, error) {
	return f.readiness, f.err
}

func (f *fakeCertificateProvider) Prepare(context.Context, string) (certificates.Material, error) {
	f.events.add("certificate.prepare")
	return f.material.Clone(), f.err
}

func (f *fakeCertificateProvider) Bootstrap(context.Context, string, int64) (certmanager.BootstrapResult, error) {
	f.bootstrapCalls++
	return f.bootstrapResult, f.bootstrapErr
}

type fakeDNS struct {
	mu sync.Mutex

	events   *eventLog
	addErr   error
	addCalls int
	zones    []dnsbalancer.ZoneMatch
}

func (f *fakeDNS) FindZonesByIP(_ context.Context, ip netip.Addr) ([]dnsbalancer.ZoneMatch, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var result []dnsbalancer.ZoneMatch
	for _, zone := range f.zones {
		for _, value := range zone.Zone.IPs {
			parsed, err := netip.ParseAddr(value)
			if err == nil && parsed.Unmap() == ip.Unmap() {
				result = append(result, zone)
				break
			}
		}
	}
	return result, nil
}

func (f *fakeDNS) FindZone(context.Context, string) (dnsbalancer.ZoneMatch, error) {
	return dnsbalancer.ZoneMatch{FQDN: "edge.example.com"}, nil
}

func (f *fakeDNS) AddIP(context.Context, string, netip.Addr) (dnsbalancer.AddIPResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.addCalls++
	f.events.add("dns.add")
	return dnsbalancer.AddIPResult{}, f.addErr
}

func (f *fakeDNS) ReplaceIP(_ context.Context, fqdn string, oldIP, newIP netip.Addr) (dnsbalancer.ReplaceIPResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events.add("dns.replace")
	for zoneIndex := range f.zones {
		if f.zones[zoneIndex].FQDN != fqdn {
			continue
		}
		for ipIndex, value := range f.zones[zoneIndex].Zone.IPs {
			if value == oldIP.String() {
				f.zones[zoneIndex].Zone.IPs[ipIndex] = newIP.String()
			}
		}
	}
	return dnsbalancer.ReplaceIPResult{Changed: true}, f.addErr
}

var _ DeploymentRepository = (*memoryRepository)(nil)
var _ RemnawaveAPI = (*fakeRemnawave)(nil)
var _ VPSOperator = (*fakeVPS)(nil)
var _ VPSOperator = (*limitedVPS)(nil)
var _ CertificateProvider = (*fakeCertificateProvider)(nil)
var _ DNSAPI = (*fakeDNS)(nil)
