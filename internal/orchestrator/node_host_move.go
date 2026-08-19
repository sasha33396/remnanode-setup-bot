package orchestrator

import (
	"context"
	"errors"
	"net/netip"
	"sort"
	"strings"
	"time"

	"remnanode-setup-bot/internal/certificates"
	"remnanode-setup-bot/internal/remnawave"
	"remnanode-setup-bot/internal/repository"
)

var (
	ErrNodeHostMoveFailed = errors.New("Node Host move failed")
	ErrNodeStateChanged   = errors.New("Node profile changed while waiting for confirmation")
	ErrNodeHostUnchanged  = errors.New("Node already uses selected Host profile")
)

type NodeHostMoveTarget struct {
	UUID                      string
	Name                      string
	Address                   string
	Managed                   bool
	CurrentHostKnown          bool
	CurrentHostRemark         string
	CurrentHostAddress        string
	ExpectedConfigProfileUUID string
	ExpectedInboundUUIDs      []string
}

type NodeHostMoveOption struct {
	UUID    string
	Remark  string
	Address string
}

type NodeHostMoveInput struct {
	NodeUUID                  string
	TargetHostUUID            string
	ExpectedConfigProfileUUID string
	ExpectedInboundUUIDs      []string
	Password                  []byte
}

type NodeHostMoveResult struct {
	NodeName          string
	PreviousHostKnown bool
	PreviousHost      string
	TargetHost        string
	TargetAddress     string
	Managed           bool
	DNSUpdated        bool
}

// PrepareNodeHostMove resolves the live Node and only returns enabled Hosts
// with a complete profile mapping from the same panel. The live profile
// fingerprint is retained for stale-confirmation protection.
func (s *DeploymentService) PrepareNodeHostMove(ctx context.Context, nodeUUID string) (NodeHostMoveTarget, []NodeHostMoveOption, error) {
	nodeUUID = strings.TrimSpace(nodeUUID)
	if nodeUUID == "" {
		return NodeHostMoveTarget{}, nil, ErrInvalidInput
	}
	node, err := s.nodeByUUID(ctx, nodeUUID)
	if err != nil {
		return NodeHostMoveTarget{}, nil, err
	}
	hosts, err := s.remnawave.GetHosts(ctx)
	if err != nil {
		return NodeHostMoveTarget{}, nil, ErrNodeHostMoveFailed
	}
	profileUUID, inboundUUIDs := nodeProfileFingerprint(node)
	target := NodeHostMoveTarget{
		UUID:                      node.UUID,
		Name:                      node.Name,
		Address:                   node.Address,
		ExpectedConfigProfileUUID: profileUUID,
		ExpectedInboundUUIDs:      inboundUUIDs,
	}
	if _, managedErr := s.repository.FindDeploymentByPanelNodeUUID(ctx, s.config.PanelID, node.UUID); managedErr == nil {
		target.Managed = true
	} else if !errors.Is(managedErr, repository.ErrNotFound) {
		return NodeHostMoveTarget{}, nil, ErrPersistenceFailed
	}

	current := matchingNodeHosts(node, hosts)
	if len(current) != 1 {
		return NodeHostMoveTarget{}, nil, ErrNodeHostMoveFailed
	}
	target.CurrentHostKnown = true
	target.CurrentHostRemark = current[0].Remark
	target.CurrentHostAddress = current[0].Address
	options := make([]NodeHostMoveOption, 0, len(hosts))
	for _, host := range hosts {
		if host.IsDisabled {
			continue
		}
		profile, profileErr := remnawave.DeploymentProfileFromHost(host)
		if profileErr != nil || sameNodeProfile(profileUUID, inboundUUIDs, profile.ActiveConfigProfileUUID, profile.ActiveInbounds) {
			continue
		}
		options = append(options, NodeHostMoveOption{UUID: host.UUID, Remark: host.Remark, Address: profile.SNIDomain})
	}
	sort.Slice(options, func(i, j int) bool {
		if strings.EqualFold(options[i].Remark, options[j].Remark) {
			return options[i].UUID < options[j].UUID
		}
		return strings.ToLower(options[i].Remark) < strings.ToLower(options[j].Remark)
	})
	return target, options, nil
}

