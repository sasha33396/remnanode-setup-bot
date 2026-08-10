package certmanager

import (
	"context"
	"errors"
	"strings"
	"time"

	"remnanode-setup-bot/internal/certificates"
)

type Manager struct {
	repository  Repository
	store       Store
	issuer      Issuer
	locker      Locker
	resolver    TargetResolver
	distributor Distributor
	observer    Observer
	config      Config
	now         func() time.Time
}

func New(repository Repository, store Store, issuer Issuer, locker Locker, resolver TargetResolver, distributor Distributor, observer Observer, config Config) (*Manager, error) {
	if repository == nil || store == nil || issuer == nil {
		return nil, ErrInvalidInput
	}
	if locker == nil {
		locker = NewMemoryLocker()
	}
	if config.RenewBefore <= 0 {
		config.RenewBefore = 30 * 24 * time.Hour
	}
	if config.IssueTimeout <= 0 {
		config.IssueTimeout = 10 * time.Minute
	}
	if config.RenewInterval <= 0 {
		config.RenewInterval = 12 * time.Hour
	}
	if config.RenewBatchSize <= 0 {
		config.RenewBatchSize = 50
	}
	if config.RenewBefore >= 365*24*time.Hour || config.RenewBatchSize > 100 {
		return nil, ErrInvalidInput
	}
	return &Manager{
		repository: repository, store: store, issuer: issuer, locker: locker,
		resolver: resolver, distributor: distributor, observer: observer,
		config: config, now: time.Now,
	}, nil
}

// Readiness checks only the centrally active certificate and never starts an
// ACME order. Prepare performs issuance when no active certificate exists.
func (m *Manager) Readiness(ctx context.Context, sni string) (certificates.Readiness, error) {
	domain, err := canonicalSNI(sni)
	if err != nil {
		return certificates.ReadinessUnknown, err
	}
	record, material, err := m.loadActive(ctx, domain, false)
	if err != nil {
		if errors.Is(err, ErrNotFound) || errors.Is(err, certificates.ErrInvalidMaterial) {
			return certificates.ReadinessNotReady, nil
		}
		return certificates.ReadinessUnknown, err
	}
	defer material.Destroy()
	m.observeExpiry(record)
	return certificates.ReadinessReady, nil
}

// Prepare returns a validated in-memory copy. A missing certificate is issued;
// a valid certificate nearing expiration is renewed opportunistically. If an
// attempted renewal fails, the still-valid active version remains usable.
func (m *Manager) Prepare(ctx context.Context, sni string) (certificates.Material, error) {
	domain, err := canonicalSNI(sni)
	if err != nil {
		return certificates.Material{}, err
	}
	unlock, err := m.locker.Lock(ctx, domain)
	if err != nil {
		return certificates.Material{}, err
	}
	defer unlock()

	record, active, activeErr := m.loadActive(ctx, domain, true)
	if activeErr == nil {
		m.observeExpiry(record)
		if record.ExpiresAt.Sub(m.now()) > m.config.RenewBefore {
			return active, nil
		}
		if renewed, err := m.issueAndActivate(ctx, domain, &record); err == nil {
			active.Destroy()
			return renewed, nil
		} else {
			m.renewalFailed(domain)
			if m.now().Before(record.ExpiresAt) {
				return active, nil
			}
			active.Destroy()
			return certificates.Material{}, err
		}
	}
	if !errors.Is(activeErr, ErrNotFound) && !errors.Is(activeErr, certificates.ErrInvalidMaterial) {
		return certificates.Material{}, activeErr
	}
	return m.issueAndActivate(ctx, domain, nil)
}

// Renew forces a new ACME order even when the active certificate is not near
// expiration. It is serialized with Prepare and other renewals for the SNI.
func (m *Manager) Renew(ctx context.Context, sni string) error {
	domain, err := canonicalSNI(sni)
	if err != nil {
		return err
	}
	unlock, err := m.locker.Lock(ctx, domain)
	if err != nil {
		return err
	}
	defer unlock()
	var previous *Record
	if record, material, loadErr := m.loadActive(ctx, domain, true); loadErr == nil {
		material.Destroy()
		previous = &record
	} else if !errors.Is(loadErr, ErrNotFound) && !errors.Is(loadErr, certificates.ErrInvalidMaterial) {
		return loadErr
	}
	material, err := m.issueAndActivate(ctx, domain, previous)
	material.Destroy()
	if err != nil {
		m.renewalFailed(domain)
	}
	return err
}

