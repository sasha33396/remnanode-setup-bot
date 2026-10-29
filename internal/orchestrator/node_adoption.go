package orchestrator

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"time"

	"remnanode-setup-bot/internal/deployment"
	"remnanode-setup-bot/internal/remnawave"
	"remnanode-setup-bot/internal/repository"
)

// AdoptLegacyNode installs the deployment key and runs the complete standard
// provisioning workflow for an already existing Remnawave Node. The Node is
// only left bound to the deployment after the workflow reaches COMPLETED.
func (s *DeploymentService) AdoptLegacyNode(ctx context.Context, input AdoptLegacyNodeInput, progress ProgressSink) (deployment.Deployment, error) {
	defer clear(input.Password)
	input.NodeUUID = strings.TrimSpace(input.NodeUUID)
	if input.OperatorUserID <= 0 || input.NodeUUID == "" || len(input.Password) == 0 {
		return deployment.Deployment{}, ErrInvalidInput
	}
	if _, err := s.repository.FindDeploymentByPanelNodeUUID(ctx, s.config.PanelID, input.NodeUUID); err == nil {
		return deployment.Deployment{}, errors.New("Node is already managed")
	} else if !errors.Is(err, repository.ErrNotFound) {
		return deployment.Deployment{}, ErrPersistenceFailed
	}

	node, err := s.nodeByUUID(ctx, input.NodeUUID)
	if err != nil {
		return deployment.Deployment{}, err
	}
	address, err := netip.ParseAddr(strings.TrimSpace(node.Address))
	if err != nil || !publicIP(address) {
		return deployment.Deployment{}, ErrInvalidInput
	}
	hosts, err := s.remnawave.GetHosts(ctx)
	if err != nil {
		return deployment.Deployment{}, safeError("HOSTS_UNAVAILABLE", "Remnawave Hosts are unavailable", ErrHostUnavailable)
	}
	matchingHosts := adoptionNodeHosts(node, hosts)
	if len(matchingHosts) != 1 {
		return deployment.Deployment{}, safeError("NODE_HOST_AMBIGUOUS", "Could not determine the Node Host profile", ErrHostUnavailable)
	}
	host, profile, err := s.selectedHost(ctx, matchingHosts[0].UUID)
	if err != nil {
		return deployment.Deployment{}, err
	}

	created, err := s.repository.CreateDeployment(ctx, repository.CreateDeploymentParams{
		PanelID:                   s.config.PanelID,
		TelegramOperatorUserID:    input.OperatorUserID,
		SelectedRemnawaveHostUUID: host.UUID,
		SelectedHostRemark:        host.Remark,
		SNIDomain:                 profile.SNIDomain,
		NodeName:                  strings.TrimSpace(node.Name),
		TargetVPSIP:               address.Unmap(),
	})
	if err != nil {
		return deployment.Deployment{}, safeError("PERSISTENCE_CREATE_FAILED", "Could not create deployment state", ErrPersistenceFailed)
	}
	if s.config.Observer != nil {
		s.config.Observer.DeploymentCreated()
	}

	preflightErr := s.preflightLegacyAdoption(ctx, created, address.Unmap(), input.Password)
	// The standard workflow uses only the installed deployment key from here.
	// Clear the caller-owned password buffer immediately after initial SSH use.
	clear(input.Password)
	if preflightErr != nil {
		return deployment.Deployment{}, preflightErr
	}
	start := StartInput{
		DeploymentID:   created.ID,
		OperatorUserID: input.OperatorUserID,
		HostID:         host.UUID,
		NodeName:       strings.TrimSpace(node.Name),
		VPSIP:          address.Unmap(),
	}
	if err := s.deployExistingNode(ctx, start, node.UUID, progress); err != nil {
		s.clearFailedAdoptionBinding(ctx, created.ID, node.UUID)
		return deployment.Deployment{}, err
	}
	completed, err := s.repository.GetDeployment(ctx, created.ID)
	if err != nil || completed.Status != deployment.StatusCompleted || completed.RemnawaveNodeUUID == nil || *completed.RemnawaveNodeUUID != node.UUID {
		s.clearFailedAdoptionBinding(ctx, created.ID, node.UUID)
		return deployment.Deployment{}, safeError("ADOPTION_VERIFY_FAILED", "Node adoption could not be verified", ErrPersistenceFailed)
	}
	return completed, nil
}

