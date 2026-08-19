package certmanager

import (
	"context"
	"errors"

	"remnanode-setup-bot/internal/certificates"
)

// FallbackStore writes all new versions to primary but can read pre-panel
// versions from legacy. It provides a non-destructive upgrade path for the
// default panel's existing certificate_store layout.
type FallbackStore struct {
	primary Store
	legacy  Store
}

func NewFallbackStore(primary, legacy Store) (*FallbackStore, error) {
	if primary == nil || legacy == nil {
		return nil, ErrInvalidInput
	}
	return &FallbackStore{primary: primary, legacy: legacy}, nil
}

func (s *FallbackStore) Stage(ctx context.Context, domain string, material certificates.Material) (string, error) {
	return s.primary.Stage(ctx, domain, material)
}

func (s *FallbackStore) Load(ctx context.Context, domain, version string) (certificates.Material, error) {
	value, err := s.primary.Load(ctx, domain, version)
	if err == nil {
		return value, nil
	}
	if !errors.Is(err, ErrStorageFailed) && !errors.Is(err, ErrNoUsableCertificate) {
		return certificates.Material{}, err
	}
	return s.legacy.Load(ctx, domain, version)
}

func (s *FallbackStore) ActiveVersion(ctx context.Context, domain string) (string, error) {
	value, err := s.primary.ActiveVersion(ctx, domain)
	if err == nil {
		return value, nil
	}
	return s.legacy.ActiveVersion(ctx, domain)
}

func (s *FallbackStore) Activate(ctx context.Context, domain, version string) error {
	return s.primary.Activate(ctx, domain, version)
}

var _ Store = (*FallbackStore)(nil)
