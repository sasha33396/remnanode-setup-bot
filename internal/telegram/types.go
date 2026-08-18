package telegram

import (
	"context"
	"errors"
	"net/netip"
	"time"
)

const (
	MenuAddNode     = "➕ Добавить ноду"
	MenuNodes       = "📡 Ноды"
	MenuDeployments = "📜 Развёртывания"
	MenuChangeIP    = "🔄 Сменить IP"
)

var (
	ErrDuplicateNodeName = errors.New("duplicate Node name")
	ErrDuplicateVPSIP    = errors.New("duplicate VPS IP")
)

// Update is the small Telegram update subset consumed by Controller.
type Update struct {
	ID            int64
	Message       *Message
	CallbackQuery *CallbackQuery
}

// Message contains only fields needed by the presentation layer.
type Message struct {
	ID         int
	ChatID     int64
	FromUserID int64
	Text       string
}

// CallbackQuery represents an inline-keyboard action.
type CallbackQuery struct {
	ID         string
	FromUserID int64
	Message    *Message
	Data       string
}

type Button struct {
	Text         string
	CallbackData string
}

// Keyboard represents either an inline or a reply keyboard.
type Keyboard struct {
	Inline [][]Button
	Reply  [][]string
}

// Messenger is the Telegram transport used by Controller.
type Messenger interface {
	SendMessage(context.Context, int64, string, Keyboard) (Message, error)
	EditMessage(context.Context, int64, int, string, Keyboard) error
	DeleteMessage(context.Context, int64, int) error
	AnswerCallback(context.Context, string, string) error
}

// Readiness is a safe presentation value. It must never contain diagnostic or
// secret material.
type Readiness string

const (
	ReadinessUnknown  Readiness = "unknown"
	ReadinessReady    Readiness = "ready"
	ReadinessNotReady Readiness = "not_ready"
)

// Host is a safe application projection for the Host picker.
type Host struct {
	ID                     string
	Remark                 string
	Address                string
	ConfigProfileReadiness Readiness
}

type Panel struct {
	ID         string
	Name       string
	DNSEnabled bool
}

type NodeSummary struct {
	PanelID           string
	PanelName         string
	UUID              string
	Name              string
	Address           string
	Connected         bool
	Connecting        bool
	Disabled          bool
	Online            int
	OnlineKnown       bool
	LastStatusMessage string
	LastStatusChange  *time.Time
}

// NodeIPChangeTarget is an operator-safe projection used by the IP-change
// wizard. UUID is retained only in transient server-side state.
type NodeIPChangeTarget struct {
	PanelName  string
	DNSEnabled bool
	UUID       string
	Name       string
	Address    netip.Addr
	Connected  bool
	DNSZones   []string
	IsManaged  bool
}

type NodeIPChangeInput struct {
	PanelID    string
	NodeUUID   string
	ExpectedIP netip.Addr
	NewIP      netip.Addr
}

// NodeHostMoveTarget is a safe preview of the Node's current profile binding.
// Profile UUIDs stay in transient server-side state and are never rendered.
type NodeHostMoveTarget struct {
	PanelName            string
	UUID                 string
	Name                 string
	Address              string
	Managed              bool
	CurrentHostKnown     bool
	CurrentHostRemark    string
	CurrentHostAddress   string
	ExpectedProfileUUID  string
	ExpectedInboundUUIDs []string
}

type NodeHostMoveInput struct {
	PanelID              string
	NodeUUID             string
	TargetHostUUID       string
	ExpectedProfileUUID  string
	ExpectedInboundUUIDs []string
}

type NodeHostMoveResult struct {
	NodeName          string
	PreviousHostKnown bool
	PreviousHost      string
	TargetHost        string
	TargetAddress     string
	Managed           bool
}

// NodeDNSSyncTarget is an operator-safe preview. Address is always the
// canonical value read from Remnawave; DNS is never allowed to overwrite it.
type NodeDNSSyncTarget struct {
	PanelName       string
	UUID            string
	Name            string
	Address         netip.Addr
	Connected       bool
	Managed         bool
	DNSZone         string
	PreviousIP      netip.Addr
	CurrentZones    []string
	CurrentPresent  bool
	PreviousPresent bool
	CanSync         bool
	Note            string
}

type NodeDNSSyncInput struct {
	PanelID    string
	NodeUUID   string
	ExpectedIP netip.Addr
}

type NodeDNSSyncResult struct {
	NodeName   string
	Address    netip.Addr
	DNSZone    string
	Action     string
	PreviousIP netip.Addr
}

// CherryIPInput contains the transient root password used only for one SSH
// connection. Implementations must not retain Password after the call returns.
type CherryIPInput struct {
	ServerIP   netip.Addr
	FloatingIP netip.Addr
	Password   []byte
}

type CherryIPResult struct {
	Interface      string
	LiveConfigured bool
	Persistent     bool
	PersistentNote string
}

// RoyalIPInput contains the transient root password used while replacing the
// primary IPv4 address and gateway on a Royal Hosting server.
type RoyalIPInput struct {
	ServerIP netip.Addr
	NewIP    netip.Addr
	Password []byte
}

type RoyalIPResult struct {
	Interface   string
	PrefixBits  int
	Gateway     netip.Addr
	NetplanFile string
	BackupFile  string
}