// adoptionNodeHosts first uses the explicit Host.Nodes binding. Older panels
// may not populate it, so an exact profile match remains the safe fallback.
// A Host inbound may also be one of several active Node inbounds.
func adoptionNodeHosts(node remnawave.Node, hosts []remnawave.Host) []remnawave.Host {
	profileUUID, inboundUUIDs := nodeProfileFingerprint(node)
	explicit := make([]remnawave.Host, 0, 1)
	compatible := make([]remnawave.Host, 0, 1)
	for _, host := range hosts {
		if host.IsDisabled {
			continue
		}
		profile, err := remnawave.DeploymentProfileFromHost(host)
		if err != nil || strings.TrimSpace(profile.ActiveConfigProfileUUID) != strings.TrimSpace(profileUUID) || !containsUUID(inboundUUIDs, profile.ActiveInbounds[0]) {
			continue
		}
		compatible = append(compatible, host)
		if containsUUID(host.Nodes, node.UUID) {
			explicit = append(explicit, host)
		}
	}
	if len(explicit) > 0 {
		return explicit
	}
	exact := matchingNodeHosts(node, hosts)
	if len(exact) == 1 {
		return exact
	}
	return compatible
}

func containsUUID(values []string, target string) bool {
	target = strings.TrimSpace(target)
	for _, value := range values {
		if strings.TrimSpace(value) == target {
			return true
		}
	}
	return false
}

func (s *DeploymentService) preflightLegacyAdoption(ctx context.Context, created deployment.Deployment, address netip.Addr, password []byte) error {
	runCtx, release, err := s.beginExecution(ctx, created.ID)
	if err != nil {
		return err
	}
	defer release()
	if err := s.acquire(runCtx); err != nil {
		return err
	}
	defer s.releaseSlot()
	if err := s.beginStage(runCtx, created.ID, deployment.StatusPreflight, stepPreflight); err != nil {
		return err
	}
	secret := append([]byte(nil), password...)
	result, preflightErr := s.vps.Preflight(runCtx, PreflightVPSInput{DeploymentID: created.ID, Address: address, Password: secret, AllowExistingInstallation: true})
	clear(secret)
	if runCtx.Err() != nil {
		return runCtx.Err()
	}
	if preflightErr != nil {
		return s.failStage(runCtx, created.ID, stepPreflight, deployment.StatusFailed, "PREFLIGHT_FAILED", "VPS preflight could not be completed", ErrPreflightFailed)
	}
	if !result.Passed() {
		message := "VPS did not pass preflight"
		if len(result.FatalFailures) > 0 {
			message = safeText(result.FatalFailures[0].Message, message)
		}
		return s.failStage(runCtx, created.ID, stepPreflight, deployment.StatusFailed, "PREFLIGHT_REJECTED", message, ErrPreflightFailed)
	}
	return s.completeStage(runCtx, created.ID, stepPreflight, "VPS preflight passed; deployment key installed")
}

func (s *DeploymentService) clearFailedAdoptionBinding(ctx context.Context, deploymentID, nodeUUID string) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	current, err := s.repository.GetDeployment(cleanupCtx, deploymentID)
	if err != nil || current.RemnawaveNodeUUID == nil || *current.RemnawaveNodeUUID != nodeUUID || current.Status == deployment.StatusCompleted {
		return
	}
	if _, err := s.repository.ClearRemnawaveNodeUUID(cleanupCtx, deploymentID); err != nil && s.logger != nil {
		s.logger.Error("failed to clear legacy Node adoption binding", "deployment_id", deploymentID, "node_uuid", nodeUUID, "error", err)
	}
}
