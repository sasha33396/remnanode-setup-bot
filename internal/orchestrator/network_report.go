package orchestrator

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"time"

	"remnanode-setup-bot/internal/networkinventory"
	"remnanode-setup-bot/internal/repository"
	"remnanode-setup-bot/internal/telegram"
)

func (a *TelegramApplication) ListNetworkReports(ctx context.Context) ([]networkinventory.Report, error) {
	store, ok := a.repository.(networkinventory.Store)
	if !ok {
		return nil, errors.New("network report store unavailable")
	}
	return store.ListNetworkReports(ctx)
}

// CollectNetworkReports never adopts legacy nodes or invokes the provisioner.
// An empty selection scans all nodes; inaccessible/legacy nodes appear explicitly.
func (a *TelegramApplication) CollectNetworkReports(ctx context.Context, panelID, nodeUUID string, progress func(telegram.NodeActualizeProgress)) ([]networkinventory.Report, error) {
	if a.maintenance == nil {
		return nil, errors.New("network maintenance unavailable")
	}
	if !a.maintenance.reportMu.TryLock() {
		return nil, errors.New("network collection already running")
	}
	defer a.maintenance.reportMu.Unlock()
	store, ok := a.repository.(networkinventory.Store)
	if !ok {
		return nil, errors.New("network report store unavailable")
	}
	nodes, err := a.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	result := []networkinventory.Report{}
	for _, node := range nodes {
		if panelID != "" && (node.PanelID != panelID || node.UUID != nodeUUID) {
			continue
		}
		report := networkinventory.Report{PanelID: node.PanelID, NodeUUID: node.UUID, NodeName: node.Name, PrimaryIP: node.Address, CollectedAt: time.Now().UTC(), Status: "failed"}
		if err := ctx.Err(); err != nil {
			return result, err
		}
		item, findErr := a.repository.FindDeploymentByPanelNodeUUID(ctx, node.PanelID, node.UUID)
		switch {
		case errors.Is(findErr, repository.ErrNotFound):
			report.Status = "skipped"
			report.Warnings = []string{"Нет установленного доступа бота по SSH. Автоматический перевод под управление не выполнялся."}
		case findErr != nil:
			report.Warnings = []string{"Не удалось прочитать данные доступа ноды."}
		default:
			address, parseErr := netip.ParseAddr(strings.TrimSpace(node.Address))
			if parseErr != nil || !publicIP(address) {
				report.Warnings = []string{"Адрес ноды не является публичным IP."}
			} else {
				probeCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
				connection, connectErr := a.maintenance.managedSSH.ConnectWithDeploymentKey(probeCtx, item.ID, address.Unmap(), "root")
				if connectErr != nil {
					report.Warnings = []string{"Не удалось подключиться по SSH."}
				} else {
					collected, collectErr := networkinventory.Collect(probeCtx, connection, address.Unmap())
					_ = connection.Close()
					if collectErr != nil {
						report.Warnings = []string{"Не удалось прочитать сеть: проверьте доступность iproute2 с поддержкой JSON."}
					} else {
						collected.PanelID, collected.NodeUUID, collected.NodeName = node.PanelID, node.UUID, node.Name
						report = collected
					}
				}
				cancel()
			}
		}
		if err := store.SaveNetworkReport(ctx, report); err != nil {
			return result, errors.New("save network report failed")
		}
		result = append(result, report)
		if progress != nil {
			progress(telegram.NodeActualizeProgress{Step: "network_inventory", Message: "Проверена нода: " + node.Name, Completed: len(result), Total: len(nodes)})
		}
	}
	if panelID != "" && len(result) == 0 {
		return nil, errors.New("node not found")
	}
	return result, nil
}
