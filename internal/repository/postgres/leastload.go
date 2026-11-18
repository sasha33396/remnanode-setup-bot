package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"remnanode-setup-bot/internal/leastload"
)

func (r *Repository) HasPendingLeastload(ctx context.Context, deploymentID, exceptJob string) (bool, error) {
	var found bool
	err := r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM leastload_ip_jobs j, jsonb_array_elements(j.job->'items') item WHERE j.id <> $1::uuid AND item->>'deploymentId'=$2 AND item->>'status' IN ('network_pending','network_done'))`, exceptJob, deploymentID).Scan(&found)
	return found, err
}

func (r *Repository) SaveLeastloadJob(ctx context.Context, job leastload.Job) error {
	if !validUUID(job.ID) || !validPanelID(job.PanelID) || job.Operator <= 0 || len(job.Items) > 50 {
		return errors.New("invalid leastload job")
	}
	data, err := json.Marshal(job)
	if err != nil {
		return err
	}
	_, err = r.pool.Exec(ctx, `INSERT INTO leastload_ip_jobs(id,job) VALUES($1,$2::jsonb) ON CONFLICT(id) DO UPDATE SET job=EXCLUDED.job,updated_at=now()`, job.ID, data)
	return err
}
func (r *Repository) GetLeastloadJob(ctx context.Context, id string) (leastload.Job, error) {
	var job leastload.Job
	var data []byte
	if !validUUID(id) {
		return job, errors.New("invalid job id")
	}
	if err := r.pool.QueryRow(ctx, `SELECT job FROM leastload_ip_jobs WHERE id=$1`, id).Scan(&data); err != nil {
		return job, err
	}
	err := json.Unmarshal(data, &job)
	return job, err
}
func (r *Repository) ListLeastloadJobs(ctx context.Context) ([]leastload.Job, error) {
	rows, err := r.pool.Query(ctx, `SELECT job FROM leastload_ip_jobs WHERE id IN (SELECT id FROM leastload_ip_jobs ORDER BY updated_at DESC LIMIT 50) OR EXISTS (SELECT 1 FROM jsonb_array_elements(job->'items') item WHERE item->>'status' IN ('network_pending','network_done')) ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []leastload.Job{}
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		var job leastload.Job
		if err := json.Unmarshal(data, &job); err != nil {
			return nil, err
		}
		result = append(result, job)
	}
	return result, rows.Err()
}
