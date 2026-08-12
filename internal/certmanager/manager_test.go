package certmanager

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
	"net/netip"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"remnanode-setup-bot/internal/certificates"
)

const testSNI = "edge.example.com"

func TestManagerCacheHit(t *testing.T) {
	fixture := newManagerFixture(t, []certificates.Material{testMaterial(t, testSNI, time.Now().Add(90*24*time.Hour), nil)})
	first, err := fixture.manager.Prepare(context.Background(), testSNI)
	if err != nil {
		t.Fatal(err)
	}
	first.Destroy()
	second, err := fixture.manager.Prepare(context.Background(), testSNI)
	if err != nil {
		t.Fatal(err)
	}
	second.Destroy()
	if fixture.issuer.calls.Load() != 1 {
		t.Fatalf("issuer calls = %d, want 1", fixture.issuer.calls.Load())
	}
}

func TestManagerInitialIssuance(t *testing.T) {
	fixture := newManagerFixture(t, []certificates.Material{testMaterial(t, testSNI, time.Now().Add(90*24*time.Hour), nil)})
	material, err := fixture.manager.Prepare(context.Background(), testSNI)
	if err != nil {
		t.Fatal(err)
	}
	defer material.Destroy()
	record, err := fixture.repository.GetActive(context.Background(), testSNI)
	if err != nil {
		t.Fatal(err)
	}
	if record.ActiveVersion == "" || record.Status != StatusActive || record.Fingerprint == "" {
		t.Fatalf("active record = %#v", record)
	}
	marker, err := fixture.store.ActiveVersion(context.Background(), testSNI)
	if err != nil || marker != record.ActiveVersion {
		t.Fatalf("active marker = %q, %v", marker, err)
	}
}

func TestManagerPreservesSafeIssuanceReason(t *testing.T) {
	fixture := newManagerFixture(t, nil)
	fixture.issuer.issueErr = safe("Cloudflare API authentication failed (HTTP 401)", ErrIssuanceFailed)
	_, err := fixture.manager.Prepare(context.Background(), testSNI)
	if !errors.Is(err, ErrIssuanceFailed) {
		t.Fatalf("Prepare() error = %v, want ErrIssuanceFailed", err)
	}
	if got, want := SafeMessage(err, "fallback"), "Cloudflare API authentication failed (HTTP 401)"; got != want {
		t.Fatalf("safe message = %q, want %q", got, want)
	}
}

func TestManagerConcurrentIssuanceRequest(t *testing.T) {
	fixture := newManagerFixture(t, []certificates.Material{testMaterial(t, testSNI, time.Now().Add(90*24*time.Hour), nil)})
	fixture.issuer.started = make(chan struct{})
	fixture.issuer.release = make(chan struct{})
	const workers = 12
	errorsSeen := make(chan error, workers)
	for index := 0; index < workers; index++ {
		go func() {
			material, err := fixture.manager.Prepare(context.Background(), testSNI)
			material.Destroy()
			errorsSeen <- err
		}()
	}
	<-fixture.issuer.started
	close(fixture.issuer.release)
	for index := 0; index < workers; index++ {
		if err := <-errorsSeen; err != nil {
			t.Fatal(err)
		}
	}
	if fixture.issuer.calls.Load() != 1 {
		t.Fatalf("issuer calls = %d, want 1", fixture.issuer.calls.Load())
	}
}

func TestManagerRenewal(t *testing.T) {
	fixture := newManagerFixture(t, []certificates.Material{
		testMaterial(t, testSNI, time.Now().Add(2*time.Hour), nil),
		testMaterial(t, testSNI, time.Now().Add(90*24*time.Hour), nil),
	})
	first, err := fixture.manager.Prepare(context.Background(), testSNI)
	if err != nil {
		t.Fatal(err)
	}
	first.Destroy()
	before, _ := fixture.repository.GetActive(context.Background(), testSNI)
	second, err := fixture.manager.Prepare(context.Background(), testSNI)
	if err != nil {
		t.Fatal(err)
	}
	second.Destroy()
	after, _ := fixture.repository.GetActive(context.Background(), testSNI)
	if before.ActiveVersion == after.ActiveVersion || after.LastRenewedAt == nil || fixture.issuer.calls.Load() != 2 {
		t.Fatalf("renewal before=%#v after=%#v calls=%d", before, after, fixture.issuer.calls.Load())
	}
}

