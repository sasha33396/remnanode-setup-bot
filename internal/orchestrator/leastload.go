package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"strings"
	"time"

	"remnanode-setup-bot/internal/deployment"
	"remnanode-setup-bot/internal/leastload"
	"remnanode-setup-bot/internal/networkinventory"
	"remnanode-setup-bot/internal/remnawave"
	"remnanode-setup-bot/internal/telegram"
)

type replaceRecord interface {
	ReplaceARecord(context.Context, string, netip.Addr, netip.Addr, bool) error
}

func (a *TelegramApplication) connectLeastload(ctx context.Context, id string, ip netip.Addr) (leastload.Session, error) {
	if a.leastloadSSH != nil {
		return a.leastloadSSH(ctx, id, ip)
	}
	return a.maintenance.managedSSH.ConnectWithDeploymentKey(ctx, id, ip, "root")
}

func (a *TelegramApplication) ListLeastloadCandidates(ctx context.Context, panelID string) ([]leastload.Candidate, error) {
	panel, err := a.panel(panelID)
	if err != nil {
		return nil, err
	}
	nodes, err := panel.Service.remnawave.GetNodes(ctx)
	if err != nil {
		return nil, err
	}
	hosts, err := panel.Service.remnawave.GetHosts(ctx)
	if err != nil {
		return nil, err
	}
	result := []leastload.Candidate{}
	for _, node := range nodes {
		var related []remnawave.Host
		for _, h := range hosts {
			if h.IsHidden && len(h.Nodes) == 1 && h.Nodes[0] == node.UUID {
				related = append(related, h)
			}
		}
		if len(related) == 0 {
			continue
		}
		c := leastload.Candidate{NodeUUID: node.UUID, Name: node.Name, Address: node.Address}
		if len(related) != 1 {
			c.Reason = "Несколько скрытых Host"
		} else {
			c.Domain = related[0].Address
		}
		item, e := a.repository.FindDeploymentByPanelNodeUUID(ctx, panelID, node.UUID)
		if e != nil {
			c.Reason = "Нет управляемой записи ноды в боте"
		} else if item.SSHHostKeyFingerprint == nil || *item.SSHHostKeyFingerprint == "" {
			c.NetworkReason = "Нет закреплённого ключа SSH"
		}
		if _, e := netip.ParseAddr(c.Domain); e == nil {
			c.Reason = "Host использует IP вместо отдельного DNS-имени"
		}
		result = append(result, c)
	}
	return result, nil
}

func (a *TelegramApplication) leastloadStore() (leastload.Store, error) {
	s, ok := a.repository.(leastload.Store)
	if !ok || a.maintenance == nil {
		return nil, leastload.Error("Сервис смены IP недоступен")
	}
	return s, nil
}
func (a *TelegramApplication) ListLeastloadJobs(ctx context.Context) ([]leastload.Job, error) {
	s, e := a.leastloadStore()
	if e != nil {
		return nil, e
	}
	return s.ListLeastloadJobs(ctx)
}

func (a *TelegramApplication) PrepareLeastload(ctx context.Context, panelID, mode string, operator int64, mappings []leastload.Mapping, progress func(telegram.NodeActualizeProgress)) (leastload.Job, error) {
	var job leastload.Job
	store, err := a.leastloadStore()
	if err != nil {
		return job, err
	}
	if mode == "" {
		mode = leastload.ModeNetwork
	}
	if operator <= 0 || len(mappings) == 0 || len(mappings) > 50 || (mode != leastload.ModeNetwork && mode != leastload.ModeControlPlane) {
		return job, ErrInvalidInput
	}
	oldSet, newSet, nodeSet := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, m := range mappings {
		o, e1 := netip.ParseAddr(m.OldIP)
		n, e2 := netip.ParseAddr(m.NewIP)
		if e1 != nil || e2 != nil || !o.Is4() || !n.Is4() || !publicIP(o) || !publicIP(n) || o == n || oldSet[m.OldIP] || newSet[m.NewIP] || nodeSet[m.NodeUUID] {
			return job, leastload.Error("Проверьте адреса, дубликаты и выбранные ноды")
		}
		oldSet[m.OldIP] = true
		newSet[m.NewIP] = true
		nodeSet[m.NodeUUID] = true
	}
	for ip := range newSet {
		if oldSet[ip] {
			return job, leastload.Error("Перестановки адресов между нодами не поддерживаются")
		}
	}
	id, err := deployment.NewID()
	if err != nil {
		return job, err
	}
	job = leastload.Job{ID: id, PanelID: panelID, Mode: mode, Operator: operator, CreatedAt: time.Now().UTC()}
	for i, m := range mappings {
		itemID, e := deployment.NewID()
		if e != nil {
			return job, e
		}
		item := leastload.Item{ID: itemID, Mapping: m, Status: "blocked"}
		probe, cancel := context.WithTimeout(ctx, 100*time.Second)
		if mode == leastload.ModeControlPlane {
			item, e = a.prepareLeastloadControlPlaneItem(probe, panelID, item)
		} else {
			item, e = a.prepareLeastloadItem(probe, panelID, item)
		}
		cancel()
		if e != nil {
			item.Status = "blocked"
			item.Message = safeLeastloadError(e)
		} else {
			item.Status = "prepared"
		}
		job.Items = append(job.Items, item)
		if progress != nil {
			progress(telegram.NodeActualizeProgress{Step: "preview", Message: "Проверена нода: " + m.OldIP, Completed: i + 1, Total: len(mappings)})
		}
	}
	job.UpdatedAt = time.Now().UTC()
	err = store.SaveLeastloadJob(ctx, job)
	return job, err
}

