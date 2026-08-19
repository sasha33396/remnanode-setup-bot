package certmanager

import (
	"context"
	"strings"
	"time"
)

type ScopedRepository struct {
	panelID string
	next    PanelRepository
}

func NewScopedRepository(panelID string, next PanelRepository) (*ScopedRepository, error) {
	panelID = strings.TrimSpace(panelID)
	if panelID == "" || next == nil {
		return nil, ErrInvalidInput
	}
	return &ScopedRepository{panelID: panelID, next: next}, nil
}

func (r *ScopedRepository) GetActive(ctx context.Context, sni string) (Record, error) {
	return r.next.GetActiveForPanel(ctx, r.panelID, sni)
}
func (r *ScopedRepository) SaveVersion(ctx context.Context, value Version) error {
	return r.next.SaveVersionForPanel(ctx, r.panelID, value)
}
func (r *ScopedRepository) SetVersionStatus(ctx context.Context, sni, version string, status VersionStatus) error {
	return r.next.SetVersionStatusForPanel(ctx, r.panelID, sni, version, status)
}
func (r *ScopedRepository) ActivateVersion(ctx context.Context, sni, version string, renewed bool) error {
	return r.next.ActivateVersionForPanel(ctx, r.panelID, sni, version, renewed)
}
func (r *ScopedRepository) SetStatus(ctx context.Context, sni string, status Status) error {
	return r.next.SetStatusForPanel(ctx, r.panelID, sni, status)
}
func (r *ScopedRepository) RecordDistribution(ctx context.Context, value DistributionRecord) error {
	return r.next.RecordDistributionForPanel(ctx, r.panelID, value)
}
func (r *ScopedRepository) ListExpiring(ctx context.Context, before time.Time, limit int) ([]Record, error) {
	return r.next.ListExpiringForPanel(ctx, r.panelID, before, limit)
}
func (r *ScopedRepository) ListVersions(ctx context.Context, sni string) ([]Version, error) {
	return r.next.ListVersionsForPanel(ctx, r.panelID, sni)
}
func (r *ScopedRepository) RecordTargetReview(ctx context.Context, value TargetReview) error {
	return r.next.RecordTargetReviewForPanel(ctx, r.panelID, value)
}
func (r *ScopedRepository) ListTargetReviews(ctx context.Context, sni string) ([]TargetReview, error) {
	return r.next.ListTargetReviewsForPanel(ctx, r.panelID, sni)
}

var _ Repository = (*ScopedRepository)(nil)
