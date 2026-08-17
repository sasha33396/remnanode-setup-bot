package orchestrator

import (
	"context"
	"errors"
	"net/netip"
	"strings"

	"remnanode-setup-bot/internal/certmanager"
	"remnanode-setup-bot/internal/cherryip"
	"remnanode-setup-bot/internal/deployment"
	"remnanode-setup-bot/internal/recovery"
	"remnanode-setup-bot/internal/remnawave"
	"remnanode-setup-bot/internal/royalip"
	"remnanode-setup-bot/internal/telegram"
)

// TelegramApplication adapts DeploymentService to the presentation-only
// Telegram application port.
type TelegramApplication struct {
	panels     map[string]PanelApplicationConfig
	order      []string
	repository DeploymentRepository
	cherryIP   *cherryip.Service
	royalIP    *royalip.Service
}

func (a *TelegramApplication) SetRoyalIPService(service *royalip.Service) error {
	if a == nil || service == nil {
		return errors.New("Royal IP service is required")
	}
	a.royalIP = service
	return nil
}

func (a *TelegramApplication) SetCherryIPService(service *cherryip.Service) error {
	if a == nil || service == nil {
		return errors.New("Cherry IP service is required")
	}
	a.cherryIP = service
	return nil
}

type PanelApplicationConfig struct {
	ID         string
	Name       string
	DNSEnabled bool
	Service    *DeploymentService
	Recovery   *recovery.Service
}

func NewMultiPanelTelegramApplication(panels []PanelApplicationConfig) (*TelegramApplication, error) {
	if len(panels) == 0 {
		return nil, errors.New("at least one panel is required")
	}
	app := &TelegramApplication{panels: make(map[string]PanelApplicationConfig, len(panels))}
	for _, panel := range panels {
		panel.ID, panel.Name = strings.TrimSpace(panel.ID), strings.TrimSpace(panel.Name)
		if panel.ID == "" || panel.Name == "" || panel.Service == nil {
			return nil, errors.New("invalid panel application configuration")
		}
		if _, exists := app.panels[panel.ID]; exists {
			return nil, errors.New("duplicate panel application configuration")
		}
		if app.repository == nil {
			app.repository = panel.Service.repository
		} else if app.repository != panel.Service.repository {
			return nil, errors.New("panels must share deployment repository")
		}
		app.panels[panel.ID] = panel
		app.order = append(app.order, panel.ID)
	}
	return app, nil
}

func NewTelegramApplicationWithRecovery(service *DeploymentService, recoveryService *recovery.Service) (*TelegramApplication, error) {
	return NewMultiPanelTelegramApplication([]PanelApplicationConfig{{ID: "default", Name: "Default", DNSEnabled: true, Service: service, Recovery: recoveryService}})
}

func NewTelegramApplication(service *DeploymentService) (*TelegramApplication, error) {
	if service == nil {
		return nil, errors.New("deployment service is required")
	}
	return NewMultiPanelTelegramApplication([]PanelApplicationConfig{{ID: "default", Name: "Default", DNSEnabled: true, Service: service}})
}

func (a *TelegramApplication) panel(id string) (PanelApplicationConfig, error) {
	panel, ok := a.panels[strings.TrimSpace(id)]
	if !ok {
		return PanelApplicationConfig{}, ErrInvalidInput
	}
	return panel, nil
}

func (a *TelegramApplication) panelForDeployment(ctx context.Context, deploymentID string) (PanelApplicationConfig, error) {
	item, err := a.repository.GetDeployment(ctx, deploymentID)
	if err != nil {
		return PanelApplicationConfig{}, err
	}
	return a.panel(item.PanelID)
}

func (a *TelegramApplication) ListPanels(context.Context) ([]telegram.Panel, error) {
	result := make([]telegram.Panel, 0, len(a.order))
	for _, id := range a.order {
		panel := a.panels[id]
		result = append(result, telegram.Panel{ID: id, Name: panel.Name, DNSEnabled: panel.DNSEnabled})
	}
	return result, nil
}

func (a *TelegramApplication) ListHosts(ctx context.Context, panelID string) ([]telegram.Host, error) {
	panel, err := a.panel(panelID)
	if err != nil {
		return nil, err
	}
	hosts, err := panel.Service.remnawave.GetHosts(ctx)
	if err != nil {
		return nil, errors.New("list Hosts failed")
	}
	result := make([]telegram.Host, 0, len(hosts))
	for _, host := range hosts {
		if host.IsDisabled {
			continue
		}
		readiness := telegram.ReadinessNotReady
		if _, err := remnawave.DeploymentProfileFromHost(host); err == nil {
			readiness = telegram.ReadinessReady
		}
		result = append(result, telegram.Host{ID: host.UUID, Remark: host.Remark, Address: host.Address, ConfigProfileReadiness: readiness})
	}
	return result, nil
}

