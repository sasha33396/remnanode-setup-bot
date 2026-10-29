package webconsole

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"strings"

	"remnanode-setup-bot/internal/telegram"
)

type deploymentApplication interface {
	ListPanels(context.Context) ([]telegram.Panel, error)
	ListHosts(context.Context, string) ([]telegram.Host, error)
	CheckNodeName(context.Context, string, string) error
	CheckVPSAddress(context.Context, string, netip.Addr) error
	Preflight(context.Context, telegram.PreflightInput) (telegram.PreflightResult, error)
	StartDeployment(context.Context, telegram.DeploymentInput, func(telegram.Progress) error) error
	CancelDeployment(context.Context, string) error
	ListDeployments(context.Context, int) ([]telegram.DeploymentSummary, error)
}

func (s *Server) deploymentApp() (deploymentApplication, bool) {
	app, ok := s.nodes.(deploymentApplication)
	return app, ok
}

func (s *Server) authenticated(w http.ResponseWriter, r *http.Request) bool {
	if _, _, ok := s.readSession(r); !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return false
	}
	return true
}

func (s *Server) mutationUser(w http.ResponseWriter, r *http.Request) (session, bool) {
	user, cookie, ok := s.readSession(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return session{}, false
	}
	if !s.validCSRF(r, cookie) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
		return session{}, false
	}
	return user, true
}

func (s *Server) handlePanels(w http.ResponseWriter, r *http.Request) {
	if !s.authenticated(w, r) {
		return
	}
	app, ok := s.deploymentApp()
	if !ok {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Добавление нод недоступно"})
		return
	}
	panels, err := app.ListPanels(r.Context())
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "Не удалось загрузить панели"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"panels": panels})
}

func (s *Server) handleHosts(w http.ResponseWriter, r *http.Request) {
	if !s.authenticated(w, r) {
		return
	}
	app, ok := s.deploymentApp()
	panelID := strings.TrimSpace(r.URL.Query().Get("panel"))
	if !ok || panelID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Панель не выбрана"})
		return
	}
	hosts, err := app.ListHosts(r.Context(), panelID)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "Не удалось загрузить Host"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"hosts": hosts})
}

func (s *Server) handleBalancerOptions(w http.ResponseWriter, r *http.Request) {
	if !s.authenticated(w, r) {
		return
	}
	app, ok := s.nodes.(telegram.AdvancedBalancerApplication)
	panelID := strings.TrimSpace(r.URL.Query().Get("panel"))
	if !ok || panelID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Добавление балансировщика недоступно"})
		return
	}
	options, err := app.ListAdvancedBalancerOptions(r.Context(), panelID)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "Не удалось загрузить параметры балансировщика"})
		return
	}
	writeJSON(w, http.StatusOK, options)
}

type standardDeploymentRequest struct {
	PanelID  string `json:"panelId"`
	HostID   string `json:"hostId"`
	NodeName string `json:"nodeName"`
	VPSIP    string `json:"vpsIp"`
	Password string `json:"password"`
}

type balancerDeploymentRequest struct {
	PanelID         string `json:"panelId"`
	InternalSquadID string `json:"internalSquadId"`
	BalancerTag     string `json:"balancerTag"`
	HostFQDN        string `json:"hostFqdn"`
	NodeName        string `json:"nodeName"`
	IngressIP       string `json:"ingressIp"`
	EgressIP        string `json:"egressIp"`
	Password        string `json:"password"`
}

func decodeJSON(w http.ResponseWriter, r *http.Request, limit int64, target any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Некорректный запрос"})
		return false
	}
	return true
}

func publicIPv4(value string) (netip.Addr, bool) {
	address, err := netip.ParseAddr(strings.TrimSpace(value))
	if err != nil || !address.Is4() || !address.IsGlobalUnicast() || address.IsPrivate() {
		return netip.Addr{}, false
	}
	return address.Unmap(), true
}

func validDeploymentText(value string, maximum int) bool {
	value = strings.TrimSpace(value)
	return value != "" && len(value) <= maximum && !strings.ContainsAny(value, "\r\n\x00")
}

