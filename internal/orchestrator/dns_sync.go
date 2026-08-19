package orchestrator

import (
	"context"
	"errors"
	"net/netip"
	"sort"
	"strings"

	"remnanode-setup-bot/internal/dnsbalancer"
	"remnanode-setup-bot/internal/remnawave"
	"remnanode-setup-bot/internal/repository"
)

var (
	ErrDNSSyncUnavailable = errors.New("DNS synchronization is unavailable")
	ErrDNSTargetUnknown   = errors.New("DNS target for Node is unknown")
)

type NodeDNSSyncTarget struct {
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

const (
	DNSSyncAdded          = "ADDED"
	DNSSyncReplaced       = "REPLACED"
	DNSSyncAlreadyPresent = "ALREADY_PRESENT"
)

// FindNodeForDNSSync resolves one Remnawave Node and previews how its current
// panel address relates to DNS. For managed Nodes the persisted SNI is the
// authoritative DNS target even when the address is absent from every zone.
func (s *DeploymentService) FindNodeForDNSSync(ctx context.Context, query string) (NodeDNSSyncTarget, error) {
	if s.config.DNSDisabled {
		return NodeDNSSyncTarget{}, ErrDNSSyncUnavailable
	}
	node, address, err := s.findExactNode(ctx, query)
	if err != nil {
		return NodeDNSSyncTarget{}, err
	}
	zones, err := s.dns.FindZonesByIP(ctx, address)
	if err != nil {
		return NodeDNSSyncTarget{}, ErrDNSUpdateFailed
	}
	zoneNames := make([]string, 0, len(zones))
	for _, zone := range zones {
		zoneNames = append(zoneNames, zone.FQDN)
	}
	sort.Strings(zoneNames)

	target := NodeDNSSyncTarget{
		UUID:         node.UUID,
		Name:         node.Name,
		Address:      address,
		Connected:    node.IsConnected,
		CurrentZones: zoneNames,
	}
	managed, err := s.repository.FindDeploymentByPanelNodeUUID(ctx, s.config.PanelID, node.UUID)
	if errors.Is(err, repository.ErrNotFound) {
		match, inferred, note, inferErr := s.inferLegacyDNSZone(ctx, node)
		if inferErr != nil {
			return NodeDNSSyncTarget{}, inferErr
		}
		target.Note = note
		if !inferred {
			return target, nil
		}
		target.DNSZone = match.FQDN
		target.CurrentPresent = zoneContainsIP(match.Zone, address)
		target.CanSync = true
		if len(match.Zone.Nodes) != 0 && !target.CurrentPresent {
			target.CanSync = false
			target.Note += "; в расширенной зоне нет старой записи, поэтому служебный address нельзя угадать безопасно"
		} else if target.CurrentPresent {
			target.Note += "; DNS уже соответствует Remnawave"
		} else {
			target.Note += "; актуальный IP будет добавлен, неизвестные старые адреса legacy-ноды удаляться не будут"
		}
		return target, nil
	}
	if err != nil {
		return NodeDNSSyncTarget{}, ErrPersistenceFailed
	}
	match, err := s.dns.FindZone(ctx, managed.SNIDomain)
	if err != nil {
		return NodeDNSSyncTarget{}, ErrDNSUpdateFailed
	}
	target.Managed = true
	target.DNSZone = match.FQDN
	target.PreviousIP = managed.TargetVPSIP.Unmap()
	target.CurrentPresent = zoneContainsIP(match.Zone, address)
	target.PreviousPresent = target.PreviousIP.IsValid() && zoneContainsIP(match.Zone, target.PreviousIP)
	target.CanSync = true
	switch {
	case len(match.Zone.Nodes) != 0 && !target.CurrentPresent && !target.PreviousPresent:
		target.CanSync = false
		target.Note = "В расширенной DNS-зоне нет старой записи ноды; адрес назначения нельзя угадать безопасно"
	case target.CurrentPresent && target.PreviousIP != address && target.PreviousPresent:
		target.Note = "Актуальный IP уже есть; устаревший IP будет удалён из целевой зоны"
	case target.CurrentPresent:
		target.Note = "DNS уже соответствует Remnawave"
	case target.PreviousIP != address && target.PreviousPresent:
		target.Note = "Устаревший IP будет заменён актуальным IP из Remnawave"
	default:
		target.Note = "Актуальный IP из Remnawave будет добавлен в DNS"
	}
	return target, nil
}

// SyncNodeDNS makes DNS match Remnawave without ever changing the Node. The
// expected address prevents a stale Telegram confirmation from applying after
// another operator changed the panel.
func (s *DeploymentService) SyncNodeDNS(ctx context.Context, input NodeDNSSyncInput) (NodeDNSSyncResult, error) {
	if s.config.DNSDisabled {
		return NodeDNSSyncResult{}, ErrDNSSyncUnavailable
	}
	if strings.TrimSpace(input.NodeUUID) == "" || !input.ExpectedIP.IsValid() || !publicIP(input.ExpectedIP) {
		return NodeDNSSyncResult{}, ErrInvalidInput
	}
	runCtx, release, err := s.beginExecution(ctx, "node-dns:"+strings.TrimSpace(input.NodeUUID))
	if err != nil {
		return NodeDNSSyncResult{}, err
	}
	defer release()
	if err := s.acquire(runCtx); err != nil {
		return NodeDNSSyncResult{}, err
	}
	defer s.releaseSlot()

	node, address, err := s.findNodeByUUID(runCtx, input.NodeUUID)
	if err != nil {
		return NodeDNSSyncResult{}, err
	}
	if address != input.ExpectedIP.Unmap() {
		return NodeDNSSyncResult{}, ErrNodeIPChangeFailed
	}
	managed, err := s.repository.FindDeploymentByPanelNodeUUID(runCtx, s.config.PanelID, node.UUID)
	managedDeployment := true
	var match dnsbalancer.ZoneMatch
	var previous netip.Addr
	if errors.Is(err, repository.ErrNotFound) {
		managedDeployment = false
		var inferred bool
		match, inferred, _, err = s.inferLegacyDNSZone(runCtx, node)
		if err != nil {
			return NodeDNSSyncResult{}, err
		}
		if !inferred {
			return NodeDNSSyncResult{}, ErrDNSTargetUnknown
		}
	} else if err != nil {
		return NodeDNSSyncResult{}, ErrPersistenceFailed
	} else {
		match, err = s.dns.FindZone(runCtx, managed.SNIDomain)
		if err != nil {
			return NodeDNSSyncResult{}, ErrDNSUpdateFailed
		}
		previous = managed.TargetVPSIP.Unmap()
	}
	currentPresent := zoneContainsIP(match.Zone, address)
	previousPresent := previous.IsValid() && previous != address && zoneContainsIP(match.Zone, previous)
	action := DNSSyncAlreadyPresent
	if previousPresent {
		result, replaceErr := s.dns.ReplaceIP(runCtx, match.FQDN, previous, address)
		if replaceErr != nil {
			return NodeDNSSyncResult{}, ErrDNSUpdateFailed
		}
		if result.Changed {
			action = DNSSyncReplaced
		}
	} else if !currentPresent {
		if len(match.Zone.Nodes) != 0 {
			return NodeDNSSyncResult{}, ErrDNSTargetUnknown
		}
		result, addErr := s.dns.AddIP(runCtx, match.FQDN, address)
		if addErr != nil {
			return NodeDNSSyncResult{}, ErrDNSUpdateFailed
		}
		if result.Added {
			action = DNSSyncAdded
		}
	}
	if managedDeployment && previous != address {
		if _, err := s.repository.SetTargetVPSIP(runCtx, managed.ID, address); err != nil {
			// DNS already follows the source of truth. Do not roll it back; the
			// idempotent next run will only repair persistence.
			return NodeDNSSyncResult{}, ErrPersistenceFailed
		}
	}
	return NodeDNSSyncResult{NodeName: node.Name, Address: address, DNSZone: match.FQDN, Action: action, PreviousIP: previous}, nil
}

// inferLegacyDNSZone maps a legacy Node to a Host only when both the active
// config profile and one active inbound match. Different Host addresses for
// the same profile/inbound are deliberately treated as ambiguous.
func (s *DeploymentService) inferLegacyDNSZone(ctx context.Context, node remnawave.Node) (dnsbalancer.ZoneMatch, bool, string, error) {
	if node.ActiveConfigProfileUUID == nil || strings.TrimSpace(*node.ActiveConfigProfileUUID) == "" || len(node.ActiveInboundUUIDs) == 0 {
		return dnsbalancer.ZoneMatch{}, false, "Legacy-нода не содержит полного профиля и inbound для определения DNS-зоны", nil
	}
	hosts, err := s.remnawave.GetHosts(ctx)
	if err != nil {
		return dnsbalancer.ZoneMatch{}, false, "", ErrNodeIPChangeFailed
	}
	activeInbounds := make(map[string]struct{}, len(node.ActiveInboundUUIDs))
	for _, inbound := range node.ActiveInboundUUIDs {
		if value := strings.TrimSpace(inbound); value != "" {
			activeInbounds[value] = struct{}{}
		}
	}
	profileUUID := strings.TrimSpace(*node.ActiveConfigProfileUUID)
	candidates := make(map[string]string)
	for _, host := range hosts {
		if host.IsDisabled {
			continue
		}
		profile, profileErr := remnawave.DeploymentProfileFromHost(host)
		if profileErr != nil || profile.ActiveConfigProfileUUID != profileUUID || len(profile.ActiveInbounds) != 1 {
			continue
		}
		if _, matched := activeInbounds[profile.ActiveInbounds[0]]; !matched {
			continue
		}
		key := normalizeDomain(profile.SNIDomain)
		if key != "" {
			candidates[key] = profile.SNIDomain
		}
	}
	if len(candidates) == 0 {
		return dnsbalancer.ZoneMatch{}, false, "Для профиля и inbound legacy-ноды не найден подходящий активный Host", nil
	}
	if len(candidates) != 1 {
		return dnsbalancer.ZoneMatch{}, false, "Профиль и inbound legacy-ноды соответствуют нескольким Host с разными SNI; выбор зоны неоднозначен", nil
	}
	var sni string
	for _, value := range candidates {
		sni = value
	}
	match, err := s.dns.FindZone(ctx, sni)
	if err != nil {
		return dnsbalancer.ZoneMatch{}, false, "", ErrDNSUpdateFailed
	}
	return match, true, "Целевая зона определена по совпадению профиля и inbound ноды с Host", nil
}

func (s *DeploymentService) findExactNode(ctx context.Context, query string) (remnawave.Node, netip.Addr, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return remnawave.Node{}, netip.Addr{}, ErrInvalidInput
	}
	nodes, err := s.remnawave.GetNodes(ctx)
	if err != nil {
		return remnawave.Node{}, netip.Addr{}, ErrNodeIPChangeFailed
	}
	queryIP, queryIPErr := netip.ParseAddr(query)
	matches := make([]remnawave.Node, 0, 1)
	for _, node := range nodes {
		matched := strings.EqualFold(strings.TrimSpace(node.Name), query)
		if queryIPErr == nil {
			address, parseErr := netip.ParseAddr(strings.TrimSpace(node.Address))
			matched = parseErr == nil && address.Unmap() == queryIP.Unmap()
		}
		if matched {
			matches = append(matches, node)
		}
	}
	if len(matches) == 0 {
		return remnawave.Node{}, netip.Addr{}, ErrNodeNotFound
	}
	if len(matches) != 1 {
		return remnawave.Node{}, netip.Addr{}, ErrAmbiguousNode
	}
	address, err := netip.ParseAddr(strings.TrimSpace(matches[0].Address))
	if err != nil || !publicIP(address) {
		return remnawave.Node{}, netip.Addr{}, ErrNodeIPChangeFailed
	}
	return matches[0], address.Unmap(), nil
}

