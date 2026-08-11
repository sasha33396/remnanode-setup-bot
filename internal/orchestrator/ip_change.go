package orchestrator

import (
	"context"
	"errors"
	"net/netip"
	"sort"
	"strconv"
	"strings"

	"remnanode-setup-bot/internal/deployment"
	"remnanode-setup-bot/internal/dnsbalancer"
	"remnanode-setup-bot/internal/remnawave"
)

var (
	ErrNodeIPChangeFailed = errors.New("Node IP change failed")
	ErrNodeNotFound       = errors.New("Node not found")
	ErrAmbiguousNode      = errors.New("ambiguous Node query")
)

type NodeIPTarget struct {
	UUID      string
	Name      string
	Address   netip.Addr
	Connected bool
	DNSZones  []string
	Managed   bool
}

type NodeIPChangeInput struct {
	NodeUUID   string
	ExpectedIP netip.Addr
	NewIP      netip.Addr
}

// FindNodeForIPChange resolves one exact Remnawave Node by name or current IP
// and discovers every DNS-balancer zone that currently contains its address.
func (s *DeploymentService) FindNodeForIPChange(ctx context.Context, query string) (NodeIPTarget, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return NodeIPTarget{}, ErrInvalidInput
	}
	nodes, err := s.remnawave.GetNodes(ctx)
	if err != nil {
		return NodeIPTarget{}, ErrNodeIPChangeFailed
	}
	queryIP, queryIsIP := netip.ParseAddr(query)
	var matches []remnawave.Node
	for _, node := range nodes {
		matched := strings.EqualFold(strings.TrimSpace(node.Name), query)
		if queryIsIP == nil {
			address, parseErr := netip.ParseAddr(strings.TrimSpace(node.Address))
			matched = parseErr == nil && address.Unmap() == queryIP.Unmap()
		}
		if matched {
			matches = append(matches, node)
		}
	}
	if len(matches) == 0 {
		return NodeIPTarget{}, ErrNodeNotFound
	}
	if len(matches) != 1 {
		return NodeIPTarget{}, ErrAmbiguousNode
	}
	node := matches[0]
	address, err := netip.ParseAddr(strings.TrimSpace(node.Address))
	if err != nil || !publicIP(address) {
		return NodeIPTarget{}, ErrNodeIPChangeFailed
	}
	address = address.Unmap()
	zones, err := s.dns.FindZonesByIP(ctx, address)
	if err != nil {
		return NodeIPTarget{}, ErrDNSUpdateFailed
	}
	zoneNames := make([]string, 0, len(zones))
	for _, zone := range zones {
		zoneNames = append(zoneNames, zone.FQDN)
	}
	sort.Strings(zoneNames)
	managed := false
	if items, listErr := s.repository.ListRecentDeployments(ctx, 1000); listErr == nil {
		managed = findManagedDeployment(items, node.UUID) != nil
	}
	return NodeIPTarget{UUID: node.UUID, Name: node.Name, Address: address, Connected: node.IsConnected, DNSZones: zoneNames, Managed: managed}, nil
}

// ReplaceNodeIP updates a Node found directly in Remnawave, so it also works
// for legacy Nodes without a deployment row. ExpectedIP prevents stale wizard
// confirmations from overwriting a newer operator change.
func (s *DeploymentService) ReplaceNodeIP(ctx context.Context, input NodeIPChangeInput) (string, error) {
	if strings.TrimSpace(input.NodeUUID) == "" || !input.ExpectedIP.IsValid() || !input.NewIP.IsValid() || !publicIP(input.NewIP) {
		return "", ErrInvalidInput
	}
	oldIP, newIP := input.ExpectedIP.Unmap(), input.NewIP.Unmap()
	if oldIP == newIP {
		return "IP ноды уже равен " + newIP.String(), nil
	}
	runCtx, release, err := s.beginExecution(ctx, "node-ip:"+strings.TrimSpace(input.NodeUUID))
	if err != nil {
		return "", err
	}
	defer release()
	if err := s.acquire(runCtx); err != nil {
		return "", err
	}
	defer s.releaseSlot()

	nodes, err := s.remnawave.GetNodes(runCtx)
	if err != nil {
		return "", ErrNodeIPChangeFailed
	}
	var selected *remnawave.Node
	for index := range nodes {
		address, parseErr := netip.ParseAddr(strings.TrimSpace(nodes[index].Address))
		if nodes[index].UUID != input.NodeUUID && parseErr == nil && address.Unmap() == newIP {
			return "", ErrDuplicateNodeAddress
		}
		if nodes[index].UUID == input.NodeUUID {
			selected = &nodes[index]
		}
	}
	if selected == nil {
		return "", ErrNodeNotFound
	}
	panelIP, parseErr := netip.ParseAddr(strings.TrimSpace(selected.Address))
	if parseErr != nil || panelIP.Unmap() != oldIP {
		return "", ErrNodeIPChangeFailed
	}

	zones, err := s.dns.FindZonesByIP(runCtx, oldIP)
	if err != nil {
		return "", ErrDNSUpdateFailed
	}
	deployments, repoErr := s.repository.ListRecentDeployments(runCtx, 1000)
	if repoErr != nil {
		return "", ErrPersistenceFailed
	}
	managed := findManagedDeployment(deployments, selected.UUID)

	if _, err := s.remnawave.UpdateNodeAddress(runCtx, remnawave.UpdateNodeAddressInput{UUID: selected.UUID, Address: newIP}); err != nil {
		return "", ErrNodeIPChangeFailed
	}
	changedZones := make([]dnsbalancer.ZoneMatch, 0, len(zones))
	for _, zone := range zones {
		result, replaceErr := s.dns.ReplaceIP(runCtx, zone.FQDN, oldIP, newIP)
		if replaceErr != nil {
			s.rollbackNodeIP(runCtx, selected.UUID, oldIP, newIP, changedZones)
			return "", ErrDNSUpdateFailed
		}
		if result.Changed {
			changedZones = append(changedZones, zone)
		}
	}
	if managed != nil {
		if _, err := s.repository.SetTargetVPSIP(runCtx, managed.ID, newIP); err != nil {
			s.rollbackNodeIP(runCtx, selected.UUID, oldIP, newIP, changedZones)
			return "", ErrPersistenceFailed
		}
	}
	return "IP ноды «" + selected.Name + "» изменён: " + oldIP.String() + " → " + newIP.String() + ". DNS-зон обновлено: " + strconv.Itoa(len(zones)), nil
}

func (s *DeploymentService) rollbackNodeIP(ctx context.Context, nodeUUID string, oldIP, newIP netip.Addr, zones []dnsbalancer.ZoneMatch) {
	for index := len(zones) - 1; index >= 0; index-- {
		_, _ = s.dns.ReplaceIP(ctx, zones[index].FQDN, newIP, oldIP)
	}
	_, _ = s.remnawave.UpdateNodeAddress(ctx, remnawave.UpdateNodeAddressInput{UUID: nodeUUID, Address: oldIP})
}

func findManagedDeployment(items []deployment.Deployment, nodeUUID string) *deployment.Deployment {
	for index := range items {
		if items[index].RemnawaveNodeUUID != nil && *items[index].RemnawaveNodeUUID == nodeUUID {
			return &items[index]
		}
	}
	return nil
}