func (s *Server) handleDeploymentAction(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/deployments/standard" {
		s.handleStandardDeployment(w, r)
		return
	}
	if r.URL.Path == "/api/deployments/balancer" {
		s.handleBalancerDeployment(w, r)
		return
	}
	s.handleRecoveryAction(w, r)
}

func (s *Server) handleStandardDeployment(w http.ResponseWriter, r *http.Request) {
	user, ok := s.mutationUser(w, r)
	if !ok {
		return
	}
	app, available := s.deploymentApp()
	if !available {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Добавление нод недоступно"})
		return
	}
	var body standardDeploymentRequest
	if !decodeJSON(w, r, 8192, &body) {
		return
	}
	address, addressOK := publicIPv4(body.VPSIP)
	if !validDeploymentText(body.PanelID, 128) || !validDeploymentText(body.HostID, 128) || !validDeploymentText(body.NodeName, 120) || !addressOK || body.Password == "" || len(body.Password) > 1024 {
		body.Password = ""
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Проверьте Host, имя, публичный IPv4 и root-пароль"})
		return
	}
	password := []byte(body.Password)
	body.Password = ""
	jobID, err := s.startBackgroundJob(user.UserID, "deploy:"+body.PanelID, body.NodeName, password, func(ctx context.Context, progress func(telegram.NodeActualizeProgress)) (any, error) {
		progress(telegram.NodeActualizeProgress{Step: "preflight", Message: "Проверка SSH, портов и готовности Host", Completed: 0, Total: 2})
		prepared, err := app.Preflight(ctx, telegram.PreflightInput{PanelID: body.PanelID, OperatorUserID: user.UserID, HostID: body.HostID, NodeName: body.NodeName, VPSIP: address, Password: password})
		if err != nil || strings.TrimSpace(prepared.PreparedDeploymentID) == "" {
			if strings.TrimSpace(prepared.PreparedDeploymentID) != "" {
				_ = app.CancelDeployment(ctx, prepared.PreparedDeploymentID)
			}
			if err == nil {
				err = errors.New("preflight did not prepare deployment")
			}
			return nil, err
		}
		progress(telegram.NodeActualizeProgress{Step: "deploy", Message: "Настройка сервера и создание ноды", Completed: 1, Total: 2})
		err = app.StartDeployment(ctx, telegram.DeploymentInput{PanelID: body.PanelID, PreparedDeploymentID: prepared.PreparedDeploymentID, OperatorUserID: user.UserID, HostID: body.HostID, NodeName: body.NodeName, VPSIP: address, Password: password}, deploymentProgress(progress))
		if err != nil {
			return nil, err
		}
		return map[string]any{"deploymentId": prepared.PreparedDeploymentID, "nodeName": body.NodeName}, nil
	})
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "Такая операция уже выполняется"})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"pending": true, "jobId": jobID})
}

