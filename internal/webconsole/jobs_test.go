package webconsole

import (
	"testing"
	"time"

	"remnanode-setup-bot/internal/telegram"
)

func TestNodeJobFailureClosesProgressWithoutLosingLastStage(t *testing.T) {
	now := time.Date(2026, time.November, 19, 12, 0, 0, 0, time.UTC)
	store := newNodeJobStore(func() time.Time { return now })
	token, _, err := store.begin(42, "theme:hit", "node-uuid")
	if err != nil {
		t.Fatal(err)
	}
	store.progress(token, telegram.NodeActualizeProgress{
		Step: "theme_prepare", Message: "Подключаюсь по SSH", Completed: 1, Total: 4,
	})
	store.fail(token, "Не удалось безопасно выполнить операцию")

	job, ok := store.get(token, 42)
	if !ok {
		t.Fatal("failed job not found")
	}
	if job.Status != jobFailed || job.Progress.Completed != 4 || job.Progress.Total != 4 {
		t.Fatalf("job = %#v", job)
	}
	if job.Progress.Step != "failed" || job.Progress.Message != "Подключаюсь по SSH" {
		t.Fatalf("progress = %#v", job.Progress)
	}
}