// Bootstrap activates the newest valid staged certificate for an SNI after an
// authorized operator explicitly acknowledges DNS targets that are not yet
// managed by this deployer. Managed targets must still accept the certificate
// before activation. No new ACME order is created.
func (m *Manager) Bootstrap(ctx context.Context, sni string, operatorUserID int64) (BootstrapResult, error) {
	domain, err := canonicalSNI(sni)
	if err != nil || operatorUserID <= 0 {
		return BootstrapResult{}, ErrInvalidInput
	}
	if m.resolver == nil {
		return BootstrapResult{}, safe("Certificate target resolver is unavailable", ErrDistributionFailed)
	}
	unlock, err := m.locker.Lock(ctx, domain)
	if err != nil {
		return BootstrapResult{}, safe("Certificate bootstrap lock is unavailable", ErrPersistenceFailed)
	}
	defer unlock()
	if _, material, activeErr := m.loadActive(ctx, domain, true); activeErr == nil {
		material.Destroy()
		return BootstrapResult{}, safe("Certificate is already active", ErrActivationFailed)
	} else if !errors.Is(activeErr, ErrNotFound) && !errors.Is(activeErr, certificates.ErrInvalidMaterial) {
		return BootstrapResult{}, safe("Active certificate state could not be checked", ErrPersistenceFailed)
	}

	versions, err := m.repository.ListVersions(ctx, domain)
	if err != nil {
		return BootstrapResult{}, safe("Certificate versions could not be loaded", ErrPersistenceFailed)
	}
	var candidate Version
	var material certificates.Material
	for _, version := range versions {
		if version.Status != VersionDistribution && version.Status != VersionPending {
			continue
		}
		loaded, loadErr := m.store.Load(ctx, domain, version.Version)
		if loadErr != nil {
			continue
		}
		info, validationErr := certificates.Validate(loaded, domain, m.now())
		if validationErr != nil || info.Fingerprint != version.Fingerprint {
			loaded.Destroy()
			continue
		}
		candidate, material = version, loaded
		break
	}
	if candidate.Version == "" {
		return BootstrapResult{}, safe("No valid staged certificate is available", ErrNotFound)
	}
	defer material.Destroy()

	resolution, err := m.resolver.Resolve(ctx, domain)
	if err != nil {
		return BootstrapResult{}, safe("Certificate distribution targets are unavailable", ErrDistributionFailed)
	}
	for _, ip := range resolution.Unmanaged {
		operator := operatorUserID
		if err := m.repository.RecordTargetReview(ctx, TargetReview{
			SNI: domain, IP: ip, State: TargetLegacyAcknowledged,
			Reason:         "Legacy DNS target explicitly acknowledged during certificate bootstrap",
			AcknowledgedBy: &operator,
		}); err != nil {
			return BootstrapResult{}, safe("Legacy target acknowledgement could not be saved", ErrPersistenceFailed)
		}
	}
	if err := m.distributeTargets(ctx, domain, candidate.Version, material, resolution.Managed); err != nil {
		return BootstrapResult{}, err
	}
	previousMarker, _ := m.store.ActiveVersion(ctx, domain)
	if err := m.store.Activate(ctx, domain, candidate.Version); err != nil {
		return BootstrapResult{}, err
	}
	if err := m.repository.ActivateVersion(ctx, domain, candidate.Version, false); err != nil {
		if previousMarker != "" {
			_ = m.store.Activate(context.WithoutCancel(ctx), domain, previousMarker)
		}
		return BootstrapResult{}, safe("Bootstrap certificate metadata could not be activated", ErrPersistenceFailed)
	}
	m.observeExpiry(Record{SNI: domain, ExpiresAt: candidate.ExpiresAt})
	return BootstrapResult{
		SNI: domain, Version: candidate.Version, ManagedTargets: len(resolution.Managed),
		AcknowledgedLegacyIPs: len(resolution.Unmanaged) + len(resolution.LegacyAcknowledged),
	}, nil
}

// Rollback redistributes and activates a previously stored valid version. The
// existing central active marker is restored if metadata activation fails.
func (m *Manager) Rollback(ctx context.Context, sni, version string) error {
	domain, err := canonicalSNI(sni)
	if err != nil || strings.TrimSpace(version) == "" {
		return ErrInvalidInput
	}
	unlock, err := m.locker.Lock(ctx, domain)
	if err != nil {
		return err
	}
	defer unlock()
	versions, err := m.repository.ListVersions(ctx, domain)
	if err != nil {
		return safe("Certificate versions could not be loaded", ErrPersistenceFailed)
	}
	found := false
	for _, item := range versions {
		if item.Version == version {
			found = true
			break
		}
	}
	if !found {
		return ErrNotFound
	}
	var previousRecord *Record
	if active, activeErr := m.repository.GetActive(ctx, domain); activeErr == nil {
		previousRecord = &active
	}
	material, err := m.store.Load(ctx, domain, version)
	if err != nil {
		return err
	}
	defer material.Destroy()
	if _, err := certificates.Validate(material, domain, m.now()); err != nil {
		return safe("Rollback certificate is invalid", certificates.ErrInvalidMaterial)
	}
	if err := m.distribute(ctx, domain, version, material); err != nil {
		m.restoreDistributedVersion(ctx, domain, previousRecord)
		return err
	}
	previous, _ := m.store.ActiveVersion(ctx, domain)
	if err := m.store.Activate(ctx, domain, version); err != nil {
		m.restoreDistributedVersion(ctx, domain, previousRecord)
		return err
	}
	if err := m.repository.ActivateVersion(ctx, domain, version, false); err != nil {
		if previous != "" {
			_ = m.store.Activate(context.WithoutCancel(ctx), domain, previous)
		}
		m.restoreDistributedVersion(ctx, domain, previousRecord)
		return safe("Rollback metadata could not be activated", ErrPersistenceFailed)
	}
	return nil
}