func (s *Server) handleBalancerDeployment(w http.ResponseWriter, r *http.Request) {
	user, ok := s.mutationUser(w, r)
	if !ok {
		return
	}
	app, baseOK := s.deploymentApp()
	advanced, advancedOK := s.nodes.(telegram.AdvancedBalancerApplication)
	if !baseOK || !advancedOK {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Добавление балансировщика недоступно"})
		return
	}
	var body balancerDeploymentRequest
	if !decodeJSON(w, r, 8192, &body) {
		return
	}
	ingress, ingressOK := publicIPv4(body.IngressIP)
	var egress netip.Addr
	if strings.TrimSpace(body.EgressIP) != "" {
		var ok bool
		egress, ok = publicIPv4(body.EgressIP)
		if !ok || egress == ingress {
			ingressOK = false
		}
	}
	if !validDeploymentText(body.PanelID, 128) || !validDeploymentText(body.InternalSquadID, 128) || !validDeploymentText(body.BalancerTag, 128) || !validDeploymentText(body.HostFQDN, 253) || !validDeploymentText(body.NodeName, 120) || !ingressOK || body.Password == "" || len(body.Password) > 1024 {
		body.Password = ""
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Проверьте панель, squad, тег, FQDN, IP и root-пароль"})
		return
	}
	password := []byte(body.Password)
	body.Password = ""
	jobID, err := s.startBackgroundJob(user.UserID, "balancer:"+body.PanelID, body.HostFQDN, password, func(ctx context.Context, progress func(telegram.NodeActualizeProgress)) (any, error) {
		progress(telegram.NodeActualizeProgress{Step: "preflight", Message: "Проверка SSH и подготовка собственного Host", Completed: 0, Total: 2})
		prepared, err := advanced.PreflightAdvancedBalancer(ctx, telegram.AdvancedBalancerPreflightInput{PanelID: body.PanelID, OperatorUserID: user.UserID, InternalSquadID: body.InternalSquadID, BalancerTag: body.BalancerTag, HostFQDN: strings.ToLower(strings.TrimSuffix(body.HostFQDN, ".")), NodeName: body.NodeName, IngressIP: ingress, EgressIP: egress, Password: password})
		if err != nil || strings.TrimSpace(prepared.PreparedDeploymentID) == "" {
			if strings.TrimSpace(prepared.PreparedDeploymentID) != "" {
				_ = app.CancelDeployment(ctx, prepared.PreparedDeploymentID)
			}
			if err == nil {
				err = errors.New("preflight did not prepare deployment")
			}
			return nil, err
		}
		progress(telegram.NodeActualizeProgress{Step: "deploy", Message: "Создание Host, профиля, inbound и ноды", Completed: 1, Total: 2})
		err = app.StartDeployment(ctx, telegram.DeploymentInput{PanelID: body.PanelID, PreparedDeploymentID: prepared.PreparedDeploymentID, OperatorUserID: user.UserID, HostID: prepared.PreparedDeploymentID, NodeName: body.NodeName, VPSIP: ingress}, deploymentProgress(progress))
		if err != nil {
			return nil, err
		}
		return map[string]any{"deploymentId": prepared.PreparedDeploymentID, "nodeName": body.NodeName, "hostFqdn": body.HostFQDN}, nil
	})
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "Такая операция уже выполняется"})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"pending": true, "jobId": jobID})
}

func deploymentProgress(report func(telegram.NodeActualizeProgress)) func(telegram.Progress) error {
	return func(update telegram.Progress) error {
		message := strings.TrimSpace(update.SafeMessage)
		if message == "" {
			message = deploymentStepMessage(update.Step)
		}
		total := update.Total + 1
		if total < 2 {
			total = 2
		}
		completed := update.Completed + 1
		if completed >= total {
			completed = total - 1
		}
		report(telegram.NodeActualizeProgress{Step: update.Step, Message: message, Completed: completed, Total: total})
		return nil
	}
}

func deploymentStepMessage(step string) string {
	switch step {
	case "prepare_certificate":
		return "Подготовка TLS-сертификата"
	case "system":
		return "Настройка системы"
	case "docker":
		return "Установка и настройка Docker"
	case "firewall":
		return "Применение firewall и блокировки порта 25"
	case "remnanode":
		return "Установка Remnanode"
	case "exporters":
		return "Настройка мониторинга"
	case "logrotate":
		return "Настройка ротации логов"
	case "xray_sni":
		return "Настройка xray-sni"
	case "healthcheck":
		return "Финальная проверка ноды"
	default:
		return "Выполнение шага: " + step
	}
}

