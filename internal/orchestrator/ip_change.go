package orchestrator

import (
	"context"
	"errors"
	"net/netip"
	"strings"

	"remnanode-setup-bot/internal/deployment"
	"remnanode-setup-bot/internal/remnawave"
)

var ErrNodeIPChangeFailed = errors.New("Node IP change failed")

// ReplaceNodeIP changes the address in Remnawave first, waits for the Node to
// reconnect, then replaces the DNS-balancer IP and finally advances the local
// canonical deployment address. Repeating the same command safely reconciles
// partial completion after an API or persistence failure.
func (s *DeploymentService) ReplaceNodeIP(ctx context.Context, deploymentID string, newIP netip.Addr) (string, error) {
	if !newIP.IsValid() || !publicIP(newIP) {
		return "", ErrInvalidInput
	}
	newIP = newIP.Unmap()
	runCtx, release, err := s.beginExecution(ctx, deploymentID)
	if err != nil {
		return "", err
	}
	defer release()
	if err := s.acquire(runCtx); err != nil {
		return "", err
	}
	defer s.releaseSlot()

	current, err := s.repository.GetDeployment(runCtx, deploymentID)
	if err != nil || current.Status != deployment.StatusCompleted || current.RemnawaveNodeUUID == nil {
		return "", ErrDeploymentNotRunnable
	}
	oldIP := current.TargetVPSIP.Unmap()
	if oldIP == newIP {
		return "Node IP is already " + newIP.String(), nil
	}

	nodes, err := s.remnawave.GetNodes(runCtx)
	if err != nil {
		return "", ErrNodeIPChangeFailed
	}
	for _, candidate := range nodes {
		if candidate.UUID == *current.RemnawaveNodeUUID {
			continue
		}
		address, parseErr := netip.ParseAddr(strings.TrimSpace(candidate.Address))
		if parseErr == nil && address.Unmap() == newIP {
			return "", ErrDuplicateNodeAddress
		}
	}

	node, err := s.remnawave.GetNode(runCtx, *current.RemnawaveNodeUUID)
	if err != nil {
		return "", ErrNodeIPChangeFailed
	}
	nodeIP, parseErr := netip.ParseAddr(strings.TrimSpace(node.Address))
	if parseErr != nil || (nodeIP.Unmap() != oldIP && nodeIP.Unmap() != newIP) {
		return "", ErrNodeIPChangeFailed
	}
	if nodeIP.Unmap() == oldIP {
		if _, err := s.remnawave.UpdateNodeAddress(runCtx, remnawave.UpdateNodeAddressInput{UUID: node.UUID, Address: newIP}); err != nil {
			return "", ErrNodeIPChangeFailed
		}
	}
	connected, err := s.pollNode(runCtx, node.UUID, nil)
	connectedIP, connectedParseErr := netip.ParseAddr(strings.TrimSpace(connected.Address))
	if err != nil || connectedParseErr != nil || !connected.IsConnected || connectedIP.Unmap() != newIP {
		return "", ErrNodeIPChangeFailed
	}
	if _, err := s.dns.ReplaceIP(runCtx, current.SNIDomain, oldIP, newIP); err != nil {
		return "", ErrDNSUpdateFailed
	}
	if _, err := s.repository.SetTargetVPSIP(runCtx, current.ID, newIP); err != nil {
		return "", ErrPersistenceFailed
	}
	return "Node IP changed from " + oldIP.String() + " to " + newIP.String(), nil
}
