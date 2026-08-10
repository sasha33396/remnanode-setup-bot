package telegram

import (
	"context"
	"errors"
	"net/netip"
	"time"
)

const (
	MenuAddNode     = "➕ Add Node"
	MenuNodes       = "📡 Nodes"
	MenuDeployments = "📜 Deployments"
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

type NodeSummary struct {
	Name      string
	Address   string
	Connected bool
}

// NodeIPChangeTarget is an operator-safe projection used by the IP-change
// wizard. UUID is retained only in transient server-side state.
type NodeIPChangeTarget struct {
	UUID      string
	Name      string
	Address   netip.Addr
	Connected bool
	DNSZones  []string
	IsManaged bool
}

type NodeIPChangeInput struct {
	NodeUUID   string
	ExpectedIP netip.Addr
	NewIP      netip.Addr
}

type DeploymentSummary struct {
	ID        string
	NodeName  string
	Status    string
	UpdatedAt time.Time
}

// PreflightInput contains the transient password. Implementations must not
// retain it after Preflight returns.
type PreflightInput struct {
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
	SafeWarnings           []string
}

// DeploymentInput contains the password retained by the transient wizard.
// StartDeployment must run synchronously and must not retain Password after it
// returns.
type DeploymentInput struct {
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
}

// Application is the only dependency from Telegram presentation code into
// deployment behavior. Implementations may orchestrate APIs, persistence and
// SSH behind this boundary.
type Application interface {
	ListHosts(context.Context) ([]Host, error)
	CheckNodeName(context.Context, string) error
	CheckVPSAddress(context.Context, netip.Addr) error
	Preflight(context.Context, PreflightInput) (PreflightResult, error)
	StartDeployment(context.Context, DeploymentInput, func(Progress) error) error
	CancelDeployment(context.Context, string) error
	ListNodes(context.Context) ([]NodeSummary, error)
	ListDeployments(context.Context, int) ([]DeploymentSummary, error)
}

// RecoveryApplication is optional. Controller exposes these operations only
// when the application implements the safe production-recovery contract.
type RecoveryApplication interface {
	RetryFailedStep(context.Context, string) error
	RetryDNS(context.Context, string) error
	RecheckRemnawave(context.Context, string) (string, error)
	ViewSafeLogs(context.Context, string) ([]string, error)
	BootstrapCertificate(context.Context, string, int64) (string, error)
}

// NodeIPApplication is optional. It powers the button-based IP replacement
// wizard for both managed and legacy Remnawave Nodes.
type NodeIPApplication interface {
	FindNodeForIPChange(context.Context, string) (NodeIPChangeTarget, error)
	ReplaceNodeIP(context.Context, NodeIPChangeInput) (string, error)
}
