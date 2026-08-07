package remnawave

import (
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"unicode/utf8"
)

var (
	// ErrInvalidHostProfile indicates that a Host cannot provide the required
	// Node config profile identifiers.
	ErrInvalidHostProfile = errors.New("invalid Remnawave Host profile")
	// ErrDuplicateNode indicates a conflicting existing Node.
	ErrDuplicateNode = errors.New("duplicate Remnawave Node")
	// ErrInvalidInput indicates invalid caller-supplied data.
	ErrInvalidInput = errors.New("invalid Remnawave client input")
)

// DuplicateError identifies which uniqueness checks failed.
type DuplicateError struct {
	Name    bool
	Address bool
}

func (e *DuplicateError) Error() string {
	switch {
	case e.Name && e.Address:
		return "duplicate Remnawave Node name and address"
	case e.Name:
		return "duplicate Remnawave Node name"
	default:
		return "duplicate Remnawave Node address"
	}
}

func (e *DuplicateError) Unwrap() error { return ErrDuplicateNode }

// DeploymentProfileFromHost validates and maps a selected Host. SNI_DOMAIN is
// always Host.Address; Host.SNI is intentionally ignored.
func DeploymentProfileFromHost(host Host) (DeploymentProfile, error) {
	address := strings.TrimSpace(host.Address)
	if address == "" {
		return DeploymentProfile{}, fmt.Errorf("Host address is missing: %w", ErrInvalidHostProfile)
	}
	if host.Inbound.ConfigProfileUUID == nil || strings.TrimSpace(*host.Inbound.ConfigProfileUUID) == "" {
		return DeploymentProfile{}, fmt.Errorf("Host inbound config profile UUID is missing: %w", ErrInvalidHostProfile)
	}
	if host.Inbound.ConfigProfileInboundUUID == nil || strings.TrimSpace(*host.Inbound.ConfigProfileInboundUUID) == "" {
		return DeploymentProfile{}, fmt.Errorf("Host inbound profile inbound UUID is missing: %w", ErrInvalidHostProfile)
	}

	profileUUID := strings.TrimSpace(*host.Inbound.ConfigProfileUUID)
	inboundUUID := strings.TrimSpace(*host.Inbound.ConfigProfileInboundUUID)
	if !validUUID(profileUUID) || !validUUID(inboundUUID) {
		return DeploymentProfile{}, fmt.Errorf("Host inbound profile UUID is invalid: %w", ErrInvalidHostProfile)
	}
	return DeploymentProfile{
		SNIDomain:               address,
		ActiveConfigProfileUUID: profileUUID,
		ActiveInbounds:          []string{inboundUUID},
	}, nil
}

// CheckNodeDuplicates checks the manually entered name and target IP against a
// Node snapshot returned by GetNodes.
func CheckNodeDuplicates(nodes []Node, name string, address netip.Addr) error {
	trimmedName := strings.TrimSpace(name)
	if !validNodeName(trimmedName) || !address.IsValid() {
		return fmt.Errorf("Node name or address: %w", ErrInvalidInput)
	}
	duplicate := &DuplicateError{}
	for _, node := range nodes {
		if strings.TrimSpace(node.Name) == trimmedName {
			duplicate.Name = true
		}
		nodeAddress, err := netip.ParseAddr(strings.TrimSpace(node.Address))
		if err == nil && nodeAddress == address {
			duplicate.Address = true
		}
	}
	if duplicate.Name || duplicate.Address {
		return duplicate
	}
	return nil
}

func validNodeName(name string) bool {
	length := utf8.RuneCountInString(name)
	return length >= 3 && length <= 30
}

func validUUID(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return false
	}
	decoded := make([]byte, 16)
	_, err := hex.Decode(decoded, []byte(strings.ReplaceAll(value, "-", "")))
	return err == nil
}
