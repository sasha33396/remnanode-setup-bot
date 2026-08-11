package certmanager

import (
	"context"
	"net/netip"
	"testing"
	"time"
)

func TestScopedRepositoryNeverDropsPanelContext(t *testing.T) {
	backend := &recordingPanelRepository{}
	europe, err := NewScopedRepository("europe", backend)
	if err != nil {
		t.Fatal(err)
	}
	test, err := NewScopedRepository("test", backend)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = europe.GetActive(context.Background(), "same.example.com")
	_ = test.RecordDistribution(context.Background(), DistributionRecord{SNI: "same.example.com", DeploymentID: "deployment", NodeIP: netip.MustParseAddr("1.1.1.1")})
	if len(backend.panels) != 2 || backend.panels[0] != "europe" || backend.panels[1] != "test" {
		t.Fatalf("panel calls = %v", backend.panels)
	}
}

type recordingPanelRepository struct{ panels []string }

func (r *recordingPanelRepository) add(panel string) { r.panels = append(r.panels, panel) }
func (r *recordingPanelRepository) GetActiveForPanel(_ context.Context, panel, _ string) (Record, error) {
	r.add(panel)
	return Record{}, ErrNotFound
}
func (r *recordingPanelRepository) SaveVersionForPanel(_ context.Context, panel string, _ Version) error {
	r.add(panel)
	return nil
}
func (r *recordingPanelRepository) SetVersionStatusForPanel(_ context.Context, panel, _, _ string, _ VersionStatus) error {
	r.add(panel)
	return nil
}
func (r *recordingPanelRepository) ActivateVersionForPanel(_ context.Context, panel, _, _ string, _ bool) error {
	r.add(panel)
	return nil
}
func (r *recordingPanelRepository) SetStatusForPanel(_ context.Context, panel, _ string, _ Status) error {
	r.add(panel)
	return nil
}
func (r *recordingPanelRepository) RecordDistributionForPanel(_ context.Context, panel string, _ DistributionRecord) error {
	r.add(panel)
	return nil
}
func (r *recordingPanelRepository) ListExpiringForPanel(_ context.Context, panel string, _ time.Time, _ int) ([]Record, error) {
	r.add(panel)
	return nil, nil
}
func (r *recordingPanelRepository) ListVersionsForPanel(_ context.Context, panel, _ string) ([]Version, error) {
	r.add(panel)
	return nil, nil
}
func (r *recordingPanelRepository) RecordTargetReviewForPanel(_ context.Context, panel string, _ TargetReview) error {
	r.add(panel)
	return nil
}
func (r *recordingPanelRepository) ListTargetReviewsForPanel(_ context.Context, panel, _ string) ([]TargetReview, error) {
	r.add(panel)
	return nil, nil
}

var _ PanelRepository = (*recordingPanelRepository)(nil)
