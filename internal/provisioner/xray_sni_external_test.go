package provisioner

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"remnanode-setup-bot/internal/certificates"
	sshclient "remnanode-setup-bot/internal/ssh"
)

const testSNIDomain = "sni.example.com"

type fakeXrayRunner struct {
	state             xraySNIState
	requests          []sshclient.CommandRequest
	failMarker        string
	stagedFullchain   bool
	stagedPrivateKey  bool
	activationCount   int
	repositoryCount   int
	composeBuildCount int
	composeUpCount    int
	reloadCount       int
	rollbackCount     int
	managedWriteCount int
	activeCertificate string
}

func (r *fakeXrayRunner) Run(_ context.Context, request sshclient.CommandRequest) (sshclient.CommandResult, error) {
	r.requests = append(r.requests, request)
	if r.failMarker != "" && strings.Contains(request.Command, r.failMarker) {
		return sshclient.CommandResult{ExitStatus: 1}, errors.New("remote failure containing top-secret-private-key")
	}
	switch {
	case strings.Contains(request.Command, "# xray-sni:inspect"):
		return sshclient.CommandResult{Stdout: stateOutput(r.state)}, nil
	case strings.Contains(request.Command, "# xray-sni:repository"):
		r.repositoryCount++
		r.state.RepositoryExists = true
		r.state.RemoteMatches = true
		r.state.RefMatches = true
		r.state.WorktreeClean = true
		r.state.ComposeExists = true
	case strings.Contains(request.Command, "target='/opt/xray-sni/.env'"):
		r.managedWriteCount++
		r.state.EnvironmentMatches = true
	case strings.Contains(request.Command, "target='/opt/xray-sni/certs/fullchain.pem.new'"):
		r.managedWriteCount++
		r.stagedFullchain = true
	case strings.Contains(request.Command, "target='/opt/xray-sni/certs/privkey.pem.new'"):
		r.managedWriteCount++
		r.stagedPrivateKey = true
	case strings.Contains(request.Command, "# xray-sni:activate-certificates"):
		if !r.stagedFullchain || !r.stagedPrivateKey {
			return sshclient.CommandResult{ExitStatus: 1}, errors.New("missing staged material")
		}
		r.activationCount++
		r.activeCertificate = "new"
		r.state.FullchainMatches = true
		r.state.PrivateKeyMatches = true
		r.state.PermissionsMatch = true
	case strings.Contains(request.Command, "# xray-sni:rollback-certificates"):
		r.rollbackCount++
		r.activeCertificate = "old"
		r.state.FullchainMatches = false
		r.state.PrivateKeyMatches = false
	case strings.Contains(request.Command, "# xray-sni:certificate-permissions"):
		r.state.PermissionsMatch = true
	case strings.Contains(request.Command, "# xray-sni:compose-build"):
		r.composeBuildCount++
		r.state.ContainerExists = true
		r.state.ContainerRunning = true
	case strings.Contains(request.Command, "# xray-sni:record-deployed-commit"):
		r.state.DeployedCommitMatches = true
	case strings.Contains(request.Command, "# xray-sni:compose-up"):
		r.composeUpCount++
		r.state.ContainerExists = true
		r.state.ContainerRunning = true
	case strings.Contains(request.Command, "# xray-sni:reload"):
		r.reloadCount++
	}
	return sshclient.CommandResult{}, nil
}

func stateOutput(state xraySNIState) string {
	return strings.Join([]string{
		"repository_exists=" + boolText(state.RepositoryExists),
		"remote_matches=" + boolText(state.RemoteMatches),
		"ref_matches=" + boolText(state.RefMatches),
		"worktree_clean=" + boolText(state.WorktreeClean),
		"compose_exists=" + boolText(state.ComposeExists),
		"deployed_commit_matches=" + boolText(state.DeployedCommitMatches),
		"environment_matches=" + boolText(state.EnvironmentMatches),
		"fullchain_matches=" + boolText(state.FullchainMatches),
		"private_key_matches=" + boolText(state.PrivateKeyMatches),
		"permissions_match=" + boolText(state.PermissionsMatch),
		"container_exists=" + boolText(state.ContainerExists),
		"container_running=" + boolText(state.ContainerRunning),
	}, "\n")
}

