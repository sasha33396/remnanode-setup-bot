package certmanager

import (
	"context"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"

	"remnanode-setup-bot/internal/certificates"
	"remnanode-setup-bot/internal/deployment"
	"remnanode-setup-bot/internal/dnsbalancer"
	"remnanode-setup-bot/internal/provisioner"
	sshclient "remnanode-setup-bot/internal/ssh"
)

type DNSConfigAPI interface {
	FindZone(context.Context, string) (dnsbalancer.ZoneMatch, error)
}

type DeploymentLookup interface {
	FindDeploymentsBySNI(context.Context, string, int) ([]deployment.Deployment, error)
	ListTargetReviews(context.Context, string) ([]TargetReview, error)
}

type PanelDeploymentLookup interface {
	FindDeploymentsByPanelSNI(context.Context, string, string, int) ([]deployment.Deployment, error)
	ListTargetReviewsForPanel(context.Context, string, string) ([]TargetReview, error)
}

type DNSDeploymentResolver struct {
	dns         DNSConfigAPI
	deployments DeploymentLookup
	panelID     string
	panelLookup PanelDeploymentLookup
}

func NewPanelDNSDeploymentResolver(panelID string, dns DNSConfigAPI, deployments PanelDeploymentLookup) (*DNSDeploymentResolver, error) {
	panelID = strings.TrimSpace(panelID)
	if panelID == "" || dns == nil || deployments == nil {
		return nil, ErrInvalidInput
	}
	return &DNSDeploymentResolver{dns: dns, panelID: panelID, panelLookup: deployments}, nil
}

func NewDNSDeploymentResolver(dns DNSConfigAPI, deployments DeploymentLookup) (*DNSDeploymentResolver, error) {
	if dns == nil || deployments == nil {
		return nil, ErrInvalidInput
	}
	return &DNSDeploymentResolver{dns: dns, deployments: deployments}, nil
}

// Resolve uses the DNS-balancer configuration as the authority for which Node
// IPs belong to an SNI, then separates verified deployment SSH identities,
// explicitly acknowledged legacy IPs, and unknown IPs requiring manual review.
func (r *DNSDeploymentResolver) Resolve(ctx context.Context, sni string) (TargetResolution, error) {
	match, err := r.dns.FindZone(ctx, sni)
	if err != nil {
		return TargetResolution{}, ErrDistributionFailed
	}
	ipSet := make(map[netip.Addr]struct{})
	for _, value := range match.Zone.IPs {
		if ip, err := netip.ParseAddr(strings.TrimSpace(value)); err == nil {
			ipSet[ip.Unmap()] = struct{}{}
		} else {
			return TargetResolution{}, ErrDistributionFailed
		}
	}
	for _, node := range match.Zone.Nodes {
		if ip, err := netip.ParseAddr(strings.TrimSpace(node.IP)); err == nil {
			ipSet[ip.Unmap()] = struct{}{}
		} else {
			return TargetResolution{}, ErrDistributionFailed
		}
	}
	if len(ipSet) == 0 {
		return TargetResolution{}, nil
	}
	var items []deployment.Deployment
	if r.panelLookup != nil {
		items, err = r.panelLookup.FindDeploymentsByPanelSNI(ctx, r.panelID, sni, 100)
	} else {
		items, err = r.deployments.FindDeploymentsBySNI(ctx, sni, 100)
	}
	if err != nil {
		return TargetResolution{}, ErrDistributionFailed
	}
	var reviews []TargetReview
	if r.panelLookup != nil {
		reviews, err = r.panelLookup.ListTargetReviewsForPanel(ctx, r.panelID, sni)
	} else {
		reviews, err = r.deployments.ListTargetReviews(ctx, sni)
	}
	if err != nil {
		return TargetResolution{}, ErrDistributionFailed
	}
	reviewByIP := make(map[netip.Addr]TargetReviewState, len(reviews))
	for _, review := range reviews {
		if review.IP.IsValid() {
			reviewByIP[review.IP.Unmap()] = review.State
		}
	}
	byIP := make(map[netip.Addr]deployment.Deployment)
	for _, item := range items {
		if strings.EqualFold(strings.TrimSuffix(strings.TrimSpace(item.SNIDomain), "."), sni) && item.RemnawaveNodeUUID != nil && item.SSHHostKeyFingerprint != nil && item.Status != deployment.StatusCancelled {
			ip := item.TargetVPSIP.Unmap()
			if _, exists := byIP[ip]; !exists {
				byIP[ip] = item
			}
		}
	}
	resolution := TargetResolution{Managed: make([]Target, 0, len(ipSet))}
	for ip := range ipSet {
		item, found := byIP[ip]
		if found {
			resolution.Managed = append(resolution.Managed, Target{DeploymentID: item.ID, IP: ip})
			continue
		}
		if reviewByIP[ip] == TargetLegacyAcknowledged {
			resolution.LegacyAcknowledged = append(resolution.LegacyAcknowledged, ip)
			continue
		}
		resolution.Unmanaged = append(resolution.Unmanaged, ip)
	}
	sort.Slice(resolution.Managed, func(left, right int) bool {
		return resolution.Managed[left].IP.Compare(resolution.Managed[right].IP) < 0
	})
	sort.Slice(resolution.Unmanaged, func(left, right int) bool { return resolution.Unmanaged[left].Compare(resolution.Unmanaged[right]) < 0 })
	sort.Slice(resolution.LegacyAcknowledged, func(left, right int) bool {
		return resolution.LegacyAcknowledged[left].Compare(resolution.LegacyAcknowledged[right]) < 0
	})
	return resolution, nil
}

