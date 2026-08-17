// Package remnawave provides a typed client for the Remnawave HTTP API.
package remnawave

import (
	"net/netip"
	"time"
)

// Host contains the Remnawave Host fields needed to deploy a Node.
type Host struct {
	UUID       string
	Remark     string
	Address    string
	SNI        *string
	IsDisabled bool
	Inbound    HostInbound
}

// HostInbound identifies the config profile and inbound selected by a Host.
// Both fields are nullable in GetAllHostsResponseDto.
type HostInbound struct {
	ConfigProfileUUID        *string
	ConfigProfileInboundUUID *string
}

// DeploymentProfile is the only valid Host-to-Node mapping for deployment.
type DeploymentProfile struct {
	SNIDomain               string
	ActiveConfigProfileUUID string
	ActiveInbounds          []string
}

// NodeConfigProfile is the configProfile body accepted by CreateNodeRequestDto.
type NodeConfigProfile struct {
	ActiveConfigProfileUUID string   `json:"activeConfigProfileUuid"`
	ActiveInbounds          []string `json:"activeInbounds"`
}

// Node contains the identity and polling state exposed by the Node DTOs.
type Node struct {
	UUID                    string
	Name                    string
	Address                 string
	Port                    *int
	IsConnected             bool
	IsConnecting            bool
	IsDisabled              bool
	LastStatusMessage       *string
	LastStatusChange        *time.Time
	ActiveConfigProfileUUID *string
	ActiveInboundUUIDs      []string
}

// NodeMetric is the per-Node online snapshot returned by the system metrics
// endpoint. It is kept separate from Node because /api/nodes does not expose
// live usersOnline values.
type NodeMetric struct {
	NodeUUID    string
	NodeName    string
	UsersOnline int
}

// CreateNodeInput contains operator input plus the selected Host. The API port
// and Node config profile are deliberately not caller-controlled.
type CreateNodeInput struct {
	Name    string
	Address netip.Addr
	Host    Host
}

// UpdateNodeAddressInput changes only the address of an existing Node.
type UpdateNodeAddressInput struct {
	UUID    string
	Address netip.Addr
}
