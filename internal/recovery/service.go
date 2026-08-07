// Package recovery classifies unfinished deployments without blindly
// repeating external side effects.
package recovery

import (
	"context"
	"errors"
	"net/netip"
	"strings"

	"remnanode-setup-bot/internal/deployment"
	"remnanode-setup-bot/internal/dnsbalancer"
	"remnanode-setup-bot/internal/remnawave"
	"remnanode-setup-bot/internal/repository"
)

type Repository interface {
	repository.DeploymentRepository
}

type RemnawaveAPI interface {
	GetNodes(context.Context) ([]remnawave.Node, error)
	GetNode(context.Context, string) (remnawave.Node, error)
}

type DNSAPI interface {
	FindZone(context.Context, string) (dnsbalancer.ZoneMatch, error)
}

type Classification string

const (
	SafeToRecheck      Classification = "SAFE_TO_RECHECK"
	SafeToRetryDNS     Classification = "SAFE_TO_RETRY_DNS"
	RecoveredCompleted Classification = "RECOVERED_COMPLETED"
	NeedsManualReview  Classification = "NEEDS_MANUAL_REVIEW"
)

type Result struct {
	DeploymentID   string
	Classification Classification
	SafeMessage    string
}

type Service struct {
	repository Repository
	remnawave  RemnawaveAPI
	dns        DNSAPI
}

func New(repository Repository, remnawaveAPI RemnawaveAPI, dns DNSAPI) (*Service, error) {
	if repository == nil || remnawaveAPI == nil || dns == nil {
		return nil, errors.New("recovery dependencies are required")
	}
	return &Service{repository: repository, remnawave: remnawaveAPI, dns: dns}, nil
}

func (s *Service) RecoverUnfinished(ctx context.Context, limit int) ([]Result, error) {
	items, err := s.repository.FindUnfinishedDeployments(ctx, limit)
	if err != nil {
		return nil, errors.New("load unfinished deployments failed")
	}
	results := make([]Result, 0, len(items))
	for _, item := range items {
		result, err := s.classify(ctx, item)
		if err != nil {
			return results, err
		}
		results = append(results, result)
	}
	return results, nil
}

func (s *Service) RecheckRemnawave(ctx context.Context, deploymentID string) (Result, error) {
	item, err := s.repository.GetDeployment(ctx, deploymentID)
	if err != nil {
		return Result{}, errors.New("load deployment for recheck failed")
	}
	return s.classify(ctx, item)
}

func (s *Service) classify(ctx context.Context, item deployment.Deployment) (Result, error) {
	switch item.Status {
	case deployment.StatusCompleted:
		return Result{DeploymentID: item.ID, Classification: RecoveredCompleted, SafeMessage: "Deployment is already completed"}, nil
	case deployment.StatusCreatingRemnawave, deployment.StatusWaitingRemnawave, deployment.StatusAddingToDNS:
		return s.inspectExternalState(ctx, item)
	case deployment.StatusFailed, deployment.StatusManualReview:
		switch item.CurrentStep {
		case "create_remnawave_node", "wait_remnawave", "add_dns":
			return s.inspectExternalState(ctx, item)
		default:
			return s.manual(ctx, item, "Failed deployment cannot be rechecked automatically at this step")
		}
	case deployment.StatusDNSFailed:
		return s.inspectExternalState(ctx, item)
	default:
		return s.manual(ctx, item, "Restart occurred during a step that cannot be repeated automatically")
	}
}

