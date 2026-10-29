package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"

	"remnanode-setup-bot/internal/provisioner"
	"remnanode-setup-bot/internal/repository"
	sshclient "remnanode-setup-bot/internal/ssh"
	"remnanode-setup-bot/internal/telegram"
)

const netplanPath = "/etc/netplan/50-cloud-init.yaml"

const nodeNetworkPollConcurrency = 8

type NodeFirewallConfig struct {
	PanelIP   netip.Addr
	MetricsIP netip.Addr
}

type NodeMaintenance struct {
	app         *TelegramApplication
	store       repository.NodeNetworkStore
	managedSSH  *sshclient.Client
	legacySSH   *sshclient.Client
	firewall    map[string]NodeFirewallConfig
	timeout     time.Duration
	logger      *slog.Logger
	nodeLocks   sync.Map
	bulkMu      sync.Mutex
	bulkRunning bool
}

func NewNodeMaintenance(app *TelegramApplication, store repository.NodeNetworkStore, managedSSH, legacySSH *sshclient.Client, firewall map[string]NodeFirewallConfig, timeout time.Duration, logger *slog.Logger) (*NodeMaintenance, error) {
	if app == nil || store == nil || managedSSH == nil || legacySSH == nil || len(firewall) == 0 || timeout <= 0 {
		return nil, errors.New("invalid node maintenance configuration")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &NodeMaintenance{app: app, store: store, managedSSH: managedSSH, legacySSH: legacySSH, firewall: firewall, timeout: timeout, logger: logger}, nil
}

func (a *TelegramApplication) SetNodeMaintenance(service *NodeMaintenance) error {
	if a == nil || service == nil {
		return errors.New("node maintenance service is required")
	}
	a.maintenance = service
	return nil
}

func (a *TelegramApplication) ActualizeNode(ctx context.Context, input telegram.NodeActualizeInput) (telegram.NodeActualizeResult, error) {
	if a == nil || a.maintenance == nil {
		return telegram.NodeActualizeResult{}, errors.New("node maintenance is unavailable")
	}
	return a.maintenance.ActualizeNode(ctx, input)
}

func (a *TelegramApplication) ActualizeManagedNodes(ctx context.Context) (telegram.ManagedNodesActualizeResult, error) {
	if a == nil || a.maintenance == nil {
		return telegram.ManagedNodesActualizeResult{}, errors.New("node maintenance is unavailable")
	}
	return a.maintenance.ActualizeManagedNodes(ctx)
}

func (m *NodeMaintenance) ActualizeNode(ctx context.Context, input telegram.NodeActualizeInput) (telegram.NodeActualizeResult, error) {
	defer clear(input.Password)
	panel, err := m.app.panel(input.PanelID)
	if err != nil {
		return telegram.NodeActualizeResult{}, err
	}
	nodes, err := panel.Service.remnawave.GetNodes(ctx)
	if err != nil {
		return telegram.NodeActualizeResult{}, errors.New("load node failed")
	}
	var name, rawAddress string
	for _, node := range nodes {
		if node.UUID == input.NodeUUID {
			name, rawAddress = node.Name, node.Address
			break
		}
	}
	address, err := netip.ParseAddr(strings.TrimSpace(rawAddress))
	if name == "" || err != nil || !address.IsGlobalUnicast() || address.IsPrivate() {
		return telegram.NodeActualizeResult{}, errors.New("node not found")
	}
	unlock := m.lockNode(input.PanelID, input.NodeUUID)
	defer unlock()
	connection, managed, err := m.connect(ctx, input.PanelID, input.NodeUUID, address.Unmap(), input.Password)
	if err != nil {
		return telegram.NodeActualizeResult{}, errors.New("SSH connection failed")
	}
	defer connection.Close()
	addresses, err := readNetplanAddresses(ctx, connection, m.timeout)
	if err != nil {
		return telegram.NodeActualizeResult{}, err
	}
	config, ok := m.firewall[input.PanelID]
	if !ok {
		return telegram.NodeActualizeResult{}, errors.New("panel firewall configuration is unavailable")
	}
	changed, err := provisioner.ReconcileFirewall(ctx, connection, config.PanelIP, config.MetricsIP, m.timeout)
	if err != nil {
		return telegram.NodeActualizeResult{}, errors.New("firewall reconciliation failed")
	}
	if err := m.store.UpsertNodeNetworkInventory(ctx, repository.NodeNetworkInventory{PanelID: input.PanelID, NodeUUID: input.NodeUUID, Addresses: addresses}); err != nil {
		return telegram.NodeActualizeResult{}, err
	}
	return telegram.NodeActualizeResult{NodeName: name, Managed: managed, Addresses: addresses, FirewallChanged: changed}, nil
}

type managedActualizeTarget struct {
	panelID      string
	panelName    string
	nodeUUID     string
	nodeName     string
	deploymentID string
	address      netip.Addr
}

func (m *NodeMaintenance) ActualizeManagedNodes(ctx context.Context) (telegram.ManagedNodesActualizeResult, error) {
	m.bulkMu.Lock()
	if m.bulkRunning {
		m.bulkMu.Unlock()
		return telegram.ManagedNodesActualizeResult{}, errors.New("managed Node bulk actualization is already running")
	}
	m.bulkRunning = true
	m.bulkMu.Unlock()
	defer func() {
		m.bulkMu.Lock()
		m.bulkRunning = false
		m.bulkMu.Unlock()
	}()

	targets := make([]managedActualizeTarget, 0)
	for _, panelID := range m.app.order {
		panel := m.app.panels[panelID]
		nodes, err := panel.Service.remnawave.GetNodes(ctx)
		if err != nil {
			return telegram.ManagedNodesActualizeResult{}, errors.New("load managed Nodes failed")
		}
		for _, node := range nodes {
			item, err := m.app.repository.FindDeploymentByPanelNodeUUID(ctx, panelID, node.UUID)
			if errors.Is(err, repository.ErrNotFound) {
				continue
			}
			if err != nil {
				return telegram.ManagedNodesActualizeResult{}, errors.New("classify managed Node failed")
			}
			address, parseErr := netip.ParseAddr(strings.TrimSpace(node.Address))
			if parseErr != nil || !address.IsGlobalUnicast() || address.IsPrivate() {
				address = netip.Addr{}
			}
			targets = append(targets, managedActualizeTarget{panelID: panelID, panelName: panel.Name, nodeUUID: node.UUID, nodeName: node.Name, deploymentID: item.ID, address: address.Unmap()})
		}
	}

	type outcome struct {
		changed bool
		reason  string
	}
	outcomes := make([]outcome, len(targets))
	semaphore := make(chan struct{}, nodeNetworkPollConcurrency)
	var workers sync.WaitGroup
	for index, target := range targets {
		index, target := index, target
		workers.Add(1)
		go func() {
			defer workers.Done()
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-ctx.Done():
				outcomes[index].reason = "cancelled"
				return
			}
			outcomes[index].changed, outcomes[index].reason = m.actualizeManagedTarget(ctx, target)
		}()
	}
	workers.Wait()

	result := telegram.ManagedNodesActualizeResult{Total: len(targets)}
	for index, target := range targets {
		outcome := outcomes[index]
		if outcome.reason == "" {
			result.Updated++
			if outcome.changed {
				result.FirewallChanged++
			}
			continue
		}
		result.Failed++
		result.Failures = append(result.Failures, telegram.ManagedNodeActualizeFailure{PanelName: target.panelName, NodeName: target.nodeName, Reason: outcome.reason})
	}
	m.logger.Info("managed node bulk actualization completed", slog.Int("total", result.Total), slog.Int("updated", result.Updated), slog.Int("failed", result.Failed), slog.Int("firewall_changed", result.FirewallChanged))
	return result, nil
}

