package postgres

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"remnanode-setup-bot/internal/certmanager"
	"remnanode-setup-bot/internal/deployment"
)

func (r *Repository) GetActive(ctx context.Context, sni string) (certmanager.Record, error) {
	return r.GetActiveForPanel(ctx, "default", sni)
}

func (r *Repository) GetActiveForPanel(ctx context.Context, panelID, sni string) (certmanager.Record, error) {
	row := r.pool.QueryRow(ctx, `
        SELECT sni_domain, certificate_fingerprint, serial_number, issued_at,
               expires_at, last_renewed_at, status, active_version, updated_at
        FROM certificate_records
        WHERE panel_id = $1 AND sni_domain = lower(btrim($2)) AND active_version IS NOT NULL`, panelID, sni)
	record, err := scanCertificateRecord(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return certmanager.Record{}, certmanager.ErrNotFound
	}
	if err != nil {
		return certmanager.Record{}, fmt.Errorf("get active certificate metadata: %w", err)
	}
	return record, nil
}

func (r *Repository) SaveVersion(ctx context.Context, version certmanager.Version) error {
	return r.SaveVersionForPanel(ctx, "default", version)
}

func (r *Repository) SaveVersionForPanel(ctx context.Context, panelID string, version certmanager.Version) error {
	if strings.TrimSpace(version.SNI) == "" || strings.TrimSpace(version.Version) == "" || version.Fingerprint == "" || version.Serial == "" || version.IssuedAt.IsZero() || !version.ExpiresAt.After(version.IssuedAt) {
		return certmanager.ErrInvalidInput
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin certificate version transaction: %w", err)
	}
	defer rollback(tx)
	if _, err := tx.Exec(ctx, `
        INSERT INTO certificate_records (panel_id, sni_domain, status)
        VALUES ($1, lower(btrim($2)), 'ISSUING')
        ON CONFLICT (panel_id, sni_domain) DO UPDATE
        SET status = 'ISSUING', updated_at = now()`, panelID, version.SNI); err != nil {
		return fmt.Errorf("mark certificate issuing: %w", err)
	}
	if _, err := tx.Exec(ctx, `
        INSERT INTO certificate_versions (
            panel_id, sni_domain, version, certificate_fingerprint, serial_number,
            issued_at, expires_at, status, created_at
        ) VALUES ($1, lower(btrim($2)), $3, $4, $5, $6, $7, $8, $9)`,
		panelID, version.SNI, version.Version, version.Fingerprint, version.Serial,
		version.IssuedAt, version.ExpiresAt, version.Status, version.CreatedAt); err != nil {
		return fmt.Errorf("save certificate version metadata: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit certificate version metadata: %w", err)
	}
	return nil
}

func (r *Repository) SetVersionStatus(ctx context.Context, sni, version string, status certmanager.VersionStatus) error {
	return r.SetVersionStatusForPanel(ctx, "default", sni, version, status)
}

func (r *Repository) SetVersionStatusForPanel(ctx context.Context, panelID, sni, version string, status certmanager.VersionStatus) error {
	tag, err := r.pool.Exec(ctx, `
        UPDATE certificate_versions
        SET status = $4
        WHERE panel_id = $1 AND sni_domain = lower(btrim($2)) AND version = $3`, panelID, sni, version, status)
	if err != nil {
		return fmt.Errorf("set certificate version status: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return certmanager.ErrNotFound
	}
	return nil
}

func (r *Repository) ActivateVersion(ctx context.Context, sni, version string, renewed bool) error {
	return r.ActivateVersionForPanel(ctx, "default", sni, version, renewed)
}

func (r *Repository) ActivateVersionForPanel(ctx context.Context, panelID, sni, version string, renewed bool) error {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin certificate activation transaction: %w", err)
	}
	defer rollback(tx)
	var fingerprint, serial string
	var issuedAt, expiresAt time.Time
	if err := tx.QueryRow(ctx, `
        SELECT certificate_fingerprint, serial_number, issued_at, expires_at
        FROM certificate_versions
        WHERE panel_id = $1 AND sni_domain = lower(btrim($2)) AND version = $3
        FOR UPDATE`, panelID, sni, version).Scan(&fingerprint, &serial, &issuedAt, &expiresAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return certmanager.ErrNotFound
		}
		return fmt.Errorf("lock certificate version: %w", err)
	}
	if _, err := tx.Exec(ctx, `
        UPDATE certificate_versions
        SET status = CASE WHEN version = $3 THEN 'ACTIVE' ELSE 'SUPERSEDED' END
        WHERE panel_id = $1 AND sni_domain = lower(btrim($2))
          AND (status = 'ACTIVE' OR version = $3)`, panelID, sni, version); err != nil {
		return fmt.Errorf("activate certificate version: %w", err)
	}
	if _, err := tx.Exec(ctx, `
        UPDATE certificate_records
        SET certificate_fingerprint = $4,
            serial_number = $5,
            issued_at = $6,
            expires_at = $7,
            last_renewed_at = CASE WHEN $8 THEN now() ELSE last_renewed_at END,
            status = 'ACTIVE',
            active_version = $3,
            updated_at = now()
        WHERE panel_id = $1 AND sni_domain = lower(btrim($2))`, panelID, sni, version, fingerprint, serial, issuedAt, expiresAt, renewed); err != nil {
		return fmt.Errorf("activate certificate record: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit certificate activation: %w", err)
	}
	return nil
}