func TestManagerRejectsInvalidCertificate(t *testing.T) {
	fixture := newManagerFixture(t, []certificates.Material{{FullchainPEM: []byte("invalid"), PrivateKeyPEM: []byte("invalid")}})
	_, err := fixture.manager.Prepare(context.Background(), testSNI)
	if !errors.Is(err, certificates.ErrInvalidMaterial) {
		t.Fatalf("Prepare() error = %v", err)
	}
	if _, err := fixture.repository.GetActive(context.Background(), testSNI); !errors.Is(err, ErrNotFound) {
		t.Fatalf("active invalid certificate error = %v", err)
	}
}

func TestManagerRejectsMismatchedKey(t *testing.T) {
	firstKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	secondKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	material := testMaterial(t, testSNI, time.Now().Add(90*24*time.Hour), firstKey)
	der, _ := x509.MarshalPKCS8PrivateKey(secondKey)
	material.PrivateKeyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	fixture := newManagerFixture(t, []certificates.Material{material})
	_, err := fixture.manager.Prepare(context.Background(), testSNI)
	if !errors.Is(err, certificates.ErrInvalidMaterial) {
		t.Fatalf("Prepare() error = %v", err)
	}
}

func TestManagerDistributionPartialFailureKeepsCentralActiveVersion(t *testing.T) {
	fixture := newManagerFixture(t, []certificates.Material{
		testMaterial(t, testSNI, time.Now().Add(90*24*time.Hour), nil),
		testMaterial(t, testSNI, time.Now().Add(120*24*time.Hour), nil),
	})
	initial, err := fixture.manager.Prepare(context.Background(), testSNI)
	if err != nil {
		t.Fatal(err)
	}
	initial.Destroy()
	before, _ := fixture.repository.GetActive(context.Background(), testSNI)
	fixture.resolver.targets = []Target{{DeploymentID: "00000000-0000-4000-8000-000000000001", IP: netip.MustParseAddr("203.0.113.10")}, {DeploymentID: "00000000-0000-4000-8000-000000000002", IP: netip.MustParseAddr("203.0.113.11")}}
	fixture.distributor.fail = fixture.resolver.targets[1].IP
	if err := fixture.manager.Renew(context.Background(), testSNI); !errors.Is(err, ErrDistributionFailed) {
		t.Fatalf("Renew() error = %v", err)
	}
	after, _ := fixture.repository.GetActive(context.Background(), testSNI)
	marker, _ := fixture.store.ActiveVersion(context.Background(), testSNI)
	if after.ActiveVersion != before.ActiveVersion || marker != before.ActiveVersion {
		t.Fatalf("active changed after partial failure: before=%q after=%q marker=%q", before.ActiveVersion, after.ActiveVersion, marker)
	}
	if len(fixture.repository.distributions) != 4 {
		t.Fatalf("candidate and compensation distribution records = %d", len(fixture.repository.distributions))
	}
	if fixture.distributor.calls != 2 {
		t.Fatalf("distributor calls = %d, want candidate plus previous-version compensation", fixture.distributor.calls)
	}
	if len(fixture.distributor.fingerprints) != 2 || fixture.distributor.fingerprints[1] != before.Fingerprint {
		t.Fatalf("compensation fingerprints = %#v, want previous %q last", fixture.distributor.fingerprints, before.Fingerprint)
	}
}

func TestManagerRollbackBehavior(t *testing.T) {
	fixture := newManagerFixture(t, []certificates.Material{
		testMaterial(t, testSNI, time.Now().Add(90*24*time.Hour), nil),
		testMaterial(t, testSNI, time.Now().Add(120*24*time.Hour), nil),
	})
	first, _ := fixture.manager.Prepare(context.Background(), testSNI)
	first.Destroy()
	versionOne, _ := fixture.repository.GetActive(context.Background(), testSNI)
	if err := fixture.manager.Renew(context.Background(), testSNI); err != nil {
		t.Fatal(err)
	}
	versionTwo, _ := fixture.repository.GetActive(context.Background(), testSNI)
	if versionOne.ActiveVersion == versionTwo.ActiveVersion {
		t.Fatal("renewal did not create a new version")
	}
	fixture.resolver.targets = []Target{{DeploymentID: "00000000-0000-4000-8000-000000000001", IP: netip.MustParseAddr("203.0.113.10")}}
	if err := fixture.manager.Rollback(context.Background(), testSNI, versionOne.ActiveVersion); err != nil {
		t.Fatal(err)
	}
	rolledBack, _ := fixture.repository.GetActive(context.Background(), testSNI)
	if rolledBack.ActiveVersion != versionOne.ActiveVersion {
		t.Fatalf("active version = %q, want %q", rolledBack.ActiveVersion, versionOne.ActiveVersion)
	}
}