type SSHDistributorConfig struct {
	RepositoryURL  string
	Ref            string
	CommandTimeout time.Duration
	MaxConcurrent  int
}

type SSHDistributor struct {
	ssh    *sshclient.Client
	config SSHDistributorConfig
}

func NewSSHDistributor(ssh *sshclient.Client, config SSHDistributorConfig) (*SSHDistributor, error) {
	if ssh == nil || strings.TrimSpace(config.RepositoryURL) == "" || strings.TrimSpace(config.Ref) == "" {
		return nil, ErrInvalidInput
	}
	if config.CommandTimeout <= 0 {
		config.CommandTimeout = 5 * time.Minute
	}
	if config.MaxConcurrent <= 0 {
		config.MaxConcurrent = 4
	}
	if config.MaxConcurrent > 32 {
		return nil, ErrInvalidInput
	}
	return &SSHDistributor{ssh: ssh, config: config}, nil
}

func (d *SSHDistributor) Distribute(ctx context.Context, sni string, material certificates.Material, targets []Target) []DistributionResult {
	results := make([]DistributionResult, len(targets))
	semaphore := make(chan struct{}, d.config.MaxConcurrent)
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
				results[index] = failedDistribution(target, "Certificate distribution cancelled")
				return
			}
			copy := material.Clone()
			defer copy.Destroy()
			connection, err := d.ssh.ConnectWithDeploymentKey(ctx, target.DeploymentID, target.IP, "root")
			if err != nil {
				results[index] = failedDistribution(target, "Could not connect to Node")
				return
			}
			defer connection.Close()
			installer, err := provisioner.NewExternalXraySNIInstaller(connection, provisioner.ExternalXraySNIConfig{RepositoryURL: d.config.RepositoryURL, Ref: d.config.Ref, SNIDomain: sni, Timeout: d.config.CommandTimeout}, copy)
			if err != nil {
				results[index] = failedDistribution(target, "Certificate was rejected before distribution")
				return
			}
			defer installer.Destroy()
			if err := installer.UpdateCertificate(ctx); err != nil {
				results[index] = failedDistribution(target, "Node certificate activation failed and was rolled back")
				return
			}
			results[index] = DistributionResult{Target: target, Status: DistributionSucceeded, SafeMessage: "Certificate activated"}
		}()
	}
	workers.Wait()
	return results
}

func failedDistribution(target Target, message string) DistributionResult {
	return DistributionResult{Target: target, Status: DistributionFailed, SafeMessage: message}
}

var (
	_ TargetResolver = (*DNSDeploymentResolver)(nil)
	_ Distributor    = (*SSHDistributor)(nil)
)