func splitNodeOperation(path, suffix string) (string, string, bool) {
	parts := strings.Split(strings.TrimPrefix(path, "/api/nodes/"), "/")
	returnValue := len(parts) == 3 && parts[0] != "" && parts[1] != "" && parts[2] == suffix
	if !returnValue {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func (s *Server) nodeName(ctx context.Context, panelID, nodeUUID string) (string, error) {
	nodes, err := s.nodes.ListNodes(ctx)
	if err != nil {
		return "", err
	}
	for _, node := range nodes {
		if node.PanelID == panelID && node.UUID == nodeUUID {
			return node.Name, nil
		}
	}
	return "", errors.New("node not found")
}

func (s *Server) handleNodeOperationPreview(w http.ResponseWriter, r *http.Request) {
	if !s.authenticated(w, r) {
		return
	}
	if strings.HasSuffix(r.URL.Path, "/move") {
		s.handleNodeMovePreview(w, r)
		return
	}
	if strings.HasSuffix(r.URL.Path, "/dns") {
		s.handleNodeDNSPreview(w, r)
		return
	}
	http.NotFound(w, r)
}

func (s *Server) handleNodeMovePreview(w http.ResponseWriter, r *http.Request) {
	panel, node, ok := splitNodeOperation(r.URL.Path, "move")
	if !ok {
		http.NotFound(w, r)
		return
	}
	app, available := s.nodes.(telegram.NodeHostMoveApplication)
	if !available {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Перенос ноды недоступен"})
		return
	}
	target, hosts, err := app.PrepareNodeHostMove(r.Context(), panel, node)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "Не удалось подготовить перенос ноды"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"target": map[string]any{"name": target.Name, "address": target.Address, "managed": target.Managed, "currentHostKnown": target.CurrentHostKnown, "currentHostRemark": target.CurrentHostRemark, "currentHostAddress": target.CurrentHostAddress}, "hosts": hosts})
}

func (s *Server) handleNodeMove(w http.ResponseWriter, r *http.Request) {
	user, authorized := s.mutationUser(w, r)
	if !authorized {
		return
	}
	panel, node, ok := splitNodeOperation(r.URL.Path, "move")
	if !ok {
		http.NotFound(w, r)
		return
	}
	app, available := s.nodes.(telegram.NodeHostMoveApplication)
	if !available {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Перенос ноды недоступен"})
		return
	}
	var body struct {
		HostID   string `json:"hostId"`
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, 4096, &body) {
		return
	}
	if !validDeploymentText(body.HostID, 128) || body.Password == "" || len(body.Password) > 1024 {
		body.Password = ""
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Выберите Host и введите root-пароль ноды"})
		return
	}
	password := []byte(body.Password)
	body.Password = ""
	jobID, err := s.startBackgroundJob(user.UserID, "move:"+panel, node, password, func(ctx context.Context, progress func(telegram.NodeActualizeProgress)) (any, error) {
		progress(telegram.NodeActualizeProgress{Step: "prepare", Message: "Повторная проверка профиля и целевого Host", Completed: 0, Total: 3})
		target, hosts, err := app.PrepareNodeHostMove(ctx, panel, node)
		if err != nil {
			return nil, err
		}
		found := false
		for _, host := range hosts {
			if host.ID == body.HostID {
				found = true
				break
			}
		}
		if !found {
			return nil, errors.New("target Host is no longer available")
		}
		progress(telegram.NodeActualizeProgress{Step: "xray_sni", Message: "Переключение SNI и TLS-сертификата на ноде", Completed: 1, Total: 3})
		result, err := app.MoveNodeToHost(ctx, telegram.NodeHostMoveInput{PanelID: panel, NodeUUID: node, TargetHostUUID: body.HostID, ExpectedProfileUUID: target.ExpectedProfileUUID, ExpectedInboundUUIDs: target.ExpectedInboundUUIDs, Password: password})
		if err != nil {
			return nil, err
		}
		progress(telegram.NodeActualizeProgress{Step: "remnawave", Message: "Проверка профиля, inbound и DNS", Completed: 2, Total: 3})
		return map[string]any{"nodeName": result.NodeName, "targetHost": result.TargetHost, "targetAddress": result.TargetAddress, "dnsUpdated": result.DNSUpdated}, nil
	})
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "Для этой ноды уже выполняется операция"})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"pending": true, "jobId": jobID})
}

