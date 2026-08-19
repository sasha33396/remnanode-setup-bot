// Package deployment contains deployment workflow domain types.
package deployment

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/netip"
	"time"
)

// Status is the persisted state of a deployment job.
type Status string

const (
	StatusCreated              Status = "CREATED"
	StatusPreflight            Status = "PREFLIGHT"
	StatusPreparingCertificate Status = "PREPARING_CERTIFICATE"
	StatusProvisioning         Status = "PROVISIONING"
	StatusCreatingRemnawave    Status = "CREATING_REMNAWAVE_NODE"
	StatusWaitingRemnawave     Status = "WAITING_REMNAWAVE"
	StatusAddingToDNS          Status = "ADDING_TO_DNS"
	StatusCompleted            Status = "COMPLETED"
	StatusFailed               Status = "FAILED"
	StatusCancelled            Status = "CANCELLED"
	StatusDNSFailed            Status = "DNS_FAILED"
	StatusManualReview         Status = "MANUAL_REVIEW"
)

// Valid reports whether s is a supported deployment state.
func (s Status) Valid() bool {
	switch s {
	case StatusCreated, StatusPreflight, StatusPreparingCertificate,
		StatusProvisioning, StatusCreatingRemnawave, StatusWaitingRemnawave,
		StatusAddingToDNS, StatusCompleted, StatusFailed, StatusCancelled,
		StatusDNSFailed, StatusManualReview:
		return true
	default:
		return false
	}
}

// Terminal reports whether a deployment has no automatic work left to run.
func (s Status) Terminal() bool {
	switch s {
	case StatusCompleted, StatusFailed, StatusCancelled, StatusDNSFailed, StatusManualReview:
		return true
	default:
		return false
	}
}

// StepStatus is the persisted execution state of one deployment step.
type StepStatus string

const (
	StepStatusPending   StepStatus = "PENDING"
	StepStatusRunning   StepStatus = "RUNNING"
	StepStatusCompleted StepStatus = "COMPLETED"
	StepStatusFailed    StepStatus = "FAILED"
	StepStatusSkipped   StepStatus = "SKIPPED"
)

// Valid reports whether s is a supported step state.
func (s StepStatus) Valid() bool {
	switch s {
	case StepStatusPending, StepStatusRunning, StepStatusCompleted,
		StepStatusFailed, StepStatusSkipped:
		return true
	default:
		return false
	}
}

// Terminal reports whether a step has finished running.
func (s StepStatus) Terminal() bool {
	switch s {
	case StepStatusCompleted, StepStatusFailed, StepStatusSkipped:
		return true
	default:
		return false
	}
}

// Deployment is a persisted deployment job. It intentionally contains no VPS
// credentials; the initial root password must only ever exist in memory.
type Deployment struct {
	ID                        string
	PanelID                   string
	TelegramOperatorUserID    int64
	SelectedRemnawaveHostUUID string
	SelectedHostRemark        string
	SNIDomain                 string
	NodeName                  string
	TargetVPSIP               netip.Addr
	RemnawaveNodeUUID         *string
	SSHHostKeyFingerprint     *string
	Status                    Status
	CurrentStep               string
	SafeErrorCode             *string
	SafeErrorMessage          *string
	CreatedAt                 time.Time
	UpdatedAt                 time.Time
	StartedAt                 *time.Time
	CompletedAt               *time.Time
}

// Step is the latest persisted record for a named deployment step.
type Step struct {
	DeploymentID string
	Name         string
	Status       StepStatus
	SafeSummary  *string
	ErrorMessage *string
	StartedAt    *time.Time
	CompletedAt  *time.Time
}

// NewID creates a random RFC 4122 version 4 UUID using the standard library.
func NewID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate deployment UUID: %w", err)
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80

	encoded := make([]byte, 36)
	hex.Encode(encoded[0:8], value[0:4])
	encoded[8] = '-'
	hex.Encode(encoded[9:13], value[4:6])
	encoded[13] = '-'
	hex.Encode(encoded[14:18], value[6:8])
	encoded[18] = '-'
	hex.Encode(encoded[19:23], value[8:10])
	encoded[23] = '-'
	hex.Encode(encoded[24:36], value[10:16])
	return string(encoded), nil
}
