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

const MaxBulkNodeIPChanges = 50

type BulkNodeIPMapping struct {
	OldIP netip.Addr
	NewIP netip.Addr
}

type BulkNodeIPPlanItem struct {
	NodeUUID    string
	NodeName    string
	OldIP       netip.Addr
	NewIP       netip.Addr
	DNSZones    []string
	Managed     bool
	WasDisabled bool
}

type BulkNodeIPPlan struct {
	Items         []BulkNodeIPPlanItem
	DisabledCount int
	WithoutDNS    int
}

type BulkNodeIPApplyInput struct {
	Items          []BulkNodeIPPlanItem
	EnableDisabled bool
}

type BulkNodeIPItemStatus string

const (
	BulkNodeIPCompleted BulkNodeIPItemStatus = "COMPLETED"
	BulkNodeIPWarning   BulkNodeIPItemStatus = "WARNING"
	BulkNodeIPFailed    BulkNodeIPItemStatus = "FAILED"
)

type BulkNodeIPItemResult struct {
	Plan                BulkNodeIPPlanItem
	Status              BulkNodeIPItemStatus
	RemnawaveUpdated    bool
	DNSZonesUpdated     int
	DNSComplete         bool
	PersistenceComplete bool
	EnableAttempted     bool
	Enabled             bool
	Connected           bool
	Connecting          bool
	LastStatusMessage   string
	SafeMessage         string
}

type BulkNodeIPProgress struct {
	Completed int
	Total     int
	Item      BulkNodeIPItemResult
}

type BulkNodeIPResult struct {
	Items     []BulkNodeIPItemResult
	Completed int
	Warnings  int
	Failed    int
}

// PrepareBulkNodeIPChange validates a complete operator-supplied mapping before
// any mutation. Disabled and unhealthy Nodes remain valid targets.
func (s *DeploymentService) PrepareBulkNodeIPChange(ctx context.Context, mappings []BulkNodeIPMapping) (BulkNodeIPPlan, error) {
	if len(mappings) == 0 || len(mappings) > MaxBulkNodeIPChanges {
		return BulkNodeIPPlan{}, ErrInvalidInput
	}
	oldIPs := make(map[netip.Addr]struct{}, len(mappings))
	newIPs := make(map[netip.Addr]struct{}, len(mappings))
	normalized := make([]BulkNodeIPMapping, len(mappings))
	for index, mapping := range mappings {
		oldIP, newIP := mapping.OldIP.Unmap(), mapping.NewIP.Unmap()
		if !oldIP.Is4() || !newIP.Is4() || !publicIP(oldIP) || !publicIP(newIP) || oldIP == newIP {
			return BulkNodeIPPlan{}, ErrInvalidInput
		}
		if _, duplicate := oldIPs[oldIP]; duplicate {
			return BulkNodeIPPlan{}, ErrInvalidInput
		}
		if _, duplicate := newIPs[newIP]; duplicate {
			return BulkNodeIPPlan{}, ErrInvalidInput
		}
		oldIPs[oldIP] = struct{}{}
		newIPs[newIP] = struct{}{}
		normalized[index] = BulkNodeIPMapping{OldIP: oldIP, NewIP: newIP}
	}
	for newIP := range newIPs {
		if _, isCurrentBatchIP := oldIPs[newIP]; isCurrentBatchIP {
			// Address swaps and cycles need a separate staged algorithm.
			return BulkNodeIPPlan{}, ErrInvalidInput
		}
	}

	nodes, err := s.remnawave.GetNodes(ctx)
	if err != nil {
		return BulkNodeIPPlan{}, ErrNodeIPChangeFailed
	}
	byAddress := make(map[netip.Addr][]remnawave.Node, len(nodes))
	for _, node := range nodes {
		address, parseErr := netip.ParseAddr(strings.TrimSpace(node.Address))
		if parseErr == nil {
			address = address.Unmap()
			byAddress[address] = append(byAddress[address], node)
		}
	}

	plan := BulkNodeIPPlan{Items: make([]BulkNodeIPPlanItem, 0, len(normalized))}
	for _, mapping := range normalized {
		matches := byAddress[mapping.OldIP]
		if len(matches) == 0 {
			return BulkNodeIPPlan{}, ErrNodeNotFound
		}
		if len(matches) != 1 {
			return BulkNodeIPPlan{}, ErrAmbiguousNode
		}
		node := matches[0]
		for _, conflict := range byAddress[mapping.NewIP] {
			if conflict.UUID != node.UUID {
				return BulkNodeIPPlan{}, ErrDuplicateNodeAddress
			}
		}

		var zones []dnsbalancer.ZoneMatch
		if !s.config.DNSDisabled {
			zones, err = s.dns.FindZonesByIP(ctx, mapping.OldIP)
			if err != nil {
				return BulkNodeIPPlan{}, ErrDNSUpdateFailed
			}
		}
		zoneNames := make([]string, 0, len(zones))
		for _, zone := range zones {
			zoneNames = append(zoneNames, zone.FQDN)
		}
		sort.Strings(zoneNames)

		_, managedErr := s.repository.FindDeploymentByPanelNodeUUID(ctx, s.config.PanelID, node.UUID)
		if managedErr != nil && !errors.Is(managedErr, repository.ErrNotFound) {
			return BulkNodeIPPlan{}, ErrPersistenceFailed
		}
		item := BulkNodeIPPlanItem{
			NodeUUID: node.UUID, NodeName: node.Name, OldIP: mapping.OldIP,
			NewIP: mapping.NewIP, DNSZones: zoneNames, Managed: managedErr == nil,
			WasDisabled: node.IsDisabled,
		}
		plan.Items = append(plan.Items, item)
		if item.WasDisabled {
			plan.DisabledCount++
		}
		if len(item.DNSZones) == 0 {
			plan.WithoutDNS++
		}
	}
	return plan, nil
}