func (s *Server) handleNodeIPChange(w http.ResponseWriter, r *http.Request) {
	user, authorized := s.mutationUser(w, r)
	if !authorized {
		return
	}
	panel, node, ok := splitNodeOperation(r.URL.Path, "ip")
	if !ok {
		http.NotFound(w, r)
		return
	}
	app, available := s.nodes.(telegram.NodeIPApplication)
	if !available {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Смена IP недоступна"})
		return
	}
	var body struct {
		NewIP string `json:"newIp"`
	}
	if !decodeJSON(w, r, 2048, &body) {
		return
	}
	newIP, valid := publicIPv4(body.NewIP)
	if !valid {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Нужен публичный IPv4"})
		return
	}
	jobID, err := s.startBackgroundJob(user.UserID, "ip:"+panel, node, nil, func(ctx context.Context, progress func(telegram.NodeActualizeProgress)) (any, error) {
		progress(telegram.NodeActualizeProgress{Step: "prepare", Message: "Проверка текущего IP в Remnawave и DNS", Completed: 0, Total: 2})
		name, err := s.nodeName(ctx, panel, node)
		if err != nil {
			return nil, err
		}
		target, err := app.FindNodeForIPChange(ctx, panel, name)
		if err != nil || target.UUID != node {
			return nil, errors.New("node changed while preparing IP replacement")
		}
		progress(telegram.NodeActualizeProgress{Step: "replace_ip", Message: "Смена IP в Remnawave, DNS и учётной записи", Completed: 1, Total: 2})
		message, err := app.ReplaceNodeIP(ctx, telegram.NodeIPChangeInput{PanelID: panel, NodeUUID: node, ExpectedIP: target.Address, NewIP: newIP})
		if err != nil {
			return nil, err
		}
		return map[string]any{"nodeName": target.Name, "newIp": newIP.String(), "message": message}, nil
	})
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "Для этой ноды уже выполняется операция"})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"pending": true, "jobId": jobID})
}

func (s *Server) dnsTarget(ctx context.Context, app telegram.NodeDNSSyncApplication, panel, node string) (telegram.NodeDNSSyncTarget, error) {
	name, err := s.nodeName(ctx, panel, node)
	if err != nil {
		return telegram.NodeDNSSyncTarget{}, err
	}
	target, err := app.FindNodeForDNSSync(ctx, panel, name)
	if err != nil {
		return target, err
	}
	if target.UUID != node {
		return target, errors.New("node changed")
	}
	return target, nil
}

func (s *Server) handleNodeDNSPreview(w http.ResponseWriter, r *http.Request) {
	panel, node, ok := splitNodeOperation(r.URL.Path, "dns")
	if !ok {
		http.NotFound(w, r)
		return
	}
	app, available := s.nodes.(telegram.NodeDNSSyncApplication)
	if !available {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "DNS-синхронизация недоступна"})
		return
	}
	target, err := s.dnsTarget(r.Context(), app, panel, node)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "Для этой ноды DNS-балансировка не определена. Это нормально для ноды с собственным Host."})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": target.Name, "address": target.Address.String(), "managed": target.Managed, "dnsZone": target.DNSZone, "currentZones": target.CurrentZones, "canSync": target.CanSync, "note": target.Note})
}

func (s *Server) handleNodeDNSSync(w http.ResponseWriter, r *http.Request) {
	user, authorized := s.mutationUser(w, r)
	if !authorized {
		return
	}
	panel, node, ok := splitNodeOperation(r.URL.Path, "dns")
	if !ok {
		http.NotFound(w, r)
		return
	}
	app, available := s.nodes.(telegram.NodeDNSSyncApplication)
	if !available {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "DNS-синхронизация недоступна"})
		return
	}
	jobID, err := s.startBackgroundJob(user.UserID, "dns:"+panel, node, nil, func(ctx context.Context, progress func(telegram.NodeActualizeProgress)) (any, error) {
		progress(telegram.NodeActualizeProgress{Step: "dns", Message: "Проверка Remnawave и целевой DNS-зоны", Completed: 0, Total: 2})
		target, err := s.dnsTarget(ctx, app, panel, node)
		if err != nil {
			return nil, err
		}
		if !target.CanSync {
			return nil, errors.New("DNS target cannot be synchronized safely")
		}
		progress(telegram.NodeActualizeProgress{Step: "dns", Message: "Синхронизация DNS по адресу из Remnawave", Completed: 1, Total: 2})
		result, err := app.SyncNodeDNS(ctx, telegram.NodeDNSSyncInput{PanelID: panel, NodeUUID: node, ExpectedIP: target.Address})
		if err != nil {
			return nil, err
		}
		return map[string]any{"nodeName": result.NodeName, "address": result.Address.String(), "dnsZone": result.DNSZone, "action": result.Action}, nil
	})
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "Для этой ноды уже выполняется операция"})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"pending": true, "jobId": jobID})
}

