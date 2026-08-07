package provisioner

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"remnanode-setup-bot/internal/deployment"
	"remnanode-setup-bot/internal/repository"
)

// StageNames is the stable provisioning order. Persisted step names are part
// of the recovery contract and must not be changed casually.
var StageNames = []string{
	"system",
	"docker",
	"sysctl",
	"limits",
	"firewall",
	"fail2ban",
	"remnanode",
	"node_exporter",
	"speedtest_exporter",
	"logrotate",
	"xray_sni",
	"healthcheck",
}

// Inspection describes whether a stage already has its desired state.
type Inspection struct {
	Satisfied bool
	Summary   string
}

// Stage implements one independently retryable inspect/apply/validate unit.
type Stage interface {
	Name() string
	Inspect(context.Context) (Inspection, error)
	Apply(context.Context) error
	Validate(context.Context) error
}

// StepStore is deliberately smaller than DeploymentRepository so the engine
// remains easy to test and the storage implementation can evolve separately.
type StepStore interface {
	RecordDeploymentStep(context.Context, repository.RecordStepParams) (deployment.Step, error)
	ListDeploymentSteps(context.Context, string) ([]deployment.Step, error)
}

// Report is a safe result suitable for an operator UI. It contains no remote
// command output and no provisioning secrets.
type Report struct {
	Name    string
	Status  deployment.StepStatus
	Changed bool
	Summary string
}

// Engine runs provisioning stages in their stable order.
type Engine struct {
	store  StepStore
	stages []Stage
}

func NewEngine(store StepStore, stages []Stage) (*Engine, error) {
	if store == nil {
		return nil, errors.New("provisioner step store is required")
	}
	if len(stages) != len(StageNames) {
		return nil, fmt.Errorf("provisioner requires %d stages", len(StageNames))
	}
	for index, stage := range stages {
		if stage == nil || stage.Name() != StageNames[index] {
			return nil, fmt.Errorf("provisioner stage %d must be %q", index, StageNames[index])
		}
	}
	return &Engine{store: store, stages: append([]Stage(nil), stages...)}, nil
}

// Run resumes a deployment. Previously completed stages are trusted and
// skipped; a failed/running stage is safely retried from its inspection phase.
func (e *Engine) Run(ctx context.Context, deploymentID string) ([]Report, error) {
	if strings.TrimSpace(deploymentID) == "" {
		return nil, errors.New("deployment ID is required")
	}
	persisted, err := e.store.ListDeploymentSteps(ctx, deploymentID)
	if err != nil {
		return nil, fmt.Errorf("load provisioning progress: %w", err)
	}
	completed := make(map[string]bool, len(persisted))
	for _, step := range persisted {
		completed[step.Name] = step.Status == deployment.StepStatusCompleted
	}

	reports := make([]Report, 0, len(e.stages))
	for _, stage := range e.stages {
		if completed[stage.Name()] {
			reports = append(reports, Report{Name: stage.Name(), Status: deployment.StepStatusSkipped, Summary: "already completed"})
			continue
		}
		if ctx.Err() != nil {
			return reports, ctx.Err()
		}
		if _, err := e.record(ctx, deploymentID, stage.Name(), deployment.StepStatusRunning, "started", ""); err != nil {
			return reports, fmt.Errorf("record stage start: %w", err)
		}

		report, runErr := runStage(ctx, stage)
		if runErr != nil {
			safeMessage := "stage failed; inspect protected service logs"
			_, persistErr := e.record(ctx, deploymentID, stage.Name(), deployment.StepStatusFailed, report.Summary, safeMessage)
			reports = append(reports, Report{Name: stage.Name(), Status: deployment.StepStatusFailed, Changed: report.Changed, Summary: report.Summary})
			if persistErr != nil {
				return reports, errors.Join(fmt.Errorf("provisioning stage %s failed: %w", stage.Name(), runErr), fmt.Errorf("record stage failure: %w", persistErr))
			}
			return reports, fmt.Errorf("provisioning stage %s failed: %w", stage.Name(), runErr)
		}
		if _, err := e.record(ctx, deploymentID, stage.Name(), deployment.StepStatusCompleted, report.Summary, ""); err != nil {
			return reports, fmt.Errorf("record stage completion: %w", err)
		}
		report.Status = deployment.StepStatusCompleted
		reports = append(reports, report)
	}
	return reports, nil
}

func runStage(ctx context.Context, stage Stage) (Report, error) {
	report := Report{Name: stage.Name()}
	inspection, err := stage.Inspect(ctx)
	if err != nil {
		report.Summary = "inspection failed"
		return report, errors.New("stage inspection failed")
	}
	if inspection.Satisfied {
		report.Summary = safeSummary(inspection.Summary, "already configured")
		return report, nil
	}
	if err := stage.Apply(ctx); err != nil {
		report.Changed = true
		report.Summary = "apply failed"
		return report, errors.New("stage apply failed")
	}
	report.Changed = true
	if err := stage.Validate(ctx); err != nil {
		report.Summary = "validation failed"
		return report, errors.New("stage validation failed")
	}
	report.Summary = "configured and validated"
	return report, nil
}

func (e *Engine) record(ctx context.Context, deploymentID, name string, status deployment.StepStatus, summary, errorMessage string) (deployment.Step, error) {
	params := repository.RecordStepParams{DeploymentID: deploymentID, Name: name, Status: status}
	if summary != "" {
		value := summary
		params.SafeSummary = &value
	}
	if errorMessage != "" {
		value := errorMessage
		params.ErrorMessage = &value
	}
	return e.store.RecordDeploymentStep(ctx, params)
}

func safeSummary(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 160 || strings.ContainsAny(value, "\r\n") {
		return fallback
	}
	return value
}