func (m *Manager) issueAndActivate(ctx context.Context, domain string, previous *Record) (certificates.Material, error) {
	issueCtx, cancel := context.WithTimeout(ctx, m.config.IssueTimeout)
	defer cancel()
	material, err := m.issuer.Issue(issueCtx, domain)
	if err != nil {
		return certificates.Material{}, safe("Certificate issuance failed", ErrIssuanceFailed)
	}
	valid := false
	defer func() {
		if !valid {
			material.Destroy()
		}
	}()
	info, err := certificates.Validate(material, domain, m.now())
	if err != nil {
		_ = m.repository.SetStatus(context.WithoutCancel(ctx), domain, StatusInvalid)
		return certificates.Material{}, safe("Issued certificate is invalid", certificates.ErrInvalidMaterial)
	}
	version, err := m.store.Stage(ctx, domain, material)
	if err != nil {
		return certificates.Material{}, err
	}
	metadata := Version{SNI: domain, Version: version, Fingerprint: info.Fingerprint, Serial: info.Serial, IssuedAt: info.IssuedAt, ExpiresAt: info.ExpiresAt, Status: VersionPending, CreatedAt: m.now().UTC()}
	if err := m.repository.SaveVersion(ctx, metadata); err != nil {
		return certificates.Material{}, safe("Certificate metadata could not be saved", ErrPersistenceFailed)
	}
	if err := m.distribute(ctx, domain, version, material); err != nil {
		m.restoreDistributedVersion(ctx, domain, previous)
		_ = m.repository.SetVersionStatus(context.WithoutCancel(ctx), domain, version, VersionDistribution)
		_ = m.repository.SetStatus(context.WithoutCancel(ctx), domain, StatusDistributionFailed)
		return certificates.Material{}, err
	}
	oldVersion := ""
	if previous != nil {
		oldVersion = previous.ActiveVersion
	}
	if err := m.store.Activate(ctx, domain, version); err != nil {
		m.restoreDistributedVersion(ctx, domain, previous)
		_ = m.repository.SetVersionStatus(context.WithoutCancel(ctx), domain, version, VersionDistribution)
		_ = m.repository.SetStatus(context.WithoutCancel(ctx), domain, StatusDistributionFailed)
		return certificates.Material{}, err
	}
	if err := m.repository.ActivateVersion(ctx, domain, version, previous != nil); err != nil {
		if oldVersion != "" {
			_ = m.store.Activate(context.WithoutCancel(ctx), domain, oldVersion)
		}
		m.restoreDistributedVersion(ctx, domain, previous)
		_ = m.repository.SetVersionStatus(context.WithoutCancel(ctx), domain, version, VersionDistribution)
		_ = m.repository.SetStatus(context.WithoutCancel(ctx), domain, StatusDistributionFailed)
		return certificates.Material{}, safe("Certificate metadata could not be activated", ErrPersistenceFailed)
	}
	m.observeExpiry(Record{SNI: domain, ExpiresAt: info.ExpiresAt})
	valid = true
	return material, nil
}

func (m *Manager) distribute(ctx context.Context, domain, version string, material certificates.Material) error {
	if m.resolver == nil {
		return nil
	}
	resolution, err := m.resolver.Resolve(ctx, domain)
	if err != nil {
		return safe("Certificate distribution targets are unavailable", ErrDistributionFailed)
	}
	if len(resolution.Unmanaged) > 0 {
		for _, ip := range resolution.Unmanaged {
			if err := m.repository.RecordTargetReview(ctx, TargetReview{
				SNI: domain, IP: ip, State: TargetManualReview,
				Reason: "DNS target has no verified deployment SSH identity",
			}); err != nil {
				return safe("Certificate target review could not be saved", ErrPersistenceFailed)
			}
		}
		return safe("Certificate distribution requires review of unmanaged DNS targets", ErrDistributionFailed)
	}
	return m.distributeTargets(ctx, domain, version, material, resolution.Managed)
}