func (a *TelegramApplication) CheckNodeName(ctx context.Context, panelID, name string) error {
	if remnawave.ValidateNodeName(name) != nil {
		return ErrInvalidInput
	}
	panel, err := a.panel(panelID)
	if err != nil {
		return err
	}
	nodes, err := panel.Service.remnawave.GetNodes(ctx)
	if err != nil {
		return errors.New("check Node name failed")
	}
	for _, node := range nodes {
		if strings.TrimSpace(node.Name) == strings.TrimSpace(name) {
			return telegram.ErrDuplicateNodeName
		}
	}
	return nil
}

func (a *TelegramApplication) CheckVPSAddress(ctx context.Context, panelID string, address netip.Addr) error {
	if !publicIP(address) {
		return ErrInvalidInput
	}
	panel, err := a.panel(panelID)
	if err != nil {
		return err
	}
	nodes, err := panel.Service.remnawave.GetNodes(ctx)
	if err != nil {
		return errors.New("check Node address failed")
	}
	for _, node := range nodes {
		parsed, parseErr := netip.ParseAddr(strings.TrimSpace(node.Address))
		if parseErr == nil && parsed.Unmap() == address.Unmap() {
			return telegram.ErrDuplicateVPSIP
		}
	}
	return nil
}

func (a *TelegramApplication) Preflight(ctx context.Context, input telegram.PreflightInput) (telegram.PreflightResult, error) {
	panel, err := a.panel(input.PanelID)
	if err != nil {
		return telegram.PreflightResult{}, err
	}
	prepared, err := panel.Service.Prepare(ctx, PrepareInput{
		PanelID:        input.PanelID,
		OperatorUserID: input.OperatorUserID,
		HostID:         input.HostID,
		NodeName:       input.NodeName,
		VPSIP:          input.VPSIP,
		Password:       input.Password,
	})
	if err != nil {
		return telegram.PreflightResult{}, err
	}
	return telegram.PreflightResult{
		PreparedDeploymentID:   prepared.Deployment.ID,
		DNSZone:                prepared.DNSZone,
		CertificateReadiness:   telegramCertificateReadiness(prepared.CertificateReadiness),
		ConfigProfileReadiness: readiness(prepared.ConfigProfileReadiness),
		Warnings:               telegramNotices(prepared.Warnings),
	}, nil
}

func (a *TelegramApplication) StartDeployment(ctx context.Context, input telegram.DeploymentInput, progress func(telegram.Progress) error) error {
	panel, err := a.panelForDeployment(ctx, input.PreparedDeploymentID)
	if err != nil {
		return err
	}
	return panel.Service.Deploy(ctx, StartInput{
		DeploymentID:   input.PreparedDeploymentID,
		OperatorUserID: input.OperatorUserID,
		HostID:         input.HostID,
		NodeName:       input.NodeName,
		VPSIP:          input.VPSIP,
	}, func(update Progress) {
		if progress != nil {
			_ = progress(telegram.Progress{Step: update.Step, Completed: update.Completed, Total: update.Total, SafeMessage: update.SafeMessage, Status: telegramProgressStatus(update.Status), Code: update.Code})
		}
	})
}

func telegramNotices(values []OperatorNotice) []telegram.OperatorNotice {
	result := make([]telegram.OperatorNotice, 0, len(values))
	for _, value := range values {
		result = append(result, telegram.OperatorNotice{Code: value.Code, Message: value.Message})
	}
	return result
}

func telegramProgressStatus(value deployment.StepStatus) telegram.ProgressStatus {
	switch value {
	case deployment.StepStatusCompleted:
		return telegram.ProgressCompleted
	case deployment.StepStatusFailed:
		return telegram.ProgressFailed
	case deployment.StepStatusSkipped:
		return telegram.ProgressSkipped
	default:
		return telegram.ProgressRunning
	}
}

func (a *TelegramApplication) CancelDeployment(ctx context.Context, deploymentID string) error {
	panel, err := a.panelForDeployment(ctx, deploymentID)
	if err != nil {
		return err
	}
	return panel.Service.Cancel(ctx, deploymentID)
}

func (a *TelegramApplication) RetryFailedStep(ctx context.Context, deploymentID string) error {
	panel, err := a.panelForDeployment(ctx, deploymentID)
	if err != nil {
		return err
	}
	return panel.Service.RetryFailedStep(ctx, deploymentID, nil)
}