type bulkIPRequest struct {
	PanelID        string `json:"panelId"`
	EnableDisabled bool   `json:"enableDisabled"`
	Mappings       []struct {
		OldIP string `json:"oldIp"`
		NewIP string `json:"newIp"`
	} `json:"mappings"`
}

func parseBulkIPRequest(body bulkIPRequest) ([]telegram.BulkNodeIPMapping, bool) {
	if !validDeploymentText(body.PanelID, 128) || len(body.Mappings) == 0 || len(body.Mappings) > telegram.MaxBulkNodeIPChanges {
		return nil, false
	}
	result := make([]telegram.BulkNodeIPMapping, 0, len(body.Mappings))
	oldSeen, newSeen := make(map[netip.Addr]struct{}), make(map[netip.Addr]struct{})
	for _, item := range body.Mappings {
		oldIP, oldOK := publicIPv4(item.OldIP)
		newIP, newOK := publicIPv4(item.NewIP)
		if !oldOK || !newOK || oldIP == newIP {
			return nil, false
		}
		if _, exists := oldSeen[oldIP]; exists {
			return nil, false
		}
		if _, exists := newSeen[newIP]; exists {
			return nil, false
		}
		oldSeen[oldIP], newSeen[newIP] = struct{}{}, struct{}{}
		result = append(result, telegram.BulkNodeIPMapping{OldIP: oldIP, NewIP: newIP})
	}
	return result, true
}

func (s *Server) handleBulkIPPreview(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.mutationUser(w, r); !ok {
		return
	}
	app, available := s.nodes.(telegram.BulkNodeIPApplication)
	if !available {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Массовая смена IP недоступна"})
		return
	}
	var body bulkIPRequest
	if !decodeJSON(w, r, 32768, &body) {
		return
	}
	mappings, valid := parseBulkIPRequest(body)
	if !valid {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Укажите от 1 до 50 уникальных пар публичных IPv4"})
		return
	}
	plan, err := app.PrepareBulkNodeIPChange(r.Context(), body.PanelID, mappings)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "Не удалось безопасно подготовить массовую смену IP"})
		return
	}
	items := make([]map[string]any, 0, len(plan.Items))
	for _, item := range plan.Items {
		items = append(items, map[string]any{"nodeName": item.NodeName, "oldIp": item.OldIP.String(), "newIp": item.NewIP.String(), "dnsZones": item.DNSZones, "managed": item.Managed, "wasDisabled": item.WasDisabled})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "disabledCount": plan.DisabledCount, "withoutDns": plan.WithoutDNS})
}

func (s *Server) handleBulkIPApply(w http.ResponseWriter, r *http.Request) {
	user, ok := s.mutationUser(w, r)
	if !ok {
		return
	}
	app, available := s.nodes.(telegram.BulkNodeIPApplication)
	if !available {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Массовая смена IP недоступна"})
		return
	}
	var body bulkIPRequest
	if !decodeJSON(w, r, 32768, &body) {
		return
	}
	mappings, valid := parseBulkIPRequest(body)
	if !valid {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Укажите от 1 до 50 уникальных пар публичных IPv4"})
		return
	}
	jobID, err := s.startBackgroundJob(user.UserID, "bulk-ip", body.PanelID, nil, func(ctx context.Context, progress func(telegram.NodeActualizeProgress)) (any, error) {
		progress(telegram.NodeActualizeProgress{Step: "prepare", Message: "Повторная проверка нод и IP перед применением", Completed: 0, Total: len(mappings) + 1})
		plan, err := app.PrepareBulkNodeIPChange(ctx, body.PanelID, mappings)
		if err != nil {
			return nil, err
		}
		result, err := app.ApplyBulkNodeIPChange(ctx, telegram.BulkNodeIPApplyInput{PanelID: body.PanelID, Items: plan.Items, EnableDisabled: body.EnableDisabled}, func(update telegram.BulkNodeIPProgress) {
			message := "Обновление IP ноды"
			if update.Item.Plan.NodeName != "" {
				message = "Обновление " + update.Item.Plan.NodeName
			}
			progress(telegram.NodeActualizeProgress{Step: "replace_ip", Message: message, Completed: update.Completed + 1, Total: update.Total + 1})
		})
		if err != nil {
			return nil, err
		}
		return map[string]any{"completed": result.Completed, "warnings": result.Warnings, "failed": result.Failed}, nil
	})
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "Для этой панели уже выполняется массовая смена IP"})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"pending": true, "jobId": jobID})
}