func boolText(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

func readyXrayState() xraySNIState {
	return xraySNIState{
		RepositoryExists: true, RemoteMatches: true, RefMatches: true, WorktreeClean: true,
		ComposeExists: true, DeployedCommitMatches: true, EnvironmentMatches: true,
		FullchainMatches: true, PrivateKeyMatches: true, PermissionsMatch: true,
		ContainerExists: true, ContainerRunning: true,
	}
}

func newTestXrayAdapter(t *testing.T, runner sshclient.CommandRunner, material certificates.Material) *ExternalXraySNIInstaller {
	t.Helper()
	adapter, err := NewExternalXraySNIInstaller(runner, ExternalXraySNIConfig{
		RepositoryURL: "https://github.com/sasha33396/sni-external.git",
		Ref:           "v0.1.0-external",
		SNIDomain:     testSNIDomain,
		Timeout:       time.Second,
	}, material)
	if err != nil {
		t.Fatalf("NewExternalXraySNIInstaller() error = %v", err)
	}
	t.Cleanup(adapter.Destroy)
	return adapter
}

func TestExternalXraySNIFreshInstallationAndSecondRun(t *testing.T) {
	runner := &fakeXrayRunner{}
	adapter := newTestXrayAdapter(t, runner, makeCertificateMaterial(t, testSNIDomain, time.Now().Add(-time.Hour), time.Now().Add(time.Hour)))
	stage := xraySNIStage{installer: adapter}

	first, err := runStage(context.Background(), stage)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Changed || runner.repositoryCount != 1 || runner.composeBuildCount != 1 || runner.activationCount != 1 {
		t.Fatalf("fresh install report=%#v repo=%d build=%d activation=%d", first, runner.repositoryCount, runner.composeBuildCount, runner.activationCount)
	}
	mutations := runner.repositoryCount + runner.composeBuildCount + runner.composeUpCount + runner.reloadCount + runner.managedWriteCount + runner.activationCount
	second, err := runStage(context.Background(), stage)
	if err != nil {
		t.Fatal(err)
	}
	after := runner.repositoryCount + runner.composeBuildCount + runner.composeUpCount + runner.reloadCount + runner.managedWriteCount + runner.activationCount
	if second.Changed || after != mutations {
		t.Fatalf("second run changed=%t, mutations before=%d after=%d", second.Changed, mutations, after)
	}
}

func TestExternalXraySNIInspectionDetectsRepairStates(t *testing.T) {
	material := makeCertificateMaterial(t, testSNIDomain, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	tests := []struct {
		name  string
		state xraySNIState
	}{
		{"not installed", xraySNIState{}},
		{"wrong repository", func() xraySNIState { value := readyXrayState(); value.RemoteMatches = false; return value }()},
		{"wrong ref", func() xraySNIState { value := readyXrayState(); value.RefMatches = false; return value }()},
		{"dirty repository", func() xraySNIState { value := readyXrayState(); value.WorktreeClean = false; return value }()},
		{"build not completed", func() xraySNIState { value := readyXrayState(); value.DeployedCommitMatches = false; return value }()},
		{"wrong SNI", func() xraySNIState { value := readyXrayState(); value.EnvironmentMatches = false; return value }()},
		{"wrong TLS mode", func() xraySNIState { value := readyXrayState(); value.EnvironmentMatches = false; return value }()},
		{"missing certificate", func() xraySNIState { value := readyXrayState(); value.FullchainMatches = false; return value }()},
		{"missing container", func() xraySNIState {
			value := readyXrayState()
			value.ContainerExists = false
			value.ContainerRunning = false
			return value
		}()},
		{"stopped container", func() xraySNIState { value := readyXrayState(); value.ContainerRunning = false; return value }()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			adapter := newTestXrayAdapter(t, &fakeXrayRunner{state: test.state}, material)
			inspection, err := adapter.Inspect(context.Background())
			if err != nil || inspection.Satisfied {
				t.Fatalf("Inspect() = %#v, %v", inspection, err)
			}
		})
	}
}

func TestExternalXraySNIRepositoryAlreadyExists(t *testing.T) {
	state := readyXrayState()
	state.EnvironmentMatches = false
	runner := &fakeXrayRunner{state: state}
	adapter := newTestXrayAdapter(t, runner, makeCertificateMaterial(t, testSNIDomain, time.Now().Add(-time.Hour), time.Now().Add(time.Hour)))
	if err := adapter.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runner.repositoryCount != 0 || runner.composeUpCount != 1 || runner.composeBuildCount != 0 {
		t.Fatalf("repo=%d up=%d build=%d", runner.repositoryCount, runner.composeUpCount, runner.composeBuildCount)
	}
}

func TestExternalXraySNIRepairsWrongRepositoryAndRef(t *testing.T) {
	state := readyXrayState()
	state.RemoteMatches = false
	state.RefMatches = false
	runner := &fakeXrayRunner{state: state}
	adapter := newTestXrayAdapter(t, runner, makeCertificateMaterial(t, testSNIDomain, time.Now().Add(-time.Hour), time.Now().Add(time.Hour)))
	if err := adapter.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runner.repositoryCount != 1 || runner.composeBuildCount != 1 {
		t.Fatalf("repository repairs=%d builds=%d", runner.repositoryCount, runner.composeBuildCount)
	}
}

func TestExternalXraySNIRepairsPermissionsWithoutReplacingCertificate(t *testing.T) {
	state := readyXrayState()
	state.PermissionsMatch = false
	runner := &fakeXrayRunner{state: state, activeCertificate: "old"}
	adapter := newTestXrayAdapter(t, runner, makeCertificateMaterial(t, testSNIDomain, time.Now().Add(-time.Hour), time.Now().Add(time.Hour)))
	if err := adapter.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runner.activationCount != 0 || runner.reloadCount != 0 || runner.managedWriteCount != 0 || runner.activeCertificate != "old" {
		t.Fatalf("activation=%d reload=%d writes=%d active=%q", runner.activationCount, runner.reloadCount, runner.managedWriteCount, runner.activeCertificate)
	}
}

func TestExternalXraySNIRetriesBuildUntilCommitMarkerIsRecorded(t *testing.T) {
	state := readyXrayState()
	state.DeployedCommitMatches = false
	runner := &fakeXrayRunner{state: state, failMarker: "# xray-sni:compose-build"}
	adapter := newTestXrayAdapter(t, runner, makeCertificateMaterial(t, testSNIDomain, time.Now().Add(-time.Hour), time.Now().Add(time.Hour)))
	if err := adapter.Install(context.Background()); !errors.Is(err, ErrXraySNIInstallationFailed) {
		t.Fatalf("first Install() error = %v", err)
	}
	if runner.state.DeployedCommitMatches {
		t.Fatal("failed build recorded a deployed commit")
	}
	runner.failMarker = ""
	if err := adapter.Install(context.Background()); err != nil {
		t.Fatalf("retry Install() error = %v", err)
	}
	if runner.composeBuildCount != 1 || !runner.state.DeployedCommitMatches {
		t.Fatalf("build count=%d marker=%t", runner.composeBuildCount, runner.state.DeployedCommitMatches)
	}
}

func TestExternalXraySNICertificateUpdateUsesReload(t *testing.T) {
	state := readyXrayState()
	state.FullchainMatches = false
	state.PrivateKeyMatches = false
	runner := &fakeXrayRunner{state: state, activeCertificate: "old"}
	adapter := newTestXrayAdapter(t, runner, makeCertificateMaterial(t, testSNIDomain, time.Now().Add(-time.Hour), time.Now().Add(time.Hour)))
	if err := adapter.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runner.activationCount != 1 || runner.reloadCount != 1 || runner.composeUpCount != 0 || runner.composeBuildCount != 0 {
		t.Fatalf("activation=%d reload=%d up=%d build=%d", runner.activationCount, runner.reloadCount, runner.composeUpCount, runner.composeBuildCount)
	}
}

func TestExternalXraySNIFailureBeforeActivationKeepsOldCertificate(t *testing.T) {
	state := readyXrayState()
	state.FullchainMatches = false
	state.PrivateKeyMatches = false
	runner := &fakeXrayRunner{state: state, activeCertificate: "old", failMarker: "target='/opt/xray-sni/certs/privkey.pem.new'"}
	adapter := newTestXrayAdapter(t, runner, makeCertificateMaterial(t, testSNIDomain, time.Now().Add(-time.Hour), time.Now().Add(time.Hour)))
	err := adapter.Install(context.Background())
	if !errors.Is(err, ErrXraySNIInstallationFailed) || runner.activeCertificate != "old" || runner.activationCount != 0 {
		t.Fatalf("Install() error=%v active=%q activation=%d", err, runner.activeCertificate, runner.activationCount)
	}
	if strings.Contains(err.Error(), "top-secret-private-key") {
		t.Fatal("private key leaked through returned error")
	}
}

func TestExternalXraySNICertificateHealthFailureRollsBack(t *testing.T) {
	state := readyXrayState()
	state.FullchainMatches = false
	state.PrivateKeyMatches = false
	runner := &fakeXrayRunner{state: state, activeCertificate: "old", failMarker: "# xray-sni:healthcheck"}
	adapter := newTestXrayAdapter(t, runner, makeCertificateMaterial(t, testSNIDomain, time.Now().Add(-time.Hour), time.Now().Add(time.Hour)))
	err := adapter.UpdateCertificate(context.Background())
	if !errors.Is(err, ErrXraySNIValidationFailed) {
		t.Fatalf("UpdateCertificate() error = %v", err)
	}
	if runner.activeCertificate != "old" || runner.rollbackCount != 1 || runner.reloadCount != 2 {
		t.Fatalf("active=%q rollback=%d reload=%d", runner.activeCertificate, runner.rollbackCount, runner.reloadCount)
	}
}

func TestExternalXraySNICertificateUpdateResumesAfterFileSwitch(t *testing.T) {
	state := readyXrayState()
	runner := &fakeXrayRunner{state: state, activeCertificate: "new"}
	adapter := newTestXrayAdapter(t, runner, makeCertificateMaterial(t, testSNIDomain, time.Now().Add(-time.Hour), time.Now().Add(time.Hour)))
	if err := adapter.UpdateCertificate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runner.activationCount != 0 || runner.reloadCount != 1 || runner.rollbackCount != 0 {
		t.Fatalf("activation=%d reload=%d rollback=%d", runner.activationCount, runner.reloadCount, runner.rollbackCount)
	}
}

func TestExternalXraySNIRuntimeFailures(t *testing.T) {
	material := makeCertificateMaterial(t, testSNIDomain, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	for _, marker := range []string{"# xray-sni:compose-config", "# xray-sni:container", "# xray-sni:caddy-validate", "# xray-sni:healthcheck"} {
		t.Run(marker, func(t *testing.T) {
			runner := &fakeXrayRunner{state: readyXrayState(), failMarker: marker}
			adapter := newTestXrayAdapter(t, runner, material)
			if err := adapter.Validate(context.Background()); !errors.Is(err, ErrXraySNIValidationFailed) {
				t.Fatalf("Validate() error = %v", err)
			}
		})
	}
	runner := &fakeXrayRunner{state: readyXrayState()}
	adapter := newTestXrayAdapter(t, runner, material)
	if err := adapter.Validate(context.Background()); err != nil {
		t.Fatalf("successful Validate() error = %v", err)
	}
}

func TestExternalXraySNIUsesPinnedRepositoryWithoutSecrets(t *testing.T) {
	runner := &fakeXrayRunner{}
	material := makeCertificateMaterial(t, testSNIDomain, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	adapter := newTestXrayAdapter(t, runner, material)
	if err := adapter.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	privateKey := string(material.PrivateKeyPEM)
	var allCommands strings.Builder
	for _, request := range runner.requests {
		allCommands.WriteString(request.Command)
		if strings.Contains(request.Command, privateKey) {
			t.Fatal("private key appeared in an SSH command")
		}
	}
	commands := allCommands.String()
	for _, required := range []string{"https://github.com/sasha33396/sni-external.git", "v0.1.0-external", "git clone --no-checkout"} {
		if !strings.Contains(commands, required) {
			t.Fatalf("commands do not contain %q", required)
		}
	}
	for _, forbidden := range []string{"CF_API_TOKEN", "checkout main", "checkout master"} {
		if strings.Contains(commands, forbidden) {
			t.Fatalf("commands contain forbidden value %q", forbidden)
		}
	}
	if strings.Contains(string(adapter.environment()), "CF_API_TOKEN") {
		t.Fatal("CF_API_TOKEN appeared in the managed environment")
	}
}

func TestCertificateMaterialValidation(t *testing.T) {
	now := time.Now()
	valid := makeCertificateMaterial(t, testSNIDomain, now.Add(-time.Hour), now.Add(time.Hour))
	other := makeCertificateMaterial(t, testSNIDomain, now.Add(-time.Hour), now.Add(time.Hour))
	tests := []struct {
		name     string
		material certificates.Material
		domain   string
	}{
		{"valid", valid, testSNIDomain},
		{"invalid certificate PEM", certificates.Material{FullchainPEM: []byte("invalid"), PrivateKeyPEM: valid.PrivateKeyPEM}, testSNIDomain},
		{"invalid private key PEM", certificates.Material{FullchainPEM: valid.FullchainPEM, PrivateKeyPEM: []byte("invalid")}, testSNIDomain},
		{"mismatched key", certificates.Material{FullchainPEM: valid.FullchainPEM, PrivateKeyPEM: other.PrivateKeyPEM}, testSNIDomain},
		{"wrong hostname", valid, "wrong.example.com"},
		{"expired", makeCertificateMaterial(t, testSNIDomain, now.Add(-2*time.Hour), now.Add(-time.Hour)), testSNIDomain},
		{"not yet valid", makeCertificateMaterial(t, testSNIDomain, now.Add(time.Hour), now.Add(2*time.Hour)), testSNIDomain},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateCertificateMaterial(test.material, test.domain, now)
			if test.name == "valid" && err != nil {
				t.Fatalf("validation error = %v", err)
			}
			if test.name != "valid" && !errors.Is(err, ErrInvalidCertificateMaterial) {
				t.Fatalf("validation error = %v", err)
			}
		})
	}
}

func TestExternalXraySNIRejectsUnpinnedRef(t *testing.T) {
	material := makeCertificateMaterial(t, testSNIDomain, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	_, err := NewExternalXraySNIInstaller(&fakeXrayRunner{}, ExternalXraySNIConfig{
		RepositoryURL: "https://github.com/sasha33396/sni-external.git",
		Ref:           "main",
		SNIDomain:     testSNIDomain,
	}, material)
	if !errors.Is(err, ErrInvalidXraySNIConfiguration) {
		t.Fatalf("constructor error = %v", err)
	}
}

func TestExternalXraySNIPrivateKeyIsNotPersisted(t *testing.T) {
	material := makeCertificateMaterial(t, testSNIDomain, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	runner := &fakeXrayRunner{state: readyXrayState(), failMarker: "# xray-sni:caddy-validate"}
	adapter := newTestXrayAdapter(t, runner, material)
	var events []string
	stages, _ := fakeStages(&events)
	stages[10] = xraySNIStage{installer: adapter}
	for index := 0; index < 10; index++ {
		stages[index].(*fakeStage).satisfied = true
	}
	store := newMemoryStepStore()
	engine, err := NewEngine(store, stages)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Run(context.Background(), "deployment"); err == nil {
		t.Fatal("Engine.Run() error = nil")
	}
	privateKey := string(material.PrivateKeyPEM)
	for _, step := range store.steps {
		for _, value := range []*string{step.SafeSummary, step.ErrorMessage} {
			if value != nil && strings.Contains(*value, privateKey) {
				t.Fatal("private key appeared in persisted step output")
			}
		}
	}
}

func makeCertificateMaterial(t *testing.T, hostname string, notBefore, notAfter time.Time) certificates.Material {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: hostname},
		DNSNames:     []string{hostname},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return certificates.Material{
		FullchainPEM:  pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		PrivateKeyPEM: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER}),
	}
}