func (s *Service) inspectExternalState(ctx context.Context, item deployment.Deployment) (Result, error) {
	if item.RemnawaveNodeUUID == nil {
		nodes, err := s.remnawave.GetNodes(ctx)
		if err != nil {
			return s.manual(ctx, item, "Remnawave state could not be verified")
		}
		var exact []remnawave.Node
		for _, node := range nodes {
			address, _ := netip.ParseAddr(strings.TrimSpace(node.Address))
			if strings.TrimSpace(node.Name) == item.NodeName && address.IsValid() && address.Unmap() == item.TargetVPSIP.Unmap() {
				exact = append(exact, node)
			}
		}
		if len(exact) != 1 {
			return s.manual(ctx, item, "Existing Remnawave Node could not be identified safely")
		}
		updated, err := s.repository.SetRemnawaveNodeUUID(ctx, item.ID, exact[0].UUID)
		if err != nil {
			return Result{}, errors.New("persist recovered Remnawave Node failed")
		}
		item = updated
	}
	node, err := s.remnawave.GetNode(ctx, *item.RemnawaveNodeUUID)
	if err != nil {
		return s.manual(ctx, item, "Remnawave Node health could not be verified")
	}
	if !node.IsConnected && node.IsConnecting {
		if _, err := s.repository.UpdateDeploymentState(ctx, item.ID, repository.UpdateDeploymentStateParams{Status: deployment.StatusWaitingRemnawave, CurrentStep: "wait_remnawave"}); err != nil {
			return Result{}, errors.New("persist Remnawave recheck state failed")
		}
		return Result{DeploymentID: item.ID, Classification: SafeToRecheck, SafeMessage: "Remnawave Node is still connecting"}, nil
	}
	if !node.IsConnected {
		return s.manual(ctx, item, safeNodeMessage(node.LastStatusMessage))
	}
	contains, err := s.dnsContains(ctx, item.SNIDomain, item.TargetVPSIP)
	if err != nil {
		return s.manual(ctx, item, "DNS state could not be verified")
	}
	if contains {
		if _, err := s.repository.UpdateDeploymentState(ctx, item.ID, repository.UpdateDeploymentStateParams{Status: deployment.StatusCompleted, CurrentStep: "completed"}); err != nil {
			return Result{}, errors.New("persist recovered completion failed")
		}
		return Result{DeploymentID: item.ID, Classification: RecoveredCompleted, SafeMessage: "Node and DNS were already healthy"}, nil
	}
	if _, err := s.repository.UpdateDeploymentState(ctx, item.ID, repository.UpdateDeploymentStateParams{Status: deployment.StatusDNSFailed, CurrentStep: "add_dns", SafeErrorCode: pointer("DNS_RETRY_REQUIRED"), SafeErrorMessage: pointer("Node is healthy and DNS requires an explicit retry")}); err != nil {
		return Result{}, errors.New("persist DNS retry state failed")
	}
	return Result{DeploymentID: item.ID, Classification: SafeToRetryDNS, SafeMessage: "Node is healthy and DNS can be retried"}, nil
}

func (s *Service) dnsContains(ctx context.Context, sni string, target netip.Addr) (bool, error) {
	match, err := s.dns.FindZone(ctx, sni)
	if err != nil {
		return false, err
	}
	for _, value := range match.Zone.IPs {
		if ip, err := netip.ParseAddr(strings.TrimSpace(value)); err == nil && ip.Unmap() == target.Unmap() {
			return true, nil
		}
	}
	for _, node := range match.Zone.Nodes {
		if ip, err := netip.ParseAddr(strings.TrimSpace(node.IP)); err == nil && ip.Unmap() == target.Unmap() {
			return true, nil
		}
	}
	return false, nil
}

func (s *Service) manual(ctx context.Context, item deployment.Deployment, message string) (Result, error) {
	message = safeNodeMessage(&message)
	if _, err := s.repository.UpdateDeploymentState(ctx, item.ID, repository.UpdateDeploymentStateParams{Status: deployment.StatusManualReview, CurrentStep: item.CurrentStep, SafeErrorCode: pointer("MANUAL_REVIEW_REQUIRED"), SafeErrorMessage: &message}); err != nil {
		return Result{}, errors.New("persist manual review state failed")
	}
	return Result{DeploymentID: item.ID, Classification: NeedsManualReview, SafeMessage: message}, nil
}

func safeNodeMessage(value *string) string {
	fallback := "Deployment requires manual review"
	if value == nil {
		return fallback
	}
	message := strings.TrimSpace(strings.NewReplacer("\r", " ", "\n", " ", "\t", " ").Replace(*value))
	if message == "" {
		return fallback
	}
	if len(message) > 200 {
		message = message[:200]
	}
	return message
}

func pointer(value string) *string { return &value }
