package webconsole

import (
	"errors"
	"sync"
	"time"

	"remnanode-setup-bot/internal/telegram"
)

const (
	jobRunning   = "running"
	jobCompleted = "completed"
	jobFailed    = "failed"
)

var errNodeJobBusy = errors.New("Node job is already running for another operator")

type nodeJob struct {
	UserID    int64
	PanelID   string
	NodeUUID  string
	Status    string
	Progress  telegram.NodeActualizeProgress
	Result    telegram.NodeActualizeResult
	SafeError string
	ExpiresAt time.Time
}

type nodeJobStore struct {
	mu      sync.Mutex
	values  map[string]nodeJob
	running map[string]string
	now     func() time.Time
	maximum int
}

func newNodeJobStore(now func() time.Time) *nodeJobStore {
	return &nodeJobStore{values: make(map[string]nodeJob), running: make(map[string]string), now: now, maximum: 1000}
}

func (s *nodeJobStore) begin(userID int64, panelID, nodeUUID string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	key := panelID + "\x00" + nodeUUID
	if token := s.running[key]; token != "" {
		job := s.values[token]
		if job.UserID == userID {
			return token, true, nil
		}
		return "", false, errNodeJobBusy
	}
	if len(s.values) >= s.maximum {
		return "", false, errors.New("too many Node jobs")
	}
	token, err := randomToken(32)
	if err != nil {
		return "", false, err
	}
	s.values[token] = nodeJob{
		UserID: userID, PanelID: panelID, NodeUUID: nodeUUID, Status: jobRunning,
		Progress:  telegram.NodeActualizeProgress{Step: "queued", Message: "Операция поставлена в очередь", Completed: 0, Total: 1},
		ExpiresAt: s.now().Add(time.Hour),
	}
	s.running[key] = token
	return token, false, nil
}

func (s *nodeJobStore) progress(token string, update telegram.NodeActualizeProgress) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.values[token]
	if !ok || job.Status != jobRunning {
		return
	}
	job.Progress = update
	job.ExpiresAt = s.now().Add(time.Hour)
	s.values[token] = job
}

func (s *nodeJobStore) complete(token string, result telegram.NodeActualizeResult) {
	s.finish(token, jobCompleted, result, "")
}

func (s *nodeJobStore) fail(token, safeError string) {
	s.finish(token, jobFailed, telegram.NodeActualizeResult{}, safeError)
}

func (s *nodeJobStore) finish(token, status string, result telegram.NodeActualizeResult, safeError string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.values[token]
	if !ok || job.Status != jobRunning {
		return
	}
	job.Status, job.Result, job.SafeError, job.ExpiresAt = status, result, safeError, s.now().Add(time.Hour)
	if status == jobCompleted {
		job.Progress.Step = "completed"
		job.Progress.Message = "Операция успешно завершена"
		if job.Progress.Total <= 0 {
			job.Progress.Total = 1
		}
		job.Progress.Completed = job.Progress.Total
	}
	s.values[token] = job
	delete(s.running, job.PanelID+"\x00"+job.NodeUUID)
}

func (s *nodeJobStore) get(token string, userID int64) (nodeJob, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	job, ok := s.values[token]
	return job, ok && job.UserID == userID
}

func (s *nodeJobStore) sweepLocked() {
	now := s.now()
	for token, job := range s.values {
		if job.ExpiresAt.After(now) {
			continue
		}
		delete(s.values, token)
		if s.running[job.PanelID+"\x00"+job.NodeUUID] == token {
			delete(s.running, job.PanelID+"\x00"+job.NodeUUID)
		}
	}
}
