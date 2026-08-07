package provisioner

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	"remnanode-setup-bot/internal/deployment"
	"remnanode-setup-bot/internal/repository"
)

type memoryStepStore struct {
	mu    sync.Mutex
	steps map[string]deployment.Step
}

func newMemoryStepStore() *memoryStepStore {
	return &memoryStepStore{steps: make(map[string]deployment.Step)}
}

func (s *memoryStepStore) RecordDeploymentStep(_ context.Context, params repository.RecordStepParams) (deployment.Step, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	step := deployment.Step{DeploymentID: params.DeploymentID, Name: params.Name, Status: params.Status, SafeSummary: params.SafeSummary, ErrorMessage: params.ErrorMessage}
	s.steps[params.Name] = step
	return step, nil
}

func (s *memoryStepStore) ListDeploymentSteps(context.Context, string) ([]deployment.Step, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]deployment.Step, 0, len(s.steps))
	for _, step := range s.steps {
		result = append(result, step)
	}
	return result, nil
}

type fakeStage struct {
	name      string
	events    *[]string
	satisfied bool
	failApply bool
	applies   int
}

func (s *fakeStage) Name() string { return s.name }
func (s *fakeStage) Inspect(context.Context) (Inspection, error) {
	*s.events = append(*s.events, "inspect:"+s.name)
	return Inspection{Satisfied: s.satisfied}, nil
}
func (s *fakeStage) Apply(context.Context) error {
	*s.events = append(*s.events, "apply:"+s.name)
	s.applies++
	if s.failApply {
		return errors.New("sensitive remote failure")
	}
	s.satisfied = true
	return nil
}
func (s *fakeStage) Validate(context.Context) error {
	*s.events = append(*s.events, "validate:"+s.name)
	return nil
}

func fakeStages(events *[]string) ([]Stage, []*fakeStage) {
	stages := make([]Stage, 0, len(StageNames))
	fakes := make([]*fakeStage, 0, len(StageNames))
	for _, name := range StageNames {
		stage := &fakeStage{name: name, events: events}
		stages = append(stages, stage)
		fakes = append(fakes, stage)
	}
	return stages, fakes
}

func TestEngineStageOrdering(t *testing.T) {
	var events []string
	stages, _ := fakeStages(&events)
	engine, err := NewEngine(newMemoryStepStore(), stages)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Run(context.Background(), "deployment"); err != nil {
		t.Fatal(err)
	}
	var inspected []string
	for _, event := range events {
		if len(event) > len("inspect:") && event[:len("inspect:")] == "inspect:" {
			inspected = append(inspected, event[len("inspect:"):])
		}
	}
	if !reflect.DeepEqual(inspected, StageNames) {
		t.Fatalf("inspection order = %v, want %v", inspected, StageNames)
	}
}

func TestEngineResumesAfterFailedStage(t *testing.T) {
	var events []string
	store := newMemoryStepStore()
	store.steps[StageNames[0]] = deployment.Step{Name: StageNames[0], Status: deployment.StepStatusCompleted}
	store.steps[StageNames[1]] = deployment.Step{Name: StageNames[1], Status: deployment.StepStatusFailed}
	stages, fakes := fakeStages(&events)
	engine, _ := NewEngine(store, stages)
	if _, err := engine.Run(context.Background(), "deployment"); err != nil {
		t.Fatal(err)
	}
	if fakes[0].applies != 0 || fakes[1].applies != 1 {
		t.Fatalf("apply counts = first %d, resumed %d", fakes[0].applies, fakes[1].applies)
	}
}

func TestEngineSecondExecutionIsIdempotent(t *testing.T) {
	var events []string
	store := newMemoryStepStore()
	stages, fakes := fakeStages(&events)
	engine, _ := NewEngine(store, stages)
	if _, err := engine.Run(context.Background(), "deployment"); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Run(context.Background(), "deployment"); err != nil {
		t.Fatal(err)
	}
	for _, stage := range fakes {
		if stage.applies != 1 {
			t.Fatalf("stage %s applied %d times, want once", stage.name, stage.applies)
		}
	}
}

func TestEngineStopsAndPersistsSafeFailure(t *testing.T) {
	var events []string
	store := newMemoryStepStore()
	stages, fakes := fakeStages(&events)
	fakes[3].failApply = true
	engine, _ := NewEngine(store, stages)
	if _, err := engine.Run(context.Background(), "deployment"); err == nil {
		t.Fatal("Run() error = nil")
	}
	if fakes[4].applies != 0 {
		t.Fatal("stage after failure was applied")
	}
	failed := store.steps[StageNames[3]]
	if failed.Status != deployment.StepStatusFailed || failed.ErrorMessage == nil || *failed.ErrorMessage != "stage failed; inspect protected service logs" {
		t.Fatalf("persisted failure = %#v", failed)
	}
}
