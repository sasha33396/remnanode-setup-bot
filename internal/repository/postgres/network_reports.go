package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"remnanode-setup-bot/internal/networkinventory"
)

func (r *Repository) SaveNetworkReport(ctx context.Context, report networkinventory.Report) error {
	if !validPanelID(report.PanelID) || !validUUID(report.NodeUUID) {
		return errors.New("invalid network report identity")
	}
	data, err := json.Marshal(report)
	if err != nil {
		return err
	}
	if len(data) > 1024*1024 {
		return errors.New("network report too large")
	}
	_, err = r.pool.Exec(ctx, `INSERT INTO node_network_reports (panel_id,node_uuid,report) VALUES ($1,$2,$3::jsonb) ON CONFLICT (panel_id,node_uuid) DO UPDATE SET report=EXCLUDED.report,updated_at=now()`, report.PanelID, report.NodeUUID, data)
	return err
}

func (r *Repository) ListNetworkReports(ctx context.Context) ([]networkinventory.Report, error) {
	rows, err := r.pool.Query(ctx, `SELECT report FROM node_network_reports ORDER BY panel_id,node_uuid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []networkinventory.Report{}
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		var report networkinventory.Report
		if err := json.Unmarshal(data, &report); err != nil {
			return nil, err
		}
		result = append(result, report)
	}
	return result, rows.Err()
}
