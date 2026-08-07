// Package orchestrator connects persistence, VPS preparation, Remnawave and
// DNS into the durable deployment workflow.
package orchestrator

import (
	"context"
	"errors"
	"net/netip"
	"time"

	"remnanode-setup-bot/internal/certificates"
	"remnanode-setup-bot/internal/deployment"
	"remnanode-setup-bot/internal/dnsbalancer"
	"remnanode-setup-bot/internal/provisioner"
	"remnanode-setup-bot/internal/remnawave"
	"remnanode-setup-bot/internal/repository"
)

var (
	ErrInvalidInput             = errors.New("invalid deployment input")
	ErrHostUnavailable          = errors.New("selected Host is unavailable")
	ErrDuplicateNodeName        = errors.New("duplicate Node name")
	ErrDuplicateNodeAddress     = errors.New("duplicate Node address")
	ErrPreflightFailed          = errors.New("VPS preflight failed")
	ErrCertificateUnavailable   = errors.New("certificate unavailable")
	ErrProvisioningFailed       = errors.New("VPS provisioning failed")
	ErrRemnawaveCreationFailed  = errors.New("Remnawave Node creation failed")
	ErrNodeConnectionFailed     = errors.New("Remnawave Node connection failed")
	ErrNodeConnectionTimeout    = errors.New("Remnawave Node connection timed out")
	ErrDNSUpdateFailed          = errors.New("DNS update failed")
	ErrDNSRetryRequired         = errors.New("deployment requires DNS retry")
	ErrDeploymentAlreadyRunning = errors.New("deployment is already running")
	ErrDeploymentNotRunnable    = errors.New("deployment is not runnable")
	ErrPersistenceFailed        = errors.New("deployment persistence failed")
)

type CertificateReadiness = certificates.Readiness

const (
	CertificateUnknown  = certificates.ReadinessUnknown
	CertificateReady    = certificates.ReadinessReady
	CertificateNotReady = certificates.ReadinessNotReady
)

// CertificateProvider owns certificate lookup/issuance. Prepare returns an
// in-memory copy that the caller must destroy.
type CertificateProvider interface {
	Readiness(context.Context, string) (CertificateReadiness, error)
	Prepare(context.Context, string) (certificates.Material, error)
}

// VPSOperator hides SSH connectivity and provisioner construction from the
// workflow. Implementations must not retain password, SECRET_KEY, or
// certificate material after the method returns.
type VPSOperator interface {
	Preflight(context.Context, PreflightVPSInput) (provisioner.PreflightResult, error)
	Provision(context.Context, ProvisionVPSInput, func(provisioner.Report)) error
}

type PreflightVPSInput struct {
	DeploymentID string
	Address      netip.Addr
	Password     []byte
}

type ProvisionVPSInput struct {
	DeploymentID       string
	Address            netip.Addr
	SNIDomain          string
	RemnanodeSecretKey []byte
	Certificate        certificates.Material
}

type RemnawaveAPI interface {
	GetHosts(context.Context) ([]remnawave.Host, error)
	GenerateSecretKey(context.Context) (string, error)
	GetNodes(context.Context) ([]remnawave.Node, error)
	GetNode(context.Context, string) (remnawave.Node, error)
	CreateNode(context.Context, remnawave.CreateNodeInput) (remnawave.Node, error)
}

type DNSAPI interface {
	FindZone(context.Context, string) (dnsbalancer.ZoneMatch, error)
	AddIP(context.Context, string, netip.Addr) (dnsbalancer.AddIPResult, error)
}

type DeploymentRepository interface {
	repository.DeploymentRepository
}

type Config struct {
	MaxConcurrentDeployments int
	NodeConnectTimeout       time.Duration
	InitialPollBackoff       time.Duration
	MaxPollBackoff           time.Duration
	Observer                 Observer
}

type Observer interface {
	DeploymentCreated()
	DeploymentFailed()
	ActiveDeployment(int64)
	DeploymentDuration(time.Duration)
	ProvisioningStepDuration(string, time.Duration)
}

type PrepareInput struct {
	OperatorUserID int64
	HostID         string
	NodeName       string
	VPSIP          netip.Addr
	Password       []byte
}

type PreparedDeployment struct {
	Deployment             deployment.Deployment
	DNSZone                string
	CertificateReadiness   CertificateReadiness
	ConfigProfileReadiness bool
	SafeWarnings           []string
}

type StartInput struct {
	DeploymentID   string
	OperatorUserID int64
	HostID         string
	NodeName       string
	VPSIP          netip.Addr
}

type Progress struct {
	Step        string
	Completed   int
	Total       int
	SafeMessage string
}

type ProgressSink func(Progress)

type SafeLogEntry struct {
	Step    string
	Status  string
	Summary string
}

// SafeError exposes only an allow-listed operator message and code.
type SafeError struct {
	Code        string
	SafeMessage string
	Kind        error
}

func (e *SafeError) Error() string {
	if e.SafeMessage != "" {
		return e.SafeMessage
	}
	return "deployment operation failed"
}

func (e *SafeError) Unwrap() error { return e.Kind }
