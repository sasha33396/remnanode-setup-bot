// Package repository defines persistent storage operations.
package repository

import (
	"context"
	"errors"
	"net/netip"

	"remnanode-setup-bot/internal/deployment"
)

var (
	// ErrNotFound is returned when a requested persisted entity does not exist.
	ErrNotFound = errors.New("repository entity not found")
	// ErrInvalidArgument is returned before executing a query with invalid input.
	ErrInvalidArgument = errors.New("repository invalid argument")
)

// CreateDeploymentParams contains immutable operator input for a new job.
type CreateDeploymentParams struct {
	ID                        string
	PanelID                   string
	TelegramOperatorUserID    int64
	SelectedRemnawaveHostUUID string
	SelectedHostRemark        string
	SNIDomain                 string
	NodeName                  string
	TargetVPSIP               netip.Addr
}

// UpdateDeploymentStateParams contains an atomic state transition snapshot.
type UpdateDeploymentStateParams struct {
	Status           deployment.Status
	CurrentStep      string
	SafeErrorCode    *string
	SafeErrorMessage *string
}

// RecordStepParams contains the latest safe outcome of a provisioning step.
type RecordStepParams struct {
	DeploymentID string
	Name         string
	Status       deployment.StepStatus
	SafeSummary  *string
	ErrorMessage *string
}

// DeploymentRepository stores deployment jobs and their individual steps.
type DeploymentRepository interface {
	CreateDeployment(context.Context, CreateDeploymentParams) (deployment.Deployment, error)
	GetDeployment(context.Context, string) (deployment.Deployment, error)
	UpdateDeploymentState(context.Context, string, UpdateDeploymentStateParams) (deployment.Deployment, error)
	SetRemnawaveNodeUUID(context.Context, string, string) (deployment.Deployment, error)
	SetTargetVPSIP(context.Context, string, netip.Addr) (deployment.Deployment, error)
	RecordDeploymentStep(context.Context, RecordStepParams) (deployment.Step, error)
	ListDeploymentSteps(context.Context, string) ([]deployment.Step, error)
	ListRecentDeployments(context.Context, int) ([]deployment.Deployment, error)
	FindUnfinishedDeployments(context.Context, int) ([]deployment.Deployment, error)
	FindDeploymentByPanelNodeUUID(context.Context, string, string) (deployment.Deployment, error)
}