func (s *Server) handleDeployments(w http.ResponseWriter, r *http.Request) {
	if !s.authenticated(w, r) {
		return
	}
	app, ok := s.deploymentApp()
	if !ok {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Развёртывания недоступны"})
		return
	}
	items, err := app.ListDeployments(r.Context(), 50)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "Не удалось загрузить развёртывания"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deployments": items})
}

func deploymentPath(path string) (string, string, bool) {
	parts := strings.Split(strings.TrimPrefix(path, "/api/deployments/"), "/")
	if len(parts) == 1 && parts[0] != "" {
		return parts[0], "", true
	}
	if len(parts) == 2 && parts[0] != "" && parts[1] != "" {
		return parts[0], parts[1], true
	}
	return "", "", false
}

func (s *Server) handleDeploymentRead(w http.ResponseWriter, r *http.Request) {
	if !s.authenticated(w, r) {
		return
	}
	id, action, ok := deploymentPath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	app, available := s.nodes.(telegram.RecoveryApplication)
	if !available {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Карточка развёртывания недоступна"})
		return
	}
	if action == "logs" {
		logs, err := app.ViewSafeLogs(r.Context(), id)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "Не удалось загрузить журнал"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"logs": logs})
		return
	}
	if action != "" {
		http.NotFound(w, r)
		return
	}
	details, err := app.GetDeploymentDetails(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Развёртывание не найдено"})
		return
	}
	writeJSON(w, http.StatusOK, details)
}

func (s *Server) handleRecoveryAction(w http.ResponseWriter, r *http.Request) {
	user, authorized := s.mutationUser(w, r)
	if !authorized {
		return
	}
	id, action, ok := deploymentPath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	recovery, available := s.nodes.(telegram.RecoveryApplication)
	app, baseOK := s.deploymentApp()
	if !available || !baseOK {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Восстановление развёртывания недоступно"})
		return
	}
	jobID, err := s.startBackgroundJob(user.UserID, "recovery", id+":"+action, nil, func(ctx context.Context, progress func(telegram.NodeActualizeProgress)) (any, error) {
		progress(telegram.NodeActualizeProgress{Step: action, Message: "Выполнение безопасного действия с развёртыванием", Completed: 0, Total: 1})
		var result string
		var err error
		switch action {
		case "retry":
			err = recovery.RetryFailedStep(ctx, id)
		case "dns":
			err = recovery.RetryDNS(ctx, id)
		case "recheck":
			result, err = recovery.RecheckRemnawave(ctx, id)
		case "repair-certificate":
			var details telegram.DeploymentDetails
			details, err = recovery.GetDeploymentDetails(ctx, id)
			if err == nil {
				result, err = recovery.BootstrapCertificate(ctx, details.SNI, user.UserID)
			}
			if err == nil {
				err = recovery.RetryFailedStep(ctx, id)
			}
		case "cancel":
			err = app.CancelDeployment(ctx, id)
		default:
			return nil, errors.New("unknown deployment action")
		}
		if err != nil {
			return nil, err
		}
		return map[string]any{"deploymentId": id, "message": result}, nil
	})
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "Действие уже выполняется"})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"pending": true, "jobId": jobID})
}