func (s *DeploymentService) findNodeByUUID(ctx context.Context, uuid string) (remnawave.Node, netip.Addr, error) {
	nodes, err := s.remnawave.GetNodes(ctx)
	if err != nil {
		return remnawave.Node{}, netip.Addr{}, ErrNodeIPChangeFailed
	}
	for _, node := range nodes {
		if node.UUID != strings.TrimSpace(uuid) {
			continue
		}
		address, parseErr := netip.ParseAddr(strings.TrimSpace(node.Address))
		if parseErr != nil || !publicIP(address) {
			return remnawave.Node{}, netip.Addr{}, ErrNodeIPChangeFailed
		}
		return node, address.Unmap(), nil
	}
	return remnawave.Node{}, netip.Addr{}, ErrNodeNotFound
}

func zoneContainsIP(zone dnsbalancer.Zone, address netip.Addr) bool {
	if !address.IsValid() {
		return false
	}
	address = address.Unmap()
	for _, value := range zone.IPs {
		parsed, err := netip.ParseAddr(strings.TrimSpace(value))
		if err == nil && parsed.Unmap() == address {
			return true
		}
	}
	for _, node := range zone.Nodes {
		parsed, err := netip.ParseAddr(strings.TrimSpace(node.IP))
		if err == nil && parsed.Unmap() == address {
			return true
		}
	}
	return false
}
