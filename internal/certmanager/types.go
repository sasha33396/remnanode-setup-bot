// Package certmanager owns centralized certificate issuance, versioning,
// renewal and distribution. Private keys never leave Material or FileStore.
package certmanager

import (
	"context"
	"errors"
	"net/netip"
	"time"

	"remnanode-setup-bot/internal/certificates"
)

var (
	ErrNotFound            = errors.New("certificate metadata not found")
	ErrInvalidInput        = errors.New("invalid certificate manager input")
	ErrIssuanceFailed      = errors.New("certificate issuance failed")
	ErrStorageFailed       = errors.New("certificate storage failed")
	ErrPersistenceFailed   = errors.New("certificate metadata persistence failed")
	ErrDistributionFailed  = errors.New("certificate distribution failed")
	ErrActivationFailed    = errors.New("certificate activation failed")
	ErrNoUsableCertificate = errors.New("no usable certificate is available")
)

type Status string

const (
	StatusIssuing            Status = "ISSUING"
	StatusActive             Status = "ACTIVE"
	StatusRenewalDue         Status = "RENEWAL_DUE"
	StatusDistributionFailed Status = "DISTRIBUTION_FAILED"
	StatusInvalid            Status = "INVALID"
)

type VersionStatus string

const (
	VersionPending      VersionStatus = "PENDING"
	VersionActive       VersionStatus = "ACTIVE"
	VersionSuperseded   VersionStatus = "SUPERSEDED"
	VersionDistribution VersionStatus = "DISTRIBUTION_FAILED"
	VersionInvalid      VersionStatus = "INVALID"
)

type DistributionStatus string

const (
	DistributionSucceeded DistributionStatus = "SUCCEEDED"
	DistributionFailed    DistributionStatus = "FAILED"
)

type Record struct {
	SNI           string
	Fingerprint   string
	Serial        string
	IssuedAt      time.Time
	ExpiresAt     time.Time
	LastRenewedAt *time.Time
	Status        Status
	ActiveVersion string
	UpdatedAt     time.Time
}

type Version struct {
	SNI         string
	Version     string
	Fingerprint string
	Serial      string
	IssuedAt    time.Time
	ExpiresAt   time.Time
	Status      VersionStatus
	CreatedAt   time.Time
}

type DistributionRecord struct {
	SNI              string
	Version          string
	DeploymentID     string
	NodeIP           netip.Addr
	Status           DistributionStatus
	SafeErrorMessage string
	AttemptedAt      time.Time
}

type Repository interface {
	GetActive(context.Context, string) (Record, error)
	SaveVersion(context.Context, Version) error
	SetVersionStatus(context.Context, string, string, VersionStatus) error
	ActivateVersion(context.Context, string, string, bool) error
	SetStatus(context.Context, string, Status) error
	RecordDistribution(context.Context, DistributionRecord) error
	ListExpiring(context.Context, time.Time, int) ([]Record, error)
	ListVersions(context.Context, string) ([]Version, error)
}

type Locker interface {
	Lock(context.Context, string) (func(), error)
}

type Store interface {
	Stage(context.Context, string, certificates.Material) (string, error)
	Load(context.Context, string, string) (certificates.Material, error)
	ActiveVersion(context.Context, string) (string, error)
	Activate(context.Context, string, string) error
}

type Issuer interface {
	Issue(context.Context, string) (certificates.Material, error)
}

type Target struct {
	DeploymentID string
	IP           netip.Addr
}

type TargetResolver interface {
	Targets(context.Context, string) ([]Target, error)
}

type DistributionResult struct {
	Target      Target
	Status      DistributionStatus
	SafeMessage string
}

type Distributor interface {
	Distribute(context.Context, string, certificates.Material, []Target) []DistributionResult
}

type Observer interface {
	SetCertificateExpiry(string, time.Duration)
	CertificateRenewalFailed(string)
}

type Config struct {
	RenewBefore    time.Duration
	IssueTimeout   time.Duration
	RenewInterval  time.Duration
	RenewBatchSize int
}

type safeError struct {
	message string
	kind    error
}

func (e *safeError) Error() string { return e.message }
func (e *safeError) Unwrap() error { return e.kind }

func safe(message string, kind error) error { return &safeError{message: message, kind: kind} }