func (m *NodeMaintenance) actualizeManagedTarget(ctx context.Context, target managedActualizeTarget) (bool, string) {
	if !target.address.IsValid() {
		return false, "invalid_address"
	}
	unlock := m.lockNode(target.panelID, target.nodeUUID)
	defer unlock()
	connection, err := m.managedSSH.ConnectWithDeploymentKey(ctx, target.deploymentID, target.address, "root")
	if err != nil {
		return false, "ssh_connect"
	}
	defer connection.Close()
	addresses, err := readNetplanAddresses(ctx, connection, m.timeout)
	if err != nil {
		return false, "netplan_read"
	}
	config, ok := m.firewall[target.panelID]
	if !ok {
		return false, "firewall_config"
	}
	changed, err := provisioner.ReconcileFirewall(ctx, connection, config.PanelIP, config.MetricsIP, m.timeout)
	if err != nil {
		return false, "firewall_reconcile"
	}
	if err := m.store.UpsertNodeNetworkInventory(ctx, repository.NodeNetworkInventory{PanelID: target.panelID, NodeUUID: target.nodeUUID, Addresses: addresses}); err != nil {
		return changed, "inventory_save"
	}
	return changed, ""
}

func (m *NodeMaintenance) lockNode(panelID, nodeUUID string) func() {
	value, _ := m.nodeLocks.LoadOrStore(panelID+"\x00"+nodeUUID, &sync.Mutex{})
	mutex := value.(*sync.Mutex)
	mutex.Lock()
	return mutex.Unlock
}

func (m *NodeMaintenance) connect(ctx context.Context, panelID, nodeUUID string, address netip.Addr, password []byte) (*sshclient.Connection, bool, error) {
	item, err := m.app.repository.FindDeploymentByPanelNodeUUID(ctx, panelID, nodeUUID)
	if err == nil {
		connection, connectErr := m.managedSSH.ConnectWithDeploymentKey(ctx, item.ID, address, "root")
		return connection, true, connectErr
	}
	if !errors.Is(err, repository.ErrNotFound) {
		return nil, false, err
	}
	if len(password) == 0 {
		return nil, false, errors.New("legacy node password is required")
	}
	credentials := sshclient.NewInitialCredentials(address, "root", password)
	connection, connectErr := m.legacySSH.ConnectInitial(ctx, "legacy:"+panelID+":"+nodeUUID, credentials)
	return connection, false, connectErr
}