// ApplyBulkNodeIPChange treats the operator-supplied new address as the source
// of truth because the hosting provider has already changed the server IP. It
// therefore never rolls Remnawave back to the obsolete address after a partial
// DNS failure. Repeating the same plan is idempotent.
func (s *DeploymentService) ApplyBulkNodeIPChange(ctx context.Context, input BulkNodeIPApplyInput, progress func(BulkNodeIPProgress)) (BulkNodeIPResult, error) {
	if len(input.Items) == 0 || len(input.Items) > MaxBulkNodeIPChanges {
		return BulkNodeIPResult{}, ErrInvalidInput
	}
	result := BulkNodeIPResult{Items: make([]BulkNodeIPItemResult, 0, len(input.Items))}
	for index, item := range input.Items {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		itemResult := s.applyBulkNodeIPItem(ctx, item, input.EnableDisabled)
		result.Items = append(result.Items, itemResult)
		switch itemResult.Status {
		case BulkNodeIPCompleted:
			result.Completed++
		case BulkNodeIPWarning:
			result.Warnings++
		default:
			result.Failed++
		}
		if progress != nil {
			progress(BulkNodeIPProgress{Completed: index + 1, Total: len(input.Items), Item: itemResult})
		}
	}
	return result, nil
}

func (s *DeploymentService) applyBulkNodeIPItem(ctx context.Context, item BulkNodeIPPlanItem, enableDisabled bool) BulkNodeIPItemResult {
	result := BulkNodeIPItemResult{Plan: item, Status: BulkNodeIPFailed}
	if strings.TrimSpace(item.NodeUUID) == "" || !item.OldIP.Is4() || !item.NewIP.Is4() || item.OldIP == item.NewIP {
		result.SafeMessage = "Некорректный план смены IP"
		return result
	}
	runCtx, release, err := s.beginExecution(ctx, "bulk-node-ip:"+strings.TrimSpace(item.NodeUUID))
	if err != nil {
		result.SafeMessage = "Операция для ноды уже выполняется"
		return result
	}
	defer release()
	if err := s.acquire(runCtx); err != nil {
		result.SafeMessage = "Не удалось запустить операцию"
		return result
	}
	defer s.releaseSlot()

	nodes, err := s.remnawave.GetNodes(runCtx)
	if err != nil {
		result.SafeMessage = "Панель временно недоступна"
		return result
	}
	var selected *remnawave.Node
	for index := range nodes {
		address, parseErr := netip.ParseAddr(strings.TrimSpace(nodes[index].Address))
		if parseErr == nil && nodes[index].UUID != item.NodeUUID && address.Unmap() == item.NewIP.Unmap() {
			result.SafeMessage = "Новый IP уже используется другой нодой"
			return result
		}
		if nodes[index].UUID == item.NodeUUID {
			selected = &nodes[index]
		}
	}
	if selected == nil {
		result.SafeMessage = "Нода больше не найдена в панели"
		return result
	}
	if selected.IsDisabled {
		result.Plan.WasDisabled = true
	}
	currentIP, parseErr := netip.ParseAddr(strings.TrimSpace(selected.Address))
	if parseErr != nil {
		result.SafeMessage = "Текущий IP ноды некорректен"
		return result
	}
	currentIP = currentIP.Unmap()
	switch currentIP {
	case item.OldIP.Unmap():
		if _, err := s.remnawave.UpdateNodeAddress(runCtx, remnawave.UpdateNodeAddressInput{UUID: item.NodeUUID, Address: item.NewIP.Unmap()}); err != nil {
			result.SafeMessage = "Не удалось обновить адрес ноды в Remnawave"
			return result
		}
	case item.NewIP.Unmap():
		// A previous attempt already changed Remnawave; continue with DNS.
	default:
		result.SafeMessage = "IP ноды изменился после предварительной проверки"
		return result
	}
	result.RemnawaveUpdated = true

	result.DNSComplete = true
	for _, zone := range item.DNSZones {
		replaced, replaceErr := s.dns.ReplaceIP(runCtx, zone, item.OldIP.Unmap(), item.NewIP.Unmap())
		if replaceErr != nil {
			result.DNSComplete = false
			continue
		}
		if replaced.Changed {
			result.DNSZonesUpdated++
		}
	}

	result.PersistenceComplete = !item.Managed
	if item.Managed {
		managed, findErr := s.repository.FindDeploymentByPanelNodeUUID(runCtx, s.config.PanelID, item.NodeUUID)
		if findErr == nil {
			_, findErr = s.repository.SetTargetVPSIP(runCtx, managed.ID, item.NewIP.Unmap())
		}
		result.PersistenceComplete = findErr == nil
	}

	if enableDisabled && result.Plan.WasDisabled && result.DNSComplete && result.PersistenceComplete {
		result.EnableAttempted = true
		enabler, available := s.remnawave.(interface {
			SetNodeDisabled(context.Context, remnawave.SetNodeDisabledInput) (remnawave.Node, error)
		})
		if available {
			updated, enableErr := enabler.SetNodeDisabled(runCtx, remnawave.SetNodeDisabledInput{UUID: item.NodeUUID, Disabled: false})
			if enableErr == nil {
				if fresh, freshErr := s.remnawave.GetNode(runCtx, item.NodeUUID); freshErr == nil {
					updated = fresh
				}
				result.Enabled = !updated.IsDisabled
				result.Connected = updated.IsConnected
				result.Connecting = updated.IsConnecting
				if updated.LastStatusMessage != nil {
					result.LastStatusMessage = strings.TrimSpace(*updated.LastStatusMessage)
				}
			}
		}
	}

	switch {
	case !result.DNSComplete:
		result.Status = BulkNodeIPWarning
		result.SafeMessage = "Remnawave обновлён, но DNS требует повтора"
	case !result.PersistenceComplete:
		result.Status = BulkNodeIPWarning
		result.SafeMessage = "IP обновлён, но локальная запись требует повтора"
	case result.EnableAttempted && !result.Enabled:
		result.Status = BulkNodeIPWarning
		result.SafeMessage = "IP и DNS обновлены, но включить ноду не удалось"
	default:
		result.Status = BulkNodeIPCompleted
		if len(item.DNSZones) == 0 {
			result.SafeMessage = "IP обновлён; DNS-зоны со старым адресом не найдены"
		}
	}
	return result
}