func (r *Repository) SetStatus(ctx context.Context, sni string, status certmanager.Status) error {
	return r.SetStatusForPanel(ctx, "default", sni, status)
}

func (r *Repository) SetStatusForPanel(ctx context.Context, panelID, sni string, status certmanager.Status) error {
	_, err := r.pool.Exec(ctx, `
        INSERT INTO certificate_records (panel_id, sni_domain, status)
        VALUES ($1, lower(btrim($2)), $3)
        ON CONFLICT (panel_id, sni_domain) DO UPDATE
        SET status = EXCLUDED.status, updated_at = now()`, panelID, sni, status)
	if err != nil {
		return fmt.Errorf("set certificate status: %w", err)
	}
	return nil
}

func (r *Repository) RecordDistribution(ctx context.Context, record certmanager.DistributionRecord) error {
	return r.RecordDistributionForPanel(ctx, "default", record)
}

func (r *Repository) RecordDistributionForPanel(ctx context.Context, panelID string, record certmanager.DistributionRecord) error {
	if !record.NodeIP.IsValid() || strings.TrimSpace(record.DeploymentID) == "" {
		return certmanager.ErrInvalidInput
	}
	var safeMessage *string
	if record.SafeErrorMessage != "" {
		safeMessage = &record.SafeErrorMessage
	}
	_, err := r.pool.Exec(ctx, `
        INSERT INTO certificate_distributions (
            panel_id, sni_domain, version, deployment_id, node_ip, status,
            safe_error_message, attempted_at
        ) VALUES ($1, lower(btrim($2)), $3, $4, $5, $6, $7, $8)
        ON CONFLICT (panel_id, sni_domain, version, deployment_id) DO UPDATE
        SET node_ip = EXCLUDED.node_ip,
            status = EXCLUDED.status,
            safe_error_message = EXCLUDED.safe_error_message,
            attempted_at = EXCLUDED.attempted_at`,
		panelID, record.SNI, record.Version, record.DeploymentID, record.NodeIP.String(),
		record.Status, safeMessage, record.AttemptedAt)
	if err != nil {
		return fmt.Errorf("record certificate distribution: %w", err)
	}
	return nil
}

func (r *Repository) RecordTargetReview(ctx context.Context, review certmanager.TargetReview) error {
	return r.RecordTargetReviewForPanel(ctx, "default", review)
}

func (r *Repository) RecordTargetReviewForPanel(ctx context.Context, panelID string, review certmanager.TargetReview) error {
	if strings.TrimSpace(review.SNI) == "" || !review.IP.IsValid() || strings.TrimSpace(review.Reason) == "" {
		return certmanager.ErrInvalidInput
	}
	if review.State != certmanager.TargetManualReview && review.State != certmanager.TargetLegacyAcknowledged {
		return certmanager.ErrInvalidInput
	}
	if review.State == certmanager.TargetLegacyAcknowledged && (review.AcknowledgedBy == nil || *review.AcknowledgedBy <= 0) {
		return certmanager.ErrInvalidInput
	}
	if review.State == certmanager.TargetManualReview {
		review.AcknowledgedBy = nil
	}
	_, err := r.pool.Exec(ctx, `
        INSERT INTO certificate_target_reviews (
            panel_id, sni_domain, node_ip, state, reason, acknowledged_by,
            acknowledged_at
        ) VALUES (
            $1, lower(btrim($2)), $3, $4, $5, $6,
            CASE WHEN $4 = 'LEGACY_ACKNOWLEDGED' THEN now() ELSE NULL END
        )
        ON CONFLICT (panel_id, sni_domain, node_ip) DO UPDATE
        SET state = EXCLUDED.state,
            reason = EXCLUDED.reason,
            acknowledged_by = EXCLUDED.acknowledged_by,
            acknowledged_at = EXCLUDED.acknowledged_at,
            updated_at = now()`,
		panelID, review.SNI, review.IP.Unmap().String(), review.State, strings.TrimSpace(review.Reason), review.AcknowledgedBy)
	if err != nil {
		return fmt.Errorf("record certificate target review: %w", err)
	}
	return nil
}

func (r *Repository) ListTargetReviews(ctx context.Context, sni string) ([]certmanager.TargetReview, error) {
	return r.ListTargetReviewsForPanel(ctx, "default", sni)
}