type managerFixture struct {
	manager     *Manager
	repository  *memoryCertificateRepository
	store       *FileStore
	issuer      *fakeIssuer
	resolver    *fakeResolver
	distributor *fakeDistributor
}

func newManagerFixture(t *testing.T, materials []certificates.Material) *managerFixture {
	t.Helper()
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repository := newMemoryCertificateRepository()
	issuer := &fakeIssuer{materials: materials}
	resolver := &fakeResolver{}
	distributor := &fakeDistributor{}
	manager, err := New(repository, store, issuer, NewMemoryLocker(), resolver, distributor, nil, Config{RenewBefore: 24 * time.Hour, IssueTimeout: time.Minute, RenewInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	return &managerFixture{manager: manager, repository: repository, store: store, issuer: issuer, resolver: resolver, distributor: distributor}
}

type fakeIssuer struct {
	mu        sync.Mutex
	materials []certificates.Material
	calls     atomic.Int64
	started   chan struct{}
	release   chan struct{}
	issueErr  error
}

func (i *fakeIssuer) Issue(ctx context.Context, _ string) (certificates.Material, error) {
	call := int(i.calls.Add(1))
	if i.started != nil && call == 1 {
		close(i.started)
	}
	if i.release != nil && call == 1 {
		select {
		case <-i.release:
		case <-ctx.Done():
			return certificates.Material{}, ctx.Err()
		}
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.issueErr != nil {
		return certificates.Material{}, i.issueErr
	}
	if call > len(i.materials) {
		return certificates.Material{}, errors.New("unexpected issuance")
	}
	return i.materials[call-1].Clone(), nil
}

type fakeResolver struct {
	targets   []Target
	unmanaged []netip.Addr
	legacy    []netip.Addr
	err       error
}

func TestManagerBootstrapAcknowledgesLegacyAndActivatesStagedCertificate(t *testing.T) {
	fixture := newManagerFixture(t, []certificates.Material{testMaterial(t, testSNI, time.Now().Add(90*24*time.Hour), nil)})
	legacyIP := netip.MustParseAddr("203.0.113.50")
	fixture.resolver.unmanaged = []netip.Addr{legacyIP}

	material, err := fixture.manager.Prepare(context.Background(), testSNI)
	material.Destroy()
	if !errors.Is(err, ErrDistributionFailed) {
		t.Fatalf("Prepare() error = %v, want distribution failure", err)
	}
	if fixture.issuer.calls.Load() != 1 {
		t.Fatalf("issuer calls = %d, want 1", fixture.issuer.calls.Load())
	}

	result, err := fixture.manager.Bootstrap(context.Background(), testSNI, 42)
	if err != nil {
		t.Fatalf("Bootstrap() error = %v", err)
	}
	if result.AcknowledgedLegacyIPs != 1 || result.Version == "" {
		t.Fatalf("Bootstrap() result = %#v", result)
	}
	if fixture.issuer.calls.Load() != 1 {
		t.Fatalf("bootstrap created a new ACME order; issuer calls = %d", fixture.issuer.calls.Load())
	}
	active, err := fixture.repository.GetActive(context.Background(), testSNI)
	if err != nil || active.ActiveVersion != result.Version || active.Status != StatusActive {
		t.Fatalf("active certificate = %#v, %v", active, err)
	}
	reviews, _ := fixture.repository.ListTargetReviews(context.Background(), testSNI)
	if len(reviews) != 1 || reviews[0].State != TargetLegacyAcknowledged || reviews[0].AcknowledgedBy == nil || *reviews[0].AcknowledgedBy != 42 {
		t.Fatalf("target reviews = %#v", reviews)
	}
}

func (r *fakeResolver) Resolve(context.Context, string) (TargetResolution, error) {
	return TargetResolution{
		Managed:            append([]Target(nil), r.targets...),
		Unmanaged:          append([]netip.Addr(nil), r.unmanaged...),
		LegacyAcknowledged: append([]netip.Addr(nil), r.legacy...),
	}, r.err
}

type fakeDistributor struct {
	fail         netip.Addr
	calls        int
	fingerprints []string
}

func (d *fakeDistributor) Distribute(_ context.Context, sni string, material certificates.Material, targets []Target) []DistributionResult {
	d.calls++
	if info, err := certificates.Validate(material, sni, time.Now()); err == nil {
		d.fingerprints = append(d.fingerprints, info.Fingerprint)
	}
	results := make([]DistributionResult, len(targets))
	for index, target := range targets {
		results[index] = DistributionResult{Target: target, Status: DistributionSucceeded, SafeMessage: "activated"}
		if d.fail.IsValid() && d.fail == target.IP {
			results[index].Status = DistributionFailed
			results[index].SafeMessage = "activation failed"
		}
	}
	return results
}

type memoryCertificateRepository struct {
	mu            sync.Mutex
	records       map[string]Record
	versions      map[string]map[string]Version
	distributions []DistributionRecord
	reviews       map[string]map[netip.Addr]TargetReview
}

func newMemoryCertificateRepository() *memoryCertificateRepository {
	return &memoryCertificateRepository{records: make(map[string]Record), versions: make(map[string]map[string]Version), reviews: make(map[string]map[netip.Addr]TargetReview)}
}
func (r *memoryCertificateRepository) GetActive(_ context.Context, sni string) (Record, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	value, ok := r.records[sni]
	if !ok || value.ActiveVersion == "" {
		return Record{}, ErrNotFound
	}
	return value, nil
}
func (r *memoryCertificateRepository) SaveVersion(_ context.Context, version Version) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.versions[version.SNI] == nil {
		r.versions[version.SNI] = make(map[string]Version)
	}
	r.versions[version.SNI][version.Version] = version
	record := r.records[version.SNI]
	record.SNI, record.Status = version.SNI, StatusIssuing
	r.records[version.SNI] = record
	return nil
}
func (r *memoryCertificateRepository) SetVersionStatus(_ context.Context, sni, version string, status VersionStatus) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.versions[sni][version]
	if !ok {
		return ErrNotFound
	}
	item.Status = status
	r.versions[sni][version] = item
	return nil
}
func (r *memoryCertificateRepository) ActivateVersion(_ context.Context, sni, version string, renewed bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	selected, ok := r.versions[sni][version]
	if !ok {
		return ErrNotFound
	}
	for key, item := range r.versions[sni] {
		if item.Status == VersionActive {
			item.Status = VersionSuperseded
		}
		if key == version {
			item.Status = VersionActive
		}
		r.versions[sni][key] = item
	}
	record := r.records[sni]
	record.SNI, record.Fingerprint, record.Serial, record.IssuedAt, record.ExpiresAt, record.Status, record.ActiveVersion = sni, selected.Fingerprint, selected.Serial, selected.IssuedAt, selected.ExpiresAt, StatusActive, version
	if renewed {
		now := time.Now()
		record.LastRenewedAt = &now
	}
	r.records[sni] = record
	return nil
}
func (r *memoryCertificateRepository) SetStatus(_ context.Context, sni string, status Status) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	record := r.records[sni]
	record.SNI, record.Status = sni, status
	r.records[sni] = record
	return nil
}
func (r *memoryCertificateRepository) RecordDistribution(_ context.Context, value DistributionRecord) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.distributions = append(r.distributions, value)
	return nil
}
func (r *memoryCertificateRepository) ListExpiring(_ context.Context, before time.Time, limit int) ([]Record, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var result []Record
	for _, record := range r.records {
		if record.ActiveVersion != "" && !record.ExpiresAt.After(before) {
			result = append(result, record)
			if len(result) == limit {
				break
			}
		}
	}
	return result, nil
}
func (r *memoryCertificateRepository) ListVersions(_ context.Context, sni string) ([]Version, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var result []Version
	for _, version := range r.versions[sni] {
		result = append(result, version)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CreatedAt.After(result[j].CreatedAt) })
	return result, nil
}
func (r *memoryCertificateRepository) RecordTargetReview(_ context.Context, review TargetReview) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.reviews[review.SNI] == nil {
		r.reviews[review.SNI] = make(map[netip.Addr]TargetReview)
	}
	r.reviews[review.SNI][review.IP.Unmap()] = review
	return nil
}
func (r *memoryCertificateRepository) ListTargetReviews(_ context.Context, sni string) ([]TargetReview, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]TargetReview, 0, len(r.reviews[sni]))
	for _, review := range r.reviews[sni] {
		result = append(result, review)
	}
	return result, nil
}

func testMaterial(t *testing.T, domain string, expires time.Time, key *ecdsa.PrivateKey) certificates.Material {
	t.Helper()
	if key == nil {
		key, _ = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: domain}, DNSNames: []string{domain}, NotBefore: time.Now().Add(-time.Hour), NotAfter: expires, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return certificates.Material{FullchainPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), PrivateKeyPEM: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER})}
}

var _ Repository = (*memoryCertificateRepository)(nil)
