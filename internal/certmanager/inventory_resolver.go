package certmanager

import (
	"context"
	"sort"
	"strings"

	"remnanode-setup-bot/internal/deployment"
)

// InventoryResolver distributes only to Nodes with a persisted deployment
// identity in one panel. Legacy Nodes stay excluded until a safe import flow
// assigns their SSH identity and certificate scope.
type InventoryResolver struct {
	panelID string
	lookup  PanelDeploymentLookup
}

func NewInventoryResolver(panelID string, lookup PanelDeploymentLookup) (*InventoryResolver, error) {
	panelID = strings.TrimSpace(panelID)
	if panelID == "" || lookup == nil {
		return nil, ErrInvalidInput
	}
	return &InventoryResolver{panelID: panelID, lookup: lookup}, nil
}

func (r *InventoryResolver) Resolve(ctx context.Context, sni string) (TargetResolution, error) {
	items, err := r.lookup.FindDeploymentsByPanelSNI(ctx, r.panelID, sni, 100)
	if err != nil {
		return TargetResolution{}, ErrDistributionFailed
	}
	result := TargetResolution{}
	seen := make(map[string]struct{})
	for _, item := range items {
		if item.PanelID != r.panelID || item.RemnawaveNodeUUID == nil || item.SSHHostKeyFingerprint == nil || item.Status == deployment.StatusCancelled {
			continue
		}
		key := item.TargetVPSIP.Unmap().String()
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result.Managed = append(result.Managed, Target{DeploymentID: item.ID, IP: item.TargetVPSIP.Unmap()})
	}
	sort.Slice(result.Managed, func(i, j int) bool { return result.Managed[i].IP.Compare(result.Managed[j].IP) < 0 })
	return result, nil
}

var _ TargetResolver = (*InventoryResolver)(nil)
