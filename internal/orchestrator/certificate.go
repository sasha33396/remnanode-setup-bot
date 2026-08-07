package orchestrator

import (
	"context"
	"strings"
	"sync"

	"remnanode-setup-bot/internal/certificates"
)

// StaticCertificateProvider is an in-memory implementation retained for tests
// and isolated development. Production runtime uses certmanager.Manager.
type StaticCertificateProvider struct {
	mu       sync.RWMutex
	material map[string]certificates.Material
}

func NewStaticCertificateProvider(initial map[string]certificates.Material) *StaticCertificateProvider {
	provider := &StaticCertificateProvider{material: make(map[string]certificates.Material, len(initial))}
	for domain, material := range initial {
		provider.material[normalizeDomain(domain)] = material.Clone()
	}
	return provider
}

func (p *StaticCertificateProvider) Readiness(ctx context.Context, domain string) (CertificateReadiness, error) {
	if err := ctx.Err(); err != nil {
		return CertificateUnknown, err
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	material, found := p.material[normalizeDomain(domain)]
	if !found || len(material.FullchainPEM) == 0 || len(material.PrivateKeyPEM) == 0 {
		return CertificateNotReady, nil
	}
	return CertificateReady, nil
}

func (p *StaticCertificateProvider) Prepare(ctx context.Context, domain string) (certificates.Material, error) {
	if err := ctx.Err(); err != nil {
		return certificates.Material{}, err
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	material, found := p.material[normalizeDomain(domain)]
	if !found || len(material.FullchainPEM) == 0 || len(material.PrivateKeyPEM) == 0 {
		return certificates.Material{}, ErrCertificateUnavailable
	}
	return material.Clone(), nil
}

// Put replaces one temporary in-memory certificate and destroys the old copy.
func (p *StaticCertificateProvider) Put(domain string, material certificates.Material) {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := normalizeDomain(domain)
	if previous, found := p.material[key]; found {
		previous.Destroy()
	}
	p.material[key] = material.Clone()
}

func (p *StaticCertificateProvider) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for domain, material := range p.material {
		material.Destroy()
		delete(p.material, domain)
	}
}

func normalizeDomain(value string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(value), "."))
}

var _ CertificateProvider = (*StaticCertificateProvider)(nil)