// MoveNodeToHost re-reads both the Node and Host, switches the server-side SNI,
// then updates Remnawave's configProfile binding, and finally moves the Node IP
// from the previous SNI zone to the target SNI zone. The Node address is never
// modified.
func (s *DeploymentService) MoveNodeToHost(ctx context.Context, input NodeHostMoveInput) (NodeHostMoveResult, error) {
	input.NodeUUID = strings.TrimSpace(input.NodeUUID)
	input.TargetHostUUID = strings.TrimSpace(input.TargetHostUUID)
	if input.NodeUUID == "" || input.TargetHostUUID == "" || len(input.Password) == 0 || s.config.NodeSNISwitcher == nil {
		return NodeHostMoveResult{}, ErrInvalidInput
	}
	runCtx, release, err := s.beginExecution(ctx, "node-host:"+input.NodeUUID)
	if err != nil {
		return NodeHostMoveResult{}, err
	}
	defer release()
	if err := s.acquire(runCtx); err != nil {
		return NodeHostMoveResult{}, err
	}
	defer s.releaseSlot()

	node, err := s.nodeByUUID(runCtx, input.NodeUUID)
	if err != nil {
		return NodeHostMoveResult{}, err
	}
	profileUUID, inboundUUIDs := nodeProfileFingerprint(node)
	if !sameNodeProfile(profileUUID, inboundUUIDs, strings.TrimSpace(input.ExpectedConfigProfileUUID), canonicalUUIDs(input.ExpectedInboundUUIDs)) {
		return NodeHostMoveResult{}, ErrNodeStateChanged
	}
	hosts, err := s.remnawave.GetHosts(runCtx)
	if err != nil {
		return NodeHostMoveResult{}, ErrNodeHostMoveFailed
	}
	var selected *remnawave.Host
	for index := range hosts {
		if hosts[index].UUID == input.TargetHostUUID {
			selected = &hosts[index]
			break
		}
	}
	if selected == nil || selected.IsDisabled {
		return NodeHostMoveResult{}, ErrHostUnavailable
	}
	targetProfile, err := remnawave.DeploymentProfileFromHost(*selected)
	if err != nil {
		return NodeHostMoveResult{}, ErrHostUnavailable
	}
	if sameNodeProfile(profileUUID, inboundUUIDs, targetProfile.ActiveConfigProfileUUID, targetProfile.ActiveInbounds) {
		return NodeHostMoveResult{}, ErrNodeHostUnchanged
	}
	previous := matchingNodeHosts(node, hosts)
	if len(previous) != 1 {
		return NodeHostMoveResult{}, ErrNodeHostMoveFailed
	}
	previousProfile, err := remnawave.DeploymentProfileFromHost(previous[0])
	if err != nil || strings.EqualFold(previousProfile.SNIDomain, targetProfile.SNIDomain) {
		return NodeHostMoveResult{}, ErrNodeHostMoveFailed
	}
	address, err := nodeAddress(node.Address)
	if err != nil {
		return NodeHostMoveResult{}, ErrNodeHostMoveFailed
	}
	managedDeployment, managedErr := s.repository.FindDeploymentByPanelNodeUUID(runCtx, s.config.PanelID, node.UUID)
	managed := managedErr == nil
	if managedErr != nil && !errors.Is(managedErr, repository.ErrNotFound) {
		return NodeHostMoveResult{}, ErrPersistenceFailed
	}
	previousCertificate, err := s.certificates.Prepare(runCtx, previousProfile.SNIDomain)
	if err != nil {
		return NodeHostMoveResult{}, ErrCertificateUnavailable
	}
	defer previousCertificate.Destroy()
	targetCertificate, err := s.certificates.Prepare(runCtx, targetProfile.SNIDomain)
	if err != nil {
		return NodeHostMoveResult{}, ErrCertificateUnavailable
	}
	defer targetCertificate.Destroy()
	if err := s.config.NodeSNISwitcher.SwitchNodeSNI(runCtx, NodeSNISwitchInput{
		Address: address, PreviousSNI: previousProfile.SNIDomain, TargetSNI: targetProfile.SNIDomain,
		Password: input.Password, PreviousCertificate: previousCertificate, Certificate: targetCertificate,
	}); err != nil {
		return NodeHostMoveResult{}, ErrNodeSNISwitchFailed
	}
	updated, err := s.remnawave.UpdateNodeProfile(runCtx, remnawave.UpdateNodeProfileInput{UUID: node.UUID, Host: *selected})
	if err != nil {
		s.rollbackNodeHostMove(runCtx, node.UUID, previous[0], address, targetProfile.SNIDomain, input.Password, targetCertificate, previousCertificate, false)
		return NodeHostMoveResult{}, ErrNodeHostMoveFailed
	}
	updatedProfile, updatedInbounds := nodeProfileFingerprint(updated)
	if !sameNodeProfile(updatedProfile, updatedInbounds, targetProfile.ActiveConfigProfileUUID, targetProfile.ActiveInbounds) {
		s.rollbackNodeHostMove(runCtx, node.UUID, previous[0], address, targetProfile.SNIDomain, input.Password, targetCertificate, previousCertificate, false)
		return NodeHostMoveResult{}, ErrNodeHostMoveFailed
	}
	dnsChanged := false
	dnsUpdated := !s.config.DNSDisabled
	if !s.config.DNSDisabled {
		dnsResult, err := s.dns.MoveIP(runCtx, previousProfile.SNIDomain, targetProfile.SNIDomain, address)
		if err != nil {
			s.rollbackNodeHostMove(runCtx, node.UUID, previous[0], address, targetProfile.SNIDomain, input.Password, targetCertificate, previousCertificate, false)
			return NodeHostMoveResult{}, ErrDNSUpdateFailed
		}
		dnsChanged = dnsResult.Changed
	}
	if managed {
		if _, err := s.repository.SetNodeHostBinding(runCtx, managedDeployment.ID, repository.SetNodeHostBindingParams{
			HostUUID: selected.UUID, HostRemark: selected.Remark, SNIDomain: targetProfile.SNIDomain,
		}); err != nil {
			s.rollbackNodeHostMove(runCtx, node.UUID, previous[0], address, targetProfile.SNIDomain, input.Password, targetCertificate, previousCertificate, dnsChanged)
			return NodeHostMoveResult{}, ErrPersistenceFailed
		}
	}
	result := NodeHostMoveResult{NodeName: node.Name, TargetHost: selected.Remark, TargetAddress: targetProfile.SNIDomain, Managed: managed, DNSUpdated: dnsUpdated}
	if len(previous) == 1 {
		result.PreviousHostKnown = true
		result.PreviousHost = previous[0].Remark
	}
	return result, nil
}