func (a *TelegramApplication) checkLeastloadTarget(ctx context.Context, panelID string, item *leastload.Item, requireSSH bool) (PanelApplicationConfig, deployment.Deployment, error) {
	panel, err := a.panel(panelID)
	if err != nil {
		return panel, deployment.Deployment{}, err
	}
	managed, err := a.repository.FindDeploymentByPanelNodeUUID(ctx, panelID, item.Mapping.NodeUUID)
	if err != nil {
		return panel, managed, leastload.Error("Нет управляемой записи ноды в боте")
	}
	if requireSSH && (managed.SSHHostKeyFingerprint == nil || *managed.SSHHostKeyFingerprint == "") {
		return panel, managed, leastload.Error("Не закреплён ключ SSH сервера")
	}
	nodes, err := panel.Service.remnawave.GetNodes(ctx)
	if err != nil {
		return panel, managed, err
	}
	found := false
	for _, node := range nodes {
		if node.UUID == item.Mapping.NodeUUID {
			found = true
			item.Name = node.Name
			resuming := item.Status == "network_pending" || item.Status == "network_done" || item.Status == "control_pending" || item.Status == "done"
			if node.Address != item.Mapping.OldIP && !(resuming && node.Address == item.Mapping.NewIP) {
				return panel, managed, leastload.Error("Адрес Node изменился после подготовки")
			}
		}
		if node.UUID != item.Mapping.NodeUUID && node.Address == item.Mapping.NewIP {
			return panel, managed, leastload.Error("Новый IP уже используется другой нодой")
		}
	}
	if !found {
		return panel, managed, ErrNodeNotFound
	}
	if managed.AdvancedEgressIP.IsValid() && (managed.AdvancedEgressIP.String() == item.Mapping.OldIP || managed.AdvancedEgressIP.String() == item.Mapping.NewIP) {
		return panel, managed, leastload.Error("Адрес совпадает с выделенным egress IP; автоматическая замена запрещена")
	}
	hosts, err := panel.Service.remnawave.GetHosts(ctx)
	if err != nil {
		return panel, managed, err
	}
	var matched []remnawave.Host
	for _, h := range hosts {
		if h.IsHidden && len(h.Nodes) == 1 && h.Nodes[0] == item.Mapping.NodeUUID {
			matched = append(matched, h)
		}
	}
	if len(matched) != 1 {
		return panel, managed, leastload.Error("Нужен один скрытый Host, привязанный только к этой ноде")
	}
	h := matched[0]
	if strings.TrimSpace(h.Address) == "" {
		return panel, managed, ErrHostUnavailable
	}
	if _, e := netip.ParseAddr(h.Address); e == nil {
		return panel, managed, leastload.Error("Скрытый Host должен использовать DNS-имя")
	}
	if item.HostID != "" && (item.HostID != h.UUID || item.Domain != h.Address || item.DeploymentID != managed.ID) {
		return panel, managed, leastload.Error("Привязка Host или сервера изменилась")
	}
	for _, other := range hosts {
		if other.UUID != h.UUID && strings.EqualFold(other.Address, h.Address) {
			return panel, managed, leastload.Error("DNS-имя используется несколькими Host")
		}
	}
	item.HostID = h.UUID
	item.Domain = h.Address
	item.DeploymentID = managed.ID
	dns, ok := panel.Service.config.DNSRecords.(replaceRecord)
	if !ok {
		return panel, managed, leastload.Error("Прямая смена A-записи Cloudflare недоступна")
	}
	if err := dns.ReplaceARecord(ctx, h.Address, netip.MustParseAddr(item.Mapping.OldIP), netip.MustParseAddr(item.Mapping.NewIP), false); err != nil {
		return panel, managed, leastload.Error("Не подтверждена единственная A-запись Cloudflare со старым или новым IP")
	}
	return panel, managed, nil
}