func (r *Repository) ListTargetReviewsForPanel(ctx context.Context, panelID, sni string) ([]certmanager.TargetReview, error) {
	rows, err := r.pool.Query(ctx, `
        SELECT sni_domain, node_ip::text, state, reason, acknowledged_by,
               created_at, updated_at, acknowledged_at
        FROM certificate_target_reviews
        WHERE panel_id = $1 AND sni_domain = lower(btrim($2))
        ORDER BY node_ip, created_at`, panelID, sni)
	if err != nil {
		return nil, fmt.Errorf("list certificate target reviews: %w", err)
	}
	defer rows.Close()
	result := make([]certmanager.TargetReview, 0)
	for rows.Next() {
		var item certmanager.TargetReview
		var ip string
		if err := rows.Scan(&item.SNI, &ip, &item.State, &item.Reason, &item.AcknowledgedBy, &item.CreatedAt, &item.UpdatedAt, &item.AcknowledgedAt); err != nil {
			return nil, fmt.Errorf("scan certificate target review: %w", err)
		}
		item.IP, err = netip.ParseAddr(ip)
		if err != nil {
			return nil, fmt.Errorf("parse certificate target review IP: %w", err)
		}
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list certificate target reviews: %w", err)
	}
	return result, nil
}

func (r *Repository) ListExpiring(ctx context.Context, before time.Time, limit int) ([]certmanager.Record, error) {
	return r.ListExpiringForPanel(ctx, "default", before, limit)
}

func (r *Repository) ListExpiringForPanel(ctx context.Context, panelID string, before time.Time, limit int) ([]certmanager.Record, error) {
	if limit < 1 || limit > 100 {
		return nil, certmanager.ErrInvalidInput
	}
	rows, err := r.pool.Query(ctx, `
        SELECT sni_domain, certificate_fingerprint, serial_number, issued_at,
               expires_at, last_renewed_at, status, active_version, updated_at
        FROM certificate_records
        WHERE panel_id = $1 AND active_version IS NOT NULL AND expires_at <= $2
        ORDER BY expires_at ASC, sni_domain ASC
        LIMIT $3`, panelID, before, limit)
	if err != nil {
		return nil, fmt.Errorf("list expiring certificates: %w", err)
	}
	defer rows.Close()
	result := make([]certmanager.Record, 0)
	for rows.Next() {
		record, err := scanCertificateRecord(rows)
		if err != nil {
			return nil, fmt.Errorf("scan expiring certificate: %w", err)
		}
		result = append(result, record)
	}
	return result, rows.Err()
}

func (r *Repository) ListVersions(ctx context.Context, sni string) ([]certmanager.Version, error) {
	return r.ListVersionsForPanel(ctx, "default", sni)
}

func (r *Repository) ListVersionsForPanel(ctx context.Context, panelID, sni string) ([]certmanager.Version, error) {
	rows, err := r.pool.Query(ctx, `
        SELECT sni_domain, version, certificate_fingerprint, serial_number,
               issued_at, expires_at, status, created_at
        FROM certificate_versions
        WHERE panel_id = $1 AND sni_domain = lower(btrim($2))
        ORDER BY created_at DESC, version DESC`, panelID, sni)
	if err != nil {
		return nil, fmt.Errorf("list certificate versions: %w", err)
	}
	defer rows.Close()
	result := make([]certmanager.Version, 0)
	for rows.Next() {
		var item certmanager.Version
		if err := rows.Scan(&item.SNI, &item.Version, &item.Fingerprint, &item.Serial, &item.IssuedAt, &item.ExpiresAt, &item.Status, &item.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan certificate version: %w", err)
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (r *Repository) FindDeploymentsBySNI(ctx context.Context, sni string, limit int) ([]deployment.Deployment, error) {
	return r.FindDeploymentsByPanelSNI(ctx, "default", sni, limit)
}

func (r *Repository) FindDeploymentsByPanelSNI(ctx context.Context, panelID, sni string, limit int) ([]deployment.Deployment, error) {
	return r.list(ctx, `
        SELECT `+deploymentColumns+`
        FROM deployments
        WHERE panel_id = $2 AND lower(sni_domain) = lower(btrim($3))
        ORDER BY updated_at DESC, id DESC
        LIMIT $1`, limit, "find deployments by panel and SNI", panelID, sni)
}

func scanCertificateRecord(row scanner) (certmanager.Record, error) {
	var record certmanager.Record
	err := row.Scan(&record.SNI, &record.Fingerprint, &record.Serial, &record.IssuedAt, &record.ExpiresAt, &record.LastRenewedAt, &record.Status, &record.ActiveVersion, &record.UpdatedAt)
	return record, err
}

func rollback(tx pgx.Tx) {
	rollbackCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = tx.Rollback(rollbackCtx)
}

var _ certmanager.Repository = (*Repository)(nil)
var _ certmanager.PanelRepository = (*Repository)(nil)