type DeploymentSummary struct {
	PanelName     string
	ID            string
	NodeName      string
	Status        string
	CurrentStep   string
	TargetIP      netip.Addr
	SafeErrorCode string
	SafeError     string
	CreatedAt     time.Time
	UpdatedAt     time.Time
	StartedAt     *time.Time
	CompletedAt   *time.Time
}

type DeploymentDetails struct {
	DeploymentSummary
	SNI           string
	CanRetryStep  bool
	CanRetryDNS   bool
	CanRecheck    bool
	CanRepairCert bool
	CanCancel     bool
}

type DeploymentLogEntry struct {
	Step        string
	Status      string
	Summary     string
	Code        string
	StartedAt   *time.Time
	CompletedAt *time.Time
}

// PreflightInput contains the transient password. Implementations must not
// retain it after Preflight returns.
type PreflightInput struct {
	PanelID        string
	OperatorUserID int64
	HostID         string
	NodeName       string
	VPSIP          netip.Addr
	Password       []byte
}

// PreflightResult contains operator-safe readiness information only.
type PreflightResult struct {
	PreparedDeploymentID   string
	DNSZone                string
	CertificateReadiness   Readiness
	ConfigProfileReadiness Readiness
	Warnings               []OperatorNotice
}

type OperatorNotice struct {
	Code    string
	Message string
}

// DeploymentInput contains the password retained by the transient wizard.
// StartDeployment must run synchronously and must not retain Password after it
// returns.
type DeploymentInput struct {
	PanelID              string
	PreparedDeploymentID string
	OperatorUserID       int64
	HostID               string
	NodeName             string
	VPSIP                netip.Addr
	Password             []byte
}

// Progress is a safe status update. SafeMessage is expected to be suitable
// for an operator and must not contain raw command output or secrets.
type Progress struct {
	Step        string
	Completed   int
	Total       int
	SafeMessage string
	Status      ProgressStatus
	Code        string
}

type ProgressStatus string

const (
	ProgressRunning   ProgressStatus = "RUNNING"
	ProgressCompleted ProgressStatus = "COMPLETED"
	ProgressWarning   ProgressStatus = "WARNING"
	ProgressFailed    ProgressStatus = "FAILED"
	ProgressSkipped   ProgressStatus = "SKIPPED"
)

// Application is the only dependency from Telegram presentation code into
// deployment behavior. Implementations may orchestrate APIs, persistence and
// SSH behind this boundary.
type Application interface {
	ListPanels(context.Context) ([]Panel, error)
	ListHosts(context.Context, string) ([]Host, error)
	CheckNodeName(context.Context, string, string) error
	CheckVPSAddress(context.Context, string, netip.Addr) error
	Preflight(context.Context, PreflightInput) (PreflightResult, error)
	StartDeployment(context.Context, DeploymentInput, func(Progress) error) error
	CancelDeployment(context.Context, string) error
	ListNodes(context.Context) ([]NodeSummary, error)
	ListDeployments(context.Context, int) ([]DeploymentSummary, error)
}

// RecoveryApplication is optional. Controller exposes these operations only
// when the application implements the safe production-recovery contract.
type RecoveryApplication interface {
	GetDeploymentDetails(context.Context, string) (DeploymentDetails, error)
	RetryFailedStep(context.Context, string) error
	RetryDNS(context.Context, string) error
	RecheckRemnawave(context.Context, string) (string, error)
	ViewSafeLogs(context.Context, string) ([]DeploymentLogEntry, error)
	BootstrapCertificate(context.Context, string, int64) (string, error)
}

// NodeIPApplication is optional. It powers the button-based IP replacement
// wizard for both managed and legacy Remnawave Nodes.
type NodeIPApplication interface {
	FindNodeForIPChange(context.Context, string, string) (NodeIPChangeTarget, error)
	ReplaceNodeIP(context.Context, NodeIPChangeInput) (string, error)
}

// NodeHostMoveApplication moves both managed and legacy Nodes between valid
// Hosts of their current Remnawave panel.
type NodeHostMoveApplication interface {
	PrepareNodeHostMove(context.Context, string, string) (NodeHostMoveTarget, []Host, error)
	MoveNodeToHost(context.Context, NodeHostMoveInput) (NodeHostMoveResult, error)
}

// NodeDNSSyncApplication repairs a managed Node's DNS membership using the
// current Remnawave address as the source of truth.
type NodeDNSSyncApplication interface {
	FindNodeForDNSSync(context.Context, string, string) (NodeDNSSyncTarget, error)
	SyncNodeDNS(context.Context, NodeDNSSyncInput) (NodeDNSSyncResult, error)
}

// CherryIPApplication is optional. It powers the SSH/netplan Cherry Servers
// floating-IP wizard and keeps the temporary password outside durable state.
type CherryIPApplication interface {
	ConfigureCherryIP(context.Context, CherryIPInput) (CherryIPResult, error)
}

// RoyalIPApplication is optional. It powers primary IPv4 and gateway
// replacement for Royal Hosting servers.
type RoyalIPApplication interface {
	ConfigureRoyalIP(context.Context, RoyalIPInput) (RoyalIPResult, error)
}