func (a *TelegramApplication) prepareLeastloadItem(ctx context.Context, panelID string, item leastload.Item) (leastload.Item, error) {
	panel, managed, err := a.checkLeastloadTarget(ctx, panelID, &item, true)
	if err != nil {
		return item, err
	}
	conn, err := a.connectLeastload(ctx, managed.ID, netip.MustParseAddr(item.Mapping.OldIP))
	if err != nil {
		return item, leastload.Error("SSH по старому адресу недоступен")
	}
	defer conn.Close()
	item.Plan, err = leastload.Prepare(ctx, conn, netip.MustParseAddr(item.Mapping.OldIP), netip.MustParseAddr(item.Mapping.NewIP))
	if err == nil && item.Plan.Mode == "replace" {
		err = checkEgress(ctx, panel.Service, item.Mapping.NodeUUID, item.Mapping.OldIP)
	}
	if err == nil {
		switch item.Plan.Mode {
		case "metadata":
			item.Message = "Новый IP уже настроен — сеть не меняется"
		case "activate":
			item.Message = "Новый IP есть в Netplan — применить конфигурацию, сохранить старый IP"
		default:
			item.Message = "Заменить только адрес Node; остальные адреса и шлюзы сохранить"
		}
	}
	return item, err
}

func (a *TelegramApplication) prepareLeastloadControlPlaneItem(ctx context.Context, panelID string, item leastload.Item) (leastload.Item, error) {
	if _, _, err := a.checkLeastloadTarget(ctx, panelID, &item, false); err != nil {
		return item, err
	}
	item.Plan = leastload.Plan{OldIP: item.Mapping.OldIP, NewIP: item.Mapping.NewIP, Mode: leastload.ModeControlPlane}
	item.Message = "Без SSH/Netplan: изменить Node, DNS и основной IP в боте"
	return item, nil
}

func checkEgress(ctx context.Context, s *DeploymentService, nodeID, oldIP string) error {
	node, err := s.remnawave.GetNode(ctx, nodeID)
	if err != nil {
		return err
	}
	api, ok := s.remnawave.(interface {
		GetConfigProfiles(context.Context) ([]remnawave.ConfigProfile, error)
	})
	if !ok || node.ActiveConfigProfileUUID == nil {
		return leastload.Error("Не удалось проверить исходящий IP профиля Xray")
	}
	profiles, err := api.GetConfigProfiles(ctx)
	if err != nil {
		return err
	}
	for _, p := range profiles {
		if p.UUID == *node.ActiveConfigProfileUUID {
			var config any
			if len(p.Config) == 0 || json.Unmarshal(p.Config, &config) != nil {
				return leastload.Error("Панель не вернула конфигурацию Xray для проверки egress")
			}
			var conflicts func(any) bool
			conflicts = func(value any) bool {
				switch v := value.(type) {
				case map[string]any:
					for key, child := range v {
						if (key == "sendThrough" || key == "listen") && child == oldIP {
							return true
						}
						if conflicts(child) {
							return true
						}
					}
				case []any:
					for _, child := range v {
						if conflicts(child) {
							return true
						}
					}
				}
				return false
			}
			if conflicts(config) {
				return leastload.Error("Старый IP используется в sendThrough/listen Xray; требуется отдельный план")
			}
			return nil
		}
	}
	return leastload.Error("Активный профиль Xray не найден")
}

func safeLeastloadError(err error) string {
	var operator interface{ OperatorMessage() string }
	if errors.As(err, &operator) {
		return operator.OperatorMessage()
	}
	var e leastload.Error
	if errors.As(err, &e) {
		return e.Error()
	}
	return "Операция не завершена; проверьте доступность панели, SSH и базы данных"
}

