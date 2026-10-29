// Package orchestrator connects persistence, VPS preparation, Remnawave and
// DNS into the durable deployment workflow.
package orchestrator

import (
	"context"
	"errors"
	"log/slog"
	"net/netip"
	"time"

	"remnanode-setup-bot/internal/certificates"
	"remnanode-setup-bot/internal/certmanager"
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

type CertificateBootstrapper interface {
	Bootstrap(context.Context, string, int64) (certmanager.BootstrapResult, error)
}

type NewTargetCertificateProvider interface {
	PrepareForNewTarget(context.Context, string) (certificates.Material, error)
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

type NodeSNISwitchInput struct {
	Address             netip.Addr
	PreviousSNI         string
	TargetSNI           string
	Password            []byte
	PreviousCertificate certificates.Material
	Certificate         certificates.Material
}

type NodeSNISwitcher interface {
	SwitchNodeSNI(context.Context, NodeSNISwitchInput) error
}

type RemnawaveAPI interface {
	GetHosts(context.Context) ([]remnawave.Host, error)
	GenerateSecretKey(context.Context) (string, error)
	GetNodes(context.Context) ([]remnawave.Node, error)
	GetNode(context.Context, string) (remnawave.Node, error)
	CreateNode(context.Context, remnawave.CreateNodeInput) (remnawave.Node, error)
	UpdateNodeAddress(context.Context, remnawave.UpdateNodeAddressInput) (remnawave.Node, error)
	UpdateNodeProfile(context.Context, remnawave.UpdateNodeProfileInput) (remnawave.Node, error)
}

type AdvancedRemnawaveAPI interface {
	GetConfigProfiles(context.Context) ([]remnawave.ConfigProfile, error)
	CreateConfigProfile(context.Context, remnawave.CreateConfigProfileInput) (remnawave.ConfigProfile, error)
	GenerateX25519(context.Context) (remnawave.X25519Keypair, error)
	GetInternalSquads(context.Context) ([]remnawave.InternalSquad, error)
	AddInboundToInternalSquad(context.Context, string, string) (remnawave.InternalSquad, error)
	CreateHost(context.Context, remnawave.CreateHostInput) (remnawave.Host, error)
}

type DNSAPI interface {
	FindZone(context.Context, string) (dnsbalancer.ZoneMatch, error)
	FindZonesByIP(context.Context, netip.Addr) ([]dnsbalancer.ZoneMatch, error)
	AddIP(context.Context, string, netip.Addr) (dnsbalancer.AddIPResult, error)
	ReplaceIP(context.Context, string, netip.Addr, netip.Addr) (dnsbalancer.ReplaceIPResult, error)
	MoveIP(context.Context, string, string, netip.Addr) (dnsbalancer.MoveIPResult, error)
}

type DNSRecordAPI interface {
	CheckARecord(context.Context, string, netip.Addr) error
	EnsureARecord(context.Context, string, netip.Addr) (bool, error)
}

type DeploymentRepository interface {
	repository.DeploymentRepository
}

type AdvancedDeploymentRepository interface {
	SetAdvancedProfile(context.Context, string, repository.SetAdvancedProfileParams) (deployment.Deployment, error)
}

type Config struct {
	PanelID                  string
	DNSDisabled              bool
	MaxConcurrentDeployments int
	NodeConnectTimeout       time.Duration
	InitialPollBackoff       time.Duration
	MaxPollBackoff           time.Duration
	Observer                 Observer
	NodeSNISwitcher          NodeSNISwitcher
	Logger                   *slog.Logger
	DNSRecords               DNSRecordAPI
}

type Observer interface {
	DeploymentCreated()
	DeploymentFailed()
	ActiveDeployment(int64)
	DeploymentDuration(time.Duration)
	ProvisioningStepDuration(string, time.Duration)
}

type PrepareInput struct {
	PanelID        string
	OperatorUserID int64
	HostID         string
	NodeName       string
	VPSIP          netip.Addr
	Password       []byte
}

type PrepareAdvancedBalancerInput struct {
	PanelID         string
	OperatorUserID  int64
	InternalSquadID string
	BalancerTag     string
	HostFQDN        string
	NodeName        string
	IngressIP       netip.Addr
	EgressIP        netip.Addr
	Password        []byte
}

type PreparedDeployment struct {
	Deployment             deployment.Deployment
	DNSZone                string
	CertificateReadiness   CertificateReadiness
	ConfigProfileReadiness bool
	Warnings               []OperatorNotice
}

type OperatorNotice struct {
	Code    string
	Message string
}

type StartInput struct {
	DeploymentID   string
	OperatorUserID int64
	HostID         string
	NodeName       string
	VPSIP          netip.Addr
}

// AdoptLegacyNodeInput promotes an existing Remnawave Node into the managed
// deployment lifecycle. Password is only used by the initial SSH preflight.
type AdoptLegacyNodeInput struct {
	OperatorUserID int64
	NodeUUID       string
	Password       []byte
}

type Progress struct {
	Step        string
	Completed   int
	Total       int
	SafeMessage string
	Status      deployment.StepStatus
	Code        string
}

type ProgressSink func(Progress)

type SafeLogEntry struct {
	Step        string
	Status      string
	Summary     string
	Code        string
	StartedAt   *time.Time
	CompletedAt *time.Time
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

func (e *SafeError) OperatorCode() string    { return e.Code }
func (e *SafeError) OperatorMessage() string { return e.SafeMessage }
