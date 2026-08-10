package certmanager

import (
	"context"
	"net/netip"
	"testing"

	"remnanode-setup-bot/internal/deployment"
	"remnanode-setup-bot/internal/dnsbalancer"
)

func TestDNSDeploymentResolverSeparatesManagedLegacyAndUnmanagedTargets(t *testing.T) {
	managedIP := netip.MustParseAddr("203.0.113.10")
	legacyIP := netip.MustParseAddr("203.0.113.11")
	unknownIP := netip.MustParseAddr("203.0.113.12")
	nodeUUID, fingerprint := "00000000-0000-4000-8000-000000000010", "SHA256:managed"
	dns := &resolverDNS{match: dnsbalancer.ZoneMatch{FQDN: testSNI, Zone: dnsbalancer.Zone{IPs: []string{managedIP.String(), legacyIP.String(), unknownIP.String()}}}}
	lookup := &resolverLookup{
		deployments: []deployment.Deployment{{
			ID: "00000000-0000-4000-8000-000000000001", SNIDomain: testSNI,
			TargetVPSIP: managedIP, RemnawaveNodeUUID: &nodeUUID,
			SSHHostKeyFingerprint: &fingerprint, Status: deployment.StatusCompleted,
		}},
		reviews: []TargetReview{{SNI: testSNI, IP: legacyIP, State: TargetLegacyAcknowledged}},
	}
	resolver, err := NewDNSDeploymentResolver(dns, lookup)
	if err != nil {
		t.Fatal(err)
	}
	resolution, err := resolver.Resolve(context.Background(), testSNI)
	if err != nil {
		t.Fatal(err)
	}
	if len(resolution.Managed) != 1 || resolution.Managed[0].IP != managedIP {
		t.Fatalf("managed = %#v", resolution.Managed)
	}
	if len(resolution.LegacyAcknowledged) != 1 || resolution.LegacyAcknowledged[0] != legacyIP {
		t.Fatalf("legacy = %#v", resolution.LegacyAcknowledged)
	}
	if len(resolution.Unmanaged) != 1 || resolution.Unmanaged[0] != unknownIP {
		t.Fatalf("unmanaged = %#v", resolution.Unmanaged)
	}
}

type resolverDNS struct{ match dnsbalancer.ZoneMatch }

func (r *resolverDNS) FindZone(context.Context, string) (dnsbalancer.ZoneMatch, error) {
	return r.match, nil
}

type resolverLookup struct {
	deployments []deployment.Deployment
	reviews     []TargetReview
}

func (r *resolverLookup) FindDeploymentsBySNI(context.Context, string, int) ([]deployment.Deployment, error) {
	return append([]deployment.Deployment(nil), r.deployments...), nil
}

func (r *resolverLookup) ListTargetReviews(context.Context, string) ([]TargetReview, error) {
	return append([]TargetReview(nil), r.reviews...), nil
}
