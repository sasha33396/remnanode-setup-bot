// Package postgres implements deployment persistence in PostgreSQL.
package postgres

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"remnanode-setup-bot/internal/deployment"
	repositorycontract "remnanode-setup-bot/internal/repository"
)

const deploymentColumns = `
    id::text,
    panel_id,
    telegram_operator_user_id,
    selected_remnawave_host_uuid::text,
    selected_host_remark,
    sni_domain,
    node_name,
    host(target_vps_ip),
    remnawave_node_uuid::text,
    ssh_host_key_fingerprint,
    status,
    current_step,
    safe_error_code,
    safe_error_message,
    created_at,
    updated_at,
    started_at,
    completed_at`

// Repository persists deployment jobs using a pgx connection pool.
type Repository struct {
	pool *pgxpool.Pool
}

var _ repositorycontract.DeploymentRepository = (*Repository)(nil)

// Open creates and verifies a PostgreSQL connection pool. The caller controls
// connection timeout through ctx and must close the returned pool.
func Open(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, errors.New("create PostgreSQL pool: invalid database configuration")
	}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, errors.New("create PostgreSQL pool: initialization failed")
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, errors.New("ping PostgreSQL: connection failed")
	}
	return pool, nil
}

// New creates a PostgreSQL deployment repository.
func New(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

// CheckSchema verifies all runtime-critical migration objects before the
// service reports readiness.
func (r *Repository) CheckSchema(ctx context.Context) error {
	var deployments, certificateRecords, certificateVersions, certificateTargetReviews *string
	var panelColumns int
	if err := r.pool.QueryRow(ctx, `
        SELECT to_regclass('public.deployments')::text,
               to_regclass('public.certificate_records')::text,
               to_regclass('public.certificate_versions')::text,
               to_regclass('public.certificate_target_reviews')::text,
               (SELECT count(*) FROM information_schema.columns
                WHERE table_schema = 'public' AND column_name = 'panel_id'
                  AND table_name IN ('deployments', 'certificate_records', 'certificate_versions', 'certificate_distributions', 'certificate_target_reviews'))`).Scan(&deployments, &certificateRecords, &certificateVersions, &certificateTargetReviews, &panelColumns); err != nil {
		return errors.New("verify PostgreSQL schema failed")
	}
	if deployments == nil || certificateRecords == nil || certificateVersions == nil || certificateTargetReviews == nil || panelColumns != 5 {
		return errors.New("required PostgreSQL migrations are not applied")
	}
	return nil
}

// CreateDeployment inserts a deployment in CREATED state.
func (r *Repository) CreateDeployment(ctx context.Context, params repositorycontract.CreateDeploymentParams) (deployment.Deployment, error) {
	if strings.TrimSpace(params.PanelID) == "" {
		params.PanelID = "default"
	}
	if params.ID == "" {
		var err error
		params.ID, err = deployment.NewID()
		if err != nil {
			return deployment.Deployment{}, err
		}
	}
	if err := validateCreate(params); err != nil {
		return deployment.Deployment{}, err
	}

	row := r.pool.QueryRow(ctx, `
        INSERT INTO deployments (
            id, panel_id, telegram_operator_user_id, selected_remnawave_host_uuid,
            selected_host_remark, sni_domain, node_name, target_vps_ip,
            status, current_step
        ) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
        RETURNING `+deploymentColumns,
		params.ID,
		params.PanelID,
		params.TelegramOperatorUserID,
		params.SelectedRemnawaveHostUUID,
		params.SelectedHostRemark,
		params.SNIDomain,
		params.NodeName,
		params.TargetVPSIP.String(),
		deployment.StatusCreated,
		"created",
	)
	result, err := scanDeployment(row)
	if err != nil {
		return deployment.Deployment{}, fmt.Errorf("create deployment: %w", err)
	}
	return result, nil
}

// GetDeployment returns a deployment by UUID.
func (r *Repository) GetDeployment(ctx context.Context, id string) (deployment.Deployment, error) {
	if !validUUID(id) {
		return deployment.Deployment{}, invalid("deployment ID")
	}
	row := r.pool.QueryRow(ctx, `SELECT `+deploymentColumns+` FROM deployments WHERE id = $1`, id)
	result, err := scanDeployment(row)
	if err != nil {
		return deployment.Deployment{}, mapNotFound("get deployment", err)
	}
	return result, nil
}

// UpdateDeploymentState atomically updates state, safe error details, and job
// lifecycle timestamps.
func (r *Repository) UpdateDeploymentState(ctx context.Context, id string, params repositorycontract.UpdateDeploymentStateParams) (deployment.Deployment, error) {
	if !validUUID(id) {
		return deployment.Deployment{}, invalid("deployment ID")
	}
	if !params.Status.Valid() {
		return deployment.Deployment{}, invalid("deployment status")
	}
	if strings.TrimSpace(params.CurrentStep) == "" {
		return deployment.Deployment{}, invalid("current step")
	}

	row := r.pool.QueryRow(ctx, `
        UPDATE deployments
        SET status = $2,
            current_step = $3,
            safe_error_code = $4,
            safe_error_message = $5,
            started_at = CASE
                WHEN $2 <> 'CREATED' THEN COALESCE(started_at, now())
                ELSE started_at
            END,
            completed_at = CASE
                WHEN $2 IN ('COMPLETED', 'FAILED', 'CANCELLED', 'DNS_FAILED', 'MANUAL_REVIEW')
                    THEN COALESCE(completed_at, now())
                ELSE NULL
            END,
            updated_at = now()
        WHERE id = $1
        RETURNING `+deploymentColumns,
		id,
		params.Status,
		params.CurrentStep,
		params.SafeErrorCode,
		params.SafeErrorMessage,
	)
	result, err := scanDeployment(row)
	if err != nil {
		return deployment.Deployment{}, mapNotFound("update deployment state", err)
	}
	return result, nil
}

// SetRemnawaveNodeUUID records the UUID assigned after successful node creation.
func (r *Repository) SetRemnawaveNodeUUID(ctx context.Context, id, nodeUUID string) (deployment.Deployment, error) {
	if !validUUID(id) {
		return deployment.Deployment{}, invalid("deployment ID")
	}
	if !validUUID(nodeUUID) {
		return deployment.Deployment{}, invalid("Remnawave Node UUID")
	}

	row := r.pool.QueryRow(ctx, `
        UPDATE deployments
        SET remnawave_node_uuid = $2, updated_at = now()
        WHERE id = $1
        RETURNING `+deploymentColumns,
		id,
		nodeUUID,
	)
	result, err := scanDeployment(row)
	if err != nil {
		return deployment.Deployment{}, mapNotFound("set Remnawave Node UUID", err)
	}
	return result, nil
}

// SetTargetVPSIP updates the canonical address after the panel and DNS have
// both completed an idempotent Node IP replacement.
func (r *Repository) SetTargetVPSIP(ctx context.Context, id string, address netip.Addr) (deployment.Deployment, error) {
	if !validUUID(id) || !address.IsValid() {
		return deployment.Deployment{}, invalid("deployment ID or VPS IP")
	}
	row := r.pool.QueryRow(ctx, `
        UPDATE deployments
        SET target_vps_ip = $2, updated_at = now()
        WHERE id = $1
        RETURNING `+deploymentColumns, id, address.Unmap().String())
	result, err := scanDeployment(row)
	if err != nil {
		return deployment.Deployment{}, mapNotFound("set target VPS IP", err)
	}
	return result, nil
}

// SetNodeHostBinding records the Host selected by a successful managed Node
// move so later certificate and DNS workflows use the new SNI.
func (r *Repository) SetNodeHostBinding(ctx context.Context, id string, params repositorycontract.SetNodeHostBindingParams) (deployment.Deployment, error) {
	if !validUUID(id) || !validUUID(params.HostUUID) || strings.TrimSpace(params.HostRemark) == "" || strings.TrimSpace(params.SNIDomain) == "" {
		return deployment.Deployment{}, invalid("deployment ID or Node Host binding")
	}
	row := r.pool.QueryRow(ctx, `
        UPDATE deployments
        SET selected_remnawave_host_uuid = $2,
            selected_host_remark = $3,
            sni_domain = $4,
            updated_at = now()
        WHERE id = $1
        RETURNING `+deploymentColumns,
		id,
		params.HostUUID,
		strings.TrimSpace(params.HostRemark),
		strings.TrimSpace(params.SNIDomain),
	)
	result, err := scanDeployment(row)
	if err != nil {
		return deployment.Deployment{}, mapNotFound("set Node Host binding", err)
	}
	return result, nil
}

// RecordDeploymentStep inserts or updates one named step. Updating the step and
// deployment timestamp/current step is one transaction.
func (r *Repository) RecordDeploymentStep(ctx context.Context, params repositorycontract.RecordStepParams) (deployment.Step, error) {
	if !validUUID(params.DeploymentID) {
		return deployment.Step{}, invalid("deployment ID")
	}
	if strings.TrimSpace(params.Name) == "" {
		return deployment.Step{}, invalid("step name")
	}
	if !params.Status.Valid() {
		return deployment.Step{}, invalid("step status")
	}

	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return deployment.Step{}, fmt.Errorf("begin deployment step transaction: %w", err)
	}
	defer func() {
		rollbackCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(rollbackCtx)
	}()

	commandTag, err := tx.Exec(ctx, `
        UPDATE deployments
        SET current_step = CASE WHEN $2 = 'RUNNING' THEN $3 ELSE current_step END,
            updated_at = now()
        WHERE id = $1`, params.DeploymentID, params.Status, params.Name)
	if err != nil {
		return deployment.Step{}, fmt.Errorf("touch deployment for step: %w", err)
	}
	if commandTag.RowsAffected() == 0 {
		return deployment.Step{}, fmt.Errorf("record deployment step: %w", repositorycontract.ErrNotFound)
	}

	row := tx.QueryRow(ctx, `
        INSERT INTO deployment_steps (
            deployment_id, step_name, status, safe_output_summary,
            error_message, started_at, completed_at
        ) VALUES (
            $1, $2, $3, $4, $5,
            CASE WHEN $3 = 'PENDING' THEN NULL ELSE now() END,
            CASE WHEN $3 IN ('COMPLETED', 'FAILED', 'SKIPPED') THEN now() ELSE NULL END
        )
        ON CONFLICT (deployment_id, step_name) DO UPDATE
        SET status = EXCLUDED.status,
            safe_output_summary = EXCLUDED.safe_output_summary,
            error_message = EXCLUDED.error_message,
            started_at = CASE
                WHEN EXCLUDED.status = 'PENDING' THEN deployment_steps.started_at
                ELSE COALESCE(deployment_steps.started_at, now())
            END,
            completed_at = CASE
                WHEN EXCLUDED.status IN ('COMPLETED', 'FAILED', 'SKIPPED')
                    THEN COALESCE(deployment_steps.completed_at, now())
                ELSE NULL
            END
        RETURNING deployment_id::text, step_name, status, safe_output_summary,
                  error_message, started_at, completed_at`,
		params.DeploymentID,
		params.Name,
		params.Status,
		params.SafeSummary,
		params.ErrorMessage,
	)
	step, err := scanStep(row)
	if err != nil {
		return deployment.Step{}, fmt.Errorf("record deployment step: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return deployment.Step{}, fmt.Errorf("commit deployment step: %w", err)
	}
	return step, nil
}

// ListDeploymentSteps returns the persisted step snapshots for one deployment.
// Ordering is deterministic and follows the time a step was first started.
func (r *Repository) ListDeploymentSteps(ctx context.Context, deploymentID string) ([]deployment.Step, error) {
	if !validUUID(deploymentID) {
		return nil, invalid("deployment ID")
	}
	rows, err := r.pool.Query(ctx, `
        SELECT deployment_id::text, step_name, status, safe_output_summary,
               error_message, started_at, completed_at
        FROM deployment_steps
        WHERE deployment_id = $1
        ORDER BY started_at NULLS FIRST, step_name`, deploymentID)
	if err != nil {
		return nil, fmt.Errorf("list deployment steps: %w", err)
	}
	defer rows.Close()

	steps := make([]deployment.Step, 0)
	for rows.Next() {
		step, err := scanStep(rows)
		if err != nil {
			return nil, fmt.Errorf("list deployment steps: scan: %w", err)
		}
		steps = append(steps, step)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list deployment steps: iterate: %w", err)
	}
	return steps, nil
}

// ListRecentDeployments returns newest deployments first.
func (r *Repository) ListRecentDeployments(ctx context.Context, limit int) ([]deployment.Deployment, error) {
	return r.list(ctx, `
        SELECT `+deploymentColumns+`
        FROM deployments
        ORDER BY created_at DESC, id DESC
        LIMIT $1`, limit, "list recent deployments")
}

// FindUnfinishedDeployments returns non-terminal deployments, oldest updates
// first, for future recovery handling. It does not resume them.
func (r *Repository) FindUnfinishedDeployments(ctx context.Context, limit int) ([]deployment.Deployment, error) {
	return r.list(ctx, `
        SELECT `+deploymentColumns+`
        FROM deployments
        WHERE status NOT IN ('COMPLETED', 'FAILED', 'CANCELLED', 'DNS_FAILED', 'MANUAL_REVIEW')
        ORDER BY updated_at ASC, id ASC
        LIMIT $1`, limit, "find unfinished deployments")
}

func (r *Repository) FindUnfinishedDeploymentsByPanel(ctx context.Context, panelID string, limit int) ([]deployment.Deployment, error) {
	if !validPanelID(panelID) {
		return nil, invalid("panel ID")
	}
	return r.list(ctx, `SELECT `+deploymentColumns+` FROM deployments WHERE panel_id = $2 AND status NOT IN ('COMPLETED', 'FAILED', 'CANCELLED', 'DNS_FAILED', 'MANUAL_REVIEW') ORDER BY updated_at ASC, id ASC LIMIT $1`, limit, "find unfinished deployments by panel", panelID)
}

func (r *Repository) FindDeploymentByPanelNodeUUID(ctx context.Context, panelID, nodeUUID string) (deployment.Deployment, error) {
	if !validPanelID(panelID) || !validUUID(nodeUUID) {
		return deployment.Deployment{}, invalid("panel ID or Remnawave Node UUID")
	}
	row := r.pool.QueryRow(ctx, `SELECT `+deploymentColumns+` FROM deployments WHERE panel_id = $1 AND remnawave_node_uuid = $2 ORDER BY updated_at DESC LIMIT 1`, panelID, nodeUUID)
	result, err := scanDeployment(row)
	if err != nil {
		return deployment.Deployment{}, mapNotFound("find deployment by panel Node UUID", err)
	}
	return result, nil
}

func (r *Repository) list(ctx context.Context, query string, limit int, operation string, queryArgs ...any) ([]deployment.Deployment, error) {
	if limit < 1 || limit > 100 {
		return nil, invalid("list limit must be between 1 and 100")
	}
	args := append([]any{limit}, queryArgs...)
	rows, err := r.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", operation, err)
	}
	defer rows.Close()

	results := make([]deployment.Deployment, 0)
	for rows.Next() {
		item, err := scanDeployment(rows)
		if err != nil {
			return nil, fmt.Errorf("%s: scan: %w", operation, err)
		}
		results = append(results, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: iterate: %w", operation, err)
	}
	return results, nil
}

type scanner interface {
	Scan(...any) error
}

func scanDeployment(row scanner) (deployment.Deployment, error) {
	var result deployment.Deployment
	var ip string
	err := row.Scan(
		&result.ID,
		&result.PanelID,
		&result.TelegramOperatorUserID,
		&result.SelectedRemnawaveHostUUID,
		&result.SelectedHostRemark,
		&result.SNIDomain,
		&result.NodeName,
		&ip,
		&result.RemnawaveNodeUUID,
		&result.SSHHostKeyFingerprint,
		&result.Status,
		&result.CurrentStep,
		&result.SafeErrorCode,
		&result.SafeErrorMessage,
		&result.CreatedAt,
		&result.UpdatedAt,
		&result.StartedAt,
		&result.CompletedAt,
	)
	if err != nil {
		return deployment.Deployment{}, err
	}
	result.TargetVPSIP, err = netip.ParseAddr(ip)
	if err != nil {
		return deployment.Deployment{}, fmt.Errorf("parse persisted target VPS IP: %w", err)
	}
	return result, nil
}

func scanStep(row scanner) (deployment.Step, error) {
	var result deployment.Step
	err := row.Scan(
		&result.DeploymentID,
		&result.Name,
		&result.Status,
		&result.SafeSummary,
		&result.ErrorMessage,
		&result.StartedAt,
		&result.CompletedAt,
	)
	return result, err
}

func validateCreate(params repositorycontract.CreateDeploymentParams) error {
	if !validUUID(params.ID) {
		return invalid("deployment ID")
	}
	if params.TelegramOperatorUserID <= 0 {
		return invalid("Telegram operator user ID")
	}
	if !validPanelID(params.PanelID) {
		return invalid("panel ID")
	}
	if !validUUID(params.SelectedRemnawaveHostUUID) {
		return invalid("selected Remnawave Host UUID")
	}
	if strings.TrimSpace(params.SNIDomain) == "" {
		return invalid("SNI domain")
	}
	if strings.TrimSpace(params.NodeName) == "" {
		return invalid("Node name")
	}
	if !params.TargetVPSIP.IsValid() {
		return invalid("target VPS IP")
	}
	return nil
}

func validPanelID(value string) bool {
	if len(value) < 1 || len(value) > 32 {
		return false
	}
	for index, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || (index > 0 && (char == '-' || char == '_')) {
			continue
		}
		return false
	}
	return true
}

func validUUID(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return false
	}
	compact := strings.ReplaceAll(value, "-", "")
	decoded := make([]byte, 16)
	_, err := hex.Decode(decoded, []byte(compact))
	return err == nil
}

func invalid(field string) error {
	return fmt.Errorf("%s: %w", field, repositorycontract.ErrInvalidArgument)
}

func mapNotFound(operation string, err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%s: %w", operation, repositorycontract.ErrNotFound)
	}
	return fmt.Errorf("%s: %w", operation, err)
}