func (m *Manager) distributeTargets(ctx context.Context, domain, version string, material certificates.Material, targets []Target) error {
	if len(targets) == 0 {
		return nil
	}
	if m.distributor == nil {
		return safe("Certificate distributor is unavailable", ErrDistributionFailed)
	}
	results := m.distributor.Distribute(ctx, domain, material, targets)
	failed := len(results) != len(targets)
	for _, result := range results {
		safeErrorMessage := ""
		if result.Status != DistributionSucceeded {
			safeErrorMessage = safeMessage(result.SafeMessage)
		}
		record := DistributionRecord{SNI: domain, Version: version, DeploymentID: result.Target.DeploymentID, NodeIP: result.Target.IP, Status: result.Status, SafeErrorMessage: safeErrorMessage, AttemptedAt: m.now().UTC()}
		if err := m.repository.RecordDistribution(ctx, record); err != nil {
			return safe("Certificate distribution result could not be saved", ErrPersistenceFailed)
		}
		if result.Status != DistributionSucceeded {
			failed = true
		}
	}
	if failed {
		return safe("Certificate distribution was incomplete", ErrDistributionFailed)
	}
	return nil
}

// restoreDistributedVersion is best-effort compensation. It never changes the
// central active marker or metadata; it only brings Nodes that accepted a
// failed candidate activation back to the previously active material.
func (m *Manager) restoreDistributedVersion(ctx context.Context, domain string, previous *Record) {
	if previous == nil || previous.ActiveVersion == "" {
		return
	}
	rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), m.config.IssueTimeout)
	defer cancel()
	material, err := m.store.Load(rollbackCtx, domain, previous.ActiveVersion)
	if err != nil {
		return
	}
	defer material.Destroy()
	if info, err := certificates.Validate(material, domain, m.now()); err != nil || info.Fingerprint != previous.Fingerprint {
		return
	}
	_ = m.distribute(rollbackCtx, domain, previous.ActiveVersion, material)
}

func (m *Manager) loadActive(ctx context.Context, domain string, repairMarker bool) (Record, certificates.Material, error) {
	record, err := m.repository.GetActive(ctx, domain)
	if err != nil {
		return Record{}, certificates.Material{}, err
	}
	marker, markerErr := m.store.ActiveVersion(ctx, domain)
	if markerErr != nil || marker != record.ActiveVersion {
		if !repairMarker {
			return Record{}, certificates.Material{}, safe("Active certificate storage is inconsistent", ErrStorageFailed)
		}
		if err := m.store.Activate(ctx, domain, record.ActiveVersion); err != nil {
			return Record{}, certificates.Material{}, err
		}
	}
	material, err := m.store.Load(ctx, domain, record.ActiveVersion)
	if err != nil {
		return Record{}, certificates.Material{}, err
	}
	info, err := certificates.Validate(material, domain, m.now())
	if err != nil || info.Fingerprint != record.Fingerprint {
		material.Destroy()
		return Record{}, certificates.Material{}, certificates.ErrInvalidMaterial
	}
	return record, material, nil
}

func (m *Manager) RenewDue(ctx context.Context) error {
	records, err := m.repository.ListExpiring(ctx, m.now().Add(m.config.RenewBefore), m.config.RenewBatchSize)
	if err != nil {
		return safe("Expiring certificates could not be listed", ErrPersistenceFailed)
	}
	var renewalErrors []error
	for _, record := range records {
		if err := m.Renew(ctx, record.SNI); err != nil {
			renewalErrors = append(renewalErrors, err)
		}
	}
	return errors.Join(renewalErrors...)
}

func (m *Manager) Run(ctx context.Context) error {
	ticker := time.NewTicker(m.config.RenewInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			_ = m.RenewDue(ctx)
		}
	}
}

func (m *Manager) observeExpiry(record Record) {
	if m.observer != nil && !record.ExpiresAt.IsZero() {
		m.observer.SetCertificateExpiry(record.SNI, record.ExpiresAt.Sub(m.now()))
	}
}

func (m *Manager) renewalFailed(domain string) {
	if m.observer != nil {
		m.observer.CertificateRenewalFailed(domain)
	}
}

func safeMessage(value string) string {
	value = strings.TrimSpace(strings.NewReplacer("\r", " ", "\n", " ", "\t", " ").Replace(value))
	if value == "" {
		return "Certificate distribution failed"
	}
	if len(value) > 200 {
		value = value[:200]
	}
	return value
}

var _ interface {
	Readiness(context.Context, string) (certificates.Readiness, error)
	Prepare(context.Context, string) (certificates.Material, error)
} = (*Manager)(nil)
