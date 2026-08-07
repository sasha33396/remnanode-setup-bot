package orchestrator

import (
	"context"
	"errors"
	"net/netip"
	"strings"

	"remnanode-setup-bot/internal/remnawave"
	"remnanode-setup-bot/internal/telegram"
)

// TelegramApplication adapts DeploymentService to the presentation-only
// Telegram application port.
type TelegramApplication struct {
	service *DeploymentService
}

func NewTelegramApplication(service *DeploymentService) (*TelegramApplication, error) {
	if service == nil {
		return nil, errors.New("deployment service is required")
	}
	return &TelegramApplication{service: service}, nil
}

func (a *TelegramApplication) ListHosts(ctx context.Context) ([]telegram.Host, error) {
	hosts, err := a.service.remnawave.GetHosts(ctx)
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

func (a *TelegramApplication) CheckNodeName(ctx context.Context, name string) error {
	if remnawave.ValidateNodeName(name) != nil {
		return ErrInvalidInput
	}
	nodes, err := a.service.remnawave.GetNodes(ctx)
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

func (a *TelegramApplication) CheckVPSAddress(ctx context.Context, address netip.Addr) error {
	if !publicIP(address) {
		return ErrInvalidInput
	}
	nodes, err := a.service.remnawave.GetNodes(ctx)
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
	prepared, err := a.service.Prepare(ctx, PrepareInput{
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
		SafeWarnings:           append([]string(nil), prepared.SafeWarnings...),
	}, nil
}

func (a *TelegramApplication) StartDeployment(ctx context.Context, input telegram.DeploymentInput, progress func(telegram.Progress) error) error {
	return a.service.Deploy(ctx, StartInput{
		DeploymentID:   input.PreparedDeploymentID,
		OperatorUserID: input.OperatorUserID,
		HostID:         input.HostID,
		NodeName:       input.NodeName,
		VPSIP:          input.VPSIP,
	}, func(update Progress) {
		if progress != nil {
			_ = progress(telegram.Progress{Step: update.Step, Completed: update.Completed, Total: update.Total, SafeMessage: update.SafeMessage})
		}
	})
}

func (a *TelegramApplication) CancelDeployment(ctx context.Context, deploymentID string) error {
	return a.service.Cancel(ctx, deploymentID)
}

func (a *TelegramApplication) ListNodes(ctx context.Context) ([]telegram.NodeSummary, error) {
	nodes, err := a.service.remnawave.GetNodes(ctx)
	if err != nil {
		return nil, errors.New("list Nodes failed")
	}
	result := make([]telegram.NodeSummary, 0, len(nodes))
	for _, node := range nodes {
		result = append(result, telegram.NodeSummary{Name: node.Name, Address: node.Address, Connected: node.IsConnected})
	}
	return result, nil
}

func (a *TelegramApplication) ListDeployments(ctx context.Context, limit int) ([]telegram.DeploymentSummary, error) {
	deployments, err := a.service.repository.ListRecentDeployments(ctx, limit)
	if err != nil {
		return nil, errors.New("list deployments failed")
	}
	result := make([]telegram.DeploymentSummary, 0, len(deployments))
	for _, item := range deployments {
		result = append(result, telegram.DeploymentSummary{ID: item.ID, NodeName: item.NodeName, Status: string(item.Status), UpdatedAt: item.UpdatedAt})
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