func (m *NodeMaintenance) Run(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		return errors.New("node network poll interval must be positive")
	}
	m.refreshManaged(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			m.refreshManaged(ctx)
		}
	}
}

func (m *NodeMaintenance) refreshManaged(ctx context.Context) {
	started := time.Now()
	var managed, updated, failed atomic.Int64
	var workers sync.WaitGroup
	semaphore := make(chan struct{}, nodeNetworkPollConcurrency)

	for _, panelID := range m.app.order {
		panel := m.app.panels[panelID]
		nodes, err := panel.Service.remnawave.GetNodes(ctx)
		if err != nil {
			m.logger.Warn("node network poll failed", slog.String("panel_id", panelID))
			continue
		}
		for _, node := range nodes {
			item, err := m.app.repository.FindDeploymentByPanelNodeUUID(ctx, panelID, node.UUID)
			if err != nil {
				continue
			}
			managed.Add(1)
			address, err := netip.ParseAddr(strings.TrimSpace(node.Address))
			if err != nil || !address.IsGlobalUnicast() || address.IsPrivate() {
				failed.Add(1)
				m.logNodePollFailure(panelID, node.Name, "invalid_address")
				continue
			}
			select {
			case semaphore <- struct{}{}:
			case <-ctx.Done():
				workers.Wait()
				return
			}
			workers.Add(1)
			go func(panelID, nodeUUID, nodeName, deploymentID string, address netip.Addr) {
				defer workers.Done()
				defer func() { <-semaphore }()
				if reason := m.pollManagedNode(ctx, panelID, nodeUUID, deploymentID, address); reason != "" {
					failed.Add(1)
					m.logNodePollFailure(panelID, nodeName, reason)
					return
				}
				updated.Add(1)
			}(panelID, node.UUID, node.Name, item.ID, address.Unmap())
		}
	}
	workers.Wait()
	m.logger.Info("node network poll completed",
		slog.Int64("managed", managed.Load()),
		slog.Int64("updated", updated.Load()),
		slog.Int64("failed", failed.Load()),
		slog.Int64("duration_ms", time.Since(started).Milliseconds()),
	)
}

func (m *NodeMaintenance) pollManagedNode(ctx context.Context, panelID, nodeUUID, deploymentID string, address netip.Addr) string {
	connection, err := m.managedSSH.ConnectWithDeploymentKey(ctx, deploymentID, address, "root")
	if err != nil {
		return "ssh_connect"
	}
	addresses, readErr := readNetplanAddresses(ctx, connection, m.timeout)
	_ = connection.Close()
	if readErr != nil {
		return "netplan_read"
	}
	if err := m.store.UpsertNodeNetworkInventory(ctx, repository.NodeNetworkInventory{PanelID: panelID, NodeUUID: nodeUUID, Addresses: addresses}); err != nil {
		return "inventory_save"
	}
	return ""
}

func (m *NodeMaintenance) logNodePollFailure(panelID, nodeName, reason string) {
	m.logger.Warn("node network poll skipped node",
		slog.String("panel_id", panelID),
		slog.String("node_name", nodeName),
		slog.String("reason", reason),
	)
}

func readNetplanAddresses(ctx context.Context, runner sshclient.CommandRunner, timeout time.Duration) ([]string, error) {
	result, err := runner.Run(ctx, sshclient.CommandRequest{Command: "cat -- " + netplanPath, Timeout: timeout})
	if err != nil {
		return nil, errors.New("read netplan failed")
	}
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(result.Stdout), &root); err != nil {
		return nil, errors.New("parse netplan failed")
	}
	set := make(map[string]struct{})
	collectNetplanAddresses(&root, set)
	addresses := make([]string, 0, len(set))
	for address := range set {
		addresses = append(addresses, address)
	}
	sort.Strings(addresses)
	if len(addresses) > 128 {
		return nil, fmt.Errorf("netplan contains too many addresses")
	}
	return addresses, nil
}

func collectNetplanAddresses(node *yaml.Node, result map[string]struct{}) {
	if node == nil || len(node.Content) == 0 {
		return
	}
	document := node
	if node.Kind == yaml.DocumentNode {
		document = node.Content[0]
	}
	network := mappingValue(document, "network")
	for _, groupName := range []string{"ethernets", "bonds", "bridges", "vlans"} {
		group := mappingValue(network, groupName)
		if group == nil || group.Kind != yaml.MappingNode {
			continue
		}
		for index := 1; index < len(group.Content); index += 2 {
			addresses := mappingValue(group.Content[index], "addresses")
			if addresses == nil || addresses.Kind != yaml.SequenceNode {
				continue
			}
			for _, candidate := range addresses.Content {
				raw := strings.TrimSpace(candidate.Value)
				if prefix, err := netip.ParsePrefix(raw); err == nil {
					result[prefix.Addr().Unmap().String()] = struct{}{}
				} else if address, err := netip.ParseAddr(raw); err == nil {
					result[address.Unmap().String()] = struct{}{}
				}
			}
		}
	}
}

func mappingValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for index := 0; index+1 < len(node.Content); index += 2 {
		if node.Content[index].Value == key {
			return node.Content[index+1]
		}
	}
	return nil
}