func (a *TelegramApplication) ApplyLeastload(ctx context.Context, id string, operator int64, progress func(telegram.NodeActualizeProgress)) (leastload.Job, error) {
	store, err := a.leastloadStore()
	if err != nil {
		return leastload.Job{}, err
	}
	unlock, err := store.Lock(ctx, "leastload-ip-global")
	if err != nil {
		return leastload.Job{}, err
	}
	defer unlock()
	job, err := store.GetLeastloadJob(ctx, id)
	if err != nil {
		return job, err
	}
	if job.Operator != operator {
		return job, ErrInvalidInput
	}
	if job.Mode == "" {
		job.Mode = leastload.ModeNetwork
	}
	for i := range job.Items {
		item := &job.Items[i]
		if item.Status == "done" || item.Status == "blocked" || item.Status == "rolled_back" {
			continue
		}
		if err := ctx.Err(); err != nil {
			return job, err
		}
		pending, pendingErr := store.HasPendingLeastload(ctx, item.DeploymentID, job.ID)
		if pendingErr != nil {
			return job, pendingErr
		}
		if pending {
			item.Message = "Есть незавершённая операция для этого сервера; сначала продолжите её"
			if e := store.SaveLeastloadJob(ctx, job); e != nil {
				return job, e
			}
			continue
		}
		update := func(status, message string) error {
			item.Status = status
			item.Message = message
			job.UpdatedAt = time.Now().UTC()
			return store.SaveLeastloadJob(ctx, job)
		}
		if job.Mode == leastload.ModeControlPlane {
			err = a.applyLeastloadControlPlaneItem(ctx, job.PanelID, item, update)
		} else {
			err = a.applyLeastloadItem(ctx, job.PanelID, item, job.CreatedAt, update)
		}
		if err != nil {
			item.Message = safeLeastloadError(err)
			job.UpdatedAt = time.Now().UTC()
			if saveErr := store.SaveLeastloadJob(ctx, job); saveErr != nil {
				return job, saveErr
			}
		}
		if progress != nil {
			progress(telegram.NodeActualizeProgress{Step: item.Status, Message: item.Name + ": " + item.Message, Completed: i + 1, Total: len(job.Items)})
		}
	}
	return job, nil
}