func (a *TelegramApplication) GetDeploymentDetails(ctx context.Context, deploymentID string) (telegram.DeploymentDetails, error) {
	item, err := a.repository.GetDeployment(ctx, deploymentID)
	if err != nil {
		return telegram.DeploymentDetails{}, err
	}
	panel, err := a.panel(item.PanelID)
	if err != nil {
		return telegram.DeploymentDetails{}, err
	}
	details := telegram.DeploymentDetails{
		DeploymentSummary: telegram.DeploymentSummary{PanelName: panel.Name, ID: item.ID, NodeName: item.NodeName, Status: string(item.Status), UpdatedAt: item.UpdatedAt},
		CurrentStep:       item.CurrentStep,
		SNI:               item.SNIDomain,
		CanRetryDNS:       item.Status == deployment.StatusDNSFailed,
		CanCancel:         !item.Status.Terminal(),
	}
	if item.SafeErrorMessage != nil {
		details.SafeError = *item.SafeErrorMessage
	}
	if item.Status == deployment.StatusFailed {
		switch item.CurrentStep {
		case stepPrepareCertificate, stepProvisioning, stepCreateNode, stepWaitNode, stepAddDNS:
			details.CanRetryStep = true
		}
		switch item.CurrentStep {
		case stepCreateNode, stepWaitNode, stepAddDNS:
			details.CanRecheck = true
		}
		details.CanRepairCert = item.CurrentStep == stepPrepareCertificate && strings.TrimSpace(item.SNIDomain) != ""
	}
	if item.Status == deployment.StatusManualReview {
		details.CanRecheck = true
	}
	return details, nil
}

func (a *TelegramApplication) RetryDNS(ctx context.Context, deploymentID string) error {
	panel, err := a.panelForDeployment(ctx, deploymentID)
	if err != nil {
		return err
	}
	return panel.Service.RetryDNS(ctx, deploymentID, nil)
}

func (a *TelegramApplication) RecheckRemnawave(ctx context.Context, deploymentID string) (string, error) {
	panel, panelErr := a.panelForDeployment(ctx, deploymentID)
	if panelErr != nil || panel.Recovery == nil {
		return "", errors.New("recovery service is unavailable")
	}
	result, err := panel.Recovery.RecheckRemnawave(ctx, deploymentID)
	if err != nil {
		return "", err
	}
	return result.SafeMessage, nil
}

func (a *TelegramApplication) ViewSafeLogs(ctx context.Context, deploymentID string) ([]string, error) {
	panel, panelErr := a.panelForDeployment(ctx, deploymentID)
	if panelErr != nil {
		return nil, panelErr
	}
	entries, err := panel.Service.SafeLogs(ctx, deploymentID)
	if err != nil {
		return nil, err
	}
	result := make([]string, 0, len(entries))
	for _, entry := range entries {
		line := entry.Step + " [" + entry.Status + "]"
		if entry.Summary != "" {
			line += ": " + entry.Summary
		}
		result = append(result, line)
	}
	return result, nil
}

func (a *TelegramApplication) BootstrapCertificate(ctx context.Context, sni string, operatorUserID int64) (string, error) {
	domain, err := certmanager.NormalizeSNI(sni)
	if err != nil {
		return "Invalid certificate SNI.", ErrInvalidInput
	}
	if len(a.order) == 1 {
		return a.panels[a.order[0]].Service.BootstrapCertificate(ctx, domain, operatorUserID)
	}

	matches := make([]PanelApplicationConfig, 0, 1)
	for _, id := range a.order {
		panel := a.panels[id]
		hosts, listErr := panel.Service.remnawave.GetHosts(ctx)
		if listErr != nil {
			return "Could not safely resolve the certificate panel because Hosts are unavailable.", listErr
		}
		for _, host := range hosts {
			if host.IsDisabled {
				continue
			}
			profile, profileErr := remnawave.DeploymentProfileFromHost(host)
			if profileErr != nil {
				continue
			}
			hostDomain, domainErr := certmanager.NormalizeSNI(profile.SNIDomain)
			if domainErr == nil && hostDomain == domain {
				matches = append(matches, panel)
				break
			}
		}
	}
	if len(matches) == 0 {
		return "No enabled Remnawave Host owns this SNI.", ErrHostUnavailable
	}
	if len(matches) > 1 {
		return "More than one panel owns this SNI; automatic certificate panel selection is unsafe.", ErrInvalidInput
	}
	return matches[0].Service.BootstrapCertificate(ctx, domain, operatorUserID)
}