func (s *DeploymentService) rollbackNodeHostMove(ctx context.Context, nodeUUID string, previousHost remnawave.Host, address netip.Addr, targetSNI string, password []byte, targetCertificate, previousCertificate certificates.Material, dnsChanged bool) {
	rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
	defer cancel()
	_, _ = s.remnawave.UpdateNodeProfile(rollbackCtx, remnawave.UpdateNodeProfileInput{UUID: nodeUUID, Host: previousHost})
	previousProfile, err := remnawave.DeploymentProfileFromHost(previousHost)
	if err != nil {
		return
	}
	if dnsChanged {
		_, _ = s.dns.MoveIP(rollbackCtx, targetSNI, previousProfile.SNIDomain, address)
	}
	_ = s.config.NodeSNISwitcher.SwitchNodeSNI(rollbackCtx, NodeSNISwitchInput{
		Address: address, PreviousSNI: targetSNI, TargetSNI: previousProfile.SNIDomain,
		Password: password, PreviousCertificate: targetCertificate, Certificate: previousCertificate,
	})
}

func (s *DeploymentService) nodeByUUID(ctx context.Context, uuid string) (remnawave.Node, error) {
	nodes, err := s.remnawave.GetNodes(ctx)
	if err != nil {
		return remnawave.Node{}, ErrNodeHostMoveFailed
	}
	for _, node := range nodes {
		if node.UUID == uuid {
			return node, nil
		}
	}
	return remnawave.Node{}, ErrNodeNotFound
}

func matchingNodeHosts(node remnawave.Node, hosts []remnawave.Host) []remnawave.Host {
	profileUUID, inboundUUIDs := nodeProfileFingerprint(node)
	result := make([]remnawave.Host, 0, 1)
	for _, host := range hosts {
		if host.IsDisabled {
			continue
		}
		profile, err := remnawave.DeploymentProfileFromHost(host)
		if err == nil && sameNodeProfile(profileUUID, inboundUUIDs, profile.ActiveConfigProfileUUID, profile.ActiveInbounds) {
			result = append(result, host)
		}
	}
	return result
}

func nodeProfileFingerprint(node remnawave.Node) (string, []string) {
	profileUUID := ""
	if node.ActiveConfigProfileUUID != nil {
		profileUUID = strings.TrimSpace(*node.ActiveConfigProfileUUID)
	}
	return profileUUID, canonicalUUIDs(node.ActiveInboundUUIDs)
}

func canonicalUUIDs(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			result = append(result, value)
		}
	}
	sort.Strings(result)
	return result
}

func sameNodeProfile(leftProfile string, leftInbounds []string, rightProfile string, rightInbounds []string) bool {
	if strings.TrimSpace(leftProfile) != strings.TrimSpace(rightProfile) {
		return false
	}
	left, right := canonicalUUIDs(leftInbounds), canonicalUUIDs(rightInbounds)
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