func (a *TelegramApplication) applyLeastloadItem(ctx context.Context, panelID string, item *leastload.Item, created time.Time, update func(string, string) error) error {
	panel, _, err := a.checkLeastloadTarget(ctx, panelID, item, true)
	if err != nil {
		return err
	}
	connect := func(ctx context.Context, ip netip.Addr) (leastload.Session, error) {
		return a.connectLeastload(ctx, item.DeploymentID, ip)
	}
	oldIP, newIP := netip.MustParseAddr(item.Mapping.OldIP), netip.MustParseAddr(item.Mapping.NewIP)
	if item.Status == "prepared" {
		if time.Since(created) > 30*time.Minute {
			return leastload.Error("План старше 30 минут; подготовьте его заново")
		}
		current, e := a.prepareLeastloadItem(ctx, panelID, *item)
		if e != nil {
			return e
		}
		if current.Plan.FileHash != item.Plan.FileHash || current.Plan.Mode != item.Plan.Mode || current.Plan.Interface != item.Plan.Interface {
			return leastload.Error("Сеть изменилась после предпросмотра; подготовьте новый план")
		}
		item.Plan = current.Plan
		if item.Plan.Mode == "metadata" {
			conn, e := connect(ctx, newIP)
			if e != nil {
				return leastload.Error("Новый IP настроен, но проверка SSH по нему не прошла")
			}
			e = leastload.Verify(ctx, conn, item.Plan)
			conn.Close()
			if e != nil {
				return e
			}
			if e = update("network_done", "Сеть не менялась; новый IP проверен"); e != nil {
				return e
			}
		} else {
			conn, e := connect(ctx, oldIP)
			if e != nil {
				return leastload.Error("SSH недоступен перед применением")
			}
			if e = update("network_pending", "Запуск сети и автономного отката"); e != nil {
				conn.Close()
				return e
			}
			e = leastload.Start(ctx, conn, item.ID, item.Plan)
			conn.Close()
			if e != nil {
				var uncertain leastload.UncertainStart
				if !errors.As(e, &uncertain) {
					if saveErr := update("rolled_back", "Изменение сети не запускалось: "+safeLeastloadError(e)); saveErr != nil {
						return saveErr
					}
				}
				return e
			}
		}
	}
	if item.Status == "network_pending" {
		status, e := leastload.Finish(ctx, connect, item.ID, item.Plan)
		if e != nil {
			if status == "rolled_back" || status == "aborted" || status == "busy" || status == "stale" {
				if saveErr := update("rolled_back", "Изменение не применено либо отменено: "+status); saveErr != nil {
					return saveErr
				}
			}
			return e
		}
		if e = update("network_done", "Новый IP и сохранность адресов проверены"); e != nil {
			return e
		}
	}
	if item.Status == "network_done" {
		if _, _, e := a.checkLeastloadTarget(ctx, panelID, item, true); e != nil {
			return e
		}
		// Recheck reachability on every retry; never point the panel at an address
		// whose remote commit cannot be observed.
		conn, e := connect(ctx, newIP)
		if e != nil {
			return leastload.Error("Новый SSH-адрес недоступен; обновление панели отложено")
		}
		e = leastload.Verify(ctx, conn, item.Plan)
		conn.Close()
		if e != nil {
			return e
		}
		if _, e = panel.Service.remnawave.UpdateNodeAddress(ctx, remnawave.UpdateNodeAddressInput{UUID: item.Mapping.NodeUUID, Address: newIP}); e != nil {
			return leastload.Error("Сервер готов, обновление Remnawave требует повтора")
		}
		if e = panel.Service.config.DNSRecords.(replaceRecord).ReplaceARecord(ctx, item.Domain, oldIP, newIP, true); e != nil {
			return leastload.Error("Сервер и Node обновлены; Cloudflare требует повтора")
		}
		if _, e = a.repository.SetTargetVPSIP(ctx, item.DeploymentID, newIP); e != nil {
			return leastload.Error("Сервер, Node и DNS обновлены; запись БД требует повтора")
		}
		message := "IP сервера, Node, Cloudflare и БД обновлены"
		if reportStore, ok := a.repository.(networkinventory.Store); ok {
			probe, cancel := context.WithTimeout(ctx, 60*time.Second)
			remote, probeErr := connect(probe, newIP)
			if probeErr == nil {
				report, readErr := networkinventory.Collect(probe, remote, newIP)
				remote.Close()
				probeErr = readErr
				if readErr == nil {
					report.PanelID = panelID
					report.NodeUUID = item.Mapping.NodeUUID
					report.NodeName = item.Name
					probeErr = reportStore.SaveNetworkReport(probe, report)
				}
			}
			cancel()
			if probeErr != nil {
				message += "; сетевой отчёт обновите отдельно"
			}
		}
		if e = update("done", message); e != nil {
			return e
		}
	}
	return nil
}

func (a *TelegramApplication) applyLeastloadControlPlaneItem(ctx context.Context, panelID string, item *leastload.Item, update func(string, string) error) error {
	panel, _, err := a.checkLeastloadTarget(ctx, panelID, item, false)
	if err != nil {
		return err
	}
	if item.Status == "prepared" {
		if err := update("control_pending", "Обновление Remnawave, DNS и основного IP без SSH"); err != nil {
			return err
		}
	}
	if item.Status != "control_pending" {
		return nil
	}
	oldIP := netip.MustParseAddr(item.Mapping.OldIP)
	newIP := netip.MustParseAddr(item.Mapping.NewIP)
	if _, err := panel.Service.remnawave.UpdateNodeAddress(ctx, remnawave.UpdateNodeAddressInput{UUID: item.Mapping.NodeUUID, Address: newIP}); err != nil {
		return leastload.Error("Обновление Node в Remnawave требует повтора")
	}
	if err := panel.Service.config.DNSRecords.(replaceRecord).ReplaceARecord(ctx, item.Domain, oldIP, newIP, true); err != nil {
		return leastload.Error("Node обновлена; DNS требует повтора")
	}
	if _, err := a.repository.SetTargetVPSIP(ctx, item.DeploymentID, newIP); err != nil {
		return leastload.Error("Node и DNS обновлены; основной IP в боте требует повтора")
	}
	return update("done", "Node, DNS и основной IP бота/веб-интерфейса обновлены без SSH/Netplan")
}