func (a *TelegramApplication) FindNodeForIPChange(ctx context.Context, panelID, query string) (telegram.NodeIPChangeTarget, error) {
	panel, err := a.panel(panelID)
	if err != nil {
		return telegram.NodeIPChangeTarget{}, err
	}
	target, err := panel.Service.FindNodeForIPChange(ctx, query)
	if err != nil {
		return telegram.NodeIPChangeTarget{}, err
	}
	return telegram.NodeIPChangeTarget{PanelName: panel.Name, DNSEnabled: panel.DNSEnabled, UUID: target.UUID, Name: target.Name, Address: target.Address, Connected: target.Connected, DNSZones: target.DNSZones, IsManaged: target.Managed}, nil
}

func (a *TelegramApplication) ReplaceNodeIP(ctx context.Context, input telegram.NodeIPChangeInput) (string, error) {
	panel, err := a.panel(input.PanelID)
	if err != nil {
		return "", err
	}
	return panel.Service.ReplaceNodeIP(ctx, NodeIPChangeInput{NodeUUID: input.NodeUUID, ExpectedIP: input.ExpectedIP, NewIP: input.NewIP})
}

func (a *TelegramApplication) ConfigureCherryIP(ctx context.Context, input telegram.CherryIPInput) (telegram.CherryIPResult, error) {
	if a.cherryIP == nil {
		return telegram.CherryIPResult{}, errors.New("Cherry IP service is unavailable")
	}
	result, err := a.cherryIP.Configure(ctx, cherryip.Input{ServerIP: input.ServerIP, FloatingIP: input.FloatingIP, Password: input.Password})
	if err != nil {
		return telegram.CherryIPResult{}, err
	}
	return telegram.CherryIPResult{Interface: result.Interface, LiveConfigured: result.LiveConfigured, Persistent: result.Persistent, PersistentNote: result.PersistentNote}, nil
}

func (a *TelegramApplication) ConfigureRoyalIP(ctx context.Context, input telegram.RoyalIPInput) (telegram.RoyalIPResult, error) {
	if a.royalIP == nil {
		return telegram.RoyalIPResult{}, errors.New("Royal IP service is unavailable")
	}
	result, err := a.royalIP.Configure(ctx, royalip.Input{ServerIP: input.ServerIP, NewIP: input.NewIP, Password: input.Password})
	if err != nil {
		return telegram.RoyalIPResult{}, err
	}
	return telegram.RoyalIPResult{Interface: result.Interface, PrefixBits: result.PrefixBits, Gateway: result.Gateway, NetplanFile: result.NetplanFile, BackupFile: result.BackupFile}, nil
}

func (a *TelegramApplication) ListNodes(ctx context.Context) ([]telegram.NodeSummary, error) {
	result := make([]telegram.NodeSummary, 0)
	for _, id := range a.order {
		panel := a.panels[id]
		nodes, err := panel.Service.remnawave.GetNodes(ctx)
		if err != nil {
			return nil, errors.New("list Nodes failed")
		}
		for _, node := range nodes {
			result = append(result, telegram.NodeSummary{PanelName: panel.Name, Name: node.Name, Address: node.Address, Connected: node.IsConnected})
		}
	}
	return result, nil
}

func (a *TelegramApplication) ListDeployments(ctx context.Context, limit int) ([]telegram.DeploymentSummary, error) {
	deployments, err := a.repository.ListRecentDeployments(ctx, limit)
	if err != nil {
		return nil, errors.New("list deployments failed")
	}
	result := make([]telegram.DeploymentSummary, 0, len(deployments))
	for _, item := range deployments {
		panelName := item.PanelID
		if panel, ok := a.panels[item.PanelID]; ok {
			panelName = panel.Name
		}
		result = append(result, telegram.DeploymentSummary{PanelName: panelName, ID: item.ID, NodeName: item.NodeName, Status: string(item.Status), UpdatedAt: item.UpdatedAt})
	}
	return result, nil
}

func telegramCertificateReadiness(value CertificateReadiness) telegram.Readiness {
	switch value {
	case CertificateReady:
		return telegram.ReadinessReady
	case CertificateNotReady:
		return telegram.ReadinessNotReady
	default:
		return telegram.ReadinessUnknown
	}
}

func readiness(ready bool) telegram.Readiness {
	if ready {
		return telegram.ReadinessReady
	}
	return telegram.ReadinessNotReady
}

var _ telegram.Application = (*TelegramApplication)(nil)
