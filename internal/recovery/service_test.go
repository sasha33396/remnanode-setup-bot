package recovery

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"remnanode-setup-bot/internal/deployment"
	"remnanode-setup-bot/internal/dnsbalancer"
	"remnanode-setup-bot/internal/remnawave"
	"remnanode-setup-bot/internal/repository"
)

func TestRecoveryFindsExistingNodeAndCompletedDNS(t *testing.T) {
	item := recoveryDeployment(deployment.StatusCreatingRemnawave)
	repo := newRecoveryRepository(item)
	remna := &recoveryRemnawave{nodes: []remnawave.Node{{UUID: "00000000-0000-4000-8000-000000000099", Name: item.NodeName, Address: item.TargetVPSIP.String(), IsConnected: true}}}
	dns := &recoveryDNS{zone: dnsbalancer.ZoneMatch{Zone: dnsbalancer.Zone{IPs: []string{item.TargetVPSIP.String()}}}}
	service, _ := New(repo, remna, dns)
	results, err := service.RecoverUnfinished(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Classification != RecoveredCompleted {
		t.Fatalf("results = %#v", results)
	}
	recovered, _ := repo.GetDeployment(context.Background(), item.ID)
	if recovered.RemnawaveNodeUUID == nil || recovered.Status != deployment.StatusCompleted {
		t.Fatalf("recovered = %#v", recovered)
	}
}

func TestRecoveryDoesNotRepeatUnknownProvisioningSideEffect(t *testing.T) {
	item := recoveryDeployment(deployment.StatusProvisioning)
	repo := newRecoveryRepository(item)
	remna := &recoveryRemnawave{}
	service, _ := New(repo, remna, &recoveryDNS{})
	results, err := service.RecoverUnfinished(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Classification != NeedsManualReview || remna.calls != 0 {
		t.Fatalf("results=%#v Remnawave calls=%d", results, remna.calls)
	}
	updated, _ := repo.GetDeployment(context.Background(), item.ID)
	if updated.Status != deployment.StatusManualReview {
		t.Fatalf("status = %s", updated.Status)
	}
}

func TestRecoveryExposesExplicitDNSRetry(t *testing.T) {
	item := recoveryDeployment(deployment.StatusAddingToDNS)
	uuid := "00000000-0000-4000-8000-000000000099"
	item.RemnawaveNodeUUID = &uuid
	repo := newRecoveryRepository(item)
	service, _ := New(repo, &recoveryRemnawave{node: remnawave.Node{UUID: uuid, IsConnected: true}}, &recoveryDNS{zone: dnsbalancer.ZoneMatch{Zone: dnsbalancer.Zone{IPs: []string{}}}})
	result, err := service.RecheckRemnawave(context.Background(), item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Classification != SafeToRetryDNS {
		t.Fatalf("result = %#v", result)
	}
	updated, _ := repo.GetDeployment(context.Background(), item.ID)
	if updated.Status != deployment.StatusDNSFailed {
		t.Fatalf("status = %s", updated.Status)
	}
}

func recoveryDeployment(status deployment.Status) deployment.Deployment {
	return deployment.Deployment{ID: "00000000-0000-4000-8000-000000000001", TelegramOperatorUserID: 1, SelectedRemnawaveHostUUID: "00000000-0000-4000-8000-000000000002", SNIDomain: "edge.example.com", NodeName: "node-one", TargetVPSIP: netip.MustParseAddr("203.0.113.10"), Status: status, CurrentStep: "step", CreatedAt: time.Now(), UpdatedAt: time.Now()}
}

type recoveryRepository struct{ item deployment.Deployment }

func newRecoveryRepository(item deployment.Deployment) *recoveryRepository {
	return &recoveryRepository{item: item}
}
func (r *recoveryRepository) CreateDeployment(context.Context, repository.CreateDeploymentParams) (deployment.Deployment, error) {
	return deployment.Deployment{}, nil
}
func (r *recoveryRepository) GetDeployment(_ context.Context, _ string) (deployment.Deployment, error) {
	return r.item, nil
}
func (r *recoveryRepository) UpdateDeploymentState(_ context.Context, _ string, params repository.UpdateDeploymentStateParams) (deployment.Deployment, error) {
	r.item.Status, r.item.CurrentStep, r.item.SafeErrorCode, r.item.SafeErrorMessage = params.Status, params.CurrentStep, params.SafeErrorCode, params.SafeErrorMessage
	return r.item, nil
}
func (r *recoveryRepository) SetRemnawaveNodeUUID(_ context.Context, _ string, uuid string) (deployment.Deployment, error) {
	r.item.RemnawaveNodeUUID = &uuid
	return r.item, nil
}
func (*recoveryRepository) RecordDeploymentStep(context.Context, repository.RecordStepParams) (deployment.Step, error) {
	return deployment.Step{}, nil
}
func (*recoveryRepository) ListDeploymentSteps(context.Context, string) ([]deployment.Step, error) {
	return nil, nil
}
func (r *recoveryRepository) ListRecentDeployments(context.Context, int) ([]deployment.Deployment, error) {
	return []deployment.Deployment{r.item}, nil
}
func (r *recoveryRepository) FindUnfinishedDeployments(context.Context, int) ([]deployment.Deployment, error) {
	if r.item.Status.Terminal() {
		return nil, nil
	}
	return []deployment.Deployment{r.item}, nil
}

type recoveryRemnawave struct {
	nodes []remnawave.Node
	node  remnawave.Node
	calls int
}

func (r *recoveryRemnawave) GetNodes(context.Context) ([]remnawave.Node, error) {
	r.calls++
	return r.nodes, nil
}
func (r *recoveryRemnawave) GetNode(context.Context, string) (remnawave.Node, error) {
	r.calls++
	if r.node.UUID != "" {
		return r.node, nil
	}
	return r.nodes[0], nil
}

type recoveryDNS struct{ zone dnsbalancer.ZoneMatch }

func (d *recoveryDNS) FindZone(context.Context, string) (dnsbalancer.ZoneMatch, error) {
	return d.zone, nil
}

var _ Repository = (*recoveryRepository)(nil)
