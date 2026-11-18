// Package leastload plans and executes narrowly scoped, recoverable IP changes.
package leastload

import (
	"context"
	"remnanode-setup-bot/internal/networkinventory"
	"time"
)

const (
	ModeNetwork      = "network"
	ModeControlPlane = "control_plane"
)

type Mapping struct {
	NodeUUID string `json:"nodeUuid"`
	OldIP    string `json:"oldIp"`
	NewIP    string `json:"newIp"`
}
type Candidate struct {
	NodeUUID      string `json:"nodeUuid"`
	Name          string `json:"name"`
	Address       string `json:"address"`
	Domain        string `json:"domain"`
	Reason        string `json:"reason,omitempty"`
	NetworkReason string `json:"networkReason,omitempty"`
}
type Plan struct {
	OldIP      string                  `json:"oldIp"`
	NewIP      string                  `json:"newIp"`
	Mode       string                  `json:"mode"`
	File       string                  `json:"file"`
	Section    string                  `json:"section"`
	Interface  string                  `json:"interface"`
	FileHash   string                  `json:"fileHash"`
	TargetHash string                  `json:"targetHash"`
	Prefix     int                     `json:"prefix"`
	Before     networkinventory.Report `json:"before"`
}
type Item struct {
	ID           string  `json:"id"`
	Mapping      Mapping `json:"mapping"`
	Name         string  `json:"name"`
	DeploymentID string  `json:"deploymentId"`
	HostID       string  `json:"hostId"`
	Domain       string  `json:"domain"`
	Plan         Plan    `json:"plan"`
	Status       string  `json:"status"`
	Message      string  `json:"message"`
}
type Job struct {
	ID        string    `json:"id"`
	PanelID   string    `json:"panelId"`
	Mode      string    `json:"mode,omitempty"`
	Operator  int64     `json:"operator"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	Items     []Item    `json:"items"`
}
type Store interface {
	SaveLeastloadJob(context.Context, Job) error
	GetLeastloadJob(context.Context, string) (Job, error)
	ListLeastloadJobs(context.Context) ([]Job, error)
	HasPendingLeastload(context.Context, string, string) (bool, error)
	Lock(context.Context, string) (func(), error)
}
type Error string

func (e Error) Error() string           { return string(e) }
func (e Error) OperatorMessage() string { return string(e) }
